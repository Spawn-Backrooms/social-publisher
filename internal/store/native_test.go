package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

type nativeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f nativeRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise the actual HTTP encoding/decoding against local handlers without
// DNS, sockets, provider calls or account credentials.
func nativeFixture(t *testing.T, handler http.HandlerFunc) *Native {
	t.Helper()
	s, err := NewNative(NativeStorageURL, strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	s.client.Transport = nativeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != NativeStorageURL || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Error("request escaped private storage protocol")
		}
		w := httptest.NewRecorder()
		handler(w, r)
		return w.Result(), nil
	})
	t.Cleanup(s.Close)
	return s
}
func nativeRequest(t *testing.T, r *http.Request) nativeEnvelope {
	t.Helper()
	var value nativeEnvelope
	if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	if value.Version != 1 || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(value.RequestID) || value.Session != strings.Repeat("a", 32) || value.IssuedAtMS <= 0 || value.Args == nil || value.Fences == nil {
		t.Fatal("invalid private request envelope")
	}
	return value
}
func nativeReply(w http.ResponseWriter, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func TestNativePrivateDTOsPreserveSuppressedFields(t *testing.T) {
	now := time.Date(2026, 10, 6, 7, 0, 0, 0, time.UTC)
	a := model.Account{ID: "a", Owner: model.Principal{TenantID: "project", Subject: "owner"}, Platform: model.X, Identity: model.Identity{ID: "provider"}, EncryptedToken: []byte("fixture-ciphertext"), TokenVersion: 7, TokenRefreshVersion: 7, TokenRefreshStartedAt: now, AuthorizationStartedAt: now, TokenExpiresAt: now}
	blob, err := json.Marshal(accountToNative(a))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(blob, []byte(`"encrypted_token":"`+base64.StdEncoding.EncodeToString(a.EncryptedToken)+`"`)) || !bytes.Contains(blob, []byte(`"owner":{"tenant_id":"project","sub":"owner"}`)) {
		t.Fatal("private account data omitted")
	}
	var decoded nativeAccount
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatal(err)
	}
	restored := decoded.model()
	if restored.Owner != a.Owner || restored.TokenVersion != 7 || restored.TokenRefreshVersion != 7 || !bytes.Equal(restored.EncryptedToken, a.EncryptedToken) || !restored.AuthorizationStartedAt.Equal(now) || !restored.TokenRefreshStartedAt.Equal(now) {
		t.Fatal("private account round trip lost state")
	}
	public, _ := json.Marshal(restored)
	for _, key := range []string{"encrypted_token", "owner", "token_version", "token_refresh_version", "authorization_started_at"} {
		if bytes.Contains(public, []byte(`"`+key+`"`)) {
			t.Fatalf("public model exposed %s", key)
		}
	}
	target := model.Target{ID: "target", Progress: model.Progress{Stage: "upload", Data: json.RawMessage(`{"remote":"fixture"}`), Unsafe: true}, LeaseToken: "lease", LeaseUntil: &now, ClaimedAt: now}
	target.Content = model.Content{Text: "thread", Reply: &model.XReply{PostID: "post_root", RemoteID: "1000", OwnerID: "42"}, PublishBefore: &now}
	p := model.Post{ID: "post", Owner: a.Owner, Targets: []model.Target{target}}
	blob, _ = json.Marshal(postToNative(p))
	var dp nativePost
	if err := json.Unmarshal(blob, &dp); err != nil {
		t.Fatal(err)
	}
	rp := dp.model()
	if rp.Targets[0].Content.Reply == nil || *rp.Targets[0].Content.Reply != *target.Content.Reply || rp.Targets[0].Content.PublishBefore == nil || !rp.Targets[0].Content.PublishBefore.Equal(now) {
		t.Fatal("native DTO lost resolved thread identity or publication window")
	}
	if rp.Owner != p.Owner || len(rp.Targets) != 1 || rp.Targets[0].LeaseToken != "lease" || !rp.Targets[0].Progress.Unsafe || rp.Targets[0].Progress.Stage != "upload" || !rp.Targets[0].ClaimedAt.Equal(now) || rp.Targets[0].LeaseUntil == nil {
		t.Fatal("private target round trip lost checkpoint or lease")
	}
	public, _ = json.Marshal(rp)
	if bytes.Contains(public, []byte(`"progress"`)) || bytes.Contains(public, []byte(`"lease_token"`)) || bytes.Contains(public, []byte(`"owner"`)) {
		t.Fatal("private bridge changed public encoding")
	}
	oauth := model.OAuthState{StateHash: "state", CookieHash: "cookie", Owner: a.Owner, Platform: model.X, EncryptedVerifier: []byte("encrypted-PKCE"), IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	blob, _ = json.Marshal(oauthToNative(oauth))
	var os nativeOAuthState
	if err := json.Unmarshal(blob, &os); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(blob, []byte(`"encrypted_verifier"`)) || !bytes.Equal(os.model().EncryptedVerifier, oauth.EncryptedVerifier) || os.model().Owner != oauth.Owner {
		t.Fatal("OAuth private DTO missing ciphertext or owner")
	}
}

func TestNativeClaimAndCreateWireResults(t *testing.T) {
	now := time.Date(2026, 10, 6, 7, 0, 0, 0, time.UTC)
	owner := model.Principal{TenantID: "project", Subject: "owner"}
	target := model.Target{ID: "t", PostID: "p", AccountID: "a", Platform: model.X, Progress: model.Progress{Stage: "processing", Unsafe: true}, LeaseToken: "worker-fence", LeaseUntil: &now, ClaimedAt: now}
	s := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		e := nativeRequest(t, r)
		switch e.Operation {
		case "Claim":
			if len(e.Args) != 1 || e.Args[0] != float64(45*time.Second) {
				t.Error("duration not encoded in nanoseconds")
			}
			nativeReply(w, targetToNative(target))
		case "CreatePost":
			request := e.Args[0].(map[string]any)
			if request["owner"].(map[string]any)["sub"] != "owner" || request["targets"].([]any)[0].(map[string]any)["progress"].(map[string]any)["unsafe"] != true || e.Args[1] != "idem" || e.Args[2] != "hash" {
				t.Error("post lost immutable input or idempotency")
			}
			nativeReply(w, map[string]any{"post": postToNative(model.Post{ID: "p", Owner: owner, Targets: []model.Target{target}}), "created": true})
		default:
			t.Errorf("unexpected operation %s", e.Operation)
		}
	})
	claimed, err := s.Claim(context.Background(), 45*time.Second)
	if err != nil || claimed.LeaseToken != target.LeaseToken || !claimed.Progress.Unsafe || !claimed.ClaimedAt.Equal(now) {
		t.Fatalf("claim lost private state: %v", err)
	}
	p, created, err := s.CreatePost(context.Background(), model.Post{ID: "p", Owner: owner, Targets: []model.Target{target}}, "idem", "hash")
	if err != nil || !created || p.Owner != owner || p.Targets[0].LeaseToken != target.LeaseToken {
		t.Fatalf("create result lost private state: %v", err)
	}
}

func TestNativeBusinessErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{{"not_found", ErrNotFound}, {"conflict", ErrConflict}, {"idempotency", ErrIdempotency}, {"lease_lost", ErrLeaseLost}, {"oauth_state", ErrOAuthState}, {"no_work", ErrNoWork}, {"account_busy", ErrAccountBusy}, {"admission_unavailable", ErrAdmissionUnavailable}} {
		t.Run(tc.code, func(t *testing.T) {
			s := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
				nativeRequest(t, r)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": tc.code, "reason": "sensitive-untrusted-value"})
			})
			if err := s.Ping(context.Background()); !errors.Is(err, tc.want) {
				t.Fatalf("error mapping: %v", err)
			}
		})
	}
	var admission *AdmissionError
	if !errors.As(nativeError("admission", "request_rate", time.Second), &admission) || admission.Reason != "request_rate" || admission.RetryAfter != time.Second {
		t.Fatal("admission retry semantics lost")
	}
	if strings.Contains(nativeError("admission", "private-ciphertext", -1).Error(), "private-ciphertext") || strings.Contains(nativeError("unknown", "private-ciphertext", 0).Error(), "private-ciphertext") {
		t.Fatal("error reason exposed private content")
	}
}

func TestNativeLostAcknowledgementNeverRetriesMutation(t *testing.T) {
	for _, mode := range []string{"transport", "malformed", "status", "oversize", "null_result"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			s := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				e := nativeRequest(t, r)
				if e.Operation != "SaveProgress" {
					t.Error("incorrect mutation")
				}
				switch mode {
				case "malformed":
					_, _ = io.WriteString(w, `{"ok":`)
				case "status":
					w.WriteHeader(503)
				case "oversize":
					_, _ = io.WriteString(w, strings.Repeat("x", nativeBodyLimit+1))
				case "null_result":
					nativeReply(w, nil)
				}
			})
			if mode == "transport" {
				s.client.Transport = nativeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					nativeRequest(t, r)
					return nil, errors.New("fixture lost reply")
				})
			}
			var err error
			if mode == "null_result" {
				var value model.Target
				err = s.call(context.Background(), "SaveProgress", &value, "target", "lease", model.Progress{Unsafe: true})
			} else {
				err = s.SaveProgress(context.Background(), "target", "lease", model.Progress{Unsafe: true})
			}
			if err == nil || calls.Load() != 1 || !strings.Contains(err.Error(), "uncertain") {
				t.Fatalf("uncertain mutation retried or acknowledged: %d %v", calls.Load(), err)
			}
		})
	}
}

