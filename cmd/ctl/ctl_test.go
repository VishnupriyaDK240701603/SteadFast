package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func buildCtlBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "ctl_test_bin")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build ctl: %v, output: %s", err, string(out))
	}
	return binPath
}

func TestCtlUsage(t *testing.T) {
	bin := buildCtlBinary(t)
	cmd := exec.Command(bin, "help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ctl help failed: %v, output: %s", err, string(out))
	}
	if len(out) == 0 {
		t.Fatal("expected non-empty usage message")
	}
}

func TestCtlUnimplementedSubcommand(t *testing.T) {
	bin := buildCtlBinary(t)
	subcommands := []string{"seed", "demo", "demo-short", "chaos", "verdict", "bench", "rehearse", "mutation-check"}
	for _, sub := range subcommands {
		cmd := exec.Command(bin, sub)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected command %s to fail with exit code 2, got success", sub)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() != 2 {
				t.Fatalf("expected exit code 2 for %s, got %d (output: %s)", sub, exitErr.ExitCode(), string(out))
			}
		}
	}
}

func TestCtlUpRefusesMissingEnv(t *testing.T) {
	bin := buildCtlBinary(t)
	_ = os.Remove(".env")
	cmd := exec.Command(bin, "up")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected ctl up to fail when .env is missing")
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit code 1 when .env is missing, got %d", exitErr.ExitCode())
		}
	}
	if len(out) == 0 {
		t.Fatal("expected error message when .env is missing")
	}
}
