// Package worker runs durable, independently tracked platform deliveries.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"sync"
	"time"

	"socialpublisher/internal/account"
	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
)

type Engine struct {
	Store         store.Store
	Tokens        *account.Manager
	Adapters      provider.Registry
	Media         *security.MediaFetcher
	Now           func() time.Time
	LeaseDuration time.Duration
	JobTimeout    time.Duration
	PollInterval  time.Duration
	Concurrency   int
	MaxFailures   int
	Logger        *slog.Logger
	OnProgress    func()
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
func (e *Engine) lease() time.Duration {
	if e.LeaseDuration > 0 {
		return e.LeaseDuration
	}
	return 90 * time.Second
}
func (e *Engine) timeout() time.Duration {
	if e.JobTimeout > 0 {
		return e.JobTimeout
	}
	return 10 * time.Minute
}
func (e *Engine) maxFailures() int {
	if e.MaxFailures > 0 {
		return e.MaxFailures
	}
	return 5
}

func (e *Engine) Run(ctx context.Context) {
	n := e.Concurrency
	if n < 1 {
		n = 4
	}
	interval := e.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				err := e.RunOnce(ctx)
				if e.OnProgress != nil && ctx.Err() == nil && (err == nil || errors.Is(err, store.ErrNoWork)) {
					e.OnProgress()
				}
				if err != nil {
					if !errors.Is(err, store.ErrNoWork) && !errors.Is(err, context.Canceled) {
						e.log("worker_iteration_failed")
					}
					timer := time.NewTimer(interval)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}
		}()
	}
	wg.Wait()
}

