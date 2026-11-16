package ops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"socialpublisher/internal/config"
	"socialpublisher/internal/model"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
)

// This explicitly enabled acceptance test reads an already restored, isolated
// database. It never starts workers, refreshes tokens, or contacts a provider.
func TestRestoredDatabaseSafety(t *testing.T) {
	if os.Getenv("RUN_RESTORE_VERIFY") != "1" {
		t.Skip("real backup restore acceptance requires docs/verify-restore.sh")
	}
	if _, set := os.LookupEnv("TEST_PGLITE"); set {
		t.Fatal("restore acceptance requires native PostgreSQL; a PGlite fixture is insufficient")
	}
	if os.Getenv("RESTORE_ISOLATED") != "true" || os.Getenv("RESTORE_MAINTENANCE_CONFIRMED") != "true" {
		t.Fatal("confirm the restored database is isolated and all publication workers are stopped")
	}
	dsn := os.Getenv("RESTORE_DATABASE_URL")
	if dsn == "" {
		t.Fatal("RESTORE_DATABASE_URL is required")
	}
	local := os.Getenv("RESTORE_ALLOW_LOCAL_PLAINTEXT") == "true"
	if err := validateRestoreDatabaseURL(dsn, local); err != nil {
		t.Fatal(err)
	}
	expectedPosts := restoreExpectedCount(t, "RESTORE_EXPECTED_POSTS", false)
	expectedTargets := restoreExpectedCount(t, "RESTORE_EXPECTED_TARGETS", false)
	expectedCredentials := restoreExpectedCount(t, "RESTORE_EXPECTED_ENCRYPTED_ACCOUNTS", true)
	expectedEvents := restoreExpectedCount(t, "RESTORE_EXPECTED_EVENTS", false)
	maxEventID := restoreExpectedCount(t, "RESTORE_MAX_EVENT_ID", false)
	var encoded map[string]string
	if err := json.Unmarshal([]byte(os.Getenv("TOKEN_ENCRYPTION_KEYS")), &encoded); err != nil || len(encoded) == 0 {
		t.Fatal("the independently retained TOKEN_ENCRYPTION_KEYS keyring is required")
	}
	keys := make(map[string][]byte, len(encoded))
	for id, value := range encoded {
		key, err := base64.StdEncoding.Strict().DecodeString(value)
		if err != nil || len(key) != 32 {
			t.Fatal("the restored keyring contains an invalid key")
		}
		keys[id] = key
	}
	vault, err := security.NewVault(keys, os.Getenv("ACTIVE_TOKEN_KEY_ID"))
	if err != nil {
		t.Fatal("the restored keyring or active key selection is invalid")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	database, err := store.New(ctx, dsn, 2)
	if err != nil {
		t.Fatal("cannot connect to the isolated restored database")
	}
	defer database.Close()
	if err = database.Ping(ctx); err != nil {
		t.Fatal("restored database failed the exact-release schema, primary, or runtime-privilege check")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("cannot open the restore verification session")
	}
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal("cannot begin read-only restore verification")
	}
	defer tx.Rollback(context.Background())
	for _, check := range []struct {
		name, query string
		want        int64
	}{
		{"posts", "SELECT count(*) FROM sp_posts", expectedPosts},
		{"targets", "SELECT count(*) FROM sp_targets", expectedTargets},
		{"encrypted accounts", "SELECT count(*) FROM sp_accounts WHERE encrypted_token IS NOT NULL", expectedCredentials},
		{"active restored accounts", "SELECT count(*) FROM sp_accounts WHERE status='active'", 0},
		{"unquarantined restored targets", "SELECT count(*) FROM sp_targets WHERE status NOT IN ('published','needs_attention')", 0},
		{"restored worker leases", "SELECT count(*) FROM sp_targets WHERE lease_token IS NOT NULL OR lease_until IS NOT NULL", 0},
		{"restored refresh intents", "SELECT count(*) FROM sp_accounts WHERE refresh_token_version IS NOT NULL OR refresh_started_at IS NOT NULL", 0},
		{"invalid idempotency records", "SELECT count(*) FROM sp_posts WHERE idempotency_key='' OR request_hash=''", 0},
	} {
		var count int64
		if err = tx.QueryRow(ctx, check.query).Scan(&count); err != nil {
			t.Fatalf("cannot verify %s", check.name)
		}
		if count != check.want {
			t.Fatalf("%s: got %d, expected %d; restore acceptance failed", check.name, count, check.want)
		}
	}
	if err = verifyRestoreEventPopulation(ctx, tx, expectedEvents, maxEventID); err != nil {
		t.Fatal(err)
	}
	var decrypted int64
	var after string
	for {
		rows, queryErr := tx.Query(ctx, `SELECT id,owner_tenant,owner_subject,platform,provider_user_id,encrypted_token
			FROM sp_accounts WHERE encrypted_token IS NOT NULL AND id>$1 ORDER BY id LIMIT 200`, after)
		if queryErr != nil {
			t.Fatal("cannot read restored ciphertext batch")
		}
		count := 0
		for rows.Next() {
			var a model.Account
			if err = rows.Scan(&a.ID, &a.Owner.TenantID, &a.Owner.Subject, &a.Platform, &a.Identity.ID, &a.EncryptedToken); err != nil {
				rows.Close()
				t.Fatal("cannot decode restored account")
			}
			plaintext, openErr := vault.Open(a.EncryptedToken, a.TokenAAD())
			var token model.Token
			if openErr != nil || json.Unmarshal(plaintext, &token) != nil || token.AccessToken == "" {
				rows.Close()
				t.Fatal("a restored account credential cannot be authenticated and decrypted with the retained keyring")
			}
			after, count = a.ID, count+1
			decrypted++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal("restored ciphertext batch could not be read completely")
		}
		if count < 200 {
			break
		}
	}
	if decrypted != expectedCredentials {
		t.Fatal("not every expected restored credential was authenticated and decrypted")
	}
	var verifiedOAuth int64
	after = ""
	for {
		rows, queryErr := tx.Query(ctx, `SELECT state_hash,encrypted_verifier FROM sp_oauth_states
			WHERE encrypted_verifier IS NOT NULL AND state_hash>$1 ORDER BY state_hash LIMIT 200`, after)
		if queryErr != nil {
			t.Fatal("cannot read restored OAuth ciphertext batch")
		}
		count := 0
		for rows.Next() {
			var blob []byte
			if err = rows.Scan(&after, &blob); err != nil {
				rows.Close()
				t.Fatal("cannot decode restored OAuth verifier")
			}
			if _, err = vault.Open(blob, "oauth:"+after); err != nil {
				rows.Close()
				t.Fatal("a restored OAuth verifier cannot be authenticated with the retained keyring")
			}
			count++
			verifiedOAuth++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal("restored OAuth batch could not be read completely")
		}
		if count < 200 {
			break
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal("read-only restore verification could not complete")
	}
	t.Logf("verified isolated restore: posts=%d targets=%d backup_events=%d max_backup_event_id=%d decrypted_accounts=%d decrypted_oauth_verifiers=%d; all nonpublished targets quarantined and all restored accounts require reconnect", expectedPosts, expectedTargets, expectedEvents, maxEventID, decrypted, verifiedOAuth)
}

type restoreRowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func verifyRestoreEventPopulation(ctx context.Context, tx restoreRowQuerier, expected, maxEventID int64) error {
	if expected < 0 || maxEventID < 0 || (expected == 0) != (maxEventID == 0) {
		return errors.New("backup event count and maximum identity must both be zero for an empty snapshot, or both positive")
	}
	var count, maximum int64
	if err := tx.QueryRow(ctx, "SELECT count(*),COALESCE(max(id),0) FROM sp_events WHERE id<=$1", maxEventID).Scan(&count, &maximum); err != nil {
		return errors.New("cannot verify retained backup events")
	}
	if count != expected || maximum != maxEventID {
		return fmt.Errorf("retained backup events: got count=%d max_id=%d, expected count=%d max_id=%d; restore acceptance failed", count, maximum, expected, maxEventID)
	}
	return nil
}

func TestRestoreEventManifestCannotBeMaskedByNewQuarantineEvents(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is unset; restore event SQL was not run")
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `CREATE TEMP TABLE sp_events(id bigint PRIMARY KEY,kind text NOT NULL) ON COMMIT DROP;
		INSERT INTO sp_events VALUES (2,'target.published'),(4,'post.created'),(7,'target.restore_quarantined'),(9,'target.queued');`); err != nil {
		t.Fatal(err)
	}
	// The manifest includes previous restore audit rows and ordinary sequence
	// gaps. Current quarantine writes above its captured identity boundary.
	if _, err := tx.Exec(t.Context(), `INSERT INTO sp_events VALUES (10,'account.restore_quarantined'),(11,'target.restore_quarantined')`); err != nil {
		t.Fatal(err)
	}
	if err := verifyRestoreEventPopulation(t.Context(), tx, 4, 9); err != nil {
		t.Fatalf("intact backup history with new audit rows was rejected: %v", err)
	}
	if _, err := tx.Exec(t.Context(), "DELETE FROM sp_events WHERE id=2"); err != nil {
		t.Fatal(err)
	}
	if err := verifyRestoreEventPopulation(t.Context(), tx, 4, 9); err == nil {
		t.Error("missing published receipt was masked by two new quarantine audit rows")
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO sp_events VALUES (2,'target.published')"); err != nil {
		t.Fatal(err)
	}
	if err := verifyRestoreEventPopulation(t.Context(), tx, 3, 9); err == nil {
		t.Error("a minimum rather than exact backup event count was accepted")
	}
	if err := verifyRestoreEventPopulation(t.Context(), tx, 4, 8); err == nil {
		t.Error("incorrect manifest identity boundary was accepted")
	}
	if _, err := tx.Exec(t.Context(), "DELETE FROM sp_events WHERE id<=9"); err != nil {
		t.Fatal(err)
	}
	if err := verifyRestoreEventPopulation(t.Context(), tx, 0, 0); err != nil {
		t.Errorf("empty backup with new quarantine audit rows was rejected: %v", err)
	}
	for _, invalid := range [][2]int64{{0, 9}, {1, 0}, {-1, 0}, {1, -1}} {
		if err := verifyRestoreEventPopulation(t.Context(), tx, invalid[0], invalid[1]); err == nil {
			t.Errorf("inconsistent event manifest count=%d max_id=%d was accepted", invalid[0], invalid[1])
		}
	}
}

