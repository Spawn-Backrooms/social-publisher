package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/store"
)

type fixtureAdmission struct {
	owner   model.Principal
	account string
}

// SQL admission behavior is exercised against the real store separately. This
// local boundary lets HTTP tests prove authentication, refusal, cleanup, and
// account-lock ordering without an external database.
func (s *apiStore) AdmitRequest(_ context.Context, owner model.Principal, limits store.AdmissionLimits) (store.AdmissionLease, error) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.requestAdmissionError != nil {
		return store.AdmissionLease{}, s.requestAdmissionError
	}
	if limits.RequestsPerMinute == 0 {
		limits.RequestsPerMinute = 60
	}
	if limits.OwnerConcurrency == 0 {
		limits.OwnerConcurrency = 4
	}
	if s.requestUsage == nil {
		s.requestUsage = make(map[model.Principal]int)
	}
	if s.admissions == nil {
		s.admissions = make(map[string]fixtureAdmission)
	}
	if s.requestUsage[owner] >= limits.RequestsPerMinute {
		return store.AdmissionLease{}, &store.AdmissionError{Reason: "request_rate", RetryAfter: time.Minute}
	}
	active := 0
	for _, a := range s.admissions {
		if a.owner == owner && a.account == "" {
			active++
		}
	}
	if active >= limits.OwnerConcurrency {
		return store.AdmissionLease{}, &store.AdmissionError{Reason: "owner_concurrency", RetryAfter: 95 * time.Second}
	}
	s.requestUsage[owner]++
	s.admissionSequence++
	token := fmt.Sprintf("request-%d", s.admissionSequence)
	s.admissions[token] = fixtureAdmission{owner: owner}
	return store.AdmissionLease{Token: token, ExpiresAt: time.Now().Add(95 * time.Second)}, nil
}

func (s *apiStore) AdmitProviderOperation(_ context.Context, owner model.Principal, platform model.Platform, providerID string, limits store.AdmissionLimits) (store.AdmissionLease, error) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.providerAdmissionError != nil {
		return store.AdmissionLease{}, s.providerAdmissionError
	}
	if limits.AccountConcurrency == 0 {
		limits.AccountConcurrency = 1
	}
	if s.admissions == nil {
		s.admissions = make(map[string]fixtureAdmission)
	}
	canonical := string(platform) + "\x00" + providerID
	active := 0
	for _, a := range s.admissions {
		if a.account == canonical {
			active++
		}
	}
	if active >= limits.AccountConcurrency {
		return store.AdmissionLease{}, &store.AdmissionError{Reason: "account_concurrency", RetryAfter: 95 * time.Second}
	}
	s.admissionSequence++
	token := fmt.Sprintf("provider-%d", s.admissionSequence)
	s.admissions[token] = fixtureAdmission{owner: owner, account: canonical}
	return store.AdmissionLease{Token: token, ExpiresAt: time.Now().Add(95 * time.Second)}, nil
}

func (s *apiStore) ReleaseAdmission(_ context.Context, owner model.Principal, token string) error {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if a, ok := s.admissions[token]; ok && a.owner == owner {
		delete(s.admissions, token)
	}
	return nil
}

func TestAuthenticatedOwnerRequestBudgetDoesNotBlockAnotherOwner(t *testing.T) {
	s, st, _, token := fixtureAPI(t)
	h := s.Handler()
	for i := 0; i < 60; i++ {
		if got := req(h, "GET", "/v1/platforms", "", token); got.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i+1, got.Code, got.Body.String())
		}
	}
	limited := req(h, "GET", "/v1/platforms", "", token)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("exhausted owner: %d retry-after=%q body=%s", limited.Code, limited.Header().Get("Retry-After"), limited.Body.String())
	}
	other, _ := s.JWT.Mint(model.Principal{TenantID: st.owner.TenantID, Subject: "another-owner"}, time.Hour, time.Now())
	if got := req(h, "GET", "/v1/platforms", "", other); got.Code != http.StatusOK {
		t.Fatalf("another owner inherited exhausted budget: %d %s", got.Code, got.Body.String())
	}
}

type blockingOwnerStore struct {
	*apiStore
	entered chan struct{}
	resume  chan struct{}
}

