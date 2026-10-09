package app

import (
	"context"
	"testing"
	"time"
)

// observedContext signals when Run reaches its cancellation boundary, allowing
// the test to synchronize startup without a sleep or a production test hook.
type observedContext struct {
	context.Context
	waiting chan struct{}
}

func (ctx observedContext) Done() <-chan struct{} {
	close(ctx.waiting)
	return ctx.Context.Done()
}

func TestRunWaitsForCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := observedContext{Context: parent, waiting: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- Run(ctx, Config{}) }()

	// This timer only bounds test failure; startup is synchronized by a channel.
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	select {
	case <-ctx.waiting:
	case <-timeout.C:
		t.Fatal("runtime did not start")
	}

	select {
	case err := <-result:
		t.Fatalf("runtime returned before cancellation: %v", err)
	default:
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("shutdown returned an error: %v", err)
		}
	case <-timeout.C:
		t.Fatal("runtime did not shut down after cancellation")
	}
}

func TestRunWithCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := make(chan error, 1)
	go func() { result <- Run(ctx, Config{}) }()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("already-cancelled context returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not return for an already-cancelled context")
	}
}
