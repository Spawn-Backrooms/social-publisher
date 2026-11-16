// nativecheck verifies that TEST_DATABASE_URL preserves independent PostgreSQL
// sessions before the native integration gate is allowed to run.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if err := run(dsn); err != nil {
		message := err.Error()
		if dsn != "" {
			message = strings.ReplaceAll(message, dsn, "<TEST_DATABASE_URL>")
		}
		if parsed, parseErr := url.Parse(dsn); parseErr == nil && parsed.User != nil {
			if password, ok := parsed.User.Password(); ok && password != "" {
				message = strings.ReplaceAll(message, password, "<redacted>")
			}
		}
		fmt.Fprintln(os.Stderr, "native PostgreSQL preflight: "+message)
		os.Exit(1)
	}
}

func run(dsn string) error {
	if dsn == "" {
		return errors.New("TEST_DATABASE_URL must name a disposable native PostgreSQL database")
	}
	if _, set := os.LookupEnv("TEST_PGLITE"); set {
		return errors.New("TEST_PGLITE must be unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	one, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect first session: %w", err)
	}
	defer closeSession(one)
	two, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect second session: %w", err)
	}
	defer closeSession(two)
	var firstPID, secondPID, versionNumber int32
	var version string
	if err = one.QueryRow(ctx, "SELECT pg_backend_pid(),current_setting('server_version_num')::integer,version()").Scan(&firstPID, &versionNumber, &version); err != nil {
		return fmt.Errorf("query server identity: %w", err)
	}
	if err = two.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&secondPID); err != nil {
		return err
	}
	if firstPID == secondPID {
		return fmt.Errorf("both client connections share backend PID %d; independent native sessions are required", firstPID)
	}
	if !strings.HasPrefix(version, "PostgreSQL ") {
		return errors.New("server did not identify itself as PostgreSQL")
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	marker := "native_gate_" + hex.EncodeToString(nonce[:8])
	key := int64(binary.BigEndian.Uint64(nonce[8:]))
	if _, err = one.Exec(ctx, "SELECT set_config('application_name',$1,false)", marker); err != nil {
		return err
	}
	var locked bool
	if err = one.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&locked); err != nil || !locked {
		return fmt.Errorf("first session could not acquire test advisory lock: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cleanupCancel()
		_, _ = one.Exec(cleanupCtx, "SELECT pg_advisory_unlock($1)", key)
	}()
	if err = two.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&locked); err != nil {
		return err
	}
	if locked {
		_, _ = two.Exec(ctx, "SELECT pg_advisory_unlock($1)", key)
		return errors.New("another client acquired the first session's exclusive advisory lock")
	}
	// Alternate queries while both connections remain open. Backend identity and
	// session-local state must survive completed statements and idle periods.
	for range 8 {
		for i, conn := range []*pgx.Conn{one, two} {
			var pid int32
			var app string
			if err = conn.QueryRow(ctx, "SELECT pg_backend_pid(),current_setting('application_name')").Scan(&pid, &app); err != nil {
				return err
			}
			wantPID := firstPID
			if i == 1 {
				wantPID = secondPID
			}
			if pid != wantPID || (i == 0 && app != marker) || (i == 1 && app == marker) {
				return errors.New("connection path did not preserve independent session identity and settings")
			}
		}
	}
	if err = one.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", key).Scan(&locked); err != nil || !locked {
		return fmt.Errorf("first session lost ownership of its advisory lock: %v", err)
	}
	if err = two.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&locked); err != nil || !locked {
		return fmt.Errorf("second session could not acquire the released advisory lock: %v", err)
	}
	if _, err = two.Exec(ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"kind": "native_postgres_preflight", "passed": true,
		"server_version": version, "server_version_num": versionNumber,
		"backend_pids":         []int32{firstPID, secondPID},
		"independent_sessions": true, "advisory_lock_exclusion": true,
	})
}

func closeSession(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}
