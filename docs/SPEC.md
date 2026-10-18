# Steadfast: product spec (v1, hackathon prototype)

Status: DRAFT for approval. Built by one Antigravity agent, in sequence, with 12-24 hours available. Demo runs locally with `docker compose` (no cloud deployment).

Plain-language terms used in this spec (defined once):
- **Lease**: a time-limited claim a worker holds on a job. If the worker dies, the lease expires and another worker may take the job.
- **Fencing token**: a number that goes up every time a job is claimed. The database only accepts a result carrying the newest number, so a "zombie" (a worker that was presumed dead but wakes up later) cannot overwrite newer work.
- **Idempotency key**: a unique label on a request. Repeating a request with the same label returns the original result instead of doing the work twice.
- **DLQ (dead-letter queue)**: where jobs go when they cannot succeed. Nothing is silently dropped.
- **Invariant**: a statement that must always be true, for example "no accepted job is lost". The checker verifies each one.
- **Baseline**: a plain Redis-backed queue we run next to Steadfast to show what typical setups do under the same failures.

---

## 1. Summary

Steadfast is a small, self-hostable asynchronous job runner. A client submits a job over REST; Steadfast stores it durably, hands it to a worker under a lease, retries failures with backoff, and moves unrecoverable jobs to a dead-letter queue.

The product is not only the queue. It is **proof**: a built-in chaos controller (kills workers and the database on demand) and an **independent verifier** that checks, from records the system cannot influence, that every accepted job ended either completed or visibly dead-lettered, and that no customer was charged twice. The same chaos script runs against a plain Redis queue side by side, with live counters: "customers double-charged: N ($X), orders lost: M" versus "0 and 0".

Audience: hackathon judges (Cloud track). Success = a flawless 90-second to 4-minute live demo plus defensible claims.

Three sentences every claim must reduce to:
1. A job we accept is either completed or visibly dead-lettered. Never silently lost.
2. Effects are effectively-once because of at-least-once delivery + idempotency keys + fencing tokens + a downstream that honours the key.
3. An independent checker, not our dashboard, proves it while we kill workers and the database.

## 2. Users and roles

| Role | Who | May | May not |
|---|---|---|---|
| Tenant client | A service submitting jobs (the checkout app / load generator) | Submit jobs, read its own jobs, read its own DLQ | See any other tenant's data (a foreign job ID returns 404, never 403) |
| Worker | A process running handlers (Go SDK) | Claim, heartbeat, complete, fail jobs of its own tenant | Read or write jobs it does not currently hold a lease on |
| Operator | Human at the dashboard | View queue, inspect DLQ, edit a DLQ payload, replay, trigger chaos and the demo | Use chaos controls outside demo mode |
| Verifier | The independent checker | Read-only access to `jobs` and `job_attempts`; read ledger export; read the load generator's ack files | Write anything to Steadfast |

Scale: single laptop; 2 seeded demo tenants (`shop-a` is the demo tenant; `shop-b` exists to prove tenant isolation). Up to 10,000 jobs per demo run.

## 3. Features, in priority order

Priority tags: **P0** = must ship (the winning core). **P1** = ship only after P0 is demo-ready. **P2** = cut unless time is left. Cut order when time runs short: P2 first, then P1 in reverse listed order. P0 is never cut.

### F1 (P0) Durable job submission
- Story: a client submits a job and, once told "accepted", can rely on it.
- Trigger: `POST /v1/jobs`.
- Result: `202` returned only after the row is committed with `synchronous_commit = on` and `fsync = on`.
- Acceptance:
  - A1.1 Kill -9 of the Postgres container immediately after a 202 never loses that job (proven by test: submit, kill, restart, `GET` returns the job).
  - A1.2 If the database is unreachable, the API returns `503` with `Retry-After`, never `202`.
  - A1.3 Payload over 64 KiB, invalid `type`, invalid JSON, `max_attempts` outside 1-20 return `400` with a generic error body.

### F2 (P0) Idempotent submission
- Rules: key scoped per tenant; stored with a SHA-256 hash of the canonical payload; TTL 24 hours; repeat with same key and same payload returns the original job ID (still `202`, header `Idempotent-Replayed: true`); same key with a different payload returns `409`.
- Acceptance:
  - A2.1 50 concurrent identical submissions with one key create exactly 1 job (concurrency test).
  - A2.2 Same key, different payload returns `409 idempotency_key_conflict`.
  - A2.3 Same key under tenant `shop-b` does not collide with `shop-a`.
  - A2.4 After the TTL (test with a shortened TTL setting) the key may be reused.

### F3 (P0) Lease-based dispatch with fencing
- Workers claim batches via `FOR UPDATE SKIP LOCKED`; each claim increments `fencing_token` and `attempts` and sets `lease_expires_at` using database time.
- Workers heartbeat to extend the lease; every heartbeat and completion carries the fencing token.
- Acceptance:
  - A3.1 Two workers claiming concurrently never receive the same job (test with 8 workers, 5,000 jobs: zero double-claims).
  - A3.2 A completion with a stale token returns `409 stale_fencing_token` and changes nothing.
  - A3.3 A completion with the current token is accepted even if `lease_expires_at` has passed, as long as the reaper has not requeued the job (the token is the authority, not the clock).
  - A3.4 No worker-side code compares its own clock to a lease time. Database `now()` is the only clock.

