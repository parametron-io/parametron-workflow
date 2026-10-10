package github

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/config"
)

const repositorySelection = `id name owner{login}`
const identitySelection = `id number repository{` + repositorySelection + `}`

type wireRepository struct {
	ID, Name string
	Owner    *struct{ Login string }
}

func (w wireRepository) normalized() (Repository, error) {
	if blank(w.ID) || blank(w.Name) || w.Owner == nil || blank(w.Owner.Login) {
		return Repository{}, failure(Malformed)
	}
	return Repository{ID: w.ID, Owner: w.Owner.Login, Name: w.Name}, nil
}
func blank(s string) bool { return strings.TrimSpace(s) == "" }

type wireIdentity struct {
	ID         string
	Number     int
	Repository wireRepository
}

func (w wireIdentity) normalized() (Identity, error) {
	r, err := w.Repository.normalized()
	if err != nil {
		return Identity{}, err
	}
	if blank(w.ID) || w.Number <= 0 {
		return Identity{}, failure(Malformed)
	}
	return Identity{ID: w.ID, Number: w.Number, Repository: r}, nil
}
func (t *Transport) Repository(ctx context.Context, owner, name string) (Repository, error) {
	if blank(owner) || blank(name) {
		return Repository{}, failure(Permanent)
	}
	var data struct{ Repository *wireRepository }
	err := t.query(ctx, `query($owner:String!,$name:String!){repository(owner:$owner,name:$name){`+repositorySelection+`}}`, map[string]any{"owner": owner, "name": name}, &data)
	if err != nil {
		return Repository{}, err
	}
	if data.Repository == nil {
		return Repository{}, failure(NotFound)
	}
	r, err := data.Repository.normalized()
	if err == nil && (r.Owner != owner || r.Name != name) {
		err = failure(Malformed)
	}
	if err != nil {
		return Repository{}, err
	}
	return r, nil
}
func (t *Transport) resource(ctx context.Context, ref Ref, kind, selection string, out any) error {
	if blank(ref.Owner) || blank(ref.Repository) || ref.Number <= 0 {
		return failure(Permanent)
	}
	var data struct {
		Repository *struct{ Result json.RawMessage }
	}
	err := t.query(ctx, `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){result:`+kind+`(number:$number){`+selection+`}}}`, map[string]any{"owner": ref.Owner, "name": ref.Repository, "number": ref.Number}, &data)
	if err != nil {
		return err
	}
	if data.Repository == nil || len(data.Repository.Result) == 0 || string(data.Repository.Result) == "null" {
		return failure(NotFound)
	}
	if json.Unmarshal(data.Repository.Result, out) != nil {
		return failure(Malformed)
	}
	return nil
}
func matches(i Identity, r Ref) bool {
	return i.Number == r.Number && i.Repository.Owner == r.Owner && i.Repository.Name == r.Repository
}
func validAuthor(a *Actor) bool { return a == nil || !blank(a.Login) }
func (t *Transport) labels(ctx context.Context, id, typ string) ([]string, error) {
	nodes, err := t.connection(ctx, id, typ, "labels", "name")
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(nodes))
	for _, n := range nodes {
		var w struct{ Name string }
		if json.Unmarshal(n, &w) != nil || blank(w.Name) {
			return nil, failure(Malformed)
		}
		result = append(result, w.Name)
	}
	return result, nil
}
func (t *Transport) identities(ctx context.Context, id, typ, field string) ([]Identity, error) {
	nodes, err := t.connection(ctx, id, typ, field, identitySelection)
	if err != nil {
		return nil, err
	}
	result := make([]Identity, 0, len(nodes))
	for _, n := range nodes {
		var w wireIdentity
		if json.Unmarshal(n, &w) != nil {
			return nil, failure(Malformed)
		}
		i, err := w.normalized()
		if err != nil {
			return nil, err
		}
		result = append(result, i)
	}
	return result, nil
}
func (t *Transport) Issue(ctx context.Context, ref Ref) (Issue, error) {
	var w struct {
		wireIdentity
		Title, Body, State *string
		Author             *Actor
		IssueType          *IssueType
		Parent             *wireIdentity
	}
	err := t.resource(ctx, ref, "issue", identitySelection+` title body state author{login} issueType{id name} parent{`+identitySelection+`}`, &w)
	if err != nil {
		return Issue{}, err
	}
	i, err := w.wireIdentity.normalized()
	if err != nil {
		return Issue{}, err
	}
	if !matches(i, ref) || w.Title == nil || w.Body == nil || w.State == nil || (*w.State != "OPEN" && *w.State != "CLOSED") || !validAuthor(w.Author) {
		return Issue{}, failure(Malformed)
	}
	if w.IssueType != nil && (blank(w.IssueType.ID) || blank(w.IssueType.Name)) {
		return Issue{}, failure(Malformed)
	}
	result := Issue{Identity: i, Title: *w.Title, Body: *w.Body, State: *w.State, Author: w.Author, Type: w.IssueType}
	if w.Parent != nil {
		p, err := w.Parent.normalized()
		if err != nil {
			return Issue{}, err
		}
		result.Parent = &p
	}
	result.Labels, err = t.labels(ctx, i.ID, "Issue")
	if err != nil {
		return Issue{}, err
	}
	nodes, err := t.connection(ctx, i.ID, "Issue", "assignees", "login")
	if err != nil {
		return Issue{}, err
	}
	for _, n := range nodes {
		var a Actor
		if json.Unmarshal(n, &a) != nil || blank(a.Login) {
			return Issue{}, failure(Malformed)
		}
		result.Assignees = append(result.Assignees, a)
	}
	result.SubIssues, err = t.identities(ctx, i.ID, "Issue", "subIssues")
	if err != nil {
		return Issue{}, err
	}
	result.BlockedBy, err = t.identities(ctx, i.ID, "Issue", "blockedBy")
	if err != nil {
		return Issue{}, err
	}
	result.Blocking, err = t.identities(ctx, i.ID, "Issue", "blocking")
	if err != nil {
		return Issue{}, err
	}
	result.LinkedPullRequests, err = t.identities(ctx, i.ID, "Issue", "closedByPullRequestsReferences")
	if err != nil {
		return Issue{}, err
	}
	return result, nil
}
func (t *Transport) PullRequest(ctx context.Context, ref Ref) (PullRequest, error) {
	var w struct {
		wireIdentity
		Title, Body, State                               *string
		IsDraft                                          *bool
		HeadRefName, HeadRefOid, BaseRefName, BaseRefOid *string
		Author                                           *Actor
	}
	err := t.resource(ctx, ref, "pullRequest", identitySelection+` title body state isDraft headRefName headRefOid baseRefName baseRefOid author{login}`, &w)
	if err != nil {
		return PullRequest{}, err
	}
	i, err := w.wireIdentity.normalized()
	if err != nil {
		return PullRequest{}, err
	}
	if !matches(i, ref) || w.Title == nil || w.Body == nil || w.State == nil || (*w.State != "OPEN" && *w.State != "CLOSED" && *w.State != "MERGED") || w.IsDraft == nil || w.HeadRefName == nil || w.HeadRefOid == nil || w.BaseRefName == nil || w.BaseRefOid == nil || blank(*w.HeadRefName) || blank(*w.HeadRefOid) || blank(*w.BaseRefName) || blank(*w.BaseRefOid) || !validAuthor(w.Author) {
		return PullRequest{}, failure(Malformed)
	}
	labels, err := t.labels(ctx, i.ID, "PullRequest")
	if err != nil {
		return PullRequest{}, err
	}
	closing, err := t.identities(ctx, i.ID, "PullRequest", "closingIssuesReferences")
	if err != nil {
		return PullRequest{}, err
	}
	return PullRequest{ClosingIssues: closing, Identity: i, Title: *w.Title, Body: *w.Body, State: *w.State, Draft: *w.IsDraft, HeadRef: *w.HeadRefName, HeadSHA: *w.HeadRefOid, BaseRef: *w.BaseRefName, BaseSHA: *w.BaseRefOid, Author: w.Author, Labels: labels}, nil
}

