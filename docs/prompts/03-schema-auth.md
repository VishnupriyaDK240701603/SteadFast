# Prompt 03: schema, migrations, roles, authentication, audit
## Goal
The database foundation and API-key authentication, with tenant isolation enforced by construction.
## Context
Prompts 01 and 02 done and approved. Spec sections 2, 5, 8 and Appendix A, B (indexes). Rules: engine laws 4 and 7.
## Scope
In scope: `migrations/`, `internal/store`, `internal/auth`, `internal/config`, `internal/audit`, `cmd/seed`, minimal `cmd/api` with `/healthz` and `/readyz`, `ctl seed`, `ctl migrate`.
Out of scope (do not touch): job submit, worker endpoints, reaper, ledger, dashboard.
## Requirements
1. Reversible migrations (up and down) creating `tenants`, `jobs`, `idempotency_keys`, `job_attempts`, `audit_log`, with the columns, `CHECK` constraints (state in `QUEUED, LEASED, SUCCEEDED, DEAD`; `max_attempts` 1 to 20; `attempts >= 0`), foreign keys, and the three indexes from Appendix B. `breaker` table is NOT created yet.
2. A database trigger writes an `audit_log` row on every `jobs.state` change (from_state, to_state, job_id, tenant_id, timestamp). Actor is read from a transaction-local setting when present, otherwise `system`. `audit_log` is append-only (revoke UPDATE and DELETE from the application role).
3. Roles: `steadfast_app` (least privilege on its tables) and `steadfast_checker_ro` (SELECT only on `jobs` and `job_attempts`). Passwords come from environment.
4. Repository layer: every function that reads or writes tenant data takes `tenantID` as a required parameter. There is no function that fetches a job by ID alone.
5. Auth middleware: `Authorization: Bearer <key>`; SHA-256 hash lookup; returns 401 with the generic error body on missing or invalid key; tenant ID injected into request context. Admin key is a separate credential, compared in constant time.
6. `seed` creates tenants `shop-a` and `shop-b`, generates 32-byte random keys, stores only hashes, writes plaintext keys only to the git-ignored `.env`, and never prints them.
7. API skeleton: structured JSON logging with request IDs, strict JSON decoding helper, error format, graceful handling of DB outage on `/readyz` (503).
## Security and privacy checklist
Parameterized queries; hashed keys; no keys in logs; 401 is generic; least-privilege roles; audit trigger cannot be bypassed by application code; `.env` never committed.
## Method
1. Read spec and rules. 2. Proceed after planning. 3. Small commits. 4. Tests alongside.
## Acceptance criteria
- Migrate up, down, up works on a clean database.
- Updating a job's state by any means produces an audit row (tested directly in SQL).
- `steadfast_checker_ro` can SELECT `jobs` and cannot INSERT or UPDATE (tested).
- Calls without a key or with a wrong key get 401; a valid key reaches the handler with the right tenant.
- A repository test proves tenant B cannot read tenant A data through any exported repository function.
- Grep proves no log line prints a key or the Authorization header (test with a captured log buffer).
## Verification
`go run ./cmd/ctl clean && go run ./cmd/ctl up`; `go run ./cmd/ctl migrate` (up, down, up); `go run ./cmd/ctl seed`; `go run ./cmd/ctl test`; `go run ./cmd/ctl lint`; `go run ./cmd/ctl scan`; paste output of the role-permission test.
## Report back
Standard report format.
## Stop and ask if
A schema change from the spec seems necessary; the trigger approach conflicts with the approved architecture.
