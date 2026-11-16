package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"socialpublisher/internal/model"
)

// Postgres uses a separate bounded pool for session advisory locks. Holding an
// account lock must not consume the last connection needed to save a checkpoint.
type Postgres struct {
	pool      *pgxpool.Pool
	locks     *pgxpool.Pool
	closeOnce sync.Once
	// At most one short database-admission computation per owner uses this pool.
	admissionInFlight sync.Map
	// PendingTargetsPerOwner bounds queued work; zero uses the default of 1000.
	PendingTargetsPerOwner int
}

var _ Store = (*Postgres)(nil)

func New(ctx context.Context, dsn string, maxConns int32) (*Postgres, error) {
	if maxConns < 1 {
		return nil, fmt.Errorf("database max connections must be positive")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database configuration: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	lockCfg := cfg.Copy()
	locks, err := pgxpool.NewWithConfig(ctx, lockCfg)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s := &Postgres{pool: pool, locks: locks}
	if err = pool.Ping(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return s, nil
}

func (s *Postgres) Ping(ctx context.Context) error { return s.checkReadiness(ctx) }
func (s *Postgres) Close()                         { s.closeOnce.Do(func() { s.pool.Close(); s.locks.Close() }) }

type querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}
type scanner interface{ Scan(...any) error }

const accountColumns = `id, owner_tenant, owner_subject, platform, identity, status,
    encrypted_token, token_version, token_expires_at, created_at, updated_at,
    COALESCE(refresh_token_version, 0), refresh_started_at`
const postColumns = `id, owner_tenant, owner_subject, status, publish_at, timezone, version, created_at`
const targetColumns = `id, post_id, account_id, platform, content, status, progress, attempts,
    failures, next_attempt_at, started_at, processing_started_at, remote_id, remote_url, last_error, warnings,
    COALESCE(lease_token, ''), lease_until`

func storeError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505", "23503", "23514", "23502", "22P02":
			return ErrConflict
		}
	}
	return err
}
func validOwner(owner model.Principal) bool { return owner.TenantID != "" && owner.Subject != "" }
func hasKnownProgress(progress model.Progress) bool {
	data := bytes.TrimSpace(progress.Data)
	return progress.Stage != "" || (len(data) != 0 && !bytes.Equal(data, []byte("null")))
}
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
func validPlatform(p model.Platform) bool {
	return p == model.TikTok || p == model.X || p == model.Instagram
}

