# Debug and recovery prompts (paste when something goes wrong)

Pick the template that matches the symptom. Always attach the failing output (copy the real error, never summarize it).

## D1. Diagnose first (default for any bug)
> Something is wrong: [describe symptom and paste exact output]. Do not change code yet. Reproduce the problem and show the command and output. State the root cause with evidence. Propose the smallest fix and its risks. WAIT for my approval. After approval: fix it, add a regression test that fails before and passes after, run the full suite, and report using the standard report format.

## D2. Output does not match the spec
> Spec section [X] says: "[quote it]". What you built does: [describe or paste]. List the exact gaps between the two. Propose a correction plan that does not rewrite unrelated code. WAIT for approval, then implement with tests and report.

## D3. Agent claims done without proof
> You reported [claim]. I do not see evidence. For each acceptance criterion of prompt [NN], run the Verification commands now and paste the raw output. Mark anything you cannot run as "Not verified" with the reason. Do not summarize; show output.

## D4. Scope creep or unrequested changes
> List every file you changed in this session with a one-line reason each. Revert anything outside prompt [NN]'s scope using git, show `git status` and `git diff --stat`, and restate the scope you will follow. Do not continue until I approve.

## D5. The checker disagrees with the system
> The verifier reports [invariant] failed. Do not edit the verifier or the ledger to make it pass. First determine which side is wrong: gather the ack file lines, the ledger entries, and the Postgres rows for three affected order IDs and show them side by side. State whether this is (a) an engine defect, (b) a verifier defect, (c) a harness or data-collection problem, or (d) a legitimate violation the demo should show. If (b) or (c), propose the fix and add a fixture test that captures the case. If (a), report it, propose the fix, and WAIT.

## D6. A demo run failed or looked wrong
> Rehearsal run [N] of profile [short/full] failed at beat [X]: [paste output]. Collect: logs of every service for that window, `docker compose ps`, the verifier output, memory usage. Build a timeline of events. Identify the first thing that went wrong, not the loudest. Do not retry until you have a cause. Propose a fix and a test or rehearsal check that would have caught it. WAIT for approval.

## D7. Docker, WSL2, or Windows environment problem
> [Paste the error.] Assume Windows with Docker Desktop (WSL2), 16 GB RAM. Check in this order and show output for each: Docker running and version, `docker compose ps`, WSL memory and CPU settings (`.wslconfig`), disk space, port conflicts on the published ports, line endings in the failing file (`git ls-files --eol`), whether the repo is on the WSL2 filesystem or a Windows path, volume permissions. Name the cause, fix it, and record the fix in `docs/RUNBOOK.md`. Do not run `docker system prune` or delete volumes without asking.

## D8. Performance is worse than expected
> Measured [metric] is [value], lower than expected [target]. Do not tune blindly. Profile first: show database query plans for claim and complete, connection pool settings, CPU and memory per container, and where time goes. Propose at most 3 changes ranked by expected effect and risk to the engine laws. WAIT. We publish only measured numbers; do not adjust expectations in docs without a recorded run.

## D9. Tests passed before and fail now (regression)
> `ctl test` failed after prompt [NN]: [paste output]. Use `git bisect` or `git diff after-p[NN-1]..HEAD` to find the change that introduced it. Show the offending change, explain why it broke the test, and fix the code (not the test) unless the test was wrong; if the test was wrong, justify with the spec. Report with evidence.

## D10. Flaky test
> Test [name] fails intermittently. Run it 20 times and show the pass/fail counts. Find the race or timing assumption causing it. Fix the cause. Do not add retries, sleeps, or skips to hide it. Show 20 consecutive green runs.

## D11. Rollback
> Roll back to tag `after-p[NN]` on a new branch `recover-p[NN]`, show `git log --oneline -5`, rerun the Verification for prompt [NN], and report whether that state is healthy. Do not delete the failed branch.
