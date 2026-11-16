package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"socialpublisher/internal/account"
	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
)

// This queue deliberately allows initial same-account claim races. The real
// worker must handle those races without waiting behind the provider call.
type coordinatedQueue struct {
	store.Store
	mu        sync.Mutex
	targets   []model.Target
	accounts  map[string]model.Account
	locks     map[string]chan struct{}
	finished  map[string]store.Finish
	bFinished chan struct{}
}

func (s *coordinatedQueue) Claim(context.Context, time.Duration) (model.Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.targets) == 0 {
		return model.Target{}, store.ErrNoWork
	}
	t := s.targets[0]
	s.targets = s.targets[1:]
	return t, nil
}
func (s *coordinatedQueue) AcquireAccount(ctx context.Context, id string) (context.Context, func(), error) {
	select {
	case s.locks[id] <- struct{}{}:
		return ctx, func() { <-s.locks[id] }, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}
func (s *coordinatedQueue) TryAcquireAccount(ctx context.Context, id string) (context.Context, func(), error) {
	select {
	case s.locks[id] <- struct{}{}:
		return ctx, func() { <-s.locks[id] }, nil
	default:
		return nil, nil, store.ErrAccountBusy
	}
}
func (s *coordinatedQueue) GetWorkerAccount(_ context.Context, id string) (model.Account, error) {
	return s.accounts[id], nil
}
func (s *coordinatedQueue) Heartbeat(ctx context.Context, _, _ string, _ time.Duration) error {
	return ctx.Err()
}
func (s *coordinatedQueue) SaveProgress(ctx context.Context, _, _ string, _ model.Progress) error {
	return ctx.Err()
}
func (s *coordinatedQueue) ReserveProviderRequest(ctx context.Context, _, _ string, _ time.Duration) (time.Duration, error) {
	return 0, ctx.Err()
}
func (s *coordinatedQueue) SetProviderCooldown(ctx context.Context, _, _ string, delay time.Duration) (time.Duration, error) {
	return delay, ctx.Err()
}
func (s *coordinatedQueue) FinishTarget(_ context.Context, id, _ string, f store.Finish) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished[id] = f
	if id == "b" && f.Status == "published" {
		close(s.bFinished)
	}
	return nil
}

type blockedAccountAdapter struct {
	provider.Adapter
	aStarted chan struct{}
	aRelease chan struct{}
	started  sync.Once
}

func (p *blockedAccountAdapter) Capabilities(context.Context, model.Token) (model.Capabilities, error) {
	return model.Capabilities{CanPublish: true}, nil
}
func (p *blockedAccountAdapter) Validate(model.Content, model.Capabilities) error { return nil }
func (p *blockedAccountAdapter) Publish(ctx context.Context, token model.Token, _ model.Content, _ model.Progress, checkpoint provider.Checkpoint) (model.Outcome, error) {
	if token.ProviderUserID == "a" {
		p.started.Do(func() { close(p.aStarted) })
		select {
		case <-p.aRelease:
		case <-ctx.Done():
			return model.Outcome{}, ctx.Err()
		}
	}
	if err := checkpoint(model.Progress{Unsafe: true}); err != nil {
		return model.Outcome{}, err
	}
	return model.Outcome{Done: true, RemoteID: "receipt-" + token.ProviderUserID}, nil
}

