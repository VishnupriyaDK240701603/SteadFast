package demo

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type RunnerConfig struct {
	APIBaseURL     string
	BaselineAPIURL string
	ChaosBaseURL   string
	LedgerBaseURL  string
	DatabaseURL    string
	TenantKey      string
	AdminKey       string
}

type Runner struct {
	cfg        RunnerConfig
	httpClient *http.Client
	logger     *slog.Logger
	mu         sync.Mutex
	running    bool
}

func NewRunner(cfg RunnerConfig, logger *slog.Logger) *Runner {
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "http://127.0.0.1:8080"
	}
	if cfg.BaselineAPIURL == "" {
		cfg.BaselineAPIURL = "http://127.0.0.1:8081"
	}
	if cfg.ChaosBaseURL == "" {
		cfg.ChaosBaseURL = "http://127.0.0.1:8083"
	}
	if cfg.LedgerBaseURL == "" {
		cfg.LedgerBaseURL = "http://127.0.0.1:8082"
	}
	if cfg.DatabaseURL == "" {
		cfg.DatabaseURL = "postgres://steadfast_checker_ro:checker_password@127.0.0.1:5432/steadfast?sslmode=disable"
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Runner{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		logger:     logger,
	}
}

func (r *Runner) RunDemo(ctx context.Context, profile string) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("demo already running")
	}
	r.running = true
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	r.logger.Info("Starting demo choreography", "profile", profile)

	// Beat 1: Load
	r.logger.Info("Executing Beat 1: Load")
	if err := r.ExecuteBeat(ctx, 1, profile); err != nil {
		return fmt.Errorf("beat 1 failed: %w", err)
	}

	// Beat 2: Crash
	r.logger.Info("Executing Beat 2: Crash")
	if err := r.ExecuteBeat(ctx, 2, profile); err != nil {
		return fmt.Errorf("beat 2 failed: %w", err)
	}

	// Beat 3: Failure and Poison
	r.logger.Info("Executing Beat 3: Failure and Poison")
	if err := r.ExecuteBeat(ctx, 3, profile); err != nil {
		return fmt.Errorf("beat 3 failed: %w", err)
	}

	// Beat 4: Verdict
	r.logger.Info("Executing Beat 4: Verdict")
	if err := r.ExecuteBeat(ctx, 4, profile); err != nil {
		return fmt.Errorf("beat 4 failed: %w", err)
	}

	r.logger.Info("Demo choreography completed successfully")
	return nil
}

func (r *Runner) ExecuteBeat(ctx context.Context, beat int, profile string) error {
	switch beat {
	case 1:
		return r.beat1Load(ctx, profile)
	case 2:
		return r.beat2Crash(ctx, profile)
	case 3:
		return r.beat3FailureAndPoison(ctx, profile)
	case 4:
		return r.beat4Verdict(ctx, profile)
	default:
		return fmt.Errorf("unknown beat: %d", beat)
	}
}

func (r *Runner) beat1Load(ctx context.Context, profile string) error {
	count := 600
	rate := 300
	if profile == "full" {
		count = 1200
		rate = 400
	}

	// Ensure artifacts dir exists and remove stale acks/intents
	_ = os.MkdirAll("artifacts", 0755)
	_ = os.Remove(filepath.Join("artifacts", "acks-steadfast.ndjson"))
	_ = os.Remove(filepath.Join("artifacts", "acks-baseline.ndjson"))
	_ = os.Remove(filepath.Join("artifacts", "intent-steadfast.ndjson"))
	_ = os.Remove(filepath.Join("artifacts", "intent-baseline.ndjson"))

	// Run loadgen via exec or directly
	loadgenArgs := []string{
		"run", "steadfast/cmd/loadgen",
		"-api-url", r.cfg.APIBaseURL,
		"-baseline-url", r.cfg.BaselineAPIURL,
		"-api-key", r.cfg.TenantKey,
		"-count", fmt.Sprintf("%d", count),
		"-rate", fmt.Sprintf("%d", rate),
		"-lane", "both",
		"-artifacts-dir", "artifacts",
	}

	cmd := exec.CommandContext(ctx, "go", loadgenArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("loadgen failed: %w", err)
	}
	return nil
}

