package provider

import (
	"context"
	"errors"
	"time"
)

// RequestPacing is attached by the worker to the monitored account context.
// Adapters invoke it immediately around paced endpoint requests; the callbacks
// persist account-scoped deadlines rather than sleeping in a worker slot.
type RequestPacing struct {
	Reserve  func(context.Context, string, time.Duration) (time.Duration, error)
	Cooldown func(context.Context, string, time.Duration) (time.Duration, error)
}
type pacingKey struct{}

func WithRequestPacing(ctx context.Context, pacing RequestPacing) context.Context {
	return context.WithValue(ctx, pacingKey{}, pacing)
}

type pacingWait struct{ cause *Error }

func (w *pacingWait) Error() string { return w.cause.Error() }
func (w *pacingWait) Unwrap() error { return w.cause }

type pacingFailure struct{ cause error }

func (e *pacingFailure) Error() string { return "provider pacing unavailable" }
func (e *pacingFailure) Unwrap() error { return e.cause }

// IsPacingWait distinguishes an already-persisted wait from a new provider 429.
// Re-saving every denied reservation as a fresh cooldown could extend it under
// a large backlog even though no request had occurred.
func IsPacingWait(err error) bool    { var wait *pacingWait; return errors.As(err, &wait) }
func IsPacingFailure(err error) bool { var failure *pacingFailure; return errors.As(err, &failure) }

func BeforeRequest(ctx context.Context, scope string, spacing time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pacing, ok := ctx.Value(pacingKey{}).(RequestPacing)
	if !ok || pacing.Reserve == nil {
		return nil
	}
	wait, err := pacing.Reserve(ctx, scope, spacing)
	if err != nil {
		return &pacingFailure{cause: err}
	}
	if wait > 0 {
		return &pacingWait{&Error{Code: "provider_cooldown", Message: "Waiting for the account request budget", Retryable: true, RateLimited: true, RetryAfter: wait}}
	}
	return nil
}

// AfterRequest keeps the minimum gap after the request completes as well as at
// admission. Thus response latency or a process scheduling pause cannot bunch
// two actual status requests closer together than the configured spacing.
func AfterRequest(ctx context.Context, scope string, spacing time.Duration) error {
	pacing, ok := ctx.Value(pacingKey{}).(RequestPacing)
	if !ok || pacing.Cooldown == nil {
		return nil
	}
	if _, err := pacing.Cooldown(ctx, scope, spacing); err != nil {
		return &pacingFailure{cause: err}
	}
	return nil
}

func RateLimitDelay(e *Error) time.Duration {
	delay := e.RetryAfter
	if delay <= 0 {
		delay = time.Minute
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	return delay
}
