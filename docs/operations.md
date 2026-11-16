# Operations, retention, and recovery

This runbook applies to the exact publisher revision whose migrations are installed. Native PostgreSQL acceptance, an actual backup restore, and authorized live-provider checks remain release gates. Passing a sequential SQL fixture does not establish independent-session locking or successful recovery of a real backup.

## Operational diagnostics

`GET /internal/ops` requires `X-Operations-Token: <OPERATIONS_TOKEN>`. The token is separate from end-user JWTs, must contain at least 32 random bytes encoded as base64 in production, and must stay on the private controller/monitoring path. Do not expose this endpoint through public routing or put its token in URLs, logs, browser storage, or checked-in examples.

The endpoint returns HTTP 200 when its local process is healthy and HTTP 503 when that process is stale or its database snapshot cannot be read. A structured unhealthy snapshot is returned when database reads succeed. It includes:

| Field | Interpretation |
| --- | --- |
| `build_revision` | Revision compiled into the running binary, supplied to the operations manager at startup. It is not read from a mutable request or runtime revision header. Compare it with the release's expected revision before resuming traffic. |
| `role` | `api`, `worker`, or `serve`. An API-only maintenance process can be healthy without a dispatcher. A `worker` process does not expose an HTTP listener; another monitoring process can read its database heartbeat. |
| `local_process.last_seen_at` | Database-time timestamp of a successful process heartbeat. Startup writes this synchronously. Subsequent heartbeats default to every 15 seconds. |
| `local_process.worker_progress_at` | Database-time timestamp advanced only when a completed successful worker iteration, including a successful no-work claim, increments its progress sequence. A heartbeat timer and an in-flight provider request do not advance it. |
| `local_process.heartbeat_fresh` | True within the 60-second default heartbeat staleness threshold. |
| `local_process.worker_progress_fresh` | True within the configured worker threshold after a real completed iteration. Main uses `JOB_TIMEOUT_SECONDS + 180 seconds`, allowing a single worker to finish a legitimate long upload. It stays false until actual progress occurs. |
| `local_process.worker_starting` | A separate initial grace signal for `serve`/`worker` before their first completed iteration. It expires at the immutable database `started_at` plus the worker threshold. A fresh heartbeat and either this bounded grace or real fresh progress are required for worker health. Heartbeat ticks cannot extend grace, and a worker that has completed an iteration cannot re-enter it. |
| `queue.due_targets` | Bounded count of queued/retrying/processing targets due now plus expired running leases. Account pacing or busy accounts can still delay those targets. |
| `queue.oldest_due_at`, `queue.oldest_due_age_seconds` | Oldest due target or expired lease, selected through queue indexes. Empty queues return a null timestamp and age zero. This measures queue delay, not when a social platform made a post visible. |
| `queue.needs_attention`, `queue.reauth_required` | Bounded counts of targets requiring reconciliation and accounts requiring a new authorization. |
| `queue.provider_cooldowns` | Bounded count of active account/endpoint pacing records. Normal reservations also appear here; this is not a count of HTTP 429 responses. |
| `processes` | A bounded summary of recently observed instances, including stale workers and `starting_workers`. Fresh workers include those explicitly within startup grace. `truncated=true` means global counts are lower bounds. Local readiness is always looked up separately. |
| `pools.runtime`, `pools.account_locks` | Local pool capacities, acquired/idle/constructing connections, total/empty/cancelled acquisitions, cumulative acquisition duration and cumulative wait on an empty pool. Differences between successive samples reveal waiting; cumulative totals do not reset until process restart. |
| `retention` | Most recent successful retention pass performed by this process: timestamp, scanned event candidates, deleted counts by table, and whether the event scan reached its end. A bounded pass runs on process startup and then hourly. A zero timestamp means no pass has succeeded in this process yet. |

Each `queue` count is an object with `value` and `capped`. The default bound is 1,000; `capped=true` means at least that many matching rows exist. The operations package permits a bound up to 10,000. Snapshots use a five-second database deadline and bounded indexed reads. They do not inspect post content, token ciphertext, owner identifiers, or all historical posts.

