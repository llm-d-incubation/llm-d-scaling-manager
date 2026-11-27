package prefill

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// V_P from light load: the lone rate (Cycle.VPLoneRate; issue
// llm-d-incubation/llm-d-scaling-manager#406).
//
// Every source in capacity.go needs a pod that queued for capacity, and the
// occupancy plateau (plateau.go) needs minutes at one request or more in
// prefill. A fleet that runs light has neither. Its quiet minutes still time
// one request alone on an engine: at under LoneMaxOccupancy requests in
// prefill per engine (rowOccupancy, Little's law), a request mostly has the
// engine's steps to itself, so its computed tokens over its prefill time is
// the speed the engine computes one prompt of that length at.
//
// That speed is V_P where the engine is compute-bound with one request: then
// batching more requests into a step adds no speed, and a busy pod serves at
// about the lone rate. In the offline study on the public Qwen3-32B and 0.6B
// recordings (issue #406) the lone rate read 0.92 to 1.07 of the busy-minute
// V_P at the same prompt, and put the prefill knee at 0.87 to 1.03 of the
// measured knee on the four staircases whose knee is prefill-bound. That
// study counted 15-second cycles, where the planner keeps one sample a
// minute, so its 10 readings were about 3 of MinLoneMinutes. Through the
// planner the switch moves no Qwen3-32B staircase knee: one of them,
// replayed whole, never holds MinLoneMinutes lone minutes at the prompt
// priced. Where a
// busy step batches short prompts into a step the lone request leaves
// mostly empty, batching does add speed, and the lone rate reads under V_P.
// The served peak tells the two apart: a pod seen to compute more than
// LoneServedTolerance over the lone rate was not compute-bound alone, and the
// lone rate is then only a lower bound.
//
// The rule, fixed before the switch was run against a recording, except that
// the served peak was first read over the samples kept only, and is now kept
// since the key's first cycle:
//
//   - A minute counts when its row's occupancy per engine is under
//     LoneMaxOccupancy, its prefill time and computed tokens a request were
//     read, and its effective prompt is within LonePromptBand of the prompt
//     the cycle's rows carry (MeanEffectivePrompt): the prompt priced. A
//     speed is a speed at a prompt length.
//   - With fewer than MinLoneMinutes such minutes over Retention, no
//     reading: abstain.
//   - The lone rate is the median of the minutes' computed tokens a request
//     over their prefill time.
//   - It is V_P when the highest served peak the key read since its first
//     cycle (servedPeak, any minute, queued or not, kept past Retention in
//     keyState.bestServed) is at most LoneServedTolerance over it; otherwise
//     it is a lower bound, recorded beside V_P (Estimate.VPLone) and never
//     taken for it: the served peak, a lower bound already, says more. A
//     replay reads its whole window, so a busy evening keeps a quiet night's
//     lone rate a bound.
//
// A lone rate that is V_P fills V_P where it is refused for want of a queue,
// or is a lower bound (the larger of the two is taken, not a bound). Busy
// minutes of a single-rank pod stand: the queue's figure wins where there is
// one.

// SourceLoneRate is V_P from the lone rate (lonerate.go): one request's
// computed tokens over its prefill time at light load, where the served peak
// does not exceed it by more than LoneServedTolerance.
const SourceLoneRate Source = "lone-rate"

// LoneMaxOccupancy is the most requests in prefill per engine, by Little's
// law, a minute may hold and still time a request alone.
const LoneMaxOccupancy = 0.3

// LonePromptBand is how far, as a fraction of it, a minute's effective
// prompt may be from the prompt priced and still count.
const LonePromptBand = 0.3

// MinLoneMinutes is the fewest lone minutes at the prompt priced the lone
// rate is read from.
const MinLoneMinutes = 10

// LoneServedTolerance is how far over the lone rate, as a fraction of it,
// the served peak may be for the lone rate to be V_P, not a lower bound: the
// spread of a one-minute rate.
const LoneServedTolerance = 0.10

