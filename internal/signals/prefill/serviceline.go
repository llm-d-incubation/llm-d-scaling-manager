package prefill

import (
	"math"
	"slices"
	"time"
)

// V_P from the prefill service line (Cycle.VPServiceLine; issues
// llm-d-incubation/llm-d-scaling-manager#368, #416, #387).
//
// A prefill fleet that never queues for capacity has no source in
// capacity.go but the served peak, a lower bound: on a fleet run at a third
// of its compute the knee priced from it sits under the load it serves. The
// lone rate (lonerate.go) and the occupancy plateau (plateau.go) read V_P
// without a queue, but both read tokens per prefill-second. Where a request
// pays a fixed time s0 whatever its tokens (a KV load from a host tier,
// scheduling, the step's own floor), that reads E[x]/(s0 + E[x]/V) at the
// prompt the minutes ran, and calls nothing of it s0: priced as the
// per-token speed at another load, or with s0 priced again as the fixed
// latency, it is wrong in both.
//
// The engine's own timing tells the two apart. vLLM times each request's
// prefill, the queue excluded (request_prefill_time_seconds,
// domain.ReplicaMetrics.AvgPrefillTime), and counts the tokens it computed
// (request_prefill_kv_computed_tokens), over the same completed requests.
// A minute's mean prefill time T at computed tokens x per request, on an
// engine holding x_occ requests in prefill (Little's law, rowOccupancy), is
// inverted for processor sharing, S = T/(1 + x_occ), the request's own work
// with the sharing taken out, as the rank-mixture fit reads it
// (whatif.FitPSInversion). The minutes' S against x is the service line
//
//	S(x) = s0 + x/V
//
// per engine, fitted by least squares. Its capacity, with s0 in it, is
// 1/(s0 + E[x]/V) requests a second an engine, so
//
//	V_P = ranks * E[x] / (s0 + E[x]/V)
//
// computed tokens a second a row, at E[x], the minutes' mean computed
// prompt (Estimate.VPPromptTokens): the prompt it is priced at, so a
// question at another prompt is flagged as V_P from any source is
// (whatif.VPShapeMoved). s0 stays in the capacity: the line cannot say
// which part of it the engine spends computing.
//
// The rule:
//
//   - A minute counts with its prefill time, computed tokens a request and
//     request rate read (timed), on a P/D prefill pod, over Retention.
//   - With fewer than MinTimingMinutes of them: TooFewTimedMinutes.
//   - Their computed prompts must spread: the 90th percentile at least
//     MinPromptSpread times the 10th (NoPromptSpread), or s0 and V are not
//     told apart.
//   - The slope must be above zero (NonPositiveSlope) with a standard error
//     at most MaxVPLineSlopeRelSE of it (SlopeUncertain), and s0 at 0 or
//     above (NonPositiveIntercept).
//
// Where V_P is taken from the line, its split of a request's work goes with
// it (Estimate.VPRequestSeconds): s0 per request on an engine, so s0/ranks
// pod-seconds on the row, and the rest, E[x]/V, per token.
//
// Precedence: V_P from the line fills only where no queue gave V_P: a V_P
// refused for want of a queue (NoPrefillCapacity), or the served peak of a
// fleet that never queued (SourceServedPeak, NeverQueued) at or under the
// line's. It is not a bound. A V_P read under a queue stands, a lower bound
// or not: busy minutes (over data-parallel ranks, DataParallelRanks, too),
// the peak under a queue (the window's best included) and the back-pressure
// bound. The queue measured the pods at capacity; the line extrapolates
// their unqueued minutes to it, and on fleets that queued it read under the
// queue's figure and the load served (issue
// llm-d-incubation/llm-d-scaling-manager#449). A V_P that is not a lower
// bound (a flattened plateau, a lone rate that is V_P) stands too. A served
// peak above the line stands: the true V_P is at or above it, so the line
// under-reads there. A line not taken is recorded (Estimate.VPLine).

// SourceServiceLine is V_P from the prefill service line (serviceline.go).
const SourceServiceLine Source = "service-line"

// MaxVPLineSlopeRelSE is the largest standard error of the service line's
// slope, as a fraction of it, V_P is read from: one standard error moves V
// by at most a tenth, the spread of a one-minute rate the queue's sources
// are read to (LoneServedTolerance).
const MaxVPLineSlopeRelSE = 0.10

// SlopeUncertain is a service line whose slope's standard error is above
// MaxVPLineSlopeRelSE of it.
const SlopeUncertain Refusal = "SlopeUncertain"

