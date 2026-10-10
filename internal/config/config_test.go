package config

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func fixture() (SourceConfig, Schema) {
	s := SourceConfig{Organization: "parametron-io", Repositories: []string{"parametron"}, Projects: map[Profile]ProjectBinding{}}
	schema := Schema{Organizations: []Organization{{ID: "org-from-discovery", Login: s.Organization}}, Repositories: []Repository{{ID: "repo-from-discovery", Owner: s.Organization, Name: "parametron"}}}
	for i, p := range profiles {
		b := ProjectBinding{Number: i + 1, Fields: map[FieldRole]string{}, StatusOptions: map[StatusRole]string{}}
		project := Project{ID: string(p) + "-project", Owner: s.Organization, Number: b.Number}
		for _, role := range fieldRoles(p) {
			b.Fields[role] = string(role) + " name"
			kind := SingleSelect
			switch role {
			case Estimate, PriorityScore, RoadmapOrder:
				kind = Number
			case StartDate:
				kind = Date
			}
			f := Field{ID: string(p) + "-" + string(role), Name: b.Fields[role], Kind: kind}
			if role == Status {
				for _, status := range statusRoles(p) {
					name := string(status) + " name"
					b.StatusOptions[status] = name
					f.Options = append(f.Options, Option{ID: string(p) + "-" + string(status), Name: name})
				}
			}
			project.Fields = append(project.Fields, f)
		}
		s.Projects[p] = b
		schema.Projects = append(schema.Projects, project)
	}
	return s, schema
}
func TestParse(t *testing.T) {
	s, _ := fixture()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, input string
		wantErr     bool
	}{
		{"valid", string(data), false},
		{"malformed", "{", true},
		{"unknown", `{"unknown":true}`, true},
		{"nested unknown", strings.Replace(string(data), `"number":1`, `"unexpected":1,"number":1`, 1), true},
		{"duplicate property", strings.Replace(string(data), `"number":1`, `"number":2,"number":1`, 1), true},
		{"duplicate binding", strings.Replace(string(data), `"ready":"ready name"`, `"ready":"other","ready":"ready name"`, 1), true},
		{"user authored ID", strings.Replace(string(data), `"number":1`, `"id":"node-id","number":1`, 1), true},
		{"trailing value", string(data) + " {}", true},
		{"trailing garbage", string(data) + " x", true},
		{"null", "null", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(tc.input))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, s) {
				t.Fatal("source changed")
			}
		})
	}
}

func TestEveryRequiredBinding(t *testing.T) {
	for _, profile := range profiles {
		for _, role := range fieldRoles(profile) {
			t.Run(string(profile)+"/field/"+string(role), func(t *testing.T) {
				s, _ := fixture()
				delete(s.Projects[profile].Fields, role)
				if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "fields."+string(role)) {
					t.Fatalf("missing field accepted: %v", err)
				}
			})
		}
		for _, role := range statusRoles(profile) {
			t.Run(string(profile)+"/option/"+string(role), func(t *testing.T) {
				s, _ := fixture()
				delete(s.Projects[profile].StatusOptions, role)
				if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "status_options."+string(role)) {
					t.Fatalf("missing status accepted: %v", err)
				}
			})
		}
	}
}

