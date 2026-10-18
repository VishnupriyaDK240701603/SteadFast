# Steadfast

Steadfast is a reliable, self-hostable asynchronous job runner designed for provable durability and execution guarantees under chaos.

Its headline feature is an **independent verifier** that proves system invariants under live worker crashes, database kills, and poison payloads.

---

## Quick Start (Windows 10/11 + Docker Desktop WSL2)

### 1. Repository Location (SPEC Appendix J1)
> [!IMPORTANT]
> The repository MUST reside inside the native WSL2 Linux filesystem (e.g. `~/steadfast` or `/home/<user>/steadfast`), NEVER under `/mnt/c` or a Windows drive.
> This avoids file locking conflicts, permissions errors, and slow cross-boundary I/O.

### 2. Prerequisites
- Docker Desktop running with the WSL2 backend.
- Go `1.27.2` (or use the built-in `cmd/ctl` wrapper).

### 3. Environment Setup
Copy the example configuration to create your local `.env`:
```bash
# In Linux / WSL2 bash:
cp .env.example .env

# In Windows PowerShell:
Copy-Item .env.example .env
```

### 4. Lifecycle Commands
Use `go run ./cmd/ctl` (or `ctl` directly) to manage services:

```bash
# Start PostgreSQL and wait until healthy
go run ./cmd/ctl up

# Run test suite
go run ./cmd/ctl test

# Run linters (go vet and golangci-lint)
go run ./cmd/ctl lint

# Run security checks (gitleaks and govulncheck)
go run ./cmd/ctl scan

# Stop containers
go run ./cmd/ctl down

# Clean all containers, volumes, and temporary artifacts
go run ./cmd/ctl clean
```

---

## Core Engine Laws
1. **Acknowledge after commit**: `POST /v1/jobs` returns 202 only after the transaction commits to disk (`fsync=on`, `synchronous_commit=on`).
2. **Database time is the only clock**: Leases, expiry, and backoff calculations use SQL `now()`.
3. **Fencing on every state change**: State transitions require matching `fencing_token` and `state = 'LEASED'`.
4. **Tenant isolation**: Every query validates `tenant_id` derived exclusively from authenticated API keys.
5. **Stable downstream idempotency**: Downstream idempotency key is `steadfast:<job_id>`; fencing token travels in a separate header.
6. **No silent drops**: Every accepted job terminates either `SUCCEEDED` or `DEAD` with audited reasons.
7. **Auditability**: Every state transition is recorded via database triggers.
8. **Independent verifier**: The auditor inspects only database and ledger storage; it never trusts API runtime telemetry.

---

## License
MIT License - see [LICENSE](LICENSE).
