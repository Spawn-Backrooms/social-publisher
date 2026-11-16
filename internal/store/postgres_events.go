package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"socialpublisher/internal/model"
)

func (s *Postgres) ListEventsPage(ctx context.Context, owner model.Principal, postID string, afterID int64, limit int) (EventPage, error) {
	page := EventPage{Events: make([]model.Event, 0), NextAfterID: afterID}
	if afterID < 0 {
		return page, ErrConflict
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return page, err
	}
	defer tx.Rollback(context.Background())
	var id string
	if err = tx.QueryRow(ctx, "SELECT id FROM sp_posts WHERE id=$1 AND owner_tenant=$2 AND owner_subject=$3", postID, owner.TenantID, owner.Subject).Scan(&id); err != nil {
		return page, storeError(err)
	}
	rows, err := tx.Query(ctx, `SELECT id,COALESCE(target_id,''),kind,detail,created_at FROM sp_events
		WHERE post_id=$1 AND id>$2 ORDER BY id LIMIT $3`, postID, afterID, limit+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var event model.Event
		if err = rows.Scan(&event.ID, &event.TargetID, &event.Kind, &event.Detail, &event.CreatedAt); err != nil {
			rows.Close()
			return EventPage{}, err
		}
		if len(page.Events) == limit {
			page.HasMore = true
			continue
		}
		event.CreatedAt = event.CreatedAt.UTC()
		page.Events = append(page.Events, event)
		page.NextAfterID = event.ID
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return EventPage{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return EventPage{}, err
	}
	return page, nil
}
