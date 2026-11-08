package config

import (
	"time"

	yaml "gopkg.in/yaml.v3"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("SLOPolicy", func() {
	parse := func(doc string) (ScalingPolicy, error) {
		var p ScalingPolicy
		err := yaml.Unmarshal([]byte(doc), &p)
		return p, err
	}

	It("reads targets as Go durations", func() {
		p, err := parse("slo:\n  tpot: {target: 100ms}\n  ttft: {target: 3s, percentile: 95}\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(p.SLO.TPOT.Target.Duration).To(Equal(100 * time.Millisecond))
		Expect(p.SLO.TTFT.Target.Duration).To(Equal(3 * time.Second))
		Expect(p.SLO.TTFTPercentile()).To(Equal(95))
		Expect(p.SLO.Validate()).To(Succeed())
	})

	It("refuses a target with no unit rather than reading it as nanoseconds", func() {
		_, err := parse("slo:\n  tpot: {target: 100}\n")
		Expect(err).To(MatchError(ContainSubstring("use a unit")))
	})

	DescribeTable("refuses a percentile that is not a whole number of percent",
		func(value string) {
			_, err := parse("slo:\n  ttft: {target: 3s, percentile: " + value + "}\n")
			Expect(err).To(MatchError(ContainSubstring("whole number of percent")))
		},
		// 0.95 is the PromQL spelling, and truncated to 0 it was judged at p90.
		Entry("a fraction", "0.95"),
		Entry("a decimal percent", "99.9"),
		Entry("a quoted number", `"95"`),
	)

	It("round-trips slo.flowControl through YAML, and leaves it out unset", func() {
		p, err := parse("slo:\n  ttft: {target: 3s}\n  flowControl: disabled\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(p.SLO.FlowControl).To(Equal(SLOFlowControlDisabled))
		b, err := yaml.Marshal(SLOPolicy{FlowControl: p.SLO.FlowControl})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).To(ContainSubstring("flowControl: disabled"))
		var back SLOPolicy
		Expect(yaml.Unmarshal(b, &back)).To(Succeed())
		Expect(back.FlowControl).To(Equal(SLOFlowControlDisabled))

		b, err = yaml.Marshal(SLOPolicy{})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).NotTo(ContainSubstring("flowControl"))
	})

	It("defaults the TTFT percentile to 90", func() {
		p, err := parse("slo:\n  ttft: {target: 3s}\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(p.SLO.TTFTPercentile()).To(Equal(DefaultTTFTPercentile))
	})

	DescribeTable("Validate",
		func(s SLOPolicy, wantErr string) {
			err := s.Validate()
			if wantErr == "" {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(MatchError(ContainSubstring(wantErr)))
		},
		Entry("a mean TPOT", SLOPolicy{TPOT: &SLOObjective{Target: SLODuration{100 * time.Millisecond}}}, ""),
		Entry("flow control declared on", SLOPolicy{FlowControl: SLOFlowControlEnabled}, ""),
		Entry("flow control declared off", SLOPolicy{FlowControl: SLOFlowControlDisabled}, ""),
		Entry("a third flow-control value", SLOPolicy{FlowControl: "on"},
			`flowControl must be "enabled" or "disabled", got "on"`),
		Entry("flow control in another case", SLOPolicy{FlowControl: "Enabled"},
			`flowControl must be "enabled" or "disabled", got "Enabled"`),
		Entry("flow control with whitespace", SLOPolicy{FlowControl: " enabled"},
			`flowControl must be "enabled" or "disabled", got " enabled"`),
		Entry("a TPOT percentile: TPOT is a mean",
			SLOPolicy{TPOT: &SLOObjective{Target: SLODuration{100 * time.Millisecond}, Percentile: 90}},
			"tpot is a mean"),
		Entry("a zero TPOT target", SLOPolicy{TPOT: &SLOObjective{}}, "tpot.target"),
		Entry("a negative TTFT target", SLOPolicy{TTFT: &SLOObjective{Target: SLODuration{-time.Second}}}, "ttft.target"),
		Entry("a TTFT percentile below 50",
			SLOPolicy{TTFT: &SLOObjective{Target: SLODuration{time.Second}, Percentile: 40}}, "between 50 and 99"),
		Entry("a TTFT percentile of 100",
			SLOPolicy{TTFT: &SLOObjective{Target: SLODuration{time.Second}, Percentile: 100}}, "between 50 and 99"),
	)

	It("fails the whole policy entry's Validate when the SLO is malformed", func() {
		p := ScalingPolicy{SLO: &SLOPolicy{TPOT: &SLOObjective{}}}
		Expect(p.Validate()).To(MatchError(ContainSubstring("slo: tpot.target")))
	})

	It("replaces the block whole on Merge, so an override cannot keep the other objective", func() {
		base := ScalingPolicy{SLO: &SLOPolicy{
			TPOT: &SLOObjective{Target: SLODuration{100 * time.Millisecond}},
			TTFT: &SLOObjective{Target: SLODuration{3 * time.Second}},
		}}
		base.Merge(ScalingPolicy{SLO: &SLOPolicy{TTFT: &SLOObjective{Target: SLODuration{time.Second}}}})
		Expect(base.SLO.TPOT).To(BeNil())
		Expect(base.SLO.TTFT.Target.Duration).To(Equal(time.Second))
	})

	It("keeps the base block when the override sets none", func() {
		base := ScalingPolicy{SLO: &SLOPolicy{TPOT: &SLOObjective{Target: SLODuration{100 * time.Millisecond}}}}
		base.Merge(ScalingPolicy{ScaleUpThreshold: 0.9})
		Expect(base.SLO.TPOT.Target.Duration).To(Equal(100 * time.Millisecond))
	})

	Context("SLOSummary", func() {
		It("reads off when neither half is set", func() {
			Expect(ScalingPolicy{}.SLOSummary()).To(Equal("off"))
		})
		It("names each target and whether the analyzer reads them", func() {
			p := ScalingPolicy{
				Analyzers: []AnalyzerScoreConfig{{Name: sloAnalyzerName}},
				SLO: &SLOPolicy{
					TPOT: &SLOObjective{Target: SLODuration{100 * time.Millisecond}},
					TTFT: &SLOObjective{Target: SLODuration{3 * time.Second}},
				},
			}
			Expect(p.SLOSummary()).To(Equal("tpot mean<=100ms, ttft p90<=3s (observe)"))
			p.Analyzers = nil
			Expect(p.SLOSummary()).To(HaveSuffix("(analyzer not listed)"))
		})
	})

	Context("SLOMisconfiguration", func() {
		targets := &SLOPolicy{TPOT: &SLOObjective{Target: SLODuration{100 * time.Millisecond}}}
		slo := AnalyzerScoreConfig{Name: sloAnalyzerName}
		sat := AnalyzerScoreConfig{Name: "saturation"}

		It("says nothing when both halves are there", func() {
			p := ScalingPolicy{SLO: targets, Analyzers: []AnalyzerScoreConfig{sat, slo}}
			Expect(p.SLOMisconfiguration()).To(BeEmpty())
		})
		It("says nothing when neither is", func() {
			p := ScalingPolicy{Analyzers: []AnalyzerScoreConfig{sat}}
			Expect(p.SLOMisconfiguration()).To(BeEmpty())
		})
		It("names targets nothing reads", func() {
			p := ScalingPolicy{SLO: targets, Analyzers: []AnalyzerScoreConfig{sat}}
			Expect(p.SLOMisconfiguration()).To(ContainSubstring("nothing reads them"))
		})
		It("names an analyzer with nothing to aim at", func() {
			p := ScalingPolicy{Analyzers: []AnalyzerScoreConfig{sat, slo}}
			Expect(p.SLOMisconfiguration()).To(ContainSubstring("no slo targets"))
		})
	})
})
