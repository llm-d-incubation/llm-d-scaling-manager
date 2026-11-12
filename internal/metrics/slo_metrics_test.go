package metrics

import (
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

var _ = Describe("PublishSLO", func() {
	var registry *prometheus.Registry
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	family := func(name string) []*dto.Metric {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		for _, mf := range mfs {
			if mf.GetName() == name {
				return mf.GetMetric()
			}
		}
		return nil
	}
	label := func(m *dto.Metric, name string) string {
		for _, l := range m.GetLabel() {
			if l.GetName() == name {
				return l.GetValue()
			}
		}
		return ""
	}
	snapshot := func(model string, variants ...SLOVariantReading) SLOModelSnapshot {
		return SLOModelSnapshot{
			Namespace: "ns", ModelName: model,
			Objectives: []SLOObjectiveReading{
				{Objective: constants.SLOObjectiveTTFT, Statistic: "p90", TargetSeconds: 3, Observed: 1.2, Attainment: 0.96},
				{Objective: constants.SLOObjectiveTPOT, Statistic: "mean", TargetSeconds: 0.1,
					Observed: math.NaN(), Attainment: math.NaN()},
			},
			Variants: variants,
		}
	}

	BeforeEach(func() {
		registry = prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
	})

	Describe("the TTFT series", func() {
		reading := func() SLOTTFTReading {
			return SLOTTFTReading{Variant: "pre", Statistic: "p90", Predicted: 1.5, PerReplica: 2, VPSource: "busy-minutes",
				Reason: constants.SLOTTFTReasonPriced, Flags: []string{"UnvalidatedTTFT", "AssumedPoissonArrivals"}}
		}
		publish := func(t SLOTTFTReading) {
			snap := snapshot("m")
			snap.TTFT = []SLOTTFTReading{t}
			PublishSLO(snap, t0)
		}
		value := func(name string) []float64 {
			metrics := family(name)
			out := make([]float64, 0, len(metrics))
			for _, m := range metrics {
				out = append(out, m.GetGauge().GetValue())
			}
			return out
		}
		oneHot := func(name, labelName string) []string {
			var on []string
			for _, m := range family(name) {
				if m.GetGauge().GetValue() == 1 {
					on = append(on, label(m, labelName))
				}
			}
			return on
		}

		It("publishes a priced variant under the four labels, and zero-fills exactly the three lists", func() {
			publish(reading())
			Expect(value(constants.LLMSMSLOPredictedTTFTSeconds)).To(Equal([]float64{1.5}))
			Expect(value(constants.LLMSMSLOTTFTReplicaRequestsPerSecond)).To(Equal([]float64{2.0}))
			Expect(value(constants.LLMSMSLOTTFTUnmet)).To(Equal([]float64{0.0}))
			m := family(constants.LLMSMSLOPredictedTTFTSeconds)[0]
			Expect(label(m, constants.LabelVariantName)).To(Equal("pre"))
			Expect(label(m, constants.LabelStatistic)).To(Equal("p90"))

			Expect(family(constants.LLMSMSLOTTFTFlag)).To(HaveLen(len(constants.SLOTTFTFlags)))
			Expect(family(constants.LLMSMSLOTTFTReason)).To(HaveLen(len(constants.SLOTTFTReasons)))
			Expect(family(constants.LLMSMSLOTTFTVPSource)).To(HaveLen(len(constants.SLOTTFTVPSources)))
			flags := make([]string, 0, len(constants.SLOTTFTFlags))
			for _, m := range family(constants.LLMSMSLOTTFTFlag) {
				flags = append(flags, label(m, constants.LabelFlag))
			}
			Expect(flags).To(ConsistOf(constants.SLOTTFTFlags))
			Expect(oneHot(constants.LLMSMSLOTTFTFlag, constants.LabelFlag)).To(ConsistOf("UnvalidatedTTFT", "AssumedPoissonArrivals"))
			Expect(oneHot(constants.LLMSMSLOTTFTReason, constants.LabelReason)).To(ConsistOf(constants.SLOTTFTReasonPriced))
			Expect(oneHot(constants.LLMSMSLOTTFTVPSource, constants.LabelVPSource)).To(ConsistOf("busy-minutes"))
			Expect(family(constants.LLMSMSLOTTFTRefused)).To(HaveLen(len(constants.SLOTTFTRefusedReasons)))
			Expect(oneHot(constants.LLMSMSLOTTFTRefused, constants.LabelReason)).To(BeEmpty())
		})

		It("publishes the joint model beside the reading, its flags on the flag series", func() {
			publish(reading())
			Expect(family(constants.LLMSMSLOTTFTJointInput)).To(BeEmpty(), "no joint reading, no joint series")
			Expect(family(constants.LLMSMSLOTTFTJointReason)).To(BeEmpty())

			r := reading()
			r.Joint = &SLOTTFTJointReading{
				Inputs: map[string]float64{constants.SLOTTFTJointPrefillWork: 0.025, constants.SLOTTFTJointOverhead: 0.014,
					"not-an-input": 1},
				MeanSeconds: 0.15, Reason: constants.SLOTTFTJointReasonPriced, Flags: []string{"OverheadMeasured"},
			}
			publish(r)
			inputs := map[string]float64{}
			for _, m := range family(constants.LLMSMSLOTTFTJointInput) {
				inputs[label(m, constants.LabelJointInput)] = m.GetGauge().GetValue()
			}
			Expect(inputs).To(Equal(map[string]float64{constants.SLOTTFTJointPrefillWork: 0.025,
				constants.SLOTTFTJointOverhead: 0.014}), "only the listed inputs, only those learned")
			Expect(value(constants.LLMSMSLOTTFTJointMeanSeconds)).To(Equal([]float64{0.15}))
			Expect(family(constants.LLMSMSLOTTFTJointReason)).To(HaveLen(len(constants.SLOTTFTJointReasons)))
			Expect(oneHot(constants.LLMSMSLOTTFTJointReason, constants.LabelReason)).To(ConsistOf(constants.SLOTTFTJointReasonPriced))
			Expect(oneHot(constants.LLMSMSLOTTFTFlag, constants.LabelFlag)).To(
				ConsistOf("UnvalidatedTTFT", "AssumedPoissonArrivals", "OverheadMeasured"))

			r.Joint = &SLOTTFTJointReading{Inputs: map[string]float64{}, MeanSeconds: math.NaN(), Reason: "no-pull"}
			publish(r)
			Expect(family(constants.LLMSMSLOTTFTJointMeanSeconds)).To(BeEmpty(), "no mean, no series")
			Expect(family(constants.LLMSMSLOTTFTJointInput)).To(BeEmpty(), "inputs no longer learned are deleted")
			Expect(oneHot(constants.LLMSMSLOTTFTJointReason, constants.LabelReason)).To(ConsistOf("no-pull"))
		})

		It("names a refused rate, and keeps the reason", func() {
			r := reading()
			r.Predicted, r.Unmet, r.Refused = math.NaN(), true, constants.SLOTTFTReasonSaturated
			publish(r)
			Expect(oneHot(constants.LLMSMSLOTTFTRefused, constants.LabelReason)).To(ConsistOf(constants.SLOTTFTReasonSaturated))
			Expect(oneHot(constants.LLMSMSLOTTFTReason, constants.LabelReason)).To(ConsistOf(constants.SLOTTFTReasonPriced))
		})

		It("is swept and deleted with the model", func() {
			names := []string{constants.LLMSMSLOPredictedTTFTSeconds, constants.LLMSMSLOTTFTUnmet, constants.LLMSMSLOTTFTReplicaRequestsPerSecond,
				constants.LLMSMSLOTTFTVPSource, constants.LLMSMSLOTTFTReason, constants.LLMSMSLOTTFTRefused, constants.LLMSMSLOTTFTFlag}
			gone := func() {
				for _, name := range names {
					Expect(family(name)).To(BeEmpty(), name)
				}
			}
			publish(reading())
			DeleteSLOModel("ns", "m")
			gone()

			publish(reading())
			SweepSLO(t0.Add(sloStaleAfter - time.Second))
			for _, name := range names {
				Expect(family(name)).NotTo(BeEmpty(), name)
			}
			SweepSLO(t0.Add(sloStaleAfter + time.Second))
			gone()
		})

		It("reads a refusal as a reason: no number, no unmet", func() {
			publish(SLOTTFTReading{Variant: "pre", Statistic: "p90", Predicted: math.NaN(), PerReplica: math.NaN(),
				VPSource: constants.SLOTTFTVPNone, Reason: constants.SLOTTFTReasonNoPrefillCapacity})
			Expect(family(constants.LLMSMSLOPredictedTTFTSeconds)).To(BeEmpty())
			Expect(family(constants.LLMSMSLOTTFTReplicaRequestsPerSecond)).To(BeEmpty())
			Expect(family(constants.LLMSMSLOTTFTUnmet)).To(BeEmpty())
			Expect(oneHot(constants.LLMSMSLOTTFTReason, constants.LabelReason)).To(ConsistOf(constants.SLOTTFTReasonNoPrefillCapacity))
		})

		It("reads a rate past the knee as unmet with no prediction", func() {
			r := reading()
			r.Predicted, r.Unmet = math.NaN(), true
			publish(r)
			Expect(family(constants.LLMSMSLOPredictedTTFTSeconds)).To(BeEmpty())
			Expect(value(constants.LLMSMSLOTTFTUnmet)).To(Equal([]float64{1.0}))
		})

		It("deletes the series when the reading goes", func() {
			publish(reading())
			PublishSLO(snapshot("m"), t0)
			Expect(family(constants.LLMSMSLOTTFTFlag)).To(BeEmpty())
			Expect(family(constants.LLMSMSLOTTFTReason)).To(BeEmpty())
			Expect(family(constants.LLMSMSLOPredictedTTFTSeconds)).To(BeEmpty())
		})

		It("says it is not a bound, in its help", func() {
			publish(reading())
			mfs, err := registry.Gather()
			Expect(err).NotTo(HaveOccurred())
			for _, mf := range mfs {
				if mf.GetName() == constants.LLMSMSLOPredictedTTFTSeconds {
					Expect(mf.GetHelp()).To(ContainSubstring(constants.SLOTTFTPredictionCaveat))
					Expect(constants.SLOTTFTPredictionCaveat).To(And(ContainSubstring("reads high"), ContainSubstring("reads low")))
				}
			}
		})
	})

	It("publishes a percentile in the +Inf bucket as +Inf, not as absent", func() {
		snap := snapshot("m")
		snap.Objectives[0].Observed = math.Inf(1)
		PublishSLO(snap, t0)
		var found bool
		for _, m := range family(constants.LLMSMSLOObservedSeconds) {
			if label(m, constants.LabelObjective) == constants.SLOObjectiveTTFT {
				Expect(math.IsInf(m.GetGauge().GetValue(), 1)).To(BeTrue())
				found = true
			}
		}
		Expect(found).To(BeTrue())
	})

	It("labels attainment with its statistic, and publishes the window's request count", func() {
		snap := snapshot("m")
		snap.Objectives[0].Requests = 300
		snap.Objectives[1].Requests = math.NaN()
		PublishSLO(snap, t0)
		att := family(constants.LLMSMSLOAttainmentRatio)
		Expect(att).To(HaveLen(1))
		Expect(label(att[0], constants.LabelStatistic)).To(Equal("p90"))
		req := family(constants.LLMSMSLOWindowRequests)
		Expect(req).To(HaveLen(1))
		Expect(req[0].GetGauge().GetValue()).To(Equal(300.0))
	})

	It("publishes targets, the judged figures, and one source per variant set to 1", func() {
		PublishSLO(snapshot("m", SLOVariantReading{Variant: "v", Source: constants.SLOSourceOwn,
			Fill: 0.6, PredictedITL: 0.04}), t0)

		Expect(family(constants.LLMSMSLOTargetSeconds)).To(HaveLen(2))
		// The TPOT mean was not judged: no observed series, rather than a zero.
		Expect(family(constants.LLMSMSLOObservedSeconds)).To(HaveLen(1))
		Expect(family(constants.LLMSMSLOAttainmentRatio)).To(HaveLen(1))
		Expect(family(constants.LLMSMSLOFill)[0].GetGauge().GetValue()).To(Equal(0.6))
		sources := family(constants.LLMSMSLOSource)
		Expect(sources).To(HaveLen(len(constants.SLOSources)))
		on := 0
		for _, m := range sources {
			if m.GetGauge().GetValue() == 1 {
				on++
				Expect(label(m, constants.LabelSource)).To(Equal(constants.SLOSourceOwn))
			}
		}
		Expect(on).To(Equal(1))
	})

	It("deletes a variant's series when it is gone from the next snapshot", func() {
		PublishSLO(snapshot("m",
			SLOVariantReading{Variant: "a", Source: constants.SLOSourceOwn, Fill: 0.6, PredictedITL: 0.04},
			SLOVariantReading{Variant: "b", Source: constants.SLOSourceOwn, Fill: 0.5, PredictedITL: 0.04}), t0)
		Expect(family(constants.LLMSMSLOFill)).To(HaveLen(2))

		PublishSLO(snapshot("m",
			SLOVariantReading{Variant: "a", Source: constants.SLOSourceNoLine, Fill: math.NaN(), PredictedITL: math.NaN()}), t0)
		Expect(family(constants.LLMSMSLOFill)).To(BeEmpty())
		Expect(family(constants.LLMSMSLOSource)).To(HaveLen(len(constants.SLOSources)))
	})

	It("sweeps a model silent past the stale horizon, and leaves a live one alone", func() {
		PublishSLO(snapshot("m1"), t0)
		PublishSLO(snapshot("m2"), t0.Add(time.Minute))
		Expect(family(constants.LLMSMSLOTargetSeconds)).To(HaveLen(4))

		SweepSLO(t0.Add(sloStaleAfter + 30*time.Second))
		targets := family(constants.LLMSMSLOTargetSeconds)
		Expect(targets).To(HaveLen(2))
		for _, m := range targets {
			Expect(label(m, constants.LabelModelName)).To(Equal("m2"))
		}
	})

	It("deletes one model's every series at once, and leaves the others", func() {
		v := SLOVariantReading{Variant: "v", Source: constants.SLOSourceOwn, Fill: 0.6, PredictedITL: 0.04}
		PublishSLO(snapshot("gone", v), t0)
		PublishSLO(snapshot("kept", v), t0)
		DeleteSLOModel("ns", "gone")
		for _, name := range []string{constants.LLMSMSLOTargetSeconds, constants.LLMSMSLOObservedSeconds,
			constants.LLMSMSLOAttainmentRatio, constants.LLMSMSLOFill, constants.LLMSMSLOPredictedITLSeconds,
			constants.LLMSMSLOSource} {
			ms := family(name)
			Expect(ms).NotTo(BeEmpty(), name)
			for _, m := range ms {
				Expect(label(m, constants.LabelModelName)).To(Equal("kept"), name)
			}
		}
	})

	// The case a sweep inside PublishSLO missed: the only model publishing
	// stops, so nothing publishes again and only the engine's per-cycle sweep
	// can remove its series.
	It("sweeps the last model even when nothing publishes after it", func() {
		PublishSLO(snapshot("m", SLOVariantReading{Variant: "v", Source: constants.SLOSourceOwn,
			Fill: 0.6, PredictedITL: 0.04}), t0)
		SweepSLO(t0.Add(sloStaleAfter - time.Second))
		Expect(family(constants.LLMSMSLOTargetSeconds)).To(HaveLen(2), "not yet stale")

		SweepSLO(t0.Add(sloStaleAfter + time.Second))
		for _, name := range []string{constants.LLMSMSLOTargetSeconds, constants.LLMSMSLOObservedSeconds,
			constants.LLMSMSLOAttainmentRatio, constants.LLMSMSLOFill, constants.LLMSMSLOPredictedITLSeconds,
			constants.LLMSMSLOSource} {
			Expect(family(name)).To(BeEmpty(), name)
		}
	})
})
