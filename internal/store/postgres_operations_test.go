package store

import (
	"errors"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

// A busy delivery history must never make the legacy endpoint allocate all rows.
func TestPostgresLegacyEventsBounded(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.X)
	p := fixturePost(t, s, owner, time.Now().Add(time.Hour), a)
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO sp_events(post_id,target_id,kind)
		SELECT $1,$2,'target.processing' FROM generate_series(1,250)`, p.ID, p.Targets[0].ID); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListEvents(t.Context(), owner, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 100 {
		t.Fatalf("legacy events returned %d rows; want bounded first 100", len(events))
	}
}

func TestPostgresEventPagesPreserveCursorAndOwner(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.X)
	p := fixturePost(t, s, owner, time.Now().Add(time.Hour), a)
	if _, err := s.pool.Exec(ctx, `INSERT INTO sp_events(post_id,target_id,kind)
		SELECT $1,$2,'target.processing' FROM generate_series(1,250)`, p.ID, p.Targets[0].ID); err != nil {
		t.Fatal(err)
	}
	first, err := s.ListEventsPage(ctx, owner, p.ID, 0, 0)
	if err != nil || len(first.Events) != 100 || !first.HasMore || first.NextAfterID != first.Events[99].ID {
		t.Fatalf("default first page: rows=%d cursor=%d more=%v err=%v", len(first.Events), first.NextAfterID, first.HasMore, err)
	}
	// A new event between reads must appear later without moving the earlier cursor.
	if err = addEvent(ctx, s.pool, p.ID, p.Targets[0].ID, a.ID, "target.processing", "later event"); err != nil {
		t.Fatal(err)
	}
	second, err := s.ListEventsPage(ctx, owner, p.ID, first.NextAfterID, 100)
	if err != nil || len(second.Events) != 100 || !second.HasMore {
		t.Fatalf("second page: rows=%d more=%v err=%v", len(second.Events), second.HasMore, err)
	}
	third, err := s.ListEventsPage(ctx, owner, p.ID, second.NextAfterID, 100)
	if err != nil || len(third.Events) != 53 || third.HasMore {
		t.Fatalf("last page: rows=%d more=%v err=%v", len(third.Events), third.HasMore, err)
	}
	var previous int64
	for _, page := range []EventPage{first, second, third} {
		for _, event := range page.Events {
			if event.ID <= previous {
				t.Fatal("event pagination repeated or reordered an ID")
			}
			previous = event.ID
		}
	}
	empty, err := s.ListEventsPage(ctx, owner, p.ID, previous, 100)
	if err != nil || len(empty.Events) != 0 || empty.HasMore || empty.NextAfterID != previous {
		t.Fatalf("empty cursor page: %+v err=%v", empty, err)
	}
	maxPage, err := s.ListEventsPage(ctx, owner, p.ID, 0, 1000000)
	if err != nil || len(maxPage.Events) != 200 || !maxPage.HasMore {
		t.Fatalf("maximum page: rows=%d err=%v", len(maxPage.Events), err)
	}
	for _, stranger := range []model.Principal{
		{TenantID: "tenant", Subject: "another"},
		{TenantID: "another", Subject: "subject"},
		{},
	} {
		if _, err = s.ListEventsPage(ctx, stranger, p.ID, first.NextAfterID, 100); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign event page did not hide the post: %v", err)
		}
	}
	if _, err = s.ListEventsPage(ctx, owner, p.ID, -1, 100); !errors.Is(err, ErrConflict) {
		t.Fatalf("negative event cursor accepted: %v", err)
	}
}

func TestPostgresHeartbeatRequiresAdvancingWorkerProgress(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	h := ProcessHeartbeat{InstanceID: "worker-instance", Role: "serve", BuildRevision: "release-one"}
	query := OperationsQuery{InstanceID: h.InstanceID, HeartbeatStaleAfter: time.Minute, WorkerStaleAfter: 2 * time.Minute, Limit: 10}
	if err := s.RecordProcessHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	first, err := s.OperationsSnapshot(ctx, query)
	if err != nil || first.LocalProcess == nil || !first.LocalProcess.HeartbeatFresh || first.LocalProcess.WorkerProgressFresh || first.LocalProcess.WorkerProgressAt != nil {
		t.Fatalf("timer-only process reported worker progress: %+v %v", first.LocalProcess, err)
	}
	h.WorkerProgressSequence = 1
	if err = s.RecordProcessHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	second, err := s.OperationsSnapshot(ctx, query)
	if err != nil || !second.LocalProcess.WorkerProgressFresh || second.LocalProcess.WorkerProgressSequence != 1 {
		t.Fatalf("completed iteration not recorded: %+v %v", second.LocalProcess, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE sp_process_heartbeats SET worker_progress_at=clock_timestamp()-interval '5 minutes',
		last_seen_at=clock_timestamp()-interval '5 minutes' WHERE instance_id=$1`, h.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordProcessHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	stalled, err := s.OperationsSnapshot(ctx, query)
	if err != nil || !stalled.LocalProcess.HeartbeatFresh || stalled.LocalProcess.WorkerProgressFresh || stalled.Processes.StaleWorkers != 1 {
		t.Fatalf("heartbeat disguised a stalled worker: %+v %+v %v", stalled.LocalProcess, stalled.Processes, err)
	}
	h.WorkerProgressSequence = 0
	if err = s.RecordProcessHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	regressed, err := s.OperationsSnapshot(ctx, query)
	if err != nil || regressed.LocalProcess.WorkerProgressSequence != 1 || regressed.LocalProcess.WorkerProgressFresh {
		t.Fatalf("old heartbeat regressed or refreshed progress: %+v %v", regressed.LocalProcess, err)
	}
	h.WorkerProgressSequence = 2
	if err = s.RecordProcessHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.OperationsSnapshot(ctx, query)
	if err != nil || !resumed.LocalProcess.WorkerProgressFresh {
		t.Fatalf("new iteration did not recover worker status: %+v %v", resumed.LocalProcess, err)
	}
	h.BuildRevision = "different-image"
	if err = s.RecordProcessHeartbeat(ctx, h); !errors.Is(err, ErrConflict) {
		t.Fatalf("process identity was reused across releases: %v", err)
	}
	if err = s.RecordProcessHeartbeat(ctx, ProcessHeartbeat{InstanceID: "api", Role: "api", BuildRevision: "release", WorkerProgressSequence: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("api-only process forged worker progress: %v", err)
	}
}

func TestPostgresOperationsMetricsBoundCountsAndKeepOldestDue(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.X)
	var oldest model.Post
	for i := 0; i < 5; i++ {
		post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), a)
		if i == 0 {
			oldest = post
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sp_targets SET next_attempt_at=clock_timestamp()-interval '1 hour' WHERE post_id=$1`, oldest.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		p := fixturePost(t, s, owner, time.Now().Add(time.Hour), a)
		if _, err := s.pool.Exec(ctx, "UPDATE sp_targets SET status='needs_attention' WHERE post_id=$1", p.ID); err != nil {
			t.Fatal(err)
		}
		b := fixtureAccount(t, s, owner, model.TikTok)
		if _, err := s.pool.Exec(ctx, "UPDATE sp_accounts SET status='reauth_required' WHERE id=$1", b.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sp_provider_pacing(account_id,scope,next_allowed_at)
		VALUES ($1,'posting',clock_timestamp()+interval '1 minute')`, a.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old-api", "recent-api", "new-api"} {
		if err := s.RecordProcessHeartbeat(ctx, ProcessHeartbeat{InstanceID: id, Role: "api", BuildRevision: "release"}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := s.OperationsSnapshot(ctx, OperationsQuery{InstanceID: "old-api", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Queue.DueTargets != (BoundedCount{Value: 2, Capped: true}) || snapshot.Queue.NeedsAttention != (BoundedCount{Value: 2, Capped: true}) || snapshot.Queue.ReauthRequired != (BoundedCount{Value: 2, Capped: true}) {
		t.Fatalf("queue metrics did not respect bound: %+v", snapshot.Queue)
	}
	if snapshot.Queue.OldestDueAt == nil || snapshot.Queue.OldestDueAgeSeconds < 3590 || snapshot.Queue.OldestDueAgeSeconds > 3610 || snapshot.Queue.ProviderCooldowns.Value != 1 {
		t.Fatalf("due age or shared cooldown metric incorrect: %+v", snapshot.Queue)
	}
	if !snapshot.Processes.Truncated || snapshot.Processes.Observed != 2 || snapshot.LocalProcess == nil || snapshot.LocalProcess.InstanceID != "old-api" {
		t.Fatalf("global process bound hid local readiness: %+v %+v", snapshot.Processes, snapshot.LocalProcess)
	}
	if snapshot.Pools.Runtime.MaxConns < 1 || snapshot.Pools.AccountLocks.MaxConns < 1 {
		t.Fatalf("database pool capacities not exposed: %+v", snapshot.Pools)
	}
}

func TestPostgresWorkerStartupGraceDoesNotInventOrExtendProgress(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	heartbeat := ProcessHeartbeat{InstanceID: "starting-worker", Role: "serve", BuildRevision: "release"}
	query := OperationsQuery{InstanceID: heartbeat.InstanceID, HeartbeatStaleAfter: time.Minute, WorkerStaleAfter: 2 * time.Minute}
	if err := s.RecordProcessHeartbeat(ctx, heartbeat); err != nil {
		t.Fatal(err)
	}
	starting, err := s.OperationsSnapshot(ctx, query)
	if err != nil || starting.LocalProcess == nil || !starting.LocalProcess.WorkerStarting || starting.LocalProcess.WorkerProgressFresh ||
		starting.LocalProcess.WorkerProgressAt != nil || starting.LocalProcess.WorkerProgressSequence != 0 || starting.Processes.StartingWorkers != 1 {
		t.Fatalf("long first iteration has no explicit startup grace or invented real progress: %+v %+v %v", starting.LocalProcess, starting.Processes, err)
	}
	var originalStart time.Time
	if err = s.pool.QueryRow(ctx, `UPDATE sp_process_heartbeats SET started_at=clock_timestamp()-interval '3 minutes',
		last_seen_at=clock_timestamp()-interval '3 minutes' WHERE instance_id=$1 RETURNING started_at`, heartbeat.InstanceID).Scan(&originalStart); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err = s.RecordProcessHeartbeat(ctx, heartbeat); err != nil {
			t.Fatal(err)
		}
		expired, err := s.OperationsSnapshot(ctx, query)
		if err != nil || !expired.LocalProcess.HeartbeatFresh || expired.LocalProcess.WorkerStarting || expired.LocalProcess.WorkerProgressFresh ||
			expired.LocalProcess.WorkerProgressAt != nil || expired.LocalProcess.WorkerProgressSequence != 0 || !expired.LocalProcess.StartedAt.Equal(originalStart) ||
			expired.Processes.StaleWorkers != 1 || expired.Processes.StartingWorkers != 0 {
			t.Fatalf("heartbeat extended startup grace or fabricated progress: %+v %+v %v", expired.LocalProcess, expired.Processes, err)
		}
	}
	heartbeat.WorkerProgressSequence = 1
	if err = s.RecordProcessHeartbeat(ctx, heartbeat); err != nil {
		t.Fatal(err)
	}
	progressed, err := s.OperationsSnapshot(ctx, query)
	if err != nil || progressed.LocalProcess.WorkerStarting || !progressed.LocalProcess.WorkerProgressFresh {
		t.Fatalf("real worker progress did not replace startup grace: %+v %v", progressed.LocalProcess, err)
	}
	// Once progress has happened, a later stall cannot re-enter startup grace,
	// even if the process remains younger than the configured grace interval.
	if _, err = s.pool.Exec(ctx, `UPDATE sp_process_heartbeats SET started_at=clock_timestamp(),
		worker_progress_at=clock_timestamp()-interval '3 minutes' WHERE instance_id=$1`, heartbeat.InstanceID); err != nil {
		t.Fatal(err)
	}
	stalled, err := s.OperationsSnapshot(ctx, query)
	if err != nil || stalled.LocalProcess.WorkerStarting || stalled.LocalProcess.WorkerProgressFresh {
		t.Fatalf("stalled worker re-entered startup grace after its first completion: %+v %v", stalled.LocalProcess, err)
	}
}
