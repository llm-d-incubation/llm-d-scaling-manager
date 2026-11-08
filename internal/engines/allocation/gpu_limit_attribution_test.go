package allocation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// SR-M5: every name LimitedBy carries for something other than a quota entry
// is reserved in the config, so no quota entry can be reported under it.
var _ = Describe("attribution names a quota entry cannot take", func() {
	It("reserves every non-entry LimitedBy value", func() {
		Expect(config.ReservedLimiterNames).To(ConsistOf(
			PhysicalLimiterName, NodeFitLimiterName, PairBorrowsWholeName, LenderCannotReclaimName))
	})
})

var _ = Describe("bindingProvider", func() {
	It("returns the provider with the tightest per-type availability", func() {
		constraints := []*ResourceConstraints{
			// 8 free vs 3 free — the quota is the tighter bound.
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"A100": {Limit: 10, Used: 2}}},
			{ProviderName: "cluster-quota", Pools: map[string]ResourcePool{"A100": {Limit: 6, Used: 3}}},
		}
		Expect(bindingProvider(constraints, "team-a", "A100")).To(Equal("cluster-quota"))
	})

	It("weighs a namespace cap against another provider's cluster pool", func() {
		constraints := []*ResourceConstraints{
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"A100": {Limit: 10}}},
			{ProviderName: "namespace-quota", NamespacePools: map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 4, Used: 1}}, // 3 free — tighter
			}},
		}
		Expect(bindingProvider(constraints, "team-a", "A100")).To(Equal("namespace-quota"))
	})

	It("credits a closed allowlist that does not list the type at all (hard deny)", func() {
		constraints := []*ResourceConstraints{
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"H100": {Limit: 10}}},
			{ProviderName: "namespace-quota", NamespacePools: map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 8}}, // H100 unlisted → denied outright
			}},
		}
		Expect(bindingProvider(constraints, "team-a", "H100")).To(Equal("namespace-quota"),
			"an unlisted type is availability 0, the tightest bound there is")
	})

	It("credits no namespace-scope provider for a namespace it does not scope", func() {
		constraints := []*ResourceConstraints{
			{ProviderName: "namespace-quota",
				Pools:          map[string]ResourcePool{"A100": {Limit: 8, Used: 3}},
				NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: 1}}},
			},
		}
		Expect(bindingProvider(constraints, "team-z", "A100")).To(BeEmpty(),
			"team-z is excluded: it bypasses the quota, whose aggregate is no cluster bound")
	})

	It("skips unlimited sentinels and returns empty when nothing finitely bounds the type", func() {
		constraints := []*ResourceConstraints{
			{ProviderName: "cluster-quota", Pools: map[string]ResourcePool{"A100": {Limit: -1}}},
		}
		Expect(bindingProvider(constraints, "team-a", "A100")).To(BeEmpty())
	})

	It("credits a cluster bound that does not list the type (a closed allowlist)", func() {
		constraints := []*ResourceConstraints{
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"A100": {Limit: 4}}},
		}
		Expect(bindingProvider(constraints, "team-a", "L40S")).To(Equal("gpu-limiter"))
	})

	It("names every provider tied at the tightest bound, in the declared order", func() {
		constraints := []*ResourceConstraints{
			// 4 free under each: raising either one alone frees nothing.
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"H100": {Limit: 12, Used: 8}}},
			{ProviderName: "team-quota", Pools: map[string]ResourcePool{"H100": {Limit: 4}}},
			{ProviderName: "loose-quota", Pools: map[string]ResourcePool{"H100": {Limit: 16}}},
		}
		Expect(bindingProviders(constraints, "team-a", "H100")).To(Equal([]string{"gpu-limiter", "team-quota"}))
		Expect(bindingProvider(constraints, "team-a", "H100")).To(Equal("gpu-limiter"),
			"one name for the metric label: the first declared")
	})

	It("tolerates a nil constraint entry", func() {
		Expect(bindingProvider([]*ResourceConstraints{nil}, "team-a", "A100")).To(BeEmpty())
	})
})

