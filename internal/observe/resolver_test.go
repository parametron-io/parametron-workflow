package observe

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func deployment() config.ResolvedConfig {
	project := func(id string, bug bool) config.ResolvedProject {
		p := config.ResolvedProject{ID: id, Fields: map[config.FieldRole]string{}, StatusOptions: map[config.StatusRole]string{}}
		for _, role := range []config.FieldRole{config.Status, config.Priority, config.Effort, config.Estimate, config.StartDate} {
			p.Fields[role] = id + "-" + string(role)
		}
		for _, role := range []config.StatusRole{config.Backlog, config.Ready, config.InProgress, config.InReview, config.Done} {
			p.StatusOptions[role] = id + "-" + string(role)
		}
		if bug {
			p.Fields[config.PriorityScore] = id + "-priority_score"
			p.StatusOptions[config.ToTriage] = id + "-to_triage"
		} else {
			p.Fields[config.RoadmapOrder] = id + "-roadmap_order"
			p.StatusOptions[config.Blocked] = id + "-blocked"
		}
		return p
	}
	return config.ResolvedConfig{Organization: config.Organization{ID: "org-id", Login: "Org"}, Repositories: []config.Repository{{ID: "repo-id", Owner: "Org", Name: "Repo"}}, Engineering: project("engineering", false), BugTracker: project("bugs", true)}
}

const prefix = `"repository":{"owner":{"login":"Org"},"name":"Repo"},`

func event(payload string) storage.Event {
	return storage.Event{Sequence: 7, Delivery: storage.Delivery{ID: "delivery", EventName: "notification", Payload: []byte(payload)}}
}
func TestResolver(t *testing.T) {
	r, err := NewResolver(deployment())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, payload, kind string
		err                 error
	}{
		{"issue", `{` + prefix + `"issue":{"number":12}}`, "issue", nil},
		{"pr", `{` + prefix + `"pull_request":{"number":12}}`, "pull_request", nil},
		{"pr comment", `{` + prefix + `"issue":{"number":12,"pull_request":{"url":"historical"}},"comment":{}}`, "pull_request", nil},
		{"issue comment", `{` + prefix + `"issue":{"number":12},"comment":{}}`, "issue", nil},
		{"review", `{` + prefix + `"pull_request":{"number":12},"review":{}}`, "pull_request", nil},
		{"normalize", `{"repository":{"owner":{"login":"ORG"},"name":"REPO"},"issue":{"number":12}}`, "issue", nil},
		{"state ignored", `{` + prefix + `"issue":{"number":12,"node_id":false,"title":{},"body":[],"state":42,"labels":false,"assignees":{},"relationships":"stale"}}`, "issue", nil},
		{"other repo", `{"repository":{"owner":{"login":"Org"},"name":"Other"},"issue":{"number":12}}`, "", ErrRepository},
		{"other org", `{"repository":{"owner":{"login":"Other"},"name":"Repo"},"issue":{"number":12}}`, "", ErrRepository},
		{"malformed", `{`, "", ErrIdentity},
		{"array", `[]`, "", ErrIdentity},
		{"null", `null`, "", ErrIdentity},
		{"trailing", `{` + prefix + `"issue":{"number":12}} {}`, "", ErrIdentity},
		{"no repo", `{"issue":{"number":12}}`, "", ErrIdentity},
		{"no owner", `{"repository":{"name":"Repo"},"issue":{"number":12}}`, "", ErrIdentity},
		{"no number", `{` + prefix + `"issue":{}}`, "", ErrIdentity},
		{"string number", `{` + prefix + `"issue":{"number":"12"}}`, "", ErrIdentity},
		{"fraction", `{` + prefix + `"issue":{"number":12.5}}`, "", ErrIdentity},
		{"zero", `{` + prefix + `"issue":{"number":0}}`, "", ErrIdentity},
		{"negative", `{` + prefix + `"issue":{"number":-1}}`, "", ErrIdentity},
		{"overflow", `{` + prefix + `"issue":{"number":9223372036854775808}}`, "", ErrIdentity},
		{"ambiguous", `{` + prefix + `"issue":{"number":12},"pull_request":{"number":12}}`, "", ErrIdentity},
		{"unsupported", `{` + prefix + `"number":12,"projects_v2_item":{"content_node_id":"node"}}`, "", ErrIdentity},
		{"null candidate", `{` + prefix + `"issue":null}`, "", ErrIdentity},
		{"bad marker", `{` + prefix + `"issue":{"number":12,"pull_request":true}}`, "", ErrIdentity},
		{"duplicate number", `{` + prefix + `"issue":{"number":12,"number":13}}`, "", ErrIdentity},
		{"duplicate candidate", `{` + prefix + `"issue":{"number":12},"issue":{"number":13}}`, "", ErrIdentity},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := r.Resolve(context.Background(), event(c.payload))
			if !errors.Is(err, c.err) {
				t.Fatalf("error=%v want %v", err, c.err)
			}
			if err == nil {
				want := storage.Resource{Owner: "org", Repository: "repo", Kind: c.kind, Number: 12}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("got %+v want %+v", got, want)
				}
			}
		})
	}
}
func TestConstruction(t *testing.T) {
	cases := []struct {
		name  string
		alter func(*config.ResolvedConfig)
	}{
		{"no org", func(d *config.ResolvedConfig) { d.Organization.Login = "" }},
		{"no repos", func(d *config.ResolvedConfig) { d.Repositories = nil }},
		{"collision", func(d *config.ResolvedConfig) {
			d.Repositories = append(d.Repositories, config.Repository{Owner: "org", Name: "repo"})
		}},
		{"wrong owner", func(d *config.ResolvedConfig) { d.Repositories[0].Owner = "other" }},
		{"missing field", func(d *config.ResolvedConfig) { delete(d.Engineering.Fields, config.Status) }},
		{"duplicate field", func(d *config.ResolvedConfig) {
			d.Engineering.Fields[config.Priority] = d.Engineering.Fields[config.Status]
		}},
		{"ambiguous status", func(d *config.ResolvedConfig) {
			d.Engineering.StatusOptions[config.Ready] = d.Engineering.StatusOptions[config.Done]
		}},
		{"same projects", func(d *config.ResolvedConfig) { d.BugTracker.ID = d.Engineering.ID }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := deployment()
			c.alter(&d)
			if _, err := NewResolver(d); !errors.Is(err, ErrConfiguration) {
				t.Fatal(err)
			}
		})
	}
}
