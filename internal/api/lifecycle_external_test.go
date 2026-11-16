package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"socialpublisher/internal/model"
	"socialpublisher/internal/security"
	"strings"
	"testing"
	"time"
)

const externalFixtureOrigin = "https://spawn.example.com"

func externalEventInput(stage string) lifecycleRequest {
	in := eventInput(stage)
	currency, quote := "TEST_USD", "15000"
	in.Event.Public.Schema = "spawn.external-work-lifecycle.v1"
	in.Event.Public.Backroom = "Independent Studio 9"
	in.Event.Public.Currency = &currency
	in.Event.Public.QuotedAmountMicroUnits = &quote
	in.Event.EventID = lifecycleEventID(in.Event.ConsentHash, stage, in.Event.Public.Schema)
	if stage == "settled" {
		url := externalFixtureOrigin + "/deliveries/delivery_" + strings.Repeat("d", 32)
		in.Event.Public.PublicDeliveryURL = &url
	}
	return in
}

func externalConfirmedParent(t *testing.T, db *apiStore) model.Post {
	t.Helper()
	root := *db.post
	root.Status = "published"
	root.Targets[0].Status = "published"
	root.Targets[0].RemoteID = "1000"
	content, _ := json.Marshal(root.Targets[0].Content)
	sum := sha256.Sum256(content)
	proof, _ := json.Marshal(map[string]any{"version": 1, "owner_id": "42", "post_id": "1000", "content_hash": hex.EncodeToString(sum[:])})
	root.Targets[0].Progress = model.Progress{Stage: "x.published", Data: proof}
	return root
}

