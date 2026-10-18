package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"steadfast/internal/chaos"
)

func TestChaosServer_AuthAndOrigin(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctrl, err := chaos.NewController(chaos.Config{
		Env:           "demo",
		AdminKey:      "secret-admin-key",
		LedgerBaseURL: "http://127.0.0.1:8082",
	})
	if err != nil {
		t.Fatalf("failed to create chaos controller: %v", err)
	}

	server := NewChaosServer(ctrl, "secret-admin-key", logger)
	handler := server.SetupRoutes()

	// 1. Unauthenticated -> 401
	req := httptest.NewRequest(http.MethodPost, "/chaos/clear-faults", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized without token, got %d", rec.Code)
	}

	// 2. Bad Token -> 401
	req = httptest.NewRequest(http.MethodPost, "/chaos/clear-faults", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized with wrong token, got %d", rec.Code)
	}

	// 3. Bad Origin -> 403
	req = httptest.NewRequest(http.MethodPost, "/chaos/clear-faults", nil)
	req.Header.Set("Authorization", "Bearer secret-admin-key")
	req.Header.Set("Origin", "http://attacker.com")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden with bad origin, got %d", rec.Code)
	}

	// 4. Good Origin -> passes middleware
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK for /healthz, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("expected ok body, got %s", rec.Body.String())
	}
}
