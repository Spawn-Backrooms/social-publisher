package x

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

func TestSavedReceiptRequiresAccountContentAndVersionBinding(t *testing.T) {
	var calls atomic.Int32
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"data":{"id":"123"}}`)
	}, nil)
	cp := new(checkpoints)
	content := model.Content{Text: "Content A"}
	receipt, err := c.Publish(context.Background(), fixtureToken(), content, model.Progress{}, cp.save)
	if err != nil || !receipt.Done {
		t.Fatalf("could not capture fixture receipt: %+v %v", receipt, err)
	}
	for _, name := range []string{"content", "account", "version", "missing owner"} {
		t.Run(name, func(t *testing.T) {
			token, other, progress := fixtureToken(), content, receipt.Progress
			var state publishState
			_ = json.Unmarshal(progress.Data, &state)
			switch name {
			case "content":
				other.Text = "Content B"
			case "account":
				token.ProviderUserID = "43"
			case "version":
				state.Version = 99
			case "missing owner":
				state.OwnerID = ""
			}
			progress.Data, _ = json.Marshal(state)
			out, err := c.Publish(context.Background(), token, other, progress, cp.save)
			if err == nil || out.Done || calls.Load() != 1 {
				t.Fatalf("unbound receipt accepted or resent: %+v %v calls=%d", out, err, calls.Load())
			}
		})
	}
	expired := fixtureToken()
	expired.AccessToken = ""
	expired.ExpiresAt = time.Now().Add(-time.Hour)
	out, err := c.Publish(context.Background(), expired, content, receipt.Progress, nil)
	if err != nil || !out.Done || out.RemoteID != "123" || calls.Load() != 1 {
		t.Fatalf("bound receipt incorrectly needs fresh credentials: %+v %v", out, err)
	}
}

func TestAppendHTTP200ErrorsOrMalformedBodyDoNotAdvance(t *testing.T) {
	for _, body := range []string{
		`{"errors":[{"status":400,"detail":"fixture-access"}]}`,
		`{"data":{"expires_at":1792000000},"errors":[{"status":400,"detail":"fixture-access"}]}`,
		`{"data":`,
		`{}`,
		``,
	} {
		t.Run(body, func(t *testing.T) {
			media := &fixtureMedia{data: []byte("image")}
			content := model.Content{Media: []model.Media{{URL: "https://cdn.example/image.jpg", MIMEType: "image/jpeg", SizeBytes: 5}}}
			cp := new(checkpoints)
			var afterAppend atomic.Int32
			c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/2/media/upload/initialize":
					_, _ = io.WriteString(w, `{"data":{"id":"900","expires_after_secs":86400}}`)
				case "/2/media/upload/900/append":
					_, _ = io.WriteString(w, body)
				default:
					afterAppend.Add(1)
					if r.URL.Path == "/2/tweets" {
						w.WriteHeader(201)
						_, _ = io.WriteString(w, `{"data":{"id":"1000"}}`)
					} else {
						_, _ = io.WriteString(w, `{"data":{"id":"900"}}`)
					}
				}
			}, media)
			out, err := c.Publish(context.Background(), fixtureToken(), content, model.Progress{}, cp.save)
			var state publishState
			_ = json.Unmarshal(cp.current().Data, &state)
			if err == nil || out.Done || afterAppend.Load() != 0 || len(state.Media) != 1 || state.Media[0].UploadedBytes != 0 || state.Media[0].NextSegment != 0 || strings.Contains(err.Error(), "fixture-access") {
				t.Fatalf("bad append advanced delivery: %+v %v state=%+v later=%d", out, err, state, afterAppend.Load())
			}
		})
	}
}

func TestMetadataHTTP200WithErrorsDoesNotConfirmOrPublish(t *testing.T) {
	media := &fixtureMedia{data: []byte("image")}
	content := model.Content{Media: []model.Media{{URL: "https://cdn.example/image.jpg", MIMEType: "image/jpeg", SizeBytes: 5, AltText: "Image description"}}}
	cp := new(checkpoints)
	var creates atomic.Int32
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2/media/upload/initialize":
			_, _ = io.WriteString(w, `{"data":{"id":"900","expires_after_secs":86400}}`)
		case "/2/media/upload/900/append":
			_, _ = io.WriteString(w, `{"data":{}}`)
		case "/2/media/upload/900/finalize":
			_, _ = io.WriteString(w, `{"data":{"id":"900"}}`)
		case "/2/media/metadata":
			_, _ = io.WriteString(w, `{"data":{"id":"900","associated_metadata":{}},"errors":[{"status":400,"detail":"fixture-access"}]}`)
		case "/2/tweets":
			creates.Add(1)
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"data":{"id":"1000"}}`)
		}
	}, media)
	out, err := c.Publish(context.Background(), fixtureToken(), content, model.Progress{}, cp.save)
	var state publishState
	_ = json.Unmarshal(cp.current().Data, &state)
	if err == nil || out.Done || creates.Load() != 0 || len(state.Media) != 1 || state.Media[0].AltTextApplied || cp.current().Unsafe || strings.Contains(err.Error(), "fixture-access") {
		t.Fatalf("partial metadata response published: %+v %v state=%+v creates=%d", out, err, state, creates.Load())
	}
}

func TestConsumedOAuthGrantFailureIsAmbiguous(t *testing.T) {
	for _, method := range []string{"refresh", "exchange"} {
		for _, tc := range []struct {
			name   string
			status int
			body   string
		}{
			{"server failure", 503, `{"error":"temporarily_unavailable"}`},
			{"request timeout", 408, `{}`},
			{"missing token", 200, `{"expires_in":7200}`},
			{"incomplete JSON", 200, `{"access_token":`},
			{"closed connection", 0, ""},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				var grants atomic.Int32
				c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					grants.Add(1) // Simulates a consumed authorization/refresh grant.
					if tc.status == 0 {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				}, nil)
				var err error
				if method == "refresh" {
					_, err = c.Refresh(context.Background(), fixtureToken())
				} else {
					_, err = c.Exchange(context.Background(), "code", "verifier")
				}
				pe := providerError(t, err)
				if !pe.Ambiguous || pe.Retryable || grants.Load() != 1 {
					t.Fatalf("consumed grant can be retried: %+v requests=%d", pe, grants.Load())
				}
			})
		}
	}
}
