package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"steadfast/internal/auth"
	"steadfast/internal/store"
)

func TestConcurrentClaimZeroDoubleClaims(t *testing.T) {
	router, s, tenant, validKey, _ := setupTestAPI(t)
	ctx := context.Background()

	// A3.1: 8 concurrent claimers over 5,000 jobs produce zero double-claims (run 3 times)
	for run := 1; run <= 3; run++ {
		t.Run(fmt.Sprintf("run_%d", run), func(t *testing.T) {
			const totalJobs = 5000
			jobType := fmt.Sprintf("claim_task_run_%d", run)

			// Fast bulk insert of 5,000 jobs
			insertQuery := `
			INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, state, priority, run_at, max_attempts)
			SELECT gen_random_uuid(), $1, $2, '{"item":1}'::jsonb, 'hash', 'QUEUED', 100, now(), 5
			FROM generate_series(1, $3);`

			_, err := s.Pool.Exec(ctx, insertQuery, tenant.ID, jobType, totalJobs)
			if err != nil {
				t.Fatalf("Bulk insert failed: %v", err)
			}

			const numWorkers = 8
			var wg sync.WaitGroup
			wg.Add(numWorkers)

			claimedMap := &sync.Map{}
			var totalClaimedCount int64
			var claimMu sync.Mutex

			for w := 0; w < numWorkers; w++ {
				workerID := fmt.Sprintf("worker-claim-%d-%d", run, w)
				go func(wid string) {
					defer wg.Done()
					for {
						bodyStr := fmt.Sprintf(`{"worker_id":"%s","types":["%s"],"max":50}`, wid, jobType)
						req := httptest.NewRequest("POST", "/v1/worker/claim", bytes.NewBufferString(bodyStr))
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("Authorization", "Bearer "+validKey)
						rec := httptest.NewRecorder()

						router.ServeHTTP(rec, req)
						if rec.Code != http.StatusOK {
							t.Errorf("Worker %s received status %d: %s", wid, rec.Code, rec.Body.String())
							return
						}

						var batch []store.ClaimedJob
						if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
							t.Errorf("Worker %s unmarshal failed: %v", wid, err)
							return
						}

						if len(batch) == 0 {
							// No more available jobs
							break
						}

						claimMu.Lock()
						for _, j := range batch {
							if _, loaded := claimedMap.LoadOrStore(j.ID.String(), wid); loaded {
								t.Errorf("A3.1 DOUBLE CLAIM DETECTED: job %s claimed more than once!", j.ID)
							}
							totalClaimedCount++
						}
						claimMu.Unlock()
					}
				}(workerID)
			}

			wg.Wait()

			if totalClaimedCount != totalJobs {
				t.Fatalf("Expected all %d jobs to be claimed, but got %d", totalJobs, totalClaimedCount)
			}
		})
	}
}

func TestCompleteStaleFencingTokenRejected(t *testing.T) {
	router, s, tenant, validKey, _ := setupTestAPI(t)
	ctx := context.Background()

	// Create job in LEASED state with fencing_token = 5
	jobID := uuid.New()
	_, err := s.Pool.Exec(ctx, `
	INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, state, fencing_token, attempts, max_attempts)
	VALUES ($1, $2, 'fencing_test', '{"task":1}', 'hash', 'LEASED', 5, 1, 5);`, jobID, tenant.ID)
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Attempt complete with stale token = 4 -> A3.2: 409 stale_fencing_token and row is unchanged
	bodyStale := `{"fencing_token":4,"result":{"done":true}}`
	req := httptest.NewRequest("POST", fmt.Sprintf("/v1/worker/jobs/%s/complete", jobID), bytes.NewBufferString(bodyStale))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+validKey)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("Expected 409 Conflict on stale token, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify row state is unchanged
	var state string
	var token int64
	err = s.Pool.QueryRow(ctx, "SELECT state, fencing_token FROM jobs WHERE id = $1", jobID).Scan(&state, &token)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if state != "LEASED" || token != 5 {
		t.Fatalf("Expected job to remain LEASED with token 5, got state=%s, token=%d", state, token)
	}
}

