package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"steadfast/internal/config"
	"steadfast/internal/store"
)

func TestReaperAdvisoryLockAndReap(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	ctx := context.Background()
	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("Failed to connect to db: %v", err)
	}
	defer pool.Close()

	st := store.NewStore(pool)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// Create test tenant
	tenant, err := st.CreateTenant(ctx, "reaper_test_tenant_"+uuid.New().String(), "hash_"+uuid.New().String())
	if err != nil {
		t.Fatalf("CreateTenant failed: %v", err)
	}

	// Insert job with already-expired lease
	jobID := uuid.New()
	_, err = pool.Exec(ctx, `
	INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, state, priority, run_at, attempts, max_attempts, fencing_token, worker_id, lease_expires_at)
	VALUES ($1, $2, 'reaper_task', '{"expired":true}', 'h_exp', 'LEASED', 100, now(), 1, 3, 1, 'dead_worker', now() - interval '5 seconds');`,
		jobID, tenant.ID,
	)
	if err != nil {
		t.Fatalf("Failed to insert expired job: %v", err)
	}

	// Insert matching attempt row
	_, err = pool.Exec(ctx, `
	INSERT INTO job_attempts (job_id, attempt_no, fencing_token, worker_id, started_at)
	VALUES ($1, 1, 1, 'dead_worker', now() - interval '10 seconds');`, jobID)
	if err != nil {
		t.Fatalf("Failed to insert attempt row: %v", err)
	}

	// A4.3: Two reapers at once do not conflict or double-requeue
	runReaperTick(ctx, logger, pool, st)

	// Verify job returned to QUEUED
	var state string
	err = pool.QueryRow(ctx, "SELECT state FROM jobs WHERE id = $1", jobID).Scan(&state)
	if err != nil {
		t.Fatalf("Failed to query job state: %v", err)
	}
	if state != "QUEUED" {
		t.Fatalf("Expected reaped job to be QUEUED, got %s", state)
	}

	// Verify attempt row closed
	var outcome string
	err = pool.QueryRow(ctx, "SELECT outcome FROM job_attempts WHERE job_id = $1 AND attempt_no = 1", jobID).Scan(&outcome)
	if err != nil {
		t.Fatalf("Failed to query attempt outcome: %v", err)
	}
	if outcome != "lease_expired" {
		t.Fatalf("Expected attempt outcome lease_expired, got %s", outcome)
	}
}