func TestDocumentedExample(t *testing.T) {
	data, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(data), "```json\n")
	if !ok {
		t.Fatal("example missing")
	}
	example, _, ok := strings.Cut(rest, "\n```")
	if !ok {
		t.Fatal("example terminator missing")
	}
	if _, err := Parse(strings.NewReader(example)); err != nil {
		t.Fatal(err)
	}
}
func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SourceConfig)
		want   string
	}{
		{"organization", func(s *SourceConfig) { s.Organization = " " }, "organization"},
		{"empty repositories", func(s *SourceConfig) { s.Repositories = nil }, "repositories"},
		{"empty repository", func(s *SourceConfig) { s.Repositories[0] = "" }, "repositories[0]"},
		{"duplicate repository", func(s *SourceConfig) { s.Repositories = append(s.Repositories, s.Repositories[0]) }, "duplicate repository"},
		{"missing project", func(s *SourceConfig) { delete(s.Projects, Engineering) }, "projects.engineering"},
		{"missing field", func(s *SourceConfig) { delete(s.Projects[Engineering].Fields, StartDate) }, "fields.start_date"},
		{"missing option", func(s *SourceConfig) { delete(s.Projects[BugTracker].StatusOptions, ToTriage) }, "status_options.to_triage"},
		{"zero number", func(s *SourceConfig) { b := s.Projects[Engineering]; b.Number = 0; s.Projects[Engineering] = b }, "number"},
		{"negative number", func(s *SourceConfig) { b := s.Projects[Engineering]; b.Number = -1; s.Projects[Engineering] = b }, "number"},
		{"duplicate project", func(s *SourceConfig) { b := s.Projects[BugTracker]; b.Number = 1; s.Projects[BugTracker] = b }, "already bound"},
		{"duplicate field", func(s *SourceConfig) {
			s.Projects[Engineering].Fields[Priority] = s.Projects[Engineering].Fields[Status]
		}, "already bound"},
		{"duplicate option", func(s *SourceConfig) {
			s.Projects[Engineering].StatusOptions[Ready] = s.Projects[Engineering].StatusOptions[Backlog]
		}, "already bound"},
		{"unknown profile", func(s *SourceConfig) { s.Projects["custom"] = ProjectBinding{} }, "unknown profile"},
		{"unknown field", func(s *SourceConfig) { s.Projects[Engineering].Fields["custom"] = "Custom" }, "unsupported field"},
		{"Engineering rejects score", func(s *SourceConfig) { s.Projects[Engineering].Fields[PriorityScore] = "Priority Score" }, "unsupported field"},
		{"Bug Tracker rejects roadmap", func(s *SourceConfig) { s.Projects[BugTracker].Fields[RoadmapOrder] = "Roadmap Order" }, "unsupported field"},
		{"contradictory status", func(s *SourceConfig) { s.Projects[BugTracker].StatusOptions[Blocked] = "Blocked" }, "unsupported status"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fixture()
			tc.mutate(&s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
			for i := 0; i < 20; i++ {
				if again := s.Validate(); again.Error() != err.Error() {
					t.Fatal("unstable diagnostics")
				}
			}
			if _, err := Resolve(s, Schema{}); err == nil {
				t.Fatal("Resolve bypassed validation")
			}
		})
	}
}
func TestResolution(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Schema)
		want   string
	}{
		{"missing organization", func(s *Schema) { s.Organizations = nil }, "organization"},
		{"ambiguous organization", func(s *Schema) { s.Organizations = append(s.Organizations, s.Organizations[0]) }, "found 2"},
		{"missing repository", func(s *Schema) { s.Repositories = nil }, "repositories[0]"},
		{"ambiguous repository", func(s *Schema) { s.Repositories = append(s.Repositories, s.Repositories[0]) }, "found 2"},
		{"missing project", func(s *Schema) { s.Projects = nil }, "found 0"},
		{"wrong owner", func(s *Schema) { s.Projects[0].Owner = "other" }, "found 0"},
		{"ambiguous project", func(s *Schema) { s.Projects = append(s.Projects, s.Projects[0]) }, "found 2"},
		{"missing field", func(s *Schema) { s.Projects[0].Fields = s.Projects[0].Fields[1:] }, "fields.status"},
		{"ambiguous field", func(s *Schema) { s.Projects[0].Fields = append(s.Projects[0].Fields, s.Projects[0].Fields[0]) }, "found 2"},
		{"incompatible status", func(s *Schema) { s.Projects[0].Fields[0].Kind = Date }, "incompatible kind"},
		{"incompatible date", func(s *Schema) { s.Projects[0].Fields[4].Kind = Number }, "incompatible kind"},
		{"incompatible score", func(s *Schema) { s.Projects[1].Fields[5].Kind = SingleSelect }, "incompatible kind"},
		{"incompatible roadmap", func(s *Schema) { s.Projects[0].Fields[5].Kind = SingleSelect }, "fields.roadmap_order"},
		{"missing option", func(s *Schema) { s.Projects[0].Fields[0].Options = nil }, "status_options.backlog"},
		{"ambiguous option", func(s *Schema) { f := &s.Projects[0].Fields[0]; f.Options = append(f.Options, f.Options[0]) }, "found 2"},
		{"exact case", func(s *Schema) { s.Projects[0].Fields[0].Name = "STATUS NAME" }, "found 0"},
		{"empty ID", func(s *Schema) { s.Projects[0].Fields[0].ID = "" }, "ID is empty"},
		{"duplicate field ID", func(s *Schema) { s.Projects[0].Fields[1].ID = s.Projects[0].Fields[0].ID }, "duplicates field"},
		{"duplicate option ID", func(s *Schema) { f := &s.Projects[0].Fields[0]; f.Options[1].ID = f.Options[0].ID }, "duplicates status"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source, schema := fixture()
			tc.mutate(&schema)
			got, err := Resolve(source, schema)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
			if !reflect.DeepEqual(got, ResolvedConfig{}) {
				t.Fatal("partial result returned")
			}
		})
	}
}
func TestResolvedRepresentation(t *testing.T) {
	source, schema := fixture()
	got, err := Resolve(source, schema)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.TypeOf(source) == reflect.TypeOf(got) {
		t.Fatal("source and runtime types coincide")
	}
	if got.Organization.ID != schema.Organizations[0].ID || got.Repositories[0].ID != schema.Repositories[0].ID {
		t.Fatal("identity mismatch")
	}
	for i, p := range profiles {
		resolved := got.Engineering
		if p == BugTracker {
			resolved = got.BugTracker
		}
		if resolved.ID != schema.Projects[i].ID {
			t.Fatal("project identity mismatch")
		}
		for _, f := range schema.Projects[i].Fields {
			for role, name := range source.Projects[p].Fields {
				if name == f.Name && resolved.Fields[role] != f.ID {
					t.Fatal("field identity mismatch")
				}
			}
			for _, o := range f.Options {
				for role, name := range source.Projects[p].StatusOptions {
					if name == o.Name && resolved.StatusOptions[role] != o.ID {
						t.Fatal("option identity mismatch")
					}
				}
			}
		}
	}
	schema.Projects[0].Fields[0].ID = "changed"
	source.Projects[Engineering].StatusOptions[Backlog] = "changed"
	if got.Engineering.Fields[Status] == "changed" || got.Engineering.StatusOptions[Backlog] == "changed" {
		t.Fatal("resolved maps alias inputs")
	}
}

func TestRoadmapFieldResolution(t *testing.T) {
	source, schema := fixture()
	got, err := Resolve(source, schema)
	if err != nil {
		t.Fatal(err)
	}
	if got.Engineering.Fields[RoadmapOrder] != "engineering-roadmap_order" || got.Engineering.Fields[PriorityScore] != "" || got.BugTracker.Fields[RoadmapOrder] != "" || got.BugTracker.Fields[PriorityScore] == "" {
		t.Fatal("profile field isolation", got)
	}
	for _, kind := range []FieldKind{SingleSelect, Date} {
		source, schema = fixture()
		schema.Projects[0].Fields[5].Kind = kind
		result, err := Resolve(source, schema)
		if err == nil || !reflect.DeepEqual(result, ResolvedConfig{}) {
			t.Fatal("non-number roadmap accepted", result, err)
		}
	}
}
