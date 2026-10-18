package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound             = errors.New("record not found")
	ErrConflict             = errors.New("record conflict")
	ErrTenantMismatch       = errors.New("tenant authorization mismatch")
	ErrIdempotencyConflict  = errors.New("idempotency key conflict: key already used with different payload")
)

type Tenant struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	APIKeyHash string    `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
}

type Job struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	JobType        string     `json:"job_type"`
	Payload        []byte     `json:"payload"`
	PayloadHash    string     `json:"payload_hash"`
	IdempotencyKey *string    `json:"idempotency_key"`
	State          string     `json:"state"`
	Priority       int        `json:"priority"`
	RunAt          time.Time  `json:"run_at"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	FencingToken   int64      `json:"fencing_token"`
	WorkerID       *string    `json:"worker_id"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at"`
	LastError      *string    `json:"last_error"`
	DeadReason     *string    `json:"dead_reason"`
	Result         []byte     `json:"result"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	FinishedAt     *time.Time `json:"finished_at"`
}

type JobAttempt struct {
	ID           int64      `json:"id"`
	JobID        uuid.UUID  `json:"job_id"`
	AttemptNo    int        `json:"attempt_no"`
	FencingToken int64      `json:"fencing_token"`
	WorkerID     string     `json:"worker_id"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	Outcome      *string    `json:"outcome"`
	ErrorClass   *string    `json:"error_class"`
	ErrorMessage *string    `json:"error_message"`
}

type JobWithAttempts struct {
	Job
	JobAttempts []JobAttempt `json:"job_attempts"`
}

type SubmitJobRequest struct {
	JobType        string
	Payload        []byte
	PayloadHash    string
	IdempotencyKey string
	Priority       int
	RunAt          time.Time
	MaxAttempts    int
}

type SubmitJobResult struct {
	JobID      uuid.UUID
	State      string
	CreatedAt  time.Time
	IsReplayed bool
}

type AuditEntry struct {
	ID        int64
	Timestamp time.Time
	TenantID  *uuid.UUID
	Actor     string
	Action    string
	JobID     *uuid.UUID
	FromState *string
	ToState   *string
	Details   []byte
}

type Store struct {
	Pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool}
}

func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database url: %w", err)
	}

	cfg.MaxConns = 25
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return pool, nil
}

func (s *Store) CreateTenant(ctx context.Context, name, apiKeyHash string) (*Tenant, error) {
	query := `
	INSERT INTO tenants (name, api_key_hash)
	VALUES ($1, $2)
	ON CONFLICT (name) DO UPDATE SET api_key_hash = EXCLUDED.api_key_hash
	RETURNING id, name, api_key_hash, created_at;`

	t := &Tenant{}
	err := s.Pool.QueryRow(ctx, query, name, apiKeyHash).Scan(&t.ID, &t.Name, &t.APIKeyHash, &t.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to create tenant %s: %w", name, err)
	}
	return t, nil
}

func (s *Store) GetTenantByAPIKeyHash(ctx context.Context, apiKeyHash string) (*Tenant, error) {
	query := `SELECT id, name, api_key_hash, created_at FROM tenants WHERE api_key_hash = $1;`
	t := &Tenant{}
	err := s.Pool.QueryRow(ctx, query, apiKeyHash).Scan(&t.ID, &t.Name, &t.APIKeyHash, &t.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get tenant by api key hash: %w", err)
	}
	return t, nil
}

func (s *Store) CreateJob(ctx context.Context, tenantID uuid.UUID, job *Job) (*Job, error) {
	// Engine Law 4: Tenant ID is strictly enforced on all writes.
	job.TenantID = tenantID
	query := `
	INSERT INTO jobs (
		tenant_id, job_type, payload, payload_hash, idempotency_key,
		state, priority, run_at, attempts, max_attempts, fencing_token
	) VALUES (
		$1, $2, $3, $4, $5,
		coalesce(nullif($6, ''), 'QUEUED'),
		coalesce(nullif($7, 0), 100),
		coalesce($8, now()),
		0,
		coalesce(nullif($9, 0), 5),
		0
	)
	RETURNING id, tenant_id, job_type, payload, payload_hash, idempotency_key, state, priority,
	          run_at, attempts, max_attempts, fencing_token, created_at, updated_at;`

	runAt := job.RunAt
	if runAt.IsZero() {
		runAt = time.Now()
	}

	err := s.Pool.QueryRow(
		ctx, query,
		tenantID, job.JobType, job.Payload, job.PayloadHash, job.IdempotencyKey,
		job.State, job.Priority, runAt, job.MaxAttempts,
	).Scan(
		&job.ID, &job.TenantID, &job.JobType, &job.Payload, &job.PayloadHash, &job.IdempotencyKey,
		&job.State, &job.Priority, &job.RunAt, &job.Attempts, &job.MaxAttempts, &job.FencingToken,
		&job.CreatedAt, &job.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create job: %w", err)
	}
	return job, nil
}

