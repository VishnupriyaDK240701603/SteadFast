# Steadfast: Demo Playbook & Operator Guide

This playbook describes how to demonstrate Steadfast's fault-tolerant architecture, circuit breakers, and independent verifier against a standard Redis Celery-style baseline.

---

## 1. Quick Demo Execution

You can run the full automated four-beat demonstration using the CLI:

```bash
# Short profile (~90 seconds, recommended for quick judging walkthroughs)
go run ./cmd/ctl demo-short

# Full profile (~4 minutes, comprehensive side-by-side run)
go run ./cmd/ctl demo
```

To watch the live interactive visualizer during the run:
- Open your browser to: **`http://127.0.0.1:8085`**
- The dashboard streams real-time SSE events, live throughput graphs, stakes counters ($ lost / double charged), DLQ drawer, and circuit breaker status.

---

## 2. Four-Beat Demo Choreography (Appendix D)

### Beat 1: Baseline & Steady Load
- **Action**: Load generator submits parallel payment charges to both Steadfast (Postgres fenced) and the Baseline (Redis visibility timeout).
- **Observation**:
  - Both runners sustain steady throughput.
  - Stakes counter displays $0.00 lost and $0.00 double-charged.

### Beat 2: Worker Hard Crash (Zombie Scenario)
- **Action**: Chaos controller sends `SIGKILL` to a worker while processing an active batch of payment jobs.
- **Observation**:
  - In Steadfast, the expired lease is recovered by the leader reaper (`fencing_token = N + 1`). When the zombie attempt attempts completion, the fenced write is rejected. Zero duplicate charges occur ($0.00 lost, $0.00 double charged).
  - In the Baseline runner, Redis visibility timeout re-dispatches the job while the first worker is still writing to the ledger, causing duplicate execution and double-charging customer accounts ($1,250.00+ double charged).

### Beat 3: Downstream Fault & Poison Pill DLQ Triage
- **Action**: Chaos controller injects a 100% error rate on the downstream payment ledger, followed by a poison-pill payload with invalid syntax.
- **Observation**:
  - The Steadfast circuit breaker (`internal/breaker`) trips from `CLOSED` to `OPEN`, pausing dispatch for `charge_customer` jobs and preventing a retry storm against the failing ledger.
  - The poison payload exhausts its attempt limit and safely transitions to the Dead Letter Queue (`DEAD`).
  - Open the **DLQ Drawer** in the dashboard (`GET /v1/dlq`), review the error history, edit the JSON payload to fix the invalid field (`PATCH /v1/dlq/{id}`), and click **Replay** (`POST /v1/dlq/{id}/replay`). The job transitions to `QUEUED` and completes as `SUCCEEDED`.

### Beat 4: Independent Verifier Audit & Verdict
- **Action**: The independent verifier (`cmd/verifier`) runs against the database and ledger files without consulting runtime API counters.
- **Observation**:
  - Evaluates Invariants I1 (Zero lost acknowledged jobs), I2 (Zero duplicate side effects), I3 (Payload integrity), and I4 (Fencing token monotonicity).
  - Steadfast reports **VERDICT: PASS (0 lost jobs, 0 duplicate charges)**.
  - Baseline reports **VERDICT: FAIL (lost jobs under crash, multiple duplicate charges)**.

---

## 3. Pitch Timing & Beat Timestamps

Measured reference timings on the test laptop (8 vCPUs, 8 GB RAM):

### Short Profile (`short`, Target: ~90s)
| Beat | Action | Start Time | End Time | Duration |
|---|---|---|---|---|
| **Beat 1** | Steady Load Submission (600 jobs @ 300 RPS) | `0:00` | `0:15` | 15s |
| **Beat 2** | Worker SIGKILL & Database Restart (>= 500 in-flight) | `0:15` | `0:35` | 20s |
| **Beat 3** | Downstream Fault Injection & DLQ Poison Replay | `0:35` | `0:55` | 20s |
| **Beat 4** | Queue Drain & Independent Verifier Audit | `0:55` | `1:22` | 27s |
| **Total** | Complete Short Demo Walkthrough | `0:00` | `1:22` | **~82 seconds** |

### Full Profile (`full`, Target: ~4 min)
| Beat | Action | Start Time | End Time | Duration |
|---|---|---|---|---|
| **Beat 1** | Extended Steady Load (1,200 jobs @ 400 RPS) | `0:00` | `0:45` | 45s |
| **Beat 2** | Extended Worker Crash & Container Partition | `0:45` | `1:45` | 60s |
| **Beat 3** | Sustained Ledger Outage, Breaker Trip & DLQ Replay | `1:45` | `2:45` | 60s |
| **Beat 4** | Comprehensive Comparative Verifier Audit | `2:45` | `3:50` | 65s |
| **Total** | Complete Comparative Demo Walkthrough | `0:00` | `3:50` | **~230 seconds (3m 50s)** |

---

## 4. Backup Video Recording Checklist (< 3 Minutes)

Follow this checklist when recording the offline submission video:
1. **Host Setup**:
   - Pause Windows Update; set power mode to "Best Performance".
   - Set screen resolution to 1080p (1920x1080) at 125% zoom.
   - Close all background notifications and messaging applications.
2. **Layout**:
   - Left Half (50% screen): Browser displaying Dashboard at `http://127.0.0.1:8085`.
   - Right Half (50% screen): Clean terminal window.
3. **Execution**:
   - Start OBS Studio screen recording (1080p, 60 FPS, H.264).
   - In terminal, run: `go run ./cmd/ctl demo-short`
   - Point out Beat 1 steady throughput ($0.00 stakes).
   - Point out Beat 2 worker crash: show Baseline double-charging while Steadfast maintains $0.00 loss.
   - Point out Beat 3 circuit breaker trip and open DLQ drawer to show poison job.
   - Point out Beat 4 verifier output: "ALL INVARIANTS PASS".
   - Stop recording. Verify duration is **under 2 minutes 30 seconds**.
   - Save master recording to `docs/evidence/demo-backup-video.mp4`.

---

## 5. Manual Operator Controls

During a live demo, the operator can manually trigger chaos actions:
- Kill active worker: `curl -X POST http://127.0.0.1:8083/kill-worker -H "Authorization: Bearer $ADMIN_KEY"`
- Restart database: `curl -X POST http://127.0.0.1:8083/restart-db -H "Authorization: Bearer $ADMIN_KEY"`
- Inject ledger error: `curl -X POST http://127.0.0.1:8083/inject-fault -H "Authorization: Bearer $ADMIN_KEY" -d '{"error_rate":1.0}'`
- Clear ledger fault: `curl -X POST http://127.0.0.1:8083/clear-fault -H "Authorization: Bearer $ADMIN_KEY"`
