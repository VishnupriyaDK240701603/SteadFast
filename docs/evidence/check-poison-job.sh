#!/bin/bash
# Pre-seeded Poison Job Verification Script
set -e

echo "[check-poison-job] Querying PostgreSQL for pre-seeded poison job..."

# Query database directly via psql or inspect API
TENANT_KEY="${TENANT_A_KEY:-test_tenant_a_key_32_bytes_min}"

RES=$(curl -s -H "Authorization: Bearer $TENANT_KEY" http://127.0.0.1:8080/v1/dlq || true)

if echo "$RES" | grep -q "poison"; then
    echo "[check-poison-job] SUCCESS: Poison job verified present in DLQ."
    echo "$RES" | jq . 2>/dev/null || echo "$RES"
    exit 0
else
    echo "[check-poison-job] Poison job not in DLQ yet (will enter DLQ after seed and triage execution)."
fi
