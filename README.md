# Social Publisher

A native Go backend for connecting users' TikTok, X, and Instagram accounts, publishing original content to several destinations, and scheduling delivery through a durable PostgreSQL queue.

Your application supplies login and its composer. This service supplies account OAuth, encrypted provider credentials, validation, scheduling, workers, delivery status, and publication receipts. A single request can select one account on each of the three platforms. Each destination progresses independently, so a successful publication remains successful when another destination needs attention.

## What is included

| Area | Behavior |
| --- | --- |
| Account connection | OAuth callbacks bound to a signed-in application user and the initiating browser |
| Access control | Short-lived HS256 JWTs; resources scoped to both tenant and user |
| Publishing | TikTok videos/photo posts, X original text/media posts, Instagram images/Reels/carousels |
| Scheduling | An explicit RFC3339 instant, stored in UTC; optional IANA timezone for display |
| Reliability | Fair, index-backed PostgreSQL dispatch, leases, checkpoints, provider pacing, and independent target status |
| Recovery | Resume known operations; quarantine interrupted refreshes; require reconciliation after uncertain publication |
| Admission | Shared owner request/concurrency budgets, per-account operation limits, and atomic pending-target caps |
| Operations | Protected diagnostics, actual worker-progress heartbeats, event pagination, and bounded retention |

Publishing now means immediately eligible for workers. Platform processing and quotas determine when a post becomes visible; the service does not promise simultaneous visibility across networks.

The integration boundary is deliberately small: the repository does not include consumer signup, a composer frontend, a hosted-upload API, transcoding, Stories, analytics, or provider webhooks. Host prepared media on an HTTPS origin you control. The common cross-platform example is one MP4 video with a short caption and explicit per-platform choices.

## Quickstart with Docker and PostgreSQL

Run these commands from the directory containing this README. Prerequisites are Docker with Compose and an available local port 5432/8080. The Go image builds the binary; you do not need Go installed for this path.

### 1. Build and generate keys

```sh
cp .env.example .env
docker build -t social-publisher:local .
docker run --rm social-publisher:local keygen
openssl rand -hex 24
```

`keygen` prints a JSON object containing `JWT_HMAC_KEY`, `TOKEN_ENCRYPTION_KEYS`, `ACTIVE_TOKEN_KEY_ID`, `OPERATIONS_TOKEN`, and `CONTROL_TOKEN`. Paste the signing, encryption and operations values into `.env`; the separate control token is only used by a hosted controller and isn't needed for Docker or native runs. Paste the random hexadecimal output into `POSTGRES_PASSWORD`. No secrets are supplied by the repository.

For the encryption keyring, preserve dotenv quoting:

```dotenv
JWT_HMAC_KEY=PASTE_GENERATED_JWT_KEY
TOKEN_ENCRYPTION_KEYS='{"primary":"PASTE_GENERATED_ENCRYPTION_KEY"}'
ACTIVE_TOKEN_KEY_ID=primary
OPERATIONS_TOKEN=PASTE_GENERATED_OPERATIONS_TOKEN
```

Keep the other local defaults for the initial startup. Provider credential pairs may stay empty until you configure developer apps. Set `MEDIA_ALLOWED_HOSTS` to your exact asset hostname before creating media posts, for example `media.example.com`.

### 2. Start the database, migrate, then start the service

```sh
docker compose config --quiet
docker compose up -d postgres
docker compose run --rm migrate
docker compose up -d publisher
curl --fail http://localhost:8080/healthz
curl --fail http://localhost:8080/readyz
```

The migration command must succeed before service startup. It applies the SQL embedded in the binary; `serve` does not migrate automatically. Compose binds both published ports to `127.0.0.1` and persists PostgreSQL data in a named volume. `docker compose down` stops containers while retaining that volume.

This configuration is for local development. Use HTTPS, separate production secrets, a direct/session-preserving PostgreSQL connection with `sslmode=verify-full`, and `DEV_MODE=false` for deployment. Local HTTP does not establish that a provider will accept a development callback URL.

### 3. Mint a token for one application user

```sh
docker compose run --rm --no-deps publisher token \
  --tenant demo --user user_123 --ttl 1h
```

Use the returned short-lived token for API calls:

```sh
export APP_TOKEN='PASTE_THE_RETURNED_USER_TOKEN'
curl --fail http://localhost:8080/v1/platforms \
  -H "Authorization: Bearer ${APP_TOKEN}"
```

The CLI is a local integration aid. In production, your trusted application backend authenticates the user and signs the corresponding JWT. The HMAC signing key stays on trusted servers. A shared token for every customer, or a user ID supplied in an HTTP header, does not provide user isolation.

### 4. Connect accounts and submit content

