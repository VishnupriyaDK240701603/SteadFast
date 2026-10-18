package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Job struct {
	ID           uuid.UUID       `json:"id"`
	Type         string          `json:"type"`
	Payload      json.RawMessage `json:"payload"`
	FencingToken int64           `json:"fencing_token"`
	Attempt      int             `json:"attempt"`
	MaxAttempts  int             `json:"max_attempts"`
	LeaseSeconds int             `json:"lease_seconds"`
}

type Handler func(ctx context.Context, job *Job) error

type Config struct {
	APIBaseURL     string
	APIKey         string
	WorkerID       string
	Handlers       map[string]Handler
	Concurrency    int
	ClaimBatchSize int
	PollInterval   time.Duration
	HTTPClient     *http.Client
	Logger         *slog.Logger
}

type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return retryableError{err: err}
}

func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

func IsRetryable(err error) bool {
	var pe permanentError
	return !errors.As(err, &pe)
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.APIBaseURL == "" {
		return errors.New("APIBaseURL is required")
	}
	if cfg.APIKey == "" {
		return errors.New("APIKey is required")
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = "worker-" + uuid.NewString()
	}
	if len(cfg.Handlers) == 0 {
		return errors.New("at least one handler must be registered")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.ClaimBatchSize <= 0 {
		cfg.ClaimBatchSize = 10
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 1 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	types := make([]string, 0, len(cfg.Handlers))
	for t := range cfg.Handlers {
		types = append(types, t)
	}

	jobsChan := make(chan *Job, cfg.Concurrency)
	var wg sync.WaitGroup

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(workerIndex int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobsChan:
					if !ok {
						return
					}
					processJob(ctx, cfg, job)
				}
			}
		}(i)
	}

	defer func() {
		close(jobsChan)
		wg.Wait()
	}()

	backoffSleep := 500 * time.Millisecond
	maxBackoff := 10 * time.Second

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		jobs, err := claimJobs(ctx, cfg, types, cfg.ClaimBatchSize)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			cfg.Logger.Warn("claim failed, retrying with backoff", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(jitterDuration(backoffSleep)):
				backoffSleep = min(backoffSleep*2, maxBackoff)
				continue
			}
		}

		backoffSleep = 500 * time.Millisecond

		if len(jobs) == 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(jitterDuration(cfg.PollInterval)):
				continue
			}
		}

		for _, job := range jobs {
			select {
			case <-ctx.Done():
				return nil
			case jobsChan <- job:
			}
		}
	}
}

func processJob(parentCtx context.Context, cfg Config, job *Job) {
	handler, ok := cfg.Handlers[job.Type]
	if !ok {
		cfg.Logger.Error("no handler found for job type", "type", job.Type, "job_id", job.ID)
		_ = reportJobFailure(parentCtx, cfg, job.ID, job.FencingToken, "missing_handler", "no registered handler", false)
		return
	}

	jobCtx, cancelJob := context.WithCancel(parentCtx)
	defer cancelJob()

	leaseSec := job.LeaseSeconds
	if leaseSec <= 0 {
		leaseSec = 30
	}
	hbInterval := time.Duration(leaseSec) * time.Second / 3
	if hbInterval < 200*time.Millisecond {
		hbInterval = 200 * time.Millisecond
	}

	stopHb := make(chan struct{})
	hbDone := make(chan struct{})

	go func() {
		defer close(hbDone)
		ticker := time.NewTicker(hbInterval)
		defer ticker.Stop()

		for {
			select {
			case <-jobCtx.Done():
				return
			case <-stopHb:
				return
			case <-ticker.C:
				status, err := sendHeartbeat(jobCtx, cfg, job.ID, job.FencingToken)
				if status == http.StatusConflict {
					cfg.Logger.Warn("heartbeat returned 409 lease_lost, cancelling handler context", "job_id", job.ID)
					cancelJob()
					return
				}
				if err != nil && !errors.Is(err, context.Canceled) {
					cfg.Logger.Warn("transient heartbeat failure", "job_id", job.ID, "error", err)
				}
			}
		}
	}()

	var handlerErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				cfg.Logger.Error("handler panicked, recovering and marking retryable", "job_id", job.ID, "panic", r)
				handlerErr = Retryable(fmt.Errorf("panic: %v", r))
			}
		}()
		handlerErr = handler(jobCtx, job)
	}()

	close(stopHb)
	<-hbDone

	if handlerErr == nil {
		if err := reportJobCompletion(parentCtx, cfg, job.ID, job.FencingToken); err != nil {
			cfg.Logger.Warn("failed to report completion", "job_id", job.ID, "error", err)
		}
	} else {
		retryable := IsRetryable(handlerErr)
		errClass := "handler_error"
		if !retryable {
			errClass = "permanent_error"
		}
		if err := reportJobFailure(parentCtx, cfg, job.ID, job.FencingToken, errClass, handlerErr.Error(), retryable); err != nil {
			cfg.Logger.Warn("failed to report failure", "job_id", job.ID, "error", err)
		}
	}
}

func claimJobs(ctx context.Context, cfg Config, types []string, max int) ([]*Job, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"worker_id": cfg.WorkerID,
		"types":     types,
		"max":       max,
	})

	url := fmt.Sprintf("%s/v1/worker/claim", cfg.APIBaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var jobs []*Job
	if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil {
		return nil, fmt.Errorf("failed to decode claimed jobs: %w", err)
	}

	return jobs, nil
}

func sendHeartbeat(ctx context.Context, cfg Config, jobID uuid.UUID, fencingToken int64) (int, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"fencing_token": fencingToken,
	})

	url := fmt.Sprintf("%s/v1/worker/jobs/%s/heartbeat", cfg.APIBaseURL, jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode, nil
}

func reportJobCompletion(ctx context.Context, cfg Config, jobID uuid.UUID, fencingToken int64) error {
	reqBody, _ := json.Marshal(map[string]any{
		"fencing_token": fencingToken,
	})

	url := fmt.Sprintf("%s/v1/worker/jobs/%s/complete", cfg.APIBaseURL, jobID)
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := cfg.HTTPClient.Do(req)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func reportJobFailure(ctx context.Context, cfg Config, jobID uuid.UUID, fencingToken int64, errClass, msg string, retryable bool) error {
	reqBody, _ := json.Marshal(map[string]any{
		"fencing_token": fencingToken,
		"error_class":   errClass,
		"message":       msg,
		"retryable":     retryable,
	})

	url := fmt.Sprintf("%s/v1/worker/jobs/%s/fail", cfg.APIBaseURL, jobID)
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := cfg.HTTPClient.Do(req)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func jitterDuration(base time.Duration) time.Duration {
	if base <= 0 {
		return 100 * time.Millisecond
	}
	jitter := time.Duration(rand.Int63n(int64(base / 4)))
	return base + jitter
}
