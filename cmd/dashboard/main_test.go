package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardHealthAndSecurityHeaders(t *testing.T) {
	cfg := Config{
		Port:   "8085",
		WebDir: "../../web/dashboard",
	}
	server := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := server.Routes()

	// 1. Healthz
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /healthz, got %d", rec.Code)
	}

	// Verify Security Headers
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("missing or invalid CSP header: %s", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing nosniff header")
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("missing X-Frame-Options: DENY")
	}
}

func TestDashboardOriginValidation(t *testing.T) {
	cfg := Config{
		Port:   "8085",
		WebDir: "../../web/dashboard",
	}
	server := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := server.Routes()

	// Forbidden origin
	req := httptest.NewRequest("POST", "/api/chaos?action=kill-workers", nil)
	req.Header.Set("Origin", "http://evil-attacker.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for foreign origin, got %d", rec.Code)
	}

	// Permitted local origin
	reqOK := httptest.NewRequest("POST", "/api/chaos?action=kill-workers", nil)
	reqOK.Header.Set("Origin", "http://127.0.0.1:8085")
	recOK := httptest.NewRecorder()
	handler.ServeHTTP(recOK, reqOK)

	if recOK.Code == http.StatusForbidden {
		t.Fatalf("expected permitted local origin not to return 403, got %d", recOK.Code)
	}
}

func TestDashboardStaticBundleNoSecrets(t *testing.T) {
	cfg := Config{
		Port:      "8085",
		WebDir:    "../../web/dashboard",
		AdminKey:  "super-secret-admin-key-123456",
		TenantKey: "super-secret-tenant-key-654321",
	}
	server := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := server.Routes()

	for _, path := range []string{"/", "/style.css", "/app.js"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("failed to serve static path %s: status %d", path, rec.Code)
		}

		body := rec.Body.String()
		if strings.Contains(body, cfg.AdminKey) {
			t.Fatalf("A11.2 violation: admin key found in served bundle for %s", path)
		}
		if strings.Contains(body, cfg.TenantKey) {
			t.Fatalf("A11.2 violation: tenant key found in served bundle for %s", path)
		}
	}
}

func TestDashboardReplayValidation(t *testing.T) {
	cfg := Config{
		Port:   "8085",
		WebDir: "../../web/dashboard",
	}
	server := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := server.Routes()

	// Malformed JSON payload
	body := `{"job_id":"123","payload":"not a json"}`
	req := httptest.NewRequest("POST", "/api/dlq/replay", bytes.NewReader([]byte(body)))
	req.Header.Set("Origin", "http://127.0.0.1:8085")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for malformed JSON payload, got %d: %s", rec.Code, rec.Body.String())
	}
}