const fieldSelection = `__typename ... on ProjectV2FieldCommon{id name dataType} ... on ProjectV2SingleSelectField{options{id name}}`

func (t *Transport) DiscoverSchema(ctx context.Context, source config.SourceConfig) (config.Schema, error) {
	if err := source.Validate(); err != nil {
		return config.Schema{}, err
	}
	var data struct{ Organization *struct{ ID, Login string } }
	if err := t.query(ctx, `query($login:String!){organization(login:$login){id login}}`, map[string]any{"login": source.Organization}, &data); err != nil {
		return config.Schema{}, err
	}
	if data.Organization == nil {
		return config.Schema{}, failure(NotFound)
	}
	org := data.Organization
	if blank(org.ID) || org.Login != source.Organization {
		return config.Schema{}, failure(Malformed)
	}
	result := config.Schema{Organizations: []config.Organization{{ID: org.ID, Login: org.Login}}}
	types, err := t.connection(ctx, org.ID, "Organization", "issueTypes", "id name")
	if err != nil {
		return config.Schema{}, err
	}
	seenNames, seenIDs := map[string]bool{}, map[string]bool{}
	for _, node := range types {
		var typ config.IssueType
		if json.Unmarshal(node, &typ) != nil || blank(typ.ID) || blank(typ.Name) || seenNames[typ.Name] || seenIDs[typ.ID] {
			return config.Schema{}, failure(Malformed)
		}
		seenNames[typ.Name], seenIDs[typ.ID] = true, true
		result.IssueTypes = append(result.IssueTypes, typ)
	}
	for _, name := range source.Repositories {
		r, err := t.Repository(ctx, source.Organization, name)
		if err != nil {
			return config.Schema{}, err
		}
		result.Repositories = append(result.Repositories, r)
	}
	for _, profile := range []config.Profile{config.Engineering, config.BugTracker} {
		binding := source.Projects[profile]
		var data struct {
			Organization *struct {
				ProjectV2 *struct {
					ID     string
					Number int
				}
			}
		}
		err := t.query(ctx, `query($login:String!,$number:Int!){organization(login:$login){projectV2(number:$number){id number}}}`, map[string]any{"login": source.Organization, "number": binding.Number}, &data)
		if err != nil {
			return config.Schema{}, err
		}
		if data.Organization == nil || data.Organization.ProjectV2 == nil {
			return config.Schema{}, failure(NotFound)
		}
		p := data.Organization.ProjectV2
		if blank(p.ID) || p.Number != binding.Number {
			return config.Schema{}, failure(Malformed)
		}
		project := config.Project{ID: p.ID, Owner: source.Organization, Number: p.Number}
		nodes, err := t.connection(ctx, p.ID, "ProjectV2", "fields", fieldSelection)
		if err != nil {
			return config.Schema{}, err
		}
		required := map[string]bool{}
		for _, name := range binding.Fields {
			required[name] = true
		}
		for _, n := range nodes {
			var w struct {
				ID, Name, DataType string
				Options            []config.Option
			}
			if json.Unmarshal(n, &w) != nil || blank(w.ID) || blank(w.Name) || blank(w.DataType) {
				return config.Schema{}, failure(Malformed)
			}
			if !required[w.Name] {
				continue
			}
			f := config.Field{ID: w.ID, Name: w.Name}
			switch w.DataType {
			case "SINGLE_SELECT":
				f.Kind = config.SingleSelect
				if w.Options == nil {
					return config.Schema{}, failure(Malformed)
				}
				for _, o := range w.Options {
					if blank(o.ID) || blank(o.Name) {
						return config.Schema{}, failure(Malformed)
					}
				}
				f.Options = w.Options
			case "NUMBER":
				f.Kind = config.Number
			case "DATE":
				f.Kind = config.Date
			default:
				return config.Schema{}, failure(Malformed)
			}
			project.Fields = append(project.Fields, f)
		}
		// Missing/ambiguous bindings are rejected by config.Resolve; discovery never
		// fabricates a field or ID for absent live configuration.
		result.Projects = append(result.Projects, project)
	}
	return result, nil
}
