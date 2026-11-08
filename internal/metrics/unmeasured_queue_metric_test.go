package metrics

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

var _ = Describe("SetUnmeasuredQueue", func() {
	series := func(registry *prometheus.Registry) []*dto.Metric {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		for _, mf := range mfs {
			if mf.GetName() == constants.LLMSMUnmeasuredQueue {
				return mf.GetMetric()
			}
		}
		return nil
	}

	It("records the queue depth a model has no attributed replica to act on", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		SetUnmeasuredQueue("ns-a", "Qwen/Qwen3-0.6B", 155)

		got := series(registry)
		Expect(got).To(HaveLen(1))
		Expect(got[0].GetGauge().GetValue()).To(Equal(float64(155)))
		Expect(getLabelValue(got[0], constants.LabelNamespace)).To(Equal("ns-a"))
		Expect(getLabelValue(got[0], constants.LabelModelName)).To(Equal("Qwen/Qwen3-0.6B"))
	})

	// The gauge must be driven back to zero by the caller rather than left to
	// expire. A model that becomes measurable again would otherwise keep
	// reporting its last bad value for the length of Prometheus' staleness
	// window, which is long enough to fire an alert about a problem that is over.
	It("falls back to zero when the model becomes measurable", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		SetUnmeasuredQueue("ns-a", "m", 42)
		SetUnmeasuredQueue("ns-a", "m", 0)

		got := series(registry)
		Expect(got).To(HaveLen(1), "the same labels must update one series, not add a second")
		Expect(got[0].GetGauge().GetValue()).To(BeZero())
	})

	It("keeps one series per model", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		SetUnmeasuredQueue("ns-a", "m1", 1)
		SetUnmeasuredQueue("ns-a", "m2", 2)
		SetUnmeasuredQueue("ns-b", "m1", 3)

		Expect(series(registry)).To(HaveLen(3))
	})

	It("is a no-op before InitMetrics rather than a panic", func() {
		unmeasuredQueue = nil
		Expect(func() { SetUnmeasuredQueue("ns", "m", 1) }).NotTo(Panic())
	})
})