`healthy=true` establishes the local role's heartbeat/progress checks, including its explicit initial grace period, and database access. It does not prove provider access, available credits, codec validity, or the absence of work on other replicas. An API-only rollout probe must be followed by a `serve` probe. A long first upload may be healthy with `worker_starting=true`; require real completed progress by its fixed startup deadline rather than repeatedly killing that first job during a short controller probe.

## Alert response

Use at least two samples to distinguish a brief restart or database interruption from a persistent incident. Choose final queue-delay thresholds from the deployment's observed Cron, cold-start, lease-recovery, and provider behavior. The following are initial operating policies to test in staging:

| Signal | Action |
| --- | --- |
| Operations HTTP 503, missing heartbeat, or no fresh `serve`/`worker` | Check durable controller state, current image revision, process exit/restart logs, database reachability, primary role, and runtime privileges. Respect an explicitly reported first-iteration startup deadline, then require real progress. A maintenance API process alone does not restore publication. Do not blindly restart a process with an uncertain provider operation. |
| Oldest due age remains above 2 minutes and increases | Investigate queue capacity, account locks, pacing, provider latency, and pool waiting. Escalate if it crosses the release's measured delay objective. Compare with worker progress: a fresh loop can still have a blocked backlog. |
| `needs_attention` increases | Inspect the affected owner-visible target history and saved checkpoint. Confirm the destination's existing result before using a resolution action. Keep the remote ID, error and audit note. Never turn uncertainty into a fresh publication automatically. |
| `reauth_required` increases | Diagnose provider access and interrupted refreshes, then request a new browser authorization from the account owner. Do not clear refresh quarantine or restore an old credential by hand. |
| Sustained provider wait plus growing due age | Check the specific provider's configured endpoint budget, shared cooldown and actual 429 responses. A normal nonzero cooldown count is expected. Retain known publish IDs and continue polling after the deadline rather than creating a replacement post. |
| Empty-pool wait or cancelled acquisitions increase across samples | Compare both pools with the replica connection budget. Reduce admitted concurrency or resolve the slow dependency before increasing replicas. Include the lock pool, migrations, administration, and monitoring clients in the database budget. |
| Repeated container restarts or changing instance IDs | Examine exit cause, memory/resource limits, startup secrets, migration compatibility, and the controller's maintenance/desired state. Verify the expected compiled revision after recovery. |
| `operations_heartbeat_failed` or `operations_retention_failed` logs | Diagnose database/network/privilege failures. These log entries contain a fixed event name and omit SQL, credentials, provider tokens, and URLs. |
| Retention repeatedly scans a full batch and its last-success timestamp falls behind | Check whether cleanup throughput matches growth and whether retained unresolved evidence is the main source. Increase the batch within its bound or schedule bounded maintenance more frequently through the operations manager. Do not remove the unresolved-state guard to clear an alert. |

Pool counts describe the process that answered the request. Aggregate them across instances to compare actual usage with the deployment connection cap. Historical heartbeat rows are retained for seven days, so `stale_workers` can include stopped instances; use the current controller instance and fresh-worker count to identify an outage.

## Events and pagination

The owner-scoped post-events API accepts `after_id` and `limit`. IDs are ascending and `after_id` is exclusive. The default limit is 100; the maximum is 200. The response provides `events`, `next_after_id`, and `has_more`. Supply the previous `next_after_id` to read the next page. An empty page retains the supplied cursor, which also supports subsequent polling. A negative cursor is rejected. A foreign owner cannot discover whether the post or its events exist.

The legacy `ListEvents` store method returns only the first 100 events. Integrations that need a complete history must follow the cursor. Deletion of old routine events may leave ID gaps; pagination does not require contiguous IDs. New events appended between page reads are returned after earlier IDs, without an offset shift. Each page has a consistent database snapshot; a sequence of pages is not a frozen export while new work and retention continue.

## Retention policy

`EVENT_RETENTION_DAYS` defaults to **90 days** and is rejected below **7 days**. `RETENTION_BATCH_SIZE` defaults to **500**, with a maximum of **2,000**. The process runs one bounded pass on startup and then once per hour by default, so frequent restarts cannot indefinitely postpone all cleanup. Each pass uses database time and has a five-second deadline; a failed pass rolls back.

