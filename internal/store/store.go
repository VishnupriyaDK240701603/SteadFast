package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound      = errors.New("record not found")
	ErrConflict      = errors.New("record conflict")
	ErrTenantMismatch = errors.New("tenant authorization mismatch")
)

type Tenant struct {
	ID         uuid.UUID
	Name       string
	APIKeyHash string
	CreatedAt  time.Time
}

type Job struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	JobType        string
	Payload        []byte
	PayloadHash    string
	IdempotencyKey *string
	State          string
	Priority       int
	RunAt          time.Time
	Attempts       int
	MaxAttempts    int
	FencingToken   int64
	WorkerID       *string
	LeaseExpiresAt *time.Time
	LastError      *string
	DeadReason     *string
	Result         []byte
	CreatedAt      time.Time
	UpdatedAt      time.Time
	FinishedAt     *time.Time
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
