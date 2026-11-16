package account

import (
	"context"
	"encoding/json"
	"errors"
	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
	"testing"
	"time"
)

type tokenStore struct {
	store.Store
	account        model.Account
	saved          int
	mark           bool
	refuse         bool
	pendingVersion int64
	beginErr       error
	clearErr       error
	markErr        error
}

func (s *tokenStore) GetWorkerAccount(context.Context, string) (model.Account, error) {
	s.account.TokenRefreshVersion = s.pendingVersion
	return s.account, nil
}
func (s *tokenStore) BeginTokenRefresh(ctx context.Context, _ string, v int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.beginErr != nil {
		return s.beginErr
	}
	if s.account.Status != "active" || s.account.TokenVersion != v || s.pendingVersion != 0 {
		return store.ErrConflict
	}
	s.pendingVersion = v
	return nil
}
func (s *tokenStore) ClearTokenRefresh(ctx context.Context, _ string, v int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.clearErr != nil {
		return s.clearErr
	}
	if s.account.Status != "active" || s.account.TokenVersion != v || s.pendingVersion != v {
		return store.ErrConflict
	}
	s.pendingVersion = 0
	s.account.TokenVersion++
	return nil
}
func (s *tokenStore) SaveToken(ctx context.Context, _ string, v int64, b []byte, exp time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.refuse || s.account.Status != "active" || v != s.account.TokenVersion || s.pendingVersion != v {
		return store.ErrConflict
	}
	s.account.EncryptedToken = b
	s.account.TokenVersion++
	s.account.TokenExpiresAt = exp
	s.pendingVersion = 0
	s.saved++
	return nil
}
func (s *tokenStore) MarkReauth(ctx context.Context, _ string, v int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.markErr != nil {
		return s.markErr
	}
	if v != s.account.TokenVersion || s.account.Status != "active" {
		return store.ErrConflict
	}
	s.mark = true
	s.account.Status = "reauth_required"
	s.account.TokenVersion++
	s.pendingVersion = 0
	return nil
}

type refreshAdapter struct {
	provider.Adapter
	token        model.Token
	err          error
	calls        int
	afterRefresh func()
}

func (a *refreshAdapter) Refresh(context.Context, model.Token) (model.Token, error) {
	a.calls++
	if a.afterRefresh != nil {
		a.afterRefresh()
	}
	return a.token, a.err
}

func setupTokenManager(t *testing.T) (*Manager, *tokenStore, *refreshAdapter) {
	t.Helper()
	vault, err := security.NewVault(map[string][]byte{"one": make([]byte, 32)}, "one")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	a := model.Account{ID: "acct", Owner: model.Principal{TenantID: "tenant", Subject: "user"}, Platform: model.X, Identity: model.Identity{ID: "remote"}, Status: "active", TokenVersion: 1}
	tok := model.Token{AccessToken: "old", RefreshToken: "refresh1", ExpiresAt: now.Add(-time.Minute), IssuedAt: now.Add(-time.Hour)}
	raw, _ := json.Marshal(tok)
	a.EncryptedToken, err = vault.Seal(raw, a.TokenAAD())
	if err != nil {
		t.Fatal(err)
	}
	s := &tokenStore{account: a}
	p := &refreshAdapter{token: model.Token{AccessToken: "new", RefreshToken: "refresh2", ExpiresAt: now.Add(time.Hour), IssuedAt: now}}
	return &Manager{Store: s, Vault: vault, Adapters: provider.Registry{model.X: p}, Now: func() time.Time { return now }}, s, p
}
func TestRefreshPersistsRotatedCredentials(t *testing.T) {
	m, s, p := setupTokenManager(t)
	tok, err := m.Usable(context.Background(), s.account, false)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "new" || p.calls != 1 || s.saved != 1 {
		t.Fatalf("refresh was not persisted: %#v %d %d", tok, p.calls, s.saved)
	}
	b, err := m.Vault.Open(s.account.EncryptedToken, s.account.TokenAAD())
	if err != nil {
		t.Fatal(err)
	}
	var saved model.Token
	if json.Unmarshal(b, &saved) != nil || saved.RefreshToken != "refresh2" {
		t.Fatal("rotated refresh lost")
	}
}

func TestRefreshIntentExistsBeforeRemoteCall(t *testing.T) {
	m, s, p := setupTokenManager(t)
	p.afterRefresh = func() {
		if s.pendingVersion != 1 {
			t.Fatal("provider refresh began without durable intent for the old token version")
		}
	}
	if _, err := m.Usable(t.Context(), s.account, true); err != nil {
		t.Fatal(err)
	}
	if s.pendingVersion != 0 || s.saved != 1 {
		t.Fatal("replacement token did not clear the intent atomically")
	}
}

