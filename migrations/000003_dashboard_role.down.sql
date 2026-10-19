-- Migration: 000003_dashboard_role.down.sql
-- Purpose: Revoke privileges and drop steadfast_dashboard_ro role

REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM steadfast_dashboard_ro;
REVOKE ALL PRIVILEGES ON SCHEMA public FROM steadfast_dashboard_ro;
REVOKE CONNECT ON DATABASE steadfast FROM steadfast_dashboard_ro;
DROP ROLE IF EXISTS steadfast_dashboard_ro;
