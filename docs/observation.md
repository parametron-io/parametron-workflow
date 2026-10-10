# Current-state observation

`internal/observe` implements issue #13's production handoff:

```text
durable event → resource identity → current GitHub reads
             → normalized ObservedState → Consumer.Evaluate(PolicyInput)
```

`NewResolver(config.ResolvedConfig)` implements `worker.ResourceResolver`.
`NewProcessor(config.ResolvedConfig, github.Client, Consumer)` implements
`worker.Processor`. Both constructors validate the resolved bindings and copy
repository slices and Project binding maps. They require explicit deployment
bindings; the processor additionally requires a client and consumer. No startup,
scheduling, retry loop, mutation, or lifecycle policy belongs to this package.
Observation is not authorization.

## Notification identity

The resolver reads only `repository.owner.login`, `repository.name`, and the
positive integer `number` in exactly one top-level `issue` or `pull_request`
object. An `issue.pull_request` object marks a PR represented in an Issue-shaped
notification, including PR comments. Thus Issue events/comments and PR
notifications/reviews/review comments share minimal identity rules independent
of event name. Additional transport fields are ignored. Both candidates, even
with the same number, are ambiguous and rejected. Null/nonobject identity
objects, missing identity, invalid numbers, duplicate object keys at parsed
identity levels, invalid JSON, and trailing values fail with `ErrIdentity`.
Payloads with only a node ID, Project item, push, or repository identity do not
identify a numbered Issue/PR and are unsupported. No node-ID lookup is invented.

Titles, bodies, labels, state, assignees, relationships, field values, and webhook
node IDs are never decoded into current state. NodeID is omitted from the durable
resolved binding. The scheduling key remains owner/repository/kind/number, using
workflow-neutral `issue` and `pull_request` kinds.

Owner/repository comparisons are case insensitive and output names are lowercase,
matching storage's identity normalization. The organization and repository must
match the resolved deployment, otherwise `ErrRepository` is returned before any
GitHub read. Normalized deployment repository collisions fail construction rather
than selecting a winner. There are no hardcoded organization/repository names.
The processor repeats this restriction even for previously bound events.

## Current reads and verification

The processor never reads `Delivery.Payload`. The bound kind selects
`Client.Issue` or `Client.PullRequest` with the configured discovered repository
spelling and exact resource number. The result must have a nonblank current node
ID, matching owner/repository and number, and matching discovered repository node
ID where configured. A mismatch produces `ErrObservation`, with no policy call.
The bound historical NodeID is ignored and replaced only in the observation by
the current primary result's node ID.

`Processor.ReadPrimary(ctx, resource)` shares these checks with normal processing
and returns primary state without Project reads or consumer calls. #27 uses this
read-only boundary for post-model content revalidation; its Projects are nil and
it is not a complete policy observation. See [semantic-integration.md](semantic-integration.md).

`Processor.ReadCurrent(ctx, resource)` reuses ReadPrimary and both managed Project
reads without a Consumer call, payload use, mutation, or policy. Process delegates
to it. #28 uses it immediately before Issue reconciliation; PR reconciliation
uses ReadPrimary. Snapshot copies Issue Type and nested field-option maps as well.

Current Issue properties, nullable author/type, parent, sub-issues, blocked-by,
blocking, and linked PRs are retained. Current PR properties include Draft,
state, head/base refs and SHAs, author, and closing Issues. Bodies remain unparsed;
Issue type is a native observation, not semantic classification. Relationships
come from the existing Issue/PR read, with no duplicate relationship requests.
Cross-repository relationships remain observations, including those outside the
deployment; their presence does not authorize fetching or changing those resources.

For each current Issue or PR, Engineering then Bug Tracker is queried through
`Client.ProjectItems(currentNodeID, configuredProjectID, expectedFields)`. Only
configured field IDs are supplied. Status/Priority/Effort expect single-select,
Estimate expects number, Start Date expects date, and Bug Tracker Priority Score
expects number. Kinds are fixed by roles, never inferred from runtime values.
The observer verifies item IDs, Project ID, content ID/kind, duplicate item IDs,
and configured value kinds. Inconsistent normalized client data fails with
`ErrObservation`; classified client errors retain their original category.

