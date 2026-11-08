package allocation

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
)

// Review W5: Kueue applies its queue's flavor -- node labels, tolerations --
// to every pod it admits, guaranteed ones too. A guaranteed replica fitted
// without it counted a free node of another pool, where its pod can never
// land. One whole 8-GPU node free in pool b, the flavor's pool a full.
func TestAGuaranteedReplicaFitsItsQueueFlavor(t *testing.T) {
	poolA, poolB := map[string]string{"pool": "a"}, map[string]string{"pool": "b"}
	for _, tc := range []struct {
		name      string
		poolAUsed int
		want      int
	}{
		{"only another pool's node is free: no replica", 8, 0},
		{"control: the flavor's node is free: the replica places there", 0, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := borrowQuota(0, nsBorrow{ns: "b", guaranteed: 16, cap: config.QuotaUnlimited})
			q.SharedPools[0].NodeLabels = poolA
			o := NewGreedyByScoreOptimizer()
			o.Nodes = NewNodeView(map[string]decision.NodeGPU{"na": h100Node(tc.poolAUsed, poolA), "nb": h100Node(0, poolB)})
			got := addedGPUs(o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("b", "m", 8, 0, 1, nil)},
				[]*ResourceConstraints{physicalH100(64), q}))
			if got["m"] != tc.want {
				t.Fatalf("guaranteed growth %d GPUs, want %d", got["m"], tc.want)
			}
			if tc.want > 0 {
				f := o.PlacedFlavors()["b/m-v"]
				if f == nil || f.NodeLabels["pool"] != "a" {
					t.Fatalf("the pass names flavor %+v for the variant's hole, want pool a (W8)", f)
				}
			}
		})
	}
}
