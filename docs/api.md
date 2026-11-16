# API and application integration

The base URL is the configured `PUBLIC_BASE_URL`. Private routes begin with `/v1/` and require one `Authorization: Bearer <application-user-JWT>` header. Send `Content-Type: application/json` for every JSON request, including POST actions whose body is `{}`. Bodies are limited to 256 KiB, unknown fields are rejected, and each request must contain one JSON object.

The backend accepts users authenticated by your application. It does not provide signup, password login, a composer, or an upload endpoint.

## Application-user authentication

Your trusted backend signs HS256 JWTs using the decoded bytes of `JWT_HMAC_KEY`. Every token identifies **one tenant and one user**. The service checks:

| Claim | Requirement |
| --- | --- |
| `iss` | Equals `JWT_ISSUER` |
| `aud` | Equals `JWT_AUDIENCE`, or an audience array containing it |
| `tenant_id` | Nonempty tenant identifier |
| `sub` | Nonempty application user identifier |
| `iat`, `nbf`, `exp` | Required integral Unix timestamps; token is valid now and its lifetime is at most 24 hours |

Use short lifetimes and synchronized clocks; the verifier does not add clock-skew tolerance. Tenant and subject are bounded to 256 UTF-8 bytes and reject control characters or surrounding whitespace. The JOSE header accepts `alg` and optional `typ`; this verifier does not support arbitrary headers such as `kid` or other signature algorithms.

Keep the signing key on trusted servers. Your backend derives tenant/user identity from its authenticated session, rather than accepting an arbitrary tenant/user pair from a caller. A browser may receive a short-lived token for that authenticated user, but never the signing key. The service does not trust `X-User-ID`, `X-Tenant-ID`, or a user-supplied CSRF header as an identity mechanism. OAuth also requires its separate browser-binding cookie.

For a local integration token after exporting the JWT environment settings:

```sh
./bin/publisher token --tenant demo --user user_123 --ttl 1h
export APP_TOKEN='PASTE_THE_RETURNED_USER_TOKEN'
export API_BASE='http://localhost:8080'
```

With Docker Compose, use `docker compose run --rm --no-deps publisher token --tenant demo --user user_123 --ttl 1h`. Account IDs and post IDs obtained with one user's token do not authorize another user, even in the same tenant. Out-of-scope resource requests return 404.

## Routes

| Method and path | Purpose | Successful response |
| --- | --- | --- |
| `GET /healthz` | Public HTTP liveness | 200, `{"status":"ok"}` |
| `GET /readyz` | Public database/schema readiness | 200, `{"status":"ready"}`; unavailable, read-only, insufficient privileges, or mismatched schema is 503 |
| `GET /internal/ops` | Private operational diagnostics | Separate `X-Operations-Token`; 200 healthy, 503 unhealthy. Blocked on the public Cloudflare route. |
| `GET /v1/platforms` | Configured adapter flags | 200, `{"platforms":[…]}` |
| `POST /v1/connections/{platform}/authorize` | Start browser OAuth; body `{}` | 200, authorization URL and expiry; sets browser cookie |
| `GET /oauth/{platform}/callback` | Provider redirect; uses stored flow and cookie | 200, connected account JSON |
| `GET /v1/accounts` | List this user's connections | 200, `{"accounts":[…]}` |
| `GET /v1/accounts/{id}/capabilities` | Fetch current publishing capabilities | 200, capabilities object |
| `DELETE /v1/accounts/{id}` | Disconnect a connection | 200, local status and provider revocation result |
| `POST /v1/posts` | Validate and queue a post | 201 on creation; 200 on idempotent replay |
| `GET /v1/posts?limit=20` | List recent posts; limit 1–100 | 200, `{"posts":[…]}` |
| `GET /v1/posts/{id}` | Inspect a post and every target | 200, post object |
| `GET /v1/posts/{id}/events?after_id=0&limit=100` | Page through ordered lifecycle events | 200, `{"events":[…],"next_after_id":123,"has_more":true}`; maximum limit 200 |
| `POST /v1/posts/{id}/cancel` | Cancel eligible pending deliveries; body `{}` | 200, current post |
| `POST /v1/posts/{id}/reschedule` | Move an unstarted post using its version | 200, updated post |
| `POST /v1/posts/{id}/targets/{target}/retry` | Explicitly retry a safe failed target; body `{}` | 200, updated post |
| `POST /v1/posts/{id}/targets/{target}/resolve` | Resolve or safely resume a target needing attention | 200, updated post |

