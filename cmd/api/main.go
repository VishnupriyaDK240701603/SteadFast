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

	"math"
	"github.com/google/uuid"
	"steadfast/internal/ratelimit"
	"github.com/jackc/pgx/v5/pgxpool"
	"steadfast/internal/auth"
	"steadfast/internal/backoff"
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

			logger.Info("http_request_started",
				"request_id", reqID,
				"method", r.Method,
				"path", r.URL.Path,
				"remote_addr", r.RemoteAddr,
			)

			rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)

			duration := time.Since(start)
			logger.Info("http_request_completed",
				"request_id", reqID,
				"status", rw.status,
				"duration_ms", duration.Milliseconds(),
			)
		})
	}
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *statusResponseWriter) WriteHeader(code int) {
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

type ClaimJobsRequest struct {
	WorkerID string   `json:"worker_id"`
	Types    []string `json:"types"`
	Max      int      `json:"max"`
}

type HeartbeatRequest struct {
	FencingToken int64 `json:"fencing_token"`
}

type CompleteRequest struct {
	FencingToken int64           `json:"fencing_token"`
	Result       json.RawMessage `json:"result,omitempty"`
}

type FailRequest struct {
	FencingToken int64  `json:"fencing_token"`
	ErrorClass   string `json:"error_class"`
	Message      string `json:"message"`
	Retryable    bool   `json:"retryable"`
}

type FailResponse struct {
	NextState string  `json:"next_state"`
	RunAt     *string `json:"run_at,omitempty"`
}

type PatchDLQPayload struct {
	Payload json.RawMessage `json:"payload"`
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
		mux.Handle("POST /v1/worker/", outageHandler)
		mux.Handle("GET /v1/dlq", outageHandler)
		mux.Handle("PATCH /v1/dlq/", outageHandler)
		mux.Handle("POST /v1/dlq/", outageHandler)
	} else {
		tenantAuth := auth.RequireTenantAuth(s)
		bo := backoff.New(100*time.Millisecond, 30*time.Second)
		rps := 5000
		if cfg != nil {
			bo = backoff.New(cfg.BackoffBase, cfg.BackoffCap)
			if cfg.RateLimitRPS > 0 {
				rps = cfg.RateLimitRPS
			}
		}
		limiter := ratelimit.NewTokenBucketLimiter(rps)

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

			// Rate limit per authenticated tenant key hash (F13, P1)
			if limiter != nil {
				allowed, waitDuration := limiter.Allow(tenant.APIKeyHash)
				if !allowed {
					retryAfterSec := int(math.Ceil(waitDuration.Seconds()))
					if retryAfterSec < 1 {
						retryAfterSec = 1
					}
					w.Header().Set("Retry-After", strconv.Itoa(retryAfterSec))
					auth.WriteJSONError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "rate limit exceeded: too many requests")
					return
				}
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

		// GET /v1/jobs/{id}/timeline (F15, Appendix K)
		mux.Handle("GET /v1/jobs/{id}/timeline", tenantAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

			timeline, err := s.GetJobTimeline(r.Context(), tenant.ID, jobID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					auth.WriteJSONError(w, http.StatusNotFound, "not_found", "job not found")
					return
				}
				auth.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "failed to retrieve job timeline")
				return
			}

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(timeline)
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

		// ==================== WORKER ENDPOINTS (F3, F4) ====================

		// POST /v1/worker/claim
		mux.Handle("POST /v1/worker/claim", tenantAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant, ok := auth.TenantFromContext(r.Context())
			if !ok || tenant == nil {
				auth.WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing authentication token")
				return
			}

			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "content-type must be application/json")
				return
			}

			var req ClaimJobsRequest
			if err := DecodeStrictJSON(r.Body, &req); err != nil {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("invalid json body: %v", err))
				return
			}

			if strings.TrimSpace(req.WorkerID) == "" {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "worker_id is required")
				return
			}
			if len(req.WorkerID) > 128 {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "worker_id must not exceed 128 characters")
				return
			}

			maxBatch := req.Max
			if maxBatch <= 0 {
				maxBatch = 10
			}
			if maxBatch > 100 {
				maxBatch = 100
			}

			leaseDuration := 10 * time.Second
			if cfg != nil && cfg.LeaseDuration > 0 {
				leaseDuration = cfg.LeaseDuration
			}

			claimed, err := s.ClaimJobs(r.Context(), tenant.ID, req.WorkerID, req.Types, maxBatch, leaseDuration)
			if err != nil {
				w.Header().Set("Retry-After", "1")
				auth.WriteJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "database query failed")
				return
			}

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(claimed)
		})))

		// POST /v1/worker/jobs/{id}/heartbeat
		mux.Handle("POST /v1/worker/jobs/{id}/heartbeat", tenantAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "content-type must be application/json")
				return
			}

			var req HeartbeatRequest
			if err := DecodeStrictJSON(r.Body, &req); err != nil {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("invalid json body: %v", err))
				return
			}

			leaseDuration := 10 * time.Second
			disableFencing := false
			if cfg != nil {
				if cfg.LeaseDuration > 0 {
					leaseDuration = cfg.LeaseDuration
				}
				disableFencing = cfg.DisableFencing
			}

			err = s.HeartbeatJob(r.Context(), tenant.ID, jobID, req.FencingToken, leaseDuration, disableFencing)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					auth.WriteJSONError(w, http.StatusNotFound, "not_found", "job not found")
					return
				}
				if errors.Is(err, store.ErrLeaseLost) {
					_ = s.RecordStaleWriteRejected(r.Context(), tenant.ID, jobID, "heartbeat", "", req.FencingToken)
					auth.WriteJSONError(w, http.StatusConflict, "lease_lost", "lease lost: token stale or job not leased")
					return
				}
				auth.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "heartbeat failed")
				return
			}

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		})))

		// POST /v1/worker/jobs/{id}/complete
		mux.Handle("POST /v1/worker/jobs/{id}/complete", tenantAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "content-type must be application/json")
				return
			}

			var req CompleteRequest
			if err := DecodeStrictJSON(r.Body, &req); err != nil {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("invalid json body: %v", err))
				return
			}

			disableFencing := false
			if cfg != nil {
				disableFencing = cfg.DisableFencing
			}

			err = s.CompleteJob(r.Context(), tenant.ID, jobID, req.FencingToken, "", req.Result, disableFencing)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					auth.WriteJSONError(w, http.StatusNotFound, "not_found", "job not found")
					return
				}
				if errors.Is(err, store.ErrStaleFencingToken) {
					_ = s.RecordStaleWriteRejected(r.Context(), tenant.ID, jobID, "complete", "", req.FencingToken)
					auth.WriteJSONError(w, http.StatusConflict, "stale_fencing_token", "stale fencing token or job not in LEASED state")
					return
				}
				auth.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "complete failed")
				return
			}

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		})))

		// POST /v1/worker/jobs/{id}/fail
		mux.Handle("POST /v1/worker/jobs/{id}/fail", tenantAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "content-type must be application/json")
				return
			}

			var req FailRequest
			if err := DecodeStrictJSON(r.Body, &req); err != nil {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("invalid json body: %v", err))
				return
			}

			if strings.TrimSpace(req.ErrorClass) == "" {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "error_class is required")
				return
			}

			disableFencing := false
			if cfg != nil {
				disableFencing = cfg.DisableFencing
			}

			// Read current attempts to calculate backoff delay
			job, err := s.GetJobByID(r.Context(), tenant.ID, jobID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					auth.WriteJSONError(w, http.StatusNotFound, "not_found", "job not found")
					return
				}
				auth.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "failed to lookup job")
				return
			}

			retryDelay := bo.Duration(job.Attempts)

			nextState, runAt, err := s.FailJob(r.Context(), tenant.ID, jobID, req.FencingToken, req.ErrorClass, req.Message, req.Retryable, retryDelay, disableFencing)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					auth.WriteJSONError(w, http.StatusNotFound, "not_found", "job not found")
					return
				}
				if errors.Is(err, store.ErrStaleFencingToken) {
					_ = s.RecordStaleWriteRejected(r.Context(), tenant.ID, jobID, "fail", "", req.FencingToken)
					auth.WriteJSONError(w, http.StatusConflict, "stale_fencing_token", "stale fencing token or job not in LEASED state")
					return
				}
				auth.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "fail failed")
				return
			}

			var runAtStr *string
			if runAt != nil {
				formatted := runAt.Format(time.RFC3339Nano)
				runAtStr = &formatted
			}

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(FailResponse{
				NextState: nextState,
				RunAt:     runAtStr,
			})
		})))

		// ==================== DLQ ENDPOINTS (F6) ====================

		// GET /v1/dlq
		mux.Handle("GET /v1/dlq", tenantAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant, ok := auth.TenantFromContext(r.Context())
			if !ok || tenant == nil {
				auth.WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing authentication token")
				return
			}

			limit := 50
			if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
				l, err := strconv.Atoi(limitStr)
				if err != nil || l < 1 || l > 200 {
					auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 200")
					return
				}
				limit = l
			}

			cursor := r.URL.Query().Get("cursor")
			jobs, nextCursor, err := s.GetDeadLetterJobs(r.Context(), tenant.ID, limit, cursor)
			if err != nil {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("failed to query dlq: %v", err))
				return
			}

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(ListJobsResponse{
				Jobs:       jobs,
				NextCursor: nextCursor,
			})
		})))

		// PATCH /v1/dlq/{id}
		mux.Handle("PATCH /v1/dlq/{id}", tenantAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "content-type must be application/json")
				return
			}

			var req PatchDLQPayload
			if err := DecodeStrictJSON(r.Body, &req); err != nil {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("invalid json body: %v", err))
				return
			}

			trimmed := bytes.TrimSpace(req.Payload)
			if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "payload cannot be empty or null")
				return
			}
			if len(req.Payload) > 65536 {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "payload exceeds maximum allowed size of 64 KiB")
				return
			}

			canonical, err := idempotency.CanonicalizeJSON(req.Payload)
			if err != nil {
				auth.WriteJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("malformed json: %v", err))
				return
			}
			newHash := idempotency.HashPayload(canonical)

			err = s.UpdateDeadLetterJobPayload(r.Context(), tenant.ID, jobID, canonical, newHash, "operator")
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					auth.WriteJSONError(w, http.StatusNotFound, "not_found", "job not found")
					return
				}
				if errors.Is(err, store.ErrNotDead) {
					auth.WriteJSONError(w, http.StatusConflict, "job_not_dead", "cannot edit payload for job that is not DEAD")
					return
				}
				auth.WriteJSONError(w, http.StatusInternalServerError, "internal_error", fmt.Sprintf("failed to update dlq payload: %v", err))
				return
			}

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		})))

		// POST /v1/dlq/{id}/replay
		mux.Handle("POST /v1/dlq/{id}/replay", tenantAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

			err = s.ReplayDeadLetterJob(r.Context(), tenant.ID, jobID, "operator")
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					auth.WriteJSONError(w, http.StatusNotFound, "not_found", "job not found")
					return
				}
				if errors.Is(err, store.ErrNotDead) {
					auth.WriteJSONError(w, http.StatusConflict, "job_not_dead", "cannot replay job that is not DEAD")
					return
				}
				auth.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "failed to replay dlq job")
				return
			}

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    jobID.String(),
				"state": "QUEUED",
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
		logger.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	// Engine Law 10: Chaos mutation check
	if cfg.DisableFencing {
		logger.Warn("WARNING: STEADFAST_DISABLE_FENCING is enabled! Fencing token validation is disabled for demo chaos testing!")
	}
	if cfg.DisableIdempotency {
		logger.Warn("WARNING: STEADFAST_DISABLE_IDEMPOTENCY is enabled! Idempotency key checking is disabled for demo chaos testing!")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("failed to connect to postgresql", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	s := store.NewStore(pool)
	router := SetupRouter(logger, pool, s, cfg)

	addr := fmt.Sprintf(":%d", cfg.Port)
	server := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("starting steadfast api server", "addr", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down steadfast api server gracefully")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}
