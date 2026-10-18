# Prompt 12: security pass, full tests, benchmark, documentation (mandatory)
## Goal
A defensible, honest package: scans clean, tests green, every number measured, every claim backed, every limit stated.
## Context
Prompts 01 to 09 done (10 and 11 if built). Spec sections 8 and 9, Appendix C. Rules: all, especially engine law 9 and the "Numbers and claims" section of `20-quality-process.md`.
## Scope
In scope: security fixes found by scans, test gaps, `ctl bench`, `results/results.json`, README, ARCHITECTURE, LIMITATIONS, DEMO, RUNBOOK, a slide-ready results table.
Out of scope (do not touch): new features, schema changes, API shape changes.
## Requirements
1. Run `ctl scan` (gitleaks, govulncheck, static analysis) and fix every high-severity finding; list lower findings with a decision for each.
2. Re-run all cross-tenant tests and the full suite three times; fix or report flaky tests.
3. `ctl bench`: measure on this machine and write `results/results.json` including OS, CPU, RAM, Docker Desktop version, WSL memory cap, container limits. Metrics: submission latency p50 and p99, sustained throughput on a single Postgres (state test duration and worker count), lost acknowledged jobs under chaos, duplicate side effects with idempotent handlers, recovery after worker crash, recovery after Postgres container restart. Include one throughput-versus-time series from a chaos run (CSV and a PNG or SVG chart).
4. Containers run non-root where possible; security headers present; Origin checks verified; Docker socket risk documented; `.env` and `artifacts/` ignored.
5. Documentation: README (what it is, one-command quick start for Windows and Linux, architecture diagram, how to run the demo, how to run the verifier against the baseline, results table with hardware stated), `docs/LIMITATIONS.md` (non-idempotent handlers can still duplicate external effects; single Postgres is the ceiling; no zero-loss failover claimed, it needs synchronous replication; Docker Desktop VM durability wording from J4; roadmap items are not shipped), `docs/DEMO.md`, `docs/RUNBOOK.md`, `docs/BASELINE-CONFIG.md` check.
6. Every sentence in README or docs that states a number references `results/results.json` or a recorded run.
## Security and privacy checklist
Everything in `10-security.md` re-verified with evidence; no secrets in git history (`gitleaks` on full history); dependency list reviewed.
## Method
1. Read spec and rules. 2. Proceed after planning. 3. Small commits. 4. Tests alongside.
## Acceptance criteria
- `ctl scan` shows no high-severity finding; paste output.
- `ctl test` green 3 times in a row.
- `results/results.json` exists with hardware recorded; the README results table matches it exactly.
- LIMITATIONS lists every item above; DEMO and RUNBOOK exist and are accurate.
- A search of docs for "exactly-once" returns nothing.
## Verification
`go run ./cmd/ctl scan`; `go run ./cmd/ctl test` three times; `go run ./cmd/ctl bench`; show `results/results.json`; gitleaks on full history.
## Report back
Standard report format, including a table of every number published and its source.
## Stop and ask if
A measured number is much worse than a stretch target (do not tune blindly or hide it; propose options).
