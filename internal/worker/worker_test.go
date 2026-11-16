package worker

import (
	"context"
	"encoding/json"
	"errors"
	"socialpublisher/internal/account"
	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
	"testing"
	"time"
)

type fixtureStore struct {
	store.Store
	target       model.Target
	account      model.Account
	claim        bool
	finished     *store.Finish
	lost         bool
	checkpoints  []model.Progress
	lockCanceled bool
}

func (s *fixtureStore) Claim(context.Context, time.Duration) (model.Target, error) {
	if s.claim {
		return model.Target{}, store.ErrNoWork
	}
	s.claim = true
	return s.target, nil
}
func (s *fixtureStore) AcquireAccount(ctx context.Context, _ string) (context.Context, func(), error) {
	if s.lockCanceled {
		guard, cancel := context.WithCancel(ctx)
		cancel()
		return guard, cancel, nil
	}
	return ctx, func() {}, nil
}
func (s *fixtureStore) TryAcquireAccount(ctx context.Context, id string) (context.Context, func(), error) {
	return s.AcquireAccount(ctx, id)
}
func (s *fixtureStore) ReserveProviderRequest(ctx context.Context, _, _ string, _ time.Duration) (time.Duration, error) {
	return 0, ctx.Err()
}
func (s *fixtureStore) SetProviderCooldown(ctx context.Context, _, _ string, delay time.Duration) (time.Duration, error) {
	return delay, ctx.Err()
}
func (s *fixtureStore) GetWorkerAccount(context.Context, string) (model.Account, error) {
	return s.account, nil
}
func (s *fixtureStore) Heartbeat(ctx context.Context, _, _ string, _ time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.lost {
		return store.ErrLeaseLost
	}
	return nil
}
func (s *fixtureStore) SaveProgress(_ context.Context, _, _ string, p model.Progress) error {
	if s.lost {
		return store.ErrLeaseLost
	}
	s.checkpoints = append(s.checkpoints, p)
	return nil
}
func (s *fixtureStore) FinishTarget(_ context.Context, _, _ string, f store.Finish) error {
	if s.lost {
		return store.ErrLeaseLost
	}
	s.finished = &f
	return nil
}

type fixtureAdapter struct {
	provider.Adapter
	fn         func(model.Progress, provider.Checkpoint) (model.Outcome, error)
	calls      int
	caps       int
	validation error
}

func (a *fixtureAdapter) Capabilities(context.Context, model.Token) (model.Capabilities, error) {
	a.caps++
	return model.Capabilities{CanPublish: true}, nil
}
func (a *fixtureAdapter) Validate(model.Content, model.Capabilities) error { return a.validation }
func (a *fixtureAdapter) Publish(_ context.Context, _ model.Token, _ model.Content, p model.Progress, c provider.Checkpoint) (model.Outcome, error) {
	a.calls++
	return a.fn(p, c)
}
func setupEngine(t *testing.T) (*Engine, *fixtureStore, *fixtureAdapter) {
	t.Helper()
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	vault, err := security.NewVault(map[string][]byte{"key": make([]byte, 32)}, "key")
	if err != nil {
		t.Fatal(err)
	}
	a := model.Account{ID: "account", Owner: model.Principal{TenantID: "t", Subject: "u"}, Identity: model.Identity{ID: "remote"}, Platform: model.X, Status: "active"}
	b, _ := json.Marshal(model.Token{AccessToken: "test-token", ExpiresAt: now.Add(time.Hour)})
	a.EncryptedToken, _ = vault.Seal(b, a.TokenAAD())
	s := &fixtureStore{account: a, target: model.Target{ID: "target", PostID: "post", AccountID: a.ID, Platform: model.X, Status: "running", LeaseToken: "lease"}}
	p := &fixtureAdapter{fn: func(_ model.Progress, c provider.Checkpoint) (model.Outcome, error) {
		if err := c(model.Progress{Unsafe: true}); err != nil {
			return model.Outcome{}, err
		}
		return model.Outcome{Done: true, RemoteID: "remote-post", RemoteURL: "https://x.com/i/status/remote-post", Progress: model.Progress{Stage: "done"}}, nil
	}}
	reg := provider.Registry{model.X: p}
	m := &account.Manager{Store: s, Vault: vault, Adapters: reg, Now: func() time.Time { return now }}
	return &Engine{Store: s, Tokens: m, Adapters: reg, Now: func() time.Time { return now }, LeaseDuration: time.Minute, JobTimeout: time.Minute, MaxFailures: 5}, s, p
}
func TestWorkerPublishesAndStoresReceipt(t *testing.T) {
	e, s, p := setupEngine(t)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 || s.finished == nil || s.finished.Status != "published" || s.finished.RemoteID != "remote-post" {
		t.Fatal("publication receipt lost")
	}
}
func TestLostAccountLockStopsBeforeProvider(t *testing.T) {
	e, s, p := setupEngine(t)
	s.lockCanceled = true
	if err := e.RunOnce(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost account lock must stop work: %v", err)
	}
	if p.calls != 0 || p.caps != 0 || s.finished != nil {
		t.Fatal("provider or terminal state changed after lock loss")
	}
}
func TestUnlabelledRemoteDataIsPreservedForReconciliation(t *testing.T) {
	e, _, _ := setupEngine(t)
	p := model.Progress{Data: json.RawMessage(`{"remote_id":"known-operation"}`)}
	f := e.failure(model.Target{}, p, &provider.Error{Code: "provider_unavailable"})
	if f.Status != "needs_attention" || string(f.Progress.Data) != string(p.Data) {
		t.Fatal("unlabelled remote progress was treated as a fresh failure")
	}
}

