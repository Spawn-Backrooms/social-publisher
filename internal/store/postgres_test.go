package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"socialpublisher/internal/model"
)

func testID(t *testing.T, prefix string) string {
	t.Helper()
	var value [10]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(value[:])
}
func postgresFixture(t *testing.T, maxConns int32) (*Postgres, string) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is unset; PostgreSQL integration test was not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect TEST_DATABASE_URL: %v", err)
	}
	schema := testID(t, "sp_test_")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	if os.Getenv("TEST_PGLITE") == "1" {
		// This single-backend bridge ignores startup search_path. Sequential
		// tests set it explicitly on that shared SQL session instead.
		if _, err = admin.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
			_ = admin.Close(ctx)
			t.Fatal(err)
		}
	}
	_ = admin.Close(ctx)
	scopedDSN := dsn
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, parseErr := url.Parse(dsn)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		scopedDSN = parsed.String()
	} else {
		scopedDSN += " search_path=" + schema
	}
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanCancel()
		conn, connectErr := pgx.Connect(cleanCtx, dsn)
		if connectErr != nil {
			t.Errorf("connect for schema cleanup: %v", connectErr)
			return
		}
		defer conn.Close(cleanCtx)
		if _, dropErr := conn.Exec(cleanCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); dropErr != nil {
			t.Errorf("cleanup schema: %v", dropErr)
		}
		if os.Getenv("TEST_PGLITE") == "1" {
			if _, resetErr := conn.Exec(cleanCtx, "SET search_path TO public"); resetErr != nil {
				t.Errorf("reset shared test search_path: %v", resetErr)
			}
		}
	})
	if err = Migrate(ctx, scopedDSN); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if os.Getenv("TEST_PGLITE") == "1" {
		maxConns = 1
	}
	store, err := New(ctx, scopedDSN, maxConns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store, scopedDSN
}
func fixtureAccount(t *testing.T, s *Postgres, owner model.Principal, platform model.Platform) model.Account {
	t.Helper()
	account, err := s.UpsertAccount(t.Context(), model.Account{ID: testID(t, "account_"), Owner: owner, Platform: platform,
		Identity: model.Identity{ID: testID(t, "provider_"), Username: "fixture", AccountType: "BUSINESS"}, EncryptedToken: []byte("encrypted-fixture"),
		TokenExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond), AuthorizationStartedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return account
}
func fixturePost(t *testing.T, s *Postgres, owner model.Principal, when time.Time, accounts ...model.Account) model.Post {
	t.Helper()
	post := model.Post{ID: testID(t, "post_"), Owner: owner, PublishAt: when, Timezone: "UTC"}
	for _, account := range accounts {
		post.Targets = append(post.Targets, model.Target{ID: testID(t, "target_"), AccountID: account.ID, Platform: account.Platform, Content: model.Content{Text: "fixture caption"}})
	}
	saved, created, err := s.CreatePost(t.Context(), post, testID(t, "key_"), "fixture-hash")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("new fixture unexpectedly replayed")
	}
	return saved
}
func expireLease(t *testing.T, s *Postgres, targetID string) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), "UPDATE sp_targets SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", targetID); err != nil {
		t.Fatal(err)
	}
}
func requireError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("error = %v, want %v", got, want)
	}
}

