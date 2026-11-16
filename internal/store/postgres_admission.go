package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"socialpublisher/internal/model"
)

const (
	admissionAcquireTimeout  = 200 * time.Millisecond
	admissionDatabaseTimeout = time.Second
	admissionCleanupTimeout  = 250 * time.Millisecond
)

func admissionBusy() error {
	return &AdmissionError{Reason: "admission_busy", RetryAfter: time.Second}
}

// Only the caller's owner key is used here, including for provider admission.
// This both precedes pool acquisition and avoids disclosing a foreign account's
// activity. Each entry lives only until its short admission computation ends.
func (s *Postgres) tryOwnerAdmission(owner model.Principal) (func(), error) {
	if _, busy := s.admissionInFlight.LoadOrStore(owner, struct{}{}); busy {
		return nil, admissionBusy()
	}
	return func() { s.admissionInFlight.Delete(owner) }, nil
}

func admissionStoreError(err error) error {
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
		return err
	}
	var denied *AdmissionError
	if errors.As(err, &denied) {
		return err
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "55P03" {
		// NOWAIT refusal and the bounded unique-insert lock timeout are both
		// ordinary contention, not permission to proceed without a budget.
		return admissionBusy()
	}
	return errors.Join(ErrAdmissionUnavailable, err)
}

func releaseAdmissionConnection(connection *pgxpool.Conn) {
	raw := connection.Conn()
	if !raw.IsClosed() && !raw.PgConn().IsBusy() && raw.PgConn().TxStatus() == 'I' {
		connection.Release()
		return
	}
	// A canceled pgx connection can still be awaiting asynchronous CancelRequest
	// cleanup. Normal pool destruction retains its slot until that completes.
	// Remove the unusable connection first, then hard-close its transport so
	// neither the pool slot nor server transaction waits on that background work.
	raw = connection.Hijack()
	transport := raw.PgConn().Conn()
	if encrypted, ok := transport.(*tls.Conn); ok {
		transport = encrypted.NetConn()
	}
	_ = transport.Close()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_ = raw.Close(cancelled)
}

func (s *Postgres) beginAdmission(ctx context.Context) (context.Context, pgx.Tx, func(), error) {
	acquire, stopAcquire := context.WithTimeout(ctx, admissionAcquireTimeout)
	connection, err := s.pool.Acquire(acquire)
	stopAcquire()
	if err != nil {
		return nil, nil, nil, err
	}
	// Account for several healthy database round trips independently of pool
	// wait time. The HTTP parent's deadline still caps this SQL context.
	bounded, cancel := context.WithTimeout(ctx, admissionDatabaseTimeout)
	tx, err := connection.Begin(bounded)
	if err != nil {
		cancel()
		releaseAdmissionConnection(connection)
		return nil, nil, nil, err
	}
	cleanup := func() {
		cancel()
		defer releaseAdmissionConnection(connection)
		rollback, stop := context.WithTimeout(context.WithoutCancel(ctx), admissionCleanupTimeout)
		defer stop()
		// Successful rollback keeps the connection reusable. A failed rollback
		// is discarded above without waiting for the pool's background destructor.
		_ = tx.Rollback(rollback)
	}
	// Unique-index conflict checking can wait even when a later row lock uses
	// NOWAIT. Bound that first-insert path as well as any unexpected SQL lock.
	if _, err = tx.Exec(bounded, "SET LOCAL lock_timeout='25ms'; SET LOCAL statement_timeout='750ms'"); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	return bounded, tx, cleanup, nil
}

func newAdmissionToken() (string, error) {
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

// admissionMutex protects only short database admission transactions. Its
// namespace differs from the long-lived account/identity session locks, so a
// slow provider operation never holds this lock or blocks quota evaluation.
func admissionMutexKey(purpose string, parts ...string) (int64, error) {
	values := append([]string{"socialpublisher:admission:" + purpose}, parts...)
	canonical, err := json.Marshal(values)
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256(canonical)
	return int64(binary.BigEndian.Uint64(sum[:8])), nil
}

// Already-admitted queue mutations retain their existing blocking mutex and
// owner/account/parent ordering. Request and provider admission use try below.
func admissionMutex(ctx context.Context, tx pgx.Tx, purpose string, parts ...string) error {
	key, err := admissionMutexKey(purpose, parts...)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key)
	return err
}

