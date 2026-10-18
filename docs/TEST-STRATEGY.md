# Steadfast: Test Strategy & Traceability Matrix

Status: Approved Test Strategy (Prompt 02)

---

## 1. Test Architecture & Principles

Steadfast tests must prove reliability under real failure modes. In accordance with `.agents/rules/20-quality-process.md`:
1. **Real PostgreSQL in Docker**: Durability and concurrency tests run against real PostgreSQL on port `5433` (`cmd/ctl test` manages the instance), never in-memory mocks.
2. **Deterministic Concurrency**: Fixed seeds for pseudo-random workloads; concurrent tests run at least 3 consecutive times before green status.
3. **Bounded Timing**: Leases and backoffs use accelerated demo timing (1s to 3s leases in unit/integration tests) rather than long sleeps. Full suite completes in under 2 minutes.
4. **No Flaky Tests**: Any flaky test is treated as an engine bug, not retried until green.

---

## 2. Traceability Matrix: SPEC Features (F1–F14)

| Feature | Description | Target Prompt | Test Type | Test Identifier | Acceptance Verification |
|---|---|---|---|---|---|
| **F1** | Durable job submission | Prompt 04 | Integration | `TestSubmit_DurabilityPostgresKill` | Submit job, hard-kill Postgres container, restart, verify job exists and is recoverable. |
| **F2** | Idempotent submission | Prompt 04 | Integration | `TestSubmit_IdempotencyKeyDeduplication` | Submit identical (tenant, key) concurrently; verify 1 job created, identical 202 response. |
| **F3** | Lease-based dispatch with fencing | Prompt 05 | Integration | `TestWorker_FencingTokenIncrement` | Verify claim atomically increments fencing token; stale token returns 409 Conflict. |
| **F4** | Reaper daemon | Prompt 05 | Integration | `TestReaper_ReclaimAndDeadLetter` | Expired leased jobs re-enqueue; jobs with attempts >= max_attempts move to DEAD. |
| **F5** | Retries, backoff, error classes | Prompt 05 | Unit / Int | `TestBackoff_ExponentialSchedule` | Verify retry schedule matches backoff curve with jitter; non-retryable errors fail immediately. |
| **F6** | DLQ inspect, edit, replay | Prompt 05 | Integration | `TestDLQ_InspectEditReplay_CrossTenant` | Inspect DLQ payload, update payload, replay to QUEUED; verify cross-tenant returns 404. |
| **F7** | Go worker SDK & handler | Prompt 06 | SDK Int | `TestSDK_ClaimProcessComplete` | SDK claim loop processes job, maintains background heartbeat, and completes successfully. |
| **F8** | Mock downstream ledger | Prompt 06 | Component | `TestLedger_IdempotencyAndFaultInjection` | Test SQLite unique idempotency key; test `apply_then_fail` mode returns 500 but avoids double charge. |
| **F9** | Chaos controls | Prompt 08 | Chaos Int | `TestChaos_ContainerKillAllowlist` | Verify container kills restricted to allowlist; refuses execution when STEADFAST_ENV != demo. |
| **F10** | Loadgen, baseline, verifier | Prompt 07 | E2E Harness | `TestVerifier_DetectsBaselineLoss` | Run loadgen against Steadfast and Redis baseline; verifier passes Steadfast and fails baseline. |
| **F11** | Live dashboard & demo console | Prompt 09 | E2E Browser | `TestDashboard_SSEStreaming` | Verify SSE stream broadcasts live state; presenter mode `?present=1` formats metrics. |
| **F12** | Per-job-type circuit breaker | Prompt 10 (P1) | Component | `TestBreaker_TripAndCooldown` | Error ratio > 0.5 trips breaker; rejects new submissions with 503; recovers after cooldown. |
| **F13** | Graceful shutdown & rate limit | Prompt 11 (P1) | Component | `TestRateLimit_TokenBucketPerKey` | Rate limit caps requests at 5000 RPS per key; SIGTERM drains in-flight jobs within grace period. |
| **F14** | Stretch features | Cut line | - | - | Deferred per P2 cut rule. |

---

## 3. Traceability Matrix: Invariants (I1–I4)

| Invariant | Statement | Target Prompt | Verifier Audit Routine | Verification Evidence |
|---|---|---|---|---|
| **I1** | No accepted job is lost | Prompt 07 | `AuditInvariant_I1_NoLostJobs` | Evaluates after drain condition: `acks == (succeeded + dead)`. Zero missing jobs. |
| **I2** | No duplicate downstream side effects | Prompt 07 | `AuditInvariant_I2_NoDuplicateCharges` | Queries SQLite ledger: `COUNT(*) == COUNT(DISTINCT idempotency_key)`. Zero duplicates. |
| **I2b** | Set-based correlation: every succeeded payment is charged | Prompt 07 | `AuditInvariant_I2b_SetCorrelation` | Symmetric difference join on `(tenant_id, job_id)` between PostgreSQL and SQLite charges. |
| **I3** | Stale fencing tokens are rejected | Prompt 08 | `AuditInvariant_I3_ZombieRejection` | Chaos zombie test: late worker completion rejected with 409; ledger duplicate charges == 0. |
| **I4** | Verifier detects injected faults (tests tester) | Prompt 08 | `AuditInvariant_I4_MutationCheck` | With `STEADFAST_DISABLE_FENCING=true`, verifier must detect violation and output FAIL. |