func TestCompleteAfterLeaseExpiryBeforeReaper(t *testing.T) {
	router, s, tenant, validKey, _ := setupTestAPI(t)
	ctx := context.Background()

	// A3.3: a completion with the current token after lease_expires_at but before the reaper is accepted
	jobID := uuid.New()
	_, err := s.Pool.Exec(ctx, `
	INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, state, fencing_token, attempts, max_attempts, lease_expires_at)
	VALUES ($1, $2, 'past_expiry_test', '{"task":1}', 'hash', 'LEASED', 3, 1, 5, now() - interval '2 seconds');`,
		jobID, tenant.ID,
	)
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	_, err = s.Pool.Exec(ctx, `
	INSERT INTO job_attempts (job_id, attempt_no, fencing_token, worker_id, started_at)
	VALUES ($1, 1, 3, 'worker_1', now() - interval '5 seconds');`, jobID)
	if err != nil {
		t.Fatalf("Failed to insert attempt: %v", err)
	}

	body := `{"fencing_token":3,"result":{"processed":true}}`
	req := httptest.NewRequest("POST", fmt.Sprintf("/v1/worker/jobs/%s/complete", jobID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+validKey)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("A3.3 violation: expected 200 OK for completion before reaper, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify state is SUCCEEDED
	var state string
	_ = s.Pool.QueryRow(ctx, "SELECT state FROM jobs WHERE id = $1", jobID).Scan(&state)
	if state != "SUCCEEDED" {
		t.Fatalf("Expected job state SUCCEEDED, got %s", state)
	}
}

func TestA34NoNonDBClockComparisons(t *testing.T) {
	// A3.4: code search shows no comparison of a non-DB clock to a lease time
	// Grep Go codebase for any time.Now() compared with lease_expires_at or LeaseExpiresAt
	cmd := exec.Command("git", "grep", "-n", "-E", `time\.Now\(\).*LeaseExpiresAt|LeaseExpiresAt.*time\.Now\(\)|time\.Since.*LeaseExpiresAt`)
	out, err := cmd.CombinedOutput()
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("A3.4 violation: non-DB clock compared with lease time: %s", string(out))
	}
}

func TestWorkerHeartbeat(t *testing.T) {
	router, s, tenant, validKey, _ := setupTestAPI(t)
	ctx := context.Background()

	// 1. Create LEASED job
	jobID := uuid.New()
	_, err := s.Pool.Exec(ctx, `
	INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, state, fencing_token, attempts, max_attempts, lease_expires_at)
	VALUES ($1, $2, 'heartbeat_task', '{"task":1}', 'hash', 'LEASED', 7, 1, 5, now() + interval '5 seconds');`,
		jobID, tenant.ID,
	)
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	// 2. Heartbeat with matching token -> 200 OK
	hbBody := `{"fencing_token":7}`
	req := httptest.NewRequest("POST", fmt.Sprintf("/v1/worker/jobs/%s/heartbeat", jobID), bytes.NewBufferString(hbBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+validKey)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK on valid heartbeat, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. Heartbeat with stale token -> 409 lease_lost
	hbStale := `{"fencing_token":6}`
	reqStale := httptest.NewRequest("POST", fmt.Sprintf("/v1/worker/jobs/%s/heartbeat", jobID), bytes.NewBufferString(hbStale))
	reqStale.Header.Set("Content-Type", "application/json")
	reqStale.Header.Set("Authorization", "Bearer "+validKey)
	recStale := httptest.NewRecorder()
	router.ServeHTTP(recStale, reqStale)

	if recStale.Code != http.StatusConflict {
		t.Fatalf("Expected 409 Conflict on stale token heartbeat, got %d: %s", recStale.Code, recStale.Body.String())
	}
}

func TestFailJobRetryAndPermanent(t *testing.T) {
	router, s, tenant, validKey, _ := setupTestAPI(t)
	ctx := context.Background()

	// A5.2: A permanent error gives DEAD after exactly 1 attempt
	jobID1 := uuid.New()
	_, err := s.Pool.Exec(ctx, `
	INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, state, fencing_token, attempts, max_attempts)
	VALUES ($1, $2, 'permanent_fail', '{"task":1}', 'hash', 'LEASED', 1, 1, 5);`, jobID1, tenant.ID)
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}
	_, _ = s.Pool.Exec(ctx, `INSERT INTO job_attempts (job_id, attempt_no, fencing_token, worker_id, started_at) VALUES ($1, 1, 1, 'w1', now());`, jobID1)

	failPermBody := `{"fencing_token":1,"error_class":"invalid_account","message":"account closed","retryable":false}`
	req1 := httptest.NewRequest("POST", fmt.Sprintf("/v1/worker/jobs/%s/fail", jobID1), bytes.NewBufferString(failPermBody))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer "+validKey)
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("Fail failed: %d: %s", rec1.Code, rec1.Body.String())
	}

	var state1 string
	var reason1 string
	_ = s.Pool.QueryRow(ctx, "SELECT state, dead_reason FROM jobs WHERE id = $1", jobID1).Scan(&state1, &reason1)
	if state1 != "DEAD" || reason1 != "permanent_error" {
		t.Fatalf("A5.2 violation: expected DEAD with permanent_error, got state=%s, reason=%s", state1, reason1)
	}

	// A5.3: one job_attempts row per attempt with full detail
	var outcome1, errClass1, errMsg1 string
	err = s.Pool.QueryRow(ctx, "SELECT outcome, error_class, error_message FROM job_attempts WHERE job_id = $1", jobID1).Scan(&outcome1, &errClass1, &errMsg1)
	if err != nil {
		t.Fatalf("Query attempt failed: %v", err)
	}
	if outcome1 != "failed_permanent" || errClass1 != "invalid_account" || errMsg1 != "account closed" {
		t.Fatalf("A5.3 violation: invalid attempt detail: outcome=%s, class=%s, msg=%s", outcome1, errClass1, errMsg1)
	}

	// Retryable failure below max_attempts -> QUEUED with run_at in future
	jobID2 := uuid.New()
	_, _ = s.Pool.Exec(ctx, `INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, state, fencing_token, attempts, max_attempts) VALUES ($1, $2, 'retry_task', '{"task":2}', 'hash', 'LEASED', 1, 1, 5);`, jobID2, tenant.ID)
	_, _ = s.Pool.Exec(ctx, `INSERT INTO job_attempts (job_id, attempt_no, fencing_token, worker_id, started_at) VALUES ($1, 1, 1, 'w1', now());`, jobID2)

	failRetryBody := `{"fencing_token":1,"error_class":"timeout","message":"upstream 504","retryable":true}`
	req2 := httptest.NewRequest("POST", fmt.Sprintf("/v1/worker/jobs/%s/fail", jobID2), bytes.NewBufferString(failRetryBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+validKey)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("Fail retryable failed: %d: %s", rec2.Code, rec2.Body.String())
	}

	var state2 string
	_ = s.Pool.QueryRow(ctx, "SELECT state FROM jobs WHERE id = $1", jobID2).Scan(&state2)
	if state2 != "QUEUED" {
		t.Fatalf("Expected retryable job to return to QUEUED, got %s", state2)
	}

	// Retryable failure reaching max_attempts -> DEAD with max_attempts_exhausted
	jobID3 := uuid.New()
	_, _ = s.Pool.Exec(ctx, `INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, state, fencing_token, attempts, max_attempts) VALUES ($1, $2, 'exhaust_task', '{"task":3}', 'hash', 'LEASED', 1, 5, 5);`, jobID3, tenant.ID)
	_, _ = s.Pool.Exec(ctx, `INSERT INTO job_attempts (job_id, attempt_no, fencing_token, worker_id, started_at) VALUES ($1, 5, 1, 'w1', now());`, jobID3)

	req3 := httptest.NewRequest("POST", fmt.Sprintf("/v1/worker/jobs/%s/fail", jobID3), bytes.NewBufferString(failRetryBody))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Authorization", "Bearer "+validKey)
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, req3)

	var state3, reason3 string
	_ = s.Pool.QueryRow(ctx, "SELECT state, dead_reason FROM jobs WHERE id = $1", jobID3).Scan(&state3, &reason3)
	if state3 != "DEAD" || reason3 != "max_attempts_exhausted" {
		t.Fatalf("Expected DEAD with max_attempts_exhausted, got state=%s, reason=%s", state3, reason3)
	}
}

