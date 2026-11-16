package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"socialpublisher/internal/model"
)

func lookupIdempotency(ctx context.Context, q querier, owner model.Principal, key, hash string) (model.Post, error) {
	var id, storedHash string
	if err := q.QueryRow(ctx, `SELECT id,request_hash FROM sp_posts WHERE owner_tenant=$1 AND owner_subject=$2 AND idempotency_key=$3`,
		owner.TenantID, owner.Subject, key).Scan(&id, &storedHash); err != nil {
		return model.Post{}, storeError(err)
	}
	if hash != storedHash {
		return model.Post{}, ErrIdempotency
	}
	return readPost(ctx, q, owner, id)
}
func (s *Postgres) LookupIdempotency(ctx context.Context, owner model.Principal, key, hash string) (model.Post, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return model.Post{}, err
	}
	defer tx.Rollback(context.Background())
	post, err := lookupIdempotency(ctx, tx, owner, key, hash)
	if err != nil {
		return model.Post{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.Post{}, err
	}
	return post, nil
}

func (s *Postgres) CreatePost(ctx context.Context, post model.Post, key, hash string) (model.Post, bool, error) {
	if post.ID == "" || !validOwner(post.Owner) || key == "" || hash == "" || len(post.Targets) == 0 || post.PublishAt.IsZero() {
		return model.Post{}, false, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Post{}, false, err
	}
	defer tx.Rollback(context.Background())
	var inserted string
	err = tx.QueryRow(ctx, `INSERT INTO sp_posts(id,owner_tenant,owner_subject,publish_at,timezone,idempotency_key,request_hash)
        VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (owner_tenant,owner_subject,idempotency_key) DO NOTHING RETURNING id`,
		post.ID, post.Owner.TenantID, post.Owner.Subject, post.PublishAt.UTC(), post.Timezone, key, hash).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT waits for the competing transaction. The next statement
		// obtains a fresh READ COMMITTED snapshot containing all its targets.
		original, lookupErr := lookupIdempotency(ctx, tx, post.Owner, key, hash)
		if lookupErr != nil {
			return model.Post{}, false, lookupErr
		}
		if err = tx.Commit(ctx); err != nil {
			return model.Post{}, false, err
		}
		return original, false, nil
	}
	if err != nil {
		return model.Post{}, false, storeError(err)
	}
	// Replays above return the original post without reserving capacity again.
	// New platform targets are admitted atomically under the owner's queue lock.
	if err = s.ensureQueueCapacity(ctx, tx, post.Owner, len(post.Targets)); err != nil {
		return model.Post{}, false, err
	}
	// Account row locks serialize creation against disconnection. Sort IDs so
	// overlapping multi-account requests take these locks in one order.
	accountIDs := make([]string, 0, len(post.Targets))
	seen := make(map[string]bool)
	for _, target := range post.Targets {
		if target.ID == "" || target.AccountID == "" || seen[target.AccountID] {
			return model.Post{}, false, ErrConflict
		}
		seen[target.AccountID] = true
		accountIDs = append(accountIDs, target.AccountID)
	}
	sort.Strings(accountIDs)
	accounts := make(map[string]model.Account, len(accountIDs))
	for _, id := range accountIDs {
		a, readErr := scanAccount(tx.QueryRow(ctx, "SELECT "+accountColumns+` FROM sp_accounts
            WHERE id=$1 AND owner_tenant=$2 AND owner_subject=$3 FOR SHARE`, id, post.Owner.TenantID, post.Owner.Subject))
		if readErr != nil {
			return model.Post{}, false, readErr
		}
		if a.Status != "active" {
			return model.Post{}, false, ErrConflict
		}
		accounts[id] = a
	}
	for position, target := range post.Targets {
		account := accounts[target.AccountID]
		if target.Platform != account.Platform {
			return model.Post{}, false, ErrConflict
		}
		content, marshalErr := json.Marshal(target.Content)
		if marshalErr != nil {
			return model.Post{}, false, ErrConflict
		}
		_, err = tx.Exec(ctx, `INSERT INTO sp_targets(id,post_id,account_id,owner_tenant,owner_subject,platform,position,content,next_attempt_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, target.ID, post.ID, target.AccountID, post.Owner.TenantID, post.Owner.Subject,
			target.Platform, position, string(content), post.PublishAt.UTC())
		if err != nil {
			return model.Post{}, false, storeError(err)
		}
		if err = addEvent(ctx, tx, post.ID, target.ID, target.AccountID, "target.queued", ""); err != nil {
			return model.Post{}, false, err
		}
	}
	if err = addEvent(ctx, tx, post.ID, "", "", "post.created", ""); err != nil {
		return model.Post{}, false, err
	}
	saved, err := readPost(ctx, tx, post.Owner, post.ID)
	if err != nil {
		return model.Post{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.Post{}, false, err
	}
	return saved, true, nil
}
func (s *Postgres) GetPost(ctx context.Context, owner model.Principal, id string) (model.Post, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return model.Post{}, err
	}
	defer tx.Rollback(context.Background())
	post, err := readPost(ctx, tx, owner, id)
	if err != nil {
		return model.Post{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.Post{}, err
	}
	return post, nil
}
func (s *Postgres) ListPosts(ctx context.Context, owner model.Principal, limit int) ([]model.Post, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, "SELECT "+postColumns+` FROM sp_posts WHERE owner_tenant=$1 AND owner_subject=$2 ORDER BY created_at DESC,id LIMIT $3`, owner.TenantID, owner.Subject, limit)
	if err != nil {
		return nil, err
	}
	posts := make([]model.Post, 0)
	for rows.Next() {
		p, scanErr := scanPost(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		posts = append(posts, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range posts {
		posts[i].Targets, err = readTargets(ctx, tx, posts[i].ID)
		if err != nil {
			return nil, err
		}
		posts[i].Status = PostStatus(posts[i].Targets)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return posts, nil
}

func (s *Postgres) CancelPost(ctx context.Context, owner model.Principal, id string) (model.Post, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Post{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = lockOwnedPost(ctx, tx, owner, id); err != nil {
		return model.Post{}, err
	}
	rows, err := tx.Query(ctx, `UPDATE sp_targets SET status='cancelled',last_error='',lease_token=NULL,lease_until=NULL
		WHERE post_id=$1 AND status IN ('queued','retrying')
        AND COALESCE(progress->>'stage','')='' AND NOT (progress ? 'data' AND progress->'data'<>'null'::jsonb)
        AND COALESCE(progress->'unsafe','false'::jsonb)<>'true'::jsonb
        RETURNING id,account_id,status`, id)
	if err != nil {
		return model.Post{}, err
	}
	type cancelled struct{ id, accountID, status string }
	changed := make([]cancelled, 0)
	for rows.Next() {
		var c cancelled
		if err = rows.Scan(&c.id, &c.accountID, &c.status); err != nil {
			rows.Close()
			return model.Post{}, err
		}
		changed = append(changed, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return model.Post{}, err
	}
	for _, c := range changed {
		if err = addEvent(ctx, tx, id, c.id, c.accountID, "target."+c.status, "cancellation requested by owner"); err != nil {
			return model.Post{}, err
		}
	}
	if len(changed) > 0 {
		if err = refreshPost(ctx, tx, id); err != nil {
			return model.Post{}, err
		}
		if err = addEvent(ctx, tx, id, "", "", "post.cancelled", "pending deliveries cancelled"); err != nil {
			return model.Post{}, err
		}
	}
	saved, err := readPost(ctx, tx, owner, id)
	if err != nil {
		return model.Post{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.Post{}, err
	}
	return saved, nil
}
func (s *Postgres) ReschedulePost(ctx context.Context, owner model.Principal, id string, version int64, at time.Time, zone string) (model.Post, error) {
	if at.IsZero() {
		return model.Post{}, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Post{}, err
	}
	defer tx.Rollback(context.Background())
	post, err := lockOwnedPost(ctx, tx, owner, id)
	if err != nil {
		return model.Post{}, err
	}
	if post.Version != version {
		return model.Post{}, ErrConflict
	}
	var blocked bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sp_targets WHERE post_id=$1 AND
		(started_at IS NOT NULL OR status NOT IN ('queued','retrying') OR COALESCE(progress->>'stage','')<>''
        OR (progress ? 'data' AND progress->'data'<>'null'::jsonb)))`, id).Scan(&blocked); err != nil {
		return model.Post{}, err
	}
	if blocked {
		return model.Post{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, "UPDATE sp_posts SET publish_at=$2,timezone=$3,version=version+1 WHERE id=$1", id, at.UTC(), zone); err != nil {
		return model.Post{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE sp_targets SET next_attempt_at=$2 WHERE post_id=$1", id, at.UTC()); err != nil {
		return model.Post{}, err
	}
	if err = addEvent(ctx, tx, id, "", "", "post.rescheduled", at.UTC().Format(time.RFC3339Nano)); err != nil {
		return model.Post{}, err
	}
	saved, err := readPost(ctx, tx, owner, id)
	if err != nil {
		return model.Post{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.Post{}, err
	}
	return saved, nil
}
func (s *Postgres) RetryTarget(ctx context.Context, owner model.Principal, postID, targetID string) (model.Post, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Post{}, err
	}
	defer tx.Rollback(context.Background())
	// Queue admission precedes parent locks: creation may next need an account
	// row lock while disconnect holds that row and waits for existing parents.
	if err = admissionMutex(ctx, tx, "queue", owner.TenantID, owner.Subject); err != nil {
		return model.Post{}, err
	}
	if _, err = lockOwnedPost(ctx, tx, owner, postID); err != nil {
		return model.Post{}, err
	}
	target, err := scanTarget(tx.QueryRow(ctx, "SELECT "+targetColumns+" FROM sp_targets WHERE id=$1 AND post_id=$2", targetID, postID))
	if err != nil {
		return model.Post{}, err
	}
	if target.Status != "failed" || target.Progress.Unsafe || hasKnownProgress(target.Progress) {
		return model.Post{}, ErrConflict
	}
	var active bool
	if err = tx.QueryRow(ctx, "SELECT status='active' FROM sp_accounts WHERE id=$1", target.AccountID).Scan(&active); err != nil {
		return model.Post{}, storeError(err)
	}
	if !active {
		return model.Post{}, ErrConflict
	}
	if err = s.ensureQueueCapacity(ctx, tx, owner, 1); err != nil {
		return model.Post{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE sp_targets SET status='queued',progress='{}'::jsonb,failures=0,next_attempt_at=clock_timestamp(),
        last_error='',warnings='[]'::jsonb,remote_id='',remote_url='',lease_token=NULL,lease_until=NULL,processing_started_at=NULL WHERE id=$1`, targetID); err != nil {
		return model.Post{}, err
	}
	if err = addEvent(ctx, tx, postID, targetID, target.AccountID, "target.retried", "safe failure retried by owner"); err != nil {
		return model.Post{}, err
	}
	if err = refreshPost(ctx, tx, postID); err != nil {
		return model.Post{}, err
	}
	saved, err := readPost(ctx, tx, owner, postID)
	if err != nil {
		return model.Post{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.Post{}, err
	}
	return saved, nil
}
func (s *Postgres) ResolveTarget(ctx context.Context, owner model.Principal, postID, targetID string, resolution Resolution) (model.Post, error) {
	if strings.TrimSpace(resolution.Note) == "" || (resolution.Action != "confirm_published" && resolution.Action != "confirm_not_published" && resolution.Action != "resume_processing") {
		return model.Post{}, ErrConflict
	}
	if resolution.Action == "confirm_published" && strings.TrimSpace(resolution.RemoteID) == "" {
		return model.Post{}, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Post{}, err
	}
	defer tx.Rollback(context.Background())
	if resolution.Action == "resume_processing" {
		if err = admissionMutex(ctx, tx, "queue", owner.TenantID, owner.Subject); err != nil {
			return model.Post{}, err
		}
	}
	if _, err = lockOwnedPost(ctx, tx, owner, postID); err != nil {
		return model.Post{}, err
	}
	target, err := scanTarget(tx.QueryRow(ctx, "SELECT "+targetColumns+" FROM sp_targets WHERE id=$1 AND post_id=$2", targetID, postID))
	if err != nil {
		return model.Post{}, err
	}
	if target.Status != "needs_attention" {
		return model.Post{}, ErrConflict
	}
	if resolution.Action == "resume_processing" {
		if target.Progress.Unsafe || !hasKnownProgress(target.Progress) {
			return model.Post{}, ErrConflict
		}
		var active bool
		if err = tx.QueryRow(ctx, "SELECT status='active' FROM sp_accounts WHERE id=$1", target.AccountID).Scan(&active); err != nil {
			return model.Post{}, storeError(err)
		}
		if !active {
			return model.Post{}, ErrConflict
		}
		if err = s.ensureQueueCapacity(ctx, tx, owner, 1); err != nil {
			return model.Post{}, err
		}
		if _, err = tx.Exec(ctx, `UPDATE sp_targets SET status='processing',failures=0,next_attempt_at=clock_timestamp(),
            last_error='',lease_token=NULL,lease_until=NULL,processing_started_at=clock_timestamp() WHERE id=$1`, targetID); err != nil {
			return model.Post{}, err
		}
	} else {
		status := "published"
		remoteID, remoteURL := resolution.RemoteID, resolution.RemoteURL
		if resolution.Action == "confirm_not_published" {
			// Confirmation removes uncertainty; a distinct explicit retry starts a
			// new delivery. It does not publish immediately as a side effect.
			status = "failed"
			remoteID = ""
			remoteURL = ""
		}
		if _, err = tx.Exec(ctx, `UPDATE sp_targets SET status=$2,progress='{}'::jsonb,remote_id=$3,remote_url=$4,
			last_error='',lease_token=NULL,lease_until=NULL,
			processing_started_at=CASE WHEN $2='failed' THEN NULL ELSE processing_started_at END WHERE id=$1`, targetID, status, remoteID, remoteURL); err != nil {
			return model.Post{}, err
		}
	}
	if err = addEvent(ctx, tx, postID, targetID, target.AccountID, "target."+resolution.Action, resolution.Note); err != nil {
		return model.Post{}, err
	}
	if err = refreshPost(ctx, tx, postID); err != nil {
		return model.Post{}, err
	}
	saved, err := readPost(ctx, tx, owner, postID)
	if err != nil {
		return model.Post{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.Post{}, err
	}
	return saved, nil
}
func (s *Postgres) ListEvents(ctx context.Context, owner model.Principal, postID string) ([]model.Event, error) {
	page, err := s.ListEventsPage(ctx, owner, postID, 0, 100)
	return page.Events, err
}
