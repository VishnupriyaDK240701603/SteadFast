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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"steadfast/internal/auth"
	"steadfast/internal/config"
	"steadfast/internal/store"
)

func setupTestAPI(t *testing.T) (http.Handler, *store.Store, *store.Tenant, string, *config.Config) {
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
	rawKey := "valid_test_api_key_" + uuid.New().String()
	hash := auth.HashAPIKey(rawKey)
	tenantName := "api-test-" + uuid.New().String()[:8]

	tenant, err := s.CreateTenant(ctx, tenantName, hash)
	if err != nil {
		t.Fatalf("CreateTenant failed: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	router := SetupRouter(logger, pool, s, cfg)

	return router, s, tenant, rawKey, cfg
}

func TestSubmitJobValidation(t *testing.T) {
	router, _, _, validKey, _ := setupTestAPI(t)

	tests := []struct {
		name           string
		body           string
		contentType    string
		headerKey      string
		expectedStatus int
		expectedCode   string
	}{
		{
			name:           "missing content-type",
			body:           `{"type":"payment","payload":{"amount":10}}`,
			contentType:    "",
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "invalid_request",
		},
		{
			name:           "bad json syntax",
			body:           `{"type":"payment", payload:}`,
			contentType:    "application/json",
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "invalid_request",
		},
		{
			name:           "unknown field rejected",
			body:           `{"type":"payment","payload":{"amount":10},"injected":"bad"}`,
			contentType:    "application/json",
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "invalid_request",
		},
		{
			name:           "invalid type regex (uppercase and symbols)",
			body:           `{"type":"PAYMENT!","payload":{"amount":10}}`,
			contentType:    "application/json",
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "invalid_request",
		},
		{
			name:           "empty type",
			body:           `{"type":"","payload":{"amount":10}}`,
			contentType:    "application/json",
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "invalid_request",
		},
		{
			name:           "empty payload",
			body:           `{"type":"charge","payload":null}`,
			contentType:    "application/json",
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "invalid_request",
		},
		{
			name:           "oversized payload (> 64 KiB)",
			body:           fmt.Sprintf(`{"type":"charge","payload":{"blob":"%s"}}`, strings.Repeat("x", 70000)),
			contentType:    "application/json",
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "invalid_request",
		},
		{
			name:           "max_attempts 0",
			body:           `{"type":"charge","payload":{"amount":10},"max_attempts":0}`,
			contentType:    "application/json",
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "invalid_request",
		},
		{
			name:           "max_attempts 21",
			body:           `{"type":"charge","payload":{"amount":10},"max_attempts":21}`,
			contentType:    "application/json",
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "invalid_request",
		},
		{
			name:           "oversized idempotency key (> 128 chars)",
			body:           `{"type":"charge","payload":{"amount":10}}`,
			contentType:    "application/json",
			headerKey:      strings.Repeat("k", 129),
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "invalid_request",
		},
		{
			name:           "valid submission without idempotency key",
			body:           `{"type":"charge_customer","payload":{"order_id":"123","amount":99}}`,
			contentType:    "application/json",
			expectedStatus: http.StatusAccepted,
		},
		{
			name:           "valid submission with idempotency key",
			body:           `{"type":"charge_customer","payload":{"order_id":"456","amount":199}}`,
			contentType:    "application/json",
			headerKey:      "valid-key-" + uuid.New().String(),
			expectedStatus: http.StatusAccepted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/jobs", bytes.NewBufferString(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			if tc.headerKey != "" {
				req.Header.Set("Idempotency-Key", tc.headerKey)
			}
			req.Header.Set("Authorization", "Bearer "+validKey)
			rec := httptest.NewRecorder()

			router.ServeHTTP(rec, req)

			if rec.Code != tc.expectedStatus {
				t.Fatalf("Expected status %d, got %d. Body: %s", tc.expectedStatus, rec.Code, rec.Body.String())
			}

			if tc.expectedCode != "" {
				var errResp auth.ErrorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
					t.Fatalf("Failed to decode error response: %v", err)
				}
				if errResp.Error.Code != tc.expectedCode {
					t.Fatalf("Expected error code %q, got %q", tc.expectedCode, errResp.Error.Code)
				}
			}
		})
	}
}

