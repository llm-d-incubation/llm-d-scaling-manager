package allocation

import (
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// shareReq builds a live single-variant model on A100 (1 GPU per replica, a
// per-replica capacity of 1000) with a scale-up threshold of 0.8, so a demand
// of D gives a need of D / 800 GPUs.
func shareReq(id, ns string, demand float64, current int) ModelScalingRequest {
	v := id + "-v"
	req := withSatEntry(&satEntryFixture{
		ModelID:     id,
		Namespace:   ns,
		TotalDemand: demand,
		VariantCapacities: []vcFixture{
			{VariantName: v, AcceleratorName: "A100", Cost: 5, ReplicaCount: current, PerReplicaCapacity: 1000},
		},
	}, ModelScalingRequest{
		ModelID:   id,
		Namespace: ns,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: v, CurrentReplicas: current, GPUsPerReplica: 1, MinReplicas: ptrInt(1)},
		},
	})
	req.CompositeSignal.ScaleUpThreshold = 0.8
	return req
}

func ptrInt(v int) *int { return &v }

func weightsByModel(w map[string]float64) func(ModelScalingRequest, bool) float64 {
	return func(req ModelScalingRequest, _ bool) float64 {
		if v, ok := w[req.ModelID]; ok {
			return v
		}
		return 1
	}
}