func TestDLQEndpoints(t *testing.T) {
	router, s, tenant, validKey, _ := setupTestAPI(t)
	ctx := context.Background()

	// 1. Create a DEAD job
	deadJobID := uuid.New()
	_, err := s.Pool.Exec(ctx, `
	INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, state, dead_reason, last_error, attempts, max_attempts)
	VALUES ($1, $2, 'broken_order', '{"bad_field": true}', 'h_old', 'DEAD', 'permanent_error', 'invalid data', 3, 5);`,
		deadJobID, tenant.ID,
	)
	if err != nil {
		t.Fatalf("Insert dead job failed: %v", err)
	}
	_, _ = s.Pool.Exec(ctx, `INSERT INTO job_attempts (job_id, attempt_no, fencing_token, worker_id, started_at, outcome) VALUES ($1, 1, 1, 'w1', now(), 'failed_permanent');`, deadJobID)

	// 2. GET /v1/dlq
	getReq := httptest.NewRequest("GET", "/v1/dlq", nil)
	getReq.Header.Set("Authorization", "Bearer "+validKey)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /v1/dlq failed: %d: %s", getRec.Code, getRec.Body.String())
	}
	var listResp ListJobsResponse
	_ = json.Unmarshal(getRec.Body.Bytes(), &listResp)
	if len(listResp.Jobs) == 0 {
		t.Fatalf("Expected dead job in dlq list, got 0")
	}

	// 3. A6.2: Edit dead job payload
	patchBody := `{"payload":{"corrected_field": 100}}`
	patchReq := httptest.NewRequest("PATCH", fmt.Sprintf("/v1/dlq/%s", deadJobID), bytes.NewBufferString(patchBody))
	patchReq.Header.Set("Content-Type", "application/json")
	patchReq.Header.Set("Authorization", "Bearer "+validKey)
	patchRec := httptest.NewRecorder()
	router.ServeHTTP(patchRec, patchReq)

	if patchRec.Code != http.StatusOK {
		t.Fatalf("PATCH /v1/dlq/{id} failed: %d: %s", patchRec.Code, patchRec.Body.String())
	}

	// Verify payload was updated
	var patchedPayload string
	_ = s.Pool.QueryRow(ctx, "SELECT payload::text FROM jobs WHERE id = $1", deadJobID).Scan(&patchedPayload)
	if !strings.Contains(patchedPayload, "corrected_field") {
		t.Fatalf("Expected payload to contain corrected_field, got %s", patchedPayload)
	}

	// 4. A6.2: Replay dead job
	replayReq := httptest.NewRequest("POST", fmt.Sprintf("/v1/dlq/%s/replay", deadJobID), nil)
	replayReq.Header.Set("Authorization", "Bearer "+validKey)
	replayRec := httptest.NewRecorder()
	router.ServeHTTP(replayRec, replayReq)

	if replayRec.Code != http.StatusAccepted {
		t.Fatalf("POST /v1/dlq/{id}/replay failed: %d: %s", replayRec.Code, replayRec.Body.String())
	}

	// Verify job returned to QUEUED with attempts reset to 0 and attempt history preserved
	var replayedState string
	var attemptsReset int
	_ = s.Pool.QueryRow(ctx, "SELECT state, attempts FROM jobs WHERE id = $1", deadJobID).Scan(&replayedState, &attemptsReset)
	if replayedState != "QUEUED" || attemptsReset != 0 {
		t.Fatalf("Expected job QUEUED with attempts=0, got state=%s, attempts=%d", replayedState, attemptsReset)
	}

	var attemptsCount int
	_ = s.Pool.QueryRow(ctx, "SELECT count(*) FROM job_attempts WHERE job_id = $1", deadJobID).Scan(&attemptsCount)
	if attemptsCount != 1 {
		t.Fatalf("Expected attempt history to be preserved, found %d attempts", attemptsCount)
	}

	// 5. A6.3: 409 for non-dead jobs on PATCH and replay
	patchReqNonDead := httptest.NewRequest("PATCH", fmt.Sprintf("/v1/dlq/%s", deadJobID), bytes.NewBufferString(patchBody))
	patchReqNonDead.Header.Set("Content-Type", "application/json")
	patchReqNonDead.Header.Set("Authorization", "Bearer "+validKey)
	patchRecNonDead := httptest.NewRecorder()
	router.ServeHTTP(patchRecNonDead, patchReqNonDead)

	if patchRecNonDead.Code != http.StatusConflict {
		t.Fatalf("A6.3 violation: expected 409 Conflict editing non-dead job, got %d", patchRecNonDead.Code)
	}

	replayReqNonDead := httptest.NewRequest("POST", fmt.Sprintf("/v1/dlq/%s/replay", deadJobID), nil)
	replayReqNonDead.Header.Set("Authorization", "Bearer "+validKey)
	replayRecNonDead := httptest.NewRecorder()
	router.ServeHTTP(replayRecNonDead, replayReqNonDead)

	if replayRecNonDead.Code != http.StatusConflict {
		t.Fatalf("A6.3 violation: expected 409 Conflict replaying non-dead job, got %d", replayRecNonDead.Code)
	}
}

