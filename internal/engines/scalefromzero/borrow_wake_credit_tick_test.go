package scalefromzero

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// clusterLenderProvider is a cluster-scope quota of 32 GPUs, all in use, and
// namespace "b"'s queue (nominal 16, idle) lending lentOut of it to a borrower
// whose pods hold those GPUs: b's figures come read and not counted
// (NamespaceNominal). The cluster quota is a cluster bound, so the wake's
// credit for what Kueue reclaims for b (CreditReclaimable) decides the fit.
type clusterLenderProvider struct{ lentOut int }

func (clusterLenderProvider) Name() string { return "cluster-quota" }
func (p clusterLenderProvider) ComputeConstraints(context.Context, map[string]int, map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	return &allocation.ResourceConstraints{ProviderName: "cluster-quota",
		Pools: map[string]allocation.ResourcePool{"H100": {Limit: 32, Used: 32}},
		NamespaceNominal: map[string]map[string]allocation.NominalFigures{
			"b": {"H100": {LentOut: p.lentOut, WithinNominal: 16, Guaranteed: 16}}},
	}, nil
}

// Review round 3 minor, checked and pinned: the wake credits its namespace's
// whole reclaimable nominal to the cluster bounds before reclaimableLeft
// takes off what the tick's earlier wakes used, so a second wake in the same
// tick sees the credit again. It also sees the earlier wake's whole demand
// withheld from those bounds (tickWakes), which nets it: used + earlier
// demand - reclaimable is what the cluster holds once Kueue preempted for the
// earlier wake. Crediting only what is left would count the earlier wake
// twice. b lent 8 and the cluster is full: a first 8-GPU wake takes the 8, a
// second in the same tick is refused. Lent 16: the second wakes on the 8
// left.
func TestSecondWakeInATickSharesTheLentOutCreditOnce(t *testing.T) {
	ctx := context.Background()
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	decision.PublishGPUUsage(map[string]int{"H100": 32}, nil)
	allocation.DefaultBorrowInFlight.Reset()
	t.Cleanup(allocation.DefaultBorrowInFlight.Reset)

	for _, tc := range []struct {
		name    string
		lentOut int
		second  bool
	}{
		{"lent 8: the second wake is refused", 8, false},
		{"lent 16: the second wake fits the 8 left", 16, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{gpuLimiter: clusterLenderProvider{lentOut: tc.lentOut}}
			e.beginTick()
			_, first, outcome := e.decideWake(ctx, modelGroup{namespace: "b", modelID: "m1"},
				[]Candidate{cand("m1-d", "decode", "H100", 8, 1)}, coverage{})
			if len(first) != 1 {
				t.Fatalf("the first wake woke %v (%s), want its decode", names(first), outcome)
			}
			_, second, outcome := e.decideWake(ctx, modelGroup{namespace: "b", modelID: "m2"},
				[]Candidate{cand("m2-d", "decode", "H100", 8, 1)}, coverage{})
			if got := len(second) == 1; got != tc.second {
				t.Fatalf("the second wake woke %v (%s), want woken %v", names(second), outcome, tc.second)
			}
		})
	}
}
