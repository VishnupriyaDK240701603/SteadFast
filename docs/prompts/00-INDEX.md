# How to run these prompts (one agent, in sequence)

Before prompt 1: copy `agents-rules/*.md` into `.agents/rules/` in the repo and `SPEC.md` into `docs/SPEC.md`. Copy this `prompts/` folder into `docs/prompts/`.

To run a prompt, paste this into Antigravity (Planning mode unless noted), replacing NN:
> Execute `docs/prompts/NN-<name>.md`. Read `docs/SPEC.md` and the rules in `.agents/rules` first. Follow the Method section exactly and end with the report format from `20-quality-process.md`.

After each prompt, the team lead does a 5-minute review: read the report; run the Verification commands yourself and compare; run `git log --oneline -10`; if anything fails or looks claimed-but-unshown, use `docs/DEBUG-PROMPTS.md`. Then tag the commit `after-pNN` so you can roll back.

| # | File | Builds | Tier | Plan approval needed |
|---|---|---|---|---|
| 01 | 01-setup.md | repo, tooling, compose skeleton, CI, secrets | core | no |
| 02 | 02-architecture-plan.md | architecture + threat model (docs only) | core | YES |
| 03 | 03-schema-auth.md | migrations, auth, roles, audit trigger | core | no |
| 04 | 04-submit-idempotency.md | F1, F2 | core | no |
| 05 | 05-worker-api-reaper-dlq.md | F3, F4, F5, F6 | core | YES |
| 06 | 06-sdk-ledger.md | F7, F8 | core | no |
| 07 | 07-loadgen-baseline-verifier.md | F10 + checkpoint | core | YES |
| 08 | 08-chaos-mutation.md | F9, I4, mutation check | core | no |
| 09 | 09-dashboard-demo.md | F11, demo profiles | core | YES |
| 10 | 10-circuit-breaker.md | F12 | P1 (cut first) | no |
| 11 | 11-shutdown-ratelimit.md | F13 | P1 (cut first) | no |
| 12 | 12-hardening-bench-docs.md | scans, bench, README, limitations | mandatory | no |
| 13 | 13-demo-hardening.md | rehearsals, flake fixes, evidence | mandatory | no |
| 14 | 14-audit.md | acceptance audit + clean-clone test | mandatory | no |

Cut rule: if time is short, skip 10 and 11, never 12 to 14. Checkpoint: after prompt 07 there must be a real verified result. If not, stop and fix the checker before anything else.
