package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type BaselineSubmitRequest struct {
	Type        string          `json:"type,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	OrderID     string          `json:"order_id,omitempty"`
	CustomerID  string          `json:"customer_id,omitempty"`
	AmountCents int64           `json:"amount_cents,omitempty"`
}

type BaselineJob struct {
	ID          string `json:"id"`
	OrderID     string `json:"order_id"`
	CustomerID  string `json:"customer_id"`
	AmountCents int64  `json:"amount_cents"`
	Attempts    int    `json:"attempts"`
	MaxAttempts int    `json:"max_attempts"`
	CreatedAt   string `json:"created_at"`
}

type BaselineServer struct {
	rdb    *redis.Client
	logger *slog.Logger
}

func NewBaselineServer(rdb *redis.Client, logger *slog.Logger) *BaselineServer {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	return &BaselineServer{
		rdb:    rdb,
		logger: logger,
	}
}

func (s *BaselineServer) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", s.handleSubmit)
	mux.HandleFunc("POST /baseline/jobs", s.handleSubmit)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

func (s *BaselineServer) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req BaselineSubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "invalid_json", "message": err.Error()},
		})
		return
	}

	orderID := req.OrderID
	custID := req.CustomerID
	amount := req.AmountCents

	// If wrapped in payload:
	if len(req.Payload) > 0 {
		var inner struct {
			OrderID     string `json:"order_id"`
			CustomerID  string `json:"customer_id"`
			AmountCents int64  `json:"amount_cents"`
		}
		if err := json.Unmarshal(req.Payload, &inner); err == nil {
			if inner.OrderID != "" {
				orderID = inner.OrderID
			}
			if inner.CustomerID != "" {
				custID = inner.CustomerID
			}
			if inner.AmountCents > 0 {
				amount = inner.AmountCents
			}
		}
	}

	if orderID == "" || custID == "" || amount <= 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "invalid_payload", "message": "order_id, customer_id, and positive amount_cents required"},
		})
		return
	}

	jobID := uuid.NewString()
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)

	job := BaselineJob{
		ID:          jobID,
		OrderID:     orderID,
		CustomerID:  custID,
		AmountCents: amount,
		Attempts:    0,
		MaxAttempts: 5,
		CreatedAt:   nowStr,
	}

	jobJSON, err := json.Marshal(job)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Push to Redis queue
	if err := s.rdb.LPush(r.Context(), "baseline:queue", string(jobJSON)).Err(); err != nil {
		s.logger.Error("failed to push job to redis", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":         jobID,
		"state":      "QUEUED",
		"created_at": nowStr,
	})
}

func (s *BaselineServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://127.0.0.1:6379"
	}

	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		logger.Error("invalid REDIS_URL", "url", redisURL, "error", err)
		os.Exit(1)
	}

	rdb := redis.NewClient(opt)
	defer func() { _ = rdb.Close() }()

	server := NewBaselineServer(rdb, logger)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	httpServer := &http.Server{
		Addr:    ":" + port,
		Handler: server.Routes(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("baseline api listening", "addr", httpServer.Addr, "redis", redisURL)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down baseline api")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
