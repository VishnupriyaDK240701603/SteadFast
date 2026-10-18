# Steadfast: Limitations and Non-Goals (Honest Claims)

Status: Living Document

## Honest Durability Claims
- An acknowledged job survives a hard kill of the Postgres container (`fsync=on`, `synchronous_commit=on`, `full_page_writes=on`, named Docker volume).
- Power loss / physical host crash durability is NOT claimed (Docker on Windows runs Linux inside a VM where `fsync` commits to the virtual disk).
- Tier B failover with zero acknowledged-job loss is NOT claimed (would require synchronous replication clusters).

## Out of Scope for Version 1
- Row-Level Security (RLS) in PostgreSQL.
- Payload encryption at rest.
- Internal mTLS within the Docker compose network.
- Automated API key rotation.
- Webhook subscriptions and outbox pattern (P2 cut line).
