package tiktok

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

const testSecret = "never-log-this-client-secret"
const testAccess = "never-log-this-access-token"
const testRefresh = "never-log-this-refresh-token"
const creatorResponse = `{"data":{"creator_username":"creator","creator_nickname":"Creator","privacy_level_options":["PUBLIC_TO_EVERYONE","MUTUAL_FOLLOW_FRIENDS","SELF_ONLY"],"comment_disabled":false,"duet_disabled":false,"stitch_disabled":false,"max_video_post_duration_sec":600},"error":{"code":"ok"}}`

func newFixture(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return New(provider.Config{
		ClientID: "client-id", ClientSecret: testSecret,
		RedirectURI: "https://publisher.example/oauth/callback/tiktok",
		APIBase:     server.URL, AuthBase: server.URL + "/authorize",
		HTTPClient: server.Client(), Audited: true,
	})
}

func jsonReply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func testToken() model.Token {
	return model.Token{
		AccessToken: testAccess, RefreshToken: testRefresh, TokenType: "Bearer",
		Scopes:         []string{"user.info.basic", "video.publish"},
		ProviderUserID: "creator-open-id", ExpiresAt: time.Now().Add(time.Hour),
		RefreshExpiresAt: time.Now().Add(24 * time.Hour),
	}
}

func approvedVideo() model.Content {
	return model.Content{
		Text:    "A creator-approved caption",
		Media:   []model.Media{{URL: "https://media.example/video.mp4?signature=private-url-marker", MIMEType: "video/mp4", SizeBytes: 1_000_000, DurationSeconds: 10, Width: 1080, Height: 1920}},
		Options: json.RawMessage(`{"privacy_level":"PUBLIC_TO_EVERYONE","consent":true,"preview_confirmed":true,"allow_comment":false,"allow_duet":false,"allow_stitch":false,"brand_content_toggle":false,"brand_organic_toggle":false}`),
	}
}

func approvedPhoto() model.Content {
	return model.Content{
		Text:    "Photo description",
		Media:   []model.Media{{URL: "https://media.example/photo.webp", MIMEType: "image/webp", SizeBytes: 1_000_000, Width: 1080, Height: 1080}},
		Options: json.RawMessage(`{"privacy_level":"PUBLIC_TO_EVERYONE","consent":true,"preview_confirmed":true,"allow_comment":false,"brand_content_toggle":false,"brand_organic_toggle":false,"title":"Photo title","photo_cover_index":0,"auto_add_music":true,"is_aigc":true}`),
	}
}

func testCapabilities() model.Capabilities {
	return model.Capabilities{
		CanPublish: true, DisplayName: "Creator", AccountType: "public",
		PrivacyLevels:           []string{"PUBLIC_TO_EVERYONE", "MUTUAL_FOLLOW_FRIENDS", "SELF_ONLY"},
		MaxVideoDurationSeconds: 600,
	}
}

func fixtureProgress(t *testing.T, publishID string) model.Progress {
	t.Helper()
	state, err := newPostingState(testToken(), approvedVideo())
	if err != nil {
		t.Fatal(err)
	}
	state.PublishID = publishID
	return savedProgress(state)
}

func changeOption(t *testing.T, content *model.Content, key string, value any) {
	t.Helper()
	var options map[string]any
	if err := json.Unmarshal(content.Options, &options); err != nil {
		t.Fatal(err)
	}
	if value == nil {
		delete(options, key)
	} else {
		options[key] = value
	}
	var err error
	content.Options, err = json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
}

func providerError(t *testing.T, err error) *provider.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var result *provider.Error
	if !errors.As(err, &result) {
		t.Fatalf("expected provider.Error, got %T: %v", err, err)
	}
	for _, secret := range []string{testSecret, testAccess, testRefresh, "private-url-marker", "sensitive-provider-body", "https://should-not-be-logged.example"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked sensitive upstream data: %q", err.Error())
		}
	}
	return result
}

func TestAuthorizationURLUsesWebContract(t *testing.T) {
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("unexpected API request") })
	u, err := url.Parse(client.AuthorizationURL("state+/value", "unsupported-web-pkce"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/authorize" || q.Get("client_key") != "client-id" || q.Get("response_type") != "code" || q.Get("state") != "state+/value" {
		t.Fatalf("incorrect authorization URL: %s", u.Redacted())
	}
	if q.Get("scope") != "user.info.basic,video.publish" || q.Get("disable_auto_auth") != "1" {
		t.Fatalf("incorrect requested permissions or consent behavior: %v", q)
	}
	if q.Get("code_challenge") != "" || q.Get("client_secret") != "" {
		t.Fatal("web authorization included unsupported or secret parameters")
	}
}

