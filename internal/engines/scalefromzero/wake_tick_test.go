package scalefromzero

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

const (
	wakeHBM3 = "NVIDIA-H100-80GB-HBM3"
	wakePCIe = "NVIDIA-H100-PCIE-80GB"
)

// wakeOnce runs one model's wake decision the way processInactiveModel does.
func wakeOnce(t *testing.T, e *Engine, ns, model string, candidates ...Candidate) SelectionOutcome {
	t.Helper()
	_, _, outcome := e.decideWake(context.Background(), modelGroup{namespace: ns, modelID: model}, candidates, coverage{})
	return outcome
}

// publishUsage publishes a physical usage snapshot, cleaned up after the test.
// decision.DefaultGPUUsage is process-wide: a test that calls this must not
// run in parallel (no t.Parallel).
func publishUsage(t *testing.T) {
	t.Helper()
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	decision.PublishGPUUsage(map[string]int{wakeHBM3: 0}, nil)
}

// Review A1: two models waking in one tick were each judged against the same
// free GPUs. Under an H100 family residual of 12, a decode on HBM3 (8) and
// another model's decode on PCIe (8) both woke. The second is refused.
func TestTwoWakesInOneTickShareTheFamilyBudget(t *testing.T) {
	publishUsage(t)
	e := &Engine{gpuLimiter: allocation.NewCompositeLimiter("composite", []allocation.Limiter{
		okProvider{name: allocation.PhysicalLimiterName, pools: map[string]allocation.ResourcePool{
			wakeHBM3: {Limit: 8}, wakePCIe: {Limit: 8}}},
		okProvider{name: "cluster-quota", pools: map[string]allocation.ResourcePool{"H100": {Limit: 12}}},
	})}
	e.beginTick()
	if got := wakeOnce(t, e, "a", "first", cand("first-decode", "decode", wakeHBM3, 8, 1)); got == OutcomeNoCapacity {
		t.Fatalf("first wake refused (%s): 12 of the family are free", got)
	}
	if got := wakeOnce(t, e, "b", "second", cand("second-decode", "decode", wakePCIe, 8, 1)); got != OutcomeNoCapacity {
		t.Fatalf("second wake in the tick = %s, want %s: the first took 8 of the family's 12", got, OutcomeNoCapacity)
	}
	// The next tick starts from what the cluster reports.
	e.beginTick()
	if got := wakeOnce(t, e, "b", "second", cand("second-decode", "decode", wakePCIe, 8, 1)); got == OutcomeNoCapacity {
		t.Fatalf("a new tick still carried the last tick's wake: %s", got)
	}
}

// publishNodes publishes 8-GPU HBM3 nodes with the given free GPUs, as a new
// scenario: the in-flight ledger, which the wakes of an earlier one filled, is
// emptied, now and after the test.
//
// The node store, the usage store and the in-flight ledger are process-wide,
// so the tests that use them must not run in parallel (no t.Parallel).
func publishNodes(t *testing.T, free ...int) {
	t.Helper()
	nodes := map[string]decision.NodeGPU{}
	for i, f := range free {
		nodes[fmt.Sprintf("n%d", i)] = decision.NodeGPU{Accelerator: wakeHBM3, Capacity: 8, Used: 8 - f}
	}
	allocation.DefaultInFlight.Reset()
	decision.DefaultNodeGPUs.Publish(nodes, time.Now())
	t.Cleanup(func() {
		decision.DefaultNodeGPUs.Publish(nil, time.Time{})
		allocation.DefaultInFlight.Reset()
	})
}

// physicalEngine is an engine whose only limiter is the physical one, with
// `free` HBM3 GPUs in its pool.
func physicalEngine(free int) *Engine {
	return &Engine{gpuLimiter: okProvider{name: allocation.PhysicalLimiterName,
		pools: map[string]allocation.ResourcePool{wakeHBM3: {Limit: free}}}}
}

