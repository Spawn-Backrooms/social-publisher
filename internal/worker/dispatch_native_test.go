package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"socialpublisher/internal/account"
	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/provider/tiktok"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
)

func nativeWorkerFixture(t *testing.T) (*store.Postgres, string, *pgx.Conn, *security.Vault) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is unset; native worker coordination was not run")
	}
	if os.Getenv("TEST_PGLITE") == "1" {
		t.Skip("independent PostgreSQL lock sessions are required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("worker_dispatch_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("native worker fixture requires a PostgreSQL URL")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	scoped := parsed.String()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close(ctx)
		if _, err = conn.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if err = store.Migrate(ctx, scoped); err != nil {
		t.Fatal(err)
	}
	s, err := store.New(ctx, scoped, 12)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	conn, err := pgx.Connect(ctx, scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	vault, err := security.NewVault(map[string][]byte{"native-fixture": make([]byte, 32)}, "native-fixture")
	if err != nil {
		t.Fatal(err)
	}
	return s, scoped, conn, vault
}

func nativeAccount(t *testing.T, s *store.Postgres, vault *security.Vault, id string, platform model.Platform) (model.Account, model.Token) {
	t.Helper()
	a := model.Account{ID: id, Owner: model.Principal{TenantID: "tenant", Subject: id}, Platform: platform, Identity: model.Identity{ID: id}, Status: "active", AuthorizationStartedAt: time.Now().UTC()}
	token := model.Token{AccessToken: "fixture-access-" + id, ProviderUserID: id, Scopes: []string{"video.publish", "user.info.basic"}, ExpiresAt: time.Now().Add(time.Hour)}
	if platform == model.Instagram {
		token.Scopes = []string{"instagram_business_basic", "instagram_business_content_publish"}
	}
	blob, _ := json.Marshal(token)
	var err error
	a.EncryptedToken, err = vault.Seal(blob, a.TokenAAD())
	if err != nil {
		t.Fatal(err)
	}
	a.TokenExpiresAt = token.ExpiresAt
	a, err = s.UpsertAccount(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	return a, token
}

func nativePost(t *testing.T, s *store.Postgres, a model.Account, id string, when time.Time, content model.Content) model.Post {
	t.Helper()
	p, created, err := s.CreatePost(t.Context(), model.Post{ID: "post-" + id, Owner: a.Owner, PublishAt: when, Targets: []model.Target{{ID: "target-" + id, AccountID: a.ID, Platform: a.Platform, Content: content}}}, "key-"+id, "hash-"+id)
	if err != nil || !created {
		t.Fatalf("create worker post: created=%v error=%v", created, err)
	}
	return p
}

func TestNativeWorkerOtherAccountFinishesWhileBusyAccountIsBlocked(t *testing.T) {
	s, _, _, vault := nativeWorkerFixture(t)
	a, _ := nativeAccount(t, s, vault, "a", model.X)
	b, _ := nativeAccount(t, s, vault, "b", model.X)
	for i := 0; i < 80; i++ {
		nativePost(t, s, a, fmt.Sprintf("a-%d", i), time.Now().Add(-time.Hour), model.Content{Text: "account a"})
	}
	other := nativePost(t, s, b, "b", time.Now().Add(-time.Minute), model.Content{Text: "account b"})
	p := &blockedAccountAdapter{aStarted: make(chan struct{}), aRelease: make(chan struct{})}
	registry := provider.Registry{model.X: p}
	e := &Engine{Store: s, Tokens: &account.Manager{Store: s, Vault: vault, Adapters: registry}, Adapters: registry, Concurrency: 6, PollInterval: 10 * time.Millisecond, LeaseDuration: 3 * time.Second, JobTimeout: time.Minute}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); close(p.aRelease); <-done }()
	select {
	case <-p.aStarted:
	case <-ctx.Done():
		t.Fatal("account A never entered its blocking provider request")
	}
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		post, err := s.GetPost(ctx, b.Owner, other.ID)
		if err != nil {
			t.Fatal(err)
		}
		if post.Targets[0].Status == "published" {
			if post.Targets[0].RemoteID != "receipt-b" {
				t.Fatal("account B lost its receipt")
			}
			select {
			case <-p.aRelease:
				t.Fatal("B only completed after releasing A")
			default:
			}
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("80-target busy-account backlog prevented B completing while A remained blocked")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestNativeWorkerTikTokStatusPacingAcrossTargetsAndReplicas(t *testing.T) {
	s, dsn, sql, vault := nativeWorkerFixture(t)
	a, token := nativeAccount(t, s, vault, "creator", model.TikTok)
	var mu sync.Mutex
	var calls []time.Time
	var initializations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/post/publish/creator_info/query/":
			_, _ = fmt.Fprint(w, `{"data":{"creator_username":"creator","creator_nickname":"Creator","privacy_level_options":["PUBLIC_TO_EVERYONE"],"comment_disabled":false,"duet_disabled":false,"stitch_disabled":false,"max_video_post_duration_sec":600},"error":{"code":"ok"}}`)
		case "/v2/post/publish/video/init/":
			count := initializations.Add(1)
			_, _ = fmt.Fprintf(w, `{"data":{"publish_id":"v_pub_url~operation-%d"},"error":{"code":"ok"}}`, count)
		case "/v2/post/publish/status/fetch/":
			mu.Lock()
			calls = append(calls, time.Now())
			mu.Unlock()
			_, _ = fmt.Fprint(w, `{"data":{"status":"PUBLISH_COMPLETE"},"error":{"code":"ok"}}`)
		default:
			t.Errorf("unexpected provider request: %s", r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	client := tiktok.New(provider.Config{ClientID: "fixture", ClientSecret: "fixture-secret", APIBase: server.URL, HTTPClient: server.Client(), Audited: true})
	content := model.Content{Text: "Creator-approved video", Media: []model.Media{{URL: "https://media.example/video.mp4", MIMEType: "video/mp4", SizeBytes: 1000, Width: 1080, Height: 1920, DurationSeconds: 10}}, Options: json.RawMessage(`{"privacy_level":"PUBLIC_TO_EVERYONE","consent":true,"preview_confirmed":true,"allow_comment":false,"allow_duet":false,"allow_stitch":false,"brand_content_toggle":false,"brand_organic_toggle":false}`)}
	posts := make([]model.Post, 0, 3)
	for i := 0; i < 3; i++ {
		post := nativePost(t, s, a, fmt.Sprintf("paced-%d", i), time.Now().Add(-time.Minute), content)
		claim, err := s.Claim(t.Context(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		out, err := client.Publish(t.Context(), token, content, model.Progress{}, func(progress model.Progress) error {
			return s.SaveProgress(t.Context(), claim.ID, claim.LeaseToken, progress)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.FinishTarget(t.Context(), claim.ID, claim.LeaseToken, store.Finish{Status: "processing", Progress: out.Progress, RetryAfter: time.Hour}); err != nil {
			t.Fatal(err)
		}
		posts = append(posts, post)
	}
	if _, err := sql.Exec(t.Context(), `UPDATE sp_targets SET next_attempt_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	other, err := store.New(t.Context(), dsn, 12)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	media, err := security.NewMediaFetcher([]string{"media.example"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.Registry{model.TikTok: client}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	var workers sync.WaitGroup
	for _, db := range []*store.Postgres{s, other} {
		e := &Engine{Store: db, Tokens: &account.Manager{Store: db, Vault: vault, Adapters: registry}, Adapters: registry, Media: media, Concurrency: 3, PollInterval: 10 * time.Millisecond, LeaseDuration: 3 * time.Second, JobTimeout: time.Minute}
		workers.Add(1)
		go func() { defer workers.Done(); e.Run(ctx) }()
	}
	defer func() { cancel(); workers.Wait() }()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		published := 0
		for _, post := range posts {
			got, err := s.GetPost(ctx, a.Owner, post.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Targets[0].Status == "published" {
				published++
			}
			if got.Targets[0].Failures != 0 {
				t.Fatal("account budget waits consumed delivery failures")
			}
			if strings.HasPrefix(got.Targets[0].Status, "failed") || got.Targets[0].Status == "needs_attention" {
				t.Fatalf("safe pacing stranded the existing operation: %+v", got.Targets[0])
			}
		}
		if published == len(posts) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("three paced targets did not complete:", ctx.Err())
		case <-ticker.C:
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("existing operations produced %d status requests, want3", len(calls))
	}
	for i := 1; i < len(calls); i++ {
		gap := calls[i].Sub(calls[i-1])
		if gap < 2100*time.Millisecond {
			t.Fatalf("two replicas sent account polls only %v apart", gap)
		}
		t.Logf("status request spacing across replicas: %v", gap)
	}
	if initializations.Load() != 3 {
		t.Fatalf("pacing reinitialized an existing operation: %d initializations", initializations.Load())
	}
}
