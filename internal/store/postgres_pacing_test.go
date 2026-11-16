package store

import (
	"testing"
	"time"

	"socialpublisher/internal/model"
)

func TestPostgresProviderPacingIsSharedAndDeniedRequestsDoNotExtendIt(t *testing.T) {
	s, dsn := postgresFixture(t, 4)
	ctx := t.Context()
	other, err := New(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.TikTok)
	b := fixtureAccount(t, s, owner, model.TikTok)
	wait, err := s.ReserveProviderRequest(ctx, a.ID, "tiktok.status", 2100*time.Millisecond)
	if err != nil || wait != 0 {
		t.Fatalf("first request was not admitted: %v %v", wait, err)
	}
	var first time.Time
	if err := s.pool.QueryRow(ctx, `SELECT next_allowed_at FROM sp_provider_pacing WHERE account_id=$1 AND scope='tiktok.status'`, a.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		wait, err = other.ReserveProviderRequest(ctx, a.ID, "tiktok.status", 2100*time.Millisecond)
		if err != nil || wait <= 0 || wait > 2100*time.Millisecond {
			t.Fatalf("another replica bypassed the account budget: %v %v", wait, err)
		}
	}
	var afterDenials time.Time
	if err := s.pool.QueryRow(ctx, `SELECT next_allowed_at FROM sp_provider_pacing WHERE account_id=$1 AND scope='tiktok.status'`, a.ID).Scan(&afterDenials); err != nil {
		t.Fatal(err)
	}
	if !afterDenials.Equal(first) {
		t.Fatal("denied targets kept extending the reservation")
	}
	if wait, err = other.ReserveProviderRequest(ctx, b.ID, "tiktok.status", 2100*time.Millisecond); err != nil || wait != 0 {
		t.Fatalf("unrelated account inherited the budget: %v %v", wait, err)
	}
	if wait, err = other.SetProviderCooldown(ctx, a.ID, "tiktok.status", time.Minute); err != nil || wait < 59*time.Second {
		t.Fatalf("provider cooldown was not retained: %v %v", wait, err)
	}
	if wait, err = s.SetProviderCooldown(ctx, a.ID, "tiktok.status", time.Second); err != nil || wait < 59*time.Second {
		t.Fatalf("shorter cooldown reset the provider deadline: %v %v", wait, err)
	}
	if wait, err = s.ReserveProviderRequest(ctx, a.ID, "tiktok.status", 2100*time.Millisecond); err != nil || wait < 59*time.Second {
		t.Fatalf("replica bypassed saved provider cooldown: %v %v", wait, err)
	}
}

func TestPostgresAccountCooldownExcludesBacklogButNotExpiredProcessing(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.TikTok)
	b := fixtureAccount(t, s, owner, model.X)
	var old model.Post
	for i := 0; i < 20; i++ {
		old = fixturePost(t, s, owner, time.Now().Add(-time.Hour), a)
	}
	other := fixturePost(t, s, owner, time.Now().Add(-time.Minute), b)
	if _, err := s.SetProviderCooldown(ctx, a.ID, "platform:tiktok", time.Hour); err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(ctx, time.Minute)
	if err != nil || claim.ID != other.Targets[0].ID {
		t.Fatalf("cooled-down account hid other work: %+v %v", claim, err)
	}
	if err = s.FinishTarget(ctx, claim.ID, claim.LeaseToken, Finish{Status: "published", RemoteID: "other-receipt"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Claim(ctx, time.Minute)
	requireError(t, err, ErrNoWork)
	if _, err = s.pool.Exec(ctx, `UPDATE sp_targets SET status='processing',progress='{"stage":"tiktok_poll","data":{"publish_id":"existing"}}',
		processing_started_at=clock_timestamp()-interval '24 hours',next_attempt_at=clock_timestamp() WHERE id=$1`, old.Targets[0].ID); err != nil {
		t.Fatal(err)
	}
	claim, err = s.Claim(ctx, time.Minute)
	if err != nil || claim.ID != old.Targets[0].ID || claim.ProcessingStartedAt == nil || claim.ClaimedAt.Sub(*claim.ProcessingStartedAt) < 24*time.Hour {
		t.Fatalf("provider cooldown postponed fixed processing deadline: %+v %v", claim, err)
	}
}

func TestPostgresRetryDelayUsesDatabaseClockAndPreservesProcessingDeadline(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.TikTok)
	post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), a)
	claim, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var dbNow time.Time
	if err = s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTarget(ctx, claim.ID, claim.LeaseToken, Finish{Status: "retrying", RetryAfter: 5 * time.Second, NextAttemptAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	saved, err := s.GetPost(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Targets[0].NextAttemptAt.After(dbNow.Add(4*time.Second)) || saved.Targets[0].NextAttemptAt.After(dbNow.Add(10*time.Second)) {
		t.Fatal("host absolute time overrode the database retry delay")
	}
	if _, err = s.pool.Exec(ctx, `UPDATE sp_targets SET next_attempt_at=clock_timestamp() WHERE id=$1`, claim.ID); err != nil {
		t.Fatal(err)
	}
	claim, err = s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	known := model.Progress{Stage: "tiktok_poll"}
	if err = s.SaveProgress(ctx, claim.ID, claim.LeaseToken, known); err != nil {
		t.Fatal(err)
	}
	var started time.Time
	if err = s.pool.QueryRow(ctx, `UPDATE sp_targets SET processing_started_at=clock_timestamp()-interval '23 hours 59 minutes 30 seconds'
		WHERE id=$1 RETURNING processing_started_at`, claim.ID).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTarget(ctx, claim.ID, claim.LeaseToken, Finish{Status: "processing", Progress: known, RetryAfter: time.Hour}); err != nil {
		t.Fatal(err)
	}
	saved, err = s.GetPost(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := saved.Targets[0]
	if target.ProcessingStartedAt == nil || !target.ProcessingStartedAt.Equal(started) || !target.NextAttemptAt.Equal(started.Add(24*time.Hour)) {
		t.Fatalf("a wait renewed or overshot the processing window: %+v", target)
	}
}
