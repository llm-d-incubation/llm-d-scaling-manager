package allocation

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// giveBackReq is one model on accelerator acc holding current 1-GPU replicas,
// short of its need.
func giveBackReq(id, ns, acc string, current int) ModelScalingRequest {
	v := id + "-v"
	req := withSatEntry(&satEntryFixture{
		ModelID: id, Namespace: ns, TotalDemand: 40000,
		VariantCapacities: []vcFixture{{VariantName: v, AcceleratorName: acc, Cost: 5, ReplicaCount: current, PerReplicaCapacity: 1000}},
	}, ModelScalingRequest{
		ModelID: id, Namespace: ns,
		VariantStates: []domain.VariantReplicaState{{VariantName: v, CurrentReplicas: current, GPUsPerReplica: 1, MinReplicas: ptrInt(1)}},
	})
	req.CompositeSignal.ScaleUpThreshold = 0.8
	return req
}

// giveBackGroup is the group of accelerator acc and the given scope that reqs form
// under the utilization share's view of cons (GuaranteedOnly).
func giveBackGroup(t *testing.T, reqs []ModelScalingRequest, cons []*ResourceConstraints, opts ShareGroupOptions, acc, scope string) ShareGroup {
	t.Helper()
	for _, g := range BuildShareGroups(reqs, GuaranteedOnly(cons), opts) {
		if g.AcceleratorType == acc && g.Scope == scope {
			return g
		}
	}
	t.Fatalf("no group %s/%q", acc, scope)
	return ShareGroup{}
}

// kueueConstraints is a namespace-scope quota entry reading snap from Kueue,
// with managed holding what the managed variants hold.
func kueueConstraints(t *testing.T, snap config.ExternalQuotas, countBorrowable *bool, managed map[string]map[string]int) []*ResourceConstraints {
	t.Helper()
	e := config.QuotaLimiterConfig{Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
		Kueue:           &config.KueueQuotaSource{Enabled: true, CountBorrowable: countBorrowable}}
	src := &fakeQuotaSource{}
	src.push(snap, nil)
	inv := NewQuotaInventoryWithSource(e, src)
	inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: &fakeEligibility{}})
	c, err := NewDefaultLimiter(e.Name, inv).ComputeConstraints(context.Background(), nil, managed)
	if err != nil {
		t.Fatal(err)
	}
	return []*ResourceConstraints{c}
}

// Kueue admitted what a namespace holds, and preempts what it must: the share
// gives back nothing Kueue granted, borrowed or beside a co-queued workload.
// Its Kueue-charged figures say how far a namespace may grow, not what it
// must give back. Only a namespace that counts borrowing, holding past its
// queue's nominal plus borrowingLimit, holds what Kueue never granted.
func TestShareGivesBackNothingKueueGranted(t *testing.T) {
	off := false
	limit := func(n int) *int { return &n }
	// Queue a of nominal 8 and borrowingLimit bl, Kueue reserving r of which
	// borrowed b; queue b an idle lender of 8.
	single := func(r, borrowed, bl int) config.ExternalBorrow {
		f := figuresOf(t,
			h100Queue("a", kueue.ReclaimNever, kueue.QueueResource{Nominal: 8, BorrowingLimit: limit(bl), Reserved: r, Borrowed: borrowed}),
			h100Queue("b", kueue.ReclaimAny, kueue.QueueResource{Nominal: 8}))
		return chargeRow("c", f, r)
	}
	// Queues qa and qb, both granting serving H100 (multi-queue-namespace).
	multi := func(q string, reserved, borrowed int) config.ExternalBorrow {
		row := config.ExternalBorrow{Queue: q, Cohort: "c", Flavor: q + "-flavor", Resource: "nvidia.com/gpu", Accelerator: "H100",
			Nominal: 8, Guaranteed: 8, WithinNominal: 8, BorrowCap: 8, Reserved: reserved, SourceBorrowed: borrowed}
		return row.Withheld(string(kueue.ReasonMultiQueueNamespace))
	}
	for _, tc := range []struct {
		name         string
		snap         config.ExternalQuotas
		count        *bool
		serving      int
		budget, over int
	}{
		{name: "multi-queue: serving 6 in qb within nominal, a training job borrowed 4 in qa",
			snap: multiQueueSnapshot("serving", multi("qa", 12, 4), multi("qb", 6, 0)), serving: 6, budget: 6},
		{name: "multi-queue: serving 10 in qa, 2 of them borrowed",
			snap: multiQueueSnapshot("serving", multi("qa", 10, 2), multi("qb", 0, 0)), serving: 10, budget: 10},
		{name: "multi-queue control: serving 6 in qb, qa idle",
			snap: multiQueueSnapshot("serving", multi("qa", 0, 0), multi("qb", 6, 0)), serving: 6, budget: 8},
		{name: "borrowing not counted: an unmanaged job holds 4 beside serving's 6",
			snap: chargeSnapshot(single(10, 2, 8), 8, "serving"), count: &off, serving: 6, budget: 6},
		{name: "borrowing not counted: serving alone holds 10, 2 of them borrowed",
			snap: chargeSnapshot(single(10, 2, 8), 8, "serving"), count: &off, serving: 10, budget: 10},
		{name: "borrowing not counted control: serving 6 alone within nominal",
			snap: chargeSnapshot(single(6, 0, 8), 8, "serving"), count: &off, serving: 6, budget: 8},
		{name: "borrowing counted, borrowingLimit 8: serving holds 10, all within it",
			snap: chargeSnapshot(single(10, 2, 8), 8, "serving"), serving: 10, budget: 10},
		{name: "borrowing counted, borrowingLimit 1: serving holds 10, 1 past nominal plus borrowingLimit",
			snap: chargeSnapshot(single(10, 2, 1), 8, "serving"), serving: 10, budget: 9, over: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cons := kueueConstraints(t, tc.snap, tc.count, map[string]map[string]int{"serving": {"H100": tc.serving}})
			reqs := []ModelScalingRequest{giveBackReq("m", "serving", "H100", tc.serving)}
			g := giveBackGroup(t, reqs, cons, ShareGroupOptions{}, "H100", "serving")
			if g.Budget != tc.budget || g.OverQuota != tc.over {
				t.Fatalf("budget %d over %d, want budget %d over %d", g.Budget, g.OverQuota, tc.budget, tc.over)
			}
		})
	}
}

