package allocation

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// K1: a reclaimWithinCohort LowerPriority lender at 500 lends 8 GPUs (one
// whole 8-GPU replica). Kueue would take them back from a canary at 0, never
// from serving at 1000, so only the canary may borrow them. Judged against
// the lowest Workload anywhere in the cohort, both borrowed, and serving's
// replica could never be reclaimed for the lender.
func TestLowerPriorityLendingOnlyForModelsItOutranks(t *testing.T) {
	const serving, canary = "kueue.x-k8s.io/serving", "kueue.x-k8s.io/canary"
	run := func(classes ...string) map[string]int {
		pool := borrowQuota(8,
			nsBorrow{ns: "serve", cap: config.QuotaUnlimited},
			nsBorrow{ns: "lab", cap: config.QuotaUnlimited})
		pool.SharedPools[0].Restricted = []PriorityLend{{Queue: "batch-cq", Below: 500, GPUs: 8}}
		pool.SharedPools[0].ClassPriority = map[string]int32{serving: 1000, canary: 0}
		reqs := []ModelScalingRequest{
			borrowModel("serve", "chat", 8, 0, 1, nil),
			borrowModel("lab", "trial", 8, 0, 1, nil),
		}
		for i, class := range classes {
			reqs[i].VariantStates[0].PriorityClass = class
		}
		o := NewGreedyByScoreOptimizer()
		o.Nodes = NewNodeView(nil)
		return addedGPUs(o.Optimize(context.Background(), reqs, []*ResourceConstraints{physicalH100(64), pool}))
	}

	got := run(serving, canary)
	if got["chat"] != 0 {
		t.Errorf("serving at 1000 borrowed %d GPUs of a lender at 500, want 0: Kueue never reclaims them", got["chat"])
	}
	if got["trial"] != 8 {
		t.Errorf("the canary at 0 borrowed %d GPUs, want 8: Kueue reclaims them for the lender", got["trial"])
	}

	if got := run(serving, serving); got["chat"]+got["trial"] != 0 {
		t.Errorf("two serving models borrowed %v, want nothing", got)
	}
	if got := run("", ""); got["chat"]+got["trial"] != 0 {
		t.Errorf("models of unknown class borrowed %v, want nothing", got)
	}
}
