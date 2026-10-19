package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
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
	Port                 string
	DatabaseURL          string
	DashboardDatabaseURL string
	RedisURL             string
	APIBaseURL           string
	BaselineAPIURL       string
	ChaosBaseURL         string
	LedgerBaseURL        string
	AdminKey             string
	TenantKey            string
	OpsToken             string
	SteadfastEnv         string
	WebDir               string
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
		dbURL = "postgres://steadfast_checker_ro:fake_ro_password_for_local_demo@127.0.0.1:5432/steadfast?sslmode=disable"
	}

	dashDBURL := os.Getenv("DASHBOARD_DATABASE_URL")
	if dashDBURL == "" {
		dashDBURL = "postgres://steadfast_dashboard_ro:fake_dashboard_ro_password@127.0.0.1:5432/steadfast?sslmode=disable"
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

	envMode := os.Getenv("STEADFAST_ENV")
	if envMode == "" {
		envMode = "demo"
	}

	adminKey := os.Getenv("ADMIN_KEY")
	tenantKey := os.Getenv("DEMO_TENANT_A_KEY")
	opsToken := os.Getenv("OPS_TOKEN")

	// Read from .env if empty
	if adminKey == "" || tenantKey == "" || opsToken == "" {
		for _, envPath := range []string{".env", "../.env", "../../.env", "/home/dkvis/steadfast/.env"} {
			if envBytes, err := os.ReadFile(envPath); err == nil {
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
				if opsToken == "" {
					re := regexp.MustCompile(`(?m)^OPS_TOKEN=(.+)$`)
					if matches := re.FindStringSubmatch(envContent); len(matches) > 1 {
						opsToken = strings.TrimSpace(matches[1])
					}
				}
				break
			}
		}
	}
	if opsToken == "" {
		opsToken = "fake_ops_token_for_local_demo_only"
	}

	webDir := os.Getenv("WEB_DIR")
	if webDir == "" {
		webDir = "web/dashboard"
	}

	return Config{
		Port:                 port,
		DatabaseURL:          dbURL,
		DashboardDatabaseURL: dashDBURL,
		RedisURL:             redisURL,
		APIBaseURL:           apiURL,
		BaselineAPIURL:       baselineURL,
		ChaosBaseURL:         chaosURL,
		LedgerBaseURL:        ledgerURL,
		AdminKey:             adminKey,
		TenantKey:            tenantKey,
		OpsToken:             opsToken,
		SteadfastEnv:         envMode,
		WebDir:               webDir,
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

	// Login rate limit (5 attempts / min) and sessions
	loginAttemptsMu sync.Mutex
	loginAttempts   map[string][]time.Time
	sessionsMu      sync.RWMutex
	activeSessions  map[string]time.Time
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
		cfg:            cfg,
		logger:         logger,
		httpClient:     &http.Client{Timeout: 10 * time.Second},
		demoRunner:     demo.NewRunner(runnerCfg, logger),
		lastStTime:     time.Now(),
		lastBlTime:     time.Now(),
		loginAttempts:  make(map[string][]time.Time),
		activeSessions: make(map[string]time.Time),
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// 1. Root redirects to /ops
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ops", http.StatusFound)
	})

	// 2. Demo UI: ONLY served when STEADFAST_ENV=demo (Requirement 1)
	mux.HandleFunc("GET /demo", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.SteadfastEnv != "demo" {
			http.NotFound(w, r)
			return
		}
		s.serveFile(w, r, "index.html", "text/html; charset=utf-8")
	})

	// Static assets
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		s.serveFile(w, r, "style.css", "text/css; charset=utf-8")
	})
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) {
		s.serveFile(w, r, "app.js", "application/javascript; charset=utf-8")
	})

	// Health
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})

	// 3. Ops Authentication & Console (Requirement 9)
	mux.HandleFunc("GET /ops/login", func(w http.ResponseWriter, r *http.Request) {
		s.serveFile(w, r, "login.html", "text/html; charset=utf-8")
	})
	mux.HandleFunc("POST /ops/login", s.handleOpsLogin)
	mux.HandleFunc("GET /ops/logout", s.handleOpsLogout)

	mux.HandleFunc("GET /ops", s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		s.serveFile(w, r, "ops.html", "text/html; charset=utf-8")
	}))

	// 4. Job Detail & Vertical Timeline View (Requirement 8)
	mux.HandleFunc("GET /jobs/{id}", s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		s.serveFile(w, r, "job_detail.html", "text/html; charset=utf-8")
	}))

	// Ops APIs (read-only queries via steadfast_dashboard_ro role)
	mux.HandleFunc("POST /api/ops/jobs/enqueue", s.requireAuth(s.handleOpsEnqueueJob))
	mux.HandleFunc("GET /api/ops/workers", s.requireAuth(s.handleOpsListWorkers))
	mux.HandleFunc("POST /api/ops/workers/{id}/action", s.requireAuth(s.handleOpsWorkerAction))
	mux.HandleFunc("GET /api/ops/jobs", s.requireAuth(s.handleOpsListJobs))
	mux.HandleFunc("GET /api/ops/audit", s.requireAuth(s.handleOpsListAudit))
	mux.HandleFunc("GET /api/ops/jobs/{id}/timeline", s.requireAuth(s.handleOpsJobTimeline))
	mux.HandleFunc("GET /api/ops/jobs/{id}/ledger", s.requireAuth(s.handleOpsJobLedger))

	// SSE stream (accessible for live dashboards)
	mux.HandleFunc("GET /api/stream", s.handleStream)

	// DLQ triage
	mux.HandleFunc("GET /api/dlq", s.handleGetDLQ)
	mux.HandleFunc("POST /api/dlq/replay", s.handleReplayDLQ)

	// Chaos & Demo endpoints: ONLY accessible in demo mode!
	mux.HandleFunc("POST /api/chaos", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.SteadfastEnv != "demo" {
			http.NotFound(w, r)
			return
		}
		s.handleChaos(w, r)
	})
	mux.HandleFunc("POST /api/demo", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.SteadfastEnv != "demo" {
			http.NotFound(w, r)
			return
		}
		s.handleDemo(w, r)
	})
	mux.HandleFunc("POST /api/demo/beat", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.SteadfastEnv != "demo" {
			http.NotFound(w, r)
			return
		}
		s.handleDemoBeat(w, r)
	})

	// Security middleware
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:;")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")

		// Validate Origin for state-changing endpoints
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
			origin := r.Header.Get("Origin")
			if origin != "" && origin != "null" && r.URL.Path != "/ops/login" {
				allowed := strings.HasPrefix(origin, "http://127.0.0.1") ||
					strings.HasPrefix(origin, "https://127.0.0.1") ||
					strings.HasPrefix(origin, "http://localhost") ||
					strings.HasPrefix(origin, "https://localhost") ||
					strings.HasPrefix(origin, "http://[::1]") ||
					strings.HasPrefix(origin, "https://[::1]") ||
					strings.HasPrefix(origin, "http://"+r.Host) ||
					strings.HasPrefix(origin, "https://"+r.Host) ||
					strings.Contains(origin, "localhost") ||
					strings.Contains(origin, "127.0.0.1") ||
					strings.HasPrefix(origin, "http://192.168.") ||
					strings.HasPrefix(origin, "http://10.") ||
					strings.HasPrefix(origin, "http://172.")
				if !allowed {
					s.logger.Warn("forbidden origin rejected", "origin", origin, "host", r.Host, "path", r.URL.Path)
					http.Error(w, `{"error":"forbidden_origin"}`, http.StatusForbidden)
					return
				}
			}
		}

		mux.ServeHTTP(w, r)
	})
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, name, contentType string) {
	fullPath := filepath.Join(s.cfg.WebDir, name)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}

