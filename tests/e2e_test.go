package tests

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

const (
	dbURL           = "postgres://steadfast_user:fake_pg_password_for_local_demo_only@127.0.0.1:5432/steadfast?sslmode=disable"
	checkerDBURL    = "postgres://steadfast_checker_ro:fake_ro_password_for_local_demo@127.0.0.1:5432/steadfast?sslmode=disable"
	redisURL        = "redis://127.0.0.1:6379/0"
	ledgerPort      = 8082
	apiPort         = 8080
	baselineAPIPort = 8081
)

func getTenantAPIKey() string {
	if k := os.Getenv("DEMO_TENANT_A_KEY"); k != "" {
		return k
	}
	for _, p := range []string{".env", "../.env", "../../.env", "/home/dkvis/steadfast/.env"} {
		data, err := os.ReadFile(p)
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "DEMO_TENANT_A_KEY=") {
					k := strings.TrimSpace(strings.TrimPrefix(line, "DEMO_TENANT_A_KEY="))
					if k != "" {
						return k
					}
				}
			}
		}
	}
	return "demo-test-tenant-key-p07"
}

func buildBinary(t *testing.T, pkg, outPath string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", outPath, pkg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to build %s: %v\nOutput: %s", pkg, err, string(out))
	}
}

func waitForHTTP(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", url)
}

func resetDBAndRedis(t *testing.T) {
	t.Helper()
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("failed to connect to db for reset: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec("TRUNCATE jobs, job_attempts, idempotency_keys, audit_log CASCADE;")
	if err != nil {
		t.Fatalf("failed to truncate db tables: %v", err)
	}

	key := getTenantAPIKey()
	hash := sha256.Sum256([]byte(key))
	hashHex := hex.EncodeToString(hash[:])
	_, err = db.Exec(`
		INSERT INTO tenants (id, name, api_key_hash)
		VALUES ('0dd769e5-fb9a-4641-9fca-30cf88a3e3bb', 'shop-a', $1)
		ON CONFLICT (id) DO UPDATE SET api_key_hash = EXCLUDED.api_key_hash;
	`, hashHex)
	if err != nil {
		t.Fatalf("failed to seed test tenant: %v", err)
	}

	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("failed to parse redis url: %v", err)
	}
	rdb := redis.NewClient(opt)
	defer func() { _ = rdb.Close() }()

	if err := rdb.FlushAll(context.Background()).Err(); err != nil {
		t.Fatalf("failed to flush redis: %v", err)
	}
}

