# GitHub integration boundary

`internal/github.Client` is the controller-facing, read-only boundary. It offers
`DiscoverSchema`, `Repository`, `Issue`, `PullRequest`, and `ProjectItems`.
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

Production should use a GitHub App installation-token provider. Such a provider
owns App identity/private-key access, JWT signing, installation-token minting,
and in-memory expiry/refresh. Its returned installation token fits `TokenSource`
without changes to reads or workflow code. Concrete minting is deferred; this
issue neither persists credentials nor implements speculative token management.
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
PR lifecycle remains Phase #5. Client itself stays read-only: composition requires
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
[prbinding](pr-binding.md). No write capability, token minting, assignment, or
production PR routing is added. Local tests exercise endpoint/identity validation,
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
CommitComparison, LifecycleFields, NumericFields and ProjectItemReader. Engineering
composition explicitly requires each capability before serving HTTP, and applies
policy/provenance checks before authorized mutations. No production semantic
provider or implicit semantic mutation authority is activated.
