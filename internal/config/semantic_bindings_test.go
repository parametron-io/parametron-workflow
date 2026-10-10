package config

import "testing"

func TestLiveIssueTypesAndFieldOptionCopies(t *testing.T) {
	s, schema := fixture()
	schema.IssueTypes = []IssueType{{ID: "TASK", Name: "Task"}, {ID: "BUG", Name: "Bug"}}
	for i := range schema.Projects {
		for j := range schema.Projects[i].Fields {
			f := &schema.Projects[i].Fields[j]
			if f.Name == s.Projects[profiles[i]].Fields[Priority] {
				f.Options = []Option{{ID: "HIGH", Name: "High"}}
			}
			if f.Name == s.Projects[profiles[i]].Fields[Effort] {
				f.Options = []Option{{ID: "SMALL", Name: "S"}}
			}
		}
	}
	d, err := Resolve(s, schema)
	if err != nil {
		t.Fatal(err)
	}
	if d.IssueTypes["Task"] != "TASK" || d.Engineering.FieldOptions[Priority]["High"] != "HIGH" || d.BugTracker.FieldOptions[Effort]["S"] != "SMALL" {
		t.Fatal(d)
	}
	schema.IssueTypes[0].ID = "changed"
	for i := range schema.Projects {
		for j := range schema.Projects[i].Fields {
			f := &schema.Projects[i].Fields[j]
			if len(f.Options) > 0 {
				f.Options[0].ID = "changed"
			}
		}
	}
	if d.IssueTypes["Task"] != "TASK" || d.Engineering.FieldOptions[Priority]["High"] != "HIGH" {
		t.Fatal("aliased schema")
	}
	d.Engineering.FieldOptions[Priority]["High"] = "new"
	if d.BugTracker.FieldOptions[Priority]["High"] != "HIGH" {
		t.Fatal("aliased projects")
	}
}
func TestMalformedIssueTypeDiscovery(t *testing.T) {
	for _, types := range [][]IssueType{{{ID: "", Name: "Task"}}, {{ID: "T", Name: ""}}, {{ID: "T", Name: "Task"}, {ID: "T2", Name: "Task"}}, {{ID: "T", Name: "Task"}, {ID: "T", Name: "Bug"}}} {
		s, schema := fixture()
		schema.IssueTypes = types
		if _, err := Resolve(s, schema); err == nil {
			t.Fatal(types)
		}
	}
}
func TestMalformedSemanticFieldOptions(t *testing.T) {
	for _, options := range [][]Option{{{ID: "", Name: "High"}}, {{ID: "H", Name: ""}}, {{ID: "H", Name: "High"}, {ID: "H2", Name: "High"}}, {{ID: "H", Name: "High"}, {ID: "H", Name: "Low"}}} {
		s, schema := fixture()
		for i := range schema.Projects[0].Fields {
			f := &schema.Projects[0].Fields[i]
			if f.Name == s.Projects[Engineering].Fields[Priority] {
				f.Options = options
			}
		}
		if _, err := Resolve(s, schema); err == nil {
			t.Fatal(options)
		}
	}
}
