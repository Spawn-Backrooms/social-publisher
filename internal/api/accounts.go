package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/security"
)

func (s *Server) cookieName(state string) string {
	prefix := "sp_oauth_"
	if s.SecureCookies {
		prefix = "__Host-sp_oauth_"
	}
	return prefix + security.Hash(state)[:24]
}
func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	var empty struct{}
	if err := decodeJSON(w, r, &empty); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	p := model.Platform(r.PathValue("platform"))
	adapter, ok := s.Adapters[p]
	if !ok {
		writeError(w, 400, "provider_unavailable", "This platform is not configured.")
		return
	}
	state, err := security.RandomToken(32)
	if err != nil {
		s.fail(w, err)
		return
	}
	binding, err := security.RandomToken(32)
	if err != nil {
		s.fail(w, err)
		return
	}
	verifier, err := security.RandomToken(32)
	if err != nil {
		s.fail(w, err)
		return
	}
	hash := security.Hash(state)
	blob, err := s.Vault.Seal([]byte(verifier), "oauth:"+hash)
	if err != nil {
		s.fail(w, err)
		return
	}
	expires := s.now().Add(10 * time.Minute)
	if err = s.Store.PutOAuthState(r.Context(), model.OAuthState{StateHash: hash, CookieHash: security.Hash(binding), Owner: owner(r), Platform: p, EncryptedVerifier: blob, ExpiresAt: expires}); err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(state), Value: binding, Path: "/", HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: 600, Expires: expires})
	writeJSON(w, 200, map[string]any{"authorization_url": adapter.AuthorizationURL(state, security.Challenge(verifier)), "expires_at": expires})
}
func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	p := model.Platform(r.PathValue("platform"))
	adapter, ok := s.Adapters[p]
	if !ok {
		writeError(w, 400, "provider_unavailable", "This platform is not configured.")
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	if len(state) < 32 || len(state) > 128 || len(q["state"]) != 1 || len(q["code"]) > 1 {
		writeError(w, 400, "invalid_callback", "Restart the connection flow.")
		return
	}
	cookie, err := r.Cookie(s.cookieName(state))
	if err != nil || len(cookie.Value) < 32 || len(cookie.Value) > 128 {
		writeError(w, 400, "invalid_oauth_state", "Restart the connection flow in the same browser.")
		return
	}
	flow, err := s.Store.ConsumeOAuthState(r.Context(), security.Hash(state), security.Hash(cookie.Value), p)
	if err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(state), Value: "", Path: "/", HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	if q.Get("error") != "" {
		writeError(w, 400, "authorization_denied", "Connection was not authorized. You can start again.")
		return
	}
	code := q.Get("code")
	if code == "" || len(code) > 8192 {
		writeError(w, 400, "invalid_callback", "Restart the connection flow.")
		return
	}
	verifier, err := s.Vault.Open(flow.EncryptedVerifier, "oauth:"+flow.StateHash)
	if err != nil {
		s.fail(w, err)
		return
	}
	releaseRequest, err := s.admitRequest(r.Context(), flow.Owner)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer releaseRequest()
	token, err := adapter.Exchange(r.Context(), code, string(verifier))
	if err != nil {
		s.fail(w, err)
		return
	}
	if token.AccessToken == "" || !token.ExpiresAt.After(s.now()) {
		writeError(w, 502, "invalid_credential", "The platform did not return a usable access token.")
		return
	}
	identity, err := adapter.Identity(r.Context(), token)
	if err != nil {
		s.fail(w, err)
		return
	}
	if identity.ID == "" || len(identity.ID) > 256 || strings.ContainsAny(identity.ID, "\x00\r\n") {
		writeError(w, 502, "invalid_identity", "The platform did not return a valid account identity.")
		return
	}
	if token.ProviderUserID != "" && token.ProviderUserID != identity.ID {
		writeError(w, 502, "identity_mismatch", "The platform credential does not match its account.")
		return
	}
	releaseOperation, err := s.admitProvider(r.Context(), flow.Owner, p, identity.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer releaseOperation()
	lockctx, release, err := s.Store.AcquireIdentity(r.Context(), p, identity.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer release()
	r = r.WithContext(lockctx)
	// A disconnect may have revoked the grant while code exchange was running.
	// Verify under the same identity lock held through disconnect/revocation.
	checked, err := adapter.Identity(r.Context(), token)
	if err != nil {
		s.fail(w, err)
		return
	}
	if checked.ID != identity.ID {
		writeError(w, 502, "identity_mismatch", "The connected account changed during authorization.")
		return
	}
	identity = checked
	token.ProviderUserID = identity.ID
	if token.IssuedAt.IsZero() {
		token.IssuedAt = s.now()
	}
	id, err := security.RandomToken(18)
	if err != nil {
		s.fail(w, err)
		return
	}
	a := model.Account{ID: "acct_" + id, Owner: flow.Owner, Platform: p, Identity: identity, Status: "active", TokenExpiresAt: token.ExpiresAt, AuthorizationStartedAt: flow.IssuedAt}
	raw, err := json.Marshal(token)
	if err != nil {
		s.fail(w, err)
		return
	}
	a.EncryptedToken, err = s.Vault.Seal(raw, a.TokenAAD())
	if err != nil {
		s.fail(w, err)
		return
	}
	a, err = s.Store.UpsertAccount(r.Context(), a)
	if err != nil {
		s.fail(w, err)
		return
	}
	// JSON is intentionally sufficient for an API callback; a composer can poll
	// /v1/accounts and close its popup. No secret or arbitrary return URL is used.
	writeJSON(w, 200, map[string]any{"status": "connected", "account": a})
}
func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	a, err := s.Store.ListAccounts(r.Context(), owner(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	if a == nil {
		a = []model.Account{}
	}
	writeJSON(w, 200, map[string]any{"accounts": a})
}
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	a, err := s.Store.GetAccount(r.Context(), owner(r), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	releaseOperation, err := s.admitProvider(r.Context(), owner(r), a.Platform, a.Identity.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer releaseOperation()
	lockctx, release, err := s.Store.AcquireAccount(r.Context(), a.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer release()
	r = r.WithContext(lockctx)
	token, err := s.Tokens.Usable(r.Context(), a, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	adapter, ok := s.Adapters[a.Platform]
	if !ok {
		writeError(w, 503, "provider_unavailable", "This platform is not configured.")
		return
	}
	caps, err := adapter.Capabilities(r.Context(), token)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, caps)
}
func (s *Server) disconnect(w http.ResponseWriter, r *http.Request) {
	current, err := s.Store.GetAccount(r.Context(), owner(r), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	releaseOperation, err := s.admitProvider(r.Context(), owner(r), current.Platform, current.Identity.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer releaseOperation()
	lockctx, release, err := s.Store.AcquireAccount(r.Context(), current.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer release()
	r = r.WithContext(lockctx)
	a, err := s.Store.Disconnect(r.Context(), owner(r), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	remote := "not_attempted"
	if len(a.EncryptedToken) > 0 {
		blob, openErr := s.Vault.Open(a.EncryptedToken, a.TokenAAD())
		if openErr == nil {
			var token model.Token
			if json.Unmarshal(blob, &token) == nil {
				if adapter, ok := s.Adapters[a.Platform]; ok {
					if adapter.Revoke(r.Context(), token) == nil {
						remote = "revoked"
					} else {
						remote = "unconfirmed"
					}
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{"account_id": a.ID, "status": "disconnected", "provider_revocation": remote, "message": "Local credentials were removed and pending deliveries stopped. Already submitted posts may still complete."})
}
