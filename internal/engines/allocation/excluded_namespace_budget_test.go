package allocation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A namespace that a namespace quota lists in `exclude` bypasses that quota
// (quota-limiter.md, "exclude"). The default optimizer and the scale-from-zero
// check still bounded it by the quota's per-type pools, which are the other
// tenants' free quota: it took a capped tenant's own free GPUs, and its wakes
// were refused once the tenants had spent theirs (#169).
var _ = Describe("a namespace a namespace quota excludes", func() {
	tenants := config.QuotaLimiterConfig{
		Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
		Exclude:         []string{"ns-x"},
		NamespaceQuotas: map[string]map[string]int{"team-a": {"A100": 8}, "team-b": {"A100": 6}},
	}
	clusterQuota := func(limit int) config.QuotaLimiterConfig {
		return config.QuotaLimiterConfig{
			Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"A100": limit},
		}
	}
	// constraintsOf builds the constraints with the real limiter, fed what each
	// namespace holds, as the engine feeds a quota.
	constraintsOf := func(byNS map[string]map[string]int, entries ...config.QuotaLimiterConfig) []*ResourceConstraints {
		GinkgoHelper()
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
	// modelOn holds `held` replicas of `gpus` GPUs each on acc and wants
	// `want` more. Its one variant is named after the model.
	modelOn := func(acc, id, ns string, held, want, gpus int) ModelScalingRequest {
		const perReplica = 10000.0
		r := &satEntryFixture{
			ModelID: id, Namespace: ns, AnalyzedAt: time.Now(),
			TotalDemand:      float64(held+want) * perReplica,
			RequiredCapacity: float64(want) * perReplica,
			VariantCapacities: []vcFixture{
				{VariantName: id + "-v", AcceleratorName: acc, Cost: 10, ReplicaCount: held, PerReplicaCapacity: perReplica},
			},
		}
		return withSatEntry(r, ModelScalingRequest{
			ModelID: id, Namespace: ns, Priority: 1, // rescale weighs by priority
			VariantStates: []domain.VariantReplicaState{
				{VariantName: id + "-v", CurrentReplicas: held, GPUsPerReplica: gpus},
			},
		})
	}
	model := func(id, ns string, held, want, gpus int) ModelScalingRequest {
		return modelOn("A100", id, ns, held, want, gpus)
	}
	targets := func(o *GreedyByScoreOptimizer, reqs []ModelScalingRequest, cons []*ResourceConstraints) map[string]domain.VariantDecision {
		return decisionMap(o.Optimize(context.Background(), reqs, cons))
	}

	Describe("the default optimizer", func() {
		// team-a holds 1 of its 8, team-b all 6 of its 6, ns-x 1. The tenants'
		// per-type pool has 7 free, all of them team-a's.
		held := map[string]map[string]int{"team-a": {"A100": 1}, "team-b": {"A100": 6}, "ns-x": {"A100": 1}}

		It("gives a capped tenant its own free quota beside an excluded namespace", func() {
			cons := constraintsOf(held, tenants)
			alone := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{model("ma", "team-a", 1, 7, 1)}, cons)
			Expect(alone["ma-v"].TargetReplicas).To(Equal(8))

			dm := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{
				model("ma", "team-a", 1, 7, 1), model("mx", "ns-x", 1, 20, 1),
			}, cons)
			Expect(dm["ma-v"].TargetReplicas).To(Equal(8), "team-a's own 7 free GPUs")
			Expect(dm["mx-v"].TargetReplicas).To(Equal(21), "no quota bounds ns-x: it gets its demand")
			Expect(dm["mx-v"].WasLimited).To(BeFalse())
		})

		It("gives a tenant its free node beside an excluded namespace of whole-node replicas", func() {
			// 8-GPU replicas, one per node. team-a may hold 2 and holds 1;
			// team-b holds its 1. The tenants' pool has one node free, team-a's.
			wholeNode := config.QuotaLimiterConfig{
				Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
				Exclude:         []string{"ns-x"},
				NamespaceQuotas: map[string]map[string]int{"team-a": {"A100": 16}, "team-b": {"A100": 8}},
			}
			cons := constraintsOf(map[string]map[string]int{
				"team-a": {"A100": 8}, "team-b": {"A100": 8}, "ns-x": {"A100": 16},
			}, wholeNode)
			dm := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{
				model("ma", "team-a", 1, 1, 8), model("mx", "ns-x", 2, 3, 8),
			}, cons)
			Expect(dm["ma-v"].TargetReplicas).To(Equal(2), "team-a's free node")
			Expect(dm["mx-v"].TargetReplicas).To(Equal(5), "no quota bounds ns-x")
		})

		It("bounds an excluded namespace by a cluster quota, not by the tenants' quota", func() {
			// Cluster quota 100, 8 held: 92 free. The tenants' pool has 7.
			cons := constraintsOf(held, tenants, clusterQuota(100))
			dm := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{
				model("ma", "team-a", 1, 7, 1), model("mx", "ns-x", 1, 20, 1),
			}, cons)
			Expect(dm["ma-v"].TargetReplicas).To(Equal(8))
			Expect(dm["mx-v"].TargetReplicas).To(Equal(21))

			// Alone and wanting more than the cluster quota has free, ns-x is
			// stopped by the cluster quota, and the decision names it.
			cons = constraintsOf(held, tenants, clusterQuota(58)) // 50 free
			dm = targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{model("mx", "ns-x", 1, 200, 1)}, cons)
			Expect(dm["mx-v"].TargetReplicas).To(Equal(51))
			Expect(dm["mx-v"].WasLimited).To(BeTrue())
			Expect(dm["mx-v"].LimitedBy).To(Equal("cluster"))
		})

		It("never spends more than a cluster quota has free across tenants and the excluded namespace", func() {
			for _, c := range []struct {
				name             string
				quota            config.QuotaLimiterConfig
				byNS             map[string]map[string]int
				limit, gpus      int
				teamWant, nsWant int
				teamCap          int
			}{
				{
					name: "1-GPU replicas, 4 free", quota: tenants, limit: 12, gpus: 1, teamWant: 7, nsWant: 20, teamCap: 8,
					byNS: held,
				},
				{
					// One 8-GPU node free in the cluster quota; both want it.
					name: "whole-node replicas, one node free", limit: 40, gpus: 8, teamWant: 1, nsWant: 3, teamCap: 2,
					quota: config.QuotaLimiterConfig{
						Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
						Exclude:         []string{"ns-x"},
						NamespaceQuotas: map[string]map[string]int{"team-a": {"A100": 16}, "team-b": {"A100": 8}},
					},
					byNS: map[string]map[string]int{"team-a": {"A100": 8}, "team-b": {"A100": 8}, "ns-x": {"A100": 16}},
				},
			} {
				cons := constraintsOf(c.byNS, c.quota, clusterQuota(c.limit))
				teamHeld, nsHeld := c.byNS["team-a"]["A100"]/c.gpus, c.byNS["ns-x"]["A100"]/c.gpus
				// Both orders: whoever the fair share serves first, the other
				// must see what it took.
				for _, reqs := range [][]ModelScalingRequest{
					{model("ma", "team-a", teamHeld, c.teamWant, c.gpus), model("mx", "ns-x", nsHeld, c.nsWant, c.gpus)},
					{model("mx", "ns-x", nsHeld, c.nsWant, c.gpus), model("ma", "team-a", teamHeld, c.teamWant, c.gpus)},
				} {
					dm := targets(NewGreedyByScoreOptimizer(), reqs, cons)
					total := (dm["ma-v"].TargetReplicas + dm["mx-v"].TargetReplicas + c.byNS["team-b"]["A100"]/c.gpus) * c.gpus
					Expect(total).To(BeNumerically("<=", c.limit), c.name)
					Expect(dm["ma-v"].TargetReplicas).To(BeNumerically("<=", c.teamCap), c.name)
					Expect(total).To(Equal(c.limit), "%s: the cluster quota's free GPUs are used", c.name)
				}
			}
		})

		It("does not rescale an excluded namespace over the tenants' quota", func() {
			// Rescale on for the cluster group. The excluded namespace's two
			// models want 16 together; without a cluster quota nothing bounds
			// them, so there is nothing to rescale and each gets its demand.
			o := NewGreedyByScoreOptimizer()
			o.Rescale = RescaleFlags{Cluster: true}
			cons := constraintsOf(map[string]map[string]int{
				"team-a": {"A100": 1}, "team-b": {"A100": 6}, "ns-x": {"A100": 2},
			}, tenants)
			dm := targets(o, []ModelScalingRequest{model("mx", "ns-x", 1, 7, 1), model("my", "ns-x", 1, 7, 1)}, cons)
			Expect(dm["mx-v"].TargetReplicas).To(Equal(8))
			Expect(dm["my-v"].TargetReplicas).To(Equal(8))
		})

		It("still caps a listed tenant by its own entry, and denies a namespace with no entry", func() {
			cons := constraintsOf(map[string]map[string]int{
				"team-a": {"A100": 1}, "team-b": {"A100": 6}, "ns-x": {"A100": 1}, "team-c": {},
			}, tenants)
			dm := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{
				model("ma", "team-a", 1, 20, 1), model("mb", "team-b", 6, 5, 1),
				model("mc", "team-c", 0, 5, 1), model("mx", "ns-x", 1, 20, 1),
			}, cons)
			Expect(dm["ma-v"].TargetReplicas).To(Equal(8), "team-a's own quota")
			Expect(dm["mb-v"].TargetReplicas).To(Equal(6), "team-b is full")
			Expect(dm["mc-v"].TargetReplicas).To(Equal(0), "no entry and no default: denied")
			Expect(dm["mx-v"].TargetReplicas).To(Equal(21))
		})

		// A cluster quota is a closed allowlist: a type it does not list is
		// denied to every namespace, the excluded one included. A type it lists
		// as unlimited bounds nothing.
		It("denies an excluded namespace, as it denies a tenant, a type the cluster quota does not list", func() {
			cons := constraintsOf(held, tenants, config.QuotaLimiterConfig{
				Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 16},
			})
			open := constraintsOf(held, tenants, config.QuotaLimiterConfig{
				Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 16, "A100": -1},
			})
			for _, rescale := range []bool{false, true} {
				o := NewGreedyByScoreOptimizer()
				o.Rescale = RescaleFlags{Cluster: rescale}
				reqs := []ModelScalingRequest{model("ma", "team-a", 1, 2, 1), model("mx", "ns-x", 1, 20, 1)}
				dm := targets(o, reqs, cons)
				Expect(dm["ma-v"].TargetReplicas).To(Equal(1), "rescale=%v: the cluster quota does not list A100", rescale)
				Expect(dm["mx-v"].TargetReplicas).To(Equal(1), "rescale=%v: the cluster quota does not list A100", rescale)

				dm = targets(o, reqs, open)
				Expect(dm["ma-v"].TargetReplicas).To(Equal(3))
				Expect(dm["mx-v"].TargetReplicas).To(Equal(21), "rescale=%v: the cluster quota leaves A100 unlimited", rescale)
			}
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"A100": 1})).To(BeFalse())
			Expect(FitsGPUBudget(open, "ns-x", map[string]int{"A100": 1000})).To(BeTrue())
		})

		It("bounds each accelerator type of an excluded namespace on its own", func() {
			// team-a is capped on H100; ns-x runs on A100, which the cluster
			// quota bounds (10 free), and on H100, which it leaves unlimited.
			h100Tenants := config.QuotaLimiterConfig{
				Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
				Exclude:         []string{"ns-x"},
				NamespaceQuotas: map[string]map[string]int{"team-a": {"H100": 8}},
			}
			cons := constraintsOf(map[string]map[string]int{
				"team-a": {"H100": 1}, "ns-x": {"A100": 2},
			}, h100Tenants, config.QuotaLimiterConfig{
				Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"A100": 12, "H100": -1},
			})
			dm := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{
				modelOn("H100", "ma", "team-a", 1, 3, 1),
				modelOn("A100", "mx", "ns-x", 2, 20, 1),
				modelOn("H100", "my", "ns-x", 0, 5, 1),
			}, cons)
			Expect(dm["ma-v"].TargetReplicas).To(Equal(4), "team-a's own H100 quota")
			Expect(dm["mx-v"].TargetReplicas).To(Equal(12), "the cluster quota's 10 free A100")
			Expect(dm["mx-v"].LimitedBy).To(Equal("cluster"))
			Expect(dm["my-v"].TargetReplicas).To(Equal(5), "nothing bounds ns-x on H100")

			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"A100": 10})).To(BeTrue())
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"A100": 11})).To(BeFalse())
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"H100": 100})).To(BeTrue())
			budgets, _ := GPUBudgets(cons, "ns-x")
			Expect(budgets).To(HaveLen(2))
			Expect(budgets).To(HaveKeyWithValue("A100", 10))
			Expect(isUnboundedBudget(budgets["H100"])).To(BeTrue(), "%v", budgets)
		})

		It("leaves an excluded namespace unbounded under an unlimited cluster quota", func() {
			cons := constraintsOf(held, tenants, config.QuotaLimiterConfig{
				Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"A100": -1},
			})
			dm := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{
				model("ma", "team-a", 1, 20, 1), model("mx", "ns-x", 1, 20, 1),
			}, cons)
			Expect(dm["ma-v"].TargetReplicas).To(Equal(8), "team-a's own quota")
			Expect(dm["mx-v"].TargetReplicas).To(Equal(21))
			Expect(dm["mx-v"].WasLimited).To(BeFalse())
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"A100": 1000})).To(BeTrue())
		})

		It("does not let an excluded namespace decide whether an unlimited tenant scales", func() {
			// team-u's quota grants A100 without limit, and no cluster bound is in
			// force. Such a tenant scales to its demand, with or without an
			// excluded namespace beside it.
			unlimited := config.QuotaLimiterConfig{
				Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
				Exclude:         []string{"ns-x"},
				NamespaceQuotas: map[string]map[string]int{"team-u": {"A100": -1}},
			}
			cons := constraintsOf(map[string]map[string]int{"team-u": {"A100": 1}, "ns-x": {"A100": 1}}, unlimited)
			alone := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{model("mu", "team-u", 1, 4, 1)}, cons)
			Expect(alone["mu-v"].TargetReplicas).To(Equal(5))

			idle := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{
				model("mu", "team-u", 1, 4, 1), model("mx", "ns-x", 1, 0, 1),
			}, cons)
			Expect(idle["mu-v"].TargetReplicas).To(Equal(5), "beside an idle excluded namespace")

			busy := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{
				model("mu", "team-u", 1, 4, 1), model("mx", "ns-x", 1, 4, 1),
			}, cons)
			Expect(busy["mu-v"].TargetReplicas).To(Equal(5), "beside a scaling excluded namespace")
			Expect(busy["mx-v"].TargetReplicas).To(Equal(5))
		})

		It("still caps a listed tenant that holds nothing", func() {
			// Exclusion is read from the quota, not from what a namespace holds:
			// team-a holds no GPUs and is still capped by its own entry.
			cons := constraintsOf(map[string]map[string]int{"team-a": {}, "team-b": {"A100": 6}, "ns-x": {"A100": 1}}, tenants)
			dm := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{model("ma", "team-a", 0, 20, 1)}, cons)
			Expect(dm["ma-v"].TargetReplicas).To(Equal(8))
			Expect(FitsGPUBudget(cons, "team-a", map[string]int{"A100": 8})).To(BeTrue())
			Expect(FitsGPUBudget(cons, "team-a", map[string]int{"A100": 9})).To(BeFalse())
		})
	})

	Describe("rescale over a finite cluster quota", func() {
		// team-a holds 1 of 8, team-b 6 of 6: the tenants' pool has 7 free.
		It("fills the excluded namespace's models up to the cluster quota's free GPUs", func() {
			// 9 held, quota 19: 10 free, more than the tenants' pool has.
			cons := constraintsOf(map[string]map[string]int{
				"team-a": {"A100": 1}, "team-b": {"A100": 6}, "ns-x": {"A100": 2},
			}, tenants, clusterQuota(19))
			o := NewGreedyByScoreOptimizer()
			o.Rescale = RescaleFlags{Cluster: true}
			dm := targets(o, []ModelScalingRequest{
				model("mx", "ns-x", 1, 7, 1), model("my", "ns-x", 1, 7, 1), model("ma", "team-a", 1, 3, 1),
			}, cons)
			Expect(dm["mx-v"].TargetReplicas+dm["my-v"].TargetReplicas).To(Equal(12), "2 held + the 10 free")
			Expect(dm["ma-v"].TargetReplicas).To(Equal(1), "ns-x took the cluster quota's last GPU")
		})

		It("charges a capped tenant's rescale fill to the cluster quota", func() {
			// 9 held, quota 14: 5 free. team-a's contended group fills them;
			// ns-x, planned after it, finds none left.
			cons := constraintsOf(map[string]map[string]int{
				"team-a": {"A100": 1}, "team-b": {"A100": 6}, "ns-x": {"A100": 2},
			}, tenants, clusterQuota(14))
			o := NewGreedyByScoreOptimizer()
			o.Rescale = RescaleFlags{ByNamespace: map[string]bool{"team-a": true}}
			dm := targets(o, []ModelScalingRequest{
				model("ma", "team-a", 1, 9, 1), model("mx", "ns-x", 2, 4, 1),
			}, cons)
			Expect(dm["ma-v"].TargetReplicas).To(Equal(6), "the 5 free")
			Expect(dm["mx-v"].TargetReplicas).To(Equal(2))
		})

		It("bounds a capped tenant's rescale fill by what the excluded namespace left", func() {
			// 8-GPU replicas in ns-x, 1-GPU in team-a. 9 held, quota 21: 12
			// free. ns-x's group is planned first and fills one 8-GPU replica,
			// which leaves 4; team-a's group may fill only those 4, though its
			// own quota has 7 free.
			cons := constraintsOf(map[string]map[string]int{
				"team-a": {"A100": 1}, "team-b": {}, "ns-x": {"A100": 8},
			}, config.QuotaLimiterConfig{
				Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
				Exclude:         []string{"ns-x"},
				NamespaceQuotas: map[string]map[string]int{"team-a": {"A100": 8}},
			}, clusterQuota(21))
			o := NewGreedyByScoreOptimizer()
			o.Rescale = RescaleFlags{Cluster: true, ByNamespace: map[string]bool{"team-a": true}}
			dm := targets(o, []ModelScalingRequest{
				model("mx", "ns-x", 1, 3, 8), model("ma", "team-a", 1, 9, 1),
			}, cons)
			Expect(dm["mx-v"].TargetReplicas).To(Equal(2))
			Expect(dm["ma-v"].TargetReplicas).To(Equal(5))
		})
	})

	It("plans an excluded namespace's rescale group in a fixed order beside another cluster group", func() {
		// team-z is neither capped nor excluded (it is missing from the usage
		// map), so its group and ns-x's are both cluster groups on A100, and
		// both want the cluster quota's 4 free GPUs. Whichever is planned
		// first takes them; the order must not change from run to run.
		cons := constraintsOf(map[string]map[string]int{
			"team-a": {"A100": 1}, "team-b": {"A100": 6}, "ns-x": {"A100": 1},
		}, tenants, clusterQuota(12))
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}
		run := func() map[string]int {
			out := map[string]int{}
			for k, d := range targets(o, []ModelScalingRequest{model("mx", "ns-x", 1, 5, 1), model("mz", "team-z", 0, 5, 1)}, cons) {
				out[k] = d.TargetReplicas
			}
			return out
		}
		first := run()
		Expect(first["mx-v"]+first["mz-v"]).To(Equal(5), "1 held + the 4 free")
		for range 30 {
			Expect(run()).To(Equal(first))
		}
	})

	Describe("configs without exclude", func() {
		// The same targets as before #169: nothing is uncapped, so nothing may
		// grow. A model the budget stops is named by the limiter that held it
		// (the fair share marks the models still asking when no budget is left).
		cluster := config.QuotaLimiterConfig{
			Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H100": 8},
		}

		It("does not scale an unlimited tenant when nothing finite has room, and names the cluster quota", func() {
			// team-c may use A100 without limit and no H100; the cluster quota
			// lists only H100, so it allows no A100. The fair share stops before
			// it starts, and the cluster quota is what held the model.
			unlimited := config.QuotaLimiterConfig{
				Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"team-c": {"A100": -1, "H100": 0}},
			}
			cons := constraintsOf(map[string]map[string]int{"team-c": {"A100": 1}}, unlimited, cluster)
			dm := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{model("mc", "team-c", 1, 3, 1)}, cons)
			Expect(dm["mc-v"].TargetReplicas).To(Equal(1))
			Expect(dm["mc-v"].WasLimited).To(BeTrue())
			Expect(dm["mc-v"].LimitedBy).To(Equal("cluster"))
		})

		It("names a full tenant's own quota beside a cluster quota with room", func() {
			full := config.QuotaLimiterConfig{
				Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"team-a": {"H100": 2}},
			}
			cons := constraintsOf(map[string]map[string]int{"team-a": {"H100": 2}}, full, cluster)
			dm := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{modelOn("H100", "ma", "team-a", 2, 3, 1)}, cons)
			Expect(dm["ma-v"].TargetReplicas).To(Equal(2))
			Expect(dm["ma-v"].WasLimited).To(BeTrue())
			Expect(dm["ma-v"].LimitedBy).To(Equal("tenants"), "its namespace quota is spent; the cluster quota has room")
		})
	})

	Describe("the scale-from-zero capacity check", func() {
		// team-a and team-b have spent their quota; ns-x holds nothing.
		full := map[string]map[string]int{"team-a": {"A100": 8}, "team-b": {"A100": 6}, "ns-x": {}}

		It("wakes an excluded namespace when the tenants have spent their quota", func() {
			cons := constraintsOf(full, tenants)
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"A100": 1})).To(BeTrue())
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"A100": 8})).To(BeTrue(), "a whole node")
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{constants.DefaultAcceleratorName: 8})).To(BeTrue())

			budgets, nsScoped := GPUBudgets(cons, "ns-x")
			Expect(nsScoped).To(BeFalse())
			Expect(budgets).To(BeNil(), "no cluster bound: unbounded, not 0")
		})

		It("bounds an excluded namespace's wake by a cluster quota alone", func() {
			// 22 - 14 = 8 free: one whole node.
			cons := constraintsOf(full, tenants, clusterQuota(22))
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"A100": 8})).To(BeTrue())
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"A100": 9})).To(BeFalse())
			budgets, nsScoped := GPUBudgets(cons, "ns-x")
			Expect(nsScoped).To(BeFalse())
			Expect(budgets).To(Equal(map[string]int{"A100": 8}))

			cons = constraintsOf(full, tenants, clusterQuota(14))
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"A100": 1})).To(BeFalse(), "the cluster quota is full")
		})

		It("still refuses a capped tenant past its quota, and a namespace with no entry", func() {
			cons := constraintsOf(map[string]map[string]int{
				"team-a": {"A100": 8}, "team-b": {"A100": 6}, "ns-x": {}, "team-c": {},
			}, tenants)
			Expect(FitsGPUBudget(cons, "team-a", map[string]int{"A100": 1})).To(BeFalse())
			Expect(FitsGPUBudget(cons, "team-c", map[string]int{"A100": 1})).To(BeFalse())
		})

		It("wakes an excluded namespace on any type only when no cluster quota is in force", func() {
			Expect(FitsGPUBudget(constraintsOf(full, tenants), "ns-x", map[string]int{"L40S": 4})).To(BeTrue())
			cons := constraintsOf(full, tenants, clusterQuota(14)) // A100 full
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"L40S": 4})).To(BeFalse(), "the cluster quota does not list L40S")
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"A100": 1})).To(BeFalse())
		})

		It("wakes an excluded namespace's model that names no accelerator only where a type has room", func() {
			// The cluster quota lists only H100, and it is full. The tenants'
			// A100 does not open A100 to ns-x: the cluster quota denies it.
			h100Full := config.QuotaLimiterConfig{
				Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 0},
			}
			anyGPU := map[string]int{constants.DefaultAcceleratorName: 8}

			cons := constraintsOf(map[string]map[string]int{"ns-x": {}}, tenants, h100Full)
			Expect(FitsGPUBudget(cons, "ns-x", anyGPU)).To(BeFalse(), "no type the cluster quota lists has room")
			budgets, _ := GPUBudgets(cons, "ns-x")
			Expect(budgets).To(Equal(map[string]int{"H100": 0}))

			// The same quota with room on an unlimited A100.
			a100Open := config.QuotaLimiterConfig{
				Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 0, "A100": -1},
			}
			unlimited := config.QuotaLimiterConfig{
				Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
				Exclude:         []string{"ns-x"},
				NamespaceQuotas: map[string]map[string]int{"team-u": {"A100": -1}},
			}
			cons = constraintsOf(map[string]map[string]int{"team-u": {}, "ns-x": {}}, unlimited, a100Open)
			Expect(FitsGPUBudget(cons, "ns-x", anyGPU)).To(BeTrue(), "A100 is unbounded for ns-x")

			// No provider names any type at all.
			noTypes := config.QuotaLimiterConfig{
				Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
				Exclude: []string{"ns-x"},
			}
			cons = constraintsOf(map[string]map[string]int{"ns-x": {}}, noTypes)
			Expect(FitsGPUBudget(cons, "ns-x", anyGPU)).To(BeTrue(), "nothing bounds ns-x")
		})

		It("charges a cluster-group promise to the cluster quota, not to the tenants' quota", func() {
			// The utilization-share optimizer has promised 5 GPUs to the
			// cluster group, which here is ns-x. team-a's own 8 stay free.
			teamA := config.QuotaLimiterConfig{
				Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
				Exclude:         []string{"ns-x"},
				NamespaceQuotas: map[string]map[string]int{"team-a": {"A100": 8}},
			}
			cons := constraintsOf(map[string]map[string]int{"team-a": {}, "ns-x": {}}, teamA, clusterQuota(100))
			withheld := WithholdPromised(cons, map[string]map[string]int{"": {"A100": 5}})
			Expect(FitsGPUBudget(withheld, "team-a", map[string]int{"A100": 8})).To(BeTrue())
			Expect(FitsGPUBudget(withheld, "ns-x", map[string]int{"A100": 95})).To(BeTrue())
			Expect(FitsGPUBudget(withheld, "ns-x", map[string]int{"A100": 96})).To(BeFalse())
		})
	})

	Describe("accelerator names", func() {
		// A cluster quota of 16 A100, two 8-GPU nodes, all free. ns-x wants 6.
		empty := map[string]map[string]int{"team-a": {}, "ns-x": {}}
		eachRescale := func(f func(o *GreedyByScoreOptimizer)) {
			GinkgoHelper()
			for _, rescale := range []bool{false, true} {
				o := NewGreedyByScoreOptimizer()
				o.Rescale = RescaleFlags{Cluster: rescale}
				f(o)
			}
		}

		It("bounds an excluded namespace by a cluster quota whatever spelling its workload uses", func() {
			cons := constraintsOf(empty, tenants, clusterQuota(16))
			// The short name, a node's product label, a GKE accelerator value,
			// and the short name in lower case.
			for _, acc := range []string{"A100", "NVIDIA-A100-SXM4-80GB", "nvidia-a100-80gb", "a100"} {
				eachRescale(func(o *GreedyByScoreOptimizer) {
					dm := targets(o, []ModelScalingRequest{modelOn(acc, "mx", "ns-x", 0, 6, 8)}, cons)
					Expect(dm["mx-v"].TargetReplicas).To(Equal(2), "%s, rescale=%v: 16 GPUs", acc, o.Rescale.Cluster)
					if !o.Rescale.Cluster {
						Expect(dm["mx-v"].LimitedBy).To(Equal("cluster"), acc)
					}
				})
				Expect(FitsGPUBudget(cons, "ns-x", map[string]int{acc: 16})).To(BeTrue(), acc)
				Expect(FitsGPUBudget(cons, "ns-x", map[string]int{acc: 17})).To(BeFalse(), acc)
			}
		})

		It("shares a cluster quota between a tenant and an excluded namespace whatever spelling each uses", func() {
			// team-a may take 16 of its own; the cluster quota has 16 free.
			wide := config.QuotaLimiterConfig{
				Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
				Exclude:         []string{"ns-x"},
				NamespaceQuotas: map[string]map[string]int{"team-a": {"A100": 16}},
			}
			cons := constraintsOf(empty, wide, clusterQuota(16))
			eachRescale(func(o *GreedyByScoreOptimizer) {
				dm := targets(o, []ModelScalingRequest{
					modelOn("A100", "ma", "team-a", 0, 6, 8), modelOn("nvidia-a100-sxm4-80gb", "mx", "ns-x", 0, 6, 8),
				}, cons)
				Expect((dm["ma-v"].TargetReplicas+dm["mx-v"].TargetReplicas)*8).To(BeNumerically("<=", 16),
					"rescale=%v: ma %d, mx %d", o.Rescale.Cluster, dm["ma-v"].TargetReplicas, dm["mx-v"].TargetReplicas)
			})
		})

		It("bounds an excluded namespace by a cluster quota spelled differently from the tenants' quota", func() {
			lower := config.QuotaLimiterConfig{
				Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"a100": 16},
			}
			cons := constraintsOf(empty, tenants, lower)
			eachRescale(func(o *GreedyByScoreOptimizer) {
				dm := targets(o, []ModelScalingRequest{modelOn("A100", "mx", "ns-x", 0, 6, 8)}, cons)
				Expect(dm["mx-v"].TargetReplicas).To(Equal(2), "rescale=%v", o.Rescale.Cluster)
			})
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"A100": 16})).To(BeTrue())
			Expect(FitsGPUBudget(cons, "ns-x", map[string]int{"A100": 17})).To(BeFalse())
			budgets, _ := GPUBudgets(cons, "ns-x")
			Expect(budgets).To(Equal(map[string]int{"a100": 16}), "one key per accelerator")
		})

		It("gives an excluded namespace's model that names no accelerator nothing under a cluster quota", func() {
			// As in every other namespace: no budget is keyed by "unknown".
			cons := constraintsOf(empty, tenants, clusterQuota(16))
			for _, acc := range []string{constants.DefaultAcceleratorName, ""} {
				eachRescale(func(o *GreedyByScoreOptimizer) {
					dm := targets(o, []ModelScalingRequest{modelOn(acc, "mx", "ns-x", 0, 6, 8)}, cons)
					Expect(dm["mx-v"].TargetReplicas).To(Equal(0), "%q, rescale=%v", acc, o.Rescale.Cluster)
				})
			}

			// With no provider over every namespace, nothing bounds it.
			cons = constraintsOf(empty, tenants)
			dm := targets(NewGreedyByScoreOptimizer(), []ModelScalingRequest{
				modelOn(constants.DefaultAcceleratorName, "mx", "ns-x", 0, 6, 8),
			}, cons)
			Expect(dm["mx-v"].TargetReplicas).To(Equal(6))
		})

		It("reports an unlimited cluster quota as unbounded", func() {
			cons := constraintsOf(empty, tenants, config.QuotaLimiterConfig{
				Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"A100": -1, "H100": 8},
			})
			budgets, nsScoped := GPUBudgets(cons, "ns-x")
			Expect(nsScoped).To(BeFalse())
			Expect(budgets).To(HaveLen(2))
			Expect(budgets).To(HaveKeyWithValue("H100", 8))
			Expect(isUnboundedBudget(budgets["A100"])).To(BeTrue(), "%v", budgets)
		})
	})
})
