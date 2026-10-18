# Steadfast: Architecture Overview

Status: Skeleton (v1 Setup)

## System Components
- **API (`cmd/api`)**: Ingestion service handling REST requests, API key authentication, and durable transaction commits.
- **Worker (`cmd/worker-example`, `sdk/worker`)**: Distributed workers claiming jobs under time-limited leases with monotonic fencing tokens.
- **Reaper (`cmd/reaper`)**: Background reconciliation process that reclaims expired leases and dead-letters jobs exceeding max attempts.
- **Mock Ledger (`cmd/ledger`)**: Downstream payment ledger validating idempotency key persistence and fault injection modes.
- **Chaos Controller (`cmd/chaos`)**: Process and container failure orchestrator active strictly under `STEADFAST_ENV=demo`.
- **Verifier (`cmd/verifier`)**: Independent auditor validating delivery invariants directly from storage without trusting API telemetry.
- **Dashboard (`cmd/dashboard`, `web/dashboard`)**: Static vanilla JS console streaming live system state via Server-Sent Events.

Detailed architecture diagrams and threat models will be elaborated in Prompt 02.
