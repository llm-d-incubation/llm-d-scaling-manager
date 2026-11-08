package allocation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
)

// benchNodes is a large GPU cluster: n nodes of 8 HBM3 GPUs, every other one
// full, each carrying `labels` labels (which the node view must not copy).
func benchNodes(n, labels int) map[string]decision.NodeGPU {
	out := make(map[string]decision.NodeGPU, n)
	for i := range n {
		l := map[string]string{"zone": fmt.Sprintf("z%d", i%4)}
		for j := len(l); j < labels; j++ {
			l[fmt.Sprintf("example.com/label-%02d", j)] = fmt.Sprintf("value-%d-%d", i%7, j)
		}
		out[fmt.Sprintf("node-%05d", i)] = decision.NodeGPU{Accelerator: famHBM3, Capacity: 8, Used: 8 * (i % 2), Labels: l}
	}
	return out
}

// fragmentedNodes is a mostly-full cluster of n 8-GPU nodes: every tenth node
// has 4 GPUs free, every other one is full. The pools hold 4 GPUs per ten
// nodes; no node holds an 8-GPU replica.
func fragmentedNodes(n, labels int) map[string]decision.NodeGPU {
	out := benchNodes(n, labels)
	for name, node := range out {
		node.Used = 8
		out[name] = node
	}
	for i := 0; i < n; i += 10 {
		name := fmt.Sprintf("node-%05d", i)
		node := out[name]
		node.Used = 4
		out[name] = node
	}
	return out
}

// benchRequests is 50 models, 25 single-role and 25 P/D, each wanting 4 more
// 8-GPU replicas per role.
func benchRequests() []ModelScalingRequest {
	requests := make([]ModelScalingRequest, 0, 50)
	for m := range 25 {
		requests = append(requests,
			famWanting(fmt.Sprintf("ns%d", m%5), fmt.Sprintf("m%d", m), famHBM3, 4, 8),
			pdWanting(fmt.Sprintf("ns%d", m%5), fmt.Sprintf("pd%d", m), 4))
	}
	return requests
}

// BenchmarkNodeFitCycle is one optimizer cycle with node fit on a 2000-node
// cluster with 80 labels per node: the engine's node view is built from the
// store (decision.NodeGPUStore.Read, no label copy) and benchRequests' 50
// models are placed. pools-only is the same cycle without node fit, for the
// difference. half-free is every other node free; fragmented is
// fragmentedNodes, where node fit refuses what the pools allow.
func BenchmarkNodeFitCycle(b *testing.B) {
	for _, c := range []struct {
		name string
		snap map[string]decision.NodeGPU
	}{
		{"half-free", benchNodes(2000, 80)},
		{"fragmented", fragmentedNodes(2000, 80)},
	} {
		b.Run(c.name+"/node-fit", func(b *testing.B) { benchNodeFitCycle(b, c.snap, true) })
		b.Run(c.name+"/pools-only", func(b *testing.B) { benchNodeFitCycle(b, c.snap, false) })
	}
}

// decideCycle runs one optimizer cycle over snap, with node fit or without,
// and returns the targets by variant.
func decideCycle(store *decision.NodeGPUStore, snap map[string]decision.NodeGPU, fit bool) map[string]int {
	o := NewGreedyByScoreOptimizer()
	if fit {
		o.Nodes, _ = NodeViewFor(physicalOf(snap), store, time.Now())
	}
	out := map[string]int{}
	for _, d := range o.Optimize(context.Background(), benchRequests(), physicalOf(snap)) {
		out[d.VariantName] = d.TargetReplicas
	}
	return out
}

// On the fragmented cluster node fit must change the decisions: the pools
// hold 800 GPUs, and no node holds an 8-GPU replica. A benchmark whose two
// arms decided the same would time nothing node fit does.
func TestFragmentedClusterDecisionsDifferFromPoolsOnly(t *testing.T) {
	snap := fragmentedNodes(2000, 80)
	store := &decision.NodeGPUStore{}
	store.Publish(snap, time.Now())
	fitted, pooled := decideCycle(store, snap, true), decideCycle(store, snap, false)
	grownFitted, grownPooled := 0, 0
	for v, n := range fitted {
		grownFitted += n - 1
		grownPooled += pooled[v] - 1
	}
	if grownPooled <= 0 || grownFitted >= grownPooled {
		t.Fatalf("fragmented: node fit grew %d replicas, pools only %d; want node fit to grow fewer", grownFitted, grownPooled)
	}
}

func benchNodeFitCycle(b *testing.B, snap map[string]decision.NodeGPU, fit bool) {
	store := &decision.NodeGPUStore{}
	constraints := physicalOf(snap)
	requests := benchRequests()
	o := NewGreedyByScoreOptimizer()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		store.Publish(snap, time.Now())
		b.StartTimer()
		if fit {
			o.Nodes, _ = NodeViewFor(constraints, store, time.Now())
		}
		if len(o.Optimize(context.Background(), requests, constraints)) == 0 {
			b.Fatal("no decisions")
		}
	}
}