func TestNewProcessingWindowAllowsRecoveryAfterOldFirstStart(t *testing.T) {
	e, s, p := setupEngine(t)
	firstStart := e.now().Add(-48 * time.Hour)
	newWindow := e.now().Add(-time.Minute)
	s.target.StartedAt = &firstStart
	s.target.ProcessingStartedAt = &newWindow
	s.target.Progress = model.Progress{Stage: "accepted", Data: json.RawMessage(`{"operation":"existing"}`)}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 || s.finished == nil || s.finished.Status != "published" {
		t.Fatal("a new processing window inherited the original delivery timeout")
	}
}

func TestExpiredProcessingWindowStillRequiresAttention(t *testing.T) {
	e, s, p := setupEngine(t)
	firstStart := e.now().Add(-time.Hour)
	expiredWindow := e.now().Add(-25 * time.Hour)
	s.target.StartedAt = &firstStart
	s.target.ProcessingStartedAt = &expiredWindow
	s.target.Progress = model.Progress{Stage: "accepted", Data: json.RawMessage(`{"operation":"existing"}`)}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 || s.finished == nil || s.finished.Status != "needs_attention" {
		t.Fatal("the processing deadline was not enforced independently of first-start history")
	}
}
func TestAmbiguousPublicationIsNeverRetried(t *testing.T) {
	e, s, p := setupEngine(t)
	p.fn = func(_ model.Progress, c provider.Checkpoint) (model.Outcome, error) {
		if err := c(model.Progress{Unsafe: true}); err != nil {
			return model.Outcome{}, err
		}
		return model.Outcome{}, &provider.Error{Code: "network_error", Retryable: true, Ambiguous: true}
	}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.finished.Status != "needs_attention" || !s.finished.Progress.Unsafe {
		t.Fatal("ambiguous post was scheduled for duplicate retry")
	}
}
func TestKnownRateRejectionRetriesAfterProviderDeadline(t *testing.T) {
	e, s, p := setupEngine(t)
	p.fn = func(_ model.Progress, c provider.Checkpoint) (model.Outcome, error) {
		if err := c(model.Progress{}); err != nil {
			return model.Outcome{}, err
		}
		return model.Outcome{}, &provider.Error{Code: "rate_limit", Retryable: true, RetryAfter: 10 * time.Minute}
	}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.finished.Status != "retrying" || s.finished.NextAttemptAt.Before(e.Now().Add(10*time.Minute)) {
		t.Fatal("provider retry deadline ignored")
	}
}
func TestWorkerCannotPublishAfterLeaseLoss(t *testing.T) {
	e, s, p := setupEngine(t)
	s.lost = true
	err := e.RunOnce(context.Background())
	if !errors.Is(err, store.ErrLeaseLost) || p.calls != 0 {
		t.Fatal("lost lease permitted external mutation")
	}
}
func TestWorkerWillNotReenterUnsafeCheckpoint(t *testing.T) {
	e, s, p := setupEngine(t)
	s.target.Progress = model.Progress{Unsafe: true, Stage: "publishing"}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 || s.finished.Status != "needs_attention" {
		t.Fatal("unsafe crash checkpoint reentered")
	}
}
func TestKnownRemoteOperationResumesPolling(t *testing.T) {
	e, s, p := setupEngine(t)
	s.target.Progress = model.Progress{Stage: "polling", Data: json.RawMessage(`{"id":"remote-operation"}`)}
	p.fn = func(p model.Progress, _ provider.Checkpoint) (model.Outcome, error) {
		if p.Stage != "polling" {
			t.Fatal("checkpoint lost")
		}
		return model.Outcome{Progress: p, PollAfter: time.Minute}, nil
	}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.caps != 0 || s.finished.Status != "processing" || s.finished.NextAttemptAt != e.Now().Add(time.Minute) {
		t.Fatal("existing operation was not preserved")
	}
}
func TestValidationStopsExternalMutation(t *testing.T) {
	e, s, p := setupEngine(t)
	p.validation = &provider.Error{Code: "invalid_privacy"}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 || s.finished.Status != "failed" {
		t.Fatal("invalid settings reached provider publish")
	}
}
func TestRetryBudgetDoesNotCountSuccessfulPolls(t *testing.T) {
	e, s, p := setupEngine(t)
	s.target.Attempts = 200
	s.target.Failures = 1
	p.fn = func(_ model.Progress, _ provider.Checkpoint) (model.Outcome, error) {
		return model.Outcome{}, &provider.Error{Code: "busy", Retryable: true}
	}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.finished.Status != "retrying" {
		t.Fatal("poll attempts exhausted failure budget")
	}
}
func TestRetryBudgetStopsRepeatedRejections(t *testing.T) {
	e, s, p := setupEngine(t)
	s.target.Failures = 4
	p.fn = func(_ model.Progress, _ provider.Checkpoint) (model.Outcome, error) {
		return model.Outcome{}, &provider.Error{Code: "busy", Retryable: true}
	}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.finished.Status != "failed" {
		t.Fatal("unbounded retry loop")
	}
}
func TestFailedPollingPreservesKnownRemoteOperation(t *testing.T) {
	e, s, p := setupEngine(t)
	s.target.Progress = model.Progress{Stage: "polling", Data: json.RawMessage(`{"publish_id":"existing"}`)}
	p.fn = func(_ model.Progress, _ provider.Checkpoint) (model.Outcome, error) {
		return model.Outcome{}, &provider.Error{Code: "busy", Retryable: true}
	}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.finished.Status != "processing" || s.finished.Progress.Stage != "polling" {
		t.Fatal("polling retry lost its in-flight classification")
	}
}
func TestExhaustedPollingRequiresReconciliation(t *testing.T) {
	e, s, p := setupEngine(t)
	s.target.Progress = model.Progress{Stage: "polling", Data: json.RawMessage(`{"publish_id":"existing"}`)}
	s.target.Failures = 4
	p.fn = func(_ model.Progress, _ provider.Checkpoint) (model.Outcome, error) {
		return model.Outcome{}, &provider.Error{Code: "busy", Retryable: true}
	}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.finished.Status != "needs_attention" || s.finished.Progress.Stage != "polling" {
		t.Fatal("accepted remote job could be blindly retried")
	}
}
func TestRemovingProviderDoesNotDiscardKnownOperation(t *testing.T) {
	e, s, p := setupEngine(t)
	s.target.Progress = model.Progress{Stage: "polling", Data: json.RawMessage(`{"id":"known"}`)}
	delete(e.Adapters, model.X)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 || s.finished.Status != "needs_attention" {
		t.Fatal("missing provider made remote job restartable")
	}
}
func TestTightenedMediaPolicyStopsScheduledDispatch(t *testing.T) {
	e, s, p := setupEngine(t)
	e.Media, _ = security.NewMediaFetcher([]string{"new.example.com"}, 1024)
	s.target.Content.Media = []model.Media{{URL: "https://old.example.com/file.jpg", MIMEType: "image/jpeg", SizeBytes: 100}}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 || s.finished.Status != "failed" || s.finished.Error != "media_policy_changed" {
		t.Fatal("stale asset policy authorized publish")
	}
}
