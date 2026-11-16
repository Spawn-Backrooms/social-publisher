package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/provider/instagram"
	"socialpublisher/internal/provider/tiktok"
	xprovider "socialpublisher/internal/provider/x"
	"socialpublisher/internal/store"
)

func TestPostgresMalformedRefreshNeverReusesOldGrant(t *testing.T) {
	database, dsn := refreshPostgresFixture(t)
	for index, tc := range []struct {
		name         string
		platform     model.Platform
		status       int
		body         string
		loseResponse bool
	}{
		{"x_empty_refresh", model.X, 200, `{"access_token":"new-access","refresh_token":"","token_type":"bearer","expires_in":3600}`, false},
		{"x_null_refresh", model.X, 200, `{"access_token":"new-access","refresh_token":null,"token_type":"bearer","expires_in":3600}`, false},
		{"x_mixed_error", model.X, 400, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"error":"server_error"}`, false},
		{"instagram_mixed_error", model.Instagram, 200, `{"data":[{"access_token":"new-access","expires_in":3600}],"error":{"code":4}}`, false},
		{"tiktok_mixed_429", model.TikTok, 429, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"error":"rate_limit_exceeded"}`, false},
		{"instagram_lost_then_429", model.Instagram, 429, `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/prime" {
					_, _ = w.Write([]byte("ready"))
					return
				}
				if requests.Add(1) == 1 && tc.loseResponse {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			transport := &http.Transport{}
			defer transport.CloseIdleConnections()
			hc := &http.Client{Transport: transport, Timeout: time.Second}
			if tc.loseResponse {
				response, err := hc.Get(server.URL + "/prime")
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
			config := provider.Config{ClientID: "fixture", ClientSecret: "fixture", APIBase: server.URL, HTTPClient: hc}
			var adapter provider.Adapter
			switch tc.platform {
			case model.X:
				adapter = xprovider.New(config)
			case model.Instagram:
				adapter = instagram.New(config)
			case model.TikTok:
				adapter = tiktok.New(config)
			}
			vault := refreshFixtureVault(t)
			a := model.Account{ID: tc.name, Owner: model.Principal{TenantID: "fixture", Subject: "owner"}, Platform: tc.platform,
				Identity: model.Identity{ID: "42"}, TokenExpiresAt: time.Now().Add(time.Hour), AuthorizationStartedAt: time.Now()}
			// Each platform/provider identity is unique in production. Give the
			// cases distinct IDs while retaining Instagram's numeric format.
			a.Identity.ID = strconv.Itoa(42 + index)
			// The fixture responses are rejected before identity validation.
			tok := model.Token{AccessToken: "old-access", RefreshToken: "old-refresh", ProviderUserID: a.Identity.ID,
				IssuedAt: time.Now().Add(-48 * time.Hour), ExpiresAt: a.TokenExpiresAt, RefreshExpiresAt: time.Now().Add(24 * time.Hour)}
			raw, err := json.Marshal(tok)
			if err != nil {
				t.Fatal(err)
			}
			a.EncryptedToken, err = vault.Seal(raw, a.TokenAAD())
			if err != nil {
				t.Fatal(err)
			}
			a, err = database.UpsertAccount(t.Context(), a)
			if err != nil {
				t.Fatal(err)
			}
			manager := &Manager{Store: database, Vault: vault, Adapters: provider.Registry{tc.platform: adapter}}
			guard, release, err := database.AcquireAccount(t.Context(), a.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = manager.Usable(guard, a, true)
			release()
			var pe *provider.Error
			if !errors.As(err, &pe) || !pe.AuthExpired {
				t.Fatalf("malformed provider outcome was not quarantined: %v", err)
			}
			// A fresh manager and connection pool see only durable state.
			restarted, err := store.New(t.Context(), dsn, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			manager = &Manager{Store: restarted, Vault: vault, Adapters: provider.Registry{tc.platform: adapter}}
			guard, release, err = restarted.AcquireAccount(t.Context(), a.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = manager.Usable(guard, a, true)
			release()
			if !errors.As(err, &pe) || !pe.AuthExpired || requests.Load() != 1 {
				t.Fatalf("old credential was reused after uncertain refresh: requests=%d err=%v", requests.Load(), err)
			}
			saved, err := restarted.GetWorkerAccount(t.Context(), a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Status != "reauth_required" || saved.TokenVersion != a.TokenVersion+1 || !bytes.Equal(saved.EncryptedToken, a.EncryptedToken) {
				t.Fatal("malformed provider response was persisted as a successful replacement")
			}
		})
	}
}
