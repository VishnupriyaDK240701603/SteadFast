package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const migrationLockID = 7182947192

type MigrationFile struct {
	Version  int
	Name     string
	UpPath   string
	DownPath string
}

type AppliedMigration struct {
	Version   int
	Name      string
	Checksum  string
	AppliedAt time.Time
}

type MigrationStatusInfo struct {
	Version   int
	Name      string
	Applied   bool
	Checksum  string
	AppliedAt *time.Time
}

func EnsureMigrationTable(ctx context.Context, pool *pgxpool.Pool) error {
	query := `
	CREATE TABLE IF NOT EXISTS schema_migrations (
		version INT PRIMARY KEY,
		name TEXT NOT NULL,
		checksum TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	);`
	_, err := pool.Exec(ctx, query)
	return err
}

func LoadMigrationFiles(migrationsDir string) ([]MigrationFile, error) {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read migrations directory: %w", err)
	}

	upMap := make(map[int]string)
	downMap := make(map[int]string)
	nameMap := make(map[int]string)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		if !strings.HasSuffix(filename, ".sql") {
			continue
		}

		parts := strings.SplitN(filename, "_", 2)
		if len(parts) < 2 {
			continue
		}

		version, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}

		if strings.HasSuffix(filename, ".up.sql") {
			baseName := strings.TrimSuffix(parts[1], ".up.sql")
			upMap[version] = filepath.Join(migrationsDir, filename)
			nameMap[version] = baseName
		} else if strings.HasSuffix(filename, ".down.sql") {
			downMap[version] = filepath.Join(migrationsDir, filename)
		}
	}

	var migrations []MigrationFile
	for version, upPath := range upMap {
		migrations = append(migrations, MigrationFile{
			Version:  version,
			Name:     nameMap[version],
			UpPath:   upPath,
			DownPath: downMap[version],
		})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})

	// Verify sequential versions starting from 1 with no gaps
	for idx, m := range migrations {
		expected := idx + 1
		if m.Version != expected {
			return nil, fmt.Errorf("non-sequential migration version: expected %d, got %d (%s)", expected, m.Version, m.Name)
		}
	}

	return migrations, nil
}

func GetAppliedMigrations(ctx context.Context, pool *pgxpool.Pool) (map[int]AppliedMigration, error) {
	if err := EnsureMigrationTable(ctx, pool); err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, "SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := make(map[int]AppliedMigration)
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.AppliedAt); err != nil {
			return nil, err
		}
		applied[a.Version] = a
	}
	return applied, rows.Err()
}

func getAppliedMigrationsTx(ctx context.Context, tx pgx.Tx) (map[int]AppliedMigration, error) {
	query := `
	CREATE TABLE IF NOT EXISTS schema_migrations (
		version INT PRIMARY KEY,
		name TEXT NOT NULL,
		checksum TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	);`
	if _, err := tx.Exec(ctx, query); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, "SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := make(map[int]AppliedMigration)
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.AppliedAt); err != nil {
			return nil, err
		}
		applied[a.Version] = a
	}
	return applied, rows.Err()
}

