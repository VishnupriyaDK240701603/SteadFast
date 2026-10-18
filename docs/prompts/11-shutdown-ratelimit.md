# Prompt 11: graceful shutdown and per-key rate limit (F13, P1)
## Goal
Draining a worker or the API loses and duplicates nothing, and abusive clients get 429.
## Context
Prompts 01 to 09 done. Spec F13. Rules: engine laws 1, 3.
## Scope
In scope: SIGTERM handling in `cmd/api` and the SDK/worker, an in-process token bucket per API key on `POST /v1/jobs`, tests.
Out of scope (do not touch): dispatch logic, schema.
## Requirements
1. Worker on SIGTERM: stop claiming, finish in-flight jobs for up to 25 s, complete them, exit 0. Jobs not finished are left to the reaper.
2. API on SIGTERM: stop accepting new connections, finish in-flight requests, exit.
3. Rate limit: token bucket per key, `RATE_LIMIT_RPS` default high enough for the demo; exceeding returns 429 with `Retry-After`.
## Security and privacy checklist
Limiter keyed by authenticated tenant key hash; no key material in logs; limiter state in memory only (document that it resets on restart).
## Method
1. Read spec and rules. 2. Proceed after planning. 3. Small commits. 4. Tests alongside.
## Acceptance criteria
- A13.1: `docker stop` on a worker mid-run (graceful) produces zero lost and zero duplicate effects in the verifier.
- A13.2: exceeding the limit returns 429 with `Retry-After`; staying below does not.
- The demo load rate is not throttled (show a `demo-short` run with no 429s).
## Verification
`go run ./cmd/ctl test`; graceful stop scenario with verdict; lint and scan.
## Report back
Standard report format.
## Stop and ask if
Draining conflicts with the approved architecture.