### F4 (P0) Reaper
- Every 1 second (configurable) a single active reaper (guarded by a Postgres advisory lock) finds expired leases: if `attempts >= max_attempts` the job goes to `DEAD` (reason `lease_expired_max_attempts`), otherwise back to `QUEUED`.
- Acceptance:
  - A4.1 Killing a worker mid-job results in the job completing on another worker within one lease interval plus 1 second.
  - A4.2 A "crash-loop" job (handler that kills its worker every time) reaches `DEAD` after `max_attempts` and does not cycle forever.
  - A4.3 Two reaper instances running together do not double-requeue (advisory lock test).

### F5 (P0) Retries, backoff, error classes
- Worker reports failure as `retryable` or `permanent`.
- Retry delay = random value between 0 and `min(cap, base * 2^(attempt-1))` ("full jitter"), computed once per failure; stored in `run_at`.
- Permanent errors and exhausted attempts go straight to `DEAD` with full error history in `job_attempts`.
- Acceptance:
  - A5.1 Unit test proves delays stay within bounds and are not identical across 1,000 samples.
  - A5.2 A permanent error produces `DEAD` after exactly 1 attempt.
  - A5.3 `job_attempts` has one row per attempt with outcome, error class, error message, worker, fencing token.

### F6 (P0) Dead-letter queue: inspect, edit, replay
- `GET /v1/dlq`, `PATCH /v1/dlq/{id}` (replace payload, recompute hash), `POST /v1/dlq/{id}/replay` (state back to `QUEUED`, `attempts = 0`, same job ID and same idempotency key, history preserved).
- Every operator action writes an audit row naming the actor.
- Acceptance:
  - A6.1 Pre-seeded poison job appears in the DLQ after `make up` + seed.
  - A6.2 Edit + replay of the poison job leads to `SUCCEEDED` and exactly 1 ledger entry.
  - A6.3 Editing or replaying a job that is not `DEAD` returns `409`.

### F7 (P0) Go worker SDK and example handler
- Package `worker`: `Run(ctx, Config)`, handler map by job type, concurrency setting, automatic heartbeats, abort of a handler when a heartbeat returns `409 lease_lost`, classification helpers `worker.Retryable(err)` and `worker.Permanent(err)`.
- Example handler `charge_customer` calls the mock ledger with `Idempotency-Key: steadfast:<job_id>` (stable across attempts) and header `X-Fencing-Token: <token>` (recorded by the ledger; not part of the key, because a key that changes per attempt would defeat deduplication).
- Acceptance: A7.1 an example worker processes jobs end to end using only the SDK; A7.2 workers hold no database credentials (they talk to the API only).

### F8 (P0) Mock downstream ledger (fake payment API)
- Separate container with its own storage (SQLite on its own volume). Survives Steadfast database kills.
- `POST /charge` {order_id, customer_id, amount_cents}; optional `Idempotency-Key`. With a key already applied it returns the original result and records a `suppressed` entry. Without a key it always applies (this is how real non-idempotent APIs behave).
- Fault injection via `POST /admin/fault`: `none`, `error_rate` (p), `latency_ms`, `down`, and **`apply_then_fail` (p)**: the charge is applied, then the response is an error or timeout. This is the real mechanism of double charges, so it is part of the demo.
- `GET /ledger/export` streams every entry as NDJSON (applied and suppressed) with `order_id`, `job_id`, `idempotency_key`, `amount_cents`, `fencing_token`, `received_at`.
- Acceptance: A8.1 idempotent replay returns the same response body; A8.2 `apply_then_fail` with no key produces duplicates (test); with a key it does not.

### F9 (P0) Chaos controls
- A `chaos` service exposes allow-listed actions only: `kill-workers {fraction}`, `kill-postgres`, `start-postgres`, `kill-redis`, `start-redis`, `ledger-fault {mode...}`, `clear-faults`. Also available as `make chaos-*` scripts that call the same code.
- Works only when `STEADFAST_ENV=demo`. Bound to 127.0.0.1. Requires `ADMIN_KEY`.
- Acceptance: A9.1 each action works from both button and script; A9.2 a non-allow-listed container name is refused; A9.3 the service refuses to start unless `STEADFAST_ENV=demo`.

### F10 (P0) Load generator, baseline, independent verifier
- **Load generator** (`loadgen`): submits N checkout jobs (default 10,000; seeded random amounts 500-50,000 cents; reproducible with `--seed`) to Steadfast and baseline at the same rate. Records every acknowledged job in its own append-only NDJSON file under `./artifacts/`, outside both systems. Retries failed submissions up to 2 times with the same body (and, for Steadfast, the same idempotency key) in both lanes.
- **Baseline** (`baseline-api`, `baseline-worker`): Redis list queue, `appendonly yes` with `appendfsync everysec` (a common production default), reliable-queue pattern with a visibility timeout (default 10 s), no fencing, no idempotency key sent downstream, retries on failure, exhausted jobs to a `failed` list (counted as failed, not lost). Its exact configuration is printed on a slide and in the README. Alternate mode `BASELINE_PATTERN=ack_on_pull` (pop and process) exists for a bonus clip. Flag `BASELINE_SEND_IDEMPOTENCY_KEY=true` runs the third lane "baseline + key" (still loses acknowledged jobs on a Redis hard kill; shows the difference is more than keys).
- **Verifier**: one binary that reads (a) the load generator ack files, (b) the ledger export over HTTP, (c) Steadfast Postgres through a read-only role; waits for drain (no `QUEUED`/`LEASED` jobs, or timeout); prints a human report and writes `artifacts/verdict.json`. It never reads dashboard streams.
- Acceptance: see section 8 (invariants I1-I4) and Appendix C.