func ComputeFileChecksum(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func MigrateUp(ctx context.Context, pool *pgxpool.Pool, migrationsDir string) error {
	migrations, err := LoadMigrationFiles(migrationsDir)
	if err != nil {
		return err
	}

	appPW := os.Getenv("STEADFAST_APP_PASSWORD")
	if appPW == "" {
		appPW = "steadfast_app_dev_password"
	}
	roPW := os.Getenv("STEADFAST_CHECKER_RO_PASSWORD")
	if roPW == "" {
		roPW = "fake_ro_password_for_local_demo"
	}

	// Begin top-level transaction and acquire advisory transaction lock
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("failed to begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1);", migrationLockID); err != nil {
		return fmt.Errorf("failed to acquire migration advisory lock: %w", err)
	}

	applied, err := getAppliedMigrationsTx(ctx, tx)
	if err != nil {
		return err
	}

	// Verify existing applied migrations for integrity and checksum match
	for _, m := range migrations {
		if app, ok := applied[m.Version]; ok {
			currSum, err := ComputeFileChecksum(m.UpPath)
			if err != nil {
				return fmt.Errorf("failed to read migration file %s: %w", m.UpPath, err)
			}
			if currSum != app.Checksum {
				return fmt.Errorf("checksum mismatch for migration %d (%s): recorded %s, current file is %s", m.Version, m.Name, app.Checksum, currSum)
			}
		}
	}

	// Check if there are applied migrations missing on disk
	for ver, app := range applied {
		found := false
		for _, m := range migrations {
			if m.Version == ver {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("applied migration %d (%s) missing from migrations directory", ver, app.Name)
		}
	}

	// Apply pending migrations in order
	for _, m := range migrations {
		if _, ok := applied[m.Version]; ok {
			continue
		}

		sqlBytes, err := os.ReadFile(m.UpPath)
		if err != nil {
			return fmt.Errorf("failed to read migration %s: %w", m.UpPath, err)
		}
		checksum := sha256.Sum256(sqlBytes)
		checksumHex := hex.EncodeToString(checksum[:])

		// Inject role passwords into transaction settings for dynamic role provisioning
		_, err = tx.Exec(ctx, "SELECT set_config('steadfast.app_password', $1, true), set_config('steadfast.ro_password', $2, true);", appPW, roPW)
		if err != nil {
			return fmt.Errorf("failed to set role passwords for migration %d: %w", m.Version, err)
		}

		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("failed to apply migration %d (%s): %w", m.Version, m.Name, err)
		}

		recordQuery := `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES ($1, $2, $3, now())`
		if _, err := tx.Exec(ctx, recordQuery, m.Version, m.Name, checksumHex); err != nil {
			return fmt.Errorf("failed to record migration %d: %w", m.Version, err)
		}

		fmt.Printf("[migrate up] Applied migration %06d_%s\n", m.Version, m.Name)
	}

	return tx.Commit(ctx)
}

func MigrateDown(ctx context.Context, pool *pgxpool.Pool, migrationsDir string) error {
	migrations, err := LoadMigrationFiles(migrationsDir)
	if err != nil {
		return err
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("failed to begin down migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1);", migrationLockID); err != nil {
		return fmt.Errorf("failed to acquire migration advisory lock: %w", err)
	}

	applied, err := getAppliedMigrationsTx(ctx, tx)
	if err != nil {
		return err
	}

	if len(applied) == 0 {
		fmt.Println("[migrate down] No applied migrations to revert.")
		return tx.Commit(ctx)
	}

	// Find the latest applied version
	var appliedVersions []int
	for v := range applied {
		appliedVersions = append(appliedVersions, v)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(appliedVersions)))
	targetVersion := appliedVersions[0]

	var targetMigration *MigrationFile
	for _, m := range migrations {
		if m.Version == targetVersion {
			targetMigration = &m
			break
		}
	}

	if targetMigration == nil {
		return fmt.Errorf("no migration file found to revert version %d", targetVersion)
	}
	if targetMigration.DownPath == "" {
		return fmt.Errorf("missing down migration file for version %d", targetVersion)
	}

	sqlBytes, err := os.ReadFile(targetMigration.DownPath)
	if err != nil {
		return fmt.Errorf("failed to read down migration %s: %w", targetMigration.DownPath, err)
	}

	if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
		return fmt.Errorf("failed to revert migration %d (%s): %w", targetVersion, targetMigration.Name, err)
	}

	if _, err := tx.Exec(ctx, "DELETE FROM schema_migrations WHERE version = $1", targetVersion); err != nil {
		return fmt.Errorf("failed to remove migration record %d: %w", targetVersion, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit down migration %d: %w", targetVersion, err)
	}

	fmt.Printf("[migrate down] Reverted migration %06d_%s\n", targetVersion, targetMigration.Name)
	return nil
}

func GetMigrationStatus(ctx context.Context, pool *pgxpool.Pool, migrationsDir string) ([]MigrationStatusInfo, error) {
	migrations, err := LoadMigrationFiles(migrationsDir)
	if err != nil {
		return nil, err
	}

	applied, err := GetAppliedMigrations(ctx, pool)
	if err != nil {
		return nil, err
	}

	var statuses []MigrationStatusInfo
	for _, m := range migrations {
		info := MigrationStatusInfo{
			Version: m.Version,
			Name:    m.Name,
		}
		if app, ok := applied[m.Version]; ok {
			info.Applied = true
			info.Checksum = app.Checksum
			t := app.AppliedAt
			info.AppliedAt = &t
		}
		statuses = append(statuses, info)
	}
	return statuses, nil
}
