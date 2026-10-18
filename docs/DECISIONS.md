# Steadfast: Architecture and Tooling Decision Record

## Stack Decisions (SPEC Appendix H & J)

| ID | Decision | Reason |
|---|---|---|
| D1 | Steadfast uses Postgres only; Redis exists only for the baseline queue. Circuit state lives in Postgres. | Fewer moving parts; allows defensive claim: "Steadfast has no Redis dependency." |
| D2 | Job states: `QUEUED`, `LEASED`, `SUCCEEDED`, `DEAD` (no `RUNNING` state). | `RUNNING` does not distinguish from `LEASED`; fewer states prevents synchronization bugs. |
| D3 | Outbox + webhooks are P2 and not claimed. | Avoids hardening extra components without demo payoff. |
| D4 | Fairness, invariant I5, and OpenTelemetry/Jaeger are P2. | Kept out of critical path to protect time for verifier and chaos hardening. |
| D5 | Workers talk to the API only; no worker database credentials. | Minimizes attack surface; enables language-agnostic worker SDK. |
| D6 | Downstream idempotency key = `steadfast:<job_id>`; fencing token sent as separate header. | Key changing per attempt would defeat downstream deduplication. |
| D7 | Baseline queue = visibility timeout, AOF `everysec`, no downstream key, plus third lane "baseline + key". | Demonstrates gap is bigger than idempotency keys; prevents dismissal as a strawman. |
| D8 | Reaper dead-letters exhausted jobs; invariant I2b; ledger `apply_then_fail` fault mode. | Prevents crash-loop hole; verifier catches "succeeded but not charged"; models real-world double-charge failure modes. |
| D9 | Demo has `full` (~4 min) and `short` (~90 s) profiles. | Accommodates short presentation slots without risking overflow. |
| D10 | Custom Go load generator instead of k6 for demo. | Load generator must write its own cryptographic acknowledgement files for independent auditor. |
| D11 | Demo timing profile: lease 10s, backoff cap 15s, breaker cooldown 10s. | Shorter intervals allow observers to watch crash recovery live. |
| D12 | Team roles: human teammates handle slides, backup video, and rehearsal; agent writes code sequentially. MIT license. | Prevents merge conflicts and drifts during rapid development. |
| D13 | `cmd/ctl` Go CLI is the primary task runner on all platforms. | Windows host lacks `make`/`bash`/`sed` by default; single binary CLI guarantees consistency. |
| D14 | Default demo profile is `short` (~90 seconds). | Guarantees compliance with standard pitch timeframes. |
| D15 | Standard 5-criteria rubric mapping maintained. | Preserves evaluation defensibility regardless of final judge rubric. |
| D16 | Windows 11/10 with Docker Desktop WSL2 backend, 9 GB WSL cap, repo in WSL2 filesystem. | Ensures stable container isolation and fast disk I/O on 16 GB laptop. |

---

## Architectural & Planning Decisions (Prompt 02)

| ID | Decision | Reason |
|---|---|---|
| D17 | PostgreSQL driver: `github.com/jackc/pgx/v5` and `pgxpool`. | Industry-standard driver for PostgreSQL in Go; supports high-performance connection pooling, binary protocol, and parameterized queries. |
| D18 | Database migrations: internal runner in `internal/store/migrate.go` executing `migrations/*.up.sql` and `migrations/*.down.sql`. | Zero external runtime dependencies; deterministic execution embedded directly in Go binaries; tracks applied versions in `schema_migrations`. |
| D19 | HTTP router: Go standard library `net/http` (`http.ServeMux` with Go 1.22+ pattern routing). | Zero third-party router dependencies; built-in method matching (`POST /v1/jobs`) and path parameter extraction (`GET /v1/jobs/{id}`). |
| D20 | Structured logging: Go standard library `log/slog` with JSON handler. | Zero external dependencies; high performance; outputs structured JSON logs with request IDs and trace attributes. |
| D21 | SQLite driver for Mock Ledger: `modernc.org/sqlite`. | 100% pure Go SQLite driver without CGO; cross-compiles cleanly across Windows and Linux without gcc requirements. |
| D22 | Automated test database: dedicated compose test container on port `5433` managed via `ctl test`. | Guarantees integration tests run against real PostgreSQL as required by quality rules, avoiding Docker socket mounting issues on Windows WSL2. |
| D23 | Set-based correlation for Invariant I2b and explicit drain states. | Verifier performs symmetric difference join on `(tenant_id, job_id)` between PostgreSQL and SQLite charges to prevent count cancellation; drain timeout exits with `INCOMPLETE (DRAIN_TIMEOUT)`. |

---

## Tooling and Dependency Pinning Record (Prompt 01)

| Tool / Component | Pinned Version | Source & Reason |
|---|---|---|
| **Go** | `1.27.2` | Official Go release (`go.dev/dl`, Oct 2026). Latest stable release with generics, memory allocator enhancements, and security patches. |
| **PostgreSQL Image** | `postgres:17.4-alpine` | Official Docker Hub PostgreSQL image. Stable 17 series on Alpine Linux for minimal memory footprint and fast startup. |
| **golangci-lint** | `v2.15.0` | Official GitHub release (`golangci/golangci-lint`). Pinned version for linting Go 1.27 code without deprecation warnings. |
| **gitleaks** | `v8.30.1` | Official GitHub release (`zricethezav/gitleaks`). High-precision secret scanner to prevent credential leaks. |
| **govulncheck** | `v1.9.0` | Official Go security tooling (`golang.org/x/vuln/cmd/govulncheck`). Updated release supporting Go 1.27 SSA analysis. |

---

## Environment & Path Evaluation: WSL2 vs Windows (Prompt 01 Requirement 8)

### Evaluation Finding
1. **Windows UNC Network Paths (`\\wsl$\Ubuntu\home\dkvis\steadfast`)**:
   - Accessing the WSL2 ext4 filesystem through Windows 9P UNC share causes Windows `go.exe` to fail with `Incorrect function` (Win32 `ERROR_INVALID_FUNCTION`) because 9P does not support Windows `LockFileEx` record locking.
   - Built-in IDE tool security policies restrict direct read/write tool calls on UNC network roots.
   - Command shells running against UNC paths issue `CMD.EXE was started with the above path as current directory. UNC paths are not supported.` warnings.

2. **WSL2 Native Linux Filesystem (`/home/dkvis/steadfast`)**:
   - POSIX file locking (`flock`/`fcntl`) functions perfectly.
   - Full Linux ext4 performance and atomic rename semantics.
   - Strict `eol=lf` line ending enforcement without Windows CRLF interference.
   - Go compiler, linters, and test suites run in milliseconds.
   - Docker Desktop CLI is accessible via WSL Windows interop.

### Recommended Path Configuration
Keep the repository located exclusively inside the WSL2 Linux filesystem (`/home/dkvis/steadfast`). In Windows PowerShell, the wrapper `go.cmd` forwards commands directly into the WSL2 environment, ensuring identical behavior and zero file locking conflicts across both PowerShell and Linux shells.