func TestRefreshCannotCallProviderBeforeIntentAcknowledgement(t *testing.T) {
	m, s, p := setupTokenManager(t)
	s.beginErr = errors.New("intent persistence unavailable")
	if _, err := m.Usable(t.Context(), s.account, true); !errors.Is(err, s.beginErr) {
		t.Fatalf("intent persistence error = %v", err)
	}
	if p.calls != 0 || s.saved != 0 {
		t.Fatal("remote refresh happened without acknowledged durable intent")
	}
}

func TestInvalidRefreshSuccessQuarantinesOldCredential(t *testing.T) {
	for _, kind := range []string{"empty", "expired", "identity"} {
		t.Run(kind, func(t *testing.T) {
			m, s, p := setupTokenManager(t)
			switch kind {
			case "empty":
				p.token.AccessToken = ""
			case "expired":
				p.token.ExpiresAt = m.Now().Add(-time.Second)
			case "identity":
				p.token.ProviderUserID = "different-user"
			}
			if _, err := m.Usable(t.Context(), s.account, true); err == nil {
				t.Fatal("invalid refreshed credential accepted")
			}
			if !s.mark || s.saved != 0 {
				t.Fatal("invalid success left a potentially rotated old credential active")
			}
			_, _ = m.Usable(t.Context(), s.account, true)
			if p.calls != 1 {
				t.Fatal("old refresh credential replayed after invalid success")
			}
		})
	}
}

func TestUnclassifiedRefreshErrorCannotProveNoRotation(t *testing.T) {
	m, s, p := setupTokenManager(t)
	p.err = errors.New("lost response after dispatch")
	if _, err := m.Usable(t.Context(), s.account, true); err == nil {
		t.Fatal("unclassified refresh error accepted")
	}
	if !s.mark {
		t.Fatal("unclassified refresh outcome left old credential active")
	}
}
func TestDisconnectedAccountCannotRefresh(t *testing.T) {
	m, s, p := setupTokenManager(t)
	s.account.Status = "disconnected"
	_, err := m.Usable(context.Background(), s.account, false)
	if err == nil || p.calls != 0 {
		t.Fatal("disconnected account refreshed")
	}
}
func TestConcurrentDisconnectWinsTokenCAS(t *testing.T) {
	m, s, _ := setupTokenManager(t)
	s.refuse = true
	_, err := m.Usable(context.Background(), s.account, false)
	if !errors.Is(err, store.ErrConflict) || s.saved != 0 {
		t.Fatal("stale credentials escaped after CAS rejection")
	}
}
func TestInvalidRefreshRequiresReconnection(t *testing.T) {
	m, s, p := setupTokenManager(t)
	p.err = &provider.Error{Code: "invalid_grant", AuthExpired: true}
	_, err := m.Usable(context.Background(), s.account, false)
	if err == nil || !s.mark {
		t.Fatal("invalid grant was not disabled")
	}
}
func TestTransientRefreshDoesNotDisableConnection(t *testing.T) {
	m, s, p := setupTokenManager(t)
	p.err = &provider.Error{Code: "rate_limit", Retryable: true, RefreshNotApplied: true}
	_, err := m.Usable(context.Background(), s.account, false)
	if err == nil || s.mark {
		t.Fatal("temporary error disabled connection")
	}
}
func TestValidCredentialDoesNotRefresh(t *testing.T) {
	m, s, p := setupTokenManager(t)
	p.token.ExpiresAt = m.Now().Add(2 * time.Hour)
	b, _ := json.Marshal(p.token)
	s.account.EncryptedToken, _ = m.Vault.Seal(b, s.account.TokenAAD())
	tok, err := m.Usable(context.Background(), s.account, false)
	if err != nil || tok.AccessToken != "new" || p.calls != 0 {
		t.Fatal("valid token unexpectedly refreshed")
	}
}
func TestExplicitlyEmptyRefreshedGrantsAreNotRestored(t *testing.T) {
	m, s, p := setupTokenManager(t)
	old := model.Token{AccessToken: "old", RefreshToken: "r", ExpiresAt: m.Now().Add(-time.Hour), Scopes: []string{"tweet.write"}}
	b, _ := json.Marshal(old)
	s.account.EncryptedToken, _ = m.Vault.Seal(b, s.account.TokenAAD())
	p.token.Scopes = []string{}
	fresh, err := m.Usable(context.Background(), s.account, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Scopes) != 0 {
		t.Fatal("withdrawn scopes were locally resurrected")
	}
}