`{platform}` is `tiktok`, `x`, or `instagram`. `{target}` is the target's `id` returned in `targets`, not its account ID or platform name.

## Admission and event pagination

Requests share PostgreSQL-backed limits across API replicas. Defaults are 60 admitted requests per owner in a fixed 60-second database window, four simultaneous requests per owner, and one simultaneous provider operation per account identity. The owner is the verified `(tenant_id, sub)` pair. The pending queue cap is 1,000 destination targets per owner, including queued, running, processing and retrying work. Creation, explicit retry and processing resumption use the same atomic queue cap; an idempotent replay does not allocate more queue space.

A limit returns 429 with a `Retry-After` header and a controlled error code such as `request_rate`, `owner_concurrency`, `account_concurrency`, or `queue_capacity`. Failure to establish admission returns 503. Honor the delay and retain the original idempotency key for a publish retry. These budgets bound application work; provider quotas, paid credits and live account eligibility still apply.

OAuth callbacks establish their owner from the consumed browser-bound state before requesting admission. If a callback fails after that one-use state has been consumed, including a 429, restart the connect flow instead of replaying the callback URL.

Events are ordered by increasing numeric ID. Request `after_id=0` for the first page, then pass the returned `next_after_id` while `has_more` is true. The default page is 100 and the maximum is 200; an event ID is a cursor, never permission to read another owner's post. Routine historical events may be removed by the retention policy. Final receipts, manual resolution records and unresolved-publication evidence are retained as described in [operations](operations.md).

## Connect an account in the browser

Configure the provider app first and register the exact callback URL:

| Platform | Callback |
| --- | --- |
| TikTok | `https://publisher.example.com/oauth/tiktok/callback` |
| X | `https://publisher.example.com/oauth/x/callback` |
| Instagram | `https://publisher.example.com/oauth/instagram/callback` |

Replace the origin with `PUBLIC_BASE_URL`. The application does not accept a user-supplied callback or arbitrary post-login redirect URL.

The authorize POST stores a one-use flow for the JWT owner and sets an HttpOnly, SameSite=Lax binding cookie. Production cookies are Secure and use the `__Host-` prefix. The flow expires after ten minutes. The provider's callback must arrive in the **same browser at the same service origin**, carrying that cookie. The callback does not need an application JWT because the owner is already bound to the stored flow.

The API has no CORS configuration. Integrate the composer and these routes under the same public origin, for example by reverse-proxying `/v1/` and `/oauth/` to this service. A backend-for-frontend integration must preserve the browser-facing cookie/callback origin and forward the relevant `Set-Cookie` response correctly; a server-only authorize call followed by a cookie-less browser redirect fails.

This example runs on your same-origin composer page after your own backend has issued `userToken`. Call it from the user's connect-button handler so the popup is opened during a user gesture:

```js
async function connectAccount(platform, userToken) {
  const popup = window.open('about:blank', 'social-account-connect', 'width=650,height=800');
  if (!popup) throw new Error('Allow a popup to connect your account.');
  try {
    const response = await fetch(`/v1/connections/${platform}/authorize`, {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        Authorization: `Bearer ${userToken}`,
        'Content-Type': 'application/json',
      },
      body: '{}',
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error?.message ?? 'Connection could not start.');
    popup.location.assign(result.authorization_url);
  } catch (error) {
    popup.close();
    throw error;
  }
}
```

