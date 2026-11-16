package x

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

var fixtureScopes = []string{"tweet.read", "tweet.write", "users.read", "offline.access", "media.write"}

func fixtureToken() model.Token {
	return model.Token{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", TokenType: "bearer", Scopes: append([]string(nil), fixtureScopes...), ProviderUserID: "42"}
}

func fixtureClient(t *testing.T, handler http.HandlerFunc, media provider.MediaFetcher) (*Client, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c := New(provider.Config{ClientID: "fixture-client", ClientSecret: "fixture-secret", RedirectURI: "https://publisher.example/oauth/x/callback", APIBase: s.URL, AuthBase: s.URL + "/authorize", HTTPClient: s.Client(), Media: media})
	return c, s
}

type checkpoints struct {
	mu        sync.Mutex
	last      model.Progress
	all       []model.Progress
	failStage string
}

func (c *checkpoints) save(p model.Progress) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.Stage == c.failStage && c.failStage != "" {
		return errors.New("fixture checkpoint unavailable")
	}
	p.Data = append(json.RawMessage(nil), p.Data...)
	c.last = p
	c.all = append(c.all, p)
	return nil
}

func (c *checkpoints) current() model.Progress {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.last
	p.Data = append(json.RawMessage(nil), p.Data...)
	return p
}

func providerError(t *testing.T, err error) *provider.Error {
	t.Helper()
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("want provider.Error, got %T: %v", err, err)
	}
	return pe
}

func TestOAuthContractsAndRotatingRefreshToken(t *testing.T) {
	var tokenCalls atomic.Int32
	var revokedMu sync.Mutex
	var revoked []string
	c, s := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/2/oauth2/token":
			id, secret, ok := r.BasicAuth()
			if !ok || id != "fixture-client" || secret != "fixture-secret" {
				t.Errorf("incorrect confidential-client authentication")
			}
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Errorf("incorrect token request method/content type")
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			switch tokenCalls.Add(1) {
			case 1:
				if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "fixture-code" || r.Form.Get("code_verifier") != "fixture-verifier" || r.Form.Get("redirect_uri") != "https://publisher.example/oauth/x/callback" {
					t.Errorf("incorrect code exchange fields: %v", r.Form)
				}
				_, _ = io.WriteString(w, `{"access_token":"access-1","refresh_token":"refresh-1","token_type":"bearer","expires_in":7200,"scope":"tweet.read tweet.write users.read offline.access media.write"}`)
			case 2:
				if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-1" {
					t.Errorf("incorrect refresh fields")
				}
				_, _ = io.WriteString(w, `{"access_token":"access-2","refresh_token":"refresh-2","token_type":"bearer","expires_in":7200}`)
			case 3:
				if r.Form.Get("refresh_token") != "refresh-2" {
					t.Errorf("rotated refresh token was not retained")
				}
				_, _ = io.WriteString(w, `{"access_token":"access-3","token_type":"bearer","expires_in":7200}`)
			default:
				t.Errorf("unexpected token request")
			}
		case "/2/users/me":
			if r.Header.Get("Authorization") != "Bearer access-3" {
				t.Errorf("identity must use user bearer token")
			}
			_, _ = io.WriteString(w, `{"data":{"id":"42","name":"Account Name","username":"example","verified":true,"verified_type":"blue"}}`)
		case "/2/oauth2/revoke":
			id, secret, ok := r.BasicAuth()
			if !ok || id != "fixture-client" || secret != "fixture-secret" {
				t.Errorf("revoke must use confidential-client authentication")
			}
			_ = r.ParseForm()
			revokedMu.Lock()
			revoked = append(revoked, r.Form.Get("token"))
			revokedMu.Unlock()
			_, _ = io.WriteString(w, `{"revoked":true}`)
		default:
			t.Errorf("unexpected OAuth endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}, nil)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	u, err := url.Parse(c.AuthorizationURL("state-value", "challenge-value"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != s.URL+"/authorize" || u.Query().Get("state") != "state-value" || u.Query().Get("code_challenge") != "challenge-value" || u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("response_type") != "code" {
		t.Fatalf("incorrect authorization URL: %s", u.Redacted())
	}
	if !reflect.DeepEqual(strings.Fields(u.Query().Get("scope")), fixtureScopes) {
		t.Fatalf("scopes: %q", u.Query().Get("scope"))
	}
	token, err := c.Exchange(context.Background(), "fixture-code", "fixture-verifier")
	if err != nil {
		t.Fatal(err)
	}
	if !token.ExpiresAt.Equal(now.Add(2*time.Hour)) || !token.IssuedAt.Equal(now) {
		t.Fatalf("token expiry is not based on expires_in")
	}
	token.ProviderUserID = "42"
	token, err = c.Refresh(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	token, err = c.Refresh(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if token.RefreshToken != "refresh-2" || token.ProviderUserID != "42" || !reflect.DeepEqual(token.Scopes, fixtureScopes) {
		t.Fatalf("refresh did not retain identity/scopes/replacement token")
	}
	identity, err := c.Identity(context.Background(), token)
	if err != nil || identity.ID != "42" || identity.Username != "example" {
		t.Fatalf("identity: %+v, %v", identity, err)
	}
	caps, err := c.Capabilities(context.Background(), token)
	if err != nil || !caps.CanPublish || caps.MaxVideoDurationSeconds != 7500 {
		t.Fatalf("capabilities: %+v, %v", caps, err)
	}
	if err := c.Revoke(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	revokedMu.Lock()
	defer revokedMu.Unlock()
	if !reflect.DeepEqual(revoked, []string{"refresh-2", "access-3"}) {
		t.Fatalf("revoked tokens: %v", revoked)
	}
}

func TestOAuthRedirectIsNotFollowed(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer other.Close()
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}, nil)
	if _, err := c.Exchange(context.Background(), "code", "verifier"); err == nil {
		t.Fatal("redirect unexpectedly accepted")
	}
	if leaked.Load() != 0 {
		t.Fatal("credential-bearing redirect was followed")
	}
}

