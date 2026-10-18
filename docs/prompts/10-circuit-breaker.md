# Prompt 10: per-job-type circuit breaker (F12, P1; skip first if time is short)
## Goal
When the downstream is failing, Steadfast pauses dispatch for that job type, probes carefully, and recovers by itself, with no retry storm.
## Context
Prompts 01 to 09 done and the demo is runnable. Spec F12, Appendix E (breaker variables). Rules: engine laws 2 and 3. Postgres only; no Redis.
## Scope
In scope: `breaker` migration, `internal/breaker`, claim query change, fail/complete hooks, dashboard breaker panel wiring, tests.
Out of scope (do not touch): other engine behavior, SDK API shape, verifier.
## Requirements
1. Per (tenant, job type) state `CLOSED`, `OPEN`, `HALF_OPEN`, stored in Postgres and updated in the same transaction as fail and complete.
2. Opens when at least `BREAKER_MIN_SAMPLES` recent outcomes exist and at least `BREAKER_FAILURE_RATIO` are failures with `error_class = downstream_unavailable`.
3. While `OPEN` the claim query skips that type until `opened_at + cooldown` (DB time). Then `HALF_OPEN` allows exactly one leased probe job; success closes, failure reopens.
4. Permanent errors (for example a poison payload) never count toward opening.
## Security and privacy checklist
Tenant-scoped; parameterized SQL; reversible migration; audit rows for breaker transitions.
## Method
1. Read spec and rules. 2. Proceed after planning. 3. Small commits. 4. Tests alongside.
## Acceptance criteria
- A12.1: ledger `error_rate=1.0` opens the breaker within 15 s and dispatch for that type pauses.
- A12.2: after `clear-faults` the breaker closes with no operator action.
- A12.3: attempts per second for that type drop measurably while `OPEN` (show before and after numbers from a test).
- A poison job does not open the breaker.
- Beat 3 of the demo still passes in `ctl demo-short --headless`.
## Verification
`go run ./cmd/ctl test`; the breaker scenario via `ctl`; `go run ./cmd/ctl demo-short --headless`; lint and scan.
## Report back
Standard report format.
## Stop and ask if
Claim-query changes affect the fencing or SKIP LOCKED behavior verified in prompt 05 (rerun those tests and report).
