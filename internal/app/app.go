// Package app provides the workflow controller's application lifecycle.
package app

import (
	"context"

	"github.com/parametron-io/parametron-workflow/internal/config"
)

// Config accepts resolved deployment bindings. A nil Deployment runs only the
// bootstrap lifecycle; live discovery and startup wiring belong to issue #9.
type Config struct {
	Deployment *config.ResolvedConfig
}

// Run starts the bootstrap runtime and waits for cancellation. Cancellation is
// a normal shutdown and returns nil. No external services or resources are used.
func Run(ctx context.Context, _ Config) error {
	<-ctx.Done()
	return nil
}
