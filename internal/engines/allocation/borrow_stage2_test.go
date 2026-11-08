package allocation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// pdGrowth is a P/D model in ns "b", 8 GPUs per replica, at one prefill and
// one decode with minReplicas min, whose demand asks for one more pair.
func pdGrowth(min int) ModelScalingRequest {
	return withSatEntry(&satEntryFixture{
		RequiredCapacity: 40000,
		RoleCapacities: map[string]domain.RoleCapacity{
			"prefill": {Role: "prefill", RequiredCapacity: 20000, TotalDemand: 20000},
			"decode":  {Role: "decode", RequiredCapacity: 20000, TotalDemand: 20000},
		},
		VariantCapacities: []vcFixture{
			{VariantName: "p", AcceleratorName: "H100", Cost: 5, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
			{VariantName: "d", AcceleratorName: "H100", Cost: 5, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 10000},
		},
	}, ModelScalingRequest{ModelID: "pd", Namespace: "b", Disaggregated: true, Priority: 1,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "p", CurrentReplicas: 1, GPUsPerReplica: 8, Role: "prefill", MinReplicas: &min},
			{VariantName: "d", CurrentReplicas: 1, GPUsPerReplica: 8, Role: "decode", MinReplicas: &min},
		}})
}

var _ = Describe("Kueue borrowing, stage 2", func() {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	// B9: the reviewer's whole-node P/D case. 8 guaranteed GPUs left and 16 in
	// the pool: placing the next pair would put its prefill on guaranteed and
	// its decode on borrowed capacity. Reclaimed, the decode leaves a prefill
	// serving nothing -- with minReplicas 1 the placement timeout cannot
	// release it, with minReplicas 0 it would release guaranteed capacity.
	DescribeTable("a P/D pair borrows only whole",
		func(min, guaranteed int, want int) {
			o := NewGreedyByScoreOptimizer()
			got := addedGPUs(o.Optimize(ctx, []ModelScalingRequest{pdGrowth(min)},
				[]*ResourceConstraints{physicalH100(64), borrowQuota(16, nsBorrow{ns: "b", guaranteed: guaranteed, used: 16, cap: config.QuotaUnlimited})}))
			Expect(got["pd"]).To(Equal(want))
		},
		// W2: the odd guaranteed remainder takes one role alone, then the next
		// pair borrows whole: 8 + 16, not 0.
		Entry("minReplicas 1: 8 guaranteed left, one role fills it, the next pair borrows whole", 1, 24, 24),
		Entry("minReplicas 0: 8 guaranteed left, one role fills it, the next pair borrows whole", 0, 24, 24),
		Entry("control: guaranteed spent, the pair borrows whole", 1, 16, 16),
		Entry("control: 16 guaranteed left, a guaranteed pair, then a wholly borrowed one", 0, 32, 32),
	)

	// W2 / A5: nominal 24, 8+8 pairs. After the first pair 8 guaranteed are
	// left; every later pair would straddle it. The remainder takes one role
	// alone on guaranteed capacity, and the pair after borrows whole: no
	// borrowed draw without its partner.
	It("fills an odd guaranteed remainder with one role, then borrows the next pair whole", func() {
		o := NewGreedyByScoreOptimizer()
		got := addedGPUs(o.Optimize(ctx, []ModelScalingRequest{pdGrowth(1)},
			[]*ResourceConstraints{physicalH100(64), borrowQuota(64, nsBorrow{ns: "b", guaranteed: 24, used: 16, cap: config.QuotaUnlimited})}))
		Expect(got["pd"]).To(Equal(32), "8 on guaranteed, then a borrowed pair and the last role alone")
		borrowed := map[string]int{}
		for _, d := range o.BorrowDraws() {
			borrowed[d.Variant] += d.GPUs
		}
		Expect(borrowed["p"]+borrowed["d"]).To(Equal(24), "everything past guaranteed is borrowed")
		Expect(borrowed["p"]).To(BeNumerically(">=", 8))
		Expect(borrowed["d"]).To(BeNumerically(">=", 8), "the borrowed pair is whole")
	})

	// A pair the rule alone holds back is not attributed to a pool.
	It("names the pair-borrows-whole rule, not a pool, when no role fills the remainder alone", func() {
		limited := gpuLimitTracker{}
		limited.markPairWhole("d")
		ds := []domain.VariantDecision{{VariantName: "d", Namespace: "b", AcceleratorName: "H100"}}
		applyGPULimitAttribution(ds, limited, nil)
		Expect(ds[0].LimitedBy).To(Equal(PairBorrowsWholeName))
	})

	// B12: per (model, cohort), the cooldown doubles on a repeat up to 4x
	// (40m with the 10m default) and one clean cooldown resets it.
	It("backs a model off per cohort, doubling to 4x and resetting after a clean cooldown", func() {
		s := DefaultBorrowSettings()
		g := NewBorrowGovernor(s)
		until := func(at time.Time) time.Duration { return g.backoff["b/m@c"].Until.Sub(at) }
		g.MarkPreemptedIn("b/m", []string{"c"}, t0)
		Expect(until(t0)).To(Equal(10 * time.Minute))
		Expect(g.BorrowBlockedIn("b", "m", "c", t0.Add(5*time.Minute))).To(BeTrue())
		Expect(g.BorrowBlockedIn("b", "m", "other", t0.Add(5*time.Minute))).To(BeFalse(), "another cohort is not backed off")
		Expect(g.BorrowBlocked("b", "m", "m-v", t0.Add(5*time.Minute))).To(BeFalse(), "per cohort, not model-wide")

		g.MarkPreemptedIn("b/m", []string{"c"}, t0.Add(20*time.Second))
		Expect(until(t0)).To(Equal(10*time.Minute), "the pods of one preemption arriving one by one are one event")

		at := t0.Add(15 * time.Minute) // within one cooldown after it ended: a repeat
		g.MarkPreemptedIn("b/m", []string{"c"}, at)
		Expect(until(at)).To(Equal(20 * time.Minute))
		at = at.Add(25 * time.Minute)
		g.MarkPreemptedIn("b/m", []string{"c"}, at)
		Expect(until(at)).To(Equal(40 * time.Minute))
		at = at.Add(45 * time.Minute)
		g.MarkPreemptedIn("b/m", []string{"c"}, at)
		Expect(until(at)).To(Equal(40*time.Minute), "capped at 4x, 40m")

		// A clean cooldown after it ended: reset.
		clean := at.Add(40*time.Minute + s.Cooldown)
		g.pruneClocks(clean)
		g.MarkPreemptedIn("b/m", []string{"c"}, clean)
		Expect(until(clean)).To(Equal(10 * time.Minute))
	})

	// B13: two entries counting borrowing over one cohort, only one with a
	// pilot list: warned, never rejected.
	It("finds two entries over one cohort that disagree on borrowNamespaces", func() {
		a := borrowQuota(8, nsBorrow{ns: "x", cap: -1})
		a.ProviderName, a.BorrowNamespacesSet = "pilot", true
		b := borrowQuota(8, nsBorrow{ns: "y", cap: -1})
		b.ProviderName = "all"
		Expect(BorrowNamespacesSplit([]*ResourceConstraints{a, b})).To(Equal([]BorrowSplit{
			{Cohort: "c", WithList: []string{"pilot"}, WithoutList: []string{"all"}}}))
		b.BorrowNamespacesSet = true
		Expect(BorrowNamespacesSplit([]*ResourceConstraints{a, b})).To(BeEmpty(), "control: both set it")
	})

	// B1: a borrowed replica must fit the cohort's free pool of every
	// co-resource the flavor's group covers; an unstated request withholds.
	It("borrows only what the co-resources hold, and withholds a variant whose co-resource request is unknown", func() {
		model := borrowModel("b", "m", 8, 0, 2, nil)
		model.VariantStates[0].ReplicaRequests = map[string]int64{"cpu": 32000, "nvidia.com/gpu": 8000}
		q := borrowQuota(16, nsBorrow{ns: "b", cap: config.QuotaUnlimited})
		q.SharedPools[0].CoFree = map[string]int64{"cpu": 40000}
		got := addedGPUs(NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{model}, []*ResourceConstraints{physicalH100(64), q}))
		Expect(got["m"]).To(Equal(8), "40 CPUs free hold one 32-CPU replica, not two")

		q = borrowQuota(16, nsBorrow{ns: "b", cap: config.QuotaUnlimited})
		q.SharedPools[0].CoFree = map[string]int64{"cpu": 64000, "memory": 1 << 40}
		withheld := WithholdByVariants([]*ResourceConstraints{q}, []ModelScalingRequest{model})
		nb := withheld[0].NamespaceBorrow["b"]["H100"]
		Expect(nb.Frozen).To(BeTrue())
		Expect(nb.Withheld).To(Equal(string(kueue.ReasonCoResourceBound)), "memory is covered and the template states none")
		Expect(q.NamespaceBorrow["b"]["H100"].Frozen).To(BeFalse(), "the input is not changed")
		got = addedGPUs(NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{model}, []*ResourceConstraints{physicalH100(64), withheld[0]}))
		Expect(got["m"]).To(Equal(0))
	})

	// B5: with a pin required, a variant must pin one of its flavors' labels.
	It("withholds as flavor-unpinned a variant that does not pin a flavor the queue requires", func() {
		h100 := map[string]string{"nvidia.com/gpu.product": borrowH100}
		pinned := func(p *domain.PodPlacement) []ModelScalingRequest {
			m := borrowModel("b", "m", 8, 0, 1, nil)
			if p != nil {
				m.VariantStates[0].Placements = []domain.PodPlacement{*p}
			}
			return []ModelScalingRequest{m}
		}
		q := borrowQuota(16, nsBorrow{ns: "b", cap: config.QuotaUnlimited})
		nb := q.NamespaceBorrow["b"]["H100"]
		nb.PinRequired, nb.PinLabels = true, []map[string]string{h100}
		q.NamespaceBorrow["b"]["H100"] = nb
		reason := func(reqs []ModelScalingRequest) string {
			return WithholdByVariants([]*ResourceConstraints{q}, reqs)[0].NamespaceBorrow["b"]["H100"].Withheld
		}
		Expect(reason(pinned(nil))).To(Equal(string(kueue.ReasonFlavorUnpinned)))
		Expect(reason(pinned(&domain.PodPlacement{NodeSelector: h100}))).To(BeEmpty(), "control: nodeSelector pins it")
		Expect(reason(pinned(&domain.PodPlacement{RequiredNodeAffinity: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "nvidia.com/gpu.product", Operator: corev1.NodeSelectorOpIn, Values: []string{borrowH100}}},
		}}}}))).To(BeEmpty(), "control: required affinity pins it")
	})

	// B8: a peer namespace's or another controller's pending Workloads come
	// off the pool too; a governed gated pod is not counted twice.
	It("takes every member's pending demand out of the pool, the larger of it and the governed gated pods", func() {
		q := borrowQuota(16, nsBorrow{ns: "b", cap: config.QuotaUnlimited})
		q.SharedPools[0].PendingGPUs = 8
		out := DeductUnreserved([]*ResourceConstraints{q}, nil, nil)
		Expect(out[0].SharedPools[0].Free).To(Equal(8), "a peer's pending ask is not borrowed from under it")
		out = DeductUnreserved([]*ResourceConstraints{q}, nil, map[string]map[string]int{"b": {"": 8}})
		Expect(out[0].SharedPools[0].Free).To(Equal(8), "the governed gated pod is one of the pending Workloads: once")
		out = DeductUnreserved([]*ResourceConstraints{q}, nil, map[string]map[string]int{"b": {"": 12}})
		Expect(out[0].SharedPools[0].Free).To(Equal(4))
	})

	// One reason per namespace (B1 table order): a frozen namespace shows the
	// freeze's own cause, not the stale pools it leaves behind.
	It("shows kueue-controller-down, not stale, for a counted namespace frozen by a controller that is down", func() {
		q := borrowQuota(0, nsBorrow{ns: "b", cap: config.QuotaUnlimited})
		q.SharedPools[0].Stale = true
		q.BorrowWithheld = map[string]string{"b": string(kueue.ReasonKueueControllerDown)}
		b := LimiterBudgets([]*ResourceConstraints{q}, nil)
		Expect(b).To(HaveLen(1))
		Expect(b[0].Reason).To(Equal(string(kueue.ReasonKueueControllerDown)))
	})
})

