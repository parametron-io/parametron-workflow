# Durable initial classification

Issue #27 implements `internal/semanticflow`. `New(Config)` constructs a
Coordinator implementing `observe.Consumer`; `Evaluate` enriches observation and
calls the new `Consumer.Evaluate(PolicyInput)` only on success. `Enrich` also exposes
the explicit Pending input on semantic failure for callers/tests.

```text
delivery → durable worker claim / resource FIFO → observe.Processor
 → semanticflow.Coordinator → enriched policy handoff → worker settlement
```

Dependencies are semanticflow → intent, semanticpolicy, semantic, observe types
and narrow primary reads, and generic storage. Semantic execution/policy remain
independent of worker/storage/app. Worker retains all scheduling and settlement.

## Gating and policy handoff

PolicyInput contains `Current observe.PolicyInput`, `Intent intent.Intent`, and
`Classification`. Current body parsing uses #24's parser with normalized current
owner/repository; webhook title/body remain historical evidence. Explicit intent
is preserved, and `Intent.Effective()` supplies defaults/master-switch semantics.

| State constant | Exact value | Meaning |
| --- | --- | --- |
| NotRequired | `not_required` | Missing resource or effective classification disabled |
| Pending | `pending` | Required classification without validated durable completion |
| Accepted | `accepted` | Required classification with validated durable completion |

Defaults require classification. Automation false, Classification false, and
Automation false with Classification explicitly true suppress Runner execution.
They fabricate no semantic metadata or accepted completion. Missing resources
retain Missing, skip execution, and expose no accepted classification.

Classification's fields are private. `State()` and `Required()` expose gating;
`Issue()` / `PR()` return typed accepted classification and bounded provenance
only in Accepted, with copied label slices. Observed GitHub Type/Priority/Effort/
labels cannot populate these accessors or bypass Pending. Failed enrichment
returns Pending with no accepted result and does not reach downstream policy.
#28 checks State and matching typed acceptance to refuse Pending convergence.
Acceptance itself grants no lifecycle, routing, or mutation authority.

## Durable one-shot completion

The existing provenance table is sufficient; SQLite schema remains version 1.
Namespace is `semantic.classification`. Stable keys are
`<lowercase-owner>/<lowercase-repository>/<issue|pull_request>/<decimal-number>`.
Node IDs are excluded. The namespace/key uniqueness boundary permits at most one
accepted initial completion. DeliveryID identifies its originating bound event;
CreatedAt comes from the injected clock and is excluded from metadata bytes.
The originating resource binding must agree before acceptance and on recovery.

Metadata uses compact deterministic struct JSON in this field order:

```text
{format: 1,
 resource: {owner, repository, kind, number},
 input_digest,
 issue: {type, labels, priority, effort} OR pr: {labels},
 provenance: {capability, provider, model, prompt_identity, prompt_digest,
              schema_identity, schema_digest}}
```

Only one classification kind is permitted. Labels use semanticpolicy's canonical
taxonomy order. Metadata excludes raw title/body, prompt, model output, HTTP
response, credential, token, authorization header, arbitrary error, timestamp,
provider request ID, and webhook payload. Only the input digest is retained.

Loading treats metadata as durable input. Records are bounded to 16 KiB, format 1,
matching resource/kind/capability, and exact canonical JSON bytes. Unknown,
duplicate, missing, case-variant, null, noncanonical, or conflicting values fail
closed. `semanticpolicy.DecodeIssueClassification` / `DecodePRClassification`
reuse strict schema/domain validation without executing a model or duplicating
taxonomy. Provider/model identifiers reuse bounded semantic deployment validation.
Prompt/schema identities must be canonical capability paths, with structurally
valid `sha256:<64 lowercase hex digits>` digests. Historical digests are retained;
loading does not depend on today's assets or provider selection.

Once completed, ordinary title/body/metadata edits do not invalidate initial
classification. No automatic reclassification or Bug triage reevaluation is
introduced. Temporarily disabling classification exposes NotRequired and current
intent while preserving historical completion; re-enabling reuses it. A later
event can complete under provider outage without invoking Runner.

## Input fingerprint and freshness

