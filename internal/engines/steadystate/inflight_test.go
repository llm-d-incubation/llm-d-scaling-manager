package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// The in-flight ledger, from the steady-state engine. DefaultInFlight is
// process-wide: no t.Parallel.

// Whole-node MAJOR 3: what a cycle orders holds its hole until its pods land.
// The engine admits each variant's target; a share-planned model is left to
// the share's promises.
func TestACycleAdmitsItsGrowthInFlight(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	now := time.Now()
	reqs := []allocation.ModelScalingRequest{growingA100("ns", "m", 8), growingA100("ns", "planned", 8)}
	decisions := []domain.VariantDecision{
		{VariantName: "m-v", Namespace: "ns", AcceleratorName: "A100", TargetReplicas: 2},
		{VariantName: "planned-v", Namespace: "ns", AcceleratorName: "A100", TargetReplicas: 2},
	}
	(&Engine{}).admitInFlight(reqs, decisions, map[string]bool{"ns/planned": true}, nil, now)
	if !allocation.DefaultInFlight.Holds("ns/m-v", now) {
		t.Fatal("the growth ordered for m holds nothing")
	}
	if allocation.DefaultInFlight.Holds("ns/planned-v", now) {
		t.Fatal("a share-planned model's growth is held twice: by the ledger and by the share's promise")
	}
	decisions[0].TargetReplicas = 1
	(&Engine{}).admitInFlight(reqs, decisions[:1], nil, nil, now)
	if allocation.DefaultInFlight.Holds("ns/m-v", now) {
		t.Fatal("a target back at what is placed still holds a hole")
	}
}