func TestExternalLifecycleExactQuoteAndConfirmedPublicDeliveryReply(t *testing.T) {
	for _, currency := range []string{"USDC", "TEST_USD"} {
		t.Run(currency, func(t *testing.T) {
			s, db, adapter, service, _ := lifecycleFixture(t)
			s.Lifecycle.PublicDeliveryOrigin = externalFixtureOrigin
			in := externalEventInput("accepted")
			in.Event.Public.Currency = &currency
			raw, _ := json.Marshal(in)
			w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
			want := "SPAWN | External Backroom Independent Studio 9\nContract " + in.Event.Public.Contract + ": accepted. Budget quote 0.015 " + currency + "."
			if w.Code != 201 || db.post.Targets[0].Content.Text != want || db.post.Targets[0].Content.Reply != nil {
				t.Fatalf("external acceptance failed: %d %s", w.Code, w.Body.String())
			}
			root := externalConfirmedParent(t, db)
			db.post = &root
			s.Store = &threadAPIStore{apiStore: db, parent: root}
			next := externalEventInput("settled")
			next.Event.Public.Currency = &currency
			next.Parent = &lifecycleParent{EventID: in.Event.EventID, RequestHash: security.Hash(string(raw))}
			nextRaw, _ := json.Marshal(next)
			w = req(s.Handler(), "POST", "/v1/lifecycle/events", string(nextRaw), service)
			want = "SPAWN | External Backroom Independent Studio 9\nContract " + in.Event.Public.Contract + ": completed. Settlement recorded. Budget quote 0.015 " + currency + ".\nPublic delivery: " + *next.Event.Public.PublicDeliveryURL
			if w.Code != 201 || db.post.Targets[0].Content.Text != want || db.post.Targets[0].Content.Reply == nil || db.post.Targets[0].Content.Reply.RemoteID != "1000" {
				t.Fatalf("external completion failed: %d %s", w.Code, w.Body.String())
			}
			caps, created := adapter.caps, db.created
			for i := 0; i < 2; i++ {
				w = req(s.Handler(), "GET", "/v1/lifecycle/receipts/"+next.Event.EventID+"?request_hash="+security.Hash(string(nextRaw)), "", service)
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"confirmed":false`) {
					t.Fatal("admission was mistaken for completion publication", w.Code, w.Body.String())
				}
			}
			if db.created != created || adapter.caps != caps {
				t.Fatal("receipt recovery repeated provider work")
			}
		})
	}
}

func TestExternalLifecycleStrictFieldsAmountsLabelsAndOrigin(t *testing.T) {
	for name, change := range map[string]func(*lifecycleRequest, *Server){
		"origin absent":        func(_ *lifecycleRequest, s *Server) { s.Lifecycle.PublicDeliveryOrigin = "" },
		"origin not canonical": func(_ *lifecycleRequest, s *Server) { s.Lifecycle.PublicDeliveryOrigin += "/" },
		"currency USD":         func(in *lifecycleRequest, _ *Server) { value := "USD"; in.Event.Public.Currency = &value },
		"currency absent":      func(in *lifecycleRequest, _ *Server) { in.Event.Public.Currency = nil },
		"zero quote":           func(in *lifecycleRequest, _ *Server) { value := "0"; in.Event.Public.QuotedAmountMicroUnits = &value },
		"fraction quote":       func(in *lifecycleRequest, _ *Server) { value := "0.1"; in.Event.Public.QuotedAmountMicroUnits = &value },
		"leading zero quote": func(in *lifecycleRequest, _ *Server) {
			value := "015000"
			in.Event.Public.QuotedAmountMicroUnits = &value
		},
		"oversized quote": func(in *lifecycleRequest, _ *Server) {
			value := strings.Repeat("9", 31)
			in.Event.Public.QuotedAmountMicroUnits = &value
		},
		"native amount":        func(in *lifecycleRequest, _ *Server) { value := "15000"; in.Event.Public.BudgetCapMicroUSD = &value },
		"newline label":        func(in *lifecycleRequest, _ *Server) { in.Event.Public.Backroom = "Studio\nPrivate" },
		"mention label":        func(in *lifecycleRequest, _ *Server) { in.Event.Public.Backroom = "@someone" },
		"unicode label":        func(in *lifecycleRequest, _ *Server) { in.Event.Public.Backroom = "Studio café" },
		"oversized label":      func(in *lifecycleRequest, _ *Server) { in.Event.Public.Backroom = strings.Repeat("a", 49) },
		"trailing space label": func(in *lifecycleRequest, _ *Server) { in.Event.Public.Backroom += " " },
		"accepted URL": func(in *lifecycleRequest, _ *Server) {
			value := externalFixtureOrigin + "/deliveries/delivery_" + strings.Repeat("d", 32)
			in.Event.Public.PublicDeliveryURL = &value
		},
		"wrong slot domain": func(in *lifecycleRequest, _ *Server) { in.Event.EventID = eventInput("accepted").Event.EventID },
		"retrospective": func(in *lifecycleRequest, _ *Server) {
			in.Event.Public.Stage = "previously_completed"
			in.Event.Sequence = 4
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, db, adapter, service, _ := lifecycleFixture(t)
			s.Lifecycle.PublicDeliveryOrigin = externalFixtureOrigin
			in := externalEventInput("accepted")
			change(&in, s)
			raw, _ := json.Marshal(in)
			w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
			if w.Code != 400 || db.created != 0 || adapter.caps != 0 {
				t.Fatalf("invalid external event reached provider: %d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, label := range []string{"X", strings.Repeat("A", 48), "AL1C3", "Another-Bot_29", "RAVEL-12"} {
		s, db, _, service, _ := lifecycleFixture(t)
		s.Lifecycle.PublicDeliveryOrigin = externalFixtureOrigin
		in := externalEventInput("accepted")
		in.Event.Public.Backroom = label
		raw, _ := json.Marshal(in)
		w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
		if w.Code != 201 || !strings.HasPrefix(db.post.Targets[0].Content.Text, "SPAWN | External Backroom ") {
			t.Fatalf("generic label %q rejected or impersonated native work: %d", label, w.Code)
		}
	}
}

func TestExternalLifecycleRejectsForeignAndNoncanonicalDeliveryURLs(t *testing.T) {
	path := "/deliveries/delivery_" + strings.Repeat("d", 32)
	for _, url := range []string{"", "http://spawn.example.com" + path, "https://evil.example.com" + path, externalFixtureOrigin + ".evil.example.com" + path, externalFixtureOrigin + path + "?x=1", externalFixtureOrigin + path + "#video", externalFixtureOrigin + "/deliveries/%64elivery_" + strings.Repeat("d", 32), externalFixtureOrigin + strings.ToUpper(path), externalFixtureOrigin + path + "/", "https://user@spawn.example.com" + path, externalFixtureOrigin + ":443" + path, externalFixtureOrigin + "/deliveries/../deliveries/delivery_" + strings.Repeat("d", 32)} {
		s, db, adapter, service, _ := lifecycleFixture(t)
		s.Lifecycle.PublicDeliveryOrigin = externalFixtureOrigin
		in := externalEventInput("settled")
		in.Event.Public.PublicDeliveryURL = &url
		in.Parent = &lifecycleParent{EventID: externalEventInput("accepted").Event.EventID, RequestHash: strings.Repeat("a", 64)}
		raw, _ := json.Marshal(in)
		w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
		if w.Code != 400 || db.created != 0 || adapter.caps != 0 {
			t.Fatalf("unsafe URL %q admitted: %d %s", url, w.Code, w.Body.String())
		}
	}
}

func TestExternalLifecycleSchemaIsolationIncludesNullFields(t *testing.T) {
	s, db, _, service, _ := lifecycleFixture(t)
	s.Lifecycle.PublicDeliveryOrigin = externalFixtureOrigin
	native, _ := json.Marshal(eventInput("accepted"))
	external, _ := json.Marshal(externalEventInput("accepted"))
	for _, raw := range []string{
		strings.Replace(string(native), `"public":{`, `"public":{"currency":null,`, 1),
		strings.Replace(string(native), `"public":{`, `"public":{"public_delivery_url":null,`, 1),
		strings.Replace(string(external), `"public":{`, `"public":{"budget_cap_micro_usd":null,`, 1),
		strings.Replace(string(external), `"public":{`, `"public":{"actual_spent_micro_usd":null,`, 1),
		strings.Replace(string(external), `"public":{`, `"public":{"public_delivery_url":null,`, 1),
		strings.Replace(string(external), `"public":{`, `"public":{"brief":"private",`, 1),
	} {
		w := req(s.Handler(), "POST", "/v1/lifecycle/events", raw, service)
		if w.Code != 400 || db.created != 0 {
			t.Fatal("cross-schema or private field admitted", w.Code, w.Body.String())
		}
	}
}

func TestExternalLifecycleKeepsExpiryIdempotencyAndCancellation(t *testing.T) {
	s, db, adapter, service, _ := lifecycleFixture(t)
	s.Lifecycle.PublicDeliveryOrigin = externalFixtureOrigin
	local := &lifecycleCancelStore{apiStore: db}
	s.Store = local
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	in := externalEventInput("accepted")
	in.ExpiresAtMS = now.Add(10 * time.Second).UnixMilli()
	raw, _ := json.Marshal(in)
	if w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service); w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	caps := adapter.caps
	now = now.Add(11 * time.Second)
	// A duplicate original envelope resolves its existing admission before
	// expiry or provider work; it is not a new send or a recovery strategy.
	w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
	if w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" || db.created != 1 || adapter.caps != caps {
		t.Fatal("expired original receipt repeated provider admission", w.Code, w.Body.String())
	}
	path := "/v1/lifecycle/receipts/" + in.Event.EventID + "/cancel?request_hash=" + security.Hash(string(raw))
	w = req(s.Handler(), "POST", path, "{}", service)
	if w.Code != 200 || local.cancelled != 1 || db.created != 1 || adapter.caps != caps {
		t.Fatal("external queued receipt cancellation changed", w.Code, w.Body.String())
	}
	w = req(s.Handler(), "POST", strings.Replace(path, security.Hash(string(raw)), strings.Repeat("e", 64), 1), "{}", service)
	if w.Code == 200 || local.cancelled != 1 {
		t.Fatal("wrong request hash cancelled external receipt")
	}
	db.post = nil
	w = req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service)
	if w.Code != 400 || db.created != 1 || adapter.caps != caps {
		t.Fatal("expired unadmitted external event reached provider", w.Code, w.Body.String())
	}
}

func TestExternalLifecycleCompletionBindsAcceptanceAndOriginalProof(t *testing.T) {
	for _, changed := range []string{"quote", "currency", "label", "contract", "consent", "event", "request hash", "missing parent", "missing URL", "manual proof", "unsafe proof"} {
		t.Run(changed, func(t *testing.T) {
			s, db, adapter, service, _ := lifecycleFixture(t)
			s.Lifecycle.PublicDeliveryOrigin = externalFixtureOrigin
			in := externalEventInput("accepted")
			raw, _ := json.Marshal(in)
			if w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(raw), service); w.Code != 201 {
				t.Fatal(w.Body.String())
			}
			root := externalConfirmedParent(t, db)
			next := externalEventInput("settled")
			next.Parent = &lifecycleParent{EventID: in.Event.EventID, RequestHash: security.Hash(string(raw))}
			switch changed {
			case "quote":
				value := "15001"
				next.Event.Public.QuotedAmountMicroUnits = &value
			case "currency":
				value := "USDC"
				next.Event.Public.Currency = &value
			case "label":
				next.Event.Public.Backroom = "Different Studio"
			case "contract":
				next.Event.Public.Contract = "contract_" + strings.Repeat("e", 32)
			case "consent":
				next.Event.ConsentHash = "sha256:" + strings.Repeat("e", 64)
				next.Event.EventID = lifecycleEventID(next.Event.ConsentHash, "settled", next.Event.Public.Schema)
			case "event":
				next.Parent.EventID = "sha256:" + strings.Repeat("e", 64)
			case "request hash":
				next.Parent.RequestHash = strings.Repeat("e", 64)
			case "missing parent":
				next.Parent = nil
			case "missing URL":
				next.Event.Public.PublicDeliveryURL = nil
			case "manual proof":
				root.Targets[0].Progress = model.Progress{Stage: "manual_resolution"}
			case "unsafe proof":
				root.Targets[0].Progress.Unsafe = true
			}
			db.post = &root
			s.Store = &threadAPIStore{apiStore: db, parent: root}
			caps := adapter.caps
			nextRaw, _ := json.Marshal(next)
			w := req(s.Handler(), "POST", "/v1/lifecycle/events", string(nextRaw), service)
			if w.Code != 409 && w.Code != 400 || db.created != 1 || adapter.caps != caps {
				t.Fatalf("changed acceptance or unproved parent admitted: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestExternalLifecycleSharedContractVectors(t *testing.T) {
	raw, err := os.ReadFile("../../docs/external-lifecycle-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Origin  string `json:"origin"`
		Vectors []struct {
			Name        string           `json:"name"`
			Request     lifecycleRequest `json:"request"`
			RequestJSON string           `json:"request_json"`
			RequestHash string           `json:"request_hash"`
			Text        string           `json:"text"`
		} `json:"vectors"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			raw, err := json.Marshal(vector.Request)
			e := vector.Request.Event
			if err != nil || string(raw) != vector.RequestJSON || security.Hash(string(raw)) != vector.RequestHash || lifecycleText(e) != vector.Text || lifecycleEventID(e.ConsentHash, e.Public.Stage, e.Public.Schema) != e.EventID {
				t.Fatal("shared producer/publisher vector differs", string(raw))
			}
			s := &Server{Lifecycle: &LifecycleGrant{PublicDeliveryOrigin: fixture.Origin}}
			if !s.lifecyclePublicAllowed(e.Public) {
				t.Fatal("shared vector failed public validation")
			}
		})
	}
}
