# GitHub integration boundary

`internal/github.Client` is the controller-facing, read-only boundary. It offers
`DiscoverSchema`, `Repository`, `Issue`, `PullRequest`, and `ProjectItems`.
`app.Prepare(ctx, source, client)` validates source configuration, discovers
live schema, then calls `config.Resolve`. It returns no partial application
configuration on failure. The command remains a cancellation-aware bootstrap;
issue #14 owns deployment loading, credential-provider construction, and full
runtime integration.

## Normalized data

Repository identity reuses `config.Repository` (node ID, owner login, name).
Issues contain node/repository identity, number, title, unparsed body, OPEN or
CLOSED state, labels, assignee logins, nullable author and Issue type, nullable
parent, sub-issues, blocked-by/blocking identities, and linked Pull Requests
(including closed PRs). PRs contain identity, title, unparsed body, OPEN/CLOSED/
MERGED state, Draft flag, head/base refs and SHAs, labels, nullable author and
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

## Production transport and discovery

`NewTransport` creates a standard-library `net/http` GraphQL adapter. No REST
adapter or SDK is needed for this surface. Queries, decoding structs, HTTP,
and credentials are private to the adapter. An alternate REST/GraphQL adapter
can implement the same `Client` without changing workflow policy. Production
uses HTTPS (the default endpoint is GitHub's GraphQL endpoint); a supplied HTTP
endpoint enables local deterministic tests. No `gh` executable is used at runtime.

Discovery selects only the configured organization, named repositories, and
both configured Project numbers. It returns the existing `config.Schema`, with
live node IDs. It enumerates all pages of each Project's fields, retaining the
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

The current surfaces require read access to organization identity and Projects
v2 schema/items, repository metadata, Issues/types/native relationships, and
Pull Requests. Install the App for every repository whose identities or links
must be visible. GitHub documents repository Issues and Pull Requests read
permissions and organization Projects access in its [App Project example](https://docs.github.com/en/issues/planning-and-tracking-with-projects/automating-your-project/automating-projects-using-actions).
That example also performs writes; its write permissions are not requirements
of this read-only implementation. Exact minimum installation permissions for
these GraphQL queries must be verified during deployment following GitHub's
[GraphQL permission guidance](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/choosing-permissions-for-a-github-app#choosing-permissions-for-graphql-api-access).
No write permission is requested here. Later authorized mutation phases will
specify their own minimum permissions when their API surfaces exist.

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

Issue #11 owns signed webhook ingress; #12 owns durable queues/retries and
ordering; #13 owns event-to-resource processing and policy-facing ObservedState;
#14 owns full startup and end-to-end integration. Lifecycle decisions, directive
parsing, semantic components, and authorized mutations remain later work.
