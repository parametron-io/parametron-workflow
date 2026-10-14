# Parametron Engineering Workflow System Overview

This document defines the workflow automation contract for the public GitHub
Project **Parametron Engineering**, including current implementation and future
PR lifecycle behavior.

It describes the controller authority model, issue and pull-request lifecycle,
Phase behavior, deterministic ordering and reconciliation, semantic
classification, declarative body directives, GitHub relationship projection,
and user-facing automation feedback.

Issue #39 completes the implemented and verified Phase #4 Engineering Issue
lifecycle/reconciliation integration gate. Production composition uses a complete deployment-wide Snapshot,
pure roadmap/lifecycle policy, owned Status and numeric Roadmap Order convergence,
authorized branch creation, and first qualifying development activity.
The default CLI enables Engineering reconciliation and can perform authorized
mutations through narrow GitHub capabilities. It does not activate a production
semantic provider or implicitly grant semantic mutation authority.

The package contracts remain separate: [Engineering context](docs/engineering-context.md)
owns current declarative facts, [lifecycle policy](docs/engineering-lifecycle-policy.md)
owns pre-development decisions, [ordering](docs/engineering-ordering.md) owns
roadmap topology and numbers, and [execution](docs/execution.md) describes branch
and activity provenance plus production reconciliation.

