# Instagram adapter

The adapter in `internal/provider/instagram` implements Instagram Login account connection and URL-based publishing for professional accounts. Its entry point is `instagram.New(provider.Config)`, which implements `provider.Adapter`.

## Supported content and options

Supported publications are a single JPEG image, a standalone video published as a Reel, and a carousel containing two to ten images and/or videos. A carousel has one caption on its parent container. The adapter rejects Stories, personal/consumer account types, non-JPEG images, unsupported video MIME types, and unknown options.

Only these Instagram options are accepted:

```json
{
  "media_type": "REELS",
  "share_to_feed": true
}
```

Both keys are optional. `media_type` must be `IMAGE`, `REELS`, or `CAROUSEL` and agree with the media supplied. When absent, the adapter infers it. `share_to_feed` applies only to a single Reel and defaults to `true`. Duplicate option names, incorrect types, and unknown names are rejected. Arbitrary Graph parameters and endpoint overrides cannot enter through content options.

Image accessibility text comes from `Media.AltText`, including carousel image children. Video alt text is rejected. The adapter does not expose a `cover_url` option because a source URL hidden inside arbitrary options would need the same origin validation as ordinary media.

The local metadata checks currently impose these bounds:

| Media | Local checks |
| --- | --- |
| Caption | Valid UTF-8; at most 2,200 Unicode code points |
| Image | `image/jpeg`; positive size, at most 8,000,000 bytes; when dimensions are supplied, aspect ratio 4:5 through 1.91:1 |
| Image alt text | Valid UTF-8; at most 1,000 Unicode code points |
| Reel | `video/mp4` or `video/quicktime`; 3–900 seconds; at most 1,000,000,000 bytes; supplied width at most 1,920 pixels |
| Carousel video | Same video MIME/size/width checks; 3–60 seconds |
| Carousel | Two through ten items |

These checks use supplied metadata. They do not inspect codecs, transcode files, crop images, or prove that metadata matches the hosted asset. The service's shared media guard must validate every media origin before accepting the post. The adapter additionally requires an absolute HTTPS URL without user credentials or a fragment. Meta must be able to fetch the media through processing; keep source URLs available for the scheduled time and subsequent processing.

## Configuration

| `provider.Config` field | Behavior |
| --- | --- |
| `ClientID` | Instagram product's App ID |
| `ClientSecret` | Instagram product's App Secret; server side only |
| `RedirectURI` | Exact registered production callback URI |
| `GraphVersion` | Defaults to `v26.0` |
| `APIBase` | Defaults to `https://graph.instagram.com`; a server-controlled fixture override is allowed |
| `AuthBase` | Empty in production. A nonempty fixture base replaces both `https://www.instagram.com` for authorization and `https://api.instagram.com` for the code exchange |
| `HTTPClient` | Optional injected client/transport; copied by the constructor. A zero timeout becomes 30 seconds; redirects are always disabled |

Production credential and endpoint validation belongs to the application's configuration layer. Request JSON cannot override these values. The adapter's endpoint fields are private. `Config.Media` is unused because this integration supplies URLs for Meta to retrieve rather than downloading and uploading asset bytes.

All upstream response bodies are bounded to 1 MiB. Errors expose controlled classifications and numeric HTTP/Graph codes. They never include the upstream message, response body, request URL, OAuth code, access token, client secret, or signed source URL. Redirects cannot forward those values to another origin.

## Account connection and tokens

The requested scopes are `instagram_business_basic` and `instagram_business_content_publish`. This is the direct Instagram Login path for Business/Creator accounts; it does not require a Facebook Page or a Page access token. The application's shared OAuth layer owns state generation, browser binding, expiry, and replay prevention. This adapter does not add an unverified Instagram PKCE parameter.

| Step | Request |
| --- | --- |
| Authorize | `GET https://www.instagram.com/oauth/authorize` with `client_id`, `redirect_uri`, `response_type=code`, comma-separated `scope`, and `state` |
| Exchange code | Form `POST https://api.instagram.com/oauth/access_token` with `client_id`, `client_secret`, `redirect_uri`, `grant_type=authorization_code`, and `code` |
| Extend token | `GET https://graph.instagram.com/access_token` with `grant_type=ig_exchange_token`, `client_secret`, and the short-lived `access_token` |
| Refresh token | `GET https://graph.instagram.com/refresh_access_token` with `grant_type=ig_refresh_token` and the current long-lived `access_token` |
| Identify account | `GET /v26.0/me?fields=user_id,username,account_type,name` with bearer authorization |

