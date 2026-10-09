// Package app provides the workflow controller's application lifecycle.
package app

import "context"

// Config is the application configuration entry point. The bootstrap requires
// no configuration; validated deployment settings belong to a later phase.
type Config struct{}

// Run starts the bootstrap runtime and waits for cancellation. Cancellation is
// a normal shutdown and returns nil. No external services or resources are used.
func Run(ctx context.Context, _ Config) error {
	<-ctx.Done()
	return nil
}
