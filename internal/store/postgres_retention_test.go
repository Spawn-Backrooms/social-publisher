package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

func TestPostgresRetentionKeepsUnresolvedEvidenceReceiptsAndIdempotency(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.X)
	resolved := fixturePost(t, s, owner, time.Now().Add(time.Hour), a)
	unresolved := fixturePost(t, s, owner, time.Now().Add(time.Hour), a)
	knownFailure := fixturePost(t, s, owner, time.Now().Add(time.Hour), a)
	if _, err := s.pool.Exec(ctx, `UPDATE sp_targets SET status='published',remote_id='confirmed-receipt',
		progress='{"stage":"polling","data":{"remote_id":"confirmed-receipt"}}' WHERE post_id=$1`, resolved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sp_targets SET status='needs_attention',
		progress='{"stage":"publishing","unsafe":true,"data":{"remote_id":"uncertain-operation"}}' WHERE post_id=$1`, unresolved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sp_targets SET status='failed',
		progress='{"stage":"polling","data":{"remote_id":"unresolved-operation"}}' WHERE post_id=$1`, knownFailure.ID); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := s.pool.QueryRow(ctx, "SELECT idempotency_key FROM sp_posts WHERE id=$1", resolved.ID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	expected := make(map[int64]bool)
	add := func(post model.Post, kind string, days int, keep bool) {
		t.Helper()
		var id int64
		if err := s.pool.QueryRow(ctx, `INSERT INTO sp_events(post_id,target_id,kind,created_at)
			VALUES ($1,$2,$3,clock_timestamp()-$4::double precision*interval '1 day') RETURNING id`,
			post.ID, post.Targets[0].ID, kind, days).Scan(&id); err != nil {
			t.Fatal(err)
		}
		expected[id] = keep
	}
	// The oldest candidates are unresolved. A saved scan cursor must pass them
	// without deleting them or starving the later completed delivery.
	for i := 0; i < 3; i++ {
		add(unresolved, "target.checkpoint", 150, true)
	}
	add(knownFailure, "target.checkpoint", 140, true)
	for _, kind := range []string{"target.queued", "target.claimed", "target.processing", "target.checkpoint"} {
		add(resolved, kind, 120, false)
	}
	for _, kind := range []string{"target.published", "target.confirm_published", "target.confirm_not_published", "target.resume_processing", "target.unknown_future_event"} {
		add(resolved, kind, 120, true)
	}
	add(resolved, "target.processing", 8, true) // Default is 90 days.
	var deleted int64
	for i := 0; i < 5; i++ {
		result, err := s.RetainOperationalRecords(ctx, RetentionPolicy{BatchSize: 2})
		if err != nil {
			t.Fatal(err)
		}
		if result.EventCandidatesScanned > 2 || result.EventsDeleted > 2 {
			t.Fatalf("retention exceeded its per-pass bound: %+v", result)
		}
		deleted += result.EventsDeleted
	}
	if deleted != 4 {
		t.Fatalf("unresolved history starved terminal cleanup: deleted=%d want=4", deleted)
	}
	for id, want := range expected {
		var present bool
		if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM sp_events WHERE id=$1)", id).Scan(&present); err != nil || present != want {
			t.Fatalf("event %d retained=%v want=%v err=%v", id, present, want, err)
		}
	}
	post, err := s.LookupIdempotency(ctx, owner, key, "fixture-hash")
	if err != nil || post.ID != resolved.ID || post.Targets[0].RemoteID != "confirmed-receipt" {
		t.Fatalf("retention erased idempotency or publication receipt: %+v %v", post, err)
	}
	post, err = s.GetPost(ctx, owner, unresolved.ID)
	if err != nil || !post.Targets[0].Progress.Unsafe || post.Targets[0].Progress.Stage != "publishing" || post.Targets[0].Status != "needs_attention" {
		t.Fatalf("retention changed unresolved publication: %+v %v", post, err)
	}
	if _, err = s.RetainOperationalRecords(ctx, RetentionPolicy{EventRetention: 6 * 24 * time.Hour}); !errors.Is(err, ErrConflict) {
		t.Fatalf("retention below seven days accepted: %v", err)
	}
}

