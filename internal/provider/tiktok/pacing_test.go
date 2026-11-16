package tiktok

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

func TestStatusBudgetDefersWithoutIssuingRequest(t *testing.T) {
	var requests atomic.Int32
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		jsonReply(w, 200, `{"data":{"status":"PROCESSING_DOWNLOAD"},"error":{"code":"ok"}}`)
	})
	wait := time.Duration(0)
	ctx := provider.WithRequestPacing(t.Context(), provider.RequestPacing{
		Reserve: func(_ context.Context, scope string, spacing time.Duration) (time.Duration, error) {
			if scope != "tiktok.status" || spacing < 2100*time.Millisecond {
				t.Fatalf("status request used wrong budget: %s %v", scope, spacing)
			}
			return wait, nil
		},
		Cooldown: func(_ context.Context, scope string, delay time.Duration) (time.Duration, error) {
			if scope != "tiktok.status" || delay < 2100*time.Millisecond {
				t.Fatalf("completed request lost minimum spacing: %s %v", scope, delay)
			}
			wait = delay
			return delay, nil
		},
	})
	progress := fixtureProgress(t, "v_pub_url~existing")
	out, err := client.Publish(ctx, testToken(), approvedVideo(), progress, func(model.Progress) error { t.Fatal("polling changed initialization checkpoint"); return nil })
	if err != nil || out.Done || requests.Load() != 1 {
		t.Fatalf("first status request did not run: %+v %v", out, err)
	}
	_, err = client.Publish(ctx, testToken(), approvedVideo(), progress, nil)
	if !provider.IsPacingWait(err) || requests.Load() != 1 {
		t.Fatalf("another target bypassed the shared budget: requests=%d error=%v", requests.Load(), err)
	}
	pe := providerError(t, err)
	if !pe.RateLimited || pe.RetryAfter < 2100*time.Millisecond || pe.Ambiguous {
		t.Fatalf("budget wait was not resumable: %+v", pe)
	}
}

func TestStatus429PersistsProviderCooldown(t *testing.T) {
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		jsonReply(w, 429, `broken provider body`)
	})
	var cooldown time.Duration
	ctx := provider.WithRequestPacing(t.Context(), provider.RequestPacing{
		Reserve: func(context.Context, string, time.Duration) (time.Duration, error) { return 0, nil },
		Cooldown: func(_ context.Context, scope string, delay time.Duration) (time.Duration, error) {
			if scope != "tiktok.status" {
				t.Fatalf("wrong cooldown scope: %s", scope)
			}
			cooldown = delay
			return delay, nil
		},
	})
	_, err := client.Publish(ctx, testToken(), approvedVideo(), fixtureProgress(t, "v_pub_url~existing"), nil)
	pe := providerError(t, err)
	if !pe.RateLimited || !pe.Retryable || pe.Ambiguous || pe.RetryAfter != 42*time.Second || cooldown != 42*time.Second {
		t.Fatalf("HTTP429 did not persist a resumable cooldown: %+v cooldown=%v", pe, cooldown)
	}
}

func TestStatusBudgetFailureStopsBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	client := newFixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
	dbErr := errors.New("fixture pacing store unavailable")
	ctx := provider.WithRequestPacing(t.Context(), provider.RequestPacing{Reserve: func(context.Context, string, time.Duration) (time.Duration, error) { return 0, dbErr }})
	_, err := client.Publish(ctx, testToken(), approvedVideo(), fixtureProgress(t, "v_pub_url~existing"), nil)
	if !provider.IsPacingFailure(err) || !errors.Is(err, dbErr) || requests.Load() != 0 {
		t.Fatalf("missing pacing authority reached status endpoint: %v requests=%d", err, requests.Load())
	}
}
