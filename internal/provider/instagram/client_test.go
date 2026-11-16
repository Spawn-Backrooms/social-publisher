package instagram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

func fixtureClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c := New(provider.Config{ClientID: "app-id", ClientSecret: "secret-sentinel", RedirectURI: "https://service.example/oauth/callback/instagram", APIBase: s.URL, AuthBase: s.URL, HTTPClient: s.Client()})
	c.now = func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) }
	return c, s
}

func TestAuthorizationAndExchange(t *testing.T) {
	var calls []string
	c, s := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/oauth/access_token":
			if r.Method != http.MethodPost {
				t.Errorf("method = %s", r.Method)
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{"client_id": "app-id", "client_secret": "secret-sentinel", "grant_type": "authorization_code", "code": "code-sentinel", "redirect_uri": "https://service.example/oauth/callback/instagram"} {
				if got := r.PostForm.Get(key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			fmt.Fprint(w, `{"data":[{"access_token":"short-sentinel","user_id":9007199254740993,"permissions":["instagram_business_basic","instagram_business_content_publish"]}]}`)
		case "/access_token":
			if r.Method != http.MethodGet || r.URL.Query().Get("grant_type") != "ig_exchange_token" || r.URL.Query().Get("access_token") != "short-sentinel" || r.URL.Query().Get("client_secret") != "secret-sentinel" {
				t.Errorf("invalid long-lived exchange")
			}
			fmt.Fprint(w, `{"access_token":"long-sentinel","token_type":"bearer","expires_in":5184000}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	u, err := url.Parse(c.AuthorizationURL("state-sentinel", "ignored-pkce"))
	if err != nil {
		t.Fatal(err)
	}
	if u.String() == "" || u.Scheme+"://"+u.Host != s.URL || u.Path != "/oauth/authorize" || u.Query().Get("state") != "state-sentinel" {
		t.Fatalf("invalid authorization URL %q", u)
	}
	if got := u.Query().Get("scope"); got != "instagram_business_basic,instagram_business_content_publish" {
		t.Fatalf("scope = %s", got)
	}
	if u.Query().Has("code_challenge") {
		t.Fatal("invented unsupported PKCE")
	}
	token, err := c.Exchange(context.Background(), "code-sentinel", "ignored-verifier")
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "long-sentinel" || token.ProviderUserID != "9007199254740993" || token.RefreshToken != "" {
		t.Fatalf("unexpected token identity or lifecycle: %+v", token)
	}
	if !token.IssuedAt.Equal(c.now()) || !token.ExpiresAt.Equal(c.now().Add(60*24*time.Hour)) {
		t.Fatal("incorrect token timestamps")
	}
	if !reflect.DeepEqual(calls, []string{"/oauth/access_token", "/access_token"}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestExchangeRejectsAmbiguousTokenRecords(t *testing.T) {
	for _, body := range []string{
		`{"data":[]}`,
		`{"data":[{"access_token":"one","user_id":"1"},{"access_token":"two","user_id":"2"}]}`,
		`{"access_token":"one","user_id":"1","data":[{"access_token":"two","user_id":"2"}]}`,
		`{"access_token":"one","access_token":"two","user_id":"1"}`,
		`{"access_token":"","user_id":"1"}`,
		`{"access_token":"one","user_id":1.5}`,
		`{"access_token":"one","user_id":0}`,
		`{"access_token":"one","user_id":"../other"}`,
		`{"error":{"code":190,"message":"secret-sentinel"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			calls := 0
			c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, body) })
			token, err := c.Exchange(context.Background(), "code-sentinel", "")
			if err == nil || token.AccessToken != "" || calls != 1 {
				t.Fatalf("token=%+v err=%v calls=%d", token, err, calls)
			}
			assertSanitized(t, err)
		})
	}
}

