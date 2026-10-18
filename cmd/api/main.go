package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"steadfast/internal/auth"
	"steadfast/internal/config"
	"steadfast/internal/store"
)

type contextKey string

const (
	requestIDKey contextKey = "steadfast.request_id"
)

func generateRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = generateRequestID()
		}
		w.Header().Set("X-Request-ID", reqID)
		ctx := context.WithValue(r.Context(), requestIDKey, reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func LoggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			reqID, _ := r.Context().Value(requestIDKey).(string)

			// Security Rule: Never log Authorization header, tokens, or query parameters!
			logger.Info("http_request_started",
				slog.String("request_id", reqID),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("remote_addr", r.RemoteAddr),
			)

			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)

			duration := time.Since(start)
			logger.Info("http_request_completed",
				slog.String("request_id", reqID),
				slog.Int("status", rw.status),
				slog.Duration("duration_ms", duration),
			)
		})
	}
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func DecodeStrictJSON(r io.Reader, dst any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("extra data after JSON payload")
	}
	return nil
}

func SetupRouter(logger *slog.Logger, pool *pgxpool.Pool, s *store.Store) http.Handler {
	mux := http.NewServeMux()

	// Liveness Probe: always 200 if process is running
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Readiness Probe: checks database ping (returns 503 if unreachable)
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if pool == nil || pool.Ping(ctx) != nil {
			auth.WriteJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "database unreachable")
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})

	// Tenant-authenticated test route (to be expanded in Prompt 04)
	if s != nil {
		tenantAuth := auth.RequireTenantAuth(s)
		mux.Handle("GET /v1/tenant/me", tenantAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t, ok := auth.TenantFromContext(r.Context())
			if !ok || t == nil {
				auth.WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing authentication token")
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tenant_id": t.ID.String(),
				"name":      t.Name,
			})
		})))
	}

	handler := RequestIDMiddleware(LoggingMiddleware(logger)(mux))
	return handler
}

func main() {
	// JSON Structured Logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration_load_failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("database_connection_failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer pool.Close()

	s := store.NewStore(pool)
	router := SetupRouter(logger, pool, s)

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	server := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("api_server_started", slog.String("addr", addr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server_listen_failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	logger.Info("shutting_down_api_server")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	_ = server.Shutdown(shutdownCtx)
}
