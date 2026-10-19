# Steadfast: System Architecture & Technical Specification

Status: Approved Architecture Plan (Prompt 02)

---

## 1. Executive Summary & Design Principles

Steadfast is a reliable, self-hostable asynchronous job runner whose primary design objective is **defensible, provable reliability under chaos**. 

### The Engine Laws
1. **Acknowledge after commit**: `POST /v1/jobs` returns 202 only after the database transaction commits to disk (`fsync=on`, `synchronous_commit=on`).
2. **Database time is the only clock**: Leases, expiry, `run_at`, and backoff calculations use PostgreSQL `now()`.
3. **Fencing on every state change**: State transitions from workers require `state = 'LEASED'` and matching monotonic `fencing_token`.
4. **Tenant isolation at data layer**: Every query requires `tenant_id` derived exclusively from authenticated API key hash.
5. **Stable downstream idempotency**: Downstream idempotency key is `steadfast:<job_id>`; fencing token travels in a separate header.
6. **No silent drops**: Every accepted job terminates either `SUCCEEDED` or `DEAD` with audited failure reasons.
7. **Every state transition is audited**: Recorded via database triggers; operator actions record the actor.
8. **The verifier never grades itself**: Reads only immutable loadgen ack files, SQLite ledger export, and PostgreSQL read-only replica role.

---

## 2. Component Architecture

```mermaid
graph TD
    subgraph Clients & Ingestion
        LG[Load Generator<br/>cmd/loadgen] -->|POST /v1/jobs<br/>Bearer Key, Idempotency-Key| API[Steadfast API<br/>cmd/api]
        AckFile[(Acks File<br/>artifacts/loadgen-acks.jsonl)]
        LG -.->|Write Acks| AckFile
    end

    subgraph Steadfast Core [Steadfast Core Platform]
        API -->|Tx Commit| PG[(PostgreSQL<br/>fsync=on, 1.5 GB limit)]
        RP[Reaper Daemon<br/>cmd/reaper] -->|Reclaim Leases<br/>attempts >= max -> DEAD| PG
        W1[Worker 1<br/>cmd/worker-example] -->|POST /v1/worker/claim<br/>SELECT FOR UPDATE SKIP LOCKED| API
        W2[Worker 2] -->|POST /v1/worker/claim| API
        W3[Worker 3] -->|POST /v1/worker/claim| API
        W4[Worker 4] -->|POST /v1/worker/claim| API
    end

    subgraph Downstream System
        ML[Mock Ledger<br/>cmd/ledger]
        MLDB[(SQLite DB<br/>modernc.org/sqlite)]
        ML --- MLDB
        W1 -->|POST /charges<br/>Key: steadfast:job_id<br/>X-Fencing-Token: token| ML
        W2 -->|POST /charges| ML
        W3 -->|POST /charges| ML
        W4 -->|POST /charges| ML
    end

    subgraph Baseline Comparison [Baseline Comparison Lane]
        BAPI[Baseline API<br/>cmd/baseline-api] -->|Push| RD[(Redis 7<br/>AOF everysec)]
        BW[Baseline Workers<br/>cmd/baseline-worker] -->|Pop / Visibility Timeout| RD
        BW -->|POST /charges<br/>No Idempotency Key| ML
    end

    subgraph Operator & Chaos [Operator & Demo Tools]
        DASH[Dashboard<br/>cmd/dashboard] -->|SSE / Status| UI[Static Console<br/>web/dashboard]
        CH[Chaos Daemon<br/>cmd/chaos] -->|Docker Socket Kill| W1
        CH -->|Docker Socket Kill| PG
        CH -->|Fault Injection| ML
    end

    subgraph Independent Audit [Independent Verifier]
        V[Verifier<br/>cmd/verifier]
        V -->|1. Read Acks| AckFile
        V -->|2. Audit Ledger Charges| MLDB
        V -->|3. Read-Only Query| PG
        V -->|Output Verdict| VF[(verdict.json)]
    end
```

---

## 3. Sequence Choreography

### 3.1 Durable & Idempotent Submission (F1, F2)
```mermaid
sequenceDiagram
    autonumber
    Client->>+API: POST /v1/jobs (Header: Idempotency-Key: K, Payload: P)
    API->>API: Hash API Key (SHA-256) -> Resolve tenant_id
    API->>Postgres: BEGIN TRANSACTION
    API->>Postgres: SELECT job_id, request_hash, response_status, response_body FROM idempotency_keys WHERE tenant_id=$1 AND key=$2 FOR UPDATE
    alt Idempotency Key Exists (Committed)
        API->>Postgres: ROLLBACK
        alt Payload Hash Matches
            API-->>Client: 202 Accepted (Cached response_body with existing job_id)
        else Payload Hash Differs
            API-->>Client: 409 Conflict ("idempotency key reused with different payload")
        end
    else In-Flight Race / New Key
        API->>Postgres: INSERT INTO jobs (tenant_id, type, payload, state='QUEUED', run_at=now()) RETURNING id
        API->>Postgres: INSERT INTO idempotency_keys (tenant_id, key, job_id, request_hash, response_status, response_body) VALUES (...)
        API->>Postgres: COMMIT (synchronous_commit=on, fsync=on)
        API-->>-Client: 202 Accepted {"job_id": "...", "state": "QUEUED"}
    end
```

