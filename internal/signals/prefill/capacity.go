package prefill

import (
	"slices"
	"time"
)

// Prefill capacity, V_P (slo-capacity-planner-ttft.md, "Prefill capacity").
//
// A vLLM prefill pod is a work-conserving server of prompt tokens, so its
// computed-token rate never exceeds its capacity: a rate read while it held
// work is at most V_P. The sources are taken in the page's order, and only
// sources that cannot over-state V_P:
//
//  1. Busy minutes: the queue above zero on every sample of the minute, the
//     pod's KV not full and decode not back-pressuring; V_P is their median
//     over the last BusyWindow, from at least MinBusyMinutes. A busy minute
//     needs the least waiting count of the minute (Cycle.QueueMin, from the
//     rows' QueueMin by QueueMins) read and above zero: a pod whose queue
//     min was not read has none. A busy minute reads at or below V_P: at it
//     when the pod ran flat out, under it when a data-parallel rank idled
//     (flagged, DataParallelRanks: one multi-rank minute in the window
//     flags the whole median, the conservative side) or the pod stalled on
//     something the tests above miss (decode back-pressure that its
//     saturation test does not catch, say). The busy minutes are counted
//     over BusyWindow and the queued minutes over Retention, so the two
//     counts are not compared (Estimate).
//  2. The peak under a queue: the highest rate sustained over PeakRun
//     minutes, each with a queue on some sample. A lower bound. A peak with
//     no queue is the offered load, not capacity, and is not used.
//  3. The back-pressure bound (Cycle.Bound). A lower bound.
//
// Otherwise NoPrefillCapacity. SGLang always refuses: it publishes no
// computed-token series.
//
// "Cannot over-state" holds up to the noise of rate(). Each sample is a
// one-minute rate, and the peak is the max, over every replica and up to
// Retention (up to 358 overlapping three-minute windows on one replica), of
// each window's least minute. A minimum of three cuts a single high minute, but the max
// over that many runs still keeps the luckiest one: the peak can read above
// V_P by about the spread of a one-minute rate, never by more.

// MinBusyMinutes is the fewest busy minutes V_P is taken from.
const MinBusyMinutes = 5

// BusyWindow is how far back busy minutes are read. V_P depends on the
// prompt lengths a pod is fed (each step pays a fixed cost the prompts
// share), so a busy minute speaks for the load it ran under, and a median
// over Retention would keep a load that has gone for six hours and win over
// the fresher sources below. Thirty minutes is six times MinBusyMinutes:
// room for a pod saturated one minute in six to keep its source, while a
// busy period that ended leaves it within half an hour.
const BusyWindow = 30 * time.Minute

// PeakRun is how many queued minutes in a row a peak must be sustained over.
const PeakRun = 3

// MaxRunGap is the widest gap between two minutes that still continues a
// run: one missed minute.
const MaxRunGap = 2 * time.Minute

// MinBoundSamples is the fewest spaced readings the back-pressure bound must
// hold before it is a source, as many as the floor needs before it orders
// from it (floor.MinThroughputSamplesToOrder).
const MinBoundSamples = 2

// capacity sets V_P on est from the key's samples and the cycle's bound.
func (est *Estimate) capacity(ks *keyState, c Cycle, sglang bool) {
	if sglang {
		est.VPRefused = SGLangRefused
		return
	}
	est.VPQueuedMinutes, est.VPLongestQueuedRun = queuedRuns(ks)
	bm := busyMedian(ks, c.Now)
	est.VPBusyMinutes = bm.n
	if bm.n >= MinBusyMinutes {
		est.VP, est.VPSource, est.VPSamples, est.VPPromptTokens = bm.rate, SourceBusyMinutes, bm.n, bm.prompt
		est.VPWholePromptTokens, est.VPPromptHitRate = bm.whole, bm.hit
		if bm.multiRank {
			est.VPLowerBound, est.VPLowerBoundReason = true, DataParallelRanks
		}
		return
	}
	if pk, ok := peakUnderQueue(ks); ok {
		est.VP, est.VPSource, est.VPLowerBound, est.VPSamples = pk.rate, SourcePeakUnderQueue, true, PeakRun
		est.VPPromptTokens, est.VPWholePromptTokens, est.VPPromptHitRate = pk.prompt, pk.whole, pk.hit
		return
	}
	if b := c.Bound; finitePositive(b.Tokens) && b.Samples >= MinBoundSamples {
		est.VP, est.VPSource, est.VPLowerBound, est.VPSamples = b.Tokens, SourceBackPressureBound, true, b.Samples
		return
	}
	est.VPRefused = NoPrefillCapacity
}

