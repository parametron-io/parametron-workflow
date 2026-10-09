package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/parametron-io/parametron-workflow/internal/app"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "parametron-workflow: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return app.Run(ctx, app.Config{})
}
