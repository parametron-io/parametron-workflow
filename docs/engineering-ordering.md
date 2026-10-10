# Engineering Roadmap Order

Issue #36 implements pure `internal/engineeringorder` policy. It computes full
RoadmapRoots, its PhaseOrder projection, and numeric assignments/clears. Actual
GitHub numeric-field reconciliation remains #39; this policy is not composed
into app, worker, webhook, semanticflow, semanticreconcile, or CLI execution.

The dependency direction remains `engineeringorder → engineeringpolicy →
engineeringcontext`. Engineeringpolicy is unaware of engineeringorder. Its
validation-only Validate entry point shares #35's existing Context/PhaseOrder
contract without computing lifecycle decisions.

## Derived field and presentation

Engineering requires the numeric custom Project field **Roadmap Order**, bound
through `config.RoadmapOrder` (`roadmap_order`). A human administrator configures
the Engineering view to sort by **Roadmap Order ASC**. GitHub's current Projects
UI displays the sort field on cards while that sorting is active; this
presentation behavior is not workflow authority. Bug Tracker instead requires
**Priority Score** and continues to sort **Priority Score DESC**; Roadmap Order
is not a Bug Tracker field role.

Roadmap Order is controller-owned, deterministic, recomputable derived state.
It is mutable current-roadmap placement, never an identifier, historical slot,
branch identity, provenance, or stable external reference. Completing earlier
work can rebase a Phase from 11000 to 10000.

Native GitHub card position, manual drag/drop, Project view order, and observed
Roadmap Order values supply no roadmap policy authority or stability signal.
There is no ProjectOrderReader, CurrentOrder, opaque-position landmark, or
native-position reconciliation target in #36. Normal resource Project membership
observation remains sorted by stable item IDs. Roadmap Order is read through the
existing configured-number field-value mechanism, for future comparison in #39.
An absent field remains unset; a present numeric zero remains explicit zero.

## Stage 1: two-level roadmap topology

```go
order, err := engineeringorder.BuildRoadmapOrder(context)
lifecycle, err := engineeringpolicy.Evaluate(engineeringpolicy.Input{
    Context: context,
    PhaseOrder: order.PhaseOrder,
})
```

Roadmap root = Phase OR Task without authoritative Parent OR Feature without
authoritative Parent. Phase-internal members are direct Task/Feature children
whose #34 Parent points to a Phase. A Phase with Parent remains a root.

BuildRoadmapOrder requires neither lifecycle Plan nor Project input. Its Order
contains complete RoadmapRoots, including CLOSED roots as topology evidence, and
PhaseOrder containing every Phase exactly once. One iterative stable Kahn
traversal orders roots using explicit root-to-root blocker → blocked edges,
including Phase → standalone Task → Phase and standalone Task → Feature chains.
PhaseOrder is a Phase-only filter of that traversal, never a separate sort.
Standalone resources never enter #35's PhaseOrder; their dependencies may induce
relative Phase order transitively. Exact resource/node evidence is preserved.

For simultaneously eligible roots, canonical owner, repository, Issue number,
then node evidence breaks ties. Dependencies override fallback. Status, Priority,
Effort, labels, titles, timestamps, Project position, webhook order, and map
iteration never determine root order. Parent membership defines the two levels;
it is not a dependency edge. Bugs remain lifecycle blockers, not roadmap roots.

A root cannot depend on, or block, another Phase's owned Task/Feature child as a
cross-level roadmap relation. A standalone Task/Feature root is likewise unrelated
to every Phase-owned child for this check. Such intent fails ErrRoadmapHierarchy,
including CLOSED endpoints. The controller never promotes child B1 → Phase A
into Phase B → Phase A; authors must declare the intended root dependency.
Child-to-child dependencies (including cross-Phase ones), and dependencies between
a Phase and its own direct child, remain legitimate lifecycle facts. They are
not root ordering edges and do not reorder segments or siblings. #35 is unchanged.

