# Prompt 16: production-readiness proof page (F17)
## Goal
A `/readiness` page and `ctl evidence` command that show what is proven, what is partial, and what is not built, from real evidence only.
## Context
Run after prompt 12 (scans, bench, docs exist) and before prompts 13 and 14. Read `docs/SPEC.md` Appendix K (F17, D20) and the Numbers and claims section of `20-quality-process.md`.
## Scope
In scope: `ctl evidence`, `docs/claims.json`, `docs/evidence/readiness.json`, the `/readiness` page, tests.
Out of scope (do not touch): engine, verifier, other views, README claims (prompt 14 audits them).
## Requirements
1. `docs/claims.json`: a fixed catalog of claims. Each: id, category (durability, correctness, security baseline, quality, operability, performance), claim text, evidence sources, reproduce command. Include at least: A1.1 durability under container hard kill; A2.1 idempotency under concurrency; A3.1 no double claims; A4.1 crash recovery; A6.2 DLQ edit and replay; I4 zombie rejection; I1 to I3 from the latest verified run; tenant isolation tests; mutation check detects violations; secret scan clean; vulnerability scan no high findings; tests pass; lint clean; non-root containers; offline demo; clean-clone test; every number from `results/results.json`. Add graceful shutdown and the circuit breaker only if built.
2. `ctl evidence` collects the latest outputs (or runs the commands) and writes `docs/evidence/readiness.json`: status (`verified`, `stale`, `partially_verified`, `not_verified`, `not_built`), evidence paths, reproduce command, timestamp, git commit, dirty flag, checksum. Never hand-edited.
3. Status rules: `verified` only if an evidence file exists and was produced at the current commit; otherwise `stale`. Missing evidence is `not_verified`. Anything unbuilt is `not_built`. Numbers displayed come only from `results/results.json`.
4. "Not built" section (always shown): TLS inside the network, API key rotation, row-level security, payload encryption at rest, high-availability failover, backups, metrics and alerting, load testing at scale. "Limits" section copied from `docs/LIMITATIONS.md`.
5. `/readiness` page: counts per status, table by category with filter, expandable rows showing an escaped evidence excerpt (first 40 lines), reproduce command, timestamp and commit; footer with commit, generation time, hardware from `results/results.json`. Read-only, no login, works offline, print-friendly CSS for PDF export.
6. `ctl evidence --check` exits 1 if any claim listed in `docs/slide-claims.txt` is not `verified`.
## Security and privacy checklist
Redaction pass on every excerpt (keys, tokens, connection strings, user profile paths, host names); escape all excerpt text; page exposes no secrets and no write actions; evidence files contain synthetic data only.
## Method
1. Read spec and rules. 2. Proceed after planning. 3. Small commits. 4. Tests alongside code.
## Acceptance criteria
- A17.1 after a new commit, old evidence shows `stale` (test).
- A17.2 no `verified` row lacks an evidence file (test).
- A17.3 a planted fake secret in an evidence file is redacted in the page (test).
- A17.4 the page renders offline and prints cleanly to PDF (screenshot).
- The "Not built" and "Limits" sections are present and match `docs/LIMITATIONS.md`.
- `ctl evidence --check` fails when a slide claim is stale and passes after refresh (paste both outputs).
## Verification
`go run ./cmd/ctl evidence`; `go run ./cmd/ctl evidence --check`; `go run ./cmd/ctl test`; `go run ./cmd/ctl lint`; `go run ./cmd/ctl scan`; screenshot of `/readiness` in `docs/evidence/`.
## Report back
Standard report format.
## Stop and ask if
A claim cannot be backed by an evidence file; a status rule would mark something `verified` without proof.
