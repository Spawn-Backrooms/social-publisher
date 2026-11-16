ALTER TABLE sp_accounts
    ADD COLUMN refresh_token_version bigint,
    ADD COLUMN refresh_started_at timestamptz,
    ADD CONSTRAINT sp_account_refresh_intent CHECK (
        (refresh_token_version IS NULL AND refresh_started_at IS NULL)
        OR (refresh_token_version IS NOT NULL AND refresh_token_version = token_version
            AND refresh_started_at IS NOT NULL AND status = 'active')
    );

-- Recovery scans must also see interrupted forced refreshes whose access token
-- has not expired. A pending attempt is never erased merely because it is old.
CREATE INDEX sp_accounts_refresh_intent ON sp_accounts(platform, refresh_started_at, id)
    WHERE refresh_token_version IS NOT NULL;