// A cluster quota counts every namespace, but the cluster group plans only the
// namespaces no namespace quota caps. Of its quota's overage it gives back
// what it holds past what the others leave it -- unless the others alone
// already hold past the quota: the overage is then theirs, and the group gives
// back only what it alone holds past the quota. A cluster quota of 20; a
// namespace quota gives team-a 30 and excludes ns-x.
func TestShareClusterGroupPaysOnlyItsOwnOverage(t *testing.T) {
	nsq := config.QuotaLimiterConfig{Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
		Exclude: []string{"ns-x"}, NamespaceQuotas: map[string]map[string]int{"team-a": {"A100": 30}}}
	clq := config.QuotaLimiterConfig{Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
		ClusterQuotas: map[string]int{"A100": 20}}
	for _, tc := range []struct {
		name         string
		teamA, nsX   int
		budget, over int
	}{
		{name: "team-a 22 past the cluster quota by itself, ns-x 6", teamA: 22, nsX: 6, budget: 6},
		{name: "team-a 30, ns-x 6", teamA: 30, nsX: 6, budget: 6},
		{name: "team-a 22, ns-x 24: ns-x alone past the quota by 4", teamA: 22, nsX: 24, budget: 20, over: 4},
		{name: "control: team-a 14, ns-x 6, within the quota", teamA: 14, nsX: 6, budget: 6},
		{name: "team-a 14 within the quota, ns-x 10: ns-x holds 4 past what team-a leaves", teamA: 14, nsX: 10, budget: 6, over: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			byNS := map[string]map[string]int{"team-a": {"A100": tc.teamA}, "ns-x": {"A100": tc.nsX}}
			byType := map[string]int{"A100": tc.teamA + tc.nsX}
			cons := make([]*ResourceConstraints, 0, 2)
			for _, q := range []config.QuotaLimiterConfig{nsq, clq} {
				rc, err := NewDefaultLimiter(q.Name, NewQuotaInventory(q)).ComputeConstraints(context.Background(), byType, byNS)
				if err != nil {
					t.Fatal(err)
				}
				cons = append(cons, rc)
			}
			reqs := []ModelScalingRequest{giveBackReq("ma", "team-a", "A100", tc.teamA), giveBackReq("mx", "ns-x", "A100", tc.nsX)}
			g := giveBackGroup(t, reqs, cons, ShareGroupOptions{ClusterIsQuota: true}, "A100", "")
			if g.Budget != tc.budget || g.OverQuota != tc.over {
				t.Fatalf("cluster group budget %d over %d, want budget %d over %d", g.Budget, g.OverQuota, tc.budget, tc.over)
			}
		})
	}
}

// The physical inventory beside a cluster quota is not an allowance: a pod
// counted beside the victims it nominated is a stale read, and the share gives
// nothing back for it. A cluster quota of 20 with 16 held; the physical
// inventory of 16 counts 18.
func TestShareGivesBackNothingForAPhysicalOverCount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phys   ResourcePool
		budget int
	}{
		{name: "physical counts 18 of 16", phys: ResourcePool{Limit: 16, Used: 18}, budget: 16},
		{name: "control: physical counts 16 of 16", phys: ResourcePool{Limit: 16, Used: 16}, budget: 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cons := []*ResourceConstraints{
				{ProviderName: "cluster", Pools: map[string]ResourcePool{"A100": {Limit: 20, Used: 16}}},
				{ProviderName: PhysicalLimiterName, Pools: map[string]ResourcePool{"A100": tc.phys}},
			}
			reqs := []ModelScalingRequest{giveBackReq("a", "ns", "A100", 10), giveBackReq("b", "ns", "A100", 6)}
			g := giveBackGroup(t, reqs, cons, ShareGroupOptions{ClusterIsQuota: true}, "A100", "")
			if g.Budget != tc.budget || g.OverQuota != 0 {
				t.Fatalf("budget %d over %d, want budget %d over 0", g.Budget, g.OverQuota, tc.budget)
			}
		})
	}
}
