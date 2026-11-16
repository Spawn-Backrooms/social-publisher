-- A live account operation excludes its queued backlog before a worker claims
-- another target. Due and expired work keep using the version-1 partial indexes.
CREATE INDEX sp_targets_running_account ON sp_targets(account_id, lease_until)
    WHERE status='running';

-- Provider endpoint reservations and provider-directed cooldowns are shared by
-- every replica. Account ownership and identity remain anchored in sp_accounts.
CREATE TABLE sp_provider_pacing (
    account_id text NOT NULL REFERENCES sp_accounts(id) ON DELETE CASCADE,
    scope text NOT NULL CHECK (scope <> '' AND length(scope) <= 128),
    next_allowed_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, scope)
);
CREATE INDEX sp_provider_pacing_expiry ON sp_provider_pacing(next_allowed_at, account_id, scope);