Complete the [browser connection flow](docs/api.md#connect-an-account-in-the-browser), then call `GET /v1/accounts` to obtain account IDs. OAuth initiation sets an HttpOnly browser cookie: starting the flow with curl and opening the returned URL in an unrelated browser will not complete it.

Use the [three-platform video example](docs/api.md#publish-one-original-video-to-three-platforms) to submit a post. The API validates all selected destinations before creating queued work. Send a stable `Idempotency-Key`: first creation returns **201**, a replay returns the existing post with **200**, and the same key with changed content returns **409**.

Omit `publish_at` for immediate eligibility. Supply a future RFC3339 instant to schedule. Retrieve `GET /v1/posts/{id}` and inspect every target's `status`, `last_error`, `warnings`, and optional receipt. A `needs_attention` result requires checking the provider account before resolving it; it is never an invitation to create another copy blindly.

## Native Go

The module requires Go 1.26 or newer; the Docker builder is pinned to Go 1.27.4. With a reachable native PostgreSQL database and your completed `.env`:

```sh
make build
set -a
. ./.env
set +a
./bin/publisher migrate
./bin/publisher serve
```

The binary reads environment variables, not dotenv files. Source only a file you control. You can use the Compose database alone with `docker compose up -d postgres`; the native `.env` URL connects through its loopback port.

For separate processes, run `./bin/publisher api` and `./bin/publisher worker` with the same database, keyring, public origin, and provider configuration. `worker` includes token maintenance and exposes no HTTP listener. At least one `worker` or `serve` process must remain running for scheduled delivery and proactive token refresh.

## Platform activation

| Platform | Required before publishing for real users |
| --- | --- |
| TikTok | Registered app with Content Posting API/Direct Post and approved `video.publish` access; verified source domain or URL prefix; compliant composer UI with current creator capabilities, preview, consent, interaction and commercial choices. Public visibility requires the relevant audit. `TIKTOK_AUDITED=false` restricts this adapter to private accounts and `SELF_ONLY`. |
| Instagram | Instagram Login app, professional Business/Creator accounts, `instagram_business_basic` and `instagram_business_content_publish`, and the access/review needed for the users you serve. This uses direct Instagram Login. |
| X | Developer app with OAuth 2.0 user authorization and the requested scopes, including `offline.access` and `media.write`; pay-per-use API credits and posting access. Original posts and confirmed same-account thread replies are supported; replies use a local publisher parent ID, never arbitrary upstream options. See [X adapter](docs/x-adapter.md). |

TikTok's [Direct Post documentation](https://developers.tiktok.com/docs/en/content-posting-api-reference-direct-post) explains audit, consent, and verified-URL requirements. The [Instagram adapter guide](docs/instagram-adapter.md) records the integration path and outstanding live checks. X documents its current [credit pricing](https://docs.x.com/x-api/getting-started/pricing) and [automation rules](https://help.x.com/en/rules-and-policies/x-automation).

Credentials and a running backend do not constitute platform approval. Provider fixtures do not establish live OAuth access, public posting, or a successful app review. Complete an authorized account smoke test for each enabled platform before exposing it to customers.

## Verification

```sh
make test
make test-race
make vet
```

Without `TEST_DATABASE_URL`, PostgreSQL integration tests report a skip. For the native PostgreSQL gate, create a dedicated test database, export its URL, unset `TEST_PGLITE`, and run `make test-postgres`. See [database testing](docs/deployment.md#database-testing).

The test layers have different meanings:

- Local HTTP fixtures check adapter payloads and recovery classifications without live provider access.
- Sequential PGlite checks can exercise PostgreSQL SQL, schema, and state transitions, with `TEST_PGLITE=1` explicitly selecting that limited mode.
- Native PostgreSQL tests are required for independent-session locks, concurrent claims, fencing, and cancellation when a database session holding a lock is lost. PGlite multiplexes sockets over one backend and cannot prove those properties.

The included [GitHub Actions workflow](.github/workflows/test.yml) runs vulnerability scanning, fixture tests with race detection, a strict native PostgreSQL gate, `go vet`, and a binary with the commit revision compiled in. The native gate verifies independent sessions, rejects skips and source changes, and retains machine-readable results. Its database is disposable and it requires no provider credentials or deployment secrets.

Running against your own database and your own provider apps is up to you: this repository doesn't claim a completed cloud deployment or live provider approval in your environment.

## Guides and source

- [Operational diagnostics, retention and recovery](docs/operations.md)
- [API and integration examples](docs/api.md)
- [Configuration, Docker, Railway, R2, and operations](docs/deployment.md)
- [TikTok adapter details](docs/tiktok-adapter.md)
- [X adapter details](docs/x-adapter.md)
- [Instagram adapter details and live verification boundaries](docs/instagram-adapter.md)

The main entry point is `cmd/publisher/main.go`; routes are in `internal/api`, persistence in `internal/store`, scheduling in `internal/worker`, and platform implementations in `internal/provider`.

The default-off [central SPAWN lifecycle service](docs/central-lifecycle.md) is the integration SPAWN itself uses to post updates about Backroom work. It stays off unless you configure it.

## Contributing and license

Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues privately: see [SECURITY.md](SECURITY.md). Licensed under [Apache-2.0](LICENSE). Copyright 2026 SPAWN.