func (s *Server) handleOpsLogin(w http.ResponseWriter, r *http.Request) {
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}

	// Rate limit: 5 attempts per minute
	s.loginAttemptsMu.Lock()
	now := time.Now()
	cutoff := now.Add(-1 * time.Minute)
	validAttempts := []time.Time{}
	for _, t := range s.loginAttempts[ip] {
		if t.After(cutoff) {
			validAttempts = append(validAttempts, t)
		}
	}
	if len(validAttempts) >= 5 {
		s.loginAttemptsMu.Unlock()
		http.Redirect(w, r, "/ops/login?error=rate_limit", http.StatusSeeOther)
		return
	}
	s.loginAttempts[ip] = append(validAttempts, now)
	s.loginAttemptsMu.Unlock()

	token := r.FormValue("ops_token")
	if token == "" {
		token = r.FormValue("token")
	}
	isJSON := strings.Contains(r.Header.Get("Content-Type"), "application/json")
	if token == "" && isJSON {
		var req struct {
			Token    string `json:"token"`
			OpsToken string `json:"ops_token"`
		}
		if bodyBytes, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(bodyBytes, &req)
			if req.OpsToken != "" {
				token = req.OpsToken
			} else if req.Token != "" {
				token = req.Token
			}
		}
	}

	if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.OpsToken)) != 1 {
		if isJSON {
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
		} else {
			http.Redirect(w, r, "/ops/login?error=invalid", http.StatusSeeOther)
		}
		return
	}

	// Generate session ID
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	sessionID := hex.EncodeToString(b)

	s.sessionsMu.Lock()
	s.activeSessions[sessionID] = now.Add(8 * time.Hour)
	s.sessionsMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     "steadfast_ops_session",
		Value:    sessionID,
		Path:     "/",
		MaxAge:   8 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	}

	http.Redirect(w, r, "/ops", http.StatusFound)
}

