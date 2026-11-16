package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

func TestPostgresNativeTryAccountAdmissionIsBoundedAndGuardSurvives(t *testing.T) {
	requireNativePostgres(t)
	s, dsn := postgresFixture(t, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.X)
	b := fixtureAccount(t, s, owner, model.X)
	other, err := New(ctx, dsn, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	guard, release, err := s.AcquireAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	start := time.Now()
	_, _, err = other.TryAcquireAccount(ctx, a.ID)
	if !errors.Is(err, ErrAccountBusy) || time.Since(start) > time.Second {
		t.Fatalf("contended advisory lock blocked admission: %v after %v", err, time.Since(start))
	}
	start = time.Now()
	_, _, err = s.TryAcquireAccount(ctx, b.ID)
	if !errors.Is(err, ErrAccountBusy) || time.Since(start) > time.Second {
		t.Fatalf("saturated lock pool blocked admission: %v after %v", err, time.Since(start))
	}
	if guard.Err() != nil {
		t.Fatal("a denied contender cancelled the winner's guard")
	}
	release()
	winner, unlock, err := other.TryAcquireAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	select {
	case <-winner.Done():
		t.Fatal("successful admission retained its short acquisition deadline")
	case <-time.After(350 * time.Millisecond):
	}
	if _, err = other.GetWorkerAccount(winner, a.ID); err != nil {
		t.Fatal(err)
	}
	unlock()
	if other.locks.Stat().AcquiredConns() != 0 || s.locks.Stat().AcquiredConns() != 0 {
		t.Fatal("denied or released admission leaked a lock session")
	}
}

func TestPostgresNativeProviderPacingFirstReservationRace(t *testing.T) {
	requireNativePostgres(t)
	s, dsn := postgresFixture(t, 8)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	other, err := New(ctx, dsn, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	a := fixtureAccount(t, s, model.Principal{TenantID: "tenant", Subject: "subject"}, model.TikTok)
	const count = 16
	// Keep every racing call in one slot even on a slow native test host. The
	// real TikTok/worker integration separately measures the 2.1-second gap.
	const spacing = time.Minute
	var workers sync.WaitGroup
	start := make(chan struct{})
	type result struct {
		wait time.Duration
		err  error
	}
	results := make(chan result, count)
	for i := 0; i < count; i++ {
		workers.Add(1)
		db := s
		if i%2 == 1 {
			db = other
		}
		go func() {
			defer workers.Done()
			<-start
			wait, err := db.ReserveProviderRequest(ctx, a.ID, "tiktok.status", spacing)
			results <- result{wait, err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	admitted := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.wait == 0 {
			admitted++
		} else if result.wait > spacing {
			t.Fatalf("reservation race extended the fixed slot: %v", result.wait)
		}
	}
	if admitted != 1 {
		t.Fatalf("independent replicas admitted %d requests into one account slot", admitted)
	}
}

func TestPostgresNativeTryAccountGuardCancelsAfterBackendLoss(t *testing.T) {
	requireNativePostgres(t)
	s, _ := postgresFixture(t, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	a := fixtureAccount(t, s, model.Principal{TenantID: "tenant", Subject: "subject"}, model.X)
	guard, release, err := s.TryAcquireAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	key := uint64(identityLockKey(a.Platform, a.Identity.ID))
	var pid int32
	if err = s.pool.QueryRow(ctx, `SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted
		AND classid::bigint=$1 AND objid::bigint=$2 AND objsubid=1`, int64(key>>32), int64(uint32(key))).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err = s.pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate lock backend: %v", err)
	}
	select {
	case <-guard.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("try-acquired lock did not monitor session loss")
	}
	if !errors.Is(context.Cause(guard), errAccountLockLost) {
		t.Fatalf("wrong lock loss cause: %v", context.Cause(guard))
	}
	release()
	if s.locks.Stat().AcquiredConns() != 0 {
		t.Fatal("lost try-acquired session was not discarded")
	}
}

func TestPostgresNativePacingReservationSurvivesConcurrentRetention(t *testing.T) {
	requireNativePostgres(t)
	s, _ := postgresFixture(t, 4)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	a := fixtureAccount(t, s, model.Principal{TenantID: "tenant", Subject: "subject"}, model.TikTok)
	if _, err := s.pool.Exec(ctx, `INSERT INTO sp_provider_pacing(account_id,scope,next_allowed_at)
		VALUES ($1,'tiktok.status',clock_timestamp()-interval '1 second')`, a.ID); err != nil {
		t.Fatal(err)
	}
	// A statement trigger creates a real interleaving after the reservation's
	// initial UPSERT. Retention must not delete the row before its deadline is
	// read and advanced, including when the previous reservation has expired.
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION pause_pacing_reservation() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_sleep(0.75); RETURN NULL; END $$;
		CREATE TRIGGER pause_pacing_insert AFTER INSERT ON sp_provider_pacing
		FOR EACH STATEMENT EXECUTE FUNCTION pause_pacing_reservation()`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		wait time.Duration
		err  error
	}
	done := make(chan result, 1)
	go func() {
		wait, err := s.ReserveProviderRequest(ctx, a.ID, "tiktok.status", time.Minute)
		done <- result{wait, err}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var paused bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event='PgSleep' AND query LIKE '%INSERT INTO sp_provider_pacing%')`).Scan(&paused); err != nil {
			t.Fatal(err)
		}
		if paused {
			break
		}
		select {
		case result := <-done:
			t.Fatalf("reservation ended before its retention barrier: %v", result.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	cleanup, cleanupErr := s.RetainOperationalRecords(ctx, RetentionPolicy{BatchSize: 10})
	reserved := <-done
	if cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if reserved.err != nil || reserved.wait != 0 || cleanup.ProviderPacingDeleted != 0 {
		t.Fatalf("retention erased a live reservation: wait=%v reserve_error=%v pacing_rows_deleted=%d", reserved.wait, reserved.err, cleanup.ProviderPacingDeleted)
	}
	if wait, err := s.ReserveProviderRequest(ctx, a.ID, "tiktok.status", time.Minute); err != nil || wait <= 0 {
		t.Fatalf("retained reservation did not gate the next request: %v %v", wait, err)
	}
}