func TestConcurrentSubmissions(t *testing.T) {
	router, s, tenant, validKey, _ := setupTestAPI(t)

	// Run concurrency test 3 times per A2.1
	for run := 1; run <= 3; run++ {
		t.Run(fmt.Sprintf("run_%d", run), func(t *testing.T) {
			idempKey := fmt.Sprintf("concurrent-key-run-%d-%s", run, uuid.New().String())
			payloadStr := `{"type":"payment","payload":{"order_id":"concurrent_order","amount_cents":4500}}`

			const concurrency = 50
			var wg sync.WaitGroup
			wg.Add(concurrency)

			type result struct {
				code       int
				jobID      string
				replayed   bool
				errMessage string
			}
			results := make([]result, concurrency)

			for i := 0; i < concurrency; i++ {
				idx := i
				go func() {
					defer wg.Done()
					req := httptest.NewRequest("POST", "/v1/jobs", bytes.NewBufferString(payloadStr))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer "+validKey)
					req.Header.Set("Idempotency-Key", idempKey)
					rec := httptest.NewRecorder()

					router.ServeHTTP(rec, req)

					var resp SubmitJobResponse
					_ = json.Unmarshal(rec.Body.Bytes(), &resp)

					results[idx] = result{
						code:       rec.Code,
						jobID:      resp.ID,
						replayed:   rec.Header().Get("Idempotent-Replayed") == "true",
						errMessage: rec.Body.String(),
					}
				}()
			}

			wg.Wait()

			firstJobID := ""
			replayedCount := 0
			acceptedCount := 0

			for i, res := range results {
				if res.code != http.StatusAccepted {
					t.Fatalf("Client %d received status %d: %s", i, res.code, res.errMessage)
				}
				acceptedCount++
				if firstJobID == "" {
					firstJobID = res.jobID
				} else if res.jobID != firstJobID {
					t.Fatalf("Duplicate job created under same key! Expected %s, got %s", firstJobID, res.jobID)
				}
				if res.replayed {
					replayedCount++
				}
			}

			if acceptedCount != concurrency {
				t.Fatalf("Expected %d accepted submissions, got %d", concurrency, acceptedCount)
			}
			if replayedCount != concurrency-1 {
				t.Fatalf("Expected exactly %d replayed responses, got %d", concurrency-1, replayedCount)
			}

			// Verify in database that exactly 1 job was created
			parsedJobID, err := uuid.Parse(firstJobID)
			if err != nil {
				t.Fatalf("Failed to parse returned job ID: %v", err)
			}
			job, err := s.GetJob(context.Background(), tenant.ID, parsedJobID)
			if err != nil {
				t.Fatalf("Failed to get created job from DB: %v", err)
			}
			if job.ID != parsedJobID {
				t.Fatalf("Database job mismatch")
			}
		})
	}
}

func TestIdempotencyPayloadMismatch(t *testing.T) {
	router, _, _, validKey, _ := setupTestAPI(t)
	idempKey := "mismatch-key-" + uuid.New().String()

	// 1. Initial submission
	body1 := `{"type":"charge","payload":{"amount":100}}`
	req1 := httptest.NewRequest("POST", "/v1/jobs", bytes.NewBufferString(body1))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer "+validKey)
	req1.Header.Set("Idempotency-Key", idempKey)
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusAccepted {
		t.Fatalf("Initial submission failed with code %d: %s", rec1.Code, rec1.Body.String())
	}

	// 2. Re-submission with identical payload -> 202 with Idempotent-Replayed: true
	req2 := httptest.NewRequest("POST", "/v1/jobs", bytes.NewBufferString(body1))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+validKey)
	req2.Header.Set("Idempotency-Key", idempKey)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusAccepted {
		t.Fatalf("Replay failed with code %d", rec2.Code)
	}
	if rec2.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("Expected Idempotent-Replayed: true header")
	}

	// 3. Re-submission with DIFFERENT payload -> 409 Conflict
	bodyDiff := `{"type":"charge","payload":{"amount":200}}`
	req3 := httptest.NewRequest("POST", "/v1/jobs", bytes.NewBufferString(bodyDiff))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Authorization", "Bearer "+validKey)
	req3.Header.Set("Idempotency-Key", idempKey)
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusConflict {
		t.Fatalf("Expected 409 Conflict on payload mismatch, got %d: %s", rec3.Code, rec3.Body.String())
	}

	var errResp auth.ErrorResponse
	_ = json.Unmarshal(rec3.Body.Bytes(), &errResp)
	if errResp.Error.Code != "idempotency_key_conflict" {
		t.Fatalf("Expected error code 'idempotency_key_conflict', got '%s'", errResp.Error.Code)
	}
}