func TestPostgresMigrations(t *testing.T) {
	s, dsn := postgresFixture(t, 4)
	if err := Migrate(t.Context(), dsn); err != nil {
		t.Fatalf("idempotent migrate: %v", err)
	}
	var count int
	if err := s.pool.QueryRow(t.Context(), "SELECT count(*) FROM sp_schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count < 1 {
		t.Fatal("no migration ledger rows")
	}
	if _, err := s.pool.Exec(t.Context(), "INSERT INTO sp_schema_migrations(version,name,checksum) VALUES (999999,'future.sql','unknown')"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), dsn); err == nil {
		t.Fatal("newer database schema was silently accepted")
	}
}
func TestPostgresOAuthState(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	state := model.OAuthState{StateHash: "state", CookieHash: "browser", Owner: model.Principal{TenantID: "tenant", Subject: "subject"}, Platform: model.Instagram, EncryptedVerifier: []byte("sealed"), ExpiresAt: time.Now().Add(time.Hour)}
	state.IssuedAt = time.Now().Add(24 * time.Hour) // Caller clocks must not define ordering.
	var before, after time.Time
	if err := s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.PutOAuthState(ctx, state); err != nil {
		t.Fatal(err)
	}
	_, err := s.ConsumeOAuthState(ctx, state.StateHash, "wrong", state.Platform)
	requireError(t, err, ErrOAuthState)
	_, err = s.ConsumeOAuthState(ctx, state.StateHash, state.CookieHash, model.X)
	requireError(t, err, ErrOAuthState)
	saved, err := s.ConsumeOAuthState(ctx, state.StateHash, state.CookieHash, state.Platform)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Owner != state.Owner || !bytes.Equal(saved.EncryptedVerifier, state.EncryptedVerifier) || saved.IssuedAt.IsZero() {
		t.Fatal("OAuth owner/verifier changed")
	}
	if err = s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if saved.IssuedAt.Before(before) || saved.IssuedAt.After(after) {
		t.Fatal("OAuth issuance time was not stamped by database")
	}
	_, err = s.ConsumeOAuthState(ctx, state.StateHash, state.CookieHash, state.Platform)
	requireError(t, err, ErrOAuthState)
	state.StateHash = "expired"
	if err = s.PutOAuthState(ctx, state); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE sp_oauth_states SET expires_at=clock_timestamp()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	_, err = s.ConsumeOAuthState(ctx, state.StateHash, state.CookieHash, state.Platform)
	requireError(t, err, ErrOAuthState)
	var remaining int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM sp_oauth_states").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("expired OAuth state was not cleaned up")
	}
}
func TestPostgresOwnershipAndIdempotency(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "one"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	for _, other := range []model.Principal{{TenantID: "different", Subject: "one"}, {TenantID: "tenant", Subject: "different"}} {
		_, err := s.GetAccount(ctx, other, account.ID)
		requireError(t, err, ErrNotFound)
		accounts, err := s.ListAccounts(ctx, other)
		if err != nil || len(accounts) != 0 {
			t.Fatalf("account isolation: %v %v", accounts, err)
		}
		takeover := account
		takeover.ID = testID(t, "account_")
		takeover.Owner = other
		takeover.AuthorizationStartedAt = time.Now().UTC()
		_, err = s.UpsertAccount(ctx, takeover)
		requireError(t, err, ErrConflict)
	}
	post := model.Post{ID: testID(t, "post_"), Owner: owner, PublishAt: time.Now().Add(time.Hour), Targets: []model.Target{{ID: testID(t, "target_"), AccountID: account.ID, Platform: account.Platform}}}
	saved, created, err := s.CreatePost(ctx, post, "same-key", "same-hash")
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	post.ID = testID(t, "post_")
	post.Targets[0].ID = testID(t, "target_")
	duplicate, created, err := s.CreatePost(ctx, post, "same-key", "same-hash")
	if err != nil || created || duplicate.ID != saved.ID {
		t.Fatalf("replay: %v %v", created, err)
	}
	_, _, err = s.CreatePost(ctx, post, "same-key", "different-hash")
	requireError(t, err, ErrIdempotency)
	_, err = s.LookupIdempotency(ctx, owner, "same-key", "different-hash")
	requireError(t, err, ErrIdempotency)
	other := model.Principal{TenantID: owner.TenantID, Subject: "other"}
	_, err = s.GetPost(ctx, other, saved.ID)
	requireError(t, err, ErrNotFound)
	_, err = s.ListEvents(ctx, other, saved.ID)
	requireError(t, err, ErrNotFound)
	_, err = s.LookupIdempotency(ctx, other, "same-key", "same-hash")
	requireError(t, err, ErrNotFound)
	invalid := post
	invalid.Owner = other
	invalid.ID = testID(t, "post_")
	invalid.Targets[0].ID = testID(t, "target_")
	_, _, err = s.CreatePost(ctx, invalid, "unauthorized-key", "hash")
	requireError(t, err, ErrNotFound)
	_, err = s.GetPost(ctx, other, invalid.ID)
	requireError(t, err, ErrNotFound)
	list, err := s.ListPosts(ctx, other, 10)
	if err != nil || len(list) != 0 {
		t.Fatalf("post isolation: %v %v", list, err)
	}
	if _, err = s.Disconnect(ctx, owner, account.ID); err != nil {
		t.Fatal(err)
	}
	// Idempotency remains valid after live prerequisites no longer hold.
	replayed, err := s.LookupIdempotency(ctx, owner, "same-key", "same-hash")
	if err != nil || replayed.ID != saved.ID {
		t.Fatalf("disconnected replay: %v", err)
	}
	takeover := account
	takeover.Owner = other
	takeover.ID = testID(t, "account_")
	takeover.AuthorizationStartedAt = time.Now().UTC()
	_, err = s.UpsertAccount(ctx, takeover)
	requireError(t, err, ErrConflict)
	account.AuthorizationStartedAt = time.Now().UTC()
	reconnected, err := s.UpsertAccount(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if reconnected.ID != account.ID || reconnected.Status != "active" || reconnected.TokenVersion <= account.TokenVersion {
		t.Fatal("same-owner tombstone reconnect failed")
	}
}
func TestPostgresTokenCASAndRefreshCandidates(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	_ = fixtureAccount(t, s, owner, model.X)
	expires := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Microsecond)
	if err := s.BeginTokenRefresh(ctx, account.ID, account.TokenVersion); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken(ctx, account.ID, account.TokenVersion, []byte("rotated"), expires); err != nil {
		t.Fatal(err)
	}
	requireError(t, s.SaveToken(ctx, account.ID, account.TokenVersion, []byte("stale"), expires), ErrConflict)
	saved, err := s.GetAccount(ctx, owner, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved.EncryptedToken) != "rotated" || saved.TokenVersion != account.TokenVersion+1 || !saved.TokenExpiresAt.Equal(expires) {
		t.Fatal("token/version/expiry were not updated atomically")
	}
	candidates, err := s.ListRefreshCandidates(ctx, model.Instagram, time.Now().Add(time.Hour), 100)
	if err != nil || len(candidates) != 1 || candidates[0].ID != account.ID {
		t.Fatalf("platform-scoped refresh candidates: %v %v", candidates, err)
	}
	if err = s.MarkReauth(ctx, account.ID, saved.TokenVersion); err != nil {
		t.Fatal(err)
	}
	requireError(t, s.SaveToken(ctx, account.ID, saved.TokenVersion+1, []byte("revived"), expires), ErrConflict)
	candidates, err = s.ListRefreshCandidates(ctx, model.Instagram, time.Now().Add(2*time.Hour), 100)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("reauth account was selected for refresh: %v", err)
	}
	previous, err := s.Disconnect(ctx, owner, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(previous.EncryptedToken) != "rotated" {
		t.Fatal("disconnect did not return prior credential for revoke")
	}
	disconnected, err := s.GetAccount(ctx, owner, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if disconnected.Status != "disconnected" || len(disconnected.EncryptedToken) != 0 || !disconnected.TokenExpiresAt.IsZero() {
		t.Fatal("disconnect retained usable stored credentials")
	}
	requireError(t, s.SaveToken(ctx, account.ID, disconnected.TokenVersion, []byte("revived"), expires), ErrConflict)
}

func TestPostgresLeasesFencingAndRecovery(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
	first, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != post.Targets[0].ID || first.StartedAt == nil || first.LeaseUntil == nil || first.LeaseToken == "" || first.Attempts != 1 {
		t.Fatal("claim did not durably initialize lease/start/attempt")
	}
	requireError(t, s.Heartbeat(ctx, first.ID, "wrong", time.Minute), ErrLeaseLost)
	requireError(t, s.SaveProgress(ctx, first.ID, "wrong", model.Progress{}), ErrLeaseLost)
	requireError(t, s.FinishTarget(ctx, first.ID, "wrong", Finish{Status: "failed"}), ErrLeaseLost)
	progress := model.Progress{Stage: "container_ready", Data: json.RawMessage(`{"container_id":"known"}`)}
	if err = s.SaveProgress(ctx, first.ID, first.LeaseToken, progress); err != nil {
		t.Fatal(err)
	}
	if err = s.Heartbeat(ctx, first.ID, first.LeaseToken, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	expireLease(t, s, first.ID)
	requireError(t, s.Heartbeat(ctx, first.ID, first.LeaseToken, time.Minute), ErrLeaseLost)
	requireError(t, s.SaveProgress(ctx, first.ID, first.LeaseToken, model.Progress{}), ErrLeaseLost)
	requireError(t, s.FinishTarget(ctx, first.ID, first.LeaseToken, Finish{Status: "failed"}), ErrLeaseLost)
	recovered, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != first.ID || recovered.LeaseToken == first.LeaseToken || recovered.Attempts != 2 || recovered.Progress.Stage != progress.Stage {
		t.Fatal("safe recovery lost identity/checkpoint or reused lease")
	}
	requireError(t, s.FinishTarget(ctx, first.ID, first.LeaseToken, Finish{Status: "published", RemoteID: "stale"}), ErrLeaseLost)
	unsafe := model.Progress{Stage: "publishing", Data: progress.Data, Unsafe: true}
	if err = s.SaveProgress(ctx, recovered.ID, recovered.LeaseToken, unsafe); err != nil {
		t.Fatal(err)
	}
	expireLease(t, s, recovered.ID)
	_, err = s.Claim(ctx, time.Minute)
	requireError(t, err, ErrNoWork)
	saved, err := s.GetPost(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := saved.Targets[0]
	if saved.Status != "needs_attention" || target.Status != "needs_attention" || !target.Progress.Unsafe || target.LeaseToken != "" || target.LeaseUntil != nil {
		t.Fatal("unsafe recovery did not fence and quarantine delivery")
	}
	_, err = s.RetryTarget(ctx, owner, post.ID, target.ID)
	requireError(t, err, ErrConflict)
	_, err = s.ResolveTarget(ctx, owner, post.ID, target.ID, Resolution{Action: "resume_processing", Note: "Try to resume uncertain publication"})
	requireError(t, err, ErrConflict)
	_, err = s.ResolveTarget(ctx, owner, post.ID, target.ID, Resolution{Action: "confirm_not_published"})
	requireError(t, err, ErrConflict)
	saved, err = s.ResolveTarget(ctx, owner, post.ID, target.ID, Resolution{Action: "confirm_not_published", Note: "Owner verified absence on provider"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Targets[0].Status != "failed" || saved.Targets[0].Progress.Unsafe || saved.Targets[0].Progress.Stage != "" {
		t.Fatal("resolution did not clear uncertainty without publishing")
	}
	if _, err = s.RetryTarget(ctx, owner, post.ID, target.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTarget(ctx, retry.ID, retry.LeaseToken, Finish{Status: "published", RemoteID: "confirmed", RemoteURL: "https://www.instagram.com/p/confirmed/"}); err != nil {
		t.Fatal(err)
	}
	saved, err = s.GetPost(ctx, owner, post.ID)
	if err != nil || saved.Status != "published" || saved.Targets[0].RemoteID != "confirmed" {
		t.Fatalf("successful retry: %v", err)
	}
	events, err := s.ListEvents(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make(map[string]bool)
	for _, event := range events {
		kinds[event.Kind] = true
	}
	for _, kind := range []string{"target.lease_recovered", "target.needs_attention", "target.confirm_not_published", "target.retried", "target.published"} {
		if !kinds[kind] {
			t.Errorf("missing transactional audit event %s", kind)
		}
	}
}

func TestPostgresFinishPreservesUncertainty(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.X)
	post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
	target, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProgress(ctx, target.ID, target.LeaseToken, model.Progress{Stage: "create_tweet", Unsafe: true}); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTarget(ctx, target.ID, target.LeaseToken, Finish{Status: "retrying", CountFailure: true}); err != nil {
		t.Fatal(err)
	}
	saved, err := s.GetPost(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Targets[0].Status != "needs_attention" || !saved.Targets[0].Progress.Unsafe || saved.Targets[0].Failures != 1 {
		t.Fatal("zero-value finish erased uncertainty")
	}
	_, err = s.ResolveTarget(ctx, owner, post.ID, target.ID, Resolution{Action: "confirm_published", RemoteID: "remote", Note: "Verified provider receipt"})
	if err != nil {
		t.Fatal(err)
	}
	saved, err = s.GetPost(ctx, owner, post.ID)
	if err != nil || saved.Status != "published" || saved.Targets[0].Progress.Unsafe {
		t.Fatalf("confirmed publication: %v", err)
	}
	other := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
	target, err = s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProgress(ctx, target.ID, target.LeaseToken, model.Progress{Stage: "create_tweet", Unsafe: true}); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProgress(ctx, target.ID, target.LeaseToken, model.Progress{Stage: "media_ready"}); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTarget(ctx, target.ID, target.LeaseToken, Finish{Status: "retrying", Progress: model.Progress{Stage: "media_ready"}, NextAttemptAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	saved, err = s.GetPost(ctx, owner, other.ID)
	if err != nil || saved.Targets[0].Status != "processing" {
		t.Fatalf("proven rejection retry: %v", err)
	}
}

func TestPostgresCancelRescheduleAndPartialSuccess(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.Instagram)
	b := fixtureAccount(t, s, owner, model.X)
	post := fixturePost(t, s, owner, time.Now().Add(time.Hour), a, b)
	_, err := s.Claim(ctx, time.Minute)
	requireError(t, err, ErrNoWork)
	due := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	rescheduled, err := s.ReschedulePost(ctx, owner, post.ID, post.Version, due, "Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	if rescheduled.Version != post.Version+1 || !rescheduled.PublishAt.Equal(due) || rescheduled.Timezone != "Europe/London" {
		t.Fatal("reschedule lost instant, metadata or version")
	}
	_, err = s.ReschedulePost(ctx, owner, post.ID, post.Version, due, "UTC")
	requireError(t, err, ErrConflict)
	target, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.GetPost(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ReschedulePost(ctx, owner, post.ID, current.Version, time.Now().Add(time.Hour), "UTC")
	requireError(t, err, ErrConflict)
	cancelled, err := s.CancelPost(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Targets[0].Status != "running" || cancelled.Targets[1].Status != "cancelled" {
		t.Fatal("cancel modified running work or missed pending work")
	}
	if err = s.FinishTarget(ctx, target.ID, target.LeaseToken, Finish{Status: "published", RemoteID: "remote"}); err != nil {
		t.Fatal(err)
	}
	finished, err := s.GetPost(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != "partially_published" || finished.Targets[0].RemoteID != "remote" {
		t.Fatal("partial success was lost")
	}
	_, err = s.RetryTarget(ctx, owner, post.ID, finished.Targets[1].ID)
	requireError(t, err, ErrConflict)
	again, err := s.CancelPost(ctx, owner, post.ID)
	if err != nil || again.Version != finished.Version {
		t.Fatalf("idempotent cancel changed terminal work: %v", err)
	}
}

func TestPostgresDisconnectAndReauthStopWork(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	processingPost := fixturePost(t, s, owner, time.Now().Add(-4*time.Minute), account)
	processing, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	progress := model.Progress{Stage: "processing", Data: json.RawMessage(`{"remote_job":"known"}`)}
	if err = s.FinishTarget(ctx, processing.ID, processing.LeaseToken, Finish{Status: "processing", Progress: progress, NextAttemptAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	runningPost := fixturePost(t, s, owner, time.Now().Add(-3*time.Minute), account)
	running, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	unsafePost := fixturePost(t, s, owner, time.Now().Add(-2*time.Minute), account)
	// Disconnect must fence even an older/raced second live lease. Normal
	// dispatch now excludes this account while its first target is running,
	// so seed that pre-existing state explicitly instead of bypassing fairness.
	unsafe, err := scanTarget(s.pool.QueryRow(ctx, `UPDATE sp_targets SET status='running',lease_token='fixture-unsafe-lease',
        lease_until=clock_timestamp()+interval '1 minute',started_at=clock_timestamp(),attempts=attempts+1
        WHERE id=$1 RETURNING `+targetColumns, unsafePost.Targets[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProgress(ctx, unsafe.ID, unsafe.LeaseToken, model.Progress{Stage: "publishing", Unsafe: true}); err != nil {
		t.Fatal(err)
	}
	pendingPost := fixturePost(t, s, owner, time.Now().Add(time.Hour), account)
	if _, err = s.Disconnect(ctx, owner, account.ID); err != nil {
		t.Fatal(err)
	}
	for _, expect := range []struct {
		post   model.Post
		status string
	}{{processingPost, "needs_attention"}, {runningPost, "cancelled"}, {unsafePost, "needs_attention"}, {pendingPost, "cancelled"}} {
		saved, getErr := s.GetPost(ctx, owner, expect.post.ID)
		if getErr != nil || saved.Targets[0].Status != expect.status || saved.Targets[0].LeaseToken != "" {
			t.Fatalf("disconnect target status = %+v, %v; want %s", saved.Targets, getErr, expect.status)
		}
	}
	requireError(t, s.Heartbeat(ctx, running.ID, running.LeaseToken, time.Minute), ErrLeaseLost)
	requireError(t, s.FinishTarget(ctx, unsafe.ID, unsafe.LeaseToken, Finish{Status: "published", RemoteID: "late"}), ErrLeaseLost)
	_, err = s.Claim(ctx, time.Minute)
	requireError(t, err, ErrNoWork)
	account.AuthorizationStartedAt = time.Now().UTC()
	account, err = s.UpsertAccount(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	queued := fixturePost(t, s, owner, time.Now().Add(time.Hour), account)
	remote := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
	working, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTarget(ctx, working.ID, working.LeaseToken, Finish{Status: "processing", Progress: progress, NextAttemptAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkReauth(ctx, account.ID, account.TokenVersion); err != nil {
		t.Fatal(err)
	}
	for _, expect := range []struct {
		post   model.Post
		status string
	}{{queued, "failed"}, {remote, "needs_attention"}} {
		saved, getErr := s.GetPost(ctx, owner, expect.post.ID)
		if getErr != nil || saved.Targets[0].Status != expect.status {
			t.Fatalf("reauth target status = %+v, %v", saved.Targets, getErr)
		}
	}
}

func TestPostgresOAuthReconnectFence(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	if _, err := s.Disconnect(ctx, owner, account.ID); err != nil {
		t.Fatal(err)
	}
	var disconnectedAt time.Time
	if err := s.pool.QueryRow(ctx, "SELECT disconnected_at FROM sp_accounts WHERE id=$1", account.ID).Scan(&disconnectedAt); err != nil {
		t.Fatal(err)
	}
	for _, started := range []time.Time{disconnectedAt.Add(-time.Minute), disconnectedAt} {
		account.AuthorizationStartedAt = started
		_, err := s.UpsertAccount(ctx, account)
		requireError(t, err, ErrConflict)
	}
	var betweenDisconnects time.Time
	if err := s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&betweenDisconnects); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Disconnect(ctx, owner, account.ID); err != nil {
		t.Fatal(err)
	}
	account.AuthorizationStartedAt = betweenDisconnects
	_, err := s.UpsertAccount(ctx, account)
	requireError(t, err, ErrConflict)
	if err = s.pool.QueryRow(ctx, "SELECT disconnected_at FROM sp_accounts WHERE id=$1", account.ID).Scan(&disconnectedAt); err != nil {
		t.Fatal(err)
	}
	account.AuthorizationStartedAt = disconnectedAt.Add(time.Second)
	reconnected, err := s.UpsertAccount(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if reconnected.Status != "active" || reconnected.ID != account.ID {
		t.Fatal("fresh authorization did not restore original owner connection")
	}
	// A callback older than the disconnect stays invalid even after a newer
	// flow reconnects; reconnect must retain the disconnect fence timestamp.
	account.AuthorizationStartedAt = disconnectedAt.Add(-time.Second)
	_, err = s.UpsertAccount(ctx, account)
	requireError(t, err, ErrConflict)
}

func TestPostgresAccountGuardContextAndRelease(t *testing.T) {
	s, _ := postgresFixture(t, 1)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	guard, release, err := s.AcquireAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if guard.Err() != nil {
		t.Fatal("fresh account guard is cancelled")
	}
	if _, err = s.GetAccount(guard, owner, account.ID); err != nil {
		t.Fatal(err)
	}
	release()
	release()
	if !errors.Is(guard.Err(), context.Canceled) {
		t.Fatal("released guard context is not cancelled")
	}
	if s.locks.Stat().AcquiredConns() != 0 {
		t.Fatal("released guard retained its database connection")
	}
}

func TestPostgresKnownProgressRecoveryCannotBeCancelledOrRetried(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.Instagram)
	b := fixtureAccount(t, s, owner, model.X)
	post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), a, b)
	first, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("same target claimed twice before lease expiry")
	}
	if err = s.SaveProgress(ctx, first.ID, first.LeaseToken, model.Progress{Stage: "processing", Data: json.RawMessage(`{"job":"one"}`)}); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProgress(ctx, second.ID, second.LeaseToken, model.Progress{Data: json.RawMessage(`{"job":"two"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE sp_targets SET lease_until=clock_timestamp()-interval '1 second' WHERE post_id=$1", post.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.CancelPost(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	var remaining model.Target
	for _, target := range saved.Targets {
		if target.ID == claimed.ID {
			if target.Status != "running" {
				t.Fatal("cancel changed running recovered operation")
			}
		} else {
			remaining = target
			if target.Status != "processing" || !hasKnownProgress(target.Progress) {
				t.Fatal("unclaimed recovered operation became safely cancellable")
			}
		}
	}
	if err = s.FinishTarget(ctx, claimed.ID, claimed.LeaseToken, Finish{Status: "failed", Error: "polling failed"}); err != nil {
		t.Fatal(err)
	}
	saved, err = s.GetPost(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range saved.Targets {
		if target.ID == claimed.ID && (target.Status != "needs_attention" || !hasKnownProgress(target.Progress)) {
			t.Fatal("terminal polling failure discarded operation ID")
		}
	}
	// Defend against legacy rows created before the worker stopped classifying
	// known remote operations as ordinary retrying/failed jobs.
	if _, err = s.pool.Exec(ctx, "UPDATE sp_targets SET status='retrying' WHERE id=$1", remaining.ID); err != nil {
		t.Fatal(err)
	}
	saved, err = s.CancelPost(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range saved.Targets {
		if target.ID == remaining.ID && target.Status != "retrying" {
			t.Fatal("cancel erased known progress in legacy retrying state")
		}
	}
	if _, err = s.pool.Exec(ctx, "UPDATE sp_targets SET status='failed' WHERE id=$1", remaining.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.RetryTarget(ctx, owner, post.ID, remaining.ID)
	requireError(t, err, ErrConflict)
	if _, err = s.pool.Exec(ctx, "UPDATE sp_targets SET status='retrying' WHERE id=$1", remaining.ID); err != nil {
		t.Fatal(err)
	}
	account, err := s.GetWorkerAccount(ctx, remaining.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MarkReauth(ctx, account.ID, account.TokenVersion); err != nil {
		t.Fatal(err)
	}
	saved, err = s.GetPost(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range saved.Targets {
		if target.ID == remaining.ID && (target.Status != "needs_attention" || !hasKnownProgress(target.Progress)) {
			t.Fatal("reauth erased known progress in legacy retrying state")
		}
	}
}

func TestPostgresResumeProcessingPreservesKnownOperation(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
	target, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	known := model.Progress{Stage: "processing", Data: json.RawMessage(`{"remote_job":"retained"}`)}
	if err = s.SaveProgress(ctx, target.ID, target.LeaseToken, known); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkReauth(ctx, account.ID, account.TokenVersion); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTarget(ctx, target.ID, target.LeaseToken, Finish{Status: "needs_attention", Progress: known, RemoteID: "retained", RemoteURL: "https://www.instagram.com/p/retained/", CountFailure: true}); err != nil {
		t.Fatal(err)
	}
	resolution := Resolution{Action: "resume_processing", Note: "Account reconnected; poll saved operation"}
	_, err = s.ResolveTarget(ctx, owner, post.ID, target.ID, resolution)
	requireError(t, err, ErrConflict)
	account.AuthorizationStartedAt = time.Now().UTC()
	if _, err = s.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	// Scheduling uses the database clock, which need not match the test host.
	// Bracket the resume so an old timestamp cannot satisfy immediate polling.
	var beforeResume, afterResume time.Time
	if err = s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&beforeResume); err != nil {
		t.Fatal(err)
	}
	saved, err := s.ResolveTarget(ctx, owner, post.ID, target.ID, resolution)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&afterResume); err != nil {
		t.Fatal(err)
	}
	resumed := saved.Targets[0]
	if resumed.Status != "processing" || resumed.Progress.Stage != known.Stage || !json.Valid(resumed.Progress.Data) ||
		resumed.RemoteID != "retained" || resumed.RemoteURL != "https://www.instagram.com/p/retained/" || resumed.Failures != 0 {
		t.Fatalf("resume did not preserve receipt/progress and reset polling budget: status=%q progress=%+v remote_id=%q remote_url=%q failures=%d",
			resumed.Status, resumed.Progress, resumed.RemoteID, resumed.RemoteURL, resumed.Failures)
	}
	if resumed.NextAttemptAt.Before(beforeResume) || resumed.NextAttemptAt.After(afterResume) {
		t.Fatalf("resume polling timestamp %s is outside database interval [%s, %s]", resumed.NextAttemptAt, beforeResume, afterResume)
	}
	var data map[string]string
	if err = json.Unmarshal(resumed.Progress.Data, &data); err != nil || data["remote_job"] != "retained" {
		t.Fatal("resume discarded remote operation ID")
	}
	if _, err = s.pool.Exec(ctx, "UPDATE sp_targets SET next_attempt_at=clock_timestamp()+interval '1 hour' WHERE id=$1", target.ID); err != nil {
		t.Fatal(err)
	}
	emptyPost := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
	empty, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTarget(ctx, empty.ID, empty.LeaseToken, Finish{Status: "needs_attention"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.ResolveTarget(ctx, owner, emptyPost.ID, empty.ID, resolution)
	requireError(t, err, ErrConflict)
}
