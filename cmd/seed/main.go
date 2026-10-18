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

func updateEnvFile(keyA, keyB string) error {
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

	// Generate key for shop-a
	keyA, err := generateAPIKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating key for shop-a: %v\n", err)
		os.Exit(1)
	}
	hashA := auth.HashAPIKey(keyA)
	tenantA, err := s.CreateTenant(ctx, "shop-a", hashA)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error seeding tenant shop-a: %v\n", err)
		os.Exit(1)
	}

	// Generate key for shop-b
	keyB, err := generateAPIKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating key for shop-b: %v\n", err)
		os.Exit(1)
	}
	hashB := auth.HashAPIKey(keyB)
	tenantB, err := s.CreateTenant(ctx, "shop-b", hashB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error seeding tenant shop-b: %v\n", err)
		os.Exit(1)
	}

	// Update git-ignored .env file with the generated plaintext keys
	if err := updateEnvFile(keyA, keyB); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving keys to .env: %v\n", err)
		os.Exit(1)
	}

	// Security Rule: Never print plaintext API keys to terminal or logs!
	fmt.Printf("[seed] Successfully provisioned tenants shop-a (%s) and shop-b (%s).\n", tenantA.ID, tenantB.ID)
	fmt.Println("[seed] Plaintext keys saved strictly to git-ignored .env file.")
}
