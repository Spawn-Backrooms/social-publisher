# Deployment and operations

This service runs as a native Go process with PostgreSQL. Use `serve` for a combined API/worker deployment, or run `api` and `worker` separately. PostgreSQL stores the durable queue, account ownership, encrypted credentials, OAuth state, checkpoints, and receipts. The application filesystem does not hold scheduled work.

## Configuration

`publisher` reads process environment variables. It does not load `.env` itself. The supplied `.env.example` is accepted by Docker Compose and by a shell when explicitly sourced.

| Variable | Requirement / default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | Required | PostgreSQL DSN with `sslmode=verify-full` in production. Use a direct or session-preserving connection for advisory locks; Hyperdrive is incompatible with these locks. |
| `PUBLIC_BASE_URL` | Required | Exact external origin, such as `https://publisher.example.com`; no path, query, fragment, or credentials. Callback URLs are derived from it. |
| `DEV_MODE` | `false` | `true` requires a loopback public origin, permits the plaintext local Compose database, and disables Secure OAuth cookies. Public origins require production mode. |
| `PORT` | `8080`; 1–65535 | HTTP listener port for `serve`/`api`. The listener binds all interfaces inside its deployment environment. |
| `JWT_HMAC_KEY` | Required | Standard base64 containing at least 32 random bytes; used to verify application JWTs. |
| `JWT_ISSUER` | Required | Expected application JWT issuer. |
| `JWT_AUDIENCE` | Required | Expected JWT audience. |
| `TOKEN_ENCRYPTION_KEYS` | Required | JSON object mapping key IDs to standard-base64, exactly 32-byte encryption keys. |
| `ACTIVE_TOKEN_KEY_ID` | Required | Existing keyring entry used for new encrypted credentials and OAuth verifiers. |
| `OPERATIONS_TOKEN` | Required in production | Separate standard-base64 token containing at least 32 random bytes for private diagnostics. |
| `EXPECTED_BUILD_REVISION` | Optional outside controlled release | Full lowercase 40/64-hex revision; startup and migration fail before side effects if it differs from the compiled revision. Cloudflare sets it from `RELEASE_REVISION`. |
| `MEDIA_ALLOWED_HOSTS` | Empty | Comma-separated exact DNS hostnames under your control. Empty rejects media URLs. Wildcards and arbitrary user origins are unsupported. |
| `MAX_MEDIA_BYTES` | `1073741824` | Global per-asset byte cap; accepted range 1–17179869184. Platform caps may be lower. |
| `WORKER_CONCURRENCY` | `6`; 1–32 | Concurrent delivery workers per process. |
| `DB_MAX_CONNS` | `32`; 4–128 | Cap for each of two pools per Go process: ordinary queries and session locks. Budget up to `2 × DB_MAX_CONNS` connections per process, plus release/administration connections. |
| `JOB_TIMEOUT_SECONDS` | `600`; 30–1800 | Timeout for one worker operation. Longer remote processing continues through saved checkpoints and polling. |
| `API_REQUESTS_PER_MINUTE` | `60`; 1–10000 | Admitted requests per verified owner in a fixed 60-second database window. |
| `API_OWNER_CONCURRENCY` | `4`; 1–64 | Active admitted API requests for one owner across replicas. |
| `API_ACCOUNT_CONCURRENCY` | `1`; 1–8 | Active API provider operations for one provider identity across replicas. |
| `PENDING_TARGETS_PER_OWNER` | `1000`; 1–100000 | Atomic destination-target cap for queue creation, retry and resumption. |
| `EVENT_RETENTION_DAYS` | `90`; 7–3650 | Minimum age of routine terminal delivery events eligible for bounded cleanup. |
| `RETENTION_BATCH_SIZE` | `500`; 1–2000 | Maximum event candidates and transient rows per cleanup batch/table. |
| `TIKTOK_CLIENT_KEY`, `TIKTOK_CLIENT_SECRET` | Both or neither | TikTok application credentials. |
| `TIKTOK_AUDITED` | `false` | Set true only after the applicable TikTok audit is approved; it is not a request for approval. |
| `X_CLIENT_ID`, `X_CLIENT_SECRET` | Both or neither | X OAuth 2.0 application credentials. |
| `INSTAGRAM_CLIENT_ID`, `INSTAGRAM_CLIENT_SECRET` | Both or neither | Credentials for the Instagram Login product. |
| `INSTAGRAM_GRAPH_VERSION` | `v26.0` | Explicit Graph version, in `vN.N` form; review compatibility before changing. |
| `POSTGRES_PASSWORD` | Local Compose only | Required password for its development database; not read by Go configuration. |

All three long-running modes load the same required configuration, including JWT and encryption settings. A platform is enabled only when its credential pair is supplied. `GET /v1/platforms` reports configured adapters; it does not establish platform approval or working account access.

