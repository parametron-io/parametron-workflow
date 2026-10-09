// Package github defines read-only, normalized GitHub access. Observation does
// not authorize mutations; workflow policy is outside this package.
package github

import (
	"context"
	"github.com/parametron-io/parametron-workflow/internal/config"
)

type Repository = config.Repository
type Ref struct {
	Owner, Repository string
	Number            int
}
type Actor struct{ Login string }
type Identity struct {
	ID         string
	Repository Repository
	Number     int
}
type Issue struct {
	Identity
	Title, Body, State             string
	Labels                         []string
	Assignees                      []Actor
	Author                         *Actor
	Type                           *IssueType
	Parent                         *Identity
	SubIssues, BlockedBy, Blocking []Identity
	LinkedPullRequests             []Identity
}
type IssueType struct{ ID, Name string }
type PullRequest struct {
	Identity
	Title, Body, State                 string
	Draft                              bool
	HeadRef, HeadSHA, BaseRef, BaseSHA string
	Labels                             []string
	Author                             *Actor
	ClosingIssues                      []Identity
}
type Content struct{ ID, Kind string }

// Missing fields in Values are unset. Zero numbers remain present values.
type FieldValue struct {
	Kind           config.FieldKind
	OptionID, Date string
	Number         float64
}
type ProjectItem struct {
	ID, ProjectID string
	Archived      bool
	Content       *Content
	Values        map[string]FieldValue
}

type Client interface {
	DiscoverSchema(context.Context, config.SourceConfig) (config.Schema, error)
	Repository(context.Context, string, string) (Repository, error)
	Issue(context.Context, Ref) (Issue, error)
	PullRequest(context.Context, Ref) (PullRequest, error)
	// ProjectItems returns membership of an Issue or PR, restricted to projectID.
	// fields maps configured field IDs to their expected kinds.
	ProjectItems(context.Context, string, string, map[string]config.FieldKind) ([]ProjectItem, error)
}

// TokenSource may mint/refresh GitHub App installation tokens. Credentials never
// enter configuration, domain models, or semantic components.
type TokenSource interface {
	Token(context.Context) (string, error)
}
type TokenFunc func(context.Context) (string, error)

func (f TokenFunc) Token(ctx context.Context) (string, error) { return f(ctx) }
