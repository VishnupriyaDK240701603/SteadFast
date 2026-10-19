# Steadfast: Offline Operation Verification (SPEC Appendix J5)

Status: Verified

## 1. Offline Guarantee
Steadfast is designed to run reliably in disconnected conference and judging environments with Wi-Fi disabled or blocked.

## 2. Evidence of Zero Remote Dependencies
1. **Vendored Web Dashboard**:
   - `web/dashboard/index.html` loads only local `/style.css` and `/app.js`.
   - Real-time throughput visualization uses native HTML5 `<canvas>` rendering without external charting libraries (e.g. no Chart.js, D3, or CDN scripts).
   - System typography uses native font stacks (`system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif`); zero remote Google Fonts calls.
2. **Local Inter-Service Networking**:
   - All REST APIs, SSE event streams, chaos commands, and database queries bind strictly to `127.0.0.1` and container internal networks.
   - Zero outbound DNS lookups or remote telemetry calls.
3. **Pre-built Container Images**:
   - All Docker images (`steadfast-api`, `steadfast-worker`, `steadfast-dashboard`, `steadfast-chaos`, `steadfast-ledger`, `steadfast-verifier`) are built and cached locally.
   - Running `go run ./cmd/ctl demo` or `ctl demo-short` succeeds identically with network interfaces disconnected.

## 3. Verification Command & Result
```bash
# Executed in sandbox with external route disabled:
go run ./cmd/ctl demo-short --headless
# Result: 4 Beats executed, DLQ replayed, verifier exit code 0 (PASS).
```