The callback currently displays JSON containing `status: "connected"` and the account; it does not send a `postMessage` or render a success frontend. Your composer can poll its authenticated `GET /v1/accounts` to detect the connection and close the popup, or ask the user to return after connection. Keep the JSON account ID for later target selection. No provider access or refresh token is returned to the browser.

Do not start OAuth with curl and then paste the URL into a different browser: curl's cookie jar is not that browser's cookie jar. A flow denied by the provider, expired, already consumed, or missing its binding cookie must be restarted. A stale flow that began before a later disconnect cannot silently reconnect that account.

## Accounts and capabilities

```sh
curl --fail-with-body "${API_BASE}/v1/accounts" \
  -H "Authorization: Bearer ${APP_TOKEN}"

curl --fail-with-body "${API_BASE}/v1/accounts/acct_REPLACE/capabilities" \
  -H "Authorization: Bearer ${APP_TOKEN}"
```

Account status is `active`, `reauth_required`, or `disconnected`. Identity and token expiry can be displayed; encrypted credentials and token versions are not exposed. Reconnect the affected owner when authorization is no longer usable.

Capabilities include `can_publish`, optional display/account information, `max_video_duration_seconds`, TikTok `privacy_levels` and disabled-interaction flags, and platform-specific `raw` information. Fetch current capabilities when rendering a composer. The service rechecks them while accepting and dispatching work, because permission, quota, privacy, and entitlement can change. Capability and validation calls may themselves consume provider API usage.

For TikTok, populate privacy choices from the returned list, show the selected creator, preview the actual content, collect consent, and honor disabled comment/duet/stitch controls. The booleans in a request are records of those choices; they are not a substitute for building that UI.

## Publish one original video to three platforms

First host a prepared MP4 on an allowed HTTPS hostname and connect one account per platform for this user. Replace all account IDs, the asset URL, and media metadata below with real values. The illustrative 30-second portrait video must fit the selected creator's current capabilities.

Save this as `post.json`:

```json
{
  "text": "A look behind the scenes of our latest project.",
  "media": [
    {
      "url": "https://media.example.com/launch/behind-the-scenes.mp4",
      "mime_type": "video/mp4",
      "size_bytes": 12000000,
      "duration_seconds": 30,
      "width": 1080,
      "height": 1920
    }
  ],
  "targets": [
    {
      "account_id": "acct_REPLACE_TIKTOK",
      "options": {
        "privacy_level": "SELF_ONLY",
        "consent": true,
        "preview_confirmed": true,
        "allow_comment": false,
        "allow_duet": false,
        "allow_stitch": false,
        "brand_content_toggle": false,
        "brand_organic_toggle": false,
        "is_aigc": false
      }
    },
    {
      "account_id": "acct_REPLACE_X",
      "options": {}
    },
    {
      "account_id": "acct_REPLACE_INSTAGRAM",
      "options": {
        "media_type": "REELS",
        "share_to_feed": true
      }
    }
  ]
}
```

The TikTok example represents a creator who chose only-me visibility and disabled interactions for a noncommercial, non-AI-generated video after previewing it. Do not copy these values as hidden defaults. `SELF_ONLY` must be a currently offered privacy option; with `TIKTOK_AUDITED=false`, the account must also be private. Public visibility requires actual audit approval, the matching configuration, and the creator's explicit available privacy choice. Set commercial/AI disclosures truthfully for the content being sent.

Submit for immediate eligibility:

```sh
curl --fail-with-body -i "${API_BASE}/v1/posts" \
  -H "Authorization: Bearer ${APP_TOKEN}" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: launch-video-2026-001' \
  --data-binary @post.json
```

This call authorizes and validates every destination before committing the parent post and its targets. An invalid destination rejects the request without queuing the others. A 201 response means the post was accepted into the queue, not that the platforms have already published it.

### Content and overrides

