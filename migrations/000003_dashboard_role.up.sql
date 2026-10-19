-- Migration: 000003_dashboard_role.up.sql
-- Purpose: Create steadfast_dashboard_ro role with least privilege read access (F16, Requirement 10)

DO $$
DECLARE
    v_dash_pw text := coalesce(nullif(current_setting('steadfast.dashboard_password', true), ''), 'fake_dashboard_ro_password');
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'steadfast_dashboard_ro') THEN
        EXECUTE format('CREATE ROLE steadfast_dashboard_ro WITH LOGIN PASSWORD %L', v_dash_pw);
    ELSE
        EXECUTE format('ALTER ROLE steadfast_dashboard_ro WITH PASSWORD %L', v_dash_pw);
    END IF;
END $$;

GRANT CONNECT ON DATABASE steadfast TO steadfast_dashboard_ro;
GRANT USAGE ON SCHEMA public TO steadfast_dashboard_ro;

-- Dashboard reads jobs, attempts, audit_log, and only non-secret tenant columns (id, name, created_at).
-- NO SELECT permission on api_key_hash!
GRANT SELECT ON jobs, job_attempts, audit_log TO steadfast_dashboard_ro;
GRANT SELECT (id, name, created_at) ON tenants TO steadfast_dashboard_ro;
REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA public FROM steadfast_dashboard_ro;
