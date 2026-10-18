package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTestLedger(t *testing.T) (*LedgerServer, *sql.DB, string) {
	tempDir, err := os.MkdirTemp("", "ledger-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	dbPath := filepath.Join(tempDir, "ledger.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}

	server, err := NewLedgerServer(db, "test-admin-key", nil)
	if err != nil {
		t.Fatalf("failed to create ledger server: %v", err)
	}

	return server, db, tempDir
}

// A8.1: replaying the same key returns an identical response body and adds a suppressed entry.
func TestLedger_A8_1_IdempotentReplayReturnsIdenticalAndAddsSuppressed(t *testing.T) {
	server, db, tempDir := setupTestLedger(t)
	defer func() { _ = db.Close() }()
	defer func() { _ = os.RemoveAll(tempDir) }()

	handler := server.Routes()
	chargeBody := `{"order_id":"ord-123","customer_id":"cust-456","amount_cents":2500}`

	// First request with key
	req1 := httptest.NewRequest(http.MethodPost, "/charge", bytes.NewReader([]byte(chargeBody)))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Idempotency-Key", "steadfast:job-001")
	req1.Header.Set("X-Fencing-Token", "1")
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w1.Code, w1.Body.String())
	}
	resp1 := w1.Body.String()

	// Second request with same key
	req2 := httptest.NewRequest(http.MethodPost, "/charge", bytes.NewReader([]byte(chargeBody)))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", "steadfast:job-001")
	req2.Header.Set("X-Fencing-Token", "2")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w2.Code, w2.Body.String())
	}
	resp2 := w2.Body.String()

	// Assert identical response body
	if resp1 != resp2 {
		t.Fatalf("expected identical response body on replay. Resp1: %s, Resp2: %s", resp1, resp2)
	}

	if w2.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("expected Idempotent-Replayed: true header on replay")
	}

	// Verify database rows: exactly 1 applied charge, exactly 1 suppressed entry
	var appliedCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM charges WHERE status = 'applied'").Scan(&appliedCount)
	if appliedCount != 1 {
		t.Fatalf("expected 1 applied charge, got %d", appliedCount)
	}

	var suppressedCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM suppressed_entries").Scan(&suppressedCount)
	if suppressedCount != 1 {
		t.Fatalf("expected 1 suppressed entry, got %d", suppressedCount)
	}

	var suppKey, suppReason string
	_ = db.QueryRow("SELECT idempotency_key, reason FROM suppressed_entries LIMIT 1").Scan(&suppKey, &suppReason)
	if suppKey != "steadfast:job-001" || suppReason != "duplicate_key" {
		t.Fatalf("unexpected suppressed entry: key=%s, reason=%s", suppKey, suppReason)
	}
}

// A8.2: with apply_then_fail p=1 and no key, one request produces a duplicate on retry; with the key it does not
func TestLedger_A8_2_ApplyThenFailDeduplication(t *testing.T) {
	server, db, tempDir := setupTestLedger(t)
	defer func() { _ = db.Close() }()
	defer func() { _ = os.RemoveAll(tempDir) }()

	handler := server.Routes()

	// Configure fault mode apply_then_fail p=1
	server.fault = FaultConfig{
		Mode:        "apply_then_fail",
		Probability: 1.0,
	}

	chargeBodyNoKey := `{"order_id":"ord-nokey","customer_id":"cust-1","amount_cents":1000}`

	// Case 1: Without key, request 1 applies then fails (500)
	req1 := httptest.NewRequest(http.MethodPost, "/charge", bytes.NewReader([]byte(chargeBodyNoKey)))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	if w1.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on apply_then_fail, got %d", w1.Code)
	}

	// Client retries without key -> applies again! Produces duplicate
	req1Retry := httptest.NewRequest(http.MethodPost, "/charge", bytes.NewReader([]byte(chargeBodyNoKey)))
	req1Retry.Header.Set("Content-Type", "application/json")
	w1Retry := httptest.NewRecorder()
	handler.ServeHTTP(w1Retry, req1Retry)
	if w1Retry.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on apply_then_fail retry, got %d", w1Retry.Code)
	}

	var noKeyAppliedCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM charges WHERE order_id = 'ord-nokey'").Scan(&noKeyAppliedCount)
	if noKeyAppliedCount != 2 {
		t.Fatalf("without idempotency key, expected 2 duplicate charges, got %d", noKeyAppliedCount)
	}

	// Case 2: With key, request 1 applies then fails (500)
	chargeBodyWithKey := `{"order_id":"ord-withkey","customer_id":"cust-2","amount_cents":2000}`
	req2 := httptest.NewRequest(http.MethodPost, "/charge", bytes.NewReader([]byte(chargeBodyWithKey)))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", "steadfast:key-protect")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on apply_then_fail with key, got %d", w2.Code)
	}

	// Turn fault off for the retry (simulating transient network recovery)
	server.fault = FaultConfig{Mode: "none"}

	// Client retries with the SAME key
	req2Retry := httptest.NewRequest(http.MethodPost, "/charge", bytes.NewReader([]byte(chargeBodyWithKey)))
	req2Retry.Header.Set("Content-Type", "application/json")
	req2Retry.Header.Set("Idempotency-Key", "steadfast:key-protect")
	w2Retry := httptest.NewRecorder()
	handler.ServeHTTP(w2Retry, req2Retry)
	if w2Retry.Code != http.StatusOK {
		t.Fatalf("expected 200 on retry with key, got %d: %s", w2Retry.Code, w2Retry.Body.String())
	}

	// With key, charges count MUST be exactly 1!
	var withKeyAppliedCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM charges WHERE order_id = 'ord-withkey'").Scan(&withKeyAppliedCount)
	if withKeyAppliedCount != 1 {
		t.Fatalf("with idempotency key, expected exactly 1 charge, got %d", withKeyAppliedCount)
	}

	var withKeySuppressedCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM suppressed_entries WHERE idempotency_key = 'steadfast:key-protect'").Scan(&withKeySuppressedCount)
	if withKeySuppressedCount != 1 {
		t.Fatalf("expected 1 suppressed entry on retry with key, got %d", withKeySuppressedCount)
	}
}

