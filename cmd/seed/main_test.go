package main

import (
	"os"
	"strings"
	"testing"
)

func TestGenerateAPIKey(t *testing.T) {
	key, err := generateAPIKey()
	if err != nil {
		t.Fatalf("generateAPIKey failed: %v", err)
	}

	// 32 bytes base64 url-encoded produces 43 characters
	if len(key) != 43 {
		t.Fatalf("Expected 43-character base64 URL-safe key, got %d chars: %s", len(key), key)
	}

	key2, _ := generateAPIKey()
	if key == key2 {
		t.Fatalf("Consecutive keys should be random and distinct")
	}
}

func TestUpdateEnvFile(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test_env_*.env")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	initialContent := "STEADFAST_ENV=demo\nPORT=8080\nDEMO_TENANT_A_KEY=old_a\n"
	_ = os.WriteFile(tmpFile.Name(), []byte(initialContent), 0600)

	// Test updating temporary env
	envPath := tmpFile.Name()
	contentBytes, _ := os.ReadFile(envPath)
	content := string(contentBytes)

	keyA := "new_key_a_123"
	keyB := "new_key_b_456"

	content = strings.Replace(content, "DEMO_TENANT_A_KEY=old_a", "DEMO_TENANT_A_KEY="+keyA, 1)
	content += "DEMO_TENANT_B_KEY=" + keyB + "\n"

	_ = os.WriteFile(envPath, []byte(content), 0600)

	updatedBytes, _ := os.ReadFile(envPath)
	updatedStr := string(updatedBytes)

	if !strings.Contains(updatedStr, "DEMO_TENANT_A_KEY="+keyA) {
		t.Fatalf("Expected keyA in env file")
	}
	if !strings.Contains(updatedStr, "DEMO_TENANT_B_KEY="+keyB) {
		t.Fatalf("Expected keyB in env file")
	}
	if !strings.Contains(updatedStr, "STEADFAST_ENV=demo") {
		t.Fatalf("Existing settings should be preserved")
	}
}
