package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestConfirmedXReplyRequiresBoundOwnedReceipt(t *testing.T) {
	owner := Principal{TenantID: "spawn", Subject: "operator"}
	account := Account{ID: "acct_spawn", Owner: owner, Platform: X, Status: "active", Identity: Identity{ID: "42"}}
	content := Content{Text: "SPAWN | Backroom VESPER-04\nTest contract SPAWN-TEST-ONE: accepted."}
	encoded, _ := json.Marshal(content)
	digest := sha256.Sum256(encoded)
	raw, _ := json.Marshal(map[string]any{"version": 1, "owner_id": "42", "post_id": "1000", "content_hash": hex.EncodeToString(digest[:])})
	original := Post{ID: "post_root", Owner: owner, Targets: []Target{{AccountID: account.ID, Platform: X, Status: "published", Content: content, RemoteID: "1000", Progress: Progress{Stage: "x.published", Data: raw}}}}
	reply, err := ConfirmedXReply(original, account)
	if err != nil || *reply != (XReply{PostID: "post_root", RemoteID: "1000", OwnerID: "42"}) {
		t.Fatalf("missing bound parent: %+v %v", reply, err)
	}
	cases := []struct {
		name   string
		mutate func(*Post, *Account)
	}{
		{"tenant", func(p *Post, a *Account) { p.Owner.TenantID = "other" }},
		{"subject", func(p *Post, a *Account) { p.Owner.Subject = "customer" }},
		{"account", func(p *Post, a *Account) { p.Targets[0].AccountID = "other" }},
		{"identity", func(p *Post, a *Account) { a.Identity.ID = "99" }},
		{"disconnected", func(p *Post, a *Account) { a.Status = "disconnected" }},
		{"unknown send", func(p *Post, a *Account) { p.Targets[0].Progress.Unsafe = true }},
		{"unconfirmed", func(p *Post, a *Account) { p.Targets[0].Status = "needs_attention" }},
		{"content changed", func(p *Post, a *Account) { p.Targets[0].Content.Text = "different" }},
		{"remote changed", func(p *Post, a *Account) { p.Targets[0].RemoteID = "2000" }},
		{"missing checkpoint", func(p *Post, a *Account) { p.Targets[0].Progress = Progress{} }},
		{"duplicate", func(p *Post, a *Account) { p.Targets = append(p.Targets, p.Targets[0]) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			p := original
			p.Targets = append([]Target(nil), original.Targets...)
			a := account
			test.mutate(&p, &a)
			if _, err := ConfirmedXReply(p, a); err == nil {
				t.Fatal("unqualified parent accepted")
			}
		})
	}
}