### Generate and enter secrets

```sh
make build
./bin/publisher keygen
openssl rand -hex 24
```

`keygen` prints fresh signing/encryption material and separate operations/control tokens as JSON; `openssl` supplies a URL-safe random local database password. Paste values into your secret manager or private `.env`. The Cloudflare `CONTROL_TOKEN` stays with the Worker; `OPERATIONS_TOKEN` authorizes the container's private diagnostic endpoint. Neither token is an application-user JWT.

The dotenv form must preserve the inner JSON quotes:

```dotenv
JWT_HMAC_KEY=PASTE_GENERATED_JWT_KEY
TOKEN_ENCRYPTION_KEYS='{"primary":"PASTE_GENERATED_ENCRYPTION_KEY"}'
ACTIVE_TOKEN_KEY_ID=primary
```

For Railway or another environment-variable dashboard, enter the keyring as the **raw JSON value** `{"primary":"PASTE_GENERATED_ENCRYPTION_KEY"}`, without the surrounding single quotes. Those outer quotes are dotenv/shell syntax, not part of the application's value. Docker Compose strips them according to its [environment-file rules](https://docs.docker.com/compose/how-tos/environment-variables/variable-interpolation/). A plain `docker run --env-file` has different parsing semantics; use the supplied Compose configuration rather than passing this quoted `.env` directly to that command.

The local example expands `POSTGRES_PASSWORD` into `DATABASE_URL`. Use the generated hexadecimal password for this path. Arbitrary passwords containing URL-reserved characters must be percent-encoded when placed in a URI. Production connection strings should come from the database's provisioned credentials.

## Docker and PostgreSQL

The Dockerfile uses `golang:1.27.4-bookworm`, builds with `CGO_ENABLED=0`, and copies the binary and CA certificate bundle into a non-root `scratch` image. The binary embeds IANA timezone data through `time/tzdata`. The image runs `/publisher serve` by default; it contains no shell or curl. Source copying is limited to Go module files, `cmd`, and `internal`; no `.env` is copied into the image.

The supplied Compose database uses `postgres:18-bookworm`. Its named volume mounts at `/var/lib/postgresql`, matching the PostgreSQL 18 image's versioned data layout. See the [official PostgreSQL image documentation](https://github.com/docker-library/docs/blob/master/postgres/README.md). The image tag fixes the major version and Debian variant while allowing patch updates; pin a reviewed digest in your deployment process if you require immutable image resolution.

After completing `.env`:

```sh
docker build -t social-publisher:local .
docker compose config --quiet
docker compose up -d postgres
docker compose run --rm migrate
docker compose up -d publisher
```

`config --quiet` checks Compose configuration without printing resolved secrets. Both published ports bind to loopback. The publisher container is non-root, read-only, and runs with dropped Linux capabilities. PostgreSQL's local `publisher` role is convenient for development; provision appropriate application and migration roles for production.

View application logs and readiness:

```sh
docker compose logs --tail=100 publisher
curl --fail http://localhost:8080/readyz
```

Use `docker compose down` to stop local services while preserving the named volume. Changing `POSTGRES_PASSWORD` in `.env` after database initialization does not rotate the existing database role's password; rotate the role and its consumers deliberately.

## Native processes and migrations

With Go 1.26+ installed, build and export the completed configuration:

```sh
make build
set -a
. ./.env
set +a
./bin/publisher migrate
./bin/publisher serve
```

Source only your own controlled configuration file. With a native PostgreSQL installation, replace the example `DATABASE_URL` as appropriate. With only the Compose database running, the example points to its loopback-bound port.

| Command | Responsibilities |
| --- | --- |
| `publisher serve` | HTTP API, publishing worker pool, proactive credential maintenance |
| `publisher api` | HTTP API only; token refresh can occur while handling provider operations |
| `publisher worker` | Publishing worker pool and proactive credential maintenance; no HTTP listener |
| `publisher migrate` | Apply embedded schema migrations; requires `DATABASE_URL` with verified production TLS, or explicit `DEV_MODE=true` for local development |
| `publisher keygen` | Generate fresh application signing/encryption material |
| `publisher token --tenant … --user … --ttl 1h` | Mint one application's user token; requires the JWT environment settings |
| `publisher version` | Print immutable compiled `build_revision`; needs no database or secrets |

Run migrations as an explicit release step before starting the new service. The migration command uses a database lock, records checksums, and rejects changed or unknown previously applied migrations. There is no automatic down-migration command. Back up the database before schema changes and check application compatibility before rolling back a binary.

For separate scaling, start `publisher api` and `publisher worker` as independently supervised processes with the same database and key material. Keep at least one worker active continuously. Increasing replicas also increases connection pools and platform API traffic; size the aggregate database connection budget and provider allowances accordingly. The account locks require a direct database connection or session-preserving pooling, rather than transaction pooling.

