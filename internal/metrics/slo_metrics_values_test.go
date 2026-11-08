package metrics

import (
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// Values, not only counts: a gauge set from the wrong field reads as a working
// metric with a wrong number.
var _ = Describe("PublishSLO values", func() {
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	snap := SLOModelSnapshot{
		Namespace: "ns", ModelName: "m",
		Objectives: []SLOObjectiveReading{
			{Objective: constants.SLOObjectiveTTFT, Statistic: "p90", TargetSeconds: 3, Observed: 1.2,
				Attainment: 0.96, Requests: 300, WindowSeconds: 300, Excluded: constants.SLOExcludedStarting},
		},
		Variants: []SLOVariantReading{{Variant: "v", Source: constants.SLOSourceOwn, Fill: 0.6, PredictedITL: 0.041}},
	}
	gather := func(registry *prometheus.Registry, name string) []*dto.Metric {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		for _, mf := range mfs {
			if mf.GetName() == name {
				return mf.GetMetric()
			}
		}
		return nil
	}
	only := func(registry *prometheus.Registry, name string) *dto.Metric {
		ms := gather(registry, name)
		Expect(ms).To(HaveLen(1), name)
		return ms[0]
	}
	labelOf := func(m *dto.Metric, name string) string {
		for _, l := range m.GetLabel() {
			if l.GetName() == name {
				return l.GetValue()
			}
		}
		return ""
	}

	It("sets each gauge from its own field", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		PublishSLO(snap, t0)
		Expect(only(registry, constants.LLMSMSLOTargetSeconds).GetGauge().GetValue()).To(Equal(3.0))
		Expect(only(registry, constants.LLMSMSLOObservedSeconds).GetGauge().GetValue()).To(Equal(1.2))
		Expect(only(registry, constants.LLMSMSLOAttainmentRatio).GetGauge().GetValue()).To(Equal(0.96))
		Expect(only(registry, constants.LLMSMSLOWindowRequests).GetGauge().GetValue()).To(Equal(300.0))
		Expect(only(registry, constants.LLMSMSLOPredictedITLSeconds).GetGauge().GetValue()).To(Equal(0.041))
		fill := only(registry, constants.LLMSMSLOFill)
		Expect(fill.GetGauge().GetValue()).To(Equal(0.6))
		Expect(labelOf(fill, constants.LabelObjective)).To(Equal(constants.SLOObjectiveTPOT))
		excluded := gather(registry, constants.LLMSMSLOExcluded)
		Expect(excluded).To(HaveLen(len(constants.SLOExcludedReasons)))
		for _, m := range excluded {
			want := 0.0
			if labelOf(m, constants.LabelReason) == constants.SLOExcludedStarting {
				want = 1
			}
			Expect(m.GetGauge().GetValue()).To(Equal(want), labelOf(m, constants.LabelReason))
			Expect(labelOf(m, constants.LabelObjective)).To(Equal(constants.SLOObjectiveTTFT))
		}
	})

	It("publishes no exclusion series for a model with no objective", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		PublishSLO(SLOModelSnapshot{Namespace: "ns", ModelName: "m"}, t0)
		Expect(gather(registry, constants.LLMSMSLOExcluded)).To(BeEmpty())
	})

	It("names the exclusion per objective", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		PublishSLO(SLOModelSnapshot{Namespace: "ns", ModelName: "m", Objectives: []SLOObjectiveReading{
			{Objective: constants.SLOObjectiveTTFT, Statistic: "p90", Excluded: constants.SLOExcludedStarting},
			{Objective: constants.SLOObjectiveTPOT, Statistic: "mean"},
		}}, t0)
		on := map[string]float64{}
		for _, m := range gather(registry, constants.LLMSMSLOExcluded) {
			on[labelOf(m, constants.LabelObjective)] += m.GetGauge().GetValue()
		}
		Expect(on).To(Equal(map[string]float64{constants.SLOObjectiveTTFT: 1, constants.SLOObjectiveTPOT: 0}))
	})

	It("carries controller_instance on every series when it is set", func() {
		Expect(os.Setenv(ControllerInstanceEnvVar, "scaling-manager-a")).To(Succeed())
		DeferCleanup(func() {
			Expect(os.Unsetenv(ControllerInstanceEnvVar)).To(Succeed())
			Expect(InitMetrics(prometheus.NewRegistry())).To(Succeed())
		})
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		PublishSLO(snap, t0)
		for _, name := range []string{constants.LLMSMSLOTargetSeconds, constants.LLMSMSLOObservedSeconds,
			constants.LLMSMSLOAttainmentRatio, constants.LLMSMSLOWindowRequests, constants.LLMSMSLOFill,
			constants.LLMSMSLOPredictedITLSeconds, constants.LLMSMSLOSource, constants.LLMSMSLOExcluded} {
			ms := gather(registry, name)
			Expect(ms).NotTo(BeEmpty(), name)
			for _, m := range ms {
				Expect(labelOf(m, constants.LabelControllerInstance)).To(Equal("scaling-manager-a"), name)
			}
		}
	})

	It("publishes the objective ratio for a TTFT percentile and not for a TPOT mean", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		PublishSLO(SLOModelSnapshot{
			Namespace: "ns", ModelName: "m",
			Objectives: []SLOObjectiveReading{
				{Objective: constants.SLOObjectiveTTFT, Statistic: "p90", TargetSeconds: 3, Observed: 1.2,
					Attainment: 0.96, ObjectiveRatio: 0.9, Requests: 300, WindowSeconds: 300},
				{Objective: constants.SLOObjectiveTPOT, Statistic: "mean", TargetSeconds: 0.1, Observed: 0.05,
					Requests: 300, WindowSeconds: 300},
			},
		}, t0)
		m := only(registry, constants.LLMSMSLOObjectiveRatio)
		Expect(m.GetGauge().GetValue()).To(Equal(0.9))
		Expect(labelOf(m, constants.LabelObjective)).To(Equal(constants.SLOObjectiveTTFT))
	})

	It("lists every source the analyzer can report, so none goes unpublished", func() {
		Expect(constants.SLOSources).To(ConsistOf(
			constants.SLOSourceOwn, constants.SLOSourcePinned, constants.SLOSourcePinnedDefault,
			constants.SLOSourceNoLine, constants.SLOSourceDeclined, constants.SLOSourceNoOutputLength, constants.SLOSourceInfeasible,
			constants.SLOSourceInputInvalid))
		Expect(constants.SLOExcludedReasons).To(ConsistOf(
			constants.SLOExcludedZeroReplicas, constants.SLOExcludedStarting, constants.SLOExcludedUnobserved))
	})
})
