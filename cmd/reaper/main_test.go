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

	// Run reaper tick
	acquired, reaped, err := runReaperTick(ctx, logger, pool, st)
	if err != nil || !acquired || reaped < 1 {
		t.Fatalf("runReaperTick failed: acquired=%v, reaped=%d, err=%v", acquired, reaped, err)
	}

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

func TestConcurrentReapersExclusiveLock(t *testing.T) {
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

	// Acquire a dedicated connection simulating Reaper Instance 1
	conn1, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Failed to acquire conn1: %v", err)
	}
	defer conn1.Release()

	// Reaper 1 acquires the advisory lock
	var lock1 bool
	err = conn1.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", reaperAdvisoryLockID).Scan(&lock1)
	if err != nil || !lock1 {
		t.Fatalf("Reaper 1 failed to acquire lock: %v, lock1=%v", err, lock1)
	}

	// While Reaper 1 holds the lock, Reaper 2 runs runReaperTick -> MUST return acquired=false
	acquired2, reaped2, err2 := runReaperTick(ctx, logger, pool, st)
	if err2 != nil {
		t.Fatalf("runReaperTick returned error on contention: %v", err2)
	}
	if acquired2 {
		t.Fatalf("MUTUAL EXCLUSION VIOLATION: Reaper 2 acquired lock while Reaper 1 was holding it!")
	}
	if reaped2 != 0 {
		t.Fatalf("Reaper 2 should not have reaped any jobs: %d", reaped2)
	}

	// Reaper 1 releases the lock
	var unlock1 bool
	err = conn1.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", reaperAdvisoryLockID).Scan(&unlock1)
	if err != nil || !unlock1 {
		t.Fatalf("Reaper 1 failed to unlock: %v, unlock1=%v", err, unlock1)
	}

	// Now Reaper 2 runs again -> MUST succeed
	acquired3, _, err3 := runReaperTick(ctx, logger, pool, st)
	if err3 != nil || !acquired3 {
		t.Fatalf("Expected Reaper 2 to acquire lock after release, got acquired=%v, err=%v", acquired3, err3)
	}
}
