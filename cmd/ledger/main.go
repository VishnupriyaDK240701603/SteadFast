package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

type FaultConfig struct {
	Mode        string  `json:"mode"` // none, error_rate, latency_ms, down, apply_then_fail
	Probability float64 `json:"probability,omitempty"`
	LatencyMs   int     `json:"latency_ms,omitempty"`
}

type LedgerServer struct {
	db       *sql.DB
	logger   *slog.Logger
	adminKey string
	mu       sync.RWMutex
	fault    FaultConfig
}

type ChargeRequest struct {
	OrderID     string `json:"order_id"`
	CustomerID  string `json:"customer_id"`
	AmountCents int64  `json:"amount_cents"`
}

type ChargeResponse struct {
	Status      string `json:"status"`
	OrderID     string `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
}

type ExportEntry struct {
	EntryType      string `json:"entry_type"` // applied or suppressed
	OrderID        string `json:"order_id"`
	CustomerID     string `json:"customer_id"`
	AmountCents    int64  `json:"amount_cents"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	JobID          string `json:"job_id,omitempty"`
	FencingToken   int64  `json:"fencing_token,omitempty"`
	ReceivedAt     string `json:"received_at"`
}

func NewLedgerServer(db *sql.DB, adminKey string, logger *slog.Logger) (*LedgerServer, error) {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	s := &LedgerServer{
		db:       db,
		adminKey: adminKey,
		logger:   logger,
		fault:    FaultConfig{Mode: "none"},
	}
	if err := s.initSchema(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *LedgerServer) initSchema() error {
	schema := `
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
	`
	_, err := s.db.Exec(schema)
	return err
}

func (s *LedgerServer) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /charge", s.handleCharge)
	mux.HandleFunc("GET /ledger/export", s.handleExport)
	mux.HandleFunc("GET /ledger/stats", s.handleStats)
	mux.HandleFunc("POST /admin/fault", s.handleAdminFault)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

func (s *LedgerServer) handleCharge(w http.ResponseWriter, r *http.Request) {
	var req ChargeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "bad_request", "message": "invalid JSON syntax"},
		})
		return
	}

	if req.OrderID == "" || req.CustomerID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "invalid_payload", "message": "order_id and customer_id are required"},
		})
		return
	}

	if req.AmountCents <= 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "invalid_amount", "message": "amount_cents must be positive"},
		})
		return
	}

	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	fencingTokenStr := strings.TrimSpace(r.Header.Get("X-Fencing-Token"))
	var fencingToken int64
	if fencingTokenStr != "" {
		_, _ = fmt.Sscanf(fencingTokenStr, "%d", &fencingToken)
	}

	var jobID string
	if strings.HasPrefix(idempotencyKey, "steadfast:") {
		jobID = strings.TrimPrefix(idempotencyKey, "steadfast:")
	} else {
		jobID = r.Header.Get("X-Job-ID")
	}

	// Fault evaluation
	s.mu.RLock()
	fault := s.fault
	s.mu.RUnlock()

	if fault.Mode == "down" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "service_unavailable", "message": "ledger is simulated down"},
		})
		return
	}

	if fault.Mode == "latency_ms" && fault.LatencyMs > 0 {
		time.Sleep(time.Duration(fault.LatencyMs) * time.Millisecond)
	}

	if fault.Mode == "error_rate" && fault.Probability > 0 {
		if rand.Float64() < fault.Probability {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"code": "injected_error", "message": "simulated ledger error"},
			})
			return
		}
	}

	applyThenFail := false
	if fault.Mode == "apply_then_fail" && fault.Probability > 0 {
		if rand.Float64() < fault.Probability {
			applyThenFail = true
		}
	}

	// Process charge
	s.mu.Lock()
	defer s.mu.Unlock()

	respObj := ChargeResponse{
		Status:      "applied",
		OrderID:     req.OrderID,
		AmountCents: req.AmountCents,
	}
	respBytes, _ := json.Marshal(respObj)

	if idempotencyKey != "" {
		// Check if charge already applied with this key
		var existingResp string
		row := s.db.QueryRow("SELECT response_body FROM charges WHERE idempotency_key = ? AND status = 'applied' LIMIT 1", idempotencyKey)
		err := row.Scan(&existingResp)
		if err == nil {
			// Duplicate! Record suppressed entry
			_, _ = s.db.Exec(`
				INSERT INTO suppressed_entries (order_id, customer_id, amount_cents, idempotency_key, job_id, fencing_token, reason)
				VALUES (?, ?, ?, ?, ?, ?, 'duplicate_key')
			`, req.OrderID, req.CustomerID, req.AmountCents, idempotencyKey, jobID, fencingToken)

			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(existingResp))
			return
		}

		// First time seeing this key -> apply
		_, err = s.db.Exec(`
			INSERT INTO charges (order_id, customer_id, amount_cents, idempotency_key, job_id, fencing_token, status, response_body)
			VALUES (?, ?, ?, ?, ?, ?, 'applied', ?)
		`, req.OrderID, req.CustomerID, req.AmountCents, idempotencyKey, jobID, fencingToken, string(respBytes))
		if err != nil {
			s.logger.Error("failed to insert charge", "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"code": "db_error", "message": "failed to record charge"},
			})
			return
		}
	} else {
		// No key -> always apply
		_, err := s.db.Exec(`
			INSERT INTO charges (order_id, customer_id, amount_cents, idempotency_key, job_id, fencing_token, status, response_body)
			VALUES (?, ?, ?, '', ?, ?, 'applied', ?)
		`, req.OrderID, req.CustomerID, req.AmountCents, jobID, fencingToken, string(respBytes))
		if err != nil {
			s.logger.Error("failed to insert charge", "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"code": "db_error", "message": "failed to record charge"},
			})
			return
		}
	}

	if applyThenFail {
		// Charge is applied to DB, but response fails!
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "downstream_failure_after_apply", "message": "charge applied but response failed"},
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}

