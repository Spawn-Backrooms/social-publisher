package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"socialpublisher/internal/model"
	"socialpublisher/internal/store"
	"testing"
	"time"
)

type threadWorkerStore struct {
	*fixtureStore
	parent model.Post
}

func TestDelayedLifecycleQueueExpiresBeforeProviderWork(t *testing.T) {
	e, base, adapter := setupEngine(t)
	deadline := e.now().Add(-time.Second)
	base.target.Content.PublishBefore = &deadline
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 0 || adapter.caps != 0 || base.finished == nil || base.finished.Status != "cancelled" || base.finished.Error != "publication_window_expired" {
		t.Fatalf("delayed test reached provider: %+v", base.finished)
	}
}

func (s *threadWorkerStore) GetPost(_ context.Context, p model.Principal, id string) (model.Post, error) {
	if p != s.parent.Owner || id != s.parent.ID {
		return model.Post{}, store.ErrNotFound
	}
	return s.parent, nil
}
func TestThreadWorkerRechecksParentBeforeAnyProviderCall(t *testing.T) {
	for _, change := range []string{"identity", "receipt", "owner", "account", "unsafe"} {
		t.Run(change, func(t *testing.T) {
			e, base, adapter := setupEngine(t)
			base.account.Identity.ID = "42"
			content := model.Content{Text: "original"}
			encoded, _ := json.Marshal(content)
			digest := sha256.Sum256(encoded)
			raw, _ := json.Marshal(map[string]any{"version": 1, "owner_id": "42", "post_id": "1000", "content_hash": hex.EncodeToString(digest[:])})
			s := &threadWorkerStore{fixtureStore: base, parent: model.Post{ID: "post_root", Owner: base.account.Owner, Targets: []model.Target{{AccountID: base.account.ID, Platform: model.X, Status: "published", Content: content, RemoteID: "1000", Progress: model.Progress{Stage: "x.published", Data: raw}}}}}
			base.target.Content = model.Content{Text: "reply", Reply: &model.XReply{PostID: "post_root", RemoteID: "1000", OwnerID: "42"}}
			switch change {
			case "identity":
				base.account.Identity.ID = "99"
			case "receipt":
				s.parent.Targets[0].RemoteID = "2000"
			case "owner":
				s.parent.Owner.Subject = "other"
			case "account":
				s.parent.Targets[0].AccountID = "other"
			case "unsafe":
				s.parent.Targets[0].Progress.Unsafe = true
			}
			e.Store = s
			if err := e.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if adapter.calls != 0 || adapter.caps != 0 || base.finished == nil || base.finished.Status != "failed" {
				t.Fatalf("changed parent reached provider: %+v", base.finished)
			}
		})
	}
}
