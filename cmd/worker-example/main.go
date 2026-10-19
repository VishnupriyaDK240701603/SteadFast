package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"steadfast/sdk/worker"

	"github.com/google/uuid"
)

type ChargePayload struct {
	OrderID     string `json:"order_id"`
	CustomerID  string `json:"customer_id"`
	AmountCents int64  `json:"amount_cents"`
}

func ChargeCustomerHandler(ledgerBaseURL string, client *http.Client) worker.Handler {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return func(ctx context.Context, job *worker.Job) error {
		var sim struct {
			SimulationMode string `json:"simulation_mode"`
			SleepSeconds   int    `json:"sleep_seconds"`
		}
		_ = json.Unmarshal(job.Payload, &sim)

		if sim.SleepSeconds > 0 {
			time.Sleep(time.Duration(sim.SleepSeconds) * time.Second)
		}
		if sim.SimulationMode == "retry_once" && job.Attempt == 1 {
			return worker.Retryable(fmt.Errorf("simulated network timeout on attempt 1 (will auto-retry)"))
		}
		if sim.SimulationMode == "poison_fail" {
			return worker.Permanent(fmt.Errorf("simulated fatal error: card expired or unrecoverable error"))
		}
		if sim.SimulationMode == "crash_reaper" && job.Attempt == 1 {
			slog.Warn("💥 JURY FLOW: Worker simulating hard crash (SIGKILL mid-execution)", "job_id", job.ID, "attempt", job.Attempt)
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
			return nil
		}

		var payload ChargePayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return worker.Permanent(fmt.Errorf("malformed payload: %w", err))
		}

		reqBody, _ := json.Marshal(payload)
		chargeURL := fmt.Sprintf("%s/charge", ledgerBaseURL)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, chargeURL, bytes.NewReader(reqBody))
		if err != nil {
			return worker.Permanent(fmt.Errorf("failed to create charge request: %w", err))
		}

		req.Header.Set("Content-Type", "application/json")
		if os.Getenv("STEADFAST_DISABLE_IDEMPOTENCY") != "1" {
			req.Header.Set("Idempotency-Key", fmt.Sprintf("steadfast:%s", job.ID.String()))
		}
		req.Header.Set("X-Fencing-Token", strconv.FormatInt(job.FencingToken, 10))

		resp, err := client.Do(req)
		if err != nil {
			// Network error, timeout, or canceled context
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return worker.Retryable(fmt.Errorf("downstream network error: %w", err))
		}
		defer func() { _ = resp.Body.Close() }()

		respBytes, _ := io.ReadAll(resp.Body)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return worker.Permanent(fmt.Errorf("downstream 4xx error (status %d): %s", resp.StatusCode, string(respBytes)))
		}

		// 5xx and others
		return worker.Retryable(fmt.Errorf("downstream 5xx error (status %d): %s", resp.StatusCode, string(respBytes)))
	}
}

func GenericSimulationHandler(name string) worker.Handler {
	return func(ctx context.Context, job *worker.Job) error {
		var sim struct {
			SimulationMode string `json:"simulation_mode"`
			SleepSeconds   int    `json:"sleep_seconds"`
		}
		_ = json.Unmarshal(job.Payload, &sim)

		if sim.SleepSeconds > 0 {
			time.Sleep(time.Duration(sim.SleepSeconds) * time.Second)
		}
		if sim.SimulationMode == "retry_once" && job.Attempt == 1 {
			return worker.Retryable(fmt.Errorf("simulated %s timeout on attempt 1 (auto-retrying)", name))
		}
		if sim.SimulationMode == "poison_fail" {
			return worker.Permanent(fmt.Errorf("simulated fatal %s validation failure (dead-lettered)", name))
		}
		return nil
	}
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	apiBaseURL := os.Getenv("API_BASE_URL")
	if apiBaseURL == "" {
		apiBaseURL = "http://127.0.0.1:8080"
	}

	ledgerBaseURL := os.Getenv("LEDGER_BASE_URL")
	if ledgerBaseURL == "" {
		ledgerBaseURL = "http://127.0.0.1:8082"
	}

	tenantKey := os.Getenv("TENANT_KEY")
	if tenantKey == "" {
		logger.Error("TENANT_KEY environment variable is required")
		os.Exit(1)
	}

	workerID := os.Getenv("WORKER_ID")
	if workerID == "" {
		workerID = "worker-" + uuid.NewString()[:8]
	}

	concurrency := 4
	if cStr := os.Getenv("CONCURRENCY"); cStr != "" {
		if c, err := strconv.Atoi(cStr); err == nil && c > 0 {
			concurrency = c
		}
	}

	pollInterval := 500 * time.Millisecond
	if pStr := os.Getenv("POLL_INTERVAL_MS"); pStr != "" {
		if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
			pollInterval = time.Duration(p) * time.Millisecond
		}
	}

	cfg := worker.Config{
		APIBaseURL:     apiBaseURL,
		APIKey:         tenantKey,
		WorkerID:       workerID,
		Concurrency:    concurrency,
		ClaimBatchSize: 10,
		PollInterval:   pollInterval,
		Logger:         logger,
		Handlers: map[string]worker.Handler{
			"charge_customer": ChargeCustomerHandler(ledgerBaseURL, nil),
			"order.charge":    ChargeCustomerHandler(ledgerBaseURL, nil),
			"email_dispatch":  GenericSimulationHandler("email_dispatch"),
			"data_pipeline":   GenericSimulationHandler("data_pipeline"),
			"report_generate": GenericSimulationHandler("report_generate"),
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("starting example worker", "worker_id", workerID, "api", apiBaseURL, "ledger", ledgerBaseURL, "concurrency", concurrency)
	if err := worker.Run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("worker terminated with error", "error", err)
		os.Exit(1)
	}
	logger.Info("example worker stopped cleanly")
}
