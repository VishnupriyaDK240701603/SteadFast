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
	"steadfast/internal/breaker"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound             = errors.New("record not found")
	ErrConflict             = errors.New("record conflict")
	ErrTenantMismatch       = errors.New("tenant authorization mismatch")
	ErrIdempotencyConflict  = errors.New("idempotency key conflict: key already used with different payload")
	ErrLeaseLost            = errors.New("lease lost: fencing token stale or job not leased")
	ErrStaleFencingToken    = errors.New("stale fencing token: state changed or token mismatch")
	ErrNotDead              = errors.New("job is not dead")
)

type Tenant struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	APIKeyHash string    `json:"api_key_hash"`
	CreatedAt  time.Time `json:"created_at"`
}

type Job struct {
	ID             uuid.UUID       `json:"id"`
	TenantID       uuid.UUID       `json:"tenant_id"`
	JobType        string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	PayloadHash    string          `json:"payload_hash"`
	IdempotencyKey *string         `json:"idempotency_key,omitempty"`
	State          string          `json:"state"`
	Priority       int             `json:"priority"`
	RunAt          time.Time       `json:"run_at"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	FencingToken   int64           `json:"fencing_token"`
	WorkerID       *string         `json:"worker_id,omitempty"`
	LeaseExpiresAt *time.Time      `json:"lease_expires_at,omitempty"`
	LastError      *string         `json:"last_error,omitempty"`
	DeadReason     *string         `json:"dead_reason,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
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

type ClaimedJob struct {
	ID           uuid.UUID       `json:"id"`
	Type         string          `json:"type"`
	Payload      json.RawMessage `json:"payload"`
	FencingToken int64           `json:"fencing_token"`
	Attempt      int             `json:"attempt"`
	MaxAttempts  int             `json:"max_attempts"`
	LeaseSeconds int             `json:"lease_seconds"`
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
	Details   map[string]any
}

type Store struct {
	Pool *pgxpool.Pool
	Breaker *breaker.Breaker
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool, Breaker: breaker.New(breaker.DefaultConfig())}
}

func Connect(ctx context.Context, connString string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}

	config.MaxConns = 15
	config.MinConns = 1
	config.MaxConnLifetime = 30 * time.Minute
	config.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, config)
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
	if job.ID == uuid.Nil {
		job.ID = uuid.New()
	}
	if job.State == "" {
		job.State = "QUEUED"
	}
	if job.Priority == 0 {
		job.Priority = 100
	}
	if job.MaxAttempts == 0 {
		job.MaxAttempts = 5
	}
	if job.RunAt.IsZero() {
		job.RunAt = time.Now().UTC()
	}

	query := `
	INSERT INTO jobs (id, tenant_id, job_type, payload, payload_hash, idempotency_key, state, priority, run_at, max_attempts)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	RETURNING created_at, updated_at;`

	err := s.Pool.QueryRow(ctx, query,
		job.ID, job.TenantID, job.JobType, job.Payload, job.PayloadHash,
		job.IdempotencyKey, job.State, job.Priority, job.RunAt, job.MaxAttempts,
	).Scan(&job.CreatedAt, &job.UpdatedAt)

	if err != nil {
		return nil, fmt.Errorf("failed to insert job: %w", err)
	}

	return job, nil
}

func (s *Store) GetJob(ctx context.Context, tenantID, jobID uuid.UUID) (*Job, error) {
	return s.GetJobByID(ctx, tenantID, jobID)
}