The top-level `text` and `media` are shared. A target may override `text`, `media`, or both; platform `options` belong inside that target. Omitting an override inherits the shared value. Supplying `"text":""` or `"media":[]` explicitly replaces it.

There must be one to three targets, with one account per platform and no duplicate account. Every selected account must belong to the JWT owner and be active. Every target must have some text or media; TikTok and Instagram require their supported media.

Each media object contains `url`, `mime_type`, positive `size_bytes`, and optional `duration_seconds`, `width`, `height`, and `alt_text`. Supply real dimensions and duration for video; TikTok requires them. Alt text is for supported images, not this video example. Media URLs must remain fetchable through the scheduled time and provider processing. See [media hosting](deployment.md#media-hosting-and-optional-cloudflare-r2).

| Platform | Current adapter options and envelope |
| --- | --- |
| TikTok video | Required `privacy_level`, true `consent`/`preview_confirmed`, and explicit `allow_comment`, `allow_duet`, `allow_stitch`, `brand_content_toggle`, `brand_organic_toggle`. Optional `is_aigc` and `video_cover_timestamp_ms`. One supported video; duration within current creator limit. |
| TikTok photos | One to 35 JPEG/WebP images; requires privacy, preview/consent, comments, and commercial choices. Optional photo `title`, `photo_cover_index`, and `auto_add_music`; omit video-only duet/stitch/cover fields. |
| X | Omit options or send `{}`. Original text/media and explicit same-account replies using target `reply_to_post_id`. Local text validation uses a conservative 280-character weight estimate, not a complete `twitter-text` implementation. Up to four photos, one GIF, or one video; account and global limits both apply. |
| Instagram | Optional `media_type` matching `IMAGE`, `REELS`, or `CAROUSEL`; optional `share_to_feed` only for a single Reel. One JPEG, one video Reel, or two to ten carousel items. See the [adapter guide](instagram-adapter.md) for local media checks. |

The shared request limits are 32,000 bytes of text and at most 35 media items before platform validation, and 16 KiB of target options. Platform text/media limits are usually stricter. The API does not accept arbitrary upstream parameters through `options`.

## Scheduling and idempotency

For an owned X thread, set `reply_to_post_id` on the destination target to a confirmed local publisher post ID. The server resolves the remote ID from a safe same-owner/account/identity receipt; raw upstream reply fields are rejected. Optional target `publish_before` is an RFC3339 deadline, within 24 hours of admission and after `publish_at`. It persists into the worker and prevents delayed test publication after the approved window.

`GET /v1/post-receipts/{key}?request_hash={sha256}` performs an owner-scoped, read-only lookup of an existing admission. The digest is the lower-case hexadecimal SHA256 of Go's canonical `CreatePostRequest` JSON encoding, including normalized timestamps and target fields. This lookup never queues or retries delivery. The SPAWN lifecycle client persists that exact body/key/digest before its single POST; a lost acknowledgement is recovered through reads only. Missing receipts or unknown sends never authorize a replacement post.

To schedule the same content, add these top-level fields to the request, choosing an instant still in the future and within one year when you send it:

```json
{
  "publish_at": "2026-11-15T14:00:00-05:00",
  "timezone": "America/New_York"
}
```

`publish_at` must contain an explicit RFC3339 UTC marker or numeric offset. The service stores the instant in UTC. `timezone` is optional IANA display metadata; it does not reinterpret the timestamp or resolve an ambiguous local time. Your composer must resolve daylight-saving ambiguity before creating the RFC3339 instant. Omit `publish_at` to publish now rather than sending a timestamp that may already be past by arrival.

Every create request requires exactly one `Idempotency-Key` containing 1–128 letters, digits, dots, colons, underscores, or hyphens. The key is scoped to the authenticated tenant/user:

| Request | Result |
| --- | --- |
| New key and valid content | 201 with the newly queued post |
| Same key and same original request content | 200 with the existing post and its current status |
| Same key but changed content, schedule, targets, or options | 409 `idempotency_conflict` |

Persist the original request body and key in your application. Retry an uncertain API submission with those same values; do not replace the schedule with a newly calculated timestamp. Replaying an existing accepted request does not create another publication. Changing content is a new intent and requires a new key, with cancellation of the old eligible work if appropriate.

## Observe delivery and partial success

```sh
curl --fail-with-body "${API_BASE}/v1/posts/post_REPLACE" \
  -H "Authorization: Bearer ${APP_TOKEN}"

curl --fail-with-body "${API_BASE}/v1/posts/post_REPLACE/events" \
  -H "Authorization: Bearer ${APP_TOKEN}"
```

The post response contains `id`, aggregate `status`, `publish_at`, optional timezone, optimistic `version`, `created_at`, and `targets`. Each target has its own IDs, content, status, attempts/failures, next attempt time, optional receipt, last error, and warnings. Internal leases, unsafe checkpoints, and provider credentials are not exposed.

| Target status | Meaning |
| --- | --- |
| `queued` | Waiting until due or ready to be claimed |
| `running` | A worker currently holds the target |
| `processing` | Continuing a known provider operation or safe provider-directed wait |
| `retrying` | Waiting for a bounded automatic retry after a safe failure |
| `published` | Completed with a durable provider receipt |
| `failed` | Delivery failed; only a safe failure with no known operation is eligible for the explicit retry action |
| `needs_attention` | Automatic continuation is blocked; inspect the account/result and use the appropriate recovery action |
| `cancelled` | Eligible pending delivery was stopped |

Aggregate statuses include `queued`, `publishing`, `partially_published`, `published`, `failed`, `cancelled`, and `needs_attention`. Always display the target results, because an aggregate alone cannot describe mixed outcomes. A `needs_attention` aggregate can still contain a successfully published target.

Optional target timestamps distinguish `started_at`, the first-ever worker claim retained for history, from `processing_started_at`, the current known provider operation's processing window. The worker stops automatic continuation after that window exceeds 24 hours. An explicit safe retry starts a fresh window when it creates new progress; `resume_processing` renews the window while retaining the known operation's IDs. Existing operations retain their age during the schema upgrade.

For example, if X has a receipt while Instagram is still processing and TikTok failed, keep the X receipt and show the remaining destinations separately. Retrying TikTok must not resubmit X. A published target can include warnings, such as Instagram completing without its caption; a warning is not a reason to publish another copy. `remote_url` is optional, and the Instagram adapter currently does not invent one from a numeric media ID.

## Cancel or reschedule

Cancel only eligible unsubmitted work:

```sh
curl --fail-with-body "${API_BASE}/v1/posts/post_REPLACE/cancel" \
  -H "Authorization: Bearer ${APP_TOKEN}" \
  -H 'Content-Type: application/json' --data '{}'
```

Cancellation does not delete published content or guarantee that an in-flight or known provider operation can be stopped. Inspect the returned targets instead of assuming every destination became cancelled.

To reschedule, first GET the post and send its current `version` with a future time:

```sh
curl --fail-with-body "${API_BASE}/v1/posts/post_REPLACE/reschedule" \
  -H "Authorization: Bearer ${APP_TOKEN}" \
  -H 'Content-Type: application/json' \
  --data '{"publish_at":"2026-11-16T15:00:00-05:00","timezone":"America/New_York","version":1}'
```

Replace the time and version with current values. Rescheduling is allowed only before delivery has started; an outdated version or started target returns 409. There is no content-edit endpoint for an existing post.

## Recovery without duplicate publication

A provider may have accepted a post even when its response was lost. The service records an unsafe checkpoint before a publication mutation and does not automatically repeat an ambiguous operation. Known asynchronous operations continue from their saved IDs instead of starting a new upload/publish sequence.

Use the action that matches the evidence:

| Situation | Action |
| --- | --- |
| Safe `failed` target, active account, no existing provider operation | POST the target's `/retry` endpoint with `{}` |
| `needs_attention` after reconnect, with a safe known operation/checkpoint | Resolve with `resume_processing` to continue that operation |
| `needs_attention` and verified already published | Resolve with `confirm_published` and the real published ID; optional HTTPS permalink |
| `needs_attention` and verified definitely not published | Resolve with `confirm_not_published`; then make a separate explicit retry if desired |

Example safe retry:

```sh
curl --fail-with-body "${API_BASE}/v1/posts/post_REPLACE/targets/target_REPLACE/retry" \
  -H "Authorization: Bearer ${APP_TOKEN}" \
  -H 'Content-Type: application/json' --data '{}'
```

For `/resolve`, send one of these JSON bodies to the same target path ending in `/resolve`:

```json
{
  "action": "resume_processing",
  "note": "The owner reconnected the account; resume checking the existing provider operation."
}
```

```json
{
  "action": "confirm_published",
  "remote_id": "REPLACE_WITH_VERIFIED_PUBLISHED_ID",
  "remote_url": "https://www.instagram.com/p/REPLACE_WITH_VERIFIED_SHORTCODE/",
  "note": "The owner verified this exact published item in the connected account."
}
```

```json
{
  "action": "confirm_not_published",
  "note": "The owner verified the operation failed and will not become a published item."
}
```

Every resolution requires an explanatory note of at most 1000 UTF-8 bytes, with at least 5 bytes after trimming surrounding whitespace. `confirm_published` needs a published item ID, not an upload/container ID. The optional receipt URL must be HTTPS on an accepted TikTok, Instagram, X, or Twitter hostname. Omit receipt fields for the other actions.

`resume_processing` preserves saved provider IDs, renews the 24-hour processing window, and resumes the known safe operation; it cannot clear an ambiguous publish checkpoint or authorize a fresh publication. `confirm_not_published` clears the uncertainty and processing window and moves the target to `failed`; it does not publish as a side effect. The separate retry makes renewed intent explicit. Mere temporary absence from a feed is not proof that an operation failed, because provider processing can still complete. Resolution actions and notes are recorded in the event history.

## Disconnect and reconnect

```sh
curl --fail-with-body -X DELETE "${API_BASE}/v1/accounts/acct_REPLACE" \
  -H "Authorization: Bearer ${APP_TOKEN}"
```

The response reports `status: "disconnected"` and `provider_revocation`, which can be `revoked`, `unconfirmed`, or `not_attempted`. Local credentials are removed and eligible pending deliveries stop even when remote revocation is unconfirmed. Already submitted operations may still complete. The current Instagram adapter does not claim a verified remote revocation endpoint; complete grant removal in the provider account when needed.

Refresh and disconnect are serialized so late credential writes cannot reactivate a disconnected connection. Ambiguous refresh responses can quarantine the connection as `reauth_required`; reconnect through a new browser OAuth flow. Review existing delivery targets afterward. Reconnection is not a request to duplicate any prior post.

## Errors

Errors use this shape:

```json
{"error":{"code":"state_conflict","message":"The resource changed or this action is unavailable in its current state."}}
```

| Status | Typical meaning |
| --- | --- |
| 400 | Invalid JSON, content, schedule, idempotency key, or OAuth binding |
| 401 | Missing, invalid, or expired application-user JWT |
| 404 | Resource missing or outside this tenant/user's scope |
| 409 | Idempotency conflict, stale version, unsafe action, unavailable connection, or reauthorization required |
| 422 | Provider validation or a definite non-retryable rejection |
| 503 | Database readiness failure or temporarily unavailable/retryable provider operation |
| 504 | Request deadline exceeded; inspect the existing resource before retrying |

Use the controlled `error.code` and current account/target state in your UI. A transport failure or 5xx is not evidence that a remote publication did not happen. For create-request uncertainty, replay the same idempotency key/body; for delivery uncertainty, use the recorded target's recovery flow.
