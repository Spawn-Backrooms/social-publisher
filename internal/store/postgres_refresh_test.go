package store

import (
	"errors"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

func TestPostgresRefreshIntentFencingAndAtomicReplacement(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	a := fixtureAccount(t, s, model.Principal{TenantID: "tenant", Subject: "owner"}, model.X)
	expires := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Microsecond)
	requireError(t, s.SaveToken(ctx, a.ID, a.TokenVersion, []byte("no-intent"), expires), ErrConflict)
	requireError(t, s.BeginTokenRefresh(ctx, a.ID, a.TokenVersion+1), ErrConflict)
	var before, after time.Time
	if err := s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginTokenRefresh(ctx, a.ID, a.TokenVersion); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&after); err != nil {
		t.Fatal(err)
	}
	pending, err := s.GetWorkerAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.TokenRefreshVersion != a.TokenVersion || pending.TokenRefreshStartedAt.Before(before) || pending.TokenRefreshStartedAt.After(after) {
		t.Fatal("refresh intent was not fenced and stamped using database time")
	}
	requireError(t, s.BeginTokenRefresh(ctx, a.ID, a.TokenVersion), ErrConflict)
	requireError(t, s.ClearTokenRefresh(ctx, a.ID, a.TokenVersion+1), ErrConflict)
	requireError(t, s.SaveToken(ctx, a.ID, a.TokenVersion+1, []byte("stale"), expires), ErrConflict)
	if _, err = s.pool.Exec(ctx, "UPDATE sp_accounts SET refresh_token_version=token_version+1 WHERE id=$1", a.ID); err == nil {
		t.Fatal("database accepted a refresh intent for a different credential version")
	}
	if err = s.SaveToken(ctx, a.ID, a.TokenVersion, []byte("replacement"), expires); err != nil {
		t.Fatal(err)
	}
	saved, err := s.GetWorkerAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.TokenVersion != a.TokenVersion+1 || string(saved.EncryptedToken) != "replacement" || !saved.TokenExpiresAt.Equal(expires) || saved.TokenRefreshVersion != 0 || !saved.TokenRefreshStartedAt.IsZero() {
		t.Fatal("token replacement, version advance and intent removal were not atomic")
	}
	requireError(t, s.MarkReauth(ctx, a.ID, a.TokenVersion), ErrConflict)
	requireError(t, s.ClearTokenRefresh(ctx, a.ID, a.TokenVersion), ErrConflict)
	if err = s.BeginTokenRefresh(ctx, a.ID, saved.TokenVersion); err != nil {
		t.Fatal(err)
	}
	if err = s.ClearTokenRefresh(ctx, a.ID, saved.TokenVersion); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.GetWorkerAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Status != "active" || cleared.TokenVersion != saved.TokenVersion+1 || cleared.TokenRefreshVersion != 0 || !cleared.TokenRefreshStartedAt.IsZero() || string(cleared.EncryptedToken) != "replacement" {
		t.Fatal("proven rejection did not retain the credential and advance its fence")
	}
	if err = s.BeginTokenRefresh(ctx, a.ID, cleared.TokenVersion); err != nil {
		t.Fatal(err)
	}
	requireError(t, s.ClearTokenRefresh(ctx, a.ID, saved.TokenVersion), ErrConflict)
	requireError(t, s.SaveToken(ctx, a.ID, saved.TokenVersion, []byte("late-completion"), expires), ErrConflict)
	stillPending, err := s.GetWorkerAccount(ctx, a.ID)
	if err != nil || stillPending.TokenRefreshVersion != cleared.TokenVersion {
		t.Fatal("late completion removed a later attempt's intent")
	}
}

func TestPostgresRefreshReconnectAndDisconnectSupersedeIntent(t *testing.T) {
	for _, action := range []string{"reconnect", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			s, _ := postgresFixture(t, 4)
			ctx := t.Context()
			a := fixtureAccount(t, s, model.Principal{TenantID: "tenant", Subject: "owner"}, model.X)
			if err := s.BeginTokenRefresh(ctx, a.ID, a.TokenVersion); err != nil {
				t.Fatal(err)
			}
			wantStatus, wantBlob := "active", "owner-reconnected"
			if action == "reconnect" {
				reconnect := a
				reconnect.EncryptedToken = []byte(wantBlob)
				reconnect.AuthorizationStartedAt = time.Now().UTC()
				if _, err := s.UpsertAccount(ctx, reconnect); err != nil {
					t.Fatal(err)
				}
			} else {
				wantStatus, wantBlob = "disconnected", ""
				if _, err := s.Disconnect(ctx, a.Owner, a.ID); err != nil {
					t.Fatal(err)
				}
			}
			requireError(t, s.BeginTokenRefresh(ctx, a.ID, a.TokenVersion), ErrConflict)
			requireError(t, s.ClearTokenRefresh(ctx, a.ID, a.TokenVersion), ErrConflict)
			requireError(t, s.SaveToken(ctx, a.ID, a.TokenVersion, []byte("late-refresh"), time.Now().Add(time.Hour)), ErrConflict)
			requireError(t, s.MarkReauth(ctx, a.ID, a.TokenVersion), ErrConflict)
			saved, err := s.GetWorkerAccount(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Status != wantStatus || string(saved.EncryptedToken) != wantBlob || saved.TokenVersion <= a.TokenVersion || saved.TokenRefreshVersion != 0 || !saved.TokenRefreshStartedAt.IsZero() {
				t.Fatal("stale refresh mutated newer owner state or left an orphan intent")
			}
		})
	}
}

