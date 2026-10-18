package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
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

func TestBaselineWorker_ProcessJob_Success(t *testing.T) {
	rdb := getTestRedis(t)
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()
	_ = rdb.Del(ctx, "baseline:queue", "baseline:processing", "baseline:failed").Err()

	var chargeCount int32
	ledgerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&chargeCount, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"applied"}`))
	}))
	defer ledgerServer.Close()

	worker := NewBaselineWorker(rdb, ledgerServer.URL, "visibility_timeout", 10, false, nil)

	// Enqueue test job
	job := BaselineJob{
		ID:          "job-bl-001",
		OrderID:     "ord-bl-001",
		CustomerID:  "cust-1",
		AmountCents: 1500,
		MaxAttempts: 5,
	}
	jobJSON, _ := json.Marshal(job)
	_ = rdb.LPush(ctx, "baseline:queue", string(jobJSON)).Err()

	// Process one job
	err := worker.processNextJob(ctx)
	if err != nil {
		t.Fatalf("processNextJob failed: %v", err)
	}

	if atomic.LoadInt32(&chargeCount) != 1 {
		t.Fatalf("expected 1 charge request, got %d", atomic.LoadInt32(&chargeCount))
	}

	// Verify baseline:processing is empty
	procCount, _ := rdb.LLen(ctx, "baseline:processing").Result()
	if procCount != 0 {
		t.Fatalf("expected 0 items in baseline:processing, got %d", procCount)
	}
}

func TestBaselineWorker_ProcessJob_ExhaustedReachesFailedList(t *testing.T) {
	rdb := getTestRedis(t)
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()
	_ = rdb.Del(ctx, "baseline:queue", "baseline:processing", "baseline:failed").Err()

	// Ledger always returns 500
	ledgerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ledgerServer.Close()

	worker := NewBaselineWorker(rdb, ledgerServer.URL, "visibility_timeout", 10, false, nil)

	// Enqueue job already at max_attempts-1
	job := BaselineJob{
		ID:          "job-bl-fail",
		OrderID:     "ord-bl-fail",
		CustomerID:  "cust-1",
		AmountCents: 1500,
		Attempts:    4,
		MaxAttempts: 5,
	}
	jobJSON, _ := json.Marshal(job)
	_ = rdb.LPush(ctx, "baseline:queue", string(jobJSON)).Err()

	err := worker.processNextJob(ctx)
	if err != nil {
		t.Fatalf("processNextJob failed: %v", err)
	}

	// Verify moved to baseline:failed
	failedCount, _ := rdb.LLen(ctx, "baseline:failed").Result()
	if failedCount == 0 {
		t.Fatalf("expected item in baseline:failed, got 0")
	}
}
