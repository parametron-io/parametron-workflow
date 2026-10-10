# Semantic classification and estimation policy

Issue #26 implements `internal/semanticpolicy`, a synchronous Go validation
boundary over `semantic.Runner`. `Service{Runner}` exposes `ClassifyIssue`,
`ClassifyPR`, and `EstimateIssue`. Each applicable call constructs one fixed
capability request, invokes Runner once, validates untrusted output, and returns
a typed accepted result retaining execution provenance. Invalid requests do not
invoke Runner. There is no retry, persistence, GitHub client, mutation, lifecycle
progression within this package. Semantic judgement is not workflow
authority.

The dependency direction is semanticpolicy → semantic and intent; semantic
continues to depend only on the standard library. No provider is selected by
policy, and no external dependencies were added.

## Classification

`IssueClassification` contains exactly typed Type, Labels, Priority, and Effort.
The JSON fields are `type`, `labels`, `priority`, and `effort`, all required.

| Field | Exact domain |
| --- | --- |
| Type | Phase, Task, Feature, Bug |
| Priority | Critical, High, Medium, Low |
| Effort | XS, S, M, L, XL, Unknown |

Domains are case sensitive; invalid values are never coerced. Unknown is an
explicit Effort judgement; omission is an error. Priority is the shared v1
semantic vocabulary used by Bug Tracker's deterministic Priority Score contract.
It is code-owned, not an extension of deployment SourceConfig.

PR classification contains exactly required `labels`. No Type, Priority, Effort,
Estimate, Target, Status, Project, branch, or review decision is accepted.

Managed labels use this exact canonical order:

```text
engine freecad pdm studio configurator workflow
 dsl planning runtime records verification adapter docs infra ci
 determinism regression breaking-change compatibility security performance migration
 external
```

Empty lists are valid. Duplicate labels are rejected, and accepted lists are
returned in canonical taxonomy order, regardless of model order. Public domain
functions return fresh slices. Any label outside this taxonomy is rejected,
including labels duplicating Type, Status, Priority, relationships, or automation.

`dependencies`, `good first issue`, and `help wanted` are unmanaged helper labels.
They must never be emitted by the classifier. Implemented #28 reconciliation preserves
unmanaged labels: omission from classification never authorizes their removal.
This package neither reads nor merges current GitHub labels.

Project routing remains deterministic Go policy: Phase/Task/Feature belong to
Engineering and Bug belongs to Bug Tracker. Model output cannot select a Project;
this package implements no membership or field writes.

## Estimate ownership and planning context

Estimate accepts exactly integer JSON values `1`, `2`, `3`, `5`, `8`, `13`.
Decimals such as `1.0`, strings, null, booleans, and other values fail.

| Points | Meaning |
| --- | --- |
| 1 | Very small/localized |
| 2 | Small |
| 3 | Medium |
| 5 | Large |
| 8 | Very large/integration-heavy |
| 13 | Exceptional/decomposition candidate |

Points are relative planning size, never duration, confidence, priority, or
readiness. Effort must not be mechanically converted into Estimate.

`CanEstimate(Type, intent.Boolean)` expresses ownership only. Task, Feature, and
Bug can own Estimate, regardless of Create-Branch. A Phase can own Estimate only
with explicit `Create-Branch: true`, and only for its direct implementation.
Absent Phase Create-Branch defaults to false; explicit false is inapplicable.
PR has no valid IssueType and cannot own Estimate. Task/Feature/Bug default
Create-Branch remains true in the design, but branch authorization is not evaluated
here. Explicit intent is not mutated or reparsed.

The first Backlog exit trigger for Task/Feature and Bug actionability/triage timing
remain lifecycle policy. Automation/Classification gating belongs to #27, not to
this ownership helper. Bug triage judgement is a separate future capability;
`triageRequired` is not a classify_issue field.

