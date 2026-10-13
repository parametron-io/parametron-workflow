# Controller runtime

The default CLI enables production Engineering lifecycle/reconciliation. Signed
GitHub notifications enter durable storage and the worker routes current resource
identity before Engineering computes complete desired state and performs authorized
mutations through narrow capabilities. Observation and PolicyInput are not
authorization. No production semantic/model provider is implicitly enabled.

Production Engineering consumes read-only [PR binding validation](pr-binding.md)
through accepted PR onboarding: managed membership, owned initial In Progress,
one-shot Draft normalization and durable completion. Membership repair requires
fresh accepted binding; completion preserves later Status and review readiness.
Deterministic onboarding and review/rework work without a semantic Runner.
`internal/prreview` consumes fresh accepted binding, validated onboarding completion,
current PR/Target/Project and native review evidence to authorize durable review
cycles and one-shot rework Draft effects. See
[review execution](execution.md#accepted-pr-review-and-rework).
Relationship projection, production companion integration and numeric convergence,
merge/completion, managed feedback and
general PR lifecycle reconciliation remain deferred.

[#48 pure PR companion ordering](pr-companion-ordering.md) is implemented and
independently tested, but is not composed into this production path. Persisted
field-ownership provenance acquisition and companion orchestration remain deferred.

Embedded callers opt in through `app.Config.Engineering`; its zero value retains
the Phase 2 foundation path, where normalized PolicyInput reaches FoundationSink.
That sink acknowledges without classification, desired-state computation, mutation,
input logging, or fabricated decision persistence. Completed means the configured
processing path succeeded, or routing settled an explicitly non-applicable event.

Supplying `app.Config.Runner` explicitly composes #27's durable semantic coordinator
using the shared Store and client. Runner + explicit Mutator composes #28's
semantic reconciler when no custom SemanticConsumer exists. A custom
SemanticConsumer retains the enriched input path; combining it with Mutator is
rejected. Without either, SemanticFoundationSink acknowledges without mutation.
This boundary requires an enriched consumer: an old Consumer cannot be combined
with Runner, and SemanticConsumer requires Runner. No production provider or CLI
semantic flags exist. Engineering enablement grants no semantic mutation authority.
Without Runner, Engineering still runs when the complete graph has Type authority:
accepted completion, or exact validated native Type with Classification disabled.
Enabled Classification without accepted completion returns engineeringcontext.ErrIncomplete
and retries fail closed, including for unrelated Issues/Bugs. See
[semantic-integration.md](semantic-integration.md) for gating, freshness, durable
completion, and restart behavior.

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
Engineering composition when enabled, worker/ingress, and finally binds HTTP.
Engineering requires IssueLister, Branches, CommitComparison, LifecycleFields,
NumericFields, ProjectItemReader, DeveloperReads, DraftConverter,
PullRequestProjectItemReader, ReviewFactsReader (part of Client), and the narrow
`pronboarding.Projects` method set
(`AddProjectItem`, `SetProjectOption`) on the shared client; missing narrow capabilities
fail composition before HTTP serving. The same client performs discovery and
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
`observe_observation`, `observe_configuration`, and `semantic_reconcile` via errors.Is. Unknown local
errors use terminal `local_processor`. No substring classification or raw error
string persistence occurs.
Semantic processing uses bounded retryable/terminal categories documented in
[semantic-integration.md](semantic-integration.md); process cancellation still
releases the claim rather than creating terminal semantic failure.
Engineering-local `engineering_changed` and `engineering_incomplete` retry.
Generic `engineering_activity_storage` and `engineering_branch_storage` are
terminal: no safe transient storage taxonomy is guessed. Graph/deployment,
policy/order/hierarchy/capacity, provenance/ambiguity, Project identity,
configuration, and malformed notification failures map to bounded terminal
Engineering categories. Provider/storage diagnostics never enter stable categories;
typed GitHub transient/rate-limit classification stays separate.
Onboarding retries only `pr_onboarding_changed` among its local categories;
invalid/rejected binding, conflicting Project evidence, corrupt provenance,
indeterminate one-shot outcome and storage failure are terminal. Exact categories
and recovery limits are in [execution](execution.md#accepted-pr-onboarding).
In particular, `ErrAmbiguous` is not automatically replayed as success.
Event-level outcomes do not crash the application; worker infrastructure or
HTTP listener/server failures stop the other component
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
exactly-once external side effects. Engineering uses fresh desired-state
comparison for ordinary field drift and durable branch/activity provenance for
one-shot effects; no reconciliation replay log is introduced.

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

Phase 2, injectable Phase #3 semantic normalization, and Phase #4 Engineering
Issue lifecycle/reconciliation are integrated and verified. The CLI enables
Engineering by default and can perform authorized Status, Roadmap Order, branch,
and first-activity/Start Date mutations. No Runner/provider is supplied by the CLI.
Explicit Runner + Mutator or custom SemanticConsumer composition remains separate;
see [semantic-reconciliation.md](semantic-reconciliation.md).

Production Engineering uses deployment-wide ResolveSnapshot, not the root-scoped
Resolve(root) relevance boundary. Every configured repository's OPEN/CLOSED Issues
must have Type authority. Complete Context → #36 BuildRoadmapOrder → #35 Evaluate
→ #36 Evaluate produces current policy and numbers; fresh Project comparisons
converge only owned drift. Completed activity supplies the OPEN In Progress overlay
and current direct-child Phase progression. Closed resources have no Status writes.
Numeric assignments/clears are independent of Set-Position and Status.

After successful field convergence, another complete Snapshot and FULL pure
pipeline run before global EnsureFrom fan-out. Exact eligible targets share that
fresh Snapshot/PhaseOrder; #37 independently validates eligibility and binds
cross-target authorization to the actual durable trigger. #38 retains first-activity
qualification, original push provenance, one-shot Start Date and no assignment.
See [execution.md](execution.md), [Engineering context](engineering-context.md),
and [ordering](engineering-ordering.md).

One context-aware deployment-wide process-local lane is acquired before complete
Engineering state computation and covers downstream semantic mutation when
explicitly composed, accepted PR onboarding, review/rework, activity, field reconciliation, and
branch work. It releases on all exits, without SQLite transactions across GitHub I/O. Model enrichment runs
outside it; PR and Issue semantic consumers execute inside it before effects.
The onboarding controller additionally serializes its own calls. Enrichment failure
does not fall through into Engineering. Worker
same-resource FIFO alone cannot serialize a plan touching multiple resources.
The lane is not a distributed/cross-process lock.

Retries resolve fresh complete state and Project observations and apply only
remaining/current drift. Changed topology wins; no batch provenance, reconciliation
completion record, replay log, or schema migration was added. One-shot branch and
activity namespaces retain their separate ownership. Accepted PR onboarding uses
its own [durable stage facts](persistence.md#accepted-pr-onboarding-provenance):
completed initialization is suppressed, membership remains separately reconcilable,
and unresolved one-shot attempts fail closed across cancellation/restart.

Review-aware reconciliation and first-activity guards preserve applicable Target
Status independently of the parent's lifecycle. Review changed-state errors,
including `github.ErrReviewFactsChanged`, retry as `pr_review_changed`.
Invalid/rejected authority, superseded cycles, correlation, provenance, ambiguity,
Project and storage errors are terminal under the `pr_review_*` categories in
[execution](execution.md#accepted-pr-review-and-rework). Superseded Status repair
may instead be suppressed by the flow/reconciler to permit newer readiness.
Typed GitHub transient/rate-limit failures retain retry handling; cancellation
releases claims. Restart preserves cycle facts and can repair applicable partial
Status writes, but cannot replay a Draft attempt without its durable receipt;
unresolved ambiguity requires manual investigation. The Engineering lane is
process-local, with no distributed serialization or GitHub/SQLite atomicity.

Phase #5 merge/completion, relationship projection, production companion
integration and numeric convergence, terminal
close/reopen synchronization, Bug Tracker scoring/admission, nested lifecycle,
branch deletion/revocation, native Project positioning, heavy agents, automatic
merge, managed feedback/decision-record persistence, App token minting, and
distributed coordination remain deferred.

## Standalone first activity (#38)

engineeringactivity is independently usable with caller-supplied complete current
#35 Input, read-only completed #37 evidence, exact current branch/ancestry reads,
fresh Project observations, narrow lifecycle fields and the shared provenance
Store. Two immutable facts provide restart/partial-write convergence; accepted
activity does not bypass current eligibility. Assignees remain unchanged.
At #38 standalone completion no app/CLI/worker composition was added. Current
production Engineering now supplies complete context, PhaseOrder, routing,
activity and reconciliation while preserving this package's narrow authority;
see [execution.md](execution.md).

For #38 qualification, the push’s immutable Delivery.ReceivedAt must be strictly
after the validated #37 successful-creation provenance CreatedAt exposed by its
read-only Completion value. Processing-time completion alone is insufficient.
Earlier or equal-time notifications grant no activity and produce no new activity
provenance or lifecycle writes, including on restart or out-of-order processing.

Stage 2 verification additionally covers complete Snapshot consistency, fresh
full policy before branch fan-out, canonical global convergence, original-delivery
activity resume, current-graph parent progression, topology-changing partial
failure, process-local lane cancellation/semantic overlap, Project/push routing,
non-applicable settlement, narrow numeric transport, explicit semantic authority,
terminal storage classification, and default CLI Engineering enablement.