var _ = Describe("BuildShareGroups", func() {
	full16 := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 16}}}}

	It("builds §5.1's fleet as one cluster group whose need is demand / threshold in GPUs", func() {
		reqs := []ModelScalingRequest{
			shareReq("A", "ns", 4000, 9),
			shareReq("B", "ns", 3000, 5),
			shareReq("C", "ns", 1000, 2),
		}
		groups := BuildShareGroups(reqs, full16, ShareGroupOptions{
			ClusterIsQuota: true,
			Weight:         weightsByModel(map[string]float64{"A": 2}),
		})
		Expect(groups).To(HaveLen(1))
		g := groups[0]
		Expect(g.AcceleratorType).To(Equal("A100"))
		Expect(g.Scope).To(BeEmpty())
		Expect(g.Budget).To(Equal(16))
		needs := map[string]float64{}
		for _, r := range g.Roles {
			needs[r.Key] = r.Need
			Expect(r.Floor).To(Equal(1))
		}
		Expect(needs["ns/A/both"]).To(BeNumerically("~", 5, 1e-9))
		Expect(needs["ns/B/both"]).To(BeNumerically("~", 3.75, 1e-9))
		Expect(needs["ns/C/both"]).To(BeNumerically("~", 1.25, 1e-9))

		// Held exactly at §5.7's integer target: nothing is actionable.
		ev := EvaluateShare(g.Roles, g.Committed, g.Thresholds, g.Budget, 0.15)
		Expect(ev.ReplicasToMove).To(BeZero())
		for _, v := range ev.Roles {
			Expect(v.Actionable).To(BeFalse(), v.Key)
		}
	})

	It("sees B's doubled demand as three replicas to move (§5.7)", func() {
		reqs := []ModelScalingRequest{
			shareReq("A", "ns", 4000, 9),
			shareReq("B", "ns", 6000, 5),
			shareReq("C", "ns", 1000, 2),
		}
		g := BuildShareGroups(reqs, full16, ShareGroupOptions{
			ClusterIsQuota: true,
			Weight:         weightsByModel(map[string]float64{"A": 2}),
		})[0]
		ev := EvaluateShare(g.Roles, g.Committed, g.Thresholds, g.Budget, 0.15)
		Expect(ev.ReplicasToMove).To(Equal(3))
	})

	It("builds no group on the physical inventory unless asked to (§7.3)", func() {
		reqs := []ModelScalingRequest{shareReq("A", "ns", 4000, 9)}
		Expect(BuildShareGroups(reqs, full16, ShareGroupOptions{ClusterIsQuota: false})).To(BeEmpty())
		Expect(BuildShareGroups(reqs, full16, ShareGroupOptions{ClusterIsQuota: false, PhysicalGroups: true})).To(HaveLen(1))
	})

	It("freezes a model whose signal is not live and keeps its GPUs out of the budget (§6.1)", func() {
		dark := shareReq("D", "ns", 2000, 3)
		dark.CompositeSignal.Live = false
		reqs := []ModelScalingRequest{shareReq("A", "ns", 4000, 9), dark}
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 12}}}}
		g := BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})[0]
		Expect(g.Roles).To(HaveLen(1))
		Expect(g.Frozen).To(ConsistOf(ContainSubstring("ns/D")))
		// 4 free + A's 9: D's 3 are neither free nor planned.
		Expect(g.Budget).To(Equal(13))
	})

	It("holds the reserve out of the budget (§7.2)", func() {
		reqs := []ModelScalingRequest{shareReq("A", "ns", 4000, 9)}
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 12}}}}
		without := BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})[0]
		with := BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true, ReserveGPUs: 2})[0]
		Expect(without.Budget - with.Budget).To(Equal(2))
	})

	It("carves the warm pools' unheld target out of the budget of the group they plan in (§7.2)", func() {
		reqs := []ModelScalingRequest{shareReq("A", "ns", 4000, 9)}
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 12}}}}
		without := BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})[0]
		with := BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true,
			PoolUnheld: map[string]map[string]int{"pools": {"A100": 3, "H100": 5}}})[0]
		Expect(without.Budget-with.Budget).To(Equal(3), "only the group's accelerator is carved")
		Expect(with.PoolCarve).To(Equal(3))
	})

	It("carves a capped namespace's pools from its own group, and the cluster group only the rest", func() {
		// team-a has its own quota; team-b does not; team-c has a quota and
		// no model, so no group. team-a and team-c claim their free quota from
		// the cluster group, pools included, so only team-b's pool is carved
		// there.
		reqs := []ModelScalingRequest{shareReq("A", "team-a", 4000, 4), shareReq("B", "team-b", 3000, 4)}
		cons := []*ResourceConstraints{
			{Pools: map[string]ResourcePool{"A100": {Limit: 32, Used: 8}}},
			{NamespaceScoped: true, NamespacePools: map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 8, Used: 4}},
				"team-c": {"A100": {Limit: 8, Used: 0}},
			}},
		}
		unheld := map[string]map[string]int{"team-a": {"A100": 1}, "team-b": {"A100": 2}, "team-c": {"A100": 4}}
		carve := map[string]int{}
		for _, g := range BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true, PoolUnheld: unheld}) {
			carve[g.Scope] = g.PoolCarve
		}
		Expect(carve).To(Equal(map[string]int{"team-a": 1, "": 2}))
	})

	// A capped namespace's claim already takes its pool's headroom out of the
	// cluster budget; carving the pool there too counted it twice.
	It("counts a capped namespace's pool once in the cluster group", func() {
		reqs := []ModelScalingRequest{shareReq("B", "team-b", 1000, 0)}
		cons := []*ResourceConstraints{
			{Pools: map[string]ResourcePool{"A100": {Limit: 24, Used: 0}}},
			{NamespaceScoped: true, NamespacePools: map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 8, Used: 0}},
			}},
		}
		groups := BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true,
			PoolUnheld: map[string]map[string]int{"team-a": {"A100": 8}}})
		Expect(groups).To(HaveLen(1))
		Expect(groups[0].Scope).To(BeEmpty())
		Expect(groups[0].PoolCarve).To(BeZero())
		Expect(groups[0].Budget).To(Equal(16), "24 free less team-a's claim of 8, its pool inside it")
	})

	// No limiter bounds a warm pool's growth, so a pool in a capped namespace
	// wanting more than its tenant may hold shrank the cluster group by all of
	// it. Its carve stops at that namespace's free quota.
	It("caps a pool's carve at its namespace's free quota", func() {
		reqs := []ModelScalingRequest{shareReq("A", "team-a", 4000, 4), shareReq("B", "team-b", 3000, 4)}
		cons := []*ResourceConstraints{
			{Pools: map[string]ResourcePool{"A100": {Limit: 64, Used: 8}}},
			{NamespaceScoped: true, NamespacePools: map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 8, Used: 4}},
				"team-c": {"A100": {Limit: 8, Used: 2}},
			}},
		}
		// team-a wants 12 and may hold 4 more; team-c wants 20 and may hold 6
		// more; team-b has no namespace quota, so its 3 is carved whole. In
		// the cluster group team-a's and team-c's claims already hold their
		// pools, so only team-b's is carved there.
		unheld := map[string]map[string]int{"team-a": {"A100": 12}, "team-b": {"A100": 3}, "team-c": {"A100": 20}}
		carve := map[string]int{}
		for _, g := range BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true, PoolUnheld: unheld}) {
			carve[g.Scope] = g.PoolCarve
		}
		Expect(carve).To(Equal(map[string]int{"team-a": 4, "": 3}))
	})

	It("skips a disabled namespace-quota group and keeps the others", func() {
		reqs := []ModelScalingRequest{shareReq("A", "team-a", 4000, 4), shareReq("B", "team-b", 3000, 4)}
		cons := []*ResourceConstraints{{
			NamespacePools: map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 8, Used: 4}},
				"team-b": {"A100": {Limit: 8, Used: 4}},
			},
		}}
		groups := BuildShareGroups(reqs, cons, ShareGroupOptions{
			NamespaceEnabled: func(ns string) bool { return ns != "team-b" },
		})
		Expect(groups).To(HaveLen(1))
		Expect(groups[0].Scope).To(Equal("team-a"))
		Expect(groups[0].Budget).To(Equal(8))
	})

	It("builds both roles of a P/D model from their role demands", func() {
		req := withSatEntry(&satEntryFixture{
			ModelID:     "pd",
			Namespace:   "ns",
			TotalDemand: 30000,
			VariantCapacities: []vcFixture{
				{VariantName: "pd-p", AcceleratorName: "H200", Role: domain.RolePrefill, Cost: 5, ReplicaCount: 2, PerReplicaCapacity: 8000},
				{VariantName: "pd-d", AcceleratorName: "H200", Role: domain.RoleDecode, Cost: 5, ReplicaCount: 3, PerReplicaCapacity: 8000},
			},
			RoleCapacities: map[string]domain.RoleCapacity{
				domain.RolePrefill: {Role: domain.RolePrefill, TotalDemand: 6400},
				domain.RoleDecode:  {Role: domain.RoleDecode, TotalDemand: 12800},
			},
		}, ModelScalingRequest{
			ModelID:       "pd",
			Namespace:     "ns",
			Disaggregated: true,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "pd-p", CurrentReplicas: 2, GPUsPerReplica: 8, Role: domain.RolePrefill, MinReplicas: ptrInt(1)},
				{VariantName: "pd-d", CurrentReplicas: 3, GPUsPerReplica: 8, Role: domain.RoleDecode, MinReplicas: ptrInt(1)},
			},
		})
		req.CompositeSignal.ScaleUpThreshold = 0.8
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"H200": {Limit: 64, Used: 40}}}}
		g := BuildShareGroups([]ModelScalingRequest{req}, cons, ShareGroupOptions{ClusterIsQuota: true})[0]
		byRole := map[string]ShareRole{}
		for _, r := range g.Roles {
			byRole[g.Origins[r.Key].Role] = r
		}
		// prefill: 6400 / 0.8 / (8000 / 8 GPUs) = 8 GPUs; decode: 16 GPUs.
		Expect(byRole[domain.RolePrefill].Need).To(BeNumerically("~", 8, 1e-9))
		Expect(byRole[domain.RoleDecode].Need).To(BeNumerically("~", 16, 1e-9))
		Expect(byRole[domain.RolePrefill].ReplicaGPUs).To(Equal(8))
		Expect(byRole[domain.RolePrefill].Floor).To(Equal(8))
		Expect(g.Committed[byRole[domain.RoleDecode].Key]).To(Equal(24))
		Expect(g.Budget).To(Equal(24 + 40))
	})

	It("freezes a P/D model whole when one role has no demand reading", func() {
		req := withSatEntry(&satEntryFixture{
			ModelID:     "pd",
			Namespace:   "ns",
			TotalDemand: 30000,
			VariantCapacities: []vcFixture{
				{VariantName: "pd-p", AcceleratorName: "H200", Role: domain.RolePrefill, Cost: 5, ReplicaCount: 2, PerReplicaCapacity: 8000},
				{VariantName: "pd-d", AcceleratorName: "H200", Role: domain.RoleDecode, Cost: 5, ReplicaCount: 3, PerReplicaCapacity: 8000},
			},
			RoleCapacities: map[string]domain.RoleCapacity{
				domain.RoleDecode: {Role: domain.RoleDecode, TotalDemand: 12800},
			},
		}, ModelScalingRequest{
			ModelID:   "pd",
			Namespace: "ns",
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "pd-p", CurrentReplicas: 2, GPUsPerReplica: 8, Role: domain.RolePrefill},
				{VariantName: "pd-d", CurrentReplicas: 3, GPUsPerReplica: 8, Role: domain.RoleDecode},
			},
		})
		req.CompositeSignal.ScaleUpThreshold = 0.8
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"H200": {Limit: 64, Used: 40}}}}
		g := BuildShareGroups([]ModelScalingRequest{req}, cons, ShareGroupOptions{ClusterIsQuota: true})[0]
		Expect(g.Roles).To(BeEmpty())
		Expect(g.Frozen).To(ConsistOf(ContainSubstring("prefill")))
	})

	It("returns nothing without constraints, and skips an unlimited budget", func() {
		reqs := []ModelScalingRequest{shareReq("A", "ns", 4000, 9)}
		Expect(BuildShareGroups(reqs, nil, ShareGroupOptions{ClusterIsQuota: true})).To(BeEmpty())
		unlimited := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: -1}}}}
		Expect(BuildShareGroups(reqs, unlimited, ShareGroupOptions{ClusterIsQuota: true})).To(BeEmpty())
	})
})

