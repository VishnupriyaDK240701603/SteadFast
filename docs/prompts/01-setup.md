# Prompt 01: repository, tooling, compose skeleton, CI, secrets
## Goal
A clean, cross-platform (Windows + Docker Desktop) project skeleton where `go run ./cmd/ctl up` starts a healthy Postgres and CI-style checks run.
## Context
Empty repo. Read `docs/SPEC.md` sections 1, 8, Appendix E, F, I, J and all rules in `.agents/rules`. This prompt creates structure only: no feature code.
## Scope
In scope: git, license, ignore files, Go module, `cmd/ctl` skeleton, docker-compose with Postgres only, `.env.example`, CI workflow, docs skeleton, README quick start.
Out of scope (do not touch): any API, schema, worker, ledger, dashboard code.
## Requirements
1. `git init`; MIT `LICENSE`; `.gitignore` (`.env`, `artifacts/`, `results/` except `results/results.json`, `*.db`, build outputs); `.gitattributes` with `* text=auto eol=lf`.
2. Go module and folder layout exactly as SPEC Appendix F, with a minimal `doc.go` per package. Read current official docs to choose and pin the latest stable Go and tool versions; record each in `docs/DECISIONS.md` with a one-line reason.
3. `cmd/ctl`: a CLI with subcommands `up`, `down`, `clean`, `test`, `lint`, `scan` working in PowerShell and in a Linux shell. Other subcommands (seed, demo, chaos, verdict, bench, rehearse, mutation-check) print "not implemented yet" and exit 2. No dependency on make/bash/sed on the host.
4. `docker-compose.yml` with only `postgres`: named volume, `fsync=on`, `synchronous_commit=on`, `full_page_writes=on` set explicitly, port bound to 127.0.0.1, memory limit 1.5 GB, healthcheck, non-default database and user from environment.
5. `.env.example` with fake values and comments; `ctl up` refuses to run if `.env` is missing and tells the user to copy the example (never auto-generates real secrets here).
6. CI: `.github/workflows/ci.yml` running vet, golangci-lint, tests, govulncheck, gitleaks. The same checks are available locally through `ctl lint` and `ctl scan`. If a tool cannot run natively on Windows, run it through a pinned container image and say so.
7. Copy rules into `.agents/rules/`. Create `docs/` skeleton (ARCHITECTURE, DECISIONS, DEMO, LIMITATIONS, RUNBOOK, evidence/). `README.md` with a Windows + Docker Desktop (WSL2) quick start and the J1 note about where to keep the repo.
8. Determine whether the repo works when opened from WSL2 versus Windows paths in this Antigravity install; record the finding and the recommended path in `docs/DECISIONS.md`.
## Security and privacy checklist
No secrets in repo; `.env` ignored; gitleaks configured; ports bound to localhost; pinned versions with lockfile; no `curl | sh` installs.
## Method
1. Read spec sections and rules. 2. Proceed after planning (state the plan briefly). 3. Small commits. 4. Tests alongside code (a trivial test per package is enough here).
## Acceptance criteria
- `ctl up` brings Postgres to healthy; `ctl down` and `ctl clean` work; data volume persists across `down`/`up`.
- `ctl test`, `ctl lint`, `ctl scan` run and pass.
- `git ls-files --eol` shows no CRLF in tracked text files.
- Postgres shows `fsync = on` and `synchronous_commit = on` (query `SHOW`).
- `docs/DECISIONS.md` lists every pinned version with its reason and the WSL2 finding.
## Verification
Run and paste output: `go run ./cmd/ctl up`; `docker compose ps`; a `SHOW fsync; SHOW synchronous_commit;` query; `go run ./cmd/ctl test`; `go run ./cmd/ctl lint`; `go run ./cmd/ctl scan`; `git ls-files --eol`.
## Report back
Use the standard report format.
## Stop and ask if
A required tool cannot be run on Windows even through a container; the repo cannot be opened in a workable way; any version choice is unclear.
