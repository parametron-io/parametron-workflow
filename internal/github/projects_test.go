package github

import (
	"context"
	"github.com/parametron-io/parametron-workflow/internal/config"
	"strings"
	"testing"
)

func projectHandler(change func([]any) []any) func(request) any {
	return func(q request) any {
		if strings.Contains(q.Query, "fieldValues") {
			values := []any{map[string]any{"__typename": "ProjectV2ItemFieldSingleSelectValue", "field": map[string]any{"id": "status"}, "optionId": "ready"}, map[string]any{"__typename": "ProjectV2ItemFieldNumberValue", "field": map[string]any{"id": "estimate"}, "number": 0}, map[string]any{"__typename": "ProjectV2ItemFieldDateValue", "field": map[string]any{"id": "start"}, "date": "2026-10-09"}, map[string]any{"__typename": "ProjectV2ItemFieldTextValue", "field": map[string]any{"id": "unconfigured"}}}
			if change != nil {
				values = change(values)
			}
			if q.Variables["cursor"] == nil {
				return connectionJSON("ProjectV2Item", values[:1], true, "values")
			}
			return connectionJSON("ProjectV2Item", values[1:], false, "")
		}
		if strings.Contains(q.Query, "projectItems") {
			item := func(id, project string) any {
				return map[string]any{"id": id, "isArchived": true, "project": map[string]any{"id": project}, "content": map[string]any{"id": "I", "__typename": "Issue"}}
			}
			if q.Variables["cursor"] == nil {
				return connectionJSON("Issue", []any{item("other", "unconfigured")}, true, "items")
			}
			return connectionJSON("Issue", []any{item("item", "P")}, false, "")
		}
		return map[string]any{"node": map[string]any{"__typename": "Issue"}}
	}
}
func TestProjectItems(t *testing.T) {
	client := adapter(t, projectHandler(nil))
	got, err := client.ProjectItems(context.Background(), "I", "P", map[string]config.FieldKind{"status": config.SingleSelect, "estimate": config.Number, "start": config.Date, "unset": config.Number})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "item" || got[0].ProjectID != "P" || got[0].Content.ID != "I" || got[0].Values["status"].OptionID != "ready" || got[0].Values["start"].Date != "2026-10-09" {
		t.Fatalf("%+v", got)
	}
	if v, ok := got[0].Values["estimate"]; !ok || v.Number != 0 {
		t.Fatal("zero lost")
	}
	if _, ok := got[0].Values["unset"]; ok {
		t.Fatal("unset fabricated")
	}
	got, err = client.ProjectItems(context.Background(), "I", "absent", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}
func TestProjectValueFailures(t *testing.T) {
	for _, test := range []string{"missing option", "missing number", "missing date", "missing field ID", "duplicate", "wrong kind"} {
		t.Run(test, func(t *testing.T) {
			client := adapter(t, projectHandler(func(values []any) []any {
				switch test {
				case "missing option":
					delete(values[0].(map[string]any), "optionId")
				case "missing number":
					delete(values[1].(map[string]any), "number")
				case "missing date":
					delete(values[2].(map[string]any), "date")
				case "missing field ID":
					values[0].(map[string]any)["field"] = map[string]any{}
				case "duplicate":
					values = append(values, values[0])
				}
				return values
			}))
			fields := map[string]config.FieldKind{"status": config.SingleSelect, "estimate": config.Number, "start": config.Date}
			if test == "wrong kind" {
				fields["status"] = config.Date
			}
			_, err := client.ProjectItems(context.Background(), "I", "P", fields)
			category(t, err, Malformed)
		})
	}
}

func TestPullRequestMembership(t *testing.T) {
	base := projectHandler(nil)
	client := adapter(t, func(q request) any {
		data := base(q).(map[string]any)
		node := data["node"].(map[string]any)
		if node["__typename"] == "Issue" {
			node["__typename"] = "PullRequest"
		}
		if strings.Contains(q.Query, "projectItems") {
			result := node["result"].(map[string]any)
			for _, n := range result["nodes"].([]any) {
				n.(map[string]any)["content"].(map[string]any)["__typename"] = "PullRequest"
			}
		}
		return data
	})
	got, err := client.ProjectItems(context.Background(), "I", "P", nil)
	if err != nil || len(got) != 1 || got[0].Content.Kind != "PullRequest" || !got[0].Archived {
		t.Fatal(got, err)
	}
}
func TestIncompatibleConfiguredValue(t *testing.T) {
	client := adapter(t, projectHandler(func(values []any) []any {
		values[0] = map[string]any{"__typename": "ProjectV2ItemFieldTextValue", "field": map[string]any{"id": "status"}}
		return values
	}))
	_, err := client.ProjectItems(context.Background(), "I", "P", map[string]config.FieldKind{"status": config.SingleSelect})
	category(t, err, Malformed)
	client = adapter(t, projectHandler(func(values []any) []any { values[2].(map[string]any)["date"] = "invalid"; return values }))
	_, err = client.ProjectItems(context.Background(), "I", "P", map[string]config.FieldKind{"start": config.Date})
	category(t, err, Malformed)
}

func TestRoadmapNumberRead(t *testing.T) {
	for _, number := range []int{0, 10020} {
		client := adapter(t, projectHandler(func(values []any) []any {
			return append(values, map[string]any{"__typename": "ProjectV2ItemFieldNumberValue", "field": map[string]any{"id": "roadmap"}, "number": number})
		}))
		got, err := client.ProjectItems(context.Background(), "I", "P", map[string]config.FieldKind{"roadmap": config.Number, "unset": config.Number})
		if err != nil {
			t.Fatal(err)
		}
		v, present := got[0].Values["roadmap"]
		if !present || v.Kind != config.Number || v.Number != float64(number) {
			t.Fatal("roadmap number lost", got)
		}
		if _, present := got[0].Values["unset"]; present {
			t.Fatal("unset fabricated")
		}
	}
}