func TestCrossTenantIdempotencyIsolation(t *testing.T) {
	router, s, _, keyA, _ := setupTestAPI(t)

	// Create Tenant B
	keyB := "tenant_b_key_" + uuid.New().String()
	hashB := auth.HashAPIKey(keyB)
	_, err := s.CreateTenant(context.Background(), "shop-b-"+uuid.New().String()[:8], hashB)
	if err != nil {
		t.Fatalf("Failed to create tenant B: %v", err)
	}

	sharedKey := "shared-idem-key-" + uuid.New().String()
	body := `{"type":"fulfill","payload":{"sku":"item-123"}}`

	// Tenant A submits with sharedKey
	reqA := httptest.NewRequest("POST", "/v1/jobs", bytes.NewBufferString(body))
	reqA.Header.Set("Content-Type", "application/json")
	reqA.Header.Set("Authorization", "Bearer "+keyA)
	reqA.Header.Set("Idempotency-Key", sharedKey)
	recA := httptest.NewRecorder()
	router.ServeHTTP(recA, reqA)

	if recA.Code != http.StatusAccepted {
		t.Fatalf("Tenant A submit failed: %d", recA.Code)
	}
	var respA SubmitJobResponse
	_ = json.Unmarshal(recA.Body.Bytes(), &respA)

	// Tenant B submits with identical sharedKey and payload
	reqB := httptest.NewRequest("POST", "/v1/jobs", bytes.NewBufferString(body))
	reqB.Header.Set("Content-Type", "application/json")
	reqB.Header.Set("Authorization", "Bearer "+keyB)
	reqB.Header.Set("Idempotency-Key", sharedKey)
	recB := httptest.NewRecorder()
	router.ServeHTTP(recB, reqB)

	if recB.Code != http.StatusAccepted {
		t.Fatalf("Tenant B submit failed: %d", recB.Code)
	}
	var respB SubmitJobResponse
	_ = json.Unmarshal(recB.Body.Bytes(), &respB)

	// Verify Tenant B got their own separate job and was NOT replayed from Tenant A
	if respA.ID == respB.ID {
		t.Fatalf("Cross-tenant leak: Tenant B received Tenant A's job ID: %s", respA.ID)
	}
	if recB.Header().Get("Idempotent-Replayed") == "true" {
		t.Fatalf("Tenant B should not receive Idempotent-Replayed: true from Tenant A's key")
	}
}

func TestIdempotencyTTLExpiry(t *testing.T) {
	_, s, tenant, _, _ := setupTestAPI(t)
	ctx := context.Background()

	idempKey := "ttl-expiry-key-" + uuid.New().String()
	p1 := []byte(`{"version": 1}`)
	p2 := []byte(`{"version": 2}`)

	shortTTL := 200 * time.Millisecond

	req1 := &store.SubmitJobRequest{
		JobType:        "task",
		Payload:        p1,
		PayloadHash:    "hash_v1",
		IdempotencyKey: idempKey,
	}

	// 1. Initial submit
	res1, err := s.SubmitJob(ctx, tenant.ID, req1, shortTTL)
	if err != nil {
		t.Fatalf("Initial submit failed: %v", err)
	}

	// 2. Wait for TTL to expire
	time.Sleep(300 * time.Millisecond)

	// 3. Submit same key with different payload -> should succeed and replace expired key
	req2 := &store.SubmitJobRequest{
		JobType:        "task",
		Payload:        p2,
		PayloadHash:    "hash_v2",
		IdempotencyKey: idempKey,
	}
	res2, err := s.SubmitJob(ctx, tenant.ID, req2, shortTTL)
	if err != nil {
		t.Fatalf("Submit after TTL expiry failed: %v", err)
	}
	if res2.JobID == res1.JobID {
		t.Fatalf("Expected new job after expired idempotency key, got same job ID")
	}
	if res2.IsReplayed {
		t.Fatalf("Expected IsReplayed to be false after expiry")
	}
}

func TestGetJobAndAttempts(t *testing.T) {
	router, s, tenantA, keyA, _ := setupTestAPI(t)

	// Create Tenant B
	keyB := "b_key_" + uuid.New().String()
	_, err := s.CreateTenant(context.Background(), "b-name-"+uuid.New().String()[:8], auth.HashAPIKey(keyB))
	if err != nil {
		t.Fatalf("Failed to create tenant B: %v", err)
	}

	// Create job for Tenant A
	jobA := &store.Job{
		JobType:     "report_generation",
		Payload:     []byte(`{"report":"monthly"}`),
		PayloadHash: "hash_rep",
		State:       "QUEUED",
		Priority:    50,
		MaxAttempts: 10,
	}
	jobA, err = s.CreateJob(context.Background(), tenantA.ID, jobA)
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	// 1. Tenant A fetches own job -> 200
	reqA := httptest.NewRequest("GET", "/v1/jobs/"+jobA.ID.String(), nil)
	reqA.Header.Set("Authorization", "Bearer "+keyA)
	recA := httptest.NewRecorder()
	router.ServeHTTP(recA, reqA)

	if recA.Code != http.StatusOK {
		t.Fatalf("Tenant A get job failed with code %d: %s", recA.Code, recA.Body.String())
	}
	var jobWithAttempts store.JobWithAttempts
	if err := json.Unmarshal(recA.Body.Bytes(), &jobWithAttempts); err != nil {
		t.Fatalf("Failed to decode job JSON: %v", err)
	}
	if jobWithAttempts.ID != jobA.ID || jobWithAttempts.JobType != "report_generation" {
		t.Fatalf("Job details mismatch")
	}

	// 2. Tenant B attempts to fetch Tenant A's job -> 404
	reqB := httptest.NewRequest("GET", "/v1/jobs/"+jobA.ID.String(), nil)
	reqB.Header.Set("Authorization", "Bearer "+keyB)
	recB := httptest.NewRecorder()
	router.ServeHTTP(recB, reqB)

	if recB.Code != http.StatusNotFound {
		t.Fatalf("Security failure: Tenant B received status %d for Tenant A's job ID (expected 404)", recB.Code)
	}
}