func TestBusyAccountCannotOccupyEveryWorker(t *testing.T) {
	now := time.Now()
	vault, err := security.NewVault(map[string][]byte{"test": make([]byte, 32)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	s := &coordinatedQueue{accounts: map[string]model.Account{}, locks: map[string]chan struct{}{}, finished: map[string]store.Finish{}, bFinished: make(chan struct{})}
	for _, id := range []string{"a", "b"} {
		a := model.Account{ID: id, Platform: model.X, Owner: model.Principal{TenantID: "tenant", Subject: id}, Identity: model.Identity{ID: id}, Status: "active"}
		blob, _ := json.Marshal(model.Token{AccessToken: "fixture-" + id, ProviderUserID: id, ExpiresAt: now.Add(time.Hour)})
		a.EncryptedToken, err = vault.Seal(blob, a.TokenAAD())
		if err != nil {
			t.Fatal(err)
		}
		s.accounts[id], s.locks[id] = a, make(chan struct{}, 1)
	}
	for i := 0; i < 6; i++ {
		s.targets = append(s.targets, model.Target{ID: fmt.Sprintf("a-%d", i), PostID: fmt.Sprintf("post-a-%d", i), AccountID: "a", Platform: model.X, LeaseToken: "lease", Status: "running"})
	}
	s.targets = append(s.targets, model.Target{ID: "b", PostID: "post-b", AccountID: "b", Platform: model.X, LeaseToken: "lease", Status: "running"})
	p := &blockedAccountAdapter{aStarted: make(chan struct{}), aRelease: make(chan struct{})}
	registry := provider.Registry{model.X: p}
	e := &Engine{Store: s, Tokens: &account.Manager{Store: s, Vault: vault, Adapters: registry}, Adapters: registry, Concurrency: 6, PollInterval: 5 * time.Millisecond, LeaseDuration: time.Minute, JobTimeout: time.Minute}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); close(p.aRelease); <-done }()
	select {
	case <-p.aStarted:
	case <-time.After(time.Second):
		t.Fatal("blocked provider operation never started")
	}
	select {
	case <-s.bFinished:
	case <-time.After(time.Second):
		t.Fatal("unrelated account B did not finish while account A was still blocked")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, f := range s.finished {
		if id != "b" && (f.Status == "published" || f.CountFailure) {
			t.Fatalf("busy admission changed delivery outcome: %s %+v", id, f)
		}
	}
}

func TestRateLimitsDoNotExhaustFailuresOrRenewProcessingWindow(t *testing.T) {
	e, s, p := setupEngine(t)
	started := e.now().Add(-23 * time.Hour)
	s.target.ProcessingStartedAt = &started
	s.target.Progress = model.Progress{Stage: "polling", Data: json.RawMessage(`{"publish_id":"same-operation"}`)}
	s.target.Failures = 4
	p.fn = func(model.Progress, provider.Checkpoint) (model.Outcome, error) {
		return model.Outcome{}, &provider.Error{Code: "rate_limit_exceeded", Retryable: true, RateLimited: true, RetryAfter: time.Minute}
	}
	for i := 0; i < 8; i++ {
		s.claim = false
		if err := e.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if s.finished == nil || s.finished.Status != "processing" || s.finished.CountFailure || string(s.finished.Progress.Data) != `{"publish_id":"same-operation"}` {
			t.Fatalf("throttling exhausted delivery failures or lost operation at attempt %d: %+v", i, s.finished)
		}
	}
	expired := e.now().Add(-24 * time.Hour)
	s.target.ProcessingStartedAt = &expired
	s.claim = false
	calls := p.calls
	if err := e.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.finished.Status != "needs_attention" || p.calls != calls {
		t.Fatal("repeated throttling extended the fixed 24-hour processing limit")
	}
}

func TestPacingFailureCannotCrossLostAccountGuard(t *testing.T) {
	e, s, p := setupEngine(t)
	s.lockCanceled = true
	if err := e.RunOnce(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost lock guard did not cancel work: %v", err)
	}
	if p.calls != 0 || s.finished != nil {
		t.Fatal("lost account guard reached provider or changed target")
	}
}

func TestProcessingDeadlineUsesDatabaseClaimTimeDespiteHostSkew(t *testing.T) {
	for _, skew := range []time.Duration{-48 * time.Hour, 48 * time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			e, s, p := setupEngine(t)
			dbNow := e.now()
			started := dbNow.Add(-time.Hour)
			s.target.ProcessingStartedAt = &started
			s.target.ClaimedAt = dbNow
			s.target.Progress = model.Progress{Stage: "polling"}
			e.Now = func() time.Time { return dbNow.Add(skew) }
			if err := e.RunOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if p.calls != 1 || s.finished.Status != "published" {
				t.Fatal("host clock rejected a valid database processing window")
			}
			expired := dbNow.Add(-24 * time.Hour)
			s.target.ProcessingStartedAt = &expired
			s.claim = false
			if err := e.RunOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if p.calls != 1 || s.finished.Status != "needs_attention" {
				t.Fatal("host clock bypassed the expired database processing window")
			}
		})
	}
}

func TestWorkerReportsCompletedNoWorkSweep(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	progress := make(chan struct{})
	e := &Engine{Store: &coordinatedQueue{}, Concurrency: 1, OnProgress: func() { close(progress); cancel() }}
	e.Run(ctx)
	select {
	case <-progress:
	default:
		t.Fatal("completed empty queue sweep was not reported to readiness")
	}
}
