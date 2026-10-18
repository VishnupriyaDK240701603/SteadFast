package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"steadfast/sdk/worker"

	// "github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// A7.1: submit 100 charge_customer jobs through the API with 4 example workers; all reach SUCCEEDED; the ledger shows exactly 100 applied entries.
// Also verifies: killing/closing the Steadfast Postgres connection does not stop or lose the ledger (ledger export works independently).
func TestA7_1_Submit100ChargeCustomerJobs(t *testing.T) {
	router, s, tenant, validKey, _ := setupTestAPI(t)

	apiServer := httptest.NewServer(router)
	defer apiServer.Close()

	// 1. Setup Mock Ledger SQLite database
	tempDir, err := os.MkdirTemp("", "ledger-a71-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	dbPath := filepath.Join(tempDir, "ledger.db")
	ledgerDB, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer func() { _ = ledgerDB.Close() }()

	_, err = ledgerDB.Exec(`
	CREATE TABLE IF NOT EXISTS charges (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		order_id TEXT NOT NULL,
		customer_id TEXT NOT NULL,
		amount_cents INTEGER NOT NULL,
		idempotency_key TEXT,
		job_id TEXT,
		fencing_token INTEGER,
		status TEXT NOT NULL,
		response_body TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_charges_idempotency_key ON charges(idempotency_key);

	CREATE TABLE IF NOT EXISTS suppressed_entries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		order_id TEXT NOT NULL,
		customer_id TEXT NOT NULL,
		amount_cents INTEGER NOT NULL,
		idempotency_key TEXT NOT NULL,
		job_id TEXT,
		fencing_token INTEGER,
		reason TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	`)
	if err != nil {
		t.Fatalf("failed to init ledger schema: %v", err)
	}

	var ledgerMu sync.Mutex
	ledgerMux := http.NewServeMux()

	ledgerMux.HandleFunc("POST /charge", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			OrderID     string `json:"order_id"`
			CustomerID  string `json:"customer_id"`
			AmountCents int64  `json:"amount_cents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		key := r.Header.Get("Idempotency-Key")
		fencingTokenStr := r.Header.Get("X-Fencing-Token")
		var fencingToken int64
		if fencingTokenStr != "" {
			_, _ = fmt.Sscanf(fencingTokenStr, "%d", &fencingToken)
		}
		var jobID string
		if strings.HasPrefix(key, "steadfast:") {
			jobID = strings.TrimPrefix(key, "steadfast:")
		}

		ledgerMu.Lock()
		defer ledgerMu.Unlock()

		respJSON := fmt.Sprintf(`{"status":"applied","order_id":"%s","amount_cents":%d}`, req.OrderID, req.AmountCents)

		if key != "" {
			var existingResp string
			err := ledgerDB.QueryRow("SELECT response_body FROM charges WHERE idempotency_key = ? AND status = 'applied' LIMIT 1", key).Scan(&existingResp)
			if err == nil {
				_, _ = ledgerDB.Exec(`
					INSERT INTO suppressed_entries (order_id, customer_id, amount_cents, idempotency_key, job_id, fencing_token, reason)
					VALUES (?, ?, ?, ?, ?, ?, 'duplicate_key')
				`, req.OrderID, req.CustomerID, req.AmountCents, key, jobID, fencingToken)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotent-Replayed", "true")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(existingResp))
				return
			}
		}

		_, err := ledgerDB.Exec(`
			INSERT INTO charges (order_id, customer_id, amount_cents, idempotency_key, job_id, fencing_token, status, response_body)
			VALUES (?, ?, ?, ?, ?, ?, 'applied', ?)
		`, req.OrderID, req.CustomerID, req.AmountCents, key, jobID, fencingToken, respJSON)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respJSON))
	})

	ledgerMux.HandleFunc("GET /ledger/stats", func(w http.ResponseWriter, r *http.Request) {
		ledgerMu.Lock()
		defer ledgerMu.Unlock()

		var appliedCount int64
		var totalAmount int64
		_ = ledgerDB.QueryRow("SELECT COUNT(*), COALESCE(SUM(amount_cents), 0) FROM charges WHERE status = 'applied'").Scan(&appliedCount, &totalAmount)

		var suppressedCount int64
		_ = ledgerDB.QueryRow("SELECT COUNT(*) FROM suppressed_entries").Scan(&suppressedCount)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"applied_count":      appliedCount,
			"suppressed_count":   suppressedCount,
			"total_amount_cents": totalAmount,
		})
	})

	ledgerMux.HandleFunc("GET /ledger/export", func(w http.ResponseWriter, r *http.Request) {
		ledgerMu.Lock()
		defer ledgerMu.Unlock()

		w.Header().Set("Content-Type", "application/x-ndjson")
		rows, err := ledgerDB.Query("SELECT order_id, customer_id, amount_cents, idempotency_key, fencing_token, strftime('%Y-%m-%dT%H:%M:%SZ', created_at) FROM charges ORDER BY id ASC")
		if err == nil {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var orderID, custID, key, recAt string
				var amt, fToken int64
				_ = rows.Scan(&orderID, &custID, &amt, &key, &fToken, &recAt)
				line, _ := json.Marshal(map[string]any{
					"type":            "applied",
					"order_id":        orderID,
					"customer_id":     custID,
					"amount_cents":    amt,
					"idempotency_key": key,
					"fencing_token":   fToken,
					"received_at":     recAt,
				})
				_, _ = w.Write(append(line, 0x0a))
			}
		}
	})

	ledgerServer := httptest.NewServer(ledgerMux)
	defer ledgerServer.Close()

	// 2. Define chargeCustomerHandler
	chargeHandler := func(ctx context.Context, job *worker.Job) error {
		var payload struct {
			OrderID     string `json:"order_id"`
			CustomerID  string `json:"customer_id"`
			AmountCents int64  `json:"amount_cents"`
		}
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return worker.Permanent(fmt.Errorf("malformed payload: %w", err))
		}

		reqBody, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ledgerServer.URL+"/charge", bytes.NewReader(reqBody))
		if err != nil {
			return worker.Permanent(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("steadfast:%s", job.ID.String()))
		req.Header.Set("X-Fencing-Token", strconv.FormatInt(job.FencingToken, 10))

		resp, err := ledgerServer.Client().Do(req)
		if err != nil {
			return worker.Retryable(err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return worker.Permanent(fmt.Errorf("4xx error: %d", resp.StatusCode))
		}
		return worker.Retryable(fmt.Errorf("5xx error: %d", resp.StatusCode))
	}

	// 3. Start 4 example worker instances concurrently
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()

	var workerWg sync.WaitGroup
	for w := 0; w < 4; w++ {
		workerWg.Add(1)
		go func(workerIdx int) {
			defer workerWg.Done()
			wCfg := worker.Config{
				APIBaseURL:     apiServer.URL,
				APIKey:         validKey,
				WorkerID:       fmt.Sprintf("worker-a71-%d", workerIdx),
				Concurrency:    1,
				ClaimBatchSize: 10,
				PollInterval:   30 * time.Millisecond,
				Handlers: map[string]worker.Handler{
					"charge_customer": chargeHandler,
				},
			}
			_ = worker.Run(workerCtx, wCfg)
		}(w)
	}

	// 4. Submit 100 charge_customer jobs through the API
	const totalJobs = 100
	var expectedTotalAmount int64
	for i := 1; i <= totalJobs; i++ {
		amt := int64(1000 + i)
		expectedTotalAmount += amt

		body := fmt.Sprintf(`{"type":"charge_customer","payload":{"order_id":"ord-%03d","customer_id":"cust-%03d","amount_cents":%d}}`, i, i, amt)
		req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+validKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("sub-key-%03d", i))

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("job submission %d failed with code %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	// 5. Poll until all 100 jobs reach SUCCEEDED
	deadline := time.Now().Add(15 * time.Second)
	var succeededCount int
	for time.Now().Before(deadline) {
		_ = s.Pool.QueryRow(context.Background(),
			"SELECT COUNT(*) FROM jobs WHERE tenant_id = $1 AND state = 'SUCCEEDED' AND job_type = 'charge_customer'",
			tenant.ID).Scan(&succeededCount)
		if succeededCount == totalJobs {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if succeededCount != totalJobs {
		t.Fatalf("expected all %d jobs to reach SUCCEEDED, got %d", totalJobs, succeededCount)
	}

	// Stop workers
	cancelWorkers()
	workerWg.Wait()

	// 6. Verify ledger stats: exactly 100 applied entries, 0 suppressed entries, correct total amount
	statsReq, _ := http.NewRequest(http.MethodGet, ledgerServer.URL+"/ledger/stats", nil)
	statsResp, err := ledgerServer.Client().Do(statsReq)
	if err != nil {
		t.Fatalf("failed to query ledger stats: %v", err)
	}
	defer func() { _ = statsResp.Body.Close() }()

	var stats struct {
		AppliedCount     int64 `json:"applied_count"`
		SuppressedCount  int64 `json:"suppressed_count"`
		TotalAmountCents int64 `json:"total_amount_cents"`
	}
	_ = json.NewDecoder(statsResp.Body).Decode(&stats)

	if stats.AppliedCount != int64(totalJobs) {
		t.Fatalf("A7.1 VIOLATION: expected %d applied ledger entries, got %d", totalJobs, stats.AppliedCount)
	}
	if stats.SuppressedCount != 0 {
		t.Fatalf("expected 0 suppressed entries for unique submissions, got %d", stats.SuppressedCount)
	}
	if stats.TotalAmountCents != expectedTotalAmount {
		t.Fatalf("expected total amount %d, got %d", expectedTotalAmount, stats.TotalAmountCents)
	}

	// 7. Test Postgres independence: Close Steadfast Postgres pool and verify Ledger export still functions
	s.Pool.Close()

	expReq, _ := http.NewRequest(http.MethodGet, ledgerServer.URL+"/ledger/export", nil)
	expResp, err := ledgerServer.Client().Do(expReq)
	if err != nil {
		t.Fatalf("failed to query ledger export after postgres closed: %v", err)
	}
	defer func() { _ = expResp.Body.Close() }()

	if expResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from ledger export with postgres closed, got %d", expResp.StatusCode)
	}

	expBytes, _ := io.ReadAll(expResp.Body)
	rawLines := strings.Split(strings.TrimSpace(string(expBytes)), string([]byte{0x0a}))
	if len(rawLines) != totalJobs {
		t.Fatalf("expected %d export lines from ledger with postgres closed, got %d", totalJobs, len(rawLines))
	}
}