func (s *Store) GetJobByID(ctx context.Context, tenantID, jobID uuid.UUID) (*Job, error) {
	query := `
	SELECT id, tenant_id, job_type, payload, payload_hash, idempotency_key, state, priority, run_at,
	       attempts, max_attempts, fencing_token, worker_id, lease_expires_at, last_error, dead_reason,
	       result, created_at, updated_at, finished_at
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
		return nil, fmt.Errorf("failed to get job by id: %w", err)
	}

	return j, nil
}

func (s *Store) GetJobWithAttempts(ctx context.Context, tenantID, jobID uuid.UUID) (*JobWithAttempts, error) {
	j, err := s.GetJobByID(ctx, tenantID, jobID)
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
		Job:         *j,
		JobAttempts: attempts,
	}, nil
}

func (s *Store) ListJobs(ctx context.Context, tenantID uuid.UUID, state string, limit int, cursor string) ([]Job, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	var cursorCreatedAt time.Time
	var cursorID uuid.UUID

	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err == nil {
			parts := strings.Split(string(decoded), "|")
			if len(parts) == 2 {
				t, errT := time.Parse(time.RFC3339Nano, parts[0])
				u, errU := uuid.Parse(parts[1])
				if errT == nil && errU == nil {
					cursorCreatedAt = t
					cursorID = u
				}
			}
		}
	}

	query := `
	SELECT id, tenant_id, job_type, payload, payload_hash, idempotency_key, state, priority, run_at,
	       attempts, max_attempts, fencing_token, worker_id, lease_expires_at,
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

// ClaimJobs claims a batch of queued jobs under lease and opens the corresponding job_attempts rows
func (s *Store) ClaimJobs(ctx context.Context, tenantID uuid.UUID, workerID string, types []string, maxBatch int, leaseDuration time.Duration) ([]ClaimedJob, error) {
	if maxBatch <= 0 {
		maxBatch = 10
	}
	if maxBatch > 100 {
		maxBatch = 100
	}

	leaseMillis := leaseDuration.Milliseconds()
	if leaseMillis <= 0 {
		leaseMillis = 10000
	}

	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	excludedTypes := []string{}
	exRows, err := tx.Query(ctx, `
		SELECT job_type FROM circuit_breakers
		WHERE tenant_id = $1 AND (
			(state = 'OPEN' AND now() < opened_at + (cooldown_seconds * interval '1 second'))
			OR probe_in_flight = true
		);
	`, tenantID)
	if err == nil {
		for exRows.Next() {
			var jt string
			if err := exRows.Scan(&jt); err == nil {
				excludedTypes = append(excludedTypes, jt)
			}
		}
		exRows.Close()
	}

	probeTypes := map[string]bool{}
	prRows, err := tx.Query(ctx, `
		SELECT job_type FROM circuit_breakers
		WHERE tenant_id = $1 AND (
			(state = 'OPEN' AND now() >= opened_at + (cooldown_seconds * interval '1 second') AND NOT probe_in_flight)
			OR (state = 'HALF_OPEN' AND NOT probe_in_flight)
		);
	`, tenantID)
	if err == nil {
		for prRows.Next() {
			var jt string
			if err := prRows.Scan(&jt); err == nil {
				probeTypes[jt] = true
			}
		}
		prRows.Close()
	}

	query := `
	WITH picked AS (
		SELECT id FROM jobs
		WHERE tenant_id = $1
		  AND state = 'QUEUED'
		  AND run_at <= now()
		  AND (cardinality($2::text[]) = 0 OR job_type = ANY($2))
		  AND (cardinality($6::text[]) = 0 OR NOT (job_type = ANY($6)))
		ORDER BY priority ASC, run_at ASC, id ASC
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	), claimed AS (
		UPDATE jobs j SET
			state = 'LEASED',
			worker_id = $4,
			lease_expires_at = now() + ($5 * interval '1 millisecond'),
			fencing_token = j.fencing_token + 1,
			attempts = j.attempts + 1,
			updated_at = now()
		FROM picked WHERE j.id = picked.id
		RETURNING j.id, j.job_type, j.payload, j.fencing_token, j.attempts, j.max_attempts
	), opened AS (
		INSERT INTO job_attempts (job_id, attempt_no, fencing_token, worker_id, started_at)
		SELECT id, attempts, fencing_token, $4, now() FROM claimed
		RETURNING job_id
	)
	SELECT id, job_type, payload, fencing_token, attempts, max_attempts FROM claimed;`

	rows, err := tx.Query(ctx, query, tenantID, types, maxBatch, workerID, leaseMillis, excludedTypes)
	if err != nil {
		return nil, fmt.Errorf("failed to claim jobs: %w", err)
	}
	defer rows.Close()

	leaseSeconds := int(leaseDuration.Seconds())
	if leaseSeconds <= 0 {
		leaseSeconds = 10
	}

	claimedList := []ClaimedJob{}
	for rows.Next() {
		var c ClaimedJob
		if err := rows.Scan(&c.ID, &c.Type, &c.Payload, &c.FencingToken, &c.Attempt, &c.MaxAttempts); err != nil {
			return nil, fmt.Errorf("failed to scan claimed job: %w", err)
		}
		c.LeaseSeconds = leaseSeconds
		claimedList = append(claimedList, c)
	}

	// If any claimed job is of a probe type, mark breaker as HALF_OPEN with probe_in_flight = true
	// and if multiple jobs of that probe type were claimed, return only 1 probe job and revert others
	if len(probeTypes) > 0 {
		filteredList := []ClaimedJob{}
		probedMap := map[string]bool{}

		for _, c := range claimedList {
			if probeTypes[c.Type] {
				if !probedMap[c.Type] {
					probedMap[c.Type] = true
					filteredList = append(filteredList, c)
					_, _ = tx.Exec(ctx, `
						UPDATE circuit_breakers SET
							state = 'HALF_OPEN',
							probe_in_flight = true,
							updated_at = now()
						WHERE tenant_id = $1 AND job_type = $2;
					`, tenantID, c.Type)
				} else {
					// Revert extra probe jobs back to QUEUED
					_, _ = tx.Exec(ctx, `
						UPDATE jobs SET
							state = 'QUEUED',
							worker_id = NULL,
							lease_expires_at = NULL,
							fencing_token = fencing_token - 1,
							attempts = attempts - 1,
							updated_at = now()
						WHERE id = $1;
					`, c.ID)
					_, _ = tx.Exec(ctx, `DELETE FROM job_attempts WHERE job_id = $1 AND attempt_no = $2;`, c.ID, c.Attempt)
				}
			} else {
				filteredList = append(filteredList, c)
			}
		}
		claimedList = filteredList
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("failed to commit claim transaction: %w", err)
	}

	return claimedList, nil
}

// HeartbeatJob extends lease for a leased job if fencing token matches
func (s *Store) HeartbeatJob(ctx context.Context, tenantID, jobID uuid.UUID, fencingToken int64, leaseDuration time.Duration, disableFencing bool) error {
	leaseMillis := leaseDuration.Milliseconds()
	if leaseMillis <= 0 {
		leaseMillis = 10000
	}

	query := `
	UPDATE jobs SET
		lease_expires_at = now() + ($4 * interval '1 millisecond'),
		updated_at = now()
	WHERE id = $1
	  AND tenant_id = $2
	  AND (fencing_token = $3 OR $5 = true)
	  AND state = 'LEASED';`

	cmdTag, err := s.Pool.Exec(ctx, query, jobID, tenantID, fencingToken, leaseMillis, disableFencing)
	if err != nil {
		return fmt.Errorf("failed to heartbeat job: %w", err)
	}

	if cmdTag.RowsAffected() == 0 {
		// Determine whether 404 or 409
		var exists bool
		_ = s.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE id = $1 AND tenant_id = $2)", jobID, tenantID).Scan(&exists)
		if !exists {
			return ErrNotFound
		}
		return ErrLeaseLost
	}

	return nil
}