func TestRefreshAgeAndRotation(t *testing.T) {
	requests := 0
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/refresh_access_token" || r.Method != http.MethodGet || r.URL.Query().Get("grant_type") != "ig_refresh_token" || r.URL.Query().Get("access_token") != "old-sentinel" || r.URL.Query().Has("refresh_token") {
			t.Error("invalid refresh request")
		}
		fmt.Fprint(w, `{"access_token":"rotated-sentinel","expires_in":5184000,"token_type":"bearer"}`)
	})
	base := model.Token{AccessToken: "old-sentinel", IssuedAt: c.now().Add(-24 * time.Hour), ExpiresAt: c.now().Add(time.Hour), ProviderUserID: "9007199254740993", Scopes: []string{"instagram_business_basic", "instagram_business_content_publish"}}
	young := base
	young.IssuedAt = c.now().Add(-24*time.Hour + time.Nanosecond)
	got, err := c.Refresh(context.Background(), young)
	if err != nil || requests != 0 || got.AccessToken != young.AccessToken {
		t.Fatalf("young token: %+v %v calls=%d", got, err, requests)
	}
	for _, issued := range []time.Time{{}, c.now().Add(time.Minute)} {
		invalid := base
		invalid.IssuedAt = issued
		if _, err := c.Refresh(context.Background(), invalid); err == nil {
			t.Error("invalid issuance accepted")
		}
	}
	expired := base
	expired.ExpiresAt = c.now()
	_, err = c.Refresh(context.Background(), expired)
	var pe *provider.Error
	if !errors.As(err, &pe) || !pe.AuthExpired || requests != 0 {
		t.Fatalf("expiry err=%v calls=%d", err, requests)
	}
	got, err = c.Refresh(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || got.AccessToken != "rotated-sentinel" || got.ProviderUserID != base.ProviderUserID || !reflect.DeepEqual(got.Scopes, base.Scopes) || !got.IssuedAt.Equal(c.now()) {
		t.Fatalf("rotation failed: %+v", got)
	}
}

func TestMalformedRefreshDoesNotReturnCredential(t *testing.T) {
	for _, body := range []string{`{"access_token":"new"}`, `{"access_token":"new","expires_in":0}`, `{"access_token":"new","expires_in":9223372036854775807}`, `{"access_token":1,"expires_in":5184000}`} {
		t.Run(body, func(t *testing.T) {
			c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			got, err := c.Refresh(context.Background(), model.Token{AccessToken: "old", IssuedAt: c.now().Add(-48 * time.Hour), ExpiresAt: c.now().Add(time.Hour)})
			if err == nil || got.AccessToken != "" {
				t.Fatalf("invalid refresh accepted: %+v %v", got, err)
			}
		})
	}
}

func TestIdentityCapabilitiesAndRevoke(t *testing.T) {
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-sentinel" {
			t.Error("missing bearer authorization")
		}
		switch r.URL.Path {
		case "/v26.0/me":
			fmt.Fprint(w, `{"id":"10","user_id":9007199254740993,"username":"professional","name":"Professional","account_type":"BUSINESS","access_token":"secret-sentinel"}`)
		case "/v26.0/9007199254740993/content_publishing_limit":
			if r.URL.Query().Get("fields") != "quota_usage,config" {
				t.Error("missing quota fields")
			}
			fmt.Fprint(w, `{"data":[{"quota_usage":3,"config":{"quota_total":7,"quota_duration":86400},"access_token":"secret-sentinel"}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	token := model.Token{AccessToken: "access-sentinel", ProviderUserID: "9007199254740993"}
	id, err := c.Identity(context.Background(), token)
	if err != nil || id.ID != "9007199254740993" || id.AccountType != "BUSINESS" {
		t.Fatalf("identity = %+v %v", id, err)
	}
	caps, err := c.Capabilities(context.Background(), token)
	if err != nil || !caps.CanPublish {
		t.Fatalf("caps=%+v err=%v", caps, err)
	}
	if strings.Contains(string(caps.Raw), "sentinel") || !json.Valid(caps.Raw) {
		t.Fatalf("unsafe capabilities: %s", caps.Raw)
	}
	var raw struct {
		PublishingQuota struct {
			Total int64 `json:"total"`
		} `json:"publishing_quota"`
	}
	if err := json.Unmarshal(caps.Raw, &raw); err != nil || raw.PublishingQuota.Total != 7 {
		t.Fatalf("runtime quota not preserved: %s", caps.Raw)
	}
	var pe *provider.Error
	if err := c.Revoke(context.Background(), token); !errors.As(err, &pe) || pe.Code != "revoke_unsupported" {
		t.Fatalf("revoke err = %v", err)
	}
}

func TestErrorsAndRedirectsNeverExposeCredentials(t *testing.T) {
	c, s := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":190,"message":"secret-sentinel code-sentinel short-sentinel https://media.example/video.mp4?sig=signed-sentinel"}}`)
	})
	_, err := c.Exchange(context.Background(), "code-sentinel", "")
	assertSanitized(t, err)
	s.Close()
	_, err = c.Exchange(context.Background(), "code-sentinel", "")
	assertSanitized(t, err)
	hits := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; fmt.Fprint(w, `{}`) }))
	defer destination.Close()
	c, _ = fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"?access_token=secret-sentinel", http.StatusTemporaryRedirect)
	})
	_, err = c.Exchange(context.Background(), "code-sentinel", "")
	if err == nil || hits != 0 {
		t.Fatalf("redirect followed: hits=%d err=%v", hits, err)
	}
	assertSanitized(t, err)
}

func assertSanitized(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, secret := range []string{"secret-sentinel", "code-sentinel", "short-sentinel", "access-sentinel", "signed-sentinel"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %s", secret)
		}
	}
}