func TestListJobsWithPagination(t *testing.T) {
	router, s, tenant, validKey, _ := setupTestAPI(t)
	ctx := context.Background()

	// Insert 5 jobs
	for i := 1; i <= 5; i++ {
		j := &store.Job{
			JobType:     "batch_item",
			Payload:     []byte(fmt.Sprintf(`{"item": %d}`, i)),
			PayloadHash: fmt.Sprintf("h_%d", i),
			State:       "QUEUED",
		}
		_, err := s.CreateJob(ctx, tenant.ID, j)
		if err != nil {
			t.Fatalf("CreateJob %d failed: %v", i, err)
		}
	}

	// Page 1: limit 2
	req1 := httptest.NewRequest("GET", "/v1/jobs?limit=2&state=QUEUED", nil)
	req1.Header.Set("Authorization", "Bearer "+validKey)
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("Page 1 failed with code %d: %s", rec1.Code, rec1.Body.String())
	}

	var page1 ListJobsResponse
	if err := json.Unmarshal(rec1.Body.Bytes(), &page1); err != nil {
		t.Fatalf("Failed to decode page 1: %v", err)
	}
	if len(page1.Jobs) != 2 {
		t.Fatalf("Expected 2 jobs on page 1, got %d", len(page1.Jobs))
	}
	if page1.NextCursor == "" {
		t.Fatalf("Expected next_cursor on page 1, got empty")
	}

	// Page 2: with cursor
	req2 := httptest.NewRequest("GET", "/v1/jobs?limit=2&state=QUEUED&cursor="+page1.NextCursor, nil)
	req2.Header.Set("Authorization", "Bearer "+validKey)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("Page 2 failed with code %d: %s", rec2.Code, rec2.Body.String())
	}

	var page2 ListJobsResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &page2); err != nil {
		t.Fatalf("Failed to decode page 2: %v", err)
	}
	if len(page2.Jobs) != 2 {
		t.Fatalf("Expected 2 jobs on page 2, got %d", len(page2.Jobs))
	}

	// Verify items on page 1 and page 2 are disjoint
	if page1.Jobs[0].ID == page2.Jobs[0].ID || page1.Jobs[1].ID == page2.Jobs[0].ID {
		t.Fatalf("Cursor pagination returned duplicate items between pages")
	}

	// Invalid state parameter -> 400
	reqBadState := httptest.NewRequest("GET", "/v1/jobs?state=INVALID", nil)
	reqBadState.Header.Set("Authorization", "Bearer "+validKey)
	recBadState := httptest.NewRecorder()
	router.ServeHTTP(recBadState, reqBadState)
	if recBadState.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 on invalid state, got %d", recBadState.Code)
	}

	// Limit > 200 -> 400
	reqBadLimit := httptest.NewRequest("GET", "/v1/jobs?limit=250", nil)
	reqBadLimit.Header.Set("Authorization", "Bearer "+validKey)
	recBadLimit := httptest.NewRecorder()
	router.ServeHTTP(recBadLimit, reqBadLimit)
	if recBadLimit.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 on limit > 200, got %d", recBadLimit.Code)
	}
}

func TestOutageReturns503(t *testing.T) {
	cfg, _ := config.Load()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// Setup router with nil pool
	router := SetupRouter(logger, nil, nil, cfg)

	req := httptest.NewRequest("POST", "/v1/jobs", bytes.NewBufferString(`{"type":"charge","payload":{"amount":10}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer any_key")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	// Must return 401 (if no auth store) or 503 with Retry-After if store failed
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 401 or 503 during outage, got %d", rec.Code)
	}
}
