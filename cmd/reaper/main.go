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
			runReaperTick(ctx, logger, pool, st)
		}
	}
}

func runReaperTick(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, st *store.Store) {
	// Acquire advisory lock
	var lockAcquired bool
	err := pool.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", reaperAdvisoryLockID).Scan(&lockAcquired)
	if err != nil {
		logger.Error("failed to query advisory lock", "error", err)
		return
	}

	if !lockAcquired {
		// Another reaper instance holds the lock
		return
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", reaperAdvisoryLockID)
	}()

	reaped, err := st.ReapExpiredLeases(ctx)
	if err != nil {
		logger.Error("error during lease reaping", "error", err)
		return
	}

	if reaped > 0 {
		logger.Info("reaped expired leases", "count", reaped)
	}
}
