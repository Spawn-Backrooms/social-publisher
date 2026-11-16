package store

import (
	"errors"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

func TestPostgresQueueAdmissionIsAtomicPerOwner(t *testing.T) {
	s, _ := postgresFixture(t, 12)
	s.PendingTargetsPerOwner = 3
	owner := model.Principal{TenantID: "tenant", Subject: "busy-owner"}
	a := fixtureAccount(t, s, owner, model.X)
	fixturePost(t, s, owner, time.Now().Add(time.Hour), a)
	fixturePost(t, s, owner, time.Now().Add(time.Hour), a)
	type outcome struct {
		created bool
		err     error
	}
	done := make(chan outcome, 8)
	for range 8 {
		post := model.Post{ID: testID(t, "post_"), Owner: owner, PublishAt: time.Now().Add(time.Hour), Targets: []model.Target{{ID: testID(t, "target_"), AccountID: a.ID, Platform: a.Platform, Content: model.Content{Text: "bounded"}}}}
		key := testID(t, "key_")
		go func() {
			_, created, err := s.CreatePost(t.Context(), post, key, "same-hash")
			done <- outcome{created: created, err: err}
		}()
	}
	accepted, denied := 0, 0
	for range 8 {
		result := <-done
		if result.err != nil {
			var deniedError *AdmissionError
			if !errors.As(result.err, &deniedError) || deniedError.Reason != "queue_capacity" {
				t.Fatalf("unexpected queue admission error: %v", result.err)
			}
			denied++
		} else if result.created {
			accepted++
		}
	}
	if accepted != 1 || denied != 7 {
		t.Fatalf("atomic queue limit: accepted=%d denied=%d, want 1/7", accepted, denied)
	}
	var count int
	if err := s.pool.QueryRow(t.Context(), "SELECT count(*) FROM sp_targets WHERE owner_tenant=$1 AND owner_subject=$2", owner.TenantID, owner.Subject).Scan(&count); err != nil || count != 3 {
		t.Fatalf("stored target count=%d err=%v, want 3", count, err)
	}
	other := model.Principal{TenantID: owner.TenantID, Subject: "other-owner"}
	b := fixtureAccount(t, s, other, model.X)
	fixturePost(t, s, other, time.Now().Add(time.Hour), b)
}

func TestPostgresQueueReplayDoesNotAllocateCapacity(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	s.PendingTargetsPerOwner = 1
	owner := model.Principal{TenantID: "tenant", Subject: "owner"}
	a := fixtureAccount(t, s, owner, model.X)
	p := model.Post{ID: testID(t, "post_"), Owner: owner, PublishAt: time.Now().Add(time.Hour), Targets: []model.Target{{ID: testID(t, "target_"), AccountID: a.ID, Platform: a.Platform, Content: model.Content{Text: "idempotent"}}}}
	first, created, err := s.CreatePost(t.Context(), p, "unique-request", "same-hash")
	if err != nil || !created {
		t.Fatalf("initial admission: created=%v err=%v", created, err)
	}
	p.ID = testID(t, "post_")
	p.Targets[0].ID = testID(t, "target_")
	replay, created, err := s.CreatePost(t.Context(), p, "unique-request", "same-hash")
	if err != nil || created || replay.ID != first.ID {
		t.Fatalf("replay allocated capacity: created=%v id=%q err=%v", created, replay.ID, err)
	}
	_, _, err = s.CreatePost(t.Context(), p, "unique-request", "different-hash")
	if !errors.Is(err, ErrIdempotency) {
		t.Fatalf("full queue obscured idempotency conflict: %v", err)
	}
}

func TestPostgresRetryAndResumeRespectQueueCapacity(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	s.PendingTargetsPerOwner = 1
	owner := model.Principal{TenantID: "tenant", Subject: "owner"}
	a := fixtureAccount(t, s, owner, model.X)
	candidate := fixturePost(t, s, owner, time.Now().Add(time.Hour), a)
	if _, err := s.pool.Exec(t.Context(), "UPDATE sp_targets SET status='failed' WHERE id=$1", candidate.Targets[0].ID); err != nil {
		t.Fatal(err)
	}
	active := fixturePost(t, s, owner, time.Now().Add(time.Hour), a)
	_, err := s.RetryTarget(t.Context(), owner, candidate.ID, candidate.Targets[0].ID)
	requireAdmissionReason(t, err, "queue_capacity")
	if _, err := s.pool.Exec(t.Context(), `UPDATE sp_targets SET status='needs_attention',progress='{"stage":"submitted","data":{"id":"checkpoint"}}'::jsonb WHERE id=$1`, candidate.Targets[0].ID); err != nil {
		t.Fatal(err)
	}
	resolution := Resolution{Action: "resume_processing", Note: "Fixture confirms the saved operation is safe to poll."}
	_, err = s.ResolveTarget(t.Context(), owner, candidate.ID, candidate.Targets[0].ID, resolution)
	requireAdmissionReason(t, err, "queue_capacity")
	if _, err := s.CancelPost(t.Context(), owner, active.ID); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.ResolveTarget(t.Context(), owner, candidate.ID, candidate.Targets[0].ID, resolution)
	if err != nil || resumed.Targets[0].Status != "processing" {
		t.Fatalf("cancel did not release capacity: post=%+v err=%v", resumed, err)
	}
}

func TestPostgresRequestAdmissionBudgetsAndLeaseExpiry(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	owner := model.Principal{TenantID: "tenant", Subject: "owner"}
	other := model.Principal{TenantID: "tenant", Subject: "other"}
	limits := AdmissionLimits{RequestsPerMinute: 2, OwnerConcurrency: 1}
	first, err := s.AdmitRequest(t.Context(), owner, limits)
	if err != nil {
		t.Fatal(err)
	}
	var now time.Time
	if err = s.pool.QueryRow(t.Context(), "SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	if delta := first.ExpiresAt.Sub(now); delta < 90*time.Second || delta > 95*time.Second {
		t.Fatalf("lease not bounded by database clock: %s", delta)
	}
	_, err = s.AdmitRequest(t.Context(), owner, limits)
	requireAdmissionReason(t, err, "owner_concurrency")
	if _, err = s.AdmitRequest(t.Context(), other, limits); err != nil {
		t.Fatalf("owner isolation: %v", err)
	}
	// A foreign owner cannot release this request's permit.
	if err = s.ReleaseAdmission(t.Context(), other, first.Token); err != nil {
		t.Fatal(err)
	}
	_, err = s.AdmitRequest(t.Context(), owner, limits)
	requireAdmissionReason(t, err, "owner_concurrency")
	// Simulate a crashed process by expiring only its database lease.
	if _, err = s.pool.Exec(t.Context(), "UPDATE sp_api_admissions SET expires_at=clock_timestamp()-interval '1 second' WHERE token=$1", first.Token); err != nil {
		t.Fatal(err)
	}
	second, err := s.AdmitRequest(t.Context(), owner, limits)
	if err != nil {
		t.Fatalf("expired lease still consumed concurrency: %v", err)
	}
	if err = s.ReleaseAdmission(t.Context(), owner, second.Token); err != nil {
		t.Fatal(err)
	}
	_, err = s.AdmitRequest(t.Context(), owner, limits)
	requireAdmissionReason(t, err, "request_rate")
	// Reset only by database time; the expired first lease must not prevent it.
	if _, err = s.pool.Exec(t.Context(), "UPDATE sp_api_owner_budgets SET window_started_at=clock_timestamp()-interval '61 seconds' WHERE owner_tenant=$1 AND owner_subject=$2", owner.TenantID, owner.Subject); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitRequest(t.Context(), owner, limits); err != nil {
		t.Fatalf("old database window did not reset: %v", err)
	}
}

func TestPostgresProviderAdmissionIsOwnedAndExpires(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	owner := model.Principal{TenantID: "tenant", Subject: "owner"}
	other := model.Principal{TenantID: "tenant", Subject: "other"}
	a := fixtureAccount(t, s, owner, model.X)
	first, err := s.AdmitProviderOperation(t.Context(), owner, a.Platform, a.Identity.ID, AdmissionLimits{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AdmitProviderOperation(t.Context(), owner, a.Platform, a.Identity.ID, AdmissionLimits{})
	requireAdmissionReason(t, err, "account_concurrency")
	_, err = s.AdmitProviderOperation(t.Context(), other, a.Platform, a.Identity.ID, AdmissionLimits{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign account usage leaked: %v", err)
	}
	if _, err = s.AdmitProviderOperation(t.Context(), owner, model.Instagram, a.Identity.ID, AdmissionLimits{}); err != nil {
		t.Fatalf("different platform collided: %v", err)
	}
	if _, err = s.pool.Exec(t.Context(), "UPDATE sp_api_admissions SET expires_at=clock_timestamp()-interval '1 second' WHERE token=$1", first.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitProviderOperation(t.Context(), owner, a.Platform, a.Identity.ID, AdmissionLimits{}); err != nil {
		t.Fatalf("abandoned account permit never expired: %v", err)
	}
}

func TestPostgresQueueCountsEveryPlatformTarget(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	s.PendingTargetsPerOwner = 2
	owner := model.Principal{TenantID: "tenant", Subject: "owner"}
	x := fixtureAccount(t, s, owner, model.X)
	ig := fixtureAccount(t, s, owner, model.Instagram)
	tk := fixtureAccount(t, s, owner, model.TikTok)
	fixturePost(t, s, owner, time.Now().Add(time.Hour), x, ig)
	p := model.Post{ID: testID(t, "post_"), Owner: owner, PublishAt: time.Now().Add(time.Hour), Targets: []model.Target{{ID: testID(t, "target_"), AccountID: tk.ID, Platform: tk.Platform, Content: model.Content{Text: "third destination"}}}}
	_, _, err := s.CreatePost(t.Context(), p, "another-request", "hash")
	requireAdmissionReason(t, err, "queue_capacity")
}

func requireAdmissionReason(t *testing.T, err error, reason string) {
	t.Helper()
	var denied *AdmissionError
	if !errors.As(err, &denied) || denied.Reason != reason || denied.RetryAfter <= 0 {
		t.Fatalf("admission error=%v, want %s with retry delay", err, reason)
	}
}

func TestPostgresNativeAdmissionSharedAcrossReplicas(t *testing.T) {
	requireNativePostgres(t)
	a, dsn := postgresFixture(t, 8)
	b, err := New(t.Context(), dsn, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	owner := model.Principal{TenantID: "tenant", Subject: "owner"}
	other := model.Principal{TenantID: "tenant", Subject: "other"}
	limits := AdmissionLimits{RequestsPerMinute: 6, OwnerConcurrency: 4}
	type outcome struct {
		lease AdmissionLease
		err   error
	}
	results := make(chan outcome, 12)
	start := make(chan struct{})
	for i := range 12 {
		db := a
		if i%2 == 1 {
			db = b
		}
		go func() {
			<-start
			lease, err := db.AdmitRequest(t.Context(), owner, limits)
			results <- outcome{lease, err}
		}()
	}
	close(start)
	accepted := 0
	leases := make([]AdmissionLease, 0, 4)
	for range 12 {
		r := <-results
		if r.err == nil {
			accepted++
			leases = append(leases, r.lease)
		} else {
			var denied *AdmissionError
			if !errors.As(r.err, &denied) || (denied.Reason != "owner_concurrency" && denied.Reason != "admission_busy") {
				t.Fatalf("unexpected concurrent admission result: %v", r.err)
			}
		}
	}
	if accepted < 1 || accepted > 4 {
		t.Fatalf("replicas admitted %d simultaneously, want at least one and at most four", accepted)
	}
	// A nonblocking admission mutex may refuse a race before capacity is full.
	// Once contention ends, fill the remaining permits and prove the exact cap.
	for len(leases) < 4 {
		lease, fillErr := a.AdmitRequest(t.Context(), owner, limits)
		if fillErr != nil {
			t.Fatalf("remaining owner capacity unavailable after contention: %v", fillErr)
		}
		leases = append(leases, lease)
	}
	_, err = b.AdmitRequest(t.Context(), owner, limits)
	requireAdmissionReason(t, err, "owner_concurrency")
	if _, err = b.AdmitRequest(t.Context(), other, limits); err != nil {
		t.Fatalf("other owner blocked by replica budget: %v", err)
	}
	// Releasing concurrency in either replica must preserve shared rate usage.
	for _, lease := range leases {
		if err = b.ReleaseAdmission(t.Context(), owner, lease.Token); err != nil {
			t.Fatal(err)
		}
	}
	for _, db := range []*Postgres{a, b} {
		lease, admitErr := db.AdmitRequest(t.Context(), owner, limits)
		if admitErr != nil {
			t.Fatalf("remaining request budget denied: %v", admitErr)
		}
		if err = db.ReleaseAdmission(t.Context(), owner, lease.Token); err != nil {
			t.Fatal(err)
		}
	}
	_, err = a.AdmitRequest(t.Context(), owner, limits)
	requireAdmissionReason(t, err, "request_rate")
	account := fixtureAccount(t, a, owner, model.X)
	if _, err = a.AdmitProviderOperation(t.Context(), owner, account.Platform, account.Identity.ID, limits); err != nil {
		t.Fatal(err)
	}
	_, err = b.AdmitProviderOperation(t.Context(), owner, account.Platform, account.Identity.ID, limits)
	requireAdmissionReason(t, err, "account_concurrency")
}
