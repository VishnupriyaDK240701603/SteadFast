# Prompt 08: chaos controller, zombie test, test-the-tester (F9, invariant I4)
## Goal
Safe, scriptable failure injection, a proof that zombie writes are rejected, and proof that the verifier can actually fail.
## Context
Prompts 01 to 07 done and the checkpoint passed. Spec F9, Appendix C (I4 and "Test the tester"), `10-security.md` (Docker socket, demo-only switches). Rules: engine law 10.
## Scope
In scope: `cmd/chaos`, `ctl chaos ...`, `ctl mutation-check`, the I4 zombie scenario in `cmd/verifier`, tests, docs on the socket risk.
Out of scope (do not touch): dashboard UI, circuit breaker, engine logic except wiring the guarded mutation flags.
## Requirements
1. Chaos service with an allow-list of actions: `kill-workers {fraction}`, `kill-postgres`, `start-postgres`, `kill-redis`, `start-redis`, `ledger-fault {mode...}`, `clear-faults`. Targets are selected only by a compose label (for example `steadfast.chaos=allow`). Anything else is refused.
2. It starts only when `STEADFAST_ENV=demo`; binds to 127.0.0.1; requires `ADMIN_KEY`; checks `Origin` on state-changing requests; logs every action with a timestamp. `ctl chaos <action>` calls the same code path.
3. Kills use SIGKILL semantics (`docker kill`), restarts use `docker start`, so Postgres loses no clean shutdown.
4. Zombie scenario (I4) in the verifier: a test worker claims a job (token n) and is paused past the lease; the reaper requeues; a second worker claims (token n+1) and completes; the first resumes and completes with token n and must get 409; the job stays `SUCCEEDED`; the ledger has exactly 1 applied entry.
5. `ctl mutation-check` performs: (a) start with `STEADFAST_DISABLE_FENCING=1`, run the zombie scenario and a chaos run: the verifier must report I4 failed; (b) start with `STEADFAST_DISABLE_IDEMPOTENCY=1` and ledger `apply_then_fail`: I2 must fail; (c) on a copy of the database delete one acknowledged job: I1 must fail. Then restore normal settings and confirm all pass. The command exits 0 only if every mutation was detected.
6. Document the Docker socket risk and the mitigations in `docs/RUNBOOK.md` and the README ("never deploy the chaos service").
## Security and privacy checklist
Allow-list for actions and containers; localhost bind; admin key; Origin check; refuses to start outside demo mode; mutation flags refused outside demo mode with a loud warning; no shell execution of user-supplied strings (use the Docker API or fixed argument lists).
## Method
1. Read spec and rules. 2. Proceed after planning. 3. Small commits. 4. Tests alongside.
## Acceptance criteria
- A9.1: every action works via the service and via `ctl chaos`. A9.2: a non-labelled container name is refused. A9.3: the service refuses to start without `STEADFAST_ENV=demo`.
- Killing 50% of workers and then Postgres during a 2,000-job run, then restarting, ends with the verifier passing I1 to I3.
- I4 passes on the normal build and FAILS with fencing disabled.
- `ctl mutation-check` exits 0 and shows each mutation detected; paste the output.
- Chaos endpoints return 401 without the key and 403 on a bad Origin.
## Verification
`go run ./cmd/ctl up`; each chaos action; the 2,000-job chaos run with verdict; `go run ./cmd/ctl mutation-check`; `go run ./cmd/ctl test`; `go run ./cmd/ctl lint`; `go run ./cmd/ctl scan`.
## Report back
Standard report format.
## Stop and ask if
The Docker socket cannot be reached from a container on this Docker Desktop setup (propose a host-side alternative; do not widen permissions).
