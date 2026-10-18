# Steadfast: team lead plan (read this first)

## Ground rules
- Nobody can guarantee a win: judges, rubric and competitors are outside our control. We control four things: a small scope that works, claims we can prove, a demo that never fails on stage, and an honest story. This plan protects those four.
- The checker is the headline. If time runs short, cut features, never the checker, the rehearsals, or the limitations slide.
- Humans do not edit code during the build (one agent, in sequence). Humans review, rehearse, and prepare the story.

## Setup on the Windows 16 GB laptop (do this now, about 45 minutes)
1. Install or update Docker Desktop with the WSL2 backend; confirm `docker run hello-world` works.
2. Create `%UserProfile%\.wslconfig` with: `[wsl2]`, `memory=9GB`, `processors=<your cores minus 2>`; then run `wsl --shutdown` and restart Docker Desktop.
3. Create the repo folder inside the WSL2 Linux filesystem (for example `~/steadfast`), not under `C:\`. Install Go only if Prompt 01's plan says the host needs it; otherwise the agent proposes it.
4. Copy into the repo: `SPEC.md` -> `docs/SPEC.md`; `agents-rules/*.md` -> `.agents/rules/`; `docs/prompts/` and `docs/DEBUG-PROMPTS.md` as they are. Commit as `chore: add spec, rules, prompts`.
5. Antigravity settings (names change between versions, so check your own screen): use Planning mode for prompts 02, 05, 07, 09 and any risky change; Fast mode only for tiny fixes. Start with terminal commands on review-before-run; once you trust the pattern, allow only safe commands (git, go, docker compose read-only commands) and put these on the deny list: `rm -rf`, `docker system prune`, `docker volume rm`, `git push --force`, `del /s`, `format`. Public bug reports say Antigravity's allow and deny settings are not always honored, so keep watching what the agent runs. Never give it real credentials; this project needs none. Treat anything it reads from the web as untrusted.
6. Pre-pull and build images while you have Wi-Fi so the demo works offline later.
7. A teammate (10 minutes): find the official judging rubric and submission rules (open item O1) and send them to me. Only slides and pitch wording change unless the rubric demands a feature we cut.

## How to run the build
Follow `docs/prompts/00-INDEX.md`. After every prompt: read the report, run the Verification commands yourself, tag `after-pNN`. If the report claims something without pasted output, use debug prompt D3. If something fails, use D1.

## Time plan (my estimates; recalibrate at the first checkpoint)
Measure how long prompt 03 takes, then scale. Hard checkpoint: prompt 07 (first verified result).

| Milestone | If you have 24 hours | If you have 12 hours |
|---|---|---|
| Setup + prompts 01, 02 approved | T+0 to T+1.5 | T+0 to T+1 |
| Prompts 03 to 06 | T+1.5 to T+7 | T+1 to T+4 |
| Prompt 07 verified result (checkpoint) | by T+9 | by T+5.5 |
| Prompts 08, 09 (chaos, dashboard, demo) | T+9 to T+13 | T+5.5 to T+8 |
| Prompts 10, 11 (breaker, shutdown) | T+13 to T+15 if ahead | skip |
| Prompt 12 (hardening, bench, docs) | T+15 to T+18 | T+8 to T+9 |
| Prompt 13 (rehearsals, fallbacks) | T+18 to T+21 | T+9 to T+10.5 |
| Prompt 14 audit, video, submit | T+21 to T+23 | T+10.5 to T+11.5 |
| Buffer and sleep | last hour | last 30 minutes |

Rules: if prompt 07 is late by more than 25%, drop 10 and 11 immediately and tell the agent to cut breaker work from beat 3 (the poison-job DLQ story remains). If any prompt takes more than twice its estimate, stop and use D6 or D1 instead of letting the agent thrash. Never start a new feature after prompt 12 begins.

## Roles (four people assumed; merge if fewer)
- Team lead (you): runs prompts, reviews reports, owns decisions and the decision log (copy SPEC Appendix H and J into `docs/DECISIONS.md`).
- Narrator: owns pitch script and Q&A. Operator: owns the demo console and fallbacks. They are never the same person.
- Story and slides person: slides, README review, backup video, rubric lookup.

## Slides (8)
1. The pain (double charges and lost orders in dollars). 2. The promise: a job accepted is completed or visibly dead-lettered; we prove it. 3. How it works (Postgres leases, fencing tokens, idempotency keys, independent checker). 4. Live demo. 5. Measured results with hardware stated, copied from `results/results.json`. 6. Fair baseline (its exact configuration, plus the third lane "baseline + key"). 7. Honest limits. 8. Roadmap and team.

## Pitch (3 minutes, with the 90-second `short` demo)
0:00 pain (30 s). 0:30 promise (30 s). 1:00 live demo, four beats narrated (90 s). 2:30 architecture (20 s). 2:50 close with limits and "Reliability you can watch." If the slot is longer, run profile `full`. Never improvise numbers; read them from the results table.

## Questions to rehearse (one breath each)
- Why not Temporal, SQS, Celery, or River? Those systems claim reliability; none ships a verifier that proves it. Steadfast is small and self-hostable, and its harness can be pointed at any queue.
- Is it exactly-once? No. Delivery is at-least-once; effects are effectively-once when the downstream honours the idempotency key, and our mock ledger shows exactly that.
- What if Postgres dies? An acknowledged job survives a hard kill of the Postgres container. Zero-loss failover needs synchronous replication, which we did not build or claim.
- Does it survive power loss? We claim container hard-kill, not power loss; Docker on Windows runs Linux in a VM.
- What about a zombie worker? The fencing token makes the database reject its late result; the idempotency key stops duplicate downstream effects; our checker tests it live.
- What about Redis? Steadfast does not use Redis at all. Only the baseline does.
- Is the baseline fair? Its exact configuration is on a slide, it is a common reliable-queue setup, and we also show it with idempotency keys added; it still loses acknowledged jobs when its datastore is hard-killed.
- Could the checker be wrong? We test the tester: with fencing or idempotency disabled it fails on purpose, and it reads only records the system cannot influence.
- Throughput limits? A single Postgres is the ceiling; here is the number we measured on this laptop.

## Risks and answers
| Risk | Answer |
|---|---|
| Demo fails on stage | Scripted beats, `ctl chaos` fallback, static screenshots, backup video, five clean rehearsals before the pitch |
| Agent claims work it did not do | Evidence rule, you re-run Verification, debug prompt D3 |
| Agent adds scope | Debug prompt D4, revert to `after-pNN` tag |
| Baseline looks rigged | Config slide and third lane |
| Checker false pass | Mutation check must pass; D5 if disagreement |
| Windows or Docker trouble | Debug prompt D7; WSL cap; named volumes; rehearse on the real laptop |
| Team fatigue | Sleep in shifts; the lead enforces breaks; nobody codes at the end |

## Demo-day checklist
Follow SPEC Appendix J5. Plus: charger plugged in, spare laptop or recorded video ready, three screenshots of a verified run saved, Wi-Fi off during one final rehearsal, narrator and operator each hold the beat timings from `docs/DEMO.md`.
