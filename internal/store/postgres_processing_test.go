package store

import (
	"encoding/json"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

func TestPostgresProcessingWindowStartsWithProviderProgress(t *testing.T) {
	for _, progress := range []model.Progress{
		{Stage: "polling", Data: json.RawMessage(`{"remote_id":"known-operation"}`)},
		{Unsafe: true},
	} {
		t.Run(map[bool]string{false: "known_operation", true: "uncertain_mutation"}[progress.Unsafe], func(t *testing.T) {
			s, _ := postgresFixture(t, 4)
			ctx := t.Context()
			owner := model.Principal{TenantID: "tenant", Subject: "subject"}
			account := fixtureAccount(t, s, owner, model.X)
			post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
			target, err := s.Claim(ctx, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if target.ProcessingStartedAt != nil {
				t.Fatal("claiming before provider work consumed the processing window")
			}
			firstStart := *target.StartedAt
			if err = s.SaveProgress(ctx, target.ID, target.LeaseToken, model.Progress{}); err != nil {
				t.Fatal(err)
			}
			empty, err := s.GetPost(ctx, owner, post.ID)
			if err != nil || empty.Targets[0].ProcessingStartedAt != nil {
				t.Fatalf("empty checkpoint consumed the processing window: %v", err)
			}
			var before time.Time
			if err = s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err = s.SaveProgress(ctx, target.ID, target.LeaseToken, progress); err != nil {
				t.Fatal(err)
			}
			saved, err := s.GetPost(ctx, owner, post.ID)
			if err != nil {
				t.Fatal(err)
			}
			window := saved.Targets[0].ProcessingStartedAt
			if window == nil || window.Before(before) {
				t.Fatal("first provider checkpoint did not open a database-clock processing window")
			}
			if saved.Targets[0].StartedAt == nil || !saved.Targets[0].StartedAt.Equal(firstStart) {
				t.Fatal("provider checkpoint changed first-start history")
			}
			if err = s.SaveProgress(ctx, target.ID, target.LeaseToken, progress); err != nil {
				t.Fatal(err)
			}
			saved, err = s.GetPost(ctx, owner, post.ID)
			if err != nil || saved.Targets[0].ProcessingStartedAt == nil || !saved.Targets[0].ProcessingStartedAt.Equal(*window) {
				t.Fatalf("later checkpoint extended the processing window: %v", err)
			}
		})
	}
}

func TestPostgresProvenRejectionClearsProcessingWindowBeforeAutomaticRetry(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.TikTok)
	post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
	target, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProgress(ctx, target.ID, target.LeaseToken, model.Progress{Stage: "initializing", Unsafe: true}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-25 * time.Hour).Truncate(time.Microsecond)
	if _, err = s.pool.Exec(ctx, "UPDATE sp_targets SET started_at=$2,processing_started_at=$2 WHERE id=$1", target.ID, old); err != nil {
		t.Fatal(err)
	}
	// Only a durably recorded definite rejection authorizes this empty safe
	// checkpoint. An ordinary Finish must continue preserving unsafe progress.
	if err = s.SaveProgress(ctx, target.ID, target.LeaseToken, model.Progress{}); err != nil {
		t.Fatal(err)
	}
	rejected, err := s.GetPost(ctx, owner, post.ID)
	if err != nil || rejected.Targets[0].ProcessingStartedAt != nil {
		t.Fatalf("proven rejection left an expired processing window attached to empty progress: %v", err)
	}
	if err = s.FinishTarget(ctx, target.ID, target.LeaseToken, Finish{Status: "retrying"}); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ProcessingStartedAt != nil || fresh.StartedAt == nil || !fresh.StartedAt.Equal(old) {
		t.Fatal("safe automatic retry inherited the rejected operation's processing window or changed history")
	}
	var before time.Time
	if err = s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	known := model.Progress{Stage: "polling", Data: json.RawMessage(`{"publish_id":"new-accepted-operation"}`)}
	if err = s.SaveProgress(ctx, fresh.ID, fresh.LeaseToken, known); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTarget(ctx, fresh.ID, fresh.LeaseToken, Finish{Status: "processing", Progress: known}); err != nil {
		t.Fatal(err)
	}
	poll, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if poll.ProcessingStartedAt == nil || poll.ProcessingStartedAt.Before(before) || poll.StartedAt == nil || !poll.StartedAt.Equal(old) {
		t.Fatal("new accepted operation did not receive a fresh processing window")
	}
}

func TestPostgresRetryOpensFreshProcessingWindowWithoutChangingHistory(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
	target, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// The first delivery began yesterday and reached a proven safe failure.
	old := time.Now().UTC().Add(-25 * time.Hour).Truncate(time.Microsecond)
	if _, err = s.pool.Exec(ctx, "UPDATE sp_targets SET started_at=$2 WHERE id=$1", target.ID, old); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProgress(ctx, target.ID, target.LeaseToken, model.Progress{Unsafe: true}); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProgress(ctx, target.ID, target.LeaseToken, model.Progress{}); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTarget(ctx, target.ID, target.LeaseToken, Finish{Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	// Old failed rows can retain an earlier processing clock. Explicit retry
	// clears it even when an older checkpoint path did not do so.
	if _, err = s.pool.Exec(ctx, "UPDATE sp_targets SET processing_started_at=$2 WHERE id=$1", target.ID, old); err != nil {
		t.Fatal(err)
	}
	retried, err := s.RetryTarget(ctx, owner, post.ID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Targets[0].ProcessingStartedAt != nil || retried.Targets[0].StartedAt == nil || !retried.Targets[0].StartedAt.Equal(old) {
		t.Fatal("explicit retry did not clear its processing window while preserving first-start history")
	}
	fresh, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ProcessingStartedAt != nil || fresh.StartedAt == nil || !fresh.StartedAt.Equal(old) {
		t.Fatal("reclaim changed first-start history or opened the window before provider work")
	}
	var before time.Time
	if err = s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	known := model.Progress{Stage: "polling", Data: json.RawMessage(`{"remote_id":"fresh-operation"}`)}
	if err = s.SaveProgress(ctx, fresh.ID, fresh.LeaseToken, known); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTarget(ctx, fresh.ID, fresh.LeaseToken, Finish{Status: "processing", Progress: known}); err != nil {
		t.Fatal(err)
	}
	poll, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if poll.ProcessingStartedAt == nil || poll.ProcessingStartedAt.Before(before) || poll.StartedAt == nil || !poll.StartedAt.Equal(old) || poll.Progress.Stage != "polling" {
		t.Fatal("new provider operation inherited yesterday's expired processing window")
	}
}

func TestPostgresResumeRenewsProcessingWindowAndPreservesOperation(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
	target, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-25 * time.Hour).Truncate(time.Microsecond)
	if _, err = s.pool.Exec(ctx, "UPDATE sp_targets SET started_at=$2 WHERE id=$1", target.ID, old); err != nil {
		t.Fatal(err)
	}
	known := model.Progress{Stage: "polling", Data: json.RawMessage(`{"remote_id":"same-operation"}`)}
	if err = s.FinishTarget(ctx, target.ID, target.LeaseToken, Finish{Status: "needs_attention", Progress: known, RemoteID: "same-operation"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE sp_targets SET processing_started_at=$2 WHERE id=$1", target.ID, old); err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err = s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	saved, err := s.ResolveTarget(ctx, owner, post.ID, target.ID, Resolution{Action: "resume_processing", Note: "Continue polling the existing operation after checking the provider"})
	if err != nil {
		t.Fatal(err)
	}
	resumed := saved.Targets[0]
	if resumed.ProcessingStartedAt == nil || resumed.ProcessingStartedAt.Before(before) || resumed.StartedAt == nil || !resumed.StartedAt.Equal(old) {
		t.Fatal("explicit resume did not renew the processing window independently of first-start history")
	}
	var data map[string]string
	if err = json.Unmarshal(resumed.Progress.Data, &data); err != nil || resumed.Progress.Stage != known.Stage || data["remote_id"] != "same-operation" || resumed.RemoteID != "same-operation" {
		t.Fatal("explicit resume discarded the existing provider operation")
	}
}

func TestPostgresConfirmedAbsenceClearsProcessingWindow(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.X)
	post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
	target, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProgress(ctx, target.ID, target.LeaseToken, model.Progress{Unsafe: true}); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTarget(ctx, target.ID, target.LeaseToken, Finish{Status: "needs_attention"}); err != nil {
		t.Fatal(err)
	}
	attention, err := s.GetPost(ctx, owner, post.ID)
	if err != nil || attention.Targets[0].ProcessingStartedAt == nil {
		t.Fatalf("uncertain operation lost its processing window: %v", err)
	}
	saved, err := s.ResolveTarget(ctx, owner, post.ID, target.ID, Resolution{Action: "confirm_not_published", Note: "Provider confirmed the operation was rejected"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Targets[0].ProcessingStartedAt != nil || saved.Targets[0].StartedAt == nil || !saved.Targets[0].StartedAt.Equal(*target.StartedAt) || saved.Targets[0].Status != "failed" {
		t.Fatal("confirmed absence did not clear the processing window and preserve first-start history")
	}
}

func TestPostgresFinishStartsProcessingWindowForKnownOperation(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.X)
	post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
	target, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err = s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	known := model.Progress{Stage: "polling", Data: json.RawMessage(`{"remote_id":"accepted-operation"}`)}
	if err = s.FinishTarget(ctx, target.ID, target.LeaseToken, Finish{Status: "processing", Progress: known}); err != nil {
		t.Fatal(err)
	}
	saved, err := s.GetPost(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Targets[0].ProcessingStartedAt == nil || saved.Targets[0].ProcessingStartedAt.Before(before) {
		t.Fatal("known provider operation returned by finish has no processing window")
	}
}

func TestPostgresProcessingWindowMigrationPreservesExistingOperationAge(t *testing.T) {
	s, dsn := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.X)
	old := time.Now().UTC().Add(-25 * time.Hour).Truncate(time.Microsecond)
	type existing struct {
		postID string
		known  bool
	}
	posts := make([]existing, 0, 3)
	for _, progress := range []model.Progress{
		{},
		{Stage: "polling", Data: json.RawMessage(`{"remote_id":"existing-operation"}`)},
		{Unsafe: true},
	} {
		post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
		target, err := s.Claim(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(ctx, "UPDATE sp_targets SET started_at=$2 WHERE id=$1", target.ID, old); err != nil {
			t.Fatal(err)
		}
		if err = s.FinishTarget(ctx, target.ID, target.LeaseToken, Finish{Status: "needs_attention", Progress: progress}); err != nil {
			t.Fatal(err)
		}
		posts = append(posts, existing{postID: post.ID, known: progress.Stage != "" || progress.Unsafe})
	}
	// Recreate the version-1 schema in this disposable test database, retaining
	// actual persisted targets, before applying the forward-only migration again.
	if _, err := s.pool.Exec(ctx, "ALTER TABLE sp_targets DROP COLUMN processing_started_at"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "DELETE FROM sp_schema_migrations WHERE version=2"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	for _, expected := range posts {
		post, err := s.GetPost(ctx, owner, expected.postID)
		if err != nil {
			t.Fatal(err)
		}
		target := post.Targets[0]
		if target.StartedAt == nil || !target.StartedAt.Equal(old) {
			t.Fatal("migration changed first-start history")
		}
		if expected.known {
			if target.ProcessingStartedAt == nil || !target.ProcessingStartedAt.Equal(old) {
				t.Fatal("migration gave an existing known or uncertain operation a fresh processing window")
			}
		} else if target.ProcessingStartedAt != nil {
			t.Fatal("migration opened a processing window for empty progress")
		}
	}
}