Automatic event cleanup considers only these routine delivery kinds: `target.queued`, `target.claimed`, `target.checkpoint`, `target.processing`, `target.retrying`, and `target.lease_recovered`. It deletes an old candidate only after locking its parent and confirming that every target of that post is resolved:

- The target statuses must all be `published`, `failed`, or `cancelled`.
- No target may have an unsafe checkpoint.
- A failed or cancelled target may not retain a known provider operation in its progress.

Thus an unresolved target preserves the routine history of its entire post, including the history of published siblings. Parent locks serialize eligibility with retry, resolution, checkpoint, and finish operations. Busy parents are skipped and reconsidered on a later sweep.

Final publication events, owner resolution events, failed/cancelled result events, all post/account history, and unknown future event kinds remain retained. Posts, targets, idempotency keys/request hashes, first-start timestamps, saved provider operations, remote IDs/URLs, and token/refresh state are never deleted by this job. In particular, idempotency keys do not become reusable as events age out.

A durable scan cursor examines at most the batch limit of old routine events per pass. It advances past protected rows and wraps at the end, so large unresolved histories do not cause an unbounded scan or permanently prevent cleanup of eligible later histories. Protected rows are reconsidered after the cursor wraps. The cursor itself is operational state and must remain present.

The same pass separately removes at most the batch limit from each transient class:

| Class | Eligibility |
| --- | --- |
| API admission leases | `expires_at` has passed. Live leases remain in force. |
| Owner request-budget rows | Their fixed 60-second window has expired and the owner has no live admission. This does not affect queue state or idempotency. |
| OAuth states | Their expiry has passed. OAuth consumption still checks expiry independently. |
| Provider pacing rows | `next_allowed_at` has passed. Future reservations/cooldowns remain intact. |
| Process heartbeats | Last seen at least seven days ago. |

No unresolved refresh intent is removed on a timer. It is resolved only by a fenced replacement, a proven rejection, quarantine, disconnect, or new authorization.

This policy bounds transient history growth while preserving reconciliation and idempotency evidence. It is not complete business-data deletion. If permanent records need archival or legal deletion, define a separate owner-authorized policy that retains the publication/idempotency guarantees before introducing destructive code.

## Backup preparation

Preserve the PostgreSQL backup and the encryption keyring independently. Keep every old key needed by current ciphertext **and by any retained backup**. Do not discard an old key merely because current active rows were re-encrypted. Back up the binary/image revision and its migration manifest as well.

The backup manifest must include exact post count, target count, non-null encrypted-account count, event count, maximum event ID, backup timestamp/snapshot identity, and the release revision. Generate the counts and maximum ID from the same consistent database snapshot as the dump, or while the service is drained. For the event population, capture `count(*)` and `COALESCE(max(id),0)` together from `sp_events`; both values must be zero for an empty snapshot. Separately observed live counts may change during backup and cannot be used as exact restore expectations. Counts alone are not an authenticity check: retain the backup's integrity/checksum record in the approved backup system.

Practice restoration into an isolated PostgreSQL writer on the same chosen direct/session-preserving connection path used by the application. The standalone command below first validates the DSN policy and runs read-only acceptance, then proves distinct stable PostgreSQL sessions and advisory-lock exclusion through that same restored DSN. Final success requires both checks. Exercise the complete native lock-loss/concurrency gate there before enabling publication as well.

## Restore and publication reconciliation

Database restoration cannot undo remote publications or token rotations performed after the backup. The restore procedure therefore requires deliberate reconciliation before any resumed provider call.

1. Enter the controller's durable maintenance state and confirm Cron/public traffic cannot restart publication. Stop every worker and token-maintenance process. Record the last known running revision and the incident time range.
2. Restore the backup into an **isolated** native PostgreSQL database. Keep outbound provider access disabled and public routing closed. Restore required roles/privileges and independently retrieve the retained keyring without writing secrets into the repository or terminal transcript.
3. Apply migrations from the exact intended release artifact if needed. Do not edit applied migration files or reuse an older binary against a newer manifest. The verifier checks that the restored database matches this source's embedded manifest and runtime privilege requirements.
4. Quarantine restored work with the guarded SQL below. It moves every nonpublished target to `needs_attention`, clears worker leases, preserves all checkpoints/receipts, and emits restore audit events. Even a backed-up failed/cancelled target could have changed after the backup, so it also requires reconciliation. All restored active accounts become `reauth_required`; token versions advance and refresh intents clear under that quarantine, while ciphertext is retained for verification.

   ```bash
   psql "$RESTORE_DATABASE_URL" -X -v ON_ERROR_STOP=1 \
     -v restore_isolated=true -v maintenance_confirmed=true \
     -f docs/restore-quarantine.sql
   ```