Before new execution, canonical map-free struct JSON serializes
`{resource: {owner, repository, kind, number}, title, body, intent, effective}`.
Intent includes #24's named explicit booleans, CreateBranch, Parent/Target, and
ordered relationship collections; effective is `Intent.Effective()`. SHA-256 of
the compact bytes yields `sha256:<64 lowercase hex digits>`. Exact title/body are
included because both are model inputs. No timestamp, delivery ID/sequence, node
ID, provider/model selection, Project field, or observed semantic output enters
this fingerprint.

Issues call only `Service.ClassifyIssue`; PRs call only `ClassifyPR`. Each required,
unaccepted worker attempt invokes Runner once. Estimate is never called. After
Go validation and before recording acceptance, `observe.Processor.ReadPrimary`
refetches Issue/PR primary state without Projects, sharing normal observation's
repository restrictions and node-ID/owner/repository/repository-ID/number checks.

Recomputed input must match. Title/body/directive changes, including newly invalid
directives, return retryable `semantic_stale` with no completion or accepted
handoff. No internal loop/retry occurs. Disappearance after execution is stale;
the next normal observation sees Missing and stops classification safely.

Changes only to classifier-owned GitHub output fields are drift rather than
semantic input changes. They do not invalidate initial judgement. GitHub reads
are not atomic; #28 refetches/compares metadata before mutation. The older
Current observation carried downstream is never mutation authority.

## Failures, retries, and restart

Typed errors are mapped with errors.Is/errors.As to bounded `Failure` identifiers.
Provider/database diagnostics are neither retained nor unwrapped into the worker
classifier, preventing nested transport errors from overriding semantic categories.
App's worker-local classifier consumes `FailureCategory`; no string matching or
raw error persistence is used.

| Category | Disposition |
| --- | --- |
| semantic_stale | Retryable |
| semantic_timeout | Retryable |
| semantic_rate_limited | Retryable |
| semantic_transient | Retryable |
| semantic_cancelled | Retryable when worker context is active |
| semantic_permanent | Terminal for this delivery |
| semantic_provider_unknown | Terminal for this delivery |
| semantic_response | Terminal malformed/structurally invalid response |
| semantic_policy | Terminal semantic domain/provenance validation failure |
| semantic_intent | Terminal invalid explicit directives/context |
| semantic_completion | Terminal corrupt/conflicting record/origin |
| semantic_persistence | Terminal unclassified persistence failure |

Storage exposes no safe transient DB taxonomy, so this integration does not guess
one. GitHub revalidation failures retain the existing GitHub taxonomy; invalid
identity/repository results retain observation categories. Terminal failure grants
no acceptance; a separate later event can attempt classification. Worker owns retry
timing, counts, and resource FIFO. There is no fallback or hidden retry policy.
Process cancellation takes precedence: worker releases active claims to Pending.
Coordinator checks context before acceptance and uses context-aware storage.

A crash before completion is recorded can repeat execution. After RecordProvenance
commits but before handoff/event settlement, restart recovery releases unfinished
work and validates/reuses the completion without another model call. The guarantee
is durable at-most-one accepted initial completion, not exactly-once inference.
Duplicate DeliveryIDs remain deduplicated, and separate later events reuse the
same completion. Retry predecessors continue blocking later work in their lane.

## App composition and phase boundary

`app.Config.Runner` explicitly enables semantic integration using the shared Store
and GitHub client. An explicit Mutator with no custom SemanticConsumer composes
the semantic reconciler. A custom SemanticConsumer receives enriched input;
without either, SemanticFoundationSink acknowledges it without mutations.
Mutator with a custom SemanticConsumer is rejected as ambiguous. Both require
Runner; the old observation Consumer cannot be combined with Runner. Without
Runner, FoundationSink/no-semantic runtime remains runnable. No CLI flags,
production provider, SDK, HTTP model transport, or model CLI is introduced.

#28 implements GitHub Type/Priority/Effort/label convergence, unmanaged-label
preservation, Project routing, and narrow unset-Status Backlog ingress; see
[semantic-reconciliation.md](semantic-reconciliation.md). This completes Phase #3
normalization. Lifecycle, branches, eligibility, triage, and Estimate timing remain
later work. No classification-pending label or storage migration is added.

Tests use real file-backed Store, worker, resolver, and observer with GitHub and
Runner/provider fakes. They cover gating/Pending, both capabilities, deterministic
metadata, corruption/origin/provenance checks, persistence failures, provider
categories, in-flight title/body/directive edits, disappearance, output-only drift,
cancellation, FIFO retries, duplicates/later events, provider outage, and reopening
after acceptance while the delivery remains Processing.
