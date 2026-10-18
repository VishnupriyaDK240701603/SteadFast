# Steadfast: Threat Model & Security Specification

Status: Approved Threat Model (Prompt 02)

---

## 1. Scope, Assets, and Attack Surfaces

This threat model establishes security controls for Steadfast running locally in Docker Desktop / WSL2 for hackathon demonstration. It explicitly adheres to `.agents/rules/10-security.md`.

### Key Assets
1. **Tenant API Keys**: High-entropy bearer credentials granting access to submit jobs and manage tenant DLQs.
2. **Server-Side Admin Key**: Administrative secret used by the dashboard and chaos services to trigger system-level mutations.
3. **Job Payloads and Business Data**: Sensitive JSON inputs and execution results stored in PostgreSQL.
4. **Downstream Ledger State**: SQLite records representing financial charge transactions.
5. **Host Environment**: Windows/WSL2 developer laptop running Docker engine.

---

## 2. Threat Analysis Matrix

| Asset / Entry Point | Threat Vector | Mitigation Strategy | Residual Risk |
|---|---|---|---|
| **Tenant API Keys** | Key extraction via logs, URLs, error bodies, or database compromise | Keys are 32 random bytes (base64url); stored exclusively as SHA-256 hashes; authentication queries lookup by hash; raw keys are never written to logs, query parameters, payloads, or UI bundle. | Compromise of the local `.env` file on the developer's workstation disk. |
| **Tenant Isolation** | Tenant B attempts cross-tenant access to Tenant A's jobs, attempts, or DLQ | Tenant ID is derived solely from the authenticated API key hash, never from user payload. Store repository requires `tenant_id` on all queries. Cross-tenant access returns generic `404 Not Found` (never 403). Mandatory automated cross-tenant tests. | Developer bypassing repository layer with custom raw SQL (prevented by strict package encapsulation). |
| **Ingestion Endpoint** | JSON payload bomb, memory exhaustion, SQL injection | Strict JSON decoding with `DisallowUnknownFields`; 64 KiB payload limit; 1 MiB HTTP body limit; regex validation on job `type` (`^[a-z0-9_.-]{1,64}$`); parameterized queries (`pgx` `$1, $2`) exclusively. | Storage saturation under massive valid traffic (mitigated by token bucket rate limiter). |
| **Zombie / Delayed Worker** | Desynchronized worker waking up after lease expiry and overwriting newer state | Monotonic `fencing_token` checked on every heartbeat, completion, and failure query (`WHERE fencing_token = $1 AND state = 'LEASED'`). Zero rows updated returns `409 Conflict`, prompting the worker to discard the result. | Worker executing un-fenced external side effects on non-idempotent third-party APIs (mitigated by `steadfast:<job_id>` key). |
| **Downstream Double Charges** | Network failure between worker and downstream ledger after charge commit | Worker transmits stable `Idempotency-Key: steadfast:<job_id>`. Downstream ledger enforces SQLite `UNIQUE(idempotency_key)` to deduplicate repeated attempts and return cached charge status. | External downstream payment gateway that does not support idempotency keys (stated as prerequisite in docs). |
| **Docker Socket & Chaos Service** | Host root privilege escalation via mounted `/var/run/docker.sock` | Service refuses to execute unless `STEADFAST_ENV=demo`; binds exclusively to `127.0.0.1`; requires server-side admin key; strict container name allowlist (`steadfast-worker-*`, `steadfast-postgres`). README explicitly warns never deploy to production. | Host administrator abusing chaos tool locally during demo. |
| **Verifier Integrity** | System components falsifying audit data or grading their own telemetry | Verifier never calls Steadfast runtime APIs; reads only immutable loadgen ack files, SQLite ledger export, and PostgreSQL read-only replica role. Tested via mutation test (`STEADFAST_DISABLE_FENCING=true` must fail verifier). | Local host file tampering of ack files (mitigated by loadgen hash chain). |
| **Dashboard Console** | Cross-Site Request Forgery (CSRF) and unauthorized command execution | State-changing endpoints validate `Origin: http://127.0.0.1:8080`; admin credentials held strictly server-side; browser receives zero credentials. | Malicious browser extension injected into operator's browser session. |
| **Supply Chain & Dependencies** | Malicious or vulnerable Go dependencies | Pinned Go module dependencies with `go.sum`; `govulncheck` and `gitleaks` run in CI and in `ctl scan`; strictly zero unapproved external libraries. | Zero-day vulnerability in vetted standard dependencies. |

---

## 3. Explicit Non-Goals for Version 1
As mandated by `10-security.md`, the following are roadmap items and are explicitly **not claimed**:
- Row-Level Security (RLS) policies in PostgreSQL.
- Payload encryption at rest.
- Internal mTLS within the private compose network.
- Automated API key rotation workflows.
- Full accessibility compliance audit.