func TestOAuthMissingPermissionsAndRevokedRefreshAreExplicit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		refresh     bool
		wantCode    string
		wantExpired bool
	}{
		{"empty scope", 200, `{"access_token":"access","refresh_token":"refresh","expires_in":7200,"scope":""}`, false, "x_exchange_uncertain", false},
		{"missing media scope", 200, `{"access_token":"access","refresh_token":"refresh","expires_in":7200,"scope":"tweet.read tweet.write users.read offline.access"}`, false, "x_missing_scope", false},
		{"missing offline grant", 200, `{"access_token":"access","expires_in":7200}`, false, "x_refresh_token_missing", false},
		{"revoked refresh", 400, `{"error":"invalid_grant","error_description":"fixture-refresh"}`, true, "x_invalid_grant", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}, nil)
			var err error
			if tc.refresh {
				_, err = c.Refresh(context.Background(), fixtureToken())
			} else {
				_, err = c.Exchange(context.Background(), "code", "verifier")
			}
			pe := providerError(t, err)
			if pe.Code != tc.wantCode || pe.AuthExpired != tc.wantExpired || strings.Contains(err.Error(), "fixture-refresh") {
				t.Fatalf("OAuth rejection: %+v", pe)
			}
		})
	}
}

func TestValidateConservativeTextAndAttachmentCombinations(t *testing.T) {
	c := New(provider.Config{})
	caps := model.Capabilities{CanPublish: true, MaxVideoDurationSeconds: 1200}
	photo := model.Media{URL: "https://cdn.example/photo.jpg", MIMEType: "image/jpeg", SizeBytes: 1024}
	video := model.Media{URL: "https://cdn.example/video.mp4", MIMEType: "video/mp4", SizeBytes: 1024, DurationSeconds: 30}
	cases := []struct {
		name    string
		content model.Content
		wantErr bool
	}{
		{"ascii boundary", model.Content{Text: strings.Repeat("a", 280)}, false},
		{"ascii excess", model.Content{Text: strings.Repeat("a", 281)}, true},
		{"CJK boundary", model.Content{Text: strings.Repeat("字", 140)}, false},
		{"CJK excess", model.Content{Text: strings.Repeat("字", 141)}, true},
		{"short URL expansion", model.Content{Text: strings.Repeat("a", 260) + " https://a.co"}, true},
		{"empty", model.Content{}, true},
		{"media only", model.Content{Media: []model.Media{photo}}, false},
		{"four images", model.Content{Media: []model.Media{photo, photo, photo, photo}}, false},
		{"five images", model.Content{Media: []model.Media{photo, photo, photo, photo, photo}}, true},
		{"video", model.Content{Media: []model.Media{video}}, false},
		{"mixed", model.Content{Media: []model.Media{photo, video}}, true},
		{"unsupported options", model.Content{Text: "text", Options: json.RawMessage(`{"quote_tweet_id":"123"}`)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.Validate(tc.content, caps); (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
	video.DurationSeconds = 1201
	if err := c.Validate(model.Content{Media: []model.Media{video}}, caps); err == nil {
		t.Fatal("standard account duration limit was ignored")
	}
	caps.MaxVideoDurationSeconds = 7500
	if err := c.Validate(model.Content{Media: []model.Media{video}}, caps); err != nil {
		t.Fatalf("verified account duration was rejected: %v", err)
	}
}

func TestPublishTextCheckpointsBeforeRequestAndResumesKnownID(t *testing.T) {
	cp := new(checkpoints)
	var calls atomic.Int32
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/2/tweets" || r.Header.Get("Authorization") != "Bearer fixture-access" {
			t.Errorf("unexpected create request")
		}
		if p := cp.current(); !p.Unsafe {
			t.Error("public mutation was sent before its unsafe checkpoint")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || string(body["text"]) != `"hello"` || body["media"] != nil {
			t.Errorf("incorrect text payload: %+v, %v", body, err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"data":{"id":"123","text":"hello"}}`)
	}, nil)
	out, err := c.Publish(context.Background(), fixtureToken(), model.Content{Text: "hello"}, model.Progress{}, cp.save)
	if err != nil || !out.Done || out.RemoteID != "123" || out.Progress.Unsafe || out.RemoteURL != "https://x.com/i/status/123" {
		t.Fatalf("publish: %+v, %v", out, err)
	}
	out, err = c.Publish(context.Background(), fixtureToken(), model.Content{Text: "hello"}, out.Progress, cp.save)
	if err != nil || !out.Done || calls.Load() != 1 {
		t.Fatalf("known ID caused another publication: %+v, %v, calls %d", out, err, calls.Load())
	}
}

func TestAmbiguousCreateCannotBeRepeated(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"server error", 503, `{"title":"Service Unavailable"}`},
		{"missing ID", 201, `{"data":{"text":"hello"}}`},
		{"malformed response", 201, `{"data":`},
		{"request timeout", 408, `{"title":"Request Timeout"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			cp := new(checkpoints)
			c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}, nil)
			_, err := c.Publish(context.Background(), fixtureToken(), model.Content{Text: "hello"}, model.Progress{}, cp.save)
			if pe := providerError(t, err); !pe.Ambiguous || pe.Retryable || !cp.current().Unsafe {
				t.Fatalf("wrong uncertainty classification: %+v", pe)
			}
			_, err = c.Publish(context.Background(), fixtureToken(), model.Content{Text: "hello"}, cp.current(), cp.save)
			if !providerError(t, err).Ambiguous || calls.Load() != 1 {
				t.Fatalf("uncertain post was reissued: calls=%d, error=%v", calls.Load(), err)
			}
		})
	}
}

func TestDefiniteCreateRejectionClearsUnsafeState(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			cp := new(checkpoints)
			c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"title":"Forbidden","detail":"fixture-access fixture-secret https://cdn.example/file?signature=secret"}`)
			}, nil)
			_, err := c.Publish(context.Background(), fixtureToken(), model.Content{Text: "hello"}, model.Progress{}, cp.save)
			pe := providerError(t, err)
			if pe.Ambiguous || cp.current().Unsafe || pe.AuthExpired != (status == 401) || pe.Retryable != (status == 429) || pe.RateLimited != (status == 429) {
				t.Fatalf("wrong rejection classification: %+v; checkpoint=%+v", pe, cp.current())
			}
			if status == 429 && pe.RetryAfter != 7*time.Second {
				t.Fatalf("Retry-After ignored: %v", pe.RetryAfter)
			}
			if strings.Contains(err.Error(), "fixture-access") || strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "signature=") {
				t.Fatal("raw provider response leaked sensitive values")
			}
		})
	}
}

type fixtureMedia struct {
	data       []byte
	opens      int
	changeNext bool
}

func (f *fixtureMedia) Open(ctx context.Context, media model.Media) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	f.opens++
	data := f.data
	if f.changeNext {
		data = append([]byte(nil), data...)
		data[0] ^= 1
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func TestChunkedUploadResumesAcknowledgedBytesAndPollsDurably(t *testing.T) {
	media := &fixtureMedia{data: bytes.Repeat([]byte{0x4b}, 4*1024*1024+19)}
	content := model.Content{Media: []model.Media{{URL: "https://cdn.example/movie.mp4", MIMEType: "video/mp4", SizeBytes: int64(len(media.data)), DurationSeconds: 10}}}
	cp := new(checkpoints)
	var initCalls, appendCalls, finalizeCalls, statusCalls, createCalls atomic.Int32
	var failedSecond atomic.Bool
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-access" {
			t.Errorf("X API request lacks user context")
		}
		switch r.URL.Path {
		case "/2/media/upload/initialize":
			initCalls.Add(1)
			if r.Header.Get("Content-Type") != "application/json" {
				t.Error("INIT must be JSON")
			}
			var body struct {
				MediaType     string `json:"media_type"`
				MediaCategory string `json:"media_category"`
				TotalBytes    int64  `json:"total_bytes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.MediaType != "video/mp4" || body.MediaCategory != "tweet_video" || body.TotalBytes != int64(len(media.data)) {
				t.Errorf("bad INIT body: %+v %v", body, err)
			}
			_, _ = io.WriteString(w, `{"data":{"id":"900","media_key":"13_900","expires_after_secs":86400}}`)
		case "/2/media/upload/900/append":
			appendCalls.Add(1)
			if err := r.ParseMultipartForm(8 * 1024 * 1024); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			defer r.MultipartForm.RemoveAll()
			index, _ := strconv.Atoi(r.FormValue("segment_index"))
			f, _, err := r.FormFile("media")
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			chunk, _ := io.ReadAll(f)
			_ = f.Close()
			start := index * 4 * 1024 * 1024
			end := start + len(chunk)
			if len(chunk) > 4*1024*1024 || end > len(media.data) || !bytes.Equal(chunk, media.data[start:end]) {
				t.Errorf("wrong chunk payload at index %d", index)
			}
			if index == 1 && !failedSecond.Swap(true) {
				w.WriteHeader(503)
				return
			}
			_, _ = io.WriteString(w, `{"data":{}}`)
		case "/2/media/upload/900/finalize":
			finalizeCalls.Add(1)
			_, _ = io.WriteString(w, `{"data":{"id":"900","expires_after_secs":86400,"processing_info":{"state":"pending","check_after_secs":2}}}`)
		case "/2/media/upload":
			statusCalls.Add(1)
			if r.Method != http.MethodGet || r.URL.Query().Get("command") != "STATUS" || r.URL.Query().Get("media_id") != "900" {
				t.Error("wrong status query")
			}
			_, _ = io.WriteString(w, `{"data":{"id":"900","processing_info":{"state":"succeeded"}}}`)
		case "/2/tweets":
			createCalls.Add(1)
			if !cp.current().Unsafe {
				t.Error("media publication not checkpointed unsafe")
			}
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			if string(body["text"]) != `""` || string(body["media"]) != `{"media_ids":["900"]}` {
				t.Errorf("wrong media-only create body: %s %s", body["text"], body["media"])
			}
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"data":{"id":"1000","text":""}}`)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}, media)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	_, err := c.Publish(context.Background(), fixtureToken(), content, model.Progress{}, cp.save)
	if pe := providerError(t, err); !pe.Retryable || pe.Ambiguous || cp.current().Unsafe {
		t.Fatalf("pre-publication append failure: %+v", pe)
	}
	out, err := c.Publish(context.Background(), fixtureToken(), content, cp.current(), cp.save)
	if err != nil || out.Done || out.PollAfter != 2*time.Second {
		t.Fatalf("processing outcome: %+v, %v", out, err)
	}
	if media.opens != 2 || initCalls.Load() != 1 || appendCalls.Load() != 3 || finalizeCalls.Load() != 1 || createCalls.Load() != 0 {
		t.Fatalf("did not resume exactly the remaining chunk: opens=%d init=%d append=%d finalize=%d create=%d", media.opens, initCalls.Load(), appendCalls.Load(), finalizeCalls.Load(), createCalls.Load())
	}
	if _, err := c.Publish(context.Background(), fixtureToken(), content, cp.current(), cp.save); err != nil || statusCalls.Load() != 0 {
		t.Fatalf("check_after_secs not respected: %v, status=%d", err, statusCalls.Load())
	}
	now = now.Add(3 * time.Second)
	out, err = c.Publish(context.Background(), fixtureToken(), content, cp.current(), cp.save)
	if err != nil || !out.Done || out.RemoteID != "1000" || statusCalls.Load() != 1 || createCalls.Load() != 1 || media.opens != 2 {
		t.Fatalf("publish after polling: %+v, %v", out, err)
	}
}

func TestCheckpointFailurePreventsPublicRequest(t *testing.T) {
	var calls atomic.Int32
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, nil)
	_, err := c.Publish(context.Background(), fixtureToken(), model.Content{Text: "hello"}, model.Progress{}, func(p model.Progress) error {
		if p.Unsafe {
			return errors.New("lease lost")
		}
		return nil
	})
	if err == nil || calls.Load() != 0 {
		t.Fatalf("request escaped failed checkpoint: %v, calls=%d", err, calls.Load())
	}
}

func TestKnownPostIDSucceedsDespiteFinalCheckpointFailure(t *testing.T) {
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"data":{"id":"123","text":"hello"}}`)
	}, nil)
	out, err := c.Publish(context.Background(), fixtureToken(), model.Content{Text: "hello"}, model.Progress{}, func(p model.Progress) error {
		if p.Stage == "x.published" {
			return errors.New("receipt write unavailable")
		}
		return nil
	})
	if err != nil || !out.Done || out.RemoteID != "123" || len(out.Warnings) == 0 {
		t.Fatalf("known success was lost: %+v, %v", out, err)
	}
}
