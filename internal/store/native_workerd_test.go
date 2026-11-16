package store

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"socialpublisher/internal/model"
	"testing"
	"time"
)

// Cross-language fixture only. NewNative still uses the fixed production URL;
// this test transport routes the request to an explicitly local test gateway.
func TestNativeWorkerdBridge(t *testing.T) {
	endpoint := os.Getenv("NATIVE_FIXTURE_URL")
	if endpoint == "" {
		t.Skip("local cross-language fixture not requested")
	}
	destination, err := url.Parse(endpoint)
	if err != nil || destination.Scheme != "http" || (destination.Hostname() != "127.0.0.1" && destination.Hostname() != "localhost" && destination.Hostname() != "::1") || destination.User != nil || destination.RawQuery != "" || destination.Fragment != "" {
		t.Fatal("fixture URL must be plain local HTTP")
	}
	session := os.Getenv("NATIVE_FIXTURE_SESSION")
	native, err := NewNative(NativeStorageURL, session)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	native.client.Transport = nativeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != NativeStorageURL {
			t.Fatal("production request URL changed")
		}
		clone := r.Clone(r.Context())
		clone.URL = destination
		clone.Host = destination.Host
		return transport.RoundTrip(clone)
	})
	t.Cleanup(transport.CloseIdleConnections)
	t.Cleanup(native.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := native.Ping(ctx); err != nil {
		t.Fatalf("readiness: %v", err)
	}
	owner := model.Principal{TenantID: "fixture-project", Subject: "fixture-owner"}
	ciphertext := []byte("storage-fixture-encrypted-ciphertext")
	accountIDs := []string{"fixture-x", "fixture-instagram"}
	platforms := []model.Platform{model.X, model.Instagram}
	for i, id := range accountIDs {
		providerID := "fixture-provider-" + id
		protected, release, err := native.AcquireIdentity(ctx, platforms[i], providerID)
		if err != nil {
			t.Fatalf("identity guard: %v", err)
		}
		account, err := native.UpsertAccount(protected, model.Account{ID: id, Owner: owner, Platform: platforms[i], Identity: model.Identity{ID: providerID, Username: id, DisplayName: id}, EncryptedToken: ciphertext, TokenExpiresAt: time.Now().UTC().Add(time.Hour), AuthorizationStartedAt: time.Now().UTC().Add(-time.Second)})
		release()
		if err != nil || account.Owner != owner || account.TokenVersion != 1 || !bytes.Equal(account.EncryptedToken, ciphertext) {
			t.Fatalf("private account round trip: %v", err)
		}
		got, err := native.GetAccount(ctx, owner, id)
		if err != nil || got.Owner != owner || !bytes.Equal(got.EncryptedToken, ciphertext) {
			t.Fatalf("stored ciphertext: %v", err)
		}
	}
	draft := model.Post{ID: "fixture-post", Owner: owner, PublishAt: time.Now().UTC().Add(-time.Second), Timezone: "UTC", Targets: []model.Target{
		{ID: "fixture-target-0", AccountID: accountIDs[0], Platform: platforms[0], Content: model.Content{Text: "explicit approved fixture X"}},
		{ID: "fixture-target-1", AccountID: accountIDs[1], Platform: platforms[1], Content: model.Content{Text: "explicit approved fixture Instagram"}},
	}}
	post, created, err := native.CreatePost(ctx, draft, "fixture-intent", "fixture-request-hash")
	if err != nil || !created || len(post.Targets) != 2 {
		t.Fatalf("atomic destinations: %v", err)
	}
	replay, created, err := native.CreatePost(ctx, draft, "fixture-intent", "fixture-request-hash")
	if err != nil || created || replay.ID != post.ID {
		t.Fatalf("idempotent post: %v", err)
	}
	if _, _, err := native.CreatePost(ctx, draft, "fixture-intent", "other-hash"); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("idempotency collision: %v", err)
	}
	first, err := native.Claim(ctx, time.Minute)
	if err != nil || first.ID != "fixture-target-0" || first.LeaseToken == "" || first.ClaimedAt.IsZero() {
		t.Fatalf("private claimed target: %v", err)
	}
	if err := native.SaveProgress(ctx, first.ID, first.LeaseToken, model.Progress{Unsafe: true, Stage: "posting"}); err != nil {
		t.Fatal(err)
	}
	if err := native.FinishTarget(ctx, first.ID, first.LeaseToken, Finish{Status: "published", RemoteID: "fixture-remote-x", RemoteURL: "https://fixture.invalid/posts/fixture-remote-x", Progress: model.Progress{}}); err != nil {
		t.Fatal(err)
	}
	second, err := native.Claim(ctx, time.Minute)
	if err != nil || second.ID != "fixture-target-1" {
		t.Fatalf("independent second channel: %v", err)
	}
	if err := native.SaveProgress(ctx, second.ID, second.LeaseToken, model.Progress{Unsafe: true}); err != nil {
		t.Fatal(err)
	}
	// A proven local fixture rejection is persisted before a safe failed finish.
	if err := native.SaveProgress(ctx, second.ID, second.LeaseToken, model.Progress{}); err != nil {
		t.Fatal(err)
	}
	if err := native.FinishTarget(ctx, second.ID, second.LeaseToken, Finish{Status: "failed", Error: "fixture provider rejection", CountFailure: true, Progress: model.Progress{}}); err != nil {
		t.Fatal(err)
	}
	receipt, err := native.GetPost(ctx, owner, post.ID)
	if err != nil || receipt.Status != "partially_published" || receipt.Targets[0].Status != "published" || receipt.Targets[0].RemoteID != "fixture-remote-x" || receipt.Targets[1].Status != "failed" {
		t.Fatalf("per-channel receipt: %v %+v", err, receipt)
	}
	if _, err := native.RetryTarget(ctx, owner, post.ID, first.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("published channel became retryable: %v", err)
	}
	retried, err := native.RetryTarget(ctx, owner, post.ID, second.ID)
	if err != nil || retried.Targets[0].Status != "published" || retried.Targets[0].RemoteID != "fixture-remote-x" || retried.Targets[1].Status != "queued" {
		t.Fatalf("safe failed-only retry: %v", err)
	}
	claim, err := native.Claim(ctx, time.Minute)
	if err != nil || claim.ID != second.ID || claim.Attempts != 2 {
		t.Fatalf("retry reclaimed wrong channel: %v", err)
	}
	events, err := native.ListEventsPage(ctx, owner, post.ID, 0, 200)
	if err != nil || len(events.Events) < 8 || events.NextAfterID <= 0 {
		t.Fatalf("event receipt: %v", err)
	}
	heartbeat := ProcessHeartbeat{InstanceID: "fixture-process", Role: "serve", BuildRevision: "native-fixture", WorkerProgressSequence: 1}
	if err := native.RecordProcessHeartbeat(ctx, heartbeat); err != nil {
		t.Fatal(err)
	}
	snapshot, err := native.OperationsSnapshot(ctx, OperationsQuery{InstanceID: "fixture-process", HeartbeatStaleAfter: time.Minute, WorkerStaleAfter: time.Minute, Limit: 100})
	if err != nil || snapshot.LocalProcess == nil || snapshot.LocalProcess.InstanceID != "fixture-process" || !snapshot.LocalProcess.HeartbeatFresh {
		t.Fatalf("native operations health: %v", err)
	}
	t.Log("Go native DTO/RPC -> Cloudflare NativeDatabase handlers: owner/ciphertext, atomic 2 destinations, idempotency, lease/progress, partial outcomes, failed-only retry, events, actual heartbeat PASS; zero provider calls")
}
