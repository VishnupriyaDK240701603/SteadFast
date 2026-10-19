-- Table: circuit_breakers
CREATE TABLE IF NOT EXISTS circuit_breakers (
    tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    job_type VARCHAR(64) NOT NULL,
    state VARCHAR(16) NOT NULL DEFAULT 'CLOSED' CHECK (state IN ('CLOSED', 'OPEN', 'HALF_OPEN')),
    consecutive_failures INT NOT NULL DEFAULT 0,
    total_samples INT NOT NULL DEFAULT 0,
    unavailable_failures INT NOT NULL DEFAULT 0,
    opened_at TIMESTAMPTZ,
    probe_in_flight BOOLEAN NOT NULL DEFAULT false,
    cooldown_seconds INT NOT NULL DEFAULT 10,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, job_type)
);

CREATE INDEX IF NOT EXISTS idx_circuit_breakers_lookup
    ON circuit_breakers (tenant_id, job_type, state);

GRANT SELECT, INSERT, UPDATE, DELETE ON circuit_breakers TO steadfast_app;
GRANT SELECT ON circuit_breakers TO steadfast_checker_ro;