// CompleteJob marks a job SUCCEEDED and closes the attempt row in a single transaction
func (s *Store) CompleteJob(ctx context.Context, tenantID, jobID uuid.UUID, fencingToken int64, workerID string, result json.RawMessage, disableFencing bool) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	resJSON := result
	if len(resJSON) == 0 {
		resJSON = []byte("{}")
	}

	query := `
	UPDATE jobs SET
		state = 'SUCCEEDED',
		result = $3,
		finished_at = now(),
		lease_expires_at = NULL,
		updated_at = now()
	WHERE id = $1
	  AND tenant_id = $2
	  AND (fencing_token = $4 OR $5 = true)
	  AND state = 'LEASED';`

	cmdTag, err := tx.Exec(ctx, query, jobID, tenantID, resJSON, fencingToken, disableFencing)
	if err != nil {
		return fmt.Errorf("failed to complete job: %w", err)
	}

	if cmdTag.RowsAffected() == 0 {
		var exists bool
		_ = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE id = $1 AND tenant_id = $2)", jobID, tenantID).Scan(&exists)
		if !exists {
			return ErrNotFound
		}
		return ErrStaleFencingToken
	}

	// Close attempt row
	closeAttemptQuery := `
	UPDATE job_attempts SET
		finished_at = now(),
		outcome = 'succeeded'
	WHERE job_id = $1
	  AND (fencing_token = $2 OR $3 = true)
	  AND outcome IS NULL;`

	_, err = tx.Exec(ctx, closeAttemptQuery, jobID, fencingToken, disableFencing)
	if err != nil {
		return fmt.Errorf("failed to close job attempt: %w", err)
	}

	// Breaker record completion
	if s.Breaker != nil {
		var jobType string
		_ = tx.QueryRow(ctx, "SELECT job_type FROM jobs WHERE id = $1", jobID).Scan(&jobType)
		if jobType != "" {
			_ = s.Breaker.RecordCompletion(ctx, tx, tenantID, jobType)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit complete transaction: %w", err)
	}

	return nil
}

