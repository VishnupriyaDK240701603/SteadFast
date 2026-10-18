# Prompt 04: durable job submission and idempotency (F1, F2)
## Goal
`POST /v1/jobs` and read endpoints that never acknowledge an uncommitted job and never create a duplicate for a repeated idempotency key.
## Context
Prompts 01 to 03 done. Spec F1, F2, Appendix A (`/jobs`), Appendix B (idempotent submit). Rules: engine laws 1, 2, 4.
## Scope
In scope: `POST /v1/jobs`, `GET /v1/jobs/{id}`, `GET /v1/jobs`, validation, durability and concurrency tests, `ctl` helper for the durability test.
Out of scope (do not touch): worker endpoints, reaper, DLQ, rate limiting, dashboard.
## Requirements
1. Validation exactly as in `10-security.md` (payload 64 KiB, type regex, key length, max_attempts 1 to 20, optional `priority` and `run_at`). Invalid input returns 400 with a generic body.
2. Idempotent submit in ONE transaction as described in Appendix B: key scoped per tenant, SHA-256 of canonical payload (canonicalize JSON so key order does not matter), TTL 24 hours (configurable), repeat returns the original ID with `Idempotent-Replayed: true`, different payload returns 409 `idempotency_key_conflict`, expired key may be reused.
3. Respond 202 only after COMMIT returns. If the transaction cannot start or commit, respond 503 with `Retry-After` and never 202. Do not retry a commit whose outcome is unknown without the idempotency check protecting it.
4. `GET /v1/jobs/{id}` returns the job and its attempts (tenant-scoped, 404 otherwise). `GET /v1/jobs` supports `state`, `limit` (max 200), and an opaque cursor.
5. Without an `Idempotency-Key` header a job is simply created; the API never invents a key.
6. Connection pool reconnects automatically after a Postgres restart without restarting the API.
## Security and privacy checklist
Tenant from key only; foreign IDs 404; parameterized SQL; payload never logged; body size caps; generic errors; no timing difference that reveals whether a key exists for another tenant.
## Method
1. Read spec and rules. 2. Proceed after planning. 3. Small commits. 4. Tests alongside code.
## Acceptance criteria
- A1.1: submit 200 jobs, record the 202s, `docker kill` the Postgres container (SIGKILL), start it, and every acknowledged job is present (test or `ctl` scenario; paste output).
- A1.2: with Postgres stopped, 20 submissions all get 503, none 202.
- A1.3: oversized payload, invalid type, bad JSON, `max_attempts` 0 and 21 each give 400.
- A2.1: 50 concurrent identical submissions with one key create exactly 1 job (run the test 3 times).
- A2.2: same key with a different payload gives 409.
- A2.3: same key under `shop-b` does not collide with `shop-a`.
- A2.4: with a shortened TTL the key can be reused after expiry.
- The API recovers after a Postgres restart without a manual API restart.
## Verification
`go run ./cmd/ctl test` (show the named tests); the kill scenario output; `go run ./cmd/ctl lint`; `go run ./cmd/ctl scan`.
## Report back
Standard report format.
## Stop and ask if
Commit-outcome ambiguity (connection lost during COMMIT) cannot be made safe with idempotency alone; propose options instead of guessing.
