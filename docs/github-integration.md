# GitHub integration boundary

`internal/github.Client` is the controller-facing, read-only boundary. It offers
`DiscoverSchema`, `Repository`, `Issue`, `PullRequest`, `ProjectItems`, and
`PullRequestReviewFacts`.
`app.Prepare(ctx, source, client)` validates source configuration, discovers
live schema, then calls `config.Resolve`. It returns no partial application
configuration on failure. `app.Run` uses this boundary before opening SQLite
and HTTP ingress. The command loads source bindings and file-backed secrets;
see [runtime.md](runtime.md).

`github.Mutator` is a separate explicit Phase #3 capability implemented by the
same Transport: SetIssueType, ResolveLabels, AddLabels, RemoveLabels,
AddProjectItem, RemoveProjectItem, and SetProjectOption. It exposes no arbitrary
GraphQL, full-label replacement, label creation, relationships, or branch API.
Inputs and response identities are validated, using the existing error/token/HTTP
infrastructure with no retries. MutationFake fails on unconfigured hooks.
Semanticreconcile receives this Phase #3 write capability; Client/observe remain
read-only.

#37 adds a separate `github.Branches` capability with `DefaultBranch`, `Branch`,
and `CreateBranch`. All accept the configured Repository identity; exact lookup
and creation use short heads names. Transport uses Repository.defaultBranchRef,
Repository.ref with `refs/heads/<name>`, and the narrow GraphQL
[createRef](https://docs.github.com/en/graphql/reference/git#createref) mutation
with repositoryId, fully qualified name, and exact commit oid. No general query,
update/delete ref, shell, default-branch mutation, or Project mutation is exposed.
It reuses token/HTTP/context/error infrastructure with no retries. BranchFake
fails unconfigured calls. Client, Mutator, and their existing fakes are unchanged.

Both enclosing and returned ref repository identities are verified against the
configured node ID, owner, and name. Ref prefix must be `refs/heads/`, the exact
requested name must match, target must be Commit, and oid must be 40 lowercase
hexadecimal digits. Creation also requires returned oid to equal requested oid.
Default lookup retains the actual default name and SHA in one response. Missing
repository/ref is NotFound; inconsistent/malformed data is Malformed. Existing
Conflict, RateLimited, Transient, and cancellation categories propagate.
Exact lookup returns `github.MissingBranch` only for an explicit null ref after
repository validation. This typed error includes the validated repository/name
and unwraps to the existing NotFound category. Generic NotFound (HTTP 404 or
missing repository) is not proof of branch absence and cannot enable creation.
Names use a bounded ASCII subset: 1..255 bytes of letters/digits, dot, underscore,
hyphen, and slash, with no empty, dot-leading, hyphen-leading, dot-ending, or
`.lock`-ending components, `..`, or `refs/` prefix. Unrepresentable default names
fail explicitly. Branch eligibility and provenance are owned by
engineeringbranch, not this capability. See [execution.md](execution.md).

## Managed feedback comment capabilities

Issue #52 adds separate `github.CommentReader` (`IssueComments`, `IssueComment`)
and `github.CommentWriter` (`CreateIssueComment`, `UpdateIssueComment`)
interfaces. These remain separate from `Client` and `Mutator`, and support only
Issue-comment conversation surfaces, including PR conversation comments, never
review comments. Issue #53 production routing invokes the existing feedback
consumer with current typed Engineering producer evidence and verified author identity.
See [managed feedback](managed-feedback.md) for ownership and reconciliation.

Transport requires exact repository/resource node and numeric identity, resource
kind, comment node and positive database ID, and valid author stable ID, login
and User/Bot type. Comment URLs require HTTPS and host exactly `github.com`,
no userinfo/query, the exact `/owner/repo/issues/N` or `/owner/repo/pull/N` path
and matching `issuecomment-<databaseId>` fragment. Enterprise origins are not
configurable. Transport validates this evidence; the controller separately
verifies the expected author and canonical marker. A marker alone grants no ownership.

Enumeration preflights native identity before and after pagination, validates
exact parent/type on every page, stable totals, complete final counts and unique
node/database identities. Pages are bounded to 100 and totals to 100000; missing,
partial or contradictory evidence fails closed. Exact single-comment lookup
validates the requested node and normalized identity. Creation uses `addComment`
with exact subject ID/body; update uses `updateIssueComment` with exact comment
ID/body after comparing a fresh full comment against the expected record.
Receipts require exact body, valid normalized evidence and matching update ID.
Expected controller authorship and post-write convergence are controller checks.

Typed transport errors and cancellation/deadlines propagate with no hidden blind
retries. Local [transport tests](../internal/github/comments_test.go) and
[HTTP integration tests](../internal/workflowfeedback/integration_test.go) cover
pagination, exact operations/receipts, failures and uncertain-write recovery.
They do not establish authenticated live API compatibility or deployed GitHub App
permissions. Fresh reads do not provide an atomic GitHub snapshot.

## Normalized data

`github.Actor` carries `Login`, stable node `ID`, and `Type` evidence. PR author
reads select ID and GraphQL typename; Issue assignees select the same evidence.
Issue author reads remain login-only. Nullable current PR `headRepository` maps
to `PullRequest.HeadRepository` (zero Repository when absent), preserving node ID,
owner, and name. Observation does not itself verify human eligibility.

Issue #34 adds a separate `github.IssueLister.ListIssues(ctx, repository)`
read capability, implemented by Transport using the paginated Repository.issues
IssueConnection. It validates discovered repository identity and complete Issue
records, excludes PRs, and performs no workflow normalization. Client and Mutator
are unchanged. ListerFake fails unconfigured calls. See
[engineering-context.md](engineering-context.md) for authoritative child discovery.

#36 requires Engineering's numeric Roadmap Order binding. Existing configured
number reads preserve its value for comparison; native Project item position is
not an Engineering input. There is no special ordered reader. Client and Mutator
are unchanged. See [engineering-ordering.md](engineering-ordering.md) for pure
numeric assignments/clears and their implemented production convergence.

Repository identity reuses `config.Repository` (node ID, owner login, name).
Issues contain node/repository identity, number, title, unparsed body, OPEN or
CLOSED state, labels, assignee Actor identities, nullable author and Issue type,
nullable
parent, sub-issues, blocked-by/blocking identities, and linked Pull Requests
(including closed PRs). PRs contain identity, title, unparsed body, OPEN/CLOSED/
MERGED state, Draft flag, head repository identity, head/base refs and SHAs,
labels, nullable author and
closing Issue identities. Author identifies the current resource author, not
an event actor. Native links are observations, never authorization.

Project item reads start from an Issue/PR node ID and retain membership only in
the requested Project. Items preserve item/Project/content IDs, content kind,
archive state, and configured field values keyed by field ID. Callers supply
expected field kinds; incompatible values fail explicitly. Single-select
values retain option IDs; numeric zero is present, and dates use YYYY-MM-DD.
Missing entries mean unset fields. Archived memberships are included. A missing
content resource is `not_found`; an existing resource without matching membership
returns an empty collection. This operation does not enumerate Project members
or draft issues unrelated to the requested resource.

`internal/observe.Processor` consumes these current reads for both configured
Projects using the freshly fetched Issue/PR node ID. It verifies primary and
membership identities, normalizes ordering, and maps field IDs and configured
Status options to semantic roles. No webhook state enters its policy input.
Primary `not_found` produces a missing observation; Project-read `not_found`
discards the incomplete observation and propagates unchanged to the worker.
See [observation.md](observation.md) for the implemented handoff contract.

## Production transport and discovery

`NewTransport` creates a standard-library `net/http` adapter. The Client surface
uses GraphQL without an SDK; narrow REST reads provide commit comparison and
developer identity/authorization evidence described below. Queries, decoding
structs, HTTP,
and credentials are private to the adapter. An alternate REST/GraphQL adapter
can implement the same `Client` without changing workflow policy. Production
uses HTTPS (the default endpoint is GitHub's GraphQL endpoint); a supplied HTTP
endpoint enables local deterministic tests. No `gh` executable is used at runtime.

Discovery selects only the configured organization, named repositories, and
both configured Project numbers. It returns the existing `config.Schema`, with
live node IDs. It enumerates organization issueTypes and all pages of each
Project's fields, retaining the
configured field names. SINGLE_SELECT, NUMBER, and DATE map explicitly to the
existing kinds. Unsupported configured kinds fail; unrelated field kinds are
ignored. Every option of a selected single-select field is retained. Missing
or duplicate configured bindings and compatible-kind mismatches remain the
responsibility of `config.Resolve`; absent API resources, required identities,
or malformed payloads fail discovery without returning partial schema.

All GraphQL connections (fields, labels, assignees, relationships, Project
memberships, item field values) use cursor pagination with pages of 100. Missing
page metadata, null nodes, empty continuation cursors, and cursor cycles fail
instead of returning incomplete state. Direct repository/Project lookup does
not enumerate unrelated repositories/Projects. Single-select options are an
array, not a paginated connection in the [GitHub Project schema](https://docs.github.com/en/graphql/reference/projects).

Each request uses its caller context, explicit User-Agent, JSON negotiation,
and GitHub API version header. Responses are limited to 8 MiB per page.
Redirects are refused. A default HTTP client has a 30-second timeout; callers
may inject a client with their own timeout/transport. There are no hidden retries.
Multiple requests are current reads, not a transactionally consistent snapshot.

## Authentication and GitHub App direction

Only `TokenSource.Token(ctx)` supplies credentials. `TokenFunc` allows a
caller-owned development credential source or a production provider to be
injected. The transport calls it per request, so providers may refresh tokens
between pages. No client method reads environment variables or credential files.
Neither source/resolved configuration nor returned models contain credentials.

`github.AppCredentials` implements App-owned installation-token minting and the
shared comment-author verification boundary. The CLI defaults to this mode;
[runtime](runtime.md#command-and-deployment) lists exact explicit bindings and the
separate user credential mode. Opaque externally minted installation tokens do
not establish supported author provenance, and installation tokens never use
`GET /user`. No runtime `gh` dependency exists.

The provider accepts bounded PKCS#1/PKCS#8 RSA PEM (at least 2048 bits), signs RS256
JWTs with the configured client ID as issuer, backdates `iat` by 60 seconds and
expires after nine minutes. Minting verifies `/app`, the exact installation and
account bindings, unsuspended state, and required Issues/Pull Requests write plus
Metadata read permissions. It requests tokens restricted to configured repositories,
checks inherited permission equality and selected repository scope, and completely
enumerates the token's repository grants (at most 500 configured repositories).
Additional permissions required by composed Project, branch and membership
operations must still be supplied and verified at deployment.

The authenticated App slug selects `/users/<slug>[bot]` as a lookup key, never as
identity proof alone. REST Bot login must exactly equal `<slug>[bot]`; the observed
GraphQL `Bot` login must exactly equal the bare authenticated App slug. Both API
representations are verified against that App and the configured trusted REST-style
author, and their stable node and database identities must agree. Live GitHub App
verification remains incomplete until the outstanding provider and managed-comment
gates are exercised. The installation
is rechecked after minting and identity observations. The same provider instance
serves transport requests and comment-author checks. Comment reconciliation
rechecks Bot identity; the installation account is never substituted for its Bot.

Tokens are cached only in memory and refreshed with a two-minute expiry margin;
a failed refresh clears the old token. Context-aware serialization prevents
concurrent refresh races. Typed provider failures/cancellation propagate without
secret diagnostics or blind mutation retries. Authentication failures can invalidate
the cached token; an uncertain mutation is not replayed inside the transport.
[App credential tests](../internal/github/app_credentials_test.go) and
[pagination/cancellation tests](../internal/github/app_pagination_test.go) verify
this chain offline using synthetic keys and local responses. They do not prove
live JWT acceptance, permissions, grants, Bot compatibility or comment receipts.

Semantic/model components receive normalized planning inputs and never receive
the client transport, token provider, or GitHub credentials.

Standalone developer reads require organization `Members: read` and repository
`Metadata: read`, alongside existing Issue/PR/repository/ref read access for the
composed readers. These are required capabilities, not evidence that deployed
credentials have them. No live GitHub App permission rehearsal is claimed.

The standalone #37 branch capability requires repository Contents read access for
ref/default-branch observation and Contents write access when branch creation is
activated. Workflows write may additionally be required where GitHub requires it
for operations affecting workflow files. Exact deployment permissions must be
verified before production composition.

The current surfaces require read access to organization identity and Projects
v2 schema/items, repository metadata, Issues/types/native relationships, and
Pull Requests. Install the App for every repository whose identities or links
must be visible. GitHub documents repository Issues and Pull Requests read
permissions and organization Projects access in its [App Project example](https://docs.github.com/en/issues/planning-and-tracking-with-projects/automating-your-project/automating-projects-using-actions).
The default CLI enables Engineering: deployment credentials need appropriate
Project write access for owned Status, Start Date and numeric Roadmap Order,
and repository write access for authorized branch creation. Explicit semantic
reconciliation requires
appropriate Issue/PR metadata and organization Project write access. Exact
minimum installation permissions must be verified during deployment following GitHub's
[GraphQL permission guidance](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/choosing-permissions-for-a-github-app#choosing-permissions-for-graphql-api-access).
See [semantic-reconciliation.md](semantic-reconciliation.md) for the narrow write
boundary; no production semantic provider is activated by the CLI.

## Errors and deterministic tests

`errors.As(err, *github.Error)` exposes Category and HTTP Status. Categories are
`not_found`, `unauthorized`, `forbidden`, `rate_limited`, `conflict`,
`malformed_response`, `transient`, and `permanent_request`.
401/403/404 map directly; 429 and 403 with rate-limit headers are rate limited;
409/412 are conflict; 5xx/network failures are transient. Other non-2xx responses
are permanent request failures. GraphQL errors are classified by type/code;
partial GraphQL data is never accepted. Malformed/oversized JSON or incompatible
normalized data is a malformed response.

Errors exclude server bodies, arbitrary provider messages, and token contents.
Context cancellation/deadlines remain detectable through `errors.Is`; only
these safe underlying causes are retained. A token provider can return a
classified `github.Error` (e.g. transient minting failure); an unclassified
provider failure is unauthorized. Retry scheduling belongs to #12.

`github.Fake` contains explicit function fields for each operation. Closures
provide fixtures/results/errors and assert arguments or order; an unconfigured
operation fails explicitly. Tests use fakes and `httptest.Server`, require no
credentials/network/CLI, and cover normalization, both Projects, pagination,
configuration preparation, authentication isolation, and failure boundaries.

Signed webhook ingress, durable worker queues/retries, observation, and startup
are integrated through `internal/app`, along with explicit Phase #3 directives,
semantic acceptance, and authorized metadata/routing reconciliation. Engineering
lifecycle/reconciliation, branches and first activity are now production-composed;
accepted PR onboarding and deterministic review/rework are production-composed.
Issue #53 also composes current PR discovery, companion numeric reconciliation,
Development projection and supported typed managed feedback. Their owners retain
independent current authority checks; unrelated relationship projection remains
deferred. See [execution](execution.md#continuous-engineering-pr-reconciliation).
Separate native lifecycle capabilities are implemented as described below.
Client itself stays read-only: composition requires
separate narrow capabilities and deterministic Go authorization.

## Read-only developer authorization evidence

`github.DeveloperReads` is separate from Client and mutation capabilities:

| Method | REST endpoint | Returned evidence |
| --- | --- | --- |
| `User(ctx, login)` | `GET /users/{username}` | `Actor` |
| `OrganizationMembership(ctx, org, login)` | `GET /orgs/{org}/memberships/{username}` | `Membership{Organization, User, State}` |
| `RepositoryPermission(ctx, repo, login)` | `GET /repos/{owner}/{repo}/collaborators/{username}/permission` | `Permission{Repository, User, Base}` |

Transport validates bounded GitHub logins before requesting, nonblank organization
ID and valid organization login for membership, and valid exact repository
identity for permission. REST user decoding requires positive numeric ID, valid
stable node ID, matching requested login, and supported User/Bot/Organization Type.
`ValidHuman` requires User; `SameUser` additionally compares exact node ID and
case-insensitive login. Membership decoding verifies organization node ID/login
and nonblank state. Permission first uses the existing GraphQL Repository read
to verify exact node ID/owner/name because the REST permission response lacks
repository node identity. Nonblank REST `permission` becomes `Base`; `role_name`
is ignored. Maintain/custom roles use GitHub's effective base mapping, without a
separate role-name grant. Policy accepts only `write` or `admin`.

These GETs reuse the private transport's per-request TokenSource, Bearer auth,
headers, context cancellation, 8 MiB response limit, redirect refusal, timeout,
and no-retry behavior. REST origin/prefix mapping matches commit comparison:
`/graphql` maps to the API root and `/api/graphql` to `/api/v3`. Invalid request
inputs are Permanent; invalid JSON or inconsistent response evidence is Malformed.
HTTP/token/network errors retain the existing typed taxonomy and cancellation
causes. Authorization `404` remains ambiguous NotFound, never proof of
nonmembership or insufficient permission. Explicit membership state and effective
permission are policy evidence only after successful verified reads.

Transport owns reads and decoding; deterministic authorization belongs to
[prbinding](pr-binding.md). These reads grant no write capability, token minting
or assignment. Production PR onboarding consumes them through the separate binding
validator. Local tests exercise endpoint/identity validation,
effective permission, error categories, cancellation, limits, redirect refusal,
and credential isolation without live credentials.

## First-activity capabilities (#38)

`github.CommitComparison.CompareCommits` accepts only a configured exact
Repository and lowercase 40-hex BaseSHA/HeadSHA. Transport verifies both Commit
objects and their enclosing/nested repository identity through GraphQL, then
uses GitHub's supported exact-SHA
[REST comparison](https://docs.github.com/en/rest/commits/commits#compare-two-commits)
at `GET /repos/{owner}/{repo}/compare/{base}...{head}?per_page=1`.
Only URL, status, ahead/behind counts, base commit and merge-base SHA are retained.
The returned API URL must exactly match the requested repository/revisions;
base commit must match BaseSHA. Counts must be present, nonnegative and consistent
with identical/ahead/behind/diverged, including revision equality. Ahead requires
strictly positive ahead count, zero behind count and merge-base equal BaseSHA.
Behind requires merge-base equal HeadSHA. Unrelated history/API failure and
malformed identities grant no authority; no history/commit-prose API is exposed.
The existing private HTTP request mechanics are shared between GraphQL and this
narrow REST read: token source, cancellation, response limit, redirect refusal,
error taxonomy and no-hidden-retry policy remain unchanged. REST uses the same
origin as the GraphQL endpoint; /graphql maps to the API root, and Enterprise
/api/graphql maps to /api/v3. Other injected endpoint prefixes are retained.

`github.LifecycleFields` exposes only existing SetProjectOption and new
SetProjectDate. The date setter validates Project/item/field IDs and canonical
valid YYYY-MM-DD (year at least 1), and validates response item/Project identity.
It uses the narrow updateProjectV2ItemFieldValue DATE value mutation. No arbitrary
mutation, clear, numeric, position or membership capability is added. #38 never
receives the full semantic Mutator. Assignment is deliberately absent because
developer binding evidence does not establish human assignability.
See [execution.md](execution.md) for authorization and partial-write safety.
At #38 standalone completion these capabilities added no app/CLI activation.
Current production Engineering composition now uses them, without implicitly
activating the semantic Mutator or a model provider.

For #38 qualification, the push’s immutable Delivery.ReceivedAt must be strictly
after the validated #37 successful-creation provenance CreatedAt exposed by its
read-only Completion value. Processing-time completion alone is insufficient.
Earlier or equal-time notifications grant no activity and produce no new activity
provenance or lifecycle writes, including on restart or out-of-order processing.

## Numeric Project fields and current item identity

`github.NumericFields` is a separate reusable narrow capability:
SetProjectNumber(project, item, field, float64) and
ClearProjectNumber(project, item, field). It grants exact NUMBER set/unset only,
not arbitrary GraphQL execution or Roadmap policy. engineeringreconcile decides
when each write/clear is authorized from the exact pure ordering plan.

Transport validates nonblank exact IDs and finite input before mutation, validates
Project/item association and exact field identity/kind through paginated Project
fields, then validates mutation result identity. It refetches identity/field kind
and paginated current item field values AFTER writing. Set requires presence and
EXACT float equality; clear requires true absence, never numeric zero. Missing,
duplicate, wrong-kind, nonfinite, or mismatched evidence fails with typed GitHub
errors. Shared authentication/error/HTTP/context mechanics apply, cancellation is
detectable and no hidden retry exists. No position, afterId, or item move is exposed.

`github.ProjectItemReader.CurrentProjectItem(ctx, itemID)` is a separate current
identity read for bounded notification routing. It validates exact item/Project
identity and returns ContentKind plus Issue identity when present, with no field
values. Missing item is typed NotFound; malformed content/identity fails closed.
PR, DraftIssue and null content can be identified without granting Engineering
Issue authority. engineeringflow validates configured Project/repository and
refetches current primary Issue identity before binding; payload Status/Roadmap
values are ignored. See [webhooks.md](webhooks.md) for applicable actions.

The default CLI's shared Transport satisfies Client, IssueLister, Branches,
CommitComparison, LifecycleFields, NumericFields, ProjectItemReader, DeveloperReads,
DraftConverter, PullRequestProjectItemReader and narrow onboarding Project writes.
Engineering composition explicitly requires each capability before serving HTTP, and applies
policy/provenance checks before authorized mutations. No production semantic
provider or implicit semantic mutation authority is activated.

## Accepted PR onboarding capabilities

`github.DraftConverter` exposes only `ConvertPullRequestToDraft(ctx, prNodeID)`.
Transport rejects blank/whitespace-containing IDs as Permanent, calls the narrow
`convertPullRequestToDraft` GraphQL mutation, and requires the returned PR ID to
match exactly with a present true `isDraft`. Missing/mismatched results are
Malformed. This grants no Ready-for-review operation, review initiation or arbitrary
PR mutation. Deterministic onboarding and review/rework policy separately authorize
its use; the capability itself grants no workflow authority.

`github.PullRequestProjectItemReader` exposes
`CurrentPullRequestProjectItem(ctx, itemID)`. It reads current ProjectV2Item identity
and content through GraphQL without field values. The requested item ID must be
valid; returned item ID must match exactly and Project ID must be valid. Missing
node is NotFound; wrong node kind, mismatched identity or malformed content is
Malformed. Output carries ID, ProjectID, ContentKind and optional normalized
Issue/PullRequest identity (node ID, number and repository identity). Null content
and DraftIssue are representable without granting onboarding authority. Onboarding
requires exact intended Project and PullRequest content identity. This separate
reader supplements the Issue-oriented `ProjectItemReader`; it does not replace it.

Both operations reuse private authentication, response limits, redirect refusal,
context and typed error handling, with no hidden retries. Context cancellation and
deadlines remain detectable with `errors.Is`, including in-flight requests. Existing
Unauthorized, Forbidden, RateLimited, Conflict, Transient and Permanent categories
propagate; malformed responses are never accepted as successful effects.

Onboarding receives only `AddProjectItem` and `SetProjectOption` for Project writes,
not the full semantic Mutator. Existing input/response identity checks remain;
policy verifies current membership/item/content before and after effects. Adding
membership does not grant Status or Draft authorization by itself. See
[execution](execution.md#accepted-pr-onboarding) for fresh binding barriers.

Engineering composition requires DeveloperReads, DraftConverter, ReviewFactsReader,
PullRequestProjectItemReader and those narrow Project writes in addition to its
existing capabilities; missing interfaces fail construction. Deployment needs the
existing developer authorization reads, current PR/Project reads, Project write
access for membership/owned Status, and Pull Requests write access for Draft
conversion. Exact credential grants must be verified at deployment; local transport
tests do not establish live installation permissions. App-owned token minting is
implemented as described above; live permission rehearsal remains pending. The CLI
activates no production model provider, and deterministic onboarding needs no Runner.

## Native PR review facts

`github.ReviewFactsReader` exposes only
`PullRequestReviewFacts(context.Context, Identity) (ReviewFacts, error)` and is
included in `github.Client`. `ReviewFacts` carries the exact PR identity,
`Transitions`, `Submissions` and `LatestOpinionated`. Review policy consumes this
narrow typed read boundary, separately from `DraftConverter` and Status writes.
These are native observations, not authorization.

`internal/github/review.go` first reads the PR by owner/repository/number and
checks exact node, number and repository identity. It then paginates the PR node's
`timelineItems` with those two item-type filters and pages of 100, retaining only
`ReadyForReviewEvent` and `ConvertToDraftEvent`;
unrelated well-formed timeline kinds are ignored. Each retained transition has
ID, kind, exact PR identity, UTC `CreatedAt` and optional actor. The paginated
`reviews` connection supplies submissions with ID, exact PR, state, optional author
and UTC `SubmittedAt`. PENDING requires null submission time; COMMENTED, APPROVED,
CHANGES_REQUESTED and DISMISSED require a nonzero time. Unsupported states,
duplicate native IDs, wrong embedded PR identities and malformed data fail closed.

The paginated `latestOpinionatedReviews` connection supplies GitHub's current
latest opinions rather than reconstructing reviewer supersession from delivery
order. Its IDs must be unique, state supported and submission time nonzero. Each
entry must exactly match a previously read submission, including actor identity;
a mismatch returns `ErrReviewFactsChanged`. The connections are independent API
reads, not snapshot isolation. The controller additionally compares repeated
current reads and validates strict timeline ordering; equal transition timestamps
cannot establish policy order.

Null actors/authors remain representable. Present actors require valid node ID,
nonblank login and typename, with User/Bot identity selections. Nullability does
not grant readiness/rework authority: policy requires matching actors for the
qualifying readiness and review submission; Draft receipt actor may be null.
Generic connection handling retains page/cursor/node validation and rejects
malformed pagination. Invalid input is Permanent; inconsistent/malformed response
identity is Malformed. Existing typed transport errors propagate, with no hidden
retry. Context cancellation/deadlines remain detectable, including in-flight reads.

No arbitrary GraphQL execution, native review submission/dismissal, reviewer
assignment or controller-driven Ready-for-review mutation was added. The existing
narrow Draft converter is reused for authorized rework. Local transport/GraphQL
fixtures exercise pagination, identities, changing facts, errors and cancellation;
they do not verify authenticated live compatibility or deployed GitHub App grants.

## Native lifecycle and Issue state capabilities

#50 adds `github.NativeLifecycleReader.NativeLifecycle(ctx, identity, kind)` and
`github.IssueStates.CloseIssue` / `ReopenIssue`. They are narrow capabilities;
workflow authority remains deterministic Go policy, with no arbitrary GraphQL
mutation policy interface. Production Engineering requires both capabilities.

NativeLifecycle reads exact Issue or PR state before and after paginated native
timeline reads. Issues support ClosedEvent/ReopenedEvent; PRs additionally support
MergedEvent. Every transition has a unique nonempty ID, exact resource identity,
nonzero timestamp and strictly increasing chronology; equal timestamps cannot
establish ordering. The final transition must agree with current OPEN/CLOSED/MERGED
state (an empty timeline is valid only for OPEN). Both transport and bounded
provider/fake output validation reject unsupported or inconsistent facts.

Identity checks cover repository owner/name/node ID, resource node ID and number,
plus resource kind. Timeline event resource identities must match exactly.
Pagination uses the existing connection boundary and malformed pages/responses
fail closed. Changed state across reads or incompatible chronology produces
`ErrNativeLifecycleChanged`; malformed identity/timeline evidence uses typed
Malformed failures, and invalid caller arguments use Permanent failures.
A native timeline fact is evidence, never mutation authorization by itself.

CloseIssue performs an exact native OPEN preflight and typed `closeIssue` mutation;
ReopenIssue requires CLOSED and uses `reopenIssue`. A raced no-op is
`ErrNativeLifecycleChanged`, not a receipt. The mutation response must contain
exact Issue identity and destination state before returning `IssueStateReceipt`
(`Issue`, `State`). Null/mismatched response data is Malformed; later observed
CLOSED state is not a substitute for the response receipt.

Context cancellation/deadlines propagate, including checks before writes and
after responses. Existing typed transport error categories remain intact; there
is no hidden transport retry. A failed/canceled close may already have executed
remotely, so the completion controller's durable attempt/receipt protocol handles
that uncertainty rather than transport replay. See [persistence](persistence.md#pr-completion-close-provenance)
and [execution](execution.md#accepted-pr-completion-and-terminal-synchronization).

Local HTTP fixtures cover exact operations/variables and every preflight identity
dimension, pagination, malformed/duplicate/unordered transitions, mutation receipt
identity/state, cancellation, typed failures and absence of hidden retries. They
do not verify authenticated live GitHub permissions or deployed App grants.

## Development relationship capabilities

#51 adds separate `github.DevelopmentReader.DevelopmentReferences(ctx, pr)` and
`github.DevelopmentWriter.AddDevelopmentReference(ctx, pr, issue)` /
`RemoveDevelopmentReference(ctx, pr, issue)`. Neither `Client` nor `Mutator` is
extended. Policy receives no arbitrary GraphQL execution.

`DevelopmentReferences` contains exact `PR`, `Manual`, `Keyword`, and `Complete`
evidence. Transport fully paginates `closingIssuesReferences` twice: manual links
with `userLinkedOnly:true`, and non-manual/keyword references with
`excludeUserLinked:true`. Nodes must decode as Issues with repository-qualified
identities. Validation rejects duplicates within a class, inconsistent repository
names/node IDs or endpoint numbers/node IDs, incomplete data, and an Issue endpoint
reusing the PR node ID. Identical overlap across the two classes is valid.

Exact PR identity and lifecycle are checked before and after observation; a changed
state is Conflict. Mutation preflight requires OPEN PR, exact Issue identity and
fresh complete relationship observation. Already-present manual adds and absent
manual removals are no-ops; keyword-only references are never removed. Narrow
`addCloseIssueReferences` / `removeCloseIssueReferences` inputs contain `issueId`
and a single-element `pullRequestIds`. The response must contain the exact Issue
identity. A receipt validates the response, not atomic convergence; the controller
re-reads current state afterward. Supported API operations do not grant workflow
authority or prove live credential permissions.

Typed GitHub failures and context cancellation/deadlines propagate, including
in-flight operations; no hidden retry is performed. Local
[HTTP/GraphQL tests](../internal/github/development_test.go) verify filters,
pagination, endpoint/lifecycle preflight, exact mutation inputs and receipts,
malformed/error handling and cancellation. Authenticated live API compatibility
and GitHub App permission rehearsal remain unverified. Issue #53 composes this
Development owner on supported routes; it adds no unrelated relationship policy.
See [the dedicated contract](pr-development.md) for authorization and exclusions.

## Current PR discovery capability

`github.PullRequestLister.ListPullRequests` enumerates configured repositories'
OPEN/CLOSED/MERGED PRs through the bounded paginated repository connection. It is
separate from the Issue-only Snapshot and the read-only `Client` surface.
`internal/prdiscovery` materializes exact current PR, declared Target, binding and
onboarding evidence in two complete passes, then repeats enumeration as a final
membership/lifecycle barrier. Bounds are 64 repositories, 4096 PRs, 50000 shared
requests and 64 MiB of aggregate evidence per discovery invocation. Incomplete,
changed, oversized or contradictory input returns no partial discovery result.
Stable reads select consumers but provide neither atomic snapshots nor authority.
See [discovery tests](../internal/prdiscovery/discovery_test.go) and
[enumeration transport tests](../internal/github/pull_requests_test.go).

## Issue closure timeline acquisition

`github.IssueClosureReader` exposes `Transport.IssueClosure(ctx, identity)`.
Each private `closureRead` validates exact native Issue/repository identity and
state, traverses the unfiltered GraphQL `timelineItems(first:100, after:cursor)`
connection, and validates total count, Issue creation time, timeline `updatedAt`,
start/end/edge cursors and exact decoded identities across pages. The normalized
result preserves server cursor sequence, not timestamp/ID sorting. Observable
opaque nodes retain their positions; invalid or missing node identity fails closed.
An unrelated event is never filtered away to manufacture command/close adjacency.

Comments retain current body, node/database identity, author/editor, creation/update
and editing timestamps and exact parent Issue. Closed/Reopened events retain their
Issue, actor, event identity and position; supported PullRequest/Commit closer
references remain distinct from ordinary manual closure. Nullable actors are not
inferred: later authorization may require evidence that a syntactically valid
observation lacks. Same-second events retain exact cursor chronology.

Each acquisition is bounded to `MaxClosureItems` (4096), 64 pages and
`MaxClosureEvidenceBytes` (16 MiB), with 100 edges per page. `IssueClosure` applies
a shared 140-request ceiling (also consuming any enclosing request budget),
requires final native identity/state agreement, performs TWO complete acquisitions,
and requires equal normalized results. These checks establish stable observations,
not a transactional GitHub snapshot or a GitHub UI transaction identifier.

Incomplete evidence uses `ErrClosureIncomplete`, changed evidence
`ErrClosureChanged`, and closure-specific page/evidence limits `ErrClosureLimit`.
Some invalid counts fail completeness checks; shared budget exhaustion remains
`ErrRequestLimit`. Typed transport errors and cancellation retain their existing
taxonomy. The public reader returns no authoritative partial result on failure.
Transport normalizes evidence only; `engineeringterminal` separately decides
merge/Force-Close authority and direct-child Phase eligibility.

The eleven committed `TestIssueClosureTransport*` tests in
[issue_closure_test.go](../internal/github/issue_closure_test.go) exercise the real
HTTP/GraphQL path: complete single/multi-page reads, same-second cross-page adjacency,
opaque nodes, cycles, editing/actor/closer decoding, inconsistent pages, differing
complete reads, cancellation, provider errors and observation limits.

## Issue #53 offline verification and live proof gate

Stage 5A independently verified Engineering code and tests at
`6f750a5797fa94bb399f234af7f89524c01c5720` with PASS. Fresh-Nix build/full suite/vet,
repeated closure/Force-Close/identity regressions and both required race groups
passed. Instrumented GitHub tests reported 100.0% statement coverage for both
`Transport.closureRead` and `Transport.IssueClosure`; behavior assertions, rather
than coverage alone, establish the offline acquisition boundary.

Synthetic HTTP/GraphQL and credential tests do not establish deployed GitHub
schema/field availability, complete timeline/cursor provider behavior, real JWT
acceptance, installation identity/permissions/repository grants, REST/GraphQL Bot
compatibility or a real managed-comment authorship receipt. These require separately
authorized live proof before final operational Issue #53 closure. Documentation
synchronization and final PR integration remain pending; parent #5 closure is not
decided. The accepted [Stage 4 proof gate](https://github.com/parametron-io/parametron-workflow/issues/53#issuecomment-6220783946)
is not waived by offline PASS.
