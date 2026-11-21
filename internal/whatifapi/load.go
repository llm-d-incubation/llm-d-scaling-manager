package whatifapi

import "math"

// LoadP95Modulation is the p95 of the per-cycle arrival rate with the
// counting noise of a one-minute rate taken out: what a load judged at the
// p95 is with only the rate's own movement in it (ModulationP95).
const LoadP95Modulation = "p95-modulation"

// RateWindowSeconds is the window of the per-cycle arrival rate the
// collector reads, rate(...[1m]): the counting noise of a steady Poisson
// stream of rate r read over it is r/60 in variance. Prometheus extrapolates
// a rate from the samples inside the window, which span less than a minute,
// so the noise read is somewhat larger: taking a minute's takes the noise
// out short, never past it.
const RateWindowSeconds = 60.0

// ModulationP95 is the p95 of rates with the counting noise of a one-minute
// rate taken out. A steady Poisson stream of rate r read over a minute has
// variance r/60 by counting alone, and the queueing formulas the planner
// prices with already hold that variance: a p95 of such rates counts it
// twice (at 1 r/s, 21% over the rate). The rates' variance less the mean's
// counting variance is the rate's own movement, its modulation; the p95's
// excess over the mean is shrunk by the modulation's share of the standard
// deviation:
//
//	p95mod = mean + (p95 - mean) * sqrt(max(0, var - mean/60) / var)
//
// So a steady stream is judged at its mean, and a day of large swings at
// close to its p95. Rates of one value, or a p95 at or under the mean, give
// the p95 back. rates are the cycles' rates (over 0) the p95 was read from.
func ModulationP95(rates []float64, mean, p95 float64) float64 {
	n := len(rates)
	if n < 2 || !(p95 > mean) {
		return p95
	}
	var ss float64
	for _, x := range rates {
		ss += (x - mean) * (x - mean)
	}
	v := ss / float64(n-1)
	if !(v > 0) {
		return p95
	}
	mod := max(0, v-mean/RateWindowSeconds)
	return mean + (p95-mean)*math.Sqrt(mod/v)
}

// FlagShapeLoadMismatch is a measured shape that differs from the shape of
// the cycles the load judged at was read from (ReportLoadShape) by more than
// ShapeLoadTolerance in its prompt or output length: the load is priced at
// traffic other than the traffic that made it.
const FlagShapeLoadMismatch = "ShapeLoadMismatch"

// ShapeLoadTolerance is how far, as a share, the measured shape's prompt or
// output length may lie from the load's cycles' and not be a mismatch.
const ShapeLoadTolerance = 0.2

// ReportLoadShape is the shape of the cycles the load judged at was read
// from: for the p95 the cycles at or above it, for the peak its cycle, for
// the mean every cycle with traffic. Each cycle is weighted by its request
// rate. FromCycle and ToCycle are the first and the last of them, Cycles how
// many read a shape, and Statistic the load's.
type ReportLoadShape struct {
	Statistic     string  `json:"statistic"`
	InputTokens   float64 `json:"inputTokens"`
	OutputTokens  float64 `json:"outputTokens"`
	PrefixHitRate float64 `json:"prefixHitRate"`
	Cycles        int     `json:"cycles"`
	FromCycle     int     `json:"fromCycle"`
	ToCycle       int     `json:"toCycle"`
	// Mismatch says the shape priced differs from this one by more than
	// ShapeLoadTolerance in a length (FlagShapeLoadMismatch).
	Mismatch bool `json:"mismatch"`
}

// ShapeLoad is the shape a report asks as "load": the shape of the cycles
// the load judged at was read from (ReportShapeAsk.Load).
const ShapeLoad = "load"

// ShapeLoadMismatch reports whether a shape of prompt il and output ol
// differs from the load's by more than ShapeLoadTolerance in either length.
func ShapeLoadMismatch(il, ol float64, load ReportLoadShape) bool {
	off := func(x, ref float64) bool { return x > 0 && ref > 0 && math.Abs(x/ref-1) > ShapeLoadTolerance }
	return off(il, load.InputTokens) || off(ol, load.OutputTokens)
}
