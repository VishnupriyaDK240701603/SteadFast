package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

type BaselineJob struct {
	ID          string `json:"id"`
	OrderID     string `json:"order_id"`
	CustomerID  string `json:"customer_id"`
	AmountCents int64  `json:"amount_cents"`
	Attempts    int    `json:"attempts"`
	MaxAttempts int    `json:"max_attempts"`
	CreatedAt   string `json:"created_at"`
}

type BaselineWorker struct {
	rdb                *redis.Client
	ledgerBaseURL      string
	pattern            string
	visibilitySeconds  int
	sendIdempotencyKey bool
	httpClient         *http.Client
	logger             *slog.Logger
}

func NewBaselineWorker(rdb *redis.Client, ledgerBaseURL, pattern string, visSec int, sendKey bool, logger *slog.Logger) *BaselineWorker {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &BaselineWorker{
		rdb:                rdb,
		ledgerBaseURL:      ledgerBaseURL,
		pattern:            pattern,
		visibilitySeconds:  visSec,
		sendIdempotencyKey: sendKey,
		httpClient:         &http.Client{Timeout: 5 * time.Second},
		logger:             logger,
	}
}

func (w *BaselineWorker) Run(ctx context.Context) {
	w.logger.Info("baseline worker started", "pattern", w.pattern, "vis_sec", w.visibilitySeconds, "send_key", w.sendIdempotencyKey)

	// If visibility timeout enabled, run supervisor in background
	if w.pattern == "visibility_timeout" {
		go w.runVisibilitySupervisor(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := w.processNextJob(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func (w *BaselineWorker) processNextJob(ctx context.Context) error {
	var rawJob string

	if w.pattern == "ack_on_pull" {
		res, rErr := w.rdb.RPop(ctx, "baseline:queue").Result()
		if rErr != nil {
			if errorsIsRedisNil(rErr) {
				time.Sleep(100 * time.Millisecond)
				return nil
			}
			return rErr
		}
		rawJob = res
	} else {
		// visibility_timeout: RPOPLPUSH from baseline:queue to baseline:processing
		res, rErr := w.rdb.LMove(ctx, "baseline:queue", "baseline:processing", "RIGHT", "LEFT").Result()
		if rErr != nil {
			if errorsIsRedisNil(rErr) {
				time.Sleep(100 * time.Millisecond)
				return nil
			}
			return rErr
		}
		rawJob = res
	}

	var job BaselineJob
	if err := json.Unmarshal([]byte(rawJob), &job); err != nil {
		w.logger.Error("failed to decode baseline job", "raw", rawJob, "error", err)
		if w.pattern != "ack_on_pull" {
			_ = w.rdb.LRem(ctx, "baseline:processing", 1, rawJob).Err()
		}
		return nil
	}

	// Set lease timestamp in Redis for visibility supervisor
	leaseKey := "baseline:lease:" + job.ID
	if w.pattern != "ack_on_pull" {
		_ = w.rdb.Set(ctx, leaseKey, strconv.FormatInt(time.Now().Unix(), 10), time.Duration(w.visibilitySeconds*2)*time.Second).Err()
	}

	// Execute charge against mock ledger
	chargeSuccess := w.executeCharge(ctx, &job)

	if w.pattern == "ack_on_pull" {
		if !chargeSuccess {
			job.Attempts++
			if job.Attempts < job.MaxAttempts {
				b, _ := json.Marshal(job)
				_ = w.rdb.LPush(ctx, "baseline:queue", string(b)).Err()
			} else {
				b, _ := json.Marshal(job)
				_ = w.rdb.LPush(ctx, "baseline:failed", string(b)).Err()
			}
		}
		return nil
	}

	// visibility_timeout cleanup
	_ = w.rdb.Del(ctx, leaseKey).Err()
	_ = w.rdb.LRem(ctx, "baseline:processing", 1, rawJob).Err()

	if !chargeSuccess {
		job.Attempts++
		b, _ := json.Marshal(job)
		if job.Attempts < job.MaxAttempts {
			_ = w.rdb.LPush(ctx, "baseline:queue", string(b)).Err()
		} else {
			_ = w.rdb.LPush(ctx, "baseline:failed", string(b)).Err()
		}
	}

	return nil
}

func (w *BaselineWorker) executeCharge(ctx context.Context, job *BaselineJob) bool {
	body, _ := json.Marshal(map[string]any{
		"order_id":     job.OrderID,
		"customer_id":  job.CustomerID,
		"amount_cents": job.AmountCents,
	})

	url := fmt.Sprintf("%s/charge", w.ledgerBaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")

	if w.sendIdempotencyKey {
		req.Header.Set("Idempotency-Key", "baseline:"+job.ID)
	}

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (w *BaselineWorker) runVisibilitySupervisor(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Inspect items in baseline:processing
			items, err := w.rdb.LRange(ctx, "baseline:processing", 0, -1).Result()
			if err != nil || len(items) == 0 {
				continue
			}

			now := time.Now().Unix()
			for _, item := range items {
				var job BaselineJob
				if err := json.Unmarshal([]byte(item), &job); err != nil {
					continue
				}
				leaseKey := "baseline:lease:" + job.ID
				val, err := w.rdb.Get(ctx, leaseKey).Result()
				if err == nil {
					leaseTime, _ := strconv.ParseInt(val, 10, 64)
					if now-leaseTime > int64(w.visibilitySeconds) {
						// Lease expired! Requeue
						w.logger.Warn("baseline visibility timeout expired, requeuing job", "job_id", job.ID)
						_ = w.rdb.LRem(ctx, "baseline:processing", 1, item).Err()
						_ = w.rdb.Del(ctx, leaseKey).Err()
						_ = w.rdb.LPush(ctx, "baseline:queue", item).Err()
					}
				}
			}
		}
	}
}

func errorsIsRedisNil(err error) bool {
	return err == redis.Nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://127.0.0.1:6379"
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		logger.Error("invalid REDIS_URL", "error", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(opt)
	defer func() { _ = rdb.Close() }()

	ledgerURL := os.Getenv("LEDGER_BASE_URL")
	if ledgerURL == "" {
		ledgerURL = "http://127.0.0.1:8082"
	}

	pattern := os.Getenv("BASELINE_PATTERN")
	if pattern == "" {
		pattern = "visibility_timeout"
	}

	visSec := 10
	if vStr := os.Getenv("BASELINE_VISIBILITY_SECONDS"); vStr != "" {
		if v, err := strconv.Atoi(vStr); err == nil && v > 0 {
			visSec = v
		}
	}

	sendKey := os.Getenv("BASELINE_SEND_IDEMPOTENCY_KEY") == "true"

	worker := NewBaselineWorker(rdb, ledgerURL, pattern, visSec, sendKey, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	worker.Run(ctx)
}
