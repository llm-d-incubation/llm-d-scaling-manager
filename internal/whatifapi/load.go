package whatifapi

import "math"

// LoadP95Modulation is the p95 of the per-cycle arrival rate with the
// counting noise of a one-minute rate taken out: what a load judged at the
// p95 is with only the rate's own movement in it (ModulationP95). It is the
// statistic judged at, and the one a day-plan bucket is sized for, when none
// is asked (#295).
const LoadP95Modulation = "p95-modulation"

// RateWindowSeconds is the window of the per-cycle arrival rate the
// collector reads, rate(...[1m]): the counting noise of a steady Poisson
// stream of rate r read over it is r/60 in variance. Prometheus extrapolates
// a rate from the samples inside the window, which span less than a minute,
// so the noise read is somewhat larger. Readings closer together than the
// window overlap (a 1-minute rate every 15 s cycle), so their sample
// variance holds less of the counting noise than r/60: about 0.86 of it over
// 20 readings (CountingShare). ModulationP95Every takes out only that share.
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
// the p95 back. rates are the cycles' rates (over 0) the p95 was read from,
// each over a window of its own (ModulationP95Every for overlapping ones).
func ModulationP95(rates []float64, mean, p95 float64) float64 {
	return ModulationP95Every(rates, mean, p95, RateWindowSeconds)
}

// ModulationP95Every is ModulationP95 of rates read every step seconds:
// where that is under RateWindowSeconds the readings overlap, and only the
// share of the counting noise their sample variance holds (CountingShare)
// is taken out, so the rate's own movement is not taken with it. A step
// that is not a figure above 0 takes all of it out, as ModulationP95 does.
func ModulationP95Every(rates []float64, mean, p95, step float64) float64 {
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
	mod := max(0, v-CountingShare(n, step)*mean/RateWindowSeconds)
	return mean + (p95-mean)*math.Sqrt(mod/v)
}

// CountingShare is the share of a one-minute rate's counting variance that
// the sample variance of n such rates, read every step seconds, holds on
// average. Two counts of a Poisson stream over windows lag seconds apart
// share the overlap, a correlation of max(0, 1 - lag/60), and the sample
// variance of correlated readings is short of their variance by the mean
// correlation over pairs:
//
//	share = 1 - 2/(n(n-1)) x sum over lags k of (n - k) x max(0, 1 - k step/60)
//
// 1 for readings a window or more apart, or a step that is not a figure
// above 0; about 0.86 for 20 readings 15 s apart.
func CountingShare(n int, step float64) float64 {
	if n < 2 || !(step > 0) || step >= RateWindowSeconds || math.IsInf(step, 0) {
		return 1
	}
	var sum float64
	for k := 1; k < n; k++ {
		rho := 1 - float64(k)*step/RateWindowSeconds
		if rho <= 0 {
			break
		}
		sum += float64(n-k) * rho
	}
	return 1 - 2*sum/(float64(n)*float64(n-1))
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

// shapeMeasured is the shape a report asks as "measured": the source's own
// lengths, the shape the experimental models price.
const shapeMeasured = "measured"