func TestOAuthExchangeAndRotatingRefreshPreserveActualGrant(t *testing.T) {
	var calls int
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v2/oauth/token/" {
			t.Fatalf("wrong token route: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatal("incorrect OAuth headers")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_key") != "client-id" || r.Form.Get("client_secret") != testSecret {
			t.Fatal("missing client authentication")
		}
		if calls == 1 {
			if r.Form.Get("code") != "code+/value" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code_verifier") != "" {
				t.Fatal("incorrect web code exchange")
			}
			if r.Form.Get("redirect_uri") != "https://publisher.example/oauth/callback/tiktok" {
				t.Fatal("redirect URI changed during exchange")
			}
			jsonReply(w, 200, `{"access_token":"access-one","expires_in":123,"open_id":"creator-open-id","refresh_token":"refresh-one","refresh_expires_in":456,"scope":"video.publish","token_type":"Bearer"}`)
		} else {
			if r.Form.Get("refresh_token") != "refresh-one" || r.Form.Get("grant_type") != "refresh_token" {
				t.Fatal("incorrect refresh request")
			}
			jsonReply(w, 200, `{"access_token":"access-two","expires_in":321,"open_id":"creator-open-id","refresh_token":"refresh-two","refresh_expires_in":654,"scope":"user.info.basic","token_type":"Bearer"}`)
		}
	})
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return now }
	token, err := client.Exchange(context.Background(), "code+/value", "unused-web-verifier")
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "access-one" || token.RefreshToken != "refresh-one" || !reflect.DeepEqual(token.Scopes, []string{"video.publish"}) {
		t.Fatalf("incorrect initial token: %+v", token)
	}
	if token.ExpiresAt != now.Add(123*time.Second) || token.RefreshExpiresAt != now.Add(456*time.Second) {
		t.Fatal("expiry did not follow provider response")
	}
	refreshed, err := client.Refresh(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "access-two" || refreshed.RefreshToken != "refresh-two" || !reflect.DeepEqual(refreshed.Scopes, []string{"user.info.basic"}) {
		t.Fatal("refresh invented or retained stale grants")
	}
	if refreshed.ExpiresAt != now.Add(321*time.Second) || refreshed.RefreshExpiresAt != now.Add(654*time.Second) {
		t.Fatal("refresh expiry did not follow response")
	}
}

func TestMalformedOAuthSuccessDoesNotInventExpiry(t *testing.T) {
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, 200, `{"access_token":"sensitive-provider-body","open_id":"creator-open-id","refresh_token":"refresh-one","refresh_expires_in":654,"scope":"video.publish","token_type":"Bearer"}`)
	})
	_, err := client.Exchange(context.Background(), "code", "")
	providerError(t, err)
}

func TestOAuthRejectionIsSanitized(t *testing.T) {
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, 400, `{"error":"invalid_grant","error_description":"sensitive-provider-body https://should-not-be-logged.example never-log-this-refresh-token"}`)
	})
	_, err := client.Refresh(context.Background(), testToken())
	if p := providerError(t, err); !p.AuthExpired || p.Retryable {
		t.Fatalf("incorrect invalid-grant classification: %+v", p)
	}
}

