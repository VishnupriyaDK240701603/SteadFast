package breaker

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type State string

const (
	StateClosed   State = "CLOSED"
	StateOpen     State = "OPEN"
	StateHalfOpen State = "HALF_OPEN"
)

type BreakerConfig struct {
	MinSamples      int
	FailureRatio    float64
	CooldownSeconds int
}

func DefaultConfig() BreakerConfig {
	return BreakerConfig{
		MinSamples:      10,
		FailureRatio:    0.5,
		CooldownSeconds: 10,
	}
}

// Breaker tracks per-job-type circuit breaker states directly in Postgres
type Breaker struct {
	cfg BreakerConfig
}

func New(cfg BreakerConfig) *Breaker {
	if cfg.MinSamples <= 0 {
		cfg.MinSamples = 10
	}
	if cfg.FailureRatio <= 0 {
		cfg.FailureRatio = 0.5
	}
	if cfg.CooldownSeconds <= 0 {
		cfg.CooldownSeconds = 10
	}
	return &Breaker{cfg: cfg}
}

// RecordCompletion is called in the same transaction as CompleteJob
func (b *Breaker) RecordCompletion(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, jobType string) error {
	// Ensure row exists
	_, err := tx.Exec(ctx, `
		INSERT INTO circuit_breakers (tenant_id, job_type, state, total_samples, unavailable_failures, cooldown_seconds)
		VALUES ($1, $2, 'CLOSED', 0, 0, $3)
		ON CONFLICT (tenant_id, job_type) DO NOTHING;
	`, tenantID, jobType, b.cfg.CooldownSeconds)
	if err != nil {
		return err
	}

	var state string
	query := `
	SELECT state
	FROM circuit_breakers
	WHERE tenant_id = $1 AND job_type = $2
	FOR UPDATE;`

	err = tx.QueryRow(ctx, query, tenantID, jobType).Scan(&state)
	if err != nil {
		return err
	}

	if state == string(StateHalfOpen) {
		// Probe succeeded! Close the circuit breaker
		_, err = tx.Exec(ctx, `
			UPDATE circuit_breakers SET
				state = 'CLOSED',
				consecutive_failures = 0,
				total_samples = 0,
				unavailable_failures = 0,
				opened_at = NULL,
				probe_in_flight = false,
				updated_at = now()
			WHERE tenant_id = $1 AND job_type = $2;
		`, tenantID, jobType)
		if err != nil {
			return err
		}

		// Audit log transition
		_, err = tx.Exec(ctx, `
			INSERT INTO audit_log (ts, tenant_id, actor, action, from_state, to_state, details)
			VALUES (now(), $1, 'breaker', 'breaker_closed', 'HALF_OPEN', 'CLOSED', jsonb_build_object('job_type', $2::text));
		`, tenantID, jobType)
		if err != nil {
			return err
		}
	} else if state == string(StateClosed) {
		_, err = tx.Exec(ctx, `
			UPDATE circuit_breakers SET
				total_samples = total_samples + 1,
				updated_at = now()
			WHERE tenant_id = $1 AND job_type = $2;
		`, tenantID, jobType)
		if err != nil {
			return err
		}
	}

	return nil
}

// RecordFailure is called in the same transaction as FailJob
func (b *Breaker) RecordFailure(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, jobType, errorClass string, retryable bool) error {
	// Permanent errors (like poison payload) never count toward opening (Requirement 4)
	if !retryable || errorClass != "downstream_unavailable" {
		return nil
	}

	// Ensure row exists
	_, err := tx.Exec(ctx, `
		INSERT INTO circuit_breakers (tenant_id, job_type, state, total_samples, unavailable_failures, cooldown_seconds)
		VALUES ($1, $2, 'CLOSED', 0, 0, $3)
		ON CONFLICT (tenant_id, job_type) DO NOTHING;
	`, tenantID, jobType, b.cfg.CooldownSeconds)
	if err != nil {
		return err
	}

	var state string
	var totalSamples int
	var unavailableFailures int

	query := `
	SELECT state, total_samples, unavailable_failures
	FROM circuit_breakers
	WHERE tenant_id = $1 AND job_type = $2
	FOR UPDATE;`

	err = tx.QueryRow(ctx, query, tenantID, jobType).Scan(&state, &totalSamples, &unavailableFailures)
	if err != nil {
		return err
	}

	if state == string(StateHalfOpen) {
		// Probe failed! Re-open circuit breaker
		_, err = tx.Exec(ctx, `
			UPDATE circuit_breakers SET
				state = 'OPEN',
				opened_at = now(),
				probe_in_flight = false,
				updated_at = now()
			WHERE tenant_id = $1 AND job_type = $2;
		`, tenantID, jobType)
		if err != nil {
			return err
		}

		// Audit log transition
		_, err = tx.Exec(ctx, `
			INSERT INTO audit_log (ts, tenant_id, actor, action, from_state, to_state, details)
			VALUES (now(), $1, 'breaker', 'breaker_reopened', 'HALF_OPEN', 'OPEN', jsonb_build_object('job_type', $2::text));
		`, tenantID, jobType)
		if err != nil {
			return err
		}
		return nil
	}

	if state == string(StateClosed) {
		totalSamples++
		unavailableFailures++

		shouldOpen := totalSamples >= b.cfg.MinSamples &&
			(float64(unavailableFailures)/float64(totalSamples)) >= b.cfg.FailureRatio

		if shouldOpen {
			_, err = tx.Exec(ctx, `
				UPDATE circuit_breakers SET
					state = 'OPEN',
					opened_at = now(),
					total_samples = $3,
					unavailable_failures = $4,
					probe_in_flight = false,
					updated_at = now()
				WHERE tenant_id = $1 AND job_type = $2;
			`, tenantID, jobType, totalSamples, unavailableFailures)
			if err != nil {
				return err
			}

			// Audit log transition
			_, err = tx.Exec(ctx, `
				INSERT INTO audit_log (ts, tenant_id, actor, action, from_state, to_state, details)
				VALUES (now(), $1, 'breaker', 'breaker_opened', 'CLOSED', 'OPEN', 
					jsonb_build_object('job_type', $2::text, 'samples', $3::int, 'unavailable_failures', $4::int));
			`, tenantID, jobType, totalSamples, unavailableFailures)
			if err != nil {
				return err
			}
		} else {
			_, err = tx.Exec(ctx, `
				UPDATE circuit_breakers SET
					total_samples = $3,
					unavailable_failures = $4,
					updated_at = now()
				WHERE tenant_id = $1 AND job_type = $2;
			`, tenantID, jobType, totalSamples, unavailableFailures)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
