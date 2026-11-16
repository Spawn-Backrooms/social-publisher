// restorefixture seeds and verifies disposable backup/restore acceptance data.
// It never contacts a provider. It refuses non-loopback or non-fixture databases.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"socialpublisher/internal/model"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
)

type manifest struct {
	Posts             int               `json:"posts"`
	Targets           int               `json:"targets"`
	EncryptedAccounts int               `json:"encrypted_accounts"`
	Events            int               `json:"events"`
	MaxEventID        int64             `json:"max_event_id"`
	PreservedHashes   map[string]string `json:"preserved_hashes"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "restore fixture:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 || (os.Args[1] != "seed" && os.Args[1] != "verify") {
		return errors.New("usage: restorefixture seed|verify")
	}
	dsn := os.Getenv("RESTORE_FIXTURE_DATABASE_URL")
	config, err := pgx.ParseConfig(dsn)
	if err != nil || dsn == "" || !strings.HasPrefix(config.Database, "sp_verify_restore_") {
		return errors.New("RESTORE_FIXTURE_DATABASE_URL must name an sp_verify_restore_ database")
	}
	hosts := []string{config.Host}
	for _, fallback := range config.Fallbacks {
		hosts = append(hosts, fallback.Host)
	}
	for _, host := range hosts {
		ip := net.ParseIP(host)
		if host != "localhost" && !strings.HasPrefix(host, "/") && (ip == nil || !ip.IsLoopback()) {
			return errors.New("restore fixtures require loopback or Unix socket hosts, including every fallback")
		}
	}
	directory := os.Getenv("RESTORE_FIXTURE_DIR")
	if directory == "" {
		return errors.New("RESTORE_FIXTURE_DIR is required")
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return errors.New("cannot connect to the disposable restore fixture database")
	}
	defer conn.Close(ctx)
	if os.Args[1] == "seed" {
		return seed(ctx, conn, dsn, directory)
	}
	return verify(ctx, conn, dsn, directory)
}

func seed(ctx context.Context, conn *pgx.Conn, dsn, directory string) error {
	var tables int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE schemaname='public'").Scan(&tables); err != nil || tables != 0 {
		return errors.New("seed requires a newly created empty fixture database")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		return err
	}
	database, err := store.New(ctx, dsn, 4)
	if err != nil {
		return err
	}
	defer database.Close()
	keys := map[string][]byte{"old": make([]byte, 32), "current": make([]byte, 32)}
	encoded := make(map[string]string)
	for id, key := range keys {
		if _, err = rand.Read(key); err != nil {
			return err
		}
		encoded[id] = base64.StdEncoding.EncodeToString(key)
	}
	keyring, _ := json.Marshal(encoded)
	if err = os.WriteFile(filepath.Join(directory, "retained-keyring.json"), keyring, 0600); err != nil {
		return err
	}
	accounts := make([]model.Account, 2)
	for i, keyID := range []string{"old", "current"} {
		vault, vaultErr := security.NewVault(keys, keyID)
		if vaultErr != nil {
			return vaultErr
		}
		account := fixtureAccount(i)
		plaintext, _ := json.Marshal(model.Token{AccessToken: fmt.Sprintf("explicit_fixture_access_%d", i), RefreshToken: fmt.Sprintf("explicit_fixture_refresh_%d", i), ExpiresAt: time.Now().Add(time.Hour)})
		account.EncryptedToken, err = vault.Seal(plaintext, account.TokenAAD())
		if err != nil {
			return err
		}
		accounts[i], err = database.UpsertAccount(ctx, account)
		if err != nil {
			return err
		}
		stateHash := fmt.Sprintf("restore_state_%d", i)
		verifier, sealErr := vault.Seal([]byte("explicit_fixture_pkce_verifier"), "oauth:"+stateHash)
		if sealErr != nil {
			return sealErr
		}
		if err = database.PutOAuthState(ctx, model.OAuthState{StateHash: stateHash, CookieHash: "fixture_browser", Owner: account.Owner, Platform: account.Platform, EncryptedVerifier: verifier, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			return err
		}
	}
	statuses := []string{"published", "running", "failed", "queued", "processing", "cancelled"}
	for i, status := range statuses {
		account := accounts[i%len(accounts)]
		post := fixturePost(i, account)
		if _, created, createErr := database.CreatePost(ctx, post, fmt.Sprintf("restore_key_%d", i), fmt.Sprintf("restore_hash_%d", i)); createErr != nil || !created {
			return fmt.Errorf("create restore fixture %d: %v", i, createErr)
		}
		// Seed valid snapshots of several real lifecycle states without sending
		// remote requests. All checkpoints and receipts are explicit fixtures.
		progress, receipt, receiptURL := `{}`, "", ""
		if status == "running" {
			progress = `{"stage":"publishing","unsafe":true,"data":{"operation":"fixture_uncertain_operation"}}`
		} else if status == "processing" {
			progress = `{"stage":"processing","data":{"operation":"fixture_known_operation"}}`
		} else if status == "published" {
			receipt, receiptURL = "fixture_published_receipt", "https://example.invalid/fixture-receipt"
		}
		_, err = conn.Exec(ctx, `UPDATE sp_targets SET status=$2,progress=$3,remote_id=$4,remote_url=$5,
            started_at=CASE WHEN $2 IN ('published','running','failed','processing') THEN clock_timestamp()-interval '5 minutes' ELSE NULL END,
            processing_started_at=CASE WHEN $2 IN ('running','processing') THEN clock_timestamp()-interval '4 minutes' ELSE NULL END,
            attempts=CASE WHEN $2 IN ('published','running','failed','processing') THEN 1 ELSE 0 END,
            lease_token=CASE WHEN $2='running' THEN 'fixture_old_lease' ELSE NULL END,
            lease_until=CASE WHEN $2='running' THEN clock_timestamp()+interval '10 minutes' ELSE NULL END
            WHERE id=$1`, post.Targets[0].ID, status, progress, receipt, receiptURL)
		if err != nil {
			return err
		}
		if _, err = conn.Exec(ctx, "UPDATE sp_posts SET status=$2 WHERE id=$1", post.ID, store.PostStatus([]model.Target{{Status: status}})); err != nil {
			return err
		}
		if status == "published" {
			if _, err = conn.Exec(ctx, `INSERT INTO sp_events(post_id,target_id,account_id,kind,detail)
				VALUES ($1,$2,$3,'target.published','Explicit fixture publication receipt.')`, post.ID, post.Targets[0].ID, account.ID); err != nil {
				return err
			}
		}
	}
	if err = database.BeginTokenRefresh(ctx, accounts[1].ID, accounts[1].TokenVersion); err != nil {
		return err
	}
	var backup manifest
	if err = conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM sp_posts),(SELECT count(*) FROM sp_targets),
        (SELECT count(*) FROM sp_accounts WHERE encrypted_token IS NOT NULL),(SELECT count(*) FROM sp_events),
        (SELECT COALESCE(max(id),0) FROM sp_events)`).Scan(&backup.Posts, &backup.Targets, &backup.EncryptedAccounts, &backup.Events, &backup.MaxEventID); err != nil {
		return err
	}
	backup.PreservedHashes, err = preservedHashes(ctx, conn, backup.MaxEventID)
	if err != nil {
		return err
	}
	encodedManifest, _ := json.MarshalIndent(backup, "", "  ")
	if err = os.WriteFile(filepath.Join(directory, "backup-manifest.json"), encodedManifest, 0600); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"fixture_seeded": true, "posts": backup.Posts, "targets": backup.Targets, "encrypted_accounts": backup.EncryptedAccounts, "oauth_verifiers": 2, "retained_encryption_keys": 2})
}