The code exchange must succeed through the long-lived exchange before the adapter returns a usable connection token. It accepts a flat code-exchange token record or one unambiguous `data` entry. Multiple records, simultaneous flat/wrapped tokens, duplicate object keys, missing credentials/IDs, malformed IDs, and invalid expiry values fail without yielding a partially usable token. Numeric IDs are decoded directly to decimal strings, preserving values above JavaScript's safe integer range. `user_id` takes precedence over a returned `id` in the identity response.

The long-lived token's timestamps come from the response's positive `expires_in` and local receipt time. The expected provider lifetime is approximately 60 days, but the adapter stores the returned duration rather than substituting a constant. No separate `refresh_token` is invented.

Refresh requires a known `Token.IssuedAt`, a nonexpired token, and an age of at least 24 hours. A younger valid token is returned unchanged without making a refresh request. Missing/future issuance times fail locally; expired tokens require reconnection. Successful refresh preserves provider account identity and granted scopes, replaces the access token if rotated, and updates issuance/expiry. A malformed refresh never returns a replacement credential.

## Capabilities and publishing quota

`Capabilities` fetches the authenticated identity and:

```text
GET /v26.0/{ig-user-id}/content_publishing_limit?fields=quota_usage,config
```

It returns only selected, safe fields in `Capabilities.Raw`:

```json
{
  "publishing_quota": {
    "usage": 3,
    "total": 7,
    "duration_seconds": 86400
  },
  "supported_media": ["image", "reel", "carousel"],
  "max_carousel_items": 10,
  "stories_supported": false
}
```

The numbers above are illustrative. The adapter reads `config.quota_total` and `config.quota_duration` on every quota check; neither a 50-post nor a 100-post allowance is hardcoded. A full rolling quota does not make `CanPublish` false: a customer can still schedule a future post.

The publisher checks capacity before starting a new set of containers and again immediately before the final publication. Known quota exhaustion produces a safe waiting outcome, normally for 15 minutes, rather than consuming the worker's bounded failure retries. With no containers created, progress remains empty. A malformed/unavailable quota response blocks publication. A provider-side rejection at `media_publish`, including a rate-limit response, follows the error/checkpoint rules below.

## Publication and durable recovery

The adapter creates media with `POST /v26.0/{ig-user-id}/media`. Images use `image_url` with no `media_type`. Reels use `video_url` and `media_type=REELS`. Carousel children use `is_carousel_item=true`; video children use `media_type=VIDEO`. Once every child is ready, the parent uses `media_type=CAROUSEL` and its ordered child IDs.

Each returned container ID is durably checkpointed before the next dependent network step. A checkpoint also binds progress to the account ID and a hash of the complete target content. A changed account or content, unknown stage, malformed progress, or inconsistent IDs cannot silently restart a fresh publication.

Readiness is checked with:

```text
GET /v26.0/{container-id}?fields=status_code,status
```

| Status | Adapter behavior |
| --- | --- |
| `IN_PROGRESS` | Return a one-minute polling delay with existing IDs |
| `FINISHED` | Eligible for the next dependent step; a final container still needs the live quota check |
| `ERROR` | Controlled processing error; do not publish |
| `EXPIRED` | If no publication was attempted, clear expired state durably and renew the container set; at most two renewals |
| `PUBLISHED` without a stored final media ID | Publication may have occurred; require reconciliation instead of inventing a receipt |
| Other/missing status | Controlled error; do not publish |

The worker's overall processing deadline may stop an old target before renewal. The adapter itself never recreates media from an unsafe checkpoint.

Before `POST /v26.0/{ig-user-id}/media_publish` with `creation_id`, it durably records `Progress.Unsafe=true`. If that write fails, no publication request is sent. A successful response must include a usable published-media ID; that ID and the final safe state are checkpointed before returning success. If saving the receipt fails, the adapter reports an uncertain publication.

Unsafe progress is never automatically retried or treated as failed merely because a container still says `FINISHED`. A transport failure, lost response, malformed success, success without an ID, contradictory receipt/error, redirect, or HTTP 5xx after sending `media_publish` remains ambiguous. The adapter emits one publication attempt and leaves the unsafe marker intact. The application's reconciliation flow must resolve it.

A definite provider rejection can clear the unsafe marker, but that clearing checkpoint must itself succeed before a safe retryable error is returned. Failure to clear it remains ambiguous. Retry metadata respects a bounded `Retry-After` header. Creation failures may be retried because creating a container cannot publish; a response lost during creation can leave an unused container, which is never guessed or published.

