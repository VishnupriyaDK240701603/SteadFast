# Steadfast: project rules (always apply)

You are building Steadfast, a reliable asynchronous job runner whose headline feature is an independent verifier that proves reliability under chaos. The full specification is `docs/SPEC.md`. If this file and the spec disagree, stop and ask. Never silently choose.

## Goal that decides every trade-off
A flawless, small, provable demo beats a large, half-working one. If a change does not make the live demo more convincing or a claim more defensible, do not build it. Anything marked P1 or P2 in the spec is built only when a prompt says so.

## Stack (fixed; versions are chosen at setup)
- Go for every service. PostgreSQL is the only data store for Steadfast. Redis is used ONLY by the baseline. The mock ledger uses SQLite (pure-Go driver). The dashboard is static HTML + vanilla JS + Server-Sent Events served by a Go service. Docker Compose runs everything.
- Do not add a language, framework, database, or message broker. Do not add a dependency without asking (see the ask-first list in `10-security.md`).
- Never write a version number from memory. Read current official docs, pin exact versions with lockfiles (`go.mod`/`go.sum`), and record each choice with a one-line reason in `docs/DECISIONS.md`.

## Environment: Windows 11/10, 16 GB RAM, Docker Desktop (WSL2)
- Nothing may depend on `make`, `bash`, `sed`, `grep`, or `curl` on the host. The task runner is `go run ./cmd/ctl <command>`. A Makefile may exist only as a thin alias for CI.
- Files use LF line endings (`.gitattributes` enforces it). Containers run compiled binaries, never mounted shell scripts.
- Postgres data lives on a named Docker volume, never a Windows bind mount.
- Keep the total container memory under about 6 GB. Limits: Postgres 1.5 GB, Redis 256 MB, each Go service 128 MB.
- Ports published to the host bind to 127.0.0.1 only.

## Repository layout (do not rearrange without asking)
`cmd/<service>/` one folder per binary (api, reaper, worker-example, ledger, baseline-api, baseline-worker, loadgen, verifier, chaos, dashboard, seed, ctl). `internal/` private packages (store, jobs, auth, idempotency, backoff, breaker, audit, config). `sdk/worker/` public Go SDK. `migrations/` versioned and reversible. `web/dashboard/` vendored static assets, no build step, no CDN. `docs/` documentation, prompts, evidence. `artifacts/` and `results/` are generated and git-ignored (results are committed only as `results/results.json` after a real benchmark).

## Naming and style
- Go: standard `gofmt`, `go vet`, `golangci-lint` clean. Small packages, explicit errors, no global mutable state, `context.Context` first parameter.
- SQL: snake_case, explicit column lists (never `SELECT *` in application code), parameter placeholders only.
- JSON fields snake_case. Error body: `{"error":{"code":"...","message":"..."}}`.
- Commit messages: `type(scope): summary` (feat, fix, test, docs, chore). Small commits. `main` stays green.

## The engine laws (never break; tests must protect each one)
1. **Acknowledge after commit.** `POST /v1/jobs` returns 202 only after the transaction has committed. If the database is unreachable return 503, never 202.
2. **Database time is the only clock.** Leases, expiry, `run_at`, and idempotency TTL use SQL `now()`. No code compares a worker's or API's clock to a lease time.
3. **Fencing on every state change by a worker.** Heartbeat, complete, and fail require the current `fencing_token` and `state = 'LEASED'`. Zero rows updated means `409` and the result is discarded.
4. **Tenant comes from the API key, never from the request.** Every query that touches tenant data takes `tenant_id` as a required argument. A foreign job ID returns 404.
5. **Idempotency key to the downstream is stable across attempts**: `steadfast:<job_id>`. The fencing token travels in a separate header and is never part of the key.
6. **Nothing is silently dropped.** Every accepted job ends `SUCCEEDED` or `DEAD` with a reason. A reaper-expired job with `attempts >= max_attempts` goes to `DEAD`.
7. **Every state transition is audited** by a database trigger; operator actions also name the actor.
8. **The verifier never grades itself.** It reads only: the load generator's ack files, the ledger export, and Postgres through the read-only role (plus the baseline `failed` list through a read-only Redis user). It never reads dashboard streams or calls Steadfast APIs.
9. **No unmeasured numbers.** Any latency, throughput, or recovery number in code, docs, or slides must come from `go run ./cmd/ctl bench` or a recorded run, with hardware stated. Never invent or round up a result.
10. **Chaos and mutation switches work only when `STEADFAST_ENV=demo`.** Otherwise the process refuses to start with them.

## How you work on every prompt
1. Use Planning mode. Read the spec sections and rules named in the prompt, then the existing code, before writing anything.
2. Produce a short plan and wait for approval when the prompt says so or when the task is risky.
3. Implement in small commits, writing tests alongside the code.
4. Run the exact verification commands in the prompt and paste real output in your report.
5. End with the report format in `20-quality-process.md`.