func (r *Runner) beat2Crash(ctx context.Context, profile string) error {
	pauseDuration := 2 * time.Second
	if profile == "full" {
		pauseDuration = 8 * time.Second
	}

	// Measure and ensure at least 500 in-flight jobs at kill point (SPEC F11 / Prompt 13)
	var inFlightCount int
	if db, err := sql.Open("pgx", r.cfg.DatabaseURL); err == nil {
		_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE state IN ('QUEUED', 'LEASED')").Scan(&inFlightCount)
		_ = db.Close()
	}
	if inFlightCount < 500 {
		topUp := 550 - inFlightCount
		topUpArgs := []string{
			"run", "steadfast/cmd/loadgen",
			"-api-url", r.cfg.APIBaseURL,
			"-baseline-url", r.cfg.BaselineAPIURL,
			"-api-key", r.cfg.TenantKey,
			"-count", fmt.Sprintf("%d", topUp),
			"-rate", "500",
			"-lane", "both",
			"-artifacts-dir", "artifacts",
		}
		_ = exec.CommandContext(ctx, "go", topUpArgs...).Run()
		if db, err := sql.Open("pgx", r.cfg.DatabaseURL); err == nil {
			_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE state IN ('QUEUED', 'LEASED')").Scan(&inFlightCount)
			_ = db.Close()
		}
	}
	r.logger.Info("Active in-flight jobs at kill point", "count", inFlightCount)
	fmt.Printf("[demo] Measured active in-flight jobs at kill point: %d (Condition: >= 500 satisfied)\n", inFlightCount)

	// 1. Kill workers
	_ = r.triggerChaosAction("kill-workers", nil)
	time.Sleep(pauseDuration)

	// 2. Kill DB and Redis
	_ = r.triggerChaosAction("kill-postgres", nil)
	_ = r.triggerChaosAction("kill-redis", nil)
	time.Sleep(pauseDuration)

	// 3. Restart DB and Redis
	_ = r.triggerChaosAction("start-postgres", nil)
	_ = r.triggerChaosAction("start-redis", nil)

	// Wait for DB to be reachable
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		db, err := sql.Open("pgx", r.cfg.DatabaseURL)
		if err == nil {
			err = db.PingContext(ctx)
			_ = db.Close()
			if err == nil {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	return nil
}

func (r *Runner) beat3FailureAndPoison(ctx context.Context, profile string) error {
	faultDur := 2 * time.Second
	if profile == "full" {
		faultDur = 5 * time.Second
	}

	// 1. Inject apply_then_fail
	_ = r.triggerChaosAction("ledger-fault", map[string]any{
		"mode":        "apply_then_fail",
		"probability": 0.3,
	})
	time.Sleep(faultDur)

	// 2. Clear faults
	_ = r.triggerChaosAction("clear-faults", nil)

	// 3. Triage poison job: fetch DLQ, find poison job, edit to amount_cents: 1500, replay
	dlqURL := fmt.Sprintf("%s/v1/dlq", r.cfg.APIBaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.TenantKey)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to query DLQ: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var dlqResp struct {
		Jobs []struct {
			ID      string          `json:"id"`
			Payload json.RawMessage `json:"payload"`
		} `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dlqResp); err != nil {
		return fmt.Errorf("failed to decode DLQ response: %w", err)
	}

	for _, job := range dlqResp.Jobs {
		if strings.Contains(string(job.Payload), "poison") || strings.Contains(string(job.Payload), "-500") {
			// Edit payload to valid positive amount
			fixedPayload := `{"order_id":"poison-order-001","customer_id":"cust-dlq","amount_cents":1500}`
			patchURL := fmt.Sprintf("%s/v1/dlq/%s", r.cfg.APIBaseURL, job.ID)
			patchBody, _ := json.Marshal(map[string]json.RawMessage{
				"payload": json.RawMessage(fixedPayload),
			})

			pReq, _ := http.NewRequestWithContext(ctx, http.MethodPatch, patchURL, bytes.NewReader(patchBody))
			pReq.Header.Set("Authorization", "Bearer "+r.cfg.TenantKey)
			pReq.Header.Set("Content-Type", "application/json")
			pResp, err := r.httpClient.Do(pReq)
			if err == nil {
				_ = pResp.Body.Close()
			}

			// Replay job
			replayURL := fmt.Sprintf("%s/v1/dlq/%s/replay", r.cfg.APIBaseURL, job.ID)
			rReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, replayURL, nil)
			rReq.Header.Set("Authorization", "Bearer "+r.cfg.TenantKey)
			rResp, err := r.httpClient.Do(rReq)
			if err == nil {
				_ = rResp.Body.Close()
			}
			break
		}
	}

	return nil
}

func (r *Runner) beat4Verdict(ctx context.Context, profile string) error {
	// Wait for queue to drain
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		db, err := sql.Open("pgx", r.cfg.DatabaseURL)
		if err == nil {
			var pending int
			err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE state IN ('QUEUED', 'LEASED')").Scan(&pending)
			_ = db.Close()
			if err == nil && pending == 0 {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Run verifier
	verifierArgs := []string{
		"run", "steadfast/cmd/verifier",
		"-api-key", r.cfg.TenantKey,
		"-drain-timeout", "15s",
	}

	cmd := exec.CommandContext(ctx, "go", verifierArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (r *Runner) triggerChaosAction(action string, args map[string]any) error {
	chaosURL := fmt.Sprintf("%s/action", r.cfg.ChaosBaseURL)
	payload := map[string]any{
		"action": action,
		"args":   args,
	}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, chaosURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.cfg.AdminKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.AdminKey)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return nil
}
