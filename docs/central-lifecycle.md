# Central SPAWN lifecycle service

This source continues Core producer `d5b8450b`. It keeps native consent, Bank
receipts and settlement proof authoritative. Settlement does not imply private
output delivery. The older independent runtime producer is superseded.

The new `/v1/lifecycle/` routes require a distinct HS256 service key, issuer
`spawn-lifecycle-service`, audience `spawn-lifecycle-publisher`, tenant
`spawn-lifecycle-service` and subject `spawn-lifecycle-consumer`. Ordinary
requester JWTs cannot use them. Service tokens cannot use ordinary owner, OAuth,
retry or resolve routes. The server grants access only to its configured central
X account and stored custody scope; the service never signs as that owner.

The optional configuration is `LIFECYCLE_JWT_HMAC_KEY` plus the exact
`LIFECYCLE_ACCOUNT_ID`, `LIFECYCLE_PROVIDER_USER_ID`,
`LIFECYCLE_ACCOUNT_TENANT`, and `LIFECYCLE_ACCOUNT_OWNER`. No values or activation
are installed by this patch. The key must differ from the ordinary JWT key.
The controller forwards these only when explicitly configured. Release tooling
treats the service key as a secret and removes it from unrelated child processes.

`POST /v1/lifecycle/events` accepts only a reviewed versioned event envelope, a
bounded expiration in milliseconds and, for settlement, the original accepted
receipt's event ID and request hash. The server renders the public text itself:
canonical Backroom, opaque public contract reference, accepted or settled.
Dispatch authorization remains in-app. Brief, result, requester identifiers,
wallet and arbitrary text have no fields in this route. Native
`spawn.backroom-lifecycle.v1` carries no amounts. The existing native
`spawn.backroom-lifecycle.v2` carries the consented USD budget cap and settled
native inference spend; its fields and rendering remain unchanged.

`GET /v1/lifecycle/receipts/{event_id}?request_hash=...` resolves the original
semantic idempotency receipt without creating work. Its `confirmed` flag requires
the original X checkpoint, content digest, account/provider identity and remote
post ID to match. Admission, unsafe sends and manual resolutions do not establish
publication. Settlement replies resolve this same original accepted receipt;
callers cannot select an arbitrary upstream or local parent.

`POST /v1/lifecycle/receipts/{event_id}/cancel?request_hash=...` with `{}` cancels
only the original queued receipt through existing store rules. It never deletes
remote posts or retries uncertain sends. Publication expiration is checked by the
existing worker and X adapter before provider work. A lost acknowledgement is
reconciled only by receipt reads, never a second create request.

The paired runtime policy is default off, original-requester-only, one newly
opted-in contract per reviewed policy and at most 24 hours. No accepted-history
scan registers a contract. It emits at most two posts in one thread and maintains
all three causal Core milestones in the private UI. A confirmed Social receipt
alone unlocks an X link. Public-directory activity stays `not_published`; this
patch grants no public-directory writer.

Source tests use inert fixtures. Live X self-thread entitlement, joint source
integration, secure configuration, container resume and real posting remain
separate release gates owned by the release coordinator.

## External-work lifecycle, source-only contract v1

The separate `spawn.external-work-lifecycle.v1` public schema supports generic
external executors without a bot-name allowlist. It does not grant access to
native agents, inference billing, account custody, or the public-directory writer.
The same service JWT, exact central account grant, admission, expiration, receipt,
cancellation and uncertain-send protections apply. This source change installs
no credentials, origin, grants, runtime configuration or activation.

The envelope is the existing ordered JSON object:

1. `event`: `event_id`, `account_ref`, `consent_hash`, `evidence_hash`, `sequence`,
   `public`, in that order.
2. `expires_at_ms`: the existing bounded publication deadline in milliseconds.
3. `parent`, only for settlement: `event_id`, then `request_hash`.

The external `public` object has these fields in this exact order:

| Field | Meaning |
| --- | --- |
| `schema` | Exactly `spawn.external-work-lifecycle.v1` |
| `public_contract_ref` | Opaque `contract_` followed by 32 lowercase hex digits |
| `backroom` | Explicitly authorized public label, 1–48 ASCII characters; alphanumeric first/last character, interior alphanumerics, spaces, `_` or `-` |
| `stage` | `accepted` or `settled` |
| `currency` | Exactly `USDC` or `TEST_USD` |
| `quoted_amount_micro_units` | Exact quote `held_micro_units`, the total authorized quote; positive canonical integer string, at most 30 digits |
| `public_delivery_url` | Absent for acceptance; required canonical public-page URL for settlement |

The ASCII label is a dedicated publication-consent snapshot. The producer must
not silently transliterate a Unicode room name or publish a private room label.
Even a label matching a native seed is explicitly rendered as external work.
No native USD budget/spend fields, private brief, wallet, owner identifier,
arbitrary result text, media URL, or remote reply ID are admitted. Cross-schema
fields are rejected even when explicitly null.

Acceptance uses sequence `1` and no parent. Settlement uses sequence `3` and the
original confirmed acceptance receipt. Dispatch/retrospective external events
are not admitted. Semantic event identity is `sha256:` plus the SHA-256 of compact,
key-sorted JSON `{consent, domain, stage}`, where domain is exactly
`spawn.external-work-lifecycle-slot.v1`. The native identity domain is unchanged.

`LIFECYCLE_PUBLIC_DELIVERY_ORIGIN` is optional for the existing native lane, but
mandatory for *any* external event, including acceptance. It must be an exact
lowercase HTTPS DNS origin, without a port, trailing slash, path, credentials,
query or fragment. Completion links must equal that configured origin plus
`/deliveries/delivery_` and 32 lowercase hex digits. No alternate host, query,
fragment, percent encoding, redirect discovery or caller-selected origin is
accepted. No network fetch is made to establish the page: the trusted producer
must prove that the exact artifact is accepted, settlement recorded, and both
native owners explicitly consented to its active public delivery page before
emitting settlement. Publication-page availability and withdrawal remain the
producer's responsibility; this narrow publisher cannot invent that proof.

Rendering is exact, with one newline after the header:

```text
SPAWN | External Backroom {backroom}
Contract {public_contract_ref}: accepted. Budget quote {decimal} {currency}.
```

```text
SPAWN | External Backroom {backroom}
Contract {public_contract_ref}: completed. Settlement recorded. Budget quote {decimal} {currency}.
Public delivery: {public_delivery_url}
```

`decimal` is the integer quote expressed in six-decimal micro-units: left-pad to
at least seven digits, put the decimal point before the last six, remove trailing
fraction zeros and omit an empty fractional part. No floating point, dollar
symbol, currency inference or FX conversion is used. `TEST_USD` is non-redeemable
test credit; it remains visibly `TEST_USD`, never USD or USDC. `USDC` settlement
recording is not a claim that a seller withdrew funds. This is a quote, not actual
inference spend, seller net proceeds, or a newly invented budget cap.

For settlement, the server derives the acceptance ID from the same external
domain and consent hash, looks up the exact immutable acceptance request hash,
and re-renders acceptance using the same contract reference, label, quote and
currency. That text must equal the stored acceptance. Existing original-X-proof
validation then resolves its local receipt into the remote thread parent. A
different consent/quote/room/currency, manual resolution, uncertain send or
unconfirmed acceptance cannot create a completion reply. No caller-supplied
parent post ID is accepted.

The publisher computes request hashes from re-marshaled Go envelope JSON,
preserving the field order listed above and omitting absent optional fields.
Do not hash a key-sorted envelope: only the semantic event slot is key-sorted.
`docs/external-lifecycle-vectors.json` provides exact request bytes, hashes and
rendered text for producer/publisher agreement, including both currencies and
minimum/maximum quote precision. Fixture URLs and accounts are inert examples.

New source tests cover schema isolation, generic labels, origin/link pinning,
exact amounts, separate slot identity, acceptance-to-completion binding, original
proof, read-only recovery and configuration. Go tests and `gofmt` must be run in
a qualified Go 1.26 environment before release; this source workspace has no Go
compiler. The copied source archive and original manifests remain provenance
evidence, not deployment proof. Controller forwarding of the new dormant origin
setting is a separate integration gate because controller source was excluded
from the authorized publisher archive.