### 3.2 Lease Claim, Heartbeat, Downstream Effect, and Fencing Completion (F3, F7, F8)
```mermaid
sequenceDiagram
    autonumber
    Worker->>+API: POST /v1/worker/claim (Bearer <tenant_key>, worker_id)
    API->>Postgres: UPDATE jobs SET state='LEASED', fencing_token=fencing_token+1, leased_by=$worker, lease_expires_at=now()+interval '10s' WHERE id = (SELECT id FROM jobs WHERE tenant_id=$1 AND state='QUEUED' AND run_at<=now() ORDER BY run_at ASC, id ASC FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING *
    API-->>-Worker: 200 OK (job_id, payload, fencing_token=1)

    par Background Heartbeat Loop (every 3s)
        Worker->>API: POST /v1/worker/jobs/:id/heartbeat {"fencing_token": 1}
        API->>Postgres: UPDATE jobs SET lease_expires_at=now()+interval '10s' WHERE id=$1 AND tenant_id=$2 AND fencing_token=1 AND state='LEASED'
        API-->>Worker: 200 OK
    and Downstream Side Effect (Mock Ledger)
        Worker->>+Ledger: POST /charges (Header: Idempotency-Key: steadfast:<job_id>, X-Fencing-Token: 1)
        Note over Ledger: SQLite: INSERT OR IGNORE INTO charges (idempotency_key=steadfast:<job_id>)
        Ledger-->>-Worker: 200 OK {"charge_id": 101, "status": "PAID"}
    end

    Worker->>+API: POST /v1/worker/jobs/:id/complete {"fencing_token": 1, "result": {...}}
    API->>Postgres: UPDATE jobs SET state='SUCCEEDED', completed_at=now(), result=$result WHERE id=$1 AND tenant_id=$2 AND fencing_token=1 AND state='LEASED'
    Postgres-->>API: 1 row affected
    API-->>-Worker: 200 OK
```

### 3.3 Zombie Worker Rejection (Invariant I3, F3)
```mermaid
sequenceDiagram
    autonumber
    Worker A->>API: Claim -> gets job_id=X, fencing_token=1
    Note over Worker A: Worker A stalls / GC pause / network partition
    Note over Reaper: Reaper detects lease_expires_at < now()
    Reaper->>Postgres: UPDATE jobs SET state='QUEUED', run_at=now(), attempts=attempts+1 WHERE id=X AND state='LEASED'
    Worker B->>API: Claim -> gets job_id=X, fencing_token=2
    Worker B->>Ledger: POST /charges (Idempotency-Key: steadfast:X, X-Fencing-Token: 2)
    Ledger-->>Worker B: 200 OK (charge committed)
    Worker B->>API: Complete (fencing_token=2) -> 200 OK (job_id=X is SUCCEEDED)
    Note over Worker A: Worker A wakes up and attempts late actions
    Worker A->>+API: POST /v1/worker/jobs/X/complete {"fencing_token": 1}
    API->>Postgres: UPDATE jobs SET state='SUCCEEDED' WHERE id=X AND tenant_id=$tenant AND fencing_token=1 AND state='LEASED'
    Postgres-->>API: 0 rows affected (fencing_token mismatch & state != 'LEASED')
    API-->>-Worker A: 409 Conflict ("stale fencing token or expired lease")
    Note over Worker A: Worker A discards result; no state corrupted
```

### 3.4 Crash Recovery: Hard Kill of PostgreSQL Container (F1, A1.1)
```mermaid
sequenceDiagram
    autonumber
    Client->>API: POST /v1/jobs -> 202 Accepted (Tx committed to disk)
    Note over Chaos: Chaos issues: docker kill -s 9 steadfast-postgres
    Note over API: Ongoing requests fail with 503 Service Unavailable (Retry-After: 1)
    Note over Worker: Workers back off with exponential jitter
    Note over Docker: Container restarted with named volume steadfast_postgres_data
    Note over Postgres: WAL replay verifies fsync=on, synchronous_commit=on, full_page_writes=on
    Postgres-->>Postgres: Database recovers to clean state
    API->>Postgres: /readyz healthcheck succeeds
    Worker->>API: Claim / Heartbeat / Complete resumes
    Client->>API: GET /v1/jobs/:id -> returns job (Zero acknowledged jobs lost)
```

