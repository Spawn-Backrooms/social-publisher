package x

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"socialpublisher/internal/model"
	"sync/atomic"
	"testing"
	"time"
)

func TestExplicitOwnedThreadPayloadAndUnknownSendNoRetry(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "unknown"}[unknown], func(t *testing.T) {
			var calls atomic.Int32
			cp := new(checkpoints)
			c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/2/tweets" {
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
				calls.Add(1)
				if !cp.current().Unsafe {
					t.Error("public send preceded unsafe durable checkpoint")
				}
				var body map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Fatal("bad payload")
				}
				var reply struct {
					ID string `json:"in_reply_to_tweet_id"`
				}
				_ = json.Unmarshal(body["reply"], &reply)
				if reply.ID != "1000" || len(body) != 2 {
					t.Errorf("unexpected reply payload %+v", body)
				}
				if unknown {
					w.WriteHeader(502)
					return
				}
				w.WriteHeader(201)
				_, _ = io.WriteString(w, `{"data":{"id":"2000"}}`)
			}, nil)
			content := model.Content{Text: "SPAWN | Backroom VESPER-04\nTest contract SPAWN-TEST-ONE: completed and settled.", Reply: &model.XReply{PostID: "post_root", RemoteID: "1000", OwnerID: "42"}}
			out, err := c.Publish(context.Background(), fixtureToken(), content, model.Progress{}, cp.save)
			if unknown {
				if err == nil || !providerError(t, err).Ambiguous {
					t.Fatal("unknown send was not quarantined")
				}
			} else if err != nil || !out.Done || out.RemoteID != "2000" {
				t.Fatalf("missing receipt: %+v %v", out, err)
			}
			_, err = c.Publish(context.Background(), fixtureToken(), content, cp.current(), cp.save)
			if calls.Load() != 1 {
				t.Fatal("confirmed or unknown send was repeated")
			}
			if unknown && (err == nil || !providerError(t, err).Ambiguous) {
				t.Fatal("unsafe retry admitted")
			}
		})
	}
}

func TestExpiredThreadWindowNeverReachesX(t *testing.T) {
	var calls atomic.Int32
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, nil)
	deadline := time.Now().Add(-time.Minute)
	content := model.Content{Text: "expired test", PublishBefore: &deadline}
	if _, err := c.Publish(context.Background(), fixtureToken(), content, model.Progress{}, new(checkpoints).save); err == nil || providerError(t, err).Code != "x_publication_window_expired" {
		t.Fatalf("expired window admitted: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("expired publication reached X")
	}
}
func TestThreadRejectsIdentityMismatchAndArbitraryOptionsBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, nil)
	for _, content := range []model.Content{
		{Text: "reply", Reply: &model.XReply{PostID: "post_root", RemoteID: "1000", OwnerID: "99"}},
		{Text: "reply", Options: json.RawMessage(`{"reply":{"in_reply_to_tweet_id":"1000"}}`)},
		{Text: "reply", Reply: &model.XReply{PostID: "post_root", RemoteID: "bad", OwnerID: "42"}},
	} {
		if _, err := c.Publish(context.Background(), fixtureToken(), content, model.Progress{}, new(checkpoints).save); err == nil {
			t.Fatal("unqualified reply accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unqualified reply reached provider")
	}
}
