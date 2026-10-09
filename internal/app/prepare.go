package app

import (
	"context"
	"errors"
	"reflect"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
)

// Diagnostics identify the startup boundary without exposing provider bodies or
// user-authored configuration values. Unwrap retains the cause for callers.
type preparationError struct {
	stage string
	cause error
}

func (e *preparationError) Error() string { return "app: " + e.stage }
func (e *preparationError) Unwrap() error { return e.cause }

// Prepare composes validated source, live discovery, and pure schema resolution.
// It performs no policy evaluation or mutation. Runtime startup uses this same boundary.
func Prepare(ctx context.Context, source config.SourceConfig, client github.Client) (Config, error) {
	if err := source.Validate(); err != nil {
		return Config{}, &preparationError{"invalid source configuration", err}
	}
	if client == nil || (reflect.ValueOf(client).Kind() == reflect.Pointer && reflect.ValueOf(client).IsNil()) {
		return Config{}, errors.New("application preparation requires GitHub client")
	}
	schema, err := client.DiscoverSchema(ctx, source)
	if err != nil {
		return Config{}, &preparationError{"GitHub schema discovery failed", err}
	}
	resolved, err := config.Resolve(source, schema)
	if err != nil {
		return Config{}, &preparationError{"configured GitHub schema resolution failed", err}
	}
	return Config{Deployment: &resolved}, nil
}
