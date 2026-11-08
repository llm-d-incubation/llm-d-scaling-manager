package allocation

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// cpuModel is an H100 model of 8-GPU, 32-CPU replicas in ns wanting want more.
func cpuModel(ns string, want int) ModelScalingRequest {
	m := borrowModel(ns, "m", 8, 0, want, nil)
	m.VariantStates[0].ReplicaRequests = map[string]int64{"cpu": 32000, "nvidia.com/gpu": 8000}
	return m
}

// setCo sets namespace ns's co-resource figures on q's H100 borrowing.
func setCo(q *ResourceConstraints, ns string, headroom, kept map[string]int64) {
	b := q.NamespaceBorrow[ns]["H100"]
	b.CoHeadroom, b.CoKept = headroom, kept
	q.NamespaceBorrow[ns]["H100"] = b
}

// K2b: Kueue stops a queue's CPU at nominal plus borrowingLimit whatever the
// cohort holds free. 40 CPUs of headroom hold one 32-CPU replica, not two,
// though the pools hold 16 GPUs and plenty of CPU.
func TestCoResourceHeadroomBoundsBorrowing(t *testing.T) {
	q := borrowQuota(16, nsBorrow{ns: "b", cap: config.QuotaUnlimited})
	q.SharedPools[0].CoFree = map[string]int64{"cpu": 1 << 20}
	setCo(q, "b", map[string]int64{"cpu": 40000}, nil)
	got := addedGPUs(NewGreedyByScoreOptimizer().Optimize(context.Background(), []ModelScalingRequest{cpuModel("b", 2)},
		[]*ResourceConstraints{physicalH100(64), q}))
	if got["m"] != 8 {
		t.Errorf("ordered %d GPUs, want 8: the second replica's 32 CPUs pass the queue's CPU borrowingLimit", got["m"])
	}
}

// K2a: a pool whose CPU is charged on a flavor not known (its resource group
// offers several) withholds a variant as co-resource-bound, even one that
// states its CPU request.
func TestUnboundCoResourceWithholds(t *testing.T) {
	q := borrowQuota(16, nsBorrow{ns: "b", cap: config.QuotaUnlimited})
	q.SharedPools[0].CoUnbound = []string{"cpu"}
	got := WithholdByVariants([]*ResourceConstraints{q}, []ModelScalingRequest{cpuModel("b", 1)})
	if nb := got[0].NamespaceBorrow["b"]["H100"]; !nb.Frozen || nb.Withheld != "co-resource-bound" {
		t.Errorf("borrowing = %+v, want withheld as co-resource-bound", nb)
	}
}

// K2c: a replica within the guaranteed GPU figure is admitted only when its
// CPU fits too. Guaranteed 16 GPUs, 40 CPUs free in the pool and none kept:
// one 32-CPU replica, not two.
func TestGuaranteedReplicaChecksCoResources(t *testing.T) {
	run := func(kept map[string]int64) int {
		q := borrowQuota(0, nsBorrow{ns: "b", guaranteed: 16, cap: config.QuotaUnlimited})
		q.SharedPools[0].CoFree = map[string]int64{"cpu": 40000}
		setCo(q, "b", nil, kept)
		return addedGPUs(NewGreedyByScoreOptimizer().Optimize(context.Background(), []ModelScalingRequest{cpuModel("b", 2)},
			[]*ResourceConstraints{physicalH100(64), q}))["m"]
	}
	if got := run(nil); got != 8 {
		t.Errorf("ordered %d GPUs within guaranteed, want 8: the second replica's CPU does not fit", got)
	}
	if got := run(map[string]int64{"cpu": 32000}); got != 16 {
		t.Errorf("ordered %d GPUs with 32 CPUs kept unlent, want 16: the kept CPU holds the second replica", got)
	}
}

// 186-4: the nominal-path co-resource check reaches only the namespaces that
// carry nominal co-resource headroom. A quota entry counting no borrowing
// bounds c (Kueue, 64 CPUs of headroom), k (Kueue, no co-resource) and s (a
// static cap). Only c's models get a placer; k's and s's decisions are what
// they are with no co-resource headroom anywhere, and rescale takes its
// path without borrowing. Before, any co-resource headroom switched the whole
// pass to the borrowing path, every namespace's models included.
func TestNominalCoCheckLeavesOtherNamespacesAlone(t *testing.T) {
	quota := func(withCo bool) *ResourceConstraints {
		q := &ResourceConstraints{ProviderName: "namespace-quota", NamespaceScoped: true,
			NamespacePools: map[string]map[string]ResourcePool{
				"c": {"H100": {Limit: 16}}, "k": {"H100": {Limit: 16}}, "s": {"H100": {Limit: 8}}}}
		if withCo {
			q.NamespaceNominal = map[string]map[string]NominalFigures{"c": {"H100": {WithinNominal: 16, Guaranteed: 16,
				CoHeadroom: map[string]int64{"cpu": 64000}}}}
		}
		return q
	}
	bs := newBorrowState([]*ResourceConstraints{physicalH100(64), quota(true)}, nil)
	if bs == nil {
		t.Fatal("no state: c's co-resource headroom is not checked")
	}
	for ns, want := range map[string]bool{"c": true, "k": false, "s": false} {
		if got := bs.newModelPlacer(ns, "m", nil, nil, gpuLimitTracker{}) != nil; got != want {
			t.Errorf("namespace %s: placer %t, want %t", ns, got, want)
		}
	}
	reqs := func() []ModelScalingRequest {
		k, s := borrowModel("k", "mk", 8, 0, 3, nil), borrowModel("s", "ms", 8, 0, 3, nil)
		return []ModelScalingRequest{cpuModel("c", 3), k, s}
	}
	with := NewGreedyByScoreOptimizer().Optimize(context.Background(), reqs(), []*ResourceConstraints{physicalH100(64), quota(true)})
	without := NewGreedyByScoreOptimizer().Optimize(context.Background(), reqs(), []*ResourceConstraints{physicalH100(64), quota(false)})
	for _, id := range []string{"mk", "ms"} {
		a, b := decisionOf(t, with, id), decisionOf(t, without, id)
		if a.TargetReplicas != b.TargetReplicas || a.WasLimited != b.WasLimited || a.LimitedBy != b.LimitedBy {
			t.Errorf("%s: target %d limited %t by %q beside c's co-resources, %d %t %q without them",
				id, a.TargetReplicas, a.WasLimited, a.LimitedBy, b.TargetReplicas, b.WasLimited, b.LimitedBy)
		}
	}
	if got := addedGPUs(with)["m"]; got != 16 {
		t.Errorf("c grew %d GPUs, want 16: two 32-CPU replicas fit its 64 CPUs, a third does not", got)
	}
}

