package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"steadfast/internal/auth"
	"steadfast/internal/config"
	"steadfast/internal/store"
)

func TestRateLimit_TokenBucketPerKey(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("store.Connect failed: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	if err := store.MigrateUp(ctx, pool, "../../migrations"); err != nil {
		t.Fatalf("MigrateUp failed: %v", err)
	}

	s := store.NewStore(pool)

	// Create Tenant A
	keyA := "dyn_key_a_" + uuid.New().String()
	hashA := auth.HashAPIKey(keyA)
	tenantA, err := s.CreateTenant(ctx, "tenant-a-"+uuid.New().String()[:8], hashA)
	if err != nil {
		t.Fatalf("CreateTenant A failed: %v", err)
	}

	// Create Tenant B
	keyB := "dyn_key_b_" + uuid.New().String()
	hashB := auth.HashAPIKey(keyB)
	tenantB, err := s.CreateTenant(ctx, "tenant-b-"+uuid.New().String()[:8], hashB)
	if err != nil {
		t.Fatalf("CreateTenant B failed: %v", err)
	}

	// Configure API with 2 RPS rate limit
	apiCfg := *cfg
	apiCfg.RateLimitRPS = 2

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	router := SetupRouter(logger, pool, s, &apiCfg)

	sendSubmit := func(key string, idemKey string) (int, string, string) {
		body := fmt.Sprintf(`{"type":"email_dispatch","payload":{"tenant_id":"%s"}}`, tenantA.ID.String())
		req, _ := http.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idemKey)

		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr.Code, rr.Header().Get("Retry-After"), rr.Body.String()
	}

	// Request 1 from Tenant A: should succeed (202)
	code, _, _ := sendSubmit(keyA, "k1-"+uuid.New().String())
	if code != http.StatusAccepted {
		t.Fatalf("expected 1st request to return 202, got %d", code)
	}

	// Request 2 from Tenant A: should succeed (202)
	code, _, _ = sendSubmit(keyA, "k2-"+uuid.New().String())
	if code != http.StatusAccepted {
		t.Fatalf("expected 2nd request to return 202, got %d", code)
	}

	// Request 3 from Tenant A: exceeds rate limit -> 429 Too Many Requests
	code, retryAfterStr, body := sendSubmit(keyA, "k3-"+uuid.New().String())
	if code != http.StatusTooManyRequests {
		t.Fatalf("expected 3rd request to return 429, got %d: %s", code, body)
	}

	// Verify Retry-After header
	if retryAfterStr == "" {
		t.Fatalf("expected Retry-After header on 429 response")
	}
	retryAfterSec, err := strconv.Atoi(retryAfterStr)
	if err != nil || retryAfterSec < 1 {
		t.Fatalf("expected valid integer Retry-After >= 1, got %q", retryAfterStr)
	}

	// Verify error response schema
	var errResp auth.ErrorResponse
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("failed to parse 429 error json: %v", err)
	}
	if errResp.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("expected error code rate_limit_exceeded, got %q", errResp.Error.Code)
	}

	// Tenant Isolation: Request from Tenant B should succeed (202)
	codeB, _, _ := sendSubmit(keyB, "kb1-"+uuid.New().String())
	if codeB != http.StatusAccepted {
		t.Fatalf("expected Tenant B request to succeed with 202, got %d", codeB)
	}

	// Verify no sensitive key material in 429 body or response
	if bytes.Contains([]byte(body), []byte(keyA)) || bytes.Contains([]byte(body), []byte(keyB)) {
		t.Fatalf("security violation: raw API key leaked in 429 error response body")
	}

	// Wait for bucket refill (at 2 RPS, 1 token refilled in 500ms)
	time.Sleep(time.Duration(retryAfterSec) * time.Second)

	// Next request from Tenant A should succeed now
	codeRefill, _, _ := sendSubmit(keyA, "k4-"+uuid.New().String())
	if codeRefill != http.StatusAccepted {
		t.Fatalf("expected request after Retry-After to succeed with 202, got %d", codeRefill)
	}
	_ = tenantB
}
