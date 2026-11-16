package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Lock and consume tuple locations within a single statement. Joining a limited
// key set back to a large table can still choose a full-table hash join, while
// this shape permits a bounded TID scan even at the maximum batch size.
const retentionAdmissionsQuery = `DELETE FROM sp_api_admissions a WHERE a.ctid=ANY(ARRAY(
	SELECT ctid FROM sp_api_admissions WHERE expires_at<=$1 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED))
	AND a.expires_at<=$1`

const retentionOwnerBudgetsQuery = `DELETE FROM sp_api_owner_budgets b WHERE b.ctid=ANY(ARRAY(
	SELECT ctid FROM sp_api_owner_budgets
	WHERE window_started_at<=$1::timestamptz-interval '60 seconds'
	ORDER BY window_started_at LIMIT $2 FOR UPDATE SKIP LOCKED))
	AND b.window_started_at<=$1::timestamptz-interval '60 seconds'
	AND NOT EXISTS(SELECT 1 FROM sp_api_admissions a WHERE a.owner_tenant=b.owner_tenant AND a.owner_subject=b.owner_subject AND a.expires_at>$1)`

const retentionOAuthQuery = `DELETE FROM sp_oauth_states s WHERE s.ctid=ANY(ARRAY(
	SELECT ctid FROM sp_oauth_states WHERE expires_at<=$1 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED))
	AND s.expires_at<=$1`

const retentionHeartbeatsQuery = `DELETE FROM sp_process_heartbeats h WHERE h.ctid=ANY(ARRAY(
	SELECT ctid FROM sp_process_heartbeats WHERE last_seen_at<=$1::timestamptz-interval '7 days'
	ORDER BY last_seen_at LIMIT $2 FOR UPDATE SKIP LOCKED))
	AND h.last_seen_at<=$1::timestamptz-interval '7 days'`

const retentionPacingQuery = `DELETE FROM sp_provider_pacing p WHERE p.ctid=ANY(ARRAY(
	SELECT ctid FROM sp_provider_pacing WHERE next_allowed_at<=$1
	ORDER BY next_allowed_at,account_id,scope LIMIT $2 FOR UPDATE SKIP LOCKED))
	AND p.next_allowed_at<=$1`

const retentionLockParentsQuery = `SELECT id FROM sp_posts WHERE id=ANY($1::text[])
	ORDER BY id FOR UPDATE SKIP LOCKED`

// Keep the safety check correlated to each selected, locked parent. OFFSET 0
// prevents EXISTS from being flattened into an anti-join that can scan every
// historical target before deleting a small event batch. The post_id prefix
// of the target uniqueness indexes supplies the parent-specific lookup.
const retentionDeleteEventsQuery = `DELETE FROM sp_events e WHERE e.id=ANY($1::bigint[]) AND e.post_id=ANY($2::text[])
	AND NOT EXISTS(SELECT 1 FROM sp_targets t WHERE t.post_id=e.post_id AND (
		t.status NOT IN ('published','failed','cancelled') OR COALESCE(t.progress->'unsafe','false'::jsonb)='true'::jsonb
		OR (t.status<>'published' AND (COALESCE(t.progress->>'stage','')<>''
			OR (t.progress ? 'data' AND t.progress->'data'<>'null'::jsonb)))) OFFSET 0)`

// RetainOperationalRecords deletes at most BatchSize rows from each transient
// table and examines at most BatchSize old delivery events. Posts, targets,
// idempotency keys, token/refresh state, and final audit receipts are retained.
func (s *Postgres) RetainOperationalRecords(ctx context.Context, policy RetentionPolicy) (RetentionResult, error) {
	var result RetentionResult
	if policy.EventRetention == 0 {
		policy.EventRetention = 90 * 24 * time.Hour
	}
	if policy.EventRetention < 7*24*time.Hour || policy.BatchSize < 0 {
		return result, ErrConflict
	}
	if policy.BatchSize == 0 {
		policy.BatchSize = 500
	}
	if policy.BatchSize > 2000 {
		policy.BatchSize = 2000
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&result.At); err != nil {
		return result, err
	}
	result.At = result.At.UTC()
	if err = retainDeliveryEvents(ctx, tx, policy, &result); err != nil {
		return RetentionResult{}, err
	}
	for _, cleanup := range []struct {
		query string
		out   *int64
	}{
		{retentionAdmissionsQuery, &result.AdmissionsDeleted},
		{retentionOwnerBudgetsQuery, &result.OwnerBudgetsDeleted},
		{retentionOAuthQuery, &result.OAuthStatesDeleted},
		{retentionPacingQuery, &result.ProviderPacingDeleted},
		{retentionHeartbeatsQuery, &result.HeartbeatsDeleted},
	} {
		tag, deleteErr := tx.Exec(ctx, cleanup.query, result.At, policy.BatchSize)
		if deleteErr != nil {
			return RetentionResult{}, deleteErr
		}
		*cleanup.out = tag.RowsAffected()
	}
	if err = tx.Commit(ctx); err != nil {
		return RetentionResult{}, err
	}
	return result, nil
}

func retainDeliveryEvents(ctx context.Context, tx pgx.Tx, policy RetentionPolicy, result *RetentionResult) error {
	var afterTime time.Time
	var afterID int64
	err := tx.QueryRow(ctx, `SELECT after_created_at,after_id FROM sp_retention_cursors
		WHERE name='delivery_events' FOR UPDATE SKIP LOCKED`).Scan(&afterTime, &afterID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // Another replica is already advancing this event scan.
	}
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id,post_id,created_at FROM sp_events
		WHERE target_id IS NOT NULL AND kind IN
			('target.queued','target.claimed','target.checkpoint','target.processing','target.retrying','target.lease_recovered')
		AND created_at<$1 AND (created_at,id)>($2,$3)
		ORDER BY created_at,id LIMIT $4`, result.At.Add(-policy.EventRetention), afterTime, afterID, policy.BatchSize)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, policy.BatchSize)
	postIDs := make([]string, 0, policy.BatchSize)
	seen := make(map[string]bool)
	for rows.Next() {
		var postID string
		if err = rows.Scan(&afterID, &postID, &afterTime); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, afterID)
		if !seen[postID] {
			seen[postID] = true
			postIDs = append(postIDs, postID)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	result.EventCandidatesScanned = int64(len(ids))
	result.EventSweepComplete = len(ids) < policy.BatchSize
	if len(ids) > 0 {
		// Parent locks serialize this eligibility check with retry, resolution,
		// checkpoint and finish. Busy parents are revisited on the next sweep.
		rows, err = tx.Query(ctx, retentionLockParentsQuery, postIDs)
		if err != nil {
			return err
		}
		locked := make([]string, 0, len(postIDs))
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			locked = append(locked, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(locked) > 0 {
			tag, deleteErr := tx.Exec(ctx, retentionDeleteEventsQuery, ids, locked)
			if deleteErr != nil {
				return deleteErr
			}
			result.EventsDeleted = tag.RowsAffected()
		}
	}
	if result.EventSweepComplete {
		afterTime, afterID = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), 0
	}
	_, err = tx.Exec(ctx, `UPDATE sp_retention_cursors SET after_created_at=$1,after_id=$2,updated_at=clock_timestamp()
		WHERE name='delivery_events'`, afterTime, afterID)
	return err
}