Successful publication returns the final media ID. The adapter does not invent a permalink from that numeric ID. It currently leaves `RemoteURL` empty.

## Partial publication warnings

`config_issue` does not change a successful published ID into a failure. Known values produce controlled warnings:

- `CAPTION_NOT_ATTACHED`: publication succeeded without its caption.
- `USER_TAGGING_FAILURE`: publication succeeded without its user tags.
- Another nonempty value: publication succeeded with an unspecified configuration issue to inspect.

Warnings never forward arbitrary upstream text. They are persisted with the successful receipt and returned through `model.Outcome.Warnings`. A missing caption or user tag never triggers a duplicate publication attempt.

## Disconnect and unsupported revocation

`Revoke` performs no HTTP request and returns `provider.Error` with `Code="revoke_unsupported"`. A current direct Instagram Login revocation endpoint was not verified from a primary source during this implementation. The adapter therefore does not invent a `DELETE /permissions` contract or report provider revocation as completed.

The shared application can disable the connection, erase local credentials, and cancel pending work independently. Provider deauthorization callbacks and data-deletion callbacks are separate application responsibilities. Disconnect must not be described as successful upstream grant revocation when this method returns `revoke_unsupported`.

## Verification evidence and release gates

The implementation was researched on 2026-10-05. Accessible primary evidence included:

- [Meta's Instagram Login Postman folder](https://www.postman.com/meta/instagram/folder/6raa77c/instagram-api-with-instagram-login): professional account types, no linked Facebook Page requirement, and current permission names.
- [Meta SDK API configuration](https://github.com/facebook/facebook-python-business-sdk/blob/main/facebook_business/apiconfig.py): API version `v26.0`.
- [Meta SDK IGUser implementation](https://github.com/facebook/facebook-python-business-sdk/blob/main/facebook_business/adobjects/iguser.py): media creation/publishing/quota signatures and publishing fields. This SDK also covers Facebook Login; the complete SDK field set is not evidence of Instagram Login feature parity.
- [Meta's Reel publishing Postman example](https://www.postman.com/meta/instagram/documentation/6yqw8pt/instagram-api?entity=request-23987686-af579d08-121e-4897-8f45-5fd41ace49df): container creation, readiness polling, and final publication.
- [Meta's Reels sample repository](https://github.com/fbsamples/reels_publishing_apis/blob/main/insta_reels_publishing_api_sample/README.md): carousel flow and media requirements. Parts of this sample's account/limit prose are old; it is not used to hardcode a publishing quota.

Meta's current Business Login, access-token, refresh, media-reference, permissions, changelog, and review pages repeatedly returned retrieval errors, HTTP 429, or a login wall. Accordingly, the full current OAuth response contract, optional identity fields, token refresh timing, all media limits/status details, direct-login carousel-video behavior, and the recent `config_issue` change still require a real authorized account check. This implementation uses the established contract and tests its behavior against local fixtures; it does not represent a successful live Meta integration or app approval.

Canonical references to recheck are [Business Login](https://developers.facebook.com/docs/instagram-platform/instagram-api-with-instagram-login/business-login), [token exchange](https://developers.facebook.com/docs/instagram-platform/reference/access_token), [refresh](https://developers.facebook.com/docs/instagram-platform/reference/refresh_access_token), [publishing](https://developers.facebook.com/docs/instagram-platform/content-publishing), [media reference](https://developers.facebook.com/docs/instagram-platform/instagram-graph-api/reference/ig-user/media/), [quota reference](https://developers.facebook.com/docs/instagram-platform/instagram-graph-api/reference/ig-user/content_publishing_limit), and [Instagram changelog](https://developers.facebook.com/docs/instagram-platform/changelog/).

Before admitting outside customer accounts, confirm the exact production callback registration, Instagram product credentials, applicable Advanced Access/App Review and Business Verification requirements, privacy policy and deletion/deauthorization configuration, reviewer access, and a working approved use case. Complete authorized connection/refresh/publication smoke tests before enabling actual customer publishing. No live social post or Meta app configuration was performed during implementation.

Local validation commands:

```sh
go test -race ./internal/provider/instagram
go vet ./internal/provider/instagram
```

The fixtures cover OAuth requests and response ambiguity, large IDs, token age boundaries and rotation, sanitized provider/transport errors, blocked redirects, image/reel/carousel payloads, durable ID order, child/parent recovery, checkpoint failures before publication and after success, lost/malformed publication responses, definite rejection versus uncertainty, quota checks on resumed work, unsupported options, readiness/expiry states, and safe warning propagation. These are offline contract and recovery tests, not Meta acceptance tests.
