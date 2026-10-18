package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func buildCtlBinary(t *testing.T) string {
	t.Helper()
	tempDir := t.TempDir()
	binPath := filepath.Join(tempDir, "ctl")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to build ctl binary: %v\nOutput: %s", err, string(out))
	}
	return binPath
}

func TestCtlUsage(t *testing.T) {
	bin := buildCtlBinary(t)
	cmd := exec.Command(bin, "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ctl --help failed: %v\nOutput: %s", err, string(out))
	}

	expectedSubstrings := []string{
		"Steadfast Control CLI (ctl)",
		"up", "down", "clean", "test", "test-durability", "lint", "scan", "migrate", "seed",
	}

	for _, sub := range expectedSubstrings {
		if !strings.Contains(string(out), sub) {
			t.Errorf("expected usage output to contain %q, but it did not", sub)
		}
	}
}

func TestCtlUnimplementedSubcommand(t *testing.T) {
	bin := buildCtlBinary(t)
	subcommands := []string{"demo", "demo-short", "chaos", "verdict", "bench", "rehearse", "mutation-check"}
	for _, sub := range subcommands {
		cmd := exec.Command(bin, sub)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("expected subcommand %q to exit with error, but it succeeded", sub)
		}
		if !strings.Contains(string(out), "is not implemented yet") {
			t.Errorf("expected output to mention 'is not implemented yet' for %q, got: %s", sub, string(out))
		}
	}
}

func TestCtlUpRefusesMissingEnv(t *testing.T) {
	bin := buildCtlBinary(t)
	tempDir := t.TempDir()
	cmd := exec.Command(bin, "up")
	cmd.Dir = tempDir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected ctl up to fail when .env is missing, but it succeeded")
	}
	if !strings.Contains(string(out), ".env file is missing") {
		t.Fatalf("expected error output about missing .env, got: %s", string(out))
	}
}
