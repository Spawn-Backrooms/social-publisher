package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func (s *Postgres) RecordProcessHeartbeat(ctx context.Context, heartbeat ProcessHeartbeat) error {
	if heartbeat.InstanceID == "" || len(heartbeat.InstanceID) > 200 || heartbeat.BuildRevision == "" || len(heartbeat.BuildRevision) > 200 ||
		(heartbeat.Role != "api" && heartbeat.Role != "worker" && heartbeat.Role != "serve") || heartbeat.WorkerProgressSequence < 0 ||
		(heartbeat.Role == "api" && heartbeat.WorkerProgressSequence != 0) {
		return ErrConflict
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO sp_process_heartbeats(instance_id,role,build_revision,worker_progress_sequence,worker_progress_at)
		VALUES ($1,$2,$3,$4,CASE WHEN $4::bigint>0 THEN clock_timestamp() ELSE NULL END)
		ON CONFLICT(instance_id) DO UPDATE SET last_seen_at=clock_timestamp(),
		worker_progress_at=CASE WHEN EXCLUDED.worker_progress_sequence>sp_process_heartbeats.worker_progress_sequence
			THEN clock_timestamp() ELSE sp_process_heartbeats.worker_progress_at END,
		worker_progress_sequence=GREATEST(EXCLUDED.worker_progress_sequence,sp_process_heartbeats.worker_progress_sequence)
		WHERE sp_process_heartbeats.role=EXCLUDED.role AND sp_process_heartbeats.build_revision=EXCLUDED.build_revision`,
		heartbeat.InstanceID, heartbeat.Role, heartbeat.BuildRevision, heartbeat.WorkerProgressSequence)
	if err != nil {
		return storeError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

const processColumns = `instance_id,role,build_revision,started_at,last_seen_at,worker_progress_at,worker_progress_sequence`

func scanProcess(row scanner, now time.Time, query OperationsQuery) (ProcessStatus, error) {
	var process ProcessStatus
	err := row.Scan(&process.InstanceID, &process.Role, &process.BuildRevision, &process.StartedAt, &process.LastSeenAt, &process.WorkerProgressAt, &process.WorkerProgressSequence)
	if err != nil {
		return process, err
	}
	process.StartedAt, process.LastSeenAt = process.StartedAt.UTC(), process.LastSeenAt.UTC()
	process.HeartbeatFresh = !process.LastSeenAt.Before(now.Add(-query.HeartbeatStaleAfter))
	if process.WorkerProgressAt != nil {
		progressAt := process.WorkerProgressAt.UTC()
		process.WorkerProgressAt = &progressAt
		process.WorkerProgressFresh = !progressAt.Before(now.Add(-query.WorkerStaleAfter))
	}
	// A first legitimate job can occupy every worker longer than the controller's
	// readiness probe. Report that bounded startup state separately; never invent
	// completed work or extend this deadline with another process heartbeat.
	process.WorkerStarting = (process.Role == "worker" || process.Role == "serve") &&
		process.WorkerProgressSequence == 0 && process.WorkerProgressAt == nil && now.Before(process.StartedAt.Add(query.WorkerStaleAfter))
	return process, nil
}

func boundedCount(value int64, limit int) BoundedCount {
	if value > int64(limit) {
		return BoundedCount{Value: int64(limit), Capped: true}
	}
	return BoundedCount{Value: value}
}

// OperationsSnapshot does not scan completed post history. Every count and
// instance list is capped; oldest due work is selected through the queue's
// existing partial indexes. All staleness comparisons use database time.
func (s *Postgres) OperationsSnapshot(ctx context.Context, query OperationsQuery) (OperationsSnapshot, error) {
	var snapshot OperationsSnapshot
	if query.Limit <= 0 {
		query.Limit = 1000
	}
	if query.Limit > 10000 {
		query.Limit = 10000
	}
	if query.HeartbeatStaleAfter == 0 {
		query.HeartbeatStaleAfter = time.Minute
	}
	if query.WorkerStaleAfter == 0 {
		query.WorkerStaleAfter = 13 * time.Minute
	}
	if query.HeartbeatStaleAfter < 0 || query.WorkerStaleAfter < 0 {
		return snapshot, ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback(context.Background())
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&snapshot.DatabaseTime); err != nil {
		return snapshot, err
	}
	snapshot.DatabaseTime = snapshot.DatabaseTime.UTC()
	var due int64
	err = tx.QueryRow(ctx, `WITH due AS (
		(SELECT next_attempt_at AS due_at FROM sp_targets WHERE status IN ('queued','retrying','processing')
			AND next_attempt_at<=$1 ORDER BY next_attempt_at,post_id,id LIMIT $2)
		UNION ALL
		(SELECT lease_until AS due_at FROM sp_targets WHERE status='running' AND lease_until<=$1
			ORDER BY lease_until,post_id,id LIMIT $2)
	) SELECT count(*),min(due_at) FROM (SELECT due_at FROM due ORDER BY due_at LIMIT $2) bounded`,
		snapshot.DatabaseTime, query.Limit+1).Scan(&due, &snapshot.Queue.OldestDueAt)
	if err != nil {
		return snapshot, err
	}
	snapshot.Queue.DueTargets = boundedCount(due, query.Limit)
	if snapshot.Queue.OldestDueAt != nil {
		oldest := snapshot.Queue.OldestDueAt.UTC()
		snapshot.Queue.OldestDueAt = &oldest
		snapshot.Queue.OldestDueAgeSeconds = max(0, snapshot.DatabaseTime.Sub(oldest).Seconds())
	}
	for _, metric := range []struct {
		query string
		out   *BoundedCount
	}{
		{`SELECT count(*) FROM (SELECT id FROM sp_targets WHERE status='needs_attention' ORDER BY id LIMIT $1) bounded`, &snapshot.Queue.NeedsAttention},
		{`SELECT count(*) FROM (SELECT id FROM sp_accounts WHERE status='reauth_required' ORDER BY id LIMIT $1) bounded`, &snapshot.Queue.ReauthRequired},
	} {
		var count int64
		if err = tx.QueryRow(ctx, metric.query, query.Limit+1).Scan(&count); err != nil {
			return snapshot, err
		}
		*metric.out = boundedCount(count, query.Limit)
	}
	var cooldowns int64
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT account_id FROM sp_provider_pacing
		WHERE next_allowed_at>$1 ORDER BY next_allowed_at,account_id,scope LIMIT $2) bounded`, snapshot.DatabaseTime, query.Limit+1).Scan(&cooldowns); err != nil {
		return snapshot, err
	}
	snapshot.Queue.ProviderCooldowns = boundedCount(cooldowns, query.Limit)
	rows, err := tx.Query(ctx, "SELECT "+processColumns+" FROM sp_process_heartbeats ORDER BY last_seen_at DESC,instance_id LIMIT $1", query.Limit+1)
	if err != nil {
		return snapshot, err
	}
	for rows.Next() {
		process, scanErr := scanProcess(rows, snapshot.DatabaseTime, query)
		if scanErr != nil {
			rows.Close()
			return snapshot, scanErr
		}
		if snapshot.Processes.Observed == query.Limit {
			snapshot.Processes.Truncated = true
			continue
		}
		snapshot.Processes.Observed++
		if process.HeartbeatFresh {
			snapshot.Processes.FreshInstances++
		} else {
			snapshot.Processes.StaleInstances++
		}
		if process.Role == "serve" || process.Role == "worker" {
			if process.HeartbeatFresh && process.WorkerStarting {
				snapshot.Processes.StartingWorkers++
			}
			if process.HeartbeatFresh && (process.WorkerProgressFresh || process.WorkerStarting) {
				snapshot.Processes.FreshWorkers++
			} else {
				snapshot.Processes.StaleWorkers++
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return snapshot, err
	}
	if query.InstanceID != "" {
		process, readErr := scanProcess(tx.QueryRow(ctx, "SELECT "+processColumns+" FROM sp_process_heartbeats WHERE instance_id=$1", query.InstanceID), snapshot.DatabaseTime, query)
		if readErr != nil && !errors.Is(readErr, pgx.ErrNoRows) {
			return snapshot, readErr
		}
		if readErr == nil {
			snapshot.LocalProcess = &process
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return snapshot, err
	}
	snapshot.Pools = PoolSummary{Runtime: poolStatistics(s.pool.Stat()), AccountLocks: poolStatistics(s.locks.Stat())}
	return snapshot, nil
}

func poolStatistics(stat *pgxpool.Stat) ConnectionPool {
	return ConnectionPool{MaxConns: stat.MaxConns(), TotalConns: stat.TotalConns(), AcquiredConns: stat.AcquiredConns(),
		IdleConns: stat.IdleConns(), ConstructingConns: stat.ConstructingConns(), AcquireCount: stat.AcquireCount(),
		EmptyAcquireCount: stat.EmptyAcquireCount(), CanceledAcquireCount: stat.CanceledAcquireCount(),
		AcquireDurationSeconds: stat.AcquireDuration().Seconds(), EmptyAcquireWaitSeconds: stat.EmptyAcquireWaitTime().Seconds()}
}
