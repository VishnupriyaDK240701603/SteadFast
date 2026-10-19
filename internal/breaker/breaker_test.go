package breaker_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"steadfast/internal/breaker"
	"steadfast/internal/store"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func setupTestDB(t *testing.T) (*pgxpool.Pool, uuid.UUID) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://steadfast_user:fake_pg_password_for_local_demo_only@127.0.0.1:5432/steadfast?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("Postgres unavailable for breaker tests: %v", err)
	}

	tenantID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, name, api_key_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING;
	`, tenantID, "test-tenant-"+tenantID.String()[:8], "hash-"+tenantID.String())
	if err != nil {
		pool.Close()
		t.Skipf("Failed to insert tenant: %v", err)
	}

	return pool, tenantID
}

func TestBreaker_PermanentErrorDoesNotOpen(t *testing.T) {
	pool, tenantID := setupTestDB(t)
	defer pool.Close()
	ctx := context.Background()

	b := breaker.New(breaker.BreakerConfig{
		MinSamples:      3,
		FailureRatio:    0.5,
		CooldownSeconds: 2,
	})

	jobType := "order.charge"

	// 5 permanent errors
	for i := 0; i < 5; i++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("failed to begin tx: %v", err)
		}
		err = b.RecordFailure(ctx, tx, tenantID, jobType, "permanent_error", false)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("RecordFailure error: %v", err)
		}
		_ = tx.Commit(ctx)
	}

	// Verify breaker state is still CLOSED or not opened
	var state string
	err := pool.QueryRow(ctx, "SELECT state FROM circuit_breakers WHERE tenant_id = $1 AND job_type = $2", tenantID, jobType).Scan(&state)
	if err == nil && state == string(breaker.StateOpen) {
		t.Fatalf("Requirement 4 violation: permanent errors tripped breaker to OPEN")
	}
}

func TestBreaker_DownstreamUnavailableTripsAndRecovers(t *testing.T) {
	pool, tenantID := setupTestDB(t)
	defer pool.Close()
	ctx := context.Background()

	b := breaker.New(breaker.BreakerConfig{
		MinSamples:      3,
		FailureRatio:    0.5,
		CooldownSeconds: 1, // 1 second cooldown for fast testing
	})

	jobType := "order.payment"

	// 3 downstream_unavailable failures -> trips breaker
	for i := 0; i < 3; i++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("failed to begin tx: %v", err)
		}
		err = b.RecordFailure(ctx, tx, tenantID, jobType, "downstream_unavailable", true)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("RecordFailure error: %v", err)
		}
		_ = tx.Commit(ctx)
	}

	// Check state is OPEN
	var state string
	err := pool.QueryRow(ctx, "SELECT state FROM circuit_breakers WHERE tenant_id = $1 AND job_type = $2", tenantID, jobType).Scan(&state)
	if err != nil || state != string(breaker.StateOpen) {
		var tot, unav int
		_ = pool.QueryRow(ctx, "SELECT total_samples, unavailable_failures FROM circuit_breakers WHERE tenant_id = $1 AND job_type = $2", tenantID, jobType).Scan(&tot, &unav)
		t.Fatalf("expected OPEN, got %s (tot=%d, unav=%d)", state, tot, unav)
	}

	// Wait for cooldown
	time.Sleep(1100 * time.Millisecond)

	// Simulate probe job leased and completed successfully
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("failed to begin tx: %v", err)
	}
	// Transition to HALF_OPEN (simulating claim of probe)
	_, _ = tx.Exec(ctx, "UPDATE circuit_breakers SET state = 'HALF_OPEN', probe_in_flight = true WHERE tenant_id = $1 AND job_type = $2", tenantID, jobType)

	// Probe succeeds -> breaker should CLOSE
	err = b.RecordCompletion(ctx, tx, tenantID, jobType)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("RecordCompletion error: %v", err)
	}
	_ = tx.Commit(ctx)

	// Verify breaker closed
	err = pool.QueryRow(ctx, "SELECT state FROM circuit_breakers WHERE tenant_id = $1 AND job_type = $2", tenantID, jobType).Scan(&state)
	if err != nil || state != string(breaker.StateClosed) {
		t.Fatalf("expected breaker state CLOSED after successful probe, got %s", state)
	}
}

func TestBreaker_ClaimQuerySkipsOpenAndAllowsProbe(t *testing.T) {
	pool, tenantID := setupTestDB(t)
	defer pool.Close()
	ctx := context.Background()

	st := store.NewStore(pool)
	b := breaker.New(breaker.BreakerConfig{
		MinSamples:      2,
		FailureRatio:    0.5,
		CooldownSeconds: 2,
	})
	st.Breaker = b

	jobType := "order.shipping"

	// 1. Submit 2 jobs
	for i := 1; i <= 2; i++ {
		_, err := st.SubmitJob(ctx, tenantID, &store.SubmitJobRequest{
			JobType:        jobType,
			Payload:        []byte(`{"item":"book"}`),
			PayloadHash:    "dummy-hash",
			IdempotencyKey: uuid.New().String(),
			Priority:       100,
			RunAt:          time.Now(),
			MaxAttempts:    5,
		}, 24*time.Hour)
		if err != nil {
			t.Fatalf("SubmitJob failed: %v", err)
		}
	}

	// 2. Trip breaker to OPEN
	for i := 0; i < 2; i++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("failed to begin tx: %v", err)
		}
		_ = b.RecordFailure(ctx, tx, tenantID, jobType, "downstream_unavailable", true)
		_ = tx.Commit(ctx)
	}

	// Verify OPEN
	var state string
	_ = pool.QueryRow(ctx, "SELECT state FROM circuit_breakers WHERE tenant_id = $1 AND job_type = $2", tenantID, jobType).Scan(&state)
	if state != "OPEN" {
		t.Fatalf("expected breaker OPEN, got %s", state)
	}

	// 3. ClaimJobs while OPEN (cooldown not elapsed) -> Must skip and claim 0 jobs (A12.1, A12.3)
	claimed, err := st.ClaimJobs(ctx, tenantID, "worker-1", []string{jobType}, 10, 10*time.Second)
	if err != nil {
		t.Fatalf("ClaimJobs error: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("A12.1 violation: expected 0 jobs claimed while OPEN, got %d", len(claimed))
	}

	// 4. Fast-forward opened_at to simulate cooldown elapsed
	_, _ = pool.Exec(ctx, `
		UPDATE circuit_breakers 
		SET opened_at = now() - interval '5 seconds'
		WHERE tenant_id = $1 AND job_type = $2;
	`, tenantID, jobType)

	// 5. Claim probe job -> Exactly 1 job allowed (HALF_OPEN probe)
	claimedProbe, err := st.ClaimJobs(ctx, tenantID, "worker-1", []string{jobType}, 10, 10*time.Second)
	if err != nil {
		t.Fatalf("ClaimJobs probe error: %v", err)
	}
	if len(claimedProbe) != 1 {
		t.Fatalf("expected exactly 1 probe job claimed, got %d", len(claimedProbe))
	}

	// 6. Verify breaker is now HALF_OPEN with probe_in_flight = true
	var probeInFlight bool
	_ = pool.QueryRow(ctx, "SELECT state, probe_in_flight FROM circuit_breakers WHERE tenant_id = $1 AND job_type = $2", tenantID, jobType).Scan(&state, &probeInFlight)
	if state != "HALF_OPEN" || !probeInFlight {
		t.Fatalf("expected HALF_OPEN with probe_in_flight=true, got state=%s in_flight=%v", state, probeInFlight)
	}

	// 7. Another claim while probe in flight -> Must return 0
	claimedDuringProbe, err := st.ClaimJobs(ctx, tenantID, "worker-2", []string{jobType}, 10, 10*time.Second)
	if err != nil {
		t.Fatalf("ClaimJobs during probe error: %v", err)
	}
	if len(claimedDuringProbe) != 0 {
		t.Fatalf("expected 0 jobs claimed while probe in flight, got %d", len(claimedDuringProbe))
	}

	// 8. Probe completes -> Breaker closes automatically (A12.2)
	err = st.CompleteJob(ctx, tenantID, claimedProbe[0].ID, claimedProbe[0].FencingToken, "worker-1", json.RawMessage(`{}`), false)
	if err != nil {
		t.Fatalf("CompleteJob error: %v", err)
	}

	// Verify CLOSED
	_ = pool.QueryRow(ctx, "SELECT state FROM circuit_breakers WHERE tenant_id = $1 AND job_type = $2", tenantID, jobType).Scan(&state)
	if state != "CLOSED" {
		t.Fatalf("A12.2 violation: expected breaker CLOSED after successful probe, got %s", state)
	}

	// 9. Now the remaining job can be claimed
	claimedRemaining, err := st.ClaimJobs(ctx, tenantID, "worker-1", []string{jobType}, 10, 10*time.Second)
	if err != nil {
		t.Fatalf("ClaimJobs remaining error: %v", err)
	}
	if len(claimedRemaining) != 1 {
		t.Fatalf("expected remaining 1 job claimed after close, got %d", len(claimedRemaining))
	}
}