func tryAdmissionMutex(ctx context.Context, tx pgx.Tx, purpose string, parts ...string) error {
	key, err := admissionMutexKey(purpose, parts...)
	if err != nil {
		return err
	}
	var acquired bool
	if err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", key).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return admissionBusy()
	}
	return nil
}

func (s *Postgres) AdmitRequest(ctx context.Context, owner model.Principal, limits AdmissionLimits) (admitted AdmissionLease, err error) {
	if !validOwner(owner) {
		return AdmissionLease{}, ErrConflict
	}
	limits, err = limits.normalized()
	if err != nil {
		return AdmissionLease{}, err
	}
	defer func() { err = admissionStoreError(err) }()
	releaseOwner, err := s.tryOwnerAdmission(owner)
	if err != nil {
		return AdmissionLease{}, err
	}
	defer releaseOwner()
	token, err := newAdmissionToken()
	if err != nil {
		return AdmissionLease{}, err
	}
	bounded, tx, cleanup, err := s.beginAdmission(ctx)
	if err != nil {
		return AdmissionLease{}, err
	}
	defer cleanup()
	ctx = bounded
	if err = tryAdmissionMutex(ctx, tx, "owner", owner.TenantID, owner.Subject); err != nil {
		return AdmissionLease{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sp_api_owner_budgets(owner_tenant,owner_subject) VALUES ($1,$2)
        ON CONFLICT (owner_tenant,owner_subject) DO NOTHING`, owner.TenantID, owner.Subject); err != nil {
		return AdmissionLease{}, err
	}
	var started, now time.Time
	var count int
	if err = tx.QueryRow(ctx, `SELECT window_started_at,request_count FROM sp_api_owner_budgets
        WHERE owner_tenant=$1 AND owner_subject=$2 FOR UPDATE NOWAIT`, owner.TenantID, owner.Subject).Scan(&started, &count); err != nil {
		return AdmissionLease{}, err
	}
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return AdmissionLease{}, err
	}
	if !started.Add(time.Minute).After(now) {
		started, count = now, 0
	}
	if count >= limits.RequestsPerMinute {
		return AdmissionLease{}, &AdmissionError{Reason: "request_rate", RetryAfter: started.Add(time.Minute).Sub(now)}
	}
	var active int
	var firstExpiry *time.Time
	if err = tx.QueryRow(ctx, `SELECT count(*),min(expires_at) FROM sp_api_admissions
        WHERE owner_tenant=$1 AND owner_subject=$2 AND scope_kind='request' AND expires_at>$3`, owner.TenantID, owner.Subject, now).Scan(&active, &firstExpiry); err != nil {
		return AdmissionLease{}, err
	}
	if active >= limits.OwnerConcurrency {
		retry := time.Second
		if firstExpiry != nil {
			retry = firstExpiry.Sub(now)
		}
		return AdmissionLease{}, &AdmissionError{Reason: "owner_concurrency", RetryAfter: retry}
	}
	lease := AdmissionLease{Token: token, ExpiresAt: now.Add(admissionLeaseDuration).UTC()}
	if _, err = tx.Exec(ctx, `INSERT INTO sp_api_admissions(token,owner_tenant,owner_subject,scope_kind,scope_key,expires_at)
        VALUES ($1,$2,$3,'request','',$4)`, lease.Token, owner.TenantID, owner.Subject, lease.ExpiresAt); err != nil {
		return AdmissionLease{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE sp_api_owner_budgets SET window_started_at=$3,request_count=$4
        WHERE owner_tenant=$1 AND owner_subject=$2`, owner.TenantID, owner.Subject, started, count+1); err != nil {
		return AdmissionLease{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return AdmissionLease{}, err
	}
	return lease, nil
}

// AdmitProviderOperation takes a canonical platform/provider identity so OAuth
// reconnects and existing-account operations share one cross-replica budget.
// Callers must already have an admitted owner request and either a verified
// owned account or a browser-bound OAuth flow. An unregistered identity is
// allowed only for that first OAuth connection; an existing foreign identity
// never grants a permit or reveals its usage.
func (s *Postgres) AdmitProviderOperation(ctx context.Context, owner model.Principal, platform model.Platform, providerID string, limits AdmissionLimits) (admitted AdmissionLease, err error) {
	if !validOwner(owner) || !validPlatform(platform) || providerID == "" {
		return AdmissionLease{}, ErrConflict
	}
	limits, err = limits.normalized()
	if err != nil {
		return AdmissionLease{}, err
	}
	defer func() { err = admissionStoreError(err) }()
	releaseOwner, err := s.tryOwnerAdmission(owner)
	if err != nil {
		return AdmissionLease{}, err
	}
	defer releaseOwner()
	token, err := newAdmissionToken()
	if err != nil {
		return AdmissionLease{}, err
	}
	bounded, tx, cleanup, err := s.beginAdmission(ctx)
	if err != nil {
		return AdmissionLease{}, err
	}
	defer cleanup()
	ctx = bounded
	var existing model.Principal
	err = tx.QueryRow(ctx, `SELECT owner_tenant,owner_subject FROM sp_accounts WHERE platform=$1 AND provider_user_id=$2`, platform, providerID).Scan(&existing.TenantID, &existing.Subject)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AdmissionLease{}, err
	}
	if err == nil && existing != owner {
		return AdmissionLease{}, ErrNotFound
	}
	// Ownership precedes the global identity guard so foreign identities keep
	// returning 404 even while their legitimate owner's admission is contended.
	if err = tryAdmissionMutex(ctx, tx, "account", string(platform), providerID); err != nil {
		return AdmissionLease{}, err
	}
	canonical, err := json.Marshal([]string{string(platform), providerID})
	if err != nil {
		return AdmissionLease{}, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return AdmissionLease{}, err
	}
	var active int
	var firstExpiry *time.Time
	if err = tx.QueryRow(ctx, `SELECT count(*),min(expires_at) FROM sp_api_admissions
        WHERE scope_kind='account' AND scope_key=$1 AND expires_at>$2`, string(canonical), now).Scan(&active, &firstExpiry); err != nil {
		return AdmissionLease{}, err
	}
	if active >= limits.AccountConcurrency {
		retry := time.Second
		if firstExpiry != nil {
			retry = firstExpiry.Sub(now)
		}
		return AdmissionLease{}, &AdmissionError{Reason: "account_concurrency", RetryAfter: retry}
	}
	lease := AdmissionLease{Token: token, ExpiresAt: now.Add(admissionLeaseDuration).UTC()}
	if _, err = tx.Exec(ctx, `INSERT INTO sp_api_admissions(token,owner_tenant,owner_subject,scope_kind,scope_key,expires_at)
        VALUES ($1,$2,$3,'account',$4,$5)`, token, owner.TenantID, owner.Subject, string(canonical), lease.ExpiresAt); err != nil {
		return AdmissionLease{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return AdmissionLease{}, err
	}
	return lease, nil
}

func (s *Postgres) ReleaseAdmission(ctx context.Context, owner model.Principal, token string) error {
	if !validOwner(owner) || token == "" {
		return ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, admissionCleanupTimeout)
	defer cancel()
	connection, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer releaseAdmissionConnection(connection)
	_, err = connection.Exec(ctx, "DELETE FROM sp_api_admissions WHERE token=$1 AND owner_tenant=$2 AND owner_subject=$3", token, owner.TenantID, owner.Subject)
	return err
}

// ensureQueueCapacity runs inside the same transaction that admits the new
// targets. All transitions from a terminal/reconciliation state back into the
// active queue use this mutex. Completions only reduce the count, so they need
// no mutex and cannot make a concurrent admission exceed the limit.
func (s *Postgres) ensureQueueCapacity(ctx context.Context, tx pgx.Tx, owner model.Principal, additional int) error {
	limit := s.PendingTargetsPerOwner
	if limit == 0 {
		limit = 1000
	}
	if limit < 0 || additional < 1 || !validOwner(owner) {
		return ErrConflict
	}
	if err := admissionMutex(ctx, tx, "queue", owner.TenantID, owner.Subject); err != nil {
		return err
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sp_targets WHERE owner_tenant=$1 AND owner_subject=$2
        AND status IN ('queued','running','processing','retrying')`, owner.TenantID, owner.Subject).Scan(&pending); err != nil {
		return err
	}
	if pending > limit-additional {
		return &AdmissionError{Reason: "queue_capacity", RetryAfter: time.Minute}
	}
	return nil
}
