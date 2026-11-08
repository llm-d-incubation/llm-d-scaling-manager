package allocation

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// Review WN3: the pod templates' nodeSelector and required node affinity
// were judged only on the Kueue borrowing path. A P/D model pins its prefill
// to role=prefill nodes and its decode to role=decode nodes; under a
// physical limiter alone, with two prefill nodes free and the decode node
// full, the steady-state pass ordered a pair -- its decode on a prefill node,
// where it can never schedule -- and the decode stayed Pending for good.

// rolePinned is pdWanting with each role's templates pinned to nodes labelled
// role=<role>.
func rolePinned(namespace, model string, more int) ModelScalingRequest {
	req := pdWanting(namespace, model, more)
	for i := range req.VariantStates {
		role := req.VariantStates[i].Role
		req.VariantStates[i].Placements = []domain.PodPlacement{{NodeSelector: map[string]string{"role": role}}}
	}
	return req
}

// roleNodes is 8-GPU HBM3 nodes, prefill ones then decode ones, each with the
// given free GPUs.
func roleNodes(prefillFree, decodeFree []int) map[string]decision.NodeGPU {
	out := map[string]decision.NodeGPU{}
	add := func(role string, free []int) {
		for i, f := range free {
			out[role+string(rune('a'+i))] = decision.NodeGPU{Accelerator: famHBM3, Capacity: 8, Used: 8 - f,
				Labels: map[string]string{"role": role}}
		}
	}
	add("prefill", prefillFree)
	add("decode", decodeFree)
	return out
}

func TestPinnedPairNeedsANodeOfEachRole(t *testing.T) {
	DefaultInFlight.Reset()
	t.Cleanup(DefaultInFlight.Reset)
	for _, tc := range []struct {
		name         string
		prefill      []int
		decode       []int
		wantP, wantD int
	}{
		{"only prefill nodes free: no pair", []int{8, 8}, []int{0}, 1, 1},
		{"control: a node of each role free: one pair", []int{8, 0}, []int{8}, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := optimizeOnNodes(roleNodes(tc.prefill, tc.decode), rolePinned("x", "pd", 3))
			if got["pd-prefill"] != tc.wantP || got["pd-decode"] != tc.wantD {
				t.Fatalf("ordered prefill %d decode %d, want %d and %d", got["pd-prefill"], got["pd-decode"], tc.wantP, tc.wantD)
			}
		})
	}
}
