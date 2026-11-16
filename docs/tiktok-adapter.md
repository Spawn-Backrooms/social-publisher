# TikTok adapter

The adapter implements TikTok Web Login Kit and Direct Post for one video or a set of photos, using official documentation reviewed on 2026-10-05. It implements every method of `provider.Adapter`. All automated tests use local HTTP fixtures; no test logs into TikTok or publishes real content.

## Integration boundary

Construct it with `tiktok.New(provider.Config{...})`. Configure the app's client ID/key, client secret, exact registered redirect URI, and whether TikTok has actually audited the Direct Post integration. `Audited` defaults to false. Setting this value does not obtain TikTok approval.

The production API base is `https://open.tiktokapis.com`. `APIBase` replaces that entire base for fixtures; `AuthBase` replaces the full authorization URL. Production deployment configuration must not expose arbitrary provider-origin overrides to API consumers.

The shared service owns user authentication, connection ownership, OAuth state and browser binding, encrypted tokens, serialization of refresh, queue leases, media-origin allowlisting, and durable checkpoints. The adapter does not accept a target account from the post body: the provided connection token selects the TikTok account.

## OAuth and account identity

The authorization URL is `https://www.tiktok.com/v2/auth/authorize/`. The adapter requests `user.info.basic,video.publish` and sets `disable_auto_auth=1` so TikTok displays authorization. This setting does not promise an account picker.

Code exchange and refresh use form-encoded `POST /v2/oauth/token/`. Revoke uses form-encoded `POST /v2/oauth/revoke/` with the access token in the `token` form field. The Web flow does not send the interface's PKCE challenge/verifier; TikTok's token documentation requires the verifier for mobile and desktop, not Web. The shared OAuth callback must decode its query normally and pass the resulting code directly to `Exchange`; do not decode it a second time.

Token responses are flat JSON. Actual scopes, access and refresh tokens, user ID, token type, and both returned expiry durations are persisted in the returned `model.Token`. No requested scope, token value, or guessed expiry is substituted for missing provider data. An incomplete successful response is uncertain because the authorization grant may already have been consumed. Refresh verifies that the returned user ID matches the original connection.

`Identity` reads `GET /v2/user/info/?fields=open_id,display_name`. It leaves `Username` empty because this request only asks for basic identity. `Capabilities.Raw` separately includes the creator-query username for the composer. The service should serialize refresh across instances and ensure disconnect wins over an in-flight refresh.

