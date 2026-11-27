package prefill

import (
	"cmp"
	"maps"
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// V_P without a queue: the occupancy plateau (Cycle.VPOccupancyPlateau;
// issue llm-d-incubation/llm-d-scaling-manager#368).
//
// Every source in capacity.go needs a pod that queued for capacity. A fleet
// that never queues gets V_P only as a lower bound, or none at all. Its
// minutes still say how close the engines came to their limit. By Little's
// law the mean number of requests in prefill on an engine over a minute is
//
//	x = completions a second * mean prefill time / engine ranks
//
// (rowOccupancy), every input already recorded: the +Inf bucket of the
// computed-token histogram (or the request rate) for the completions,
// AvgPrefillTime for the prefill time, Ranks for the engines of the row.
// Plotted against x, the row's served computed tokens a second rises while
// more requests in prefill add work to each step, and flattens at V_P, where
// they no longer can: the plateau.
//
// The per-request speed, tokens over prefill time, is not V_P: requests in
// prefill together share the engine's steps, so each one's prefill time
// stretches, and under load it reads a fraction of V_P. Nor is an
// extrapolation of the rising curve to where it would flatten: it reads well
// above V_P whenever that point lies past the occupancy seen. The plateau is
// taken only where the window shows it.
//
// The rule, fixed before any recording was read against it:
//
//   - Only minutes at x >= PlateauMinOccupancy count. An engine that holds
//     under one request in prefill on average was idle at least 1 - x of the
//     minute, so its rate cannot be its limit.
//   - The minutes of every replica of the key, over Retention, are binned
//     by x in bins a quarter of an octave wide (plateauBinsPerOctave),
//     anchored at x = 1, so the rule reads a 32B engine flat at x 15 and a
//     0.6B engine flat at x 1.5 alike. A bin with at least
//     MinPlateauBinMinutes minutes counts; its level is the median served
//     rate of its minutes, which cuts a stalled minute (a long prefill time
//     with little computed, from a cause other than compute) and a single
//     high one.
//   - The plateau's level P is the highest counted bin's level. The plateau
//     is the run of counted bins beside that bin, in order of x, whose levels
//     are all within PlateauTolerance under P; a counted bin under that ends
//     the run, a bin with too few minutes neither ends nor extends it.
//   - The curve has flattened when the run spans PlateauSpan in occupancy
//     (the median x of its highest bin over that of its lowest) and holds at
//     least MinPlateauMinutes minutes. Within the tolerance over the span the
//     served rate's elasticity to occupancy is under ln(1.10)/ln(1.5) = 0.24;
//     on a rising curve it is near 1. In a processor-sharing queue, where
//     x = rho/(1 - rho), a run that flat sits at rho >= 0.8, so P reads V_P
//     or under it, never over: the safe side.
//   - Otherwise the curve is still rising at the highest occupancy the window
//     reached, and P is a lower bound: a pod computes no faster than V_P, so
//     the median of minutes it served is at or under it.
//
// A flattened plateau is V_P, not a bound. A rising one is a lower bound and
// composes with the other lower bounds: the largest stands, flagged
// VPLowerBound. Busy minutes of a single-rank pod are V_P itself and win;
// a lower bound above the plateau wins too (Estimate.plateauFill).

// SourceOccupancyPlateau is V_P from the plateau of the served computed
// tokens against the prefill occupancy (plateau.go). A lower bound where the
// curve did not flatten in the window.
const SourceOccupancyPlateau Source = "occupancy-plateau"

// PlateauMinOccupancy is the least requests in prefill per engine, by
// Little's law, a minute must hold to count towards the plateau.
const PlateauMinOccupancy = 1.0

// plateauBinsPerOctave is how many occupancy bins span a doubling of x.
const plateauBinsPerOctave = 4

// MinPlateauBinMinutes is the fewest minutes an occupancy bin holds to
// count.
const MinPlateauBinMinutes = 5

// PlateauTolerance is how far under the plateau's level, as a fraction of
// it, a bin's level may sit and still be on the plateau.
const PlateauTolerance = 0.10

// PlateauSpan is the least ratio of occupancies the plateau's run must span
// for the curve to have flattened.
const PlateauSpan = 1.5

// MinPlateauMinutes is the fewest minutes the plateau's run holds for the
// curve to have flattened.
const MinPlateauMinutes = 15

// plateau is the occupancy plateau's reading of a key.
type plateau struct {
	// level is P, the highest counted bin's median rate; zero with no
	// counted bin.
	level float64
	// flat says the curve flattened by the rule; otherwise level is a
	// lower bound.
	flat bool
	// minutes is how many minutes the run holds, span the occupancy it
	// spans, and maxOcc the highest occupancy of a counted bin's minute.
	minutes int
	span    float64
	maxOcc  float64
	// prompt, whole and hit are the mean effective prompt, whole prompt and
	// hit rate of the top bin's minutes (meanPrompt, meanWhole).
	prompt, whole, hit float64
	// lo and hi are the range of the top bin's prompts (promptRange).
	lo, hi float64
}

// rowOccupancy is the row's requests in prefill per engine over the minute, by
// Little's law, and whether it is a reading: the completions a second (the
// +Inf bucket of the computed-token histogram where the row carries one, the
// request rate otherwise) times the mean prefill time, over the engine
// ranks.
func rowOccupancy(rm domain.ReplicaMetrics) (float64, bool) {
	if !rm.AvgPrefillTimeKnown || !finitePositive(rm.AvgPrefillTime) {
		return 0, false
	}
	n, ok := completions(rm)
	if !ok {
		return 0, false
	}
	x := n * rm.AvgPrefillTime / float64(rm.Ranks())
	return x, finitePositive(x)
}

// completions is the row's completed prefills a second: the +Inf bucket of
// ComputedTokensBuckets, over the same minute as AvgPrefillTime, where the
// row carries it; else the request rate.
func completions(rm domain.ReplicaMetrics) (float64, bool) {
	if n := len(rm.ComputedTokensBuckets); n > 0 {
		if last := rm.ComputedTokensBuckets[n-1]; math.IsInf(last.UpperBound, 1) && finitePositive(last.Rate) {
			return last.Rate, true
		}
	}
	return requestRate(rm)
}

// plateauBin is one occupancy bin's minutes.
type plateauBin struct {
	index   int
	minutes []sample
}

// level is the median served rate of the bin's minutes, and occ their
// median occupancy.
func (b plateauBin) level() (rate, occ float64) {
	rates := make([]float64, len(b.minutes))
	occs := make([]float64, len(b.minutes))
	for i, s := range b.minutes {
		rates[i], occs[i] = s.rate, s.occ
	}
	return median(rates), median(occs)
}

// occupancyPlateau reads the key's plateau by the rule above. ok is false
// with no counted bin.
func occupancyPlateau(ks *keyState) (plateau, bool) {
	byIndex := map[int][]sample{}
	for _, name := range ks.podNames() {
		for _, s := range ks.pods[name].samples {
			if !s.occOK || !s.rateOK || !(s.rate > 0) || s.occ < PlateauMinOccupancy {
				continue
			}
			i := int(math.Floor(plateauBinsPerOctave * math.Log2(s.occ)))
			byIndex[i] = append(byIndex[i], s)
		}
	}
	var bins []plateauBin
	for _, i := range slices.Sorted(maps.Keys(byIndex)) {
		if len(byIndex[i]) >= MinPlateauBinMinutes {
			bins = append(bins, plateauBin{index: i, minutes: byIndex[i]})
		}
	}
	if len(bins) == 0 {
		return plateau{}, false
	}
	levels, occs := make([]float64, len(bins)), make([]float64, len(bins))
	top := 0
	var out plateau
	for i, b := range bins {
		levels[i], occs[i] = b.level()
		if levels[i] > levels[top] {
			top = i
		}
		out.maxOcc = max(out.maxOcc, slices.MaxFunc(b.minutes, func(p, q sample) int { return cmp.Compare(p.occ, q.occ) }).occ)
	}
	out.level = levels[top]
	floor := (1 - PlateauTolerance) * out.level
	lo, hi := top, top
	for lo > 0 && levels[lo-1] >= floor {
		lo--
	}
	for hi < len(bins)-1 && levels[hi+1] >= floor {
		hi++
	}
	for i := lo; i <= hi; i++ {
		out.minutes += len(bins[i].minutes)
	}
	out.span = occs[hi] / occs[lo]
	out.flat = out.span >= PlateauSpan && out.minutes >= MinPlateauMinutes
	prompts := make([]float64, len(bins[top].minutes))
	for i, s := range bins[top].minutes {
		prompts[i] = s.prompt
	}
	out.prompt = meanPrompt(prompts)
	out.lo, out.hi = promptRange(prompts)
	out.whole, out.hit = meanWhole(bins[top].minutes)
	return out, true
}

// plateauFill reads the occupancy plateau, with on (Cycle.VPOccupancyPlateau),
// records it on est, and sets V_P from it where V_P is refused for want of a
// queue or is a lower bound under the plateau's level. A flattened plateau
// is V_P, and clears VPLowerBound; a rising one is a lower bound, and is
// floored at the served peak as every lower bound is (floorAtServed). V_P
// from busy minutes of a single-rank pod is left, as is a lower bound at or
// above the plateau's level: the queue's figure wins where there is one.
func (est *Estimate) plateauFill(ks *keyState, c Cycle) {
	if !c.VPOccupancyPlateau {
		return
	}
	pl, ok := occupancyPlateau(ks)
	if !ok {
		return
	}
	est.VPPlateau, est.VPPlateauFlat = pl.level, pl.flat
	est.VPPlateauMinutes, est.VPPlateauSpan, est.VPPlateauMaxOccupancy = pl.minutes, pl.span, pl.maxOcc
	switch {
	case est.VPRefused == NoPrefillCapacity:
	case est.VPRefused == "" && est.VPLowerBound && pl.level > est.VP:
	default:
		return
	}
	est.VP, est.VPSource, est.VPRefused, est.VPSamples = pl.level, SourceOccupancyPlateau, "", pl.minutes
	est.VPLowerBound, est.VPLowerBoundReason, est.VPFlooredAtServed = !pl.flat, "", false
	est.VPPromptTokens, est.VPWholePromptTokens, est.VPPromptHitRate = pl.prompt, pl.whole, pl.hit
	est.VPPromptLow, est.VPPromptHigh = pl.lo, pl.hi
	if !pl.flat {
		est.floorAtServed(ks, c.VPFloorServed)
	}
}
