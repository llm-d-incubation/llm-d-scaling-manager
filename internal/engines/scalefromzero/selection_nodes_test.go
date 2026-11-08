package scalefromzero

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// selHBM3 is the product every node below carries.
const selHBM3 = "NVIDIA-H100-80GB-HBM3"

// nodeView is 8-GPU HBM3 nodes with the given free GPUs.
func nodeView(free ...int) *allocation.NodeView {
	nodes := map[string]decision.NodeGPU{}
	for i, f := range free {
		nodes[fmt.Sprintf("n%d", i)] = decision.NodeGPU{Accelerator: selHBM3, Capacity: 8, Used: 8 - f}
	}
	return allocation.NewNodeView(nodes)
}

// Review A3: the wake check counted GPUs per pool. A P/D wake of an 8-GPU
// prefill and an 8-GPU decode passed on 16 free as 4+4+8, and the second role
// could never schedule. Each member's replica must land on a node; the decode
// alone still serves. Whole 8-GPU units.
func TestSelectionFitsEachRoleToANode(t *testing.T) {
	set := []Candidate{cand("d", "decode", selHBM3, 8, 1), cand("p", "prefill", selHBM3, 8, 1)}
	in := SelectionInput{Namespace: selNS, Candidates: set, Constraints: pools(map[string]int{selHBM3: 16})}

	in.Nodes = nodeView(4, 4, 8)
	got, outcome := selectServingSet(in)
	if outcome != OutcomeDecodeOnly || len(got) != 1 || got[0].VariantName != "d" {
		t.Fatalf("16 free as 4+4+8: woke %v (%s), want the decode alone", names(got), outcome)
	}

	in.Nodes = nodeView(8, 8)
	if got, outcome = selectServingSet(in); outcome != OutcomePair {
		t.Fatalf("16 free as 8+8: woke %v (%s), want the pair", names(got), outcome)
	}

	in.Nodes = nodeView(4, 4, 4, 4)
	if got, outcome = selectServingSet(in); outcome != OutcomeNoCapacity {
		t.Fatalf("16 free as 4+4+4+4: woke %v (%s), want no-capacity", names(got), outcome)
	}

	// No node view (no physical limiter, or it is skipped): pools alone.
	in.Nodes = nil
	if got, outcome = selectServingSet(in); outcome != OutcomePair {
		t.Fatalf("no node view: woke %v (%s), want the pair on 16 pooled GPUs", names(got), outcome)
	}
}

// A node tainted for another workload is no hole for a wake whose pods do not
// tolerate the taint.
func TestAWakeDoesNotCountAnUntoleratedNode(t *testing.T) {
	in := SelectionInput{Namespace: selNS, Constraints: pools(map[string]int{selHBM3: 8}),
		Candidates: []Candidate{cand("d", "decode", selHBM3, 8, 1)}, Nodes: nodeView(8)}
	if _, outcome := selectServingSet(in); outcome == OutcomeNoCapacity {
		t.Fatalf("setup: an untainted free node refused the wake")
	}
	in.Nodes = taintedNodeView(8)
	if got, outcome := selectServingSet(in); outcome != OutcomeNoCapacity {
		t.Fatalf("only a node tainted dedicated=x is free: woke %v (%s), want no-capacity", names(got), outcome)
	}
	in.Candidates[0].Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}}
	if _, outcome := selectServingSet(in); outcome == OutcomeNoCapacity {
		t.Fatal("a wake that tolerates the taint was refused")
	}
}

// taintedNodeView is 8-GPU HBM3 nodes with the given free GPUs, each tainted
// dedicated=x:NoSchedule.
func taintedNodeView(free ...int) *allocation.NodeView {
	nodes := map[string]decision.NodeGPU{}
	for i, f := range free {
		nodes[fmt.Sprintf("n%d", i)] = decision.NodeGPU{Accelerator: selHBM3, Capacity: 8, Used: 8 - f,
			Taints: []corev1.Taint{{Key: "dedicated", Value: "x", Effect: corev1.TaintEffectNoSchedule}}}
	}
	return allocation.NewNodeView(nodes)
}