func (e *Engine) RunOnce(parent context.Context) error {
	target, err := e.Store.Claim(parent, e.lease())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, e.timeout())
	defer cancel()
	done := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		tick := time.NewTicker(e.lease() / 3)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := e.Store.Heartbeat(ctx, target.ID, target.LeaseToken, e.lease()); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { close(done); <-heartbeatDone }()
	progress := target.Progress
	finish := func(f store.Finish) error {
		savectx, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer stop()
		return e.Store.FinishTarget(savectx, target.ID, target.LeaseToken, f)
	}
	if progress.Unsafe {
		return finish(store.Finish{Status: "needs_attention", Progress: progress, Error: "uncertain_publish: Confirm the existing result before another attempt."})
	}
	processingStarted := target.ProcessingStartedAt
	if processingStarted == nil {
		// Older rows and legacy store implementations only record the first
		// claim. New progress and explicit recovery get a separate window.
		processingStarted = target.StartedAt
	}
	claimTime := target.ClaimedAt
	if claimTime.IsZero() {
		claimTime = e.now()
	}
	if processingStarted != nil && knownProgress(progress) && claimTime.Sub(*processingStarted) >= 24*time.Hour {
		return finish(store.Finish{Status: "needs_attention", Progress: progress, Error: "processing_timeout: Check the destination account before retrying."})
	}
	lockctx, release, err := e.Store.TryAcquireAccount(ctx, target.AccountID)
	if errors.Is(err, store.ErrAccountBusy) {
		// Refresh/revocation can own the account without a publishing lease.
		// Defer that account's whole backlog, not just this losing claim.
		if _, cooldownErr := e.Store.SetProviderCooldown(ctx, target.AccountID, "platform:"+string(target.Platform), time.Second); cooldownErr != nil {
			return cooldownErr
		}
		return finish(e.waiting(target, progress, time.Second, ""))
	}
	if err != nil {
		return err
	}
	defer release()
	// Nonblocking admission is still not sufficient authority: verify the lease
	// using the monitored lock-session context before any provider operation.
	if err = e.Store.Heartbeat(lockctx, target.ID, target.LeaseToken, e.lease()); err != nil {
		return err
	}
	lockctx = provider.WithRequestPacing(lockctx, provider.RequestPacing{
		Reserve: func(ctx context.Context, scope string, spacing time.Duration) (time.Duration, error) {
			return e.Store.ReserveProviderRequest(ctx, target.AccountID, scope, spacing)
		},
		Cooldown: func(ctx context.Context, scope string, delay time.Duration) (time.Duration, error) {
			return e.Store.SetProviderCooldown(ctx, target.AccountID, scope, delay)
		},
	})
	fail := func(cause error) error {
		if lockctx.Err() != nil && !progress.Unsafe {
			return lockctx.Err()
		}
		if provider.IsPacingFailure(cause) {
			// Without durable admission/cooldown evidence, keep the lease and
			// checkpoint available for safe recovery; do not consume failures.
			return cause
		}
		var limited *provider.Error
		if errors.As(cause, &limited) && limited.RateLimited && !limited.Ambiguous && !progress.Unsafe && !provider.IsPacingWait(cause) {
			delay := provider.RateLimitDelay(limited)
			wait, saveErr := e.Store.SetProviderCooldown(lockctx, target.AccountID, "platform:"+string(target.Platform), delay)
			if saveErr != nil {
				return saveErr
			}
			copy := *limited
			copy.RetryAfter = delay
			if wait > copy.RetryAfter {
				copy.RetryAfter = wait
			}
			cause = &copy
		}
		return finish(e.failure(target, progress, cause))
	}
	if err := provider.BeforeRequest(lockctx, "platform:"+string(target.Platform), 0); err != nil {
		return fail(err)
	}
	adapter, ok := e.Adapters[target.Platform]
	if !ok {
		return finish(e.failure(target, progress, &provider.Error{Code: "provider_not_configured"}))
	}
	a, err := e.Store.GetWorkerAccount(lockctx, target.AccountID)
	if err != nil {
		return err
	}
	if a.Status != "active" {
		status := "cancelled"
		if knownProgress(progress) {
			status = "needs_attention"
		}
		return finish(store.Finish{Status: status, Progress: progress, Error: "account_unavailable: Reconnect the account to continue."})
	}
	if deadline := target.Content.PublishBefore; deadline != nil && !deadline.After(e.now()) {
		status := "cancelled"
		if knownProgress(progress) {
			status = "needs_attention"
		}
		return finish(store.Finish{Status: status, Progress: progress, Error: "publication_window_expired"})
	}
	if reply := target.Content.Reply; reply != nil {
		parent, parentErr := e.Store.GetPost(lockctx, a.Owner, reply.PostID)
		var confirmed *model.XReply
		if parentErr == nil {
			confirmed, parentErr = model.ConfirmedXReply(parent, a)
		}
		if parentErr != nil || confirmed == nil || *confirmed != *reply || target.Platform != model.X {
			return finish(e.failure(target, progress, &provider.Error{Code: "x_thread_parent_changed", Message: "Reconcile the original thread receipt before publication."}))
		}
	}
	token, err := e.Tokens.Usable(lockctx, a, false)
	if err != nil {
		return fail(err)
	}
	for _, media := range target.Content.Media {
		if e.Media == nil || e.Media.Validate(media) != nil {
			return finish(e.failure(target, progress, &provider.Error{Code: "media_policy_changed", Message: "The scheduled media no longer satisfies the configured asset policy."}))
		}
	}
	// Revalidate user-approved choices immediately before starting remote work.
	// Subsequent claims resume the known remote operation instead of initializing it.
	if !knownProgress(progress) {
		caps, capErr := adapter.Capabilities(lockctx, token)
		if capErr != nil {
			var pe *provider.Error
			if errors.As(capErr, &pe) && pe.AuthExpired {
				token, capErr = e.Tokens.Usable(lockctx, a, true)
				if capErr == nil {
					caps, capErr = adapter.Capabilities(lockctx, token)
				}
			}
		}
		if capErr != nil {
			return fail(capErr)
		}
		if err = adapter.Validate(target.Content, caps); err != nil {
			return finish(e.failure(target, progress, err))
		}
	}
	checkpoint := func(p model.Progress) error {
		if lockctx.Err() != nil {
			return lockctx.Err()
		}
		if err := e.Store.SaveProgress(lockctx, target.ID, target.LeaseToken, p); err != nil {
			cancel()
			return err
		}
		progress = p
		return nil
	}
	outcome, err := adapter.Publish(lockctx, token, target.Content, progress, checkpoint)
	if err != nil && !progress.Unsafe {
		var pe *provider.Error
		if errors.As(err, &pe) && pe.AuthExpired && !pe.Ambiguous {
			token, err = e.Tokens.Usable(lockctx, a, true)
			if err == nil {
				outcome, err = adapter.Publish(lockctx, token, target.Content, progress, checkpoint)
			}
		}
	}
	if err != nil {
		if errors.Is(err, store.ErrLeaseLost) {
			return err
		}
		// A lost context leaves durable checkpoints for lease recovery. It is never
		// evidence that a transmitted public mutation did not complete.
		if lockctx.Err() != nil && !progress.Unsafe {
			return lockctx.Err()
		}
		return fail(err)
	}
	if outcome.Done {
		if outcome.RemoteID == "" {
			return finish(store.Finish{Status: "needs_attention", Progress: progress, Error: "missing_receipt: Provider completion lacks a durable identifier."})
		}
		return finish(store.Finish{Status: "published", Progress: outcome.Progress, RemoteID: outcome.RemoteID, RemoteURL: outcome.RemoteURL, Warnings: outcome.Warnings})
	}
	if outcome.Progress.Unsafe {
		return finish(store.Finish{Status: "needs_attention", Progress: outcome.Progress, Error: "uncertain_publish: Publication requires reconciliation."})
	}
	delay := outcome.PollAfter
	if delay < 3*time.Second {
		delay = 3 * time.Second
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	return finish(store.Finish{Status: "processing", Progress: outcome.Progress, NextAttemptAt: e.now().Add(delay), RetryAfter: delay, Warnings: outcome.Warnings})
}