// FailJob records a failure, either scheduling retry with backoff or moving to DEAD
func (s *Store) FailJob(ctx context.Context, tenantID, jobID uuid.UUID, fencingToken int64, errorClass, message string, retryable bool, retryDelay time.Duration, disableFencing bool) (string, *time.Time, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the row to check state and token
	var state string
	var currentToken int64
	var attempts int
	var maxAttempts int

	var jobType string
	checkQuery := `
	SELECT state, fencing_token, attempts, max_attempts, job_type
	FROM jobs
	WHERE id = $1 AND tenant_id = $2
	FOR UPDATE;`

	err = tx.QueryRow(ctx, checkQuery, jobID, tenantID).Scan(&state, &currentToken, &attempts, &maxAttempts, &jobType)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, ErrNotFound
		}
		return "", nil, err
	}

	if state != "LEASED" || (!disableFencing && currentToken != fencingToken) {
		return "", nil, ErrStaleFencingToken
	}

	delayMillis := retryDelay.Milliseconds()
	if delayMillis < 0 {
		delayMillis = 0
	}

	fullErr := fmt.Sprintf("[%s] %s", errorClass, message)

	if retryable && attempts < maxAttempts {
		// Return to QUEUED with backoff delay
		nextState := "QUEUED"
		var runAt time.Time

		updateQuery := `
		UPDATE jobs SET
			state = 'QUEUED',
			run_at = now() + ($3 * interval '1 millisecond'),
			worker_id = NULL,
			lease_expires_at = NULL,
			last_error = $4,
			updated_at = now()
		WHERE id = $1 AND tenant_id = $2
		RETURNING run_at;`

		if err := tx.QueryRow(ctx, updateQuery, jobID, tenantID, delayMillis, fullErr).Scan(&runAt); err != nil {
			return "", nil, fmt.Errorf("failed to update job to QUEUED: %w", err)
		}

		attemptQuery := `
		UPDATE job_attempts SET
			finished_at = now(),
			outcome = 'failed_retryable',
			error_class = $3,
			error_message = $4
		WHERE job_id = $1
		  AND (fencing_token = $2 OR $5 = true)
		  AND outcome IS NULL;`

		if _, err := tx.Exec(ctx, attemptQuery, jobID, fencingToken, errorClass, message, disableFencing); err != nil {
			return "", nil, fmt.Errorf("failed to update attempt row: %w", err)
		}

		if s.Breaker != nil && jobType != "" {
			_ = s.Breaker.RecordFailure(ctx, tx, tenantID, jobType, errorClass, retryable)
		}

		if err := tx.Commit(ctx); err != nil {
			return "", nil, err
		}

		return nextState, &runAt, nil
	}

	// Move to DEAD (either permanent or max attempts exhausted)
	nextState := "DEAD"
	deadReason := "permanent_error"
	if attempts >= maxAttempts {
		deadReason = "max_attempts_exhausted"
	}

	updateQuery := `
	UPDATE jobs SET
		state = 'DEAD',
		dead_reason = $3,
		finished_at = now(),
		worker_id = NULL,
		lease_expires_at = NULL,
		last_error = $4,
		updated_at = now()
	WHERE id = $1 AND tenant_id = $2;`

	if _, err := tx.Exec(ctx, updateQuery, jobID, tenantID, deadReason, fullErr); err != nil {
		return "", nil, fmt.Errorf("failed to update job to DEAD: %w", err)
	}

	attemptQuery := `
	UPDATE job_attempts SET
		finished_at = now(),
		outcome = 'failed_permanent',
		error_class = $3,
		error_message = $4
		WHERE job_id = $1
		  AND (fencing_token = $2 OR $5 = true)
		  AND outcome IS NULL;`

	if _, err := tx.Exec(ctx, attemptQuery, jobID, fencingToken, errorClass, message, disableFencing); err != nil {
		return "", nil, fmt.Errorf("failed to update attempt row: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", nil, err
	}

	return nextState, nil, nil
}