func (s *Store) GetJob(ctx context.Context, tenantID, jobID uuid.UUID) (*Job, error) {
	// Engine Law 4: Tenant ID is strictly enforced. A foreign job ID returns ErrNotFound (404).
	query := `
	SELECT id, tenant_id, job_type, payload, payload_hash, idempotency_key, state, priority,
	       run_at, attempts, max_attempts, fencing_token, worker_id, lease_expires_at,
	       last_error, dead_reason, result, created_at, updated_at, finished_at
	FROM jobs
	WHERE id = $1 AND tenant_id = $2;`

	j := &Job{}
	err := s.Pool.QueryRow(ctx, query, jobID, tenantID).Scan(
		&j.ID, &j.TenantID, &j.JobType, &j.Payload, &j.PayloadHash, &j.IdempotencyKey, &j.State, &j.Priority,
		&j.RunAt, &j.Attempts, &j.MaxAttempts, &j.FencingToken, &j.WorkerID, &j.LeaseExpiresAt,
		&j.LastError, &j.DeadReason, &j.Result, &j.CreatedAt, &j.UpdatedAt, &j.FinishedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get job %s: %w", jobID, err)
	}
	return j, nil
}

func (s *Store) GetJobWithAttempts(ctx context.Context, tenantID, jobID uuid.UUID) (*JobWithAttempts, error) {
	job, err := s.GetJob(ctx, tenantID, jobID)
	if err != nil {
		return nil, err
	}

	attemptsQuery := `
	SELECT id, job_id, attempt_no, fencing_token, worker_id, started_at, finished_at, outcome, error_class, error_message
	FROM job_attempts
	WHERE job_id = $1
	ORDER BY attempt_no ASC;`

	rows, err := s.Pool.Query(ctx, attemptsQuery, jobID)
	if err != nil {
		return nil, fmt.Errorf("failed to query job attempts: %w", err)
	}
	defer rows.Close()

	attempts := []JobAttempt{}
	for rows.Next() {
		var a JobAttempt
		if err := rows.Scan(
			&a.ID, &a.JobID, &a.AttemptNo, &a.FencingToken, &a.WorkerID,
			&a.StartedAt, &a.FinishedAt, &a.Outcome, &a.ErrorClass, &a.ErrorMessage,
		); err != nil {
			return nil, fmt.Errorf("failed to scan job attempt: %w", err)
		}
		attempts = append(attempts, a)
	}

	return &JobWithAttempts{
		Job:         *job,
		JobAttempts: attempts,
	}, nil
}

func (s *Store) ListJobs(ctx context.Context, tenantID uuid.UUID, state string, limit int, cursor string) ([]Job, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	var cursorCreatedAt time.Time
	var cursorID uuid.UUID

	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor format")
		}
		parts := strings.Split(string(decoded), "|")
		if len(parts) != 2 {
			return nil, "", fmt.Errorf("invalid cursor content")
		}
		t, err := time.Parse(time.RFC3339Nano, parts[0])
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor timestamp")
		}
		u, err := uuid.Parse(parts[1])
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor uuid")
		}
		cursorCreatedAt = t
		cursorID = u
	}

	query := `
	SELECT id, tenant_id, job_type, payload, payload_hash, idempotency_key, state, priority,
	       run_at, attempts, max_attempts, fencing_token, worker_id, lease_expires_at,
	       last_error, dead_reason, result, created_at, updated_at, finished_at
	FROM jobs
	WHERE tenant_id = $1
	  AND ($2 = '' OR state = $2)
	  AND ($3 = '0001-01-01T00:00:00Z'::timestamptz OR (created_at, id) < ($3, $4))
	ORDER BY created_at DESC, id DESC
	LIMIT $5;`

	fetchLimit := limit + 1
	rows, err := s.Pool.Query(ctx, query, tenantID, state, cursorCreatedAt, cursorID, fetchLimit)
	if err != nil {
		return nil, "", fmt.Errorf("failed to query jobs: %w", err)
	}
	defer rows.Close()

	jobs := []Job{}
	for rows.Next() {
		var j Job
		if err := rows.Scan(
			&j.ID, &j.TenantID, &j.JobType, &j.Payload, &j.PayloadHash, &j.IdempotencyKey, &j.State, &j.Priority,
			&j.RunAt, &j.Attempts, &j.MaxAttempts, &j.FencingToken, &j.WorkerID, &j.LeaseExpiresAt,
			&j.LastError, &j.DeadReason, &j.Result, &j.CreatedAt, &j.UpdatedAt, &j.FinishedAt,
		); err != nil {
			return nil, "", fmt.Errorf("failed to scan job: %w", err)
		}
		jobs = append(jobs, j)
	}

	nextCursor := ""
	if len(jobs) > limit {
		last := jobs[limit-1]
		cursorRaw := fmt.Sprintf("%s|%s", last.CreatedAt.Format(time.RFC3339Nano), last.ID.String())
		nextCursor = base64.RawURLEncoding.EncodeToString([]byte(cursorRaw))
		jobs = jobs[:limit]
	}

	return jobs, nextCursor, nil
}

