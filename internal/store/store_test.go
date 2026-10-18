package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"steadfast/internal/config"
)

func setupTestStore(t *testing.T) (*Store, *config.Config) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	// Ensure migrations are up
	if err := MigrateUp(ctx, pool, "../../migrations"); err != nil {
		t.Fatalf("MigrateUp failed: %v", err)
	}

	return NewStore(pool), cfg
}

func TestTenantIsolation(t *testing.T) {
	s, _ := setupTestStore(t)
	ctx := context.Background()

	// Provision two isolated tenants
	tenantA, err := s.CreateTenant(ctx, "iso-tenant-a-"+uuid.New().String()[:8], "hash_a_"+uuid.New().String())
	if err != nil {
		t.Fatalf("CreateTenant A failed: %v", err)
	}

	tenantB, err := s.CreateTenant(ctx, "iso-tenant-b-"+uuid.New().String()[:8], "hash_b_"+uuid.New().String())
	if err != nil {
		t.Fatalf("CreateTenant B failed: %v", err)
	}

	// Tenant A creates a job
	payload := json.RawMessage(`{"amount": 100, "currency": "USD"}`)
	jobA := &Job{
		JobType:     "charge_customer",
		Payload:     payload,
		PayloadHash: "hash_payload_123",
		State:       "QUEUED",
		Priority:    10,
		MaxAttempts: 5,
	}
	jobA, err = s.CreateJob(ctx, tenantA.ID, jobA)
	if err != nil {
		t.Fatalf("CreateJob for Tenant A failed: %v", err)
	}

	// Tenant A reads jobA -> succeeds
	fetchedJob, err := s.GetJob(ctx, tenantA.ID, jobA.ID)
	if err != nil {
		t.Fatalf("Tenant A failed to get own job: %v", err)
	}
	if fetchedJob.ID != jobA.ID || fetchedJob.TenantID != tenantA.ID {
		t.Fatalf("Job mismatch: %+v", fetchedJob)
	}

	// Engine Law 4: Tenant B attempts to read jobA -> must return ErrNotFound
	_, err = s.GetJob(ctx, tenantB.ID, jobA.ID)
	if err == nil {
		t.Fatalf("Security breach: Tenant B was able to fetch Tenant A's job!")
	}
	if err != ErrNotFound {
		t.Fatalf("Expected ErrNotFound, got: %v", err)
	}

	// Tenant B attempts to update jobA -> must return ErrNotFound
	err = s.UpdateJobState(ctx, tenantB.ID, jobA.ID, "LEASED", "malicious_worker")
	if err == nil {
		t.Fatalf("Security breach: Tenant B was able to update Tenant A's job state!")
	}
	if err != ErrNotFound {
		t.Fatalf("Expected ErrNotFound, got: %v", err)
	}

	// Tenant B attempts to read audit logs of jobA -> must return empty
	auditEntriesB, err := s.GetAuditEntriesForJob(ctx, tenantB.ID, jobA.ID)
	if err != nil {
		t.Fatalf("GetAuditEntriesForJob failed: %v", err)
	}
	if len(auditEntriesB) != 0 {
		t.Fatalf("Security breach: Tenant B retrieved audit entries for Tenant A's job!")
	}
}

func TestAuditStateTransitionTrigger(t *testing.T) {
	s, _ := setupTestStore(t)
	ctx := context.Background()

	tenant, err := s.CreateTenant(ctx, "audit-tenant-"+uuid.New().String()[:8], "hash_"+uuid.New().String())
	if err != nil {
		t.Fatalf("CreateTenant failed: %v", err)
	}

	job := &Job{
		JobType:     "order_fulfillment",
		Payload:     []byte(`{"order_id": "ord_999"}`),
		PayloadHash: "hash_999",
		State:       "QUEUED",
	}
	job, err = s.CreateJob(ctx, tenant.ID, job)
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	// 1. Transition QUEUED -> LEASED with actor "worker-node-1"
	if err := s.UpdateJobState(ctx, tenant.ID, job.ID, "LEASED", "worker-node-1"); err != nil {
		t.Fatalf("UpdateJobState to LEASED failed: %v", err)
	}

	// 2. Transition LEASED -> SUCCEEDED with actor "" (should record "system")
	if err := s.UpdateJobState(ctx, tenant.ID, job.ID, "SUCCEEDED", ""); err != nil {
		t.Fatalf("UpdateJobState to SUCCEEDED failed: %v", err)
	}

	// Verify audit log entries
	entries, err := s.GetAuditEntriesForJob(ctx, tenant.ID, job.ID)
	if err != nil {
		t.Fatalf("GetAuditEntriesForJob failed: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("Expected 2 audit log entries, found %d", len(entries))
	}

	// Entry 1: QUEUED -> LEASED by worker-node-1
	e1 := entries[0]
	if e1.Actor != "worker-node-1" || *e1.FromState != "QUEUED" || *e1.ToState != "LEASED" {
		t.Fatalf("Unexpected entry 1: %+v", e1)
	}

	// Entry 2: LEASED -> SUCCEEDED by system
	e2 := entries[1]
	if e2.Actor != "system" || *e2.FromState != "LEASED" || *e2.ToState != "SUCCEEDED" {
		t.Fatalf("Unexpected entry 2: %+v", e2)
	}
}

func TestSteadfastCheckerRoRolePermissions(t *testing.T) {
	_, cfg := setupTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	roPool, err := Connect(ctx, cfg.CheckerDatabaseURL)
	if err != nil {
		t.Fatalf("Connect as steadfast_checker_ro failed: %v", err)
	}
	defer roPool.Close()

	// 1. SELECT on jobs -> must SUCCEED
	var jobCount int
	if err := roPool.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&jobCount); err != nil {
		t.Fatalf("steadfast_checker_ro should be able to SELECT jobs, failed: %v", err)
	}

	// 2. SELECT on job_attempts -> must SUCCEED
	var attemptCount int
	if err := roPool.QueryRow(ctx, "SELECT count(*) FROM job_attempts").Scan(&attemptCount); err != nil {
		t.Fatalf("steadfast_checker_ro should be able to SELECT job_attempts, failed: %v", err)
	}

	// 3. INSERT on jobs -> must FAIL (permission denied)
	_, err = roPool.Exec(ctx, "INSERT INTO jobs (tenant_id, job_type, payload, payload_hash) VALUES ($1, 't', '{}', 'h')", uuid.New())
	if err == nil {
		t.Fatalf("Security failure: steadfast_checker_ro was able to INSERT into jobs!")
	}

	// 4. UPDATE on jobs -> must FAIL (permission denied)
	_, err = roPool.Exec(ctx, "UPDATE jobs SET state = 'DEAD'")
	if err == nil {
		t.Fatalf("Security failure: steadfast_checker_ro was able to UPDATE jobs!")
	}

	// 5. SELECT on tenants -> must FAIL (permission denied - read-only strictly on jobs and job_attempts)
	_, err = roPool.Exec(ctx, "SELECT count(*) FROM tenants")
	if err == nil {
		t.Fatalf("Security failure: steadfast_checker_ro was able to SELECT tenants!")
	}
}