func TestIdentityAndRevoke(t *testing.T) {
	var revoked bool
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/user/info/":
			if r.Method != http.MethodGet || r.URL.Query().Get("fields") != "open_id,display_name" || r.Header.Get("Authorization") != "Bearer "+testAccess {
				t.Fatal("incorrect identity request")
			}
			jsonReply(w, 200, `{"data":{"user":{"open_id":"creator-open-id","display_name":"Creator"}},"error":{"code":"ok"}}`)
		case "/v2/oauth/revoke/":
			revoked = true
			if r.Method != http.MethodPost {
				t.Fatal("incorrect revoke method")
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("token") != testAccess || r.Form.Get("client_secret") != testSecret {
				t.Fatal("incorrect revoke fields")
			}
			jsonReply(w, 200, `{}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})
	identity, err := client.Identity(context.Background(), testToken())
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID != "creator-open-id" || identity.DisplayName != "Creator" || identity.Username != "" {
		t.Fatalf("incorrect identity: %+v", identity)
	}
	if err := client.Revoke(context.Background(), testToken()); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("revoke was not sent")
	}
}

func TestCreatorInfoHandlesHTTP200Errors(t *testing.T) {
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, 200, `{"error":{"code":"spam_risk_too_many_posts","message":"sensitive-provider-body"}}`)
	})
	caps, err := client.Capabilities(context.Background(), testToken())
	if caps.CanPublish {
		t.Fatal("HTTP 200 rejection was treated as publishable")
	}
	if p := providerError(t, err); p.Code != "spam_risk_too_many_posts" {
		t.Fatalf("wrong code: %+v", p)
	}
}

func TestValidateRequiresExplicitConsentAndCurrentSettings(t *testing.T) {
	client := New(provider.Config{Audited: true})
	if err := client.Validate(approvedVideo(), testCapabilities()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*model.Content, *model.Capabilities)
	}{
		{"missing privacy", func(c *model.Content, _ *model.Capabilities) { changeOption(t, c, "privacy_level", nil) }},
		{"no consent", func(c *model.Content, _ *model.Capabilities) { changeOption(t, c, "consent", false) }},
		{"no preview", func(c *model.Content, _ *model.Capabilities) { changeOption(t, c, "preview_confirmed", false) }},
		{"implicit comments", func(c *model.Content, _ *model.Capabilities) { changeOption(t, c, "allow_comment", nil) }},
		{"implicit disclosure", func(c *model.Content, _ *model.Capabilities) { changeOption(t, c, "brand_content_toggle", nil) }},
		{"disabled comments", func(c *model.Content, caps *model.Capabilities) {
			changeOption(t, c, "allow_comment", true)
			caps.CommentDisabled = true
		}},
		{"stale privacy", func(c *model.Content, caps *model.Capabilities) { caps.PrivacyLevels = []string{"SELF_ONLY"} }},
		{"branded private", func(c *model.Content, _ *model.Capabilities) {
			changeOption(t, c, "privacy_level", "SELF_ONLY")
			changeOption(t, c, "brand_content_toggle", true)
		}},
		{"duration changed", func(c *model.Content, caps *model.Capabilities) { caps.MaxVideoDurationSeconds = 5 }},
		{"insecure source", func(c *model.Content, _ *model.Capabilities) { c.Media[0].URL = "http://media.example/video.mp4" }},
		{"source credentials", func(c *model.Content, _ *model.Capabilities) {
			c.Media[0].URL = "https://never-log-this-access-token@media.example/video.mp4"
		}},
		{"unknown option", func(c *model.Content, _ *model.Capabilities) { changeOption(t, c, "schedule_time", 123) }},
		{"text only", func(c *model.Content, _ *model.Capabilities) { c.Media = nil }},
		{"unknown dimensions", func(c *model.Content, _ *model.Capabilities) { c.Media[0].Width = 0 }},
		{"multiple videos", func(c *model.Content, _ *model.Capabilities) { c.Media = append(c.Media, c.Media[0]) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content, caps := approvedVideo(), testCapabilities()
			tc.change(&content, &caps)
			providerError(t, client.Validate(content, caps))
		})
	}
}

func TestValidateCountsUTF16AndDisablesUnauditedPublicAccounts(t *testing.T) {
	client := New(provider.Config{Audited: true})
	content := approvedVideo()
	content.Text = strings.Repeat("😀", 1100)
	if err := client.Validate(content, testCapabilities()); err != nil {
		t.Fatal(err)
	}
	content.Text += "😀"
	providerError(t, client.Validate(content, testCapabilities()))
	content = approvedVideo()
	changeOption(t, &content, "privacy_level", "SELF_ONLY")
	client = New(provider.Config{Audited: false})
	providerError(t, client.Validate(content, testCapabilities()))
	caps := testCapabilities()
	caps.AccountType = "private"
	caps.PrivacyLevels = []string{"FOLLOWER_OF_CREATOR", "MUTUAL_FOLLOW_FRIENDS", "SELF_ONLY"}
	if err := client.Validate(content, caps); err != nil {
		t.Fatal(err)
	}
}

func TestVideoInitializationCheckpointAndResumedPolling(t *testing.T) {
	var saved model.Progress
	var initCalls, creatorCalls, statusCalls int
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/post/publish/creator_info/query/":
			creatorCalls++
			jsonReply(w, 200, creatorResponse)
		case "/v2/post/publish/video/init/":
			initCalls++
			if !saved.Unsafe {
				t.Fatal("initialization began before durable unsafe checkpoint")
			}
			var body struct {
				PostInfo   map[string]any `json:"post_info"`
				SourceInfo map[string]any `json:"source_info"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.SourceInfo["source"] != "PULL_FROM_URL" || body.PostInfo["disable_comment"] != true || body.PostInfo["brand_content_toggle"] != false {
				t.Fatalf("incorrect video payload: %+v", body.PostInfo)
			}
			if _, ok := body.SourceInfo["video_size"]; ok {
				t.Fatal("server-hosted content used file upload")
			}
			jsonReply(w, 200, `{"data":{"publish_id":"v_pub_url~operation-1"},"error":{"code":"ok"}}`)
		case "/v2/post/publish/status/fetch/":
			statusCalls++
			if statusCalls == 1 {
				jsonReply(w, 200, `{"data":{"status":"PROCESSING_DOWNLOAD"},"error":{"code":"ok"}}`)
			} else {
				jsonReply(w, 200, `{"data":{"status":"PUBLISH_COMPLETE","publicaly_available_post_id":[9223372036854775807]},"error":{"code":"ok"}}`)
			}
		default:
			t.Fatalf("unexpected route %s", r.URL.Path)
		}
	})
	checkpoint := func(p model.Progress) error { saved = p; return nil }
	result, err := client.Publish(context.Background(), testToken(), approvedVideo(), model.Progress{}, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if result.Done || saved.Unsafe || !strings.Contains(string(saved.Data), "v_pub_url~operation-1") || result.PollAfter <= 0 || statusCalls != 0 {
		t.Fatal("init did not durably hand off to polling")
	}
	result, err = client.Publish(context.Background(), testToken(), approvedVideo(), saved, checkpoint)
	if err != nil || result.Done {
		t.Fatalf("processing result: %+v, %v", result, err)
	}
	result, err = client.Publish(context.Background(), testToken(), approvedVideo(), saved, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Done || result.RemoteID != "9223372036854775807" || result.RemoteURL != "" {
		t.Fatalf("incorrect completed receipt: %+v", result)
	}
	if initCalls != 1 || creatorCalls != 1 {
		t.Fatal("resuming a known operation reinitialized or revalidated it")
	}
}

func TestPhotoRequestKeepsAIFlagAtTopLevel(t *testing.T) {
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/post/publish/creator_info/query/" {
			jsonReply(w, 200, creatorResponse)
			return
		}
		if r.URL.Path != "/v2/post/publish/content/init/" {
			t.Fatalf("wrong photo endpoint %s", r.URL.Path)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["is_aigc"]) != "true" || string(body["post_mode"]) != `"DIRECT_POST"` || string(body["media_type"]) != `"PHOTO"` {
			t.Fatal("incorrect photo top-level fields")
		}
		var info map[string]json.RawMessage
		if err := json.Unmarshal(body["post_info"], &info); err != nil {
			t.Fatal(err)
		}
		if _, ok := info["is_aigc"]; ok {
			t.Fatal("photo AI flag was nested like a video")
		}
		if _, ok := info["disable_duet"]; ok {
			t.Fatal("photo included video-only interaction")
		}
		jsonReply(w, 200, `{"data":{"publish_id":"p_pub_url~operation-1"},"error":{"code":"ok"}}`)
	})
	_, err := client.Publish(context.Background(), testToken(), approvedPhoto(), model.Progress{}, func(model.Progress) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestInitializationFailureClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		ambiguous bool
		retryable bool
		auth      bool
	}{
		{"rate limit", 429, `{"error":{"code":"rate_limit_exceeded","message":"sensitive-provider-body"}}`, false, true, false},
		{"expired token", 401, `{"error":{"code":"access_token_invalid","message":"sensitive-provider-body"}}`, false, false, true},
		{"invalid metadata", 400, `{"error":{"code":"invalid_param","message":"sensitive-provider-body"}}`, false, false, false},
		{"HTTP200 rejection", 200, `{"error":{"code":"spam_risk_too_many_posts","message":"sensitive-provider-body"}}`, false, true, false},
		{"server error", 503, `{"error":{"code":"internal_error","message":"sensitive-provider-body"}}`, true, false, false},
		{"missing operation ID", 200, `{"data":{},"error":{"code":"ok"}}`, true, false, false},
		{"malformed success", 200, `sensitive-provider-body`, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var checkpoints []model.Progress
			client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/post/publish/creator_info/query/" {
					jsonReply(w, 200, creatorResponse)
					return
				}
				w.Header().Set("Retry-After", "42")
				jsonReply(w, tc.status, tc.body)
			})
			_, err := client.Publish(context.Background(), testToken(), approvedVideo(), model.Progress{}, func(p model.Progress) error { checkpoints = append(checkpoints, p); return nil })
			p := providerError(t, err)
			if p.Ambiguous != tc.ambiguous || p.Retryable != tc.retryable || p.AuthExpired != tc.auth {
				t.Fatalf("incorrect classification: %+v", p)
			}
			if len(checkpoints) == 0 || !checkpoints[0].Unsafe {
				t.Fatal("missing initial durable checkpoint")
			}
			last := checkpoints[len(checkpoints)-1]
			if tc.ambiguous != last.Unsafe {
				t.Fatalf("checkpoint unsafe=%v, ambiguous=%v", last.Unsafe, tc.ambiguous)
			}
			if tc.retryable && tc.status == 429 && p.RetryAfter != 42*time.Second {
				t.Fatal("Retry-After was lost")
			}
		})
	}
}

