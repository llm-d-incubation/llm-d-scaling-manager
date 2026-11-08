package main

import (
	"context"
	"testing"
)

// returningLooper's loop returns at once, as a loop that hits an unexpected
// exit does.
type returningLooper struct{}

func (returningLooper) StartOptimizeLoop(context.Context) {}

// A loop that stops while the manager is still running is an error, so the
// manager stops and releases the lease. Returning nil left a leader holding the
// lease with no engine: nothing scaled and no other replica could take over.
func TestOptimizeLoopRunnableFailsWhenTheLoopStopsEarly(t *testing.T) {
	runnable := optimizeLoopRunnable(func(context.Context) (optimizeLooper, error) { return returningLooper{}, nil })
	if err := runnable(context.Background()); err == nil {
		t.Fatal("the loop stopped with the manager still running and the runnable returned nil")
	}

	// Stopping because the manager is shutting down is not an error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runnable(ctx); err != nil {
		t.Fatalf("the loop stopped on shutdown and the runnable returned %v, want nil", err)
	}
}
