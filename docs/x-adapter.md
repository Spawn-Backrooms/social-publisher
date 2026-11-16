# X adapter

The `internal/provider/x` package implements `provider.Adapter` using X API v2 and OAuth 2.0 user context. It publishes original text, image sets, animated GIFs, video, and explicit same-account thread replies. Scheduling belongs to the service's durable worker: X receives the create-post request when the target is due. Quote posts, polls, advertisements and arbitrary upstream options remain unsupported.

Thread requests supply `reply_to_post_id` on the X target, referring to an existing local publisher post. The API requires a published target owned by the same tenant, subject and account, with a safe X receipt binding the original content hash, remote ID and provider identity. The worker rechecks this receipt under the account lock; the adapter checks the posting identity and pins reply content in its checkpoint. The API does not accept raw remote reply IDs. It emits `reply.in_reply_to_tweet_id` only from the resolved receipt.

Optional target `publish_before` closes a bounded publication window (after the scheduled time and within 24 hours of admission). The worker refuses expired queued work before provider calls; the X adapter also checks immediately before public creation. Existing confirmed receipts can still be read after expiry. Unknown sends remain quarantined with no retry. These source checks do not establish account entitlement: [X's current documentation](https://docs.x.com/x-api/posts/manage-tweets/introduction) restricts self-serve replies. A definitive reply rejection does not create a standalone fallback.

The implementation was checked against current official X documentation on **October 5, 2026**. Local HTTP fixtures verify request contracts and recovery behavior; no live X account was connected and no real post was created during implementation.

## Configuration and account connection

Construct the adapter with `x.New(provider.Config{...})`. Required production values are `ClientID`, `ClientSecret`, and `RedirectURI`. `HTTPClient` and the shared secure `Media` fetcher can be supplied by the service.

| Configuration | Default / interpretation |
|---|---|
| `APIBase` | `https://api.x.com`; base for `/2/...` API paths |
| `AuthBase` | `https://x.com/i/oauth2/authorize`; a complete authorization URL |
| `HTTPClient` | A client with a 45-second request timeout when none is supplied, or when the supplied client has no timeout |
| `Media` | Required to upload an attachment; the adapter only downloads source bytes through `Media.Open` |

Both base overrides support local HTTP fixtures. Production configuration must retain the intended X endpoints. Credential-bearing API redirects are disabled even when a custom HTTP client is supplied. Upstream response bodies are limited to 1 MiB; provider body text is not copied into errors or logs because it can contain credentials, source signatures, or submitted content.

The app must be configured as a confidential Web App with OAuth 2.0 enabled and the exact callback URL registered. Requested scopes are:

```text
tweet.read tweet.write users.read offline.access media.write
```

`AuthorizationURL` sets `response_type=code`, `code_challenge_method=S256`, the supplied state/challenge, and the scopes. The service owns generation, storage, single use, expiry, and browser binding of state and the PKCE verifier. The adapter does not generate an independent verifier or bypass those protections.

Code exchange and refresh use `POST /2/oauth2/token`, form encoding, and HTTP Basic authentication using the client ID and secret. The exchange includes `grant_type=authorization_code`, `code`, `redirect_uri`, and `code_verifier`; refresh includes `grant_type=refresh_token` and `refresh_token`. Token expiry follows `expires_in`. A replacement refresh token is retained when returned; an omitted refresh token retains the previous value. An explicitly empty grant or missing required exchange scope is rejected.

`Identity` calls `GET /2/users/me`. `Revoke` attempts to revoke both refresh and access credentials with `POST /2/oauth2/revoke`, rather than assuming revocation cascades. The core service must persist refresh results under its account lock and token-version fence. A publishing 401 is returned with `AuthExpired` after clearing the unsafe checkpoint; publication never performs an unpersisted internal refresh.

A token grant can be consumed even when its response is lost. Refresh and code exchange therefore classify lost connections, 5xx/408 responses, and incomplete successful token responses as `Ambiguous`, without automatic retry. The account manager must quarantine an uncertain refresh and require reconnection, since the stored refresh token may already have rotated. A definite 429 rejection can be retried after its reset; `invalid_grant` requires reconnection.

Sources: [Authorization and PKCE](https://docs.x.com/fundamentals/authentication/oauth-2-0/authorization-code), [token, refresh, and revoke requests](https://docs.x.com/fundamentals/authentication/oauth-2-0/user-access-token), [authentication mapping](https://docs.x.com/fundamentals/authentication/guides/v2-authentication-mapping).

## Capabilities and validation

`Capabilities` reads the authenticated user's verification fields. Recognized verification enables the documented 125-minute / 16-GiB video bounds; standard or unknown status uses 20 minutes / 8 GiB. The response's raw metadata describes image/GIF byte limits, the known media scope, and the conservative text validator.

This is a capability estimate, not an account-entitlement probe. Premium membership that is not apparent in returned verification fields can be treated conservatively. `CanPublish` reflects known token scopes and a readable account; it does not certify developer credits, account standing, every user posting limit, or that a particular video will be accepted. The worker must obtain capabilities and call `Validate` again at dispatch. X checks actual permissions and media entitlements when the post is created.

| Supported content | Adapter validation |
|---|---|
| Text | Nonempty text unless media is present; conservative weight at most 280 |
| Images | Up to four JPEG, PNG, or WEBP images, each at most 5,000,000 bytes |
| GIF | One GIF, at most 15,000,000 bytes; all GIF input is conservatively treated as animated |
| Video | One MP4, WEBM, MPEG-TS, or QuickTime file; at least 0.5 seconds; account duration/size bounds apply |
| Image/GIF descriptions | Valid UTF-8, at most 1,000 Unicode code points; uploaded through the v2 metadata endpoint |
| Options | Empty object or null only; unsupported options are rejected |

Every attachment requires a positive declared `size_bytes` and a persistent HTTPS source. Videos also require a declared `duration_seconds`. Source byte length is checked during upload. The adapter does not decode/transcode the file, measure its actual duration, inspect codecs, or verify declared dimensions; the asset pipeline must supply correct metadata and compatible files.

Text validation counts ASCII code points as one and other Unicode code points as two. Recognized short URL candidates consume at least 23; long URLs receive no shortening discount. URL detection is an approximation, and no full NFC normalization, emoji-sequence rules, or complete TLD database is implemented. Consequently some X-valid text is rejected, and passing this estimate is **not** a guarantee of passing X's exact rules. Larger Premium text posts are not exposed. Video alt text is rejected; use the post text to describe a video.

Sources: [counting characters](https://docs.x.com/fundamentals/counting-characters), [media size and entitlement rules](https://docs.x.com/x-api/media/introduction), [create-post contract](https://docs.x.com/x-api/posts/create-post), [media metadata](https://docs.x.com/x-api/media/create-media-metadata), [OpenAPI schema](https://docs.x.com/openapi.json).

## Resumable media flow

Images, GIFs, and video use the same current v2 chunked sequence:

1. `POST /2/media/upload/initialize` with JSON `media_type`, `total_bytes`, and `media_category`.
2. Save the returned `data.id` and expiry before sending any bytes.
3. `POST /2/media/upload/{id}/append`, using multipart fields `segment_index` and raw `media`. Segments are 4 MiB, beginning at zero.
4. Save each acknowledged segment's byte position and the SHA-256 digest of its complete acknowledged prefix.
5. Save the `finalizing` stage before `POST /2/media/upload/{id}/finalize`.
6. If `processing_info` exists, persist `next_poll_at` and return an outcome with `PollAfter`. Poll `GET /2/media/upload?command=STATUS&media_id=...` until `succeeded` or `failed`.
7. Apply supplied image/GIF alt text through `POST /2/media/metadata` with `id` and `metadata.alt_text.text`, and save that acknowledgement before posting.

The previous `command=INIT|APPEND|FINALIZE` upload protocol is not used. Absence of `processing_info` in a confirmed finalize response means the media is ready. Unknown/malformed processing states do not result in publication. The provider's polling delay is used, with a one-second fallback and a one-day upper bound for an implausible response.

Media HTTP 200 responses are inspected for `errors[]` before acknowledging a chunk, confirming metadata, or advancing readiness. An error array remains a failure even alongside `data`; empty or malformed success responses do not advance the upload. An initialization ID received in an unconfirmed response is saved separately, but no chunks are sent to that session; a subsequent safe attempt starts a fresh upload.

Checkpoints are versioned, bound to a hash of the target content and the connected account identity, and contain no access tokens or source URLs. A resumed append reopens the source, rereads the acknowledged prefix in bounded memory, and verifies its digest before continuing. An object replaced at the same URL cannot silently produce a mixture of old and new chunks. Because the shared fetcher does not expose ranges, resuming a large upload consumes bandwidth to reread that prefix.

Memory remains bounded: one 4-MiB source chunk, its multipart request buffer, the bounded response, and small hashing/transport buffers. Cancellation closes an active source reader and stops further upload requests. The adapter never adds the X bearer token to a source download.

Returned expiry values are retained. Uploads with less than one minute remaining are refreshed before use; sessions expiring mid-upload are reset for a safe new upload. A media-status 404/410 also starts a new upload. A crash after the `finalizing` checkpoint resumes by checking status; an explicit status rejection indicating finalization was not started permits another finalize. A stored processing failure stops publication.

Source: [current chunked media protocol](https://docs.x.com/x-api/media/quickstart/media-upload-chunked), [initialize](https://docs.x.com/x-api/media/initialize-media-upload), [append](https://docs.x.com/x-api/media/append-media-upload), [finalize](https://docs.x.com/x-api/media/finalize-media-upload), [status](https://docs.x.com/x-api/media/get-media-upload-status).

## Publication safety and failure handling

Once all media is ready, `POST /2/tweets` receives `text` and, when present, `media.media_ids`. A media-only post explicitly sends an empty text string. Text-only posts omit the media field. The remote ID and receipt URL are returned to the worker.

The durable checkpoint with `Unsafe=true` must succeed **before** the create request. The adapter refuses to repeat a resumed unsafe operation without a saved post ID. Its safety classification is:

| Result | Handling |
|---|---|
| A valid returned `data.id` | Published, even if a status or a later body-read error is inconsistent; preserve the known receipt |
| 400–499 except 408 | Definite rejection; clear the unsafe checkpoint before returning the error |
| 401 | Authentication expired; the core coordinates token refresh/reconnection |
| 429 | Safe retry after `Retry-After` / `x-rate-limit-reset`; use the later valid delay |
| Lost connection, timeout, 5xx, redirect, malformed success, or success without an ID | Uncertain publication; no automatic second create request |
| Failure to save a definite rejection | Keep uncertainty until an operator reconciles the outcome |
| Failure to save the final known receipt | Return successful receipt plus a warning; the core's fenced completion can still save it |

Before accepting any saved receipt, the adapter checks its checkpoint version, content hash, media count, and exact nonempty account identity binding. A receipt from another account or different content is rejected without a network request. A correctly bound post ID then takes precedence over stale unsafe state, token expiry, and subsequent validation. If a worker loses its lease after publication, the shared store's fence decides whether it can save completion. An unsafe record without a receipt must remain an operator reconciliation case. The adapter does not claim an exactly-once API guarantee or invent an upstream idempotency key.

Errors during nonpublic media preparation can be retried when transient; processing/validation failures require correction. Detailed upstream bodies are deliberately not exposed. Generic X documentation recommends retrying 5xx responses, but a create-post retry after an ambiguous result can duplicate content, so the stricter policy above applies.

Sources: [create-post response](https://docs.x.com/x-api/posts/create-post), [X response codes](https://docs.x.com/x-api/fundamentals/response-codes-and-errors).

## Deployment dependencies and verification

The operator still needs an approved X app, a configured production callback, appropriate API enrollment, and sufficient usage credits. Each customer authorizes their own account. Current pricing is prepaid pay-per-use: the public page lists $0.015 per ordinary created post and $0.200 for a post with a URL. Upload pricing is not assumed free. Confirm actual charges and entitlement in the Developer Console before enabling live dispatch.

The dedicated rate table lists 100 create requests per user per 15 minutes and 10,000 per app per day. Another integration page retains different historical values. The adapter therefore honors response headers and does not treat a fixed local number as guaranteed capacity. Account-wide posting restrictions, permissions, and credit exhaustion may bind before endpoint capacity.

Sources: [current pricing](https://docs.x.com/x-api/getting-started/pricing), [rate-limit reference](https://docs.x.com/x-api/fundamentals/rate-limits), [integration guide with conflicting limits](https://docs.x.com/x-api/posts/manage-tweets/integrate).

Verified locally with:

```sh
go test -race ./internal/provider/x
go vet ./internal/provider/x
```

Fixtures cover confidential-client OAuth forms, PKCE URL/scopes, rotated and omitted refresh tokens, consumed-but-unacknowledged token grants, explicit missing permissions, revocation, blocked redirects, validation, exact post bodies, media chunk boundaries and restart positions, prefix changes, expiry, alt-text persistence, asynchronous polling, finalization crashes, source cancellation, HTTP 200 media error envelopes, malformed media successes, unsafe checkpoint ordering, rate-limited/authentication rejections, failed checkpoint writes, lost create connections, receipt/account/content binding, and retention of known post IDs.

Live OAuth consent, API enrollment, real media transcoding, actual account allowances, production token revocation, and a controlled real-account publication remain deployment checks. No fixture result proves those external approvals or end-to-end availability.