func (e *Engine) waiting(t model.Target, p model.Progress, delay time.Duration, code string) store.Finish {
	status := "retrying"
	if knownProgress(p) {
		status = "processing"
	}
	return store.Finish{Status: status, Progress: p, NextAttemptAt: e.now().Add(delay), RetryAfter: delay,
		RemoteID: t.RemoteID, RemoteURL: t.RemoteURL, Warnings: t.Warnings, Error: code}
}

func (e *Engine) failure(t model.Target, p model.Progress, err error) store.Finish {
	f := store.Finish{Status: "failed", Progress: p, Error: "delivery_failed: Check the connection and post settings.", CountFailure: true}
	if knownProgress(p) {
		f.Status = "needs_attention"
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		// Adapter codes are controlled classifications, not raw provider payloads.
		f.Error = pe.Code
		if pe.Ambiguous || p.Unsafe {
			f.Status = "needs_attention"
			return f
		}
		if pe.AuthExpired {
			if knownProgress(p) {
				f.Status = "needs_attention"
			}
			return f
		}
		if pe.RateLimited {
			return e.waiting(t, p, provider.RateLimitDelay(pe), pe.Code)
		}
		if pe.Retryable && t.Failures+1 < e.maxFailures() {
			f.Status = "retrying"
			if knownProgress(p) {
				f.Status = "processing"
			}
			delay := backoff(t.Failures)
			if pe.RetryAfter > delay {
				delay = pe.RetryAfter
			}
			if delay > 24*time.Hour {
				delay = 24 * time.Hour
			}
			f.NextAttemptAt = e.now().Add(delay)
			f.RetryAfter = delay
		}
	} else if p.Unsafe {
		f.Status = "needs_attention"
		f.Error = "uncertain_publish"
	}
	return f
}
func knownProgress(p model.Progress) bool { return p.Stage != "" || len(p.Data) > 0 }
func backoff(failures int) time.Duration {
	if failures < 0 {
		failures = 0
	}
	if failures > 8 {
		failures = 8
	}
	base := 5 * time.Second * time.Duration(1<<uint(failures))
	if base > 15*time.Minute {
		base = 15 * time.Minute
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return base
	}
	return base + time.Duration(binary.BigEndian.Uint64(b[:])%uint64(base/4+1))
}
func (e *Engine) log(message string, args ...any) {
	if e.Logger != nil {
		e.Logger.Warn(message, args...)
	}
}
