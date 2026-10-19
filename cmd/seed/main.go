package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"steadfast/internal/auth"
	"steadfast/internal/config"
	"steadfast/internal/store"
)

func generateAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func updateEnvFile(keyA, keyB, opsToken string) error {
	envPath := ".env"
	contentBytes, err := os.ReadFile(envPath)
	content := ""
	if err == nil {
		content = string(contentBytes)
	} else if os.IsNotExist(err) {
		exBytes, err := os.ReadFile(".env.example")
		if err == nil {
			content = string(exBytes)
		}
	}

	setOrAppend := func(src, varName, value string) string {
		re := regexp.MustCompile(`(?m)^` + varName + `=.*$`)
		newLine := fmt.Sprintf("%s=%s", varName, value)
		if re.MatchString(src) {
			return re.ReplaceAllString(src, newLine)
		}
		if len(src) > 0 && !strings.HasSuffix(src, "\n") {
			src += "\n"
		}
		return src + newLine + "\n"
	}

	content = setOrAppend(content, "DEMO_TENANT_A_KEY", keyA)
	content = setOrAppend(content, "DEMO_TENANT_B_KEY", keyB)
	content = setOrAppend(content, "OPS_TOKEN", opsToken)

	return os.WriteFile(envPath, []byte(content), 0600)
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading configuration: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Ensure migrations are up before seeding
	if err := store.MigrateUp(ctx, pool, "migrations"); err != nil {
		fmt.Fprintf(os.Stderr, "Error applying migrations: %v\n", err)
		os.Exit(1)
	}

	s := store.NewStore(pool)

	readEnvKey := func(varName string) string {
		if val := os.Getenv(varName); val != "" {
			return val
		}
		if envBytes, err := os.ReadFile(".env"); err == nil {
			for _, line := range strings.Split(string(envBytes), "\n") {
				if strings.HasPrefix(line, varName+"=") {
					return strings.TrimSpace(strings.TrimPrefix(line, varName+"="))
				}
			}
		}
		return ""
	}

	keyA := readEnvKey("DEMO_TENANT_A_KEY")
	if keyA == "" {
		keyA, err = generateAPIKey()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error generating key for shop-a: %v\n", err)
			os.Exit(1)
		}
	}
	hashA := auth.HashAPIKey(keyA)
	tenantA, err := s.CreateTenant(ctx, "shop-a", hashA)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error seeding tenant shop-a: %v\n", err)
		os.Exit(1)
	}

	keyB := readEnvKey("DEMO_TENANT_B_KEY")
	if keyB == "" {
		keyB, err = generateAPIKey()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error generating key for shop-b: %v\n", err)
			os.Exit(1)
		}
	}
	hashB := auth.HashAPIKey(keyB)
	tenantB, err := s.CreateTenant(ctx, "shop-b", hashB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error seeding tenant shop-b: %v\n", err)
		os.Exit(1)
	}

	opsToken := readEnvKey("OPS_TOKEN")
	if opsToken == "" {
		opsToken, err = generateAPIKey()
		if err != nil {
			opsToken = "fake_ops_token_for_local_demo_only"
		}
	}

	// Update git-ignored .env file with the generated plaintext keys
	if err := updateEnvFile(keyA, keyB, opsToken); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving keys to .env: %v\n", err)
		os.Exit(1)
	}

	// Security Rule: Never print plaintext API keys to terminal or logs!
	fmt.Printf("[seed] Successfully provisioned tenants shop-a (%s) and shop-b (%s).\n", tenantA.ID, tenantB.ID)
	fmt.Println("[seed] Plaintext keys saved strictly to git-ignored .env file.")

	// Pre-seed one poison job for demo DLQ triage (A6.1, F11)
	poisonJobID := "11111111-1111-1111-1111-111111111111"
	poisonPayload := `{"order_id":"poison-order-001","customer_id":"cust-dlq","amount_cents":-500}`
	poisonErr := "downstream 4xx error (status 400): amount_cents must be positive"
	hashBytes := auth.HashAPIKey(poisonPayload) // simple 64-char sha256 hex string

	_, err = pool.Exec(ctx, `
		INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, idempotency_key, state, attempts, max_attempts, last_error, created_at, updated_at)
		VALUES ($1, $2, 'order.charge', $3::jsonb, $4, 'poison-job-001', 'DEAD', 1, 5, $5, now(), now())
		ON CONFLICT (id) DO NOTHING;
	`, poisonJobID, tenantA.ID, poisonPayload, hashBytes, poisonErr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to pre-seed poison job: %v\n", err)
	} else {
		_, _ = pool.Exec(ctx, `
			INSERT INTO job_attempts (job_id, attempt_no, fencing_token, worker_id, started_at, finished_at, outcome, error_class, error_message)
			SELECT $1, 1, 1, 'worker-seed', now() - interval '5 seconds', now(), 'failed_permanent', 'invalid_amount', $2
			WHERE NOT EXISTS (SELECT 1 FROM job_attempts WHERE job_id = $1);
		`, poisonJobID, poisonErr)
		fmt.Println("[seed] Pre-seeded 1 poison job in DLQ with error history.")
	}

}