### F11 (P0) Live dashboard and demo console
- Panels: queue depth, throughput (jobs/s over time), latency p50/p99, retries, DLQ count, worker count, per-system "stakes" counters (from the verifier output or the ledger export, not from Steadfast), invariant status (displays `artifacts/verdict.json`; does not compute it), circuit breaker state (when F12 exists).
- When Postgres is down the Steadfast panel shows "DATABASE DOWN" and keeps rendering the baseline panel. This is a feature: the audience sees the outage.
- Controls: "Run demo" (scripted four beats), per-beat buttons, kill buttons, DLQ drawer (inspect history, edit payload, replay), operator mode (`?operator=1`) with beat timer and fallback triggers.
- No external network at runtime: all JS/CSS/fonts vendored in the repo.
- Acceptance: A11.1 works with the Wi-Fi off; A11.2 browser bundle contains no API keys (the dashboard server adds credentials); A11.3 a full demo run needs one click on "Run demo".

### F12 (P1) Per-job-type circuit breaker
- Per (tenant, job type): `CLOSED` -> `OPEN` when at least 10 recent outcomes and at least 50% are `downstream_unavailable` failures; `OPEN` for a cooldown (demo default 10 s); then `HALF_OPEN`: exactly one probe job claimed at a time; probe success closes, failure reopens. State lives in Postgres and is updated in the same transaction as the complete/fail call. While `OPEN`, the claim query skips that type.
- Acceptance: A12.1 with the ledger set to `error_rate=1.0` the breaker opens within 15 seconds and dispatch for that type pauses; A12.2 after clearing the fault it closes without operator action; A12.3 no retry storm: attempts per second for that type drop while `OPEN` (measured in a test).

### F13 (P1) Graceful shutdown and rate limit
- SIGTERM on a worker: stop claiming, finish in-flight jobs (up to 25 s), exit. SIGTERM on the API: stop accepting, finish in-flight requests.
- Per-key in-process token-bucket limit on `POST /v1/jobs` (default high enough not to throttle the demo; configurable).
- Acceptance: A13.1 draining a worker loses and duplicates nothing; A13.2 exceeding the limit returns `429` with `Retry-After`.

### F14 (P2) Stretch
OpenTelemetry trace to Jaeger; per-tenant round-robin fairness (and invariant I5); standalone verifier docs; Python SDK; outbox + webhooks. Each is cut by default. Do not claim any of them in slides unless built and tested.

## 4. Screens and flows

1. **Dashboard (single page, two columns).** Left column "Steadfast", right column "Baseline". Each column: throughput graph, queue depth, p50/p99, retries, DLQ (Steadfast only), and a large stakes counter ("double-charged: N ($X) / orders lost: M"). Bottom strip: invariant verdict (I1-I4 with pass/fail), beat timer, "Run demo" button, chaos buttons (operator mode).
2. **DLQ drawer.** Opens from the DLQ count. Lists dead jobs; selecting one shows error history per attempt, an editable JSON payload box with validation, and a "Replay" button. Result shows the job moving to `SUCCEEDED`.
3. **Verdict screen.** Full-screen view of the verifier output: counts (submitted, acknowledged, succeeded, dead, applied charges, duplicates, dollars), per-invariant result, and the baseline configuration summary.

Demo flow (four beats; the exact choreography is Appendix D): Load -> Crash -> Failure and poison -> Verdict.

## 5. Data model (high level)

Sensitivity levels: public, internal, personal, sensitive. All demo data is synthetic. No real card data exists anywhere in the system.

| Entity | Key fields | Sensitivity |
|---|---|---|
| `tenants` | id, name, `api_key_hash` (SHA-256 of a 32-byte random key), created_at | api_key_hash: sensitive; rest internal |
| `jobs` | id, tenant_id, job_type, payload (jsonb), payload_hash, idempotency_key, state (`QUEUED`,`LEASED`,`SUCCEEDED`,`DEAD`), priority, run_at, attempts, max_attempts, fencing_token, worker_id, lease_expires_at, last_error, dead_reason, result, created_at, updated_at, finished_at | payload: internal (synthetic customer ids and amounts); never logged |
| `idempotency_keys` | tenant_id, key, payload_hash, job_id, created_at, expires_at (primary key: tenant_id + key) | internal |
| `job_attempts` | id, job_id, attempt_no, fencing_token, worker_id, started_at, finished_at, outcome (`succeeded`,`failed_retryable`,`failed_permanent`,`lease_expired`), error_class, error_message | internal |
| `audit_log` | id, ts, tenant_id, actor, action, job_id, from_state, to_state, details | internal; append-only |
| `breaker` (P1) | tenant_id, job_type, state, opened_at, window counters | internal |
| Ledger (separate store) | id, order_id, job_id, idempotency_key, amount_cents, fencing_token, status (`applied`,`suppressed`), received_at | internal |
| Load generator ack file (outside the system) | order_id, job_id, amount_cents, ack_at, lane | internal |

