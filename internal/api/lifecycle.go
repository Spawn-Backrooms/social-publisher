package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"socialpublisher/internal/model"
	"socialpublisher/internal/security"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// A dedicated service identity receives a narrow resource grant. It cannot use
// requester JWTs, OAuth, normal post routes, retry/resolve or arbitrary text.
// Owner is the account's stored custody scope, supplied only by server config.
type LifecycleGrant struct {
	JWT                       *security.JWT
	Owner                     model.Principal
	AccountID, ProviderUserID string
	PublicDeliveryOrigin      string
}

var LifecycleService = model.Principal{TenantID: "spawn-lifecycle-service", Subject: "spawn-lifecycle-consumer"}

type lifecycleHashKey struct{}
type lifecyclePublic struct {
	Title                  *string `json:"title,omitempty"`
	WorkURL                *string `json:"work_url,omitempty"`
	Schema                 string  `json:"schema"`
	Contract               string  `json:"public_contract_ref"`
	Backroom               string  `json:"backroom"`
	Stage                  string  `json:"stage"`
	BudgetCapMicroUSD      *string `json:"budget_cap_micro_usd,omitempty"`
	ActualSpentMicroUSD    *string `json:"actual_spent_micro_usd,omitempty"`
	Currency               *string `json:"currency,omitempty"`
	QuotedAmountMicroUnits *string `json:"quoted_amount_micro_units,omitempty"`
	PublicDeliveryURL      *string `json:"public_delivery_url,omitempty"`
}

// Keep the original native schemas closed even though this Go representation
// also carries the separate external-work schema. Presence (including null)
// matters: adding an external field must not silently alter a native envelope.
func (p *lifecyclePublic) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	var schema string
	if err := json.Unmarshal(fields["schema"], &schema); err != nil {
		return err
	}
	for name, value := range fields {
		switch name {
		case "schema", "public_contract_ref", "backroom", "stage":
		case "title", "work_url":
			if schema != characterLifecycleSchema || string(value) == "null" {
				return errors.New("character work fields require their reviewed schema")
			}
		case "budget_cap_micro_usd", "actual_spent_micro_usd":
			if schema == externalLifecycleSchema {
				return errors.New("native amounts are not external-work quotes")
			}
		case "currency", "quoted_amount_micro_units", "public_delivery_url":
			if schema != externalLifecycleSchema || string(value) == "null" {
				return errors.New("external-work fields require their reviewed schema and values")
			}
		default:
			return errors.New("unsupported lifecycle public field")
		}
	}
	type plain lifecyclePublic
	return json.Unmarshal(raw, (*plain)(p))
}

type lifecycleEvent struct {
	EventID      string          `json:"event_id"`
	AccountRef   string          `json:"account_ref"`
	ConsentHash  string          `json:"consent_hash"`
	EvidenceHash string          `json:"evidence_hash"`
	Sequence     int             `json:"sequence"`
	Public       lifecyclePublic `json:"public"`
}
type lifecycleParent struct {
	EventID     string `json:"event_id"`
	RequestHash string `json:"request_hash"`
}
type lifecycleRequest struct {
	Event       lifecycleEvent   `json:"event"`
	ExpiresAtMS int64            `json:"expires_at_ms"`
	Parent      *lifecycleParent `json:"parent,omitempty"`
}

var lifecycleDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var lifecycleRef = regexp.MustCompile(`^contract_[a-f0-9]{32}$`)
var lifecycleAmount = regexp.MustCompile(`^(0|[1-9][0-9]{0,29})$`)
var lifecycleBackrooms = map[string]bool{"VESPER-04": true, "RAVEL-12": true, "INDEX-09": true, "MOTH-07": true, "ECHO-19": true, "LATCH-03": true}
var externalLifecycleLabel = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9 _-]{0,46}[A-Za-z0-9])?$`)
var externalLifecycleDeliveryPath = regexp.MustCompile(`^/deliveries/delivery_[a-f0-9]{32}$`)

const characterLifecycleSchema = "spawn.character-work-lifecycle.v1"

var characterWorkPath = regexp.MustCompile(`^/works/[a-f0-9]{64}/[a-f0-9]{32}$`)

const externalLifecycleSchema = "spawn.external-work-lifecycle.v1"

func lifecycleEventID(consent, stage, schema string) string {
	domain := "spawn.platform-lifecycle-slot.v1"
	if schema == externalLifecycleSchema {
		domain = "spawn.external-work-lifecycle-slot.v1"
	} else if schema == characterLifecycleSchema {
		domain = "spawn.character-work-lifecycle-slot.v1"
	}
	// Go map keys are sorted, matching the producer's canonical compact JSON.
	slot, _ := json.Marshal(map[string]string{"domain": domain, "consent": consent, "stage": stage})
	return "sha256:" + security.Hash(string(slot))
}

func lifecycleText(e lifecycleEvent) string {
	if e.Public.Schema == characterLifecycleSchema {
		p := e.Public
		status := "Accepted. Up to " + lifecycleUSD(*p.BudgetCapMicroUSD) + "."
		if p.Stage == "settled" {
			status = "Completed. " + lifecycleUSD(*p.ActualSpentMicroUSD) + "."
		}
		return "SPAWN | " + p.Backroom + "\n" + *p.Title + " — " + status + "\n" + *p.WorkURL
	}

	if e.Public.Schema == externalLifecycleSchema {
		// Both declared currencies use six decimal places. This is an exact
		// denomination conversion, not a USD valuation or native inference cost.
		quote := strings.TrimPrefix(lifecycleUSD(*e.Public.QuotedAmountMicroUnits), "$") + " " + *e.Public.Currency
		status := "accepted. Budget quote " + quote + "."
		if e.Public.Stage == "settled" {
			status = "completed. Settlement recorded. Budget quote " + quote + ".\nPublic delivery: " + *e.Public.PublicDeliveryURL
		}
		return "SPAWN | External Backroom " + e.Public.Backroom + "\nContract " + e.Public.Contract + ": " + status
	}
	if e.Public.Schema == "spawn.backroom-lifecycle.v2" {
		cap := lifecycleUSD(*e.Public.BudgetCapMicroUSD)
		status := "accepted. Budget cap " + cap + "."
		if e.Public.Stage == "settled" {
			status = "completed and settled. Spent " + lifecycleUSD(*e.Public.ActualSpentMicroUSD) + " of " + cap + " budget cap."
		}
		return "SPAWN | Backroom " + e.Public.Backroom + "\nContract " + e.Public.Contract + ": " + status
	}
	status := "accepted."
	if e.Public.Stage == "settled" {
		status = "settled. Private delivery is tracked in SPAWN."
	} else if e.Public.Stage == "previously_completed" {
		status = "Previously completed. Private delivery and settlement verified in SPAWN."
	}
	return "SPAWN | Backroom " + e.Public.Backroom + "\nContract " + e.Public.Contract + ": " + status
}
func lifecycleUSD(value string) string {
	if len(value) < 7 {
		value = strings.Repeat("0", 7-len(value)) + value
	}
	fraction := strings.TrimRight(value[len(value)-6:], "0")
	result := "$" + value[:len(value)-6]
	if fraction != "" {
		result += "." + fraction
	}
	return result
}
func lifecyclePublicValid(p lifecyclePublic) bool {
	if p.Schema == characterLifecycleSchema {
		safe := func(s string, max int) bool {
			if strings.TrimSpace(s) != s || s == "" || utf8.RuneCountInString(s) > max {
				return false
			}
			for _, r := range s {
				if unicode.IsControl(r) {
					return false
				}
			}
			return true
		}
		if p.Title == nil || p.WorkURL == nil || !safe(*p.Title, 100) || !safe(p.Backroom, 80) || p.Currency != nil || p.QuotedAmountMicroUnits != nil || p.PublicDeliveryURL != nil || p.BudgetCapMicroUSD == nil || !lifecycleAmount.MatchString(*p.BudgetCapMicroUSD) || *p.BudgetCapMicroUSD == "0" {
			return false
		}
		if p.Stage == "accepted" {
			return p.ActualSpentMicroUSD == nil
		}
		if p.Stage != "settled" || p.ActualSpentMicroUSD == nil || !lifecycleAmount.MatchString(*p.ActualSpentMicroUSD) {
			return false
		}
		spent, _ := new(big.Int).SetString(*p.ActualSpentMicroUSD, 10)
		cap, _ := new(big.Int).SetString(*p.BudgetCapMicroUSD, 10)
		return spent.Cmp(cap) <= 0
	}
	if p.Title != nil || p.WorkURL != nil {
		return false
	}

	if p.Schema == externalLifecycleSchema {
		if p.BudgetCapMicroUSD != nil || p.ActualSpentMicroUSD != nil || p.Currency == nil || (*p.Currency != "USDC" && *p.Currency != "TEST_USD") || p.QuotedAmountMicroUnits == nil || !lifecycleAmount.MatchString(*p.QuotedAmountMicroUnits) || *p.QuotedAmountMicroUnits == "0" {
			return false
		}
		return p.Stage == "accepted" && p.PublicDeliveryURL == nil || p.Stage == "settled" && p.PublicDeliveryURL != nil
	}
	if p.Currency != nil || p.QuotedAmountMicroUnits != nil || p.PublicDeliveryURL != nil {
		return false
	}
	if p.Schema == "spawn.backroom-lifecycle.v1" {
		return p.BudgetCapMicroUSD == nil && p.ActualSpentMicroUSD == nil
	}
	if p.Schema != "spawn.backroom-lifecycle.v2" || p.BudgetCapMicroUSD == nil || !lifecycleAmount.MatchString(*p.BudgetCapMicroUSD) || *p.BudgetCapMicroUSD == "0" {
		return false
	}
	if p.Stage == "accepted" {
		return p.ActualSpentMicroUSD == nil
	}
	if p.Stage != "settled" || p.ActualSpentMicroUSD == nil || !lifecycleAmount.MatchString(*p.ActualSpentMicroUSD) {
		return false
	}
	spent, _ := new(big.Int).SetString(*p.ActualSpentMicroUSD, 10)
	cap, _ := new(big.Int).SetString(*p.BudgetCapMicroUSD, 10)
	return spent.Cmp(cap) <= 0
}

func (s *Server) lifecyclePublicAllowed(p lifecyclePublic) bool {
	if !lifecyclePublicValid(p) {
		return false
	}
	if p.Schema == characterLifecycleSchema {
		return s.Lifecycle != nil && security.ValidPublicDeliveryOrigin(s.Lifecycle.PublicDeliveryOrigin) && strings.HasPrefix(*p.WorkURL, s.Lifecycle.PublicDeliveryOrigin) && characterWorkPath.MatchString(strings.TrimPrefix(*p.WorkURL, s.Lifecycle.PublicDeliveryOrigin))
	}
	if p.Schema != externalLifecycleSchema {
		return lifecycleBackrooms[p.Backroom]
	}
	// No origin means no external admission, including acceptance. Never use
	// the publisher's own URL or an event-supplied domain as a fallback.
	if s.Lifecycle == nil || !security.ValidPublicDeliveryOrigin(s.Lifecycle.PublicDeliveryOrigin) || !externalLifecycleLabel.MatchString(p.Backroom) {
		return false
	}
	if p.Stage == "accepted" {
		return true
	}
	origin := s.Lifecycle.PublicDeliveryOrigin
	return strings.HasPrefix(*p.PublicDeliveryURL, origin) && externalLifecycleDeliveryPath.MatchString(strings.TrimPrefix(*p.PublicDeliveryURL, origin))
}
func lifecycleKey(id string) string { return "lifecycle." + strings.TrimPrefix(id, "sha256:") }
func (s *Server) lifecycleHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/lifecycle/events", s.createLifecycle)
	mux.HandleFunc("GET /v1/lifecycle/receipts/{key}", s.lifecycleReceipt)
	mux.HandleFunc("POST /v1/lifecycle/receipts/{key}/cancel", s.cancelLifecycle)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g := s.Lifecycle
		if g == nil || g.JWT == nil {
			writeError(w, 503, "lifecycle_disabled", "Lifecycle service is not configured.")
			return
		}
		h := r.Header.Values("Authorization")
		if len(h) != 1 || !strings.HasPrefix(h[0], "Bearer ") {
			writeError(w, 401, "unauthorized", "A lifecycle service token is required.")
			return
		}
		p, err := g.JWT.Verify(strings.TrimPrefix(h[0], "Bearer "), s.now())
		if err != nil || p != LifecycleService {
			writeError(w, 401, "unauthorized", "A lifecycle service token is required.")
			return
		}
		// Admit under the actual service identity, then apply this exact server grant.
		release, err := s.admitRequest(r.Context(), p)
		if err != nil {
			s.fail(w, err)
			return
		}
		defer release()
		a, err := s.Store.GetAccount(r.Context(), g.Owner, g.AccountID)
		if err != nil || a.Owner != g.Owner || a.Platform != model.X || a.Status != "active" || a.Identity.ID != g.ProviderUserID {
			writeError(w, 409, "lifecycle_account_changed", "The pinned central account is unavailable.")
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, g.Owner)))
	})
}
func (s *Server) createLifecycle(w http.ResponseWriter, r *http.Request) {
	// New character envelopes pin their exact bounded wire bytes. Re-marshalling
	// a Go struct changes JS key order/HTML escaping and breaks receipt recovery.
	// Retain historical hash semantics for existing native/external contracts.
	var received bytes.Buffer
	originalBody := r.Body
	defer originalBody.Close()
	r.Body = io.NopCloser(io.TeeReader(originalBody, &received))
	var in lifecycleRequest
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, 400, "invalid_lifecycle", err.Error())
		return
	}
	e := in.Event
	if !lifecycleDigest.MatchString(e.EventID) || !lifecycleDigest.MatchString(e.ConsentHash) || !lifecycleDigest.MatchString(e.EvidenceHash) || e.AccountRef != s.Lifecycle.AccountID || !s.lifecyclePublicAllowed(e.Public) || !lifecycleRef.MatchString(e.Public.Contract) || !(e.Public.Stage == "accepted" && e.Sequence == 1 && in.Parent == nil || e.Public.Stage == "settled" && e.Sequence == 3 && in.Parent != nil || e.Public.Stage == "previously_completed" && e.Sequence == 4 && in.Parent == nil) {
		writeError(w, 400, "invalid_lifecycle", "Use the reviewed lifecycle envelope and configured public origin.")
		return
	}
	if lifecycleEventID(e.ConsentHash, e.Public.Stage, e.Public.Schema) != e.EventID {
		writeError(w, 400, "invalid_lifecycle", "Semantic identity changed.")
		return
	}
	raw, _ := json.Marshal(in)
	if e.Public.Schema == characterLifecycleSchema {
		raw = received.Bytes()
	}
	digest := security.Hash(string(raw))
	deadline := time.UnixMilli(in.ExpiresAtMS).UTC()
	target := model.TargetRequest{AccountID: s.Lifecycle.AccountID, PublishBefore: &deadline}
	if in.Parent != nil {
		if !lifecycleDigest.MatchString(in.Parent.EventID) || !validReceiptHash(in.Parent.RequestHash) {
			writeError(w, 400, "invalid_lifecycle_parent", "Use the original accepted receipt.")
			return
		}
		parent, err := s.Store.LookupIdempotency(r.Context(), s.Lifecycle.Owner, lifecycleKey(in.Parent.EventID), in.Parent.RequestHash)
		a, aerr := s.Store.GetAccount(r.Context(), s.Lifecycle.Owner, s.Lifecycle.AccountID)
		if err != nil || aerr != nil {
			writeError(w, 409, "lifecycle_parent_unconfirmed", "Original publication is not confirmed.")
			return
		}
		accepted := e
		accepted.Public.Stage = "accepted"
		accepted.Public.ActualSpentMicroUSD = nil
		accepted.Public.PublicDeliveryURL = nil
		if in.Parent.EventID != lifecycleEventID(e.ConsentHash, "accepted", e.Public.Schema) || len(parent.Targets) != 1 || parent.Targets[0].Content.Text != lifecycleText(accepted) {
			writeError(w, 409, "lifecycle_parent_changed", "Thread correlation changed.")
			return
		}
		if _, err = model.ConfirmedXReply(parent, a); err != nil {
			writeError(w, 409, "lifecycle_parent_unconfirmed", "Original publication is not confirmed.")
			return
		}
		target.ReplyToPostID = parent.ID
	}
	// Receipt lookup in createPost precedes expiry validation and never resends.
	r.Header.Set("Idempotency-Key", lifecycleKey(e.EventID))
	r = r.WithContext(context.WithValue(r.Context(), lifecycleHashKey{}, digest))
	s.createPostRequest(w, r, model.CreatePostRequest{Text: lifecycleText(e), Targets: []model.TargetRequest{target}})
}
func validReceiptHash(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}
func (s *Server) lookupLifecycle(w http.ResponseWriter, r *http.Request) (model.Post, bool) {
	key, digest := r.PathValue("key"), r.URL.Query().Get("request_hash")
	if !lifecycleDigest.MatchString(key) || !validReceiptHash(digest) {
		writeError(w, 400, "invalid_receipt_lookup", "Use the original event and request hash.")
		return model.Post{}, false
	}
	post, err := s.Store.LookupIdempotency(r.Context(), s.Lifecycle.Owner, lifecycleKey(key), digest)
	if err != nil {
		s.fail(w, err)
		return model.Post{}, false
	}
	if len(post.Targets) != 1 || post.Targets[0].AccountID != s.Lifecycle.AccountID || post.Targets[0].Platform != model.X {
		writeError(w, 409, "lifecycle_receipt_changed", "Receipt account changed.")
		return model.Post{}, false
	}
	return post, true
}
func (s *Server) lifecycleReceipt(w http.ResponseWriter, r *http.Request) {
	post, ok := s.lookupLifecycle(w, r)
	if !ok {
		return
	}
	a, err := s.Store.GetAccount(r.Context(), s.Lifecycle.Owner, s.Lifecycle.AccountID)
	confirmed := false
	if err == nil {
		_, err = model.ConfirmedXReply(post, a)
		confirmed = err == nil
	}
	writeJSON(w, 200, map[string]any{"event_id": r.PathValue("key"), "request_hash": r.URL.Query().Get("request_hash"), "confirmed": confirmed, "post": post})
}
func (s *Server) cancelLifecycle(w http.ResponseWriter, r *http.Request) {
	post, ok := s.lookupLifecycle(w, r)
	if !ok {
		return
	}
	var input struct{}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	result, err := s.Store.CancelPost(r.Context(), s.Lifecycle.Owner, post.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}
