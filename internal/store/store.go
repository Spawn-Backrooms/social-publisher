package store

import (
	"context"
	"errors"
	"socialpublisher/internal/model"
	"time"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrConflict    = errors.New("state conflict")
	ErrIdempotency = errors.New("idempotency key reused with different content")
	ErrLeaseLost   = errors.New("worker lease lost")
	ErrOAuthState  = errors.New("invalid, expired, or used OAuth state")
	ErrNoWork      = errors.New("no work due")
)

type Finish struct {
	Status        string
	Progress      model.Progress
	NextAttemptAt time.Time
	// RetryAfter schedules against database time when set by a worker.
	RetryAfter   time.Duration
	RemoteID     string
	RemoteURL    string
	Error        string
	Warnings     []string
	CountFailure bool
}
type Resolution struct {
	Action    string `json:"action"`
	RemoteID  string `json:"remote_id,omitempty"`
	RemoteURL string `json:"remote_url,omitempty"`
	Note      string `json:"note"`
}
type Store interface {
	Ping(context.Context) error
	Close()
	PutOAuthState(context.Context, model.OAuthState) error
	ConsumeOAuthState(context.Context, string, string, model.Platform) (model.OAuthState, error)
	UpsertAccount(context.Context, model.Account) (model.Account, error)
	GetAccount(context.Context, model.Principal, string) (model.Account, error)
	GetWorkerAccount(context.Context, string) (model.Account, error)
	ListAccounts(context.Context, model.Principal) ([]model.Account, error)
	SaveToken(context.Context, string, int64, []byte, time.Time) error
	BeginTokenRefresh(context.Context, string, int64) error
	ClearTokenRefresh(context.Context, string, int64) error
	ListRefreshCandidates(context.Context, model.Platform, time.Time, int) ([]model.Account, error)
	MarkReauth(context.Context, string, int64) error
	Disconnect(context.Context, model.Principal, string) (model.Account, error)
	AcquireIdentity(context.Context, model.Platform, string) (context.Context, func(), error)
	// AcquireAccount serializes provider operations and refresh across processes.
	// Use the returned context for protected operations; it is cancelled if the
	// lock session fails. Defer release. Hold the lock for Disconnect through
	// upstream revocation, and hold the identity lock for UpsertAccount.
	AcquireAccount(context.Context, string) (context.Context, func(), error)
	TryAcquireAccount(context.Context, string) (context.Context, func(), error)
	ReserveProviderRequest(context.Context, string, string, time.Duration) (time.Duration, error)
	SetProviderCooldown(context.Context, string, string, time.Duration) (time.Duration, error)
	AdmitRequest(context.Context, model.Principal, AdmissionLimits) (AdmissionLease, error)
	AdmitProviderOperation(context.Context, model.Principal, model.Platform, string, AdmissionLimits) (AdmissionLease, error)
	ReleaseAdmission(context.Context, model.Principal, string) error
	CreatePost(context.Context, model.Post, string, string) (post model.Post, created bool, err error)
	LookupIdempotency(context.Context, model.Principal, string, string) (model.Post, error)
	GetPost(context.Context, model.Principal, string) (model.Post, error)
	ListPosts(context.Context, model.Principal, int) ([]model.Post, error)
	CancelPost(context.Context, model.Principal, string) (model.Post, error)
	ReschedulePost(context.Context, model.Principal, string, int64, time.Time, string) (model.Post, error)
	RetryTarget(context.Context, model.Principal, string, string) (model.Post, error)
	ResolveTarget(context.Context, model.Principal, string, string, Resolution) (model.Post, error)
	ListEvents(context.Context, model.Principal, string) ([]model.Event, error)
	ListEventsPage(context.Context, model.Principal, string, int64, int) (EventPage, error)
	Claim(context.Context, time.Duration) (model.Target, error)
	Heartbeat(context.Context, string, string, time.Duration) error
	SaveProgress(context.Context, string, string, model.Progress) error
	FinishTarget(context.Context, string, string, Finish) error
}

func PostStatus(targets []model.Target) string {
	if len(targets) == 0 {
		return "queued"
	}
	counts := map[string]int{}
	for _, t := range targets {
		counts[t.Status]++
	}
	if counts["published"] == len(targets) {
		return "published"
	}
	if counts["cancelled"] == len(targets) {
		return "cancelled"
	}
	if counts["needs_attention"] > 0 {
		return "needs_attention"
	}
	if counts["running"]+counts["processing"] > 0 {
		return "publishing"
	}
	if counts["queued"]+counts["retrying"] > 0 {
		if counts["published"] > 0 {
			return "partially_published"
		}
		return "queued"
	}
	if counts["published"] > 0 {
		return "partially_published"
	}
	return "failed"
}
