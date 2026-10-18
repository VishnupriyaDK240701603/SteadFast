package chaos

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChaos_RefusesOutsideDemo(t *testing.T) {
	_, err := NewController(Config{Env: "prod"})
	if err == nil {
		t.Fatal("expected error creating chaos controller in prod, got nil")
	}

	_, err = NewController(Config{Env: "demo"})
	if err != nil {
		t.Fatalf("expected success creating chaos controller in demo, got: %v", err)
	}
}

func TestChaos_ContainerAllowlist(t *testing.T) {
	c, err := NewController(Config{Env: "demo"})
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	ctx := context.Background()

	// Allowed names
	allowed := []string{"steadfast-postgres", "steadfast-redis", "steadfast-worker-1", "steadfast-baseline-worker"}
	for _, name := range allowed {
		if err := c.VerifyAllowed(ctx, name); err != nil {
			t.Errorf("expected %q to be allowed, got error: %v", name, err)
		}
	}

	// Disallowed names (should fail without label)
	disallowed := []string{"unrelated-container", "my-database", "evil-container"}
	for _, name := range disallowed {
		if err := c.VerifyAllowed(ctx, name); err == nil {
			t.Errorf("expected %q to be disallowed, got nil error", name)
		}
	}
}

func TestChaos_LedgerFault(t *testing.T) {
	var receivedMode string
	var receivedAuth string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/fault" {
			receivedAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	c, err := NewController(Config{
		Env:           "demo",
		AdminKey:      "test-admin-key",
		LedgerBaseURL: ts.URL,
	})
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	ctx := context.Background()
	if err := c.SetLedgerFault(ctx, "apply_then_fail", 0.5, 100); err != nil {
		t.Fatalf("SetLedgerFault failed: %v", err)
	}
	if receivedAuth != "Bearer test-admin-key" {
		t.Errorf("expected Authorization header Bearer test-admin-key, got %q", receivedAuth)
	}

	if err := c.ClearLedgerFaults(ctx); err != nil {
		t.Fatalf("ClearLedgerFaults failed: %v", err)
	}
	_ = receivedMode
}