func TestCheckpointFailureAndUnsafeResumeNeverSendInitialization(t *testing.T) {
	var initCalls atomic.Int32
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/post/publish/creator_info/query/" {
			jsonReply(w, 200, creatorResponse)
			return
		}
		initCalls.Add(1)
		t.Error("initialization must not be sent")
		jsonReply(w, 500, `{}`)
	})
	sentinel := errors.New("checkpoint failed")
	_, err := client.Publish(context.Background(), testToken(), approvedVideo(), model.Progress{}, func(model.Progress) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("checkpoint failure not preserved: %v", err)
	}
	_, err = client.Publish(context.Background(), testToken(), approvedVideo(), model.Progress{Unsafe: true}, func(model.Progress) error { return nil })
	if !providerError(t, err).Ambiguous {
		t.Fatal("unsafe resume was not marked uncertain")
	}
	if initCalls.Load() != 0 {
		t.Fatal("unsafe initialization was sent")
	}
}

func TestCancelledInitializationRetainsUnsafeCheckpoint(t *testing.T) {
	started := make(chan struct{})
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/post/publish/creator_info/query/" {
			jsonReply(w, 200, creatorResponse)
			return
		}
		// Consume the request before waiting: net/http cannot observe a peer
		// disconnect while this fixture leaves the request body unread.
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var saved model.Progress
	done := make(chan error, 1)
	go func() {
		_, err := client.Publish(ctx, testToken(), approvedVideo(), model.Progress{}, func(p model.Progress) error { saved = p; return nil })
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("init did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !providerError(t, err).Ambiguous || !saved.Unsafe || !errors.Is(err, context.Canceled) {
			t.Fatal("cancelled mutation lost ambiguity or cancellation cause")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not stop initialization")
	}
}

