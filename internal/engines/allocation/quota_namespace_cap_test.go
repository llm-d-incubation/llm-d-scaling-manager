package allocation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A namespace quota has to bound what the optimizer allocates, which is the one
// thing the tenant-gpu-quotas path promises. Measured on kind it did not: a cap
// of 2 GPUs let the fleet reach 4, with the quota limiter built, the
// constraint-consuming optimizer selected, and no error or fallback logged.
//
// This walks the same seam without a cluster, so which link drops the cap is
// visible: the limiter's constraints first, then the optimizer's use of them.
var _ = Describe("a namespace quota bounds the optimizer", func() {
	const (
		ns    = "llm-d-sim"
		accel = "NVIDIA-A100-PCIE-80GB"
	)
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	// The fleet as the cluster run had it: one replica holding one GPU, wanting
	// four, in a namespace capped at two.
	constraintsFor := func(usedByNS int) []*ResourceConstraints {
		inv := NewQuotaInventory(config.QuotaLimiterConfig{
			Name:  "namespace-quota",
			Type:  "quota",
			Scope: config.QuotaScopeNamespace,
			NamespaceQuotas: map[string]map[string]int{
				ns: {accel: 2},
			},
		})
		limiter := NewDefaultLimiter("namespace-quota", inv)
		rc, err := limiter.ComputeConstraints(ctx,
			map[string]int{accel: usedByNS},
			map[string]map[string]int{ns: {accel: usedByNS}},
		)
		Expect(err).NotTo(HaveOccurred())
		return []*ResourceConstraints{rc}
	}

	It("materializes the namespace cap in the constraints", func() {
		rc := constraintsFor(1)[0]
		Expect(rc.NamespacePools).To(HaveKey(ns),
			"the namespace is in the usage map, so its cap must be materialized")
		Expect(rc.NamespacePools[ns]).To(HaveKey(accel))
		Expect(rc.NamespacePools[ns][accel].Limit).To(Equal(2),
			"the cap is what the operator declared, not the cluster's inventory")
	})

	It("stops the fleet at the cap instead of the replica ceiling", func() {
		r := &satEntryFixture{
			ModelID:          "test-model",
			Namespace:        ns,
			AnalyzedAt:       time.Now(),
			RequiredCapacity: 40000, // far more than one replica serves
			VariantCapacities: []vcFixture{
				{VariantName: "v", AcceleratorName: accel, Cost: 10.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
			},
		}
		requests := []ModelScalingRequest{
			withSatEntry(r, ModelScalingRequest{
				ModelID:   "test-model",
				Namespace: ns,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			}),
		}

		decisions := NewGreedyByScoreOptimizer().Optimize(ctx, requests, constraintsFor(1))
		dm := decisionMap(decisions)

		Expect(dm).To(HaveKey("v"))
		Expect(dm["v"].TargetReplicas).To(BeNumerically("<=", 2),
			"a 2-GPU namespace quota with 1 GPU per replica cannot fund more than 2 replicas")
	})
})