#35 consumes this sequence directly without conversion or re-resolution. It owns
Backlog/Blocked/Ready, active Phase selection, child Ready membership, execution
eligibility, Create-Branch, and Set-Status. #36 does not take those authorities.

## Stage 2: current numeric roadmap

```go
plan, err := engineeringorder.Evaluate(engineeringorder.Input{
    Context: context,
    Lifecycle: lifecycle,
    Order: order,
})
```

Evaluate validates and consumes Order unchanged: roots are exact, complete,
unique, and dependency-consistent; PhaseOrder must equal their Phase-only
projection and satisfy #35's contract. This prevents contradictory ordering
inputs without a second topology algorithm. Plan embeds Order and contains:

- RoadmapRoots: full top-level topology, including completed roots.
- PhaseOrder: the Phase-only subsequence consumed directly by #35.
- Assignments: Resource, integer RoadmapOrder, optional child AnchorPhase;
  ordered by ascending numeric value.
- Clear: canonical unset for CLOSED roots and authoritative direct Task/Feature
  children, ordered by canonical resource identity.

There are no Project item IDs, native positions, mutation commands, ownership
flags, or canonical maps. Slices and anchor pointers are caller-owned.

A standalone Task/Feature segment is retained while its root is OPEN. A Phase
segment is retained when its Phase is OPEN **or** at least one authoritative
direct Task/Feature child is OPEN. A CLOSED Phase with OPEN children retains its
segment base without receiving an assignment itself; its children retain their
anchor and values. This handles transitional terminal mismatches without
inferring Done, reopening work, or changing #35 lifecycle policy.

## Fixed five-digit v1 encoding

```text
FIRST_ROADMAP_BASE  = 10000
ROADMAP_STRIDE      = 1000
DIRECT_CHILD_STRIDE = 10

rootBase = 10000 + (retainedSegmentIndex * 1000)
```

The exported Go constants are FirstRoadmapBase, RoadmapStride, DirectChildStride,
MaxRoadmapSegments, and MaxDirectChildren. Retained indices start at zero:

```text
first root      10000
second root     11000
third root      12000
...
ninetieth root   99000
```

Every OPEN root gets its base with AnchorPhase nil. A parentless Task/Feature
consumes a full segment and has no +10 descendants in v1. OPEN Phase children
are sorted by canonical resource
identity and receive `base + (childIndex + 1) * 10`:

```text
Phase          10000
child 1        10010
child 2        10020
...
child 99       10990
```

Every primary roadmap Issue slot reserves its final decimal offsets +1..+9
as a generic companion namespace:

```text
10000          Phase/root Issue
10001..10009   reserved companions of 10000
10010          direct child Issue
10011..10019   reserved companions of 10010
10020          next direct child Issue
10021..10029   reserved companions of 10020
```

A future companion may be a PR, nested child Issue, additional PR, or another
explicitly defined related workflow item. No offset has a designated companion
type, and there is no single-PR assumption. #36 neither assigns nor interprets
companion slots. Primary Issue assignments always end in zero. This reservation
does not implement nested lifecycle or relax #35's direct Phase-parent validation.

V1 capacity is exactly **90 retained top-level segments** across mixed Phases,
standalone Tasks, and standalone Features; **99 OPEN direct children per Phase
segment**; and **nine reserved companion slots per roadmap Issue slot**
(`MaxCompanionsPerIssue`).
All assigned values remain 10000..99999; the maximum current direct-child value
is 99990. Excess capacity fails closed with ErrRoadmapCapacity and no partial
Plan. Spacing never adapts dynamically. CLOSED topology/children do not consume
numeric capacity.

## Compaction, grouping, and inherited anchor

Only #34's authoritative Parent edges define direct Task/Feature children.
Cross-repository children use the same canonical ordering. Native SubIssues is
not input. Phase-with-Parent remains a Phase, not a direct child assignment.
Bugs never receive Engineering assignments or clears. Child dependencies remain
#35 execution facts and do not affect visual numeric order.

