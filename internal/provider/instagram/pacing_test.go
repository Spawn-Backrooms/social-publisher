package instagram

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

func brokenThrottleResponse(w http.ResponseWriter, broken string) {
	w.Header().Set("Retry-After", "42")
	if broken == "truncated" {
		w.Header().Set("Content-Length", "256")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"id":"201"}`)
		return
	}
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = fmt.Fprint(w, strings.Repeat("private-response-sentinel", maxResponseBytes/24+1))
}

func Test429BodyFailurePreservesThrottleAndMutationUncertainty(t *testing.T) {
	for _, broken := range []string{"truncated", "oversized"} {
		for _, operation := range []struct {
			name      string
			kind      requestKind
			method    string
			ambiguous bool
			retryable bool
		}{
			{"read", requestRead, http.MethodGet, false, true},
			{"create", requestCreate, http.MethodPost, false, true},
			{"publish", requestPublish, http.MethodPost, true, false},
			{"refresh", requestRefresh, http.MethodGet, true, false},
		} {
			t.Run(broken+"/"+operation.name, func(t *testing.T) {
				client, server := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { brokenThrottleResponse(w, broken) })
				_, err := client.request(t.Context(), operation.method, server.URL+"/fixture", nil, "fixture-token", operation.kind)
				var pe *provider.Error
				if !errors.As(err, &pe) || !pe.RateLimited || pe.RetryAfter != 42*time.Second {
					t.Fatalf("HTTP429 body failure lost its account cooldown: %+v", pe)
				}
				if pe.Ambiguous != operation.ambiguous || pe.Retryable != operation.retryable || pe.RefreshNotApplied {
					t.Fatalf("body failure changed operation certainty: %+v", pe)
				}
				if strings.Contains(err.Error(), "private-response-sentinel") || strings.Contains(err.Error(), "fixture-token") {
					t.Fatal("incomplete response leaked provider content")
				}
			})
		}
	}
}

func TestIncompletePublish429CannotClearUnsafeCheckpointOrPublishAgain(t *testing.T) {
	for _, broken := range []string{"truncated", "oversized"} {
		t.Run(broken, func(t *testing.T) {
			var creates, publishes atomic.Int32
			client, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v26.0/42/content_publishing_limit":
					quotaResponse(w, 0, 7)
				case "/v26.0/42/media":
					creates.Add(1)
					_, _ = fmt.Fprint(w, `{"id":"101"}`)
				case "/v26.0/101":
					_, _ = fmt.Fprint(w, `{"status_code":"FINISHED"}`)
				case "/v26.0/42/media_publish":
					publishes.Add(1)
					brokenThrottleResponse(w, broken)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			var progress model.Progress
			checkpoint := func(next model.Progress) error { progress = next; return nil }
			out, err := client.Publish(t.Context(), publishToken(), imageContent(), model.Progress{}, checkpoint)
			var pe *provider.Error
			if !errors.As(err, &pe) || !pe.RateLimited || !pe.Ambiguous || pe.Retryable || !progress.Unsafe || !out.Progress.Unsafe || progress.Stage != stagePublishing {
				t.Fatalf("incomplete public response authorized a retry: error=%+v progress=%+v", pe, progress)
			}
			_, err = client.Publish(t.Context(), publishToken(), imageContent(), progress, checkpoint)
			if !errors.As(err, &pe) || !pe.Ambiguous || !progress.Unsafe || creates.Load() != 1 || publishes.Load() != 1 {
				t.Fatalf("unsafe retry repeated remote work: error=%v creates=%d publishes=%d", err, creates.Load(), publishes.Load())
			}
		})
	}
}
