package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"steadfast/internal/config"
	"steadfast/internal/store"
)

const reaperAdvisoryLockID = 987654321

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("reaper failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	st := store.NewStore(pool)
	interval := cfg.ReaperInterval
	if interval <= 0 {
		interval = 1000 * time.Millisecond
	}

	logger.Info("reaper started", "interval_ms", interval.Milliseconds())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("reaper shutting down gracefully")
			return
		case <-ticker.C:
			_, _, _ = runReaperTick(ctx, logger, pool, st)
		}
	}
}

// runReaperTick acquires a dedicated pinned session from the connection pool,
// takes the session-level advisory lock, executes the reap query on that exact session,
// and unlocks the advisory lock before releasing the session back to the pool.
func runReaperTick(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, st *store.Store) (bool, int64, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		logger.Error("failed to acquire dedicated connection for reaper tick", "error", err)
		return false, 0, err
	}
	defer conn.Release()

	// Acquire advisory lock on the pinned connection session
	var lockAcquired bool
	err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", reaperAdvisoryLockID).Scan(&lockAcquired)
	if err != nil {
		logger.Error("failed to query advisory lock", "error", err)
		return false, 0, err
	}

	if !lockAcquired {
		// Another reaper instance currently holds the leader lock
		return false, 0, nil
	}

	defer func() {
		// Release the advisory lock on the exact same pinned connection
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", reaperAdvisoryLockID)
	}()

	// Execute the reap query on the exact same pinned connection holding the lock
	reaped, err := st.ReapExpiredLeasesExecutor(ctx, conn)
	if err != nil {
		logger.Error("error during lease reaping", "error", err)
		return true, 0, err
	}

	if reaped > 0 {
		logger.Info("reaped expired leases", "count", reaped)
	}

	return true, reaped, nil
}
