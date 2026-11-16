package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"socialpublisher/internal/model"
)

func leaseMicros(duration time.Duration) (int64, error) {
	value := int64(duration / time.Microsecond)
	if value <= 0 {
		return 0, ErrConflict
	}
	return value, nil
}

func (s *Postgres) Claim(ctx context.Context, duration time.Duration) (model.Target, error) {
	micros, err := leaseMicros(duration)
	if err != nil {
		return model.Target{}, err
	}
	for {
		target, recovered, claimErr := s.claimOnce(ctx, micros)
		if !errors.Is(claimErr, ErrNoWork) || !recovered {
			return target, claimErr
		}
		if err = ctx.Err(); err != nil {
			return model.Target{}, err
		}
	}
}
func (s *Postgres) claimOnce(ctx context.Context, micros int64) (model.Target, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Target{}, false, err
	}
	defer tx.Rollback(context.Background())
	var postID string
	err = tx.QueryRow(ctx, dispatchPostQuery).Scan(&postID)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Target{}, false, ErrNoWork
	}
	if err != nil {
		return model.Target{}, false, err
	}
	recovered, err := recoverPostLeases(ctx, tx, postID)
	if err != nil {
		return model.Target{}, false, err
	}
	var targetID string
	err = tx.QueryRow(ctx, `SELECT t.id FROM sp_targets t
        WHERE t.post_id=$1 AND `+dueTargetEligibility+`
        ORDER BY t.next_attempt_at,t.position FOR UPDATE OF t SKIP LOCKED LIMIT 1`, postID).Scan(&targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return model.Target{}, false, commitErr
		}
		return model.Target{}, recovered, ErrNoWork
	}
	if err != nil {
		return model.Target{}, false, err
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return model.Target{}, false, err
	}
	lease := hex.EncodeToString(random[:])
	target, err := scanTarget(tx.QueryRow(ctx, `UPDATE sp_targets SET status='running',lease_token=$2,
        lease_until=clock_timestamp()+($3::bigint*interval '1 microsecond'),
        started_at=COALESCE(started_at,clock_timestamp()),attempts=attempts+1
        WHERE id=$1 RETURNING `+targetColumns, targetID, lease, micros))
	if err != nil {
		return model.Target{}, false, err
	}
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&target.ClaimedAt); err != nil {
		return model.Target{}, false, err
	}
	if err = addEvent(ctx, tx, postID, target.ID, target.AccountID, "target.claimed", ""); err != nil {
		return model.Target{}, false, err
	}
	if err = refreshPost(ctx, tx, postID); err != nil {
		return model.Target{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.Target{}, false, err
	}
	return target, recovered, nil
}

