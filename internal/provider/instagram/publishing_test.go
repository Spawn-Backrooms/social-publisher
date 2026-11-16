package instagram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

func imageContent() model.Content {
	return model.Content{Text: "A caption", Media: []model.Media{{URL: "https://media.example/image.jpg?sig=signed-sentinel", MIMEType: "image/jpeg", SizeBytes: 50000, Width: 1080, Height: 1080, AltText: "A useful description"}}}
}

func reelContent() model.Content {
	return model.Content{Text: "A video caption", Media: []model.Media{{URL: "https://media.example/video.mp4?sig=signed-sentinel", MIMEType: "video/mp4", SizeBytes: 1000000, DurationSeconds: 10, Width: 1080, Height: 1920}}, Options: json.RawMessage(`{"share_to_feed":false}`)}
}

func publishToken() model.Token {
	return model.Token{AccessToken: "access-sentinel", ProviderUserID: "42"}
}

func quotaResponse(w http.ResponseWriter, usage, total int) {
	fmt.Fprintf(w, `{"data":[{"quota_usage":%d,"config":{"quota_total":%d,"quota_duration":86400}}]}`, usage, total)
}

func TestPublishPersistsBeforeEveryDependentStep(t *testing.T) {
	var persisted model.Progress
	var creates, publishes int
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/42/content_publishing_limit":
			quotaResponse(w, 0, 7)
		case "/v26.0/42/media":
			creates++
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("image_url") != imageContent().Media[0].URL || r.Form.Get("alt_text") != "A useful description" || r.Form.Has("media_type") {
				t.Errorf("invalid image payload: %+v", r.Form)
			}
			fmt.Fprint(w, `{"id":"101"}`)
		case "/v26.0/101":
			var state publishState
			_ = json.Unmarshal(persisted.Data, &state)
			if state.ContainerID != "101" || persisted.Unsafe {
				t.Error("container was not durably recorded before polling")
			}
			fmt.Fprint(w, `{"status_code":"FINISHED"}`)
		case "/v26.0/42/media_publish":
			publishes++
			if !persisted.Unsafe || persisted.Stage != stagePublishing {
				t.Error("publish sent without durable unsafe checkpoint")
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.PostForm.Get("creation_id") != "101" {
				t.Error("wrong container published")
			}
			fmt.Fprint(w, `{"id":9007199254740993,"config_issue":"CAPTION_NOT_ATTACHED"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	checkpoint := func(p model.Progress) error { persisted = p; return nil }
	out, err := c.Publish(context.Background(), publishToken(), imageContent(), model.Progress{}, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Done || out.RemoteID != "9007199254740993" || len(out.Warnings) != 1 || persisted.Unsafe || persisted.Stage != stagePublished {
		t.Fatalf("invalid outcome: %+v", out)
	}
	if creates != 1 || publishes != 1 {
		t.Fatalf("mutations: creates=%d publishes=%d", creates, publishes)
	}
	out, err = c.Publish(context.Background(), publishToken(), imageContent(), persisted, checkpoint)
	if err != nil || !out.Done || creates != 1 || publishes != 1 {
		t.Fatalf("receipt resume repeated mutation: %+v %v", out, err)
	}
}

func TestResumeProcessingDoesNotRecreateReel(t *testing.T) {
	var ready bool
	var creates, publishes int
	var createForm url.Values
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/42/content_publishing_limit":
			quotaResponse(w, 0, 50)
		case "/v26.0/42/media":
			creates++
			_ = r.ParseForm()
			createForm = r.PostForm
			fmt.Fprint(w, `{"id":"101"}`)
		case "/v26.0/101":
			if ready {
				fmt.Fprint(w, `{"status_code":"FINISHED"}`)
			} else {
				fmt.Fprint(w, `{"status_code":"IN_PROGRESS"}`)
			}
		case "/v26.0/42/media_publish":
			publishes++
			fmt.Fprint(w, `{"id":"201"}`)
		default:
			http.NotFound(w, r)
		}
	})
	var persisted model.Progress
	checkpoint := func(p model.Progress) error { persisted = p; return nil }
	out, err := c.Publish(context.Background(), publishToken(), reelContent(), model.Progress{}, checkpoint)
	if err != nil || out.Done || out.PollAfter != time.Minute || publishes != 0 {
		t.Fatalf("processing: %+v %v", out, err)
	}
	if createForm.Get("media_type") != "REELS" || createForm.Get("share_to_feed") != "false" || createForm.Get("video_url") != reelContent().Media[0].URL {
		t.Fatal("invalid reel payload")
	}
	ready = true
	out, err = c.Publish(context.Background(), publishToken(), reelContent(), persisted, checkpoint)
	if err != nil || !out.Done || out.RemoteID != "201" || creates != 1 || publishes != 1 {
		t.Fatalf("resumption: %+v %v creates=%d publishes=%d", out, err, creates, publishes)
	}
}

func TestCarouselResumesChildrenAndWaitsBeforeParent(t *testing.T) {
	content := imageContent()
	content.Media = append(content.Media, reelContent().Media[0])
	var imageCreates, videoCreates, parentCreates, publishes int
	var videoReady bool
	var persisted model.Progress
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/42/content_publishing_limit":
			quotaResponse(w, 0, 7)
		case "/v26.0/42/media":
			_ = r.ParseForm()
			if r.Form.Get("is_carousel_item") == "true" {
				if r.Form.Has("caption") {
					t.Error("caption set on child")
				}
				if r.Form.Has("image_url") {
					imageCreates++
					fmt.Fprint(w, `{"id":"101"}`)
				} else {
					videoCreates++
					var state publishState
					_ = json.Unmarshal(persisted.Data, &state)
					if len(state.Children) != 2 || state.Children[0] != "101" {
						t.Error("first child not checkpointed before second")
					}
					if r.Form.Get("media_type") != "VIDEO" {
						t.Error("invalid carousel video type")
					}
					if videoCreates == 1 {
						w.WriteHeader(503)
						fmt.Fprint(w, `{"error":{"code":2,"is_transient":true}}`)
						return
					}
					fmt.Fprint(w, `{"id":"102"}`)
				}
			} else {
				parentCreates++
				if !videoReady || r.Form.Get("media_type") != "CAROUSEL" || r.Form.Get("children") != "101,102" || r.Form.Get("caption") != content.Text {
					t.Error("invalid/early carousel parent")
				}
				fmt.Fprint(w, `{"id":"103"}`)
			}
		case "/v26.0/101", "/v26.0/103":
			fmt.Fprint(w, `{"status_code":"FINISHED"}`)
		case "/v26.0/102":
			if videoReady {
				fmt.Fprint(w, `{"status_code":"FINISHED"}`)
			} else {
				fmt.Fprint(w, `{"status_code":"IN_PROGRESS"}`)
			}
		case "/v26.0/42/media_publish":
			publishes++
			_ = r.ParseForm()
			if r.Form.Get("creation_id") != "103" {
				t.Error("published child instead of parent")
			}
			fmt.Fprint(w, `{"id":"201"}`)
		default:
			http.NotFound(w, r)
		}
	})
	checkpoint := func(p model.Progress) error { persisted = p; return nil }
	_, err := c.Publish(context.Background(), publishToken(), content, model.Progress{}, checkpoint)
	var pe *provider.Error
	if !errors.As(err, &pe) || !pe.Retryable || pe.Ambiguous || parentCreates != 0 {
		t.Fatalf("creation retry error: %v", err)
	}
	out, err := c.Publish(context.Background(), publishToken(), content, persisted, checkpoint)
	if err != nil || out.Done || parentCreates != 0 || publishes != 0 {
		t.Fatalf("early parent publication: %+v %v", out, err)
	}
	videoReady = true
	out, err = c.Publish(context.Background(), publishToken(), content, persisted, checkpoint)
	if err != nil || !out.Done || imageCreates != 1 || videoCreates != 2 || parentCreates != 1 || publishes != 1 {
		t.Fatalf("carousel counts image=%d video=%d parent=%d publish=%d err=%v", imageCreates, videoCreates, parentCreates, publishes, err)
	}
}

func TestCheckpointFailureStopsDependentRequests(t *testing.T) {
	for _, stage := range []string{stageProcessing, stagePublishing} {
		t.Run(stage, func(t *testing.T) {
			var publishes, polls int
			c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v26.0/42/content_publishing_limit":
					quotaResponse(w, 0, 7)
				case "/v26.0/42/media":
					fmt.Fprint(w, `{"id":"101"}`)
				case "/v26.0/101":
					polls++
					fmt.Fprint(w, `{"status_code":"FINISHED"}`)
				case "/v26.0/42/media_publish":
					publishes++
					fmt.Fprint(w, `{"id":"201"}`)
				default:
					http.NotFound(w, r)
				}
			})
			checkpointErr := errors.New("checkpoint unavailable")
			_, err := c.Publish(context.Background(), publishToken(), imageContent(), model.Progress{}, func(p model.Progress) error {
				if p.Stage == stage {
					return checkpointErr
				}
				return nil
			})
			if !errors.Is(err, checkpointErr) || publishes != 0 || (stage == stageProcessing && polls != 0) {
				t.Fatalf("continued after checkpoint failed: polls=%d publishes=%d err=%v", polls, publishes, err)
			}
		})
	}
}

func TestAmbiguousPublishNeverRepeats(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		drop   bool
	}{
		{"missing ID", 200, `{}`, false},
		{"malformed JSON", 200, `{"id":`, false},
		{"wrong ID type", 200, `{"id":1.5}`, false},
		{"zero ID", 200, `{"id":"0"}`, false},
		{"server error", 500, `{"error":{"code":2,"is_transient":true,"message":"secret-sentinel"}}`, false},
		{"unstructured rejection", 400, `secret-sentinel`, false},
		{"contradictory receipt and error", 400, `{"id":"201","error":{"code":4,"is_transient":true}}`, false},
		{"lost response", 0, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests, publishes atomic.Int32
			var persisted model.Progress
			c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/v26.0/42/content_publishing_limit":
					quotaResponse(w, 0, 7)
				case "/v26.0/42/media":
					fmt.Fprint(w, `{"id":"101"}`)
				case "/v26.0/101":
					fmt.Fprint(w, `{"status_code":"FINISHED"}`)
				case "/v26.0/42/media_publish":
					publishes.Add(1)
					if tt.drop {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					w.WriteHeader(tt.status)
					fmt.Fprint(w, tt.body)
				default:
					http.NotFound(w, r)
				}
			})
			cp := func(p model.Progress) error { persisted = p; return nil }
			_, err := c.Publish(context.Background(), publishToken(), imageContent(), model.Progress{}, cp)
			var pe *provider.Error
			if !errors.As(err, &pe) || !pe.Ambiguous || pe.Retryable || !persisted.Unsafe || publishes.Load() != 1 {
				t.Fatalf("unsafe result lost: err=%v state=%+v publishes=%d", err, persisted, publishes.Load())
			}
			assertSanitized(t, err)
			before := requests.Load()
			_, err = c.Publish(context.Background(), publishToken(), imageContent(), persisted, cp)
			if !errors.As(err, &pe) || !pe.Ambiguous || requests.Load() != before || publishes.Load() != 1 {
				t.Fatalf("unsafe retry made requests: err=%v requests=%d before=%d", err, requests.Load(), before)
			}
		})
	}
}

func TestDefiniteRejectionClearsUnsafeOnlyDurably(t *testing.T) {
	for _, failClear := range []bool{false, true} {
		t.Run(fmt.Sprint(failClear), func(t *testing.T) {
			var persisted model.Progress
			var publishes int
			c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v26.0/42/content_publishing_limit":
					quotaResponse(w, 0, 7)
				case "/v26.0/42/media":
					fmt.Fprint(w, `{"id":"101"}`)
				case "/v26.0/101":
					fmt.Fprint(w, `{"status_code":"FINISHED"}`)
				case "/v26.0/42/media_publish":
					publishes++
					w.Header().Set("Retry-After", "37")
					w.WriteHeader(429)
					fmt.Fprint(w, `{"error":{"code":4,"message":"access-sentinel"}}`)
				default:
					http.NotFound(w, r)
				}
			})
			cp := func(p model.Progress) error {
				if failClear && persisted.Unsafe && !p.Unsafe {
					return errors.New("save failed")
				}
				persisted = p
				return nil
			}
			_, err := c.Publish(context.Background(), publishToken(), imageContent(), model.Progress{}, cp)
			var pe *provider.Error
			if !errors.As(err, &pe) || publishes != 1 {
				t.Fatalf("err=%v publishes=%d", err, publishes)
			}
			if failClear {
				if !pe.Ambiguous || pe.Retryable || !persisted.Unsafe {
					t.Fatalf("failed clear lost ambiguity: %v %+v", pe, persisted)
				}
			} else if pe.Ambiguous || !pe.Retryable || !pe.RateLimited || pe.RetryAfter != 37*time.Second || persisted.Unsafe || persisted.Stage != stageProcessing {
				t.Fatalf("rejection not safely resumable: %v %+v", pe, persisted)
			}
			assertSanitized(t, err)
		})
	}
}

func TestLiveQuotaBlocksResumedPublication(t *testing.T) {
	ready, full := false, false
	var creates, publishes int
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/42/content_publishing_limit":
			if full {
				quotaResponse(w, 7, 7)
			} else {
				quotaResponse(w, 0, 7)
			}
		case "/v26.0/42/media":
			creates++
			fmt.Fprint(w, `{"id":"101"}`)
		case "/v26.0/101":
			if ready {
				fmt.Fprint(w, `{"status_code":"FINISHED"}`)
			} else {
				fmt.Fprint(w, `{"status_code":"IN_PROGRESS"}`)
			}
		case "/v26.0/42/media_publish":
			publishes++
			fmt.Fprint(w, `{"id":"201"}`)
		default:
			http.NotFound(w, r)
		}
	})
	var persisted model.Progress
	cp := func(p model.Progress) error { persisted = p; return nil }
	_, err := c.Publish(context.Background(), publishToken(), reelContent(), model.Progress{}, cp)
	if err != nil {
		t.Fatal(err)
	}
	ready, full = true, true
	waiting, err := c.Publish(context.Background(), publishToken(), reelContent(), persisted, cp)
	var pe *provider.Error
	if !errors.As(err, &pe) || !pe.RateLimited || !pe.Retryable || pe.Ambiguous || pe.RetryAfter != 15*time.Minute ||
		waiting.Done || waiting.Progress.Stage != persisted.Stage || !bytes.Equal(waiting.Progress.Data, persisted.Data) ||
		publishes != 0 || creates != 1 || persisted.Unsafe {
		t.Fatalf("quota bypassed: err=%v creates=%d publishes=%d", err, creates, publishes)
	}
	full = false
	out, err := c.Publish(context.Background(), publishToken(), reelContent(), persisted, cp)
	if err != nil || !out.Done || creates != 1 || publishes != 1 {
		t.Fatalf("quota retry recreated media: %+v %v", out, err)
	}
}

func TestInvalidAndMismatchedProgressCannotRestart(t *testing.T) {
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request for invalid progress: %s", r.URL.Path)
		http.NotFound(w, r)
	})
	content := imageContent()
	state, _ := readState(content, model.Progress{})
	state.AccountID = "42"
	state.ContainerID = "101"
	data, _ := json.Marshal(state)
	changed := content
	changed.Text = "Changed after creation"
	for _, tt := range []struct {
		p model.Progress
		c model.Content
	}{
		{model.Progress{Stage: "unknown", Data: data}, content},
		{model.Progress{Stage: stageProcessing, Data: json.RawMessage(`{`)}, content},
		{model.Progress{Stage: stageProcessing}, content},
		{model.Progress{Stage: stagePublishing, Data: data, Unsafe: false}, content},
		{model.Progress{Stage: stageProcessing, Data: data}, changed},
	} {
		if _, err := c.Publish(context.Background(), publishToken(), tt.c, tt.p, func(model.Progress) error { return nil }); err == nil {
			t.Error("invalid progress accepted")
		}
	}
}

func TestUnsupportedOptionsAndFormats(t *testing.T) {
	c := New(provider.Config{})
	caps := model.Capabilities{CanPublish: true, MaxVideoDurationSeconds: 900}
	for _, raw := range []string{`{"media_type":"STORIES"}`, `{"api_base":"https://other.example"}`, `{"cover_url":"https://other.example/a.jpg"}`, `{"share_to_feed":true}`, `{"media_type":"IMAGE","media_type":"IMAGE"}`, `{"media_type":null}`, `[]`, `{} {}`} {
		content := imageContent()
		content.Options = json.RawMessage(raw)
		if err := c.Validate(content, caps); err == nil {
			t.Errorf("accepted invalid options: %s", raw)
		}
	}
	for _, modify := range []func(*model.Content){
		func(c *model.Content) { c.Media = nil },
		func(c *model.Content) { c.Media[0].MIMEType = "image/png" },
		func(c *model.Content) { c.Media[0].URL = "https://user:password@media.example/image.jpg" },
		func(c *model.Content) { c.Media[0].SizeBytes = 0 },
		func(c *model.Content) { c.Text = strings.Repeat("🙂", 2201) },
	} {
		content := imageContent()
		modify(&content)
		if err := c.Validate(content, caps); err == nil {
			t.Error("invalid content accepted")
		}
	}
}

func TestConfigurationWarningDoesNotLeakProviderText(t *testing.T) {
	warnings := configurationWarnings(json.RawMessage(`"access-sentinel signed-sentinel"`))
	if len(warnings) != 1 || strings.Contains(warnings[0], "sentinel") {
		t.Fatalf("unsafe warning: %v", warnings)
	}
}

func TestReceiptCheckpointFailureRemainsUnsafe(t *testing.T) {
	var persisted model.Progress
	var publishes int
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/42/content_publishing_limit":
			quotaResponse(w, 0, 7)
		case "/v26.0/42/media":
			fmt.Fprint(w, `{"id":"101"}`)
		case "/v26.0/101":
			fmt.Fprint(w, `{"status_code":"FINISHED"}`)
		case "/v26.0/42/media_publish":
			publishes++
			fmt.Fprint(w, `{"id":"201"}`)
		default:
			http.NotFound(w, r)
		}
	})
	cp := func(p model.Progress) error {
		if p.Stage == stagePublished {
			return errors.New("lost receipt checkpoint")
		}
		persisted = p
		return nil
	}
	_, err := c.Publish(context.Background(), publishToken(), imageContent(), model.Progress{}, cp)
	var pe *provider.Error
	if !errors.As(err, &pe) || !pe.Ambiguous || !persisted.Unsafe || publishes != 1 {
		t.Fatalf("lost receipt could be retried: err=%v state=%+v publishes=%d", err, persisted, publishes)
	}
	_, err = c.Publish(context.Background(), publishToken(), imageContent(), persisted, cp)
	if !errors.As(err, &pe) || !pe.Ambiguous || publishes != 1 {
		t.Fatal("lost receipt caused duplicate publication")
	}
}

func TestContainerStatusNeverBypassesReadiness(t *testing.T) {
	for _, status := range []string{"IN_PROGRESS", "ERROR", "EXPIRED", "PUBLISHED", "UNKNOWN"} {
		t.Run(status, func(t *testing.T) {
			var persisted model.Progress
			var publishes int
			c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v26.0/42/content_publishing_limit":
					quotaResponse(w, 0, 7)
				case "/v26.0/42/media":
					fmt.Fprint(w, `{"id":"101"}`)
				case "/v26.0/101":
					fmt.Fprintf(w, `{"status_code":%q,"status":"access-sentinel"}`, status)
				case "/v26.0/42/media_publish":
					publishes++
					fmt.Fprint(w, `{"id":"201"}`)
				default:
					http.NotFound(w, r)
				}
			})
			out, err := c.Publish(context.Background(), publishToken(), imageContent(), model.Progress{}, func(p model.Progress) error { persisted = p; return nil })
			if publishes != 0 || out.Done {
				t.Fatalf("published while status=%s", status)
			}
			switch status {
			case "IN_PROGRESS":
				if err != nil || out.PollAfter != time.Minute {
					t.Fatalf("processing: %+v %v", out, err)
				}
			case "EXPIRED":
				var state publishState
				_ = json.Unmarshal(persisted.Data, &state)
				if err != nil || state.ContainerID != "" || state.Renewals != 1 || persisted.Unsafe {
					t.Fatalf("expiry not renewed safely: %+v %v", state, err)
				}
			case "PUBLISHED":
				var pe *provider.Error
				if !errors.As(err, &pe) || !pe.Ambiguous {
					t.Fatalf("published container fabricated a receipt: %v", err)
				}
			default:
				assertSanitized(t, err)
			}
		})
	}
}
