package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"socialpublisher/internal/model"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
	"testing"
	"time"
)

type threadAPIStore struct {
	*apiStore
	parent model.Post
}

func (s *threadAPIStore) GetPost(_ context.Context, owner model.Principal, id string) (model.Post, error) {
	if id != s.parent.ID || owner != s.parent.Owner {
		return model.Post{}, store.ErrNotFound
	}
	return s.parent, nil
}
func TestThreadParentOwnershipAndReadOnlyAdmissionRecovery(t *testing.T) {
	srv, base, adapter, token := fixtureAPI(t)
	base.account.Identity.ID = "42"
	rawToken, _ := json.Marshal(model.Token{AccessToken: "fixture", ProviderUserID: "42", ExpiresAt: time.Now().Add(time.Hour)})
	base.account.EncryptedToken, _ = srv.Vault.Seal(rawToken, base.account.TokenAAD())
	content := model.Content{Text: "original"}
	raw, _ := json.Marshal(content)
	digest := sha256.Sum256(raw)
	receipt, _ := json.Marshal(map[string]any{"version": 1, "owner_id": "42", "post_id": "1000", "content_hash": hex.EncodeToString(digest[:])})
	s := &threadAPIStore{apiStore: base, parent: model.Post{ID: "post_root", Owner: base.owner, Targets: []model.Target{{AccountID: base.account.ID, Platform: model.X, Status: "published", Content: content, RemoteID: "1000", Progress: model.Progress{Stage: "x.published", Data: receipt}}}}}
	srv.Store = s
	body := `{"text":"completed and settled","targets":[{"account_id":"acct-x","reply_to_post_id":"post_root"}]}`
	w := req(srv.Handler(), "POST", "/v1/posts", body, token)
	if w.Code != 201 {
		t.Fatalf("confirmed local reply rejected: %d %s", w.Code, w.Body.String())
	}
	if base.post.Targets[0].Content.Reply == nil || base.post.Targets[0].Content.Reply.RemoteID != "1000" {
		t.Fatal("parent was not server resolved")
	}
	caps := adapter.caps
	for i := 0; i < 2; i++ {
		w = req(srv.Handler(), "GET", "/v1/post-receipts/request-1?request_hash="+security.Hash(body), "", token)
		if w.Code != 200 {
			t.Fatalf("read-only recovery failed %d %s", w.Code, w.Body.String())
		}
	}
	if base.created != 1 || adapter.caps != caps {
		t.Fatal("receipt lookup admitted provider work")
	}
	base.post = nil
	s.parent.Owner.Subject = "other"
	w = req(srv.Handler(), "POST", "/v1/posts", body, token)
	if w.Code != 409 || base.created != 1 || adapter.caps != caps {
		t.Fatal("foreign parent reached admission")
	}
	s.parent.Owner = base.owner
	s.parent.Targets[0].Progress.Unsafe = true
	w = req(srv.Handler(), "POST", "/v1/posts", body, token)
	if w.Code != 409 || base.created != 1 {
		t.Fatal("unknown parent admitted")
	}
	w = req(srv.Handler(), "POST", "/v1/posts", `{"text":"x","targets":[{"account_id":"acct-x","reply":{"remote_id":"1000"}}]}`, token)
	if w.Code != 400 {
		t.Fatal("arbitrary remote reply fields accepted")
	}
}

func TestLifecycleReceiptDigestUsesRuntimeFieldOrderAndFractionalDeadline(t *testing.T) {
	// Shared protocol example: the runtime normalizes JS millisecond ISO strings
	// before hashing, matching Go's canonical request encoding on admission.
	input := `{"text":"SPAWN | Backroom VESPER-04\nTest contract SPAWN-TEST-ONE: completed and settled.","targets":[{"account_id":"acct_spawn","reply_to_post_id":"post_root","publish_before":"1970-01-01T01:16:40.100Z"}]}`
	canonical := `{"text":"SPAWN | Backroom VESPER-04\nTest contract SPAWN-TEST-ONE: completed and settled.","targets":[{"account_id":"acct_spawn","reply_to_post_id":"post_root","publish_before":"1970-01-01T01:16:40.1Z"}]}`
	var request model.CreatePostRequest
	if err := json.Unmarshal([]byte(input), &request); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil || string(encoded) != canonical {
		t.Fatalf("runtime receipt digest would differ: %s %v", encoded, err)
	}
}
