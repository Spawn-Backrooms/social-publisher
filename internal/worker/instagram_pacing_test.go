package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/provider/instagram"
	"socialpublisher/internal/security"
)

type instagramCooldownRecord struct {
	account, scope string
	delay          time.Duration
}

type instagramPacingStore struct {
	*fixtureStore
	cooldowns []instagramCooldownRecord
}

func (s *instagramPacingStore) SetProviderCooldown(ctx context.Context, account, scope string, delay time.Duration) (time.Duration, error) {
	s.cooldowns = append(s.cooldowns, instagramCooldownRecord{account, scope, delay})
	return delay, ctx.Err()
}

func (s *instagramPacingStore) ReserveProviderRequest(ctx context.Context, account, scope string, _ time.Duration) (time.Duration, error) {
	for _, cooldown := range s.cooldowns {
		if account == cooldown.account && scope == cooldown.scope {
			return cooldown.delay, ctx.Err()
		}
	}
	return 0, ctx.Err()
}

func instagramImageContent() model.Content {
	return model.Content{Text: "A caption", Media: []model.Media{{URL: "https://media.example/image.jpg", MIMEType: "image/jpeg", SizeBytes: 50000, Width: 1080, Height: 1080}}}
}

func TestInstagramThrottleReachesWorkerCooldownWithoutConsumingFailure(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		delay time.Duration
	}{
		{"truncated_status", 42 * time.Second},
		{"oversized_status", 42 * time.Second},
		{"provider_quota", 2 * time.Hour},
		{"local_quota", 15 * time.Minute},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			e, base, _ := setupEngine(t)
			s := &instagramPacingStore{fixtureStore: base}
			base.account.Platform, base.account.Identity.ID = model.Instagram, "42"
			base.target.Platform, base.target.Content = model.Instagram, instagramImageContent()
			token := model.Token{AccessToken: "fixture-access", ProviderUserID: "42", ExpiresAt: e.now().Add(time.Hour)}
			encoded, _ := json.Marshal(token)
			var err error
			base.account.EncryptedToken, err = e.Tokens.Vault.Seal(encoded, base.account.TokenAAD())
			if err != nil {
				t.Fatal(err)
			}
			var prepared atomic.Bool
			var requests, creates, publishes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/v26.0/42/content_publishing_limit":
					if prepared.Load() && scenario.name == "provider_quota" {
						w.Header().Set("Retry-After", "7200")
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = fmt.Fprint(w, `{"error":{"code":4,"error_subcode":2207042}}`)
					} else if prepared.Load() && scenario.name == "local_quota" {
						_, _ = fmt.Fprint(w, `{"data":[{"quota_usage":7,"config":{"quota_total":7,"quota_duration":86400}}]}`)
					} else {
						_, _ = fmt.Fprint(w, `{"data":[{"quota_usage":0,"config":{"quota_total":7,"quota_duration":86400}}]}`)
					}
				case "/v26.0/42/media":
					creates.Add(1)
					_, _ = fmt.Fprint(w, `{"id":"101"}`)
				case "/v26.0/101":
					if !prepared.Load() {
						_, _ = fmt.Fprint(w, `{"status_code":"IN_PROGRESS"}`)
						return
					}
					if scenario.name == "truncated_status" || scenario.name == "oversized_status" {
						w.Header().Set("Retry-After", "42")
						if scenario.name == "truncated_status" {
							w.Header().Set("Content-Length", "256")
						}
						w.WriteHeader(http.StatusTooManyRequests)
						if scenario.name == "truncated_status" {
							_, _ = fmt.Fprint(w, "{}")
						} else {
							_, _ = fmt.Fprint(w, strings.Repeat("x", 2<<20))
						}
						return
					}
					_, _ = fmt.Fprint(w, `{"status_code":"FINISHED"}`)
				case "/v26.0/42/media_publish":
					publishes.Add(1)
					_, _ = fmt.Fprint(w, `{"id":"201"}`)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := instagram.New(provider.Config{APIBase: server.URL, HTTPClient: server.Client()})
			// Let the actual adapter create the checkpoint; the worker then resumes
			// that same operation rather than trusting a hand-built private state.
			out, err := client.Publish(t.Context(), token, base.target.Content, model.Progress{}, func(model.Progress) error { return nil })
			if err != nil || out.Done || out.Progress.Stage != "instagram:processing" {
				t.Fatalf("prepare existing operation: %+v %v", out, err)
			}
			base.target.Progress = out.Progress
			base.target.Failures = 4
			base.target.Warnings = []string{"existing delivery warning"}
			started := e.now().Add(-time.Hour)
			base.target.ProcessingStartedAt = &started
			prepared.Store(true)
			e.Adapters = provider.Registry{model.Instagram: client}
			e.Tokens.Adapters, e.Store, e.Tokens.Store = e.Adapters, s, s
			e.Media, err = security.NewMediaFetcher([]string{"media.example"}, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if err = e.RunOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			finish := base.finished
			if finish == nil || finish.Status != "processing" || finish.CountFailure || finish.RetryAfter != scenario.delay || !reflect.DeepEqual(finish.Progress, out.Progress) || !reflect.DeepEqual(finish.Warnings, base.target.Warnings) {
				t.Fatalf("throttle did not remain a safe full-duration wait: %+v", finish)
			}
			if len(s.cooldowns) != 1 || s.cooldowns[0] != (instagramCooldownRecord{base.account.ID, "platform:instagram", scenario.delay}) {
				t.Fatalf("worker did not persist the account cooldown: %+v", s.cooldowns)
			}
			before := requests.Load()
			base.claim = false
			if err = e.RunOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != before || len(s.cooldowns) != 1 || base.finished.CountFailure || creates.Load() != 1 || publishes.Load() != 0 || len(base.checkpoints) != 0 {
				t.Fatal("saved cooldown was bypassed, extended, or changed the existing operation")
			}
		})
	}
}