func (s *Store) SubmitJob(ctx context.Context, tenantID uuid.UUID, req *SubmitJobRequest, ttl time.Duration) (*SubmitJobResult, error) {
	priority := req.Priority
	if priority <= 0 {
		priority = 100
	}
	maxAttempts := req.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	runAt := req.RunAt
	if runAt.IsZero() {
		runAt = time.Now().UTC()
	}

	// Case 1: Non-idempotent submit (no Idempotency-Key header)
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to begin transaction: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		jobID := uuid.New()
		query := `
		INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, idempotency_key, state, priority, run_at, max_attempts)
		VALUES ($1, $2, $3, $4, $5, NULL, 'QUEUED', $6, $7, $8)
		RETURNING created_at;`

		var createdAt time.Time
		if err := tx.QueryRow(ctx, query, jobID, tenantID, req.JobType, req.Payload, req.PayloadHash, priority, runAt, maxAttempts).Scan(&createdAt); err != nil {
			return nil, fmt.Errorf("failed to insert non-idempotent job: %w", err)
		}

		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("failed to commit non-idempotent job: %w", err)
		}

		return &SubmitJobResult{
			JobID:      jobID,
			State:      "QUEUED",
			CreatedAt:  createdAt,
			IsReplayed: false,
		}, nil
	}

	// Case 2: Idempotent submit with key
	ttlMillis := ttl.Milliseconds()
	if ttlMillis <= 0 {
		ttlMillis = 86400 * 1000
	}

	for attempt := 0; attempt < 5; attempt++ {
		tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to begin transaction: %w", err)
		}

		// Check if key exists
		var existingHash string
		var existingJobID uuid.UUID
		var expiresAt time.Time

		checkQuery := `
		SELECT payload_hash, job_id, expires_at
		FROM idempotency_keys
		WHERE tenant_id = $1 AND key = $2
		FOR UPDATE;`

		err = tx.QueryRow(ctx, checkQuery, tenantID, req.IdempotencyKey).Scan(&existingHash, &existingJobID, &expiresAt)
		if err == nil {
			// Row exists
			if expiresAt.After(time.Now()) {
				// Key is active
				if existingHash != req.PayloadHash {
					_ = tx.Rollback(ctx)
					return nil, ErrIdempotencyConflict
				}

				// Replay matching job
				var state string
				var createdAt time.Time
				jobLookup := `SELECT state, created_at FROM jobs WHERE id = $1 AND tenant_id = $2;`
				if err := tx.QueryRow(ctx, jobLookup, existingJobID, tenantID).Scan(&state, &createdAt); err != nil {
					_ = tx.Rollback(ctx)
					return nil, fmt.Errorf("idempotency record refers to missing job %s: %w", existingJobID, err)
				}

				_ = tx.Commit(ctx)
				return &SubmitJobResult{
					JobID:      existingJobID,
					State:      state,
					CreatedAt:  createdAt,
					IsReplayed: true,
				}, nil
			}

			// Key is expired: reuse it!
			newJobID := uuid.New()
			var createdAt time.Time
			jobInsert := `
			INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, idempotency_key, state, priority, run_at, max_attempts)
			VALUES ($1, $2, $3, $4, $5, $6, 'QUEUED', $7, $8, $9)
			RETURNING created_at;`

			if err := tx.QueryRow(ctx, jobInsert, newJobID, tenantID, req.JobType, req.Payload, req.PayloadHash, req.IdempotencyKey, priority, runAt, maxAttempts).Scan(&createdAt); err != nil {
				_ = tx.Rollback(ctx)
				return nil, fmt.Errorf("failed to insert job on expired key reuse: %w", err)
			}

			respBody, _ := json.Marshal(map[string]any{
				"id":         newJobID.String(),
				"state":      "QUEUED",
				"created_at": createdAt.Format(time.RFC3339Nano),
			})

			updateKey := `
			UPDATE idempotency_keys
			SET payload_hash = $1, job_id = $2, response_code = 202, response_body = $3, created_at = now(), expires_at = now() + ($4 * interval '1 millisecond')
			WHERE tenant_id = $5 AND key = $6;`

			if _, err := tx.Exec(ctx, updateKey, req.PayloadHash, newJobID, respBody, ttlMillis, tenantID, req.IdempotencyKey); err != nil {
				_ = tx.Rollback(ctx)
				return nil, fmt.Errorf("failed to update expired idempotency key: %w", err)
			}

			if err := tx.Commit(ctx); err != nil {
				return nil, fmt.Errorf("failed to commit expired key reuse: %w", err)
			}

			return &SubmitJobResult{
				JobID:      newJobID,
				State:      "QUEUED",
				CreatedAt:  createdAt,
				IsReplayed: false,
			}, nil
		}

		if errors.Is(err, pgx.ErrNoRows) {
			// Key does not exist yet. Insert into jobs FIRST so foreign key constraint holds.
			candidateJobID := uuid.New()
			var createdAt time.Time
			jobInsert := `
			INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, idempotency_key, state, priority, run_at, max_attempts)
			VALUES ($1, $2, $3, $4, $5, $6, 'QUEUED', $7, $8, $9)
			RETURNING created_at;`

			if err := tx.QueryRow(ctx, jobInsert, candidateJobID, tenantID, req.JobType, req.Payload, req.PayloadHash, req.IdempotencyKey, priority, runAt, maxAttempts).Scan(&createdAt); err != nil {
				_ = tx.Rollback(ctx)
				return nil, fmt.Errorf("failed to insert candidate job: %w", err)
			}

			respBody, _ := json.Marshal(map[string]any{
				"id":         candidateJobID.String(),
				"state":      "QUEUED",
				"created_at": createdAt.Format(time.RFC3339Nano),
			})

			keyInsert := `
			INSERT INTO idempotency_keys (tenant_id, key, payload_hash, job_id, response_code, response_body, expires_at)
			VALUES ($1, $2, $3, $4, 202, $5, now() + ($6 * interval '1 millisecond'));`

			if _, err := tx.Exec(ctx, keyInsert, tenantID, req.IdempotencyKey, req.PayloadHash, candidateJobID, respBody, ttlMillis); err != nil {
				_ = tx.Rollback(ctx)
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" {
					// Unique constraint violation: another concurrent transaction claimed this key.
					// Retry loop to read or handle winner's key.
					continue
				}
				return nil, fmt.Errorf("failed to insert idempotency key: %w", err)
			}

			if err := tx.Commit(ctx); err != nil {
				return nil, fmt.Errorf("failed to commit new idempotency key: %w", err)
			}

			return &SubmitJobResult{
				JobID:      candidateJobID,
				State:      "QUEUED",
				CreatedAt:  createdAt,
				IsReplayed: false,
			}, nil
		}

		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("idempotency check query failed: %w", err)
	}

	return nil, fmt.Errorf("could not resolve idempotency after multiple attempts")
}

