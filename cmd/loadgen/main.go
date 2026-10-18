package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Config struct {
	Count       int
	Rate        int // RPS, 0 = unthrottled
	Seed        int64
	Lane        string // steadfast, baseline, both
	APIBaseURL  string
	BaselineURL string
	TenantKey   string
	OutDir      string
	RunID       string
}

type IntentRecord struct {
	RunID          string `json:"run_id"`
	Seq            int    `json:"seq"`
	OrderID        string `json:"order_id"`
	IdempotencyKey string `json:"idempotency_key"`
	AmountCents    int64  `json:"amount_cents"`
	Lane           string `json:"lane"`
	CreatedAt      string `json:"created_at"`
}

type AckRecord struct {
	RunID       string `json:"run_id"`
	Seq         int    `json:"seq"`
	OrderID     string `json:"order_id"`
	JobID       string `json:"job_id"`
	AmountCents int64  `json:"amount_cents"`
	AckAt       string `json:"ack_at"`
	Lane        string `json:"lane"`
}

type LoadGenerator struct {
	cfg        Config
	httpClient *http.Client
	logger     *slog.Logger
	mu         sync.Mutex
}

func NewLoadGenerator(cfg Config, logger *slog.Logger) *LoadGenerator {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	return &LoadGenerator{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		logger:     logger,
	}
}

func (lg *LoadGenerator) Run(ctx context.Context) error {
	if err := os.MkdirAll(lg.cfg.OutDir, 0755); err != nil {
		return fmt.Errorf("failed to create output dir: %w", err)
	}

	if lg.cfg.Lane == "both" || lg.cfg.Lane == "steadfast" {
		if err := lg.runLane(ctx, "steadfast"); err != nil {
			return fmt.Errorf("steadfast lane error: %w", err)
		}
	}

	if lg.cfg.Lane == "both" || lg.cfg.Lane == "baseline" {
		if err := lg.runLane(ctx, "baseline"); err != nil {
			return fmt.Errorf("baseline lane error: %w", err)
		}
	}

	return nil
}

func (lg *LoadGenerator) runLane(ctx context.Context, lane string) error {
	intentPath := filepath.Join(lg.cfg.OutDir, fmt.Sprintf("intent-%s.ndjson", lane))
	ackPath := filepath.Join(lg.cfg.OutDir, fmt.Sprintf("acks-%s.ndjson", lane))

	intentFile, err := os.OpenFile(intentPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open intent file: %w", err)
	}
	defer func() { _ = intentFile.Close() }()

	ackFile, err := os.OpenFile(ackPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open ack file: %w", err)
	}
	defer func() { _ = ackFile.Close() }()

	rng := rand.New(rand.NewSource(lg.cfg.Seed))

	var throttleTicker *time.Ticker
	if lg.cfg.Rate > 0 {
		interval := time.Second / time.Duration(lg.cfg.Rate)
		if interval <= 0 {
			interval = time.Millisecond
		}
		throttleTicker = time.NewTicker(interval)
		defer throttleTicker.Stop()
	}

	lg.logger.Info("starting loadgen lane", "lane", lane, "count", lg.cfg.Count, "rate", lg.cfg.Rate, "seed", lg.cfg.Seed)

	for seq := 1; seq <= lg.cfg.Count; seq++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if throttleTicker != nil {
			<-throttleTicker.C
		}

		// Reproducible amount 500 to 50,000 cents
		amount := 500 + rng.Int63n(49501)
		orderID := fmt.Sprintf("ord-%s-%s-%06d", lanePrefix(lane), lg.cfg.RunID, seq)
		custID := fmt.Sprintf("cust-%04d", (seq%1000)+1)
		nowStr := time.Now().UTC().Format(time.RFC3339Nano)

		// 1. Pre-call Intent Log
		intent := IntentRecord{
			RunID:          lg.cfg.RunID,
			Seq:            seq,
			OrderID:        orderID,
			IdempotencyKey: orderID,
			AmountCents:    amount,
			Lane:           lane,
			CreatedAt:      nowStr,
		}
		intentBytes, _ := json.Marshal(intent)
		lg.mu.Lock()
		_, _ = intentFile.Write(append(intentBytes, 0x0a))
		_ = intentFile.Sync()
		lg.mu.Unlock()

		// 2. Submit with idempotent retries
		jobID, ackErr := lg.submitWithRetries(ctx, lane, orderID, custID, amount)
		if ackErr != nil {
			lg.logger.Warn("submission failed after retries", "lane", lane, "seq", seq, "order_id", orderID, "error", ackErr)
			continue
		}

		// 3. Post-call Ack Log
		ack := AckRecord{
			RunID:       lg.cfg.RunID,
			Seq:         seq,
			OrderID:     orderID,
			JobID:       jobID,
			AmountCents: amount,
			AckAt:       time.Now().UTC().Format(time.RFC3339Nano),
			Lane:        lane,
		}
		ackBytes, _ := json.Marshal(ack)
		lg.mu.Lock()
		_, _ = ackFile.Write(append(ackBytes, 0x0a))
		_ = ackFile.Sync()
		lg.mu.Unlock()

		if seq%200 == 0 || seq == lg.cfg.Count {
			lg.logger.Info("loadgen progress", "lane", lane, "acknowledged", seq, "total", lg.cfg.Count)
		}
	}

	lg.logger.Info("loadgen lane completed", "lane", lane, "count", lg.cfg.Count)
	return nil
}