func TestLedger_ExportAndStats(t *testing.T) {
	server, db, tempDir := setupTestLedger(t)
	defer func() { _ = db.Close() }()
	defer func() { _ = os.RemoveAll(tempDir) }()

	handler := server.Routes()

	// Apply 2 charges, 1 replay
	for i := 1; i <= 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/charge", bytes.NewReader([]byte(fmt.Sprintf(`{"order_id":"ord-%d","customer_id":"c-%d","amount_cents":%d}`, i, i, i*1000))))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("steadfast:job-%d", i))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	}

	// Replay job 1
	replayReq := httptest.NewRequest(http.MethodPost, "/charge", bytes.NewReader([]byte(`{"order_id":"ord-1","customer_id":"c-1","amount_cents":1000}`)))
	replayReq.Header.Set("Content-Type", "application/json")
	replayReq.Header.Set("Idempotency-Key", "steadfast:job-1")
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, replayReq)
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 on replay, got %d", rw.Code)
	}

	// Test GET /ledger/stats
	statsReq := httptest.NewRequest(http.MethodGet, "/ledger/stats", nil)
	sw := httptest.NewRecorder()
	handler.ServeHTTP(sw, statsReq)
	if sw.Code != http.StatusOK {
		t.Fatalf("expected 200 for stats, got %d", sw.Code)
	}

	var stats struct {
		AppliedCount     int64 `json:"applied_count"`
		SuppressedCount  int64 `json:"suppressed_count"`
		TotalAmountCents int64 `json:"total_amount_cents"`
	}
	_ = json.NewDecoder(sw.Body).Decode(&stats)
	if stats.AppliedCount != 2 {
		t.Fatalf("expected applied_count=2, got %d", stats.AppliedCount)
	}
	if stats.SuppressedCount != 1 {
		t.Fatalf("expected suppressed_count=1, got %d", stats.SuppressedCount)
	}
	if stats.TotalAmountCents != 3000 {
		t.Fatalf("expected total_amount_cents=3000, got %d", stats.TotalAmountCents)
	}

	// Test GET /ledger/export
	expReq := httptest.NewRequest(http.MethodGet, "/ledger/export", nil)
	ew := httptest.NewRecorder()
	handler.ServeHTTP(ew, expReq)
	if ew.Code != http.StatusOK {
		t.Fatalf("expected 200 for export, got %d", ew.Code)
	}

	rawExport := strings.TrimSpace(ew.Body.String())
	lines := strings.Split(rawExport, string([]byte{0x0a}))
	if len(lines) != 3 {
		t.Fatalf("expected 3 NDJSON export lines (2 applied, 1 suppressed), got %d: %s", len(lines), rawExport)
	}
}

func TestLedger_AdminFaultAuthAndModes(t *testing.T) {
	server, db, tempDir := setupTestLedger(t)
	defer func() { _ = db.Close() }()
	defer func() { _ = os.RemoveAll(tempDir) }()

	handler := server.Routes()

	// 1. Without admin key -> 401 Unauthorized
	reqUnauthorized := httptest.NewRequest(http.MethodPost, "/admin/fault", bytes.NewReader([]byte(`{"mode":"down"}`)))
	wUnauth := httptest.NewRecorder()
	handler.ServeHTTP(wUnauth, reqUnauthorized)
	if wUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized without admin key, got %d", wUnauth.Code)
	}

	// 2. With valid admin key -> 200 OK
	reqValid := httptest.NewRequest(http.MethodPost, "/admin/fault", bytes.NewReader([]byte(`{"mode":"error_rate","probability":0.5}`)))
	reqValid.Header.Set("X-Admin-Key", "test-admin-key")
	wValid := httptest.NewRecorder()
	handler.ServeHTTP(wValid, reqValid)
	if wValid.Code != http.StatusOK {
		t.Fatalf("expected 200 OK with valid admin key, got %d: %s", wValid.Code, wValid.Body.String())
	}

	// Verify fault state
	if server.fault.Mode != "error_rate" || server.fault.Probability != 0.5 {
		t.Fatalf("expected fault error_rate 0.5, got mode=%s, prob=%v", server.fault.Mode, server.fault.Probability)
	}
}
