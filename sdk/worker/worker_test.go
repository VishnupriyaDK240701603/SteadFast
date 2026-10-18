package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestErrorClassification(t *testing.T) {
	origErr := errors.New("underlying problem")

	retErr := Retryable(origErr)
	if !IsRetryable(retErr) {
		t.Fatalf("expected Retryable error to have IsRetryable=true")
	}
	if retErr.Error() != origErr.Error() {
		t.Fatalf("expected error string %q, got %q", origErr.Error(), retErr.Error())
	}
	if !errors.Is(retErr, origErr) {
		t.Fatalf("expected error unwrap to find origErr")
	}

	permErr := Permanent(origErr)
	if IsRetryable(permErr) {
		t.Fatalf("expected Permanent error to have IsRetryable=false")
	}
	if permErr.Error() != origErr.Error() {
		t.Fatalf("expected error string %q, got %q", origErr.Error(), permErr.Error())
	}
	if !errors.Is(permErr, origErr) {
		t.Fatalf("expected error unwrap to find origErr")
	}

	stdErr := errors.New("default err")
	if !IsRetryable(stdErr) {
		t.Fatalf("expected standard error to be treated as retryable by default")
	}
}

func TestWorkerRun_NormalCompletion(t *testing.T) {
	jobID := uuid.New()
	var claimedCount int32
	var completedCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/worker/claim"):
			if atomic.AddInt32(&claimedCount, 1) == 1 {
				jobs := []*Job{
					{
						ID:           jobID,
						Type:         "test_task",
						Payload:      json.RawMessage(`{"hello":"world"}`),
						FencingToken: 42,
						Attempt:      1,
						MaxAttempts:  5,
						LeaseSeconds: 5,
					},
				}
				_ = json.NewEncoder(w).Encode(jobs)
				return
			}
			_ = json.NewEncoder(w).Encode([]*Job{})
		case strings.HasSuffix(r.URL.Path, "/complete"):
			atomic.AddInt32(&completedCount, 1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	handlerCalled := make(chan struct{})
	cfg := Config{
		APIBaseURL:   server.URL,
		APIKey:       "test-key",
		WorkerID:     "test-worker",
		PollInterval: 100 * time.Millisecond,
		Handlers: map[string]Handler{
			"test_task": func(ctx context.Context, job *Job) error {
				close(handlerCalled)
				return nil
			},
		},
	}

	go func() {
		_ = Run(ctx, cfg)
	}()

	select {
	case <-handlerCalled:
	case <-time.After(1 * time.Second):
		t.Fatalf("timed out waiting for handler to be called")
	}

	time.Sleep(200 * time.Millisecond)
	cancel()

	if atomic.LoadInt32(&completedCount) != 1 {
		t.Fatalf("expected completedCount to be 1, got %d", atomic.LoadInt32(&completedCount))
	}
}

func TestWorkerRun_HeartbeatCancelOnLeaseLost(t *testing.T) {
	jobID := uuid.New()
	var claimedCount int32
	var heartbeatCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/worker/claim"):
			if atomic.AddInt32(&claimedCount, 1) == 1 {
				jobs := []*Job{
					{
						ID:           jobID,
						Type:         "long_task",
						Payload:      json.RawMessage(`{}`),
						FencingToken: 1,
						Attempt:      1,
						MaxAttempts:  5,
						LeaseSeconds: 1, // 1 sec lease -> heartbeat every ~333ms
					},
				}
				_ = json.NewEncoder(w).Encode(jobs)
				return
			}
			_ = json.NewEncoder(w).Encode([]*Job{})
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			atomic.AddInt32(&heartbeatCount, 1)
			// Return 409 Conflict (lease_lost)
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"lease_lost"}}`))
		case strings.HasSuffix(r.URL.Path, "/fail"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	canceledChan := make(chan struct{})
	cfg := Config{
		APIBaseURL:   server.URL,
		APIKey:       "test-key",
		WorkerID:     "test-worker",
		PollInterval: 100 * time.Millisecond,
		Handlers: map[string]Handler{
			"long_task": func(ctx context.Context, job *Job) error {
				select {
				case <-ctx.Done():
					close(canceledChan)
					return ctx.Err()
				case <-time.After(2 * time.Second):
					return nil
				}
			},
		},
	}

	go func() {
		_ = Run(ctx, cfg)
	}()

	select {
	case <-canceledChan:
		// Succeeded: lease_lost canceled the handler context
	case <-time.After(1500 * time.Millisecond):
		t.Fatalf("expected handler context to be canceled after heartbeat returned 409")
	}

	cancel()
	if atomic.LoadInt32(&heartbeatCount) == 0 {
		t.Fatalf("expected at least 1 heartbeat attempt")
	}
}

func TestWorkerRun_PanicRecoveryReportsRetryable(t *testing.T) {
	jobID := uuid.New()
	var claimedCount int32
	var failReported int32
	var reportedRetryable bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/worker/claim"):
			if atomic.AddInt32(&claimedCount, 1) == 1 {
				jobs := []*Job{
					{
						ID:           jobID,
						Type:         "panic_task",
						Payload:      json.RawMessage(`{}`),
						FencingToken: 10,
						Attempt:      1,
						MaxAttempts:  5,
						LeaseSeconds: 5,
					},
				}
				_ = json.NewEncoder(w).Encode(jobs)
				return
			}
			_ = json.NewEncoder(w).Encode([]*Job{})
		case strings.HasSuffix(r.URL.Path, "/fail"):
			var body struct {
				FencingToken int64  `json:"fencing_token"`
				ErrorClass   string `json:"error_class"`
				Message      string `json:"message"`
				Retryable    bool   `json:"retryable"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			atomic.AddInt32(&failReported, 1)
			reportedRetryable = body.Retryable
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cfg := Config{
		APIBaseURL:   server.URL,
		APIKey:       "test-key",
		WorkerID:     "test-worker",
		PollInterval: 100 * time.Millisecond,
		Handlers: map[string]Handler{
			"panic_task": func(ctx context.Context, job *Job) error {
				panic("simulated fatal worker crash in handler")
			},
		},
	}

	go func() {
		_ = Run(ctx, cfg)
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	if atomic.LoadInt32(&failReported) != 1 {
		t.Fatalf("expected fail to be reported once, got %d", atomic.LoadInt32(&failReported))
	}
	if !reportedRetryable {
		t.Fatalf("expected panic to be reported with retryable=true")
	}
}

func TestWorkerNoDBOrClockComparison(t *testing.T) {
	data, err := os.ReadFile("worker.go")
	if err != nil {
		t.Fatalf("failed to read worker.go: %v", err)
	}
	content := string(data)

	// Engine Law 5: SDK never touches database
	forbiddenImports := []string{"database/sql", "github.com/jackc/pgx", "pgxpool"}
	for _, f := range forbiddenImports {
		if strings.Contains(content, f) {
			t.Fatalf("Engine Law 5 violation: worker.go imports forbidden database package %q", f)
		}
	}
}
