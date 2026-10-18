package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"steadfast/internal/config"
	"steadfast/internal/store"
)

func TestHealthzEndpoint(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	router := SetupRouter(logger, nil, nil)

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK from /healthz, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Failed to parse /healthz body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("Expected status 'ok', got '%s'", body["status"])
	}
}

func TestReadyzOutageHandling(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// When pool is nil (outage scenario)
	routerNilPool := SetupRouter(logger, nil, nil)
	req := httptest.NewRequest("GET", "/readyz", nil)
	rec := httptest.NewRecorder()
	routerNilPool.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 Service Unavailable when DB is down, got %d", rec.Code)
	}

	var errBody map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("Failed to decode 503 response body: %v", err)
	}
	if errBody["error"]["code"] != "service_unavailable" {
		t.Fatalf("Expected error code 'service_unavailable', got '%s'", errBody["error"]["code"])
	}

	// When pool is connected and healthy
	cfg, err := config.Load()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pool, err := store.Connect(ctx, cfg.DatabaseURL)
		if err == nil {
			defer pool.Close()
			routerHealthy := SetupRouter(logger, pool, nil)
			reqOK := httptest.NewRequest("GET", "/readyz", nil)
			recOK := httptest.NewRecorder()
			routerHealthy.ServeHTTP(recOK, reqOK)

			if recOK.Code != http.StatusOK {
				t.Fatalf("Expected 200 OK when DB is healthy, got %d", recOK.Code)
			}
		}
	}
}

func TestDecodeStrictJSON(t *testing.T) {
	type SamplePayload struct {
		JobType string `json:"job_type"`
		Amount  int    `json:"amount"`
	}

	// 1. Valid payload
	validJSON := strings.NewReader(`{"job_type":"charge","amount":100}`)
	var dst1 SamplePayload
	if err := DecodeStrictJSON(validJSON, &dst1); err != nil {
		t.Fatalf("Expected valid json to decode cleanly, got %v", err)
	}
	if dst1.JobType != "charge" || dst1.Amount != 100 {
		t.Fatalf("Unexpected decoded struct: %+v", dst1)
	}

	// 2. Disallowed unknown field
	unknownFieldJSON := strings.NewReader(`{"job_type":"charge","amount":100,"hacker_field":"injected"}`)
	var dst2 SamplePayload
	if err := DecodeStrictJSON(unknownFieldJSON, &dst2); err == nil {
		t.Fatalf("Expected error on unknown field, got nil")
	}

	// 3. Trailing junk data
	trailingJSON := strings.NewReader(`{"job_type":"charge","amount":100} extra_garbage`)
	var dst3 SamplePayload
	if err := DecodeStrictJSON(trailingJSON, &dst3); err == nil {
		t.Fatalf("Expected error on trailing data, got nil")
	}
}

func TestRequestIDHeaderGeneration(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	router := SetupRouter(logger, nil, nil)

	// Case 1: Client provides X-Request-ID
	req1 := httptest.NewRequest("GET", "/healthz", nil)
	req1.Header.Set("X-Request-ID", "custom-req-id-12345")
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	if rec1.Header().Get("X-Request-ID") != "custom-req-id-12345" {
		t.Fatalf("Expected preserved X-Request-ID, got %s", rec1.Header().Get("X-Request-ID"))
	}

	// Case 2: No X-Request-ID provided -> generates one
	req2 := httptest.NewRequest("GET", "/healthz", nil)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	generated := rec2.Header().Get("X-Request-ID")
	if len(generated) != 32 {
		t.Fatalf("Expected 32-character generated request ID hex, got %s", generated)
	}
}
