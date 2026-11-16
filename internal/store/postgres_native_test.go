package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

func requireNativePostgres(t *testing.T) {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is unset; native PostgreSQL concurrency test was not run")
	}
	if os.Getenv("TEST_PGLITE") == "1" {
		t.Skip("PGlite socket bridge shares one backend; independent PostgreSQL transactions/sessions are required for this test")
	}
}

func TestPostgresNativeConcurrentIdempotency(t *testing.T) {
	requireNativePostgres(t)
	s, _ := postgresFixture(t, 8)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	for _, mixed := range []bool{false, true} {
		const workers = 12
		type request struct {
			post model.Post
			hash string
		}
		type response struct {
			id, hash string
			created  bool
			err      error
		}
		requests := make([]request, workers)
		key := testID(t, "concurrent_")
		for i := range requests {
			requests[i] = request{post: model.Post{ID: testID(t, "post_"), Owner: owner, PublishAt: time.Now().Add(time.Hour),
				Targets: []model.Target{{ID: testID(t, "target_"), AccountID: account.ID, Platform: account.Platform, Content: model.Content{Text: "same content"}}}}, hash: "alpha"}
			if mixed && i%2 == 1 {
				requests[i].hash = "beta"
			}
		}
		start := make(chan struct{})
		responses := make(chan response, workers)
		for _, r := range requests {
			go func(r request) {
				<-start
				post, created, err := s.CreatePost(ctx, r.post, key, r.hash)
				responses <- response{id: post.ID, hash: r.hash, created: created, err: err}
			}(r)
		}
		close(start)
		results := make([]response, 0, workers)
		winnerID, winnerHash := "", ""
		createdCount := 0
		for range workers {
			result := <-responses
			results = append(results, result)
			if result.created && result.err == nil {
				winnerID = result.id
				winnerHash = result.hash
				createdCount++
			}
		}
		if createdCount != 1 {
			t.Fatalf("created count=%d; results=%+v", createdCount, results)
		}
		for _, result := range results {
			if result.hash == winnerHash {
				if result.err != nil || result.id != winnerID {
					t.Fatalf("same-content replay failed: %+v", result)
				}
			} else if !errors.Is(result.err, ErrIdempotency) {
				t.Fatalf("different-content conflict failed: %+v", result)
			}
		}
		var posts, targets int
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM sp_posts WHERE idempotency_key=$1", key).Scan(&posts); err != nil {
			t.Fatal(err)
		}
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM sp_targets WHERE post_id=$1", winnerID).Scan(&targets); err != nil {
			t.Fatal(err)
		}
		if posts != 1 || targets != 1 {
			t.Fatalf("duplicate durable rows: posts=%d targets=%d", posts, targets)
		}
	}
}

func TestPostgresNativeOAuthConsumeIsAtomic(t *testing.T) {
	requireNativePostgres(t)
	s, _ := postgresFixture(t, 8)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	state := model.OAuthState{StateHash: "one-use", CookieHash: "browser", Owner: model.Principal{TenantID: "tenant", Subject: "subject"}, Platform: model.X, ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.PutOAuthState(ctx, state); err != nil {
		t.Fatal(err)
	}
	const workers = 12
	start := make(chan struct{})
	results := make(chan error, workers)
	for range workers {
		go func() {
			<-start
			_, err := s.ConsumeOAuthState(ctx, state.StateHash, state.CookieHash, state.Platform)
			results <- err
		}()
	}
	close(start)
	successes := 0
	for range workers {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrOAuthState) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("OAuth state consumed %d times", successes)
	}
}

