package config

import (
	"errors"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Env                 string
	DatabaseURL         string
	CheckerDatabaseURL  string
	AdminKey            string
	LeaseDuration       time.Duration
	HeartbeatInterval   time.Duration
	ReaperInterval      time.Duration
	IdempotencyTTL      time.Duration
	DefaultMaxAttempts  int
	Port                int
	DisableFencing      bool
	DisableIdempotency  bool
	BackoffBase         time.Duration
	BackoffCap          time.Duration
	RateLimitRPS        int
}

func Load() (*Config, error) {
	env := getEnv("STEADFAST_ENV", "demo")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://steadfast_user:fake_pg_password_for_local_demo_only@127.0.0.1:5432/steadfast?sslmode=disable"
	}

	checkerURL := os.Getenv("CHECKER_DATABASE_URL")
	if checkerURL == "" {
		checkerURL = "postgres://steadfast_checker_ro:fake_ro_password_for_local_demo@127.0.0.1:5432/steadfast?sslmode=disable"
	}

	adminKey := os.Getenv("ADMIN_KEY")
	if adminKey == "" {
		adminKey = "fake_admin_key_for_local_demo_only_32_bytes_min"
	}

	leaseSec := getEnvInt("LEASE_SECONDS", 10)
	hbSec := getEnvInt("HEARTBEAT_SECONDS", 3)
	reaperMs := getEnvInt("REAPER_INTERVAL_MS", 1000)
	ttlHours := getEnvInt("IDEMPOTENCY_TTL_HOURS", 24)
	maxAttempts := getEnvInt("DEFAULT_MAX_ATTEMPTS", 5)
	port := getEnvInt("PORT", 8080)

	disableFencing := os.Getenv("STEADFAST_DISABLE_FENCING") == "1"
	disableIdempotency := os.Getenv("STEADFAST_DISABLE_IDEMPOTENCY") == "1"

	baseMs := getEnvInt("BACKOFF_BASE_MS", 100)
	capMs := getEnvInt("BACKOFF_CAP_MS", 30000)
	rateLimitRPS := getEnvInt("RATE_LIMIT_RPS", 5000)

	cfg := &Config{
		Env:                 env,
		DatabaseURL:         dbURL,
		CheckerDatabaseURL:  checkerURL,
		AdminKey:            adminKey,
		LeaseDuration:       time.Duration(leaseSec) * time.Second,
		HeartbeatInterval:   time.Duration(hbSec) * time.Second,
		ReaperInterval:      time.Duration(reaperMs) * time.Millisecond,
		IdempotencyTTL:      time.Duration(ttlHours) * time.Hour,
		DefaultMaxAttempts:  maxAttempts,
		Port:                port,
		DisableFencing:      disableFencing,
		DisableIdempotency:  disableIdempotency,
		BackoffBase:         time.Duration(baseMs) * time.Millisecond,
		BackoffCap:          time.Duration(capMs) * time.Millisecond,
		RateLimitRPS:        rateLimitRPS,
	}

	if err := cfg.ValidateMutationFlags(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) ValidateMutationFlags() error {
	// Engine Law 10: Chaos and mutation switches work only when STEADFAST_ENV=demo
	if (c.DisableFencing || c.DisableIdempotency) && c.Env != "demo" {
		return errors.New("mutation switches (STEADFAST_DISABLE_FENCING / STEADFAST_DISABLE_IDEMPOTENCY) are strictly forbidden outside demo environment")
	}
	return nil
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	valStr := os.Getenv(key)
	if valStr == "" {
		return defaultVal
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		return defaultVal
	}
	return val
}
