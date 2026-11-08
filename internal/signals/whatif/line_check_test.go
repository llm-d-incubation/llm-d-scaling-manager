package whatif

import (
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

var _ = Describe("CheckLine", func() {
	const kvReq = 1500.0
	d := Decode{Line: itl.Model{A: 0.01, B: 0.005}, CapacityTokens: 100000, KVPerRequest: kvReq, ReplicaCap: 1000}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	// rate is the token rate d's line predicts at k, off by factor.
	rate := func(k, factor float64) float64 {
		return factor * ReplicaTokenRate(d.Line, k, d.CapacityTokens, kvReq, d.ReplicaCap)
	}
	row := func(cycle int, k, factor float64) LineRow {
		return LineRow{Cycle: cycle, At: now, K: k, KVPerRequest: kvReq, TokenRate: rate(k, factor)}
	}
	// rows is n rows from cycle 1 at fill 0.5, off by factor.
	rows := func(n int, factor float64) []LineRow {
		out := make([]LineRow, n)
		for i := range out {
			out[i] = row(i+1, 0.5, factor)
		}
		return out
	}
	check := func(rs ...[]LineRow) LineCheck {
		all := make([]LineRow, 0, 8)
		for _, r := range rs {
			all = append(all, r...)
		}
		return CheckLine(d, all, now)
	}

	It("passes a line that predicts the rows, and says how many it read", func() {
		c := check([]LineRow{row(7, 0.4, 1.02), row(8, 0.5, 0.98), row(9, 0.6, 1), row(10, 0.5, 1), row(11, 0.5, 1)})
		Expect(c.Ran()).To(BeTrue())
		Expect(c.Mismatch()).To(BeFalse())
		Expect(c.OtherShape).To(BeFalse())
		Expect(c.Compared).To(Equal(5))
		Expect(c.Past).To(Equal(0))
		Expect(c.FromCycle).To(Equal(7))
		Expect(c.ToCycle).To(Equal(11))
		Expect(c.FillLo).To(Equal(0.4))
		Expect(c.FillHi).To(Equal(0.6))
	})

	It("flags a line past 15% and signs it: a line reading high over-promises", func() {
		// The replica made half the tokens the line says: 100% off, line high.
		c := check(rows(5, 0.5))
		Expect(c.Mismatch()).To(BeTrue())
		Expect(c.MedianErrorPct).To(BeNumerically("~", 100, 1e-6))
		Expect(c.SignedErrorPct).To(BeNumerically("~", 100, 1e-6))
		Expect(c.Past).To(Equal(5))

		low := check(rows(5, 2))
		Expect(low.Mismatch()).To(BeTrue())
		Expect(low.SignedErrorPct).To(BeNumerically("~", -50, 1e-6))
	})

	It("reads the threshold as the analyzers do: 15% is not past it", func() {
		on := check(rows(5, 1/1.15))
		Expect(on.MedianErrorPct).To(BeNumerically("~", 15, 1e-9))
		Expect(check(rows(5, 1/1.16)).Mismatch()).To(BeTrue())
	})

	It("takes the median: one noisy row of five decides nothing, and is still counted", func() {
		c := check(rows(4, 1), []LineRow{row(5, 0.6, 0.3)})
		Expect(c.Mismatch()).To(BeFalse())
		Expect(c.Past).To(Equal(1))
		Expect(c.WorstErrorPct).To(BeNumerically(">", 200))
	})

	It("reads no verdict from fewer rows than the validation does", func() {
		c := check(rows(MinLineRows-1, 0.5))
		Expect(c.Compared).To(Equal(4))
		Expect(c.Ran()).To(BeFalse())
		Expect(c.Mismatch()).To(BeFalse(), "four rows at 100% are no mismatch, and no pass")
		Expect(check(rows(MinLineRows, 0.5)).Mismatch()).To(BeTrue())
	})

	It("leaves rows on a ramp out, and says how many", func() {
		ramp := row(6, 0.5, 0.2)
		ramp.Ramp = true
		c := check(rows(5, 1), []LineRow{ramp})
		Expect(c.Compared).To(Equal(5))
		Expect(c.OnRamp).To(Equal(1))
		Expect(c.Mismatch()).To(BeFalse())
	})

	It("leaves rows above the band out, and says how many", func() {
		above := row(6, 0.9, 0.2)
		above.AboveBand = true
		c := check(rows(5, 1), []LineRow{above})
		Expect(c.Compared).To(Equal(5))
		Expect(c.AboveBand).To(Equal(1))
		Expect(c.OnRamp).To(Equal(0))
	})

	// A row can be both. It is counted once, for what puts it furthest from
	// the check: its age, then the band, then the ramp.
	It("counts a row that is two of them once", func() {
		both := row(6, 0.9, 0.2)
		both.AboveBand, both.Ramp = true, true
		c := check(rows(5, 1), []LineRow{both})
		Expect(c.AboveBand).To(Equal(1))
		Expect(c.OnRamp).To(Equal(0))
		Expect(c.Compared).To(Equal(5))

		both.At = now.Add(-MaxLineRowAge - time.Second)
		c = check(rows(5, 1), []LineRow{both})
		Expect(c.TooOld).To(Equal(1))
		Expect(c.AboveBand).To(Equal(0))
		Expect(c.OnRamp).To(Equal(0))
	})

	It("leaves a row older than the tracker's window out, once, and an equal age in", func() {
		old := row(1, 0.5, 0.2)
		old.At, old.Ramp = now.Add(-MaxLineRowAge-time.Second), true
		edge := row(2, 0.5, 1)
		edge.At = now.Add(-MaxLineRowAge)
		c := check([]LineRow{old, edge})
		Expect(c.TooOld).To(Equal(1))
		Expect(c.OnRamp).To(Equal(0), "counted once")
		Expect(c.Compared).To(Equal(1))
	})

	// A row of another traffic says the line held for that traffic. An old
	// line check read ok at KV 6512 on an answer priced at KV 1250.
	It("says when the rows are of another shape than the one priced", func() {
		at := func(kv float64) []LineRow {
			out := rows(5, 1)
			for i := range out {
				out[i].KVPerRequest = kv
				out[i].TokenRate = ReplicaTokenRate(d.Line, out[i].K, d.CapacityTokens, kv, d.ReplicaCap)
			}
			return out
		}
		within := at(kvReq * (1 + shape.DefaultChangeTolerance - 0.01))
		Expect(check(within).OtherShape).To(BeFalse())
		far := check(at(kvReq * (1 + shape.DefaultChangeTolerance + 0.01)))
		Expect(far.OtherShape).To(BeTrue())
		Expect(far.Mismatch()).To(BeFalse())
		Expect(far.RowKVPerRequest).To(BeNumerically("~", kvReq*1.21, 1e-6))
		Expect(check(at(kvReq*0.7)).OtherShape).To(BeTrue(), "either way")
	})

	// The analyzers' floor: below 0.30 a percentage is quantisation.
	It("leaves out a row the GPS check would not compare", func() {
		c := check([]LineRow{
			row(1, 0.29, 0.1),
			{Cycle: 2, At: now, K: 0.5, KVPerRequest: kvReq},
			{Cycle: 3, At: now, K: 0.5, TokenRate: 100},
		})
		Expect(c.Compared).To(Equal(0))
		Expect(c.OnRamp).To(Equal(0))
	})

	It("is no mismatch and no pass with nothing to compare", func() {
		c := check()
		Expect(c.Mismatch()).To(BeFalse())
		Expect(c.Ran()).To(BeFalse())
	})

	// itl.GPSErrorPct prices a cache: a replica at its batch cap holds fewer
	// sequences than k implies. Uncapped, a true line at the cap reads as a
	// mismatch of 100%.
	It("caps the sequences as every price does", func() {
		capped := d
		capped.ReplicaCap = 20
		k := 0.6
		seqs := k * capped.CapacityTokens / kvReq
		Expect(seqs).To(BeNumerically(">=", 2*float64(capped.ReplicaCap)))
		r := LineRow{Cycle: 1, At: now, K: k, KVPerRequest: kvReq, TokenRate: float64(capped.ReplicaCap) / capped.Line.ITLAt(k)}
		Expect(CheckLine(capped, []LineRow{r}, now).MedianErrorPct).To(BeNumerically("~", 0, 1e-9))
	})
})

var _ = Describe("EngineTPOTRatio", func() {
	// window is 100 observations at a mean of 10 ms, the p90 in the bucket
	// from 10 ms to edge.
	window := func(edge float64) TPOTWindow {
		return TPOTWindow{Sum: 1, Count: 100, Buckets: []HistogramBucket{{UpperEdge: 0.010, Count: 50}, {UpperEdge: edge, Count: 95}, {UpperEdge: 2 * edge, Count: 100}}}
	}

	It("answers, flagged, where the buckets resolve the percentile", func() {
		r, flags, refused := EngineTPOTRatio([]TPOTWindow{window(0.012)}, 0.9)
		Expect(refused).ToNot(HaveOccurred())
		Expect(r.Value).To(BeNumerically("~", 1.2, 1e-9))
		Expect(flags).To(ConsistOf(EngineTPOTHistogram))
	})

	It("answers, coarse and flagged, up to the line", func() {
		r, flags, refused := EngineTPOTRatio([]TPOTWindow{window(0.020)}, 0.9)
		Expect(refused).ToNot(HaveOccurred())
		Expect(r.Value).To(BeNumerically("~", 2, 1e-9))
		Expect(flags).To(ConsistOf(CoarseTPOTBucket, EngineTPOTHistogram))
	})

	It("refuses past it, keeping both flags", func() {
		r, _, refused := EngineTPOTRatio([]TPOTWindow{window(0.0201)}, 0.9)
		Expect(refused).To(HaveOccurred())
		Expect(refused.Reason).To(Equal(TPOTBucketTooWide))
		Expect(refused.Flags).To(ConsistOf(CoarseTPOTBucket, EngineTPOTHistogram))
		Expect(r.Value).To(Equal(0.0), "a zero ratio prices nothing")
	})

	// R_q's own rank: one window in twenty past the line is not the histogram.
	It("reads the width at R_q's rank", func() {
		ws := make([]TPOTWindow, 20)
		for i := range ws {
			ws[i] = window(0.012)
		}
		ws[0] = window(0.5)
		_, _, refused := EngineTPOTRatio(ws, 0.9)
		Expect(refused).ToNot(HaveOccurred())
		// Rank 18 of 20, the eighteenth width sorted: two wide windows are the
		// most it stands, and a third reaches the rank.
		ws[1] = window(0.5)
		_, _, refused = EngineTPOTRatio(ws, 0.9)
		Expect(refused).ToNot(HaveOccurred())
		ws[2] = window(0.5)
		_, _, refused = EngineTPOTRatio(ws, 0.9)
		Expect(refused).To(HaveOccurred())
	})

	It("passes the histogram's own refusals on, flagged", func() {
		_, _, refused := EngineTPOTRatio([]TPOTWindow{{Sum: 1, Count: 10, Buckets: []HistogramBucket{{UpperEdge: 0.01, Count: 1}, {UpperEdge: math.Inf(1), Count: 10}}}}, 0.9)
		Expect(refused).To(HaveOccurred())
		Expect(refused.Reason).To(Equal(TPOTQuantileUnbounded))
		Expect(refused.Flags).To(ConsistOf(EngineTPOTHistogram))
	})
})

// engineRatioFrom is EngineTPOTRatio as a Result, as ratioFrom is.
func engineRatioFrom(windows []TPOTWindow, q float64) Result {
	r, flags, u := EngineTPOTRatio(windows, q)
	if u != nil {
		return *u
	}
	return Answer{Value: r.Value, Flags: flags}
}
