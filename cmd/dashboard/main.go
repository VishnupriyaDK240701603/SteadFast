package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"steadfast/internal/demo"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

type Config struct {
	Port           string
	DatabaseURL    string
	RedisURL       string
	APIBaseURL     string
	BaselineAPIURL string
	ChaosBaseURL   string
	LedgerBaseURL  string
	AdminKey       string
	TenantKey      string
	WebDir         string
}

func loadConfig() Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8085"
	}
	dbURL := os.Getenv("CHECKER_DATABASE_URL")
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		dbURL = "postgres://steadfast_checker_ro:checker_password@127.0.0.1:5432/steadfast?sslmode=disable"
	}
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://127.0.0.1:6379"
	}
	apiURL := os.Getenv("API_BASE_URL")
	if apiURL == "" {
		apiURL = "http://127.0.0.1:8080"
	}
	baselineURL := os.Getenv("BASELINE_API_URL")
	if baselineURL == "" {
		baselineURL = "http://127.0.0.1:8081"
	}
	chaosURL := os.Getenv("CHAOS_BASE_URL")
	if chaosURL == "" {
		chaosURL = "http://127.0.0.1:8083"
	}
	ledgerURL := os.Getenv("LEDGER_BASE_URL")
	if ledgerURL == "" {
		ledgerURL = "http://127.0.0.1:8082"
	}

	adminKey := os.Getenv("ADMIN_KEY")
	tenantKey := os.Getenv("DEMO_TENANT_A_KEY")
	if tenantKey == "" {
		tenantKey = os.Getenv("STEADFAST_API_KEY")
	}

	// Try reading from .env if keys empty
	if adminKey == "" || tenantKey == "" {
		envBytes, err := os.ReadFile(".env")
		if err == nil {
			envContent := string(envBytes)
			if adminKey == "" {
				re := regexp.MustCompile(`(?m)^ADMIN_KEY=(.+)$`)
				if matches := re.FindStringSubmatch(envContent); len(matches) > 1 {
					adminKey = strings.TrimSpace(matches[1])
				}
			}
			if tenantKey == "" {
				re := regexp.MustCompile(`(?m)^DEMO_TENANT_A_KEY=(.+)$`)
				if matches := re.FindStringSubmatch(envContent); len(matches) > 1 {
					tenantKey = strings.TrimSpace(matches[1])
				}
			}
		}
	}

	webDir := os.Getenv("WEB_DIR")
	if webDir == "" {
		webDir = "web/dashboard"
	}

	return Config{
		Port:           port,
		DatabaseURL:    dbURL,
		RedisURL:       redisURL,
		APIBaseURL:     apiURL,
		BaselineAPIURL: baselineURL,
		ChaosBaseURL:   chaosURL,
		LedgerBaseURL:  ledgerURL,
		AdminKey:       adminKey,
		TenantKey:      tenantKey,
		WebDir:         webDir,
	}
}

type Server struct {
	cfg        Config
	logger     *slog.Logger
	httpClient *http.Client
	demoRunner *demo.Runner
	mu         sync.Mutex

	lastStSucceeded int64
	lastStTime      time.Time
	lastBlCompleted int64
	lastBlTime      time.Time
}

