package prefill

import (
	"math"
	"slices"
)

// The fixed cost a from the engine's own prefill timing (Cycle.AFromTiming),
// where the low-load fit has too few minutes (TooFewLowLoadMinutes).
//
// A prefill fleet whose load never goes idle has no low-load minute to read
// a from: on a P/D fleet of LWS groups under steady load, each row a group of
// several engines, no minute of a whole day may come near LowLoadOccupancy
// per engine. Its busy minutes still say what a request costs. vLLM times each
// request's prefill (request_prefill_time_seconds, the queue wait excluded:
// domain.ReplicaMetrics.AvgPrefillTime) and counts the tokens it computed
// (request_prefill_kv_computed_tokens), both over the same completed
// requests. So the minute's mean prefill time is fitted against its computed
// tokens per request, T = a0 + b*I, and the intercept a0 is the part a
// request pays whatever its tokens.
//
// A busy minute shares the pod's steps, and the sharing stretches the
// per-token part: binned by per-engine occupancy, such a fleet's slope
// roughly doubles from the quietest bin to the busiest, while the intercept
// moves by a tenth. That is the model the TTFT pricing takes (whatif.Prefill:
// a enters a request's own latency, never the shared work). Busier minutes
// also compute longer prompts, so a fit over every minute reads the stretch
// as a steeper slope and pulls the intercept down, by a quarter there. The
// fit reads the quieter half of the minutes instead: those at or under the
// median per-engine occupancy, whose intercept held within a few percent
// over trailing six-hour windows of a day.
//
// The computed tokens, not the prompt: at a prefix hit near 0.95 a request
// computes a twentieth of its prompt. The cost of the cached part (attention
// over it, a load from a CPU KV tier) is not modelled apart; it lands in the
// intercept at the shape the minutes were read at: a second term in the
// cached tokens can carry a third of it at long prompts, so a is the shape's,
// and moves with the prompt length and the hit rate.
//
// The overhead added to it is the median, over the same minutes, of the
// engine's TTFT less its prefill time and its queue time (the request's wait
// is priced as a wait elsewhere): what a request pays on the pod outside
// both, tokenization and the KV load among it. The low-load fit takes the
// least, where the queue time is not read apart; here it is read, and the
// least of hundreds of busy minutes is a tail, not the typical request.
//
// It is refused, and the low-load refusal stands, with fewer than
// MinTimingMinutes minutes (TooFewTimedMinutes), prompts that do not spread
// over MinPromptSpread between the 10th and 90th percentiles
// (NoPromptSpread), a slope or an intercept that is not above zero
// (NonPositiveSlope, NonPositiveIntercept), or an intercept whose standard
// error is above MaxAInterceptRelSE of it (AUncertain). No slope bound on
// V_P is read from it: its slope is a shared pod's, not a lone prompt's.
//
// Its shortest prompt is the 10th percentile of the minutes it reads
// (Estimate.AShortestPrompt). Past MaxAShortestPrompt the intercept is an
// extrapolation that reads low, as the low-load fit's does; that fit refuses
// it (NoShortPrompts), this one keeps it and marks it AExtrapolated, since a
// long-context fleet is the one with no other a to read.

// MinTimingMinutes is the fewest minutes the fit of a from the engine's
// prefill timing reads, counted after the quieter half is taken.
const MinTimingMinutes = 30

// timingPercentile is the percentile the prompt spread is read between, at
// both ends: the 10th and the 90th.
const timingPercentile = 0.10

// ASource is where a came from.
type ASource string

const (
	// ASourceLowLoad is the fit over low-load minutes (fixed.go).
	ASourceLowLoad ASource = "low-load"
	// ASourceEngineTiming is the fit of the engine's prefill time against its
	// computed tokens over the quieter half of the busy minutes (timing.go).
	ASourceEngineTiming ASource = "engine-prefill-timing"
)

// TooFewTimedMinutes is a from the engine's prefill timing with fewer than
// MinTimingMinutes minutes.
const TooFewTimedMinutes Refusal = "TooFewTimedMinutes"

// timedSample is a minute's engine prefill timing: the mean prefill time,
// the computed tokens per request, the per-engine occupancy, and the
// overhead outside the prefill and the queue where the TTFT and the queue
// time were read.
type timedSample struct {
	ieff, prefill, occ float64
	overhead           float64
	overheadOK         bool
	// ranks is the row's engine ranks: the service line's V_P is a row's
	// (serviceline.go).
	ranks int
}