func TestCrossTenantWorkerAndDLQIsolation(t *testing.T) {
	router, s, _, keyA, _ := setupTestAPI(t)
	ctx := context.Background()

	// Create Tenant B
	keyB := "tenant_b_key_" + uuid.New().String()
	hashB := auth.HashAPIKey(keyB)
	tenantB, err := s.CreateTenant(ctx, "tenant_b_worker_"+uuid.New().String(), hashB)
	if err != nil {
		t.Fatalf("Create tenant B failed: %v", err)
	}

	// Job owned by Tenant B
	jobB := uuid.New()
	_, err = s.Pool.Exec(ctx, `
	INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, state, fencing_token, attempts, max_attempts)
	VALUES ($1, $2, 'tenant_b_job', '{"data":1}', 'hash', 'LEASED', 1, 1, 5);`,
		jobB, tenantB.ID,
	)
	if err != nil {
		t.Fatalf("Insert tenant B job failed: %v", err)
	}

	// Tenant A tries to heartbeat Tenant B's job -> 404
	reqHB := httptest.NewRequest("POST", fmt.Sprintf("/v1/worker/jobs/%s/heartbeat", jobB), bytes.NewBufferString(`{"fencing_token":1}`))
	reqHB.Header.Set("Content-Type", "application/json")
	reqHB.Header.Set("Authorization", "Bearer "+keyA)
	recHB := httptest.NewRecorder()
	router.ServeHTTP(recHB, reqHB)
	if recHB.Code != http.StatusNotFound {
		t.Fatalf("Expected cross-tenant heartbeat 404, got %d", recHB.Code)
	}

	// Tenant A tries to complete Tenant B's job -> 404
	reqComp := httptest.NewRequest("POST", fmt.Sprintf("/v1/worker/jobs/%s/complete", jobB), bytes.NewBufferString(`{"fencing_token":1}`))
	reqComp.Header.Set("Content-Type", "application/json")
	reqComp.Header.Set("Authorization", "Bearer "+keyA)
	recComp := httptest.NewRecorder()
	router.ServeHTTP(recComp, reqComp)
	if recComp.Code != http.StatusNotFound {
		t.Fatalf("Expected cross-tenant complete 404, got %d", recComp.Code)
	}

	// Tenant A tries to fail Tenant B's job -> 404
	reqFail := httptest.NewRequest("POST", fmt.Sprintf("/v1/worker/jobs/%s/fail", jobB), bytes.NewBufferString(`{"fencing_token":1,"error_class":"err","message":"msg"}`))
	reqFail.Header.Set("Content-Type", "application/json")
	reqFail.Header.Set("Authorization", "Bearer "+keyA)
	recFail := httptest.NewRecorder()
	router.ServeHTTP(recFail, reqFail)
	if recFail.Code != http.StatusNotFound {
		t.Fatalf("Expected cross-tenant fail 404, got %d", recFail.Code)
	}
}


