# Controller foundation runtime

Phase 2 is a runnable deterministic controller foundation. It accepts signed
GitHub notifications durably, executes serialized work, refetches current GitHub
state, and hands normalized PolicyInput to `app.FoundationSink`. The sink returns
success; it does not classify, calculate desired state, mutate GitHub, log inputs,
or persist fabricated decisions. A completed event means this foundation handoff
succeeded. Observation and PolicyInput are not authorization.

## Command and deployment

```sh
./parametron-workflow \
  --config /etc/parametron-workflow/bindings.json \
  --data-dir /var/lib/parametron-workflow \
  --github-token-file /run/secrets/github-token \
  --webhook-secret-file /run/secrets/github-webhook
```

| Flag | Default / requirement |
| --- | --- |
| `--config` | Required strict SourceConfig JSON file; see [configuration.md](configuration.md) |
| `--data-dir` | Required directory; database filename is `workflow.sqlite` |
| `--github-token-file` | Required file; validated at startup and reread on every transport request |
| `--webhook-secret-file` | Required file; loaded once, rotation requires restart |
| `--listen` | `127.0.0.1:8080` |
| `--worker-concurrency` | `1`, must be positive; concurrent unrelated resource lanes |
| `--worker-poll-interval` | `1s`, must be at least one second |
| `--retry-delay` | `30s`, must be positive |

Secret files may end in one LF or CRLF. Empty values, embedded newlines, NUL,
and surrounding whitespace fail. Errors identify the setting/file without
printing contents. Credentials and operational settings never enter SourceConfig
or ResolvedConfig. No environment-variable fallback or `gh` runtime dependency
exists. Flag parsing and files belong to the command; lower-level packages do
not read arguments or environment variables.

The command's file-backed `github.TokenSource` is a deployment adapter. Tokens
may be replaced atomically between requests. It does not assume personal-token
semantics. A future GitHub App installation-token provider can replace it without
changing configuration, observation, worker, webhook, or policy boundaries.
JWT signing and installation-token minting remain deferred. The transport uses
`https://api.github.com/graphql`, a 30s HTTP timeout, and no hidden retries.

## Startup and storage

Startup parses the source file, loads secrets, constructs the token source and
transport, validates process settings, discovers live GitHub schema, calls
`config.Resolve`, validates observer boundaries, initializes SQLite, constructs
worker/ingress, and finally binds HTTP. The same client performs discovery and
all current Issue/PR/Project reads. Missing, ambiguous, or incompatible schema
prevents serving; no partial deployment or alternate current-state path exists.

The application creates the data directory with mode 0700 when absent and
creates/enforces mode 0600 on `<data-dir>/workflow.sqlite` before SQLite opens it.
Existing directory permissions are not changed. Deploy on a trusted local data
directory with exclusive process ownership. SQLite WAL/SHM files belong to the
live database. There is exactly one Store shared by ingress and worker. No DSN
flag, ingress cache, alternate database, or multi-process coordination exists.

## HTTP, execution, and errors

Only `/webhooks/github` mounts the signed handler. Unknown routes return 404.
Accepted and equivalent duplicate deliveries return 204 after durable insertion;
SQLite uniqueness preserves attempts, retry times, and terminal state. Conflicting
redelivery remains 409. See [webhooks.md](webhooks.md) for transport validation.
TLS certificates and public reverse-proxy termination are external deployment
concerns. The server uses a 5s header-read timeout, 30s read/write timeouts, and
60s idle timeout. There are no health/admin/UI endpoints.

Worker.Run and HTTP Serve run concurrently. Worker.Run recovers interrupted
processing before executing snapshots and polls between them. Same-resource FIFO
and unrelated-resource concurrency use the existing durable worker. Retry delay
is exactly `now + retry-delay`; there is no jitter, exponential backoff, alternate
scheduler, or immediate provider retry. Durable eligibility is checked by Worker.

GitHub transient/rate-limited errors remain retryable under the worker taxonomy.
The application's non-GitHub classifier uses terminal categories
`observe_identity`, `observe_repository`, `observe_binding`,
`observe_observation`, and `observe_configuration` via errors.Is. Unknown local
errors use terminal `local_processor`. No substring classification or raw error
string persistence occurs. Event-level outcomes do not crash the application;
worker infrastructure or HTTP listener/server failures stop the other component
and return a bounded error. Startup failures identify the failed boundary.
No payloads, signatures, Issue/PR bodies, PolicyInput, credentials, or arbitrary
provider errors are logged by runtime composition.

## Shutdown and recovery

SIGINT/SIGTERM cancel the application context. The app closes its ingress
admission gate, initiates HTTP Shutdown, and cancels worker processing. Requests
arriving through the closed gate receive 503. HTTP shutdown has a fixed 10s
budget; on expiry connections close forcibly. The app still waits for admitted
handlers, worker cleanup, and both owned goroutines before closing SQLite.
Worker cancellation releases owned claims using an uncancelled cleanup context.
Processors must honor cancellation; cleanup is not cut short by a sleep/timeout.
Store failures during cleanup are fatal and durable interrupted state remains
recoverable. Normal context cancellation returns success.

On reopen, Worker.Run uses existing recovery to release processing rows to
pending, retaining claim generations, evidence, and resource bindings. Pending
work resumes, retry eligibility times remain intact, and completed/failed work
stays terminal. Stale generations cannot settle a recovered attempt. Unfinished
handoffs have at-least-once recovery semantics; this foundation does not promise
exactly-once future external side effects. Later policy must use provenance and
idempotency protocols for its authorized effects.

## Verification and phase boundary

`internal/app` tests use the production composition function with real file-backed
SQLite, webhook handler, worker, resolver and processor, github.Fake, and recording
consumers. They cover signed local HTTP serving, stale payload/current-state
handoff, both Projects, duplicate acceptance after completion, pending and
processing close/reopen recovery, stale claim rejection, clock-stepped retry,
barrier-controlled resource concurrency/FIFO, invalid schema/settings, fatal
component propagation, and cancellation/reopen. Command tests cover strict file
loading, missing/malformed config, invalid secrets, and replaceable credentials.
No test needs real GitHub, credentials, external HTTP, or gh.

Phase 2 documentation is ready for closure. Semantic/model integration,
Engineering/PR/Bug Tracker lifecycle policy, directives, desired-state
reconciliation, branch authorization, managed feedback, decision writing,
GitHub mutation APIs, App token minting, and distributed coordination remain
later work. No GitHub state is mutated by this runtime or its integration tests.
