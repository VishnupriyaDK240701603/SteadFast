package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func getTestRedis(t *testing.T) *redis.Client {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://127.0.0.1:6379"
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Skipf("skipping redis test: %v", err)
	}
	rdb := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("skipping redis test, cannot ping: %v", err)
	}
	return rdb
}

func TestBaselineAPI_SubmitJob_Success(t *testing.T) {
	rdb := getTestRedis(t)
	defer func() { _ = rdb.Close() }()

	server := NewBaselineServer(rdb, nil)
	handler := server.Routes()

	body := `{"order_id":"ord-bl-001","customer_id":"cust-1","amount_cents":2500}`
	req := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["id"] == "" || resp["state"] != "QUEUED" {
		t.Fatalf("unexpected response content: %v", resp)
	}
}

func TestBaselineAPI_SubmitJob_ValidationErrors(t *testing.T) {
	rdb := getTestRedis(t)
	defer func() { _ = rdb.Close() }()

	server := NewBaselineServer(rdb, nil)
	handler := server.Routes()

	// Missing order_id
	body := `{"customer_id":"cust-1","amount_cents":2500}`
	req := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d", w.Code)
	}

	// Negative amount
	bodyNeg := `{"order_id":"ord-1","customer_id":"cust-1","amount_cents":-50}`
	reqNeg := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewReader([]byte(bodyNeg)))
	wNeg := httptest.NewRecorder()
	handler.ServeHTTP(wNeg, reqNeg)
	if wNeg.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on negative amount, got %d", wNeg.Code)
	}
}

func TestBaselineAPI_Healthz(t *testing.T) {
	server := NewBaselineServer(nil, nil)
	handler := server.Routes()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from healthz, got %d", w.Code)
	}
}
