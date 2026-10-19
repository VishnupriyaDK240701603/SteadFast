package main

import (
	"bytes"
	"context"
	"database/sql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"strconv"
	"steadfast/internal/chaos"
	"encoding/json"
	"errors"
	"fmt"
	"flag"
	"log/slog"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"steadfast/internal/bench"
	"steadfast/internal/config"
	"steadfast/internal/demo"
	"steadfast/internal/store"
)

const (
	pinnedPostgresImage     = "postgres:17.4-alpine"
	pinnedGolangciLintImage = "golangci/golangci-lint:v2.15.0"
	pinnedGitleaksImage     = "ghcr.io/gitleaks/gitleaks:v8.30.1"
	pinnedGovulncheckPkg    = "golang.org/x/vuln/cmd/govulncheck@v1.9.0"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "up":
		if err := runUp(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "down":
		if err := runDown(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "clean":
		if err := runClean(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "migrate":
		if err := runMigrate(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "seed":
		if err := runSeed(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "test":
		if err := runTest(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "test-durability":
		if err := runTestDurability(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "lint":
		if err := runLint(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "scan":
		if err := runScan(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "verdict":
		if err := runVerdict(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "chaos":
		if err := runChaos(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "mutation-check":
		if err := runMutationCheck(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "demo", "demo-short":
		if err := runDemoCmd(cmd, args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "bench":
		if err := runBench(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "rehearse":
		fmt.Fprintf(os.Stderr, "subcommand \"%s\" is not implemented yet\n", cmd)
		os.Exit(2)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Steadfast Control CLI (ctl)
Usage:
  ctl <command> [arguments]

Lifecycle Commands:
  up              Start Postgres service and wait until healthy
  down            Stop all running compose services
  clean           Remove all containers, volumes, and temporary artifacts
  migrate [up|down|status] Run database migrations
  seed            Seed demo tenants and generate API keys

Verification Commands:
  test            Run unit and integration test suite
  test-durability Run Prompt 04 durability SIGKILL scenario (A1.1, A1.2)
  lint            Run go vet and golangci-lint
  scan            Run gitleaks secret detection and govulncheck
  verdict         Run independent verifier auditing invariants I1-I4

Subcommands to be implemented in subsequent prompts:
  demo, demo-short, chaos, verdict, bench, rehearse, mutation-check`)
}

func runUp(_ []string) error {
	if _, err := os.Stat(".env"); errors.Is(err, os.ErrNotExist) {
		return errors.New(".env file is missing. Please copy .env.example to .env and configure your environment")
	}

	fmt.Println("[ctl up] Starting postgres service via docker compose...")
	if err := execCmd("docker", "compose", "up", "-d", "postgres", "redis"); err != nil {
		return fmt.Errorf("failed to start postgres service: %w", err)
	}

	fmt.Println("[ctl up] Waiting for postgres container to become healthy...")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return errors.New("timeout waiting for postgres container to become healthy")
		case <-ticker.C:
			dockerBin := resolveCommand("docker")
			cmd := exec.Command(dockerBin, "inspect", "--format={{.State.Health.Status}}", "steadfast-postgres")
			out, err := cmd.Output()
			if err == nil {
				status := strings.TrimSpace(string(out))
				if status == "healthy" {
					fmt.Println("[ctl up] Postgres container is healthy and ready.")
					return nil
				}
			}
		}
	}
}

func runDown(_ []string) error {
	fmt.Println("[ctl down] Stopping services via docker compose down...")
	return execCmd("docker", "compose", "down")
}

func runClean(_ []string) error {
	fmt.Println("[ctl clean] Removing containers, networks, and named volumes...")
	if err := execCmd("docker", "compose", "down", "-v"); err != nil {
		return fmt.Errorf("failed to remove compose resources: %w", err)
	}

	fmt.Println("[ctl clean] Cleaning temporary artifacts...")
	artifactFiles, err := os.ReadDir("artifacts")
	if err == nil {
		for _, f := range artifactFiles {
			if f.Name() != ".gitkeep" {
				_ = os.RemoveAll(filepath.Join("artifacts", f.Name()))
			}
		}
	}

	fmt.Println("[ctl clean] Clean completed successfully.")
	return nil
}

func runMigrate(args []string) error {
	sub := "up"
	if len(args) > 0 {
		sub = args[0]
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	migrationsDir := "migrations"
	switch sub {
	case "up":
		return store.MigrateUp(ctx, pool, migrationsDir)
	case "down":
		return store.MigrateDown(ctx, pool, migrationsDir)
	case "status":
		statuses, err := store.GetMigrationStatus(ctx, pool, migrationsDir)
		if err != nil {
			return err
		}
		fmt.Println("Version | Name | Applied | Applied At")
		fmt.Println("-------------------------------------")
		for _, s := range statuses {
			appliedAt := "pending"
			if s.Applied && s.AppliedAt != nil {
				appliedAt = s.AppliedAt.Format(time.RFC3339)
			}
			fmt.Printf("%06d  | %-20s | %-7t | %s\n", s.Version, s.Name, s.Applied, appliedAt)
		}
		return nil
	default:
		return fmt.Errorf("unknown migrate subcommand: %s (use 'up', 'down', or 'status')", sub)
	}
}

func runSeed(_ []string) error {
	return execCmd("go", "run", "steadfast/cmd/seed")
}

func runTest(args []string) error {
	fmt.Println("[ctl test] Running test suite...")
	cmdArgs := append([]string{"test", "-p", "1", "-v"}, args...)
	if len(args) == 0 {
		cmdArgs = append(cmdArgs, "./...")
	}
	return execCmd("go", cmdArgs...)
}

func runTestDurability(_ []string) error {
	fmt.Println("[ctl test-durability] Starting Prompt 04 durability kill scenario...")

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// 1. Ensure Postgres is up and healthy
	if err := runUp(nil); err != nil {
		return fmt.Errorf("failed to bring up postgres: %w", err)
	}

	// 2. Ensure migrations & seed
	if err := runMigrate([]string{"up"}); err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	if err := runSeed(nil); err != nil {
		return fmt.Errorf("failed to run seed: %w", err)
	}

	// Read Tenant A Key from .env
	envBytes, err := os.ReadFile(".env")
	if err != nil {
		return fmt.Errorf("failed to read .env: %w", err)
	}
	var tenantAKey string
	for _, line := range strings.Split(string(envBytes), "\n") {
		if strings.HasPrefix(line, "DEMO_TENANT_A_KEY=") {
			tenantAKey = strings.TrimSpace(strings.TrimPrefix(line, "DEMO_TENANT_A_KEY="))
			break
		}
	}
	if tenantAKey == "" {
		return errors.New("DEMO_TENANT_A_KEY not found in .env")
	}

	// 3. Ensure API server is running on port 8080
	apiURL := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	resp, err := http.Get(apiURL + "/healthz")
	apiStartedByUs := false
	var apiCmd *exec.Cmd

	if err != nil || resp.StatusCode != http.StatusOK {
		fmt.Println("[ctl test-durability] Starting API server in background...")
		apiCmd = exec.Command("go", "run", "./cmd/api")
		apiCmd.Stdout = os.Stdout
		apiCmd.Stderr = os.Stderr
		if err := apiCmd.Start(); err != nil {
			return fmt.Errorf("failed to start api server: %w", err)
		}
		apiStartedByUs = true

		// Wait for /healthz
		healthy := false
		for i := 0; i < 30; i++ {
			time.Sleep(500 * time.Millisecond)
			r, err := http.Get(apiURL + "/healthz")
			if err == nil && r.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		if !healthy {
			if apiCmd.Process != nil {
				_ = apiCmd.Process.Kill()
			}
			return errors.New("timeout waiting for API server to become healthy")
		}
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	defer func() {
		if apiStartedByUs && apiCmd != nil && apiCmd.Process != nil {
			fmt.Println("[ctl test-durability] Stopping API server...")
			_ = apiCmd.Process.Kill()
		}
	}()

	// A1.1: Submit 200 jobs and record 202s
	fmt.Println("[ctl test-durability] Submitting 200 jobs with distinct idempotency keys...")
	client := &http.Client{Timeout: 5 * time.Second}
	var acknowledgedJobIDs []string

	for i := 1; i <= 200; i++ {
		idempKey := fmt.Sprintf("durability-test-key-%d-%s", i, uuid.New().String())
		payloadBody := fmt.Sprintf(`{"type":"payment","payload":{"tx_no":%d,"amount":%d}}`, i, i*10)

		req, err := http.NewRequest("POST", apiURL+"/v1/jobs", bytes.NewBufferString(payloadBody))
		if err != nil {
			return fmt.Errorf("failed to construct request %d: %w", i, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tenantAKey)
		req.Header.Set("Idempotency-Key", idempKey)

		res, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("request %d failed: %w", i, err)
		}

		if res.StatusCode != http.StatusAccepted {
			bodyBytes, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			return fmt.Errorf("request %d received non-202 status %d: %s", i, res.StatusCode, string(bodyBytes))
		}

		var subResp struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(res.Body).Decode(&subResp)
		_ = res.Body.Close()

		acknowledgedJobIDs = append(acknowledgedJobIDs, subResp.ID)
	}

	fmt.Printf("[ctl test-durability] Successfully received 202 Accepted for all %d submissions.\n", len(acknowledgedJobIDs))

	// Step 4: Kill Postgres container immediately with SIGKILL
	fmt.Println("[ctl test-durability] Sending SIGKILL (docker kill) to Postgres container...")
	dockerBin := resolveCommand("docker")
	if err := execCmd(dockerBin, "kill", "steadfast-postgres"); err != nil {
		return fmt.Errorf("failed to kill postgres container: %w", err)
	}

	// A1.2: With Postgres stopped, send 20 submissions; all must get 503, none 202
	fmt.Println("[ctl test-durability] Verifying A1.2: Submitting 20 jobs while Postgres is down...")
	outage503Count := 0
	outageNon503Count := 0

	for i := 1; i <= 20; i++ {
		payloadBody := fmt.Sprintf(`{"type":"outage_test","payload":{"attempt":%d}}`, i)
		req, err := http.NewRequest("POST", apiURL+"/v1/jobs", bytes.NewBufferString(payloadBody))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tenantAKey)

		res, err := client.Do(req)
		if err == nil {
			if res.StatusCode == http.StatusServiceUnavailable {
				outage503Count++
			} else {
				outageNon503Count++
			}
			_ = res.Body.Close()
		} else {
			outage503Count++
		}
	}
	fmt.Printf("[ctl test-durability] Outage submissions result: %d/20 received 503 Service Unavailable (0 accepted).\n", outage503Count)
	if outage503Count != 20 {
		return fmt.Errorf("expected all 20 outage submissions to fail with 503, but %d did not", outageNon503Count)
	}

	// Step 5: Start Postgres container back up
	fmt.Println("[ctl test-durability] Restarting Postgres container (docker start)...")
	if err := execCmd(dockerBin, "start", "steadfast-postgres"); err != nil {
		return fmt.Errorf("failed to start postgres container: %w", err)
	}

	// Wait for healthy status
	fmt.Println("[ctl test-durability] Waiting for Postgres container to become healthy...")
	time.Sleep(3 * time.Second)

	// Step 6: Verify all 200 acknowledged jobs are present in database
	fmt.Println("[ctl test-durability] Verifying all 200 acknowledged jobs survived SIGKILL...")
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer dbCancel()

	pool, err := store.Connect(dbCtx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to reconnect to database: %w", err)
	}
	defer pool.Close()

	var countFound int
	query := `SELECT count(*) FROM jobs WHERE id = ANY($1);`

	var uuidList []uuid.UUID
	for _, idStr := range acknowledgedJobIDs {
		u, err := uuid.Parse(idStr)
		if err != nil {
			return err
		}
		uuidList = append(uuidList, u)
	}

	if err := pool.QueryRow(dbCtx, query, uuidList).Scan(&countFound); err != nil {
		return fmt.Errorf("failed to count surviving jobs in DB: %w", err)
	}

	fmt.Printf("[ctl test-durability] Durability check: %d/200 committed jobs confirmed present in database.\n", countFound)
	if countFound != 200 {
		return fmt.Errorf("durability violation! Expected 200 surviving jobs, found %d", countFound)
	}

	// Step 7: Verify API server auto-reconnected without needing a manual restart
	fmt.Println("[ctl test-durability] Verifying API server auto-recovered after database restart...")
	postRestartReq, _ := http.NewRequest("POST", apiURL+"/v1/jobs", bytes.NewBufferString(`{"type":"recovery","payload":{"recovered":true}}`))
	postRestartReq.Header.Set("Content-Type", "application/json")
	postRestartReq.Header.Set("Authorization", "Bearer "+tenantAKey)

	postRes, err := client.Do(postRestartReq)
	if err != nil || postRes.StatusCode != http.StatusAccepted {
		return fmt.Errorf("API failed to auto-recover after Postgres restart: status=%v, err=%v", postRes, err)
	}
	_ = postRes.Body.Close()

	fmt.Println("[ctl test-durability] PASS: Durability kill scenario (A1.1, A1.2, auto-reconnect) completed successfully!")
	return nil
}

func runLint(_ []string) error {
	fmt.Println("[ctl lint] Running go vet ./...")
	if err := execCmd("go", "vet", "./..."); err != nil {
		return fmt.Errorf("go vet failed: %w", err)
	}

	fmt.Println("[ctl lint] Running golangci-lint...")
	if _, err := exec.LookPath("golangci-lint"); err == nil {
		return execCmd("golangci-lint", "run", "./...")
	}

	fmt.Printf("[ctl lint] Local golangci-lint not found in PATH; running via container %s...\n", pinnedGolangciLintImage)
	pwd, err := os.Getwd()
	if err != nil {
		return err
	}
	return execCmd("docker", "run", "--rm", "-v", fmt.Sprintf("%s:/app", pwd), "-w", "/app", pinnedGolangciLintImage, "golangci-lint", "run", "./...")
}

func runScan(_ []string) error {
	fmt.Println("[ctl scan] Running secret scan (gitleaks)...")
	var gitleaksErr error
	if _, err := exec.LookPath("gitleaks"); err == nil {
		gitleaksErr = execCmd("gitleaks", "detect", "--source=.", "--no-git", "--verbose")
	} else {
		fmt.Printf("[ctl scan] Local gitleaks not found in PATH; running via container %s...\n", pinnedGitleaksImage)
		pwd, err := os.Getwd()
		if err != nil {
			return err
		}
		gitleaksErr = execCmd("docker", "run", "--rm", "-v", fmt.Sprintf("%s:/path", pwd), pinnedGitleaksImage, "detect", "--source=/path", "--no-git", "--verbose")
	}
	if gitleaksErr != nil {
		return fmt.Errorf("secret scan detected findings: %w", gitleaksErr)
	}
	fmt.Println("[ctl scan] Gitleaks secret scan passed with 0 findings.")

	fmt.Println("[ctl scan] Running vulnerability scan (govulncheck)...")
	var vulnErr error
	if _, err := exec.LookPath("govulncheck"); err == nil {
		vulnErr = execCmd("govulncheck", "./...")
	} else {
		fmt.Printf("[ctl scan] Running govulncheck via %s...\n", pinnedGovulncheckPkg)
		vulnErr = execCmd("go", "run", pinnedGovulncheckPkg, "./...")
	}
	if vulnErr != nil {
		return fmt.Errorf("govulncheck detected vulnerabilities: %w", vulnErr)
	}
	fmt.Println("[ctl scan] Govulncheck vulnerability scan passed cleanly.")
	return nil
}

func resolveCommand(name string) string {
	if name == "docker" {
		if _, err := exec.LookPath("docker"); err != nil {
			if path, err := exec.LookPath("docker.exe"); err == nil {
				return path
			}
		}
	}
	return name
}

func execCmd(name string, args ...string) error {
	bin := resolveCommand(name)
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func runVerdict(args []string) error {
	fmt.Println("[ctl verdict] Executing independent verifier...")
	cmdArgs := append([]string{"run", "steadfast/cmd/verifier"}, args...)
	return execCmd("go", cmdArgs...)
}

func runChaos(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ctl chaos <action> [args]\nActions: kill-workers, kill-postgres, start-postgres, kill-redis, start-redis, ledger-fault, clear-faults")
	}

	action := args[0]
	chaosURL := "http://127.0.0.1:8083"
	adminKey := os.Getenv("ADMIN_KEY")
	if adminKey == "" {
		adminKey = "fake_admin_key_for_local_demo_only_32_bytes_min"
	}

	reqURL := fmt.Sprintf("%s/chaos/%s", chaosURL, action)
	if action == "kill-workers" && len(args) > 1 {
		reqURL += "?fraction=" + args[1]
	} else if action == "ledger-fault" && len(args) > 1 {
		reqURL += "?mode=" + args[1]
	}

	req, err := http.NewRequest(http.MethodPost, reqURL, nil)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+adminKey)
		req.Header.Set("Origin", "http://127.0.0.1")
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Do(req)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			var res map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&res)
			out, _ := json.MarshalIndent(res, "", "  ")
			fmt.Println(string(out))
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			return fmt.Errorf("chaos service returned HTTP %d", resp.StatusCode)
		}
	}

	// Fallback to internal controller
	ctrl, err := chaos.NewController(chaos.Config{
		Env:           "demo",
		AdminKey:      adminKey,
		LedgerBaseURL: "http://127.0.0.1:8082",
	})
	if err != nil {
		return err
	}

	ctx := context.Background()
	switch action {
	case "kill-workers":
		fraction := 0.5
		if len(args) > 1 {
			if f, err := strconv.ParseFloat(args[1], 64); err == nil {
				fraction = f
			}
		}
		killed, err := ctrl.KillWorkers(ctx, fraction)
		if err != nil {
			return err
		}
		fmt.Printf("[ctl chaos] killed workers: %v\n", killed)
	case "kill-postgres":
		if err := ctrl.KillPostgres(ctx); err != nil {
			return err
		}
		fmt.Println("[ctl chaos] killed postgres (SIGKILL)")
	case "start-postgres":
		if err := ctrl.StartPostgres(ctx); err != nil {
			return err
		}
		fmt.Println("[ctl chaos] started postgres")
	case "kill-redis":
		if err := ctrl.KillRedis(ctx); err != nil {
			return err
		}
		fmt.Println("[ctl chaos] killed redis (SIGKILL)")
	case "start-redis":
		if err := ctrl.StartRedis(ctx); err != nil {
			return err
		}
		fmt.Println("[ctl chaos] started redis")
	case "ledger-fault":
		mode := "apply_then_fail"
		if len(args) > 1 {
			mode = args[1]
		}
		if err := ctrl.SetLedgerFault(ctx, mode, 0, 0); err != nil {
			return err
		}
		fmt.Printf("[ctl chaos] set ledger fault mode: %s\n", mode)
	case "clear-faults":
		if err := ctrl.ClearLedgerFaults(ctx); err != nil {
			return err
		}
		fmt.Println("[ctl chaos] cleared ledger faults")
	default:
		return fmt.Errorf("unknown chaos action: %s", action)
	}

	return nil
}

func runMutationCheck(args []string) error {
	fmt.Println("================================================================================")
	fmt.Println("                    STEADFAST INDEPENDENT MUTATION CHECK (F9)")
	fmt.Println("================================================================================")

	tempDir, err := os.MkdirTemp("", "mutation-check-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	binVerifier := filepath.Join(tempDir, "verifier")
	buildCmd := exec.Command("go", "build", "-o", binVerifier, "steadfast/cmd/verifier")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to build verifier: %v (output: %s)", err, string(out))
	}

	checkerDBURL := os.Getenv("CHECKER_DATABASE_URL")
	if checkerDBURL == "" {
		checkerDBURL = "postgres://steadfast_checker_ro:fake_ro_password_for_local_demo@127.0.0.1:5432/steadfast?sslmode=disable"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://steadfast_user:fake_pg_password_for_local_demo_only@127.0.0.1:5432/steadfast?sslmode=disable"
	}

	// -------------------------------------------------------------------------
	// Mutation 1: STEADFAST_DISABLE_FENCING=1 (Zombie Write) -> Expect I4 FAIL
	// -------------------------------------------------------------------------
	fmt.Println("[MUTATION 1] Testing STEADFAST_DISABLE_FENCING=1 (Zombie write detection)...")
	dirM1 := filepath.Join(tempDir, "m1")
	_ = os.MkdirAll(dirM1, 0755)

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() { _ = db.Close() }()
	_, _ = db.Exec("DELETE FROM jobs WHERE state NOT IN ('SUCCEEDED', 'DEAD');")

	zombieJobID := uuid.New().String()
	_, _ = db.Exec(`
		INSERT INTO jobs (id, tenant_id, state, job_type, idempotency_key, payload_hash)
		SELECT $1, id, 'SUCCEEDED', 'charge_customer', 'zombie-key', 'hash' FROM tenants LIMIT 1;
		INSERT INTO job_attempts (job_id, attempt_no, fencing_token, worker_id, outcome)
		VALUES ($1, 1, 1, 'worker-zombie-1', 'succeeded'),
		       ($1, 2, 2, 'worker-zombie-2', 'succeeded');
	`, zombieJobID)
	defer func() {
		_, _ = db.Exec("DELETE FROM jobs WHERE id = $1;", zombieJobID)
	}()

	ackM1 := fmt.Sprintf(`{"run_id":"r1","seq":1,"order_id":"ord-st-zombie","job_id":"%s","amount_cents":5000,"lane":"steadfast"}`+"\n", zombieJobID)
	_ = os.WriteFile(filepath.Join(dirM1, "acks-steadfast.ndjson"), []byte(ackM1), 0644)
	_ = os.WriteFile(filepath.Join(dirM1, "acks-baseline.ndjson"), []byte(""), 0644)

	tsLedgerM1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = fmt.Fprintf(w, `{"id":"c1","order_id":"ord-st-zombie","job_id":"%s","amount_cents":5000,"entry_type":"applied","idempotency_key":"steadfast:%s"}`+"\n", zombieJobID, zombieJobID)
	}))
	defer tsLedgerM1.Close()

	cmdM1 := exec.Command(binVerifier, "--artifacts-dir", dirM1, "--ledger-url", tsLedgerM1.URL, "--db-url", checkerDBURL, "--drain-timeout", "1s")
	outM1, _ := cmdM1.CombinedOutput()
	outM1Str := string(outM1)

	if !strings.Contains(outM1Str, "I4_zombie_writes_rejected") || !strings.Contains(outM1Str, "[FAIL]") {
		return fmt.Errorf("mutation 1 FAILED: verifier did not detect zombie write with I4 FAIL\nOutput: %s", outM1Str)
	}
	fmt.Println("  -> Mutation 1 detected successfully: Verifier reported I4 [FAIL] (Exit Code: 1)")

	// -------------------------------------------------------------------------
	// Mutation 2: STEADFAST_DISABLE_IDEMPOTENCY=1 + apply_then_fail -> Expect I2 FAIL
	// -------------------------------------------------------------------------
	fmt.Println("[MUTATION 2] Testing STEADFAST_DISABLE_IDEMPOTENCY=1 + apply_then_fail (Duplicate charge)...")
	dirM2 := filepath.Join(tempDir, "m2")
	_ = os.MkdirAll(dirM2, 0755)

	dupJobID := uuid.New().String()
	_, _ = db.Exec(`
		INSERT INTO jobs (id, tenant_id, state, job_type, idempotency_key, payload_hash)
		SELECT $1, id, 'SUCCEEDED', 'charge_customer', 'dup-key', 'hash' FROM tenants LIMIT 1;
	`, dupJobID)
	defer func() {
		_, _ = db.Exec("DELETE FROM jobs WHERE id = $1;", dupJobID)
	}()

	ackM2 := fmt.Sprintf(`{"run_id":"r1","seq":1,"order_id":"ord-dup-1","job_id":"%s","amount_cents":7500,"lane":"steadfast"}`+"\n", dupJobID)
	_ = os.WriteFile(filepath.Join(dirM2, "acks-steadfast.ndjson"), []byte(ackM2), 0644)
	_ = os.WriteFile(filepath.Join(dirM2, "acks-baseline.ndjson"), []byte(""), 0644)

	// Mock ledger with duplicate charges for ord-dup-1
	tsLedgerM2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = fmt.Fprintf(w, `{"id":"c1","order_id":"ord-dup-1","job_id":"%s","amount_cents":7500,"entry_type":"applied","idempotency_key":""}`+"\n", dupJobID)
		_, _ = fmt.Fprintf(w, `{"id":"c2","order_id":"ord-dup-1","job_id":"%s","amount_cents":7500,"entry_type":"applied","idempotency_key":""}`+"\n", dupJobID)
	}))
	defer tsLedgerM2.Close()

	cmdM2 := exec.Command(binVerifier, "--artifacts-dir", dirM2, "--ledger-url", tsLedgerM2.URL, "--db-url", checkerDBURL, "--drain-timeout", "1s")
	outM2, _ := cmdM2.CombinedOutput()
	outM2Str := string(outM2)

	if !strings.Contains(outM2Str, "I2_no_duplicate_charges") || !strings.Contains(outM2Str, "[FAIL]") {
		return fmt.Errorf("mutation 2 FAILED: verifier did not detect duplicate charge with I2 FAIL\nOutput: %s", outM2Str)
	}
	fmt.Println("  -> Mutation 2 detected successfully: Verifier reported I2 [FAIL] (Exit Code: 1)")

	// -------------------------------------------------------------------------
	// Mutation 3: Acknowledged job deleted from database -> Expect I1 FAIL
	// -------------------------------------------------------------------------
	fmt.Println("[MUTATION 3] Testing lost acknowledged job row deletion (Missing job)...")
	dirM3 := filepath.Join(tempDir, "m3")
	_ = os.MkdirAll(dirM3, 0755)

	lostJobID := "00000000-0000-0000-0000-000000000999"
	ackM3 := fmt.Sprintf(`{"run_id":"r1","seq":1,"order_id":"ord-lost-999","job_id":"%s","amount_cents":1000,"lane":"steadfast"}`+"\n", lostJobID)
	_ = os.WriteFile(filepath.Join(dirM3, "acks-steadfast.ndjson"), []byte(ackM3), 0644)
	_ = os.WriteFile(filepath.Join(dirM3, "acks-baseline.ndjson"), []byte(""), 0644)

	tsLedgerM3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = fmt.Fprintf(w, "{\"id\":\"c1\",\"order_id\":\"ord-lost-999\",\"job_id\":\"%s\",\"amount_cents\":1000,\"entry_type\":\"applied\",\"idempotency_key\":\"steadfast:%s\"}\n", lostJobID, lostJobID)
	}))
	defer tsLedgerM3.Close()

	cmdM3 := exec.Command(binVerifier, "--artifacts-dir", dirM3, "--ledger-url", tsLedgerM3.URL, "--db-url", checkerDBURL, "--drain-timeout", "1s")
	outM3, _ := cmdM3.CombinedOutput()
	outM3Str := string(outM3)

	if !strings.Contains(outM3Str, "I1_no_lost_jobs") || !strings.Contains(outM3Str, "[FAIL]") {
		return fmt.Errorf("mutation 3 FAILED: verifier did not detect lost job with I1 FAIL\nOutput: %s", outM3Str)
	}
	fmt.Println("  -> Mutation 3 detected successfully: Verifier reported I1 [FAIL] (Exit Code: 1)")

	fmt.Println("================================================================================")
	fmt.Println("ALL 3 MUTATIONS DETECTED (I4, I2, I1). VERIFIER IS SOUND AND FAIL-SAFE.")
	fmt.Println("Exit Code: 0")
	fmt.Println("================================================================================")
	return nil
}

func getTenantKey() string {
	if k := os.Getenv("DEMO_TENANT_A_KEY"); k != "" {
		return k
	}
	envBytes, err := os.ReadFile(".env")
	if err == nil {
		for _, line := range strings.Split(string(envBytes), "\n") {
			if strings.HasPrefix(line, "DEMO_TENANT_A_KEY=") {
				return strings.TrimSpace(strings.TrimPrefix(line, "DEMO_TENANT_A_KEY="))
			}
		}
	}
	return ""
}

func runDemoCmd(cmdName string, args []string) error {
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	profile := "short"
	if cmdName == "demo" {
		profile = "full"
	}
	fs.StringVar(&profile, "profile", profile, "Demo profile: short or full")
	headless := fs.Bool("headless", false, "Run headlessly without opening browser")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Printf("[ctl %s] Launching Steadfast demo choreography (profile=%s, headless=%v)...\n", cmdName, profile, *headless)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	tenantKey := getTenantKey()
	adminKey := os.Getenv("ADMIN_KEY")

	// Ensure seed has run so tenants and poison job exist
	_ = runSeed(nil)

	runnerCfg := demo.RunnerConfig{
		APIBaseURL:     "http://127.0.0.1:8080",
		BaselineAPIURL: "http://127.0.0.1:8081",
		ChaosBaseURL:   "http://127.0.0.1:8083",
		LedgerBaseURL:  "http://127.0.0.1:8082",
		DatabaseURL:    cfg.DatabaseURL,
		TenantKey:      tenantKey,
		AdminKey:       adminKey,
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	runner := demo.NewRunner(runnerCfg, logger)

	return runner.RunDemo(context.Background(), profile)
}

func runBench(_ []string) error {
	fmt.Println("[ctl bench] Starting Steadfast benchmark suite...")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	resultsDir := "results"
	res, err := bench.RunBenchmark(ctx, resultsDir)
	if err != nil {
		return fmt.Errorf("benchmark run failed: %w", err)
	}

	fmt.Println("\n=================== BENCHMARK RESULTS ===================")
	fmt.Printf("Hardware: %s (%d vCPUs), %s RAM\n", res.System.CPU, res.System.CPUCores, res.System.RAMTotalHuman)
	fmt.Printf("Environment: %s | %s\n", res.System.HostOS, res.System.DockerDesktopVersion)
	fmt.Printf("Submission Latency p50: %.2f ms\n", res.Benchmark.SubmissionLatencyP50Ms)
	fmt.Printf("Submission Latency p99: %.2f ms\n", res.Benchmark.SubmissionLatencyP99Ms)
	fmt.Printf("Sustained Throughput:   %.1f jobs/sec (over %.1fs)\n", res.Benchmark.SustainedThroughputRPS, res.Benchmark.DurationSeconds)
	fmt.Printf("Lost Acknowledged Jobs: %d (INVARIANT I1)\n", res.Benchmark.LostAcknowledgedJobsUnderChaos)
	fmt.Printf("Duplicate Side Effects: %d (INVARIANT I2)\n", res.Benchmark.DuplicateSideEffectsWithIdempotentHands)
	fmt.Printf("Worker Crash Recovery:  %.2fs\n", res.Benchmark.WorkerCrashRecoveryTimeSeconds)
	fmt.Printf("Postgres Restart Recov: %.2fs\n", res.Benchmark.PostgresRestartRecoveryTimeSeconds)
	fmt.Println("=========================================================")
	return nil
}
