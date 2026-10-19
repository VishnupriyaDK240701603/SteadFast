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
		Port:         "8085",
		WebDir:       "../../web/dashboard",
		AdminKey:     "super-secret-admin-key-123456",
		TenantKey:    "super-secret-tenant-key-654321",
		SteadfastEnv: "demo",
	}
	server := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := server.Routes()

	// Root redirects to /ops
	rootReq := httptest.NewRequest("GET", "/", nil)
	rootRec := httptest.NewRecorder()
	handler.ServeHTTP(rootRec, rootReq)
	if rootRec.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect on /, got %d", rootRec.Code)
	}

	for _, path := range []string{"/demo", "/style.css", "/app.js"} {
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

func TestDashboardEnvDemoGuard(t *testing.T) {
	cfg := Config{
		Port:         "8085",
		WebDir:       "../../web/dashboard",
		SteadfastEnv: "production",
	}
	server := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := server.Routes()

	// 1. /demo should 404 in production
	req := httptest.NewRequest("GET", "/demo", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for /demo in production, got %d", rec.Code)
	}

	// 2. /api/chaos should 404 in production
	reqChaos := httptest.NewRequest("POST", "/api/chaos", nil)
	reqChaos.Header.Set("Origin", "http://127.0.0.1:8085")
	recChaos := httptest.NewRecorder()
	handler.ServeHTTP(recChaos, reqChaos)
	if recChaos.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for /api/chaos in production, got %d", recChaos.Code)
	}
}

func TestDashboardOpsAuthAndSession(t *testing.T) {
	cfg := Config{
		Port:         "8085",
		WebDir:       "../../web/dashboard",
		OpsToken:     "test-valid-ops-token-12345",
		SteadfastEnv: "demo",
	}
	server := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := server.Routes()

	// 1. Unauthenticated GET /ops -> 302 to /ops/login
	req := httptest.NewRequest("GET", "/ops", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect for unauthed /ops, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ops/login" {
		t.Fatalf("expected redirect to /ops/login, got %s", loc)
	}

	// 2. Login with valid token
	loginBody := `{"token":"test-valid-ops-token-12345"}`
	loginReq := httptest.NewRequest("POST", "/ops/login", strings.NewReader(loginBody))
	loginReq.Header.Set("Origin", "http://127.0.0.1:8085")
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, loginReq)

	if loginRec.Code != http.StatusOK && loginRec.Code != http.StatusSeeOther {
		t.Fatalf("expected 200 on login with valid token, got %d", loginRec.Code)
	}

	cookie := loginRec.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookie {
		if c.Name == "steadfast_ops_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatalf("expected steadfast_ops_session cookie set on successful login")
	}

	// 3. Authenticated GET /ops with session cookie
	authedReq := httptest.NewRequest("GET", "/ops", nil)
	authedReq.AddCookie(sessionCookie)
	authedRec := httptest.NewRecorder()
	handler.ServeHTTP(authedRec, authedReq)
	if authedRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for authed /ops, got %d", authedRec.Code)
	}

	body := authedRec.Body.String()
	if strings.Contains(body, "kill-workers") || strings.Contains(body, "kill-postgres") {
		t.Fatalf("A15.5 violation: chaos controls found in /ops view")
	}
}