func TestE2E_1000JobsCleanRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e integration test in short mode")
	}

	// Verify DB and Redis are up
	testDB, err := sql.Open("pgx", dbURL)
	if err != nil || testDB.Ping() != nil {
		t.Skip("PostgreSQL not available at 127.0.0.1:5432, skipping e2e test")
	}
	_ = testDB.Close()

	binDir := t.TempDir()
	binLedger := filepath.Join(binDir, "ledger")
	binAPI := filepath.Join(binDir, "api")
	binWorker := filepath.Join(binDir, "worker-example")
	binReaper := filepath.Join(binDir, "reaper")
	binBaselineAPI := filepath.Join(binDir, "baseline-api")
	binBaselineWorker := filepath.Join(binDir, "baseline-worker")
	binLoadgen := filepath.Join(binDir, "loadgen")
	binVerifier := filepath.Join(binDir, "verifier")

	t.Log("Building binaries...")
	buildBinary(t, "steadfast/cmd/ledger", binLedger)
	buildBinary(t, "steadfast/cmd/api", binAPI)
	buildBinary(t, "steadfast/cmd/worker-example", binWorker)
	buildBinary(t, "steadfast/cmd/reaper", binReaper)
	buildBinary(t, "steadfast/cmd/baseline-api", binBaselineAPI)
	buildBinary(t, "steadfast/cmd/baseline-worker", binBaselineWorker)
	buildBinary(t, "steadfast/cmd/loadgen", binLoadgen)
	buildBinary(t, "steadfast/cmd/verifier", binVerifier)

	resetDBAndRedis(t)

	artifactsDir := filepath.Join(binDir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0755); err != nil {
		t.Fatalf("failed to create artifacts dir: %v", err)
	}

	commonEnv := append(os.Environ(),
		"DATABASE_URL="+dbURL,
		"CHECKER_DATABASE_URL="+checkerDBURL,
		"REDIS_URL="+redisURL,
		"LEDGER_BASE_URL=http://127.0.0.1:8082",
		"API_BASE_URL=http://127.0.0.1:8080",
		"TENANT_KEY="+getTenantAPIKey(),
		"STEADFAST_ENV=demo",
		"BASELINE_PATTERN=visibility_timeout",
		"BASELINE_VISIBILITY_SECONDS=10",
		"BASELINE_SEND_IDEMPOTENCY_KEY=false",
	)

	// 1. Start Ledger on 8082
	ledgerEnv := append(commonEnv, "LEDGER_PORT=8082", "LEDGER_DB_PATH="+filepath.Join(binDir, "ledger.db"))
	cmdLedger := exec.Command(binLedger)
	cmdLedger.Env = ledgerEnv
	if err := cmdLedger.Start(); err != nil {
		t.Fatalf("failed to start ledger: %v", err)
	}
	defer func() {
		_ = cmdLedger.Process.Kill()
		_ = cmdLedger.Wait()
	}()

	// 2. Start API on 8080
	apiEnv := append(commonEnv, "PORT=8080")
	cmdAPI := exec.Command(binAPI)
	cmdAPI.Env = apiEnv
	if err := cmdAPI.Start(); err != nil {
		t.Fatalf("failed to start api: %v", err)
	}
	defer func() {
		_ = cmdAPI.Process.Kill()
		_ = cmdAPI.Wait()
	}()

	// 3. Start Baseline API on 8081
	bapiEnv := append(commonEnv, "PORT=8081")
	cmdBAPI := exec.Command(binBaselineAPI)
	cmdBAPI.Env = bapiEnv
	if err := cmdBAPI.Start(); err != nil {
		t.Fatalf("failed to start baseline-api: %v", err)
	}
	defer func() {
		_ = cmdBAPI.Process.Kill()
		_ = cmdBAPI.Wait()
	}()

	if err := waitForHTTP("http://127.0.0.1:8082/export", 5*time.Second); err != nil {
		t.Fatalf("ledger health check failed: %v", err)
	}
	if err := waitForHTTP("http://127.0.0.1:8080/healthz", 5*time.Second); err != nil {
		t.Fatalf("api health check failed: %v", err)
	}
	if err := waitForHTTP("http://127.0.0.1:8081/healthz", 5*time.Second); err != nil {
		t.Fatalf("baseline api health check failed: %v", err)
	}

	// 4. Start 2 Steadfast Workers & Reaper
	cmdW1 := exec.Command(binWorker)
	cmdW1.Env = commonEnv
	_ = cmdW1.Start()
	defer func() { _ = cmdW1.Process.Kill(); _ = cmdW1.Wait() }()

	cmdW2 := exec.Command(binWorker)
	cmdW2.Env = commonEnv
	_ = cmdW2.Start()
	defer func() { _ = cmdW2.Process.Kill(); _ = cmdW2.Wait() }()

	cmdReaper := exec.Command(binReaper)
	cmdReaper.Env = commonEnv
	_ = cmdReaper.Start()
	defer func() { _ = cmdReaper.Process.Kill(); _ = cmdReaper.Wait() }()

	// 5. Start 2 Baseline Workers
	cmdBW1 := exec.Command(binBaselineWorker)
	cmdBW1.Env = commonEnv
	_ = cmdBW1.Start()
	defer func() { _ = cmdBW1.Process.Kill(); _ = cmdBW1.Wait() }()

	cmdBW2 := exec.Command(binBaselineWorker)
	cmdBW2.Env = commonEnv
	_ = cmdBW2.Start()
	defer func() { _ = cmdBW2.Process.Kill(); _ = cmdBW2.Wait() }()

	t.Log("All background services running. Launching load generator (1,000 jobs per lane)...")

	// 6. Run Loadgen: 1000 jobs both lanes
	cmdLoadgen := exec.Command(binLoadgen,
		"--count", "1000",
		"--lane", "both",
		"--api-url", "http://127.0.0.1:8080",
		"--baseline-api-url", "http://127.0.0.1:8081",
		"--api-key", getTenantAPIKey(),
		"--artifacts-dir", artifactsDir,
		"--rate", "0",
	)
	cmdLoadgen.Env = commonEnv
	loadgenOut, err := cmdLoadgen.CombinedOutput()
	if err != nil {
		t.Fatalf("loadgen failed: %v\nOutput: %s", err, string(loadgenOut))
	}
	t.Logf("Loadgen output:\n%s", string(loadgenOut))

	// 7. Wait for drain in PostgreSQL and Redis
	t.Log("Waiting for jobs to drain...")
	db, _ := sql.Open("pgx", dbURL)
	defer func() { _ = db.Close() }()

	opt, _ := redis.ParseURL(redisURL)
	rdb := redis.NewClient(opt)
	defer func() { _ = rdb.Close() }()

	deadline := time.Now().Add(60 * time.Second)
	drained := false
	for time.Now().Before(deadline) {
		var nonterminal, total int
		_ = db.QueryRow("SELECT count(*) FROM jobs WHERE state NOT IN ('SUCCEEDED', 'DEAD')").Scan(&nonterminal)
		_ = db.QueryRow("SELECT count(*) FROM jobs").Scan(&total)
		rq := rdb.LLen(context.Background(), "baseline:queue").Val() + rdb.LLen(context.Background(), "baseline:processing").Val()

		if total >= 1000 && nonterminal == 0 && rq == 0 {
			drained = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !drained {
		t.Fatal("Timeout waiting for jobs to drain in PostgreSQL and Redis")
	}
	t.Log("Drained cleanly! Running independent verifier...")

	// 8. Run Verifier
	cmdVerifier := exec.Command(binVerifier,
		"--artifacts-dir", artifactsDir,
		"--ledger-url", "http://127.0.0.1:8082",
		"--db-url", checkerDBURL,
		"--redis-url", redisURL,
		"--drain-timeout", "10s",
	)
	var stdout, stderr bytes.Buffer
	cmdVerifier.Stdout = &stdout
	cmdVerifier.Stderr = &stderr
	vErr := cmdVerifier.Run()
	t.Logf("Verifier Stdout:\n%s", stdout.String())
	t.Logf("Verifier Stderr:\n%s", stderr.String())

	if vErr != nil {
		t.Fatalf("Verifier failed unexpectedly: %v", vErr)
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "Final Verdict: PASS (Exit Code: 0)") {
		t.Errorf("expected Final Verdict: PASS (Exit Code: 0), got:\n%s", outStr)
	}
	verdictPath := filepath.Join(artifactsDir, "verdict.json")
	data, err := os.ReadFile(verdictPath)
	if err != nil {
		t.Fatalf("failed to read verdict.json: %v", err)
	}
	var vData struct {
		Status    string `json:"status"`
		ExitCode  int    `json:"exit_code"`
		Steadfast struct {
			Invariants map[string]struct {
				Status string `json:"status"`
			} `json:"invariants"`
		} `json:"steadfast"`
	}
	if err := json.Unmarshal(data, &vData); err != nil {
		t.Fatalf("failed to parse verdict.json: %v", err)
	}
	if vData.Status != "PASS" || vData.ExitCode != 0 {
		t.Errorf("expected verdict to be PASS (0), got status=%s exit_code=%d", vData.Status, vData.ExitCode)
	}
	for name, inv := range vData.Steadfast.Invariants {
		if inv.Status != "PASS" {
			t.Errorf("expected invariant %s to be PASS, got %s", name, inv.Status)
		}
	}
}

func TestE2E_WorkerKillMidRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e integration test in short mode")
	}

	testDB, err := sql.Open("pgx", dbURL)
	if err != nil || testDB.Ping() != nil {
		t.Skip("PostgreSQL not available at 127.0.0.1:5432, skipping e2e test")
	}
	_ = testDB.Close()

	binDir := t.TempDir()
	binLedger := filepath.Join(binDir, "ledger")
	binAPI := filepath.Join(binDir, "api")
	binWorker := filepath.Join(binDir, "worker-example")
	binReaper := filepath.Join(binDir, "reaper")
	binLoadgen := filepath.Join(binDir, "loadgen")
	binVerifier := filepath.Join(binDir, "verifier")

	buildBinary(t, "steadfast/cmd/ledger", binLedger)
	buildBinary(t, "steadfast/cmd/api", binAPI)
	buildBinary(t, "steadfast/cmd/worker-example", binWorker)
	buildBinary(t, "steadfast/cmd/reaper", binReaper)
	buildBinary(t, "steadfast/cmd/loadgen", binLoadgen)
	buildBinary(t, "steadfast/cmd/verifier", binVerifier)

	resetDBAndRedis(t)

	artifactsDir := filepath.Join(binDir, "artifacts")
	_ = os.MkdirAll(artifactsDir, 0755)

	commonEnv := append(os.Environ(),
		"DATABASE_URL="+dbURL,
		"CHECKER_DATABASE_URL="+checkerDBURL,
		"LEDGER_BASE_URL=http://127.0.0.1:8082",
		"API_BASE_URL=http://127.0.0.1:8080",
		"TENANT_KEY="+getTenantAPIKey(),
		"STEADFAST_ENV=demo",
		"LEASE_SECONDS=3",
		"HEARTBEAT_SECONDS=1",
		"REAPER_INTERVAL_MS=500",
	)

	// 1. Start Ledger on 8082
	ledgerEnv := append(commonEnv, "LEDGER_PORT=8082", "LEDGER_DB_PATH="+filepath.Join(binDir, "ledger.db"))
	cmdLedger := exec.Command(binLedger)
	cmdLedger.Env = ledgerEnv
	_ = cmdLedger.Start()
	defer func() { _ = cmdLedger.Process.Kill(); _ = cmdLedger.Wait() }()

	// 2. Start API on 8080
	apiEnv := append(commonEnv, "PORT=8080")
	cmdAPI := exec.Command(binAPI)
	cmdAPI.Env = apiEnv
	_ = cmdAPI.Start()
	defer func() { _ = cmdAPI.Process.Kill(); _ = cmdAPI.Wait() }()

	_ = waitForHTTP("http://127.0.0.1:8082/export", 5*time.Second)
	_ = waitForHTTP("http://127.0.0.1:8080/healthz", 5*time.Second)

	// 3. Start Reaper
	cmdReaper := exec.Command(binReaper)
	cmdReaper.Env = commonEnv
	_ = cmdReaper.Start()
	defer func() { _ = cmdReaper.Process.Kill(); _ = cmdReaper.Wait() }()

	// 4. Start Worker 1
	cmdW1 := exec.Command(binWorker)
	cmdW1.Env = commonEnv
	_ = cmdW1.Start()

	// 5. Run Loadgen (200 jobs) at rate 40
	cmdLoadgen := exec.Command(binLoadgen,
		"--count", "200",
		"--lane", "steadfast",
		"--api-url", "http://127.0.0.1:8080",
		"--api-key", getTenantAPIKey(),
		"--artifacts-dir", artifactsDir,
		"--rate", "40",
	)
	cmdLoadgen.Env = commonEnv
	_ = cmdLoadgen.Start()

	// Wait 1.5 seconds, then KILL Worker 1 mid-run
	time.Sleep(1500 * time.Millisecond)
	t.Log("KILLING WORKER 1 MID-RUN (SIGKILL)...")
	_ = cmdW1.Process.Kill()
	_ = cmdW1.Wait()

	// Spawn replacement Worker 2
	t.Log("Spawning replacement Worker 2...")
	cmdW2 := exec.Command(binWorker)
	cmdW2.Env = commonEnv
	_ = cmdW2.Start()
	defer func() { _ = cmdW2.Process.Kill(); _ = cmdW2.Wait() }()

	// Wait for loadgen to complete
	if err := cmdLoadgen.Wait(); err != nil {
		t.Fatalf("loadgen failed: %v", err)
	}

	// Wait for drain
	db, _ := sql.Open("pgx", dbURL)
	defer func() { _ = db.Close() }()

	deadline := time.Now().Add(30 * time.Second)
	drained := false
	for time.Now().Before(deadline) {
		var nonterminal, total int
		_ = db.QueryRow("SELECT count(*) FROM jobs WHERE state NOT IN ('SUCCEEDED', 'DEAD')").Scan(&nonterminal)
		_ = db.QueryRow("SELECT count(*) FROM jobs").Scan(&total)
		if total >= 200 && nonterminal == 0 {
			drained = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !drained {
		t.Fatal("Timeout waiting for jobs to drain after worker kill")
	}

	// Run Verifier
	cmdVerifier := exec.Command(binVerifier,
		"--artifacts-dir", artifactsDir,
		"--ledger-url", "http://127.0.0.1:8082",
		"--db-url", checkerDBURL,
		"--drain-timeout", "10s",
	)
	var stdout, stderr bytes.Buffer
	cmdVerifier.Stdout = &stdout
	cmdVerifier.Stderr = &stderr
	vErr := cmdVerifier.Run()
	t.Logf("Worker kill verifier stdout:\n%s", stdout.String())
	t.Logf("Worker kill verifier stderr:\n%s", stderr.String())

	if vErr != nil {
		t.Fatalf("Verifier failed after worker kill: %v", vErr)
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "Final Verdict: PASS (Exit Code: 0)") {
		t.Errorf("expected Final Verdict: PASS after worker kill, got:\n%s", outStr)
	}
	verdictPath := filepath.Join(artifactsDir, "verdict.json")
	data, err := os.ReadFile(verdictPath)
	if err != nil {
		t.Fatalf("failed to read verdict.json: %v", err)
	}
	var vData struct {
		Status    string `json:"status"`
		ExitCode  int    `json:"exit_code"`
		Steadfast struct {
			Invariants map[string]struct {
				Status string `json:"status"`
			} `json:"invariants"`
		} `json:"steadfast"`
	}
	if err := json.Unmarshal(data, &vData); err != nil {
		t.Fatalf("failed to parse verdict.json: %v", err)
	}
	if vData.Status != "PASS" || vData.ExitCode != 0 {
		t.Errorf("expected verdict to be PASS (0) after worker kill, got status=%s exit_code=%d", vData.Status, vData.ExitCode)
	}
	for name, inv := range vData.Steadfast.Invariants {
		if inv.Status != "PASS" {
			t.Errorf("expected invariant %s to be PASS after worker kill, got %s", name, inv.Status)
		}
	}
}