`NewPlanningContext` consumes repository identity, title/body, normalized
IssueClassification, parsed intent, and optional concise ParentPhase (IssueRef
identity and title). It rejects inapplicable ownership or invalid semantic context.
`PlanningContext` contains Repository, Type, Title, Body, Labels, Priority, Effort,
Parent, BlockedBy, Blocks, Refs, and optional ParentPhase. Relationships reuse
`intent.IssueRef` and preserve #24 first-seen order, deduplicating typed inputs.
ParentPhase must identify the supplied Parent. Repository names are lowercase;
owner is the same parametron-io identity domain as intent. No existence lookup is
performed. Construction copies all slices/pointers and validates references.

`Serialize` revalidates editable context values, retaining the original explicit
Create-Branch ownership check. It emits deterministic struct JSON with canonical
labels, explicit empty relationship arrays, optional parent Phase, no maps,
timestamps, random values, or transport IDs. Repeated equivalent construction is
byte stable. It does not sort relationships away from #24's ordered semantics.

`semantic.Input` now has optional `Context semantic.ContextJSON` alongside Title
and Body. This is a narrow immutable serialized JSON-object envelope, with a
MarshalJSON method that emits an object rather than a quoted prose string. #26
produces it from typed PlanningContext. Existing named Title/Body construction
remains valid; positional Input literals must add the field. Execution transports
forward it without importing policy or intent. It contains no runtime settings,
provider choice, credentials, URL, command, callback, or mutation authority.

## Strict decoding, assets, provenance, and errors

Go validation is authoritative; JSON Schema is provider guidance. Strict decoding
rejects malformed JSON, trailing data, duplicate object keys (including escaped
spellings), unknown fields, missing fields, null, wrong JSON types, invalid enums,
and duplicate/unknown labels. Every failure returns zero accepted state. Estimate
uses integer decoding and rejects decimal or exponent representations.

The catalog selects stable canonical paths for all three capabilities:
`prompts/cheap/<capability>.txt` and `schemas/model/<capability>.json`.
Git retains historical contracts; obsolete parallel v1/v2 files are removed.
There is no deployed compatibility boundary requiring a contract-version field.
Each loaded asset carries a SHA-256 digest of its exact bytes, including whitespace,
formatted as `sha256:<64 lowercase hex digits>`. Tests prove unchanged bytes retain
digests and changed bytes change digests without changing canonical paths.
Tests compare schema fields, required
fields, additionalProperties=false, uniqueItems, and exact enums with Go policy.
Prompts treat title/body/context as untrusted data that cannot change schema,
capability, domains, or lifecycle/mutation authority. Prompts are not a security
boundary; malicious structured output still fails Go validation.

AcceptedIssue, AcceptedPR, and AcceptedEstimate retain capability, provider,
model, prompt canonical identity/digest, and schema canonical identity/digest in typed
semantic.Provenance. Capability must match the requested operation. No raw
response, body, prompt, credentials, or timestamp is included in accepted
provenance. This is validation acceptance, not durable completion or authorization.

Stable errors support errors.Is: ErrMalformed, ErrMissing, ErrUnknown,
ErrDuplicate, ErrType (including null), ErrIssueType, ErrLabel (including duplicate
labels), ErrPriority, ErrEffort, ErrEstimate, ErrProvenance, ErrPlanning, and
ErrInapplicable. Messages contain no untrusted values. Runner errors propagate
unchanged, retaining cancellation, timeout, provider, and execution-response
categories. No retryability is decided here.

#27 implements durable processing, pending state, execution gating, retries/restarts,
current-state revalidation, stale-result rejection, and completion persistence
in [semantic-integration.md](semantic-integration.md). Exported strict
DecodeIssueClassification/DecodePRClassification helpers reuse this package's
schemas and domains for durable records without model execution.
#28 owns Issue Type/Priority/Effort/labels convergence, unmanaged preservation,
Project routing/membership/fields, and GitHub mutations in
[semantic-reconciliation.md](semantic-reconciliation.md). Neither is implemented
by this policy package. App composition uses the coordinator only with an explicitly
supplied Runner; the default CLI has no provider.