Relationships: tenant 1-N jobs; job 1-N job_attempts; job 0-1 idempotency_keys row; audit rows reference jobs. State transitions are audited by a database trigger so that no code path (API, reaper, operator) can skip it; operator actions additionally record the actor.

## 6. AI behavior

None in v1. No model, no prompts, no AI keys. The playbook's "LLM-assisted DLQ triage" is cut (see section 10). Consequence: the AI-specific security baseline (prompt injection, model cost caps) does not apply, and slides must not mention AI features.

## 7. Integrations

| Integration | Purpose | Notes |
|---|---|---|
| Mock ledger (own container, SQLite) | Stands in for a payment API | Our own code; the only "downstream" |
| Redis | Baseline queue only | Steadfast does not depend on Redis (decision D1) |
| Docker Engine socket | Chaos controller kills/starts containers | Demo-only; see security notes |
| None else | No email, SMS, payments, analytics, cloud services | Everything runs offline after images are built |

## 8. Non-functional requirements

**Security (hackathon-scope, stated honestly; nothing here makes the system "fully secure")**
1. API keys: 32 random bytes, shown once at seed time via local env file, stored only as SHA-256 hashes (keys are high-entropy so a fast hash is acceptable); lookup by hash; no key in logs, URLs, payloads, or the browser bundle.
2. Tenant isolation: every SQL statement that touches `jobs`, `idempotency_keys`, `job_attempts` takes `tenant_id` from the authenticated key, never from the request body. Foreign IDs return 404. Automated cross-tenant tests are mandatory.
3. Parameterized queries only (pgx placeholders). JSON bodies decoded strictly with size limits (64 KiB payload, 1 MiB request cap).
4. Secrets only in environment variables; `.env.example` has fake values; `.env` is git-ignored; secret scan (gitleaks) in CI.
5. Containers run as non-root where the image allows; Postgres and Redis ports bound to 127.0.0.1 only; dashboard and chaos bound to 127.0.0.1; state-changing dashboard/chaos requests check `Origin` and require the admin credential held server-side.
6. The chaos service mounts the Docker socket, which is effectively root on the host. Risk accepted for a local demo, mitigated by allow-list, localhost bind, `STEADFAST_ENV=demo` guard. README states: never deploy it.
7. The verification switches `STEADFAST_DISABLE_FENCING` and `STEADFAST_DISABLE_IDEMPOTENCY` exist only to test the tester. They are honored only when `STEADFAST_ENV=demo`, and the process logs a loud warning on start.
8. Errors returned to clients are generic with an error code; detail goes to logs. Logs never contain payloads, keys, or connection strings.
9. Audit log records every state transition (database trigger) and every operator action (actor named).
10. Dependencies pinned with lockfile; `govulncheck` and a static analyzer run in CI; no new high-severity finding at submission.
Not in v1 (roadmap, say so on the slide): row-level security, payload encryption at rest, TLS inside the compose network, key rotation.

**Availability and durability**
- Tier A (always demoed): an acknowledged job survives worker crash, API crash, and hard kill of the Postgres container (named volume, `fsync=on`, `synchronous_commit=on`, `full_page_writes=on`, set explicitly in compose).
- Tier B (failover with zero acknowledged-job loss) is NOT claimed. It would need synchronous replication and costs latency. It is roadmap only.
- During a database outage: API returns 503; workers retry with backoff and discard results rejected with 409; nothing is acknowledged that was not committed.

**Performance**: Targets are stretch, not promises. Only numbers produced by `make bench` on the demo laptop (hardware recorded automatically in `results/results.json`) may appear in README or slides: submission latency p99, sustained jobs/s on a single Postgres, lost acknowledged jobs under chaos (target 0), duplicate side effects (target 0), recovery after worker crash, recovery after Postgres restart.

**Observability**: structured JSON logs with request IDs; `/healthz` (process alive), `/readyz` (database reachable); dashboard reads the database through a read-only role.

**Environments**: Linux or macOS or Windows with Docker Desktop/Engine and Compose v2; modern Chromium-based browser for the dashboard. Exact demo laptop is an open question (section 11).

**Accessibility**: dashboard text contrast and a large-font presenter mode (`?present=1`) so counters are legible on a projector. Not a full accessibility audit.

## 9. Compliance and legal notes