Official references: [Web Login Kit](https://developers.tiktok.com/docs/en/login-kit-web), [token management](https://developers.tiktok.com/docs/en/oauth-user-access-token-management), [OAuth errors](https://developers.tiktok.com/docs/en/oauth-error-handling), [scopes](https://developers.tiktok.com/docs/en/tiktok-api-scopes).

## Creator choices and JSON options

Fetch capabilities for the composer and show the creator's identity, current privacy choices, and interaction availability. `Publish` fetches them again immediately before a new initialization and validates the saved choices. If the account's choices changed, the post fails validation rather than silently selecting different settings.

For a video, `Content.Text` is its caption. This is a complete example of the target's `options` object for a creator who explicitly selected only-me visibility:

```json
{
  "privacy_level": "SELF_ONLY",
  "consent": true,
  "preview_confirmed": true,
  "allow_comment": false,
  "allow_duet": false,
  "allow_stitch": false,
  "brand_content_toggle": false,
  "brand_organic_toggle": false,
  "is_aigc": false,
  "video_cover_timestamp_ms": 0
}
```

For photos, `Content.Text` is the description:

```json
{
  "privacy_level": "SELF_ONLY",
  "consent": true,
  "preview_confirmed": true,
  "allow_comment": false,
  "brand_content_toggle": false,
  "brand_organic_toggle": false,
  "title": "A creator-approved photo title",
  "photo_cover_index": 0,
  "auto_add_music": false,
  "is_aigc": false
}
```

| Option | Local contract |
| --- | --- |
| `privacy_level` | Required. Must be one of the latest returned options; no default. |
| `consent` | Required and true. Records express consent for this exact content and the applicable TikTok declarations. |
| `preview_confirmed` | Required and true. The creator must have reviewed the media and editable caption. |
| `allow_comment` | Required explicit boolean, limited by creator settings. |
| `allow_duet`, `allow_stitch` | Required explicit booleans for video; prohibited for photos. |
| `brand_content_toggle` | Required explicit boolean for third-party branded content. Cannot combine true with `SELF_ONLY`. |
| `brand_organic_toggle` | Required explicit boolean for the creator's own brand/business. |
| `is_aigc` | Optional explicit AI label. Video requests nest it in `post_info`; photo requests put it at the top level. |
| `title` | Optional photo title, at most 90 UTF-16 code units. Prohibited for video. |
| `photo_cover_index` | Optional zero-based selected photo index; defaults to the first photo in the approved preview. |
| `auto_add_music` | Optional photo-only TikTok recommended-music setting; no music is selected by this service. |
| `video_cover_timestamp_ms` | Optional video-only timestamp within the video and within the API's signed 32-bit range. |

Unknown, duplicate, differently cased, or incorrectly typed option names/values are rejected. Omitted required booleans do not become defaults. JSON options are limited to 64 KiB.

Consent fields are assertions from the authenticated integrating application. A backend cannot prove that a preview was actually shown. The composer must implement TikTok's required declarations and interaction controls, allow edits to preset text, obtain consent before transmission, and show processing status. Use the current wording and policy links in the [Content Sharing Guidelines](https://developers.tiktok.com/docs/en/content-sharing-guidelines).

Unaudited clients require an actually private account and `SELF_ONLY`, with TikTok's limit of five posting users per 24 hours. All clients have an app-specific active-creator cap and a variable posting cap typically around 15 posts per creator per 24 hours across clients. Public service requires app approval and a Direct Post audit; TikTok excludes private/internal account-upload utilities. A developed website, policy pages, and an integration demonstration are additional [app-review requirements](https://developers.tiktok.com/docs/en/app-review-guidelines).

## Hosted-media policy

The adapter exclusively uses `PULL_FROM_URL`. TikTok requires this method for media already stored on an API client's servers. Before this adapter is called, the shared media policy must verify exact-host HTTPS allowlisting; the operator must also verify the corresponding domain or URL prefix in TikTok's app portal. Allowlisting an origin locally does not perform TikTok ownership verification.

URLs must not contain user credentials or fragments, must use a DNS host and HTTPS port 443, must not redirect, and must stay accessible throughout TikTok's download. Download tasks can run for up to an hour. Do not schedule a URL that will expire before delivery and processing finish.

The adapter checks declared metadata as follows:

| Media | Validation |
| --- | --- |
| Video | Exactly one MP4, QuickTime/MOV, or WebM; positive known size and duration; both dimensions 360–4096 pixels; duration within the current creator limit; at most 4,000,000,000 bytes. |
| Photos | 1–35 JPEG or WebP images; positive known size and dimensions; at most 20,000,000 bytes each. |
| Video caption | At most 2,200 UTF-16 code units. |
| Photo description | At most 4,000 UTF-16 code units. |
| Photo dimensions | Local conservative policy: longer side at most 1920 pixels, shorter side at most 1080. TikTok describes its limit as “maximum 1080p” without an orientation-specific pair; this local envelope does not promise TikTok acceptance. |

The decimal byte limits are conservative service limits. TikTok also restricts video encoding to H.264, H.265, VP8, or VP9 and frame rate to 23–60 FPS. `model.Media` contains no codec or frame-rate field, and the adapter does not download or transcode hosted files. The media pipeline must inspect actual encoding and must supply trustworthy metadata. Declared metadata alone cannot certify the original file.

The current Direct Post request schemas provide no custom alternate-text field. If shared media contains `alt_text`, the adapter adds an outcome warning and omits that unsupported field.

Official references: [media transfer](https://developers.tiktok.com/docs/en/content-posting-api-media-transfer-guide), [video Direct Post](https://developers.tiktok.com/docs/en/content-posting-api-reference-direct-post), [photo Direct Post](https://developers.tiktok.com/docs/en/content-posting-api-reference-photo-post).

## Durable dispatch and safe recovery

TikTok's current init schemas have no scheduled-publication field. The service's queue decides when to call the adapter. Dispatch time is not a guarantee of profile visibility because TikTok processes and moderates asynchronously.

The adapter performs one external phase per worker call:

1. For a fresh attempt, request creator settings, validate consent/content, and persist `Stage=tiktok_init, Unsafe=true`.
2. Send the applicable init request. A confirmed rejection clears the unsafe checkpoint before returning an authorization or retryable error.
3. For accepted initialization, persist `Stage=tiktok_poll, Unsafe=false` with `publish_id` before returning.
4. Later worker calls poll the saved operation ID. They do not reinitialize or change the approved post.

Timeouts, cancellation during initialization, HTTP 408/5xx, redirects, malformed success, or success without an operation ID leave initialization uncertain. An accepted operation whose ID cannot be saved is also uncertain. The adapter never automatically replays such a mutation. Restarting with `Unsafe=true` or invalid saved progress requires reconciliation.

Read-only creator/status requests can be retried. Explicit rate-limit rejections preserve a valid `Retry-After`. A terminal `FAILED` result ends the saved attempt, including retry-permitted processing failures; a new attempt must be submitted explicitly after its cause is reviewed. A failed poll does not discard the saved operation.

The shared worker must fence checkpoint writes with its lease and serialize account/token work. Persist the returned `Outcome.Progress` when recording an outcome. A missing checkpoint callback prevents fresh initialization.

Every nonempty checkpoint carries state version 1, the TikTok `token.ProviderUserID` as `owner_id`, and a SHA-256 `content_hash` of canonical `model.Content` JSON. Nested option-object ordering and whitespace are normalized without converting numbers through floating point. The intent, accepted operation, and completion receipt preserve this binding. Before resuming, the adapter checks the version, account, and content without making an HTTP request. Changing a caption, media URL/metadata, or publishing choice requires a separate attempt; access-token rotation for the same TikTok account does not invalidate progress. Missing bindings and unknown versions are rejected; this initial release has no legacy-state migration.

The documented endpoint limits are 6 init requests, 20 creator queries, and 30 status polls per minute per access token. The adapter proposes five seconds between polls for an individual operation and handles provider throttling. Service-level scheduling must consider all operations for the account together. There is no documented provider initialization idempotency key; local queue idempotency cannot supply provider-level exactly-once behavior.

## Receipts and status

Polling uses `POST /v2/post/publish/status/fetch/` and the saved `publish_id`. Processing states return an incomplete outcome; `PUBLISH_COMPLETE` returns completion. A terminal failure has a sanitized reason and does not become an automatic new post.

The status response's public-ID field is spelled `publicaly_available_post_id`. The adapter preserves integer and quoted-integer IDs as strings without a floating-point conversion.

- When public post IDs are available, `RemoteID` is the first public post ID.
- Without public IDs, `RemoteID` is the **publish operation ID**, and a warning explains the distinction. This occurs for private posts and can also occur before public moderation/visibility is confirmed.
- Worker-side `Progress.Data` retains the version/account/content binding, `publish_id`, and the complete returned public-ID list.
- `RemoteURL` is left empty because this API response does not supply a canonical share URL.

Completion does not mean public visibility. The absence of a public ID does not prove that the user selected private visibility. See [Get Post Status](https://developers.tiktok.com/docs/en/content-posting-api-reference-get-video-status).

This iteration uses durable polling and does not register or consume TikTok provider webhooks.

## HTTP behavior and verification

API calls have bounded response reads and a default 30-second timeout. Context cancellation is respected. Redirects and cookie jars are disabled even for an injected HTTP client. Upstream bodies, URLs, tokens, descriptions, and unknown error-code strings are never included in adapter error messages.

The package's fixture suite covers OAuth rotation and granted scopes, missing token metadata, canonical consent fields, current creator settings, media limits, payload nesting, mutation checkpoints, response ambiguity, cancellation, redirect handling, private receipts, terminal failures, and numeric-ID precision. Run:

```sh
go test -race -timeout 60s ./internal/provider/tiktok
go vet ./internal/provider/tiktok
```

Live app approval, verified hosting, and an authorized real-account smoke test remain deployment checks outside this fixture suite.
