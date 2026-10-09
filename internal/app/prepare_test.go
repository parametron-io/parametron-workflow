package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
)

func preparationFixture() (config.SourceConfig, config.Schema) {
	s := config.SourceConfig{Organization: "org", Repositories: []string{"repo"}, Projects: map[config.Profile]config.ProjectBinding{}}
	schema := config.Schema{Organizations: []config.Organization{{ID: "O", Login: "org"}}, Repositories: []config.Repository{{ID: "R", Owner: "org", Name: "repo"}}}
	for i, p := range []config.Profile{config.Engineering, config.BugTracker} {
		b := config.ProjectBinding{Number: i + 1, Fields: map[config.FieldRole]string{}, StatusOptions: map[config.StatusRole]string{}}
		project := config.Project{ID: string(p), Owner: "org", Number: b.Number}
		roles := []config.FieldRole{config.Status, config.Priority, config.Effort, config.Estimate, config.StartDate}
		statuses := []config.StatusRole{config.Backlog, config.Ready, config.InProgress, config.InReview, config.Done}
		if p == config.BugTracker {
			roles = append(roles, config.PriorityScore)
			statuses = append(statuses, config.ToTriage)
		} else {
			statuses = append(statuses, config.Blocked)
		}
		for _, r := range roles {
			b.Fields[r] = string(r)
			kind := config.SingleSelect
			if r == config.Estimate || r == config.PriorityScore {
				kind = config.Number
			}
			if r == config.StartDate {
				kind = config.Date
			}
			f := config.Field{ID: string(r), Name: string(r), Kind: kind}
			if r == config.Status {
				for _, status := range statuses {
					b.StatusOptions[status] = string(status)
					f.Options = append(f.Options, config.Option{ID: string(status), Name: string(status)})
				}
			}
			project.Fields = append(project.Fields, f)
		}
		s.Projects[p] = b
		schema.Projects = append(schema.Projects, project)
	}
	return s, schema
}
func TestPrepare(t *testing.T) {
	source, schema := preparationFixture()
	calls := 0
	client := &github.Fake{DiscoverSchemaFunc: func(ctx context.Context, s config.SourceConfig) (config.Schema, error) {
		calls++
		if !reflect.DeepEqual(s, source) {
			t.Fatal("wrong source")
		}
		return schema, nil
	}}
	result, err := Prepare(context.Background(), source, client)
	if err != nil || result.Deployment == nil || result.Deployment.Organization.ID != "O" || calls != 1 {
		t.Fatal(result, err, calls)
	}
	schema.Projects[0].Fields = nil
	result, err = Prepare(context.Background(), source, client)
	if err == nil || result.Deployment != nil {
		t.Fatal("partial configuration accepted")
	}
	injected := errors.New("discovery unavailable")
	client.DiscoverSchemaFunc = func(context.Context, config.SourceConfig) (config.Schema, error) { return config.Schema{}, injected }
	_, err = Prepare(context.Background(), source, client)
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	_, err = Prepare(context.Background(), source, nil)
	if err == nil {
		t.Fatal("nil client accepted")
	}
	client.DiscoverSchemaFunc = func(context.Context, config.SourceConfig) (config.Schema, error) {
		t.Fatal("invalid source triggered discovery")
		return config.Schema{}, nil
	}
	_, err = Prepare(context.Background(), config.SourceConfig{}, client)
	if err == nil {
		t.Fatal("invalid source accepted")
	}
}