All data is synthetic; no real card data, no real personal data; the "payment API" is fake, so card-payment rules do not apply. If this is ever pointed at real customer data, privacy law (for example India's DPDP Act, GDPR) and payment rules apply and need a lawyer plus the security items listed as "not in v1". Repository license: MIT (to be confirmed, see D12).

## 10. Out of scope for version 1

Node/Python SDKs (Python is P2), Helm chart, KEDA autoscaling, DAG workflows, cron and delayed-job UI, weighted fair queuing, row-level security, payload encryption, partitioning, read replicas, Patroni failover (Tier B), LLM triage, outbox + webhooks (P2), OpenTelemetry/Jaeger (P2), per-tenant fairness and invariant I5 (P2), multi-region, user accounts and login screens (operators use the demo console on localhost).

## 11. Open questions and decisions

Status: approved by delegation. The team lead (Claude, acting at the user's request) owns decisions D1-D16 (Appendix H and Appendix J). Resolved since the draft:
- Demo laptop = Windows, 16 GB RAM -> Appendix J.
- Pitch slot unknown -> decision D14 (default demo profile is `short`; `full` is the backup-video and extended-slot version).
- Judging rubric unknown -> decision D15.

Only open item (does not block the build): **O1** the official rubric text. One teammate finds it on the hackathon page or asks the organizers. Effect: slides and pitch wording only, unless the rubric demands a feature we cut.

---

# Appendix A: API contract (base path `/v1`)

Auth: `Authorization: Bearer <tenant key>`. Errors: `{"error":{"code":"...","message":"..."}}` (generic message). All timestamps RFC 3339 UTC.

| Method and path | Who | Success | Errors |
|---|---|---|---|
| `POST /jobs` body `{type, payload, priority?, run_at?, max_attempts?}`; header `Idempotency-Key` | client | `202 {id, state:"QUEUED", created_at}`; replay adds header `Idempotent-Replayed: true` | `400` invalid, `401`, `409 idempotency_key_conflict`, `429`, `503` (+`Retry-After`) |
| `GET /jobs/{id}` | client | `200` full job + attempts | `401`, `404` |
| `GET /jobs?state=&limit=&cursor=` | client | `200` page | `400`, `401` |
| `GET /dlq` | client/operator | `200` dead jobs with last error | `401` |
| `PATCH /dlq/{id}` body `{payload}` | operator | `200` | `404`, `409` not DEAD, `400` |
| `POST /dlq/{id}/replay` | operator | `202` | `404`, `409` not DEAD |
| `POST /worker/claim` body `{worker_id, types[], max}` | worker | `200 [{id,type,payload,fencing_token,attempt,max_attempts,lease_seconds}]` (empty list if none) | `401`, `503` |
| `POST /worker/jobs/{id}/heartbeat` body `{fencing_token}` | worker | `200` | `409 lease_lost` |
| `POST /worker/jobs/{id}/complete` body `{fencing_token, result?}` | worker | `200` | `409 stale_fencing_token` |
| `POST /worker/jobs/{id}/fail` body `{fencing_token, error_class, message, retryable}` | worker | `200 {next_state, run_at?}` | `409 stale_fencing_token` |
| `GET /healthz`, `GET /readyz` (no auth) | probes | `200` | `503` when DB down |

Behavior notes: `202` for submit is sent only after COMMIT returns. Heartbeat extends the lease from database `now()`. Fail: retryable and `attempts < max_attempts` -> `QUEUED` with `run_at = now() + jittered delay`; otherwise `DEAD` with `dead_reason` (`permanent_error` or `max_attempts_exhausted`). Complete and fail also close the matching `job_attempts` row in the same transaction. Replay keeps the job ID and idempotency key.

# Appendix B: reference SQL (starting point; agent must write tests proving each behavior)

Claim a batch (one transaction; also opens the attempt row). For F12, add a condition excluding job types whose breaker is `OPEN`, and limit `HALF_OPEN` types to one leased job.
```sql
WITH picked AS (
  SELECT id FROM jobs
  WHERE tenant_id = $1 AND state = 'QUEUED' AND run_at <= now() AND job_type = ANY($2)
  ORDER BY priority, run_at, id
  LIMIT $3
  FOR UPDATE SKIP LOCKED
), claimed AS (
  UPDATE jobs j SET state = 'LEASED', worker_id = $4,
         lease_expires_at = now() + make_interval(secs => $5),
         fencing_token = j.fencing_token + 1, attempts = j.attempts + 1, updated_at = now()
  FROM picked WHERE j.id = picked.id
  RETURNING j.id, j.job_type, j.payload, j.fencing_token, j.attempts, j.max_attempts
), opened AS (
  INSERT INTO job_attempts (job_id, attempt_no, fencing_token, worker_id, started_at)
  SELECT id, attempts, fencing_token, $4, now() FROM claimed RETURNING job_id
)
SELECT * FROM claimed;
```
Heartbeat (0 rows -> `409 lease_lost`):
```sql
UPDATE jobs SET lease_expires_at = now() + make_interval(secs => $4), updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND fencing_token = $3 AND state = 'LEASED';
```
Complete (0 rows -> `409 stale_fencing_token`; result discarded):
```sql
UPDATE jobs SET state = 'SUCCEEDED', result = $5, finished_at = now(), lease_expires_at = NULL, updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND fencing_token = $3 AND worker_id = $4 AND state = 'LEASED';
```
Reaper (corrected from the playbook: exhausted jobs go to DEAD, not back to the queue; the token is not changed here, the next claim increments it):
```sql
WITH expired AS (
  SELECT id FROM jobs WHERE state = 'LEASED' AND lease_expires_at < now() FOR UPDATE SKIP LOCKED
), moved AS (
  UPDATE jobs j SET
    state = CASE WHEN j.attempts >= j.max_attempts THEN 'DEAD' ELSE 'QUEUED' END,
    dead_reason = CASE WHEN j.attempts >= j.max_attempts THEN 'lease_expired_max_attempts' END,
    finished_at = CASE WHEN j.attempts >= j.max_attempts THEN now() END,
    worker_id = NULL, lease_expires_at = NULL, run_at = now(), updated_at = now()
  FROM expired e WHERE j.id = e.id
  RETURNING j.id, j.fencing_token
)
UPDATE job_attempts a SET finished_at = now(), outcome = 'lease_expired'
FROM moved m WHERE a.job_id = m.id AND a.fencing_token = m.fencing_token AND a.outcome IS NULL;
```
Idempotent submit (one transaction; job ID generated in the app): insert into `idempotency_keys` with `ON CONFLICT (tenant_id, key) DO UPDATE ... WHERE idempotency_keys.expires_at < now()` (replaces only an expired key). If no row is returned the key is live: read it, compare `payload_hash`; equal -> return stored `job_id`; different -> `409`. Otherwise insert the job in the same transaction, then COMMIT, then answer 202.

Indexes: partial index on `jobs (tenant_id, priority, run_at, id) WHERE state = 'QUEUED'`; partial index on `jobs (lease_expires_at) WHERE state = 'LEASED'`; primary key on `idempotency_keys (tenant_id, key)`; `job_attempts (job_id)`.

# Appendix C: invariants and the verifier

Definitions: *submitted* = attempted by loadgen; *acknowledged* = loadgen received `202` (Steadfast) or a Redis enqueue reply (baseline); both recorded in the loadgen's own ack file before any other component can see them. *Applied charge* = ledger entry with status `applied`.

| # | Invariant (Steadfast lane) | Check |
|---|---|---|
| I1 | No acknowledged job is lost | every acknowledged `job_id` exists in Postgres in `SUCCEEDED` or `DEAD` after drain |
| I2 | No duplicate side effects | for each `order_id`, applied charges <= 1 |
| I2b | No phantom success | every `SUCCEEDED` job has exactly 1 applied charge for its order |
| I3 | Totals reconcile | acknowledged = succeeded + dead-lettered (acknowledged jobs only; committed-but-unacknowledged extras are reported separately) |
| I4 | Zombie writes rejected | scripted test: worker A claims (token n), is paused past the lease, reaper requeues, worker B claims (token n+1) and completes, A resumes and completes with token n -> `409`; job stays `SUCCEEDED`; ledger shows 1 applied entry |

Baseline lane report (same verifier, same ledger, plus read-only access to the baseline `failed` list so exhausted jobs count as failed, not lost): orders lost = acknowledged orders with no applied charge and not in `failed`; double-charged = orders with more than 1 applied charge; dollars = sum of `amount_cents x (applied - 1)`.

Verifier rules: independent process and container; reads only the three external records above; never reads dashboard streams; exits `0` all pass, `1` an invariant failed, `2` could not evaluate; writes `artifacts/verdict.json` and a readable console report.

Test the tester (`make mutation-check`, required before submission): (a) run chaos with `STEADFAST_DISABLE_FENCING=1` -> I4 must fail; (b) run with `STEADFAST_DISABLE_IDEMPOTENCY=1` plus ledger `apply_then_fail` -> I2 must fail; (c) delete one acknowledged job row from a copy of the database -> I1 must fail. A verifier that cannot fail proves nothing.

# Appendix D: demo choreography (profile `full`, about 4 minutes; profile `short` compresses to about 90 seconds)

Preconditions: `docker compose up` done, seeded, poison job already in the DLQ, 4 Steadfast workers and 4 baseline workers, demo timings (lease 10 s, heartbeat 3 s, backoff base 500 ms and cap 15 s, breaker cooldown 10 s). Load rate and handler latency are tuned in rehearsal so that at least 500 jobs are queued or leased in each system when the kills happen.

| Beat | Time (full / short) | Script (automated; each step also a button) | Expected on screen | Fallback |
|---|---|---|---|---|
| 1 Load | 0:00-0:45 / 0:00-0:15 | loadgen starts 10,000 checkout jobs per lane | throughput climbs both sides | pre-recorded clip |
| 2 Crash | 0:45-1:45 / 0:15-0:45 | kill 50% of workers in both lanes; 15 s later hard-kill Postgres and Redis together; restart both | Steadfast: DATABASE DOWN, then recovers, leases expire, run finishes; baseline: loses and redelivers | run the same steps via `make chaos-*` |
| 3 Failure and poison | 1:45-3:00 / 0:45-1:15 | ledger `apply_then_fail` p=0.3 for 10 s, then `error_rate=1.0` for 20 s, then clear; operator fixes the poison job payload in the DLQ drawer and replays | duplicates appear on baseline; Steadfast breaker opens then closes by itself; poison job succeeds | the pre-seeded poison job is always ready |
| 4 Verdict | 3:00-4:00 / 1:15-1:30 | verifier runs after drain | I1-I4 pass; stakes: baseline "$X, M lost" vs Steadfast "0 and 0" | static screenshot of a prior verified run |

Rehearsal gate: five complete runs on the presentation laptop on its network conditions (or offline) before the pitch. Keyboard shortcuts 1-4 trigger beats in operator mode. One narrator speaks, one operator clicks.

# Appendix E: configuration (environment variables; `.env.example` holds fake values)

| Variable | Default | Demo value |
|---|---|---|
| `STEADFAST_ENV` | `prod` (refuses chaos and mutation flags) | `demo` |
| `DATABASE_URL`, `CHECKER_DATABASE_URL` (read-only role), `REDIS_URL`, `LEDGER_URL` | none | local compose addresses |
| `ADMIN_KEY`, tenant API keys | none (generated by seed script into git-ignored `.env`) | generated |
| `LEASE_SECONDS` / `HEARTBEAT_SECONDS` | 30 / 10 | 10 / 3 |
| `REAPER_INTERVAL_MS` | 1000 | 1000 |
| `BACKOFF_BASE_MS` / `BACKOFF_CAP_MS` | 1000 / 60000 | 500 / 15000 |
| `DEFAULT_MAX_ATTEMPTS` | 5 | 5 |
| `IDEMPOTENCY_TTL_HOURS` | 24 | 24 |
| `MAX_PAYLOAD_BYTES` | 65536 | 65536 |
| `BREAKER_MIN_SAMPLES` / `BREAKER_FAILURE_RATIO` / `BREAKER_COOLDOWN_SECONDS` | 10 / 0.5 / 30 | 10 / 0.5 / 10 |
| `RATE_LIMIT_RPS` (per key) | 5000 | 5000 |
| `BASELINE_PATTERN` | `visibility_timeout` | same (`ack_on_pull` for the bonus clip) |
| `BASELINE_VISIBILITY_SECONDS` | 10 | 10 |
| `BASELINE_SEND_IDEMPOTENCY_KEY` | `false` | `false` (third lane `true`) |

# Appendix F: repository layout and compose services

```
steadfast/
  cmd/api  cmd/reaper  cmd/worker-example  cmd/ledger  cmd/baseline-api  cmd/baseline-worker
  cmd/loadgen  cmd/verifier  cmd/chaos  cmd/dashboard  cmd/seed
  internal/ (store, jobs, auth, idempotency, backoff, breaker, audit, config)
  sdk/worker/            # public Go worker SDK
  migrations/            # versioned, reversible
  web/dashboard/         # vendored static HTML/JS/CSS, no build step, no CDN
  scripts/  Makefile  docker-compose.yml  .env.example
  docs/ (SPEC.md, ARCHITECTURE.md, DECISIONS.md, DEMO.md, LIMITATIONS.md, RUNBOOK.md)
  artifacts/ (git-ignored: ack files, verdict.json)   results/ (bench output)
  .agents/rules/ (Antigravity rules)   .github/workflows/ci.yml
```
Compose services: `postgres`, `api`, `reaper`, `worker` (scale 4), `ledger`, `redis`, `baseline-api`, `baseline-worker` (scale 4), `dashboard`, `chaos`; on-demand profiles: `loadgen`, `verifier`, `seed`. Makefile targets: `up`, `down`, `clean`, `seed`, `demo`, `demo-short`, `chaos-kill-workers`, `chaos-kill-postgres`, `chaos-kill-redis`, `chaos-ledger-fault`, `verdict`, `mutation-check`, `test`, `lint`, `scan`, `bench`.

# Appendix G: build order and cut line (one agent, in sequence)

1. Repo, tooling, compose skeleton, CI skeleton, secret handling. 2. Architecture and threat model (planning only, wait for approval). 3. Schema, migrations, tenants and API-key auth. 4. Submit API with idempotency and durability (F1, F2). 5. Worker API, fencing, reaper, retries, DLQ (F3-F6). 6. Go worker SDK, example handler, mock ledger (F7, F8). 7. Load generator, baseline, verifier v0 -> **first end-to-end verified result (go/no-go checkpoint)**. 8. Chaos controller, zombie test, mutation check (F9, I4). 9. Dashboard and demo runner (F11). 10. Circuit breaker (F12, P1). 11. Graceful shutdown and rate limit (F13, P1). 12. Hardening: security scans, tests, bench, README, limitations. 13. Demo hardening and rehearsal tooling. 14. Pre-submission audit.

Cut rule: prompts 1-9 are the winning core; 12-14 are mandatory; 10-11 are cut first, then anything marked P2. If the checkpoint after prompt 7 fails, stop adding features and fix the checker first (the checker is the headline).

# Appendix H: decisions vs the playbook, with reasons

| # | Decision | Reason |
|---|---|---|
| D1 | Steadfast uses Postgres only; Redis exists only for the baseline. Circuit state lives in Postgres. | Fewer moving parts in 12-24 hours; Q&A answer becomes "Steadfast has no Redis dependency", which is stronger than "Redis is disposable". |
| D2 | Job states: `QUEUED`, `LEASED`, `SUCCEEDED`, `DEAD` (no `RUNNING`) | Nothing distinguishes it from `LEASED`; fewer states, fewer bugs. |
| D3 | Outbox + webhooks are P2 and not claimed | Not in the playbook's must-have list; would add a component to harden with no demo payoff. |
| D4 | Fairness, invariant I5 and OpenTelemetry/Jaeger are P2 | Time-limited; the playbook already treats I5 as a bonus. |
| D5 | Workers talk to the API only; no worker database credentials | Smaller attack surface; language-agnostic SDK. |
| D6 | Downstream idempotency key = `steadfast:<job_id>`; fencing token sent as a separate header | A key that changes per attempt would defeat deduplication. |
| D7 | Baseline = reliable queue with visibility timeout, AOF `everysec`, no downstream key, plus optional third lane "baseline + key" | Hardest for judges to call a strawman; shows the gap is bigger than idempotency keys. |
| D8 | Added: reaper dead-letters exhausted jobs, invariant I2b, ledger `apply_then_fail` mode | Closes the crash-loop hole; the checker also catches "succeeded but not charged"; this is how real double-charges happen. |
| D9 | Demo has `full` and `short` profiles | The playbook's 4-minute demo does not fit its own 90-second slot. |
| D10 | Custom Go load generator instead of k6 for the demo | It must write its own acknowledgement file; k6 can be added for `make bench`. |
| D11 | Demo timing profile (lease 10 s, backoff cap 15 s, breaker cooldown 10 s) | A 30-second lease makes recovery too slow to watch. Defaults stay production-like. |
| D12 | Teammates do slides, recorded backup video, rehearsal, Q&A drill, README review; they do not edit code during the build. License MIT. | One sequential agent means parallel human edits cause merge conflicts and drift. Confirm if your team plans otherwise. |

# Appendix I: stack proposal (for confirmation)

**Pick: Go for all services**, PostgreSQL (data store), Redis (baseline only), SQLite via a pure-Go driver (ledger), static HTML + vanilla JS + Server-Sent Events for the dashboard (served by the Go dashboard service), Docker Compose, Make, GitHub Actions, `golangci-lint`, `govulncheck`, `gitleaks`.
Why: single static binaries are easy to run many times in compose and to kill; Go handles concurrent workers and Postgres (pgx) well; the SDK the playbook asks for is Go anyway; one language lowers agent error rate; no Node toolchain means no npm supply chain and no build step to fail on stage; works fully offline.
Trade-offs: Go is more verbose; a vanilla-JS dashboard is less polished than React. Mitigation: a small vendored charting approach (hand-drawn canvas/SVG) and a presenter mode.
**Alternative (not recommended): TypeScript (Node) services with a React dashboard.** Faster to make a pretty UI, but weaker fit for a concurrency-heavy queue, bigger dependency surface, extra build step, no single-binary kill/restart story.
Versions: not chosen here. The agent must read current official docs at setup, pin the latest stable versions with lockfiles, and record the versions it chose in `docs/DECISIONS.md`.

# Appendix J: Windows + 16 GB addendum and decisions D13-D16

**J1 Environment.** Docker Desktop with the WSL2 backend. Put the repository inside the WSL2 Linux filesystem (for example `~/steadfast`), never under `/mnt/c` (slow file access, line-ending and permission problems). Open the folder in Antigravity from WSL2 if it supports that; if it does not, keep the repo on Windows with LF line endings enforced and run commands in PowerShell. Prompt 1 verifies which path works and records the result in `docs/DECISIONS.md`.

**J2 Tooling (D13).** Nothing may depend on `make`, `bash`, `sed`, `grep`, or `curl` on the host. The primary entry point is the Go CLI: `go run ./cmd/ctl <command>` (up, down, clean, seed, demo, demo-short, chaos, verdict, mutation-check, test, lint, scan, bench, rehearse). A Makefile may exist as a thin alias for Linux CI only. `.gitattributes` forces `eol=lf`. No shell scripts are mounted into containers; containers use compiled binaries.

**J3 Resource budget (16 GB total).** Cap WSL2 in `%UserProfile%\.wslconfig`: `memory=9GB`, `processors` = total cores minus 2, so Windows and the browser stay responsive. Container limits: Postgres 1.5 GB, Redis 256 MB (with `maxmemory-policy noeviction` so the baseline cannot silently evict jobs, which would be unfair), each Go service 128 MB. Target under 6 GB in total; measure with `docker stats` and record in `docs/evidence/`.

**J4 Honest durability wording.** Say: "an acknowledged job survives a hard kill of the Postgres container." Do not say it survives power loss or a host crash. Docker Desktop runs Linux inside a VM, so `fsync` reaches the VM's virtual disk, not necessarily the physical disk. Postgres data must live on a named Docker volume, never a Windows bind mount.

**J5 Demo-day checklist (Windows).** Plugged in; High performance power plan; sleep and screen-off disabled; Do Not Disturb/Focus on; Windows Update paused; all other apps closed; fresh browser profile with extensions off; images already built so `docker compose up` works with Wi-Fi off; resolution and zoom tested on the projector (or 1080p at 125% zoom); `?present=1` large-font mode; full rehearsal on this exact laptop.

**J6 Decisions.**
| # | Decision | Reason |
|---|---|---|
| D13 | `cmd/ctl` Go CLI is the primary task runner | Windows has no `make`/bash by default; one language, same behavior everywhere. |
| D14 | Default demo profile is `short` (about 90 s); `full` (about 4 min) is for the backup video and for a longer slot | The playbook's own pitch gives the demo 90 s; unknown slot must not break the pitch. |
| D15 | Rubric unknown: keep the five generic criteria mapping from the playbook; a teammate finds the real rubric (O1) | Real text may change slide wording, not the build. |
| D16 | Windows 11 or 10 + Docker Desktop (WSL2), repo in WSL2 filesystem, 9 GB WSL cap | Reliable container kills and acceptable disk behavior on a 16 GB laptop. |
