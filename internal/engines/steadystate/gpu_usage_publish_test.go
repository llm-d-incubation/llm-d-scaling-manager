package steadystate

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// The saturation engine is a CONSUMER of decision.DefaultGPUUsage now, not its
// producer — internal/gpuusage discovers usage from the pods occupying GPU nodes
// and is the sole writer. These specs pin the consumer half.
//
// The engine used to publish what its own population held, and every way of
// getting that wrong was found the hard way: publishing from inside
// selectV2Optimizer left default (CostAware) deployments with no snapshot at all;
// publishing zeros on the empty-population path conflated "nothing is in use"
// with "nothing could be measured". Both are now unreachable by construction,
// because this engine no longer writes the store — which is the point of moving
// the producer out.
var _ = Describe("GPU usage snapshot consumption", func() {
	BeforeEach(func() {
		// Reset rather than reassigning the package global: PublishGPUUsage and
		// LatestGPUUsage read that variable unsynchronized, so swapping the
		// pointer races with any concurrent user.
		decision.DefaultGPUUsage.Reset()
	})
	AfterEach(func() {
		decision.DefaultGPUUsage.Reset()
	})

	It("never writes the snapshot itself", func() {
		// One producer. If the engine writes here too, the two writers are free to
		// disagree about the same cluster and whichever ran last wins.
		e := &Engine{optimizer: allocation.NewCostAwareOptimizer()}

		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt).To(Equal(e.optimizer), "a non-GreedyByScore optimizer is left untouched")
		Expect(constraints).To(BeNil())

		_, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeFalse(), "the saturation engine must not publish; internal/gpuusage does")
	})

	It("falls back to the unlimited optimizer when nothing has been observed", func() {
		// The dangerous alternative is treating an absent snapshot as zero usage:
		// GreedyByScore would then read the whole cluster as free and allocate it.
		// Absent means unknown, and unknown must not become a confident claim.
		e := &Engine{optimizer: allocation.NewGreedyByScoreOptimizer()}

		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(constraints).To(BeNil())
		Expect(opt.Name()).To(Equal(allocation.NewCostAwareOptimizer().Name()),
			"with no usage observed the GPU-aware optimizer must not run on a zero baseline")
	})

	It("keeps an absent snapshot distinguishable from a measured-empty one", func() {
		// Absence is the signal for "could not measure", and the consumer must be
		// able to tell it apart from a real reading. An empty measured snapshot is
		// a legitimate value and reports ok=true.
		_, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeFalse(), "nothing published yet")

		decision.PublishGPUUsage(map[string]int{}, map[string]map[string]int{})
		snap, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeTrue(), "a measured-empty snapshot is still a snapshot")
		Expect(snap.ByType).To(BeEmpty())
	})

	It("leaves the snapshot alone through a cycle that measured no model", func() {
		// Reaching optimizeV2 with no usable request does NOT mean the cluster is
		// idle — it means every active model failed collection. This used to be the
		// most dangerous path in the engine, because it decided whether the rest of
		// the scaling manager got a capacity picture at all. Now it cannot touch it: the observation
		// belongs to the refresher, whose own view of the cluster is unaffected by
		// whether Prometheus answered this cycle.
		decision.PublishGPUUsage(map[string]int{"A100": 6}, map[string]map[string]int{"chat": {"A100": 6}})

		e := &Engine{Config: config.NewTestConfig(), optimizer: allocation.NewCostAwareOptimizer()}
		decisions := e.optimizeV2(ctx, map[string][]llmdvariant.Variant{
			"chat/m": {{
				ObjectMeta: metav1.ObjectMeta{Name: "chat-a", Namespace: "chat"},
				Spec:       llmdvariant.Spec{ModelID: "m"},
			}},
		}, nil)
		Expect(decisions).To(BeEmpty())

		snap, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeTrue(), "a blind cycle must not clear the observation")
		Expect(snap.ByType).To(HaveKeyWithValue("A100", 6),
			"a blind cycle must not overwrite the observation with zeros")
	})
})

// Unattributed GPUs are the silent half of the capacity picture: usage keyed by
// the unresolved sentinel is charged to no accelerator pool, because
// GetResourcePools iterates the DISCOVERED types. Every pool then over-states
// how much is free by that amount, and the wake capacity check can allow a
// placement it should refuse — with nothing erroring anywhere.
var _ = Describe("Unattributed GPU reporting", func() {
	It("counts GPUs held under an unresolved accelerator", func() {
		total, keys := unattributedGPUs(map[string]int{"A100": 6, "unknown": 4})
		Expect(total).To(Equal(4), "the unresolved bucket is what no pool accounts for")
		Expect(keys).To(Equal([]string{"unknown"}))
	})

	It("reports nothing when every variant's accelerator resolved", func() {
		total, keys := unattributedGPUs(map[string]int{"A100": 6, "H100": 2})
		Expect(total).To(BeZero())
		Expect(keys).To(BeEmpty())
	})

	It("counts the empty name and the sentinel together", func() {
		// They arrive by different routes — discovery resolving nothing at all
		// versus resolving to the placeholder — and have identical consequences.
		total, keys := unattributedGPUs(map[string]int{"": 3, "unknown": 2, "A100": 8})
		Expect(total).To(Equal(5))
		Expect(keys).To(Equal([]string{"", "unknown"}), "keys are sorted for a stable log line")
	})
})
