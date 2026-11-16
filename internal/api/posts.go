package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
)

func (s *Server) createPost(w http.ResponseWriter, r *http.Request) {
	var in model.CreatePostRequest
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	s.createPostRequest(w, r, in)
}
func (s *Server) createPostRequest(w http.ResponseWriter, r *http.Request, in model.CreatePostRequest) {
	key := r.Header.Get("Idempotency-Key")
	if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) < 1 || len(key) > 128 || !safeKey(key) {
		writeError(w, 400, "idempotency_key_required", "Send a unique Idempotency-Key of 1–128 letters, digits, dots, colons, underscores or hyphens.")
		return
	}
	encoded, err := json.Marshal(in)
	if err != nil {
		writeError(w, 400, "invalid_request", "Invalid post content.")
		return
	}
	hash := security.Hash(string(encoded))
	if value, ok := r.Context().Value(lifecycleHashKey{}).(string); ok {
		hash = value
	}
	principal := owner(r)
	existing, err := s.Store.LookupIdempotency(r.Context(), principal, key, hash)
	if err == nil {
		w.Header().Set("Idempotency-Replayed", "true")
		writeJSON(w, 200, existing)
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	if len(in.Targets) < 1 || len(in.Targets) > 3 || len(in.Text) > 32000 || len(in.Media) > 35 {
		writeError(w, 400, "invalid_post", "Choose one to three destinations and a supported amount of content.")
		return
	}
	now := s.now()
	when := now
	if in.PublishAt != nil {
		when = in.PublishAt.UTC()
		if when.Before(now) || when.After(now.AddDate(1, 0, 0)) {
			writeError(w, 400, "invalid_schedule", "publish_at must be a future RFC3339 instant within one year; omit it to publish now.")
			return
		}
	}
	if err = validTimezone(in.Timezone); err != nil {
		writeError(w, 400, "invalid_timezone", err.Error())
		return
	}
	for _, m := range in.Media {
		if s.Media.Validate(m) != nil {
			writeError(w, 400, "invalid_media", "Use supported media metadata and an approved HTTPS asset host.")
			return
		}
	}
	seenAccounts := map[string]bool{}
	seenPlatforms := map[model.Platform]bool{}
	targets := make([]model.Target, 0, len(in.Targets))
	accounts := make([]model.Account, 0, len(in.Targets))
	for _, t := range in.Targets {
		if t.AccountID == "" || len(t.AccountID) > 128 || seenAccounts[t.AccountID] {
			writeError(w, 400, "duplicate_destination", "Choose each account once.")
			return
		}
		seenAccounts[t.AccountID] = true
		a, err := s.Store.GetAccount(r.Context(), principal, t.AccountID)
		if err != nil {
			s.fail(w, err)
			return
		}
		if a.Status != "active" {
			writeError(w, 409, "account_unavailable", "Reconnect the destination before creating a post.")
			return
		}
		if _, ok := s.Adapters[a.Platform]; !ok {
			writeError(w, 503, "provider_unavailable", "A selected platform is not configured.")
			return
		}
		if seenPlatforms[a.Platform] {
			writeError(w, 400, "duplicate_platform", "Choose one account per platform for each post.")
			return
		}
		seenPlatforms[a.Platform] = true
		content := model.Content{Text: in.Text, Media: in.Media, Options: t.Options}
		if t.PublishBefore != nil {
			deadline := t.PublishBefore.UTC()
			if !deadline.After(when) || deadline.After(now.Add(24*time.Hour)) {
				writeError(w, 400, "invalid_publication_window", "The bounded publication window must end after publication and within 24 hours.")
				return
			}
			content.PublishBefore = &deadline
		}
		if t.ReplyToPostID != "" {
			if a.Platform != model.X || len(t.ReplyToPostID) > 128 || !safeKey(t.ReplyToPostID) {
				writeError(w, 400, "invalid_thread_parent", "Choose a confirmed local X post for this account.")
				return
			}
			parent, parentErr := s.Store.GetPost(r.Context(), principal, t.ReplyToPostID)
			if parentErr != nil {
				writeError(w, 409, "x_thread_parent_unavailable", "The parent must be a confirmed post owned by this account.")
				return
			}
			content.Reply, parentErr = model.ConfirmedXReply(parent, a)
			if parentErr != nil {
				writeError(w, 409, "x_thread_parent_unavailable", "The parent must be a confirmed post owned by this account.")
				return
			}
		}
		if t.Text != nil {
			content.Text = *t.Text
		}
		if t.Media != nil {
			content.Media = *t.Media
		}
		if len(content.Text) > 32000 || len(content.Media) > 35 || len(content.Options) > 16384 {
			writeError(w, 400, "invalid_content", "Destination content exceeds the request limits.")
			return
		}
		if strings.TrimSpace(content.Text) == "" && len(content.Media) == 0 {
			writeError(w, 400, "empty_content", "Add text or media for every destination.")
			return
		}
		for _, m := range content.Media {
			if s.Media.Validate(m) != nil {
				writeError(w, 400, "invalid_media", "Use supported media metadata and an approved HTTPS asset host.")
				return
			}
		}
		id, err := security.RandomToken(18)
		if err != nil {
			s.fail(w, err)
			return
		}
		targets = append(targets, model.Target{ID: "target_" + id, AccountID: a.ID, Platform: a.Platform, Content: content, Status: "queued", NextAttemptAt: when})
		accounts = append(accounts, a)
	}
	// All ownership/media checks precede any provider reads. Validate every
	// destination before committing any work to the durable queue.
	for i, a := range accounts {
		err := func() error {
			releaseOperation, err := s.admitProvider(r.Context(), principal, a.Platform, a.Identity.ID)
			if err != nil {
				return err
			}
			defer releaseOperation()
			lockctx, release, err := s.Store.AcquireAccount(r.Context(), a.ID)
			if err != nil {
				return err
			}
			defer release()
			tok, err := s.Tokens.Usable(lockctx, a, false)
			if err != nil {
				return err
			}
			adapter := s.Adapters[a.Platform]
			caps, err := adapter.Capabilities(lockctx, tok)
			if err != nil {
				return err
			}
			return adapter.Validate(targets[i].Content, caps)
		}()
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	id, err := security.RandomToken(18)
	if err != nil {
		s.fail(w, err)
		return
	}
	post := model.Post{ID: "post_" + id, Owner: principal, Status: "queued", PublishAt: when, Timezone: in.Timezone, Version: 1, CreatedAt: now, Targets: targets}
	for i := range post.Targets {
		post.Targets[i].PostID = post.ID
	}
	saved, created, err := s.Store.CreatePost(r.Context(), post, key, hash)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	w.Header().Set("Location", "/v1/posts/"+saved.ID)
	writeJSON(w, status, saved)
}
func safeKey(v string) bool {
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:-", c)) {
			return false
		}
	}
	return true
}