5. Supply expectations from the independent backup manifest and run the verifier. Set the DSN and keyring with the environment's secret mechanism; the following command intentionally contains no secret values.

   ```bash
   export RESTORE_ISOLATED=true
   export RESTORE_MAINTENANCE_CONFIRMED=true
   # Also supply RESTORE_DATABASE_URL, TOKEN_ENCRYPTION_KEYS,
   # ACTIVE_TOKEN_KEY_ID, RESTORE_EXPECTED_POSTS,
   # RESTORE_EXPECTED_TARGETS, RESTORE_EXPECTED_ENCRYPTED_ACCOUNTS,
   # RESTORE_EXPECTED_EVENTS, and RESTORE_MAX_EVENT_ID from the independent
   # backup snapshot. Do not recount after quarantine to create expectations.
   # Only a loopback/Unix-socket test database may additionally use
   # RESTORE_ALLOW_LOCAL_PLAINTEXT=true. Production uses verified TLS.
   docs/verify-restore.sh
   ```

   The command requires Go and Python 3. It disables inherited Go flags, workspace and persistent Go configuration, fixes the selected test and skip expressions, and requires JSON evidence that `TestRestoredDatabaseSafety` ran and passed exactly once with no skipped or failed tests. An exit-zero command that ran no tests fails acceptance. Unset `TEST_PGLITE` completely; even an empty or false value is rejected. The restored DSN is the connection authority for both the native preflight and the read-only test; ambient `PG*` defaults are removed. Stdout contains one safe JSON acceptance summary; failed test output is written to stderr.

   Acceptance fails for missing expectations, a non-isolated/non-maintenance declaration, failed independent-session proof, incompatible schema/privileges, unexpected row counts, unquarantined work, remaining leases/refresh intents, or any credential/verifier that the retained keys cannot authenticate and decrypt. It compares the exact count and maximum ID of events with `id <= RESTORE_MAX_EVENT_ID` to the independent backup manifest. New quarantine audit rows cannot mask missing original receipts, and audit rows already present in an older backup remain part of that backup's population. Preserve the restored event identity sequence and run this check before retention or any other writer is enabled.

   At least one restored encrypted account is required; an empty credential set cannot prove key recovery. The test checks all ciphertext in batches of 200 using the original owner/platform/provider identity or OAuth-state binding. It does not print decrypted credentials, perform OAuth, refresh tokens, or publish.

6. After decryption verification, invalidate all restored OAuth states before allowing browser traffic; a consumed state may have reappeared in the backup. On this isolated restore, execute `DELETE FROM sp_oauth_states` in the controlled administrative session. Request fresh browser authorization for restored accounts. Do not make old ciphertext active by hand.
7. For every quarantined target, inspect the provider's result over the backup-to-incident interval and use the owner-scoped resolution workflow with an audit note. Confirm an existing publication with its remote receipt; confirm absence only with sufficient provider evidence; resume processing only for the known existing operation. Resolve uncertainty before permitting a fresh retry.
8. Start the intended image through the controlled maintenance verification path. Verify the immutable `build_revision`, an API-role probe, then the `serve` role with acceptable oldest-due age. Honor the explicit initial worker grace during long first jobs and require actual progress before its deadline. Run the native PostgreSQL acceptance suite through the actual connection path. Keep maintenance enabled if any check fails.
9. Resume supervision/public routing deliberately. Watch worker freshness, queue delay, account reauthorization, attention counts, restart rate, and pool wait. Retain the restore transcript's non-secret results, manifest, revision, verification exit status, and reconciliation decisions with the incident record.

Run a timed restore drill for each release that changes schemas, key handling, or recovery semantics. Record measured restoration time and data-loss interval from the drill; do not infer them from a successful unit test.