func TestPostgresNativeIdentityLocksCancelAndRelease(t *testing.T) {
	requireNativePostgres(t)
	s, dsn := postgresFixture(t, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	other, err := New(ctx, dsn, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	guard, release, err := s.AcquireAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// This query must succeed even at maxConns=1 while the advisory-lock pool
	// holds its only connection. Otherwise workers can deadlock checkpoints.
	if _, err = s.GetAccount(guard, owner, account.ID); err != nil {
		t.Fatal(err)
	}
	blockedCtx, blockedCancel := context.WithTimeout(ctx, 150*time.Millisecond)
	_, _, err = other.AcquireIdentity(blockedCtx, account.Platform, account.Identity.ID)
	blockedContextErr := blockedCtx.Err()
	blockedCancel()
	if err == nil {
		t.Fatal("account and provider identity did not share an exclusive lock")
	}
	if !errors.Is(blockedContextErr, context.DeadlineExceeded) {
		t.Fatalf("lock wait did not honor context deadline: %v", err)
	}
	if other.locks.Stat().AcquiredConns() != 0 {
		t.Fatal("cancelled acquisition leaked a pool connection")
	}
	release()
	release()
	_, unlock, err := other.AcquireIdentity(ctx, account.Platform, account.Identity.ID)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	unlock()
	if s.locks.Stat().AcquiredConns() != 0 || other.locks.Stat().AcquiredConns() != 0 {
		t.Fatal("release leaked a pool connection")
	}
}

func TestPostgresNativeClaimsAreExclusive(t *testing.T) {
	requireNativePostgres(t)
	s, _ := postgresFixture(t, 8)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	const workers = 16
	expected := make(map[string]struct{}, workers)
	for range workers {
		// Independent accounts can all be admitted concurrently. Same-account
		// backlog exclusion is exercised by the dedicated dispatch regressions.
		account := fixtureAccount(t, s, owner, model.X)
		post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
		expected[post.Targets[0].ID] = struct{}{}
	}
	type result struct {
		target model.Target
		err    error
	}
	results := make(chan result, workers)
	start := make(chan struct{})
	for range workers {
		go func() {
			<-start
			for {
				target, err := s.Claim(ctx, time.Minute)
				if !errors.Is(err, ErrNoWork) {
					results <- result{target, err}
					return
				}
				// A candidate may become ineligible between the parent-selection
				// snapshot and the target recheck. Retry that observation, as the
				// worker does, while keeping every successful lease live below.
				timer := time.NewTimer(10 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					results <- result{err: ctx.Err()}
					return
				case <-timer.C:
				}
			}
		}()
	}
	close(start)
	seen := make(map[string]string)
	for range workers {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if _, exists := expected[result.target.ID]; !exists || result.target.LeaseToken == "" {
			t.Fatal("claim did not return a fixture target with a lease token")
		}
		if _, exists := seen[result.target.ID]; exists {
			t.Fatal("two workers claimed one live lease")
		}
		seen[result.target.ID] = result.target.LeaseToken
	}
	var running, singleAttempts int
	if err := s.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE attempts=1 AND lease_token IS NOT NULL)
		FROM sp_targets WHERE status='running'`).Scan(&running, &singleAttempts); err != nil {
		t.Fatal(err)
	}
	if running != workers || singleAttempts != workers {
		t.Fatalf("live claims: running=%d, exactly-once attempts=%d, want %d", running, singleAttempts, workers)
	}
	for id, lease := range seen {
		if err := s.FinishTarget(ctx, id, lease, Finish{Status: "published", RemoteID: "remote-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Claim(ctx, time.Minute)
	requireError(t, err, ErrNoWork)
}

func TestPostgresNativeClaimCancelAndRescheduleRaces(t *testing.T) {
	requireNativePostgres(t)
	s, _ := postgresFixture(t, 4)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	for i := 0; i < 8; i++ {
		post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
		var claimed model.Target
		var claimErr, mutationErr error
		var group sync.WaitGroup
		group.Add(2)
		start := make(chan struct{})
		go func() { defer group.Done(); <-start; claimed, claimErr = s.Claim(ctx, time.Minute) }()
		if i%2 == 0 {
			go func() { defer group.Done(); <-start; _, mutationErr = s.CancelPost(ctx, owner, post.ID) }()
		} else {
			go func() {
				defer group.Done()
				<-start
				_, mutationErr = s.ReschedulePost(ctx, owner, post.ID, post.Version, time.Now().Add(time.Hour), "UTC")
			}()
		}
		close(start)
		group.Wait()
		saved, err := s.GetPost(ctx, owner, post.ID)
		if err != nil {
			t.Fatal(err)
		}
		if claimErr == nil {
			if claimed.ID != post.Targets[0].ID || saved.Targets[0].Status != "running" {
				t.Fatal("mutation changed claimed work")
			}
			if i%2 == 1 && !errors.Is(mutationErr, ErrConflict) {
				t.Fatalf("reschedule succeeded after delivery began: %v", mutationErr)
			}
			if i%2 == 0 && mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if err = s.FinishTarget(ctx, claimed.ID, claimed.LeaseToken, Finish{Status: "failed"}); err != nil {
				t.Fatal(err)
			}
		} else {
			requireError(t, claimErr, ErrNoWork)
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if saved.Targets[0].StartedAt != nil {
				t.Fatal("mutation winner retained a delivery start")
			}
			if i%2 == 0 && saved.Targets[0].Status != "cancelled" {
				t.Fatal("cancel winner did not stop pending work")
			}
			if i%2 == 1 && !saved.PublishAt.After(time.Now()) {
				t.Fatal("reschedule winner did not move work to future")
			}
		}
	}
}

func awaitNativeBlockedBy(t *testing.T, ctx context.Context, s *Postgres, pid int32, done <-chan error) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
            WHERE $1::integer=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("account transition did not wait for parent transaction: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestPostgresNativeDisconnectDoesNotDeadlockAuditForeignKey(t *testing.T) {
	requireNativePostgres(t)
	s, _ := postgresFixture(t, 4)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
	_, release, err := s.AcquireAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	worker, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Rollback(context.Background())
	var workerPID int32
	if err = worker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&workerPID); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.Exec(ctx, "SELECT id FROM sp_posts WHERE id=$1 FOR UPDATE", post.ID); err != nil {
		t.Fatal(err)
	}
	disconnected := make(chan error, 1)
	go func() { _, disconnectErr := s.Disconnect(ctx, owner, account.ID); disconnected <- disconnectErr }()
	// Disconnect now owns its account row and waits for this parent. The
	// worker's event FK must still be allowed to take account FOR KEY SHARE.
	awaitNativeBlockedBy(t, ctx, s, workerPID, disconnected)
	if err = addEvent(ctx, worker, post.ID, post.Targets[0].ID, account.ID, "target.test_checkpoint", "native lock-order regression"); err != nil {
		t.Fatalf("parent/account FK deadlock: %v", err)
	}
	if err = worker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-disconnected; err != nil {
		t.Fatalf("disconnect failed after worker released parent: %v", err)
	}
	stored, err := s.GetAccount(ctx, owner, account.ID)
	if err != nil || stored.Status != "disconnected" {
		t.Fatalf("disconnect did not commit: %v", err)
	}
}

func TestPostgresNativeAccountDisableSerializesWithRetryAndResume(t *testing.T) {
	requireNativePostgres(t)
	s, _ := postgresFixture(t, 4)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	for _, reauth := range []bool{false, true} {
		for _, resume := range []bool{false, true} {
			account := fixtureAccount(t, s, owner, model.Instagram)
			post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), account)
			claimed, err := s.Claim(ctx, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			finish := Finish{Status: "failed"}
			if resume {
				finish.Status = "needs_attention"
				finish.Progress = model.Progress{Stage: "processing"}
			}
			if err = s.FinishTarget(ctx, claimed.ID, claimed.LeaseToken, finish); err != nil {
				t.Fatal(err)
			}
			// Reproduce the critical middle of RetryTarget/ResolveTarget: the parent
			// is locked and the old committed account state has been validated.
			retry, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer retry.Rollback(context.Background())
			var retryPID int32
			if err = retry.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&retryPID); err != nil {
				_ = retry.Rollback(ctx)
				t.Fatal(err)
			}
			if _, err = lockOwnedPost(ctx, retry, owner, post.ID); err != nil {
				_ = retry.Rollback(ctx)
				t.Fatal(err)
			}
			var active bool
			if err = retry.QueryRow(ctx, "SELECT status='active' FROM sp_accounts WHERE id=$1", account.ID).Scan(&active); err != nil || !active {
				_ = retry.Rollback(ctx)
				t.Fatalf("expected active account: %v", err)
			}
			disabled := make(chan error, 1)
			go func(account model.Account, reauth bool) {
				if reauth {
					disabled <- s.MarkReauth(ctx, account.ID, account.TokenVersion)
					return
				}
				_, disconnectErr := s.Disconnect(ctx, owner, account.ID)
				disabled <- disconnectErr
			}(account, reauth)
			awaitNativeBlockedBy(t, ctx, s, retryPID, disabled)
			status := "queued"
			if resume {
				status = "processing"
			}
			if _, err = retry.Exec(ctx, "UPDATE sp_targets SET status=$2,next_attempt_at=clock_timestamp() WHERE id=$1", claimed.ID, status); err != nil {
				_ = retry.Rollback(ctx)
				t.Fatal(err)
			}
			if err = retry.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-disabled; err != nil {
				t.Fatal(err)
			}
			saved, err := s.GetPost(ctx, owner, post.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "cancelled"
			if reauth {
				want = "failed"
			}
			if resume {
				want = "needs_attention"
			}
			if saved.Targets[0].Status != want {
				t.Fatalf("new work survived account disable: status=%s want=%s", saved.Targets[0].Status, want)
			}
		}
	}
}

func TestPostgresNativeLockBackendLossCancelsGuard(t *testing.T) {
	requireNativePostgres(t)
	s, _ := postgresFixture(t, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	account := fixtureAccount(t, s, owner, model.Instagram)
	guard, release, err := s.AcquireAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	key := uint64(identityLockKey(account.Platform, account.Identity.ID))
	var pid int32
	if err = s.pool.QueryRow(ctx, `SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted
        AND classid::bigint=$1 AND objid::bigint=$2 AND objsubid=1`, int64(key>>32), int64(uint32(key))).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err = s.pool.QueryRow(ctx, "SELECT pg_terminate_backend($1)", pid).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate lock backend: %v", err)
	}
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	select {
	case <-guard.Done():
	case <-timeout.C:
		t.Fatal("lock session loss did not cancel protected operation within probe bound")
	}
	if !errors.Is(context.Cause(guard), errAccountLockLost) {
		t.Fatalf("unexpected guard cancellation: %v", context.Cause(guard))
	}
	release()
	if s.locks.Stat().AcquiredConns() != 0 {
		t.Fatal("lost lock connection was not discarded")
	}
	replacement, replacementRelease, err := s.AcquireAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer replacementRelease()
	if replacement.Err() != nil {
		t.Fatal("could not acquire healthy replacement guard")
	}
}