Retained segments are dense. Starting with A=10000, B=11000, C=12000, completing
A and all A's direct work produces B=10000, C=11000. Completing B and its work
then produces C=10000. Full topology can still contain A, B, C. Completed roots
and direct children appear in Clear so #39 can clear stale field values.

Standalone roots compact identically. Phase A=10000, Task X=11000, Phase B=12000
becomes Phase A=10000, Phase B=11000 when X closes, with X in Clear. Giving X an
authoritative Parent A instead moves it into A's +10 child namespace and removes
its root segment. The entire retained roadmap, not a Phase-only list, compacts.

OPEN siblings compact too: Phase=10000, A1=10010, A2=10020 becomes Phase=10000,
A2=10010 when A1 closes; A1 appears in Clear. No historical slot is preserved.

AnchorPhase is explicit for each direct child and independent of workflow Status:

```text
Phase #45 10000
Task #48  10010
Task #49  10020
Phase #60 11000
Task #61  11010
```

Filtering a view to omit #45 and started #48 leaves #49=10020 ahead of
#60=11000. OPEN started work retains its values. A blocked child keeps its number
while #35 separately denies execution eligibility. No view-specific API behavior
or In Progress transition is implemented here.

## Ownership and standalone work

Set-Position remains parsed unchanged for grammar compatibility. Default, true,
and false all produce identical Engineering PhaseOrder and Roadmap Order.
The derived field is a controller-owned invariant, like Bug Tracker Priority
Score; there is no PositionOwned or replacement opt-out directive. Automation
false likewise does not disable topology, grouping, values, or compaction.

Parentless Task/Feature are first-class roots with controller-owned Roadmap Order.
Their root dependencies affect topology; their CLOSED values enter Clear. #35's
ParentlessReady Priority/Effort ranking is a separate lifecycle-relative list and
does not determine numeric root slots. Ready and Blocked roots use the same
dependency/canonical traversal without Status-based heuristics.

## Validation and tests

Shared #35 validation checks root, exact unique identities/node evidence,
Type/source/classification/state, Parent endpoints/intent, cycles, and complete
PhaseOrder with Phase dependency precedence. Projection also validates complete,
unique, exact lifecycle decisions, supported Status values, parent evidence, and
ParentlessReady resources. Nested OPEN Task/Feature parents remain rejected.
Errors contain only stable local categories: ErrInvalid, ErrPhaseOrder,
ErrRoadmap, ErrRoadmapHierarchy, ErrRoadmapCapacity. Failures return no partial Plan.

Tests cover topology chains/diamonds/cross-repository edges, direct #35
compatibility, mixed-root dependencies and Phase-only projection, cross-level
rejection without promotion, five-digit values/reservations, mixed 90/91 segments,
99/100 children, standalone clearing/regrouping,
compaction, clearing, closed-parent/open-child retention, inherited numeric
anchors, ownership switches, malformed inputs, input immutability, and independent
output pointers. Twenty equivalent evaluations shuffle nodes and both edge
collections and compare encoding/json bytes. The pure multi-repository fixture
includes accepted/manual Types, a blocked child, Set-Position false, parentless
Ready work, a CLOSED child, and a completed earlier segment.

## Boundary left to #39

#39 owns fresh normal Project observation, recomputation of topology/lifecycle/
Roadmap Order, current-versus-desired number comparison, missing/stale value
writes, canonical field clearing, and idempotent retry after partial failure.
Native item-position mutation and afterId are not Engineering mechanisms.
#36 adds no number writer, reconciliation loop, persistence/schema migration,
runtime composition, CLI behavior, dependencies, Nix changes, or nested lifecycle.
The administrator's view sort is configured
outside the controller; no live Project is mutated by this revision.
