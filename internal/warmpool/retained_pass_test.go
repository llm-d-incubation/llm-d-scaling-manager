package warmpool

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/pool"
)

// A RETAINED pool must complete a pass.
//
// It could not. Once held r.mu while building policy.Input, and one of that
// struct's fields was `r.awakeIntent(...)`, which takes r.mu itself to read the
// switch clock. sync.Mutex is not reentrant, so the pass deadlocked on its own
// lock and never returned -- and because nothing releases a mutex held by a
// blocked goroutine, the pool stopped reconciling for the life of the process.
// It held its GPUs and did nothing with them.
//
// Only retained pools were affected, which is exactly why nothing caught it:
// awakeIntent returns at its `!Retained` guard before reaching the lock, so
// every ordinary bridge pool takes the harmless path, and the switching rule's
// own tests call chooseAwake directly and never go through a pass at all.
//
// Measured on the shared GPU cluster, 2026-08-30: the warm pool logged for twenty seconds after
// startup and then went silent, with no error and no panic, every time
// warmPoolRetained was set.
//
// The timeout is the assertion. A deadlock does not fail, it hangs, so the test
// has to bound it itself -- `go test` would otherwise report a panic ten minutes
// later with no indication which pass was stuck.
func TestARetainedPoolCompletesAPass(t *testing.T) {
	cfg := testConfig()
	cfg.Retained = true

	p := &fakePool{memberships: []pool.Membership{
		{Pod: podA(), State: pool.Absent, Pool: "retained"},
	}}

	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{{
		Name:     "retained",
		Config:   cfg,
		Replicas: 1,
		Switch:   SwitchConfig{SpareThreshold: 0.2, MinInterval: time.Minute},
	}}
	// Non-nil, so awakeIntent gets past its second guard and reaches the lock
	// that used to deadlock. A nil Pressure returns early and proves nothing.
	r.Pressure = func(string, string) (decision.Pressure, bool) {
		return decision.Pressure{}, false
	}

	done := make(chan error, 1)
	go func() {
		_, err := r.Once(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Once: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a retained pool's pass never finished: Once is deadlocking on r.mu, " +
			"which stops the pool reconciling for the life of the process")
	}
}