## Stable representation

`ObservedState` includes the normalized resource identity, `Presence`, exactly
one current Issue/PR when present, and both configured `ProjectState` entries.
Each Project entry contains its semantic profile, configured ID, and item list.
An empty list explicitly means absent membership. An item preserves ID and
archived state; its `Fields` map uses `config.FieldRole` keys. Missing roles mean
unset, distinct from present numeric zero. Dates and unmapped single-select
option IDs remain intact. A mapped Status option additionally receives its
`config.StatusRole`; unmapped options retain an empty semantic role. No status,
priority, eligibility, routing, or desired-state calculation is performed.

These are unordered resource facts, not manual Project positions. #36's separate
Project-wide `github.ProjectOrderReader` preserves explicit POSITION ASC connection
order as semantic input; see [engineering-ordering.md](engineering-ordering.md).
`ProjectState.Items` retains its existing stable-ID normalization.

Canonical ordering is:

- Projects: Engineering, then Bug Tracker.
- Items: item ID ascending.
- Labels: exact label name ascending.
- Assignees: exact login ascending.
- Relationship identities: lowercase owner, lowercase repository, number, node
  ID, repository node ID, ascending in that order.

Current primary/relationship repository names are lowercase. Empty unordered
collections normalize to empty slices; optional author/type/parent remain nil.
Collections are copied before sorting; client-owned data is not mutated or
aliased to consumer-owned state. Duplicate relationship/label observations are
retained and sorted rather than inventing deduplication policy. Field maps are
compared by key/value equality; Go's JSON encoder orders their string keys.
There is no wall-clock value in the observation or handoff. Multiple GitHub reads
are not an atomic snapshot, as documented by the client contract.

## Missing resources and failures

A primary Issue/PR `github.NotFound` produces a stable `Missing` observation:
requested resource identity, no current node ID, no Issue/PR, and no Projects.
No Project reads occur and no historical payload values substitute for missing
state. Present resources without membership have two known-empty Project lists;
missing resources have no Project observations.

A `not_found` during a required Project read discards the entire attempt's
observation and propagates unchanged. Even a successful earlier Project read
cannot produce a partial handoff. Under #12's existing classifier this is a
terminal failed attempt; the observer introduces no retry exception or fallback.
All other GitHub errors, including transient/rate-limit and unauthorized/forbidden,
propagate unchanged. #12 remains the durable retry/classification authority.
Consumer errors also propagate. Local errors are stable sentinel values usable
with `errors.Is`; the runtime must explicitly supply their worker classifier.
Errors contain no payload/provider data, and the observer performs no logging.

## Policy and decision-record handoff

`Consumer.Evaluate(context.Context, PolicyInput) error` receives delivery ID,
durable sequence, and the complete normalized current observation, including its
resource identity. These associate a future decision record with its durable
trigger and observed input. The envelope is not a workflow decision. This issue
persists neither observations nor fabricated decisions/provenance and requires
no storage migration. Policy input contains no webhook payload, HTTP metadata,
signature, secret, credential provider, or client.

Tests prove stale webhook/current-state disagreement through the actual resolver
and processor, and compose them with the real worker and temporary file-backed
SQLite store. No webhook HTTP server, startup, credentials, or network participates.

`internal/app` integrates source preparation, the shared GitHub client and Store,
this resolver/processor, worker Run, and signed HTTP ingress. The production
`app.FoundationSink` accepts PolicyInput and returns success without policy,
classification, mutations, logs of input, or decision persistence. Supplying
app.Config.Runner instead composes #27's semantic coordinator and enriched
consumer; semantic output remains separate from observed GitHub metadata. Integration
tests inject recording consumers and prove current-state handoff through signed
HTTP, SQLite, retries, restart, FIFO ordering, and shutdown. Lifecycle policy and
decision writing remain later phases. See [runtime.md](runtime.md).

Issue #34's standalone [Engineering context](engineering-context.md) reuses
ReadPrimary for relevant graph participants. Projects are unnecessary for its
current identities, body intent, and open/closed facts; it performs no lifecycle
policy handoff or mutations. Native relationship observations never determine
its authoritative declared child/dependency graph.