func TestNativeGuardFencesProtectedCallsAndReleaseIsExact(t *testing.T) {
	var mu sync.Mutex
	var releaseCount int
	var seenFence bool
	var expectedToken string
	s := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		e := nativeRequest(t, r)
		switch e.Operation {
		case "AcquireIdentityGuard":
			expectedToken = e.Args[2].(string)
			if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(expectedToken) {
				t.Error("client guard token invalid")
			}
			nativeReply(w, nativeFence{Key: `identity:["x","provider"]`, Token: expectedToken})
		case "SaveToken":
			if len(e.Fences) != 1 || e.Fences[0].Key != `identity:["x","provider"]` || e.Fences[0].Token != expectedToken {
				t.Error("protected mutation lacks guard")
			}
			mu.Lock()
			seenFence = true
			mu.Unlock()
			nativeReply(w, nil)
		case "ReleaseIdentityGuard":
			if e.Args[0] != `identity:["x","provider"]` || e.Args[1] != expectedToken {
				t.Error("release not exact")
			}
			mu.Lock()
			releaseCount++
			mu.Unlock()
			nativeReply(w, nil)
		default:
			nativeReply(w, nil)
		}
	})
	guard, release, err := s.AcquireIdentity(context.Background(), model.X, "provider")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken(guard, "account", 1, []byte("ciphertext"), time.Now()); err != nil {
		t.Fatal(err)
	}
	release()
	release()
	mu.Lock()
	defer mu.Unlock()
	if !seenFence || releaseCount != 1 || guard.Err() == nil {
		t.Fatal("guard/release lifecycle incorrect")
	}
}

func TestNativeFailedProbeCancelsGuardWithoutTakingOver(t *testing.T) {
	var acquired, released atomic.Int32
	s := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		e := nativeRequest(t, r)
		switch e.Operation {
		case "AcquireIdentityGuard":
			acquired.Add(1)
			nativeReply(w, nativeFence{Key: `identity:["x","provider"]`, Token: e.Args[2].(string)})
		case "ProbeIdentityGuard":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "lease_lost"})
		case "ReleaseIdentityGuard":
			released.Add(1)
			w.WriteHeader(503)
		default:
			t.Errorf("unexpected operation %s", e.Operation)
		}
	})
	guard, release, err := s.AcquireIdentity(context.Background(), model.X, "provider")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-guard.Done():
	case <-time.After(4 * time.Second):
		t.Fatal("lost probe did not cancel protected context")
	}
	if !errors.Is(context.Cause(guard), errAccountLockLost) {
		t.Fatal("lost guard cause not preserved")
	}
	release()
	if acquired.Load() != 1 || released.Load() != 1 {
		t.Fatal("failed guard silently reacquired or repeated release")
	}
	if err := s.Ping(context.Background()); !errors.Is(err, errNativeQuarantined) {
		t.Fatal("failed exact release left writer healthy")
	}
}

