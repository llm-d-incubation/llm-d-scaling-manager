package allocation

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// Review N-m3: a lagging draw (its pods exist, no read shows them yet) was
// netted against every gated pod of its namespace, whatever it ran on. In
// namespace a, an H100 decode's 8 borrowed GPUs lag; another variant's A100
// pod sits at Kueue's gate. The A100 pod absorbed the lag, and the H100
// chunk counted the 8 as free: a second borrower could take them. A gated pod
// covers only a draw of its own accelerator. Control: the gated pod is the
// H100 draw's own.
func TestLagCoveredOnlyByGatedPodsOfItsAccelerator(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		gated string
		want  int
	}{
		{"another variant's A100 pod at the gate: the H100 lag stays", "A100", 8},
		{"control: the draw's own H100 pod at the gate: counted once", "H100", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := borrowQuota(16, nsBorrow{ns: "a", cap: -1})
			c.SharedPools[0].PendingGPUs = 0
			lag := BorrowDraw{Namespace: "a", Variant: "d", Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
				GPUs: 8, Replicas: 1, Target: 1, OrderedAt: now.Add(-time.Minute), ResolvedAt: now,
				Placement: &kueue.FlavorPlacement{AcceleratorType: borrowH100}}
			after := DeductUnreservedAt([]*ResourceConstraints{c}, Unreserved{InFlight: []BorrowDraw{lag},
				Gated: map[string]map[string]int{"a": {tc.gated: 8}}})
			if got := after[0].SharedPools[0].Free; got != tc.want {
				t.Fatalf("the H100 chunk reads %d free, want %d", got, tc.want)
			}
		})
	}
}
