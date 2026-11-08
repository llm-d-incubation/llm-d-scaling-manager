package whatif

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// line is ITL(k) = 40 ms * k + 20 ms: 26 ms at the bottom of the fitted range,
// 44 ms at k = 0.6, 52 ms at the top.
var line = itl.Model{A: 0.040, B: 0.020}

var _ = Describe("PriceTPOT", func() {
	It("inverts the line at the target", func() {
		p := PriceTPOT(DefaultBand(), line, 0.044)
		Expect(p.OK).To(BeTrue())
		Expect(p.Feasible).To(BeTrue())
		Expect(p.Fill).To(BeNumerically("~", 0.6, 1e-12))
	})

	It("clamps a loose target to the top of the fitted range", func() {
		p := PriceTPOT(DefaultBand(), line, 1.0)
		Expect(p.Feasible).To(BeTrue())
		Expect(p.Fill).To(Equal(DefaultBand().Hi))
	})

	It("prices just inside the bottom of the range", func() {
		p := PriceTPOT(DefaultBand(), line, 0.027)
		Expect(p.Feasible).To(BeTrue())
		Expect(p.Fill).To(BeNumerically("~", 0.175, 1e-12))
	})

	It("is infeasible when the line exceeds the target at the bottom of its range", func() {
		p := PriceTPOT(DefaultBand(), line, line.ITLAt(DefaultBand().Lo))
		Expect(p.OK).To(BeTrue())
		Expect(p.Feasible).To(BeFalse())
	})

	DescribeTable("refuses an unusable input instead of pricing it",
		func(m itl.Model, target float64) {
			Expect(PriceTPOT(DefaultBand(), m, target).OK).To(BeFalse())
		},
		Entry("a flat line", itl.Model{A: 0, B: 0.02}, 0.05),
		Entry("an inverted line", itl.Model{A: -0.01, B: 0.02}, 0.05),
		Entry("a NaN slope", itl.Model{A: math.NaN(), B: 0.02}, 0.05),
		Entry("an infinite intercept", itl.Model{A: 0.04, B: math.Inf(1)}, 0.05),
		Entry("a NaN target", line, math.NaN()),
		Entry("a zero target", line, 0.0),
	)

	// It used to clamp into the band unchecked: a NaN edge priced OK with a
	// NaN fill, and an inverted band OK at whichever edge clamped last.
	DescribeTable("refuses a band that breaks FillBand's precondition",
		func(b FillBand) {
			Expect(PriceTPOT(b, line, 0.044).OK).To(BeFalse())
		},
		Entry("a NaN bottom", FillBand{Lo: math.NaN(), Hi: 0.8}),
		Entry("a NaN top", FillBand{Lo: 0.15, Hi: math.NaN()}),
		Entry("an infinite top", FillBand{Lo: 0.15, Hi: math.Inf(1)}),
		Entry("an inverted band", FillBand{Lo: 0.8, Hi: 0.15}),
		Entry("a zero bottom", FillBand{Lo: 0, Hi: 0.8}),
	)

	// The negative control for the bound this replaced. A fill capped at the
	// highest fill observed plus a step ratchets down, because the fleet runs
	// below the fill it is priced at. The fill here depends on the line and the
	// target only, so a loose SLO prices at the top of the range on every cycle
	// and never below the saturation analyzer's default point.
	It("prices a loose SLO at least as high as the saturation analyzer's default point", func() {
		const saturationPricingK = 0.80 * 0.85
		loose := PriceTPOT(DefaultBand(), line, 1.0)
		Expect(loose.Fill).To(BeNumerically(">=", saturationPricingK))
		Expect(ReplicaTokenRate(line, loose.Fill, 1e6, 4000, 0)).
			To(BeNumerically(">=", ReplicaTokenRate(line, saturationPricingK, 1e6, 4000, 0)))
	})
})

var _ = Describe("ReplicaTokenRate", func() {
	It("is the resident sequences over the ITL at k", func() {
		// 0.6 * 100000 / 500 = 120 sequences, one token each per 44 ms.
		Expect(ReplicaTokenRate(line, 0.6, 100000, 500, 0)).To(BeNumerically("~", 120/0.044, 1e-9))
	})

	It("caps the sequences at the engine's max_num_seqs", func() {
		Expect(ReplicaTokenRate(line, 0.6, 100000, 500, 64)).To(BeNumerically("~", 64/0.044, 1e-9))
	})

	It("is zero when an input is unusable", func() {
		Expect(ReplicaTokenRate(line, 0.6, 0, 500, 0)).To(BeZero())
		Expect(ReplicaTokenRate(line, 0.6, 100000, 0, 0)).To(BeZero())
		Expect(ReplicaTokenRate(itl.Model{A: math.NaN()}, 0.6, 100000, 500, 0)).To(BeZero())
	})
})

var _ = Describe("RealisedFill", func() {
	It("finds the fill that carries scaleUp times the priced rate, below the priced fill", func() {
		k := RealisedFill(DefaultBand(), line, 0.6, 0.85, 100000, 500, 0)
		Expect(k).To(BeNumerically("<", 0.6))
		Expect(ReplicaTokenRate(line, k, 100000, 500, 0)).
			To(BeNumerically("~", 0.85*ReplicaTokenRate(line, 0.6, 100000, 500, 0), 1e-6))
	})

	It("is the priced fill when there is no headroom", func() {
		Expect(RealisedFill(DefaultBand(), line, 0.6, 1, 100000, 500, 0)).To(Equal(0.6))
	})

	It("is NaN when the priced rate is unusable", func() {
		Expect(math.IsNaN(RealisedFill(DefaultBand(), line, 0.6, 0.85, 0, 500, 0))).To(BeTrue())
	})
})

var _ = Describe("ReachableFill", func() {
	It("is k when the engine admits every sequence k implies", func() {
		Expect(ReachableFill(DefaultBand(), 0.6, 100000, 500, 256)).To(Equal(0.6))
		Expect(ReachableFill(DefaultBand(), 0.6, 100000, 500, 0)).To(Equal(0.6))
	})

	It("is the fill max_num_seqs stops at when it binds", func() {
		Expect(ReachableFill(DefaultBand(), 0.6, 100000, 500, 64)).To(BeNumerically("~", 0.32, 1e-12))
	})

	It("never goes below the fitted range", func() {
		Expect(ReachableFill(DefaultBand(), 0.6, 100000, 500, 8)).To(Equal(DefaultBand().Lo))
	})
})

var _ = Describe("RealisedFill below the fitted range", func() {
	It("reports the bottom of the range rather than extrapolating under it", func() {
		// A very low headroom drives the realised fill under 0.15.
		Expect(RealisedFill(DefaultBand(), line, 0.2, 0.3, 100000, 500, 0)).To(Equal(DefaultBand().Lo))
	})
})