func scanAccount(row scanner) (model.Account, error) {
	var a model.Account
	var identity []byte
	var expires, refreshStarted *time.Time
	err := row.Scan(&a.ID, &a.Owner.TenantID, &a.Owner.Subject, &a.Platform, &identity,
		&a.Status, &a.EncryptedToken, &a.TokenVersion, &expires, &a.CreatedAt, &a.UpdatedAt,
		&a.TokenRefreshVersion, &refreshStarted)
	if err != nil {
		return a, storeError(err)
	}
	if err = json.Unmarshal(identity, &a.Identity); err != nil {
		return model.Account{}, fmt.Errorf("decode stored account identity: %w", err)
	}
	if expires != nil {
		a.TokenExpiresAt = expires.UTC()
	}
	if refreshStarted != nil {
		a.TokenRefreshStartedAt = refreshStarted.UTC()
	}
	a.CreatedAt = a.CreatedAt.UTC()
	a.UpdatedAt = a.UpdatedAt.UTC()
	return a, nil
}
func scanPost(row scanner) (model.Post, error) {
	var p model.Post
	err := row.Scan(&p.ID, &p.Owner.TenantID, &p.Owner.Subject, &p.Status, &p.PublishAt, &p.Timezone, &p.Version, &p.CreatedAt)
	if err != nil {
		return p, storeError(err)
	}
	p.PublishAt = p.PublishAt.UTC()
	p.CreatedAt = p.CreatedAt.UTC()
	return p, nil
}
func scanTarget(row scanner) (model.Target, error) {
	var t model.Target
	var content, progress, warnings []byte
	err := row.Scan(&t.ID, &t.PostID, &t.AccountID, &t.Platform, &content, &t.Status, &progress,
		&t.Attempts, &t.Failures, &t.NextAttemptAt, &t.StartedAt, &t.ProcessingStartedAt, &t.RemoteID, &t.RemoteURL,
		&t.LastError, &warnings, &t.LeaseToken, &t.LeaseUntil)
	if err != nil {
		return t, storeError(err)
	}
	if err = json.Unmarshal(content, &t.Content); err != nil {
		return model.Target{}, fmt.Errorf("decode stored content: %w", err)
	}
	if err = json.Unmarshal(progress, &t.Progress); err != nil {
		return model.Target{}, fmt.Errorf("decode stored progress: %w", err)
	}
	if err = json.Unmarshal(warnings, &t.Warnings); err != nil {
		return model.Target{}, fmt.Errorf("decode stored warnings: %w", err)
	}
	t.NextAttemptAt = t.NextAttemptAt.UTC()
	if t.StartedAt != nil {
		v := t.StartedAt.UTC()
		t.StartedAt = &v
	}
	if t.ProcessingStartedAt != nil {
		v := t.ProcessingStartedAt.UTC()
		t.ProcessingStartedAt = &v
	}
	if t.LeaseUntil != nil {
		v := t.LeaseUntil.UTC()
		t.LeaseUntil = &v
	}
	return t, nil
}
func readTargets(ctx context.Context, q querier, postID string) ([]model.Target, error) {
	rows, err := q.Query(ctx, "SELECT "+targetColumns+" FROM sp_targets WHERE post_id=$1 ORDER BY position", postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make([]model.Target, 0)
	for rows.Next() {
		t, scanErr := scanTarget(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
}
func readPost(ctx context.Context, q querier, owner model.Principal, id string) (model.Post, error) {
	p, err := scanPost(q.QueryRow(ctx, "SELECT "+postColumns+" FROM sp_posts WHERE id=$1 AND owner_tenant=$2 AND owner_subject=$3", id, owner.TenantID, owner.Subject))
	if err != nil {
		return p, err
	}
	p.Targets, err = readTargets(ctx, q, id)
	if err != nil {
		return model.Post{}, err
	}
	p.Status = PostStatus(p.Targets)
	return p, nil
}
func lockOwnedPost(ctx context.Context, q querier, owner model.Principal, id string) (model.Post, error) {
	return scanPost(q.QueryRow(ctx, "SELECT "+postColumns+" FROM sp_posts WHERE id=$1 AND owner_tenant=$2 AND owner_subject=$3 FOR UPDATE", id, owner.TenantID, owner.Subject))
}
func addEvent(ctx context.Context, q querier, postID, targetID, accountID, kind, detail string) error {
	_, err := q.Exec(ctx, `INSERT INTO sp_events(post_id,target_id,account_id,kind,detail)
        VALUES (NULLIF($1,''),NULLIF($2,''),NULLIF($3,''),$4,$5)`, postID, targetID, accountID, kind, detail)
	return err
}

// The caller holds the post row lock. This common ordering avoids deadlocks
// between API operations and workers changing different targets of one post.
func refreshPost(ctx context.Context, q querier, id string) error {
	targets, err := readTargets(ctx, q, id)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, "UPDATE sp_posts SET status=$2,version=version+1 WHERE id=$1", id, PostStatus(targets))
	return err
}

// Opportunistic cleanup stays bounded even after an outage or a large abandoned
// OAuth backlog. The operations retention loop performs the remaining work.
// A stable cutoff and expiry-only ordering let the existing index bound the
// candidate scan. Locked tuple IDs avoid a planner-selected full-table join
// when deleting the small selected batch; they never escape this statement.
const pruneExpiredOAuthStates = `DELETE FROM sp_oauth_states WHERE ctid=ANY(ARRAY(
    SELECT ctid FROM sp_oauth_states WHERE expires_at<=statement_timestamp()
    ORDER BY expires_at LIMIT 200 FOR UPDATE SKIP LOCKED
))`

func (s *Postgres) PutOAuthState(ctx context.Context, state model.OAuthState) error {
	if !validOwner(state.Owner) || !validPlatform(state.Platform) || state.StateHash == "" || state.CookieHash == "" || state.ExpiresAt.IsZero() {
		return ErrOAuthState
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, pruneExpiredOAuthStates); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO sp_oauth_states(state_hash,cookie_hash,owner_tenant,owner_subject,platform,encrypted_verifier,expires_at)
        SELECT $1,$2,$3,$4,$5,$6,$7 WHERE $7::timestamptz>clock_timestamp()`, state.StateHash, state.CookieHash,
		state.Owner.TenantID, state.Owner.Subject, state.Platform, state.EncryptedVerifier, state.ExpiresAt.UTC())
	if err != nil {
		return storeError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrOAuthState
	}
	return tx.Commit(ctx)
}
func (s *Postgres) ConsumeOAuthState(ctx context.Context, hash, cookie string, platform model.Platform) (model.OAuthState, error) {
	var state model.OAuthState
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return state, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, pruneExpiredOAuthStates); err != nil {
		return state, err
	}
	err = tx.QueryRow(ctx, `DELETE FROM sp_oauth_states WHERE state_hash=$1 AND cookie_hash=$2 AND platform=$3 AND expires_at>clock_timestamp()
		RETURNING state_hash,cookie_hash,owner_tenant,owner_subject,platform,encrypted_verifier,issued_at,expires_at`, hash, cookie, platform).
		Scan(&state.StateHash, &state.CookieHash, &state.Owner.TenantID, &state.Owner.Subject, &state.Platform, &state.EncryptedVerifier, &state.IssuedAt, &state.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return state, commitErr
		}
		return state, ErrOAuthState
	}
	if err != nil {
		return state, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.OAuthState{}, err
	}
	state.ExpiresAt = state.ExpiresAt.UTC()
	state.IssuedAt = state.IssuedAt.UTC()
	return state, nil
}

func (s *Postgres) GetAccount(ctx context.Context, owner model.Principal, id string) (model.Account, error) {
	return scanAccount(s.pool.QueryRow(ctx, "SELECT "+accountColumns+" FROM sp_accounts WHERE id=$1 AND owner_tenant=$2 AND owner_subject=$3", id, owner.TenantID, owner.Subject))
}
func (s *Postgres) GetWorkerAccount(ctx context.Context, id string) (model.Account, error) {
	return scanAccount(s.pool.QueryRow(ctx, "SELECT "+accountColumns+" FROM sp_accounts WHERE id=$1", id))
}
func (s *Postgres) ListAccounts(ctx context.Context, owner model.Principal) ([]model.Account, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+accountColumns+" FROM sp_accounts WHERE owner_tenant=$1 AND owner_subject=$2 ORDER BY created_at,id", owner.TenantID, owner.Subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Account, 0)
	for rows.Next() {
		a, scanErr := scanAccount(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Postgres) ListRefreshCandidates(ctx context.Context, platform model.Platform, before time.Time, limit int) ([]model.Account, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, "SELECT "+accountColumns+` FROM sp_accounts WHERE platform=$1 AND status='active'
        AND encrypted_token IS NOT NULL AND (refresh_token_version IS NOT NULL OR token_expires_at<=$2)
        ORDER BY refresh_started_at NULLS LAST,token_expires_at,id LIMIT $3`, platform, before.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Account, 0)
	for rows.Next() {
		a, scanErr := scanAccount(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Postgres) UpsertAccount(ctx context.Context, account model.Account) (model.Account, error) {
	if account.ID == "" || !validOwner(account.Owner) || !validPlatform(account.Platform) || account.Identity.ID == "" || len(account.EncryptedToken) == 0 || account.AuthorizationStartedAt.IsZero() {
		return model.Account{}, ErrConflict
	}
	identity, err := json.Marshal(account.Identity)
	if err != nil {
		return model.Account{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Account{}, err
	}
	defer tx.Rollback(context.Background())
	saved, err := scanAccount(tx.QueryRow(ctx, `INSERT INTO sp_accounts(id,owner_tenant,owner_subject,platform,provider_user_id,identity,status,encrypted_token,token_expires_at)
        VALUES ($1,$2,$3,$4,$5,$6,'active',$7,$8)
        ON CONFLICT (platform,provider_user_id) DO UPDATE SET identity=EXCLUDED.identity,status='active',
            encrypted_token=EXCLUDED.encrypted_token,token_expires_at=EXCLUDED.token_expires_at,
            token_version=sp_accounts.token_version+1,refresh_token_version=NULL,refresh_started_at=NULL,updated_at=clock_timestamp()
		WHERE sp_accounts.owner_tenant=EXCLUDED.owner_tenant AND sp_accounts.owner_subject=EXCLUDED.owner_subject
            AND (sp_accounts.disconnected_at IS NULL OR sp_accounts.disconnected_at<$9::timestamptz)
        RETURNING `+accountColumns, account.ID, account.Owner.TenantID, account.Owner.Subject, account.Platform,
		account.Identity.ID, string(identity), account.EncryptedToken, nullableTime(account.TokenExpiresAt), account.AuthorizationStartedAt.UTC()))
	if errors.Is(err, ErrNotFound) {
		return model.Account{}, ErrConflict
	}
	if err != nil {
		return model.Account{}, err
	}
	kind := "account.connected"
	if saved.TokenVersion > 1 {
		kind = "account.reconnected"
	}
	if err = addEvent(ctx, tx, "", "", saved.ID, kind, ""); err != nil {
		return model.Account{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.Account{}, err
	}
	return saved, nil
}
func (s *Postgres) SaveToken(ctx context.Context, id string, version int64, blob []byte, expiresAt time.Time) error {
	if len(blob) == 0 {
		return ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `UPDATE sp_accounts SET encrypted_token=$3,token_expires_at=$4,token_version=token_version+1,
        refresh_token_version=NULL,refresh_started_at=NULL,updated_at=clock_timestamp()
        WHERE id=$1 AND token_version=$2 AND refresh_token_version=$2 AND status='active'`, id, version, blob, nullableTime(expiresAt))
	if err != nil {
		return storeError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if err = addEvent(ctx, tx, "", "", id, "account.token_refreshed", ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func identityLockKey(platform model.Platform, providerID string) int64 {
	sum := sha256.Sum256([]byte("socialpublisher:identity:" + string(platform) + "\x00" + providerID))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}
func discardLockConnection(conn *pgxpool.Conn) {
	raw := conn.Hijack()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = raw.Close(ctx)
}
func (s *Postgres) AcquireAccount(ctx context.Context, id string) (context.Context, func(), error) {
	if id == "" {
		return nil, nil, ErrNotFound
	}
	account, err := s.GetWorkerAccount(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return s.AcquireIdentity(ctx, account.Platform, account.Identity.ID)
}

// TryAcquireAccount bounds both pool admission and the SQL round trip without
// attaching that short deadline to the successfully acquired session guard.
func (s *Postgres) TryAcquireAccount(ctx context.Context, id string) (context.Context, func(), error) {
	admission, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	account, err := s.GetWorkerAccount(admission, id)
	if err == nil {
		var guard context.Context
		var release func()
		guard, release, err = s.acquireIdentity(admission, ctx, account.Platform, account.Identity.ID, true)
		if err == nil {
			return guard, release, nil
		}
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if admission.Err() != nil {
		return nil, nil, ErrAccountBusy
	}
	return nil, nil, err
}

var errAccountLockLost = errors.New("account lock session lost")

func (s *Postgres) AcquireIdentity(ctx context.Context, platform model.Platform, providerID string) (context.Context, func(), error) {
	return s.acquireIdentity(ctx, ctx, platform, providerID, false)
}

func (s *Postgres) acquireIdentity(admission, parent context.Context, platform model.Platform, providerID string, try bool) (context.Context, func(), error) {
	if !validPlatform(platform) || providerID == "" {
		return nil, nil, ErrConflict
	}
	conn, err := s.locks.Acquire(admission)
	if err != nil {
		return nil, nil, err
	}
	key := identityLockKey(platform, providerID)
	var backendPID int32
	if try {
		var acquired bool
		err = conn.QueryRow(admission, "SELECT pg_try_advisory_lock($1),pg_backend_pid()", key).Scan(&acquired, &backendPID)
		if err == nil && !acquired {
			conn.Release()
			return nil, nil, ErrAccountBusy
		}
	} else {
		var ignored any
		err = conn.QueryRow(admission, "SELECT pg_advisory_lock($1),pg_backend_pid()", key).Scan(&ignored, &backendPID)
	}
	if err != nil {
		// A cancelled query may have acquired the lock before its response was
		// lost. Never return that uncertain session to the pool.
		discardLockConnection(conn)
		return nil, nil, err
	}
	guard, cancelGuard := context.WithCancelCause(parent)
	stopped := make(chan struct{})
	discard := false // Read after stopped closes, so release never races the probe.
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-guard.Done():
				return
			case <-ticker.C:
				probeCtx, probeCancel := context.WithTimeout(guard, 2*time.Second)
				var currentPID int32
				probeErr := conn.QueryRow(probeCtx, "SELECT pg_backend_pid()").Scan(&currentPID)
				probeCancel()
				if probeErr != nil || currentPID != backendPID {
					discard = true
					cancelGuard(errAccountLockLost)
					return
				}
			}
		}
	}()
	var once sync.Once
	return guard, func() {
		once.Do(func() {
			cancelGuard(context.Canceled)
			<-stopped // No concurrent pgx use while unlocking or discarding.
			if discard {
				discardLockConnection(conn)
				return
			}
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var unlocked bool
			if unlockErr := conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", key).Scan(&unlocked); unlockErr != nil || !unlocked {
				discardLockConnection(conn)
				return
			}
			conn.Release()
		})
	}, nil
}
