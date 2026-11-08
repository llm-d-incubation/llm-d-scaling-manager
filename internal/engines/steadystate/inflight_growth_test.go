package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// Review minor (whole-node): every target above the filled replicas was
// admitted to the ledger, not only growth. A borrower Kueue preempted has
// its replacement pod at the admission gate, holding no node: its target
// equals its current replicas, yet the ledger held a phantom hole for it on
// the node the preemption freed, for five minutes, from the very lender the
// preemption was for. Only growth starts a hold; growth still in flight
// keeps its entry.
func TestOnlyGrowthStartsAHold(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	now := time.Now()
	gated := growingA100("gated", "m", 8)
	gated.VariantStates[0].CurrentReplicas = 2
	gated.VariantStates[0].FilledReplicas = 1
	gated.VariantStates[0].HeldKnown = true
	// The replacement pod has waited at Kueue's gate past a cycle.
	gated.VariantStates[0].UnplacedReplicas = 1
	at := func(target int) []domain.VariantDecision {
		return []domain.VariantDecision{{VariantName: "m-v", Namespace: "gated", AcceleratorName: "A100", TargetReplicas: target}}
	}
	reqs := []allocation.ModelScalingRequest{gated}

	(&Engine{}).admitInFlight(reqs, at(2), nil, nil, now)
	if allocation.DefaultInFlight.Holds("gated/m-v", now) {
		t.Fatal("a target at the current replicas holds a hole for a pod at Kueue's gate")
	}
	(&Engine{}).admitInFlight(reqs, at(3), nil, nil, now)
	if !allocation.DefaultInFlight.Holds("gated/m-v", now) {
		t.Fatal("growth past the current replicas holds nothing")
	}
	// The next cycle: the growth's pod exists, Pending; the target no
	// longer grows the variant, and the hold stays.
	gated.VariantStates[0].CurrentReplicas = 3
	(&Engine{}).admitInFlight(reqs, at(3), nil, nil, now.Add(time.Minute))
	if !allocation.DefaultInFlight.Holds("gated/m-v", now.Add(time.Minute)) {
		t.Fatal("growth in flight lost its hold once its pod was created")
	}
}