func (s *Server) handleOpsLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("steadfast_ops_session"); err == nil {
		s.sessionsMu.Lock()
		delete(s.activeSessions, c.Value)
		s.sessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "steadfast_ops_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/ops/login", http.StatusFound)
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("steadfast_ops_session")
		if err != nil || cookie.Value == "" {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			} else {
				http.Redirect(w, r, "/ops/login", http.StatusFound)
			}
			return
		}

		s.sessionsMu.RLock()
		expiry, ok := s.activeSessions[cookie.Value]
		s.sessionsMu.RUnlock()

		if !ok || time.Now().After(expiry) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			} else {
				http.Redirect(w, r, "/ops/login", http.StatusFound)
			}
			return
		}

		next(w, r)
	}
}

// handleOpsEnqueueJob dispatches a real user task to the Steadfast engine
func (s *Server) handleOpsEnqueueJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type                     string         `json:"type"`
		CustomerID               string         `json:"customer_id"`
		AmountDollars            float64        `json:"amount_dollars"`
		OrderID                  string         `json:"order_id"`
		SimulationMode           string         `json:"simulation_mode"`
		SleepSeconds             int            `json:"sleep_seconds"`
		HeartbeatIntervalSeconds int            `json:"heartbeat_interval_seconds"`
		HeartbeatSeconds         int            `json:"heartbeat_seconds"`
		Payload                  map[string]any `json:"payload"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid_json"}`, http.StatusBadRequest)
		return
	}

	if req.Type == "" {
		req.Type = "charge_customer"
	}

	payload := req.Payload
	if payload == nil {
		payload = make(map[string]any)
	}

	if req.CustomerID != "" {
		payload["customer_id"] = req.CustomerID
	}
	if req.OrderID != "" {
		payload["order_id"] = req.OrderID
	}
	if req.AmountDollars > 0 {
		payload["amount_cents"] = int64(req.AmountDollars * 100)
	}
	if req.SimulationMode != "" {
		payload["simulation_mode"] = req.SimulationMode
	}
	if req.SleepSeconds > 0 {
		payload["sleep_seconds"] = req.SleepSeconds
	}
	if req.HeartbeatIntervalSeconds > 0 {
		payload["heartbeat_interval_seconds"] = req.HeartbeatIntervalSeconds
		payload["heartbeat_seconds"] = req.HeartbeatIntervalSeconds
	} else if req.HeartbeatSeconds > 0 {
		payload["heartbeat_interval_seconds"] = req.HeartbeatSeconds
		payload["heartbeat_seconds"] = req.HeartbeatSeconds
	}

	submitBody := map[string]any{
		"type":    req.Type,
		"payload": payload,
	}
	bodyBytes, _ := json.Marshal(submitBody)

	apiURL := fmt.Sprintf("%s/v1/jobs", s.cfg.APIBaseURL)
	apiReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		http.Error(w, `{"error":"request_creation_failed"}`, http.StatusInternalServerError)
		return
	}

	b := make([]byte, 8)
	_, _ = rand.Read(b)
	idemKey := fmt.Sprintf("console-%d-%s", time.Now().UnixNano(), hex.EncodeToString(b))

	apiReq.Header.Set("Authorization", "Bearer "+s.cfg.TenantKey)
	apiReq.Header.Set("Content-Type", "application/json")
	apiReq.Header.Set("Idempotency-Key", idemKey)

	resp, err := s.httpClient.Do(apiReq)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"api_unreachable","details":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBytes)
}

