package allocation

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// passiveBorrower is model y of namespace b, holding 16 borrowed GPUs (two
// 8-GPU replicas) in cohort c, whose decision lowers it to one replica.
func passiveBorrower(passive bool) (BorrowCycle, *SharedPoolChunk, map[string]domain.VariantReplicaState, []domain.VariantDecision) {
	req := borrowModel("b", "y", 8, 2, 1, nil)
	req.Passive = passive
	c := BorrowCycle{Constraints: []*ResourceConstraints{borrowQuota(0, nsBorrow{ns: "b", guaranteed: 0, used: 16})},
		Requests: []ModelScalingRequest{req}}
	ch := &SharedPoolChunk{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
		FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: borrowH100}}
	states := map[string]domain.VariantReplicaState{"b/y-v": req.VariantStates[0]}
	ds := []domain.VariantDecision{{Namespace: "b", ModelID: "y", VariantName: "y-v", AcceleratorName: "H100",
		GPUsPerReplica: 8, CurrentReplicas: 2, TargetReplicas: 1}}
	return c, ch, states, ds
}

// A passive model's lower target frees nothing for a waiting lender: nothing
// lowers it. The active model is the control.
func TestPassiveShrinkFreesNothingForALender(t *testing.T) {
	for _, tc := range []struct {
		passive bool
		want    int
	}{{false, 8}, {true, 0}} {
		c, ch, states, ds := passiveBorrower(tc.passive)
		b := (&BorrowGovernor{}).budgetOf(c, ch, map[string][]string{"b": {"c"}}, states, ds, nil)
		if b.free != tc.want {
			t.Errorf("passive=%v: the pool's budget counts %d GPUs freed, want %d", tc.passive, b.free, tc.want)
		}
	}
}

// A passive model is no candidate to release to a lender.
func TestPassiveModelIsNoLenderCandidate(t *testing.T) {
	for _, tc := range []struct {
		passive bool
		want    int
	}{{false, 1}, {true, 0}} {
		c, ch, states, _ := passiveBorrower(tc.passive)
		cands, _ := (&BorrowGovernor{}).lenderCandidates(c, LenderKey{Cohort: "c", Lender: "lender"},
			kueue.LenderState{Queue: "lender", Reclaim: kueue.ReclaimAny}, ch, map[string][]string{"b": {"c"}},
			states, map[string]map[string]int{"b": {"H100": 16}})
		if len(cands) != tc.want {
			t.Errorf("passive=%v: %d candidates, want %d", tc.passive, len(cands), tc.want)
		}
	}
}

// A reclaim does not lower a passive model: its target is a would-be figure,
// and lowering it gives nothing back. The active model is the control.
func TestReclaimYieldSkipsAPassiveModel(t *testing.T) {
	for _, tc := range []struct {
		passive bool
		want    int
	}{{false, 0}, {true, 1}} {
		ds := []domain.VariantDecision{{Namespace: "b", ModelID: "y", VariantName: "y-v", AcceleratorName: "H100",
			GPUsPerReplica: 8, CurrentReplicas: 1, TargetReplicas: 1}}
		req := ModelScalingRequest{Namespace: "b", ModelID: "y", Passive: tc.passive,
			VariantStates: []domain.VariantReplicaState{{VariantName: "y-v", CurrentReplicas: 1, GPUsPerReplica: 8}}}
		yieldState(8, 8).yieldReclaimed(ds, []ModelScalingRequest{req})
		if ds[0].TargetReplicas != tc.want {
			t.Errorf("passive=%v: target %d after the reclaim, want %d", tc.passive, ds[0].TargetReplicas, tc.want)
		}
	}
}

// ChargeGrowth charges each decision's growth to its pools, cluster and
// namespace, and leaves the input untouched.
func TestChargeGrowth(t *testing.T) {
	in := []*ResourceConstraints{{
		Pools:          map[string]ResourcePool{"H100": {Limit: 32, Used: 8}},
		NamespacePools: map[string]map[string]ResourcePool{"b": {"H100": {Limit: 16, Used: 8}}},
		TotalLimit:     32, TotalUsed: 8, TotalAvail: 24,
	}}
	out := ChargeGrowth(in, []domain.VariantDecision{
		{Namespace: "b", AcceleratorName: "H100", GPUsPerReplica: 8, CurrentReplicas: 1, TargetReplicas: 2},
		{Namespace: "b", AcceleratorName: "H100", GPUsPerReplica: 8, CurrentReplicas: 3, TargetReplicas: 1},
	})
	if got := out[0].Pools["H100"].Used; got != 16 {
		t.Errorf("cluster pool used %d, want 16", got)
	}
	if got := out[0].NamespacePools["b"]["H100"].Used; got != 16 {
		t.Errorf("namespace pool used %d, want 16", got)
	}
	if out[0].TotalAvail != 16 {
		t.Errorf("total available %d, want 16", out[0].TotalAvail)
	}
	if in[0].Pools["H100"].Used != 8 || in[0].NamespacePools["b"]["H100"].Used != 8 {
		t.Error("ChargeGrowth changed its input")
	}
}