func TestAmbiguousRefreshStopsReuseOfPossiblyRotatedCredential(t *testing.T) {
	m, s, p := setupTokenManager(t)
	p.err = &provider.Error{Code: "refresh_outcome_unknown", Ambiguous: true}
	if _, err := m.Usable(context.Background(), s.account, true); err == nil {
		t.Fatal("ambiguous refresh accepted")
	}
	if !s.mark {
		t.Fatal("ambiguous refresh was not quarantined")
	}
	if _, err := m.Usable(context.Background(), s.account, true); err == nil {
		t.Fatal("quarantined connection reused")
	}
	if p.calls != 1 {
		t.Fatal("possibly rotated refresh credential was sent again")
	}
}

func TestCancelledAmbiguousRefreshStillQuarantinesCredential(t *testing.T) {
	m, s, p := setupTokenManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.afterRefresh = cancel
	p.err = &provider.Error{Code: "refresh_outcome_unknown", Ambiguous: true}
	if _, err := m.Usable(ctx, s.account, true); err == nil {
		t.Fatal("cancelled uncertain refresh accepted")
	}
	if !s.mark || s.account.Status != "reauth_required" {
		t.Fatal("request cancellation prevented credential quarantine")
	}
}

func TestCancelledRefreshPersistenceQuarantinesOldGrant(t *testing.T) {
	m, s, p := setupTokenManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.afterRefresh = cancel
	if _, err := m.Usable(ctx, s.account, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled credential persistence: %v", err)
	}
	if !s.mark || s.saved != 0 {
		t.Fatal("old grant remained usable after refreshed credential was not saved")
	}
}

type refreshAckLostStore struct {
	*tokenStore
	loseBegin bool
}

var errRefreshAckLost = errors.New("fixture lost database acknowledgement")

func (s refreshAckLostStore) BeginTokenRefresh(ctx context.Context, id string, v int64) error {
	if err := s.tokenStore.BeginTokenRefresh(ctx, id, v); err != nil {
		return err
	}
	if s.loseBegin {
		return errRefreshAckLost
	}
	return nil
}

func (s refreshAckLostStore) SaveToken(ctx context.Context, id string, v int64, b []byte, expires time.Time) error {
	if err := s.tokenStore.SaveToken(ctx, id, v, b, expires); err != nil {
		return err
	}
	return errRefreshAckLost
}

func TestLostRefreshSaveAcknowledgementPreservesNewToken(t *testing.T) {
	m, s, p := setupTokenManager(t)
	m.Store = refreshAckLostStore{tokenStore: s}
	if _, err := m.Usable(t.Context(), s.account, true); !errors.Is(err, errRefreshAckLost) {
		t.Fatalf("lost persistence acknowledgement: %v", err)
	}
	if s.mark || s.account.Status != "active" || s.account.TokenVersion != 2 || s.pendingVersion != 0 {
		t.Fatal("old refresh attempt disabled the newer committed token")
	}
	m.Store = s
	tok, err := m.Usable(t.Context(), s.account, false)
	if err != nil || tok.AccessToken != "new" || p.calls != 1 {
		t.Fatal("newly saved token was not reusable after acknowledgement loss")
	}
}

func TestLostIntentAcknowledgementPreventsDispatchAndReuse(t *testing.T) {
	m, s, p := setupTokenManager(t)
	m.Store = refreshAckLostStore{tokenStore: s, loseBegin: true}
	if _, err := m.Usable(t.Context(), s.account, true); !errors.Is(err, errRefreshAckLost) {
		t.Fatalf("lost intent acknowledgement: %v", err)
	}
	if p.calls != 0 {
		t.Fatal("provider called after intent acknowledgement was lost")
	}
	m.Store = s
	if _, err := m.Usable(t.Context(), s.account, false); err == nil || !s.mark || p.calls != 0 {
		t.Fatal("unresolved intent was reused instead of quarantined")
	}
}

func TestRefreshCompletionCannotOverwriteReconnectOrDisconnect(t *testing.T) {
	for _, status := range []string{"active", "disconnected"} {
		t.Run(status, func(t *testing.T) {
			m, s, p := setupTokenManager(t)
			p.afterRefresh = func() {
				// A newer authorization/disconnect wins even if the old guard
				// was lost while its provider request was still in flight.
				s.account.Status = status
				s.account.TokenVersion = 2
				s.account.EncryptedToken = []byte("newer-owner-state")
				s.pendingVersion = 0
			}
			if _, err := m.Usable(t.Context(), s.account, true); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("stale refresh completion: %v", err)
			}
			if s.account.Status != status || s.account.TokenVersion != 2 || string(s.account.EncryptedToken) != "newer-owner-state" || s.mark {
				t.Fatal("stale refresh completion or quarantine changed newer owner state")
			}
		})
	}
}

