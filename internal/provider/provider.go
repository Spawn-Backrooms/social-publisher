package provider

import (
	"context"
	"io"
	"net/http"
	"time"

	"socialpublisher/internal/model"
)

// Checkpoint must succeed before the next external step. Unsafe marks a step
// whose completion cannot safely be inferred after a lost response or crash.
type Checkpoint func(model.Progress) error
type Adapter interface {
	Platform() model.Platform
	AuthorizationURL(state, challenge string) string
	Exchange(context.Context, string, string) (model.Token, error)
	Refresh(context.Context, model.Token) (model.Token, error)
	Identity(context.Context, model.Token) (model.Identity, error)
	Revoke(context.Context, model.Token) error
	Capabilities(context.Context, model.Token) (model.Capabilities, error)
	Validate(model.Content, model.Capabilities) error
	Publish(context.Context, model.Token, model.Content, model.Progress, Checkpoint) (model.Outcome, error)
}
type MediaFetcher interface {
	Open(context.Context, model.Media) (io.ReadCloser, int64, error)
}
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	APIBase      string
	AuthBase     string
	GraphVersion string
	HTTPClient   *http.Client
	Media        MediaFetcher
	Audited      bool
}
type Error struct {
	Code        string
	Message     string
	Retryable   bool
	Ambiguous   bool
	AuthExpired bool
	RetryAfter  time.Duration
	// RateLimited carries observed throttling metadata. It can coexist with
	// Ambiguous; publication uncertainty always takes precedence over retrying.
	RateLimited bool
	// RefreshNotApplied is explicit evidence that a refresh did not rotate a grant.
	RefreshNotApplied bool
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

type Registry map[model.Platform]Adapter