// Read-only recovery of admission whose acknowledgement was lost. It never
// queues or retries a send and requires the original owner/key/content digest.
func (s *Server) getPostReceipt(w http.ResponseWriter, r *http.Request) {
	key, digest := r.PathValue("key"), r.URL.Query().Get("request_hash")
	if len(key) < 1 || len(key) > 128 || !safeKey(key) || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		writeError(w, 400, "invalid_receipt_lookup", "Use the original idempotency key and request digest.")
		return
	}
	post, err := s.Store.LookupIdempotency(r.Context(), owner(r), key, digest)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, post)
}
func validTimezone(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > 100 || v == "Local" {
		return errors.New("Use a named IANA timezone such as Africa/Lagos or America/Chicago.")
	}
	if _, err := time.LoadLocation(v); err != nil {
		return errors.New("Use a named IANA timezone such as Africa/Lagos or America/Chicago.")
	}
	return nil
}
func (s *Server) getPost(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetPost(r.Context(), owner(r), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, p)
}
func (s *Server) listPosts(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, 400, "invalid_limit", "limit must be between 1 and 100.")
			return
		}
		limit = n
	}
	posts, err := s.Store.ListPosts(r.Context(), owner(r), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	if posts == nil {
		posts = []model.Post{}
	}
	writeJSON(w, 200, map[string]any{"posts": posts})
}
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if len(q["after_id"]) > 1 || len(q["limit"]) > 1 {
		writeError(w, 400, "invalid_pagination", "Send one after_id and one limit value.")
		return
	}
	var after int64
	if v := q.Get("after_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, 400, "invalid_cursor", "after_id must be a nonnegative event ID.")
			return
		}
		after = n
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, 400, "invalid_limit", "limit must be between 1 and 200.")
			return
		}
		limit = n
	}
	page, err := s.Store.ListEventsPage(r.Context(), owner(r), r.PathValue("id"), after, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	if page.Events == nil {
		page.Events = []model.Event{}
	}
	writeJSON(w, 200, page)
}
func (s *Server) cancelPost(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	p, err := s.Store.CancelPost(r.Context(), owner(r), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, p)
}
func (s *Server) reschedulePost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PublishAt time.Time `json:"publish_at"`
		Timezone  string    `json:"timezone"`
		Version   int64     `json:"version"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if in.Version < 1 || !in.PublishAt.After(s.now()) || in.PublishAt.After(s.now().AddDate(1, 0, 0)) || validTimezone(in.Timezone) != nil {
		writeError(w, 400, "invalid_schedule", "Send the current version, a future RFC3339 instant within one year, and a valid IANA timezone.")
		return
	}
	p, err := s.Store.ReschedulePost(r.Context(), owner(r), r.PathValue("id"), in.Version, in.PublishAt.UTC(), in.Timezone)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, p)
}
func (s *Server) retryTarget(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	p, err := s.Store.RetryTarget(r.Context(), owner(r), r.PathValue("id"), r.PathValue("target"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, p)
}
func (s *Server) resolveTarget(w http.ResponseWriter, r *http.Request) {
	var in store.Resolution
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if len(strings.TrimSpace(in.Note)) < 5 || len(in.Note) > 1000 || (in.Action != "confirm_published" && in.Action != "confirm_not_published" && in.Action != "resume_processing") {
		writeError(w, 400, "invalid_resolution", "Choose confirm_published, confirm_not_published or resume_processing and provide a note describing the recovery decision.")
		return
	}
	if in.Action != "confirm_published" && (in.RemoteID != "" || in.RemoteURL != "") {
		writeError(w, 400, "invalid_resolution", "A confirmed unpublished result cannot include a publication receipt.")
		return
	}
	if in.Action == "confirm_published" {
		if in.RemoteID == "" || len(in.RemoteID) > 256 || strings.ContainsAny(in.RemoteID, "\x00\r\n") {
			writeError(w, 400, "invalid_receipt", "A valid remote ID is required.")
			return
		}
		if in.RemoteURL != "" {
			u, err := url.Parse(in.RemoteURL)
			if err != nil || len(in.RemoteURL) > 2048 || u.Scheme != "https" || u.User != nil || u.Fragment != "" || !receiptHost(u.Hostname()) {
				writeError(w, 400, "invalid_receipt", "Use a platform HTTPS permalink for the receipt.")
				return
			}
		}
	}
	p, err := s.Store.ResolveTarget(r.Context(), owner(r), r.PathValue("id"), r.PathValue("target"), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, p)
}
func receiptHost(host string) bool {
	switch strings.ToLower(host) {
	case "x.com", "www.x.com", "twitter.com", "www.twitter.com", "instagram.com", "www.instagram.com", "tiktok.com", "www.tiktok.com":
		return true
	}
	return false
}