## Cloudflare Containers

SPAWN runs this service on Cloudflare Containers with a Worker controller. That controller isn't published in this repository yet. Docker, native processes and Railway below are complete paths.

## Railway

Deploy the Go service as a long-running native container. Railway [detects a Dockerfile](https://docs.railway.com/builds/dockerfiles) in the service's source root and supports a separate [pre-deploy migration command](https://docs.railway.com/deployments/pre-deploy-command).

1. Create a Railway project and add a PostgreSQL service. Configure its storage, backup policy, and credentials. The [PostgreSQL service documentation](https://docs.railway.com/databases/postgresql) explains its connection variables; retain a direct/session-preserving connection for this application.
2. Add the repository as a service. Set its root directory to the directory containing this Dockerfile if the repository has a higher-level parent. Use the supplied Dockerfile for the build.
3. Add the direct, session-preserving PostgreSQL connection using Railway's variable picker. The service requires server identity verification in production (`sslmode=verify-full`). Confirm the selected PostgreSQL endpoint actually supports verified TLS; a plaintext service URL or a TCP proxy alone does not supply TLS. For a database service named `Postgres`, the usual variable reference is `${{Postgres.DATABASE_URL}}`, but its transport must meet this requirement.
4. Generate the service's HTTPS domain or configure a custom domain. Set `PUBLIC_BASE_URL` to that exact origin and `DEV_MODE=false`. Add the generated signing/encryption keys, issuer/audience, media hosts, and provider credentials as runtime variables. Let Railway supply `PORT`; the Go server reads it.
5. Set the pre-deploy command to `/publisher migrate`. Give it an explicit timeout suitable for your migration size. Migrations use the database and embedded SQL, so they do not require persistent application filesystem access.
6. Use the default image command for a combined `serve` deployment. Set the HTTP healthcheck path to `/readyz` and use an always-running service so delivery and token maintenance continue when there are no incoming requests.
7. Register the final callback URLs with each provider, then complete the browser connection and authorized publishing checks before enabling customer use.

For separate API and worker services, deploy the same image twice. Use full custom start commands `/publisher api` and `/publisher worker`; Railway's [Docker start-command override](https://docs.railway.com/deployments/start-command) replaces the image entrypoint. Keep the migration pre-deploy step on the API/release path. Share the same runtime settings with both services. Expose a public domain and `/readyz` healthcheck only for the API; the worker has no HTTP server.

Railway's [healthcheck](https://docs.railway.com/deployments/healthchecks) controls readiness during deployment and uses the configured/injected port. Set up ongoing monitoring separately. Deployment instructions here have not been demonstrated against a live Railway account.

## Media hosting and optional Cloudflare R2

The service accepts existing media URLs. Your application uploads assets to your storage layer and obtains reliable metadata before submitting a post. Use immutable object paths: changing bytes under a scheduled URL can change what gets published.

For R2, connect an HTTPS custom domain such as `media.example.com` to a bucket and add that exact hostname to `MEDIA_ALLOWED_HOSTS`. Cloudflare documents custom domains for production access and describes `r2.dev` as a development endpoint in its [public-bucket guide](https://developers.cloudflare.com/r2/buckets/public-buckets/).

Ensure the selected objects are fetchable without browser cookies or an interactive challenge, have a correct `Content-Type`, and stay available until delivery and all provider processing have completed. A redirect to another hostname is unsuitable: this service's media downloads disable redirects. Avoid expiring source URLs for distant schedules. Serve files on HTTPS port 443; the media policy rejects private/internal destinations and unapproved hosts. TikTok additionally requires verified ownership of the domain or URL prefix used by its pull-from-URL publishing flow.

For a common video, prepare an MP4 with compatible H.264 video/AAC audio, known duration, dimensions, and byte size; every destination still applies its own limits. The backend does not transcode or prove codec compatibility from metadata. X uploads bytes through the service; TikTok and Instagram retrieve approved source URLs through their publishing APIs.

R2 can provide the assets while Cloudflare Containers or another container host runs the Go daemon. SPAWN's Cloudflare controller isn't published here yet; with Docker or Railway, R2 works the same way.

## Credential maintenance and rotation

`serve` and `worker` scan for refresh candidates every minute, including accounts whose scheduled posts are far in the future. Refresh operations and publishing are serialized per account. Credentials are stored encrypted, and replacement tokens use a version check so an outdated refresh cannot resurrect a disconnected account.

Before any refresh request, the service commits a version-fenced intent. Saving the replacement token clears that intent in the same transaction. After a process dies or an acknowledgement is lost, an unresolved intent is quarantined before using the previous credential; it cannot recover a new token whose response was lost. Explicit proof that the provider rejected a refresh without rotation permits a fenced retry.

An expired/revoked grant or an uncertain refresh can mark an account `reauth_required`. Ask that owner to reconnect. Reconnection restores usable authorization but does not automatically retry an uncertain publication; use the target's status and the [recovery API](api.md#recovery-without-duplicate-publication).

To rotate the encryption key, first deploy a keyring containing both the old and new keys and select the new `ACTIVE_TOKEN_KEY_ID`. Retain old keys while any stored ciphertext still requires them. Refresh/reconnection writes new credentials with the active key; there is no bulk re-encryption CLI. Keep recoverable keys separately from database backups. JWT signing uses a single verification key: coordinate its rotation with your token issuer and clients because existing tokens signed with the previous key stop validating.

Disconnect removes local credentials and stops eligible pending deliveries. Remote revocation is a separate best-effort result in the response; the current Instagram adapter reports it as unconfirmed. Already submitted provider operations may still finish. For account removal, complete any provider-side grant removal and lifecycle requirements in your integrating application.

## Health and monitoring

`GET /healthz` returns 200 when the HTTP process is serving. `GET /readyz` performs bounded, non-mutating queries: migration versions, names and checksums must exactly match this binary; required runtime columns must resolve; the endpoint must be a writable primary; and the role must have every required runtime privilege. It returns 503 on any failure. All long-running modes perform the same check before starting. These checks do not contact providers or prove their quotas and permissions.

`GET /internal/ops` requires the separate `X-Operations-Token` and reports the compiled build revision, database time, process heartbeats, completed worker-loop progress, bounded queue summaries, both connection pools and last successful retention result. It returns 503 if the local role's required progress is stale. Keep `/internal/` off your public routes and reach it only from operator tooling. Worker-only mode has no listener but records its heartbeat and progress in PostgreSQL. See [operations](operations.md) for metrics, alerts and restoration checks.

The exact migration-manifest check deliberately rejects a database newer than the binary. Use the supplied controller's durable maintenance state to drain and stop the old service, suppress public routing and Cron restarts, and apply migrations from the verified release image. Resume supervision explicitly after the intended image and schema pass verification. A previous binary cannot be assumed compatible with a migrated database; rollback may require a tested database restoration. Zero-downtime schema changes require an explicit compatibility window beyond this release's maintenance procedure.

Monitor overdue targets, `needs_attention` outcomes, repeated safe failures, `reauth_required` accounts, database capacity, and provider consumption. Logs contain controlled events rather than raw callback URLs or credentials. Configure your reverse proxy and hosting logs to preserve that boundary: OAuth query strings and bearer headers must not become access-log fields.

Send SIGTERM for shutdown. The HTTP server stops gracefully and worker contexts are cancelled; unfinished work remains in PostgreSQL for recovery. Do not infer that a timed-out remote mutation failed merely because a process stopped.

## Database testing

Normal tests use local provider fixtures. The native release gate requires a dedicated disposable PostgreSQL database whose test role can create/drop test schemas and temporary roles and terminate its own lock backends. The temporary-role permission is used to prove readiness rejects missing write privileges. Never point this gate at a production database.

With the local Compose database running and `.env` exported:

```sh
docker compose exec postgres createdb -U publisher publisher_test
export TEST_DATABASE_URL="postgres://publisher:${POSTGRES_PASSWORD}@127.0.0.1:5432/publisher_test?sslmode=disable"
unset TEST_PGLITE
make test-postgres
```

Create `publisher_test` once. The `test-postgres` target requires a URL and requires `TEST_PGLITE` to be unset. It first proves independent sessions and advisory exclusion, runs the store/API/account/worker suites with race detection, and rejects skipped or missing required tests and source changes. The gate includes a real process-death refresh test, busy-account fairness and multi-replica TikTok pacing. See [verification tooling](../scripts/verify/README.md) for retained evidence and an unprivileged emulated Linux guest option.

The repository's [GitHub Actions workflow](../.github/workflows/test.yml) starts a disposable `postgres:18.4-bookworm` service and uses Go 1.27.4. It runs fixtures, the strict native gate, vulnerability checks, `go vet`, a revision-stamped static build,. It does not publish to social accounts or deploy the service. A committed workflow is not evidence of a completed external CI run; inspect the results for the revision you plan to deploy.

When the separate development tooling provides PGlite, `TEST_PGLITE=1` explicitly selects sequential compatibility checks. PGlite runs real PostgreSQL SQL in WebAssembly but its [socket adapter](https://pglite.dev/docs/pglite-socket) multiplexes clients over one backend. Those checks can find migration/query/state defects while leaving native lock correctness unproven. They do not replace the native PostgreSQL gate or authorized live-provider tests.
