package metrics

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// O1, O2, B4: the gauges the new alerts read. version_known and the cooldown
// carry no label beyond the controller instance.
var _ = Describe("Kueue borrowing gauges, stage 2", func() {
	var registry *prometheus.Registry
	BeforeEach(func() {
		registry = prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
	})
	series := func(name string) map[string]float64 {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		out := map[string]float64{}
		for _, mf := range mfs {
			if mf.GetName() != name {
				continue
			}
			for _, m := range mf.GetMetric() {
				var key strings.Builder
				for _, l := range m.GetLabel() {
					key.WriteString(l.GetName() + "=" + l.GetValue() + ";")
				}
				out[key.String()] = value(m)
			}
		}
		return out
	}

	// R2: the series pages only while some entry counts borrowing, and 0
	// while one of them does not know the version.
	It("publishes whether the Kueue controller's version is known to every entry that counts borrowing", func() {
		PublishKueueVersionKnown("a", false, true)
		Expect(series(constants.LLMSMKueueControllerVersionKnown)).To(Equal(map[string]float64{"": 0}))
		PublishKueueVersionKnown("b", true, true)
		Expect(series(constants.LLMSMKueueControllerVersionKnown)).To(Equal(map[string]float64{"": 0}), "a still does not know it")
		PublishKueueVersionKnown("a", true, true)
		Expect(series(constants.LLMSMKueueControllerVersionKnown)).To(Equal(map[string]float64{"": 1}))
		PublishKueueVersionKnown("a", false, false)
		PublishKueueVersionKnown("b", false, false)
		Expect(series(constants.LLMSMKueueControllerVersionKnown)).To(BeEmpty(), "no entry counts borrowing: no series to page on")
	})

	It("publishes the cooldown in force while borrowing is counted, and removes it after", func() {
		PublishKueueBorrowCooldown(1200, true)
		Expect(series(constants.LLMSMKueueBorrowCooldownSeconds)).To(Equal(map[string]float64{"": 1200}))
		PublishKueueBorrowCooldown(1200, false)
		Expect(series(constants.LLMSMKueueBorrowCooldownSeconds)).To(BeEmpty())
	})

	It("publishes the GPUs held borrowed per cohort and flavor, exactly this cycle's", func() {
		PublishKueueBorrowedGPUs([]BorrowedGPUs{{Cohort: "c", Flavor: "h100", GPUs: 16}, {Cohort: "c", Flavor: "a100", GPUs: 0}})
		Expect(series(constants.LLMSMKueueBorrowedGPUs)).To(Equal(map[string]float64{"cohort=c;flavor=h100;": 16}))
		PublishKueueBorrowedGPUs(nil)
		Expect(series(constants.LLMSMKueueBorrowedGPUs)).To(BeEmpty())
	})
})
