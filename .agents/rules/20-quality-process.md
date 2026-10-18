# Steadfast: quality, evidence, and reporting rules (always apply)

## Definition of done (every prompt, every feature)
A feature is done only when all are true:
1. It works end to end through the real API or CLI, not only in a unit test.
2. At least one automated test covers it, and for reliability features a failure-mode test passes (crash, kill, stale token, duplicate request, outage).
3. Negative tests exist for invalid input and for permissions (wrong tenant, missing key).
4. `go run ./cmd/ctl lint`, `go vet`, and `go run ./cmd/ctl test` pass with real output shown.
5. `go run ./cmd/ctl scan` (secret scan and vulnerability scan) reports no new high-severity finding.
6. It survives `go run ./cmd/ctl clean` followed by `go run ./cmd/ctl up` from a clean state.
7. It appears correctly in logs, the audit log, or the dashboard where the spec says so.
8. `README.md` or the relevant doc has 2 to 3 lines about it.
9. No secrets in the diff. No leftover debug code. No TODO without an owner and a prompt number.

## Evidence rule (most important)
You must show real command output for every acceptance criterion. Do not write "tests pass" or "verified" without the output. If you could not run something, list it under "Not verified" with the reason. Fabricated or assumed results are the worst possible outcome for this project, because the project is about proof.

## Testing guidance
- Reliability tests use real Postgres in Docker, not mocks. Use whichever approach was approved in prompt 2 (for example testcontainers or a compose test file).
- Concurrency tests must be repeatable: fixed seeds where randomness is used, and run at least 3 times in a row before reporting green.
- Time-dependent tests use short configured intervals (for example 1-second leases), never long sleeps that make the suite slow. The full suite should finish in a few minutes.
- A flaky test is a bug. Do not retry-until-green. Find the cause or report it.

## Scope discipline
- Do exactly what the prompt asks. List every file you changed in the report. If you changed a file outside the prompt's scope, revert it or explain it and wait for approval.
- Do not refactor unrelated code. Do not rename public APIs. Do not "improve" the spec.
- If the prompt and the spec disagree, or you see a better design, stop and propose it. Do not implement it unasked.

## Numbers and claims
- Performance and recovery numbers come only from `go run ./cmd/ctl bench` or a recorded demo run. Record hardware automatically (OS, CPU, RAM, Docker Desktop version) in `results/results.json`.
- Never describe a guarantee stronger than the spec: "survives a hard kill of the Postgres container", "effectively-once effects when the downstream honours the idempotency key", "at-least-once delivery". Never write "exactly-once".
- Do not name a specific third-party product as broken. Describe the pattern.

## Report format (end every prompt with this)
1. **Summary**: 3 to 5 sentences of what now works.
2. **Files changed**: list.
3. **Acceptance criteria**: each criterion, its status, and the evidence (command and output, or file path).
4. **Commands run**: exact commands with pass/fail.
5. **Not verified**: anything you could not run or check, and why.
6. **Assumptions or deviations**: each one, plainly. If it affects behavior, you should have asked first.
7. **Risks and suggestions**: at most 3.
8. **Next prompt readiness**: yes or no, with what blocks it.

## Stop and ask the user when
- The spec is ambiguous, contradictory, or impossible on Windows/Docker Desktop.
- A test fails and the cause is not clear after one honest investigation.
- You would need to break an engine law, add a dependency, change the schema or an API shape, or exceed the memory budget.
- The verifier and the system disagree and you cannot tell which is wrong (use the debug prompt "checker disagrees", never edit the verifier to make it pass).