func lanePrefix(lane string) string {
	if lane == "steadfast" {
		return "st"
	}
	return "bl"
}

func (lg *LoadGenerator) submitWithRetries(ctx context.Context, lane, orderID, custID string, amount int64) (string, error) {
	var lastErr error

	for attempt := 0; attempt < 3; attempt++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		jobID, err := lg.submitOnce(ctx, lane, orderID, custID, amount)
		if err == nil {
			return jobID, nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}

	return "", lastErr
}

func (lg *LoadGenerator) submitOnce(ctx context.Context, lane, orderID, custID string, amount int64) (string, error) {
	if lane == "steadfast" {
		payload := map[string]any{
			"type": "charge_customer",
			"payload": map[string]any{
				"order_id":     orderID,
				"customer_id":  custID,
				"amount_cents": amount,
			},
		}
		bodyBytes, _ := json.Marshal(payload)

		url := fmt.Sprintf("%s/v1/jobs", lg.cfg.APIBaseURL)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+lg.cfg.TenantKey)
		req.Header.Set("Idempotency-Key", orderID)

		resp, err := lg.httpClient.Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp.Body)
			return "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(b))
		}

		var ack struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
			return "", err
		}
		return ack.ID, nil
	}

	// baseline lane
	payload := map[string]any{
		"order_id":     orderID,
		"customer_id":  custID,
		"amount_cents": amount,
	}
	bodyBytes, _ := json.Marshal(payload)

	url := fmt.Sprintf("%s/jobs", lg.cfg.BaselineURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := lg.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("unexpected baseline status %d: %s", resp.StatusCode, string(b))
	}

	var ack struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return "", err
	}
	return ack.ID, nil
}

func main() {
	countFlag := flag.Int("count", 1000, "number of jobs to submit per lane")
	rateFlag := flag.Int("rate", 0, "target requests per second (0 = unthrottled)")
	seedFlag := flag.Int64("seed", 42, "reproducible random seed")
	laneFlag := flag.String("lane", "both", "lane to target (steadfast, baseline, both)")
	apiURLFlag := flag.String("api-url", "http://127.0.0.1:8080", "Steadfast API base URL")
	baselineURLFlag := flag.String("baseline-url", "http://127.0.0.1:8081", "Baseline API base URL")
	baselineAPIURLFlag := flag.String("baseline-api-url", "", "Alias for baseline-url")
	tenantKeyFlag := flag.String("tenant-key", "", "Steadfast tenant API key")
	apiKeyFlag := flag.String("api-key", "", "Alias for tenant-key")
	outDirFlag := flag.String("out-dir", "", "directory to write acks and intent files")
	artifactsDirFlag := flag.String("artifacts-dir", "", "Alias for out-dir")
	flag.Parse()

	baselineURL := *baselineURLFlag
	if *baselineAPIURLFlag != "" {
		baselineURL = *baselineAPIURLFlag
	}

	tenantKey := *tenantKeyFlag
	if *apiKeyFlag != "" {
		tenantKey = *apiKeyFlag
	}
	if tenantKey == "" {
		tenantKey = os.Getenv("DEMO_TENANT_A_KEY")
		if tenantKey == "" {
			tenantKey = os.Getenv("TENANT_KEY")
		}
	}

	outDir := *outDirFlag
	if *artifactsDirFlag != "" {
		outDir = *artifactsDirFlag
	}
	if outDir == "" {
		outDir = "artifacts"
	}

	runID := "run-" + uuid.NewString()[:8]

	cfg := Config{
		Count:       *countFlag,
		Rate:        *rateFlag,
		Seed:        *seedFlag,
		Lane:        *laneFlag,
		APIBaseURL:  *apiURLFlag,
		BaselineURL: baselineURL,
		TenantKey:   tenantKey,
		OutDir:      outDir,
		RunID:       runID,
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	lg := NewLoadGenerator(cfg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := lg.Run(ctx); err != nil {
		logger.Error("loadgen terminated with error", "error", err)
		os.Exit(1)
	}
	logger.Info("loadgen completed successfully", "run_id", runID)
}
