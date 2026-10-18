# Prompt 07: load generator, baseline queue, independent verifier (F10) and CHECKPOINT
## Goal
The first real, independently verified result. This is the headline feature; do not rush it and do not make it grade itself.
## Context
Prompts 01 to 06 done. Spec F10, Appendix C, Appendix E (baseline variables), SPEC section 8. Rules: engine laws 8 and 9.
## Scope
In scope: `cmd/loadgen`, `cmd/baseline-api`, `cmd/baseline-worker`, `cmd/verifier`, Redis compose service with a read-only ACL user, `ctl verdict`, tests.
Out of scope (do not touch): chaos controller, dashboard, circuit breaker, Steadfast engine code (bugs found go to the report, not into silent fixes; ask).
## Requirements
1. Loadgen: `--count`, `--rate`, `--seed`, `--lane steadfast|baseline|both`. Each job is a checkout: unique `order_id`, `customer_id`, `amount_cents` 500 to 50,000 from the seeded RNG (reproducible). Steadfast lane sends `Idempotency-Key: <order_id>`. Retries a failed submission up to 2 times with the same body in BOTH lanes. Appends every acknowledgement (`order_id`, `job_id`, `amount_cents`, `ack_at`, `lane`) to `artifacts/acks-<lane>.ndjson` with periodic fsync, before doing anything else with the result. It never reads Steadfast or baseline state.
2. Baseline: Redis list queue with a processing set and a visibility timeout (default 10 s), AOF on with `appendfsync everysec`, `maxmemory-policy noeviction`, workers that do NOT send an idempotency key downstream, retry on failure, exhausted jobs to a `failed` list. Modes by env: `BASELINE_PATTERN=visibility_timeout|ack_on_pull`, `BASELINE_SEND_IDEMPOTENCY_KEY=true|false`. Write `docs/BASELINE-CONFIG.md` listing the exact configuration and why it is a common, fair pattern. Do not name a specific third-party product as broken.
3. Verifier: reads ONLY (a) the ack files, (b) the ledger export over HTTP, (c) Postgres through `steadfast_checker_ro`, (d) the baseline `failed` list through a read-only Redis user. Waits for drain (no `QUEUED` or `LEASED` jobs, or a timeout), then evaluates I1, I2, I2b, I3 per Appendix C for Steadfast and the baseline loss/duplicate/dollar report. Writes `artifacts/verdict.json` and a readable console report. Exit codes 0, 1, 2 as in the spec. It never calls Steadfast APIs and never reads dashboard data.
4. Unit tests for the verifier using hand-made fixture data (including violating fixtures for each invariant).
## Security and privacy checklist
Read-only DB and Redis credentials; no keys in artifacts; ack files contain synthetic data only; no secrets in `verdict.json`.
## Method
1. Read spec and rules. 2. Produce a plan (data flow, file formats, fixture list) and WAIT for approval. 3. Small commits. 4. Tests alongside.
## Acceptance criteria
- Without chaos: 1,000 jobs per lane run to completion; the verifier exits 0 for Steadfast with I1, I2, I2b, I3 all passing and prints baseline numbers.
- With one manual worker kill (`docker kill` a Steadfast worker) mid-run: verifier still exits 0 for Steadfast.
- Fixture tests: each violating fixture makes the verifier exit 1 naming the right invariant.
- A deliberately deleted acknowledged job row (in a throwaway copy or test database) makes I1 fail.
- `docs/BASELINE-CONFIG.md` exists and matches the actual compose settings.
- The verifier source contains no import of Steadfast client code or dashboard code (show the import list).
## Verification
`go run ./cmd/ctl up`; `go run ./cmd/ctl seed`; the two runs above via `ctl` (paste console reports and `verdict.json`); `go run ./cmd/ctl test`; `go run ./cmd/ctl lint`; `go run ./cmd/ctl scan`.
## Report back
Standard report format PLUS a "CHECKPOINT REPORT": what works end to end, what the first real numbers are (stated as measured on this laptop), and an honest go or no-go for continuing to prompt 08.
## Stop and ask if
The verifier and Steadfast disagree and you cannot tell which is wrong (do not edit the verifier to make it pass); baseline behavior seems unfair either way.