func (s *blockingOwnerStore) ListAccounts(ctx context.Context, _ model.Principal) ([]model.Account, error) {
	s.entered <- struct{}{}
	select {
	case <-s.resume:
		return []model.Account{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestConcurrentOwnerRequestsAreBoundedWithoutBlockingAnotherOwner(t *testing.T) {
	s, base, _, token := fixtureAPI(t)
	st := &blockingOwnerStore{apiStore: base, entered: make(chan struct{}, 4), resume: make(chan struct{})}
	s.Store = st
	h := s.Handler()
	done := make(chan int, 4)
	defer func() {
		close(st.resume)
		for range 4 {
			<-done
		}
	}()
	for range 4 {
		go func() { done <- req(h, "GET", "/v1/accounts", "", token).Code }()
	}
	for range 4 {
		select {
		case <-st.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("admitted request did not reach handler")
		}
	}
	limited := req(h, "GET", "/v1/platforms", "", token)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("fifth concurrent owner request: %d %s", limited.Code, limited.Body.String())
	}
	other, _ := s.JWT.Mint(model.Principal{TenantID: base.owner.TenantID, Subject: "another-owner"}, time.Hour, time.Now())
	if got := req(h, "GET", "/v1/platforms", "", other); got.Code != http.StatusOK {
		t.Fatalf("another owner inherited concurrency usage: %d %s", got.Code, got.Body.String())
	}
}

type blockedAccountStore struct {
	*apiStore
	entered chan struct{}
	resume  chan struct{}
	calls   atomic.Int32
}

func (s *blockedAccountStore) AcquireAccount(ctx context.Context, _ string) (context.Context, func(), error) {
	if s.calls.Add(1) > 1 {
		return nil, nil, errors.New("second request reached busy account lock")
	}
	close(s.entered)
	select {
	case <-s.resume:
		return ctx, func() {}, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func TestProviderAdmissionRejectsBeforeWaitingOnAccountLock(t *testing.T) {
	s, base, _, token := fixtureAPI(t)
	st := &blockedAccountStore{apiStore: base, entered: make(chan struct{}), resume: make(chan struct{})}
	s.Store = st
	h := s.Handler()
	done := make(chan int, 1)
	defer func() { close(st.resume); <-done }()
	go func() { done <- req(h, "GET", "/v1/accounts/acct-x/capabilities", "", token).Code }()
	select {
	case <-st.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not acquire account admission")
	}
	got := req(h, "GET", "/v1/accounts/acct-x/capabilities", "", token)
	if got.Code != http.StatusTooManyRequests || st.calls.Load() != 1 {
		t.Fatalf("busy account bypassed admission: status=%d account-lock calls=%d body=%s", got.Code, st.calls.Load(), got.Body.String())
	}
}

func TestAdmissionDatabaseFailureFailsClosed(t *testing.T) {
	for _, providerFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("provider_%t", providerFailure), func(t *testing.T) {
			s, st, ad, token := fixtureAPI(t)
			if providerFailure {
				st.providerAdmissionError = errors.New("database unavailable")
			} else {
				st.requestAdmissionError = errors.New("database unavailable")
			}
			got := req(s.Handler(), "GET", "/v1/accounts/acct-x/capabilities", "", token)
			if got.Code != http.StatusServiceUnavailable || ad.caps != 0 || got.Header().Get("Retry-After") == "" {
				t.Fatalf("failed admission proceeded: status=%d caps=%d body=%s", got.Code, ad.caps, got.Body.String())
			}
		})
	}
}

func TestAdmissionReleaseAllowsNextProviderRequest(t *testing.T) {
	s, _, ad, token := fixtureAPI(t)
	h := s.Handler()
	for range 3 {
		got := req(h, "GET", "/v1/accounts/acct-x/capabilities", "", token)
		if got.Code != http.StatusOK {
			t.Fatalf("completed request retained permit: %d %s", got.Code, got.Body.String())
		}
	}
	if ad.caps != 3 {
		t.Fatalf("provider calls=%d, want 3", ad.caps)
	}
}

func TestBrowserBoundOAuthCallbackUsesItsOwnersBudget(t *testing.T) {
	s, st, _, token := fixtureAPI(t)
	s.AdmissionLimits.RequestsPerMinute = 1
	h := s.Handler()
	start := req(h, "POST", "/v1/connections/x/authorize", `{}`, token)
	if start.Code != http.StatusOK {
		t.Fatalf("authorize: %d %s", start.Code, start.Body.String())
	}
	var authorization struct {
		URL string `json:"authorization_url"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &authorization); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(authorization.URL)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/oauth/x/callback?state="+url.QueryEscape(u.Query().Get("state"))+"&code=fixture-once", nil)
	r.AddCookie(start.Result().Cookies()[0])
	other, _ := s.JWT.Mint(model.Principal{TenantID: st.owner.TenantID, Subject: "another-owner"}, time.Hour, time.Now())
	r.Header.Set("Authorization", "Bearer "+other)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests || st.exchanges != 0 {
		t.Fatalf("callback bypassed bound owner's budget: status=%d exchanges=%d body=%s", w.Code, st.exchanges, w.Body.String())
	}
}