func verify(ctx context.Context, conn *pgx.Conn, dsn, directory string) error {
	data, err := os.ReadFile(filepath.Join(directory, "backup-manifest.json"))
	if err != nil {
		return err
	}
	var backup manifest
	if err = json.Unmarshal(data, &backup); err != nil {
		return err
	}
	after, err := preservedHashes(ctx, conn, backup.MaxEventID)
	if err != nil {
		return err
	}
	for name, expected := range backup.PreservedHashes {
		if after[name] != expected {
			return fmt.Errorf("restore changed retained %s", name)
		}
	}
	var published, attention, reauth int
	if err = conn.QueryRow(ctx, `SELECT
        (SELECT count(*) FROM sp_targets WHERE id='restore_target_0' AND status='published'),
        (SELECT count(*) FROM sp_targets WHERE id<>'restore_target_0' AND status='needs_attention'),
        (SELECT count(*) FROM sp_accounts WHERE status='reauth_required' AND token_version=2)`).Scan(&published, &attention, &reauth); err != nil || published != 1 || attention != backup.Targets-1 || reauth != backup.EncryptedAccounts {
		return fmt.Errorf("restore did not preserve published status and fence every pending target/account: published=%d attention=%d reauth=%d error=%v", published, attention, reauth, err)
	}
	database, err := store.New(ctx, dsn, 4)
	if err != nil {
		return err
	}
	defer database.Close()
	if _, err = database.Claim(ctx, time.Minute); !errors.Is(err, store.ErrNoWork) {
		return fmt.Errorf("restored database still exposes dispatchable work: %v", err)
	}
	if err = database.SaveProgress(ctx, "restore_target_1", "fixture_old_lease", model.Progress{}); !errors.Is(err, store.ErrLeaseLost) {
		return fmt.Errorf("restored stale worker lease was not fenced: %v", err)
	}
	if err = database.SaveToken(ctx, "restore_account_1", 1, []byte("unused_stale_fixture"), time.Now().Add(time.Hour)); !errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("restored stale token version was not fenced: %v", err)
	}
	for i := range backup.Posts {
		account := fixtureAccount(i % 2)
		post := fixturePost(i, account)
		post.ID = fmt.Sprintf("would_duplicate_%d", i)
		got, created, createErr := database.CreatePost(ctx, post, fmt.Sprintf("restore_key_%d", i), fmt.Sprintf("restore_hash_%d", i))
		if createErr != nil || created || got.ID != fmt.Sprintf("restore_post_%d", i) {
			return fmt.Errorf("retained idempotency replay %d failed: %v", i, createErr)
		}
		if _, err = database.GetPost(ctx, fixtureAccount(1-i%2).Owner, got.ID); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("restored owner isolation failed: %v", err)
		}
	}
	afterReplay, err := preservedHashes(ctx, conn, backup.MaxEventID)
	if err != nil {
		return err
	}
	for name, expected := range backup.PreservedHashes {
		if afterReplay[name] != expected {
			return fmt.Errorf("replay or stale completion changed retained %s", name)
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"restore_preservation_passed": true, "immutable_rows_preserved": afterReplay, "published_receipt_remains_published": true, "old_lease_fenced": true, "old_token_version_fenced": true, "retained_idempotency_replays": backup.Posts, "cross_owner_reads_rejected": true, "dispatchable_work": 0})
}

