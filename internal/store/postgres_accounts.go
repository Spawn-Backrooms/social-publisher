package store

import (
	"context"
	"github.com/jackc/pgx/v5"
	"socialpublisher/internal/model"
)

func (s *Postgres) MarkReauth(ctx context.Context, id string, version int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `UPDATE sp_accounts SET status='reauth_required',token_version=token_version+1,
        refresh_token_version=NULL,refresh_started_at=NULL,updated_at=clock_timestamp()
        WHERE id=$1 AND token_version=$2 AND status='active'`, id, version)
	if err != nil {
		return storeError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if err = stopAccountTargets(ctx, tx, id, false); err != nil {
		return err
	}
	if err = addEvent(ctx, tx, "", "", id, "account.reauth_required", "reconnect required"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Disconnect returns the previous encrypted credentials for a separate,
// best-effort upstream revocation. The caller holds AcquireAccount through this
// method and upstream revocation. The returned credentials are no longer stored.
func (s *Postgres) Disconnect(ctx context.Context, owner model.Principal, id string) (model.Account, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Account{}, err
	}
	defer tx.Rollback(context.Background())
	previous, err := scanAccount(tx.QueryRow(ctx, "SELECT "+accountColumns+` FROM sp_accounts
		WHERE id=$1 AND owner_tenant=$2 AND owner_subject=$3 FOR NO KEY UPDATE`, id, owner.TenantID, owner.Subject))
	if err != nil {
		return model.Account{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE sp_accounts SET status='disconnected',encrypted_token=NULL,token_expires_at=NULL,
		token_version=token_version+1,refresh_token_version=NULL,refresh_started_at=NULL,
        updated_at=clock_timestamp(),disconnected_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		return model.Account{}, err
	}
	if err = stopAccountTargets(ctx, tx, id, true); err != nil {
		return model.Account{}, err
	}
	if err = addEvent(ctx, tx, "", "", id, "account.disconnected", "credentials erased; pending deliveries stopped"); err != nil {
		return model.Account{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.Account{}, err
	}
	return previous, nil
}

func stopAccountTargets(ctx context.Context, tx pgx.Tx, accountID string, disconnect bool) error {
	eligible := "'queued','retrying','processing'"
	if disconnect {
		eligible = "'queued','retrying','processing','running'"
	}
	// Lock every related parent, including currently failed/attention targets.
	// Otherwise Retry/Resume could validate the old active account, then enqueue
	// work after this transaction's scan had excluded its previous target state.
	rows, err := tx.Query(ctx, `SELECT p.id FROM sp_posts p WHERE EXISTS
		(SELECT 1 FROM sp_targets t WHERE t.post_id=p.id AND t.account_id=$1)
        ORDER BY p.id FOR UPDATE OF p`, accountID)
	if err != nil {
		return err
	}
	postIDs := make([]string, 0)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		postIDs = append(postIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, postID := range postIDs {
		statusSQL := `CASE WHEN status='processing' OR COALESCE(progress->>'stage','')<>''
            OR (progress ? 'data' AND progress->'data'<>'null'::jsonb)
            OR COALESCE(progress->'unsafe','false'::jsonb)='true'::jsonb THEN 'needs_attention' ELSE 'failed' END`
		reason := "account requires reconnection"
		if disconnect {
			statusSQL = `CASE WHEN status='processing' OR
				(COALESCE(progress->>'stage','')<>'' OR (progress ? 'data' AND progress->'data'<>'null'::jsonb)
                OR COALESCE(progress->'unsafe','false'::jsonb)='true'::jsonb)
                THEN 'needs_attention' ELSE 'cancelled' END`
			reason = "account disconnected; remote work may need reconciliation"
		}
		changed, queryErr := tx.Query(ctx, `UPDATE sp_targets SET status=`+statusSQL+`,last_error=$3,lease_token=NULL,lease_until=NULL
            WHERE post_id=$1 AND account_id=$2 AND status IN (`+eligible+`) RETURNING id,status`, postID, accountID, reason)
		if queryErr != nil {
			return queryErr
		}
		type change struct{ id, status string }
		changes := make([]change, 0)
		for changed.Next() {
			var c change
			if err = changed.Scan(&c.id, &c.status); err != nil {
				changed.Close()
				return err
			}
			changes = append(changes, c)
		}
		err = changed.Err()
		changed.Close()
		if err != nil {
			return err
		}
		for _, c := range changes {
			if err = addEvent(ctx, tx, postID, c.id, accountID, "target."+c.status, reason); err != nil {
				return err
			}
		}
		if len(changes) > 0 {
			if err = refreshPost(ctx, tx, postID); err != nil {
				return err
			}
		}
	}
	return nil
}
