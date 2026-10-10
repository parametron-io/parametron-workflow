package github

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
)

func sourceConfig() config.SourceConfig {
	s := config.SourceConfig{Organization: "org", Repositories: []string{"repo", "other"}, Projects: map[config.Profile]config.ProjectBinding{}}
	for i, p := range []config.Profile{config.Engineering, config.BugTracker} {
		b := config.ProjectBinding{Number: i + 1, Fields: map[config.FieldRole]string{config.Status: "Status", config.Priority: "Priority", config.Effort: "Effort", config.Estimate: "Estimate", config.StartDate: "Start date"}, StatusOptions: map[config.StatusRole]string{config.Backlog: "Backlog", config.Ready: "Ready", config.InProgress: "In Progress", config.InReview: "In Review", config.Done: "Done"}}
		if p == config.Engineering {
			b.Fields[config.RoadmapOrder] = "Roadmap Order"
			b.StatusOptions[config.Blocked] = "Blocked"
		} else {
			b.Fields[config.PriorityScore] = "Priority Score"
			b.StatusOptions[config.ToTriage] = "To Triage"
		}
		s.Projects[p] = b
	}
	return s
}
func fieldsJSON(project string) []any {
	opts := []any{}
	for _, name := range []string{"Backlog", "Ready", "In Progress", "In Review", "Done", "Blocked", "To Triage"} {
		opts = append(opts, map[string]any{"id": project + name, "name": name})
	}
	result := []any{}
	for _, f := range []struct{ name, kind string }{{"Status", "SINGLE_SELECT"}, {"Priority", "SINGLE_SELECT"}, {"Effort", "SINGLE_SELECT"}, {"Estimate", "NUMBER"}, {"Start date", "DATE"}, {"Priority Score", "NUMBER"}, {"Roadmap Order", "NUMBER"}, {"Unrelated", "TEXT"}} {
		w := map[string]any{"id": project + f.name, "name": f.name, "dataType": f.kind}
		if f.kind == "SINGLE_SELECT" {
			w["options"] = opts
		}
		result = append(result, w)
	}
	return result
}
func schemaHandler(t *testing.T, change func([]any) []any) func(request) any {
	return func(q request) any {
		switch {
		case strings.Contains(q.Query, "issueTypes("):
			return connectionJSON("Organization", []any{map[string]any{"id": "IT", "name": "Task"}}, false, "")
		case strings.Contains(q.Query, "projectV2(number:"):
			n := int(q.Variables["number"].(float64))
			return map[string]any{"organization": map[string]any{"projectV2": map[string]any{"id": fmt.Sprintf("P%d", n), "number": n}}}
		case strings.Contains(q.Query, "organization(login:"):
			return map[string]any{"organization": map[string]any{"id": "O", "login": "org"}}
		case strings.Contains(q.Query, "repository(owner:"):
			r := repoJSON()
			r["name"] = q.Variables["name"]
			r["id"] = "R" + q.Variables["name"].(string)
			return map[string]any{"repository": r}
		default:
			id := q.Variables["id"].(string)
			fields := fieldsJSON(id)
			if change != nil {
				fields = change(fields)
			}
			if q.Variables["cursor"] == nil {
				return connectionJSON("ProjectV2", fields[:1], true, "second")
			}
			if q.Variables["cursor"] != "second" {
				t.Error("wrong field cursor")
			}
			return connectionJSON("ProjectV2", fields[1:], false, "")
		}
	}
}
func TestDiscoverSchema(t *testing.T) {
	client := adapter(t, schemaHandler(t, nil))
	schema, err := client.DiscoverSchema(context.Background(), sourceConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(schema.Organizations) != 1 || schema.Organizations[0].ID != "O" || len(schema.Repositories) != 2 || schema.Repositories[1].Name != "other" || len(schema.Projects) != 2 || schema.Projects[1].ID != "P2" {
		t.Fatalf("%+v", schema)
	}
	got, err := config.Resolve(sourceConfig(), schema)
	if err != nil {
		t.Fatal(err)
	}
	if got.Engineering.Fields[config.RoadmapOrder] != "P1Roadmap Order" || got.Engineering.Fields[config.Estimate] != "P1Estimate" || got.Engineering.Fields[config.StartDate] != "P1Start date" || got.BugTracker.Fields[config.PriorityScore] != "P2Priority Score" || got.Engineering.StatusOptions[config.Ready] != "P1Ready" {
		t.Fatalf("%+v", got)
	}
}
func TestSchemaFailures(t *testing.T) {
	for _, test := range []string{"empty ID", "unsupported", "missing options", "empty option ID", "missing field", "duplicate field", "wrong kind"} {
		t.Run(test, func(t *testing.T) {
			client := adapter(t, schemaHandler(t, func(fields []any) []any {
				switch test {
				case "empty ID":
					fields[0].(map[string]any)["id"] = ""
				case "unsupported":
					fields[0].(map[string]any)["dataType"] = "TEXT"
				case "missing options":
					delete(fields[0].(map[string]any), "options")
				case "empty option ID":
					fields[0].(map[string]any)["options"].([]any)[0].(map[string]any)["id"] = ""
				case "missing field":
					fields = fields[1:]
				case "duplicate field":
					fields = append(fields, fields[0])
				case "wrong kind":
					fields[3].(map[string]any)["dataType"] = "DATE"
				}
				return fields
			}))
			schema, err := client.DiscoverSchema(context.Background(), sourceConfig())
			if test == "missing field" || test == "duplicate field" || test == "wrong kind" {
				if err != nil {
					t.Fatal(err)
				}
				_, err = config.Resolve(sourceConfig(), schema)
				if err == nil {
					t.Fatal("resolution accepted incompatible schema")
				}
			} else {
				category(t, err, Malformed)
				if len(schema.Projects) != 0 {
					t.Fatal("partial schema")
				}
			}
		})
	}
	for _, kind := range []string{"organization", "project"} {
		t.Run(kind, func(t *testing.T) {
			base := schemaHandler(t, nil)
			client := adapter(t, func(q request) any {
				if kind == "organization" && strings.Contains(q.Query, "{id login}") {
					return map[string]any{"organization": map[string]any{"id": "", "login": "org"}}
				}
				if kind == "project" && strings.Contains(q.Query, "projectV2(number:") {
					return map[string]any{"organization": map[string]any{"projectV2": map[string]any{"number": 1}}}
				}
				return base(q)
			})
			_, err := client.DiscoverSchema(context.Background(), sourceConfig())
			category(t, err, Malformed)
		})
	}
}
func TestPaginationFailure(t *testing.T) {
	for _, test := range []string{"missing metadata", "missing flag", "missing nodes", "null node", "empty cursor", "repeated cursor", "wrong type"} {
		t.Run(test, func(t *testing.T) {
			client := adapter(t, func(request) any {
				c := connectionJSON("Issue", []any{}, true, "repeat").(map[string]any)
				n := c["node"].(map[string]any)
				r := n["result"].(map[string]any)
				switch test {
				case "missing metadata":
					delete(r, "pageInfo")
				case "missing flag":
					delete(r["pageInfo"].(map[string]any), "hasNextPage")
				case "missing nodes":
					delete(r, "nodes")
				case "null node":
					r["nodes"] = []any{nil}
				case "empty cursor":
					r["pageInfo"].(map[string]any)["endCursor"] = ""
				case "wrong type":
					n["__typename"] = "PullRequest"
				}
				return c
			})
			_, err := client.connection(context.Background(), "I", "Issue", "labels", "name")
			category(t, err, Malformed)
		})
	}
}

func TestMissingDiscoveryResources(t *testing.T) {
	for _, kind := range []string{"organization", "repository", "project"} {
		t.Run(kind, func(t *testing.T) {
			base := schemaHandler(t, nil)
			client := adapter(t, func(q request) any {
				if kind == "organization" && strings.Contains(q.Query, "{id login}") {
					return map[string]any{"organization": nil}
				}
				if kind == "repository" && strings.Contains(q.Query, "repository(owner:") {
					return map[string]any{"repository": nil}
				}
				if kind == "project" && strings.Contains(q.Query, "projectV2(number:") {
					return map[string]any{"organization": map[string]any{"projectV2": nil}}
				}
				return base(q)
			})
			schema, err := client.DiscoverSchema(context.Background(), sourceConfig())
			category(t, err, NotFound)
			if len(schema.Organizations) != 0 || len(schema.Repositories) != 0 || len(schema.Projects) != 0 {
				t.Fatal("partial schema returned")
			}
		})
	}
}
func TestLaterPageFailure(t *testing.T) {
	client := adapter(t, func(q request) any {
		if q.Variables["cursor"] == nil {
			return connectionJSON("Issue", []any{map[string]any{"name": "first"}}, true, "next")
		}
		return map[string]any{"node": nil}
	})
	result, err := client.labels(context.Background(), "I", "Issue")
	category(t, err, NotFound)
	if result != nil {
		t.Fatal("partial collection returned")
	}
}

func TestIssueTypeDiscoveryPaginationAndMalformed(t *testing.T) {
	for _, test := range []string{"valid", "blank ID", "blank name", "duplicate name", "duplicate ID", "missing connection"} {
		t.Run(test, func(t *testing.T) {
			base := schemaHandler(t, nil)
			client := adapter(t, func(q request) any {
				if !strings.Contains(q.Query, "issueTypes(") {
					return base(q)
				}
				if test == "missing connection" {
					return map[string]any{"node": map[string]any{"__typename": "Organization"}}
				}
				if q.Variables["cursor"] == nil {
					return connectionJSON("Organization", []any{map[string]any{"id": "TASK", "name": "Task"}}, true, "types-next")
				}
				id, name := "BUG", "Bug"
				switch test {
				case "blank ID":
					id = ""
				case "blank name":
					name = ""
				case "duplicate name":
					name = "Task"
				case "duplicate ID":
					id = "TASK"
				}
				return connectionJSON("Organization", []any{map[string]any{"id": id, "name": name}}, false, "")
			})
			schema, err := client.DiscoverSchema(context.Background(), sourceConfig())
			if test == "valid" {
				if err != nil || !reflect.DeepEqual(schema.IssueTypes, []config.IssueType{{ID: "TASK", Name: "Task"}, {ID: "BUG", Name: "Bug"}}) {
					t.Fatal(schema.IssueTypes, err)
				}
			} else {
				category(t, err, Malformed)
			}
		})
	}
}

func TestRoadmapSchemaBindingFailures(t *testing.T) {
	for _, mode := range []string{"missing", "wrong kind"} {
		t.Run(mode, func(t *testing.T) {
			client := adapter(t, schemaHandler(t, func(fields []any) []any {
				for i, f := range fields {
					if f.(map[string]any)["name"] == "Roadmap Order" {
						if mode == "missing" {
							return append(fields[:i], fields[i+1:]...)
						}
						f.(map[string]any)["dataType"] = "DATE"
					}
				}
				return fields
			}))
			schema, err := client.DiscoverSchema(context.Background(), sourceConfig())
			if err != nil {
				t.Fatal(err)
			}
			_, err = config.Resolve(sourceConfig(), schema)
			if err == nil || !strings.Contains(err.Error(), "roadmap_order") {
				t.Fatal("malformed roadmap binding accepted", err)
			}
		})
	}
}
