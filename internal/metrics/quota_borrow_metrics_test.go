package metrics

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

var _ = Describe("Kueue borrowing series", func() {
	var registry *prometheus.Registry

	BeforeEach(func() {
		registry = prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
	})

	// rows is each series of name as "label=value,..." (sorted label order as
	// gathered) -> value.
	rows := func(name string, labels ...string) map[string]float64 {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		out := map[string]float64{}
		for _, mf := range mfs {
			if mf.GetName() != name {
				continue
			}
			for _, m := range mf.GetMetric() {
				parts := make([]string, 0, len(labels))
				for _, l := range labels {
					parts = append(parts, getLabelValue(m, l))
				}
				out[strings.Join(parts, ",")] = value(m)
			}
		}
		return out
	}

	It("publishes both tiers and the withheld reason, and clears what no longer holds", func() {
		PublishQuotaBudgets([]QuotaBudget{
			{Limiter: "kueue", Namespace: "a", Accelerator: "H100", Guaranteed: 8, Borrowable: 4},
			{Limiter: "kueue", Namespace: "b", Accelerator: "H100", Guaranteed: 0, Reason: "not-kueue-managed"},
			{Limiter: "kueue", Namespace: "c", Accelerator: "H100", Guaranteed: -1},
		})
		Expect(rows(constants.LLMSMQuotaLimitGPUs, constants.LabelNamespace, constants.LabelTier)).To(Equal(map[string]float64{
			"a,guaranteed": 8, "a,borrowable": 4, "b,guaranteed": 0, "b,borrowable": 0,
		}), "an unlimited guaranteed figure publishes nothing")
		Expect(rows(constants.LLMSMQuotaBorrowWithheld, constants.LabelLimiterName, constants.LabelNamespace,
			constants.LabelAccelerator, constants.LabelReason)).To(Equal(map[string]float64{
			"kueue,b,H100,not-kueue-managed": 1,
		}))

		// b's reason changes, a stops borrowing, and namespace c leaves.
		PublishQuotaBudgets([]QuotaBudget{
			{Limiter: "kueue", Namespace: "a", Accelerator: "H100", Guaranteed: 8, Reason: "cohort-pool-empty"},
			{Limiter: "kueue", Namespace: "b", Accelerator: "H100", Guaranteed: 2, Borrowable: 6},
		})
		Expect(rows(constants.LLMSMQuotaLimitGPUs, constants.LabelNamespace, constants.LabelTier)).To(Equal(map[string]float64{
			"a,guaranteed": 8, "a,borrowable": 0, "b,guaranteed": 2, "b,borrowable": 6,
		}))
		Expect(rows(constants.LLMSMQuotaBorrowWithheld, constants.LabelNamespace, constants.LabelReason)).To(Equal(map[string]float64{
			"a,cohort-pool-empty": 1,
		}), "the stale not-kueue-managed series is deleted, not left at 1")

		PublishQuotaBudgets(nil)
		Expect(rows(constants.LLMSMQuotaLimitGPUs, constants.LabelNamespace)).To(BeEmpty())
		Expect(rows(constants.LLMSMQuotaBorrowWithheld, constants.LabelNamespace)).To(BeEmpty())
	})

	It("publishes unplaced pods and waiting lenders only while they hold", func() {
		PublishKueueUnplacedPods([]UnplacedVariant{{Namespace: "a", Variant: "v1", Pods: 2}, {Namespace: "a", Variant: "v2"}})
		PublishKueueLenderWaiting([]WaitingLender{{Cohort: "c", ClusterQueue: "lender", Seconds: 700}})
		Expect(rows(constants.LLMSMKueueUnplacedPods, constants.LabelNamespace, constants.LabelVariantName)).To(Equal(map[string]float64{"a,v1": 2}))
		Expect(rows(constants.LLMSMKueueLenderWaitingSeconds, constants.LabelCohort, constants.LabelClusterQueue)).To(Equal(map[string]float64{"c,lender": 700}))

		PublishKueueUnplacedPods(nil)
		PublishKueueLenderWaiting(nil)
		Expect(rows(constants.LLMSMKueueUnplacedPods, constants.LabelNamespace)).To(BeEmpty())
		Expect(rows(constants.LLMSMKueueLenderWaitingSeconds, constants.LabelCohort)).To(BeEmpty())
	})

	It("publishes the borrow drift per ClusterQueue, zeros included, and drops a queue that left", func() {
		PublishKueueBorrowDrift(map[string]int{"agree": 0, "over": 3})
		Expect(rows(constants.LLMSMKueueBorrowDriftGPUs, constants.LabelClusterQueue)).To(Equal(map[string]float64{"agree": 0, "over": 3}))
		PublishKueueBorrowDrift(map[string]int{"over": 1})
		Expect(rows(constants.LLMSMKueueBorrowDriftGPUs, constants.LabelClusterQueue)).To(Equal(map[string]float64{"over": 1}))
	})

	It("publishes a never-read Kueue entry only while it holds", func() {
		PublishKueueNeverRead([]string{"kq", "kq2"})
		Expect(rows(constants.LLMSMKueueNeverRead, constants.LabelLimiterName)).To(Equal(map[string]float64{"kq": 1, "kq2": 1}))
		PublishKueueNeverRead([]string{"kq2"})
		Expect(rows(constants.LLMSMKueueNeverRead, constants.LabelLimiterName)).To(Equal(map[string]float64{"kq2": 1}),
			"an entry read at last loses its series")
		PublishKueueNeverRead(nil)
		Expect(rows(constants.LLMSMKueueNeverRead, constants.LabelLimiterName)).To(BeEmpty())
	})

	It("counts preempted pods per model", func() {
		CountKueuePreemptedPods("a", "m", 2)
		CountKueuePreemptedPods("a", "m", 1)
		CountKueuePreemptedPods("a", "m", 0)
		Expect(rows(constants.LLMSMKueuePreemptedPodsTotal, constants.LabelNamespace, constants.LabelModelName)).To(Equal(map[string]float64{"a,m": 3}))
	})

	It("counts borrow releases per model and cause", func() {
		CountBorrowRelease("a", "m", "placement-timeout")
		CountBorrowRelease("a", "m", "placement-timeout")
		CountBorrowRelease("b", "m", "placement-timeout")
		Expect(rows(constants.LLMSMBorrowReleasesTotal, constants.LabelNamespace, constants.LabelModelName, constants.LabelCause)).To(
			Equal(map[string]float64{"a,m,placement-timeout": 2, "b,m,placement-timeout": 1}))
	})

	It("gives every reason exactly one producer", func() {
		owner := map[string]string{}
		for producer, reasons := range constants.ScalingBlockedReasonOwners {
			for _, r := range reasons {
				Expect(owner).NotTo(HaveKey(r), "%s is owned by %s and %s: two owners flap", r, owner[r], producer)
				owner[r] = producer
			}
		}
		Expect(owner).To(HaveKeyWithValue("wake-no-capacity", "wake-capacity"))
		Expect(owner).To(HaveKeyWithValue("nominal-lent-out", "borrowing"))
	})

	It("registers the borrowing reasons as one producer's set", func() {
		Expect(constants.ScalingBlockedReasonsBorrowing).To(ConsistOf(
			"lender-reclaimed", "borrow-unplaced", "nominal-lent-out", "released-to-lender"))
		for _, other := range [][]string{constants.ScalingBlockedReasonsPolicy, constants.ScalingBlockedReasonsWake,
			constants.ScalingBlockedReasonsUtilizationShare} {
			for _, r := range constants.ScalingBlockedReasonsBorrowing {
				Expect(other).NotTo(ContainElement(r), "a reason owned by two producers flaps")
			}
		}
	})
})

// value is a gauge's or a counter's value.
func value(m *dto.Metric) float64 {
	if m.GetCounter() != nil {
		return m.GetCounter().GetValue()
	}
	return m.GetGauge().GetValue()
}
