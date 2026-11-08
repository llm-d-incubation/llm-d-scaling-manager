package steadystate

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// The seam between the engine and the limiter, which is where a namespace quota
// stopped binding on a real cluster: the limiter and the optimizer are both
// correct in isolation (see quota_namespace_cap_test.go), so what the engine
// hands them is the remaining suspect.
var _ = Describe("selectV2Optimizer with a namespace quota", func() {
	const (
		ns    = "llm-d-sim"
		accel = "NVIDIA-A100-PCIE-80GB"
	)
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	// One variant, one replica, one GPU each -- the fleet as the cluster had it.
	request := func() allocation.ModelScalingRequest {
		return allocation.ModelScalingRequest{
			ModelID:   "test-model",
			Namespace: ns,
			CompositeSignal: allocation.NamedAnalyzerResult{
				Name: domain.SaturationAnalyzerName, Result: &domain.AnalyzerResult{},
			},
			Variants: []domain.VariantMetadata{
				{VariantName: "v", AcceleratorName: accel},
			},
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
		}
	}

	It("passes the namespace cap through to the optimizer", func() {
		quota := allocation.NewDefaultLimiter("namespace-quota",
			allocation.NewQuotaInventory(config.QuotaLimiterConfig{
				Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{ns: {accel: 2}},
			}))

		e := &Engine{
			GPULimiter: quota,
			optimizer:  allocation.NewGreedyByScoreOptimizer(),
		}

		optimizer, constraints := e.selectV2Optimizer(ctx, []allocation.ModelScalingRequest{request()})

		Expect(optimizer.Name()).To(Equal("greedy-by-score"),
			"a declared quota must select the constraint-consuming optimizer")
		Expect(constraints).ToNot(BeEmpty(), "the quota must produce constraints")

		var pools map[string]map[string]allocation.ResourcePool
		for _, c := range constraints {
			if c != nil && c.NamespacePools != nil {
				pools = c.NamespacePools
			}
		}
		Expect(pools).ToNot(BeNil(), "the namespace cap must survive into the constraints")
		Expect(pools).To(HaveKey(ns))
		Expect(pools[ns]).To(HaveKey(accel))
		Expect(pools[ns][accel].Limit).To(Equal(2))
	})
})
