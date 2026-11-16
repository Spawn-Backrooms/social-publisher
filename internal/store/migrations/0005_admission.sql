CREATE TABLE sp_api_owner_budgets (
    owner_tenant text NOT NULL CHECK (owner_tenant <> ''),
    owner_subject text NOT NULL CHECK (owner_subject <> ''),
    window_started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    request_count integer NOT NULL DEFAULT 0 CHECK (request_count >= 0),
    PRIMARY KEY (owner_tenant, owner_subject)
);
CREATE INDEX sp_api_owner_budgets_expiry ON sp_api_owner_budgets(window_started_at);

CREATE TABLE sp_api_admissions (
    token text PRIMARY KEY CHECK (token <> ''),
    owner_tenant text NOT NULL CHECK (owner_tenant <> ''),
    owner_subject text NOT NULL CHECK (owner_subject <> ''),
    scope_kind text NOT NULL CHECK (scope_kind IN ('request', 'account')),
    scope_key text NOT NULL,
    expires_at timestamptz NOT NULL,
    CHECK ((scope_kind = 'request' AND scope_key = '') OR (scope_kind = 'account' AND scope_key <> ''))
);
CREATE INDEX sp_api_admissions_owner ON sp_api_admissions(owner_tenant, owner_subject, expires_at)
    WHERE scope_kind = 'request';
CREATE INDEX sp_api_admissions_account ON sp_api_admissions(scope_key, expires_at)
    WHERE scope_kind = 'account';
CREATE INDEX sp_api_admissions_expiry ON sp_api_admissions(expires_at);

CREATE INDEX sp_targets_owner_pending ON sp_targets(owner_tenant, owner_subject)
    WHERE status IN ('queued', 'running', 'processing', 'retrying');
