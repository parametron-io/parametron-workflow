package app

import (
	"context"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
)

func TestConfiguredOrganizationStartupComposition(t *testing.T) {
	for _, org := range []string{"parametron-io", "prm-workflow"} {
		t.Run(org, func(t *testing.T) {
			c, f, _ := completionRuntimeFixture(t)
			c.Source.Organization = org
			discover := f.DiscoverSchemaFunc
			f.DiscoverSchemaFunc = func(ctx context.Context, s config.SourceConfig) (config.Schema, error) {
				schema, err := discover(ctx, s)
				for i := range schema.Organizations {
					schema.Organizations[i].Login = org
				}
				for i := range schema.Repositories {
					schema.Repositories[i].Owner = org
				}
				for i := range schema.Projects {
					schema.Projects[i].Owner = org
				}
				return schema, err
			}
			// Composition exercises all production constructors with fake discovery,
			// temporary SQLite storage, and no HTTP listener or GitHub mutations.
			r, err := compose(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.store.Close(); err != nil {
				t.Fatal(err)
			}
			if f.creates != 0 || f.adds != 0 || f.statuses != 0 || f.drafts != 0 || f.closes != 0 || f.reopens != 0 {
				t.Fatal("startup mutated fake GitHub state")
			}
		})
	}
}
