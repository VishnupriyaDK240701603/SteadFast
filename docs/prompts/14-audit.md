# Prompt 14: pre-submission audit and clean-clone test (mandatory)
## Goal
Independent proof that what we submit matches the spec, works from a fresh clone, and claims nothing it cannot show.
## Context
All earlier prompts done. Spec: all sections. Rules: all. You are now an auditor, not a builder: do not add features.
## Scope
In scope: `docs/AUDIT.md`, tiny fixes for broken docs or typos, final tags.
Out of scope (do not touch): any code change beyond a one-line fix for something the audit proves broken (list each such fix).
## Requirements
1. For every acceptance criterion in SPEC section 3 and every invariant in Appendix C, write a row in `docs/AUDIT.md`: criterion, command or test that proves it, the real output or file path, status (verified, partially verified, not verified), notes.
2. Run `ctl mutation-check` and `ctl scan` one last time and record output.
3. Clean-clone test: clone the repository into a brand-new directory (ideally on another machine), follow only the README quick start, run `ctl up`, `ctl demo-short --headless`, and confirm the verifier exits 0. Record every place where the README was wrong or incomplete and fix the README.
4. Check claims: every number in README or slides data matches `results/results.json`; LIMITATIONS present; no "exactly-once"; no feature claimed that is marked cut; license and README present; public repo history has no secrets.
5. Produce a submission checklist status (from the spec) and a one-page "what we can and cannot claim" list for the pitch team.
## Security and privacy checklist
Re-run secret scan on full history; confirm demo-only switches refuse to start outside demo mode.
## Method
1. Read spec and rules. 2. Proceed after planning. 3. Evidence for every row.
## Acceptance criteria
- `docs/AUDIT.md` has no empty status cell; every "not verified" item has a reason and an owner.
- Clean-clone run passes.
- The "can and cannot claim" list exists.
## Verification
The commands above, with pasted output.
## Report back
Standard report format, ending with the list of anything not verified.
## Stop and ask if
An audit row cannot be verified and the cause is a defect (report it; do not hide it).