func TestAPIRedirectDoesNotForwardBearerToken(t *testing.T) {
	var forwarded atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer destination.Close()
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	})
	_, err := client.Capabilities(context.Background(), testToken())
	providerError(t, err)
	if forwarded.Load() != 0 {
		t.Fatal("redirect forwarded a user bearer token")
	}
}

func TestOptionsRejectDuplicateAndCaseFoldedConsent(t *testing.T) {
	client := New(provider.Config{Audited: true})
	for _, prefix := range []string{`"consent":false,`, `"Consent":false,`, `"PRIVACY_LEVEL":"SELF_ONLY",`} {
		content := approvedVideo()
		content.Options = append([]byte("{"+prefix), content.Options[1:]...)
		if err := client.Validate(content, testCapabilities()); err == nil {
			t.Errorf("accepted a competing or noncanonical option name: %s", prefix)
		}
	}
}

func TestPhotoLimitsAndVideoOnlyFields(t *testing.T) {
	client := New(provider.Config{Audited: true})
	cases := []struct {
		name   string
		change func(*model.Content)
	}{
		{"too many photos", func(c *model.Content) {
			for len(c.Media) < 36 {
				c.Media = append(c.Media, c.Media[0])
			}
		}},
		{"PNG unsupported", func(c *model.Content) { c.Media[0].MIMEType = "image/png" }},
		{"oversize", func(c *model.Content) { c.Media[0].SizeBytes = 20_000_001 }},
		{"cover out of range", func(c *model.Content) { changeOption(t, c, "photo_cover_index", 1) }},
		{"duet unavailable", func(c *model.Content) { changeOption(t, c, "allow_duet", false) }},
		{"title UTF16", func(c *model.Content) { changeOption(t, c, "title", strings.Repeat("😀", 46)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := approvedPhoto()
			tc.change(&content)
			providerError(t, client.Validate(content, testCapabilities()))
		})
	}
}

func TestAcceptedInitializationMustPersistItsIdentifier(t *testing.T) {
	var saved model.Progress
	var checkpoints int
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/post/publish/creator_info/query/" {
			jsonReply(w, 200, creatorResponse)
			return
		}
		jsonReply(w, 200, `{"data":{"publish_id":"v_pub_url~accepted"},"error":{"code":"ok"}}`)
	})
	_, err := client.Publish(context.Background(), testToken(), approvedVideo(), model.Progress{}, func(p model.Progress) error {
		checkpoints++
		if checkpoints == 2 {
			return errors.New("storage is unavailable")
		}
		saved = p
		return nil
	})
	if p := providerError(t, err); !p.Ambiguous || p.Retryable || !saved.Unsafe {
		t.Fatal("accepted but unsaved initialization became retryable")
	}
}

