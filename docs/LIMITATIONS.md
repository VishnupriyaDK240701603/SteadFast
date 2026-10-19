# Steadfast: Limitations and Non-Goals (Honest Claims)

Status: Living Document (Prompt 12 Hardening Pass)

## 1. Non-Idempotent Handlers & Downstream Side Effects
- Steadfast guarantees at-least-once execution dispatch with monotonic fencing tokens.
- **Downstream side-effect safety requires receiver idempotency**: If an external API or service does not honour idempotency keys, duplicate network attempts following worker crashes or network timeouts can cause duplicate downstream actions.
- Steadfast transmits `Idempotency-Key: steadfast:<job_id>` and `X-Fencing-Token: <token>` to protect idempotent receivers (such as the ledger service). Non-idempotent handlers can still duplicate external effects.

## 2. Single PostgreSQL Ceiling
- The architectural ceiling of Steadfast v1 is bounded by the write and fsync capacity of a single PostgreSQL primary instance.
- On the benchmark reference machine (8 vCPUs, 8 GB RAM), measured sustained throughput is **84.0 jobs/sec** with durable `fsync=on` commits, and p50 submission latency is **6.65 ms** (source: `results/results.json`).
- Horizontal scaling across multiple primaries or database sharding is out of scope for v1.

## 3. High Availability & Failover Boundaries
- **No zero-loss failover claimed**: Steadfast v1 operates against a single primary database. Surviving physical host loss without job loss requires synchronous replication clusters (e.g. Patroni / synchronous standby), which is not claimed or configured in v1.
- **Docker Desktop VM Durability (SPEC Appendix J4)**: Under Docker Desktop on Windows 10/11 or macOS, PostgreSQL `fsync=on` commits to the virtual disk inside the WSL2 Linux utility VM. An acknowledged job survives container hard kills (`docker kill steadfast-postgres`). Host OS power loss or physical host hardware failure durability requires bare-metal or cloud block storage.

## 4. Unshipped Roadmap Items (P2 Scope)
The following features were classified as P2 stretch goals and are explicitly **NOT shipped** in this version:
- OpenTelemetry tracing export to Jaeger.
- Per-tenant fair-share round-robin queue scheduling (Invariant I5).
- Python Worker SDK (only the Go Worker SDK is shipped).
- Outbox pattern and transactional webhooks.

## 5. Graceful Shutdown & Drain Bounds (F13, P1)
- API graceful shutdown drains active HTTP submissions for up to 25 seconds on SIGTERM before terminating listeners.
- Worker SDK graceful shutdown stops new claim polling immediately upon SIGTERM and allows active in-flight jobs up to 25 seconds (`DrainTimeout`) to complete and report.
- In-flight jobs exceeding the 25-second drain window are abandoned to PostgreSQL lease expiration and safely recovered by the reaper; no duplicate execution or hung leases occur.

## 6. Rate Limiting Semantics (F13, P1)
- The token-bucket rate limiter (`internal/ratelimit`) is an in-process, memory-only implementation keyed by the SHA-256 hash of the tenant API key.
- Limiter state resets on process restart.
- Per-process scoping: in multi-instance horizontal deployments, each API instance maintains its own local bucket unless backed by a centralized coordinator.
- Default limit is 5000 RPS (`RATE_LIMIT_RPS=5000`), well above demo and normal production burst rates.

## 7. Out of Scope for Version 1
- Row-Level Security (RLS) in PostgreSQL.
- Payload encryption at rest.
- Internal mTLS within the Docker compose network.
- Automated API key rotation.