type busyMaintenanceStore struct{ *tokenStore }

func (s busyMaintenanceStore) ListRefreshCandidates(context.Context, model.Platform, time.Time, int) ([]model.Account, error) {
	return []model.Account{{ID: "busy"}, s.account}, nil
}

func (s busyMaintenanceStore) AcquireAccount(ctx context.Context, _ string) (context.Context, func(), error) {
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

func (s busyMaintenanceStore) TryAcquireAccount(ctx context.Context, id string) (context.Context, func(), error) {
	if id == "busy" {
		return nil, nil, store.ErrAccountBusy
	}
	return ctx, func() {}, nil
}

func TestRefreshMaintenanceRecoversUnexpiredIntentBeyondBusyAccount(t *testing.T) {
	m, s, p := setupTokenManager(t)
	s.account.TokenExpiresAt = m.Now().Add(30 * 24 * time.Hour)
	s.pendingVersion = 1
	m.Store = busyMaintenanceStore{tokenStore: s}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	m.RefreshDue(ctx)
	if !s.mark || p.calls != 0 {
		t.Fatal("busy account hid unresolved refresh intent for an unexpired token")
	}
}

func TestUnresolvedRefreshIntentBlocksUseWhenCleanupFails(t *testing.T) {
	for _, failure := range []string{"quarantine", "clear"} {
		t.Run(failure, func(t *testing.T) {
			m, s, p := setupTokenManager(t)
			if failure == "quarantine" {
				p.err = errors.New("response lost after provider rotation")
				s.markErr = errors.New("database unavailable during quarantine")
			} else {
				p.err = &provider.Error{Code: "rate_limit", Retryable: true, RefreshNotApplied: true}
				s.clearErr = errors.New("database unavailable during clear")
			}
			if _, err := m.Usable(t.Context(), s.account, true); err == nil {
				t.Fatal("failed refresh cleanup accepted")
			}
			if s.pendingVersion != 1 || s.account.Status != "active" {
				t.Fatal("failed cleanup erased the only durable evidence of refresh uncertainty")
			}
			// Even an otherwise usable access token must not escape a pending
			// intent. The next guard holder recovers it before decryption.
			s.account.EncryptedToken = []byte("must-not-be-decrypted")
			s.account.TokenExpiresAt = m.Now().Add(30 * 24 * time.Hour)
			var pe *provider.Error
			_, err := m.Usable(t.Context(), s.account, false)
			if !errors.As(err, &pe) || !pe.AuthExpired || p.calls != 1 {
				t.Fatal("unresolved intent allowed old credential use or refresh")
			}
		})
	}
}

func TestCancelledProvenRejectionCanRetainOldGrant(t *testing.T) {
	m, s, p := setupTokenManager(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.afterRefresh = cancel
	p.err = &provider.Error{Code: "rate_limit", Retryable: true, RefreshNotApplied: true}
	if _, err := m.Usable(ctx, s.account, true); err == nil {
		t.Fatal("provider rejection accepted")
	}
	if s.mark || s.pendingVersion != 0 || s.account.Status != "active" || s.account.TokenVersion != 2 {
		t.Fatal("request cancellation prevented safe rejection cleanup")
	}
	p.err, p.afterRefresh = nil, nil
	if _, err := m.Usable(t.Context(), s.account, true); err != nil || p.calls != 2 {
		t.Fatal("proven rejected grant was not available for a later controlled retry")
	}
}

func TestAmbiguityOverridesClaimOfNoRotation(t *testing.T) {
	m, s, p := setupTokenManager(t)
	p.err = &provider.Error{Code: "contradictory", Retryable: true, RefreshNotApplied: true, Ambiguous: true}
	_, _ = m.Usable(t.Context(), s.account, true)
	if !s.mark || s.account.Status != "reauth_required" {
		t.Fatal("ambiguous refresh was treated as proof of a rejected grant")
	}
}

func TestManagerDoesNotResurrectAbsentRefreshCredential(t *testing.T) {
	m, s, p := setupTokenManager(t)
	p.token.RefreshToken = ""
	fresh, err := m.Usable(t.Context(), s.account, true)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.RefreshToken != "" {
		t.Fatal("manager resurrected a previous refresh credential")
	}
	raw, err := m.Vault.Open(s.account.EncryptedToken, s.account.TokenAAD())
	if err != nil {
		t.Fatal(err)
	}
	var saved model.Token
	if err = json.Unmarshal(raw, &saved); err != nil || saved.RefreshToken != "" {
		t.Fatal("manager persisted a resurrected refresh credential")
	}
}