var _ = Describe("GPU limit attribution (greedy-by-score)", func() {
	var (
		optimizer *GreedyByScoreOptimizer
		ctx       context.Context
	)

	BeforeEach(func() {
		optimizer = NewGreedyByScoreOptimizer()
		ctx = context.Background()
	})

	// oneVariantRequest builds a single-variant model on A100 wanting `required`
	// tokens of additional capacity. One replica supplies 10000 tokens and costs
	// 2 GPUs, so `required` translates directly into a replica (and GPU) demand
	// each spec can weigh against the budget it configures.
	const perReplicaCapacity, gpusPerReplica = 10000.0, 2
	oneVariantRequest := func(required float64, maxReplicas *int) []ModelScalingRequest {
		f := &satEntryFixture{
			ModelID:          "model-1",
			Namespace:        "team-a",
			AnalyzedAt:       time.Now(),
			RequiredCapacity: required,
			VariantCapacities: []vcFixture{
				{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: perReplicaCapacity},
			},
		}
		return []ModelScalingRequest{
			withSatEntry(f, ModelScalingRequest{
				ModelID:   "model-1",
				Namespace: "team-a",
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: gpusPerReplica, MaxReplicas: maxReplicas},
				},
			}),
		}
	}

	It("marks a variant whose scale-up the GPU budget cut short", func() {
		// Wants 4 more replicas (40000/10000); the budget affords 1.
		constraints := []*ResourceConstraints{
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"A100": {Limit: 2}}},
		}
		d := decisionMap(optimizer.Optimize(ctx, oneVariantRequest(40000, nil), constraints))["v1"]

		Expect(d.TargetReplicas).To(Equal(2), "1 current + the single replica the budget affords")
		Expect(d.WasLimited).To(BeTrue())
		Expect(d.LimitedBy).To(Equal("gpu-limiter"))
	})

	It("names both limiters when two bound the decision equally", func() {
		constraints := []*ResourceConstraints{
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"A100": {Limit: 2}}},
			{ProviderName: "cluster-quota", Pools: map[string]ResourcePool{"A100": {Limit: 2}}},
		}
		d := decisionMap(optimizer.Optimize(ctx, oneVariantRequest(40000, nil), constraints))["v1"]

		Expect(d.WasLimited).To(BeTrue())
		Expect(d.LimitedBy).To(Equal("gpu-limiter"), "the metric label's one value: the first declared")
		Expect(d.LimitedByAlso).To(Equal([]string{"cluster-quota"}))
	})

	It("marks a variant the budget could not grow at all", func() {
		// Not even one replica's worth of GPUs is free.
		constraints := []*ResourceConstraints{
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"A100": {Limit: 4, Used: 3}}},
		}
		d := decisionMap(optimizer.Optimize(ctx, oneVariantRequest(40000, nil), constraints))["v1"]

		Expect(d.TargetReplicas).To(Equal(1), "no growth")
		Expect(d.Action).To(Equal(domain.ActionNoChange))
		Expect(d.WasLimited).To(BeTrue(), "wanted to grow and was denied every GPU")
		Expect(d.LimitedBy).To(Equal("gpu-limiter"))
	})

	It("does NOT mark a variant held back by its own MaxReplicas ceiling", func() {
		// GPUs are ample (10 free); maxReplicas=2 is what stops the scale-up.
		maxReplicas := 2
		constraints := []*ResourceConstraints{
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"A100": {Limit: 10}}},
		}
		d := decisionMap(optimizer.Optimize(ctx, oneVariantRequest(40000, &maxReplicas), constraints))["v1"]

		Expect(d.TargetReplicas).To(Equal(2), "capped at maxReplicas")
		Expect(d.WasLimited).To(BeFalse(), "a user ceiling is intent, not scarcity")
		Expect(d.LimitedBy).To(BeEmpty())
	})

	It("does NOT mark a variant whose demand the budget fully satisfied", func() {
		// Wants 1 more replica (10000/10000) and the budget affords it.
		constraints := []*ResourceConstraints{
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"A100": {Limit: 10}}},
		}
		d := decisionMap(optimizer.Optimize(ctx, oneVariantRequest(10000, nil), constraints))["v1"]

		Expect(d.TargetReplicas).To(Equal(2))
		Expect(d.WasLimited).To(BeFalse())
	})

	It("names the limiter for a model the last GPUs did not reach", func() {
		// Quota H100:16, 8 in use: one 8-GPU replica is left, and two models
		// each want one. The one served takes the last 8; the fair-share loop
		// then stops with the other still asking, before its pick runs.
		model := func(id string) ModelScalingRequest {
			f := &satEntryFixture{
				ModelID:          id,
				Namespace:        "team-a",
				AnalyzedAt:       time.Now(),
				RequiredCapacity: 10000,
				VariantCapacities: []vcFixture{
					{VariantName: id + "-v", AcceleratorName: "H100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			return withSatEntry(f, ModelScalingRequest{
				ModelID:   id,
				Namespace: "team-a",
				VariantStates: []domain.VariantReplicaState{
					{VariantName: id + "-v", CurrentReplicas: 1, GPUsPerReplica: 8},
				},
			})
		}
		constraints := []*ResourceConstraints{
			{ProviderName: "team-quota", Pools: map[string]ResourcePool{"H100": {Limit: 16, Used: 8}}},
		}
		dm := decisionMap(optimizer.Optimize(ctx, []ModelScalingRequest{model("A"), model("B")}, constraints))

		grown, held := dm["A-v"], dm["B-v"]
		if held.TargetReplicas > grown.TargetReplicas {
			grown, held = held, grown
		}
		Expect(grown.TargetReplicas).To(Equal(2), "one model takes the last 8 GPUs")
		Expect(grown.WasLimited).To(BeFalse(), "it got all it asked for")
		Expect(held.TargetReplicas).To(Equal(1), "nothing was left for the other")
		Expect(held.WasLimited).To(BeTrue(), "the model the budget did not reach is limited")
		Expect(held.LimitedBy).To(Equal("team-quota"))
	})

	It("does NOT mark a model at its own ceiling when the budget runs out", func() {
		// As above, but B already sits at maxReplicas: running out of GPUs did
		// not hold it back.
		model := func(id string, maxReplicas *int) ModelScalingRequest {
			f := &satEntryFixture{
				ModelID:          id,
				Namespace:        "team-a",
				AnalyzedAt:       time.Now(),
				RequiredCapacity: 10000,
				VariantCapacities: []vcFixture{
					{VariantName: id + "-v", AcceleratorName: "H100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			return withSatEntry(f, ModelScalingRequest{
				ModelID:   id,
				Namespace: "team-a",
				VariantStates: []domain.VariantReplicaState{
					{VariantName: id + "-v", CurrentReplicas: 1, GPUsPerReplica: 8, MaxReplicas: maxReplicas},
				},
			})
		}
		one := 1
		constraints := []*ResourceConstraints{
			{ProviderName: "team-quota", Pools: map[string]ResourcePool{"H100": {Limit: 8}}},
		}
		dm := decisionMap(optimizer.Optimize(ctx, []ModelScalingRequest{model("A", nil), model("B", &one)}, constraints))

		Expect(dm["A-v"].TargetReplicas).To(Equal(2))
		Expect(dm["B-v"].WasLimited).To(BeFalse(), "a user ceiling is intent, not scarcity")
	})

	It("attributes to the namespace quota when it is the binding constraint", func() {
		constraints := []*ResourceConstraints{
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"A100": {Limit: 100}}},
			{ProviderName: "namespace-quota",
				Pools:          map[string]ResourcePool{"A100": {Limit: 2}},
				NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: 2}}},
			},
		}
		d := decisionMap(optimizer.Optimize(ctx, oneVariantRequest(40000, nil), constraints))["v1"]

		Expect(d.WasLimited).To(BeTrue())
		Expect(d.LimitedBy).To(Equal("namespace-quota"))
	})
})