func TestPollingKeepsPrivateReceiptDistinctFromPublicIDs(t *testing.T) {
	for _, tc := range []struct {
		name, ids, want string
		wantWarning     bool
	}{
		{"no public ID", "[]", "v_pub_url~private", true},
		{"quoted public ID", `["9223372036854775808"]`, "9223372036854775808", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v2/post/publish/status/fetch/" {
					t.Fatal("polling unexpectedly initialized another post")
				}
				jsonReply(w, 200, `{"data":{"status":"PUBLISH_COMPLETE","publicaly_available_post_id":`+tc.ids+`},"error":{"code":"ok"}}`)
			})
			saved := fixtureProgress(t, "v_pub_url~private")
			outcome, err := client.Publish(context.Background(), testToken(), approvedVideo(), saved, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.Done || outcome.RemoteID != tc.want || outcome.RemoteURL != "" || (len(outcome.Warnings) > 0) != tc.wantWarning {
				t.Fatalf("incorrect receipt: %+v", outcome)
			}
		})
	}
}

func TestKnownOperationFailuresNeverInitializeAnotherPost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		retry  bool
	}{
		{"polling transport failure", 503, `sensitive-provider-body`, true},
		{"terminal media failure", 200, `{"data":{"status":"FAILED","fail_reason":"video_pull_failed"},"error":{"code":"ok"}}`, false},
		{"terminal unknown failure", 200, `{"data":{"status":"FAILED","fail_reason":"sensitive-provider-body"},"error":{"code":"ok"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v2/post/publish/status/fetch/" {
					t.Fatal("a known operation was reinitialized")
				}
				jsonReply(w, tc.status, tc.body)
			})
			progress := fixtureProgress(t, "v_pub_url~known")
			_, err := client.Publish(context.Background(), testToken(), approvedVideo(), progress, func(model.Progress) error { t.Fatal("polling reset saved progress"); return nil })
			if p := providerError(t, err); p.Ambiguous || p.Retryable != tc.retry {
				t.Fatalf("incorrect polling failure: %+v", p)
			}
		})
	}
}

func TestRevocationRequiresAnEmptyObjectOrEmptyBody(t *testing.T) {
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, "null") })
	if p := providerError(t, client.Revoke(context.Background(), testToken())); !p.Ambiguous {
		t.Fatal("an invalid revoke response was treated as confirmed")
	}
}

func TestResumeRejectsChangedAccountContentOrUnboundProgressBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/v2/post/publish/creator_info/query/":
			jsonReply(w, 200, creatorResponse)
		case "/v2/post/publish/video/init/":
			jsonReply(w, 200, `{"data":{"publish_id":"v_pub_url~bound"},"error":{"code":"ok"}}`)
		case "/v2/post/publish/status/fetch/":
			jsonReply(w, 200, `{"data":{"status":"PUBLISH_COMPLETE"},"error":{"code":"ok"}}`)
		default:
			t.Errorf("unexpected route: %s", r.URL.Path)
		}
	})
	var accepted model.Progress
	outcome, err := client.Publish(context.Background(), testToken(), approvedVideo(), model.Progress{}, func(p model.Progress) error {
		accepted = p
		return nil
	})
	if err != nil || outcome.Done {
		t.Fatalf("fresh initialization failed: %+v, %v", outcome, err)
	}
	cases := []struct {
		name   string
		change func(*model.Token, *model.Content, *model.Progress)
	}{
		{"another account", func(token *model.Token, _ *model.Content, _ *model.Progress) {
			token.ProviderUserID = "another-creator"
		}},
		{"missing account identity", func(token *model.Token, _ *model.Content, _ *model.Progress) { token.ProviderUserID = "" }},
		{"changed caption", func(_ *model.Token, content *model.Content, _ *model.Progress) {
			content.Text = "Another approved caption"
		}},
		{"changed media", func(_ *model.Token, content *model.Content, _ *model.Progress) {
			content.Media[0].URL = "https://media.example/other.mp4"
		}},
		{"changed choices", func(_ *model.Token, content *model.Content, _ *model.Progress) {
			changeOption(t, content, "privacy_level", "SELF_ONLY")
		}},
		{"unbound old checkpoint", func(_ *model.Token, _ *model.Content, progress *model.Progress) {
			progress.Data = json.RawMessage(`{"publish_id":"v_pub_url~bound"}`)
		}},
		{"unknown version", func(_ *model.Token, _ *model.Content, progress *model.Progress) {
			var saved map[string]any
			if err := json.Unmarshal(progress.Data, &saved); err != nil {
				t.Fatal(err)
			}
			saved["version"] = 999
			progress.Data, _ = json.Marshal(saved)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, content, progress := testToken(), approvedVideo(), accepted
			progress.Data = append(json.RawMessage(nil), accepted.Data...)
			tc.change(&token, &content, &progress)
			before := requests.Load()
			_, err := client.Publish(context.Background(), token, content, progress, nil)
			if err == nil {
				t.Fatal("resumed an operation with an invalid account/content binding")
			}
			providerError(t, err)
			if requests.Load() != before {
				t.Fatal("invalid progress reached TikTok before its binding was checked")
			}
		})
	}
}

func TestBoundProgressNormalizesJSONAndSurvivesTokenRotation(t *testing.T) {
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/post/publish/creator_info/query/":
			jsonReply(w, 200, creatorResponse)
		case "/v2/post/publish/video/init/":
			jsonReply(w, 200, `{"data":{"publish_id":"v_pub_url~canonical"},"error":{"code":"ok"}}`)
		case "/v2/post/publish/status/fetch/":
			jsonReply(w, 200, `{"data":{"status":"PUBLISH_COMPLETE"},"error":{"code":"ok"}}`)
		default:
			t.Errorf("unexpected route: %s", r.URL.Path)
		}
	})
	var checkpoints []model.Progress
	_, err := client.Publish(context.Background(), testToken(), approvedVideo(), model.Progress{}, func(p model.Progress) error {
		checkpoints = append(checkpoints, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 2 {
		t.Fatalf("expected intent and accepted checkpoints, got %d", len(checkpoints))
	}
	for _, progress := range checkpoints {
		var data struct {
			Version     int    `json:"version"`
			OwnerID     string `json:"owner_id"`
			ContentHash string `json:"content_hash"`
		}
		if err := json.Unmarshal(progress.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.Version != 1 || data.OwnerID != testToken().ProviderUserID || len(data.ContentHash) != 64 {
			t.Fatal("checkpoint did not retain its version/account/content binding")
		}
	}
	content := approvedVideo()
	var choices map[string]any
	if err := json.Unmarshal(content.Options, &choices); err != nil {
		t.Fatal(err)
	}
	content.Options, _ = json.MarshalIndent(choices, "", "  ")
	token := testToken()
	token.AccessToken = "rotated-token"
	outcome, err := client.Publish(context.Background(), token, content, checkpoints[1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Done || outcome.RemoteID != "v_pub_url~canonical" || len(outcome.Warnings) == 0 {
		t.Fatal("canonical content or token rotation changed private completion behavior")
	}
}
