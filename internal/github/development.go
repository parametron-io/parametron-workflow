package github

import (
	"context"
	"encoding/json"
	"strings"
)

// DevelopmentReferences contains complete, separately typed closing references.
// Manual links are the Target projection class; keyword links are never removable.
type DevelopmentReferences struct {
	PR              Identity
	Manual, Keyword []Identity
	Complete        bool
}
type DevelopmentReader interface {
	DevelopmentReferences(context.Context, Identity) (DevelopmentReferences, error)
}
type DevelopmentWriter interface {
	AddDevelopmentReference(context.Context, Identity, Identity) error
	RemoveDevelopmentReference(context.Context, Identity, Identity) error
}

func ValidDevelopmentIdentity(i Identity) bool {
	return ValidBranchRepository(i.Repository) && validIDs(i.ID) && i.Number > 0
}
func (r DevelopmentReferences) Validate(pr Identity) error {
	if !ValidDevelopmentIdentity(pr) || r.PR != pr || !r.Complete {
		return failure(Malformed)
	}
	ids := map[string]Identity{}
	names := map[Ref]Identity{}
	repositories := map[string]Repository{pr.Repository.ID: pr.Repository}
	repositoryNames := map[string]string{strings.ToLower(pr.Repository.Owner + "/" + pr.Repository.Name): pr.Repository.ID}
	for _, list := range [][]Identity{r.Manual, r.Keyword} {
		seen := map[string]bool{}
		for _, i := range list {
			ref := Ref{Owner: strings.ToLower(i.Repository.Owner), Repository: strings.ToLower(i.Repository.Name), Number: i.Number}
			if !ValidDevelopmentIdentity(i) || i.ID == pr.ID || seen[i.ID] {
				return failure(Malformed)
			}
			name := strings.ToLower(i.Repository.Owner + "/" + i.Repository.Name)
			if old, ok := repositories[i.Repository.ID]; ok && old != i.Repository {
				return failure(Malformed)
			}
			if old, ok := repositoryNames[name]; ok && old != i.Repository.ID {
				return failure(Malformed)
			}
			repositories[i.Repository.ID], repositoryNames[name] = i.Repository, i.Repository.ID
			if old, ok := ids[i.ID]; ok && old != i {
				return failure(Malformed)
			}
			if old, ok := names[ref]; ok && old != i {
				return failure(Malformed)
			}
			ids[i.ID], names[ref], seen[i.ID] = i, i, true
		}
	}
	return nil
}
func (t *Transport) DevelopmentReferences(ctx context.Context, pr Identity) (DevelopmentReferences, error) {
	state, err := t.nativeIdentity(ctx, pr, "pull_request")
	if err != nil {
		return DevelopmentReferences{}, err
	}
	out := DevelopmentReferences{PR: pr, Complete: true}
	for idx, extra := range []string{",userLinkedOnly:true", ",excludeUserLinked:true"} {
		nodes, err := t.connectionWithExtra(ctx, pr.ID, "PullRequest", "closingIssuesReferences", "__typename "+identitySelection, extra)
		if err != nil {
			return DevelopmentReferences{}, err
		}
		list := make([]Identity, 0, len(nodes))
		for _, raw := range nodes {
			var w struct {
				wireIdentity
				Type string `json:"__typename"`
			}
			if json.Unmarshal(raw, &w) != nil || w.Type != "Issue" {
				return DevelopmentReferences{}, failure(Malformed)
			}
			i, err := w.wireIdentity.normalized()
			if err != nil {
				return DevelopmentReferences{}, err
			}
			list = append(list, i)
		}
		if idx == 0 {
			out.Manual = list
		} else {
			out.Keyword = list
		}
	}
	if err := out.Validate(pr); err != nil {
		return DevelopmentReferences{}, err
	}
	final, err := t.nativeIdentity(ctx, pr, "pull_request")
	if err != nil {
		return DevelopmentReferences{}, err
	}
	if final != state {
		return DevelopmentReferences{}, failure(Conflict)
	}
	return out, ctx.Err()
}
func (t *Transport) developmentReference(ctx context.Context, pr, issue Identity, add bool) error {
	state, err := t.nativeIdentity(ctx, pr, "pull_request")
	if err != nil {
		return err
	}
	if state != "OPEN" {
		return failure(Conflict)
	}
	if _, err = t.nativeIdentity(ctx, issue, "issue"); err != nil {
		return err
	}
	refs, err := t.DevelopmentReferences(ctx, pr)
	if err != nil {
		return err
	}
	present := false
	for _, i := range refs.Manual {
		if i == issue {
			present = true
		}
	}
	if present == add {
		return ctx.Err()
	}
	operation, input := "removeCloseIssueReferences", "RemoveCloseIssueReferencesInput"
	if add {
		operation, input = "addCloseIssueReferences", "AddCloseIssueReferencesInput"
	}
	var data struct {
		Result *struct{ Issue *wireIdentity }
	}
	q := `mutation($input:` + input + `!){result:` + operation + `(input:$input){issue{` + identitySelection + `}}}`
	if err = t.query(ctx, q, map[string]any{"input": map[string]any{"issueId": issue.ID, "pullRequestIds": []string{pr.ID}}}, &data); err != nil {
		return err
	}
	if data.Result == nil || data.Result.Issue == nil {
		return failure(Malformed)
	}
	actual, err := data.Result.Issue.normalized()
	if err != nil || actual != issue {
		return failure(Malformed)
	}
	return ctx.Err()
}
func (t *Transport) AddDevelopmentReference(ctx context.Context, pr, issue Identity) error {
	return t.developmentReference(ctx, pr, issue, true)
}
func (t *Transport) RemoveDevelopmentReference(ctx context.Context, pr, issue Identity) error {
	return t.developmentReference(ctx, pr, issue, false)
}
