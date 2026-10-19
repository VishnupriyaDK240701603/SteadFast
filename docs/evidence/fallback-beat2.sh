#!/bin/bash
# Fallback Script for Beat 2: Worker Hard Crash and Database Kill
# Use this script if the automated demo runner is interrupted or for manual rehearsal.
set -e

echo "[fallback-beat2] Executing Beat 2 via ctl chaos commands..."

# 1. Kill workers
echo "[fallback-beat2] Sending SIGKILL to active worker containers..."
go run ./cmd/ctl chaos kill-workers
sleep 2

# 2. Kill database and redis
echo "[fallback-beat2] Sending SIGKILL to PostgreSQL and Redis containers..."
go run ./cmd/ctl chaos kill-postgres
go run ./cmd/ctl chaos kill-redis
sleep 2

# 3. Restart database and redis
echo "[fallback-beat2] Restarting PostgreSQL and Redis containers..."
go run ./cmd/ctl chaos start-postgres
go run ./cmd/ctl chaos start-redis

echo "[fallback-beat2] Waiting for PostgreSQL readiness..."
for i in {1..30}; do
    if go run ./cmd/ctl up > /dev/null 2>&1; then
        echo "[fallback-beat2] PostgreSQL is healthy and accepting connections."
        break
    fi
    sleep 1
done

echo "[fallback-beat2] Beat 2 fallback completed successfully."
