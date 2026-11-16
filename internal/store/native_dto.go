package store

import (
	"socialpublisher/internal/model"
	"time"
)

// These DTOs belong exclusively to the private storage bridge. Public model
// encoding intentionally omits ownership, ciphertext and worker checkpoints.
type nativeAccount struct {
	model.Account
	Owner                  model.Principal `json:"owner"`
	EncryptedToken         []byte          `json:"encrypted_token"`
	TokenVersion           int64           `json:"token_version"`
	TokenRefreshVersion    int64           `json:"token_refresh_version"`
	TokenRefreshStartedAt  time.Time       `json:"token_refresh_started_at"`
	AuthorizationStartedAt time.Time       `json:"authorization_started_at"`
}

func accountToNative(a model.Account) nativeAccount {
	return nativeAccount{a, a.Owner, a.EncryptedToken, a.TokenVersion, a.TokenRefreshVersion, a.TokenRefreshStartedAt, a.AuthorizationStartedAt}
}
func (a nativeAccount) model() model.Account {
	v := a.Account
	v.Owner, v.EncryptedToken, v.TokenVersion = a.Owner, a.EncryptedToken, a.TokenVersion
	v.TokenRefreshVersion, v.TokenRefreshStartedAt, v.AuthorizationStartedAt = a.TokenRefreshVersion, a.TokenRefreshStartedAt, a.AuthorizationStartedAt
	return v
}

type nativeTarget struct {
	model.Target
	Progress   model.Progress `json:"progress"`
	LeaseToken string         `json:"lease_token"`
	LeaseUntil *time.Time     `json:"lease_until"`
	ClaimedAt  time.Time      `json:"claimed_at"`
}

func targetToNative(t model.Target) nativeTarget {
	return nativeTarget{t, t.Progress, t.LeaseToken, t.LeaseUntil, t.ClaimedAt}
}
func (t nativeTarget) model() model.Target {
	v := t.Target
	v.Progress, v.LeaseToken, v.LeaseUntil, v.ClaimedAt = t.Progress, t.LeaseToken, t.LeaseUntil, t.ClaimedAt
	return v
}

type nativePost struct {
	model.Post
	Owner   model.Principal `json:"owner"`
	Targets []nativeTarget  `json:"targets"`
}

func postToNative(p model.Post) nativePost {
	targets := make([]nativeTarget, len(p.Targets))
	for i, t := range p.Targets {
		targets[i] = targetToNative(t)
	}
	return nativePost{p, p.Owner, targets}
}
func (p nativePost) model() model.Post {
	v := p.Post
	v.Owner = p.Owner
	v.Targets = make([]model.Target, len(p.Targets))
	for i, t := range p.Targets {
		v.Targets[i] = t.model()
	}
	return v
}

type nativeOAuthState struct {
	StateHash         string          `json:"state_hash"`
	CookieHash        string          `json:"cookie_hash"`
	Owner             model.Principal `json:"owner"`
	Platform          model.Platform  `json:"platform"`
	EncryptedVerifier []byte          `json:"encrypted_verifier"`
	IssuedAt          time.Time       `json:"issued_at"`
	ExpiresAt         time.Time       `json:"expires_at"`
}

func oauthToNative(s model.OAuthState) nativeOAuthState {
	return nativeOAuthState{s.StateHash, s.CookieHash, s.Owner, s.Platform, s.EncryptedVerifier, s.IssuedAt, s.ExpiresAt}
}
func (s nativeOAuthState) model() model.OAuthState {
	return model.OAuthState{StateHash: s.StateHash, CookieHash: s.CookieHash, Owner: s.Owner, Platform: s.Platform, EncryptedVerifier: s.EncryptedVerifier, IssuedAt: s.IssuedAt, ExpiresAt: s.ExpiresAt}
}
