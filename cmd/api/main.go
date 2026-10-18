package main

import (
	"bytes"
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
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"steadfast/internal/auth"
	"steadfast/internal/config"
	"steadfast/internal/idempotency"
	"steadfast/internal/store"
)

type contextKey string

const (
	requestIDKey contextKey = "steadfast.request_id"
)

var (
	typeRegex = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)
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

type SubmitJobPayload struct {
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload"`
	Priority    *int            `json:"priority,omitempty"`
	RunAt       *string         `json:"run_at,omitempty"`
	MaxAttempts *int            `json:"max_attempts,omitempty"`
}

type SubmitJobResponse struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
}

type ListJobsResponse struct {
	Jobs       []store.Job `json:"jobs"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

func SetupRouter(logger *slog.Logger, pool *pgxpool.Pool, s *store.Store, cfg *config.Config) http.Handler {
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

	if s == nil {
		outageHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1")
			auth.WriteJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "database unreachable or service initializing")
		})
		mux.Handle("POST /v1/jobs", outageHandler)
		mux.Handle("GET /v1/jobs", outageHandler)
		mux.Handle("GET /v1/jobs/", outageHandler)
	} else {
		tenantAuth := auth.RequireTenantAuth(s)

		// GET /v1/tenant/me
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

		// POST /v1/jobs
		mux.Handle("POST /v1/jobs", tenantAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant, ok := auth.TenantFromContext(r.Context())
			if !ok || tenant == nil {
				auth.WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing authentication token")
				return
			}

			// Validate Content-Type
			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "content-type must be application/json")
				return
			}

			// Cap body size to 1 MiB
			r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)

			var req SubmitJobPayload
			if err := DecodeStrictJSON(r.Body, &req); err != nil {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("invalid json payload: %v", err))
				return
			}

			// Validate type
			if !typeRegex.MatchString(req.Type) {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "type must match ^[a-z0-9_.-]{1,64}$")
				return
			}

			// Validate payload
			trimmedPayload := bytes.TrimSpace(req.Payload)
			if len(trimmedPayload) == 0 || bytes.Equal(trimmedPayload, []byte("null")) {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "payload cannot be empty or null")
				return
			}
			if len(req.Payload) > 65536 {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "payload exceeds maximum allowed size of 64 KiB")
				return
			}

			canonicalPayload, err := idempotency.CanonicalizeJSON(req.Payload)
			if err != nil {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("malformed json payload: %v", err))
				return
			}
			payloadHash := idempotency.HashPayload(canonicalPayload)

			// Validate max_attempts (1 to 20)
			maxAttempts := 5
			if cfg != nil && cfg.DefaultMaxAttempts > 0 {
				maxAttempts = cfg.DefaultMaxAttempts
			}
			if req.MaxAttempts != nil {
				if *req.MaxAttempts < 1 || *req.MaxAttempts > 20 {
					auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "max_attempts must be between 1 and 20")
					return
				}
				maxAttempts = *req.MaxAttempts
			}

			// Validate priority
			priority := 100
			if req.Priority != nil {
				priority = *req.Priority
			}

			// Validate run_at
			var runAt time.Time
			if req.RunAt != nil && *req.RunAt != "" {
				t, err := time.Parse(time.RFC3339, *req.RunAt)
				if err != nil {
					auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "run_at must be an RFC3339 timestamp")
					return
				}
				runAt = t
			}

			// Validate Idempotency-Key header
			idempKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
			if len(idempKey) > 128 {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "idempotency key must not exceed 128 characters")
				return
			}

			ttl := 24 * time.Hour
			if cfg != nil && cfg.IdempotencyTTL > 0 {
				ttl = cfg.IdempotencyTTL
			}

			submitReq := &store.SubmitJobRequest{
				JobType:        req.Type,
				Payload:        canonicalPayload,
				PayloadHash:    payloadHash,
				IdempotencyKey: idempKey,
				Priority:       priority,
				RunAt:          runAt,
				MaxAttempts:    maxAttempts,
			}

			result, err := s.SubmitJob(r.Context(), tenant.ID, submitReq, ttl)
			if err != nil {
				if errors.Is(err, store.ErrIdempotencyConflict) {
					auth.WriteJSONError(w, http.StatusConflict, "idempotency_key_conflict", "idempotency key already used with different payload")
					return
				}

				// Engine Law 1: Never answer 202 if transaction cannot commit
				w.Header().Set("Retry-After", "1")
				auth.WriteJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "database unreachable or transaction failed")
				return
			}

			if result.IsReplayed {
				w.Header().Set("Idempotent-Replayed", "true")
			}

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(SubmitJobResponse{
				ID:        result.JobID.String(),
				State:     result.State,
				CreatedAt: result.CreatedAt.Format(time.RFC3339Nano),
			})
		})))

		// GET /v1/jobs/{id}
		mux.Handle("GET /v1/jobs/{id}", tenantAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant, ok := auth.TenantFromContext(r.Context())
			if !ok || tenant == nil {
				auth.WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing authentication token")
				return
			}

			idStr := r.PathValue("id")
			jobID, err := uuid.Parse(idStr)
			if err != nil {
				auth.WriteJSONError(w, http.StatusNotFound, "not_found", "job not found")
				return
			}

			jobWithAttempts, err := s.GetJobWithAttempts(r.Context(), tenant.ID, jobID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					auth.WriteJSONError(w, http.StatusNotFound, "not_found", "job not found")
					return
				}
				auth.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "failed to retrieve job")
				return
			}

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(jobWithAttempts)
		})))

		// GET /v1/jobs
		mux.Handle("GET /v1/jobs", tenantAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant, ok := auth.TenantFromContext(r.Context())
			if !ok || tenant == nil {
				auth.WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing authentication token")
				return
			}

			q := r.URL.Query()
			state := q.Get("state")
			if state != "" && state != "QUEUED" && state != "LEASED" && state != "SUCCEEDED" && state != "DEAD" {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "invalid state parameter; must be QUEUED, LEASED, SUCCEEDED, or DEAD")
				return
			}

			limit := 50
			if limitStr := q.Get("limit"); limitStr != "" {
				l, err := strconv.Atoi(limitStr)
				if err != nil || l < 1 || l > 200 {
					auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 200")
					return
				}
				limit = l
			}

			cursor := q.Get("cursor")
			jobs, nextCursor, err := s.ListJobs(r.Context(), tenant.ID, state, limit, cursor)
			if err != nil {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("failed to query jobs: %v", err))
				return
			}

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(ListJobsResponse{
				Jobs:       jobs,
				NextCursor: nextCursor,
			})
		})))
	}

	handler := RequestIDMiddleware(LoggingMiddleware(logger)(mux))
	return handler
}

func main() {
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
	router := SetupRouter(logger, pool, s, cfg)

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
