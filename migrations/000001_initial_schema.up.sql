-- Table: tenants
CREATE TABLE IF NOT EXISTS tenants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(64) UNIQUE NOT NULL,
    api_key_hash VARCHAR(64) UNIQUE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Table: jobs
CREATE TABLE IF NOT EXISTS jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    job_type VARCHAR(64) NOT NULL,
    payload JSONB NOT NULL,
    payload_hash VARCHAR(64) NOT NULL,
    idempotency_key VARCHAR(128),
    state VARCHAR(16) NOT NULL DEFAULT 'QUEUED' CHECK (state IN ('QUEUED', 'LEASED', 'SUCCEEDED', 'DEAD')),
    priority INT NOT NULL DEFAULT 100,
    run_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts INT NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INT NOT NULL DEFAULT 5 CHECK (max_attempts >= 1 AND max_attempts <= 20),
    fencing_token BIGINT NOT NULL DEFAULT 0 CHECK (fencing_token >= 0),
    worker_id VARCHAR(128),
    lease_expires_at TIMESTAMPTZ,
    last_error TEXT,
    dead_reason TEXT,
    result JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);

-- Table: idempotency_keys
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    key VARCHAR(128) NOT NULL,
    payload_hash VARCHAR(64) NOT NULL,
    job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    response_code INT NOT NULL DEFAULT 202,
    response_body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL DEFAULT now() + interval '24 hours',
    PRIMARY KEY (tenant_id, key)
);

-- Table: job_attempts
CREATE TABLE IF NOT EXISTS job_attempts (
    id BIGSERIAL PRIMARY KEY,
    job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    attempt_no INT NOT NULL,
    fencing_token BIGINT NOT NULL,
    worker_id VARCHAR(128) NOT NULL,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    outcome VARCHAR(32) CHECK (outcome IS NULL OR outcome IN ('succeeded', 'failed_retryable', 'failed_permanent', 'lease_expired')),
    error_class VARCHAR(64),
    error_message TEXT
);

-- Table: audit_log (append-only)
CREATE TABLE IF NOT EXISTS audit_log (
    id BIGSERIAL PRIMARY KEY,
    ts TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id UUID REFERENCES tenants(id) ON DELETE SET NULL,
    actor VARCHAR(128) NOT NULL DEFAULT 'system',
    action VARCHAR(64) NOT NULL,
    job_id UUID,
    from_state VARCHAR(16),
    to_state VARCHAR(16),
    details JSONB
);

-- Indexes from Appendix B
CREATE INDEX IF NOT EXISTS idx_jobs_claim_queue 
    ON jobs (tenant_id, priority, run_at, id) 
    WHERE state = 'QUEUED';

CREATE INDEX IF NOT EXISTS idx_jobs_reaper_leased 
    ON jobs (lease_expires_at) 
    WHERE state = 'LEASED';

CREATE INDEX IF NOT EXISTS idx_job_attempts_job_id 
    ON job_attempts (job_id);

-- Trigger function: Audit state transitions
CREATE OR REPLACE FUNCTION audit_job_state_transition() 
RETURNS TRIGGER AS $$
DECLARE
    v_actor VARCHAR(128);
BEGIN
    IF OLD.state IS DISTINCT FROM NEW.state THEN
        v_actor := current_setting('steadfast.actor', true);
        IF v_actor IS NULL OR v_actor = '' THEN
            v_actor := 'system';
        END IF;

        INSERT INTO audit_log (ts, tenant_id, actor, action, job_id, from_state, to_state, details)
        VALUES (
            now(),
            NEW.tenant_id,
            v_actor,
            'state_transition',
            NEW.id,
            OLD.state,
            NEW.state,
            jsonb_build_object(
                'fencing_token', NEW.fencing_token,
                'attempts', NEW.attempts,
                'worker_id', NEW.worker_id,
                'dead_reason', NEW.dead_reason
            )
        );
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_audit_job_state ON jobs;
CREATE TRIGGER trg_audit_job_state
    AFTER UPDATE OF state ON jobs
    FOR EACH ROW
    EXECUTE FUNCTION audit_job_state_transition();

-- Roles and Privileges
DO $$
DECLARE
    v_app_pw text := coalesce(nullif(current_setting('steadfast.app_password', true), ''), 'steadfast_app_dev_password');
    v_ro_pw text := coalesce(nullif(current_setting('steadfast.ro_password', true), ''), 'fake_ro_password_for_local_demo');
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'steadfast_app') THEN
        EXECUTE format('CREATE ROLE steadfast_app WITH LOGIN PASSWORD %L', v_app_pw);
    ELSE
        EXECUTE format('ALTER ROLE steadfast_app WITH PASSWORD %L', v_app_pw);
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'steadfast_checker_ro') THEN
        EXECUTE format('CREATE ROLE steadfast_checker_ro WITH LOGIN PASSWORD %L', v_ro_pw);
    ELSE
        EXECUTE format('ALTER ROLE steadfast_checker_ro WITH PASSWORD %L', v_ro_pw);
    END IF;
END $$;

-- Grant permissions to steadfast_app (least privilege on its tables)
GRANT CONNECT ON DATABASE steadfast TO steadfast_app;
GRANT USAGE ON SCHEMA public TO steadfast_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenants, jobs, idempotency_keys, job_attempts TO steadfast_app;
GRANT SELECT, INSERT ON audit_log TO steadfast_app;
REVOKE UPDATE, DELETE ON audit_log FROM steadfast_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO steadfast_app;

-- Grant permissions to steadfast_checker_ro (read-only audit role: SELECT only on jobs and job_attempts)
GRANT CONNECT ON DATABASE steadfast TO steadfast_checker_ro;
GRANT USAGE ON SCHEMA public TO steadfast_checker_ro;
GRANT SELECT ON jobs, job_attempts TO steadfast_checker_ro;
REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA public FROM steadfast_checker_ro;
REVOKE SELECT ON tenants, idempotency_keys, audit_log FROM steadfast_checker_ro;