func TestNativeTryAcquireDeadlineDoesNotEscapeToProtectedContext(t *testing.T) {
	s := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		e := nativeRequest(t, r)
		switch e.Operation {
		case "GetWorkerAccount":
			nativeReply(w, accountToNative(model.Account{ID: "a", Platform: model.X, Identity: model.Identity{ID: "provider"}}))
		case "AcquireIdentityGuard":
			nativeReply(w, nativeFence{Key: `identity:["x","provider"]`, Token: e.Args[2].(string)})
		default:
			nativeReply(w, nil)
		}
	})
	guard, release, err := s.TryAcquireAccount(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	select {
	case <-guard.Done():
		t.Fatal("admission cancellation cancelled live guard")
	case <-time.After(300 * time.Millisecond):
	}
	if err := s.Ping(guard); err != nil {
		t.Fatalf("successful guard inherited short admission deadline: %v", err)
	}
}

func TestNativeTryAcquireIsBoundedAndBlockingAcquireHonorsCancellation(t *testing.T) {
	s := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		nativeRequest(t, r)
		<-r.Context().Done()
		w.WriteHeader(503)
	})
	start := time.Now()
	if _, _, err := s.TryAcquireAccount(context.Background(), "a"); !errors.Is(err, ErrAccountBusy) || time.Since(start) > time.Second {
		t.Fatalf("try acquire not bounded: %v", err)
	}
	s = nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		nativeRequest(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "account_busy"})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := s.AcquireIdentity(ctx, model.X, "provider"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocking guard ignores cancellation: %v", err)
	}
}

func TestNativeRejectsExternalRoutingAndIncompleteSession(t *testing.T) {
	for _, url := range []string{"https://publisher-storage.internal/v1", "http://publisher-storage.internal/v1/", "http://internet.example/v1", "http://user:password@publisher-storage.internal/v1", "http://publisher-storage.internal/v1?x=1"} {
		if _, err := NewNative(url, strings.Repeat("a", 32)); err == nil {
			t.Fatalf("accepted native storage override %s", url)
		}
	}
	for _, session := range []string{"", "short", strings.Repeat("a", 201), strings.Repeat("a", 32) + "\n"} {
		if _, err := NewNative(NativeStorageURL, session); err == nil {
			t.Fatal("accepted invalid session")
		}
	}
	s, err := NewNative(NativeStorageURL, strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("private transport accepts ambient proxy")
	}
	if s.client.CheckRedirect(&http.Request{}, nil) == nil {
		t.Fatal("private transport follows redirects")
	}
}

func TestNativeIdentityKeyMatchesJavaScriptCanonicalEncoding(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"provider", `identity:["x","provider"]`},
		{"<>&", `identity:["x","<>&"]`},
		{"\"\\", `identity:["x","\"\\"]`},
		{"\x00\x01\b\f\n\r\t", `identity:["x","\u0000\u0001\b\f\n\r\t"]`},
		{"a\u2028b\u2029c", "identity:[\"x\",\"a\u2028b\u2029c\"]"},
		{"日本語", `identity:["x","日本語"]`},
	} {
		if got := nativeIdentityKey(model.X, tc.id); got != tc.want {
			t.Fatalf("canonical identity mismatch: %q != %q", got, tc.want)
		}
	}
}

