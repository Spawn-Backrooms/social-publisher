package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"socialpublisher/internal/account"
	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
	"strings"
	"sync"
	"testing"
	"time"
)

type apiStore struct {
	store.Store
	owner                  model.Principal
	account                model.Account
	states                 map[string]model.OAuthState
	post                   *model.Post
	key, hash              string
	created                int
	exchanges              int
	admissionMu            sync.Mutex
	admissionSequence      int
	requestUsage           map[model.Principal]int
	admissions             map[string]fixtureAdmission
	requestAdmissionError  error
	providerAdmissionError error
}

func (s *apiStore) Ping(context.Context) error { return nil }
func (s *apiStore) PutOAuthState(_ context.Context, v model.OAuthState) error {
	v.IssuedAt = time.Now().UTC()
	s.states[v.StateHash] = v
	return nil
}
func (s *apiStore) ConsumeOAuthState(_ context.Context, hash, cookie string, p model.Platform) (model.OAuthState, error) {
	v, ok := s.states[hash]
	if !ok || v.CookieHash != cookie || v.Platform != p || v.ExpiresAt.Before(time.Now()) {
		return model.OAuthState{}, store.ErrOAuthState
	}
	delete(s.states, hash)
	return v, nil
}
func (s *apiStore) UpsertAccount(_ context.Context, a model.Account) (model.Account, error) {
	s.account = a
	return a, nil
}
func (s *apiStore) GetAccount(_ context.Context, p model.Principal, id string) (model.Account, error) {
	if p != s.account.Owner || id != s.account.ID {
		return model.Account{}, store.ErrNotFound
	}
	return s.account, nil
}
func (s *apiStore) GetWorkerAccount(context.Context, string) (model.Account, error) {
	return s.account, nil
}
func (s *apiStore) AcquireAccount(ctx context.Context, _ string) (context.Context, func(), error) {
	return ctx, func() {}, nil
}
func (s *apiStore) AcquireIdentity(ctx context.Context, _ model.Platform, _ string) (context.Context, func(), error) {
	return ctx, func() {}, nil
}
func (s *apiStore) LookupIdempotency(_ context.Context, p model.Principal, k, h string) (model.Post, error) {
	if s.post == nil || p != s.post.Owner || k != s.key {
		return model.Post{}, store.ErrNotFound
	}
	if h != s.hash {
		return model.Post{}, store.ErrIdempotency
	}
	return *s.post, nil
}
func (s *apiStore) CreatePost(_ context.Context, p model.Post, k, h string) (model.Post, bool, error) {
	s.created++
	s.post = &p
	s.key = k
	s.hash = h
	return p, true, nil
}

type apiAdapter struct {
	provider.Adapter
	store *apiStore
	caps  int
}

