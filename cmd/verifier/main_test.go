package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestVerifierDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite: %v", err)
	}

	schema := `
	CREATE TABLE jobs (
		id TEXT PRIMARY KEY,
		tenant_id TEXT,
		state TEXT,
		job_type TEXT,
		payload_hash TEXT,
		idempotency_key TEXT,
		dead_reason TEXT,
		run_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE job_attempts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id TEXT,
		attempt_no INTEGER,
		fencing_token INTEGER,
		worker_id TEXT,
		outcome TEXT
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("failed to create verifier test schema: %v", err)
	}
	return db
}

func setupTestFiles(t *testing.T, acksContent, intentContent, blAcksContent string) string {
	tempDir, err := os.MkdirTemp("", "verifier-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	if acksContent != "" {
		_ = os.WriteFile(filepath.Join(tempDir, "acks-steadfast.ndjson"), []byte(acksContent), 0644)
	}
	if intentContent != "" {
		_ = os.WriteFile(filepath.Join(tempDir, "intent-steadfast.ndjson"), []byte(intentContent), 0644)
	}
	if blAcksContent != "" {
		_ = os.WriteFile(filepath.Join(tempDir, "acks-baseline.ndjson"), []byte(blAcksContent), 0644)
	}

	return tempDir
}

func setupTestLedgerServer(exportLines []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, line := range exportLines {
			_, _ = w.Write(append([]byte(line), 0x0a))
		}
	}))
}

func TestVerifier_CleanRun_Passes(t *testing.T) {
	db := setupTestVerifierDB(t)
	defer func() { _ = db.Close() }()

	_, err := db.Exec(`
		INSERT INTO jobs (id, state, job_type, payload_hash, idempotency_key)
		VALUES ('job-01', 'SUCCEEDED', 'charge_customer', 'hash1', 'ord-st-01'),
		       ('job-02', 'SUCCEEDED', 'charge_customer', 'hash2', 'ord-st-02');
	`)
	if err != nil {
		t.Fatalf("insert jobs failed: %v", err)
	}

	acks := `{"run_id":"r1","seq":1,"order_id":"ord-st-01","job_id":"job-01","amount_cents":1000,"lane":"steadfast"}
{"run_id":"r1","seq":2,"order_id":"ord-st-02","job_id":"job-02","amount_cents":2000,"lane":"steadfast"}`

	tempDir := setupTestFiles(t, acks, "", "")
	defer func() { _ = os.RemoveAll(tempDir) }()

	ledgerLines := []string{
		`{"entry_type":"applied","order_id":"ord-st-01","customer_id":"c1","amount_cents":1000,"idempotency_key":"steadfast:job-01","job_id":"job-01","fencing_token":1}`,
		`{"entry_type":"applied","order_id":"ord-st-02","customer_id":"c2","amount_cents":2000,"idempotency_key":"steadfast:job-02","job_id":"job-02","fencing_token":1}`,
	}
	ledgerServer := setupTestLedgerServer(ledgerLines)
	defer ledgerServer.Close()

	v := NewVerifier(Config{
		ArtifactsDir: tempDir,
		LedgerURL:    ledgerServer.URL,
	})

	verdict := &Verdict{Status: "PASS"}
	verdict.Steadfast.Invariants = make(map[string]InvariantResult)

	stAcks, _ := readAcksFile(filepath.Join(tempDir, "acks-steadfast.ndjson"))
	entries, _ := v.fetchLedgerExport(context.Background())

	v.auditSteadfast(context.Background(), db, stAcks, nil, entries, verdict)

	for invName, res := range verdict.Steadfast.Invariants {
		if res.Status != "PASS" {
			t.Fatalf("expected invariant %s to PASS, got %s: %s", invName, res.Status, res.Details)
		}
	}
}

func TestVerifier_I1_LostJob_Fails(t *testing.T) {
	db := setupTestVerifierDB(t)
	defer func() { _ = db.Close() }()

	// Job-01 present, but Job-02 missing!
	_, _ = db.Exec(`INSERT INTO jobs (id, state, job_type) VALUES ('job-01', 'SUCCEEDED', 'charge_customer')`)

	acks := `{"run_id":"r1","seq":1,"order_id":"ord-st-01","job_id":"job-01","amount_cents":1000,"lane":"steadfast"}
{"run_id":"r1","seq":2,"order_id":"ord-st-02","job_id":"job-02","amount_cents":2000,"lane":"steadfast"}`

	tempDir := setupTestFiles(t, acks, "", "")
	defer func() { _ = os.RemoveAll(tempDir) }()

	v := NewVerifier(Config{ArtifactsDir: tempDir})
	verdict := &Verdict{Status: "PASS"}
	verdict.Steadfast.Invariants = make(map[string]InvariantResult)

	stAcks, _ := readAcksFile(filepath.Join(tempDir, "acks-steadfast.ndjson"))
	v.auditSteadfast(context.Background(), db, stAcks, nil, nil, verdict)

	i1 := verdict.Steadfast.Invariants["I1_no_lost_jobs"]
	if i1.Status != "FAIL" {
		t.Fatalf("expected I1 to FAIL on missing acknowledged job, got %s", i1.Status)
	}
}

func TestVerifier_I2_DuplicateCharge_Fails(t *testing.T) {
	db := setupTestVerifierDB(t)
	defer func() { _ = db.Close() }()

	_, _ = db.Exec(`INSERT INTO jobs (id, state, job_type) VALUES ('job-01', 'SUCCEEDED', 'charge_customer')`)
	acks := `{"run_id":"r1","seq":1,"order_id":"ord-st-01","job_id":"job-01","amount_cents":1000,"lane":"steadfast"}`
	tempDir := setupTestFiles(t, acks, "", "")
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Duplicate charges for ord-st-01
	ledgerLines := []string{
		`{"entry_type":"applied","order_id":"ord-st-01","customer_id":"c1","amount_cents":1000,"idempotency_key":"steadfast:job-01","job_id":"job-01","fencing_token":1}`,
		`{"entry_type":"applied","order_id":"ord-st-01","customer_id":"c1","amount_cents":1000,"idempotency_key":"steadfast:job-01","job_id":"job-01","fencing_token":2}`,
	}
	ledgerServer := setupTestLedgerServer(ledgerLines)
	defer ledgerServer.Close()

	v := NewVerifier(Config{ArtifactsDir: tempDir, LedgerURL: ledgerServer.URL})
	verdict := &Verdict{Status: "PASS"}
	verdict.Steadfast.Invariants = make(map[string]InvariantResult)

	stAcks, _ := readAcksFile(filepath.Join(tempDir, "acks-steadfast.ndjson"))
	entries, _ := v.fetchLedgerExport(context.Background())
	v.auditSteadfast(context.Background(), db, stAcks, nil, entries, verdict)

	i2 := verdict.Steadfast.Invariants["I2_no_duplicate_charges"]
	if i2.Status != "FAIL" {
		t.Fatalf("expected I2 to FAIL on duplicate charge, got %s", i2.Status)
	}
}

func TestVerifier_I2b_PhantomSuccess_Fails(t *testing.T) {
	db := setupTestVerifierDB(t)
	defer func() { _ = db.Close() }()

	// Job is marked SUCCEEDED in PostgreSQL, but no charge exists in ledger!
	_, _ = db.Exec(`INSERT INTO jobs (id, state, job_type) VALUES ('job-phantom', 'SUCCEEDED', 'charge_customer')`)
	acks := `{"run_id":"r1","seq":1,"order_id":"ord-st-phantom","job_id":"job-phantom","amount_cents":1000,"lane":"steadfast"}`
	tempDir := setupTestFiles(t, acks, "", "")
	defer func() { _ = os.RemoveAll(tempDir) }()

	ledgerServer := setupTestLedgerServer([]string{})
	defer ledgerServer.Close()

	v := NewVerifier(Config{ArtifactsDir: tempDir, LedgerURL: ledgerServer.URL})
	verdict := &Verdict{Status: "PASS"}
	verdict.Steadfast.Invariants = make(map[string]InvariantResult)

	stAcks, _ := readAcksFile(filepath.Join(tempDir, "acks-steadfast.ndjson"))
	entries, _ := v.fetchLedgerExport(context.Background())
	v.auditSteadfast(context.Background(), db, stAcks, nil, entries, verdict)

	i2b := verdict.Steadfast.Invariants["I2b_set_based_correlation"]
	if i2b.Status != "FAIL" {
		t.Fatalf("expected I2b to FAIL on phantom success, got %s", i2b.Status)
	}
}

func TestVerifier_I2b_DeadWithAppliedCharge_Fails(t *testing.T) {
	db := setupTestVerifierDB(t)
	defer func() { _ = db.Close() }()

	// Job is DEAD, but has an applied charge in mock ledger!
	_, _ = db.Exec(`INSERT INTO jobs (id, state, job_type, dead_reason) VALUES ('job-dead', 'DEAD', 'charge_customer', 'max_attempts_exhausted')`)
	acks := `{"run_id":"r1","seq":1,"order_id":"ord-st-dead","job_id":"job-dead","amount_cents":1000,"lane":"steadfast"}`
	tempDir := setupTestFiles(t, acks, "", "")
	defer func() { _ = os.RemoveAll(tempDir) }()

	ledgerLines := []string{
		`{"entry_type":"applied","order_id":"ord-st-dead","customer_id":"c1","amount_cents":1000,"idempotency_key":"steadfast:job-dead","job_id":"job-dead","fencing_token":1}`,
	}
	ledgerServer := setupTestLedgerServer(ledgerLines)
	defer ledgerServer.Close()

	v := NewVerifier(Config{ArtifactsDir: tempDir, LedgerURL: ledgerServer.URL})
	verdict := &Verdict{Status: "PASS"}
	verdict.Steadfast.Invariants = make(map[string]InvariantResult)

	stAcks, _ := readAcksFile(filepath.Join(tempDir, "acks-steadfast.ndjson"))
	entries, _ := v.fetchLedgerExport(context.Background())
	v.auditSteadfast(context.Background(), db, stAcks, nil, entries, verdict)

	deadCheck := verdict.Steadfast.Invariants["dead_with_applied_charge"]
	if deadCheck.Status != "FAIL" {
		t.Fatalf("expected dead_with_applied_charge to FAIL, got %s", deadCheck.Status)
	}
	if verdict.Steadfast.DeadWithAppliedChargeCount != 1 {
		t.Fatalf("expected 1 dead_with_applied_charge, got %d", verdict.Steadfast.DeadWithAppliedChargeCount)
	}
}

func TestVerifier_I3_TotalsMismatch_Fails(t *testing.T) {
	db := setupTestVerifierDB(t)
	defer func() { _ = db.Close() }()

	// 2 jobs acknowledged, but only 1 job in terminal state
	_, _ = db.Exec(`INSERT INTO jobs (id, state, job_type) VALUES ('job-01', 'SUCCEEDED', 'charge_customer')`)
	acks := `{"run_id":"r1","seq":1,"order_id":"ord-st-01","job_id":"job-01","amount_cents":1000,"lane":"steadfast"}
{"run_id":"r1","seq":2,"order_id":"ord-st-02","job_id":"job-02","amount_cents":2000,"lane":"steadfast"}`

	tempDir := setupTestFiles(t, acks, "", "")
	defer func() { _ = os.RemoveAll(tempDir) }()

	v := NewVerifier(Config{ArtifactsDir: tempDir})
	verdict := &Verdict{Status: "PASS"}
	verdict.Steadfast.Invariants = make(map[string]InvariantResult)

	stAcks, _ := readAcksFile(filepath.Join(tempDir, "acks-steadfast.ndjson"))
	v.auditSteadfast(context.Background(), db, stAcks, nil, nil, verdict)

	i3 := verdict.Steadfast.Invariants["I3_totals_reconcile"]
	if i3.Status != "FAIL" {
		t.Fatalf("expected I3 to FAIL on totals mismatch, got %s", i3.Status)
	}
}

func TestVerifier_I4_ProperFencing_Passes(t *testing.T) {
	db := setupTestVerifierDB(t)
	defer func() { _ = db.Close() }()

	_, _ = db.Exec(`
		INSERT INTO jobs (id, state, job_type) VALUES ('job-fence', 'SUCCEEDED', 'charge_customer');
		INSERT INTO job_attempts (job_id, attempt_no, fencing_token, worker_id, outcome)
		VALUES ('job-fence', 1, 1, 'worker-A', 'lease_expired'),
		       ('job-fence', 2, 2, 'worker-B', 'succeeded');
	`)

	acks := `{"run_id":"r1","seq":1,"order_id":"ord-st-fence","job_id":"job-fence","amount_cents":1000,"lane":"steadfast"}`
	tempDir := setupTestFiles(t, acks, "", "")
	defer func() { _ = os.RemoveAll(tempDir) }()

	ledgerLines := []string{
		`{"entry_type":"applied","order_id":"ord-st-fence","customer_id":"c1","amount_cents":1000,"idempotency_key":"steadfast:job-fence","job_id":"job-fence","fencing_token":2}`,
	}
	ledgerServer := setupTestLedgerServer(ledgerLines)
	defer ledgerServer.Close()

	v := NewVerifier(Config{ArtifactsDir: tempDir, LedgerURL: ledgerServer.URL})
	verdict := &Verdict{Status: "PASS"}
	verdict.Steadfast.Invariants = make(map[string]InvariantResult)

	stAcks, _ := readAcksFile(filepath.Join(tempDir, "acks-steadfast.ndjson"))
	entries, _ := v.fetchLedgerExport(context.Background())
	v.auditSteadfast(context.Background(), db, stAcks, nil, entries, verdict)

	i4 := verdict.Steadfast.Invariants["I4_zombie_writes_rejected"]
	if i4.Status != "PASS" {
		t.Fatalf("expected I4 to PASS on proper fencing, got %s: %s", i4.Status, i4.Details)
	}
}

func TestVerifier_I4_DisabledFencingZombieWrite_Fails(t *testing.T) {
	db := setupTestVerifierDB(t)
	defer func() { _ = db.Close() }()

	// Simulating disabled fencing where two attempts both succeeded (zombie write accepted!)
	_, _ = db.Exec(`
		INSERT INTO jobs (id, state, job_type) VALUES ('job-zombie', 'SUCCEEDED', 'charge_customer');
		INSERT INTO job_attempts (job_id, attempt_no, fencing_token, worker_id, outcome)
		VALUES ('job-zombie', 1, 1, 'worker-A', 'succeeded'),
		       ('job-zombie', 2, 2, 'worker-B', 'succeeded');
	`)

	acks := `{"run_id":"r1","seq":1,"order_id":"ord-st-zombie","job_id":"job-zombie","amount_cents":1000,"lane":"steadfast"}`
	tempDir := setupTestFiles(t, acks, "", "")
	defer func() { _ = os.RemoveAll(tempDir) }()

	v := NewVerifier(Config{ArtifactsDir: tempDir})
	verdict := &Verdict{Status: "PASS"}
	verdict.Steadfast.Invariants = make(map[string]InvariantResult)

	stAcks, _ := readAcksFile(filepath.Join(tempDir, "acks-steadfast.ndjson"))
	v.auditSteadfast(context.Background(), db, stAcks, nil, nil, verdict)

	i4 := verdict.Steadfast.Invariants["I4_zombie_writes_rejected"]
	if i4.Status != "FAIL" {
		t.Fatalf("expected I4 to FAIL on zombie write, got %s", i4.Status)
	}
}

func TestVerifier_CorruptAcks_Incomplete(t *testing.T) {
	corruptAcks := `{"run_id":"r1","seq":1,"order_id":"ord-st-01","job_id":"duplicate-job","amount_cents":1000,"lane":"steadfast"}
{"run_id":"r1","seq":2,"order_id":"ord-st-02","job_id":"duplicate-job","amount_cents":2000,"lane":"steadfast"}`

	tempDir := setupTestFiles(t, corruptAcks, "", "")
	defer func() { _ = os.RemoveAll(tempDir) }()

	v := NewVerifier(Config{ArtifactsDir: tempDir})
	verdict := &Verdict{Status: "PASS"}
	verdict.Steadfast.Invariants = make(map[string]InvariantResult)

	stAcks, _ := readAcksFile(filepath.Join(tempDir, "acks-steadfast.ndjson"))
	v.auditSteadfast(context.Background(), nil, stAcks, nil, nil, verdict)

	if verdict.Status != "INCOMPLETE" || verdict.ExitCode != 2 {
		t.Fatalf("expected INCOMPLETE (exit code 2) on corrupt acks with duplicate job_id, got status=%s exit=%d", verdict.Status, verdict.ExitCode)
	}
}

func TestVerifier_ZeroSteadfastImports(t *testing.T) {
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("failed to read verifier main.go: %v", err)
	}
	content := string(data)

	forbidden := []string{
		"steadfast/cmd/api",
		"steadfast/cmd/reaper",
		"steadfast/sdk/worker",
		"steadfast/internal/store",
		"steadfast/internal/auth",
		"steadfast/cmd/dashboard",
	}

	for _, f := range forbidden {
		if strings.Contains(content, f) {
			t.Fatalf("Verifier independence violation: main.go imports forbidden package %q", f)
		}
	}
}

func TestVerifier_DrainTimeout_Incomplete(t *testing.T) {
	db := setupTestVerifierDB(t)
	defer func() { _ = db.Close() }()

	// Insert a job still in LEASED state
	_, _ = db.Exec(`INSERT INTO jobs (id, state, job_type) VALUES ('job-leased', 'LEASED', 'charge_customer')`)

	tempDir := setupTestFiles(t, "", "", "")
	defer func() { _ = os.RemoveAll(tempDir) }()

	v := NewVerifier(Config{
		ArtifactsDir: tempDir,
		DrainTimeout: 100 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
	})

	// Test drain loop directly
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	drainDeadline := time.Now().Add(v.cfg.DrainTimeout)
	timedOut := false
	for {
		var nonterminalCount int
		_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE state IN ('QUEUED', 'LEASED')").Scan(&nonterminalCount)
		if nonterminalCount == 0 {
			break
		}
		if time.Now().After(drainDeadline) {
			timedOut = true
			break
		}
		time.Sleep(v.cfg.PollInterval)
	}

	if !timedOut {
		t.Fatalf("expected drain loop to time out when nonterminal jobs exist")
	}
}

func TestVerifier_UnacknowledgedIntent_AmbiguousEvidence_Incomplete(t *testing.T) {
	db := setupTestVerifierDB(t)
	defer func() { _ = db.Close() }()

	// Intent exists, but job in DB remains in LEASED state (ambiguous / incomplete!)
	_, _ = db.Exec(`INSERT INTO jobs (id, state, job_type, idempotency_key) VALUES ('job-unacked', 'LEASED', 'charge_customer', 'ord-st-unacked')`)

	intent := `{"run_id":"r1","seq":1,"order_id":"ord-st-unacked","idempotency_key":"ord-st-unacked","amount_cents":1000,"lane":"steadfast"}`
	tempDir := setupTestFiles(t, "", intent, "")
	defer func() { _ = os.RemoveAll(tempDir) }()

	v := NewVerifier(Config{ArtifactsDir: tempDir})
	verdict := &Verdict{Status: "PASS"}
	verdict.Steadfast.Invariants = make(map[string]InvariantResult)

	intents, _ := readIntentFile(filepath.Join(tempDir, "intent-steadfast.ndjson"))
	v.auditSteadfast(context.Background(), db, nil, intents, nil, verdict)

	// Since job was in LEASED (not terminal), reconciledUnacked remains 0 and cannot produce pass
	if verdict.Steadfast.ReconciledUnackedCount != 0 {
		t.Fatalf("expected 0 reconciled unacknowledged jobs for nonterminal job, got %d", verdict.Steadfast.ReconciledUnackedCount)
	}
}

func TestVerifier_DeletedAcknowledgedJobRow_FailsI1(t *testing.T) {
	db := setupTestVerifierDB(t)
	defer func() { _ = db.Close() }()

	// Acknowledged job is inserted, then deliberately deleted from the database
	_, _ = db.Exec(`INSERT INTO jobs (id, state, job_type) VALUES ('job-deleted', 'SUCCEEDED', 'charge_customer')`)
	// Delete it!
	_, _ = db.Exec(`DELETE FROM jobs WHERE id = 'job-deleted'`)

	acks := `{"run_id":"r1","seq":1,"order_id":"ord-st-del","job_id":"job-deleted","amount_cents":1000,"lane":"steadfast"}`
	tempDir := setupTestFiles(t, acks, "", "")
	defer func() { _ = os.RemoveAll(tempDir) }()

	v := NewVerifier(Config{ArtifactsDir: tempDir})
	verdict := &Verdict{Status: "PASS"}
	verdict.Steadfast.Invariants = make(map[string]InvariantResult)

	stAcks, _ := readAcksFile(filepath.Join(tempDir, "acks-steadfast.ndjson"))
	v.auditSteadfast(context.Background(), db, stAcks, nil, nil, verdict)

	i1 := verdict.Steadfast.Invariants["I1_no_lost_jobs"]
	if i1.Status != "FAIL" {
		t.Fatalf("A deliberately deleted acknowledged job MUST fail I1, got %s", i1.Status)
	}
}