// busy is the busy minutes' reading: the median rate, how many minutes,
// whether any came from a pod with more than one data-parallel rank, and
// their mean prompt length (zero when one of them read none). whole and hit
// are their mean whole prompt and prefix-cache hit rate (meanWhole).
type busy struct {
	rate      float64
	n         int
	multiRank bool
	prompt    float64
	whole     float64
	hit       float64
}

// busyMedian is the busy reading over every replica's busy minutes no older
// than BusyWindow at now; zero with none.
func busyMedian(ks *keyState, now time.Time) busy {
	cutoff := now.Add(-BusyWindow)
	var rates, prompts []float64
	var minutes []sample
	var out busy
	for _, name := range ks.podNames() {
		for _, s := range ks.pods[name].samples {
			if s.busy && s.rateOK && s.rate > 0 && !s.at.Before(cutoff) {
				rates, prompts = append(rates, s.rate), append(prompts, s.prompt)
				minutes = append(minutes, s)
				out.multiRank = out.multiRank || s.multiRank
			}
		}
	}
	if len(rates) == 0 {
		return busy{}
	}
	out.rate, out.n, out.prompt = median(rates), len(rates), meanPrompt(prompts)
	out.whole, out.hit = meanWhole(minutes)
	return out
}

// meanWhole is the mean whole prompt of minutes and their mean prefix-cache
// hit rate, both zero when a minute read no whole prompt, as meanPrompt is:
// what V_P was read at beside its effective prompt
// (Estimate.VPWholePromptTokens).
func meanWhole(minutes []sample) (whole, hit float64) {
	for _, s := range minutes {
		if !(s.whole > 0) {
			return 0, 0
		}
		whole, hit = whole+s.whole, hit+s.hit
	}
	if len(minutes) == 0 {
		return 0, 0
	}
	n := float64(len(minutes))
	return whole / n, hit / n
}

// meanPrompt is the mean of prompts, zero when one of them is zero (not
// read): a mean that left a minute out would not be the mean of the
// minutes V_P rests on.
func meanPrompt(prompts []float64) float64 {
	sum := 0.0
	for _, p := range prompts {
		if !(p > 0) {
			return 0
		}
		sum += p
	}
	return sum / float64(len(prompts))
}

// peakUnderQueue is the highest rate any replica sustained over PeakRun
// queued minutes in a row: the least rate of the run, the highest run; and
// the mean prompt length of that run's PeakRun minutes, with their whole
// prompt and hit rate (meanWhole). n is unset.
func peakUnderQueue(ks *keyState) (busy, bool) {
	var best busy
	found := false
	for _, name := range ks.podNames() {
		run, prompts, minutes := []float64{}, []float64{}, []sample{}
		var last time.Time
		for _, s := range ks.pods[name].samples {
			if !s.queued || !s.rateOK || !(s.rate > 0) {
				run, prompts, minutes = run[:0], prompts[:0], minutes[:0]
				continue
			}
			if len(run) > 0 && s.at.Sub(last) > MaxRunGap {
				run, prompts, minutes = run[:0], prompts[:0], minutes[:0]
			}
			run, prompts, minutes = append(run, s.rate), append(prompts, s.prompt), append(minutes, s)
			last = s.at
			if len(run) < PeakRun {
				continue
			}
			sustained := slices.Min(run[len(run)-PeakRun:])
			if sustained > best.rate {
				best = busy{rate: sustained, prompt: meanPrompt(prompts[len(prompts)-PeakRun:])}
				best.whole, best.hit = meanWhole(minutes[len(minutes)-PeakRun:])
				found = true
			}
		}
	}
	return best, found
}

// queuedRuns is how many samples of the key's replicas held a queue with a
// rate read, and the longest run of them on one replica, by the rule
// peakUnderQueue reads runs by.
func queuedRuns(ks *keyState) (queued, longest int) {
	for _, name := range ks.podNames() {
		run := 0
		var last time.Time
		for _, s := range ks.pods[name].samples {
			if !s.queued || !s.rateOK || !(s.rate > 0) {
				run = 0
				continue
			}
			if run > 0 && s.at.Sub(last) > MaxRunGap {
				run = 0
			}
			run++
			queued++
			last = s.at
			longest = max(longest, run)
		}
	}
	return queued, longest
}

// median is the median of xs, which it sorts; NaN never reaches it.
func median(xs []float64) float64 {
	slices.Sort(xs)
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}
