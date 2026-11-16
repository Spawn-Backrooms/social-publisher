package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type readinessMigration struct {
	name, checksum string
}

// These zero-row reads resolve the same columns that normal store operations
// use, including columns written or filtered on but absent from result scans.
var readinessTables = []struct {
	name, columns, privileges string
}{
	{"sp_schema_migrations", "version, name, checksum, applied_at", "SELECT"},
	{"sp_accounts", accountColumns + ", provider_user_id, disconnected_at", "SELECT,INSERT,UPDATE"},
	{"sp_oauth_states", "state_hash, cookie_hash, owner_tenant, owner_subject, platform, encrypted_verifier, issued_at, expires_at", "SELECT,INSERT,UPDATE,DELETE"},
	{"sp_posts", postColumns + ", idempotency_key, request_hash", "SELECT,INSERT,UPDATE"},
	{"sp_targets", targetColumns + ", owner_tenant, owner_subject, position", "SELECT,INSERT,UPDATE"},
	{"sp_events", "id, post_id, target_id, account_id, kind, detail, created_at", "SELECT,INSERT,DELETE"},
	{"sp_provider_pacing", "account_id, scope, next_allowed_at", "SELECT,INSERT,UPDATE,DELETE"},
	{"sp_api_owner_budgets", "owner_tenant, owner_subject, window_started_at, request_count", "SELECT,INSERT,UPDATE,DELETE"},
	{"sp_api_admissions", "token, owner_tenant, owner_subject, scope_kind, scope_key, expires_at", "SELECT,INSERT,UPDATE,DELETE"},
	{"sp_process_heartbeats", "instance_id, role, build_revision, started_at, last_seen_at, worker_progress_at, worker_progress_sequence", "SELECT,INSERT,UPDATE,DELETE"},
	{"sp_retention_cursors", "name, after_created_at, after_id, updated_at", "SELECT,UPDATE"},
}

// checkReadiness only reads the migration ledger and runtime schema. It never
// applies migrations, repairs drift, or takes migration/advisory locks.
func (s *Postgres) checkReadiness(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	expected, err := expectedReadinessMigrations()
	if err != nil {
		return err
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire readiness connection: %w", err)
	}
	defer conn.Release()
	var writable bool
	if err := conn.QueryRow(ctx, `SELECT NOT pg_is_in_recovery()
        AND current_setting('transaction_read_only')='off'
        AND current_setting('default_transaction_read_only')='off'`).Scan(&writable); err != nil {
		return fmt.Errorf("check writable database: %w", err)
	}
	if !writable {
		return fmt.Errorf("database must be a writable primary")
	}
	// One excess row is enough to reject any ledger larger than this binary's
	// manifest, without an unbounded scan of an accidentally corrupted ledger.
	rows, err := conn.Query(ctx, "SELECT version, name, checksum FROM sp_schema_migrations ORDER BY version LIMIT $1", len(expected)+1)
	if err != nil {
		return fmt.Errorf("read readiness migration ledger: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version int64
		var applied readinessMigration
		if err := rows.Scan(&version, &applied.name, &applied.checksum); err != nil {
			return fmt.Errorf("scan readiness migration ledger: %w", err)
		}
		want, exists := expected[version]
		if !exists {
			return fmt.Errorf("database migration %d is unknown or duplicated", version)
		}
		if applied.name != want.name {
			return fmt.Errorf("database migration %d name mismatch", version)
		}
		if applied.checksum != want.checksum {
			return fmt.Errorf("database migration %d checksum mismatch", version)
		}
		delete(expected, version)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read readiness migration ledger: %w", err)
	}
	rows.Close()
	for version, migration := range expected {
		return fmt.Errorf("database migration %d (%s) is not applied", version, migration.name)
	}
	// Check each privilege separately: PostgreSQL interprets a comma-separated
	// has_table_privilege argument as ANY privilege, which would accept readers.
	var tables, privileges []string
	for _, table := range readinessTables {
		for _, privilege := range strings.Split(table.privileges, ",") {
			tables = append(tables, table.name)
			privileges = append(privileges, privilege)
		}
	}
	var missingPrivilege string
	if err := conn.QueryRow(ctx, `SELECT COALESCE((
        SELECT name || ':' || privilege FROM unnest($1::text[],$2::text[]) AS required(name,privilege)
        WHERE NOT has_table_privilege(current_user,name,privilege) LIMIT 1), '')`, tables, privileges).Scan(&missingPrivilege); err != nil {
		return fmt.Errorf("check runtime database privileges: %w", err)
	}
	if missingPrivilege != "" {
		return fmt.Errorf("runtime database privilege is missing: %s", missingPrivilege)
	}
	// Batch the zero-row probes so adding schema checks does not add one network
	// round trip per table. None of these statements changes data or the schema.
	batch := &pgx.Batch{}
	for _, table := range readinessTables {
		batch.Queue("SELECT " + table.columns + " FROM " + table.name + " LIMIT 0")
	}
	results := conn.SendBatch(ctx, batch)
	defer results.Close()
	for _, table := range readinessTables {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("runtime schema %s is not ready: %w", table.name, err)
		}
	}
	return results.Close()
}

func expectedReadinessMigrations() (map[int64]readinessMigration, error) {
	paths, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("list embedded readiness migrations: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no embedded readiness migrations")
	}
	expected := make(map[int64]readinessMigration, len(paths))
	for _, path := range paths {
		name := strings.TrimPrefix(path, "migrations/")
		version, err := strconv.ParseInt(strings.SplitN(name, "_", 2)[0], 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("invalid embedded migration name %q", name)
		}
		if _, duplicate := expected[version]; duplicate {
			return nil, fmt.Errorf("duplicate embedded migration version %d", version)
		}
		body, err := migrationFiles.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read embedded readiness migration %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		expected[version] = readinessMigration{name: name, checksum: hex.EncodeToString(sum[:])}
	}
	return expected, nil
}
