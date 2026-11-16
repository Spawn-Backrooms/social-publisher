CREATE TABLE sp_process_heartbeats (
    instance_id text PRIMARY KEY CHECK (instance_id <> '' AND length(instance_id) <= 200),
    role text NOT NULL CHECK (role IN ('api', 'worker', 'serve')),
    build_revision text NOT NULL CHECK (build_revision <> '' AND length(build_revision) <= 200),
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_seen_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    worker_progress_at timestamptz,
    worker_progress_sequence bigint NOT NULL DEFAULT 0 CHECK (worker_progress_sequence >= 0),
    CHECK ((worker_progress_sequence = 0 AND worker_progress_at IS NULL)
        OR (role IN ('worker', 'serve') AND worker_progress_sequence > 0 AND worker_progress_at IS NOT NULL))
);
CREATE INDEX sp_process_heartbeats_seen ON sp_process_heartbeats(last_seen_at DESC, instance_id);

CREATE INDEX sp_targets_attention ON sp_targets(id) WHERE status = 'needs_attention';
CREATE INDEX sp_accounts_reauth ON sp_accounts(id) WHERE status = 'reauth_required';
CREATE INDEX sp_api_admissions_owner_retention ON sp_api_admissions(owner_tenant, owner_subject, expires_at);

-- Only routine delivery events participate in automatic retention. Final
-- receipts, owner resolutions, account history and unfamiliar event kinds do
-- not enter the candidate stream. Unresolved parent deliveries are checked
-- again under the parent lock before an eligible candidate can be deleted.
CREATE INDEX sp_events_retention ON sp_events(created_at, id)
    WHERE target_id IS NOT NULL AND kind IN
        ('target.queued', 'target.claimed', 'target.checkpoint', 'target.processing', 'target.retrying', 'target.lease_recovered');

CREATE TABLE sp_retention_cursors (
    name text PRIMARY KEY CHECK (name = 'delivery_events'),
    after_created_at timestamptz NOT NULL DEFAULT '0001-01-01 00:00:00+00',
    after_id bigint NOT NULL DEFAULT 0 CHECK (after_id >= 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO sp_retention_cursors(name) VALUES ('delivery_events');
