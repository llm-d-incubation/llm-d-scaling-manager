package allocation

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// pdAt is a P/D model in ns "b", 8 GPUs per replica, at current replicas of
// each role, whose demand asks for two more pairs.
func pdAt(current int) ModelScalingRequest {
	min := 0
	return withSatEntry(&satEntryFixture{
		RequiredCapacity: float64(current+2) * 20000,
		RoleCapacities: map[string]domain.RoleCapacity{
			"prefill": {Role: "prefill", RequiredCapacity: float64(current+2) * 10000, TotalDemand: float64(current+2) * 10000},
			"decode":  {Role: "decode", RequiredCapacity: float64(current+2) * 10000, TotalDemand: float64(current+2) * 10000},
		},
		VariantCapacities: []vcFixture{
			{VariantName: "p", AcceleratorName: "H100", Cost: 5, Role: "prefill", ReplicaCount: current, PerReplicaCapacity: 10000},
			{VariantName: "d", AcceleratorName: "H100", Cost: 5, Role: "decode", ReplicaCount: current, PerReplicaCapacity: 10000},
		},
	}, ModelScalingRequest{ModelID: "pd", Namespace: "b", Disaggregated: true, Priority: 1,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "p", CurrentReplicas: current, GPUsPerReplica: 8, Role: "prefill", MinReplicas: &min},
			{VariantName: "d", CurrentReplicas: current, GPUsPerReplica: 8, Role: "decode", MinReplicas: &min},
		}})
}

// Review N-m2: fillGuaranteed put one role alone on an odd guaranteed
// remainder even when the model had no replica of the other role that is not
// borrowed. A P/D model at zero, 8 guaranteed GPUs, 16 in the pool: the
// prefill took the guaranteed 8 and a borrowed pair followed. Once Kueue
// reclaims the pair, the prefill on guaranteed capacity serves nothing. A
// role fills the remainder alone only while the other role has a replica
// that is not borrowed. Control: the model already runs a pair on guaranteed
// capacity, and the remainder still fills (W2).
func TestGuaranteedFillNeedsAPartnerOfTheOtherRole(t *testing.T) {
	added := func(current, guaranteed, used int) map[string]int {
		o := NewGreedyByScoreOptimizer()
		ds := o.Optimize(context.Background(), []ModelScalingRequest{pdAt(current)},
			[]*ResourceConstraints{physicalH100(64), borrowQuota(16, nsBorrow{ns: "b", guaranteed: guaranteed, used: used,
				cap: config.QuotaUnlimited})})
		out := map[string]int{}
		for _, d := range ds {
			out[d.VariantName] += (d.TargetReplicas - d.CurrentReplicas) * d.GPUsPerReplica
		}
		return out
	}
	got := added(0, 8, 0)
	if got["p"] != got["d"] {
		t.Fatalf("a model at zero grew prefill %d and decode %d GPUs: one role sits alone on guaranteed capacity, "+
			"its only partner borrowed", got["p"], got["d"])
	}
	got = added(1, 24, 16)
	if got["p"]+got["d"] != 24 {
		t.Fatalf("control: with a guaranteed pair running, the remainder fills and a pair borrows whole: grew %v, want 24 in all", got)
	}
}

// Review round 3 (N-m2 partial): guaranteedPartners counted only the
// partner's draws in flight as borrowed. A decode borrowed and landed -- its
// pods exist, the read shows its namespace 8 past guaranteed -- counted as a
// guaranteed partner, and the prefill filled the guaranteed remainder alone,
// serving nothing once Kueue reclaims the decode. Whose the 8 past guaranteed
// are the read does not say, so they count against every partner, the
// cautious side; a decode this pass drew counts too. Control: nothing past
// guaranteed, the running decode partners the prefill.
func TestGuaranteedPartnerCountsLandedBorrowing(t *testing.T) {
	variants := []variantRecord{
		{VariantMetadata: domain.VariantMetadata{VariantName: "p", AcceleratorName: "H100"}},
		{VariantMetadata: domain.VariantMetadata{VariantName: "d", AcceleratorName: "H100"}},
	}
	states := map[string]domain.VariantReplicaState{"p": {VariantName: "p", GPUsPerReplica: 8}, "d": {VariantName: "d", GPUsPerReplica: 8}}
	byRole := map[string]string{"prefill": "p", "decode": "d"}
	for _, tc := range []struct {
		name     string
		heldPast int
		drawn    bool
		want     bool
	}{
		{"the decode is borrowed and landed: no guaranteed partner", 8, false, false},
		{"the decode was drawn this pass: no guaranteed partner", 0, true, false},
		{"control: nothing past guaranteed, the decode partners it", 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bs := &borrowState{heldPast: map[string]map[string]int{"b": {"H100": tc.heldPast}}}
			if tc.drawn {
				bs.draws = []BorrowDraw{{Namespace: "b", Variant: "d", GPUs: 8, Replicas: 1}}
			}
			p := &modelPlacer{bs: bs, namespace: "b", targets: map[string]int{"p": 1, "d": 1}}
			if got := p.guaranteedPartners([]string{"prefill", "decode"}, "prefill", byRole, variants, states); got != tc.want {
				t.Fatalf("guaranteedPartners = %v, want %v", got, tc.want)
			}
		})
	}
}