Accepted Engineering PR onboarding is implemented through read-only #46 binding
and `internal/pronboarding`: managed membership, owned initial Status, one-shot
Draft normalization, and durable completion. #48's pure
[PR companion ordering](docs/pr-companion-ordering.md) is implemented and
independently tested. Production companion composition, numeric-field convergence,
ownership-provenance acquisition and orchestration remain deferred.
`internal/prreview` implements deterministic review/rework after accepted binding
and completed onboarding, without a semantic Runner. Issue #50 implements
independently verified accepted PR merge/abandonment,
native Issue close/reopen synchronization and the direct-child Phase closure gate.
Native Issue Close is manual completion authority; Project Done projects valid
terminal state and never authorizes closure. Durable merge-close attempts and
validated receipts protect later native reopen. Production Engineering integration
and local policy, transport, persistence/restart and worker tests cover these
boundaries; see [terminal execution](docs/execution.md#accepted-pr-completion-and-terminal-synchronization).
Issue #51 implements independently tested [standalone Development reconciliation](docs/pr-development.md)
from fresh accepted binding, validated onboarding and current managed membership.
Production-wide Development routing, other relationship projection, managed feedback,
and full Phase #5 continuous/global orchestration,
nested-child lifecycle, Bug Tracker scoring/admission, branch deletion/revocation,
native Project position reconciliation, heavy agents, automatic merge, and
distributed coordination are deferred. Declarative graph facts are current;
Parent/dependency/SubIssues projection remains a future contract.
Assignee initialization is suppressed while reliable assignability evidence is
unavailable. Future sections below specify contracts rather than implemented
Phase #4 behavior.

The **Bug Tracker** reuses the controller architecture and shared lifecycle
primitives defined here, but applies a separate Project-specific policy for
triage, priority scoring, and Ready admission. That policy is defined in
[bug-tracker.md](bug-tracker.md).

---

## 1. Goals

The workflow system exists to make GitHub planning and execution predictable,
reproducible, and resistant to accidental UI state drift.

The system must:

- keep live engineering state in GitHub Issues and GitHub Projects
- preserve GitHub-native relationships where they are useful
- express automation intent in plain text where GitHub UI mutation would be
  ambiguous or non-authoritative
- use deterministic policy for lifecycle transitions and side effects
- use an LLM only where semantic judgement is genuinely useful
- reconcile accidental or unauthorized UI changes back to canonical state
- explain rejected user intent without producing comment spam
- preserve repository ownership and cross-repository dependency boundaries
- avoid making the LLM an authority over GitHub lifecycle state

The central rule is:

```text
LLM interprets semantic content.
Go owns policy and mutations.
GitHub records observable state.
```

A related authority rule is:

```text
observation != authorization
```

Seeing a GitHub state, relationship, branch, commit, or user action does not by
itself authorize a downstream side effect. Side effects are produced only by a
validated controller policy decision.

---

## 2. System Shape

The controller is an event-driven Go service with durable local state. The
following shared architecture includes future decision/feedback recording;
Engineering reconciliation has no batch completion record or replay log.

```text
GitHub event
    ↓
durable SQLite inbox
    ↓
fetch current GitHub state
    ↓
parse explicit body directives
    ↓
optional semantic classification
    ↓
compute canonical desired state
    ↓
compare actual vs desired state
    ↓
reconcile controller-owned GitHub state
    ↓
record decision / feedback metadata
```

The webhook payload is a notification that something changed. It is not the
canonical state used for policy decisions.

Before acting, the controller fetches the current GitHub state of the affected
resource and evaluates policy against that current state.

---

## 3. Authority Model

### 3.1 Go controller

The Go service is the workflow authority and side-effect executor.

It owns:

- GitHub Project routing
- normal Project Status lifecycle policy
- Engineering Roadmap Order derived numeric field
- Phase and child eligibility rules
- dependency gating
- authorized development-branch creation
- authorized branch provenance
- Start Date initialization
- GitHub relationship projection owned by the workflow
- PR onboarding normalization
- PR review-state lifecycle transitions
- close / reopen synchronization
- reconciliation of controller-owned state
- managed workflow feedback
- validation of all LLM output before mutation

The controller must remain useful without an LLM for already-classified work.
If semantic classification is temporarily unavailable, existing deterministic
workflow behavior must continue to operate.

### 3.2 Cheap semantic model

Issue #25 implements a standalone replaceable execution boundary in
`internal/semantic` for `classify_issue`, `classify_pr`, and `estimate_issue`.
Workflow callers request a capability; separate deployment configuration selects
one cheap provider/model through constructor-injected adapters. Canonical prompt
and schema assets with content-digest provenance and bounded execution are documented in
[docs/semantic-execution.md](docs/semantic-execution.md). Issue #26 adds strict
classification/Estimate validation and typed normalized planning context in
`internal/semanticpolicy`, documented in
[docs/semantic-policy.md](docs/semantic-policy.md). Issue #27 implements injectable
durable initial classification, gating, Pending safety, completion reuse, and
stale-result rejection in `internal/semanticflow`, documented in
[docs/semantic-integration.md](docs/semantic-integration.md). No production
provider is implemented. Issue #28 adds accepted semantic metadata and deterministic
Issue Project convergence through explicit Mutator injection, completing Phase #3
normalization; see [docs/semantic-reconciliation.md](docs/semantic-reconciliation.md).
Pure pre-development lifecycle policy is composed with Engineering reconciliation
in production. Accepted semantic Type can feed deterministic Engineering without
another model call. Every enumerated Issue, including unrelated work and Bugs,
requires Type authority: accepted semantic completion, or exact validated native
Type only when Classification is disabled. Enabled Classification without
accepted completion returns ErrIncomplete; invalid native fallback fails closed.
Labels, Project membership, and Status cannot designate
nonparticipants. No Snapshot-wide semantic backfill is performed.
Semantic output is not workflow authority.

A cheap LLM is used only for bounded semantic interpretation: initial
classification and relative work estimation.

For Issues it may initially classify:

- Issue Type
- labels
- Priority
- coarse Effort

Before an eligible Issue first leaves `Backlog`, the same cheap semantic model
may also produce a bounded relative `Estimate` using the normalized planning
context defined later in this document.

For Pull Requests it may classify:

- labels

The model does not directly mutate GitHub and receives no GitHub credential.
It returns strict structured output which is validated by the Go service
against configured allowlists and schemas.

The model does not decide:

- lifecycle Status
- Engineering Roadmap Order
- branch creation
- dependency eligibility
- Parent / blocked-by / blocking relationships
- PR Target relationship
- Development relationship
- close / reopen behavior
- Phase activation
- review transitions

### 3.3 Future heavy agents

Triage, Audit, Validation, Review, and other repository-aware capabilities may
later be implemented by heavier agents.

Those agents remain judgement providers rather than GitHub authorities. The Go
controller continues to validate capability policy and perform allowed
mutations.

Heavy-agent behavior is not required for the Parametron Engineering lifecycle
defined in this document.

---

## 4. Durable Event Processing

SQLite provides durable local coordination for webhook processing and
controller provenance.

At minimum, the controller should be able to persist:

- GitHub delivery identity for deduplication
- event payload or a stable event reference
- processing state
- retry state
- affected resource identity
- authorized branch identity
- branch creation/base SHA
- PR onboarding completion
- managed automation comment identity
- relevant reconciliation/violation state

Delivery identity must be unique so repeated webhook deliveries do not repeat
one-shot side effects.

Events affecting the same Issue or Pull Request are serialized in resource order
by the worker. Because one Engineering plan can mutate multiple resources, one
context-aware deployment-wide process-local Engineering lane is acquired before
complete state computation and covers downstream semantic mutation when explicitly
composed, accepted PR onboarding, activity handling, reconciliation, and branch work.
It releases on all exits and holds no SQLite transaction across GitHub I/O. Model enrichment runs
outside the lane; Issue and PR semantic consumers execute inside it before
Engineering effects. Enrichment failure stops that delivery before Engineering.
Onboarding uses durable stage facts rather than delivery deduplication alone to
suppress repeated initialization; unresolved one-shot attempts fail closed. See
[persistence](docs/persistence.md#accepted-pr-onboarding-provenance).
This is not a distributed lock or cross-process serialization guarantee.

---

## 5. Live Workflow State

The **Parametron Engineering** GitHub Project is the source of truth for live
workflow state.

The canonical statuses are:

```text
Backlog
Ready
In Progress
In Review
Blocked
Done
```

These statuses describe workflow state. They must not be duplicated with
status labels.

---

## 6. Issue Types

The workflow recognizes the existing GitHub Issue Types:

- `Phase`
- `Task`
- `Feature`
- `Bug`

A Phase is a bounded engineering objective that groups related work and has
explicit completion criteria.

Task, Feature, and Bug are work-item types. Task creation remains a
maintainer-owned planning operation according to the existing development
workflow.

A Bug classification is routed outside Parametron Engineering to the Bug
Tracker according to deterministic project-routing policy. Bug Tracker
lifecycle, triage, scoring, and Ready-admission policy is defined in
[bug-tracker.md](bug-tracker.md).

---

## 7. Declarative Body Directives

Automation directives are optional plain-text controls parsed deterministically
from an Issue or Pull Request body.

The parser must use an exact grammar and allowlist. Loose substring matching is
not sufficient.

### 7.1 Boolean directives

```text
Automation: true | false

Classification: true | false
Triage: true | false
Audit: true | false
Validation: true | false
Review: true | false

Set-Status: true | false
Set-Position: true | false
Create-Branch: true | false
```

### 7.2 Relationship directives

Within the same repository, a relationship may use:

```text
#<issue-number>
```

Across repositories, use the canonical qualified form:

```text
parametron-io/<repository>#<issue-number>
```

Supported relationship directives are:

```text
Parent: <issue-ref>
Blocked-By: <issue-ref>...
Blocks: <issue-ref>...
Refs: <issue-ref>...
Target: <issue-ref>
```

`Parent` and `Target` are singular. `Blocked-By`, `Blocks`, and `Refs` support
one or more whitespace-separated references. Repeated lines normalize into a
deterministic deduplicated collection in first-seen order. Conflicting singular
declarations fail; comma and semicolon separators are rejected. The implemented
pure parsing boundary and exact line grammar are documented in
[docs/directives.md](docs/directives.md).

An absent relationship directive implies no relationship of that directive
type from the body contract.

---

## 8. Directive Defaults

Missing boolean directives use policy defaults.

### 8.1 Global defaults

| Directive | Default | Meaning |
| --- | --- | --- |
| `Automation` | `true` | Enables model/agent-driven capabilities for the item. |
| `Classification` | `true` | Enables initial semantic classification. |
| `Triage` | `false` | Heavy technical triage is opt-in. |
| `Audit` | `false` | Repository audit is opt-in. |
| `Validation` | `false` | Heavy implementation/contract validation is opt-in. |
| `Review` | `false` | Heavy agent review is opt-in. |
| `Set-Status` | `true` | Allows normal Project lifecycle status ownership and reconciliation. |
| `Set-Position` | `true` | Retained grammar; does not gate Engineering Roadmap Order or Bug Tracker Priority Score. |

### 8.2 `Create-Branch` defaults

`Create-Branch` is type-sensitive.

| Item type | Default |
| --- | --- |
| `Phase` | `false` |
| `Task` | `true` |
| `Feature` | `true` |
| `Bug` | `true` |
| Pull Request | not applicable |

A Phase therefore receives no development branch merely because it is active.
A Bug uses the normal implementation branch path by default; Bug-specific
readiness gating is defined in [bug-tracker.md](bug-tracker.md).

A Phase that directly carries implementation work must opt in explicitly:

```text
Create-Branch: true
```

`Automation: false` disables model/agent capabilities such as Classification,
Triage, Audit, Validation, and Review.

It does not disable deterministic relationship directives, dependency
processing, Project membership synchronization, close/reopen synchronization,
or the deterministic lifecycle controller itself.

Because Estimate generation is model-driven, the master `Automation: false`
switch also disables automatic Estimate generation.

Explicit child capability flags do not override the master automation switch:

```text
Automation: false
Audit: true
```

still does not run an audit.

---

## 9. Label Taxonomy

Labels are semantic metadata. They must not duplicate native Issue Type,
Project Status, Priority, or automation directives.

The cheap classifier may select labels only from the configured organization
allowlist. It may return an empty label set. It should prefer the smallest
sufficient label set rather than labelling aggressively.

### 9.1 Area / subsystem labels

```text
engine
freecad
pdm
studio
configurator
workflow

dsl
planning
runtime
records
verification
adapter
docs
infra
ci
```

These answer where the work belongs or what subsystem it concerns.

Product-specific labels such as `engine`, `freecad`, `pdm`, `studio`,
`configurator`, and `workflow` may be combined with concern labels such as
`runtime`, `verification`, or `adapter`.

`adapter` represents generic CAD or external-system adapter architecture.
`freecad` represents the Parametron FreeCAD adapter specifically.

### 9.2 Engineering characteristic / risk labels

```text
determinism
regression
breaking-change
compatibility
security
performance
migration
```

These describe a meaningful engineering property or risk.

### 9.3 Coordination label

```text
external
```

`external` is reserved for cross-repository dependency/coordination signalling.

### 9.4 Unmanaged helper labels

The canonical repository label set also contains a small set of helper labels
that are intentionally outside the classifier-managed allowlist:

```text
dependencies
good first issue
help wanted
```

The controller must preserve these labels when present, but semantic
classification must not add or remove them.

### 9.5 Labels that must not be introduced

Do not duplicate Issue Type:

```text
bug
feature
task
phase
```

Do not duplicate Project Status:

```text
backlog
ready
in-progress
in-review
blocked
done
```

Do not duplicate Priority with labels such as:

```text
high-priority
critical
```

Do not encode automation capabilities as labels such as:

```text
needs-audit
needs-review
needs-validation
```

Those belong in body directives.

Do not encode native relationship state as labels.

---

## 10. Initial Classification and Project Ingress

A newly created Issue or Pull Request first enters a normalization path.

Classification may enrich the item, but classification itself never advances
an item out of `Backlog`.

### 10.1 Issue classification

For a naked Issue, the cheap semantic model may propose:

```text
Type
Labels
Priority
Effort
```

Effort is coarse planning metadata and should support uncertainty, for example:

```text
XS | S | M | L | XL | Unknown
```

The controller validates the returned values against configured fields and
allowlists.

Project routing is deterministic and derived from the normalized type. The LLM
does not independently choose the Project.

`Estimate` is not part of initial classification. It is Project planning
metadata produced later, when the item is about to leave `Backlog` for the
first time.

### 10.2 Estimate on first Backlog exit

Before an eligible Task or Feature first moves from `Backlog` to `Ready` or
`Blocked`, the controller may request a bounded relative Estimate from the
cheap semantic model.

The allowed Estimate values are:

```text
1 | 2 | 3 | 5 | 8 | 13
```

Estimate is a relative planning point, not a duration in hours or days.

The intended scale is:

| Estimate | Intended meaning |
| --- | --- |
| `1` | Very small, localized change with one obvious implementation surface and focused verification. |
| `2` | Small change across a few related files or one narrow component with straightforward tests. |
| `3` | Medium change with meaningful implementation in one subsystem and multiple tests and/or documentation synchronization. |
| `5` | Large change spanning several related implementation surfaces, contract/behavior changes, or substantial verification. |
| `8` | Very large change involving multiple components/packages or significant integration and coordination work. |
| `13` | Exceptional scope. Valid, but normally a decomposition candidate. |

The estimator receives normalized planning context rather than raw repository
credentials or mutation authority. Useful inputs include:

- Type
- title and body
- canonical labels
- Priority
- Effort
- Parent
- Blocked-By / Blocks
- Refs
- repository identity
- concise parent Phase context when relevant

The controller validates the model result against the exact allowed value set
before writing the Project field.

Effort and Estimate are intentionally different:

```text
Effort
→ coarse semantic size impression from initial classification

Estimate
→ relative planning point produced at Backlog exit
```

Effort must not be mapped mechanically to Estimate.

Estimate also does not determine readiness, dependency eligibility, Phase
ordering, or branch authorization.

Estimate ownership is type-sensitive:

```text
Task / Feature
→ may receive Estimate before first Backlog exit

normal Phase
→ no Estimate

Phase + Create-Branch: true
→ may receive Estimate for the Phase's direct implementation work only

Pull Request
→ no Estimate; Target Issue or Phase owns planning Estimate
```

A Phase Estimate must never represent the sum of its child work.

### 10.3 Project ingress

When an item enters Parametron Engineering, the controller initializes the
canonical workflow state rather than relying on GitHub's built-in
"item added" workflow.

Initial classification/normalization results in:

```text
Project membership
+ normalized metadata
+ Backlog
```

Further lifecycle policy then determines whether the item should remain
Backlog, become Blocked, or become Ready.

---

## 11. Relationship Model

GitHub-native relationships are used where they carry useful semantics, but
controller-owned relationship state is projected from the declarative body
contract.

### 11.1 Parent

```text
Parent: <issue-ref>
```

represents Phase/work hierarchy.

A child may live in a different repository from its parent Phase. The
repository owning the behavior continues to own the child Issue; the parent
Phase determines workflow grouping.

### 11.2 Blocked-By / Blocks

```text
Blocked-By: <issue-ref>
Blocks: <issue-ref>
```

represent dependency edges.

These relationships affect lifecycle/execution eligibility. Dependencies between
roadmap roots order the complete root graph: Phase, parentless Task, and parentless
Feature. Phase-owned child dependencies do not order direct children or become
root edges; invalid cross-level roadmap dependencies fail closed without promotion.
Cross-repository dependency edges use qualified Issue references and the
`external` label according to existing repository conventions.

### 11.3 Refs / Relates to

```text
Refs: <issue-ref>
```

projects to GitHub's native `Relates to` relationship.

This is traceability only.

`Refs` does not:

- affect readiness
- affect branch creation
- affect Phase ordering
- block closure
- trigger lifecycle transitions

### 11.4 Target / Development

```text
Target: <issue-ref>
```

is Pull-Request-specific implementation intent.

It identifies the Issue or Phase whose implementation is represented by the
Pull Request and projects to GitHub's native Development relationship.

`Refs` must never substitute for `Target` in lifecycle policy.

Standalone #51 projection consumes current accepted #46 binding and validated
#47 onboarding completion, independently requiring exact current Engineering
membership for PR and Target. Native links, labels, Project positions and branch
names alone cannot select or replace the Target. Repository-qualified identities
are preserved. Manual links are correctable only with unambiguous owned evidence;
keyword links are never removed and conflicting keyword evidence fails closed.
See [Development relationships](docs/pr-development.md); production routing remains deferred.

### 11.5 Security Alert

GitHub Security Alert relationships are treated as external/native metadata.
They are not currently controller-owned and carry no Parametron Engineering
workflow semantics.

Bug Tracker currently treats Security Alert relationships the same way: they
remain native/external metadata with no v1 workflow semantics. Future security
policy may extend that boundary.

---

## 12. Phase Dependency and Activation Model

Phase ordering is not determined by a global readiness score.

Phase dependencies define roadmap order as a partial order.

For a Phase:

```text
any active OPEN blocker
→ Blocked

unblocked and selected current roadmap Phase for its repository
→ Ready

otherwise (OPEN and unblocked)
→ Backlog
```

When a Phase becomes `Ready`, its OPEN direct Task/Feature children become
`Ready`, even when an individual child still has a sibling or other work-item
blocker. Bugs remain dependency facts without Engineering Status decisions;
CLOSED children receive no Status reconciliation.

This is intentional:

```text
Ready
= included in the active Phase work set

execution eligible
= Ready + no active blocker + branch policy permits execution
```

A blocked child therefore remains visible as `Ready` under an active Phase but
cannot begin execution until its blocker clears.

The implemented policy selects at most **one active Phase per repository**
from the validated PhaseOrder. An active Phase may still have multiple child
work items in progress concurrently.

If the parent Phase is not active, unstarted children cannot independently
advance to `Ready` or begin authorized development activity. A child whose parent
Phase is blocked has pre-development Status `Backlog`. Validated completed first
activity remains durable started-work evidence: under normal Status ownership,
an OPEN started child stays desired `In Progress` despite later parent inactivity,
blockers, or Create-Branch false. Those facts prevent new starts; they do not
erase a completed start. Observed manual In Progress supplies no such evidence.

---

## 13. Phase and Child Ordering

Dependencies between top-level roadmap roots define roadmap order.

A stable iterative topological ordering uses dependencies between roadmap roots:
Phases and parentless Task/Feature, including CLOSED topology evidence. Canonical
owner, repository, Issue number, then node evidence breaks ties among eligible roots.
PhaseOrder for #35 is the Phase-only projection of that single traversal.
Phase-internal Task/Feature membership comes from authoritative #34 Parent edges.
Native card position,
manual drag/drop, Project view order, and metadata do not supply roadmap authority.

Engineering requires a controller-owned numeric **Roadmap Order** Project field.
A human administrator configures the view to sort **Roadmap Order ASC**. GitHub's
current Projects UI displays the sort field on cards while that sorting is
active; this presentation behavior is not workflow authority. Roadmap Order is
recomputable derived state, not identity or provenance. `Set-Position` and
Automation do not suppress its computation.

Full RoadmapRoots contains every top-level item; PhaseOrder contains every Phase.
Numeric segments retain OPEN parentless Task/Feature roots and Phases that are
OPEN or have OPEN authoritative direct Task/Feature children. Retained
segment bases are `10000 + index * 1000`: 10000, 11000, through 99000. OPEN
children sort by canonical resource identity and receive base + 10, +20, through
+990. Every primary roadmap Issue slot, including roots, reserves final-digit
+1..+9 offsets as nine generic companion slots. #48 supplies the pure PR companion
contract; other categories and shared occupancy arbitration remain deferred.
#36 neither assigns nor interprets companions. Capacity is 90 retained top-level segments
across mixed Phases/standalone Tasks/standalone Features, and 99 OPEN
direct children per segment; excess fails closed. Assigned values stay five-digit.

CLOSED roots and direct children have canonical Roadmap Order unset. Remaining
OPEN siblings and retained segments compact immediately: completing A=10000 and
all A's children rebases B=11000 to 10000. A CLOSED parent with OPEN children
retains its segment for those children without assigning the parent itself.

Root-to-root dependencies determine roadmap segments, including Phase → standalone
Task → Phase chains. A root cannot use another Phase's owned Task/Feature child as
a dependency endpoint; incorrect cross-level intent fails closed. The controller
never promotes child edges into Phase edges. Child-to-child and same-Phase
parent/child dependencies remain lifecycle facts and do not reorder root segments.
Closing a standalone root clears its value and compacts later mixed-root slots.

When a Phase is `Ready`, its OPEN direct Task/Feature children are displayed
contiguously with it under Roadmap Order sorting:

```text
Ready

Phase #45
  #48
  #49
  #50

Phase #60
  #61
  #62
```

### 13.1 Numeric anchor after Phase activation

When the first child starts work, the parent Phase moves to `In Progress`.
Unstarted children may remain `Ready`.

Those Ready children must not become visually detached from the Phase's Roadmap
Order segment.

A child inherits its parent Phase's Roadmap Order segment regardless of the
parent's current workflow status.

Conceptually:

```text
effective child order
=
parent retained segment base + canonical direct-child offset
```

Therefore, if Phase #45 moves to `In Progress`, its remaining Ready children
continue to occupy the Phase #45 roadmap segment and stay ahead of a later
Phase #60:

```text
Ready

#49
#50
#51

Phase #60
#61
#62
```

The absence of Phase #45 from the Ready column must not allow a later Phase to
interleave above #45's remaining Ready work.

For example, #45=10000, #48=10010, #49=10020, and #60=11000. Filtering out #45
and started #48 leaves #49's unchanged value ahead of #60. Child dependencies
affect execution eligibility, not numeric sibling order. Cross-repository Parent
edges use the same rules. Production reconciliation consumes exact assignments
and Clear from the pure ordering plan against fresh normal Project observation.
Missing/stale assignments are written; matches are no-ops; finite zero,
fractional, or out-of-domain values are drift. Clear truly unsets the field,
never writes numeric zero. Set-Position false does not suppress this convergence.
No native item-position, afterId, card comparison, or movement is used.

---

## 14. Parentless Work Items

A normal Task or Feature without a parent Phase is allowed but expected to be
uncommon.

For parentless work:

```text
blocked
→ Blocked

unblocked
→ Ready
```

All unblocked parentless Task/Feature work may be Ready; no lifecycle admission
capacity or numeric admission score is defined. Roadmap encoding retains its
separate bounded segment capacity. The v1 relative readiness order uses accepted
Priority (Critical, High, Medium, Low), then smaller Effort (XS, S, M, L, XL,
Unknown), then canonical resource identity. Manual/native items without accepted
classification remain eligible, with ranking metadata unavailable; ranked work
precedes unranked work, and unranked work sorts by resource identity.

Labels (including area and risk labels) and creation timestamps do not modify
v1 rank. Numeric weights remain undefined. This order does not reorder the Phase
roadmap or assign Roadmap Order. Parentless Task/Feature are first-class roadmap
roots under #36's dependency/canonical topology, with OPEN root assignments and
CLOSED clears. Their dependencies can induce relative Phase order, but #35 still
receives only Phases as PhaseOrder. See
[the pure policy contract](docs/engineering-lifecycle-policy.md).

---

## 15. Execution Eligibility and Branch Creation

A development branch is a controller-authorized side effect, not a consequence
of merely observing `Status = Ready`.

A normal work item becomes branch-eligible only when all applicable conditions
hold:

```text
Status = Ready
+ no active Blocked-By relationship
+ parent Phase is active, when a parent exists
+ Create-Branch policy resolves to true
```

When eligibility is satisfied, the controller creates the deterministic
development branch and records its provenance, including the branch creation or
base SHA.

The deterministic v1 short branch name is `development/issue-<number>` in the
Issue's configured owning repository. The number is positive base-10 without
leading zeros; the name is at most 37 ASCII bytes. Task, Feature, and explicitly
branch-enabled Phase use the same format. Type and title are not identity inputs;
renaming an Issue cannot rename or replace its authorized branch. One work item
has at most one controller-authorized development branch.

`internal/engineeringbranch` evaluates caller-supplied authoritative current
`engineeringpolicy.Input` and requires #35's `ExecutionEligible = true` before
authorization or an unfinished creation attempt. Observed Project Ready is never
authority. Production supplies complete Snapshot context and validated PhaseOrder;
the root/relevance-scoped resolver alone does not supply complete activation input.
The branch package retains its standalone target eligibility/provenance authority
and does not depend on the ordering package or assemble runtime itself.

Global convergence can authorize another eligible target from the actual durable
trigger. Same-target provenance remains v1; cross-target origin metadata uses a
version-2 envelope in the existing branch authorization/creation v1 namespaces,
binding the actual trigger DeliveryID/resource to exact target branch evidence.
Legacy same-target v1 records remain readable; no namespace or schema migration
was added. Observation of an event never supplies branch eligibility.

Before creation, the controller resolves the configured repository's current
default heads ref and exact commit SHA. It durably reserves that branch/base tuple
before requesting creation at that exact SHA. A separate immutable completion
fact proves a validated successful controller create response. Retries reuse
the reserved name/SHA; completed provenance suppresses all further branch writes.
Authorization alone plus an existing branch is ambiguous and fails closed, even
with an exact SHA match. Remote success followed by missing local completion is
never silently adopted; there is no deletion or destructive recovery.
See [execution.md](docs/execution.md) for the exact two-fact protocol.

A manually created lookalike branch is not equivalent to an automation-created
branch and does not authorize lifecycle progression.

Branch creation alone does not mean work has started.

---

## 16. Ready to In Progress

After branch creation, the controller waits for qualifying development activity
on the authorized branch.

Conceptually:

```text
authorized branch created at SHA A
        ↓
branch head advances to SHA B
        ↓
current eligibility is revalidated
        ↓
Issue → In Progress
```

The push notification is observation. The controller rechecks current
workflow and dependency state before authorizing the transition.

On first authorized development activity:

- the work item becomes `In Progress`
- its parent Phase becomes `In Progress`, when applicable
- Start Date is initialized once
- Assignee initialization requires reliable human/assignability evidence; current
  policy suppresses assignment because that evidence is unavailable

Start Date is not reset by later commits, review rework, or subsequent
transitions.

If the work item is currently blocked, a push does not override the blocker.

Issue #38 implements this as standalone engineeringactivity, with complete current
#35 Input supplied by the caller. It requires validated completed #37 provenance,
requires the push’s immutable Delivery.ReceivedAt to be strictly after the validated
#37 successful-creation provenance CreatedAt, refetches the exact authorized
branch, and proves strict forward ancestry from
its recorded BaseSHA; SHA inequality, branch-name match and observed Project
In Progress never authorize a start. Processing-time existence of completion
alone is insufficient: earlier or equal-time pushes produce no activity facts or
lifecycle writes, including after restart or delayed processing. Every unfinished
attempt re-evaluates #35
ExecutionEligible. Target and applicable parent Status each require their own
Set-Status ownership. A directly implemented Phase is only the target.

Immutable first-activity acceptance precedes external writes; separate completion
follows converged authorized effects. Retries revalidate current eligibility and
compare fresh exact Engineering items to finish only remaining effects. The first
qualifying current head and UTC date of durable Delivery.ReceivedAt never change.
Start Date initializes only when absent and preserves an existing valid value;
it is never cleared. Assignees remain unchanged because available login-only
actor evidence cannot prove human repository assignability. No assignment API is
introduced. See [execution.md](docs/execution.md) for canonical facts and recovery.
The activity package's standalone completion introduced no runtime or PR lifecycle
composition. Production now composes routing, complete activation context, policy,
activity, and desired-state reconciliation; PR lifecycle remains Phase #5.

The read-only ActivityState boundary returns empty state when there is no accepted
first activity, even with completed branch creation. Only validated acceptance
populates WorkItem, original push DeliveryID, and Evidence; matching paired
completion sets Completed. Orphan/corrupt/mismatched facts fail closed without
branch/commit/GitHub mutation work.

Completed activity overlays desired In Progress for OPEN work only. An OPEN Phase
also progresses from any CURRENT direct Task/Feature child's completed activity,
even when that child is CLOSED. Bugs and non-direct descendants do not contribute.
Reparenting moves contribution to the current authoritative parent; no historical
parent was added to activity provenance. The parent's own Set-Status independently
gates its write. CLOSED resources receive no Status reconciliation.

Accepted unfinished activity supplies no overlay. Production resumes it only when
the target has a current lifecycle Decision, using the original accepted push
DeliveryID and all unfinished activity checks. CLOSED/no-Decision acceptance stays
durable without Handle or fabricated completion; roadmap clearing continues.

---

## 17. In-Progress Board Grouping

An active Phase appears directly above its active work.

For a child Issue without an implementation PR:

```text
In Progress

Phase #45
Issue #48
```

If multiple children of the same Phase are concurrently active, they remain
contiguous under the Phase in canonical resource identity order. Child dependency
edges affect lifecycle/execution eligibility, not sibling Roadmap Order.

Cross-repository children remain grouped under their parent Phase.

Ready children not yet started remain in the Ready column while preserving the
parent Phase roadmap anchor described earlier.

---

## 18. Pull Request Binding and Onboarding

Read-only binding validation is owned by `internal/prbinding` and consumed by
production Engineering PR routing through `internal/pronboarding`. Accepted
onboarding, review/rework and #48 pure companion ordering are implemented;
production companion integration and relationships remain deferred. #50 separately
implements accepted completion and terminal synchronization (Sections 22–27).
Production does not yet write PR companion numbers.

A Pull Request does not become valid workflow implementation work merely by
being opened or manually moved on the Project board.

A normal implementation Pull Request must declare:

```text
Target: <issue-ref>
```

The implemented binding validation is conjunctive:

```text
valid current Target
+ eligible current Issue/Phase
+ exact authorized development branch
+ validated durable branch creation completion
+ matching current PR head repository/ref/SHA
+ verified active organization membership
+ verified effective repository WRITE-or-higher permission
+ matching current assignee, when applicable
= accepted implementation PR binding
```

`Target:` uses the existing strict `intent.Parse` directive contract. Resolution
uses configured repository identity and current GitHub Issue identity. Complete
Engineering Snapshot, roadmap ordering, and lifecycle policy determine current
execution eligibility for Task, Feature, and explicitly branch-enabled Phase
work. Bug Tracker binding is not authorized by this standalone validator.

Validated successful #37 creation completion is required through
`engineeringbranch.Controller.Completed`, for both same-target v1 and
cross-target origin v2 provenance. A matching branch name or SHA alone is
insufficient. Native Development links, labels, observed Project Status, webhook
payloads, and assignee data cannot independently authorize binding.

The current PR author must be a verified human with matching stable GitHub
identity, active configured-organization membership, and effective `write` or
`admin` permission on the exact Target repository. Membership and permission are
separate mandatory conditions; outside collaborators do not qualify through write
permission alone. With manual assignees, the author must match one of them; without
assignees, membership and write permission suffice. GitHub's effective base
permission is used, never an independent grant from `role_name`.

This neither assigns developers nor changes first-development-activity ownership.
The author need not be the person who triggered branch creation; #37 provenance
contains no invented author identity. Acceptance is a current validation result,
not a permanent grant. Fresh revalidation detects observable drift but provides
no atomic GitHub snapshot or same-ref/same-SHA branch recreation detection.
See [PR binding](docs/pr-binding.md) for the API, reasons, and verification contract.

### 18.1 PR Project and relationship projection

The implemented Engineering onboarding contract is:

```text
fresh accepted PR binding
  → validate exact PR / Target / Project identity
  → ensure managed Engineering membership
  → initialize owned In Progress Status
  → normalize initially non-Draft PR to Draft once
  → record durable onboarding completion
```

The controller requires an OPEN PR, exact configured identity, and consistent
current membership evidence. Duplicate/archived Engineering items or Bug Tracker
membership fail closed. `Set-Status: false` disables normal initial Status
ownership; membership and Draft normalization still require accepted binding and
are independent of Status ownership. Already matching In Progress needs no write.
Initial Status is one-shot, not continuous lifecycle reconciliation.
Before preparation, an owned observed In Review or Done Status fails closed as
`ErrAmbiguous`; it cannot establish initial lifecycle authority. Each initial
Status mutation also requires current ownership, not preparation alone.

Project membership is separately reconcilable after fresh accepted binding,
including after completion, without replaying initial Status or Draft effects.
This is a narrow onboarding controller, not a general Project reconciliation engine.
Durable evidence is historical fact, never a permanent authorization grant.

Standalone `internal/prdevelopment` now projects accepted `Target` authority and
repairs contradictory manual Development links after fresh binding, validated
onboarding and current membership checks. It adds no onboarding or lifecycle
effects. Production-wide invocation, semantic PR labeling and production PR
companion numeric convergence remain deferred. Pure #48 allocation is implemented
separately (Section 19). See [Development relationships](docs/pr-development.md),
[PR binding](docs/pr-binding.md),
[execution](docs/execution.md#accepted-pr-onboarding), and
[persistence](docs/persistence.md#accepted-pr-onboarding-provenance).

### 18.2 Initial Draft normalization

Initial Draft normalization is implemented by `internal/pronboarding`, separate
from the read-only validator. Deterministic onboarding needs no semantic Runner.

PR creation is never treated as an explicit review-start signal.

On first accepted PR onboarding only:

```text
valid Target
+ valid authorized branch
+ valid author policy
+ onboarding not previously completed
        ↓
if PR is not Draft
→ convert to Draft once
        ↓
record onboarding complete after required Status and Draft stages
```

Draft conversion is a one-shot ingress normalization action, not a permanent
invariant.

The controller must not continuously enforce "PR must be Draft" after
onboarding, because doing so would prevent the later Ready-for-review
transition. Explicit subsequent Ready-for-review processing belongs to the
separate review controller described in Sections 20–21.
Persisted attempts without success facts return `ErrAmbiguous`; neither a current
Draft flag nor matching Status proves historical success. No exactly-once GitHub
execution or GitHub/SQLite transactional atomicity is claimed.

---

## 19. Pull Request Roadmap Order Companions

Issue #48 implements and independently tests pure `internal/prcompanion` policy.
It consumes current #36 primary assignments and exact current accepted #46 binding
evidence. OPEN eligible PRs group by exact Target and sort by normalized owner,
repository, numeric PR number, then node identity. Dense offsets +1 through +9
follow that canonical identity order, not webhook chronology or review order:

```text
Target Issue #48 = 10010
Accepted PR A   = 10011
Accepted PR B   = 10012
Accepted PR C   = 10013
```

The tenth eligible PR for one Target fails with typed capacity error and no partial
plan. Targets cannot share namespaces. Removal compacts remaining PRs; Target
rebasing recomputes numbers from the current primary assignment. Historical slots
are not identities. Every primary Issue reserves nine generic companion slots;
PRs are one supported category, without PR-exclusive offsets. Other categories
and shared occupancy arbitration remain deferred.

#36 assigns primary Roadmap Order values for Phase roots, standalone Task/Feature
roots, and direct Phase children. It only reserves the companion namespace and
does not assign or interpret companions. Native Project item position is not the
Engineering ordering mechanism. A CLOSED Phase may retain a segment for OPEN
direct children without receiving an assignment itself; contradictory retained
segment ownership is rejected, without rebuilding #36 topology.

CLOSED/MERGED PRs receive no active assignment. A reopened OPEN PR needs current
accepted evidence and receives a freshly computed position. Rejected or unknown
binding gives no new assignment; an OPEN accepted PR without its exact Target
assignment fails. Draft/review Status, Set-Position, timestamps, observed numbers,
and manual Project position do not determine companion order.

Desired clearing requires explicit caller-authenticated prior field ownership and
terminal PR state or a definitive current denial. For OPEN PRs, unknown binding
and MissingPR, UnresolvedTarget, or Changed rejections do not authorize clearing.
Other supported current rejections still require separate ownership evidence.
Historical ownership Target may differ after retargeting and is not current
authorization. Eligible PRs never receive simultaneous clears; absent ownership
gives no clear and contradictory evidence fails safely. Clear means desired unset
of the owned Engineering Roadmap Order field, not an immediate mutation.

The pure package performs no reads, writes, storage, or lifecycle transitions and
cannot authenticate ownership provenance itself. Production composition, live PR
numeric-field writes/clears, provenance acquisition, drift repair, retries and
webhook/cross-resource orchestration remain deferred. These computed numbers are
not automatically displayed in the live Engineering Project. Exact interface,
validation, determinism and clearing rules are in
[PR companion ordering](docs/pr-companion-ordering.md).

---

## 20. In Progress to In Review

The explicit developer signal for review readiness is GitHub's native Draft to
Ready-for-review transition.

```text
PR Draft
→ developer marks Ready for review
→ controller revalidates binding and eligibility
→ PR → In Review
→ Target Issue → In Review
```

The parent Phase remains `In Progress` while child review is occurring.

A PR being opened as non-Draft does not bypass onboarding because the controller
normalizes the initial accepted PR to Draft once.

Only a later explicit Ready-for-review action starts review workflow.

`internal/prreview` requires fresh accepted #46 binding, validated completed #47
onboarding, exact current PR/Target identity and appropriate current Engineering
membership. The PR must be OPEN and non-Draft. Native timeline evidence must
identify an unambiguous `ReadyForReviewEvent` by the authorized developer strictly
after onboarding completion. Webhook sender and `updated_at` select a candidate;
neither grants authority. Creation and current non-Draft state cannot synthesize
a cycle. Missing correlation, equal transition timestamps, conflicting evidence
and unsupported manual Draft sequences fail closed.

The initial cycle permits no intervening post-onboarding native transition.
Later cycles require the previous cycle's completed rework and its exact native
`ConvertToDraftEvent`, immediately followed by the new readiness transition.
Current authorization is revalidated before persistence and consequential effects.
PR and Target Status writes independently honor their current `Set-Status`;
neither overrides `Set-Status: false`. This review boundary introduces no terminal
exception; the separate #50 consistency exception follows Section 25.
See [review execution](docs/execution.md#accepted-pr-review-and-rework)
and [cycle provenance](docs/persistence.md#pr-review-cycle-provenance).

---

## 21. Review Outcomes and Rework

GitHub native review states drive review outcomes deterministically.

### 21.1 Comment-only review

```text
COMMENTED
→ no workflow transition
```

### 21.2 Approval

```text
APPROVED
→ remain In Review
→ wait for merge
```

Approval alone does not mark the work Done.
Approval initiates no lifecycle transition; an existing valid In Review cycle
remains applicable. It does not close or merge either resource.

### 21.3 Changes requested

```text
CHANGES_REQUESTED
→ controller converts PR to Draft
→ PR → In Progress
→ Target Issue → In Progress
```

This Draft conversion is triggered specifically by the changes-requested review
event. It is not a continuous "In Progress means Draft" reconciliation rule.
The submission must match current native identity, author, state and submission
time, belong to the active cycle, and remain in GitHub's latest-opinion evidence.
Stale, dismissed, superseded or unauthorized evidence cannot trigger rework.
Initial onboarding Draft normalization and this per-cycle rework action have
separate durable provenance. A completed rework remains historical authority
after later review dismissal; dismissal supplies no reversal transition.

Rework preserves:

- original Start Date
- Assignee
- review history
- original authorized Target association
- authorized branch identity

When the developer finishes rework, the developer explicitly marks the PR Ready
for review again:

```text
Draft
→ Ready for review
→ PR + Target Issue → In Review
```

A reviewer can request rework, but a reviewer does not independently return the
work to review-ready state.

Distinct native readiness identities link durable cycles. Duplicate notifications
may converge the applicable cycle's remaining owned Status drift but cannot replay
completed Draft conversion or roll back newer readiness. An attempted Draft effect
without a valid durable receipt is indeterminate and requires manual investigation;
the observed Draft flag cannot establish historical success. GitHub and SQLite
are not atomic, and exactly-once external mutation execution is not guaranteed.

---

## 22. Merge, Completion, and Next Work

Only an accepted implementation PR with durable completed onboarding and a
current authoritatively verified merge supplies controller closure authority.
Historical accepted evidence is checked against fresh exact PR, Target, branch,
author/developer, Type, Project membership and native timeline facts. The terminal
path does not broaden #46’s OPEN-only binding validator. The native merge must
follow completed onboarding; approval or review readiness alone never completes work.

For a normal Task or Feature Target, the successful merge path is:

```text
PR merged
→ PR closed
→ PR Status = Done
→ Target Issue closed
→ Target Issue Status = Done
```

Existing #35/#36 authorities recompute downstream dependency eligibility and
numeric roadmap state from fresh facts. Completion adds no independent ordering
policy. See [execution](docs/execution.md#accepted-pr-completion-and-terminal-synchronization)
and [close provenance](docs/persistence.md#pr-completion-close-provenance).

If closing the target removes the final blocker from the next child in the same
active Phase:

```text
next child already Ready
+ Blocked-By becomes empty
+ branch policy allows creation
        ↓
create authorized development branch
        ↓
remain Ready until first qualifying commit
```

This intentionally prepares the next work item without pretending that work has
started.

The parent Phase remains `In Progress` while open child work remains.

### 22.1 Pull Request closed without merge

Closing an accepted implementation Pull Request without merging it is an
abandoned implementation attempt, not successful completion of its Target.

The canonical recovery path is:

```text
PR closed without merge
→ PR Status = Done
→ Target Issue remains open
→ Target Issue → In Progress where normal Set-Status ownership permits
```

The Target Issue must not become `Done` or close merely because its
implementation PR was closed.

The standalone Development controller preserves existing relationships for CLOSED
or MERGED PRs without adding or removing links. Historical traceability is not
Target completion evidence. Reopened PRs require fresh current binding and lifecycle
prerequisites; an old link cannot authorize fresh writes to a closed Target.
A later replacement PR may bind to the same Target through a valid `Target:`
directive and the authorized development branch policy. It requires its own
accepted authority. Durable Target branch/start provenance is preserved. A current
validated replacement review in In Review takes precedence over abandonment rollback.

Under normal status ownership, the target returns directly to `In Progress`;
it is not reset through `Backlog` or `Ready`.

---

## 23. Phase-Owned Implementation

A Phase may itself carry direct implementation work when explicitly enabled:

```text
Create-Branch: true
```

In that case, an eligible Phase may receive its own authorized development
branch and may be the valid target of a Pull Request:

```text
Target: #45
```

`Phase` is therefore not an invalid PR Target solely because of Issue Type.

Direct Phase implementation does not replace or implicitly complete the Phase's
child work. Its accepted implementation PR merge never automatically closes the
Phase, even when all direct children are CLOSED. It completes the implementation
attempt only; the Phase requires explicit maintainer native Close.

```text
Phase implementation completed
!=
Phase objective completed
```

---

## 24. Phase Closure

Completing all child Issues makes a Phase eligible for closure. It does not
close the Phase automatically.

The normal Phase closure remains an explicit maintainer action, including the
existing close-with-comment practice.

Workflow closure eligibility does not replace the existing Verification
Standard. Implementation, required tests, deterministic expectations,
documentation synchronization, and applicable exit criteria remain required
for legitimate closure. The controller rule below is an additional lifecycle
gate, not a substitute for engineering verification.

The complete authoritative declarative `Parent` graph determines direct children,
including cross-repository children. All direct children must be CLOSED for a
native Phase close to be valid. All-closed children supply eligibility only;
neither their completion nor a direct Phase PR merge initiates Phase closure.

A natively CLOSED Phase with an authoritative OPEN direct child is reopened,
then returned directly to freshly recomputed canonical nonterminal Status.
The typed rejection reason is `open_direct_children`, ready for a later feedback
boundary. Managed user-facing feedback emission is not implemented. Incomplete
or invalid graph/identity evidence fails closed.

An OPEN Phase with Project Done is corrected to canonical nonterminal Status
regardless of child completion eligibility. Project Done never requests Close.
Nested-child lifecycle remains deferred. This mechanical gate does not establish
Verification Standard evidence or replace maintainer verification responsibility.

---

## 25. Close, Done, and Reopen Synchronization

Normal workflow status ownership and repository-native open/closed state are
separate concerns.

`Set-Status` controls ordinary workflow progression and rollback among:

```text
Backlog
Blocked
Ready
In Progress
In Review
```

Repository-native terminal/open state is stronger and must remain synchronized.

The following canonical synchronization events bypass `Set-Status`:

```text
Issue closed
Pull Request closed
Pull Request merged
Issue reopened
Pull Request reopened
```

Therefore:

```text
Set-Status: false
+ Issue closed
→ Status = Done
```

and:

```text
Set-Status: false
+ Item reopened
→ recompute canonical open workflow state
```

Reopen is not equivalent to unconditional `Ready`.

The controller recomputes the correct state from current policy, for example:

```text
blocked Phase
→ Blocked

unblocked Phase
→ Ready

child of active Phase
→ Ready

child of inactive Phase
→ Backlog
```

Repository-native Issue Close is authoritative for manual completion. A validated
accepted PR merge supplies the separate narrow Task/Feature closure authority
in Section 22. Project Done never supplies native closure authority:

```text
valid native Issue CLOSED → Project Done
native Issue OPEN + Project Done → keep Issue OPEN → canonical nonterminal Status
```

This holds after manual Done edits, delayed Project notifications, native reopen,
retry and restart, including `Set-Status: false`. It grants no general ownership
of ordinary Backlog/Blocked/Ready/In Progress/In Review transitions. Ordinary
reconciliation and abandonment rollback retain their Set-Status gates. A Phase
close additionally requires Section 24’s direct-child gate.

Canonical open state reuses #35/#36 policy, validated completed first-activity
provenance and applicable accepted implementation/review/rework evidence from
#47/#49. Reopen never restores a stale Status or unconditionally selects Ready.
Stale pre-close review intervals cannot authorize effects. Fresh supported
post-reopen readiness can enter review when validated completed rework permits
continuation; unsupported or ambiguous review continuation fails closed.

---

## 26. Reconciliation Model

The controller is a desired-state reconciler.

```text
event
→ fetch actual state
→ parse directives
→ obtain optional semantic classification
→ compute desired state
→ compare actual / desired
→ reconcile controller-owned differences
```

It does not use a "reset to Backlog and replay" strategy.

If an item is manually placed in an invalid state, it moves directly to its
canonical state. The examples below concern work without completed first-activity
evidence; completed activity supplies the In Progress overlay described above.

Examples:

```text
blocked child manually moved to In Progress
→ direct correction to Ready or Backlog according to parent policy
```

```text
blocked Phase manually moved to In Progress
→ direct correction to Blocked
```

```text
child manually moved to Ready while parent Phase is inactive
→ direct correction to Backlog
```

The controller only reconciles fields it owns. Set-Status gates normal Status
writes; numeric Roadmap Order remains an unconditional controller-owned invariant,
independent of Status and Set-Position.

Terminal consistency uses fresh primary/Project facts before ordinary Engineering
convergence. Changed authoritative state requires fresh recomputation; no snapshot
survives a native mutation. Section 25 defines terminal Status exceptions;
downstream eligibility and ordering remain #35/#36-owned.

### 26.1 Implemented Engineering reconciliation

Applicable durable review state overlays the exact Target's normal Status after
the pure lifecycle calculation. The reconciler rechecks Snapshot and Project
evidence after review reads. The earlier first-development-activity Status path
also checks review authority: it suppresses In Progress writes during active
In Review and rechecks current evidence before retained writes. New native
readiness supersedes older convergence without itself authorizing a new cycle;
both paths suppress older Status effects until explicit readiness is accepted.
These are current-read barriers, not transactional snapshot isolation.
The parent Phase retains its own lifecycle policy and does not inherit child
In Review. Start Date and assignees retain their separate activity ownership.

Production uses the separate `engineeringcontext.ResolveSnapshot` boundary:

```text
E1: enumerate every configured repository (OPEN and CLOSED Issues, no PRs)
  ↓ materialize every E1 Issue from current primary state and resolve Type
E2: enumerate every configured repository again
  ↓ compare complete membership and relevant facts
stable complete Snapshot, or ErrChanged (no partial result)
```

Membership, node identity, exact body, OPEN/CLOSED state, and native Type evidence
when fallback supplies authority participate in the barrier. Appearance of accepted
semantic completion for a native-fallback Issue also causes ErrChanged. This is
not transactional GitHub snapshot isolation. Bugs remain graph/dependency facts;
Project Status/membership and native relationships confer no graph authority.
Snapshot.Context canonicalizes semantic root identity, ignores caller NodeID,
uses current Snapshot evidence, deep-copies nested mutable values, and retains
the COMPLETE graph with only Root changed. Resolve(root) remains relevance-scoped.

```text
complete Snapshot → complete Context
  → BuildRoadmapOrder (#36) → Evaluate (#35) → Evaluate (#36)
  → fresh Project comparison → authorized field reconciliation
```

The reconciler refetches the complete write-target union and checks primary facts
against Snapshot.CheckCurrent before drift writes. Exact Engineering membership,
nonarchived item identity, and field evidence are required; it does not guess or
create membership. The numeric capability validates exact Project/item/NUMBER
field identity and verifies current set/unset state after writing, with pagination,
finite-value checks, exact float equality, typed errors, cancellation, and no
hidden retry or arbitrary GraphQL mutation surface.

After field convergence succeeds, ANOTHER complete Snapshot and the FULL
BuildRoadmapOrder → lifecycle Evaluate → numeric Evaluate pipeline run before
any EnsureFrom fan-out. Fresh blockers, Phase activation, Create-Branch directives,
hierarchy, segment capacity, and direct-child capacity can stop authorization
before branch calls. Every target uses the SAME fresh Snapshot and validated
fresh PhaseOrder; the branch package independently reevaluates exact eligibility.

Canonical configured development push notifications and bounded Engineering
Project-item notifications are production-routed. Supported Project actions are
created, edited, reordered, archived, and restored. Current item/content/Issue
identity is refetched; payload Status/Roadmap fields never enter authority.
Unsupported deleted actions, unrelated Projects, PR/DraftIssue/no content, and
unavailable items become non-applicable as defined by routing. Ordinary/unrelated
pushes are non-applicable; malformed canonical development candidates fail closed.
Payload before/after SHAs are not activity authority. Worker.ErrNotApplicable
settles Completed without fake binding, Processor call, or failure category and
continues later eligible work in the same Step, preserving cancellation/recovery.

Failure → retry → fresh complete Snapshot → fresh Project observations → new
desired state → only remaining/current drift. Successful effects become no-ops
when current state proves convergence; changed topology wins over the old plan.
There is no reconciliation batch provenance, completion record, replay log, or
SQLite migration. Branch/activity retain one-shot provenance because observation
alone cannot safely reconstruct their authority. Runtime Engineering-local
changed/incomplete categories retry; generic activity/branch storage failures and
structural/policy/provenance/Project/configuration corruption are bounded terminal
failures. Typed GitHub transient/rate-limit behavior remains separate.

Standalone #51 Development reconciliation uses its own process-local lane and
two fresh authority/state barriers before each effect, then reassesses from new
observations. Contradictory manual removals precede a missing canonical add.
Partial failures return to the caller; retry skips effects already converged.
This is bounded recovery, not an atomic snapshot or distributed lock. No production
event routing or dedicated worker error mapping is composed for this controller.
See [execution](docs/execution.md#standalone-development-relationship-reconciliation).

---

## 27. One-Shot Actions vs Continuous Invariants

The implementation must distinguish initialization/transition side effects from
continuous desired-state invariants.

Examples of one-shot actions:

- initial PR Draft normalization
- authorized branch creation
- Start Date initialization
- changes-requested Draft conversion

Examples of continuous invariants:

- valid Project membership
- normal lifecycle eligibility
- canonical Engineering Roadmap Order assignments and clears
- Target / Development consistency
- Parent/dependency relationship consistency where controller-owned
- Phase closure constraints

A one-shot action must not be repeatedly replayed merely because its resulting
state later changes legitimately.
Implemented PR onboarding uses its own initialization stage facts; review uses
per-cycle start, Draft-attempt and Draft-completion facts. Status remains separately
convergent while the relevant cycle and fresh authorization remain applicable.
See [persistence](docs/persistence.md#pr-review-cycle-provenance) for validation
and ambiguity boundaries. Relationship invariants remain future contracts where
identified as deferred.
Accepted merge-close uses separate identity-bound attempt/receipt provenance.
Validated completion prevents close replay after explicit native reopen; an
attempt without a valid receipt fails closed as indeterminate, even when the
Issue is observed CLOSED. See [close provenance](docs/persistence.md#pr-completion-close-provenance).

---

## 28. Controller-Owned UI Projections

The GitHub UI is not authoritative for controller-owned relationships or
workflow fields.

For example:

```text
Target: #48
→ canonical intent

GitHub Development relationship
→ projected state
```

If a user manually adds an unrelated Development Issue through the GitHub UI,
the standalone controller can remove proven contradictory manual drift and restore
the declared Target projection only while current accepted binding, validated
onboarding, exact managed membership and unambiguous endpoint evidence authorize
it. Keyword-derived references cannot be removed; conflicting evidence fails
closed. Production does not yet invoke this correction automatically on every
manual change.

The same principle applies to other controller-owned relationships.

This does not make all GitHub UI state controller-owned. Human discussion and
review content remain human-owned.

Typical controller-owned surfaces include:

- Project Status, when `Set-Status` permits normal ownership
- Engineering Roadmap Order, independent of `Set-Position`
- workflow-managed Project membership
- Development relationship derived from `Target`
- Parent/dependency relationships derived from explicit directives
- automation-created branch provenance

Typical human-owned surfaces include:

- Issue/PR prose
- comments
- review text
- normal discussion

---

## 29. Rejected Intent and Managed Feedback

Routine successful reconciliation should be quiet.

When a human action is rejected because it violates workflow policy, the
controller should explain:

1. what was reverted or corrected
2. why it was invalid
3. what the user must do to make the action valid

Examples include:

- missing PR Target
- wrong PR branch
- invalid PR author binding
- blocked item manually moved to In Progress
- child advanced while parent Phase is inactive
- invalid Phase closure with open children
- manually altered Development relationship

The controller should avoid comment spam by maintaining one managed automation
feedback comment per resource.

Repeated equivalent violations should reuse or update the existing managed
comment rather than creating a new comment for every attempt.

A new violation may update the same comment with the current constraint set.

Routine housekeeping corrections such as normal item reordering need not
produce user-facing comments.

The general rule is:

```text
normal reconciliation
→ silent

rejected user intent
→ explain and deduplicate
```

---

## 30. GitHub Built-In Workflow Boundary

Most lifecycle automation belongs to the Go controller so there is a single
version-controlled workflow authority.

The controller should own:

- item added to Project → canonical ingress / Backlog initialization
- sub-issue added → correct Project membership
- item closed → Done synchronization
- accepted verified Task/Feature PR merge → authorized native Target Close
- native-open Issue with Project Done → canonical nonterminal Status; preserve OPEN
- item reopened → canonical open-state recomputation

GitHub's built-in completed-item archival may remain enabled:

```text
closed/completed item
→ after 14 days
→ Archive item
```

Archival is housekeeping rather than engineering lifecycle policy and does not
need to become controller-owned unless future requirements justify it.

---

## 31. Security and Credentials

The LLM must not receive a GitHub credential.

The Go service fetches selected GitHub context, sends only the required content
to the semantic model, validates the structured response, and performs all
GitHub mutations itself.

A prompt instruction is not a security boundary.

For production, GitHub authentication should use a GitHub App with narrowly
scoped repository and organization Project permissions.

A prototype may use the authenticated `gh` CLI, but production transport should
remain replaceable behind an internal GitHub client boundary.

---

## 32. Failure and Recovery Principles

The controller must be safe under duplicate deliveries, restarts, partial
failure, and temporary external API failure.

Required properties include:

- webhook deduplication
- idempotent reconciliation
- durable retry state
- per-resource event serialization
- current-state re-fetch before consequential mutation
- one-shot side-effect provenance
- no lifecycle authority delegated to LLM output

If classification fails, the controller should fail closed for semantic
mutation rather than inventing metadata. Existing deterministic workflow state
must remain usable.

---

## 33. Deferred Decisions

The following are intentionally not defined by this overview:

- exact numeric readiness-score weights
- future heavy-agent implementation/provider selection
- advanced security-alert workflow behavior

These decisions can be specified independently without changing the authority
and lifecycle model defined here.

---

## 34. End-to-End Example

The flow below combines implemented branch/activity, PR onboarding and review/rework
and accepted completion/terminal synchronization with standalone Development
projection and planned production companion numeric convergence. Development is
not yet production-routed; planned steps are marked:

```text
Phase #45 unblocked
→ Phase #45 Ready
→ children #48..#54 Ready

#48 unblocked
→ controller creates authorized #48 branch
→ #48 remains Ready

first qualifying commit on #48 branch
→ #48 In Progress
→ Phase #45 In Progress
→ Start Date initialized

PR #80 opened
Target: #48
→ Target + branch + author validated
→ PR added to Project
→ PR normalized to Draft once
→ durable onboarding completion recorded
→ Development relationship projected (standalone capability; production routing deferred)
→ PR Roadmap Order converges after Target within +1..+9 companions (planned production integration)

PR #80 Draft → Ready for review
→ PR #80 In Review
→ #48 In Review
→ Phase #45 remains In Progress

review CHANGES_REQUESTED
→ PR #80 Draft
→ PR #80 In Progress
→ #48 In Progress

PR #80 Draft → Ready for review
→ PR #80 In Review
→ #48 In Review

accepted PR #80 authoritatively verified merged
→ PR #80 Done
→ #48 closed + Done

#49 blocker now resolved
→ controller creates authorized #49 branch
→ #49 remains Ready
→ waits for first qualifying commit

...

all authoritative direct Phase #45 children closed
→ Phase #45 becomes eligible for explicit maintainer closure
→ maintainer closes Phase with comment
→ Phase #45 Done
```

This sequence captures the intended separation between readiness, execution,
review, completion, and Phase-level objective closure.

---

## 35. Summary

```text
GitHub Issues / Project
        ↓ observable state and human intent

Go workflow controller
        ↓ authority, policy, reconciliation, mutations

Cheap semantic LLM
        ↓ bounded classification and relative estimation

SQLite
        ↓ durable event processing and one-shot provenance
```

The workflow is intentionally deterministic wherever deterministic policy is
possible.

The LLM is used where free-form engineering text must be interpreted, not where
workflow state can be derived from explicit relationships, GitHub-native events,
or stable controller rules.

Phases define bounded engineering objectives and roadmap structure. Children
inherit Phase workflow membership and Roadmap Order anchors. Root dependencies
determine roadmap-root order; dependencies also determine execution eligibility.
Authorized branch activity starts work. Draft/Ready
transitions start review. Changes requested returns work to implementation.
Merge completes normal child work. Phase closure remains an explicit maintainer
action after all child work is closed.

The result is a declarative, reconciling workflow in which GitHub remains the
visible collaboration surface while lifecycle authority remains centralized,
version-controlled, and testable.
