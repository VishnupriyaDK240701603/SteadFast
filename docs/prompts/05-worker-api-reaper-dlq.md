# Prompt 05: leases, fencing, reaper, retries, DLQ (F3, F4, F5, F6)
## Goal
The reliability core: workers claim jobs under leases, results are accepted only with the current fencing token, crashed workers' jobs return to the queue, failures retry with backoff, and unrecoverable jobs land in an inspectable, replayable DLQ.
## Context
Prompts 01 to 04 done. Spec F3 to F6, Appendix A (worker and DLQ endpoints), Appendix B (claim, heartbeat, complete, reaper). Rules: engine laws 2, 3, 6, 7, 10. This is the heart of the project; accuracy beats speed.
## Scope
In scope: `/v1/worker/*` endpoints, `/v1/dlq*` endpoints, `internal/backoff`, `cmd/reaper`, tests, the guarded mutation flags `STEADFAST_DISABLE_FENCING` and `STEADFAST_DISABLE_IDEMPOTENCY` (the second may be wired now but is tested in prompt 08).
Out of scope (do not touch): SDK, ledger, circuit breaker (prompt 10), dashboard, rate limiting.
## Requirements
1. Claim: Appendix B query as the starting point, in one transaction that also opens the `job_attempts` row. Parameters: tenant from key, worker ID, job types, max batch, lease seconds from config.
2. Heartbeat: extends the lease from DB `now()` only when token is current and state is `LEASED`; otherwise 409 `lease_lost`.
3. Complete: accepted only with current token and state `LEASED`, even if `lease_expires_at` has passed but the reaper has not run. Closes the attempt row in the same transaction. Otherwise 409 `stale_fencing_token` and nothing changes.
4. Fail: body has `error_class`, `message`, `retryable`. Retryable and `attempts < max_attempts` returns the job to `QUEUED` with `run_at = now() + jittered delay` (full jitter: random between 0 and `min(cap, base * 2^(attempt-1))`; base and cap configurable). Permanent or exhausted goes to `DEAD` with `dead_reason` (`permanent_error` or `max_attempts_exhausted`). Attempt row records outcome, class, message. Stale token gives 409.
5. Reaper (`cmd/reaper`): every `REAPER_INTERVAL_MS`, guarded by a Postgres advisory lock so only one instance is active; uses the corrected Appendix B query: expired lease goes to `QUEUED`, or to `DEAD` with `lease_expired_max_attempts` when `attempts >= max_attempts`; the attempt row is closed with outcome `lease_expired`.
6. DLQ: `GET /v1/dlq`; `PATCH /v1/dlq/{id}` replaces payload (validate, recompute hash, audit with actor); `POST /v1/dlq/{id}/replay` sets `QUEUED`, `attempts = 0`, `run_at = now()`, keeps job ID, idempotency key, and attempt history; both return 409 if the job is not `DEAD`.
7. Mutation flag `STEADFAST_DISABLE_FENCING`: when set (demo mode only) the fencing check is skipped. The process refuses to start with the flag unless `STEADFAST_ENV=demo` and logs a loud warning.
## Security and privacy checklist
Workers see only their tenant's jobs and only those they currently lease; token and state checked in SQL, not in application code after a read; no payloads in logs; audit rows for every transition and operator action; cross-tenant tests for every new endpoint.
## Method
1. Read spec and rules. 2. Produce an implementation plan (SQL for each endpoint, transaction boundaries, test list) and WAIT for approval. 3. Implement in small commits, one endpoint group per commit. 4. Tests alongside code.
## Acceptance criteria
- A3.1: 8 concurrent claimers over 5,000 jobs produce zero double-claims (run 3 times).
- A3.2: a completion with a stale token returns 409 and the row is unchanged.
- A3.3: a completion with the current token after `lease_expires_at` but before the reaper is accepted.
- A3.4: code search shows no comparison of a non-DB clock to a lease time (list the grep evidence).
- A4.1: killing a worker mid-job leads to completion on another worker within one lease interval plus 1 second (use 2-second leases in tests).
- A4.2: a handler that never completes reaches `DEAD` after `max_attempts` with reason `lease_expired_max_attempts`.
- A4.3: two reapers at once do not double-requeue.
- A5.1: 1,000 sampled delays stay within bounds and are not all equal. A5.2: a permanent error gives `DEAD` after exactly 1 attempt. A5.3: one `job_attempts` row per attempt with full detail.
- A6.2: edit plus replay of a dead job returns it to `QUEUED` with history intact. A6.3: 409 for non-dead jobs.
- Cross-tenant 404 on every new endpoint.
## Verification
`go run ./cmd/ctl test` (show all named tests above, 3 consecutive green runs for concurrency tests); `go run ./cmd/ctl lint`; `go run ./cmd/ctl scan`; paste the SQL of each endpoint.
## Report back
Standard report format.
## Stop and ask if
Any query in Appendix B turns out to be wrong or unsafe (propose the fix, do not patch silently); a concurrency test is flaky.
