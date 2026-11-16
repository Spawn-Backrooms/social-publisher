package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"socialpublisher/internal/account"
	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
	"socialpublisher/internal/worker"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type receiptAdapter struct {
	*apiAdapter
	fail bool
}

// Safe contention is an expected dispatch result. Force it once so this flow
// cannot accidentally assume every worker iteration finishes a delivery.
type deferFirstAccountStore struct {
	store.Store
	deferred bool
}

func (s *deferFirstAccountStore) TryAcquireAccount(ctx context.Context, id string) (context.Context, func(), error) {
	if !s.deferred {
		s.deferred = true
		return nil, nil, store.ErrAccountBusy
	}
	return s.Store.TryAcquireAccount(ctx, id)
}

func databaseFlowStore(t *testing.T, ctx context.Context, dsn, schema string) *store.Postgres {
	t.Helper()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(cleanup, dsn)
		if err != nil {
			t.Errorf("connect for API schema cleanup: %v", err)
			return
		}
		defer conn.Close(cleanup)
		if _, err = conn.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("clean API schema: %v", err)
		}
		if os.Getenv("TEST_PGLITE") == "1" {
			if _, err = conn.Exec(cleanup, "SET search_path TO public"); err != nil {
				t.Errorf("reset shared test search_path: %v", err)
			}
		}
	})
	if os.Getenv("TEST_PGLITE") == "1" {
		if _, err = admin.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
			_ = admin.Close(ctx)
			t.Fatal(err)
		}
	}
	_ = admin.Close(ctx)
	scoped := dsn
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		scoped = parsed.String()
	} else {
		scoped += " search_path=" + schema
	}
	if err = store.Migrate(ctx, scoped); err != nil {
		t.Fatal(err)
	}
	maxConns := int32(4)
	if os.Getenv("TEST_PGLITE") == "1" {
		maxConns = 1
	}
	db, err := store.New(ctx, scoped, maxConns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func (p *receiptAdapter) Publish(_ context.Context, _ model.Token, _ model.Content, _ model.Progress, save provider.Checkpoint) (model.Outcome, error) {
	if p.fail {
		return model.Outcome{}, &provider.Error{Code: "fixture_rejected"}
	}
	if err := save(model.Progress{Stage: "publishing", Unsafe: true}); err != nil {
		return model.Outcome{}, err
	}
	return model.Outcome{Done: true, RemoteID: "fixture-post-123", Progress: model.Progress{Stage: "done"}}, nil
}

// This exercises the real SQL store, HTTP API and worker together. Provider
// boundaries are explicit fixtures: the test never publishes to a real account.
func TestDatabaseBackedPostFlow(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for database-backed API test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	nonce, err := security.RandomToken(12)
	if err != nil {
		t.Fatal(err)
	}
	db := databaseFlowStore(t, ctx, dsn, "sp_api_"+hex.EncodeToString([]byte(nonce)))
	vault, _ := security.NewVault(map[string][]byte{"primary": make([]byte, 32)}, "primary")
	jwt, _ := security.NewJWT(make([]byte, 32), "test-app", "publisher")
	principal := model.Principal{TenantID: "database-flow-" + nonce, Subject: "alice"}
	userToken, _ := jwt.Mint(principal, time.Hour, time.Now())
	fixture := &apiStore{}
	x := &receiptAdapter{apiAdapter: &apiAdapter{store: fixture}}
	ig := &receiptAdapter{apiAdapter: &apiAdapter{store: fixture}, fail: true}
	registry := provider.Registry{model.X: x, model.Instagram: ig}
	for _, p := range []model.Platform{model.X, model.Instagram} {
		a := model.Account{ID: string(p) + "-" + nonce, Owner: principal, Platform: p, Identity: model.Identity{ID: "provider-" + string(p) + "-" + nonce, AccountType: "BUSINESS"}, Status: "active", TokenExpiresAt: time.Now().Add(time.Hour), AuthorizationStartedAt: time.Now()}
		raw, _ := json.Marshal(model.Token{AccessToken: "fixture-access", ExpiresAt: a.TokenExpiresAt})
		a.EncryptedToken, _ = vault.Seal(raw, a.TokenAAD())
		lockctx, release, err := db.AcquireIdentity(ctx, p, a.Identity.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.UpsertAccount(lockctx, a)
		release()
		if err != nil {
			t.Fatal(err)
		}
	}
	tokens := &account.Manager{Store: db, Vault: vault, Adapters: registry}
	media, _ := security.NewMediaFetcher(nil, 1024)
	s := &Server{Store: db, Adapters: registry, JWT: jwt, Vault: vault, Tokens: tokens, Media: media}
	h := s.Handler()
	body := `{"text":"database workflow fixture","targets":[{"account_id":"x-` + nonce + `"},{"account_id":"instagram-` + nonce + `"}]}`
	result := req(h, "POST", "/v1/posts", body, userToken)
	if result.Code != 201 {
		t.Fatalf("new post: %d %s", result.Code, result.Body.String())
	}
	var p model.Post
	if err = json.Unmarshal(result.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	replay := req(h, "POST", "/v1/posts", body, userToken)
	if replay.Code != 200 {
		t.Fatalf("replay %d %s", replay.Code, replay.Body.String())
	}
	dispatch := &deferFirstAccountStore{Store: db}
	engine := &worker.Engine{Store: dispatch, Tokens: tokens, Adapters: registry, Media: media}
	for {
		if err = engine.RunOnce(ctx); err != nil && !errors.Is(err, store.ErrNoWork) {
			t.Fatal(err)
		}
		current, err := db.GetPost(ctx, principal, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == "partially_published" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("deliveries did not finish after safe deferral: %v, post=%+v", ctx.Err(), current)
		case <-time.After(25 * time.Millisecond):
		}
	}
	result = req(h, "GET", "/v1/posts/"+p.ID, "", userToken)
	if result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	json.Unmarshal(result.Body.Bytes(), &p)
	if p.Status != "partially_published" || len(p.Targets) != 2 || p.Targets[0].Status != "published" || p.Targets[0].RemoteID != "fixture-post-123" || p.Targets[1].Status != "failed" {
		t.Fatalf("independent results lost: %+v", p)
	}
	if !dispatch.deferred || p.Targets[0].Failures != 0 || p.Targets[1].Failures != 1 {
		t.Fatalf("safe contention changed delivery failure accounting: %+v", p.Targets)
	}
	other, _ := jwt.Mint(model.Principal{TenantID: principal.TenantID, Subject: "bob"}, time.Hour, time.Now())
	if w := req(h, "GET", "/v1/posts/"+p.ID, "", other); w.Code != 404 {
		t.Fatal("database ownership isolation failed")
	}
	// Add a distinct scheduled post, prove it cannot be claimed early, then
	// cancel it and verify cancellation persists through the real query path.
	scheduled := model.Post{ID: "scheduled-" + nonce, Owner: principal, PublishAt: time.Now().Add(time.Hour), Timezone: "Africa/Lagos", Version: 1, Targets: []model.Target{{ID: "scheduled-target-" + nonce, AccountID: "x-" + nonce, Platform: model.X, Content: model.Content{Text: "future"}, Status: "queued", NextAttemptAt: time.Now().Add(time.Hour)}}}
	scheduled.Targets[0].PostID = scheduled.ID
	if _, created, err := db.CreatePost(ctx, scheduled, "future-key", "future-hash"); err != nil || !created {
		t.Fatalf("schedule persist: %v created=%v", err, created)
	}
	if _, err = db.Claim(ctx, time.Minute); !errors.Is(err, store.ErrNoWork) {
		t.Fatalf("future schedule claimed early: %v", err)
	}
	cancelled, err := db.CancelPost(ctx, principal, scheduled.ID)
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatalf("cancel: %v %+v", err, cancelled)
	}
	events, err := db.ListEvents(ctx, principal, p.ID)
	if err != nil || len(events) < 3 {
		t.Fatal("missing transactional delivery events")
	}
}
