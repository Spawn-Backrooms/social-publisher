package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
	xprovider "socialpublisher/internal/provider/x"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
)

// This fixture kills a real disposable Go worker after the HTTP provider has
// rotated the grant, at the boundary immediately before token persistence.
// Recovery starts another process against the same database and provider.
func TestPostgresRefreshCrashRecovery(t *testing.T) {
	if os.Getenv("TEST_REFRESH_WORKER_MODE") != "" {
		refreshCrashFixtureWorker(t)
		return
	}
	s, dsn := refreshPostgresFixture(t)
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2/oauth2/token" || r.Method != http.MethodPost {
			t.Error("worker sent an unexpected provider request")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "fixture-old-refresh" {
			t.Error("worker sent an unexpected refresh credential")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if requests.Add(1) > 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"fixture-new-access","refresh_token":"fixture-new-refresh","token_type":"bearer","expires_in":3600}`))
	}))
	defer remote.Close()
	a := model.Account{ID: "refresh-crash-account", Owner: model.Principal{TenantID: "fixture", Subject: "worker"},
		Platform: model.X, Identity: model.Identity{ID: "fixture-provider-user"}, TokenExpiresAt: time.Now().Add(-time.Hour),
		AuthorizationStartedAt: time.Now().UTC()}
	tok := model.Token{AccessToken: "fixture-old-access", RefreshToken: "fixture-old-refresh", ProviderUserID: a.Identity.ID,
		IssuedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: a.TokenExpiresAt}
	raw, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	a.EncryptedToken, err = refreshFixtureVault(t).Seal(raw, a.TokenAAD())
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.UpsertAccount(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "provider-rotated-before-save")
	newWorker := func(mode string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPostgresRefreshCrashRecovery$", "-test.timeout=25s")
		cmd.Env = append(os.Environ(), "TEST_REFRESH_WORKER_MODE="+mode, "TEST_REFRESH_WORKER_DSN="+dsn,
			"TEST_REFRESH_WORKER_ENDPOINT="+remote.URL, "TEST_REFRESH_WORKER_MARKER="+marker)
		return cmd
	}
	first := newWorker("crash")
	var output bytes.Buffer
	first.Stdout, first.Stderr = &output, &output
	if err = first.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- first.Wait() }()
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = first.Process.Kill()
			<-exited
		}
	})
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	waiting := true
	for waiting {
		select {
		case err = <-exited:
			waited = true
			t.Fatalf("fixture exited before persistence boundary: %v\n%s", err, output.String())
		case <-deadline.C:
			t.Fatal("fixture never reached persistence boundary")
		case <-tick.C:
			if _, statErr := os.Stat(marker); statErr == nil {
				waiting = false
			}
		}
	}
	if requests.Load() != 1 {
		t.Fatal("provider did not record exactly one completed rotation before the kill")
	}
	if err = first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = <-exited
	waited = true
	if err == nil {
		t.Fatal("fixture worker was not terminated")
	}
	if os.Getenv("TEST_PGLITE") == "1" {
		// PGlite keeps one shared backend after socket/process death. Native
		// PostgreSQL releases these session locks automatically on disconnect.
		conn, connectErr := pgx.Connect(t.Context(), dsn)
		if connectErr != nil {
			t.Fatal(connectErr)
		}
		_, unlockErr := conn.Exec(t.Context(), "SELECT pg_advisory_unlock_all()")
		_ = conn.Close(t.Context())
		if unlockErr != nil {
			t.Fatal(unlockErr)
		}
	}
	if data, restartErr := newWorker("recover").CombinedOutput(); restartErr != nil {
		t.Fatalf("restarted fixture failed: %v\n%s", restartErr, data)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("restart resent the uncertain old refresh credential: provider requests=%d, want 1", got)
	}
	saved, err := s.GetWorkerAccount(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "reauth_required" || saved.TokenVersion <= a.TokenVersion {
		t.Fatal("restarted worker did not quarantine the uncertain old credential version")
	}
}

type crashBeforeTokenSave struct{ store.Store }

func (s crashBeforeTokenSave) SaveToken(ctx context.Context, _ string, _ int64, _ []byte, _ time.Time) error {
	if err := os.WriteFile(os.Getenv("TEST_REFRESH_WORKER_MARKER"), []byte("ready"), 0600); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func refreshCrashFixtureWorker(t *testing.T) {
	t.Helper()
	mode := os.Getenv("TEST_REFRESH_WORKER_MODE")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	s, err := store.New(ctx, os.Getenv("TEST_REFRESH_WORKER_DSN"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.GetWorkerAccount(ctx, "refresh-crash-account")
	if err != nil {
		t.Fatal(err)
	}
	guard, release, err := s.AcquireAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var database store.Store = s
	if mode == "crash" {
		database = crashBeforeTokenSave{Store: s}
	}
	m := &Manager{Store: database, Vault: refreshFixtureVault(t), Adapters: provider.Registry{model.X: xprovider.New(provider.Config{
		ClientID: "fixture-client", ClientSecret: "fixture-secret", APIBase: os.Getenv("TEST_REFRESH_WORKER_ENDPOINT"),
	})}}
	_, err = m.Usable(guard, a, true)
	if mode == "crash" {
		t.Fatal("worker was not killed at the persistence boundary")
	}
	var pe *provider.Error
	if !errors.As(err, &pe) || !pe.AuthExpired {
		t.Fatalf("restarted worker did not require reconnection: %v", err)
	}
}

func refreshFixtureVault(t *testing.T) *security.Vault {
	t.Helper()
	v, err := security.NewVault(map[string][]byte{"fixture": make([]byte, 32)}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func refreshPostgresFixture(t *testing.T) (*store.Postgres, string) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is unset; durable refresh crash recovery was not run")
	}
	schemaID, err := security.RandomToken(12)
	if err != nil {
		t.Fatal(err)
	}
	schema := "sp_refresh_" + strings.ToLower(strings.ReplaceAll(schemaID, "-", "_"))
	admin, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TEST_PGLITE") == "1" {
		if _, err = admin.Exec(t.Context(), "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	_ = admin.Close(t.Context())
	scopedDSN := dsn
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, parseErr := url.Parse(dsn)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		query := u.Query()
		query.Set("search_path", schema)
		u.RawQuery = query.Encode()
		scopedDSN = u.String()
	} else {
		scopedDSN += " search_path=" + schema
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, connectErr := pgx.Connect(ctx, dsn)
		if connectErr != nil {
			t.Errorf("connect cleanup: %v", connectErr)
			return
		}
		defer conn.Close(ctx)
		if _, dropErr := conn.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); dropErr != nil {
			t.Errorf("drop fixture schema: %v", dropErr)
		}
		if os.Getenv("TEST_PGLITE") == "1" {
			_, _ = conn.Exec(ctx, "SET search_path TO public")
		}
	})
	if err = store.Migrate(t.Context(), scopedDSN); err != nil {
		t.Fatal(err)
	}
	s, err := store.New(t.Context(), scopedDSN, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, scopedDSN
}
