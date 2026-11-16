CREATE TABLE sp_accounts (
    id text PRIMARY KEY CHECK (id <> ''),
    owner_tenant text NOT NULL CHECK (owner_tenant <> ''),
    owner_subject text NOT NULL CHECK (owner_subject <> ''),
    platform text NOT NULL CHECK (platform IN ('tiktok', 'x', 'instagram')),
    provider_user_id text NOT NULL CHECK (provider_user_id <> ''),
    identity jsonb NOT NULL CHECK (jsonb_typeof(identity) = 'object'),
    status text NOT NULL CHECK (status IN ('active', 'reauth_required', 'disconnected')),
    encrypted_token bytea,
    token_version bigint NOT NULL DEFAULT 1 CHECK (token_version > 0),
    token_expires_at timestamptz,
    disconnected_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT sp_account_provider_owner UNIQUE (platform, provider_user_id),
    CONSTRAINT sp_account_owner_identity UNIQUE (owner_tenant, owner_subject, platform, provider_user_id),
    CONSTRAINT sp_account_owner_reference UNIQUE (id, owner_tenant, owner_subject, platform),
    CHECK (status <> 'active' OR (encrypted_token IS NOT NULL AND octet_length(encrypted_token) > 0)),
    CHECK (status <> 'disconnected' OR encrypted_token IS NULL)
);
CREATE INDEX sp_accounts_owner ON sp_accounts(owner_tenant, owner_subject, created_at, id);
CREATE INDEX sp_accounts_refresh ON sp_accounts(platform, token_expires_at, id)
    WHERE status = 'active' AND encrypted_token IS NOT NULL;

CREATE TABLE sp_oauth_states (
    state_hash text PRIMARY KEY CHECK (state_hash <> ''),
    cookie_hash text NOT NULL CHECK (cookie_hash <> ''),
    owner_tenant text NOT NULL CHECK (owner_tenant <> ''),
    owner_subject text NOT NULL CHECK (owner_subject <> ''),
    platform text NOT NULL CHECK (platform IN ('tiktok', 'x', 'instagram')),
    encrypted_verifier bytea,
    issued_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX sp_oauth_states_expiry ON sp_oauth_states(expires_at);

CREATE TABLE sp_posts (
    id text PRIMARY KEY CHECK (id <> ''),
    owner_tenant text NOT NULL CHECK (owner_tenant <> ''),
    owner_subject text NOT NULL CHECK (owner_subject <> ''),
    status text NOT NULL DEFAULT 'queued' CHECK (status IN
        ('queued', 'publishing', 'published', 'partially_published', 'failed', 'needs_attention', 'cancelled')),
    publish_at timestamptz NOT NULL,
    timezone text NOT NULL DEFAULT '',
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    idempotency_key text NOT NULL CHECK (idempotency_key <> ''),
    request_hash text NOT NULL CHECK (request_hash <> ''),
    CONSTRAINT sp_posts_idempotency UNIQUE (owner_tenant, owner_subject, idempotency_key),
    CONSTRAINT sp_posts_owner_reference UNIQUE (id, owner_tenant, owner_subject)
);
CREATE INDEX sp_posts_owner ON sp_posts(owner_tenant, owner_subject, created_at DESC, id);
CREATE INDEX sp_posts_dispatch ON sp_posts(publish_at, created_at, id);

CREATE TABLE sp_targets (
    id text PRIMARY KEY CHECK (id <> ''),
    post_id text NOT NULL,
    account_id text NOT NULL,
    owner_tenant text NOT NULL,
    owner_subject text NOT NULL,
    platform text NOT NULL CHECK (platform IN ('tiktok', 'x', 'instagram')),
    position integer NOT NULL CHECK (position >= 0),
    content jsonb NOT NULL CHECK (jsonb_typeof(content) = 'object'),
    status text NOT NULL DEFAULT 'queued' CHECK (status IN
        ('queued', 'running', 'processing', 'retrying', 'published', 'failed', 'needs_attention', 'cancelled')),
    progress jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(progress) = 'object'),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    failures integer NOT NULL DEFAULT 0 CHECK (failures >= 0),
    next_attempt_at timestamptz NOT NULL,
    started_at timestamptz,
    remote_id text NOT NULL DEFAULT '',
    remote_url text NOT NULL DEFAULT '',
    last_error text NOT NULL DEFAULT '',
    warnings jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(warnings) = 'array'),
    lease_token text,
    lease_until timestamptz,
    CONSTRAINT sp_targets_post_owner FOREIGN KEY (post_id, owner_tenant, owner_subject)
        REFERENCES sp_posts(id, owner_tenant, owner_subject) ON DELETE CASCADE,
    CONSTRAINT sp_targets_account_owner FOREIGN KEY (account_id, owner_tenant, owner_subject, platform)
        REFERENCES sp_accounts(id, owner_tenant, owner_subject, platform),
    UNIQUE (post_id, account_id),
    UNIQUE (post_id, position),
    UNIQUE (id, post_id),
    CHECK ((status = 'running' AND lease_token IS NOT NULL AND lease_until IS NOT NULL)
        OR (status <> 'running' AND lease_token IS NULL AND lease_until IS NULL)),
    CHECK (status <> 'published' OR remote_id <> ''),
    CHECK (NOT (progress ? 'unsafe') OR jsonb_typeof(progress->'unsafe') = 'boolean'),
    CHECK (COALESCE(progress->'unsafe', 'false'::jsonb) <> 'true'::jsonb
        OR status IN ('running', 'needs_attention'))
);
CREATE INDEX sp_targets_due ON sp_targets(next_attempt_at, post_id, id)
    WHERE status IN ('queued', 'retrying', 'processing');
CREATE INDEX sp_targets_expired_lease ON sp_targets(lease_until, post_id, id) WHERE status = 'running';
CREATE INDEX sp_targets_account ON sp_targets(account_id, post_id, status);

CREATE TABLE sp_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    post_id text REFERENCES sp_posts(id) ON DELETE CASCADE,
    target_id text,
    account_id text REFERENCES sp_accounts(id),
    kind text NOT NULL CHECK (kind <> ''),
    detail text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (target_id, post_id) REFERENCES sp_targets(id, post_id) ON DELETE CASCADE,
    CHECK (post_id IS NOT NULL OR account_id IS NOT NULL),
    CHECK (target_id IS NULL OR post_id IS NOT NULL)
);
CREATE INDEX sp_events_post ON sp_events(post_id, id);
CREATE INDEX sp_events_account ON sp_events(account_id, id) WHERE account_id IS NOT NULL;
