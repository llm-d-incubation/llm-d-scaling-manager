package allocation

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// A workload whose pod spec names no accelerator can be scheduled onto any GPU
// node. "Any accelerator can serve this" is the literal meaning of that
// configuration, so the placement question is whether ANY pool has room — not
// which pool, which nothing can answer until the scheduler has chosen.
//
// The two wrong answers are both reachable and both were live at some point: an
// inference.optimization/acceleratorName label named a pool the scheduler was
// free to contradict (removed), and denying outright refused a workload that
// several accelerators could have served.
func TestUnconstrainedDemandFitsIfAnyPoolFits(t *testing.T) {
	constraints := []*ResourceConstraints{{
		ProviderName: "gpu-limiter",
		Pools: map[string]ResourcePool{
			"A100": {Limit: 8, Used: 8}, // full
			"H100": {Limit: 8, Used: 4}, // room for 4
		},
	}}

	if !FitsGPUBudget(constraints, "chat", map[string]int{constants.DefaultAcceleratorName: 4}) {
		t.Error("H100 has room and nothing pins this workload to A100, so it fits")
	}
	if FitsGPUBudget(constraints, "chat", map[string]int{constants.DefaultAcceleratorName: 5}) {
		t.Error("no pool has room for 5, so it must not fit")
	}
}

// The single-type cluster needs no special case: with one pool, "any pool has
// room" and "the sole pool has room" are the same statement.
func TestUnconstrainedDemandOnASingleTypeCluster(t *testing.T) {
	constraints := []*ResourceConstraints{{
		ProviderName: "gpu-limiter",
		Pools:        map[string]ResourcePool{"A100": {Limit: 8, Used: 2}},
	}}

	if !FitsGPUBudget(constraints, "chat", map[string]int{constants.DefaultAcceleratorName: 4}) {
		t.Error("6 free, 4 demanded — must fit")
	}
	if FitsGPUBudget(constraints, "chat", map[string]int{constants.DefaultAcceleratorName: 7}) {
		t.Error("deducing the type must not skip the budget check (6 free, 7 demanded)")
	}
}

// Being unconstrained is a claim about the WORKLOAD, not a wildcard for any name
// the cluster does not recognise. A named accelerator that no pool covers — a
// type that has left the cluster, or a typo — must still be denied, or a workload
// would be placed onto hardware it explicitly did not ask for.
func TestNamedButUnknownAcceleratorIsStillDenied(t *testing.T) {
	constraints := []*ResourceConstraints{{
		ProviderName: "gpu-limiter",
		Pools:        map[string]ResourcePool{"A100": {Limit: 8}},
	}}

	if FitsGPUBudget(constraints, "chat", map[string]int{"MI300X": 1}) {
		t.Error("a named accelerator the cluster does not have must not fall through to another pool")
	}
}
