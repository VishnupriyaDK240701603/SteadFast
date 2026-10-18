# Prompt 06: Go worker SDK, example handler, mock ledger (F7, F8)
## Goal
A worker that processes checkout jobs end to end and a separate fake payment API that records every charge independently.
## Context
Prompts 01 to 05 done. Spec F7, F8, Appendix A (worker endpoints), Appendix E. Rules: engine law 5.
## Scope
In scope: `sdk/worker`, `cmd/worker-example`, `cmd/ledger`, compose services `ledger` and `worker` (scale 4), tests.
Out of scope (do not touch): loadgen, baseline, verifier, chaos, dashboard, circuit breaker.
## Requirements
1. SDK: `worker.Run(ctx, Config)` with API base URL, key, worker ID, handler map, concurrency, claim batch size; polls with jittered interval; automatic heartbeats every lease/3 (value learned from the claim response); cancels the handler context when a heartbeat returns 409 `lease_lost`; transient API errors (network, 503) retry with backoff and never crash the worker; `worker.Retryable(err)` and `worker.Permanent(err)` classify errors; a handler panic is caught and reported as a retryable failure. The SDK never compares its clock to a lease time and never touches a database.
2. Example handler `charge_customer`: payload `{order_id, customer_id, amount_cents}`; calls the ledger `POST /charge` with `Idempotency-Key: steadfast:<job_id>` and header `X-Fencing-Token`. Ledger 5xx or timeout is retryable; ledger 4xx (for example negative amount) is permanent.
3. Ledger service: separate container with its own volume, SQLite via a pure-Go driver. `POST /charge` applies a charge; with a key already applied it returns the original response and records a `suppressed` entry; without a key it always applies. `GET /ledger/export` streams NDJSON of all entries (applied and suppressed). `GET /ledger/stats`. `POST /admin/fault` with modes `none`, `error_rate p`, `latency_ms n`, `down`, `apply_then_fail p` (apply, then return an error or hang past the client timeout); admin endpoints require `ADMIN_KEY`, bind to 127.0.0.1 on the host.
4. The ledger never reads from or writes to Steadfast's Postgres.
## Security and privacy checklist
Workers hold only a tenant API key, no DB credentials; admin faults need the key; synthetic data only; payloads not logged; ledger bound to localhost for admin.
## Method
1. Read spec and rules. 2. Proceed after planning. 3. Small commits. 4. Tests alongside.
## Acceptance criteria
- A7.1: submit 100 `charge_customer` jobs through the API with 4 example workers; all reach `SUCCEEDED`; the ledger shows exactly 100 applied entries.
- A7.2: the worker container environment has no database URL (show `docker compose config` excerpt).
- A8.1: replaying the same key returns an identical response body and adds a `suppressed` entry.
- A8.2: with `apply_then_fail p=1` and no key, one request produces a duplicate on retry; with the key it does not (tests).
- Killing the Steadfast Postgres container does not stop or lose the ledger (show ledger export before and after).
- A handler panic and a `lease_lost` cancel are covered by tests.
## Verification
`go run ./cmd/ctl up`; the 100-job scenario via `ctl` or a test; `go run ./cmd/ctl test`; `go run ./cmd/ctl lint`; `go run ./cmd/ctl scan`.
## Report back
Standard report format.
## Stop and ask if
The pure-Go SQLite driver is unusable on this setup or the ledger needs a different store.
