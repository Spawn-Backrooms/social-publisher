-- Run ONLY on an isolated restored database with all publication processes
-- stopped. Supply both psql booleans explicitly; see operations.md.
\set ON_ERROR_STOP on
\if :{?restore_isolated}
\else
  DO $$ BEGIN RAISE EXCEPTION 'Refusing restore quarantine: restore_isolated must be supplied.'; END $$;
\endif
\if :restore_isolated
\else
  DO $$ BEGIN RAISE EXCEPTION 'Refusing restore quarantine: the restored database must be isolated.'; END $$;
\endif
\if :{?maintenance_confirmed}
\else
  DO $$ BEGIN RAISE EXCEPTION 'Refusing restore quarantine: maintenance_confirmed must be supplied.'; END $$;
\endif
\if :maintenance_confirmed
\else
  DO $$ BEGIN RAISE EXCEPTION 'Refusing restore quarantine: all publication processes must be stopped.'; END $$;
\endif

BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';
LOCK TABLE sp_accounts, sp_posts, sp_targets, sp_events IN EXCLUSIVE MODE;

-- A backed-up credential may already have rotated after the backup, even when
-- its backed-up refresh marker is absent. Never refresh any restored token.
WITH quarantined AS (
  UPDATE sp_accounts SET status='reauth_required',token_version=token_version+1,
    refresh_token_version=NULL,refresh_started_at=NULL,updated_at=clock_timestamp()
  WHERE status='active' RETURNING id
)
INSERT INTO sp_events(account_id,kind,detail)
SELECT id,'account.restore_quarantined','Restored credentials require a new browser authorization before use.' FROM quarantined;

-- Any nonpublished backup row might have been published after the backup.
-- Preserve its checkpoint, first-start history, processing clock, and remote
-- receipt; require an explicit provider reconciliation before another attempt.
WITH quarantined AS (
  UPDATE sp_targets SET status='needs_attention',lease_token=NULL,lease_until=NULL,
    last_error='restore_reconciliation_required: verify the provider result before resuming or retrying'
  WHERE status<>'published' RETURNING id,post_id,account_id
)
INSERT INTO sp_events(post_id,target_id,account_id,kind,detail)
SELECT post_id,id,account_id,'target.restore_quarantined',
  'Backup restore: check the provider result before another publication attempt.' FROM quarantined;

UPDATE sp_posts p SET status='needs_attention',version=version+1
WHERE EXISTS(SELECT 1 FROM sp_targets t WHERE t.post_id=p.id AND t.status='needs_attention');
COMMIT;