func (a *apiAdapter) Platform() model.Platform { return model.X }
func (a *apiAdapter) AuthorizationURL(state, challenge string) string {
	return "https://x.com/i/oauth2/authorize?state=" + url.QueryEscape(state) + "&code_challenge=" + url.QueryEscape(challenge)
}
func (a *apiAdapter) Exchange(context.Context, string, string) (model.Token, error) {
	a.store.exchanges++
	return model.Token{AccessToken: "super-secret-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (a *apiAdapter) Identity(context.Context, model.Token) (model.Identity, error) {
	return model.Identity{ID: "remote-user", Username: "user"}, nil
}
func (a *apiAdapter) Capabilities(context.Context, model.Token) (model.Capabilities, error) {
	a.caps++
	return model.Capabilities{CanPublish: true}, nil
}
func (a *apiAdapter) Validate(model.Content, model.Capabilities) error { return nil }
func fixtureAPI(t *testing.T) (*Server, *apiStore, *apiAdapter, string) {
	t.Helper()
	jwt, err := security.NewJWT(make([]byte, 32), "app", "publisher")
	if err != nil {
		t.Fatal(err)
	}
	vault, _ := security.NewVault(map[string][]byte{"one": make([]byte, 32)}, "one")
	media, _ := security.NewMediaFetcher([]string{"assets.example.com"}, 1<<30)
	p := model.Principal{TenantID: "tenant", Subject: "alice"}
	a := model.Account{ID: "acct-x", Owner: p, Platform: model.X, Identity: model.Identity{ID: "remote-user"}, Status: "active"}
	b, _ := json.Marshal(model.Token{AccessToken: "secret", ExpiresAt: time.Now().Add(time.Hour)})
	a.EncryptedToken, _ = vault.Seal(b, a.TokenAAD())
	s := &apiStore{owner: p, account: a, states: map[string]model.OAuthState{}}
	ad := &apiAdapter{store: s}
	reg := provider.Registry{model.X: ad}
	tokens := &account.Manager{Store: s, Vault: vault, Adapters: reg}
	srv := &Server{Store: s, Adapters: reg, JWT: jwt, Vault: vault, Tokens: tokens, Media: media, PublicBaseURL: "https://publisher.example.com", SecureCookies: true}
	token, _ := jwt.Mint(p, time.Hour, time.Now())
	return srv, s, ad, token
}
func req(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Idempotency-Key", "request-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestPrivateRoutesRequireSignedIdentity(t *testing.T) {
	s, _, _, _ := fixtureAPI(t)
	r := httptest.NewRequest("GET", "/v1/accounts", nil)
	r.Header.Set("X-User-ID", "alice")
	r.Header.Set("X-Tenant-ID", "tenant")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("spoofed headers accepted: %d", w.Code)
	}
}
func TestCreateRejectsUnknownJSONFields(t *testing.T) {
	s, st, _, token := fixtureAPI(t)
	w := req(s.Handler(), "POST", "/v1/posts", `{"text":"hi","targets":[{"account_id":"acct-x"}],"user_id":"victim"}`, token)
	if w.Code != 400 || st.created != 0 {
		t.Fatalf("unknown authority field accepted %d", w.Code)
	}
}
func TestCreateRejectsCrossOwnerAccount(t *testing.T) {
	s, st, _, token := fixtureAPI(t)
	st.account.Owner.Subject = "bob"
	w := req(s.Handler(), "POST", "/v1/posts", `{"text":"hi","targets":[{"account_id":"acct-x"}]}`, token)
	if w.Code != 404 || st.created != 0 {
		t.Fatalf("foreign account accepted %d", w.Code)
	}
}
func TestCreateIsIdempotentBeforeLiveValidation(t *testing.T) {
	s, st, ad, token := fixtureAPI(t)
	h := s.Handler()
	body := `{"text":"hello","targets":[{"account_id":"acct-x"}]}`
	w := req(h, "POST", "/v1/posts", body, token)
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body.String())
	}
	st.account.Status = "disconnected"
	w = req(h, "POST", "/v1/posts", body, token)
	if w.Code != 200 || st.created != 1 || ad.caps != 1 {
		t.Fatalf("idempotent retry was revalidated: %d %s", w.Code, w.Body.String())
	}
	w = req(h, "POST", "/v1/posts", `{"text":"changed","targets":[{"account_id":"acct-x"}]}`, token)
	if w.Code != 409 {
		t.Fatal("idempotency payload conflict ignored")
	}
}
func TestOAuthCallbackRequiresBrowserBindingAndSingleUse(t *testing.T) {
	s, st, _, token := fixtureAPI(t)
	h := s.Handler()
	start := req(h, "POST", "/v1/connections/x/authorize", `{}`, token)
	if start.Code != 200 {
		t.Fatalf("start %d %s", start.Code, start.Body.String())
	}
	var b struct {
		URL string `json:"authorization_url"`
	}
	json.Unmarshal(start.Body.Bytes(), &b)
	u, _ := url.Parse(b.URL)
	state := u.Query().Get("state")
	if state == "" || len(start.Result().Cookies()) != 1 {
		t.Fatal("missing flow binding")
	}
	// Issuance uses the database clock even if the app clock used for expiry
	// differs; disconnect fences must compare timestamps from the same clock.
	flow := st.states[security.Hash(state)]
	issuedAt := flow.IssuedAt
	flow.ExpiresAt = flow.ExpiresAt.Add(time.Hour)
	st.states[security.Hash(state)] = flow
	callback := "/oauth/x/callback?code=one-use-code&state=" + url.QueryEscape(state)
	w := req(h, "GET", callback, "", "")
	if w.Code != 400 || st.exchanges != 0 {
		t.Fatal("cookie-free account linking permitted")
	}
	r := httptest.NewRequest("GET", callback, nil)
	r.AddCookie(start.Result().Cookies()[0])
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || st.exchanges != 1 {
		t.Fatalf("bound callback failed: %d %s", w.Code, w.Body.String())
	}
	if !st.account.AuthorizationStartedAt.Equal(issuedAt) {
		t.Fatal("OAuth issuance was reconstructed from the app-clock expiry")
	}
	if strings.Contains(w.Body.String(), "super-secret-token") || strings.Contains(w.Body.String(), "one-use-code") {
		t.Fatal("callback leaked secrets")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || st.exchanges != 1 {
		t.Fatal("OAuth callback replay exchanged twice")
	}
}
func TestScheduleRequiresUnambiguousTimestamp(t *testing.T) {
	s, st, _, token := fixtureAPI(t)
	w := req(s.Handler(), "POST", "/v1/posts", `{"text":"hi","publish_at":"2026-11-01T01:30:00","targets":[{"account_id":"acct-x"}]}`, token)
	if w.Code != 400 || st.created != 0 {
		t.Fatal("ambiguous local schedule accepted")
	}
}
func TestMediaHostIsValidatedBeforeProvider(t *testing.T) {
	s, st, ad, token := fixtureAPI(t)
	w := req(s.Handler(), "POST", "/v1/posts", `{"text":"hi","media":[{"url":"http://169.254.169.254/latest/meta-data","mime_type":"image/jpeg","size_bytes":1}],"targets":[{"account_id":"acct-x"}]}`, token)
	if w.Code != 400 || st.created != 0 || ad.caps != 0 {
		t.Fatal("unsafe media reached platform")
	}
}
func TestMultipleAccountsOfSamePlatformAreRejected(t *testing.T) {
	s, st, _, token := fixtureAPI(t)
	w := req(s.Handler(), "POST", "/v1/posts", `{"text":"hi","targets":[{"account_id":"acct-x"},{"account_id":"acct-x"}]}`, token)
	if w.Code != 400 || st.created != 0 {
		t.Fatal("duplicate destinations accepted")
	}
}