func validateRestoreDatabaseURL(dsn string, local bool) error {
	if local {
		connection, err := pgx.ParseConfig(dsn)
		if err != nil {
			return errors.New("RESTORE_DATABASE_URL is invalid")
		}
		hosts := []string{connection.Host}
		for _, fallback := range connection.Fallbacks {
			hosts = append(hosts, fallback.Host)
		}
		for _, host := range hosts {
			ip := net.ParseIP(host)
			if host != "localhost" && !strings.HasPrefix(host, "/") && (ip == nil || !ip.IsLoopback()) {
				return errors.New("plaintext restore verification is restricted to local PostgreSQL, including fallback hosts")
			}
		}
	}
	return config.ValidateDatabaseURL(dsn, local)
}

func TestRestoreLocalExceptionRejectsRemoteFallback(t *testing.T) {
	if err := validateRestoreDatabaseURL("host=127.0.0.1,192.0.2.20 user=fixture dbname=fixture sslmode=disable", true); err == nil {
		t.Fatal("local restore plaintext exception admitted a remote fallback host")
	}
	if err := validateRestoreDatabaseURL("host=127.0.0.1 user=fixture dbname=fixture sslmode=disable", true); err != nil {
		t.Fatalf("local restore fixture was rejected: %v", err)
	}
}

func restoreExpectedCount(t *testing.T, name string, positive bool) int64 {
	t.Helper()
	value, err := strconv.ParseInt(os.Getenv(name), 10, 64)
	if err != nil || value < 0 || (positive && value == 0) {
		t.Fatalf("%s must be supplied from the independent backup manifest; encrypted-account count must be positive", name)
	}
	return value
}
