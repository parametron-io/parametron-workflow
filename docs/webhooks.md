# Signed webhook ingress

`internal/webhook.New(Config)` constructs an `http.Handler`. Its only storage
capability is `InsertDelivery(context.Context, storage.Delivery) (bool, error)`;
`*storage.Store` implements it directly. Construction rejects missing (including
typed-nil) stores and empty secrets. The handler starts no server or workers and
performs no GitHub reads, mutations, redirects, or payload-driven callbacks.

## Transport and authentication

Only POST is accepted. Content-Type is parsed with `mime.ParseMediaType` and must
be `application/json`; valid parameters such as `charset=utf-8` are permitted.
Content-Type and each required GitHub header must have exactly one nonempty value.
Repeated fields, including identical repetitions, are rejected.

Required headers:

- `X-GitHub-Delivery`: 1–128 ASCII letters, digits, or hyphens. This is an opaque
  deduplication ID, not a resource identity; a UUID-specific grammar is not required.
- `X-GitHub-Event`: a lowercase ASCII letter followed by up to 127 lowercase
  letters, digits, or underscores. No event allowlist or workflow inference applies.
- `X-Hub-Signature-256`: exactly `sha256=` followed by 64 hexadecimal digits.
  Uppercase hexadecimal digits are accepted. Other algorithms, malformed encoding,
  repeated values, and comma-joined signatures are rejected.

Metadata is preserved without trimming or rewriting. Whitespace, control
characters, commas, and repeated delivery/event values fail their grammar.

`MaxBodyBytes` is 25 MiB (26,214,400 bytes), inclusive. GitHub documents a
[25 MB webhook payload cap](https://docs.github.com/en/webhooks/webhook-events-and-payloads).
The binary-sized limit accommodates that cap under either MB interpretation,
without imposing a small limit that excludes realistic events. Oversized declared
Content-Length fails early; `http.MaxBytesReader` also bounds actual reads,
including unknown-length/chunked bodies. No partial body reaches storage.

The explicitly injected secret is copied into private handler memory. It is
never loaded from the environment, placed in deployment configuration, or
persisted. Production secret loading belongs to #14. Authentication uses
standard-library HMAC-SHA-256 over the exact received bytes, hex decoding, and
`hmac.Equal` for constant-time digest comparison. JSON is never normalized before
verification. After authentication, the body must be syntactically valid JSON
with an object root. Empty bodies, arrays, scalars, null, and trailing JSON values
fail. There are no event-specific schemas or directive parsing.

## Durable acceptance

The handler validates method, media type, required headers, signature encoding,
body size/read, HMAC, and JSON before calling `InsertDelivery`. It passes the
original payload bytes, delivery ID, event name, a UTC acceptance timestamp, and
`Resource: nil`. `Config.Clock` defaults to `time.Now`; tests inject a function.
There is no hidden storage clock or in-memory queue.

A success acknowledgement is written only after insertion returns successfully.
SQLite uniqueness remains the final deduplication authority. `true, nil` means
new durable acceptance; `false, nil` means an equivalent redelivery. Both succeed.
Equivalent duplicates retain the first acceptance timestamp and all processing,
retry, resource-binding, and provenance state. Different event names or payload
bytes under the same ID return a conflict, preserving the original record.
Semantically equivalent JSON with different bytes is conflicting evidence.

## HTTP responses and observability

Every response has an empty body. Checks follow the order described above, so a
request invalid at multiple boundaries receives the first applicable response.

| Outcome | Status |
| --- | --- |
| New durable acceptance or equivalent duplicate | 204 No Content |
| Unsupported method | 405 Method Not Allowed, `Allow: POST` |
| Missing/malformed delivery or event metadata, body read failure, invalid JSON/object shape | 400 Bad Request |
| Missing, malformed, repeated, unsupported, or mismatched signature | 401 Unauthorized |
| Missing, malformed, repeated, or unsupported Content-Type | 415 Unsupported Media Type |
| Declared or actual body exceeds limit | 413 Content Too Large |
| Conflicting immutable delivery evidence | 409 Conflict |
| Other storage failure | 500 Internal Server Error |

`errors.Is(err, storage.ErrConflict)` handles wrapped conflicts. Raw Go/SQLite
errors never enter responses or logs. Failures do not receive a success response;
retry execution policy remains outside ingress.

`Config.Logger` accepts a standard-library `slog.Logger` and defaults to
`slog.Default()`. Each reception emits an info-level `webhook reception` record
with only a controller-owned, stable `outcome` category.
Categories include `accepted`, `duplicate`, `rejected_authentication`,
`rejected_metadata`, `rejected_json`, `rejected_body`, `rejected_oversized`,
`rejected_method`, `rejected_media_type`, `conflict`, and `persistence_failure`.
No delivery ID, event name, raw payload, secret, signature, credentials, or
underlying error text is logged.
There are no metrics or tracing dependencies.

## Authority and remaining work

Webhook payloads are immutable historical notification evidence, never canonical
current GitHub state or authorization. The handler does not resolve resources or
interpret Issue/PR fields, relationships, or Parametron directives.

Issue #12 owns claiming, workers, attempt accounting, retry execution/timing,
resource serialization, and crash recovery. Issue #13 owns event-to-resource
resolution from this preserved evidence, current GitHub state refetch, normalized
observations, and the policy handoff. Issue #14 owns server/listener configuration,
secret and credential loading, database paths, runtime composition, and graceful
end-to-end shutdown. The command remains the existing bootstrap.
