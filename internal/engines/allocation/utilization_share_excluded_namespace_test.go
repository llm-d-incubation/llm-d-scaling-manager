package allocation

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A namespace-scoped quota that excludes a namespace bounds that namespace not
// at all (quota-limiter.md, "exclude"). Its pools are the sum of the caps of
// the namespaces it lists, net of only their holdings, so a cluster group
// planned over them gave the excluded namespace the other tenants' free quota
// plus whatever it already held: a budget of 12, 24 and 36 as it held 2, 14
// and 26, and an idle model there was filled toward its maxReplicas.
var _ = Describe("BuildShareGroups: a namespace a namespace quota excludes", func() {
	namespaceQuota := config.QuotaLimiterConfig{
		Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
		Exclude:         []string{"ns-x"},
		NamespaceQuotas: map[string]map[string]int{"team-a": {"A100": 8}, "team-b": {"A100": 6}},
	}
	constraintsOf := func(byNS map[string]map[string]int, entries ...config.QuotaLimiterConfig) []*ResourceConstraints {
		byType := map[string]int{}
		for _, perType := range byNS {
			for acc, n := range perType {
				byType[acc] += n
			}
		}
		out := make([]*ResourceConstraints, 0, len(entries))
		for _, q := range entries {
			rc, err := NewDefaultLimiter(q.Name, NewQuotaInventory(q)).ComputeConstraints(context.Background(), byType, byNS)
			Expect(err).NotTo(HaveOccurred())
			out = append(out, rc)
		}
		return out
	}
	// build returns the groups by scope ("" for the cluster group) and the
	// models reported unshared, as "namespace/model: why".
	build := func(reqs []ModelScalingRequest, cons []*ResourceConstraints, opts ShareGroupOptions) (map[string]ShareGroup, []string) {
		var unshared []string
		opts.Unshared = func(req ModelScalingRequest, why string) {
			unshared = append(unshared, modelKey(req)+": "+why)
		}
		groups := map[string]ShareGroup{}
		for _, g := range BuildShareGroups(reqs, cons, opts) {
			groups[g.Scope] = g
		}
		return groups, unshared
	}
	fleet := func(held int) []ModelScalingRequest {
		return []ModelScalingRequest{
			shareReq("ma", "team-a", 1000, 2),
			shareReq("mb", "team-b", 1000, 2),
			shareReq("mx", "ns-x", 100, held), // nearly idle: a need of 0.125 GPU
		}
	}
	// node is a model whose one replica takes a whole 8-GPU node.
	node := func(id, ns string, replicas int) ModelScalingRequest {
		req := shareReq(id, ns, 9000, replicas)
		req.VariantStates[0].GPUsPerReplica = 8
		return req
	}
	excluded := []string{"ns-x/mx: " + UnsharedExcludedNamespace("A100")}
	// expectNotPlanned checks that no group plans a role or a variant of the
	// excluded namespace, not only that the cluster group is absent.
	expectNotPlanned := func(groups map[string]ShareGroup) {
		GinkgoHelper()
		for scope, g := range groups {
			for _, r := range g.Roles {
				Expect(r.Key).NotTo(HavePrefix("ns-x/"), "group %q plans %s", scope, r.Key)
			}
			for k := range g.Variants {
				Expect(k).NotTo(HavePrefix("ns-x/"), "group %q lists variants of %s", scope, k)
			}
		}
	}

	It("forms no cluster group without a cluster quota, however much the namespace holds", func() {
		for _, held := range []int{2, 14, 26} {
			cons := constraintsOf(map[string]map[string]int{
				"team-a": {"A100": 2}, "team-b": {"A100": 2}, "ns-x": {"A100": held},
			}, namespaceQuota)
			// ClusterIsQuota as the engine used to pass it for any quota entry:
			// the namespace quota's sum must not be read as a cluster budget.
			for _, opts := range []ShareGroupOptions{{ClusterIsQuota: true}, {PhysicalGroups: true}, {}} {
				groups, unshared := build(fleet(held), cons, opts)
				Expect(groups).NotTo(HaveKey(""), "held=%d: a cluster group over the other tenants' quota", held)
				expectNotPlanned(groups)
				Expect(unshared).To(Equal(excluded), "held=%d", held)
				// The tenants keep their own groups and their own budgets.
				Expect(groups).To(HaveKey("team-a"))
				Expect(groups).To(HaveKey("team-b"))
				Expect(groups["team-a"].Budget).To(Equal(8), "held=%d: team-a's free 6 + its 2", held)
				Expect(groups["team-b"].Budget).To(Equal(6), "held=%d: team-b's free 4 + its 2", held)
				// Nothing bounds a namespace group's fill but its own budget:
				// the tenants' summed quota is not the cluster's free GPUs.
				Expect(groups["team-a"].ClusterFree).To(BeNumerically(">", 1<<30))
			}
		}
	})

	It("forms no cluster group when the excluded namespace is the only one active", func() {
		// No active namespace is capped, so the namespace quota carries no
		// NamespacePools; its Pools are still only the tenants' summed caps.
		cons := constraintsOf(map[string]map[string]int{"ns-x": {"A100": 3}}, namespaceQuota)
		Expect(cons[0].NamespacePools).To(BeEmpty())
		Expect(cons[0].NamespaceScoped).To(BeTrue())
		groups, unshared := build([]ModelScalingRequest{shareReq("mx", "ns-x", 100, 3)}, cons, ShareGroupOptions{ClusterIsQuota: true})
		Expect(groups).To(BeEmpty())
		Expect(unshared).To(Equal(excluded))
	})

	It("says which accelerator type has no finite cluster quota", func() {
		Expect(UnsharedExcludedNamespace("H100")).To(Equal(
			"excluded from the namespace quota, and no finite scope: cluster quota covers H100; add one to share it"))
	})

	It("asks nothing more of an unshared model: no exclusion check, no weight", func() {
		cons := constraintsOf(map[string]map[string]int{
			"team-a": {"A100": 2}, "team-b": {"A100": 2}, "ns-x": {"A100": 2},
		}, namespaceQuota)
		var asked []string
		opts := ShareGroupOptions{
			ClusterIsQuota: true,
			Exclude: func(req ModelScalingRequest, _ bool) string {
				asked = append(asked, "exclude "+modelKey(req))
				return ""
			},
			Weight: func(req ModelScalingRequest, _ bool) float64 {
				asked = append(asked, "weight "+modelKey(req))
				return 1
			},
		}
		groups, unshared := build(fleet(2), cons, opts)
		Expect(unshared).To(Equal(excluded))
		expectNotPlanned(groups)
		Expect(asked).NotTo(ContainElement(HaveSuffix("ns-x/mx")))
		Expect(asked).To(ContainElement("exclude team-a/ma"))
	})

	It("does not share it under a cluster quota of -1 for its type: an unlimited quota is no finite bound", func() {
		unlimited := config.QuotaLimiterConfig{
			Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"A100": -1},
		}
		cons := constraintsOf(map[string]map[string]int{
			"team-a": {"A100": 2}, "team-b": {"A100": 2}, "ns-x": {"A100": 2},
		}, namespaceQuota, unlimited)
		groups, unshared := build(fleet(2), cons, ShareGroupOptions{ClusterIsQuota: true})
		Expect(groups).NotTo(HaveKey(""))
		expectNotPlanned(groups)
		Expect(unshared).To(Equal(excluded))
	})

	It("plans it in the cluster group over what the cluster quota leaves after the tenants' claims", func() {
		clusterQuota := config.QuotaLimiterConfig{
			Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"A100": 20},
		}
		for _, c := range []struct {
			held, budget, idle int
		}{
			// The cluster quota's free 20-4-held, less what team-a (free 6)
			// and team-b (free 4) may still claim of it. Past that ns-x keeps
			// what it holds and fills nothing. Over the quota itself it is
			// planned over what the quota leaves it, 20-4, and the rest is a
			// debt.
			{held: 2, budget: 2 + 4, idle: 4}, // 14 free; 14-(6+4) = 4
			{held: 8, budget: 8 + 0, idle: 0}, // 8 free; 8-(6+4) < 0
			{held: 14, budget: 14, idle: 0},   // 2 free
			{held: 26, budget: 16, idle: 0},   // over the quota by 10
		} {
			cons := constraintsOf(map[string]map[string]int{
				"team-a": {"A100": 2}, "team-b": {"A100": 2}, "ns-x": {"A100": c.held},
			}, namespaceQuota, clusterQuota)
			groups, unshared := build(fleet(c.held), cons, ShareGroupOptions{ClusterIsQuota: true})
			Expect(unshared).To(BeEmpty())
			Expect(groups).To(HaveKey(""))
			g := groups[""]
			Expect(g.Roles).To(HaveLen(1))
			Expect(g.Roles[0].Key).To(Equal("ns-x/mx/both"))
			Expect(g.Budget).To(Equal(c.budget), "held=%d", c.held)
			Expect(max(0, g.Budget-g.Committed["ns-x/mx/both"])).To(Equal(c.idle), "held=%d", c.held)
			Expect(g.ClusterFree).To(Equal(c.idle), "held=%d: what the tenants leave of the cluster free", c.held)
			// The tenants keep their own budgets; their fills share the
			// cluster free with the cluster group's (splitClusterFree), so
			// the three together never fill more than it holds.
			clusterFree := max(0, 20-4-c.held)
			Expect(groups["team-a"].Budget).To(Equal(8))
			Expect(groups["team-b"].Budget).To(Equal(6))
			Expect(groups["team-a"].ClusterFree).To(BeNumerically("<=", clusterFree), "held=%d", c.held)
			Expect(g.ClusterFree+groups["team-a"].ClusterFree+groups["team-b"].ClusterFree).
				To(BeNumerically("<=", clusterFree), "held=%d", c.held)
		}
	})

	// A tenant that forms no share group still grows into its own free quota
	// through today's optimizer. Its claim was made only by a group formed
	// this cycle, so a tenant disabled for sharing, or one whose model spans
	// two accelerator types, left its headroom to the cluster group's idle
	// fill: ns-x was budgeted 2+8 instead of 2+4.
	It("protects the headroom of a tenant that forms no group", func() {
		clusterQuota := config.QuotaLimiterConfig{
			Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"A100": 20},
		}
		cons := constraintsOf(map[string]map[string]int{
			"team-a": {"A100": 2}, "team-b": {"A100": 2}, "ns-x": {"A100": 2},
		}, namespaceQuota, clusterQuota)
		// team-b's model spans A100 and H100: a fixed consumer, in no group.
		mixedEntry := withSatEntry(&satEntryFixture{
			ModelID: "mb", Namespace: "team-b", TotalDemand: 1000,
			VariantCapacities: []vcFixture{
				{VariantName: "mb-v", AcceleratorName: "A100", Cost: 5, ReplicaCount: 2, PerReplicaCapacity: 1000},
				{VariantName: "mb-h100", AcceleratorName: "H100", Cost: 5, ReplicaCount: 0, PerReplicaCapacity: 1000},
			},
		}, ModelScalingRequest{
			ModelID: "mb", Namespace: "team-b",
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "mb-v", CurrentReplicas: 2, GPUsPerReplica: 1, MinReplicas: ptrInt(1)},
				{VariantName: "mb-h100", CurrentReplicas: 0, GPUsPerReplica: 1, MinReplicas: ptrInt(0)},
			},
		})
		mixedEntry.CompositeSignal.ScaleUpThreshold = 0.8
		for name, c := range map[string]struct {
			reqs []ModelScalingRequest
			opts ShareGroupOptions
		}{
			"team-b disabled for sharing": {fleet(2), ShareGroupOptions{ClusterIsQuota: true,
				NamespaceEnabled: func(ns string) bool { return !strings.HasPrefix(ns, "team-b") }}},
			"team-b on mixed accelerators": {[]ModelScalingRequest{
				shareReq("ma", "team-a", 1000, 2), mixedEntry, shareReq("mx", "ns-x", 100, 2)},
				ShareGroupOptions{ClusterIsQuota: true}},
		} {
			groups, _ := build(c.reqs, cons, c.opts)
			Expect(groups).NotTo(HaveKey("team-b"), name)
			Expect(groups).To(HaveKey(""), name)
			// The cluster quota's free 20-6 = 14, less team-a's 6 and team-b's 4.
			Expect(groups[""].Budget).To(Equal(2+4), name)
			Expect(groups[""].ClusterFree).To(Equal(4), name)
		}
	})

	// Every group's idle fill is charged to the cluster quota, so the groups
	// together must not plan more than it has free. a6d6e1828 gave the cluster
	// group the cluster quota's whole free count beside the tenants' own:
	// 14 + 6 + 4 = 24 planned at a cluster quota of 20 with 14 free.
	It("plans no more idle fill across all groups than the cluster quota has free", func() {
		for _, limit := range []int{20, 100} {
			clusterQuota := config.QuotaLimiterConfig{
				Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"A100": limit},
			}
			cons := constraintsOf(map[string]map[string]int{
				"team-a": {"A100": 2}, "team-b": {"A100": 2}, "ns-x": {"A100": 2},
			}, namespaceQuota, clusterQuota)
			reqs := []ModelScalingRequest{
				shareReq("ma", "team-a", 9000, 2), shareReq("mb", "team-b", 9000, 2), shareReq("mx", "ns-x", 9000, 2),
			}
			groups, unshared := build(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})
			Expect(unshared).To(BeEmpty())
			Expect(groups).To(HaveLen(3))
			clusterFree := limit - 6
			idle := map[string]int{}
			total := 0
			for scope, g := range groups {
				committed := 0
				for _, n := range g.Committed {
					committed += n
				}
				idle[scope] = max(0, min(g.Budget-committed, g.ClusterFree))
				total += idle[scope]
			}
			Expect(total).To(BeNumerically("<=", clusterFree), "limit=%d: idle by group %v", limit, idle)
			// The tenants' own fill is untouched; the excluded namespace gets
			// the rest of the cluster free.
			Expect(idle).To(Equal(map[string]int{"team-a": 6, "team-b": 4, "": clusterFree - 10}), "limit=%d", limit)
		}
	})

	It("never budgets the cluster group below what it holds when the tenants claim more than is free", func() {
		// Whole 8-GPU nodes: the tenants may claim 16 + 8, the cluster has 8
		// free. The cluster group keeps its 16 GPUs, and fills nothing. The
		// one free node goes to one tenant whole (splitClusterFree), not half
		// to each, where neither half places a replica.
		cons := []*ResourceConstraints{
			{Pools: map[string]ResourcePool{"A100": {Limit: 48, Used: 40}}},
			{NamespaceScoped: true, NamespacePools: map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 24, Used: 8}},
				"team-b": {"A100": {Limit: 16, Used: 8}},
			}},
		}
		reqs := []ModelScalingRequest{node("ma", "team-a", 1), node("mb", "team-b", 1), node("mx", "ns-x", 2)}
		groups, _ := build(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})
		Expect(groups).To(HaveKey(""))
		Expect(groups[""].Budget).To(Equal(16))
		Expect(groups[""].ClusterFree).To(Equal(0))
		Expect(groups["team-a"].ClusterFree).To(Equal(8))
		Expect(groups["team-b"].ClusterFree).To(Equal(0))
	})

	// A tenant claims its free quota in whole replicas only. With 8-GPU
	// replicas, team-a's 4 free GPUs place nothing; counting them against the
	// cluster free left one free node that no group could fill: idle 4 in the
	// cluster group and 4 in team-a's, and neither fits an 8-GPU replica.
	It("claims a tenant's free quota in whole replicas, so a free node stays usable", func() {
		for _, c := range []struct {
			teamALimit, clusterIdle, teamIdle int
		}{
			// 4 free: no whole replica, claims 0, and fills none of the
			// cluster free (splitClusterFree), which it could not place.
			{teamALimit: 12, clusterIdle: 8, teamIdle: 0},
			{teamALimit: 16, clusterIdle: 0, teamIdle: 8}, // 8 free: one replica, claims 8
		} {
			cons := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 48, Used: 40}}},
				{NamespaceScoped: true, NamespacePools: map[string]map[string]ResourcePool{
					"team-a": {"A100": {Limit: c.teamALimit, Used: 8}},
				}},
			}
			reqs := []ModelScalingRequest{node("ma", "team-a", 1), node("mx", "ns-x", 1)}
			groups, unshared := build(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})
			Expect(unshared).To(BeEmpty())
			idle := map[string]int{}
			for scope, g := range groups {
				committed := 0
				for _, n := range g.Committed {
					committed += n
				}
				idle[scope] = max(0, min(g.Budget-committed, g.ClusterFree))
			}
			Expect(idle).To(Equal(map[string]int{"": c.clusterIdle, "team-a": c.teamIdle}),
				"team-a limit %d", c.teamALimit)
		}
	})

	// A tenant that forms no group claims too (MT6), and its claim is rounded
	// to whole replicas the same way: of its own models' replica size, else of
	// the cluster group's. Unrounded, team-a's 4 free GPUs -- no 8-GPU replica
	// -- left the cluster group 4 of its one free node, which it cannot place.
	It("rounds the claim of a tenant that forms no group to whole replicas", func() {
		cons := []*ResourceConstraints{
			{Pools: map[string]ResourcePool{"A100": {Limit: 48, Used: 40}}},
			{NamespaceScoped: true, NamespacePools: map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 12, Used: 8}},
			}},
		}
		for name, c := range map[string]struct {
			reqs []ModelScalingRequest
			opts ShareGroupOptions
		}{
			"team-a disabled for sharing": {[]ModelScalingRequest{node("ma", "team-a", 1), node("mx", "ns-x", 1)},
				ShareGroupOptions{ClusterIsQuota: true, NamespaceEnabled: func(ns string) bool { return ns != "team-a" }}},
			"team-a with no model": {[]ModelScalingRequest{node("mx", "ns-x", 1)}, ShareGroupOptions{ClusterIsQuota: true}},
		} {
			groups, _ := build(c.reqs, cons, c.opts)
			Expect(groups).NotTo(HaveKey("team-a"), name)
			Expect(groups).To(HaveKey(""), name)
			Expect(groups[""].ClusterFree).To(Equal(8), name)
		}
	})

	// A tenant with a 1-GPU and an 8-GPU variant on one type grows only the
	// variant shareGrowVariant picks. Rounding its claim to the smallest
	// variant claimed 4 of the one free node for an 8-GPU grow it could not
	// place, and left the cluster group 4, too few for its 8-GPU replica: the
	// node stayed idle. The claim is rounded to the replicas it grows.
	It("rounds a tenant's claim to the variant it grows, not its smallest", func() {
		cons := []*ResourceConstraints{
			{Pools: map[string]ResourcePool{"A100": {Limit: 48, Used: 40}}},
			{NamespaceScoped: true, NamespacePools: map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 12, Used: 8}},
			}},
		}
		// mixed is team-a's model: an 8-GPU and a 1-GPU variant, the 8-GPU
		// one the most cost-efficient unless smallFirst.
		mixed := func(smallFirst bool) ModelScalingRequest {
			bigCost, smallCost := 5.0, 5.0
			if smallFirst {
				bigCost = 500
			} else {
				smallCost = 500
			}
			req := withSatEntry(&satEntryFixture{
				ModelID: "ma", Namespace: "team-a", TotalDemand: 9000,
				VariantCapacities: []vcFixture{
					{VariantName: "ma-big", AcceleratorName: "A100", Cost: bigCost, ReplicaCount: 1, PerReplicaCapacity: 8000},
					{VariantName: "ma-small", AcceleratorName: "A100", Cost: smallCost, ReplicaCount: 0, PerReplicaCapacity: 1000},
				},
			}, ModelScalingRequest{
				ModelID: "ma", Namespace: "team-a",
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "ma-big", CurrentReplicas: 1, GPUsPerReplica: 8, MinReplicas: ptrInt(0)},
					{VariantName: "ma-small", CurrentReplicas: 0, GPUsPerReplica: 1, MinReplicas: ptrInt(0)},
				},
			})
			req.CompositeSignal.ScaleUpThreshold = 0.8
			return req
		}
		for _, c := range []struct {
			smallFirst              bool
			grow                    string
			clusterIdle, tenantIdle int
		}{
			// Grows 8-GPU replicas: 4 free place none, so it claims none,
			// and the cluster group can fill the free node.
			{smallFirst: false, grow: "ma-big", clusterIdle: 8, tenantIdle: 0},
			// Grows 1-GPU replicas: it can place all 4, and claims them.
			{smallFirst: true, grow: "ma-small", clusterIdle: 0, tenantIdle: 4},
		} {
			reqs := []ModelScalingRequest{mixed(c.smallFirst), node("mx", "ns-x", 1)}
			groups, unshared := build(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})
			Expect(unshared).To(BeEmpty())
			Expect(groups["team-a"].Grow["team-a/ma/both"].Name).To(Equal(c.grow))
			idle := map[string]int{}
			for scope, g := range groups {
				idle[scope] = max(0, min(idleClaim(g), g.ClusterFree))
			}
			Expect(idle).To(Equal(map[string]int{"": c.clusterIdle, "team-a": c.tenantIdle}), "grows %s", c.grow)
		}
	})

	// A tenant whose every role is at its ceiling grows nothing, so its fill
	// places nothing. It claimed its 8 free GPUs all the same, and the free
	// node went to it, not to the cluster group's 8-GPU replica.
	It("claims nothing for a tenant that grows nothing", func() {
		cons := []*ResourceConstraints{
			{Pools: map[string]ResourcePool{"A100": {Limit: 48, Used: 40}}},
			{NamespaceScoped: true, NamespacePools: map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 16, Used: 8}},
			}},
		}
		full := node("ma", "team-a", 1)
		full.VariantStates[0].MaxReplicas = ptrInt(1)
		groups, unshared := build([]ModelScalingRequest{full, node("mx", "ns-x", 1)}, cons, ShareGroupOptions{ClusterIsQuota: true})
		Expect(unshared).To(BeEmpty())
		Expect(groups["team-a"].Grow).To(BeEmpty())
		idle := map[string]int{}
		for scope, g := range groups {
			idle[scope] = max(0, min(idleClaim(g), g.ClusterFree))
		}
		Expect(idle).To(Equal(map[string]int{"": 8, "team-a": 0}))
	})

	// The models of a tenant that no group plans -- frozen, or spanning
	// several accelerator types -- still grow through today's optimizer, so
	// the claim counts them. Which of them can still grow decides it.
	Describe("a tenant's models that no group plans", func() {
		// 8 free GPUs in the cluster and 8 in team-a's quota of A100.
		consFor := func(teamAUsed int) []*ResourceConstraints {
			return []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 8 + teamAUsed + 8, Used: teamAUsed + 8}}},
				{NamespaceScoped: true, NamespacePools: map[string]map[string]ResourcePool{
					"team-a": {"A100": {Limit: teamAUsed + 8, Used: teamAUsed}},
				}},
			}
		}
		full := func() ModelScalingRequest {
			req := node("a1", "team-a", 1)
			req.VariantStates[0].MaxReplicas = ptrInt(1)
			return req
		}
		frozenA3 := ShareGroupOptions{ClusterIsQuota: true, Exclude: func(req ModelScalingRequest, _ bool) string {
			if req.ModelID == "a3" {
				return "frozen"
			}
			return ""
		}}
		// clusterGroup returns the cluster group's budget and the GPUs it may
		// place this cycle.
		clusterGroup := func(reqs []ModelScalingRequest, cons []*ResourceConstraints, opts ShareGroupOptions) (int, int) {
			GinkgoHelper()
			groups, unshared := build(reqs, cons, opts)
			Expect(unshared).To(BeEmpty())
			Expect(groups).To(HaveKey(""))
			return groups[""].Budget, groups[""].ClusterFree
		}

		// team-a's only planned model is at its ceiling, so its group grows
		// nothing. A model on A100 and H100 forms no group but can grow an
		// 8-GPU A100 replica. The group was taken to place nothing and
		// claimed nothing, and the cluster group filled the free node.
		It("keeps the claim of a multi-accelerator model that can grow", func() {
			multi := withSatEntry(&satEntryFixture{
				ModelID: "a2", Namespace: "team-a", TotalDemand: 9000,
				VariantCapacities: []vcFixture{
					{VariantName: "a2-a100", AcceleratorName: "A100", Cost: 5, ReplicaCount: 0, PerReplicaCapacity: 8000},
					{VariantName: "a2-h100", AcceleratorName: "H100", Cost: 9, ReplicaCount: 0, PerReplicaCapacity: 8000},
				},
			}, ModelScalingRequest{
				ModelID: "a2", Namespace: "team-a",
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "a2-a100", CurrentReplicas: 0, GPUsPerReplica: 8, MinReplicas: ptrInt(0)},
					{VariantName: "a2-h100", CurrentReplicas: 0, GPUsPerReplica: 8, MinReplicas: ptrInt(0)},
				},
			})
			multi.CompositeSignal.ScaleUpThreshold = 0.8
			budget, clusterFree := clusterGroup([]ModelScalingRequest{full(), multi, node("mx", "ns-x", 1)},
				consFor(8), ShareGroupOptions{ClusterIsQuota: true})
			Expect(budget).To(Equal(8), "ns-x holds 8; team-a claims the 8 free")
			Expect(clusterFree).To(Equal(0))
		})

		// A frozen model that cannot grow -- at its maxReplicas, or held from
		// scaling up -- placed nothing, yet its 1-GPU replica kept team-a's
		// whole free quota claimed, and the free node stayed idle. One that
		// can grow keeps the claim.
		It("claims nothing for frozen models that cannot grow, and keeps the claim of one that can", func() {
			atCeiling := shareReq("a3", "team-a", 9000, 1)
			atCeiling.VariantStates[0].MaxReplicas = ptrInt(1)
			noScaleUp := shareReq("a3", "team-a", 9000, 1)
			noScaleUp.CompositeSignal.Holds = map[string]RoleHold{domain.RoleBoth: {NoScaleUp: true}}
			canGrow := shareReq("a3", "team-a", 9000, 1)
			for name, c := range map[string]struct {
				a3                  ModelScalingRequest
				budget, clusterFree int
			}{
				"at its ceiling":       {atCeiling, 16, 8},
				"held from scaling up": {noScaleUp, 16, 8},
				"below its ceiling":    {canGrow, 8, 0},
			} {
				budget, clusterFree := clusterGroup([]ModelScalingRequest{full(), c.a3, node("mx", "ns-x", 1)},
					consFor(9), frozenA3)
				Expect(budget).To(Equal(c.budget), name)
				Expect(clusterFree).To(Equal(c.clusterFree), name)
			}
		})

		// team-a's group grows 8-GPU replicas, so its 4 free GPUs place none
		// of them. A frozen 1-GPU model can place all 4 through today's
		// optimizer, so they are claimed, and the cluster group's 1-GPU model
		// does not fill them.
		It("rounds the claim to the smallest replica anything in the namespace can grow", func() {
			mixed := withSatEntry(&satEntryFixture{
				ModelID: "a1", Namespace: "team-a", TotalDemand: 9000,
				VariantCapacities: []vcFixture{
					{VariantName: "a1-big", AcceleratorName: "A100", Cost: 5, ReplicaCount: 1, PerReplicaCapacity: 8000},
					{VariantName: "a1-small", AcceleratorName: "A100", Cost: 500, ReplicaCount: 1, PerReplicaCapacity: 1000},
				},
			}, ModelScalingRequest{
				ModelID: "a1", Namespace: "team-a",
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "a1-big", CurrentReplicas: 1, GPUsPerReplica: 8, MinReplicas: ptrInt(0)},
					{VariantName: "a1-small", CurrentReplicas: 1, GPUsPerReplica: 1, MinReplicas: ptrInt(0), MaxReplicas: ptrInt(1)},
				},
			})
			mixed.CompositeSignal.ScaleUpThreshold = 0.8
			cons := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 15, Used: 11}}},
				{NamespaceScoped: true, NamespacePools: map[string]map[string]ResourcePool{
					"team-a": {"A100": {Limit: 14, Used: 10}},
				}},
			}
			reqs := []ModelScalingRequest{mixed, shareReq("a3", "team-a", 9000, 1), shareReq("mx", "ns-x", 9000, 1)}
			groups, unshared := build(reqs, cons, frozenA3)
			Expect(unshared).To(BeEmpty())
			Expect(groups["team-a"].Grow["team-a/a1/both"].Name).To(Equal("a1-big"))
			Expect(groups[""].Budget).To(Equal(1), "ns-x holds 1; team-a claims the 4 free")
			Expect(groups[""].ClusterFree).To(Equal(0))
		})
	})

	// A claim rounded down to whole replicas leaves part of the namespace's
	// free quota unclaimed. A warm pool there can grow into that remainder,
	// so the cluster group carves it: team-a has 12 free, claims one 8-GPU
	// replica, and its pool wanting 8 may still take the other 4.
	It("carves the part of a claimed namespace's pool that rounding left unclaimed", func() {
		cons := []*ResourceConstraints{
			{Pools: map[string]ResourcePool{"A100": {Limit: 48, Used: 16}}},
			{NamespaceScoped: true, NamespacePools: map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 20, Used: 8}},
			}},
		}
		reqs := []ModelScalingRequest{node("ma", "team-a", 1), node("mx", "ns-x", 1)}
		groups, _ := build(reqs, cons, ShareGroupOptions{ClusterIsQuota: true,
			PoolUnheld: map[string]map[string]int{"team-a": {"A100": 8}}})
		Expect(groups).To(HaveKey(""))
		Expect(groups[""].PoolCarve).To(Equal(4))
		Expect(groups["team-a"].PoolCarve).To(Equal(8))
	})

	// config.quotaBoundsNamespaceLocked counts a namespace a scope: namespace
	// entry does not list (and no `default` covers) as bounded, so a
	// namespace-scoped controller there gets no physicalGroups note. That holds
	// only while such a namespace is in the quota's pools with nothing in
	// them, and no physical group plans it.
	It("plans no physical group for a namespace the namespace quota does not list", func() {
		listed := config.QuotaLimiterConfig{
			Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
			NamespaceQuotas: map[string]map[string]int{"team-a": {"A100": 8}},
		}
		cons := constraintsOf(map[string]map[string]int{"team-a": {"A100": 2}, "team-z": {"A100": 2}}, listed)
		Expect(cons[0].NamespacePools).To(HaveKeyWithValue("team-z", BeEmpty()))
		inventory := &ResourceConstraints{Pools: map[string]ResourcePool{"A100": {Limit: 32, Used: 4}}}
		groups, _ := build([]ModelScalingRequest{shareReq("mz", "team-z", 4000, 2)},
			append(cons, inventory), ShareGroupOptions{PhysicalGroups: true})
		Expect(groups).To(BeEmpty())
	})

	It("reports nothing unshared without a namespace quota", func() {
		reqs := []ModelScalingRequest{shareReq("mx", "ns-x", 100, 2)}
		for name, cons := range map[string][]*ResourceConstraints{
			// A cluster quota of -1: no finite bound, and no namespace quota.
			"unlimited cluster quota": {{Pools: map[string]ResourcePool{"A100": {Limit: -1}}}},
			// An inventory with no pool of the model's type.
			"inventory without the type": {{Pools: map[string]ResourcePool{"H100": {Limit: 8}}}},
		} {
			for _, opts := range []ShareGroupOptions{{ClusterIsQuota: true}, {PhysicalGroups: true}} {
				groups, unshared := build(reqs, cons, opts)
				Expect(unshared).To(BeEmpty(), name)
				Expect(groups).To(BeEmpty(), name)
			}
		}
	})

	It("forms no physical group in quota mode: the namespace quota's sum is not the inventory", func() {
		cons := constraintsOf(map[string]map[string]int{
			"team-a": {"A100": 2}, "team-b": {"A100": 2}, "ns-x": {"A100": 2},
		}, namespaceQuota)
		groups, unshared := build(fleet(2), cons, ShareGroupOptions{PhysicalGroups: true})
		Expect(groups).NotTo(HaveKey(""))
		expectNotPlanned(groups)
		Expect(unshared).To(Equal(excluded))
	})

	// NamespaceResourcePools documents that a namespace-scoped inventory
	// returns a map; one that carries NamespacePools without setting
	// NamespaceScoped (a third-party inventory, or an older one) is still no
	// budget over every namespace.
	It("reads a provider with namespace pools as namespace-scoped, whether or not it says so", func() {
		cluster := &ResourceConstraints{Pools: map[string]ResourcePool{"A100": {Limit: 20, Used: 6}}}
		unflagged := &ResourceConstraints{
			Pools:          map[string]ResourcePool{"A100": {Limit: 14, Used: 4}},
			NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: 8, Used: 2}}},
		}
		available, namespaced := clusterBudgets([]*ResourceConstraints{unflagged, cluster, nil})
		Expect(namespaced).To(BeTrue())
		Expect(available).To(Equal(map[string]int{"A100": 14}), "the cluster quota's free, not the namespace sum's 10")

		available, namespaced = clusterBudgets([]*ResourceConstraints{unflagged})
		Expect(namespaced).To(BeTrue())
		Expect(available).To(BeEmpty())
	})

	It("still plans every namespace in a physical group in inventory mode", func() {
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 6}}}}
		groups, unshared := build(fleet(2), cons, ShareGroupOptions{PhysicalGroups: true})
		Expect(unshared).To(BeEmpty())
		Expect(groups).To(HaveKey(""))
		Expect(groups[""].Roles).To(HaveLen(3))
		Expect(groups[""].Budget).To(Equal(10 + 6))
	})
})