func (s *Store) UpdateJobState(ctx context.Context, tenantID, jobID uuid.UUID, newState, actor string) error {
	// Engine Law 4: Tenant ID is strictly enforced. No state update occurs across tenant boundary.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.SetActorTx(ctx, tx, actor); err != nil {
		return fmt.Errorf("failed to set actor: %w", err)
	}

	query := `
	UPDATE jobs
	SET state = $1, updated_at = now()
	WHERE id = $2 AND tenant_id = $3;`

	tag, err := tx.Exec(ctx, query, newState, jobID, tenantID)
	if err != nil {
		return fmt.Errorf("failed to update job state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	return tx.Commit(ctx)
}

func (s *Store) SetActorTx(ctx context.Context, tx pgx.Tx, actor string) error {
	if actor == "" {
		actor = "system"
	}
	_, err := tx.Exec(ctx, "SELECT set_config('steadfast.actor', $1, true);", actor)
	return err
}

func (s *Store) GetAuditEntriesForJob(ctx context.Context, tenantID, jobID uuid.UUID) ([]AuditEntry, error) {
	// Engine Law 4: Tenant ID is strictly enforced on audit retrieval.
	query := `
	SELECT id, ts, tenant_id, actor, action, job_id, from_state, to_state, details
	FROM audit_log
	WHERE job_id = $1 AND tenant_id = $2
	ORDER BY id ASC;`

	rows, err := s.Pool.Query(ctx, query, jobID, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []AuditEntry
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.ID, &a.Timestamp, &a.TenantID, &a.Actor, &a.Action, &a.JobID, &a.FromState, &a.ToState, &a.Details); err != nil {
			return nil, err
		}
		entries = append(entries, a)
	}
	return entries, rows.Err()
}