func TestA41WorkerCrashRecovery(t *testing.T) {
	router, s, tenant, validKey, _ := setupTestAPI(t)
	ctx := context.Background()

	// A4.1: killing a worker mid-job leads to completion on another worker within one lease interval plus 1s
	jobID := uuid.New()
	_, err := s.Pool.Exec(ctx, `
	INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, state, priority, run_at, max_attempts)
	VALUES ($1, $2, 'crash_recovery_task', '{"data":1}', 'hash', 'QUEUED', 100, now(), 3);`,
		jobID, tenant.ID,
	)
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	// 1. Worker 1 claims job with 2s lease
	claimed, err := s.ClaimJobs(ctx, tenant.ID, "worker_1", []string{"crash_recovery_task"}, 1, 2*time.Second)
	if err != nil || len(claimed) == 0 {
		t.Fatalf("Worker 1 claim failed: %v", err)
	}
	if claimed[0].ID != jobID || claimed[0].FencingToken != 1 || claimed[0].Attempt != 1 {
		t.Fatalf("Unexpected claimed job: %+v", claimed[0])
	}

	// 2. Worker 1 crashes. Wait 2.2s for lease to expire
	time.Sleep(2200 * time.Millisecond)

	// 3. Reaper runs
	reaped, err := s.ReapExpiredLeases(ctx)
	if err != nil || reaped == 0 {
		t.Fatalf("Reaper failed to reap expired lease: reaped=%d, err=%v", reaped, err)
	}

	// 4. Worker 2 claims the recovered job
	claimed2, err := s.ClaimJobs(ctx, tenant.ID, "worker_2", []string{"crash_recovery_task"}, 1, 2*time.Second)
	if err != nil || len(claimed2) == 0 {
		t.Fatalf("Worker 2 claim failed: %v", err)
	}
	if claimed2[0].ID != jobID || claimed2[0].FencingToken != 2 || claimed2[0].Attempt != 2 {
		t.Fatalf("Expected job reclaimed with token 2 and attempt 2, got: %+v", claimed2[0])
	}

	// 5. Worker 2 completes job
	err = s.CompleteJob(ctx, tenant.ID, jobID, 2, "worker_2", []byte(`{"recovered":true}`), false)
	if err != nil {
		t.Fatalf("Worker 2 completion failed: %v", err)
	}

	// 6. Zombie Worker 1 wakes up and tries to complete with stale token 1 -> 409
	reqZombie := httptest.NewRequest("POST", fmt.Sprintf("/v1/worker/jobs/%s/complete", jobID), bytes.NewBufferString(`{"fencing_token":1}`))
	reqZombie.Header.Set("Content-Type", "application/json")
	reqZombie.Header.Set("Authorization", "Bearer "+validKey)
	recZombie := httptest.NewRecorder()
	router.ServeHTTP(recZombie, reqZombie)

	if recZombie.Code != http.StatusConflict {
		t.Fatalf("Expected zombie worker 1 to get 409 Conflict, got %d", recZombie.Code)
	}

	// Verify job remains SUCCEEDED
	var state string
	_ = s.Pool.QueryRow(ctx, "SELECT state FROM jobs WHERE id = $1", jobID).Scan(&state)
	if state != "SUCCEEDED" {
		t.Fatalf("Expected job to remain SUCCEEDED, got %s", state)
	}
}