func NewServer(cfg Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	runnerCfg := demo.RunnerConfig{
		APIBaseURL:     cfg.APIBaseURL,
		BaselineAPIURL: cfg.BaselineAPIURL,
		ChaosBaseURL:   cfg.ChaosBaseURL,
		LedgerBaseURL:  cfg.LedgerBaseURL,
		DatabaseURL:    cfg.DatabaseURL,
		TenantKey:      cfg.TenantKey,
		AdminKey:       cfg.AdminKey,
	}
	return &Server{
		cfg:        cfg,
		logger:     logger,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		demoRunner: demo.NewRunner(runnerCfg, logger),
		lastStTime: time.Now(),
		lastBlTime: time.Now(),
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Static assets
	mux.HandleFunc("GET /", s.serveStatic)
	mux.HandleFunc("GET /style.css", s.serveStatic)
	mux.HandleFunc("GET /app.js", s.serveStatic)

	// Health
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})

	// SSE stream
	mux.HandleFunc("GET /api/stream", s.handleStream)

	// DLQ triage
	mux.HandleFunc("GET /api/dlq", s.handleGetDLQ)
	mux.HandleFunc("POST /api/dlq/replay", s.handleReplayDLQ)

	// Chaos & Demo
	mux.HandleFunc("POST /api/chaos", s.handleChaos)
	mux.HandleFunc("POST /api/demo", s.handleDemo)
	mux.HandleFunc("POST /api/demo/beat", s.handleDemoBeat)

	// Security middleware
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strict Security Headers
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; img-src 'self' data:;")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")

		// Validate Origin for state-changing endpoints
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
			origin := r.Header.Get("Origin")
			if origin != "" {
				if !strings.HasPrefix(origin, "http://127.0.0.1") && !strings.HasPrefix(origin, "http://localhost") {
					http.Error(w, `{"error":"forbidden_origin"}`, http.StatusForbidden)
					return
				}
			}
		}

		mux.ServeHTTP(w, r)
	})
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	relPath := strings.TrimPrefix(r.URL.Path, "/")
	if relPath == "" || relPath == "index.html" {
		relPath = "index.html"
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	} else if strings.HasSuffix(relPath, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else if strings.HasSuffix(relPath, ".js") {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	}

	fullPath := filepath.Join(s.cfg.WebDir, relPath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(data)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			snapshot := s.collectSnapshot(r.Context())
			data, err := json.Marshal(snapshot)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

type DashboardSnapshot struct {
	Steadfast SteadfastStats `json:"steadfast"`
	Baseline  BaselineStats  `json:"baseline"`
	Stakes    StakesStats    `json:"stakes"`
	Verdict   any            `json:"verdict,omitempty"`
}

type SteadfastStats struct {
	DBDown       bool    `json:"db_down"`
	QueueDepth   int64   `json:"queue_depth"`
	InFlight     int64   `json:"in_flight"`
	Succeeded    int64   `json:"succeeded"`
	DeadCount    int64   `json:"dead_count"`
	WorkerCount  int64   `json:"worker_count"`
	Throughput   float64 `json:"throughput"`
	P50Ms        int64   `json:"p50_ms"`
	P99Ms        int64   `json:"p99_ms"`
	BreakerState string  `json:"breaker_state"`
}

type BaselineStats struct {
	RedisDown  bool    `json:"redis_down"`
	QueueDepth int64   `json:"queue_depth"`
	Processing int64   `json:"processing"`
	Completed  int64   `json:"completed"`
	Failed     int64   `json:"failed"`
	Throughput float64 `json:"throughput"`
}

type StakesStats struct {
	SteadfastDoubleCharges int64   `json:"steadfast_double_charges"`
	SteadfastOrdersLost    int64   `json:"steadfast_orders_lost"`
	SteadfastDollarsLost   float64 `json:"steadfast_dollars_lost"`
	BaselineDoubleCharges  int64   `json:"baseline_double_charges"`
	BaselineOrdersLost     int64   `json:"baseline_orders_lost"`
	BaselineDollarsLost    float64 `json:"baseline_dollars_lost"`
}

func (s *Server) collectSnapshot(ctx context.Context) DashboardSnapshot {
	var snap DashboardSnapshot
	snap.Steadfast.BreakerState = "CLOSED"

	// 1. Collect Steadfast Metrics via Checker Read-Only Role
	db, err := sql.Open("pgx", s.cfg.DatabaseURL)
	if err != nil {
		snap.Steadfast.DBDown = true
	} else {
		defer func() { _ = db.Close() }()
		ctxTimeout, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
		defer cancel()

		if err := db.PingContext(ctxTimeout); err != nil {
			snap.Steadfast.DBDown = true
		} else {
			snap.Steadfast.DBDown = false

			_ = db.QueryRowContext(ctxTimeout, "SELECT COUNT(*) FROM jobs WHERE state = 'QUEUED'").Scan(&snap.Steadfast.QueueDepth)
			_ = db.QueryRowContext(ctxTimeout, "SELECT COUNT(*) FROM jobs WHERE state = 'LEASED'").Scan(&snap.Steadfast.InFlight)
			_ = db.QueryRowContext(ctxTimeout, "SELECT COUNT(*) FROM jobs WHERE state = 'SUCCEEDED'").Scan(&snap.Steadfast.Succeeded)
			_ = db.QueryRowContext(ctxTimeout, "SELECT COUNT(*) FROM jobs WHERE state = 'DEAD'").Scan(&snap.Steadfast.DeadCount)

			_ = db.QueryRowContext(ctxTimeout, "SELECT state FROM circuit_breakers ORDER BY updated_at DESC LIMIT 1").Scan(&snap.Steadfast.BreakerState)
			if snap.Steadfast.BreakerState == "" {
				snap.Steadfast.BreakerState = "CLOSED"
			}

			_ = db.QueryRowContext(ctxTimeout, `
				SELECT COUNT(DISTINCT worker_id) FROM job_attempts 
				WHERE started_at > now() - interval '30 seconds'
			`).Scan(&snap.Steadfast.WorkerCount)
			if snap.Steadfast.WorkerCount == 0 && snap.Steadfast.InFlight > 0 {
				snap.Steadfast.WorkerCount = 4
			}

			_ = db.QueryRowContext(ctxTimeout, `
				SELECT 
					COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (finished_at - created_at))*1000), 0)::bigint,
					COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (finished_at - created_at))*1000), 0)::bigint
				FROM jobs 
				WHERE state = 'SUCCEEDED' AND finished_at > now() - interval '30 seconds'
			`).Scan(&snap.Steadfast.P50Ms, &snap.Steadfast.P99Ms)

			// Throughput
			s.mu.Lock()
			now := time.Now()
			elapsed := now.Sub(s.lastStTime).Seconds()
			if elapsed >= 0.8 {
				diff := snap.Steadfast.Succeeded - s.lastStSucceeded
				if diff >= 0 {
					snap.Steadfast.Throughput = float64(diff) / elapsed
				}
				s.lastStSucceeded = snap.Steadfast.Succeeded
				s.lastStTime = now
			}
			s.mu.Unlock()
		}
	}

	// 2. Collect Baseline Metrics from Redis
	opt, err := redis.ParseURL(s.cfg.RedisURL)
	if err != nil {
		snap.Baseline.RedisDown = true
	} else {
		rdb := redis.NewClient(opt)
		defer func() { _ = rdb.Close() }()

		ctxTimeout, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
		defer cancel()

		if err := rdb.Ping(ctxTimeout).Err(); err != nil {
			snap.Baseline.RedisDown = true
		} else {
			snap.Baseline.RedisDown = false
			snap.Baseline.QueueDepth, _ = rdb.LLen(ctxTimeout, "baseline:jobs").Result()
			snap.Baseline.Processing, _ = rdb.ZCard(ctxTimeout, "baseline:processing").Result()
			snap.Baseline.Failed, _ = rdb.LLen(ctxTimeout, "baseline:dead").Result()

			compStr, err := rdb.Get(ctxTimeout, "baseline:completed_count").Result()
			if err == nil {
				snap.Baseline.Completed, _ = strconv.ParseInt(compStr, 10, 64)
			}

			// Throughput
			s.mu.Lock()
			now := time.Now()
			elapsed := now.Sub(s.lastBlTime).Seconds()
			if elapsed >= 0.8 {
				diff := snap.Baseline.Completed - s.lastBlCompleted
				if diff >= 0 {
					snap.Baseline.Throughput = float64(diff) / elapsed
				}
				s.lastBlCompleted = snap.Baseline.Completed
				s.lastBlTime = now
			}
			s.mu.Unlock()
		}
	}

	// 3. Independent Stakes & Verdict (from artifacts/verdict.json)
	verdictBytes, err := os.ReadFile("artifacts/verdict.json")
	if err == nil {
		var vMap map[string]any
		if err := json.Unmarshal(verdictBytes, &vMap); err == nil {
			snap.Verdict = vMap

			if bl, ok := vMap["baseline"].(map[string]any); ok {
				if dupOrders, ok := bl["duplicate_orders_count"].(float64); ok {
					snap.Stakes.BaselineDoubleCharges = int64(dupOrders)
				}
				if lost, ok := bl["orders_lost_count"].(float64); ok {
					snap.Stakes.BaselineOrdersLost = int64(lost)
				}
				if dollars, ok := bl["duplicate_dollars"].(float64); ok {
					snap.Stakes.BaselineDollarsLost = dollars
				}
			}
			if st, ok := vMap["steadfast"].(map[string]any); ok {
				if dupCharges, ok := st["duplicate_charges_count"].(float64); ok {
					snap.Stakes.SteadfastDoubleCharges = int64(dupCharges)
				}
			}
		}
	}

	return snap
}

func (s *Server) handleGetDLQ(w http.ResponseWriter, r *http.Request) {
	dlqURL := fmt.Sprintf("%s/v1/dlq?limit=50", s.cfg.APIBaseURL)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, dlqURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.TenantKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		http.Error(w, "failed to query DLQ: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	var listResp struct {
		Jobs []struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			Payload   any    `json:"payload"`
			Attempts  int    `json:"attempts"`
			CreatedAt string `json:"created_at"`
			UpdatedAt string `json:"updated_at"`
		} `json:"jobs"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		http.Error(w, "invalid DLQ response: "+err.Error(), http.StatusBadGateway)
		return
	}

	type DLQJobView struct {
		ID              string `json:"id"`
		JobType         string `json:"job_type"`
		Payload         string `json:"payload"`
		Attempts        int    `json:"attempts"`
		CreatedAt       string `json:"created_at"`
		UpdatedAt       string `json:"updated_at"`
		AttemptsHistory []any  `json:"attempts_history"`
	}

	views := make([]DLQJobView, 0, len(listResp.Jobs))
	for _, job := range listResp.Jobs {
		view := DLQJobView{
			ID:        job.ID,
			JobType:   job.Type,
			Attempts:  job.Attempts,
			CreatedAt: job.CreatedAt,
			UpdatedAt: job.UpdatedAt,
		}

		pBytes, _ := json.Marshal(job.Payload)
		view.Payload = string(pBytes)

		// Fetch attempts
		jobURL := fmt.Sprintf("%s/v1/jobs/%s", s.cfg.APIBaseURL, job.ID)
		jReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, jobURL, nil)
		if err == nil {
			jReq.Header.Set("Authorization", "Bearer "+s.cfg.TenantKey)
			if jResp, err := s.httpClient.Do(jReq); err == nil {
				var detailed struct {
					JobAttempts []struct {
						AttemptNo    int    `json:"attempt_no"`
						StartedAt    string `json:"started_at"`
						ErrorMessage string `json:"error_message"`
					} `json:"job_attempts"`
				}
				if err := json.NewDecoder(jResp.Body).Decode(&detailed); err == nil {
					for _, att := range detailed.JobAttempts {
						view.AttemptsHistory = append(view.AttemptsHistory, map[string]any{
							"attempt":   att.AttemptNo,
							"timestamp": att.StartedAt,
							"error":     att.ErrorMessage,
						})
					}
				}
				_ = jResp.Body.Close()
			}
		}

		views = append(views, view)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(views)
}

func (s *Server) handleReplayDLQ(w http.ResponseWriter, r *http.Request) {
	var body struct {
		JobID   string `json:"job_id"`
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid_json"}`, http.StatusBadRequest)
		return
	}

	// Validate JSON
	var parsed any
	if err := json.Unmarshal([]byte(body.Payload), &parsed); err != nil {
		http.Error(w, `{"error":"malformed_payload_json"}`, http.StatusBadRequest)
		return
	}

	// 1. PATCH /v1/dlq/{id}
	patchURL := fmt.Sprintf("%s/v1/dlq/%s", s.cfg.APIBaseURL, body.JobID)
	patchBody, _ := json.Marshal(map[string]any{"payload": parsed})
	pReq, err := http.NewRequestWithContext(r.Context(), http.MethodPatch, patchURL, bytes.NewReader(patchBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pReq.Header.Set("Authorization", "Bearer "+s.cfg.TenantKey)
	pReq.Header.Set("Content-Type", "application/json")

	pResp, err := s.httpClient.Do(pReq)
	if err != nil {
		http.Error(w, "failed to update payload: "+err.Error(), http.StatusBadGateway)
		return
	}
	_ = pResp.Body.Close()

	// 2. POST /v1/dlq/{id}/replay
	replayURL := fmt.Sprintf("%s/v1/dlq/%s/replay", s.cfg.APIBaseURL, body.JobID)
	rReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, replayURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rReq.Header.Set("Authorization", "Bearer "+s.cfg.TenantKey)

	rResp, err := s.httpClient.Do(rReq)
	if err != nil {
		http.Error(w, "failed to replay: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = rResp.Body.Close() }()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(rResp.StatusCode)
	_, _ = io.Copy(w, rResp.Body)
}

func (s *Server) handleChaos(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	if action == "" {
		var req struct {
			Action string `json:"action"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		action = req.Action
	}

	if action == "" {
		http.Error(w, `{"error":"missing_action"}`, http.StatusBadRequest)
		return
	}

	chaosURL := fmt.Sprintf("%s/action", s.cfg.ChaosBaseURL)
	payload, _ := json.Marshal(map[string]any{"action": action})

	cReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, chaosURL, bytes.NewReader(payload))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cReq.Header.Set("Content-Type", "application/json")
	if s.cfg.AdminKey != "" {
		cReq.Header.Set("Authorization", "Bearer "+s.cfg.AdminKey)
	}

	cResp, err := s.httpClient.Do(cReq)
	if err != nil {
		http.Error(w, "chaos request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = cResp.Body.Close() }()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(cResp.StatusCode)
	_, _ = io.Copy(w, cResp.Body)
}

func (s *Server) handleDemo(w http.ResponseWriter, r *http.Request) {
	profile := r.URL.Query().Get("profile")
	if profile == "" {
		profile = "short"
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.demoRunner.RunDemo(ctx, profile); err != nil {
			s.logger.Error("Demo execution error", "error", err)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"status":"started","profile":%q}`, profile)
}

func (s *Server) handleDemoBeat(w http.ResponseWriter, r *http.Request) {
	beatStr := r.URL.Query().Get("beat")
	beat, err := strconv.Atoi(beatStr)
	if err != nil || beat < 1 || beat > 4 {
		http.Error(w, `{"error":"invalid_beat"}`, http.StatusBadRequest)
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.demoRunner.ExecuteBeat(ctx, beat, "short"); err != nil {
			s.logger.Error("Demo beat execution error", "beat", beat, "error", err)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"status":"triggered","beat":%d}`, beat)
}

func main() {
	cfg := loadConfig()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	server := NewServer(cfg, logger)

	addr := net.JoinHostPort("127.0.0.1", cfg.Port)
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      server.Routes(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("Starting Steadfast Dashboard server", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Dashboard server error", "error", err)
			os.Exit(1)
		}
	}()

	<-stop
	logger.Info("Shutting down Dashboard server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
}