func recoverPostLeases(ctx context.Context, tx pgx.Tx, postID string) (bool, error) {
	rows, err := tx.Query(ctx, `UPDATE sp_targets t SET status=CASE
        WHEN COALESCE(t.progress->'unsafe','false'::jsonb)='true'::jsonb THEN 'needs_attention'
        WHEN a.status<>'active' AND (COALESCE(t.progress->>'stage','')<>''
            OR (t.progress ? 'data' AND t.progress->'data'<>'null'::jsonb)) THEN 'needs_attention'
        WHEN a.status='disconnected' THEN 'cancelled'
        WHEN a.status='reauth_required' THEN 'failed'
        WHEN COALESCE(t.progress->>'stage','')<>'' OR (t.progress ? 'data' AND t.progress->'data'<>'null'::jsonb) THEN 'processing'
        ELSE 'queued' END,
        lease_token=NULL,lease_until=NULL,next_attempt_at=clock_timestamp(),
        last_error=CASE WHEN COALESCE(t.progress->'unsafe','false'::jsonb)='true'::jsonb
            THEN 'worker lease expired during an uncertain provider operation; reconciliation required'
            WHEN a.status<>'active' THEN 'worker lease expired after account access was disabled'
            ELSE 'worker lease expired; safe checkpoint recovered' END
        FROM sp_accounts a WHERE t.account_id=a.id AND t.post_id=$1 AND t.status='running'
            AND t.lease_until<=clock_timestamp() RETURNING t.id,t.account_id,t.status`, postID)
	if err != nil {
		return false, err
	}
	type recoveredTarget struct{ id, accountID, status string }
	recovered := make([]recoveredTarget, 0)
	for rows.Next() {
		var t recoveredTarget
		if err = rows.Scan(&t.id, &t.accountID, &t.status); err != nil {
			rows.Close()
			return false, err
		}
		recovered = append(recovered, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	for _, t := range recovered {
		kind := "target." + t.status
		if t.status == "queued" || t.status == "processing" {
			kind = "target.lease_recovered"
		}
		if err = addEvent(ctx, tx, postID, t.id, t.accountID, kind, "expired worker lease recovered"); err != nil {
			return false, err
		}
	}
	if len(recovered) > 0 {
		if err = refreshPost(ctx, tx, postID); err != nil {
			return false, err
		}
	}
	return len(recovered) > 0, nil
}

func (s *Postgres) Heartbeat(ctx context.Context, targetID, lease string, duration time.Duration) error {
	micros, err := leaseMicros(duration)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE sp_targets SET lease_until=GREATEST(lease_until,clock_timestamp()+($3::bigint*interval '1 microsecond'))
        WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_until>clock_timestamp()`, targetID, lease, micros)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}
func lockWorkerPost(ctx context.Context, tx pgx.Tx, targetID string) (string, error) {
	var postID string
	if err := tx.QueryRow(ctx, "SELECT post_id FROM sp_targets WHERE id=$1", targetID).Scan(&postID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrLeaseLost
		}
		return "", err
	}
	var locked string
	if err := tx.QueryRow(ctx, "SELECT id FROM sp_posts WHERE id=$1 FOR UPDATE", postID).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrLeaseLost
		}
		return "", err
	}
	return postID, nil
}
func (s *Postgres) SaveProgress(ctx context.Context, targetID, lease string, progress model.Progress) error {
	encoded, err := json.Marshal(progress)
	if err != nil {
		return ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	postID, err := lockWorkerPost(ctx, tx, targetID)
	if err != nil {
		return err
	}
	// A deliberately saved empty safe checkpoint records a proven rejection.
	// Its eventual fresh retry must not inherit the rejected operation's age.
	tag, err := tx.Exec(ctx, `UPDATE sp_targets SET progress=$3,
			processing_started_at=CASE WHEN $4 THEN COALESCE(processing_started_at,clock_timestamp()) ELSE NULL END
		WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_until>clock_timestamp()`,
		targetID, lease, string(encoded), progress.Unsafe || hasKnownProgress(progress))
	if err != nil {
		return storeError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	detail := "safe checkpoint saved"
	if progress.Unsafe {
		detail = "unsafe checkpoint saved before external mutation"
	}
	if err = addEvent(ctx, tx, postID, targetID, "", "target.checkpoint", detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Postgres) FinishTarget(ctx context.Context, targetID, lease string, finish Finish) error {
	switch finish.Status {
	case "queued", "processing", "retrying", "published", "failed", "needs_attention", "cancelled":
	default:
		return ErrConflict
	}
	if finish.Status == "published" && strings.TrimSpace(finish.RemoteID) == "" {
		return ErrConflict
	}
	if finish.RetryAfter < 0 || finish.RetryAfter > 24*time.Hour {
		return ErrConflict
	}
	if finish.Warnings == nil {
		finish.Warnings = []string{}
	}
	warnings, err := json.Marshal(finish.Warnings)
	if err != nil {
		return ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	postID, err := lockWorkerPost(ctx, tx, targetID)
	if err != nil {
		return err
	}
	previous, err := scanTarget(tx.QueryRow(ctx, "SELECT "+targetColumns+` FROM sp_targets
        WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_until>clock_timestamp() FOR UPDATE`, targetID, lease))
	if errors.Is(err, ErrNotFound) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if finish.Status == "published" {
		finish.Progress.Unsafe = false
	} else if previous.Progress.Unsafe || finish.Progress.Unsafe {
		// A caller cannot accidentally erase uncertainty through a zero-value
		// Finish. Proven rejection must first save a safe checkpoint.
		finish.Status = "needs_attention"
		if previous.Progress.Unsafe {
			finish.Progress = previous.Progress
		}
		if finish.Error == "" {
			finish.Error = "provider outcome is uncertain; reconciliation required"
		}
	}
	if finish.Status != "published" && hasKnownProgress(previous.Progress) && !hasKnownProgress(finish.Progress) {
		finish.Progress = previous.Progress
	}
	if hasKnownProgress(finish.Progress) {
		switch finish.Status {
		case "queued", "retrying":
			finish.Status = "processing"
		case "failed", "cancelled":
			finish.Status = "needs_attention"
			if finish.Error == "" {
				finish.Error = "provider work has started; reconciliation required"
			}
		}
	}
	if finish.Status == "queued" || finish.Status == "retrying" || finish.Status == "processing" {
		var accountStatus string
		if err = tx.QueryRow(ctx, "SELECT status FROM sp_accounts WHERE id=$1", previous.AccountID).Scan(&accountStatus); err != nil {
			return storeError(err)
		}
		if accountStatus != "active" {
			if finish.Status == "processing" || hasKnownProgress(finish.Progress) {
				finish.Status = "needs_attention"
			} else if accountStatus == "disconnected" {
				finish.Status = "cancelled"
			} else {
				finish.Status = "failed"
			}
			finish.Error = "account access is disabled; delivery stopped"
		}
	}
	progress, err := json.Marshal(finish.Progress)
	if err != nil {
		return ErrConflict
	}
	failures := 0
	if finish.CountFailure {
		failures = 1
	}
	tag, err := tx.Exec(ctx, `UPDATE sp_targets SET status=$3,progress=$4,
        next_attempt_at=CASE WHEN $3 IN ('queued','retrying','processing') AND $11
            THEN LEAST(CASE WHEN $12::bigint>0 THEN clock_timestamp()+($12::bigint*interval '1 microsecond') ELSE COALESCE($5::timestamptz,clock_timestamp()) END,
                COALESCE(processing_started_at,clock_timestamp())+interval '24 hours')
            ELSE CASE WHEN $12::bigint>0 THEN clock_timestamp()+($12::bigint*interval '1 microsecond') ELSE COALESCE($5::timestamptz,clock_timestamp()) END END,
        remote_id=$6,remote_url=$7,
        last_error=$8,warnings=$9,failures=failures+$10,lease_token=NULL,lease_until=NULL,
        processing_started_at=CASE WHEN $11 THEN COALESCE(processing_started_at,clock_timestamp()) ELSE processing_started_at END
        WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_until>clock_timestamp()`,
		targetID, lease, finish.Status, string(progress), nullableTime(finish.NextAttemptAt), finish.RemoteID, finish.RemoteURL, finish.Error, string(warnings), failures,
		finish.Progress.Unsafe || hasKnownProgress(finish.Progress), finish.RetryAfter.Microseconds())
	if err != nil {
		return storeError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	if err = addEvent(ctx, tx, postID, targetID, previous.AccountID, "target."+finish.Status, finish.Error); err != nil {
		return err
	}
	if err = refreshPost(ctx, tx, postID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