func TestPostgresRetentionKeepsPublishedSiblingHistory(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.X)
	b := fixtureAccount(t, s, owner, model.Instagram)
	p := fixturePost(t, s, owner, time.Now().Add(time.Hour), a, b)
	if _, err := s.pool.Exec(ctx, "UPDATE sp_targets SET status='published',remote_id='receipt' WHERE id=$1", p.Targets[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE sp_targets SET status='needs_attention' WHERE id=$1", p.Targets[1].ID); err != nil {
		t.Fatal(err)
	}
	var eventID int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO sp_events(post_id,target_id,kind,created_at)
		VALUES ($1,$2,'target.processing',clock_timestamp()-interval '120 days') RETURNING id`, p.ID, p.Targets[0].ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	result, err := s.RetainOperationalRecords(ctx, RetentionPolicy{})
	if err != nil || result.EventsDeleted != 0 || result.EventCandidatesScanned != 1 {
		t.Fatalf("retention erased the published sibling's history while its post remained unresolved: %+v %v", result, err)
	}
	var exists bool
	if err = s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM sp_events WHERE id=$1)", eventID).Scan(&exists); err != nil || !exists {
		t.Fatalf("mixed-result event missing: %v", err)
	}
}

func TestPostgresNativeRetentionSkipsBusyParentAndRechecksResolution(t *testing.T) {
	requireNativePostgres(t)
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.X)
	p := fixturePost(t, s, owner, time.Now().Add(time.Hour), a)
	if _, err := s.pool.Exec(ctx, "UPDATE sp_targets SET status='failed' WHERE post_id=$1", p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sp_events(post_id,target_id,kind,created_at)
		VALUES ($1,$2,'target.checkpoint',clock_timestamp()-interval '8 days')`, p.ID, p.Targets[0].ID); err != nil {
		t.Fatal(err)
	}
	changing, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer changing.Rollback(context.Background())
	if _, err = changing.Exec(ctx, "SELECT id FROM sp_posts WHERE id=$1 FOR UPDATE", p.ID); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	result, err := s.RetainOperationalRecords(bounded, RetentionPolicy{EventRetention: 7 * 24 * time.Hour})
	cancel()
	if err != nil || result.EventsDeleted != 0 {
		t.Fatalf("retention blocked on or erased a busy parent: %+v %v", result, err)
	}
	if _, err = changing.Exec(ctx, `UPDATE sp_targets SET status='needs_attention',progress='{"stage":"polling","data":{"remote_id":"existing"}}'
		WHERE post_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err = changing.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result, err = s.RetainOperationalRecords(ctx, RetentionPolicy{EventRetention: 7 * 24 * time.Hour})
	if err != nil || result.EventsDeleted != 0 {
		t.Fatalf("newly unresolved checkpoint was erased: %+v %v", result, err)
	}
	if _, err = s.ResolveTarget(ctx, owner, p.ID, p.Targets[0].ID, Resolution{Action: "confirm_published", RemoteID: "existing", Note: "Native retention fixture confirms the existing provider result"}); err != nil {
		t.Fatal(err)
	}
	result, err = s.RetainOperationalRecords(ctx, RetentionPolicy{EventRetention: 7 * 24 * time.Hour})
	if err != nil || result.EventsDeleted != 1 {
		t.Fatalf("seven-day retention did not reconsider safely resolved checkpoint: %+v %v", result, err)
	}
}

func TestPostgresRetentionBoundsExpiredOperationalRecords(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.X)
	for _, stmt := range []string{
		`INSERT INTO sp_api_admissions(token,owner_tenant,owner_subject,scope_kind,scope_key,expires_at)
			SELECT 'expired-'||i,'tenant','expired','request','',clock_timestamp()-interval '1 minute' FROM generate_series(1,3) i`,
		`INSERT INTO sp_api_admissions(token,owner_tenant,owner_subject,scope_kind,scope_key,expires_at)
			VALUES ('live','tenant','live','request','',clock_timestamp()+interval '1 hour')`,
		`INSERT INTO sp_api_owner_budgets(owner_tenant,owner_subject,window_started_at,request_count)
			VALUES ('tenant','expired',clock_timestamp()-interval '2 minutes',5),
			('tenant','live',clock_timestamp()-interval '2 minutes',5),('tenant','new',clock_timestamp(),5)`,
		`INSERT INTO sp_oauth_states(state_hash,cookie_hash,owner_tenant,owner_subject,platform,expires_at)
			SELECT 'old-'||i,'binding','tenant','subject','x',clock_timestamp()-interval '1 minute' FROM generate_series(1,3) i`,
		`INSERT INTO sp_oauth_states(state_hash,cookie_hash,owner_tenant,owner_subject,platform,expires_at)
			VALUES ('live','binding','tenant','subject','x',clock_timestamp()+interval '1 hour')`,
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sp_provider_pacing(account_id,scope,next_allowed_at)
		VALUES ($1,'expired',clock_timestamp()-interval '1 minute'),($1,'live',clock_timestamp()+interval '1 hour')`, a.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "live"} {
		if err := s.RecordProcessHeartbeat(ctx, ProcessHeartbeat{InstanceID: id, Role: "api", BuildRevision: "release"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, "UPDATE sp_process_heartbeats SET last_seen_at=clock_timestamp()-interval '8 days' WHERE instance_id='old'"); err != nil {
		t.Fatal(err)
	}
	result, err := s.RetainOperationalRecords(ctx, RetentionPolicy{BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.AdmissionsDeleted != 2 || result.OAuthStatesDeleted != 2 || result.OwnerBudgetsDeleted != 1 || result.ProviderPacingDeleted != 1 || result.HeartbeatsDeleted != 1 {
		t.Fatalf("wrong bounded expiration result: %+v", result)
	}
	for _, check := range []struct {
		query string
		want  int
	}{
		{"SELECT count(*) FROM sp_api_admissions", 2},
		{"SELECT count(*) FROM sp_oauth_states", 2},
		{"SELECT count(*) FROM sp_api_owner_budgets WHERE owner_subject IN ('live','new')", 2},
		{"SELECT count(*) FROM sp_provider_pacing WHERE scope='live'", 1},
		{"SELECT count(*) FROM sp_process_heartbeats WHERE instance_id='live'", 1},
	} {
		var count int
		if err = s.pool.QueryRow(ctx, check.query).Scan(&count); err != nil || count != check.want {
			t.Fatalf("live/expired records changed incorrectly: count=%d want=%d err=%v", count, check.want, err)
		}
	}
}