// shareVariant is one variant of a multi-variant fixture.
type shareVariant struct {
	name          string
	gpus, current int
	cost, prc     float64
	min, max      *int
}

// shareReqVariants builds model "m" in namespace "ns" from several variants.
func shareReqVariants(demand float64, vs ...shareVariant) ModelScalingRequest {
	fx := &satEntryFixture{ModelID: "m", Namespace: "ns", TotalDemand: demand}
	req := ModelScalingRequest{ModelID: "m", Namespace: "ns"}
	for _, v := range vs {
		fx.VariantCapacities = append(fx.VariantCapacities, vcFixture{
			VariantName: v.name, AcceleratorName: "A100", Cost: v.cost, ReplicaCount: v.current, PerReplicaCapacity: v.prc,
		})
		req.VariantStates = append(req.VariantStates, domain.VariantReplicaState{
			VariantName: v.name, CurrentReplicas: v.current, GPUsPerReplica: v.gpus, MinReplicas: v.min, MaxReplicas: v.max,
		})
	}
	req = withSatEntry(fx, req)
	req.CompositeSignal.ScaleUpThreshold = 0.8
	return req
}

var _ = Describe("BuildShareGroups: floors, ceilings and replica sizes", func() {
	roomy := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 64, Used: 0}}}}

	only := func(req ModelScalingRequest) ShareRole {
		g := BuildShareGroups([]ModelScalingRequest{req}, roomy, ShareGroupOptions{ClusterIsQuota: true})
		ExpectWithOffset(1, g).To(HaveLen(1))
		ExpectWithOffset(1, g[0].Roles).To(HaveLen(1))
		return g[0].Roles[0]
	}

	It("sums floors and ceilings over a role's variants, each at its own replica size", func() {
		r := only(shareReqVariants(4000,
			shareVariant{name: "big", gpus: 4, cost: 5, prc: 4000, min: ptrInt(1), max: ptrInt(2)},
			shareVariant{name: "small", gpus: 1, cost: 2, prc: 1000, min: ptrInt(2), max: ptrInt(3)},
		))
		Expect(r.Floor).To(Equal(1*4 + 2*1))
		Expect(r.Ceiling).To(Equal(2*4 + 3*1))
		// The most cost-efficient variant (5/4000 < 2/1000) sizes the role:
		// one replica is 4 GPUs, and a GPU of it serves 1000.
		Expect(r.ReplicaGPUs).To(Equal(4))
		Expect(r.Need).To(BeNumerically("~", 4000/0.8/1000, 1e-9))
	})

	It("leaves a role unbounded when any of its variants has no ceiling", func() {
		r := only(shareReqVariants(4000,
			shareVariant{name: "big", gpus: 4, cost: 5, prc: 4000, max: ptrInt(2)},
			shareVariant{name: "small", gpus: 1, cost: 2, prc: 1000},
		))
		Expect(r.Ceiling).To(BeZero())
		Expect(r.Floor).To(BeZero())
	})

	It("freezes an aggregated model with demand but no measured capacity", func() {
		req := shareReqVariants(4000, shareVariant{name: "v", gpus: 1, cost: 5, prc: 0, current: 2})
		g := BuildShareGroups([]ModelScalingRequest{req}, roomy, ShareGroupOptions{ClusterIsQuota: true})
		Expect(g).To(HaveLen(1))
		Expect(g[0].Roles).To(BeEmpty())
		Expect(g[0].Frozen).To(ConsistOf(ContainSubstring("no measured capacity")))
	})

	// Issue ev-shindin/llm-scaling-manager#193: variant a is 8-GPU replicas serving 800 (100 per GPU),
	// variant b 16-GPU replicas serving 3200 (200 per GPU). b is the most
	// cost-efficient, so it is the one the role grows. Demand is 8800.
	issue193 := func(a, b int) ModelScalingRequest {
		req := shareReqVariants(8800,
			shareVariant{name: "a", gpus: 8, current: a, cost: 8, prc: 800},
			shareVariant{name: "b", gpus: 16, current: b, cost: 16, prc: 3200},
		)
		req.CompositeSignal.ScaleUpThreshold = 0.85
		return req
	}

	It("counts the need of a role that runs a less dense variant in the GPUs it runs (ev-shindin/llm-scaling-manager#193)", func() {
		// 16 replicas of a hold 128 GPUs and serve 12800. Demand 8800 at 0.85
		// needs 10353, which is 103.5 GPUs of a. Priced on b it was 51.8.
		r := only(issue193(16, 0))
		Expect(r.Need).To(BeNumerically("~", 8800/0.85/100, 1e-9))
	})

	It("counts a role's growth in GPUs of the variant it grows (ev-shindin/llm-scaling-manager#193)", func() {
		// One replica of a (8 GPUs, 800) is held; the rest is grown as b,
		// at 200 per GPU.
		r := only(issue193(1, 0))
		Expect(r.Need).To(BeNumerically("~", 8+(8800/0.85-800)/200, 1e-9))
	})

	It("grows past the best variant's ceiling in GPUs of the next one (ev-shindin/llm-scaling-manager#193)", func() {
		// b has room for two replicas (32 GPUs, 6400); the rest is grown as
		// a, at 100 per GPU.
		req := shareReqVariants(8800,
			shareVariant{name: "a", gpus: 8, current: 1, cost: 8, prc: 800},
			shareVariant{name: "b", gpus: 16, cost: 16, prc: 3200, max: ptrInt(2)},
		)
		req.CompositeSignal.ScaleUpThreshold = 0.85
		Expect(only(req).Need).To(BeNumerically("~", 8+32+(8800/0.85-800-6400)/100, 1e-9))
	})

	It("shares a shortfall at the utilization a role really runs at (ev-shindin/llm-scaling-manager#193)", func() {
		// m needs 103.5 GPUs of a, o needs 100 of its own: 168 GPUs hold 82.5%
		// of each. Priced on b, m looked like 51.8 and was given 57 GPUs --
		// 55% of its need as the a replicas it holds -- while o got 110%.
		o := shareReq("o", "ns", 80000, 40)
		full := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 168, Used: 168}}}}
		g := BuildShareGroups([]ModelScalingRequest{issue193(16, 0), o}, full, ShareGroupOptions{ClusterIsQuota: true})
		Expect(g).To(HaveLen(1))
		Expect(g[0].Budget).To(Equal(168))
		cont := ContinuousShareTargets(g[0].Roles, float64(g[0].Budget))
		mCovered := cont["ns/m/both"] * 100 / (8800 / 0.85)
		oCovered := cont["ns/o/both"] * 1000 / (80000 / 0.8)
		Expect(oCovered).To(BeNumerically("<", 1))
		Expect(mCovered).To(BeNumerically("~", oCovered, 1e-9))
	})

	It("leaves the need of a role that runs only its best variant as it was (ev-shindin/llm-scaling-manager#193)", func() {
		d, k := 8800.0, 0.85
		r := only(issue193(0, 4))
		Expect(r.Need).To(Equal(d / k / (3200.0 / 16)))
		// Two running variants as dense as the best one change nothing either.
		d, k = 4000, 0.8
		same := shareReqVariants(d,
			shareVariant{name: "big", gpus: 4, current: 2, cost: 5, prc: 4000},
			shareVariant{name: "small", gpus: 1, current: 3, cost: 2, prc: 1000},
		)
		Expect(only(same).Need).To(Equal(d / k / (4000.0 / 4)))
	})

	It("prices a shrink on the replicas a role gives first, the denser ones when they cost more (ev-shindin/llm-scaling-manager#209)", func() {
		// v0 is 1-GPU replicas serving 100 at cost 0.5, the most cost-efficient;
		// v1 one 8-GPU replica serving 1600 (200 per GPU) at cost 16, the least,
		// so v1 is what the role gives. Required is 1100 of 2400: giving v1
		// leaves 800, so the role keeps v0's 8 GPUs (800) and 300 of v1, 1.5
		// of its GPUs. The fleet average, 1100 x 16 / 2400 = 7.33, priced the
		// GPUs it keeps at what v1 serves.
		req := shareReqVariants(1100*0.8,
			shareVariant{name: "v0", gpus: 1, current: 8, cost: 0.5, prc: 100},
			shareVariant{name: "v1", gpus: 8, current: 1, cost: 16, prc: 1600},
		)
		r := only(req)
		Expect(r.Need).To(BeNumerically("~", 9.5, 1e-9))
		Expect(r.Capacity).To(Equal([]ShareCapacityStep{{GPUs: 8, PerGPU: 1}, {GPUs: 8, PerGPU: 2}, {GPUs: math.Inf(1), PerGPU: 1}}))
	})

	It("prices a shrink on the replicas a role gives first, the sparser ones when cost follows GPUs (ev-shindin/llm-scaling-manager#211)", func() {
		// a is 1-GPU replicas serving 100, b 4-GPU replicas serving 800 (200
		// per GPU); cost is 1 per GPU, so b is the most cost-efficient and a
		// is given first. Four a and two b serve 2000 on 12 GPUs. Required
		// 1500: all four a go (1600 left), and a b replica would leave 800, so
		// the need is b's 8 GPUs less the 100 of it the role does not need,
		// 0.5 of a GPU. The fleet average, 1500 x 12 / 2000 = 9, overstated it.
		req := shareReqVariants(1500*0.8,
			shareVariant{name: "a", gpus: 1, current: 4, cost: 1, prc: 100},
			shareVariant{name: "b", gpus: 4, current: 2, cost: 4, prc: 800},
		)
		Expect(only(req).Need).To(BeNumerically("~", 7.5, 1e-9))
	})

	It("gives a variant held at its minReplicas last (ev-shindin/llm-scaling-manager#209)", func() {
		// As above, but a's minReplicas keeps all four a: the role gives b
		// first, and a's 4 GPUs (400, 2 GPUs of b) are what it keeps last
		// given. Required 1500 is 7.5 GPUs of b: 2 on a's 4 GPUs, 5.5 on b.
		req := shareReqVariants(1500*0.8,
			shareVariant{name: "a", gpus: 1, current: 4, cost: 1, prc: 100, min: ptrInt(4)},
			shareVariant{name: "b", gpus: 4, current: 2, cost: 4, prc: 800},
		)
		Expect(only(req).Need).To(BeNumerically("~", 4+5.5, 1e-9))
	})

	It("keeps a single-variant role's need and leaves it no capacity curve (ev-shindin/llm-scaling-manager#209)", func() {
		d, k := 1200.0, 0.8
		r := only(shareReqVariants(d, shareVariant{name: "b", gpus: 4, current: 5, cost: 4, prc: 800}))
		Expect(r.Need).To(Equal(d / k / (800.0 / 4)))
		Expect(r.Capacity).To(BeNil())
	})

	It("counts a running variant with a NaN or infinite capacity as dense as best (ev-shindin/llm-scaling-manager#209)", func() {
		// The 8-GPU replica with no reading counts as 8 GPUs of best, so the
		// role holds 10 GPUs of best and required 300 is 3 of them. A NaN
		// froze the model; an infinity was ranked the best variant and made
		// the need 0.
		for _, prc := range []float64{math.NaN(), math.Inf(1), 0, -5} {
			req := shareReqVariants(300*0.8,
				shareVariant{name: "best", gpus: 1, current: 2, cost: 1, prc: 100},
				shareVariant{name: "odd", gpus: 8, current: 1, cost: 8, prc: prc},
			)
			r := only(req)
			Expect(r.Need).To(BeNumerically("~", 3, 1e-9), "capacity %v", prc)
			Expect(r.Capacity).To(BeNil(), "capacity %v", prc)
		}
	})

	It("reads a role's utilization from what its GPUs serve, not from their count (ev-shindin/llm-scaling-manager#209)", func() {
		// 8 GPUs of best, then 8 that serve twice as much: 16 GPUs serve 24 GPUs
		// of best, and a need of 10 GPUs serves 12.
		r := ShareRole{Need: 10, Capacity: []ShareCapacityStep{{GPUs: 8, PerGPU: 1}, {GPUs: 8, PerGPU: 2}, {GPUs: math.Inf(1), PerGPU: 1}}}
		Expect(r.Utilization(16, 0.8)).To(BeNumerically("~", 12*0.8/24, 1e-12))
		// Its score, too: 16 GPUs are 100% headroom over the need, not 60%.
		r.Weight = 1
		Expect(r.Score(16)).To(BeNumerically("~", 1, 1e-12))
		// 12 of 24 is calm (at most 0.6); 10 of 16 GPUs read as 0.625 was not.
		Expect(ShareSkipWaits{}.Calm(r, 16)).To(BeTrue())
		// 12 of the 14 that 11 GPUs serve is below 0.9; 10 of 11 GPUs read as urgent.
		Expect(ShareSkipWaits{}.Urgent(r, 11)).To(BeFalse())
		// Without a curve both read the GPU count, as before.
		r.Capacity = nil
		Expect(r.Utilization(16, 0.8)).To(BeNumerically("~", 10*0.8/16, 1e-12))
		Expect(r.Score(16)).To(BeNumerically("~", 0.6, 1e-12))
		Expect(ShareSkipWaits{}.Calm(r, 16)).To(BeFalse())
		Expect(ShareSkipWaits{}.Urgent(r, 11)).To(BeTrue())
	})

	It("moves a dense replica off a shrinking role only when the quota cannot hold both (ev-shindin/llm-scaling-manager#209)", func() {
		// X as in the first ev-shindin/llm-scaling-manager#209 spec (need 9.5 on 16 GPUs), Y one 8-GPU
		// replica. Giving X's 8-GPU v1 leaves it serving 800 of 1100.
		k := 0.8
		model := func(id string, demand float64, vs ...shareVariant) ModelScalingRequest {
			fx := &satEntryFixture{ModelID: id, Namespace: "ns", TotalDemand: demand}
			req := ModelScalingRequest{ModelID: id, Namespace: "ns"}
			for _, v := range vs {
				name := id + "-" + v.name
				fx.VariantCapacities = append(fx.VariantCapacities, vcFixture{
					VariantName: name, AcceleratorName: "A100", Cost: v.cost, ReplicaCount: v.current, PerReplicaCapacity: v.prc,
				})
				req.VariantStates = append(req.VariantStates, domain.VariantReplicaState{
					VariantName: name, CurrentReplicas: v.current, GPUsPerReplica: v.gpus, MinReplicas: ptrInt(0),
				})
			}
			req = withSatEntry(fx, req)
			req.CompositeSignal.ScaleUpThreshold = k
			return req
		}
		given := func(yNeed float64) map[string]int {
			x := model("x", 1100*k,
				shareVariant{name: "v0", gpus: 1, current: 8, cost: 0.5, prc: 100},
				shareVariant{name: "v1", gpus: 8, current: 1, cost: 16, prc: 1600})
			y := model("y", yNeed*100*k, shareVariant{name: "v", gpus: 8, current: 1, cost: 8, prc: 800})
			full := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 24, Used: 24}}}}
			g := BuildShareGroups([]ModelScalingRequest{x, y}, full, ShareGroupOptions{ClusterIsQuota: true})
			ExpectWithOffset(1, g).To(HaveLen(1))
			in := SharePlanInput{
				Roles: g[0].Roles, Held: g[0].Committed, Thresholds: g[0].Thresholds, DownThresholds: g[0].DownThresholds,
				Budget: g[0].Budget, Tolerance: 0.15, Give: g[0].Give, Grow: g[0].Grow, PhysicalFree: g[0].ClusterFree,
				Headroom: ShareHeadroom{Capped: true, Replicas: 1, ReturnLoad: 0.6}, Origins: g[0].Origins,
			}
			l := NewShareLedger()
			t0 := time.Unix(1_000_000, 0)
			for c := 0; c < 6; c++ {
				PlanShareTransfers(l, in, t0.Add(time.Duration(c)*simCycle), simTimings())
			}
			out := map[string]int{}
			for _, t := range l.Transfers() {
				out[t.DonorVariant]++
			}
			return out
		}
		// Y at or near its need: X keeps v1. At 10, Y holds 80% of what it
		// needs, and X left with 800 would hold 73%.
		Expect(given(8)).To(BeEmpty())
		Expect(given(10)).To(BeEmpty())
		// Y short by a replica or more: the quota cannot hold both, and the
		// share moves v1 to Y as it did before ev-shindin/llm-scaling-manager#193.
		Expect(given(16)).To(Equal(map[string]int{"x-v1": 1}))
		Expect(given(24)).To(Equal(map[string]int{"x-v1": 1}))
	})

	It("plans a model with no demand and no capacity reading at its floor", func() {
		r := only(shareReqVariants(0, shareVariant{name: "v", gpus: 2, cost: 5, prc: 0, min: ptrInt(1)}))
		Expect(r.Need).To(BeZero())
		Expect(r.Floor).To(Equal(2))
		Expect(r.ReplicaGPUs).To(Equal(2))
	})
})