// Review A3: the wake check counted GPUs per pool. A P/D wake of an 8-GPU
// prefill and an 8-GPU decode passed on 16 free as 4+4+8; the second role
// could never schedule. Whole 8-GPU units.
func TestPDWakeNeedsANodeForEachRole(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 4, 4, 8)
	e := physicalEngine(16)
	e.beginTick()
	_, selected, _ := e.decideWake(context.Background(), modelGroup{namespace: "a", modelID: "pd"},
		[]Candidate{cand("pd-decode", "decode", wakeHBM3, 8, 1), cand("pd-prefill", "prefill", wakeHBM3, 8, 1)},
		coverage{})
	if len(selected) != 1 || selected[0].VariantName != "pd-decode" {
		t.Fatalf("16 free as 4+4+8: woke %v, want the decode alone (one 8-GPU hole)", names(selected))
	}

	publishNodes(t, 8, 8)
	e.beginTick()
	_, selected, _ = e.decideWake(context.Background(), modelGroup{namespace: "a", modelID: "pd"},
		[]Candidate{cand("pd-decode", "decode", wakeHBM3, 8, 1), cand("pd-prefill", "prefill", wakeHBM3, 8, 1)},
		coverage{})
	if len(selected) != 2 {
		t.Fatalf("16 free as 8+8: woke %v, want the pair", names(selected))
	}
}

// Within a tick a wake also takes its node hole: a second model does not wake
// into the 8-GPU hole the first just took, though the pool still counts 8.
func TestAWakeTakesItsNodeHoleForTheTick(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 8, 4, 4)
	e := physicalEngine(16)
	e.beginTick()
	if got := wakeOnce(t, e, "a", "first", cand("first-decode", "decode", wakeHBM3, 8, 1)); got == OutcomeNoCapacity {
		t.Fatalf("first wake refused (%s): an 8-GPU hole is free", got)
	}
	if got := wakeOnce(t, e, "b", "second", cand("second-decode", "decode", wakeHBM3, 8, 1)); got != OutcomeNoCapacity {
		t.Fatalf("second wake = %s, want %s: the only 8-GPU hole was taken", got, OutcomeNoCapacity)
	}
}

// No physical limiter among the constraints -- a quota alone, or the physical
// provider skipped -- means no node fit: the wake is judged by pools alone.
func TestAQuotaAloneDoesNotFitWakesToNodes(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 4, 4)
	e := &Engine{gpuLimiter: okProvider{name: "cluster-quota",
		pools: map[string]allocation.ResourcePool{wakeHBM3: {Limit: 8}}}}
	e.beginTick()
	if got := wakeOnce(t, e, "a", "m", cand("m-decode", "decode", wakeHBM3, 8, 1)); got == OutcomeNoCapacity {
		t.Fatalf("a quota alone with 8 free refused an 8-GPU wake (%s): node fit belongs to the physical limiter", got)
	}
}

// Go review 6: the wake ledger under concurrency. Two models decide in one
// tick from separate goroutines (processInactiveModel runs them concurrently)
// with one free 8-GPU node: exactly one wakes. Run with -race.
func TestConcurrentWakesInOneTickShareOneHole(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 8)
	e := physicalEngine(8)
	e.beginTick()
	var wg sync.WaitGroup
	outcomes := make([]SelectionOutcome, 2)
	for i, m := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, outcomes[i] = e.decideWake(context.Background(), modelGroup{namespace: "ns-" + m, modelID: m},
				[]Candidate{cand(m+"-decode", "decode", wakeHBM3, 8, 1)}, coverage{})
		}()
	}
	wg.Wait()
	woke := 0
	for _, o := range outcomes {
		if o != OutcomeNoCapacity {
			woke++
		}
	}
	if woke != 1 {
		t.Fatalf("two concurrent wakes on one 8-GPU hole: %v, want exactly one woken", outcomes)
	}
}
