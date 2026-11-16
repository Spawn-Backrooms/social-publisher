package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"socialpublisher/internal/model"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
	"strings"
	"testing"
	"time"
)

func lifecycleFixture(t *testing.T) (*Server, *apiStore, *apiAdapter, string, string) {
	s, db, adapter, requester := fixtureAPI(t)
	jwt, _ := security.NewJWT(bytes.Repeat([]byte{1}, 32), "spawn-lifecycle-service", "spawn-lifecycle-publisher")
	s.Lifecycle = &LifecycleGrant{JWT: jwt, Owner: db.owner, AccountID: db.account.ID, ProviderUserID: "42"}
	db.account.Identity.ID = "42"
	raw, _ := json.Marshal(model.Token{AccessToken: "offline-fixture", ProviderUserID: "42", ExpiresAt: time.Now().Add(time.Hour)})
	db.account.EncryptedToken, _ = s.Vault.Seal(raw, db.account.TokenAAD())
	service, _ := jwt.Mint(LifecycleService, time.Minute, time.Now())
	return s, db, adapter, service, requester
}
func eventInput(stage string) lifecycleRequest {
	consent := "sha256:" + strings.Repeat("b", 64)
	slot, _ := json.Marshal(map[string]string{"domain": "spawn.platform-lifecycle-slot.v1", "consent": consent, "stage": stage})
	sequence := 1
	if stage == "settled" {
		sequence = 3
	}
	return lifecycleRequest{Event: lifecycleEvent{EventID: "sha256:" + security.Hash(string(slot)), AccountRef: "acct-x", ConsentHash: consent, EvidenceHash: "sha256:" + strings.Repeat("c", 64), Sequence: sequence, Public: lifecyclePublic{Schema: "spawn.backroom-lifecycle.v1", Contract: "contract_" + strings.Repeat("a", 32), Backroom: "RAVEL-12", Stage: stage}}, ExpiresAtMS: time.Now().Add(time.Hour).UnixMilli()}
}
func TestLifecycleGrantRejectsRequesterAndNormalOwnerRoutes(t *testing.T) {
	s, db, _, service, requester := lifecycleFixture(t)
	raw, _ := json.Marshal(eventInput("accepted"))
	if w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), requester); w.Code != 401 {
		t.Fatalf("requester reached central grant: %d", w.Code)
	}
	for _, path := range []string{"/v1/accounts", "/v1/posts", "/v1/connections/x/authorize"} {
		if w := req(s.Handler(), "GET", path, "", service); w.Code != 401 {
			t.Fatalf("service reached normal route %s: %d", path, w.Code)
		}
	}
	if db.created != 0 {
		t.Fatal("negative auth admitted a post")
	}
	s.Lifecycle = nil
	if w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service); w.Code != 503 {
		t.Fatalf("default off failed: %d", w.Code)
	}
}
func TestLifecycleBudgetV2AcceptsAndRepliesWithExactSpend(t *testing.T) {
	s, db, _, service, _ := lifecycleFixture(t)
	cap, spent := "15000", "965"
	in := eventInput("accepted")
	in.Event.Public.Schema = "spawn.backroom-lifecycle.v2"
	in.Event.Public.BudgetCapMicroUSD = &cap
	raw, _ := json.Marshal(in)
	w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
	if w.Code != 201 || !strings.HasSuffix(db.post.Targets[0].Content.Text, "accepted. Budget cap $0.015.") {
		t.Fatalf("v2 acceptance failed: %d %s", w.Code, w.Body.String())
	}
	root := *db.post
	root.Status = "published"
	root.Targets[0].Status = "published"
	root.Targets[0].RemoteID = "1000"
	content, _ := json.Marshal(root.Targets[0].Content)
	sum := sha256.Sum256(content)
	proof, _ := json.Marshal(map[string]any{"version": 1, "owner_id": "42", "post_id": "1000", "content_hash": hex.EncodeToString(sum[:])})
	root.Targets[0].Progress = model.Progress{Stage: "x.published", Data: proof}
	db.post = &root
	s.Store = &threadAPIStore{apiStore: db, parent: root}
	next := eventInput("settled")
	next.Event.Public.Schema = "spawn.backroom-lifecycle.v2"
	next.Event.Public.BudgetCapMicroUSD = &cap
	next.Event.Public.ActualSpentMicroUSD = &spent
	next.Parent = &lifecycleParent{EventID: in.Event.EventID, RequestHash: security.Hash(string(raw))}
	nextRaw, _ := json.Marshal(next)
	w = req(s.Handler(), "POST", "/v1/lifecycle/events", string(nextRaw), service)
	if w.Code != 201 || !strings.HasSuffix(db.post.Targets[0].Content.Text, "completed and settled. Spent $0.000965 of $0.015 budget cap.") || db.post.Targets[0].Content.Reply == nil || db.post.Targets[0].Content.Reply.RemoteID != "1000" {
		t.Fatalf("v2 completion reply failed: %d %s", w.Code, w.Body.String())
	}
	created := db.created
	w = req(s.Handler(), "GET", "/v1/lifecycle/receipts/"+next.Event.EventID+"?request_hash="+security.Hash(string(nextRaw)), "", service)
	if w.Code != 200 || db.created != created {
		t.Fatal("v2 receipt recovery repeated admission")
	}
}
func TestLifecycleBudgetV2RejectsUnprovedAmounts(t *testing.T) {
	s, db, _, service, _ := lifecycleFixture(t)
	cap, spent := "15000", "15001"
	in := eventInput("settled")
	in.Event.Public.Schema = "spawn.backroom-lifecycle.v2"
	in.Event.Public.BudgetCapMicroUSD = &cap
	in.Parent = &lifecycleParent{EventID: eventInput("accepted").Event.EventID, RequestHash: strings.Repeat("a", 64)}
	for _, actual := range []*string{nil, &spent} {
		in.Event.Public.ActualSpentMicroUSD = actual
		raw, _ := json.Marshal(in)
		w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
		if w.Code != 400 || db.created != 0 {
			t.Fatalf("unproved spend admitted: %d %s", w.Code, w.Body.String())
		}
	}
	in = eventInput("accepted")
	in.Event.Public.BudgetCapMicroUSD = &cap
	raw, _ := json.Marshal(in)
	w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
	if w.Code != 400 || db.created != 0 {
		t.Fatal("legacy event acquired unconsented amount fields")
	}
}
func TestLifecycleStrictEnvelopeCentralPinAndSafePublicText(t *testing.T) {
	s, db, ad, service, _ := lifecycleFixture(t)
	in := eventInput("accepted")
	raw, _ := json.Marshal(in)
	w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
	if w.Code != 201 {
		t.Fatalf("admission failed: %d %s", w.Code, w.Body.String())
	}
	if db.created != 1 || db.post.Owner != db.owner || len(db.post.Targets) != 1 || db.post.Targets[0].AccountID != "acct-x" || db.post.Targets[0].Content.Text != lifecycleText(in.Event) {
		t.Fatal("grant escaped pinned account or text")
	}
	caps := ad.caps
	hash := security.Hash(string(raw))
	// Simulate a lost acknowledgement: only read-only receipt lookup follows it.
	for i := 0; i < 2; i++ {
		w = req(s.Handler(), "GET", "/v1/lifecycle/receipts/"+in.Event.EventID+"?request_hash="+hash, "", service)
		if w.Code != 200 {
			t.Fatalf("receipt failed %d %s", w.Code, w.Body.String())
		}
	}
	if db.created != 1 || ad.caps != caps {
		t.Fatal("receipt lookup admitted or repeated provider work")
	}
	if !strings.Contains(w.Body.String(), `"confirmed":false`) {
		t.Fatal("admission invented public proof")
	}
	db.post = nil
	for _, bad := range []string{
		strings.Replace(string(raw), `"account_ref":"acct-x"`, `"account_ref":"foreign"`, 1),
		strings.Replace(string(raw), `"backroom":"RAVEL-12"`, `"backroom":"customer private brief"`, 1),
		strings.Replace(string(raw), `"stage":"accepted"`, `"stage":"dispatch_authorized"`, 1),
		strings.Replace(string(raw), `"event":{`, `"brief":"private","event":{`, 1),
	} {
		w = req(s.Handler(), "POST", "/v1/lifecycle/events", bad, service)
		if w.Code != 400 {
			t.Fatalf("unsafe envelope accepted %d %s", w.Code, w.Body.String())
		}
	}
	db.account.Identity.ID = "99"
	w = req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
	if w.Code != 409 || db.created != 1 {
		t.Fatal("changed identity escaped central pin")
	}
}
func TestLifecycleReplyRequiresOriginalNativeProofAndSameContractConsent(t *testing.T) {
	s, db, _, service, _ := lifecycleFixture(t)
	in := eventInput("accepted")
	raw, _ := json.Marshal(in)
	w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	root := *db.post
	root.Status = "published"
	root.Targets[0].Status = "published"
	root.Targets[0].RemoteID = "1000"
	content, _ := json.Marshal(root.Targets[0].Content)
	sum := sha256.Sum256(content)
	proof, _ := json.Marshal(map[string]any{"version": 1, "owner_id": "42", "post_id": "1000", "content_hash": hex.EncodeToString(sum[:])})
	root.Targets[0].Progress = model.Progress{Stage: "x.published", Data: proof}
	db.post = &root
	hash := security.Hash(string(raw))
	w = req(s.Handler(), "GET", "/v1/lifecycle/receipts/"+in.Event.EventID+"?request_hash="+hash, "", service)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"confirmed":true`) {
		t.Fatalf("original proof not confirmed %d %s", w.Code, w.Body.String())
	}
	threadStore := &threadAPIStore{apiStore: db, parent: root}
	s.Store = threadStore
	next := eventInput("settled")
	next.Parent = &lifecycleParent{EventID: in.Event.EventID, RequestHash: hash}
	// Lookup fixture only holds one post, so the existing accepted key is matched
	// until the final CreatePost replaces it after verified parent correlation.
	nextRaw, _ := json.Marshal(next)
	w = req(s.Handler(), "POST", "/v1/lifecycle/events", string(nextRaw), service)
	if w.Code != http.StatusCreated {
		t.Fatalf("settled reply failed %d %s", w.Code, w.Body.String())
	}
	if db.post.Targets[0].Content.Reply == nil || db.post.Targets[0].Content.Reply.RemoteID != "1000" {
		t.Fatal("reply not server resolved")
	}
	// A manual 'published' resolution without the original unsafe-send checkpoint
	// must not establish activity or release the next public update.
	root.Targets[0].Progress = model.Progress{Stage: "manual_resolution"}
	db.post = &root
	threadStore.parent = root
	db.key = lifecycleKey(in.Event.EventID)
	db.hash = hash
	w = req(s.Handler(), "GET", "/v1/lifecycle/receipts/"+in.Event.EventID+"?request_hash="+hash, "", service)
	if !strings.Contains(w.Body.String(), `"confirmed":false`) {
		t.Fatal("manual resolution invented original proof")
	}
	w = req(s.Handler(), "POST", "/v1/lifecycle/events", string(nextRaw), service)
	if w.Code != 409 {
		t.Fatalf("unproved reply admitted %d %s", w.Code, w.Body.String())
	}
}

type lifecycleCancelStore struct {
	*apiStore
	cancelled int
}

func (s *lifecycleCancelStore) CancelPost(_ context.Context, owner model.Principal, id string) (model.Post, error) {
	if s.post == nil || owner != s.post.Owner || id != s.post.ID {
		return model.Post{}, store.ErrNotFound
	}
	s.cancelled++
	s.post.Status = "cancelled"
	return *s.post, nil
}
func TestLifecycleWithdrawalCancelsOnlyItsOriginalReceiptWithoutProviderWork(t *testing.T) {
	s, db, ad, service, _ := lifecycleFixture(t)
	local := &lifecycleCancelStore{apiStore: db}
	s.Store = local
	in := eventInput("accepted")
	raw, _ := json.Marshal(in)
	w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	caps := ad.caps
	path := "/v1/lifecycle/receipts/" + in.Event.EventID + "/cancel?request_hash=" + security.Hash(string(raw))
	w = req(s.Handler(), "POST", path, "{}", service)
	if w.Code != 200 || local.cancelled != 1 || db.created != 1 || ad.caps != caps {
		t.Fatal("receipt cancellation escaped original admission", w.Code, w.Body.String())
	}
	w = req(s.Handler(), "POST", strings.Replace(path, security.Hash(string(raw)), strings.Repeat("f", 64), 1), "{}", service)
	if w.Code == 200 || local.cancelled != 1 {
		t.Fatal("wrong receipt hash cancelled a post")
	}
}

func TestLifecyclePreviouslyCompletedIsOneTruthfulOriginalWithImmutableReceipt(t *testing.T) {
	s, db, ad, service, _ := lifecycleFixture(t)
	in := eventInput("previously_completed")
	in.Event.Sequence = 4
	raw, _ := json.Marshal(in)
	w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
	if w.Code != 201 || db.created != 1 || db.post.Targets[0].Content.Reply != nil || !strings.Contains(db.post.Targets[0].Content.Text, "Previously completed. Private delivery and settlement verified in SPAWN.") {
		t.Fatalf("retrospective original rejected or fabricated start: %d %s", w.Code, w.Body.String())
	}
	caps := ad.caps
	w = req(s.Handler(), "GET", "/v1/lifecycle/receipts/"+in.Event.EventID+"?request_hash="+security.Hash(string(raw)), "", service)
	if w.Code != 200 || db.created != 1 || ad.caps != caps {
		t.Fatal("receipt read repeated admission or provider work")
	}
	in.Parent = &lifecycleParent{EventID: eventInput("accepted").Event.EventID, RequestHash: strings.Repeat("a", 64)}
	bad, _ := json.Marshal(in)
	if w = req(s.Handler(), "POST", "/v1/lifecycle/events", string(bad), service); w.Code != 400 || db.created != 1 {
		t.Fatal("retrospective summary admitted a fabricated parent")
	}
}
