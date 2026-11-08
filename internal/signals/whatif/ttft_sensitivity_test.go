package whatif

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// pricedCorners is price at each corner of fl's sweep.
func pricedCorners(fl TTFTFleet, price func(TTFTFleet) Result) []Result {
	fleets := SensitivityFleets(fl)
	out := make([]Result, len(fleets))
	for i, c := range fleets {
		out[i] = price(c)
	}
	return out
}

var _ = Describe("the V_P and f sweep", func() {
	fleet := func(pods int, f float64) TTFTFleet {
		return TTFTFleet{Prefill: Prefill{VP: 100000, FixedLatency: 0.01, Replicas: pods, Imbalance: f, PromptTokens: 2000, PromptCV: 1},
			FlowControl: FlowControl{State: FlowControlOff}}
	}

	It("sweeps V_P 15% either way on one pod, which has one f", func() {
		corners := SensitivityFleets(fleet(1, 0))
		Expect(corners).To(HaveLen(2))
		Expect(corners[0].Prefill.VP).To(BeNumerically("~", 85000, 1e-6))
		Expect(corners[1].Prefill.VP).To(BeNumerically("~", 115000, 1e-6))
		Expect(corners[0].Prefill.Imbalance).To(Equal(0.0))
	})

	It("sweeps both at more than one pod, f held to its bounds", func() {
		corners := SensitivityFleets(fleet(2, 1.9))
		Expect(corners).To(HaveLen(4))
		fs := make([]float64, len(corners))
		for i, c := range corners {
			fs[i] = c.Prefill.Imbalance
		}
		Expect(fs).To(HaveEach(BeNumerically(">=", 1)))
		Expect(fs).To(HaveEach(BeNumerically("<=", 2)), "no more than the pods")
		Expect(fs[0]).To(BeNumerically("~", 1.615, 1e-9))
		Expect(fs[1]).To(Equal(2.0))

		low := SensitivityFleets(fleet(4, 1.1))
		Expect(low[0].Prefill.Imbalance).To(Equal(1.0), "never below 1")
	})

	It("leaves the fleet it was given alone", func() {
		fl := fleet(2, 1.5)
		SensitivityFleets(fl)
		Expect(fl.Prefill.VP).To(Equal(100000.0))
		Expect(fl.Prefill.Imbalance).To(Equal(1.5))
	})

	It("reads the knee's span from the closed form", func() {
		fl := fleet(2, 1.5)
		corners := pricedCorners(fl, func(c TTFTFleet) Result { return PrefillKnee(c.Prefill) })
		knee := PrefillKnee(fl.Prefill).(Answer).Value
		s := SensitivitySpan(knee, corners)
		// (1 - margin) x N_p x V_P / (f x I): down by 0.85/1.15, up by 1.15/0.85.
		Expect(s.Lo).To(BeNumerically("~", knee*0.85/1.15, 1e-9))
		Expect(s.Hi).To(BeNumerically("~", knee*1.15/0.85, 1e-9))
		Expect(string(s.Refused)).To(BeEmpty())
	})

	It("spans the figure, and a TTFT rises as V_P falls", func() {
		fl := fleet(1, 0)
		v := TTFTAt(fl, 10, 0.9).(TTFT)
		corners := pricedCorners(fl, func(c TTFTFleet) Result { return TTFTAt(c, 10, 0.9) })
		s := SensitivitySpan(v.Seconds, corners)
		Expect(s.Lo).To(BeNumerically("<", v.Seconds))
		Expect(s.Hi).To(BeNumerically(">", v.Seconds))
		Expect(s.Hi).To(Equal(corners[0].(TTFT).Seconds))
	})

	It("names a corner that refuses, and never reads it as a number", func() {
		fl := fleet(1, 0)
		// At 90% of the knee, V_P 15% down is past it.
		rate := 0.9 * PrefillKnee(fl.Prefill).(Answer).Value
		v := TTFTAt(fl, rate, 0.9).(TTFT)
		corners := pricedCorners(fl, func(c TTFTFleet) Result { return TTFTAt(c, rate, 0.9) })
		s := SensitivitySpan(v.Seconds, corners)
		Expect(s.Refused).To(Equal(Saturated))
		Expect(s.Hi).To(Equal(v.Seconds), "the worse side has no figure")
		Expect(s.Lo).To(BeNumerically("<", v.Seconds))
	})
})