// lone is the lone rate's reading of a key.
type lone struct {
	// rate is the median lone speed, minutes how many minutes it rests on,
	// and prompt, whole and hit their mean effective prompt, whole prompt
	// and hit rate.
	rate               float64
	minutes            int
	prompt, whole, hit float64
	// lo and hi are the range of the minutes' prompts (promptRange): the
	// prompts a lone-rate V_P speaks for, with Cycle.VPPromptCoverage.
	lo, hi float64
}

// loneSpeed is the row's lone speed: its computed tokens a request over its
// mean prefill time, at an effective prompt ieff, times its engine ranks
// (V_P is a row's, and each of a row's engines times its own request), and
// whether it is a reading.
func loneSpeed(rm domain.ReplicaMetrics, ieff float64) (float64, bool) {
	if !rm.AvgPrefillTimeKnown || !finitePositive(rm.AvgPrefillTime) || !finitePositive(ieff) {
		return 0, false
	}
	v := float64(rm.Ranks()) * ieff / rm.AvgPrefillTime
	return v, finitePositive(v)
}

// loneRate is the key's lone rate at the prompt ref by the rule above, and
// how many minutes counted; ok is false with fewer than MinLoneMinutes.
func loneRate(ks *keyState, ref float64) (lone, int, bool) {
	if !finitePositive(ref) {
		return lone{}, 0, false
	}
	lo, hi := ref*(1-LonePromptBand), ref*(1+LonePromptBand)
	var speeds, prompts []float64
	var minutes []sample
	for _, name := range ks.podNames() {
		for _, s := range ks.pods[name].samples {
			if !s.loneOK || !s.occOK || s.occ >= LoneMaxOccupancy || s.prompt < lo || s.prompt > hi {
				continue
			}
			speeds, prompts, minutes = append(speeds, s.lone), append(prompts, s.prompt), append(minutes, s)
		}
	}
	if len(speeds) < MinLoneMinutes {
		return lone{}, len(speeds), false
	}
	out := lone{rate: median(speeds), minutes: len(speeds), prompt: meanPrompt(prompts)}
	out.whole, out.hit = meanWhole(minutes)
	out.lo, out.hi = promptRange(prompts)
	return out, out.minutes, true
}

// loneFill reads the lone rate at ref, the prompt the cycle's rows carry,
// with on (Cycle.VPLoneRate), records it on est, and sets V_P from it where
// it is V_P (the served peak at most LoneServedTolerance over it) and V_P is
// refused for want of a queue or is a lower bound: to the larger of the
// two, not a bound. A lone rate that is only a lower bound is recorded and
// not taken.
func (est *Estimate) loneFill(ks *keyState, c Cycle, ref float64) {
	if !c.VPLoneRate {
		return
	}
	if pk, ok := servedPeak(ks); ok {
		ks.bestServed = max(ks.bestServed, pk.rate)
	}
	ln, n, ok := loneRate(ks, ref)
	est.VPLoneMinutes = n
	if !ok {
		return
	}
	est.VPLone = ln.rate
	est.VPLoneIsVP = ks.bestServed <= (1+LoneServedTolerance)*ln.rate
	if !est.VPLoneIsVP {
		return
	}
	switch {
	case est.VPRefused == NoPrefillCapacity:
	case est.VPRefused == "" && est.VPLowerBound:
	default:
		return
	}
	vp := ln.rate
	if est.VPRefused == "" && est.VP > vp {
		vp = est.VP
	}
	est.VP, est.VPSource, est.VPRefused, est.VPSamples = vp, SourceLoneRate, "", ln.minutes
	est.VPLowerBound, est.VPLowerBoundReason, est.VPFlooredAtServed = false, "", false
	est.VPPromptTokens, est.VPWholePromptTokens, est.VPPromptHitRate = ln.prompt, ln.whole, ln.hit
	// The range is the lone minutes', not the source V_P replaced: a lone
	// rate speaks for the prompts it was timed at, within LonePromptBand of
	// the prompt priced (Estimate.VPPromptLow; zeroed in Observe without
	// Cycle.VPPromptCoverage).
	est.VPPromptLow, est.VPPromptHigh = ln.lo, ln.hi
}