// A namespace on two queues that both grant H100 (multi-queue-namespace)
// takes the smaller queue's nominal figures, its CPU headroom included: a
// replica is admitted through one queue, and each has 64 CPUs. One 48-CPU
// replica, not two. Before, the CPU headroom of the two queues was summed (128).
func TestMultiQueueNominalCoFollowsMinRule(t *testing.T) {
	rows := []config.ExternalBorrow{queueRow("cq-1", "H100", 0, true), queueRow("cq-2", "H100", 0, true)}
	for i := range rows {
		rows[i].CoUnused = map[string]int64{"cpu": 64000}
	}
	e := config.QuotaLimiterConfig{Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
		Kueue:           &config.KueueQuotaSource{Enabled: true, CountBorrowable: new(bool)}}
	src := &fakeQuotaSource{}
	src.push(multiQueueSnapshot("c", rows...), nil)
	inv := NewQuotaInventoryWithSource(e, src)
	inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: &fakeEligibility{}})
	c, err := NewDefaultLimiter(e.Name, inv).ComputeConstraints(context.Background(), nil, map[string]map[string]int{"c": {}})
	if err != nil {
		t.Fatal(err)
	}
	m := cpuModel("c", 2)
	m.VariantStates[0].ReplicaRequests["cpu"] = 48000
	got := addedGPUs(NewGreedyByScoreOptimizer().Optimize(context.Background(), []ModelScalingRequest{m},
		[]*ResourceConstraints{physicalH100(64), c}))
	if got["m"] != 8 {
		t.Errorf("ordered %d GPUs, want 8: each queue has 64 CPUs, and one of them admits the replicas", got["m"])
	}
}

// K2 on the nominal path: Kueue admits a replica within nominal only when its
// CPU fits the queue's nominal CPU too, borrowing counted or not. A queue of
// 16 GPUs and 64 CPUs, nothing reserved: one 48-CPU replica, not two. Before,
// the co-resources were checked only where borrowing was counted, so an entry
// with countBorrowable false, or a queue in no cohort, ordered both and the
// second waited gated.
func TestNominalPathChecksCoResources(t *testing.T) {
	noCohort := kueue.QueueFigures{Nominal: 16, Guaranteed: 16, WithinNominal: 16, BorrowCap: config.QuotaUnlimited,
		Reason: kueue.ReasonNoCohort}
	inCohort := kueue.QueueFigures{Nominal: 16, Guaranteed: 16, WithinNominal: 16, BorrowCap: config.QuotaUnlimited}
	f := false
	for _, tc := range []struct {
		name  string
		row   config.ExternalBorrow
		count *bool
	}{
		{"countBorrowable false", chargeRow("c", inCohort, 0), &f},
		{"no cohort", chargeRow("", noCohort, 0), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.row.CoUnused = map[string]int64{"cpu": 64000}
			e := config.QuotaLimiterConfig{Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
				Kueue:           &config.KueueQuotaSource{Enabled: true, CountBorrowable: tc.count}}
			src := &fakeQuotaSource{}
			src.push(chargeSnapshot(tc.row, 0, "c"), nil)
			inv := NewQuotaInventoryWithSource(e, src)
			inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: &fakeEligibility{}})
			c, err := NewDefaultLimiter(e.Name, inv).ComputeConstraints(context.Background(), nil, map[string]map[string]int{"c": {}})
			if err != nil {
				t.Fatal(err)
			}
			m := cpuModel("c", 2)
			m.VariantStates[0].ReplicaRequests["cpu"] = 48000
			got := addedGPUs(NewGreedyByScoreOptimizer().Optimize(context.Background(), []ModelScalingRequest{m},
				[]*ResourceConstraints{physicalH100(64), c}))
			if got["m"] != 8 {
				t.Errorf("ordered %d GPUs, want 8: the second 48-CPU replica does not fit the queue's 64 CPUs", got["m"])
			}
		})
	}
}
