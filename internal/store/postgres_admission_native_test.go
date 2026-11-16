package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"socialpublisher/internal/model"
)

func TestPostgresNativeAdmissionContentionPreservesOtherOwner(t *testing.T) {
	requireNativePostgres(t)
	for _, contention := range []string{"owner_row", "first_insert", "provider_mutex"} {
		t.Run(contention, func(t *testing.T) {
			s, dsn := postgresFixture(t, 4)
			owner := model.Principal{TenantID: "tenant", Subject: "contended-owner"}
			other := model.Principal{TenantID: "tenant", Subject: "independent-owner"}
			limits := AdmissionLimits{RequestsPerMinute: 1, OwnerConcurrency: 1}
			account := fixtureAccount(t, s, owner, model.X)
			if contention == "owner_row" {
				lease, err := s.AdmitRequest(t.Context(), owner, limits)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.ReleaseAdmission(t.Context(), owner, lease.Token); err != nil {
					t.Fatal(err)
				}
			}
			// Exclude cold connection startup from the contention timing check.
			warmAdmissionPool(t, s, 4)
			locker, err := pgx.Connect(t.Context(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = locker.Close(cleanup)
			}()
			held, err := locker.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			releaseHeld := func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = held.Rollback(cleanup)
			}
			defer releaseHeld()
			switch contention {
			case "owner_row":
				_, err = held.Exec(t.Context(), `SELECT owner_tenant FROM sp_api_owner_budgets WHERE owner_tenant=$1 AND owner_subject=$2 FOR UPDATE`, owner.TenantID, owner.Subject)
			case "first_insert":
				_, err = held.Exec(t.Context(), `INSERT INTO sp_api_owner_budgets(owner_tenant,owner_subject) VALUES ($1,$2)`, owner.TenantID, owner.Subject)
			case "provider_mutex":
				err = admissionMutex(t.Context(), held, "account", string(account.Platform), account.Identity.ID)
			}
			if err != nil {
				t.Fatal(err)
			}

			type outcome struct {
				err     error
				elapsed time.Duration
			}
			const requests = 12
			results := make(chan outcome, requests)
			start := make(chan struct{})
			ctx, cancelRequests := context.WithTimeout(t.Context(), 3*time.Second)
			var group sync.WaitGroup
			for range requests {
				group.Go(func() {
					<-start
					began := time.Now()
					var admissionErr error
					if contention == "provider_mutex" {
						_, admissionErr = s.AdmitProviderOperation(ctx, owner, account.Platform, account.Identity.ID, limits)
					} else {
						_, admissionErr = s.AdmitRequest(ctx, owner, limits)
					}
					results <- outcome{admissionErr, time.Since(began)}
				})
			}
			defer func() { cancelRequests(); releaseHeld(); group.Wait() }()
			close(start)

			// Broken admission fills the pool; corrected admission promptly
			// refuses A. In either case keep the blocker held while B calls.
			observed := make([]outcome, 0, requests)
			wait := time.NewTimer(100 * time.Millisecond)
			poll := time.NewTicker(time.Millisecond)
		waitForContention:
			for {
				select {
				case result := <-results:
					observed = append(observed, result)
					break waitForContention
				case <-poll.C:
					if s.pool.Stat().AcquiredConns() == 4 {
						break waitForContention
					}
				case <-wait.C:
					break waitForContention
				}
			}
			wait.Stop()
			poll.Stop()
			otherCtx, cancelOther := context.WithTimeout(t.Context(), 250*time.Millisecond)
			began := time.Now()
			lease, err := s.AdmitRequest(otherCtx, other, limits)
			cancelOther()
			if err != nil {
				t.Errorf("owner B could not admit while %s remained held: elapsed=%s err=%v", contention, time.Since(began), err)
			} else if err = s.ReleaseAdmission(t.Context(), other, lease.Token); err != nil {
				t.Fatal(err)
			}
			refusalDeadline := time.NewTimer(350 * time.Millisecond)
			defer refusalDeadline.Stop()
			for len(observed) < requests {
				select {
				case result := <-results:
					observed = append(observed, result)
				case <-refusalDeadline.C:
					t.Fatalf("only %d/%d owner A admissions promptly refused while %s was held", len(observed), requests, contention)
				}
			}
			for _, result := range observed {
				var denied *AdmissionError
				if !errors.As(result.err, &denied) || denied.RetryAfter <= 0 {
					t.Errorf("uncontrolled refusal: %v", result.err)
				}
				if result.elapsed > 350*time.Millisecond {
					t.Errorf("owner A retained resources for %s", result.elapsed)
				}
			}
			// Failure cleanup must release transaction state and local guards.
			releaseHeld()
			if contention == "provider_mutex" {
				lease, err = s.AdmitProviderOperation(t.Context(), owner, account.Platform, account.Identity.ID, limits)
			} else {
				lease, err = s.AdmitRequest(t.Context(), owner, limits)
			}
			if contention == "owner_row" {
				requireAdmissionReason(t, err, "request_rate")
			} else {
				if err != nil {
					t.Fatalf("admission remained blocked after %s cleanup: %v", contention, err)
				}
				if err = s.ReleaseAdmission(t.Context(), owner, lease.Token); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPostgresNativeProviderAdmissionContentionKeepsForeignIdentityHidden(t *testing.T) {
	requireNativePostgres(t)
	s, dsn := postgresFixture(t, 4)
	owner := model.Principal{TenantID: "tenant", Subject: "owner"}
	foreign := model.Principal{TenantID: "tenant", Subject: "foreign"}
	account := fixtureAccount(t, s, owner, model.X)
	locker, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = locker.Close(cleanup)
	}()
	held, err := locker.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = held.Rollback(cleanup)
	}()
	if err = admissionMutex(t.Context(), held, "account", string(account.Platform), account.Identity.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	_, err = s.AdmitProviderOperation(ctx, foreign, account.Platform, account.Identity.ID, AdmissionLimits{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign identity disclosed contention instead of not-found: %v", err)
	}
	_, err = s.AdmitProviderOperation(t.Context(), owner, account.Platform, account.Identity.ID, AdmissionLimits{})
	requireAdmissionReason(t, err, "admission_busy")
}

func TestPostgresNativeAdmissionReleaseCleanupIsBounded(t *testing.T) {
	requireNativePostgres(t)
	s, dsn := postgresFixture(t, 1)
	owner := model.Principal{TenantID: "tenant", Subject: "owner"}
	other := model.Principal{TenantID: "tenant", Subject: "other"}
	lease, err := s.AdmitRequest(t.Context(), owner, AdmissionLimits{})
	if err != nil {
		t.Fatal(err)
	}
	locker, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = locker.Close(cleanup)
	}()
	held, err := locker.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = held.Rollback(cleanup)
	}()
	if _, err = held.Exec(t.Context(), "SELECT token FROM sp_api_admissions WHERE token=$1 FOR UPDATE", lease.Token); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	began := time.Now()
	err = s.ReleaseAdmission(ctx, owner, lease.Token)
	if err == nil || time.Since(began) > 350*time.Millisecond {
		t.Fatalf("cleanup retained the shared connection: elapsed=%s err=%v", time.Since(began), err)
	}
	otherCtx, stop := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer stop()
	if _, err = s.AdmitRequest(otherCtx, other, AdmissionLimits{}); err != nil {
		t.Fatalf("canceled cleanup did not restore another owner's access: %v", err)
	}
}

func TestPostgresNativeAdmissionBoundsPoolWaitAndClearsCanceledGuard(t *testing.T) {
	requireNativePostgres(t)
	for _, canceled := range []bool{false, true} {
		name := "internal_deadline"
		if canceled {
			name = "canceled_parent"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := postgresFixture(t, 1)
			held, err := s.pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer held.Release()
			owner := model.Principal{TenantID: "tenant", Subject: "owner"}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if canceled {
				cancel()
			}
			began := time.Now()
			_, err = s.AdmitRequest(ctx, owner, AdmissionLimits{})
			if err == nil {
				t.Fatal("admission proceeded without a connection")
			}
			if time.Since(began) > 350*time.Millisecond {
				t.Errorf("unbounded pool acquisition: elapsed=%s err=%v", time.Since(began), err)
			}
			held.Release()
			lease, err := s.AdmitRequest(t.Context(), owner, AdmissionLimits{})
			if err != nil {
				t.Fatalf("canceled acquisition retained owner guard: %v", err)
			}
			if err = s.ReleaseAdmission(t.Context(), owner, lease.Token); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func warmAdmissionPool(t *testing.T, s *Postgres, size int) {
	t.Helper()
	connections := make([]*pgxpool.Conn, 0, size)
	defer func() {
		for _, connection := range connections {
			connection.Release()
		}
	}()
	for range size {
		connection, err := s.pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
	}
}
