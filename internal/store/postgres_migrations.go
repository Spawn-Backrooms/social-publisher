package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrate applies embedded migrations under a database-wide session advisory
// lock. Each schema change and its checksum record commit in one transaction.
func Migrate(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect for migration: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	// Two-int locks occupy a different namespace from account bigint locks.
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock($1, $2)", int32(1397773650), int32(1)); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if _, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS sp_schema_migrations (
        version bigint PRIMARY KEY, name text NOT NULL, checksum text NOT NULL,
        applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	paths, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(paths)
	known := make(map[int64]string, len(paths))
	for _, path := range paths {
		name := strings.TrimPrefix(path, "migrations/")
		version, parseErr := strconv.ParseInt(strings.SplitN(name, "_", 2)[0], 10, 64)
		if parseErr != nil || version <= 0 {
			return fmt.Errorf("invalid embedded migration name %q", name)
		}
		if _, exists := known[version]; exists {
			return fmt.Errorf("duplicate embedded migration version %d", version)
		}
		known[version] = path
	}
	rows, err := conn.Query(ctx, "SELECT version, name, checksum FROM sp_schema_migrations ORDER BY version")
	if err != nil {
		return fmt.Errorf("read migration ledger: %w", err)
	}
	applied := make(map[int64]string)
	for rows.Next() {
		var version int64
		var name, checksum string
		if err = rows.Scan(&version, &name, &checksum); err != nil {
			rows.Close()
			return err
		}
		path, exists := known[version]
		if !exists {
			rows.Close()
			return fmt.Errorf("database migration %d is newer than or absent from this binary", version)
		}
		if strings.TrimPrefix(path, "migrations/") != name {
			rows.Close()
			return fmt.Errorf("migration %d name mismatch", version)
		}
		applied[version] = checksum
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, path := range paths {
		name := strings.TrimPrefix(path, "migrations/")
		version, _ := strconv.ParseInt(strings.SplitN(name, "_", 2)[0], 10, 64)
		body, readErr := migrationFiles.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])
		if previous, exists := applied[version]; exists {
			if previous != checksum {
				return fmt.Errorf("migration %d checksum mismatch; applied migrations must not be edited", version)
			}
			continue
		}
		tx, beginErr := conn.Begin(ctx)
		if beginErr != nil {
			return beginErr
		}
		if _, err = tx.Exec(ctx, string(body)); err == nil {
			_, err = tx.Exec(ctx, "INSERT INTO sp_schema_migrations(version,name,checksum) VALUES ($1,$2,$3)", version, name, checksum)
		}
		if err != nil {
			_ = tx.Rollback(context.Background())
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %d: %w", version, err)
		}
	}
	return nil
}
