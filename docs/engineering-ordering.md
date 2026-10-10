# Engineering roadmap and desired position policy

Issue #36 implements `internal/engineeringorder`. It computes deterministic
PhaseOrder and desired Engineering Phase/child position policy. Actual GitHub
Project position convergence remains #39. Neither policy stage performs reads,
mutations, model execution, persistence, filesystem access, retries, clock reads,
or random selection. App, worker, webhook, semantic composition, and the default
CLI do not invoke this package.

The dependency direction is `engineeringorder → engineeringpolicy →
engineeringcontext`. Engineeringpolicy remains unaware of engineeringorder.
Its validation-only `Validate(Input) error` exposes the existing #35 input
contract without producing a lifecycle Plan or changing lifecycle behavior.

## Supported Project order observation

`github.ProjectOrderReader.ListProjectItemsInOrder(ctx, projectID)` is a separate
read-only capability implemented by `github.Transport`. Client and Mutator are
unchanged. ProjectOrderFake fails unconfigured calls with Permanent. The reader
reuses the existing GraphQL, token source, HTTP limits, context, and typed errors;
it logs nothing and performs no retries.

The supported [GitHub Project GraphQL contract](https://docs.github.com/en/graphql/reference/projects#projectv2)
exposes `ProjectV2.items(orderBy: ProjectV2ItemOrder)` and
[ProjectV2ItemOrderField.POSITION](https://docs.github.com/en/graphql/reference/projects#projectv2itemorderfield).
The reader explicitly selects `orderBy:{field:POSITION,direction:ASC}`. This is
Project item position, corresponding to
[updateProjectV2ItemPosition](https://docs.github.com/en/graphql/reference/projects#updateprojectv2itemposition),
whose return value is the reordered item connection and whose `afterId` input
positions an item after another item (null/omitted means top). No undocumented
default connection order or field-value sorting is assumed.

Verified on 2026-10-10 against official documentation and read-only live schema
introspection: POSITION is supported and items accepts orderBy and archivedStates.
A live read of the Engineering Project also accepted the explicit POSITION ASC
query with both archive states and returned cursor metadata. No mutation was used
to verify ordering. View-specific sorting is not this global position contract.
The inherited anchor needs only global sequence filtering, no view API behavior.

Pages contain up to 100 items and are appended exactly as returned until
hasNextPage is false. No sort or canonical identity normalization changes that
sequence. Missing page metadata/nodes, null item nodes, missing continuation
cursors, cursor cycles, duplicate item IDs, duplicate non-null content node IDs,
and duplicate Issue owner/repository/number identities fail with Malformed,
without partial output. Issue identity uses the existing canonical GitHub
Identity, including repository node ID. Project identity is verified both on
the queried node and every item. A missing Project is NotFound; blank requested
Project IDs are Permanent. Other existing typed GitHub errors propagate.

OrderedProjectItem retains item ID, Project ID, archive flag, Project item type,
content typename/node ID, and optional Issue Identity. PRs and DraftIssues are
preserved with no Issue conversion. Null/deleted/redacted content retains the
item type and sequence slot without a content identity. Non-Issue opaque content
remains uncontrolled. Archived items are read and preserved but never supply a
manual rank. Reads across pages are not transactionally atomic; duplicates
reject the attempt, and #39 must obtain fresh evidence before moving items.

Normal `observe.ProjectState.Items` remains a resource membership/field facts
collection sorted by stable item ID. It is **not** position evidence. The new
Project-wide sequence is a different semantic surface: order itself is data.
A caller maps only Issue identities to canonical `storage.Resource` values,
preserving exact node evidence and lowercase owner/repository; all other items
map to nil Resource while retaining their ProjectItemID and archive flag.
The pure package needs no GitHub transport types.

## Stage 1: PhaseOrder

```go
phaseOrder, err := engineeringorder.BuildPhaseOrder(context, currentOrder)
lifecycle, err := engineeringpolicy.Evaluate(engineeringpolicy.Input{
    Context: context,
    PhaseOrder: phaseOrder,
})
```

BuildPhaseOrder does not need a lifecycle Plan. It returns every Context Phase
exactly once, including CLOSED Phases, with exact Context resource evidence.
Task, Feature, Bug, PR, and opaque items never enter PhaseOrder.

Iterative stable Kahn traversal uses only Phase → Phase dependency edges:
blocker precedes blocked, including closed endpoints and cross-repository edges.
Parent edges and non-Phase dependency edges do not impose Phase order.
For simultaneously zero-indegree candidates, current non-archived Project rank
wins; ranked candidates precede unranked candidates. Unranked ties use canonical
owner, repository, Issue number, then node evidence. Dependencies override a
conflicting manual order without rejecting the snapshot merely for that conflict.
A missing/archived position is valid and uses fallback; no numeric GitHub position
is fabricated. Only otherwise-free choices preserve manual order.

CurrentOrder requires unique Project item IDs and unique non-null Issue semantic
identities/node IDs, including archived membership. An archived duplicate plus
an active item is rejected too; neither is chosen arbitrarily. Unrelated Issues
are allowed as landmarks, but a relevant identity or node ID must match Context
exactly. Opaque slots do not become lifecycle inputs. Current input is not mutated.

## Stage 2: desired Phase segments

```go
positions, err := engineeringorder.Evaluate(engineeringorder.Input{
    Context: context,
    Lifecycle: lifecycle,
    PhaseOrder: phaseOrder,
    CurrentOrder: currentOrder,
})
```

Evaluate validates and consumes the supplied PhaseOrder unchanged; it does not
compute a replacement order from a later manual snapshot. Plan contains copied
PhaseOrder and a globally ordered DesiredOrder of Placement values. Each
Placement contains Resource, optional AnchorPhase, and PositionOwned. There are
no mutation commands, pseudo-positions, or canonical maps.

Each Phase supplies one segment in PhaseOrder order: the OPEN Phase placement,
then its OPEN Task/Feature children. The authoritative source is #34's Parent
edges, never native Parent/SubIssues. A Phase with Parent remains a Phase anchor,
not a child placement. Cross-repository children use precisely the same rules.
Within siblings, preserve current non-archived manual order; ranked children
precede missing-position children, then canonical resource identity breaks ties.
Child dependency edges govern #35 execution eligibility, not sibling visual order.
Priority, Effort, labels, titles, blockers, and webhook order do not rank siblings.
Bug children receive no Engineering placement.

All segments are contiguous in the desired controlled sequence. A later Phase
cannot interleave into an earlier Phase's children. This applies independently
of lifecycle Status/activation: filtering `Phase #45, child #48, child #49,
Phase #60, child #61` to omit #45 and started #48 leaves `#49, #60, #61`.
AnchorPhase is explicit, and the child stays in the first Phase's roadmap slot.
This proves the property needed after #38 without implementing In Progress
transitions or inventing view-specific rules.

PhaseOrder topology differs from actionable positioning: CLOSED Phases remain
in topology but have no DesiredOrder placement; CLOSED children are excluded too.
OPEN children of a CLOSED Phase retain that Phase's segment/AnchorPhase while
#35 independently keeps them Backlog. No Done policy is inferred.

Parentless Task/Feature work stays outside DesiredOrder. #35's ParentlessReady
relative ranking remains unchanged and available to the caller. It never
interleaves into Phase segments. Uncontrolled items are not assigned positions
or direct moves merely to satisfy grouping. DesiredOrder describes the controlled
subsequence; #39 must compare it with the fresh full Project sequence.

## Ownership, validation, and determinism

PositionOwned equals `node.Intent.Effective().SetPosition`. Default and explicit
true yield true; false yields false. Canonical PhaseOrder, DesiredOrder resources,
Parent grouping, anchors, and lifecycle eligibility are unchanged. Automation
false does not suppress deterministic Set-Position semantics. Position is
relational: moving an owned neighbor may change an unowned item's apparent index.
This package adds no fixed-index semantics or mutation scheduling policy.

Shared #35 validation checks Context root, exact unique resources/node evidence,
Type/source/classification/state, declared Parent endpoints, graph cycles, and
complete dependency-consistent PhaseOrder. Local validation adds Project sequence
identity/duplicate checks and exact complete unique lifecycle decisions, supported
Status values, child parent evidence, and valid unique ParentlessReady identities.
Bugs/closed/unknown resources cannot be injected as lifecycle decisions.

Local `*engineeringorder.Error` supports errors.Is through ErrInvalid,
ErrProjectOrder, ErrPhaseOrder, and ErrPlacement. Errors contain only stable local
categories, never Issue content/provider data. Failures return no partial Plan.
Slices and anchor pointers are caller-owned. Equivalent semantic Context input
produces byte-identical JSON; CurrentOrder is preserved semantic input, not
shuffled transport noise.

Tests cover one/multi-page exact unsorted reads and failures, topology chains
(including 1,000 Phases), diamonds, cross-repository edges, closed topology,
manual/fallback ordering, direct #35 compatibility, grouping, ownership, and
malformed inputs. A pure engine #45/#48/#49, #60/#61 and freecad #70/#71 fixture
adds cross-repository child #72, a missing child position, Set-Position false,
manual/native and accepted Types, and parentless Ready work. Twenty evaluations
shuffle nodes/Parent/dependency edges while holding Project order fixed and
compare encoded bytes, including the inherited-anchor filtering proof.

## Boundary left to #39

#39 owns production composition, fresh reads before mutation, current/desired
comparison, minimal moves with afterId, ownership-aware scheduling around
uncontrolled/unowned items, partial-failure retry, and drift convergence.
#36 adds no position mutation to Mutator, Status mutation, storage/schema,
branch/PR/Bug Tracker behavior, CLI flags, external dependencies, or Nix changes.
Project position reconciliation is not live.