// The namespace's own conditions and its queues' are one table: the first
// that holds wins, whichever side decided it.
var _ = Describe("QuotaInventory reason precedence", func() {
	It("shows not-kueue-managed over a cohort condition further down the table", func() {
		observed := time.Unix(1000, 0)
		row := config.ExternalBorrow{Queue: "q", Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
			Accelerator: "H100", Nominal: 8, Guaranteed: 8, BorrowCap: 8, WithinNominal: 8, Reason: string(kueue.ReasonNoStatus)}
		src := &fakeQuotaSource{}
		src.push(config.ExternalQuotas{
			Namespace: map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": 8}}},
			Borrow:    map[string][]config.ExternalBorrow{"team-a": {row}},
			SharedPools: []config.ExternalSharedPool{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
				Accelerator: "H100", Reason: string(kueue.ReasonNoStatus)}},
			ObservedAt: observed,
		}, nil)
		e := config.QuotaLimiterConfig{Name: "nq", Type: "quota", Scope: config.QuotaScopeNamespace,
			NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
			Kueue:           &config.KueueQuotaSource{Enabled: true}}
		inv := NewQuotaInventoryWithSource(e, src)
		inv.now = func() time.Time { return observed.Add(time.Second) }
		inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: &fakeEligibility{
			byNS: map[string]kueue.NamespaceEligibility{"team-a": {Reason: kueue.ReasonNotKueueManaged}}}})
		Expect(inv.Refresh(context.Background())).To(Succeed())
		Expect(inv.BorrowWithheld([]string{"team-a"})).To(Equal(map[string]string{"team-a": string(kueue.ReasonNotKueueManaged)}))
	})
})
