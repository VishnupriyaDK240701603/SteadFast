# Steadfast: Limitations and Non-Goals (Honest Claims)

Status: Living Document

## Honest Durability Claims
- An acknowledged job survives a hard kill of the Postgres container (`fsync=on`, `synchronous_commit=on`, `full_page_writes=on`, named Docker volume).
- Power loss / physical host crash durability is NOT claimed (Docker on Windows runs Linux inside a VM where `fsync` commits to the virtual disk).
- Tier B failover with zero acknowledged-job loss is NOT claimed (would require synchronous replication clusters).

## Graceful Shutdown & Drain Bounds (F13, P1)
- API graceful shutdown drains active HTTP submissions and queries for up to 25 seconds on SIGTERM before terminating listeners.
- Worker SDK graceful shutdown stops new claim polling immediately upon SIGTERM and allows active in-flight jobs up to 25 seconds (`DrainTimeout`) to complete and report.
- In-flight jobs exceeding the 25-second drain window are abandoned to PostgreSQL lease expiration and recovered by the reaper; no duplicate execution or hung leases occur.

## Rate Limiting Semantics (F13, P1)
- The token-bucket rate limiter (`internal/ratelimit`) is an in-process, memory-only implementation keyed by the SHA-256 hash of the tenant API key.
- Limiter state resets on process restart.
- Per-process scoping: in multi-instance horizontal deployments, each API instance maintains its own local bucket unless backed by a centralized coordinator.
- Default limit is 5000 RPS (`RATE_LIMIT_RPS=5000`), well above demo and normal production burst rates.

## Out of Scope for Version 1
- Row-Level Security (RLS) in PostgreSQL.
- Payload encryption at rest.
- Internal mTLS within the Docker compose network.
- Automated API key rotation.
- Webhook subscriptions and outbox pattern (P2 cut line).