func fixtureAccount(i int) model.Account {
	return model.Account{ID: fmt.Sprintf("restore_account_%d", i), Owner: model.Principal{TenantID: fmt.Sprintf("fixture_tenant_%d", i), Subject: "fixture_subject"}, Platform: model.Instagram,
		Identity: model.Identity{ID: fmt.Sprintf("fixture_provider_%d", i), Username: "fixture", AccountType: "BUSINESS"}, TokenExpiresAt: time.Now().Add(time.Hour), AuthorizationStartedAt: time.Now().Add(-time.Minute)}
}

func fixturePost(i int, account model.Account) model.Post {
	return model.Post{ID: fmt.Sprintf("restore_post_%d", i), Owner: account.Owner, PublishAt: time.Now().Add(time.Hour), Timezone: "UTC",
		Targets: []model.Target{{ID: fmt.Sprintf("restore_target_%d", i), AccountID: account.ID, Platform: account.Platform, Content: model.Content{Text: "Explicit offline backup/restore fixture"}}}}
}

func preservedHashes(ctx context.Context, conn *pgx.Conn, maxEventID int64) (map[string]string, error) {
	queries := map[string]string{
		"credentials":                  `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb)::text FROM (SELECT id,owner_tenant,owner_subject,platform,provider_user_id,encrypted_token FROM sp_accounts) x`,
		"idempotency":                  `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb)::text FROM (SELECT id,owner_tenant,owner_subject,idempotency_key,request_hash,publish_at,timezone,created_at FROM sp_posts) x`,
		"checkpoints_receipts_history": `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb)::text FROM (SELECT id,post_id,account_id,owner_tenant,owner_subject,platform,position,content,progress,attempts,failures,started_at,processing_started_at,remote_id,remote_url,warnings FROM sp_targets) x`,
		"oauth_verifiers":              `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY state_hash),'[]'::jsonb)::text FROM sp_oauth_states x`,
		"original_events":              fmt.Sprintf(`SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb)::text FROM (SELECT * FROM sp_events WHERE id<=%d) x`, maxEventID),
	}
	result := make(map[string]string)
	for name, query := range queries {
		var encoded string
		if err := conn.QueryRow(ctx, query).Scan(&encoded); err != nil {
			return nil, err
		}
		sum := sha256.Sum256([]byte(encoded))
		result[name] = hex.EncodeToString(sum[:])
	}
	return result, nil
}
