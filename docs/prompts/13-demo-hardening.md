# Prompt 13: demo hardening and rehearsal tooling (mandatory)
## Goal
The demo works every time on the presentation laptop, and every beat has a rehearsed fallback.
## Context
Prompts 01 to 12 done. Spec Appendix D, J3, J5. Demo profiles: `short` (default, about 90 s) and `full` (about 4 min).
## Scope
In scope: `ctl rehearse`, flake fixes in demo scripts and tuning (load rate, handler latency, timings), evidence files, fallback assets, resource report.
Out of scope (do not touch): engine behavior (a demo-exposed engine bug is reported, not patched silently; ask).
## Requirements
1. `ctl rehearse --profile short|full --runs N` runs the complete demo headless N times from a clean state (`clean`, `up`, `seed`, demo, verdict), records pass or fail, timings, and verifier exit code per run in `docs/evidence/rehearsal-<date>.json`, and prints a summary.
2. Tune load rate and handler latency so at least 500 jobs are queued or leased in each system when the kills happen; show the measured numbers.
3. Fix every flaky step; any run that fails is investigated using the debug prompts, not retried silently.
4. Fallbacks: (a) a script that plays beat 2 using `ctl chaos` commands; (b) the pre-seeded poison job check; (c) `ctl screenshot` that saves the dashboard and verdict view after a verified run to `docs/evidence/` as the static fallback; (d) a checklist for the human to record the backup video (under 3 minutes) of a verified run.
5. Resource report: `docker stats` during a full run showing total memory under the budget in J3; note the WSL cap used.
6. Offline check: run a short demo with the network adapter disabled and record the result.
7. Pitch timing file `docs/DEMO.md` lists each beat with start and end timestamps measured, for both profiles.
## Security and privacy checklist
Evidence files contain no secrets; fallbacks do not weaken demo-only guards.
## Method
1. Read spec and rules. 2. Proceed after planning. 3. Small commits. 4. Tests alongside.
## Acceptance criteria
- 5 consecutive `short` runs pass (verifier exit 0, all four beats complete) and 2 consecutive `full` runs pass; paste the summary.
- The 500-in-flight condition is demonstrated in each run's evidence.
- Total container memory stays under 6 GB; paste the `docker stats` summary.
- Offline run passes.
- Static fallback screenshots exist.
## Verification
`go run ./cmd/ctl rehearse --profile short --runs 5`; `go run ./cmd/ctl rehearse --profile full --runs 2`; resource and offline evidence files.
## Report back
Standard report format, with the rehearsal summary table.
## Stop and ask if
Any run fails twice for an unexplained reason; the memory budget is exceeded.
