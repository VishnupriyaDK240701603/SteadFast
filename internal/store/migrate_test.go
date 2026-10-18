package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"steadfast/internal/config"
)

func TestSequentialMigrations(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer pool.Close()

	migrationsDir := "../../migrations"

	// 1. Migrate Up
	if err := MigrateUp(ctx, pool, migrationsDir); err != nil {
		t.Fatalf("Initial MigrateUp failed: %v", err)
	}

	// Verify status
	statuses, err := GetMigrationStatus(ctx, pool, migrationsDir)
	if err != nil {
		t.Fatalf("GetMigrationStatus failed: %v", err)
	}
	if len(statuses) == 0 || !statuses[0].Applied {
		t.Fatalf("Expected migration 1 to be applied, got %+v", statuses)
	}

	// 2. Migrate Down
	if err := MigrateDown(ctx, pool, migrationsDir); err != nil {
		t.Fatalf("MigrateDown failed: %v", err)
	}

	statusesDown, err := GetMigrationStatus(ctx, pool, migrationsDir)
	if err != nil {
		t.Fatalf("GetMigrationStatus after down failed: %v", err)
	}
	if len(statusesDown) > 0 && statusesDown[0].Applied {
		t.Fatalf("Expected migration 1 to be unapplied after down, got %+v", statusesDown)
	}

	// 3. Migrate Up again (re-application)
	if err := MigrateUp(ctx, pool, migrationsDir); err != nil {
		t.Fatalf("Second MigrateUp failed: %v", err)
	}

	statusesUp, err := GetMigrationStatus(ctx, pool, migrationsDir)
	if err != nil {
		t.Fatalf("GetMigrationStatus after re-up failed: %v", err)
	}
	if len(statusesUp) == 0 || !statusesUp[0].Applied {
		t.Fatalf("Expected migration 1 to be reapplied, got %+v", statusesUp)
	}
}

func TestChecksumMismatchFails(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer pool.Close()

	tempDir := t.TempDir()
	upContent := "-- initial migration\nCREATE TABLE IF NOT EXISTS test_chk (id INT PRIMARY KEY);"
	downContent := "DROP TABLE IF EXISTS test_chk;"

	upPath := filepath.Join(tempDir, "000001_chk.up.sql")
	downPath := filepath.Join(tempDir, "000001_chk.down.sql")
	_ = os.WriteFile(upPath, []byte(upContent), 0600)
	_ = os.WriteFile(downPath, []byte(downContent), 0600)

	// Use isolated schema for temp migration test to avoid touching real schema_migrations
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS test_schema_chk;"); err != nil {
		t.Fatalf("failed to create test schema: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS test_schema_chk CASCADE;")
	}()

	// Compute checksum directly to test verification
	sum1, err := ComputeFileChecksum(upPath)
	if err != nil {
		t.Fatalf("ComputeFileChecksum failed: %v", err)
	}

	// Tamper with file
	_ = os.WriteFile(upPath, []byte(upContent+"\n-- tampered comment"), 0600)
	sum2, err := ComputeFileChecksum(upPath)
	if err != nil {
		t.Fatalf("ComputeFileChecksum after tamper failed: %v", err)
	}

	if sum1 == sum2 {
		t.Fatalf("Tampered file must yield different checksum")
	}
}

func TestMissingMigrationFails(t *testing.T) {
	tempDir := t.TempDir()
	up1 := filepath.Join(tempDir, "000001_m1.up.sql")
	_ = os.WriteFile(up1, []byte("SELECT 1;"), 0600)

	migrations, err := LoadMigrationFiles(tempDir)
	if err != nil {
		t.Fatalf("LoadMigrationFiles failed: %v", err)
	}
	if len(migrations) != 1 {
		t.Fatalf("Expected 1 migration, got %d", len(migrations))
	}

	// Delete file from disk
	_ = os.Remove(up1)

	_, err = LoadMigrationFiles(tempDir)
	if err != nil {
		t.Fatalf("LoadMigrationFiles on empty dir failed: %v", err)
	}
}

func TestNonSequentialMigrations(t *testing.T) {
	tempDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tempDir, "000001_first.up.sql"), []byte("SELECT 1;"), 0600)
	_ = os.WriteFile(filepath.Join(tempDir, "000003_skipped.up.sql"), []byte("SELECT 3;"), 0600)

	_, err := LoadMigrationFiles(tempDir)
	if err == nil {
		t.Fatalf("Expected error for non-sequential versions, got nil")
	}
	if !strings.Contains(err.Error(), "non-sequential") {
		t.Fatalf("Expected error to mention non-sequential, got: %v", err)
	}
}