// timed is the row's engine prefill timing, and whether it is one: a
// prefill time read above zero, computed tokens per request, and a request
// rate. ieff and rr are the caller's (effectivePrompt, requestRate).
func timed(rm rowTiming, ieff, rr float64) (timedSample, bool) {
	if !rm.prefillKnown || !finitePositive(rm.prefill) || !finitePositive(ieff) || !finitePositive(rr) {
		return timedSample{}, false
	}
	t := timedSample{ieff: ieff, prefill: rm.prefill, occ: rr * rm.prefill / float64(max(rm.ranks, 1)), ranks: max(rm.ranks, 1)}
	if rm.ttftKnown && rm.queueKnown && finitePositive(rm.ttft) && rm.queue >= 0 && isFinite(rm.queue) {
		if o := rm.ttft - rm.prefill - rm.queue; o >= 0 {
			t.overhead, t.overheadOK = o, true
		}
	}
	return t, true
}

// rowTiming is what timed reads of a row.
type rowTiming struct {
	prefill      float64
	prefillKnown bool
	ttft         float64
	ttftKnown    bool
	queue        float64
	queueKnown   bool
	ranks        int
}

// fixedFromTiming sets a on est from the key's timed minutes, where the
// low-load fit refused it for too few minutes. A refusal here keeps the
// low-load refusal and says its own in ATimingRefused.
func (est *Estimate) fixedFromTiming(ks *keyState) {
	var all []timedSample
	for _, name := range ks.podNames() {
		for _, s := range ks.pods[name].samples {
			if s.timedOK {
				all = append(all, s.timed)
			}
		}
	}
	if len(all) == 0 {
		est.ATimingRefused = TooFewTimedMinutes
		return
	}
	occ := make([]float64, len(all))
	for i, s := range all {
		occ[i] = s.occ
	}
	cut := median(occ)
	var xs, ys, over []float64
	for _, s := range all {
		if !(s.occ <= cut) {
			continue
		}
		xs, ys = append(xs, s.ieff), append(ys, s.prefill)
		if s.overheadOK {
			over = append(over, s.overhead)
		}
	}
	est.ATimingMinutes, est.ATimingOccupancy = len(xs), cut
	if len(xs) < MinTimingMinutes {
		est.ATimingRefused = TooFewTimedMinutes
		return
	}
	sorted := slices.Sorted(slices.Values(xs))
	lo, hi := percentileSorted(sorted, timingPercentile), percentileSorted(sorted, 1-timingPercentile)
	if !(hi >= MinPromptSpread*lo) {
		est.ATimingRefused = NoPromptSpread
		return
	}
	intercept, slope := ols(xs, ys)
	if !(slope > 0) || !isFinite(slope) {
		est.ATimingRefused = NonPositiveSlope
		return
	}
	if !(intercept > 0) || !isFinite(intercept) {
		est.ATimingRefused = NonPositiveIntercept
		return
	}
	se := olsInterceptSE(xs, ys, intercept, slope)
	if !(se <= MaxAInterceptRelSE*intercept) {
		est.ATimingRefused = AUncertain
		return
	}
	est.ARefused = ""
	est.ASource = ASourceEngineTiming
	est.ASamples = len(xs)
	est.APromptMin, est.APromptMax = sorted[0], sorted[len(sorted)-1]
	est.AModel, est.ASlope, est.ALineReason = AModelLinear, slope, ""
	est.AInterceptSE = se
	// Extrapolated from long prompts, the intercept reads low (fixed.go,
	// MaxAShortestPrompt), the unsafe side. It is not refused here: a fleet
	// that only sends long prompts is the one this fit is for. It is flagged.
	est.AShortestPrompt = lo
	est.AExtrapolated = !(lo <= MaxAShortestPrompt)
	est.AIntercept, est.A = intercept, intercept
	if len(over) > 0 {
		est.AOverhead, est.AOverheadKnown = median(over), true
		est.A += est.AOverhead
	}
}

// percentileSorted is the p-th percentile of sorted, by the nearest rank
// below.
func percentileSorted(sorted []float64, p float64) float64 {
	i := int(math.Floor(p * float64(len(sorted)-1)))
	return sorted[i]
}

// olsInterceptSE is the standard error of the least-squares line's
// intercept through the points: sqrt(s^2 (1/n + mean(x)^2/Sxx)), s^2 the
// residuals' variance over n-2. NaN with fewer than three points or no
// spread in x.
func olsInterceptSE(xs, ys []float64, intercept, slope float64) float64 {
	n := float64(len(xs))
	if n < 3 {
		return math.NaN()
	}
	var mx float64
	for _, x := range xs {
		mx += x
	}
	mx /= n
	var sxx, rss float64
	for i, x := range xs {
		sxx += (x - mx) * (x - mx)
		r := ys[i] - intercept - slope*x
		rss += r * r
	}
	if !(sxx > 0) {
		return math.NaN()
	}
	return math.Sqrt(rss / (n - 2) * (1/n + mx*mx/sxx))
}
