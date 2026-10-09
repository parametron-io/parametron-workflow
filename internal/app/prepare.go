package app

import (
	"context"
	"errors"
	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
)

// Prepare composes validated source, live discovery, and pure schema resolution.
// It performs no policy evaluation or mutation. Runtime wiring belongs to #14.
func Prepare(ctx context.Context, source config.SourceConfig, client github.Client) (Config, error) {
	if err := source.Validate(); err != nil {
		return Config{}, err
	}
	if client == nil {
		return Config{}, errors.New("application preparation requires GitHub client")
	}
	schema, err := client.DiscoverSchema(ctx, source)
	if err != nil {
		return Config{}, err
	}
	resolved, err := config.Resolve(source, schema)
	if err != nil {
		return Config{}, err
	}
	return Config{Deployment: &resolved}, nil
}
