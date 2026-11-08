package main

import (
	"context"
	"errors"
	"testing"
	"time"

	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// blockingLooper is an engine whose cycle is in flight when the context ends:
// its loop returns only once release is closed.
type blockingLooper struct {
	started chan struct{}
	release chan struct{}
}

func (l *blockingLooper) StartOptimizeLoop(ctx context.Context) {
	close(l.started)
	<-ctx.Done()
	<-l.release
}

// The manager's graceful shutdown waits for a runnable to return before it
// releases the lease. The runnable must therefore not return while a cycle is
// still running, or the lease is released under a cycle that is publishing.
func TestOptimizeLoopRunnableWaitsForTheCycle(t *testing.T) {
	looper := &blockingLooper{started: make(chan struct{}), release: make(chan struct{})}
	runnable := optimizeLoopRunnable(func(context.Context) (optimizeLooper, error) { return looper, nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runnable(ctx) }()

	select {
	case <-looper.started:
	case err := <-done:
		cancel()
		close(looper.release)
		t.Fatalf("runnable returned (%v) before its loop started", err)
	case <-time.After(5 * time.Second):
		t.Fatal("loop never started")
	}
	cancel()

	select {
	case err := <-done:
		close(looper.release)
		t.Fatalf("runnable returned (%v) while the cycle was still in flight", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(looper.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runnable returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runnable did not return after the cycle finished")
	}
}

// A setup error stops the manager through the runnable's return value. Exiting
// instead would skip the graceful shutdown and the lease release.
func TestOptimizeLoopRunnableReturnsTheSetupError(t *testing.T) {
	want := errors.New("no limiter")
	runnable := optimizeLoopRunnable(func(context.Context) (optimizeLooper, error) { return nil, want })
	if err := runnable(context.Background()); !errors.Is(err, want) {
		t.Fatalf("runnable returned %v, want %v", err, want)
	}
}

// A failed registration is returned to the runnable, not turned into os.Exit,
// which would end this test process too.
func TestRegisterMetricsSourceReturnsTheError(t *testing.T) {
	reg := source.NewSourceRegistry()
	if err := registerMetricsSource(reg, "prometheus", nil); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}
	if err := registerMetricsSource(reg, "prometheus", nil); err == nil {
		t.Fatal("a duplicate registration returned nil, want an error")
	}
}

// The shutdown budget is only a constant until main hands it to the manager.
// Without it the manager uses its own default, which is not checked against
// the pod's grace period, and the lease release is not wired either.
func TestManagerOptionsCarryTheShutdownBudget(t *testing.T) {
	opts := managerOptions(config.NewTestConfig(), metricsserver.Options{}, nil)
	if opts.GracefulShutdownTimeout == nil {
		t.Fatal("GracefulShutdownTimeout is not set")
	}
	if *opts.GracefulShutdownTimeout != managerGracefulShutdownTimeout {
		t.Errorf("GracefulShutdownTimeout = %s, want %s", *opts.GracefulShutdownTimeout, managerGracefulShutdownTimeout)
	}
	if !opts.LeaderElectionReleaseOnCancel {
		t.Error("LeaderElectionReleaseOnCancel is false, want true")
	}
}
