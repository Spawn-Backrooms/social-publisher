package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/account"
	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/provider/instagram"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
)

func TestNativeInstagramQuotaCooldownGatesOtherTargetsAndKeepsDeadline(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		local      bool
		delay, age time.Duration
	}{
		{"provider_two_hours", false, 2 * time.Hour, time.Hour},
		{"provider_with_one_hour_left", false, 2 * time.Hour, 23 * time.Hour},
		{"locally_exhausted_quota", true, 15 * time.Minute, time.Hour},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s, dsn, sql, vault := nativeWorkerFixture(t)
			a, tokenA := nativeAccount(t, s, vault, "42", model.Instagram)
			b, tokenB := nativeAccount(t, s, vault, "43", model.Instagram)
			var running atomic.Bool
			var creates, requestsA, publishesA, publishesB atomic.Int32
			var containerMu sync.Mutex
			containers := make(map[string]string)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if running.Load() && r.Header.Get("Authorization") == "Bearer "+tokenA.AccessToken {
					requestsA.Add(1)
				}
				path := strings.TrimPrefix(r.URL.Path, "/v26.0/")
				parts := strings.Split(path, "/")
				switch {
				case len(parts) == 2 && parts[1] == "content_publishing_limit":
					if running.Load() && parts[0] == "42" {
						if scenario.local {
							_, _ = fmt.Fprint(w, `{"data":[{"quota_usage":7,"config":{"quota_total":7,"quota_duration":86400}}]}`)
						} else {
							w.Header().Set("Retry-After", "7200")
							w.WriteHeader(http.StatusTooManyRequests)
							_, _ = fmt.Fprint(w, `{"error":{"code":4,"error_subcode":2207042}}`)
						}
					} else {
						_, _ = fmt.Fprint(w, `{"data":[{"quota_usage":0,"config":{"quota_total":7,"quota_duration":86400}}]}`)
					}
				case len(parts) == 2 && parts[1] == "media":
					id := fmt.Sprint(100 + creates.Add(1))
					containerMu.Lock()
					containers[id] = parts[0]
					containerMu.Unlock()
					_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
				case len(parts) == 1:
					containerMu.Lock()
					_, exists := containers[path]
					containerMu.Unlock()
					if !exists {
						t.Errorf("unknown container: %s", path)
						http.NotFound(w, r)
						return
					}
					if running.Load() {
						_, _ = fmt.Fprint(w, `{"status_code":"FINISHED"}`)
					} else {
						_, _ = fmt.Fprint(w, `{"status_code":"IN_PROGRESS"}`)
					}
				case len(parts) == 2 && parts[1] == "media_publish":
					if parts[0] == "42" {
						publishesA.Add(1)
					} else {
						publishesB.Add(1)
					}
					_, _ = fmt.Fprint(w, `{"id":"901"}`)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := instagram.New(provider.Config{APIBase: server.URL, HTTPClient: server.Client()})
			content := instagramImageContent()
			var posts []model.Post
			existing := make(map[string]model.Progress)
			for i, account := range []model.Account{a, a, b} {
				post := nativePost(t, s, account, fmt.Sprintf("instagram-%d", i), time.Now().Add(-time.Minute), content)
				claim, err := s.Claim(t.Context(), time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				token := tokenA
				if account.ID == b.ID {
					token = tokenB
				}
				out, err := client.Publish(t.Context(), token, content, model.Progress{}, func(next model.Progress) error { return s.SaveProgress(t.Context(), claim.ID, claim.LeaseToken, next) })
				if err != nil || out.Done {
					t.Fatalf("prepare known container: %+v %v", out, err)
				}
				if err = s.FinishTarget(t.Context(), claim.ID, claim.LeaseToken, store.Finish{Status: "processing", Progress: out.Progress, RetryAfter: time.Hour, Warnings: []string{"existing delivery warning"}}); err != nil {
					t.Fatal(err)
				}
				saved, err := s.GetPost(t.Context(), account.Owner, post.ID)
				if err != nil {
					t.Fatal(err)
				}
				existing[post.ID] = saved.Targets[0].Progress
				posts = append(posts, post)
			}
			if _, err := sql.Exec(t.Context(), `UPDATE sp_targets SET
				next_attempt_at=clock_timestamp()-CASE WHEN id=$1 THEN interval '3 minutes' WHEN account_id=$2 THEN interval '2 minutes' ELSE interval '1 minute' END,
				started_at=clock_timestamp()-interval '25 hours',processing_started_at=clock_timestamp()-($3::bigint*interval '1 microsecond'),
				failures=CASE WHEN account_id=$2 THEN 4 ELSE 0 END`, posts[0].Targets[0].ID, a.ID, scenario.age.Microseconds()); err != nil {
				t.Fatal(err)
			}
			before, err := s.GetPost(t.Context(), a.Owner, posts[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			started := *before.Targets[0].ProcessingStartedAt
			running.Store(true)
			media, err := security.NewMediaFetcher([]string{"media.example"}, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			registry := provider.Registry{model.Instagram: client}
			engine := func(db *store.Postgres) *Engine {
				return &Engine{Store: db, Tokens: &account.Manager{Store: db, Vault: vault, Adapters: registry}, Adapters: registry, Media: media, LeaseDuration: time.Minute, JobTimeout: time.Minute}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			if err := engine(s).RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			waiting, err := s.GetPost(ctx, a.Owner, posts[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			first := waiting.Targets[0]
			if first.Status != "processing" || first.Failures != 4 || !reflect.DeepEqual(first.Progress, existing[posts[0].ID]) || first.ProcessingStartedAt == nil || !first.ProcessingStartedAt.Equal(started) || !reflect.DeepEqual(first.Warnings, []string{"existing delivery warning"}) {
				t.Fatalf("quota wait changed existing delivery state: %+v", first)
			}
			var next, dbNow time.Time
			if err := sql.QueryRow(ctx, `SELECT next_allowed_at,clock_timestamp() FROM sp_provider_pacing WHERE account_id=$1 AND scope='platform:instagram'`, a.ID).Scan(&next, &dbNow); err != nil {
				t.Fatalf("worker did not persist the shared quota cooldown: %v", err)
			}
			if wait := next.Sub(dbNow); wait < scenario.delay-5*time.Second || wait > scenario.delay+time.Second {
				t.Fatalf("provider-directed cooldown was shortened: got %v want %v", wait, scenario.delay)
			}
			if scenario.age == 23*time.Hour {
				if !first.NextAttemptAt.Equal(started.Add(24 * time.Hour)) {
					t.Fatal("quota wait extended the original 24-hour processing deadline")
				}
			} else if first.NextAttemptAt.Sub(dbNow) < scenario.delay-5*time.Second {
				t.Fatalf("ordinary polling cap truncated provider delay: next=%v db_now=%v", first.NextAttemptAt, dbNow)
			}
			other, err := store.New(ctx, dsn, 8)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			replica := engine(other)
			aCalls := requestsA.Load()
			if err := replica.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			published, err := other.GetPost(ctx, b.Owner, posts[2].ID)
			if err != nil || published.Targets[0].Status != "published" || publishesB.Load() != 1 {
				t.Fatalf("other account could not finish during A's quota cooldown: %+v %v", published, err)
			}
			if err := replica.RunOnce(ctx); !errors.Is(err, store.ErrNoWork) {
				t.Fatalf("another target bypassed the shared quota gate: %v", err)
			}
			sibling, err := other.GetPost(ctx, a.Owner, posts[1].ID)
			if err != nil {
				t.Fatal(err)
			}
			if sibling.Targets[0].Attempts != 1 || sibling.Targets[0].Failures != 4 || !reflect.DeepEqual(sibling.Targets[0].Progress, existing[posts[1].ID]) || requestsA.Load() != aCalls || creates.Load() != 3 || publishesA.Load() != 0 {
				t.Fatal("second replica reached A, recreated a container, or consumed delivery failures during quota wait")
			}
			// Keep the two-hour provider cooldown in place but advance only this
			// operation to its fixed deadline. Reconciliation must still be due.
			if _, err := sql.Exec(ctx, `UPDATE sp_targets SET processing_started_at=clock_timestamp()-interval '24 hours',next_attempt_at=clock_timestamp() WHERE id=$1`, first.ID); err != nil {
				t.Fatal(err)
			}
			if err := replica.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			expired, err := other.GetPost(ctx, a.Owner, posts[0].ID)
			if err != nil || expired.Targets[0].Status != "needs_attention" || expired.Targets[0].Failures != 4 || !reflect.DeepEqual(expired.Targets[0].Progress, existing[posts[0].ID]) || requestsA.Load() != aCalls {
				t.Fatalf("fixed processing deadline did not take precedence over quota waiting: %+v %v", expired, err)
			}
		})
	}
}