func TestA42UncompletedJobReachesDeadAfterMaxAttempts(t *testing.T) {
	_, s, tenant, _, _ := setupTestAPI(t)
	ctx := context.Background()

	// A4.2: a handler that never completes reaches DEAD after max_attempts with reason lease_expired_max_attempts
	jobID := uuid.New()
	_, err := s.Pool.Exec(ctx, `
	INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, state, priority, run_at, max_attempts)
	VALUES ($1, $2, 'never_completes', '{"data":1}', 'hash', 'QUEUED', 100, now(), 2);`,
		jobID, tenant.ID,
	)
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	// Attempt 1: Claim with 1s lease
	c1, err := s.ClaimJobs(ctx, tenant.ID, "w1", []string{"never_completes"}, 1, 1*time.Second)
	if err != nil || len(c1) == 0 {
		t.Fatalf("Attempt 1 claim failed: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	reaped1, err := s.ReapExpiredLeases(ctx)
	if err != nil || reaped1 != 1 {
		t.Fatalf("Reaper 1 failed: reaped=%d, err=%v", reaped1, err)
	}

	// Attempt 2 (reaches max_attempts=2): Claim with 1s lease
	c2, err := s.ClaimJobs(ctx, tenant.ID, "w2", []string{"never_completes"}, 1, 1*time.Second)
	if err != nil || len(c2) == 0 {
		t.Fatalf("Attempt 2 claim failed: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	reaped2, err := s.ReapExpiredLeases(ctx)
	if err != nil || reaped2 != 1 {
		t.Fatalf("Reaper 2 failed: reaped=%d, err=%v", reaped2, err)
	}

	// Verify job reached DEAD with lease_expired_max_attempts
	var state, deadReason string
	var attempts int
	err = s.Pool.QueryRow(ctx, "SELECT state, dead_reason, attempts FROM jobs WHERE id = $1", jobID).Scan(&state, &deadReason, &attempts)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}

	if state != "DEAD" || deadReason != "lease_expired_max_attempts" || attempts != 2 {
		t.Fatalf("A4.2 violation: expected DEAD with lease_expired_max_attempts, got state=%s, reason=%s, attempts=%d", state, deadReason, attempts)
	}
}
