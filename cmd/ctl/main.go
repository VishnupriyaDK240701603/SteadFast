package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
	case "demo", "demo-short", "chaos", "verdict", "bench", "rehearse", "mutation-check":
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

Quality and Security Commands:
  test            Run unit and integration test suite
  lint            Run go vet and golangci-lint
  scan            Run gitleaks secret detection and govulncheck

Subcommands to be implemented in subsequent prompts:
  demo, demo-short, chaos, verdict, bench, rehearse, mutation-check`)
}

func runUp(_ []string) error {
	if _, err := os.Stat(".env"); errors.Is(err, os.ErrNotExist) {
		return errors.New(".env file is missing. Please copy .env.example to .env and configure your environment")
	}

	fmt.Println("[ctl up] Starting postgres service via docker compose...")
	if err := execCmd("docker", "compose", "up", "-d", "postgres"); err != nil {
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
		// If docker is not available directly (e.g. in WSL), try docker.exe
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
