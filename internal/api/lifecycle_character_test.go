package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"socialpublisher/internal/model"
	"socialpublisher/internal/security"
	"strings"
	"testing"
)

func TestCharacterPublicWorkRequiresPinnedOriginAndPostsFactualReceipt(t *testing.T) {
	s, db, _, service, _ := lifecycleFixture(t)
	s.Lifecycle.PublicDeliveryOrigin = "https://spawnbackrooms.com"
	in := eventInput("accepted")
	in.Event.EventID = "sha256:" + security.Hash(`{"consent":"`+in.Event.ConsentHash+`","domain":"spawn.character-work-lifecycle-slot.v1","stage":"accepted"}`)
	raw, _ := json.Marshal(in)
	var packet map[string]any
	json.Unmarshal(raw, &packet)
	p := packet["event"].(map[string]any)["public"].(map[string]any)
	p["schema"] = "spawn.character-work-lifecycle.v1"
	p["backroom"] = "ELF-01"
	p["title"] = "Elf football trick"
	p["budget_cap_micro_usd"] = "2757888"
	p["work_url"] = "https://attacker.example/works/" + strings.Repeat("a", 64) + "/" + strings.Repeat("b", 32)
	raw, _ = json.Marshal(packet)
	if w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service); w.Code != 400 || db.created != 0 {
		t.Fatal("untrusted public origin admitted")
	}
	p["work_url"] = "https://spawnbackrooms.com/works/" + strings.Repeat("a", 64) + "/" + strings.Repeat("b", 32)
	raw, _ = json.Marshal(packet)
	w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
	if w.Code != 201 {
		t.Fatalf("character acceptance rejected: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(db.post.Targets[0].Content.Text, "Elf football trick — Accepted. Up to $2.757888.") || !strings.Contains(db.post.Targets[0].Content.Text, p["work_url"].(string)) {
		t.Fatal("missing exact work title/price/link")
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
	packet["parent"] = map[string]any{"event_id": in.Event.EventID, "request_hash": security.Hash(string(raw))}
	event := packet["event"].(map[string]any)
	event["event_id"] = lifecycleEventID(in.Event.ConsentHash, "settled", characterLifecycleSchema)
	event["sequence"] = 3
	p["stage"] = "settled"
	p["actual_spent_micro_usd"] = "1200000"
	next, _ := json.Marshal(packet)
	w = req(s.Handler(), "POST", "/v1/lifecycle/events", string(next), service)
	if w.Code != 201 || db.post.Targets[0].Content.Reply == nil || db.post.Targets[0].Content.Reply.RemoteID != "1000" || !strings.Contains(db.post.Targets[0].Content.Text, "Completed. $1.2.") {
		t.Fatalf("character completion did not reply to exact acceptance: %d %s", w.Code, w.Body.String())
	}
}
