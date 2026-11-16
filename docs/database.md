# PostgreSQL storage and recovery

The service stores connections, OAuth state, posts, individual delivery targets,
idempotency records, and audit events in PostgreSQL. The `sp_` table prefix keeps
the schema separate from unrelated application tables. Use a dedicated database
or schema, a migration role allowed to create its objects, and a separate runtime
role with the DML permissions below.

## Migrations

`store.Migrate(ctx, dsn)` applies the SQL files embedded from
`internal/store/migrations`. A database-wide advisory lock serializes migrations.
Each migration and its ledger record commit in one transaction. The
`sp_schema_migrations` table records its version, filename, SHA-256 checksum and
application time. Already applied migrations must remain immutable. The runner
rejects changed checksums and versions that this binary does not recognize.

Run migrations as an explicit deployment step before starting API processes or
workers. Runtime startup does not silently migrate the schema. Take normal
PostgreSQL backups and verify restoration separately from application tests.

Migrations 0003–0006 add refresh intent, dispatch pacing, request admission and
operational state. The previously released migration bytes remain immutable.
Use maintenance for this release: older binaries reject the newer exact manifest.

### Runtime permissions

The runtime needs schema `USAGE` and the following table permissions. Grant them
in the application's dedicated schema to its runtime role after migrations. It
does not need schema creation, migration-ledger writes, or permission to delete
post/idempotency records. `UPDATE` on OAuth and admission tables is required for
row locking during bounded cleanup, even though ordinary updates are absent.

| Tables | Runtime permissions |
| --- | --- |
| `sp_schema_migrations` | `SELECT` |
| `sp_accounts`, `sp_posts`, `sp_targets` | `SELECT`, `INSERT`, `UPDATE` |
| `sp_events` | `SELECT`, `INSERT`, `DELETE` |
| `sp_oauth_states`, `sp_provider_pacing`, `sp_api_owner_budgets`, `sp_api_admissions`, `sp_process_heartbeats` | `SELECT`, `INSERT`, `UPDATE`, `DELETE` |
| `sp_retention_cursors` | `SELECT`, `UPDATE` |

Readiness checks every required privilege independently, a writable primary,
the migration manifest and required runtime columns using non-mutating queries.
The native preflight separately verifies independent sessions and advisory-lock
handoff through the selected connection path. The isolated test role requires
additional schema/role/backend-management permissions to exercise failure cases;
those permissions are not a requirement for the production runtime role.

## Connections and ownership

`store.New(ctx, dsn, maxConns)` creates a query pool and a separate advisory-lock
pool, each bounded by `maxConns`. The connection budget per process is therefore
up to **twice `maxConns`**, plus a temporary migration connection. The separation
prevents workers holding account locks from exhausting all connections needed to
save checkpoints, heartbeat leases or finish deliveries.

Every public account, post, target and event lookup checks both the tenant ID and
the subject. Composite foreign keys also prevent a target from referencing a
post or connection belonging to a different owner. Provider account ownership is
permanent for this version: `(platform, provider_user_id)` is globally unique,
including disconnected tombstones. A different local user cannot claim that
identity. Reauthorization by the same owner restores the original connection ID.

Encrypted token updates are compare-and-swap operations on `token_version` and
require an active connection. Disconnect and reauthorization-required transitions
increment that version. A stale refresh cannot restore disconnected credentials.
Expiry timestamps are stored alongside ciphertext and updated atomically.
Refresh candidates are selected per platform, ordered by expiry and ID, and
exclude inactive connections and absent tokens.

Every refresh first writes an intent tied to the current token version. Token
replacement requires that intent and clears it atomically with the new version.
An unresolved intent after process loss requires reauthorization before the old
credential can be decrypted or used for refresh/publishing. A proven rejection
can clear the intent while advancing the version, fencing any late completion.
Maintenance also selects unresolved intents whose access token has not expired.

### Connection lifecycle locking

The application must hold the lifecycle lock around provider operations and
connection mutations:

- `AcquireAccount` resolves the existing account to a stable provider identity.
- `AcquireIdentity` acquires the same lock before the first connection row exists.
- OAuth callbacks acquire the identity lock, revalidate the provider identity,
  and then call `UpsertAccount`.
- Disconnect callers acquire the account lock and retain it through both the
  local `Disconnect` transaction and the subsequent best-effort provider revoke.
- Refresh and publishing callers use the same account lock.

`Disconnect` does **not** acquire an inner lock. Both acquisition methods return
a derived context, a release function and an error. Callers use that context for
all protected operations and defer release. The store checks the held session's
backend ID once per second with a two-second probe timeout. Probe failure or a
changed backend cancels the guard context. Cancellation stops subsequent requests;
provider requests already transmitted may still require reconciliation.

Release cancels the guard and waits for the probe to stop before unlocking or
discarding the connection, so pgx never receives concurrent commands on one
session. Release functions are idempotent. A cancelled acquisition discards its
session because the server may have acquired the lock before its response was
lost. Probe or unlock failure also discards the session rather than returning a
potentially locked connection to the pool.

Use a direct PostgreSQL connection or a pooler in session mode for the lock pool;
transaction pooling cannot preserve a session advisory lock. Avoid idle-session
termination for held locks. Detection is bounded by the probe cadence and timeout,
not instantaneous. Native failover and lock-session-loss behavior remain a
deployment verification gate. Token-version fencing and target leases cannot undo
a provider mutation already accepted upstream.

`UpsertAccount` requires the database-stamped `OAuthState.IssuedAt` as its
`AuthorizationStartedAt` timestamp. Both issuance and disconnection use the same
database clock, independent of application-clock skew. The latest `disconnected_at`
timestamp remains as a fence even
after a later reconnection: a flow that began at or before the disconnect cannot
restore credentials. Repeating disconnect also advances this fence. The
authorization timestamp is ephemeral account input and is not returned in public
account JSON.

## Idempotency and scheduling

Post creation stores the parent, all targets, idempotency key/hash, and creation
events in one transaction. A unique `(tenant, subject, idempotency_key)` constraint
serializes competing requests. The same key and hash returns the original post;
the same key with a different hash returns `ErrIdempotency`. The boolean returned
by `CreatePost` means **created**: true for insertion and false for a replay.
`LookupIdempotency` lets the API return a legitimate replay before rechecking live
account availability or whether the originally requested time is now in the past.

All schedule instants are `timestamptz`; Go results are normalized to UTC. The
optional timezone is display metadata. Rescheduling checks the exact post version
and rejects a post once any target has started, reached a terminal state, or has
provider progress. The parent lock serializes this check with worker claims.

## Queue and uncertainty

Claims use `FOR UPDATE SKIP LOCKED`. A short parent-row lock establishes one
consistent lock order for API mutations and target updates. Targets then receive
a random lease token, database-clock lease expiry, initial start timestamp and
incremented attempt count. Independent workers can claim different posts while
each transaction skips rows already being changed.

Selection begins with indexed due targets and expired leases rather than retained
parent history. It excludes the backlog of an account with live work or an active
shared cooldown. Workers try account admission with a short pool/lock deadline;
a busy account is deferred so other accounts can run. The monitored session guard
then remains valid for the actual job lifetime. Parent locks still precede target
locks, including recovery and mixed-result parents.

TikTok status dispatch reserves a database-time slot at least 2.1 seconds apart
for the same account across processes, including authentication retries. Provider
429/capacity waits persist a shared cooldown and do not consume delivery failure
budgets. Rate metadata never overrides an ambiguous/unsafe publication, and
waiting does not extend a known operation's fixed 24-hour processing window.

Heartbeat, checkpoint and finish writes require a running target, the matching
lease token, and an unexpired lease at the database. Stale workers receive
`ErrLeaseLost`. Heartbeats cannot revive expired leases or shorten a later expiry.

Before a public provider mutation, the adapter saves an unsafe checkpoint. Known
provider operation IDs are checkpointed before polling. Recovery distinguishes:

