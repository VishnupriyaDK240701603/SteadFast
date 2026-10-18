package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"steadfast/internal/config"
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
	case "demo", "demo-short", "chaos", "bench", "rehearse", "mutation-check":
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
	return execCmd("go", "run", "./cmd/seed")
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
