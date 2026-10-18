# Baseline Queue Configuration and Fairness Specification

## 1. Overview
The Steadfast benchmark compares Steadfast against a standard, widely deployed asynchronous queue architecture: a Redis-backed queue with visibility timeouts and retry queues.

This document describes the exact configuration of the baseline queue, the rationale for each setting, and why this represents a common and fair production baseline without implying any product defects.

---

## 2. Redis Configuration

The baseline queue runs in a dedicated container with the following explicit configuration:

| Setting | Value | Rationale |
|---|---|---|
| **Image** | `redis:7.4-alpine` | Official stable Redis image on Alpine Linux. |
| **Append-Only File (AOF)** | `appendonly yes` | Standard persistence mechanism ensuring queue items are written to an append-only log on disk. |
| **AOF Fsync Frequency** | `appendfsync everysec` | The industry-standard default balancing disk I/O performance and durability. Redis flushes OS write buffers to disk once per second. |
| **Memory Eviction Policy** | `maxmemory-policy noeviction` | Prevents Redis from silently evicting jobs under memory pressure; returns an out-of-memory error instead of dropping queue items. |
| **Volume Mount** | `redis_data:/data` | Named persistent volume to survive normal container restarts. |

---

## 3. Queue Pattern: Visibility Timeout (`BASELINE_PATTERN=visibility_timeout`)

The baseline implements the reliable queue pattern common in Redis job processing frameworks:

1. **Enqueue**:
   - `baseline-api` receives `POST /baseline/jobs` and executes `LPUSH baseline:queue <job_json>`.
   - Returns `202 Accepted` immediately upon Redis acknowledging the list push.
2. **Claim & Visibility**:
   - `baseline-worker` atomically moves a job from `baseline:queue` to a processing queue `baseline:processing` with an associated lease timestamp.
   - Visibility timeout default: **10 seconds** (`BASELINE_VISIBILITY_SECONDS=10`).
   - A supervisor loop monitors `baseline:processing`: if a worker does not acknowledge completion before the visibility timeout expires, the job is re-queued to `baseline:queue` for another worker to process.
3. **Downstream Execution**:
   - By default (`BASELINE_SEND_IDEMPOTENCY_KEY=false`), baseline workers execute `POST /charge` against the downstream payment ledger **without an idempotency key**.
   - This reflects standard architectures where downstream APIs lack client-managed idempotency or where workers omit deduplication headers.
4. **Retries & Exhaustion**:
   - If downstream execution fails (e.g. 5xx or network timeout), the worker increments the attempt counter and re-queues the job with backoff.
   - When attempts reach `DEFAULT_MAX_ATTEMPTS` (5), the job is moved to `baseline:failed` (a dead-letter list).
   - This ensures exhausted jobs are counted as **failed**, rather than lost.

---

## 4. Alternate Modes

- **`BASELINE_PATTERN=ack_on_pull`**:
  An alternative pop-and-process pattern where jobs are removed from the queue immediately upon dequeue. Under a worker crash, in-flight jobs are permanently dropped.
- **`BASELINE_SEND_IDEMPOTENCY_KEY=true`**:
  An optional third lane demonstrating baseline operation with downstream idempotency keys enabled. While this prevents duplicate downstream charges, acknowledged jobs are still lost during a hard container crash within the 1-second AOF fsync window.

---

## 5. Architectural Durability Comparison & Fairness Guarantee

### Workload Equivalence
- Both lanes process identical order payloads, arrival rates, randomized amounts ($5.00 to $500.00), and client retry bounds (up to 2 retries on submission failure).
- Both lanes are evaluated by the same independent verifier using the same Mock Ledger export.

### The Durability Contract
- **Steadfast**:
  Enforces single-node Tier A durability. The API returns `202 Accepted` only after PostgreSQL commits the transaction to write-ahead log on disk (`fsync=on`, `synchronous_commit=on`, `full_page_writes=on`). An acknowledged job survives container termination, worker kills, and hard kills (`SIGKILL`) of PostgreSQL.
- **Baseline Queue**:
  Redis `appendfsync everysec` buffers writes in memory and flushes to disk once per second. If the Redis container or host is abruptly killed (`docker kill`), writes acknowledged within the preceding 1-second window can be lost.
- **Fairness Note**:
  The baseline is not misconfigured; `appendfsync everysec` is the recommended production default for Redis queues because synchronous `fsync always` severely limits throughput. The comparison demonstrates the fundamental architectural trade-off between transactional database queues and in-memory log queues under chaos.

> **Scope Boundary Note:** Durability claims in this benchmark are strictly bounded to the tested application-crash, worker-kill, and single-node PostgreSQL-container-hard-kill (SIGKILL) boundaries. They do not claim survival across untracked bare-metal storage hardware failures or untracked catastrophic infrastructure loss.
