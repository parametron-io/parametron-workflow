package github

import (
	"context"
	"strings"
)

// Mutator is the explicit Phase-3 write capability. It cannot replace complete
// label sets, create labels, mutate relationships, or perform lifecycle actions.
type Mutator interface {
	SetIssueType(context.Context, string, string) error
	ResolveLabels(context.Context, Repository, []string) ([]string, error)
	AddLabels(context.Context, string, []string) error
	RemoveLabels(context.Context, string, []string) error
	AddProjectItem(context.Context, string, string) (string, error)
	RemoveProjectItem(context.Context, string, string) error
	SetProjectOption(context.Context, string, string, string, string) error
}

func validIDs(ids ...string) bool {
	for _, id := range ids {
		if blank(id) || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\r\n\t ") {
			return false
		}
	}
	return true
}
func (t *Transport) SetIssueType(ctx context.Context, issue, typ string) error {
	if !validIDs(issue, typ) {
		return failure(Permanent)
	}
	var data struct {
		Result *struct {
			Issue *struct {
				ID        string
				IssueType *struct{ ID string }
			}
		}
	}
	err := t.query(ctx, `mutation($input:UpdateIssueInput!){result:updateIssue(input:$input){issue{id issueType{id}}}}`, map[string]any{"input": map[string]any{"id": issue, "issueTypeId": typ}}, &data)
	if err != nil {
		return err
	}
	if data.Result == nil || data.Result.Issue == nil || data.Result.Issue.ID != issue || data.Result.Issue.IssueType == nil || data.Result.Issue.IssueType.ID != typ {
		return failure(Malformed)
	}
	return nil
}

// ResolveLabels looks up existing exact repository names; it never provisions labels.
func (t *Transport) ResolveLabels(ctx context.Context, repo Repository, names []string) ([]string, error) {
	if !validIDs(repo.ID) || blank(repo.Owner) || blank(repo.Name) {
		return nil, failure(Permanent)
	}
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		if blank(name) || seen[name] {
			return nil, failure(Permanent)
		}
		seen[name] = true
	}
	for _, name := range names {
		var data struct {
			Repository *struct {
				ID    string
				Label *struct{ ID, Name string }
			}
		}
		if err := t.query(ctx, `query($owner:String!,$name:String!,$label:String!){repository(owner:$owner,name:$name){id label(name:$label){id name}}}`, map[string]any{"owner": repo.Owner, "name": repo.Name, "label": name}, &data); err != nil {
			return nil, err
		}
		if data.Repository == nil || data.Repository.ID != repo.ID {
			return nil, failure(Malformed)
		}
		if data.Repository.Label == nil {
			return nil, failure(Permanent)
		}
		if !validIDs(data.Repository.Label.ID) || data.Repository.Label.Name != name {
			return nil, failure(Malformed)
		}
		out = append(out, data.Repository.Label.ID)
	}
	return out, nil
}
func (t *Transport) labelDelta(ctx context.Context, resource string, ids []string, add bool) error {
	if !validIDs(resource) || len(ids) == 0 {
		return failure(Permanent)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !validIDs(id) || seen[id] {
			return failure(Permanent)
		}
		seen[id] = true
	}
	operation, input := "removeLabelsFromLabelable", "RemoveLabelsFromLabelableInput"
	if add {
		operation, input = "addLabelsToLabelable", "AddLabelsToLabelableInput"
	}
	var data struct {
		Result *struct{ Labelable *struct{ ID string } }
	}
	err := t.query(ctx, `mutation($input:`+input+`!){result:`+operation+`(input:$input){labelable{id}}}`, map[string]any{"input": map[string]any{"labelableId": resource, "labelIds": ids}}, &data)
	if err != nil {
		return err
	}
	if data.Result == nil || data.Result.Labelable == nil || data.Result.Labelable.ID != resource {
		return failure(Malformed)
	}
	return nil
}
func (t *Transport) AddLabels(ctx context.Context, id string, labels []string) error {
	return t.labelDelta(ctx, id, labels, true)
}
func (t *Transport) RemoveLabels(ctx context.Context, id string, labels []string) error {
	return t.labelDelta(ctx, id, labels, false)
}
func (t *Transport) AddProjectItem(ctx context.Context, project, content string) (string, error) {
	if !validIDs(project, content) {
		return "", failure(Permanent)
	}
	var data struct {
		Result *struct {
			Item *struct {
				ID      string
				Project *struct{ ID string }
				Content *struct{ ID string }
			}
		}
	}
	err := t.query(ctx, `mutation($input:AddProjectV2ItemByIdInput!){result:addProjectV2ItemById(input:$input){item{id project{id} content{... on Issue{id} ... on PullRequest{id}}}}}`, map[string]any{"input": map[string]any{"projectId": project, "contentId": content}}, &data)
	if err != nil {
		return "", err
	}
	if data.Result == nil || data.Result.Item == nil {
		return "", failure(Malformed)
	}
	i := data.Result.Item
	if !validIDs(i.ID) || i.Project == nil || i.Project.ID != project || i.Content == nil || i.Content.ID != content {
		return "", failure(Malformed)
	}
	return i.ID, nil
}
func (t *Transport) RemoveProjectItem(ctx context.Context, project, item string) error {
	if !validIDs(project, item) {
		return failure(Permanent)
	}
	var data struct {
		Result *struct{ DeletedItemID string }
	}
	err := t.query(ctx, `mutation($input:DeleteProjectV2ItemInput!){result:deleteProjectV2Item(input:$input){deletedItemId}}`, map[string]any{"input": map[string]any{"projectId": project, "itemId": item}}, &data)
	if err != nil {
		return err
	}
	if data.Result == nil || data.Result.DeletedItemID != item {
		return failure(Malformed)
	}
	return nil
}
func (t *Transport) SetProjectOption(ctx context.Context, project, item, field, option string) error {
	if !validIDs(project, item, field, option) {
		return failure(Permanent)
	}
	var data struct {
		Result *struct {
			ProjectV2Item *struct {
				ID      string
				Project *struct{ ID string }
			}
		}
	}
	err := t.query(ctx, `mutation($input:UpdateProjectV2ItemFieldValueInput!){result:updateProjectV2ItemFieldValue(input:$input){projectV2Item{id project{id}}}}`, map[string]any{"input": map[string]any{"projectId": project, "itemId": item, "fieldId": field, "value": map[string]any{"singleSelectOptionId": option}}}, &data)
	if err != nil {
		return err
	}
	if data.Result == nil || data.Result.ProjectV2Item == nil || data.Result.ProjectV2Item.ID != item || data.Result.ProjectV2Item.Project == nil || data.Result.ProjectV2Item.Project.ID != project {
		return failure(Malformed)
	}
	return nil
}

var _ Mutator = (*Transport)(nil)
