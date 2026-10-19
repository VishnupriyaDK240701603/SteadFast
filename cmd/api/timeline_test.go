package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"steadfast/internal/store"
)

func TestTimeline_A15_3_ForeignTenantGets404(t *testing.T) {
	router, s, _, keyA, _ := setupTestAPI(t)
	ctx := context.Background()

	// Create tenant B
	tenantB, err := s.CreateTenant(ctx, "shop-b-timeline", "hash-b-timeline")
	if err != nil {
		t.Fatalf("Failed to create tenant B: %v", err)
	}

	// Create job for tenant B
	reqSub := &store.SubmitJobRequest{
		JobType:     "order.charge",
		Payload:     []byte(`{"amount_cents":100}`),
		PayloadHash: "hash-b",
		MaxAttempts: 5,
	}
	jobB, err := s.SubmitJob(ctx, tenantB.ID, reqSub, 0)
	if err != nil {
		t.Fatalf("Failed to create job for tenant B: %v", err)
	}

	// Request timeline for tenant B's job using Tenant A's API key -> MUST return 404
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/jobs/%s/timeline", jobB.JobID), nil)
	req.Header.Set("Authorization", "Bearer "+keyA)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("Expected 404 for foreign tenant timeline request, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestTimeline_A15_4_StaleWriteAudit20RowCap(t *testing.T) {
	router, s, tenant, key, _ := setupTestAPI(t)
	ctx := context.Background()

	// 1. Submit job
	reqSub := &store.SubmitJobRequest{
		JobType:     "order.charge",
		Payload:     []byte(`{"amount_cents":100}`),
		PayloadHash: "hash-cap",
		MaxAttempts: 5,
	}
	sub, err := s.SubmitJob(ctx, tenant.ID, reqSub, 0)
	if err != nil {
		t.Fatalf("Failed to submit job: %v", err)
	}

	// 2. Claim job (gets fencing_token = 1)
	claimed, err := s.ClaimJobs(ctx, tenant.ID, "worker-1", []string{"order.charge"}, 1, 10*time.Second)
	if err != nil || len(claimed) == 0 {
		t.Fatalf("Claim failed: %v", err)
	}

	// 3. Attempt to complete with stale fencing token 999 25 times
	for i := 1; i <= 25; i++ {
		compBody := `{"fencing_token": 999, "result": {"status": "ok"}}`
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/worker/jobs/%s/complete", sub.JobID), strings.NewReader(compBody))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)
		if w.Code != http.StatusConflict {
			t.Fatalf("Iteration %d: Expected 409 Conflict, got %d", i, w.Code)
		}
	}

	// 4. Query audit_log: COUNT(*) WHERE action = 'stale_write_rejected' MUST be exactly 20 (capped)
	var count int
	err = s.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_log WHERE job_id = $1 AND action = 'stale_write_rejected'", sub.JobID).Scan(&count)
	if err != nil {
		t.Fatalf("Failed to query audit_log: %v", err)
	}

	if count != 20 {
		t.Fatalf("Expected exactly 20 stale_write_rejected audit records (20-row cap), got %d", count)
	}

	// 5. Query timeline endpoint: must return 200 and include attempts and events
	tReq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/jobs/%s/timeline", sub.JobID), nil)
	tReq.Header.Set("Authorization", "Bearer "+key)
	tW := httptest.NewRecorder()

	router.ServeHTTP(tW, tReq)
	if tW.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK from timeline endpoint, got %d", tW.Code)
	}

	var timeline store.JobTimeline
	if err := json.NewDecoder(tW.Body).Decode(&timeline); err != nil {
		t.Fatalf("Failed to decode timeline response: %v", err)
	}

	if timeline.JobID != sub.JobID {
		t.Fatalf("Timeline job ID mismatch: got %s, expected %s", timeline.JobID, sub.JobID)
	}
	if len(timeline.Attempts) != 1 {
		t.Fatalf("Expected 1 attempt in timeline, got %d", len(timeline.Attempts))
	}
}