func TestNativeLostAcquireAckCleansOnlyClientTokenBeforeReacquisition(t *testing.T) {
	var mu sync.Mutex
	held := map[string]string{}
	var acquisition, cleanup int
	var drop atomic.Bool
	var initialToken string
	s := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		e := nativeRequest(t, r)
		mu.Lock()
		defer mu.Unlock()
		switch e.Operation {
		case "AcquireIdentityGuard":
			key := nativeIdentityKey(model.Platform(e.Args[0].(string)), e.Args[1].(string))
			token := e.Args[2].(string)
			if held[key] != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "account_busy"})
				return
			}
			held[key] = token
			acquisition++
			if acquisition == 1 {
				initialToken = token
				drop.Store(true)
			} else if token == initialToken {
				t.Error("new acquisition reused old token")
			}
			nativeReply(w, nativeFence{Key: key, Token: token})
		case "ReleaseIdentityGuard":
			key, token := e.Args[0].(string), e.Args[1].(string)
			if held[key] != token || token == "" {
				t.Error("uncertain cleanup does not match exact client guard")
			}
			delete(held, key)
			cleanup++
			nativeReply(w, nil)
		default:
			nativeReply(w, nil)
		}
	})
	actual := s.client.Transport
	s.client.Transport = nativeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := actual.RoundTrip(r)
		if drop.Swap(false) {
			if response != nil {
				_ = response.Body.Close()
			}
			return nil, errors.New("fixture acquisition acknowledgement lost")
		}
		return response, err
	})
	if _, _, err := s.AcquireIdentity(context.Background(), model.X, "provider"); err == nil {
		t.Fatal("lost acquisition was acknowledged")
	}
	mu.Lock()
	if acquisition != 1 || cleanup != 1 || len(held) != 0 {
		t.Fatal("uncertain acquisition automatically retried or orphaned")
	}
	mu.Unlock()
	guard, release, err := s.AcquireIdentity(context.Background(), model.X, "provider")
	if err != nil || guard.Err() != nil {
		t.Fatalf("exact cleanup did not permit fresh acquisition: %v", err)
	}
	release()
	mu.Lock()
	defer mu.Unlock()
	if acquisition != 2 || cleanup != 2 || len(held) != 0 {
		t.Fatal("fresh guard lifecycle incorrect")
	}
}

func TestNativeLostAcquireAndCleanupQuarantinesWriterAndExistingGuards(t *testing.T) {
	var drop atomic.Bool
	var acquired atomic.Int32
	s := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		e := nativeRequest(t, r)
		switch e.Operation {
		case "AcquireIdentityGuard":
			acquired.Add(1)
			if e.Args[1] == "lost-provider" {
				drop.Store(true)
			}
			nativeReply(w, nativeFence{Key: nativeIdentityKey(model.Platform(e.Args[0].(string)), e.Args[1].(string)), Token: e.Args[2].(string)})
		case "ReleaseIdentityGuard":
			w.WriteHeader(503)
		default:
			nativeReply(w, nil)
		}
	})
	actual := s.client.Transport
	s.client.Transport = nativeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := actual.RoundTrip(r)
		if drop.Swap(false) {
			if response != nil {
				_ = response.Body.Close()
			}
			return nil, errors.New("fixture acquisition acknowledgement lost")
		}
		return response, err
	})
	guard, release, err := s.AcquireIdentity(context.Background(), model.X, "healthy-provider")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, _, err := s.AcquireIdentity(context.Background(), model.X, "lost-provider"); err == nil {
		t.Fatal("ambiguous acquisition accepted")
	}
	select {
	case <-guard.Done():
	case <-time.After(time.Second):
		t.Fatal("quarantine did not cancel prior protected work")
	}
	if !errors.Is(context.Cause(guard), errNativeQuarantined) {
		t.Fatal("quarantine cause lost")
	}
	if err := s.Ping(context.Background()); !errors.Is(err, errNativeQuarantined) {
		t.Fatalf("ambiguous writer reported healthy: %v", err)
	}
	if _, _, err := s.AcquireIdentity(context.Background(), model.X, "new-provider"); !errors.Is(err, errNativeQuarantined) {
		t.Fatalf("quarantined writer could acquire provider authority: %v", err)
	}
	if acquired.Load() != 2 {
		t.Fatal("quarantine silently reacquired a guard")
	}
}

func TestNativeLongRetentionUsesExactDecimalNanoseconds(t *testing.T) {
	const retention = time.Duration(3650) * 24 * time.Hour
	s := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		e := nativeRequest(t, r)
		if e.Operation != "RetainOperationalRecords" {
			t.Fatal("wrong operation")
		}
		policy := e.Args[0].(map[string]any)
		if policy["EventRetention"] != "315360000000000000" || policy["BatchSize"] != float64(500) {
			t.Fatal("long duration was rounded by JavaScript wire encoding")
		}
		nativeReply(w, RetentionResult{At: time.Now().UTC(), EventSweepComplete: true})
	})
	result, err := s.RetainOperationalRecords(context.Background(), RetentionPolicy{EventRetention: retention, BatchSize: 500})
	if err != nil || result.At.IsZero() || !result.EventSweepComplete {
		t.Fatalf("retention result invalid: %v", err)
	}
}
