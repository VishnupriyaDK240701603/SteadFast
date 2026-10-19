# Steadfast: Production & Operator Runbook

Status: Living Document (Prompt 12 Hardening Pass)

This runbook describes operational procedures, diagnostics, and recovery workflows for operators running Steadfast.

---

## 1. Health & Readiness Probes

The API exposes standard Kubernetes-compatible HTTP health probes:
- **Liveness Probe**: `GET /healthz`
  - Returns `200 OK` (`{"status":"ok"}`) as long as the process is alive.
- **Readiness Probe**: `GET /readyz`
  - Probes live PostgreSQL database connectivity (`pool.Ping()`).
  - Returns `200 OK` (`{"status":"ready"}`) if healthy.
  - Returns `503 Service Unavailable` with `Retry-After: 1` if the database is unreachable.

---

## 2. Circuit Breaker Triage (F12, P1)

### Symptom
- Dashboard shows breaker status **`OPEN`** for a specific job type (e.g., `charge_customer`).
- Jobs of that type remain queued and are skipped by worker claim queries (`internal/breaker`).

### Root Cause
- The downstream service (e.g., payment ledger) is returning retryable errors (5xx, timeouts, network partition) exceeding the 50% failure threshold over the evaluation window.

### Automated Recovery
- When the downstream dependency stabilizes, the breaker enters **`HALF_OPEN`** cooldown (default: 15s).
- A single canary probe job is dispatched.
- Upon successful execution of the canary job, the breaker automatically transitions back to **`CLOSED`** without operator intervention.

---

## 3. Dead Letter Queue (DLQ) Remediation (F6)

### Symptom
- Jobs transition to state **`DEAD`** after exhausting their maximum retry attempts (`DEFAULT_MAX_ATTEMPTS=5`).
- Dashboard DLQ badge indicates non-zero count.

### Step-by-Step Remediation:
1. **Inspect Dead Jobs**:
   ```bash
   curl -s -H "Authorization: Bearer <TENANT_KEY>" http://127.0.0.1:8080/v1/dlq | jq .
   ```
2. **Review Error History**:
   Examine `last_error`, `dead_reason`, and attempt records in the returned JSON.
3. **Correct Payload (if poisoned/malformed)**:
   ```bash
   curl -X PATCH -H "Authorization: Bearer <TENANT_KEY>" \
        -H "Content-Type: application/json" \
        -d '{"payload":{"order_id":"ord-123","amount_cents":5000}}' \
        http://127.0.0.1:8080/v1/dlq/<JOB_ID>
   ```
4. **Replay Job**:
   ```bash
   curl -X POST -H "Authorization: Bearer <TENANT_KEY>" \
        http://127.0.0.1:8080/v1/dlq/<JOB_ID>/replay
   ```
   The job immediately returns to `QUEUED` with reset attempt counter and is dispatched to available workers.

---

## 4. Database Outage & Failover Recovery

### Behavior During Outage
- If PostgreSQL stops or restarts, the API stops accepting submissions and returns `503 Service Unavailable` with `Retry-After: 1`.
- Worker nodes enter exponential backoff jitter sleep without crashing.
- In-flight jobs whose leases expire during the outage are recovered by the leader reaper as soon as the database is back online.

### Leader Reaper Session Pinning
- The reaper acquires PostgreSQL advisory lock `987654321`.
- The lock is pinned to a dedicated connection for the entire lifetime of the process.
- If the network connection drops or the process terminates, PostgreSQL automatically releases the advisory lock, permitting another instance to assume leadership immediately.

---

## 5. Rate Limiting Incidents (F13, P1)

### Symptom
- Client receives `429 Too Many Requests` with response header `Retry-After: <seconds>`.
- Error code: `rate_limit_exceeded`.

### Action
- The token-bucket rate limiter enforces `RATE_LIMIT_RPS` (default: 5000 RPS) per tenant API key hash in memory.
- If high-volume batch submissions legitimately require higher throughput, set environment variable `RATE_LIMIT_RPS=<value>` in `.env` and restart the API service.

---

## 6. Chaos Controller & Security Guardrails

> **CRITICAL SECURITY REQUIREMENT: Never expose the chaos controller in production.**

- Restricted to `STEADFAST_ENV=demo`.
- Binds exclusively to `127.0.0.1:8083`.
- Strict origin check (`localhost` / `127.0.0.1`).
- Only controls containers labeled `steadfast.chaos=allow`.