type QueryExec interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// ReapExpiredLeases executes the Appendix B query to recover or dead-letter expired jobs using the store pool
func (s *Store) ReapExpiredLeases(ctx context.Context) (int64, error) {
	return s.ReapExpiredLeasesExecutor(ctx, s.Pool)
}

// ReapExpiredLeasesExecutor executes the reaper query on a specific pinned connection or pool
func (s *Store) ReapExpiredLeasesExecutor(ctx context.Context, executor QueryExec) (int64, error) {
	query := `
	WITH expired AS (
		SELECT id FROM jobs
		WHERE state = 'LEASED' AND lease_expires_at < now()
		FOR UPDATE SKIP LOCKED
	), moved AS (
		UPDATE jobs j SET
			state = CASE WHEN j.attempts >= j.max_attempts THEN 'DEAD' ELSE 'QUEUED' END,
			dead_reason = CASE WHEN j.attempts >= j.max_attempts THEN 'lease_expired_max_attempts' END,
			finished_at = CASE WHEN j.attempts >= j.max_attempts THEN now() END,
			worker_id = NULL,
			lease_expires_at = NULL,
			run_at = now(),
			updated_at = now()
		FROM expired e WHERE j.id = e.id
		RETURNING j.id, j.fencing_token
	)
	UPDATE job_attempts a SET
		finished_at = now(),
		outcome = 'lease_expired'
	FROM moved m
	WHERE a.job_id = m.id
	  AND a.fencing_token = m.fencing_token
	  AND a.outcome IS NULL;`

	cmdTag, err := executor.Exec(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("reaper query failed: %w", err)
	}

	return cmdTag.RowsAffected(), nil
}

// GetDeadLetterJobs returns dead-lettered jobs for a tenant
func (s *Store) GetDeadLetterJobs(ctx context.Context, tenantID uuid.UUID, limit int, cursor string) ([]Job, string, error) {
	return s.ListJobs(ctx, tenantID, "DEAD", limit, cursor)
}

