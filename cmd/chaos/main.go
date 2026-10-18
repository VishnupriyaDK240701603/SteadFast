package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"steadfast/internal/chaos"
)

type ChaosServer struct {
	controller *chaos.Controller
	adminKey   string
	logger     *slog.Logger
}

func NewChaosServer(controller *chaos.Controller, adminKey string, logger *slog.Logger) *ChaosServer {
	return &ChaosServer{
		controller: controller,
		adminKey:   adminKey,
		logger:     logger,
	}
}

func (s *ChaosServer) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   "unauthorized",
			"message": "missing or malformed authorization header",
		})
		return false
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.adminKey)) != 1 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   "unauthorized",
			"message": "invalid admin key",
		})
		return false
	}
	return true
}

func (s *ChaosServer) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // Direct CLI/API invocation allowed
	}

	u, err := url.Parse(origin)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   "forbidden",
			"message": "cross-origin request rejected",
		})
		return false
	}
	return true
}

func (s *ChaosServer) SetupRoutes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	handlerWrap := func(fn func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !s.checkAuth(w, r) {
				return
			}
			if !s.checkOrigin(w, r) {
				return
			}
			fn(w, r)
		}
	}

	mux.HandleFunc("POST /chaos/kill-workers", handlerWrap(s.handleKillWorkers))
	mux.HandleFunc("POST /chaos/kill-postgres", handlerWrap(s.handleKillPostgres))
	mux.HandleFunc("POST /chaos/start-postgres", handlerWrap(s.handleStartPostgres))
	mux.HandleFunc("POST /chaos/kill-redis", handlerWrap(s.handleKillRedis))
	mux.HandleFunc("POST /chaos/start-redis", handlerWrap(s.handleStartRedis))
	mux.HandleFunc("POST /chaos/ledger-fault", handlerWrap(s.handleLedgerFault))
	mux.HandleFunc("POST /chaos/clear-faults", handlerWrap(s.handleClearFaults))

	return mux
}

func (s *ChaosServer) handleKillWorkers(w http.ResponseWriter, r *http.Request) {
	fraction := 0.5
	if fStr := r.URL.Query().Get("fraction"); fStr != "" {
		if f, err := strconv.ParseFloat(fStr, 64); err == nil {
			fraction = f
		}
	}

	killed, err := s.controller.KillWorkers(r.Context(), fraction)
	if err != nil {
		s.logger.Error("failed to kill workers", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.logger.Info("chaos action executed", "action", "kill-workers", "fraction", fraction, "killed", killed)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"action":    "kill-workers",
		"killed":    killed,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *ChaosServer) handleKillPostgres(w http.ResponseWriter, r *http.Request) {
	err := s.controller.KillPostgres(r.Context())
	if err != nil {
		s.logger.Error("failed to kill postgres", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.logger.Info("chaos action executed", "action", "kill-postgres")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"action":    "kill-postgres",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *ChaosServer) handleStartPostgres(w http.ResponseWriter, r *http.Request) {
	err := s.controller.StartPostgres(r.Context())
	if err != nil {
		s.logger.Error("failed to start postgres", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.logger.Info("chaos action executed", "action", "start-postgres")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"action":    "start-postgres",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *ChaosServer) handleKillRedis(w http.ResponseWriter, r *http.Request) {
	err := s.controller.KillRedis(r.Context())
	if err != nil {
		s.logger.Error("failed to kill redis", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.logger.Info("chaos action executed", "action", "kill-redis")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"action":    "kill-redis",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *ChaosServer) handleStartRedis(w http.ResponseWriter, r *http.Request) {
	err := s.controller.StartRedis(r.Context())
	if err != nil {
		s.logger.Error("failed to start redis", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.logger.Info("chaos action executed", "action", "start-redis")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"action":    "start-redis",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *ChaosServer) handleLedgerFault(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	var errorRate float64
	var latencyMs int

	if r.Header.Get("Content-Type") == "application/json" {
		var req struct {
			Mode      string  `json:"mode"`
			ErrorRate float64 `json:"error_rate"`
			LatencyMs int     `json:"latency_ms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			if req.Mode != "" {
				mode = req.Mode
			}
			errorRate = req.ErrorRate
			latencyMs = req.LatencyMs
		}
	}

	if mode == "" {
		http.Error(w, "missing mode parameter", http.StatusBadRequest)
		return
	}

	err := s.controller.SetLedgerFault(r.Context(), mode, errorRate, latencyMs)
	if err != nil {
		s.logger.Error("failed to set ledger fault", "mode", mode, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.logger.Info("chaos action executed", "action", "ledger-fault", "mode", mode)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"action":    "ledger-fault",
		"mode":      mode,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *ChaosServer) handleClearFaults(w http.ResponseWriter, r *http.Request) {
	err := s.controller.ClearLedgerFaults(r.Context())
	if err != nil {
		s.logger.Error("failed to clear ledger faults", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.logger.Info("chaos action executed", "action", "clear-faults")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"action":    "clear-faults",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	env := os.Getenv("STEADFAST_ENV")
	if env != "demo" {
		logger.Error("chaos service refused to start: STEADFAST_ENV must be 'demo'", "env", env)
		os.Exit(1)
	}

	adminKey := os.Getenv("ADMIN_KEY")
	if adminKey == "" {
		adminKey = "fake_admin_key_for_local_demo_only_32_bytes_min"
	}

	ledgerURL := os.Getenv("LEDGER_BASE_URL")
	if ledgerURL == "" {
		ledgerURL = "http://127.0.0.1:8082"
	}

	ctrl, err := chaos.NewController(chaos.Config{
		Env:           env,
		AdminKey:      adminKey,
		LedgerBaseURL: ledgerURL,
	})
	if err != nil {
		logger.Error("failed to create chaos controller", "error", err)
		os.Exit(1)
	}

	server := NewChaosServer(ctrl, adminKey, logger)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8083"
	}

	addr := fmt.Sprintf("127.0.0.1:%s", port)
	logger.Info("starting chaos service", "addr", addr)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           server.SetupRoutes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("chaos service error", "error", err)
		os.Exit(1)
	}
}