var _ = Describe("GPU limit attribution (rescale)", func() {
	It("marks a fill the cycle's free GPUs could not cover", func() {
		// A holds the whole pool; B (higher priority) wins the water-fill but a
		// reclaim only frees GPUs for the NEXT cycle, so B's fill gets nothing now.
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		reqs := []ModelScalingRequest{
			rescaleReq("A", "default", 1, 8000, 8),
			rescaleReq("B", "default", 3, 8000, 0),
		}
		cons := []*ResourceConstraints{
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"A100": {Limit: 8, Used: 8}}},
		}
		dm := decisionMap(o.Optimize(context.Background(), reqs, cons))

		Expect(dm["B-v"].TargetReplicas).To(Equal(0), "nothing is free to hand B this cycle")
		Expect(dm["B-v"].WasLimited).To(BeTrue())
		Expect(dm["B-v"].LimitedBy).To(Equal("gpu-limiter"))
		Expect(dm["A-v"].WasLimited).To(BeFalse(), "A is being reclaimed from, not held back")
	})
})

var _ = Describe("GPUsAllocated", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	It("reports the whole GPU footprint at the target, not the scale-up delta", func() {
		f := &satEntryFixture{
			ModelID:          "model-1",
			Namespace:        "team-a",
			AnalyzedAt:       time.Now(),
			RequiredCapacity: 20000,
			VariantCapacities: []vcFixture{
				{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
			},
		}
		requests := []ModelScalingRequest{
			withSatEntry(f, ModelScalingRequest{
				ModelID:   "model-1",
				Namespace: "team-a",
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 4},
				},
			}),
		}
		constraints := []*ResourceConstraints{
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"A100": {Limit: 100}}},
		}

		d := decisionMap(NewGreedyByScoreOptimizer().Optimize(ctx, requests, constraints))["v1"]
		Expect(d.TargetReplicas).To(Equal(3), "1 current + 2 to cover 20000")
		Expect(d.GPUsAllocated).To(Equal(12), "3 replicas × 4 GPUs, the full footprint")
		Expect(d.GPUsPerReplica).To(Equal(4))
	})

	It("defaults an unset GPUsPerReplica to 1", func() {
		f := &satEntryFixture{
			ModelID:          "model-1",
			Namespace:        "team-a",
			AnalyzedAt:       time.Now(),
			RequiredCapacity: 10000,
			VariantCapacities: []vcFixture{
				{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
			},
		}
		requests := []ModelScalingRequest{
			withSatEntry(f, ModelScalingRequest{
				ModelID:   "model-1",
				Namespace: "team-a",
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v1", CurrentReplicas: 2}, // GPUsPerReplica unset
				},
			}),
		}

		d := decisionMap(NewCostAwareOptimizer().Optimize(ctx, requests, nil))["v1"]
		Expect(d.GPUsPerReplica).To(Equal(1))
		Expect(d.GPUsAllocated).To(Equal(d.TargetReplicas))
	})
})
