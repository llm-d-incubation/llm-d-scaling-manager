package slo

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

func buckets(pairs ...float64) []domain.LatencyBucket {
	var out []domain.LatencyBucket
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, domain.LatencyBucket{UpperBound: pairs[i], Rate: pairs[i+1]})
	}
	return out
}

var inf = math.Inf(1)

var _ = Describe("histogram", func() {
	// Out of order on purpose: the router's series arrive in no order.
	ttft := buckets(1, 90, 0.1, 10, inf, 100, 0.5, 50, 3, 100)

	It("interpolates a quantile inside its bucket, as histogram_quantile does", func() {
		h, ok := newHistogram(ttft)
		Expect(ok).To(BeTrue())
		Expect(h.quantile(0.90)).To(BeNumerically("~", 1.0, 1e-12))
		Expect(h.quantile(0.95)).To(BeNumerically("~", 2.0, 1e-12))
		Expect(h.quantile(0.05)).To(BeNumerically("~", 0.05, 1e-12))
	})

	It("reports a quantile in the +Inf bucket as +Inf, not the top finite bound", func() {
		h, ok := newHistogram(buckets(1, 90, 3, 100, inf, 120))
		Expect(ok).To(BeTrue())
		Expect(h.quantile(0.90)).To(Equal(inf))
	})

	It("repairs non-monotone bucket rates before reading them", func() {
		h, ok := newHistogram(buckets(0.5, 60, 1, 50, inf, 100))
		Expect(ok).To(BeTrue())
		Expect(h.buckets[1].Rate).To(Equal(60.0))
	})

	It("is exact for a target on a bucket bound", func() {
		h, _ := newHistogram(buckets(1, 90, 3, 100, inf, 120))
		f, exact := h.fractionWithin(3)
		Expect(exact).To(BeTrue())
		Expect(f).To(BeNumerically("~", 100.0/120, 1e-12))
	})

	It("interpolates a target between two bounds and says so", func() {
		h, _ := newHistogram(buckets(1, 90, 3, 100, inf, 120))
		f, exact := h.fractionWithin(2)
		Expect(exact).To(BeFalse())
		Expect(f).To(BeNumerically("~", 95.0/120, 1e-12))
	})

	It("counts nothing past the last finite bound as within the target", func() {
		h, _ := newHistogram(buckets(1, 90, 3, 100, inf, 120))
		f, _ := h.fractionWithin(10)
		Expect(f).To(BeNumerically("~", 100.0/120, 1e-12))
	})

	It("refuses a histogram with no +Inf bucket or no requests", func() {
		_, ok := newHistogram(buckets(1, 90, 3, 100))
		Expect(ok).To(BeFalse())
		_, ok = newHistogram(buckets(1, 0, inf, 0))
		Expect(ok).To(BeFalse())
		_, ok = newHistogram(nil)
		Expect(ok).To(BeFalse())
	})

	It("drops NaN and negative rates rather than letting them poison the repair", func() {
		h, ok := newHistogram(buckets(0.5, math.NaN(), 1, -3, inf, 10))
		Expect(ok).To(BeTrue())
		Expect(h.buckets).To(HaveLen(1))
	})
})

var _ = Describe("minSamples", func() {
	It("asks for ten requests beyond the percentile, and never fewer than 50", func() {
		// Exactly: a window of exactly the documented minimum is judged.
		Expect(minSamples(90)).To(Equal(100.0))
		Expect(minSamples(95)).To(Equal(200.0))
		Expect(minSamples(99)).To(Equal(1000.0))
		Expect(minSamples(80)).To(Equal(50.0))
		Expect(minSamples(50)).To(Equal(50.0))
		Expect(minSamples(0)).To(Equal(50.0))
	})
})