// getWorkerContainerStates queries docker engine API via /var/run/docker.sock or docker ps
func getWorkerContainerStates() map[string]string {
	res := make(map[string]string)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", "/var/run/docker.sock")
		},
	}
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	resp, err := client.Get("http://localhost/containers/json?all=1")
	if err == nil {
		defer resp.Body.Close()
		var items []struct {
			Names []string `json:"Names"`
			State string   `json:"State"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&items); err == nil {
			for _, it := range items {
				for _, n := range it.Names {
					cName := strings.TrimPrefix(n, "/")
					if strings.HasPrefix(cName, "steadfast-worker") {
						res[cName] = it.State
					}
				}
			}
			if len(res) > 0 {
				return res
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "name=steadfast-worker", "--format", "{{.Names}}	{{.State}}")
	out, err := cmd.CombinedOutput()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		for _, l := range lines {
			parts := strings.Split(strings.TrimSpace(l), "	")
			if len(parts) >= 2 {
				res[parts[0]] = parts[1]
			}
		}
	}
	return res
}

func executeDockerAction(ctx context.Context, containerName string, actionArgs ...string) error {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", "/var/run/docker.sock")
		},
	}
	client := &http.Client{Transport: tr, Timeout: 4 * time.Second}

	var endpoint string
	if len(actionArgs) > 0 {
		switch actionArgs[0] {
		case "kill":
			endpoint = fmt.Sprintf("http://localhost/containers/%s/kill?signal=SIGKILL", containerName)
		case "pause":
			endpoint = fmt.Sprintf("http://localhost/containers/%s/pause", containerName)
		case "unpause":
			endpoint = fmt.Sprintf("http://localhost/containers/%s/unpause", containerName)
		case "start":
			endpoint = fmt.Sprintf("http://localhost/containers/%s/start", containerName)
		}
	}

	if endpoint != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
			}
		}
	}

	args := append(actionArgs, containerName)
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker action failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// handleOpsWorkerAction triggers Crash, Freeze, or Resume/Restart on a specific worker container
func (s *Server) handleOpsWorkerAction(w http.ResponseWriter, r *http.Request) {
	workerID := r.PathValue("id")
	if workerID == "" {
		http.Error(w, `{"error":"missing worker id"}`, http.StatusBadRequest)
		return
	}

	var reqBody struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(r.Body).Decode(&reqBody)
	action := reqBody.Action
	if action == "" {
		action = r.URL.Query().Get("action")
	}

	targetContainer := workerID
	for i := 1; i <= 4; i++ {
		if workerID == fmt.Sprintf("worker-%d", i) || workerID == fmt.Sprintf("%d", i) || workerID == fmt.Sprintf("steadfast-worker-%d", i) {
			targetContainer = fmt.Sprintf("steadfast-worker-%d", i)
			break
		}
	}

	if !strings.HasPrefix(targetContainer, "steadfast-worker") {
		http.Error(w, `{"error":"invalid worker target"}`, http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var err error
	switch action {
	case "crash":
		err = executeDockerAction(ctx, targetContainer, "kill", "-s", "SIGKILL")
	case "freeze":
		err = executeDockerAction(ctx, targetContainer, "pause")
	case "resume":
		err = executeDockerAction(ctx, targetContainer, "unpause")
		if err != nil {
			err = executeDockerAction(ctx, targetContainer, "start")
		}
	default:
		http.Error(w, `{"error":"unknown action (supported: crash, freeze, resume)"}`, http.StatusBadRequest)
		return
	}

	if err != nil {
		s.logger.Error("failed worker action", "target", targetContainer, "action", action, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":     err.Error(),
			"worker_id": workerID,
			"action":    action,
		})
		return
	}

	s.logger.Info("worker action executed successfully", "target", targetContainer, "action", action)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"worker_id": workerID,
		"action":    action,
		"container": targetContainer,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// handleOpsListWorkers queries active worker fleet and currently leased jobs
func (s *Server) handleOpsListWorkers(w http.ResponseWriter, r *http.Request) {
	db, err := sql.Open("pgx", s.cfg.DashboardDatabaseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer db.Close()

	// 1. Query currently leased jobs
	leasedRows, err := db.QueryContext(r.Context(), `
		SELECT id, worker_id, fencing_token, lease_expires_at, updated_at
		FROM jobs
		WHERE state = 'LEASED' AND worker_id IS NOT NULL;
	`)

	type LeasedJobInfo struct {
		JobID        string
		FencingToken int64
		ExpiresAt    time.Time
		UpdatedAt    time.Time
	}
	leasedMap := make(map[string]LeasedJobInfo)
	if err == nil {
		defer leasedRows.Close()
		for leasedRows.Next() {
			var jID, wID string
			var fToken int64
			var expAt, updAt time.Time
			if err := leasedRows.Scan(&jID, &wID, &fToken, &expAt, &updAt); err == nil && wID != "" {
				leasedMap[wID] = LeasedJobInfo{
					JobID:        jID,
					FencingToken: fToken,
					ExpiresAt:    expAt,
					UpdatedAt:    updAt,
				}
			}
		}
	}

	type WorkerCard struct {
		WorkerID          string `json:"worker_id"`
		Name              string `json:"name"`
		ContainerName     string `json:"container_name"`
		ContainerState    string `json:"container_state"` // "running", "paused", "exited"
		Status            string `json:"status"`          // "IDLE", "BUSY", "FROZEN", "CRASHED"
		CurrentJobID      string `json:"current_job_id,omitempty"`
		FencingToken      int64  `json:"fencing_token,omitempty"`
		LeaseExpiresInSec int    `json:"lease_expires_in_sec,omitempty"`
		HeartbeatsActive  bool   `json:"heartbeats_active"`
		Concurrency       int    `json:"concurrency"`
		LastSeen          string `json:"last_seen"`
	}

	states := getWorkerContainerStates()
	workersList := make([]WorkerCard, 0, 4)

	for i := 1; i <= 4; i++ {
		wID := fmt.Sprintf("worker-%d", i)
		cName := fmt.Sprintf("steadfast-worker-%d", i)
		cState := states[cName]
		if cState == "" {
			cState = "running"
		}

		card := WorkerCard{
			WorkerID:       wID,
			Name:           fmt.Sprintf("Worker #%d", i),
			ContainerName:  cName,
			ContainerState: cState,
			Concurrency:    4,
			LastSeen:       time.Now().Format(time.RFC3339),
		}

		if cState == "paused" {
			card.Status = "FROZEN"
			card.HeartbeatsActive = false
		} else if cState == "exited" {
			card.Status = "CRASHED"
			card.HeartbeatsActive = false
		} else {
			card.Status = "IDLE"
			card.HeartbeatsActive = true
		}

		for leasedWorker, leased := range leasedMap {
			if leasedWorker == wID || leasedWorker == cName || strings.HasSuffix(wID, strings.TrimPrefix(leasedWorker, "worker-")) {
				card.CurrentJobID = leased.JobID
				card.FencingToken = leased.FencingToken
				rem := int(time.Until(leased.ExpiresAt).Seconds())
				if rem < 0 {
					rem = 0
				}
				card.LeaseExpiresInSec = rem
				if card.Status != "FROZEN" && card.Status != "CRASHED" {
					card.Status = "BUSY"
					card.HeartbeatsActive = true
				}
				break
			}
		}

		workersList = append(workersList, card)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"workers": workersList,
	})
}

// handleOpsListJobs queries jobs via steadfast_dashboard_ro role
func (s *Server) handleOpsListJobs(w http.ResponseWriter, r *http.Request) {
	db, err := sql.Open("pgx", s.cfg.DashboardDatabaseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer db.Close()

	rows, err := db.QueryContext(r.Context(), `
		SELECT j.id, j.job_type, j.state, j.attempts, j.max_attempts, j.fencing_token,
		       COALESCE(NULLIF(j.worker_id, ''), (SELECT worker_id FROM job_attempts ja WHERE ja.job_id = j.id ORDER BY ja.attempt_no DESC LIMIT 1), ''),
		       j.created_at, j.finished_at, COALESCE(j.last_error, '')
		FROM jobs j
		ORDER BY j.created_at DESC
		LIMIT 200;
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type JobRow struct {
		ID           string  `json:"id"`
		JobType      string  `json:"job_type"`
		State        string  `json:"state"`
		Attempts     int     `json:"attempts"`
		MaxAttempts  int     `json:"max_attempts"`
		FencingToken int64   `json:"fencing_token"`
		WorkerID     string  `json:"worker_id"`
		CreatedAt    string  `json:"created_at"`
		FinishedAt   *string `json:"finished_at"`
		LastError    string  `json:"last_error"`
	}

	jobs := []JobRow{}
	for rows.Next() {
		var j JobRow
		var crAt time.Time
		var fnAt *time.Time
		if err := rows.Scan(&j.ID, &j.JobType, &j.State, &j.Attempts, &j.MaxAttempts, &j.FencingToken, &j.WorkerID, &crAt, &fnAt, &j.LastError); err == nil {
			j.CreatedAt = crAt.Format(time.RFC3339)
			if fnAt != nil {
				formatted := fnAt.Format(time.RFC3339)
				j.FinishedAt = &formatted
			}
			jobs = append(jobs, j)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jobs": jobs})
}

// handleOpsListAudit queries audit_log via steadfast_dashboard_ro role
func (s *Server) handleOpsListAudit(w http.ResponseWriter, r *http.Request) {
	db, err := sql.Open("pgx", s.cfg.DashboardDatabaseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer db.Close()

	rows, err := db.QueryContext(r.Context(), `
		SELECT ts, actor, action, COALESCE(job_id::text, ''), COALESCE(from_state, ''), COALESCE(to_state, ''), details
		FROM audit_log
		ORDER BY id DESC
		LIMIT 50;
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type AuditRow struct {
		TS        string         `json:"ts"`
		Actor     string         `json:"actor"`
		Action    string         `json:"action"`
		JobID     string         `json:"job_id"`
		FromState string         `json:"from_state"`
		ToState   string         `json:"to_state"`
		Details   map[string]any `json:"details"`
	}

	events := []AuditRow{}
	for rows.Next() {
		var a AuditRow
		var ts time.Time
		var rawDetails []byte
		if err := rows.Scan(&ts, &a.Actor, &a.Action, &a.JobID, &a.FromState, &a.ToState, &rawDetails); err == nil {
			a.TS = ts.Format(time.RFC3339)
			if len(rawDetails) > 0 {
				_ = json.Unmarshal(rawDetails, &a.Details)
			}
			events = append(events, a)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(events)
}

func (s *Server) handleOpsJobTimeline(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if jobID == "" {
		http.Error(w, `{"error":"missing_job_id"}`, http.StatusBadRequest)
		return
	}

	db, err := sql.Open("pgx", s.cfg.DashboardDatabaseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer db.Close()

	// 1. Query job metadata via steadfast_dashboard_ro
	var (
		id           string
		tenantID     string
		jobType      string
		state        string
		attempts     int
		maxAttempts  int
		fencingToken int64
		workerID     sql.NullString
		payloadRaw   []byte
		createdAt    time.Time
		finishedAt   *time.Time
		lastError    sql.NullString
	)

		var (
			leaseExpiresAt *time.Time
			updatedAt      time.Time
		)
		err = db.QueryRowContext(r.Context(), `
			SELECT id, tenant_id, job_type, state, attempts, max_attempts, fencing_token, worker_id, payload, created_at, finished_at, last_error, lease_expires_at, updated_at
			FROM jobs
			WHERE id = $1;
		`, jobID).Scan(&id, &tenantID, &jobType, &state, &attempts, &maxAttempts, &fencingToken, &workerID, &payloadRaw, &createdAt, &finishedAt, &lastError, &leaseExpiresAt, &updatedAt)

	if err != nil {
		http.Error(w, `{"error":"job_not_found"}`, http.StatusNotFound)
		return
	}

	var payload any
	if len(payloadRaw) > 0 {
		_ = json.Unmarshal(payloadRaw, &payload)
	}

	var fnAtStr *string
	if finishedAt != nil {
		formatted := finishedAt.Format(time.RFC3339)
		fnAtStr = &formatted
	}

	var leaseExpStr *string
	remSec := 0
	if leaseExpiresAt != nil {
		s := leaseExpiresAt.Format(time.RFC3339)
		leaseExpStr = &s
		diff := int(time.Until(*leaseExpiresAt).Seconds())
		if diff > 0 {
			remSec = diff
		}
	}

	jobMeta := map[string]any{
		"id":                   id,
		"tenant_id":            tenantID,
		"job_type":             jobType,
		"state":                state,
		"attempts":             attempts,
		"max_attempts":         maxAttempts,
		"fencing_token":        fencingToken,
		"worker_id":            workerID.String,
		"payload":              payload,
		"created_at":           createdAt.Format(time.RFC3339),
		"finished_at":          fnAtStr,
		"last_error":           lastError.String,
		"lease_expires_at":     leaseExpStr,
		"lease_expires_in_sec": remSec,
		"updated_at":           updatedAt.Format(time.RFC3339),
	}

	// 2. Query attempts via steadfast_dashboard_ro
	attRows, err := db.QueryContext(r.Context(), `
		SELECT attempt_no, COALESCE(worker_id, ''), fencing_token, started_at, finished_at, outcome, COALESCE(error_message, '')
		FROM job_attempts
		WHERE job_id = $1
		ORDER BY attempt_no ASC;
	`, jobID)

	type AttemptMeta struct {
		AttemptNo    int     `json:"attempt_no"`
		WorkerID     string  `json:"worker_id"`
		FencingToken int64   `json:"fencing_token"`
		StartedAt    string  `json:"started_at"`
		FinishedAt   *string `json:"finished_at"`
		Outcome      string  `json:"outcome"`
		ErrorMessage string  `json:"error_message,omitempty"`
	}

	attemptsList := []AttemptMeta{}
	if err == nil {
		defer attRows.Close()
		for attRows.Next() {
			var att AttemptMeta
			var stAt time.Time
			var fnAt *time.Time
			if err := attRows.Scan(&att.AttemptNo, &att.WorkerID, &att.FencingToken, &stAt, &fnAt, &att.Outcome, &att.ErrorMessage); err == nil {
				att.StartedAt = stAt.Format(time.RFC3339)
				if fnAt != nil {
					f := fnAt.Format(time.RFC3339)
					att.FinishedAt = &f
				}
				attemptsList = append(attemptsList, att)
			}
		}
	}

	// 3. Query audit log events via steadfast_dashboard_ro
	evRows, err := db.QueryContext(r.Context(), `
		SELECT ts, actor, action, COALESCE(from_state, ''), COALESCE(to_state, ''), details
		FROM audit_log
		WHERE job_id = $1
		ORDER BY id DESC;
	`, jobID)

	type EventMeta struct {
		Timestamp string `json:"timestamp"`
		Actor     string `json:"actor"`
		Action    string `json:"action"`
		FromState string `json:"from_state,omitempty"`
		ToState   string `json:"to_state,omitempty"`
		Details   any    `json:"details,omitempty"`
	}

	eventsList := []EventMeta{}
	if err == nil {
		defer evRows.Close()
		for evRows.Next() {
			var ev EventMeta
			var ts time.Time
			var detailsRaw []byte
			if err := evRows.Scan(&ts, &ev.Actor, &ev.Action, &ev.FromState, &ev.ToState, &detailsRaw); err == nil {
				ev.Timestamp = ts.Format(time.RFC3339)
				if len(detailsRaw) > 0 {
					_ = json.Unmarshal(detailsRaw, &ev.Details)
				}
				eventsList = append(eventsList, ev)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"job": jobMeta,
		"timeline": map[string]any{
			"attempts": attemptsList,
			"events":   eventsList,
		},
	})
}

func (s *Server) handleOpsJobLedger(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	lURL := fmt.Sprintf("%s/ledger/entries?job_id=%s", s.cfg.LedgerBaseURL, jobID)
	lReq, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, lURL, nil)
	if s.cfg.AdminKey != "" {
		lReq.Header.Set("X-Admin-Key", s.cfg.AdminKey)
	}

	lResp, err := s.httpClient.Do(lReq)
	if err != nil {
		http.Error(w, `{"error":"ledger_unavailable"}`, http.StatusBadGateway)
		return
	}
	defer lResp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(lResp.StatusCode)
	_, _ = io.Copy(w, lResp.Body)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
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

	// 1. Collect Steadfast Metrics
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

			_ = db.QueryRowContext(ctxTimeout, "SELECT count(*) FROM jobs WHERE state = 'QUEUED'").Scan(&snap.Steadfast.QueueDepth)
			_ = db.QueryRowContext(ctxTimeout, "SELECT count(*) FROM jobs WHERE state = 'LEASED'").Scan(&snap.Steadfast.InFlight)
			_ = db.QueryRowContext(ctxTimeout, "SELECT count(*) FROM jobs WHERE state = 'SUCCEEDED'").Scan(&snap.Steadfast.Succeeded)
			_ = db.QueryRowContext(ctxTimeout, "SELECT count(*) FROM jobs WHERE state = 'DEAD'").Scan(&snap.Steadfast.DeadCount)
			_ = db.QueryRowContext(ctxTimeout, "SELECT count(DISTINCT worker_id) FROM jobs WHERE state = 'LEASED' AND worker_id IS NOT NULL").Scan(&snap.Steadfast.WorkerCount)

			_ = db.QueryRowContext(ctxTimeout, `
				SELECT 
					COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch from (finished_at - created_at)) * 1000)::bigint, 0),
					COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY extract(epoch from (finished_at - created_at)) * 1000)::bigint, 0)
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

	// 3. Independent Stakes & Verdict
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

	var parsed any
	if err := json.Unmarshal([]byte(body.Payload), &parsed); err != nil {
		http.Error(w, `{"error":"malformed_payload_json"}`, http.StatusBadRequest)
		return
	}

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

	addr := ":" + cfg.Port
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      server.Routes(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("Starting Steadfast Dashboard server", "addr", addr, "env", cfg.SteadfastEnv)
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
