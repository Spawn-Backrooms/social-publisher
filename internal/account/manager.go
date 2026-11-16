// Package account manages encrypted credentials. Callers must hold the store's
// per-account lock while obtaining and using a token for a provider operation.
package account

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
)

type Manager struct {
	Store    store.Store
	Vault    *security.Vault
	Adapters provider.Registry
	Now      func() time.Time
	Logger   *slog.Logger
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Usable rereads the current credential under the caller's account lock. A
// failed compare-and-swap never returns the newly obtained token to the caller.
func (m *Manager) Usable(ctx context.Context, a model.Account, force bool) (model.Token, error) {
	if err := ctx.Err(); err != nil {
		return model.Token{}, err
	}
	a, err := m.Store.GetWorkerAccount(ctx, a.ID)
	if err != nil {
		return model.Token{}, err
	}
	if a.Status != "active" {
		return model.Token{}, &provider.Error{Code: "account_disconnected", Message: "Reconnect the account before publishing.", AuthExpired: true}
	}
	if a.TokenRefreshVersion != 0 {
		// An acquired exclusive guard cannot inherit a live refresh. Its prior
		// owner either died or lost the lock/session; the provider may already
		// have rotated the stored grant. Do this before even decrypting it,
		// including a forced refresh of an otherwise unexpired access token.
		return model.Token{}, m.uncertainRefresh(ctx, a)
	}
	raw, err := m.Vault.Open(a.EncryptedToken, a.TokenAAD())
	if err != nil {
		return model.Token{}, errors.New("credential decryption failed")
	}
	var tok model.Token
	if err = json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return model.Token{}, errors.New("stored credential invalid")
	}
	now := m.now()
	if !force && tok.ExpiresAt.After(now.Add(2*time.Minute)) {
		return tok, nil
	}
	adapter, ok := m.Adapters[a.Platform]
	if !ok {
		return model.Token{}, errors.New("provider not configured")
	}
	if err = m.Store.BeginTokenRefresh(ctx, a.ID, a.TokenVersion); err != nil {
		// A lost acknowledgement may still have committed. Leave that durable
		// intent for the next guard holder; no remote call has happened here.
		return model.Token{}, err
	}
	if err = ctx.Err(); err != nil {
		// Cancellation before invoking the adapter is direct evidence that
		// this attempt did not reach the provider.
		if clearErr := m.clearRefresh(ctx, a); clearErr != nil && !errors.Is(clearErr, store.ErrConflict) {
			m.log("credential_refresh_clear_failed", "account_id", a.ID)
		}
		return model.Token{}, err
	}
	fresh, err := adapter.Refresh(ctx, tok)
	if err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) && pe.AuthExpired && !pe.Ambiguous {
			_ = m.uncertainRefresh(ctx, a)
			return model.Token{}, err
		}
		if pe != nil && pe.RefreshNotApplied && !pe.Ambiguous {
			// Retryable alone says nothing about rotation. Only explicit proof
			// of rejection/no dispatch permits reuse of the old credential.
			if clearErr := m.clearRefresh(ctx, a); clearErr != nil {
				return model.Token{}, clearErr
			}
			return model.Token{}, err
		}
		return model.Token{}, m.uncertainRefresh(ctx, a)
	}
	if fresh.AccessToken == "" || !fresh.ExpiresAt.After(m.now()) {
		return model.Token{}, m.uncertainRefresh(ctx, a)
	}
	if fresh.ProviderUserID != "" && fresh.ProviderUserID != a.Identity.ID {
		return model.Token{}, m.uncertainRefresh(ctx, a)
	}
	// Adapters resolve documented omission rules before returning success.
	// Never resurrect an old refresh grant from an explicitly empty replacement.
	if fresh.IssuedAt.IsZero() {
		fresh.IssuedAt = now
	}
	if fresh.ProviderUserID == "" {
		fresh.ProviderUserID = tok.ProviderUserID
	}
	raw, err = json.Marshal(fresh)
	if err != nil {
		_ = m.uncertainRefresh(ctx, a)
		return model.Token{}, err
	}
	blob, err := m.Vault.Seal(raw, a.TokenAAD())
	if err != nil {
		_ = m.uncertainRefresh(ctx, a)
		return model.Token{}, err
	}
	if err = m.Store.SaveToken(ctx, a.ID, a.TokenVersion, blob, fresh.ExpiresAt); err != nil {
		// The provider may already have rotated the grant. If the fenced save
		// did not commit, prevent reuse of the old credential. A newer version
		// (including a save whose response was lost) cannot be quarantined by
		// this old version, and a disconnected account remains disconnected.
		if markErr := m.quarantine(ctx, a); markErr != nil && !errors.Is(markErr, store.ErrConflict) {
			m.log("credential_quarantine_failed", "account_id", a.ID)
		}
		return model.Token{}, err
	}
	if err = ctx.Err(); err != nil {
		return model.Token{}, err
	}
	return fresh, nil
}

func (m *Manager) uncertainRefresh(ctx context.Context, a model.Account) error {
	if err := m.quarantine(ctx, a); err != nil && !errors.Is(err, store.ErrConflict) {
		// If quarantine cannot commit, the unresolved durable intent still
		// prevents a later guard holder from returning or refreshing the token.
		m.log("credential_quarantine_failed", "account_id", a.ID)
	}
	return &provider.Error{Code: "credential_refresh_uncertain", Message: "Reconnect the account because its refreshed credentials could not be confirmed.", AuthExpired: true}
}

func (m *Manager) clearRefresh(ctx context.Context, a model.Account) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return m.Store.ClearTokenRefresh(cleanup, a.ID, a.TokenVersion)
}

func (m *Manager) quarantine(ctx context.Context, a model.Account) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return m.Store.MarkReauth(cleanup, a.ID, a.TokenVersion)
}

// RunMaintenance refreshes credentials even when their next post is weeks away.
// Refresh token expiry/revocation still requires the owner to reconnect.
func (m *Manager) RunMaintenance(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		m.RefreshDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (m *Manager) RefreshDue(ctx context.Context) {
	for _, platform := range []model.Platform{model.TikTok, model.X, model.Instagram} {
		if _, ok := m.Adapters[platform]; !ok {
			continue
		}
		margin := 10 * time.Minute
		if platform == model.Instagram {
			margin = 48 * time.Hour
		}
		accounts, err := m.Store.ListRefreshCandidates(ctx, platform, m.now().Add(margin), 200)
		if err != nil {
			m.log("credential_scan_failed", "platform", platform)
			continue
		}
		for _, a := range accounts {
			if ctx.Err() != nil {
				return
			}
			func() {
				opctx, cancel := context.WithTimeout(ctx, 90*time.Second)
				defer cancel()
				lockctx, release, err := m.Store.TryAcquireAccount(opctx, a.ID)
				if err != nil {
					if !errors.Is(err, store.ErrAccountBusy) {
						m.log("credential_lock_failed", "account_id", a.ID)
					}
					return
				}
				defer release()
				current, err := m.Store.GetWorkerAccount(lockctx, a.ID)
				if err != nil || current.Status != "active" || (current.TokenRefreshVersion == 0 && current.TokenExpiresAt.After(m.now().Add(margin))) {
					return
				}
				if _, err = m.Usable(lockctx, current, true); err != nil {
					m.log("credential_refresh_failed", "account_id", a.ID)
				}
			}()
		}
	}
}
func (m *Manager) log(message string, args ...any) {
	if m.Logger != nil {
		m.Logger.Warn(message, args...)
	}
}