func (s *LedgerServer) handleExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Export applied charges
	rows, err := s.db.Query(`
		SELECT order_id, customer_id, amount_cents, COALESCE(idempotency_key, ''), COALESCE(job_id, ''), COALESCE(fencing_token, 0), strftime('%Y-%m-%dT%H:%M:%SZ', created_at)
		FROM charges ORDER BY id ASC
	`)
	if err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var e ExportEntry
			e.EntryType = "applied"
			if err := rows.Scan(&e.OrderID, &e.CustomerID, &e.AmountCents, &e.IdempotencyKey, &e.JobID, &e.FencingToken, &e.ReceivedAt); err == nil {
				data, _ := json.Marshal(e)
				_, _ = w.Write(append(data, 0x0a))
			}
		}
	}

	// Export suppressed entries
	sRows, err := s.db.Query(`
		SELECT order_id, customer_id, amount_cents, COALESCE(idempotency_key, ''), COALESCE(job_id, ''), COALESCE(fencing_token, 0), strftime('%Y-%m-%dT%H:%M:%SZ', created_at)
		FROM suppressed_entries ORDER BY id ASC
	`)
	if err == nil {
		defer func() { _ = sRows.Close() }()
		for sRows.Next() {
			var e ExportEntry
			e.EntryType = "suppressed"
			if err := sRows.Scan(&e.OrderID, &e.CustomerID, &e.AmountCents, &e.IdempotencyKey, &e.JobID, &e.FencingToken, &e.ReceivedAt); err == nil {
				data, _ := json.Marshal(e)
				_, _ = w.Write(append(data, 0x0a))
			}
		}
	}
}

func (s *LedgerServer) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	s.mu.RLock()
	defer s.mu.RUnlock()

	var appliedCount int64
	var totalAmount int64
	_ = s.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(amount_cents), 0) FROM charges WHERE status = 'applied'").Scan(&appliedCount, &totalAmount)

	var suppressedCount int64
	_ = s.db.QueryRow("SELECT COUNT(*) FROM suppressed_entries").Scan(&suppressedCount)

	_ = json.NewEncoder(w).Encode(map[string]any{
		"applied_count":      appliedCount,
		"suppressed_count":   suppressedCount,
		"total_amount_cents": totalAmount,
	})
}

func (s *LedgerServer) handleAdminFault(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Authenticate admin key
	authHeader := r.Header.Get("Authorization")
	adminKeyHeader := r.Header.Get("X-Admin-Key")
	key := adminKeyHeader
	if key == "" && strings.HasPrefix(authHeader, "Bearer ") {
		key = strings.TrimPrefix(authHeader, "Bearer ")
	}

	if s.adminKey != "" && key != s.adminKey {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "unauthorized", "message": "invalid admin key"},
		})
		return
	}

	var req FaultConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "bad_request", "message": "invalid fault JSON"},
		})
		return
	}

	validModes := map[string]bool{"none": true, "error_rate": true, "latency_ms": true, "down": true, "apply_then_fail": true}
	if !validModes[req.Mode] {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "invalid_mode", "message": "unknown fault mode"},
		})
		return
	}

	s.mu.Lock()
	s.fault = req
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "fault_updated",
		"mode":   req.Mode,
	})
}

func (s *LedgerServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	dbPath := os.Getenv("LEDGER_DB_PATH")
	if dbPath == "" {
		dbPath = "/data/ledger.db"
	}

	// Ensure directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		logger.Error("failed to create database directory", "path", dir, "error", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		logger.Error("failed to open sqlite database", "path", dbPath, "error", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	adminKey := os.Getenv("ADMIN_KEY")
	server, err := NewLedgerServer(db, adminKey, logger)
	if err != nil {
		logger.Error("failed to initialize ledger server", "error", err)
		os.Exit(1)
	}

	port := os.Getenv("LEDGER_PORT")
	if port == "" {
		port = "8082"
	}

	httpServer := &http.Server{
		Addr:    ":" + port,
		Handler: server.Routes(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("ledger service listening", "addr", httpServer.Addr, "db", dbPath)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down ledger service")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
