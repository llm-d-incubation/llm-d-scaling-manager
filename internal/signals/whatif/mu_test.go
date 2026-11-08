package whatif

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

var _ = Describe("PricingK", func() {
	It("is the two thresholds multiplied, inside the band", func() {
		Expect(PricingK(0.80, 0.85, DefaultBand())).To(BeNumerically("~", 0.68, 1e-15))
	})

	It("clamps to the band it is given", func() {
		Expect(PricingK(0.80, 0.85, FillBand{Lo: 0.3, Hi: 0.5})).To(Equal(0.5))
		Expect(PricingK(0.20, 0.50, FillBand{Lo: 0.3, Hi: 0.5})).To(Equal(0.3))
	})

	// Clamped to an inverted band the answer is whichever edge the last clamp
	// applied: 0.3 one way round, 0.5 the other. Neither is a price, so a band
	// that breaks FillBand's precondition is replaced by the default. The two
	// products sit above and below the default band, so each case tells the
	// default apart from any other band the repair might have used.
	DescribeTable("prices on the default band when the band given breaks FillBand's precondition",
		func(band FillBand) {
			Expect(PricingK(0.99, 0.99, band)).To(Equal(DefaultBand().Hi), "a product above the band")
			Expect(PricingK(0.1, 0.5, band)).To(Equal(DefaultBand().Lo), "a product below the band")
		},
		Entry("inverted", FillBand{Lo: 0.5, Hi: 0.3}),
		Entry("a point", FillBand{Lo: 0.4, Hi: 0.4}),
		Entry("NaN bottom", FillBand{Lo: math.NaN(), Hi: 0.8}),
		Entry("NaN top", FillBand{Lo: 0.15, Hi: math.NaN()}),
		Entry("bottom at 0", FillBand{Lo: 0, Hi: 0.8}),
		Entry("bottom below 0", FillBand{Lo: -0.1, Hi: 0.8}),
		Entry("top at 1", FillBand{Lo: 0.15, Hi: 1}),
		Entry("top at 1, a narrow band", FillBand{Lo: 0.5, Hi: 1.0}),
		Entry("top above 1", FillBand{Lo: 0.15, Hi: 1.5}),
		Entry("infinite top", FillBand{Lo: 0.15, Hi: math.Inf(1)}),
	)
})

var _ = Describe("CapacityTokens", func() {
	flag := &capacity.EngineParams{TotalKvTokensOverride: 100_000}

	It("takes a live reading as the whole replica's", func() {
		Expect(CapacityTokens(flag, 4, 250_000)).To(Equal(250_000.0))
	})

	It("multiplies the flag, which sizes one engine, by the ranks", func() {
		Expect(CapacityTokens(flag, 4, 0)).To(Equal(400_000.0))
	})

	It("reads a rank count of zero or below as one rank", func() {
		Expect(CapacityTokens(flag, 0, 0)).To(Equal(100_000.0))
		Expect(CapacityTokens(flag, -3, 0)).To(Equal(100_000.0))
	})

	It("reads the collector's MaxInt64 overflow sentinel as no reading", func() {
		Expect(CapacityTokens(flag, 2, math.MaxInt64)).To(Equal(200_000.0))
		Expect(CapacityTokens(nil, 2, math.MaxInt64)).To(BeZero())
		Expect(CapacityTokens(flag, 2, math.MaxInt64-1)).To(Equal(float64(math.MaxInt64 - 1)))
	})

	It("is zero with neither a reading nor a positive flag", func() {
		Expect(CapacityTokens(nil, 1, 0)).To(BeZero())
		Expect(CapacityTokens(&capacity.EngineParams{TotalKvTokensOverride: -5}, 1, -1)).To(BeZero())
	})
})

var _ = Describe("DeriveMuAtCap", func() {
	// 1,000 in / 500 out with no hits: KVreq = 1,250 tokens.
	s := shape.New(1000, 500, 0)
	line := itl.Model{A: 0.01, B: 0.005}

	It("prices the capped sequences over ITL(k) and the output length", func() {
		// 0.68 * 1,000,000 / 1,250 = 544 sequences, under 2 ranks x 512.
		d := DeriveMuAtCap(line, nil, 512, 2, 1_000_000, s, 0.68, 500)
		Expect(d.OK).To(BeTrue())
		Expect(d.Seqs).To(BeNumerically("~", 544, 1e-9))
		Expect(d.TokenSec).To(BeNumerically("~", 544/line.ITLAt(0.68), 1e-9))
		Expect(d.Rate).To(BeNumerically("~", 544/line.ITLAt(0.68)/500, 1e-12))
		Expect(d.Why).To(BeEmpty())
		Expect(d.CapAtMeasured).To(BeFalse())
	})

	It("caps the sequences at the batch cap times the ranks", func() {
		Expect(DeriveMuAtCap(line, nil, 100, 2, 1_000_000, s, 0.68, 500).Seqs).To(Equal(200.0))
		Expect(DeriveMuAtCap(line, nil, 100, 0, 1_000_000, s, 0.68, 500).Seqs).To(Equal(100.0))
	})

	DescribeTable("says which term was missing",
		func(m itl.Model, kvTokens int64, sh shape.Shape, k, out float64, why string) {
			d := DeriveMuAtCap(m, nil, 0, 1, kvTokens, sh, k, out)
			Expect(d.OK).To(BeFalse())
			Expect(d.Why).To(Equal(why))
			Expect(d.Rate).To(BeZero())
		},
		Entry("no line", itl.Model{}, int64(1_000_000), s, 0.68, 500.0, "no-line"),
		Entry("no output length", line, int64(1_000_000), s, 0.68, 0.0, "no-shape"),
		Entry("no KV footprint", line, int64(1_000_000), shape.Shape{}, 0.68, 500.0, "no-shape"),
		Entry("a pricing fill of 0", line, int64(1_000_000), s, 0.0, 500.0, "k-price"),
		Entry("a pricing fill above 1", line, int64(1_000_000), s, 1.5, 500.0, "k-price"),
		Entry("no capacity", line, int64(0), s, 0.68, 500.0, "no-seqs"),
		Entry("a line negative at the pricing fill", itl.Model{A: -1, B: 0.1}, int64(1_000_000), s, 0.68, 500.0, "no-itl"),
		Entry("a rate that rounds to zero", line, int64(1_000_000), s, 0.68, math.Inf(1), "no-rate"),
	)
})
