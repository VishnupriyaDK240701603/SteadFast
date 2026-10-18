# Prompt 02: architecture and threat model (planning only, no feature code)
## Goal
A written, reviewable plan so that prompts 03 to 14 build on settled decisions. Wait for approval before any code.
## Context
Prompt 01 is done. Read `docs/SPEC.md` fully and all rules. You may only create or edit files under `docs/`.
## Scope
In scope: `docs/ARCHITECTURE.md`, `docs/THREAT-MODEL.md`, `docs/TEST-STRATEGY.md`, updates to `docs/DECISIONS.md`.
Out of scope (do not touch): all code, migrations, compose changes.
## Requirements
1. Architecture: component diagram (Mermaid) of every service in Appendix F and which store each touches; sequence diagrams (Mermaid) for submit, claim/complete, crash recovery (worker kill), zombie rejection, Postgres hard kill, DLQ edit and replay, and verifier data flow.
2. Decide and justify: migration tool, logging library (or stdlib `slog`), HTTP router (stdlib preferred), Postgres driver, SQLite driver, test approach for real Postgres (testcontainers versus a dedicated compose test file; must work on Windows Docker Desktop), how services share config and the repository layer that forces `tenant_id`.
3. Threat model table: assets, entry points, threats, mitigation, residual risk, for: API key theft or guessing, tenant crossing, payload abuse, malicious worker, chaos service and Docker socket, verifier tampering, dashboard CSRF, log leakage, dependency risk.
4. Test strategy: a table mapping every acceptance criterion in SPEC section 3 and invariants I1 to I4 to a test type, test name, and the prompt that creates it.
5. A traceability table: every spec feature F1 to F14 mapped to prompt numbers, plus anything in the spec you find ambiguous, impossible, or risky on Windows with 16 GB. List questions; do not guess.
## Security and privacy checklist
Threat model covers each item in `10-security.md`; Docker socket risk and mitigations are explicit.
## Method
1. Read everything. 2. Write the documents. 3. STOP and wait for the user's approval. Do not start prompt 03.
## Acceptance criteria
- All four documents exist and every F-item and acceptance criterion is traceable to a prompt and a test.
- Open questions are listed, none hidden.
- No file outside `docs/` changed (`git status` shows this).
## Verification
`git status`; `git diff --stat`; show the traceability table.
## Report back
Standard report format, ending with the open questions list.
## Stop and ask if
Anything in the spec conflicts with the rules or cannot be done on this environment.
