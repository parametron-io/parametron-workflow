package github

import (
	"context"
	"encoding/json"
	"math"
)

// NumericFields grants only exact NUMBER field set/unset operations.
type NumericFields interface {
	SetProjectNumber(context.Context, string, string, string, float64) error
	ClearProjectNumber(context.Context, string, string, string) error
}

func (t *Transport) numberTarget(ctx context.Context, project, item, field string) error {
	var data struct {
		Item *struct {
			Typename string `json:"__typename"`
			ID       string
			Project  *struct{ ID string }
		}
		Project *struct {
			Typename string `json:"__typename"`
			ID       string
		}
	}
	if err := t.query(ctx, `query($item:ID!,$project:ID!){item:node(id:$item){__typename ... on ProjectV2Item{id project{id}}} project:node(id:$project){__typename ... on ProjectV2{id}}}`, map[string]any{"item": item, "project": project}, &data); err != nil {
		return err
	}
	if data.Item == nil || data.Project == nil {
		return failure(NotFound)
	}
	if data.Item.Typename != "ProjectV2Item" || data.Item.ID != item || data.Item.Project == nil || data.Item.Project.ID != project || data.Project.Typename != "ProjectV2" || data.Project.ID != project {
		return failure(Malformed)
	}
	fields, err := t.connection(ctx, project, "ProjectV2", "fields", fieldSelection)
	if err != nil {
		return err
	}
	found := false
	for _, raw := range fields {
		var f struct{ ID, DataType string }
		if json.Unmarshal(raw, &f) != nil || !validIDs(f.ID) {
			return failure(Malformed)
		}
		if f.ID == field {
			if found || f.DataType != "NUMBER" {
				return failure(Malformed)
			}
			found = true
		}
	}
	if !found {
		return failure(Malformed)
	}
	return ctx.Err()
}
func (t *Transport) numberValue(ctx context.Context, item, field string) (float64, bool, error) {
	values, err := t.connection(ctx, item, "ProjectV2Item", "fieldValues", fieldValueSelection())
	if err != nil {
		return 0, false, err
	}
	var number float64
	found := false
	for _, raw := range values {
		var v struct {
			Typename string `json:"__typename"`
			Field    *struct{ ID string }
			Number   *float64
		}
		if json.Unmarshal(raw, &v) != nil || v.Field == nil || !validIDs(v.Field.ID) {
			return 0, false, failure(Malformed)
		}
		if v.Field.ID != field {
			continue
		}
		if found || v.Typename != "ProjectV2ItemFieldNumberValue" || v.Number == nil || math.IsNaN(*v.Number) || math.IsInf(*v.Number, 0) {
			return 0, false, failure(Malformed)
		}
		found, number = true, *v.Number
	}
	return number, found, ctx.Err()
}
func (t *Transport) numberMutation(ctx context.Context, project, item, field string, value *float64) error {
	if !validIDs(project, item, field) || value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
		return failure(Permanent)
	}
	if err := t.numberTarget(ctx, project, item, field); err != nil {
		return err
	}
	operation, input := "clearProjectV2ItemFieldValue", "ClearProjectV2ItemFieldValueInput"
	vars := map[string]any{"projectId": project, "itemId": item, "fieldId": field}
	if value != nil {
		operation, input = "updateProjectV2ItemFieldValue", "UpdateProjectV2ItemFieldValueInput"
		vars["value"] = map[string]any{"number": *value}
	}
	var data struct {
		Result *struct {
			ProjectV2Item *struct {
				ID      string
				Project *struct{ ID string }
			}
		}
	}
	if err := t.query(ctx, `mutation($input:`+input+`!){result:`+operation+`(input:$input){projectV2Item{id project{id}}}}`, map[string]any{"input": vars}, &data); err != nil {
		return err
	}
	if data.Result == nil || data.Result.ProjectV2Item == nil || data.Result.ProjectV2Item.ID != item || data.Result.ProjectV2Item.Project == nil || data.Result.ProjectV2Item.Project.ID != project {
		return failure(Malformed)
	}
	if err := t.numberTarget(ctx, project, item, field); err != nil {
		return err
	}
	got, present, err := t.numberValue(ctx, item, field)
	if err != nil {
		return err
	}
	if value == nil && present || value != nil && (!present || got != *value) {
		return failure(Malformed)
	}
	return ctx.Err()
}
func (t *Transport) SetProjectNumber(ctx context.Context, p, i, f string, v float64) error {
	return t.numberMutation(ctx, p, i, f, &v)
}
func (t *Transport) ClearProjectNumber(ctx context.Context, p, i, f string) error {
	return t.numberMutation(ctx, p, i, f, nil)
}

var _ NumericFields = (*Transport)(nil)
