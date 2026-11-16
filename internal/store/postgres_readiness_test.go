package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPostgresReadinessRejectsReadOnlyDatabase(t *testing.T) {
	s, _ := postgresFixture(t, 1)
	conn, err := s.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(t.Context(), "SET default_transaction_read_only = on")
	conn.Release()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.pool.Exec(ctx, "SET default_transaction_read_only = off"); err != nil {
			t.Errorf("reset read-only test session: %v", err)
		}
	})
	if err := s.Ping(t.Context()); err == nil {
		t.Fatal("read-only database was reported ready for publishing")
	}
}

func TestPostgresReadinessRequiresRuntimeWritePrivileges(t *testing.T) {
	s, _ := postgresFixture(t, 1)
	ctx := t.Context()
	var schema string
	if err := s.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := pgx.Identifier{testID(t, "sp_runtime_")}.Sanitize()
	if _, err := s.pool.Exec(ctx, "CREATE ROLE "+role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, statement := range []string{"RESET ROLE", "DROP OWNED BY " + role, "DROP ROLE " + role} {
			if _, err := s.pool.Exec(ctx, statement); err != nil {
				t.Errorf("clean runtime privilege fixture: %v", err)
			}
		}
	})
	for _, statement := range []string{
		"GRANT USAGE ON SCHEMA " + pgx.Identifier{schema}.Sanitize() + " TO " + role,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA " + pgx.Identifier{schema}.Sanitize() + " TO " + role,
		"GRANT USAGE ON ALL SEQUENCES IN SCHEMA " + pgx.Identifier{schema}.Sanitize() + " TO " + role,
		"SET ROLE " + role,
	} {
		if _, err := s.pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("DML-only runtime role should be ready: %v", err)
	}
	for _, tc := range []struct{ name, privilege, object string }{
		{"account token updates", "UPDATE", "TABLE sp_accounts"},
		{"target lease updates", "UPDATE", "TABLE sp_targets"},
		{"audit event inserts", "INSERT", "TABLE sp_events"},
		{"OAuth cleanup row locks", "UPDATE", "TABLE sp_oauth_states"},
		{"admission cleanup row locks", "UPDATE", "TABLE sp_api_admissions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, statement := range []string{"RESET ROLE", "REVOKE " + tc.privilege + " ON " + tc.object + " FROM " + role, "SET ROLE " + role} {
				if _, err := s.pool.Exec(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Ping(ctx); err == nil {
				t.Error("missing runtime write privilege was reported ready")
			}
			for _, statement := range []string{"RESET ROLE", "GRANT " + tc.privilege + " ON " + tc.object + " TO " + role, "SET ROLE " + role} {
				if _, err := s.pool.Exec(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPostgresReadinessRejectsSchemaDrift(t *testing.T) {
	for _, tc := range []struct {
		name, mutate, unchanged string
	}{
		{
			name:      "empty database",
			mutate:    "DROP TABLE sp_events, sp_targets, sp_posts, sp_accounts, sp_oauth_states, sp_schema_migrations, sp_provider_pacing, sp_api_owner_budgets, sp_api_admissions, sp_process_heartbeats, sp_retention_cursors CASCADE",
			unchanged: "SELECT count(*)=0 FROM pg_tables WHERE schemaname=current_schema() AND tablename LIKE 'sp_%'",
		},
		{
			name:      "missing migration ledger",
			mutate:    "DROP TABLE sp_schema_migrations",
			unchanged: "SELECT to_regclass('sp_schema_migrations') IS NULL",
		},
		{
			name:      "unapplied migrations",
			mutate:    "DELETE FROM sp_schema_migrations",
			unchanged: "SELECT count(*)=0 FROM sp_schema_migrations",
		},
		{
			name:   "pending latest migration",
			mutate: "DELETE FROM sp_schema_migrations WHERE version=(SELECT max(version) FROM sp_schema_migrations)",
		},
		{
			name:   "unknown migration",
			mutate: "INSERT INTO sp_schema_migrations(version,name,checksum) VALUES (999999,'future.sql','unknown')",
		},
		{
			name:   "changed migration name",
			mutate: "UPDATE sp_schema_migrations SET name='renamed.sql' WHERE version=(SELECT min(version) FROM sp_schema_migrations)",
		},
		{
			name:   "changed migration checksum",
			mutate: "UPDATE sp_schema_migrations SET checksum=repeat('0',64) WHERE version=(SELECT min(version) FROM sp_schema_migrations)",
		},
		{
			name:      "missing runtime table",
			mutate:    "DROP TABLE sp_events",
			unchanged: "SELECT to_regclass('sp_events') IS NULL",
		},
		{
			name:   "missing runtime column",
			mutate: "ALTER TABLE sp_oauth_states DROP COLUMN encrypted_verifier",
		},
		{
			name:   "missing processing window column",
			mutate: "ALTER TABLE sp_targets DROP COLUMN processing_started_at",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := postgresFixture(t, 1)
			if err := s.Ping(t.Context()); err != nil {
				t.Fatalf("fixture was not ready before schema drift: %v", err)
			}
			if _, err := s.pool.Exec(t.Context(), tc.mutate); err != nil {
				t.Fatal(err)
			}
			if err := s.Ping(t.Context()); err == nil {
				t.Fatal("schema drift was reported ready")
			} else if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("readiness timed out instead of identifying schema drift: %v", err)
			}
			if tc.unchanged != "" {
				var unchanged bool
				if err := s.pool.QueryRow(t.Context(), tc.unchanged).Scan(&unchanged); err != nil {
					t.Fatal(err)
				}
				if !unchanged {
					t.Fatal("readiness changed the database schema or migration ledger")
				}
			}
		})
	}
}

func TestPostgresReadinessHonorsDeadlineWhilePoolIsBusy(t *testing.T) {
	s, _ := postgresFixture(t, 1)
	conn, err := s.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := s.Ping(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy-pool readiness did not honor caller deadline: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("readiness exceeded the caller deadline by too much: %s", elapsed)
	}
}
