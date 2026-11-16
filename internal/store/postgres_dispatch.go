package store

import (
	"context"
	"errors"
	"time"
)

// ErrAccountBusy is an admission result, not a failed delivery. A worker must
// release its target lease and let another account use that worker slot.
var ErrAccountBusy = errors.New("account is busy")

// Capture database time in scalar subqueries: a volatile clock_timestamp()
// expression directly against every row is not an indexable due-time bound.
// Expired processing windows may bypass an account's cooldown or active work
// because their only permitted action is durable reconciliation, not a request.
const dueTargetEligibility = `t.status IN ('queued','retrying','processing')
    AND t.next_attempt_at <= (SELECT clock_timestamp())
    AND EXISTS (SELECT 1 FROM sp_accounts a WHERE a.id=t.account_id AND a.status='active')
    AND (t.processing_started_at <= (SELECT clock_timestamp())-interval '24 hours'
        OR (NOT EXISTS (SELECT 1 FROM sp_targets busy WHERE busy.account_id=t.account_id
                AND busy.status='running' AND busy.lease_until>(SELECT clock_timestamp()))
            AND NOT EXISTS (SELECT 1 FROM sp_provider_pacing budget WHERE budget.account_id=t.account_id
                AND budget.scope='platform:'||t.platform AND budget.next_allowed_at>(SELECT clock_timestamp()))))`

// Start at the partial target indexes, then lock a parent by primary key before
// touching its targets. LATERAL keeps parent lookup dependent on the eligible
// stream; terminal posts are never the outer side of a historical scan. No
// candidate-count limit can let a locked parent hide later eligible accounts.
const dispatchPostQuery = `SELECT parent.id FROM (
    SELECT t.post_id,t.next_attempt_at AS due_at,t.id FROM sp_targets t WHERE ` + dueTargetEligibility + `
    UNION ALL
    SELECT t.post_id,t.lease_until AS due_at,t.id FROM sp_targets t
        WHERE t.status='running' AND t.lease_until <= (SELECT clock_timestamp())
    ORDER BY due_at,post_id,id
) eligible
CROSS JOIN LATERAL (SELECT p.id FROM sp_posts p WHERE p.id=eligible.post_id FOR UPDATE OF p SKIP LOCKED) parent
LIMIT 1`

// ReserveProviderRequest grants an immediate request only when this scope has
// no active reservation/cooldown. Denied callers do not extend the deadline.
// Row locking plus a post-lock database clock also handles first-insert races.
func (s *Postgres) ReserveProviderRequest(ctx context.Context, accountID, scope string, spacing time.Duration) (time.Duration, error) {
	if accountID == "" || scope == "" || len(scope) > 128 || spacing < 0 || spacing > 24*time.Hour {
		return 0, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `INSERT INTO sp_provider_pacing(account_id,scope,next_allowed_at)
        VALUES ($1,$2,clock_timestamp()) ON CONFLICT (account_id,scope)
        DO UPDATE SET next_allowed_at=sp_provider_pacing.next_allowed_at`, accountID, scope); err != nil {
		return 0, storeError(err)
	}
	// The no-change conflict update owns the existing row through commit. An
	// expired row must not be deleted by retention between this UPSERT and the
	// following read; denied reservations still leave its deadline unchanged.
	var next, now time.Time
	if err = tx.QueryRow(ctx, `SELECT next_allowed_at FROM sp_provider_pacing WHERE account_id=$1 AND scope=$2 FOR UPDATE`, accountID, scope).Scan(&next); err != nil {
		return 0, storeError(err)
	}
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return 0, err
	}
	wait := next.Sub(now)
	if wait <= 0 {
		wait = 0
		if _, err = tx.Exec(ctx, `UPDATE sp_provider_pacing SET next_allowed_at=clock_timestamp()+($3::bigint*interval '1 microsecond')
            WHERE account_id=$1 AND scope=$2`, accountID, scope, spacing.Microseconds()); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return wait, nil
}

// SetProviderCooldown never shortens a persisted reservation. The returned
// remaining delay comes from database time, so replica wall-clock skew cannot
// let a caller resume early.
func (s *Postgres) SetProviderCooldown(ctx context.Context, accountID, scope string, delay time.Duration) (time.Duration, error) {
	if accountID == "" || scope == "" || len(scope) > 128 || delay <= 0 || delay > 24*time.Hour {
		return 0, ErrConflict
	}
	var micros int64
	err := s.pool.QueryRow(ctx, `INSERT INTO sp_provider_pacing(account_id,scope,next_allowed_at)
        VALUES ($1,$2,clock_timestamp()+($3::bigint*interval '1 microsecond'))
        ON CONFLICT (account_id,scope) DO UPDATE SET next_allowed_at=GREATEST(sp_provider_pacing.next_allowed_at,EXCLUDED.next_allowed_at)
        RETURNING GREATEST(0,CEIL(EXTRACT(EPOCH FROM next_allowed_at-clock_timestamp())*1000000))::bigint`,
		accountID, scope, delay.Microseconds()).Scan(&micros)
	if err != nil {
		return 0, storeError(err)
	}
	return time.Duration(micros) * time.Microsecond, nil
}
