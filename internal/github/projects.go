package github

import (
	"context"
	"encoding/json"
	"github.com/parametron-io/parametron-workflow/internal/config"
	"time"
)

const valueSelection = `__typename ... on ProjectV2ItemFieldSingleSelectValue{optionId field{... on ProjectV2SingleSelectField{id}}} ... on ProjectV2ItemFieldNumberValue{number field{... on ProjectV2Field{id}}} ... on ProjectV2ItemFieldDateValue{date field{... on ProjectV2Field{id}}}`

// Read identities even for unsupported value types so a changed configured
// field cannot silently masquerade as unset.
func fieldValueSelection() string {
	selection := valueSelection
	for _, typ := range []string{"Text", "Iteration", "MultiSelect", "Label", "Milestone", "PullRequest", "Repository", "Reviewer", "User"} {
		selection += ` ... on ProjectV2ItemField` + typ + `Value{field{... on ProjectV2FieldCommon{id}}}`
	}
	selection += ` ... on ProjectV2ItemIssueFieldValue{field{... on ProjectV2FieldCommon{id}}}`
	return selection
}

func (t *Transport) ProjectItems(ctx context.Context, contentID, projectID string, fields map[string]config.FieldKind) ([]ProjectItem, error) {
	if blank(contentID) || blank(projectID) {
		return nil, failure(Permanent)
	}
	for id, kind := range fields {
		if blank(id) || (kind != config.SingleSelect && kind != config.Number && kind != config.Date) {
			return nil, failure(Permanent)
		}
	}
	var data struct {
		Node *struct {
			Typename string `json:"__typename"`
		}
	}
	if err := t.query(ctx, `query($id:ID!){node(id:$id){__typename}}`, map[string]any{"id": contentID}, &data); err != nil {
		return nil, err
	}
	if data.Node == nil {
		return nil, failure(NotFound)
	}
	typ := data.Node.Typename
	if typ != "Issue" && typ != "PullRequest" {
		return nil, failure(Malformed)
	}
	nodes, err := t.connection(ctx, contentID, typ, "projectItems", `id isArchived project{id} content{__typename ... on Issue{id} ... on PullRequest{id} ... on DraftIssue{id}}`)
	if err != nil {
		return nil, err
	}
	result := []ProjectItem{}
	for _, n := range nodes {
		var w struct {
			ID         string
			IsArchived *bool
			Project    *struct{ ID string }
			Content    *struct {
				ID       string
				Typename string `json:"__typename"`
			}
		}
		if json.Unmarshal(n, &w) != nil || blank(w.ID) || w.IsArchived == nil || w.Project == nil || blank(w.Project.ID) {
			return nil, failure(Malformed)
		}
		if w.Project.ID != projectID {
			continue
		}
		if w.Content == nil || w.Content.ID != contentID || w.Content.Typename != typ {
			return nil, failure(Malformed)
		}
		item := ProjectItem{ID: w.ID, Archived: *w.IsArchived, ProjectID: projectID, Content: &Content{ID: w.Content.ID, Kind: typ}, Values: map[string]FieldValue{}}
		values, err := t.connection(ctx, w.ID, "ProjectV2Item", "fieldValues", fieldValueSelection())
		if err != nil {
			return nil, err
		}
		for _, v := range values {
			var w struct {
				Typename       string `json:"__typename"`
				Field          *struct{ ID string }
				OptionID, Date *string
				Number         *float64
			}
			if json.Unmarshal(v, &w) != nil || blank(w.Typename) {
				return nil, failure(Malformed)
			}
			if w.Field == nil || blank(w.Field.ID) {
				return nil, failure(Malformed)
			}
			kind, ok := fields[w.Field.ID]
			if !ok {
				continue
			}
			value := FieldValue{}
			switch w.Typename {
			case "ProjectV2ItemFieldSingleSelectValue":
				value.Kind = config.SingleSelect
				if w.OptionID == nil || blank(*w.OptionID) {
					return nil, failure(Malformed)
				}
				value.OptionID = *w.OptionID
			case "ProjectV2ItemFieldNumberValue":
				value.Kind = config.Number
				if w.Number == nil {
					return nil, failure(Malformed)
				}
				value.Number = *w.Number
			case "ProjectV2ItemFieldDateValue":
				value.Kind = config.Date
				if w.Date == nil || blank(*w.Date) {
					return nil, failure(Malformed)
				}
				if _, err := time.Parse(time.DateOnly, *w.Date); err != nil {
					return nil, failure(Malformed)
				}
				value.Date = *w.Date
			default:
				return nil, failure(Malformed)
			}
			if kind != value.Kind {
				return nil, failure(Malformed)
			}
			if _, exists := item.Values[w.Field.ID]; exists {
				return nil, failure(Malformed)
			}
			item.Values[w.Field.ID] = value
		}
		result = append(result, item)
	}
	return result, nil
}