// serviceLine is the line's reading of a key.
type serviceLine struct {
	// s0 is the intercept in seconds, slope the seconds a computed token on
	// one engine, prompt the minutes' mean computed prompt, and vp the
	// row's capacity at it (the rule above), over ranks engines.
	s0, slope, prompt, vp float64
	minutes, ranks        int
	whole, hit            float64
	lo, hi                float64
}

// fitServiceLine fits the key's service line by the rule above; the refusal
// is "" when it is read. minutes is how many timed minutes it read.
func fitServiceLine(ks *keyState) (serviceLine, int, Refusal) {
	var xs, ys []float64
	var minutes []sample
	ranks := 0
	for _, name := range ks.podNames() {
		for _, s := range ks.pods[name].samples {
			if !s.timedOK {
				continue
			}
			xs = append(xs, s.timed.ieff)
			ys = append(ys, s.timed.prefill/(1+s.timed.occ))
			minutes = append(minutes, s)
			if ranks == 0 || s.timed.ranks < ranks {
				ranks = s.timed.ranks
			}
		}
	}
	n := len(xs)
	if n < MinTimingMinutes {
		return serviceLine{}, n, TooFewTimedMinutes
	}
	sorted := slices.Sorted(slices.Values(xs))
	lo, hi := percentileSorted(sorted, timingPercentile), percentileSorted(sorted, 1-timingPercentile)
	if !(hi >= MinPromptSpread*lo) {
		return serviceLine{}, n, NoPromptSpread
	}
	intercept, slope := ols(xs, ys)
	if !(slope > 0) || !isFinite(slope) {
		return serviceLine{}, n, NonPositiveSlope
	}
	if !(intercept >= 0) || !isFinite(intercept) {
		return serviceLine{}, n, NonPositiveIntercept
	}
	if !(olsSlopeSE(xs, ys, intercept, slope) <= MaxVPLineSlopeRelSE*slope) {
		return serviceLine{}, n, SlopeUncertain
	}
	prompt := meanPrompt(xs)
	out := serviceLine{s0: intercept, slope: slope, prompt: prompt, minutes: n, ranks: max(ranks, 1), lo: lo, hi: hi}
	out.vp = float64(out.ranks) * prompt / (intercept + slope*prompt)
	out.whole, out.hit = meanWhole(minutes)
	return out, n, ""
}

// lineFill reads the service line, records it on est, and sets V_P from it
// where V_P is refused for want of a queue, or is the served peak of a fleet
// that never queued at or under the line's (the rule above).
func (est *Estimate) lineFill(ks *keyState) {
	ln, n, refused := fitServiceLine(ks)
	est.VPLineMinutes, est.VPLineRefused = n, refused
	if refused != "" {
		return
	}
	est.VPLine, est.VPLineS0, est.VPLineSlope = ln.vp, ln.s0, ln.slope
	switch {
	case est.VPRefused == NoPrefillCapacity:
	case est.VPRefused == "" && est.VPLowerBound && est.VPLowerBoundReason == NeverQueued && est.VP <= ln.vp:
	default:
		return
	}
	est.VP, est.VPSource, est.VPRefused, est.VPSamples = ln.vp, SourceServiceLine, "", ln.minutes
	est.VPLowerBound, est.VPLowerBoundReason, est.VPFlooredAtServed = false, "", false
	// Read now, not from an earlier peak in the window (capacity.go).
	est.VPLearnedAt = time.Time{}
	est.VPPromptTokens, est.VPWholePromptTokens, est.VPPromptHitRate = ln.prompt, ln.whole, ln.hit
	est.VPPromptLow, est.VPPromptHigh = ln.lo, ln.hi
	// The line splits a request's work: s0 of it per request on an engine,
	// s0/ranks pod-seconds on the row, the rest per token. The knee prices
	// V_P along it at the asked prompt (whatif.Prefill.VPRequestSeconds).
	est.VPRequestSeconds = ln.s0 / float64(ln.ranks)
}

// olsSlopeSE is the standard error of the least-squares line's slope through
// the points: sqrt(s^2/Sxx), s^2 the residuals' variance over n-2. NaN with
// fewer than three points or no spread in x.
func olsSlopeSE(xs, ys []float64, intercept, slope float64) float64 {
	n := float64(len(xs))
	if n < 3 {
		return math.NaN()
	}
	mx := 0.0
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
	return math.Sqrt(rss / (n - 2) / sxx)
}