### 3.5 DLQ Inspect, Edit, and Replay (F6)
```mermaid
sequenceDiagram
    autonumber
    Note over Reaper: Job Y exceeds max_attempts (5) -> state='DEAD', dead_lettered_at=now()
    Operator->>+API: GET /v1/dlq/:id (Admin key)
    API-->>-Operator: 200 OK (payload, last_error, attempts)
    Operator->>+API: PATCH /v1/dlq/:id {"payload": {"fixed_field": "valid_value"}}
    API->>Postgres: UPDATE jobs SET payload=$new_payload WHERE id=$1 AND tenant_id=$2 AND state='DEAD'
    API->>Postgres: INSERT INTO audit_log (job_id, actor='operator', action='dlq_edit')
    API-->>-Operator: 200 OK
    Operator->>+API: POST /v1/dlq/:id/replay
    API->>Postgres: UPDATE jobs SET state='QUEUED', run_at=now(), attempts=0, last_error=NULL WHERE id=$1 AND tenant_id=$2 AND state='DEAD'
    API->>Postgres: INSERT INTO audit_log (job_id, actor='operator', action='dlq_replay')
    API-->>-Operator: 200 OK (Job re-enqueued for workers)
```

---

## 4. Exact Fencing vs. Idempotency Guarantees (Guaranteed Non-Duplication via Receiver Idempotency)

Steadfast explicitly rejects universal duplicate prevention across unreliable networks without receiver cooperation. Instead, it provides two distinct, orthogonal guarantees:

1. **Downstream Side-Effect Protection (Idempotency Key)**:
   - Worker transmits `Idempotency-Key: steadfast:<job_id>` to downstream services.
   - Downstream services (such as `cmd/ledger`) enforce SQLite `UNIQUE(idempotency_key)` constraints.
   - If network partitions or worker crashes cause repeated calls, the downstream deduplicates the call and returns the cached result without duplicate execution (e.g., zero duplicate charges).
2. **Steadfast Job State Protection (Fencing Token)**:
   - The database increments `fencing_token` on every lease claim.
   - Updates (`heartbeat`, `complete`, `fail`) enforce:
     ```sql
     WHERE id = $id AND tenant_id = $tenant_id AND fencing_token = $fencing_token AND state = 'LEASED'
     ```
   - This ensures a late, partitioned "zombie" worker cannot overwrite progress made by a subsequent worker.
3. **Downstream Token Handling**:
   - The ledger records `X-Fencing-Token` with the charge for auditing. If an incoming attempt presents a stale token for an existing charge, the unique idempotency key already shields the customer from a second charge, and the ledger audits the stale attempt.

---

## 5. Verifier Audit Logic & Drain Specification

The independent verifier operates under strict boundaries:
- **No Self-Grading**: Does not call Steadfast REST APIs, does not read dashboard memory or SSE feeds.
- **Data Sources**: (1) `artifacts/loadgen-acks.jsonl`, (2) SQLite `charges` table, (3) PostgreSQL read-only replica connection.

### 5.1 Nonterminal Drain Condition
To prevent active in-flight jobs from being incorrectly flagged as lost, verification requires a nonterminal drain:
```sql
SELECT COUNT(*) FROM jobs 
WHERE state IN ('QUEUED', 'LEASED') 
   OR run_at > now();
```
- If `nonterminal_count > 0` after the test harness timeout (default 60s), the verifier exits with:
  `STATUS: INCOMPLETE (DRAIN_TIMEOUT)` and outputs unresolved job IDs and states. It does **not** declare an Invariant I1 failure.
- When `nonterminal_count == 0`, every acknowledged job has reached a terminal state (`SUCCEEDED` or `DEAD`), and invariant verification commences.

### 5.2 Invariants Evaluated

$$\text{I1: Zero Lost Jobs} \iff \text{Acked Jobs} - (\text{Succeeded Jobs} + \text{Dead Jobs}) == 0$$

$$\text{I2: Effectively-Once Downstream} \iff \text{Duplicate Ledger Charges} == 0$$

$$\text{I2b: Set-Based Payment Correlation} \iff \forall \text{job} \in \text{Jobs}(\text{type='charge\_customer'}, \text{state='SUCCEEDED'}), \exists! \, \text{charge} \in \text{Charges}(\text{status='PAID'}) \text{ where charge.idempotency\_key} = \text{'steadfast:'} \parallel \text{job.id}$$

- **Set-Based Correlation Rule**: Rather than comparing aggregate numbers (`COUNT == COUNT`), the verifier performs a symmetric difference join on `(tenant_id, job_id)`. This prevents an extra charge on one job from masking a missing charge on another.
