# Steadfast: Operator Runbook

Status: Skeleton (v1 Setup)

## Common Commands
- Start Postgres: `go run ./cmd/ctl up`
- Stop containers: `go run ./cmd/ctl down`
- Clean all resources: `go run ./cmd/ctl clean`
- Run test suite: `go run ./cmd/ctl test`
- Run linters: `go run ./cmd/ctl lint`
- Run security scans: `go run ./cmd/ctl scan`

Troubleshooting steps and recovery workflows will be added as features are developed.

## Chaos Controller & Docker Socket Security (F9)

The chaos controller (`cmd/chaos`) orchestrates failure injection (container kills and restarts, ledger fault modes) to prove queue durability and fencing token correctness under active disruption.

### Docker Socket Risk & Mitigations
> **SECURITY NOTICE: Never deploy the chaos service in production environments.**

1. **Root Equivalent Privilege**: Access to `/var/run/docker.sock` allows controlling host Docker containers. Mount access is restricted strictly to local developer environments and demo profiles.
2. **Environment Guard**: The chaos service strictly refuses to start unless `STEADFAST_ENV=demo`.
3. **Localhost Binding**: Binds exclusively to `127.0.0.1:8083`.
4. **Authentication**: All state-changing endpoints require `Authorization: Bearer <ADMIN_KEY>`.
5. **Origin Validation**: State-changing endpoints reject requests from untrusted browser origins (enforcing `http://127.0.0.1:*` or `http://localhost:*`).
6. **Container Allow-List**: The chaos controller only targets containers labeled with `steadfast.chaos=allow` (or named `steadfast-postgres`, `steadfast-redis`, `steadfast-worker`, `steadfast-baseline-worker`). All unapproved targets are rejected with HTTP 400.
7. **No Shell Injection**: Commands are invoked using structured argument slices without shell interpolation.
