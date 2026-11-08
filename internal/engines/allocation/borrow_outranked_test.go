package allocation

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// K1 follow-up: a namespace whose only free pool is what a LowerPriority
// lender at 500 lends, and whose every model is at 1000, which the lender
// does not outrank. Kueue would never reclaim those GPUs for it, so none is
// borrowed -- and the refusal is that rule (lender-cannot-reclaim), not
// scarcity: no pool ran out, and the operator's remedy is the lender's
// policy or the model's priority, not more GPUs. One whole 8-GPU replica.
func TestOutrankedModelsAreHeldByLenderCannotReclaim(t *testing.T) {
	const serving, canary = "kueue.x-k8s.io/serving", "kueue.x-k8s.io/canary"
	run := func(free int, restricted bool, class string) domain.VariantDecision {
		pool := borrowQuota(free, nsBorrow{ns: "serve", cap: config.QuotaUnlimited})
		if restricted {
			pool.SharedPools[0].Restricted = []PriorityLend{{Queue: "batch-cq", Below: 500, GPUs: free}}
		}
		pool.SharedPools[0].ClassPriority = map[string]int32{serving: 1000, canary: 0}
		req := borrowModel("serve", "chat", 8, 0, 1, nil)
		req.VariantStates[0].PriorityClass = class
		o := NewGreedyByScoreOptimizer()
		o.Nodes = NewNodeView(nil)
		ds := o.Optimize(context.Background(), []ModelScalingRequest{req}, []*ResourceConstraints{physicalH100(64), pool})
		if len(ds) != 1 {
			t.Fatalf("got %d decisions, want 1", len(ds))
		}
		return ds[0]
	}

	d := run(8, true, serving)
	if d.TargetReplicas != 0 {
		t.Fatalf("serving at 1000 ordered %d replicas on a lender at 500, want 0", d.TargetReplicas)
	}
	if !d.WasLimited || d.LimitedBy != string(kueue.ReasonLenderCannotReclaim) {
		t.Errorf("outranked by no lender: limited %t by %q, want by %q: the GPUs are there, the lender cannot reclaim them",
			d.WasLimited, d.LimitedBy, string(kueue.ReasonLenderCannotReclaim))
	}

	// Control: a model the lender outranks borrows them.
	if d := run(8, true, canary); d.TargetReplicas != 1 {
		t.Errorf("the canary at 0 ordered %d replicas, want 1", d.TargetReplicas)
	}
	// Control: an empty pool is scarcity, the provider's.
	if d := run(0, false, serving); !d.WasLimited || d.LimitedBy != "namespace-quota" {
		t.Errorf("an empty pool: limited %t by %q, want by namespace-quota", d.WasLimited, d.LimitedBy)
	}
}