// UpdateDeadLetterJobPayload modifies payload for a DEAD job
func (s *Store) UpdateDeadLetterJobPayload(ctx context.Context, tenantID, jobID uuid.UUID, newPayload []byte, newHash string, actor string) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var state string
	err = tx.QueryRow(ctx, "SELECT state FROM jobs WHERE id = $1 AND tenant_id = $2 FOR UPDATE", jobID, tenantID).Scan(&state)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}

	if state != "DEAD" {
		return ErrNotDead
	}

	query := `
	UPDATE jobs SET
		payload = $3,
		payload_hash = $4,
		updated_at = now()
	WHERE id = $1 AND tenant_id = $2;`

	if _, err := tx.Exec(ctx, query, jobID, tenantID, newPayload, newHash); err != nil {
		return fmt.Errorf("failed to update dlq job payload: %w", err)
	}

	// Audit log entry for DLQ edit
	auditQuery := `
	INSERT INTO audit_log (ts, tenant_id, actor, action, job_id, details)
	VALUES (now(), $1, $2, 'dlq_edit_payload', $3, jsonb_build_object('new_payload_hash', $4::text));`

	if _, err := tx.Exec(ctx, auditQuery, tenantID, actor, jobID, newHash); err != nil {
		return fmt.Errorf("failed to insert audit entry: %w", err)
	}

	return tx.Commit(ctx)
}

// ReplayDeadLetterJob resets a DEAD job to QUEUED, preserving ID, idempotency key, and attempts history
func (s *Store) ReplayDeadLetterJob(ctx context.Context, tenantID, jobID uuid.UUID, actor string) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var state string
	err = tx.QueryRow(ctx, "SELECT state FROM jobs WHERE id = $1 AND tenant_id = $2 FOR UPDATE", jobID, tenantID).Scan(&state)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}

	if state != "DEAD" {
		return ErrNotDead
	}

	// Reset attempts = 0, state = 'QUEUED', run_at = now()
	query := `
	UPDATE jobs SET
		state = 'QUEUED',
		attempts = 0,
		run_at = now(),
		dead_reason = NULL,
		last_error = NULL,
		worker_id = NULL,
		lease_expires_at = NULL,
		finished_at = NULL,
		updated_at = now()
	WHERE id = $1 AND tenant_id = $2;`

	if _, err := tx.Exec(ctx, query, jobID, tenantID); err != nil {
		return fmt.Errorf("failed to replay dlq job: %w", err)
	}

	auditQuery := `
	INSERT INTO audit_log (ts, tenant_id, actor, action, job_id, from_state, to_state, details)
	VALUES (now(), $1, $2, 'dlq_replay', $3, 'DEAD', 'QUEUED', jsonb_build_object('reset_attempts', true));`

	if _, err := tx.Exec(ctx, auditQuery, tenantID, actor, jobID); err != nil {
		return fmt.Errorf("failed to insert audit entry: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *Store) UpdateJobState(ctx context.Context, tenantID, jobID uuid.UUID, newState, actor string) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if actor == "" {
		actor = "system"
	}
	_, err = tx.Exec(ctx, fmt.Sprintf("SET LOCAL steadfast.actor = '%s'", actor))
	if err != nil {
		return fmt.Errorf("failed to set actor session variable: %w", err)
	}

	query := `UPDATE jobs SET state = $1, updated_at = now() WHERE id = $2 AND tenant_id = $3;`
	cmdTag, err := tx.Exec(ctx, query, newState, jobID, tenantID)
	if err != nil {
		return fmt.Errorf("failed to update job state: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrNotFound
	}

	return tx.Commit(ctx)
}

func (s *Store) GetAuditEntriesForJob(ctx context.Context, tenantID, jobID uuid.UUID) ([]AuditEntry, error) {
	query := `
	SELECT id, ts, tenant_id, actor, action, job_id, from_state, to_state
	FROM audit_log
	WHERE job_id = $1 AND tenant_id = $2
	ORDER BY id ASC;`

	rows, err := s.Pool.Query(ctx, query, jobID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("failed to query audit log: %w", err)
	}
	defer rows.Close()

	entries := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.TenantID, &e.Actor, &e.Action, &e.JobID, &e.FromState, &e.ToState); err != nil {
			return nil, fmt.Errorf("failed to scan audit entry: %w", err)
		}
		entries = append(entries, e)
	}

	return entries, nil
}