| Expired running target | Recovery |
| --- | --- |
| Unsafe checkpoint | `needs_attention`; preserve progress |
| Safe checkpoint with provider stage or data | `processing`; preserve IDs and resume polling |
| Empty checkpoint and active account | `queued` |
| Disabled account with known provider work | `needs_attention` |
| Disconnected account with empty checkpoint | `cancelled` |
| Reauthorization required with empty checkpoint | `failed` |

A generic finish cannot erase persisted uncertainty or known operation IDs.
An explicit proven rejection must durably save its safe checkpoint before a retry
finish. A confirmed publication with a remote ID can complete the target directly.
Retryable errors with known progress remain `processing`; terminal errors with
known progress become `needs_attention`.

Cancel only changes queued/retrying targets with completely empty safe progress.
It leaves running, processing and legacy queued/retrying targets with known
provider work visible. Disconnect erases credentials and cancels empty pending
work; running or pending targets with known progress become `needs_attention` and
their leases are fenced. Reauthorization-required transitions fail empty pending
work and put known remote work into `needs_attention`; the current running worker
handles its own result.

Manual retry accepts only `failed` targets with empty safe progress and an active
account. Reconciliation accepts only `needs_attention` targets and requires an
audit note. `confirm_published` also requires a remote ID. `confirm_not_published`
clears progress and moves the target to `failed`; a separate explicit retry is
required to publish again. Successful destinations are not reset by another
destination's retry or cancellation.

After restoring account access, `resume_processing` can return a target from
`needs_attention` to `processing`. It requires an active account, a safe checkpoint,
existing provider progress and an audit note. It preserves progress and receipts,
resets the failure budget and resumes polling immediately. It cannot resume an
unsafe public mutation or an empty checkpoint.

Status changes and their audit events commit together. Checkpoint audit details
contain only a safe/unsafe marker, not raw provider payloads or media URLs.
The caller remains responsible for sanitizing error messages before passing them
to `FinishTarget`.

## Integration tests

Use a dedicated disposable PostgreSQL database whose test role can create and
drop schemas. Every test creates an isolated random schema and drops it during
cleanup; it does not truncate shared application tables.

Use `make test-postgres` for the strict release gate rather than interpreting an
ordinary test run with skipped database cases as acceptance. It verifies session
independence, requires the original concurrency tests plus newly added native
tests and process-death recovery, rejects skips, and fingerprints the source before
and after execution. See [verification tooling](../scripts/verify/README.md).

```sh
TEST_DATABASE_URL='postgres://user:password@localhost:5432/social_publisher_test?sslmode=disable' \
  go test -race -count=1 ./internal/store
```

When `TEST_DATABASE_URL` is absent, database tests explicitly report skipped work.
Connection or migration failures with the variable present are failures, not skips.
The sequential suite exercises actual SQL for schema versions, owner isolation,
OAuth consumption and expiry, token CAS and disconnect fencing, idempotency,
lease fencing and recovery, reconciliation, cancellation and rescheduling.

Tests whose names include `Native` require independent PostgreSQL sessions. They
cover concurrent same/different-content idempotency, single-use OAuth races,
exclusive worker claims, claim/cancel/reschedule races, identity/account lock
equivalence, cancellation and connection release at a one-connection pool limit.
Forced interleavings also cover account-row/audit-foreign-key lock ordering and
disconnect or reauthorization racing with a retry or processing resumption.
Terminating the backend holding a lifecycle lock must cancel its operation guard
and permit acquisition of a healthy replacement session.

For a PGlite socket bridge that shares one PostgreSQL backend, set `TEST_PGLITE=1`.
Only the independent-session concurrency tests skip; the sequential SQL suite
still runs. The fixture explicitly sets its schema search path because that bridge
does not apply startup `search_path`. A passing PGlite run does **not** validate
native transaction concurrency, session advisory locking, crash durability or
production database operation. Run the native suite against PostgreSQL before
release.
