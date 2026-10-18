# Prompt 09: live dashboard, demo runner, stakes counters (F11, demo profiles)
## Goal
One screen that tells the story: two systems side by side, live chaos, dollars at stake, and a verdict from the independent verifier. Works offline, one click to run.
## Context
Prompts 01 to 08 done. Spec F11, section 4, Appendix D, Appendix E, J5. Rules: engine law 8 (the dashboard never computes the verdict).
## Scope
In scope: `cmd/dashboard`, `web/dashboard/`, demo runner (`ctl demo`, `ctl demo-short`), seed of one pre-seeded poison job, presenter and operator modes.
Out of scope (do not touch): engine logic, verifier logic, circuit breaker internals (the panel shows breaker state only if it exists).
## Requirements
1. Dashboard server reads Steadfast metrics from Postgres through the read-only role (queue depth, throughput, p50 and p99 of `finished_at - created_at` over the last 30 s, retries, DLQ count, worker count) and baseline metrics from Redis read-only; streams to the browser with Server-Sent Events. If Postgres is unreachable it sends "database down" and keeps the baseline panel alive.
2. Layout: left "Steadfast", right "Baseline", each with throughput graph, queue depth, p50/p99; Steadfast also DLQ count and breaker state; large stakes counters ("double-charged: N ($X), orders lost: M" versus "0 and 0") taken from the ledger export and ack files via the verifier's report logic or a read-only summary, never from Steadfast itself; verdict strip showing `artifacts/verdict.json`.
3. DLQ drawer: list dead jobs, show per-attempt error history, edit payload JSON with validation, Replay button. Calls go through the dashboard server, which holds the tenant key and admin credential; the browser never sees keys.
4. Controls: "Run demo" (uses profile `short` by default, `full` via toggle), per-beat buttons, chaos buttons, keyboard shortcuts 1 to 4 in operator mode (`?operator=1`) with a beat timer and fallback triggers; large-font mode `?present=1`.
5. Demo runner executes Appendix D beats with the configured timing profile (lease 10 s, heartbeat 3 s, backoff base 500 ms and cap 15 s, breaker cooldown 10 s; `short` compresses windows). Pre-seed one poison job through the real path so it sits in the DLQ with real history.
6. All JS, CSS, and fonts vendored in the repo; no CDN; works with Wi-Fi off. Charts drawn with canvas or SVG without a build step.
## Security and privacy checklist
Server-side credentials only; Origin check on state changes; bound to 127.0.0.1; no keys in the bundle (grep proof); escape all job data rendered in the page (no raw HTML injection); security headers (CSP restricting to self).
## Method
1. Read spec and rules. 2. Produce a plan (layout sketch in text, data endpoints, SSE message format) and WAIT for approval. 3. Small commits. 4. Tests alongside.
## Acceptance criteria
- A11.1: with the network adapter disabled, `ctl up` then the dashboard loads and a short demo completes.
- A11.2: grep of the served bundle finds no API key or admin key value.
- A11.3: one click on "Run demo" executes all four beats without further input.
- `ctl demo-short --headless` completes and the verifier exits 0 for Steadfast.
- During the Postgres kill the Steadfast panel shows the database-down state and the baseline panel keeps updating.
- Poison job edit and replay works from the drawer and ends with exactly 1 applied ledger entry.
- Escaping test: a job payload containing HTML renders as text.
## Verification
`go run ./cmd/ctl up`; `go run ./cmd/ctl demo-short --headless` (paste verdict); manual walkthrough with screenshots saved in `docs/evidence/`; `go run ./cmd/ctl test`; `go run ./cmd/ctl lint`; `go run ./cmd/ctl scan`.
## Report back
Standard report format, with screenshot file names.
## Stop and ask if
A layout or story choice would change what the verdict screen claims.