var _ = Describe("BuildShareGroups: scopes and bounds", func() {
	It("builds a namespace-quota group beside the cluster group, each with its own budget", func() {
		reqs := []ModelScalingRequest{shareReq("A", "team-a", 4000, 4), shareReq("B", "other", 3000, 8)}
		cons := []*ResourceConstraints{
			{Pools: map[string]ResourcePool{"A100": {Limit: 20, Used: 12}}},
			{NamespaceScoped: true, NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: 8, Used: 4}}}},
		}
		groups := BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})
		Expect(groups).To(HaveLen(2))
		Expect(groups[0].Scope).To(BeEmpty())
		// The cluster's free 8 less team-a's claim of 4, + B's 8.
		Expect(groups[0].Budget).To(Equal(4 + 8))
		Expect(groups[0].ClusterFree).To(Equal(4))
		Expect(groups[1].Scope).To(Equal("team-a"))
		Expect(groups[1].Budget).To(Equal(4 + 4)) // team-a's free 4 + A's 4
		// The two groups share the cluster free 8 (splitClusterFree): each
		// fills at most what it could claim, 4 and 4.
		Expect(groups[1].ClusterFree).To(Equal(4))
	})

	It("builds a namespace-quota group even when the cluster budget is the physical inventory", func() {
		reqs := []ModelScalingRequest{shareReq("A", "team-a", 4000, 4)}
		cons := []*ResourceConstraints{{NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: 8, Used: 4}}}}}
		Expect(BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: false})).To(HaveLen(1))
	})

	It("skips a namespace whose quota does not list the model's accelerator, rather than borrowing the cluster pool", func() {
		reqs := []ModelScalingRequest{shareReq("A", "team-a", 4000, 4)}
		cons := []*ResourceConstraints{{
			Pools:          map[string]ResourcePool{"A100": {Limit: 16, Used: 4}},
			NamespacePools: map[string]map[string]ResourcePool{"team-a": {"H100": {Limit: 8, Used: 0}}},
		}}
		Expect(BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})).To(BeEmpty())
	})

	It("skips an unlimited namespace pool and a budget no cluster could hold", func() {
		reqs := []ModelScalingRequest{shareReq("A", "team-a", 4000, 4)}
		unlimited := []*ResourceConstraints{{NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: -1}}}}}
		Expect(BuildShareGroups(reqs, unlimited, ShareGroupOptions{})).To(BeEmpty())

		huge := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: MaxShareBudgetGPUs + 1, Used: 0}}}}
		Expect(BuildShareGroups([]ModelScalingRequest{shareReq("A", "ns", 4000, 4)}, huge, ShareGroupOptions{ClusterIsQuota: true})).To(BeEmpty())
	})
})