func TestPostgresRefreshCandidatesIncludeUnexpiredInterruptedAttempts(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "owner"}
	ordinary := fixtureAccount(t, s, owner, model.X)
	interrupted := fixtureAccount(t, s, owner, model.X)
	otherPlatform := fixtureAccount(t, s, owner, model.TikTok)
	for _, a := range []model.Account{interrupted, otherPlatform} {
		if err := s.BeginTokenRefresh(ctx, a.ID, a.TokenVersion); err != nil {
			t.Fatal(err)
		}
	}
	// This token is far outside maintenance's expiry window; intent age never
	// supplies evidence of a safe rollback of the remote operation.
	if _, err := s.pool.Exec(ctx, `UPDATE sp_accounts SET token_expires_at=clock_timestamp()+interval '30 days',
        refresh_started_at=clock_timestamp()-interval '30 days' WHERE id=$1`, interrupted.ID); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.ListRefreshCandidates(ctx, model.X, time.Now().Add(2*time.Hour), 1)
	if err != nil || len(accounts) != 1 || accounts[0].ID != interrupted.ID {
		t.Fatalf("interrupted intent was not prioritized: count=%d err=%v", len(accounts), err)
	}
	if err = s.MarkReauth(ctx, interrupted.ID, interrupted.TokenVersion); err != nil {
		t.Fatal(err)
	}
	quarantined, err := s.GetWorkerAccount(ctx, interrupted.ID)
	if err != nil || quarantined.Status != "reauth_required" || quarantined.TokenRefreshVersion != 0 || !quarantined.TokenRefreshStartedAt.IsZero() {
		t.Fatal("quarantine did not atomically resolve and fence its old intent")
	}
	accounts, err = s.ListRefreshCandidates(ctx, model.X, time.Now().Add(2*time.Hour), 10)
	if err != nil || len(accounts) != 1 || accounts[0].ID != ordinary.ID {
		t.Fatal("refresh scan included quarantined or cross-platform state")
	}
}

func TestPostgresNativeConcurrentRefreshIntentHasOneWinner(t *testing.T) {
	requireNativePostgres(t)
	s, _ := postgresFixture(t, 8)
	a := fixtureAccount(t, s, model.Principal{TenantID: "tenant", Subject: "owner"}, model.X)
	const workers = 12
	start, results := make(chan struct{}), make(chan error, workers)
	for range workers {
		go func() {
			<-start
			results <- s.BeginTokenRefresh(t.Context(), a.ID, a.TokenVersion)
		}()
	}
	close(start)
	winners := 0
	for range workers {
		if err := <-results; err == nil {
			winners++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d workers were authorized to refresh the same credential", winners)
	}
}

func TestPostgresNativeRefreshCompletionCannotDisableReconnect(t *testing.T) {
	requireNativePostgres(t)
	s, _ := postgresFixture(t, 8)
	for _, completion := range []string{"save", "clear", "quarantine"} {
		t.Run(completion, func(t *testing.T) {
			a := fixtureAccount(t, s, model.Principal{TenantID: "tenant", Subject: "owner"}, model.X)
			if err := s.BeginTokenRefresh(t.Context(), a.ID, a.TokenVersion); err != nil {
				t.Fatal(err)
			}
			newer := a
			newer.EncryptedToken = []byte("new-authorization")
			newer.AuthorizationStartedAt = time.Now().UTC()
			start := make(chan struct{})
			ownerResult, oldResult := make(chan error, 1), make(chan error, 1)
			go func() {
				<-start
				_, err := s.UpsertAccount(t.Context(), newer)
				ownerResult <- err
			}()
			go func() {
				<-start
				switch completion {
				case "save":
					oldResult <- s.SaveToken(t.Context(), a.ID, a.TokenVersion, []byte("late-rotation"), time.Now().Add(time.Hour))
				case "clear":
					oldResult <- s.ClearTokenRefresh(t.Context(), a.ID, a.TokenVersion)
				case "quarantine":
					oldResult <- s.MarkReauth(t.Context(), a.ID, a.TokenVersion)
				}
			}()
			close(start)
			ownerErr, oldErr := <-ownerResult, <-oldResult
			if ownerErr != nil || (oldErr != nil && !errors.Is(oldErr, ErrConflict)) {
				t.Fatalf("owner reconnect=%v, stale completion=%v", ownerErr, oldErr)
			}
			saved, err := s.GetWorkerAccount(t.Context(), a.ID)
			if err != nil || saved.Status != "active" || string(saved.EncryptedToken) != "new-authorization" || saved.TokenRefreshVersion != 0 {
				t.Fatal("stale refresh completion replaced or disabled newer authorization")
			}
		})
	}
}
