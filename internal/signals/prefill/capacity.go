package prefill

import (
	"math"
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
// With Cycle.VPFloorServed a lower bound (2, 3, or busy minutes of a
// data-parallel pod) is raised to the served peak, the highest rate any
// replica sustained over PeakRun minutes queued or not (floorAtServed): a
// lower bound too, read over Retention as the peak under a queue is. It
// keeps the mean prompt of its run as VPPromptTokens, so the what-if model
// rescales it on the curve to a longer prompt asked, or without a curve flags
// one far from it VPShapeMoved, as it does the peak under a queue. Taking
// the max over unqueued minutes too adds windows, so its noise is that of
// source 2 over more runs: still within a one-minute rate's spread.
//
// With Cycle.VPWindowBest the peak under a queue (2) is the best the key read
// since its first cycle, not only over the samples kept (Retention): a replay
// of a recording reads its whole window, so a fleet that queued at its
// evening peak keeps that V_P through the quiet hours after it, flagged with
// when it was read (Estimate.VPLearnedAt). It stays a lower bound, and is
// floored at the served peak as any lower bound is.
//
// With Cycle.VPServedUnqueued, where none of the three has a reading, the
// served peak itself is V_P (SourceServedPeak), a lower bound for the same
// reason (NeverQueued): a fleet whose pods never held a capacity queue for
// PeakRun minutes in a row in Retention gets V_P as at least what its pods
// computed, not no V_P at all.
// With Cycle.VPOccupancyPlateau, a V_P refused here for want of a queue, or
// a lower bound under it, is read from the plateau of the served rate
// against the prefill occupancy (plateau.go): V_P where the curve flattened
// in the window, a lower bound where it did not.
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
		est.VP, est.VPSource, est.VPSamples = bm.rate, SourceBusyMinutes, bm.n
		est.setPrompts(bm)
		if bm.multiRank {
			est.VPLowerBound, est.VPLowerBoundReason = true, DataParallelRanks
			est.floorAtServed(ks, c.VPFloorServed)
		}
		return
	}
	pk, ok := peakUnderQueue(ks)
	if c.VPWindowBest {
		pk, ok = ks.windowBestQueued(pk, ok, c.Now, est)
	}
	if ok {
		est.VP, est.VPSource, est.VPLowerBound, est.VPSamples = pk.rate, SourcePeakUnderQueue, true, PeakRun
		est.setPrompts(pk)
		est.floorAtServed(ks, c.VPFloorServed)
		return
	}
	if b := c.Bound; finitePositive(b.Tokens) && b.Samples >= MinBoundSamples {
		est.VP, est.VPSource, est.VPLowerBound, est.VPSamples = b.Tokens, SourceBackPressureBound, true, b.Samples
		est.floorAtServed(ks, c.VPFloorServed)
		return
	}
	if pk, ok := servedPeak(ks); ok && c.VPServedUnqueued {
		est.VP, est.VPSource, est.VPLowerBound, est.VPSamples = pk.rate, SourceServedPeak, true, PeakRun
		est.VPLowerBoundReason, est.VPServedPeak = NeverQueued, pk.rate
		est.setPrompts(pk)
		return
	}
	est.VPRefused = NoPrefillCapacity
}

// windowBestQueued is the peak under a queue to take with
// Cycle.VPWindowBest at now: the key's best since its first cycle
// (keyState.bestQueued), where its run ended before the samples kept
// (Retention) and it is higher than the peak in those samples, or where
// those hold none; else the current peak, as without the switch. It notes
// the current peak first. A peak taken from before the samples kept sets
// est.VPLearnedAt, the last minute of its run, so the caller can say how
// old it is. Only Retention ages a peak out here: a run the samples kept
// still hold, but no longer read as the best (a later cycle in the same
// minute replaced one of its samples), is not taken from the memory. It is
// a lower bound as the current one is: a rate sustained under a queue, at
// the prompts of its run, which the what-if model rescales to the prompt
// asked (VPPromptTokens).
func (ks *keyState) windowBestQueued(cur busy, ok bool, now time.Time, est *Estimate) (busy, bool) {
	if ok && (!ks.bestQueuedOK || cur.rate >= ks.bestQueued.rate) {
		ks.bestQueued, ks.bestQueuedOK = cur, true
		return cur, true
	}
	if !ks.bestQueuedOK || !ks.bestQueued.at.Before(now.Add(-Retention)) {
		return cur, ok
	}
	est.VPLearnedAt = ks.bestQueued.at
	return ks.bestQueued, true
}

// floorAtServed raises a lower-bound V_P to the served peak (servedPeak)
// where that is higher, with on (Cycle.VPFloorServed), and records the peak
// in VPServedPeak either way. A pod computes no faster than V_P whether or
// not it queued, so the highest rate it sustained is a lower bound as good
// as the peak under a queue, with the same noise. On a fleet whose pods
// never queue for capacity, the peak under a queue comes from a few queued
// minutes at a light load, and the knee priced from it sits far under the
// load the pods were seen to serve (issue
// llm-d-incubation/llm-d-scaling-manager#280). The source is kept and stays
// a lower bound; VPFlooredAtServed says the figure is the served peak's,
// and the floored V_P takes the served run's prompt, whole prompt and hit
// rate with it.
// Only a lower bound is floored: busy minutes of a single-rank pod are V_P
// itself, and no source at all stays refused here, as a peak with no queue
// cannot tell capacity from the offered load (Cycle.VPServedUnqueued takes
// it as a lower bound and no more).
func (est *Estimate) floorAtServed(ks *keyState, on bool) {
	pk, ok := servedPeak(ks)
	if !ok {
		return
	}
	est.VPServedPeak = pk.rate
	if !on || !(pk.rate > est.VP) {
		return
	}
	est.VP, est.VPSamples, est.VPFlooredAtServed = pk.rate, PeakRun, true
	est.setPrompts(pk)
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
	// at is the last minute of a peak's run (peakRun); unset otherwise.
	at time.Time
	// lo and hi are the 10th and 90th percentiles of the minutes' prompts
	// (promptRange), both zero when one of them read none.
	lo, hi float64
}

// setPrompts records the prompts b was read at on est: its mean prompt,
// whole prompt and hit rate, and the range of its minutes' prompts.
func (est *Estimate) setPrompts(b busy) {
	est.VPPromptTokens, est.VPWholePromptTokens, est.VPPromptHitRate = b.prompt, b.whole, b.hit
	est.VPPromptLow, est.VPPromptHigh = b.lo, b.hi
}

// promptLowQuantile and promptHighQuantile are the quantiles of the minutes'
// prompts that bound the range a V_P speaks for (Estimate.VPPromptLow,
// VPPromptHigh): a tenth of the minutes may sit outside each end, so one odd
// minute does not widen the range.
const (
	promptLowQuantile  = 0.10
	promptHighQuantile = 0.90
)

// promptRange is the promptLowQuantile and promptHighQuantile of prompts, by
// the nearest rank, both zero when one of them is zero (not read), as
// meanPrompt is. It does not reorder prompts.
func promptRange(prompts []float64) (lo, hi float64) {
	if len(prompts) == 0 || meanPrompt(prompts) == 0 {
		return 0, 0
	}
	xs := slices.Sorted(slices.Values(prompts))
	rank := func(q float64) float64 {
		i := int(math.Ceil(q*float64(len(xs)))) - 1
		return xs[min(max(i, 0), len(xs)-1)]
	}
	return rank(promptLowQuantile), rank(promptHighQuantile)
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
	out.lo, out.hi = promptRange(prompts)
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
	return peakRun(ks, true)
}

// servedPeak is peakUnderQueue over every minute with a rate, queued or
// not: the highest rate any replica was seen to compute, sustained over
// PeakRun minutes in a row.
func servedPeak(ks *keyState) (busy, bool) {
	return peakRun(ks, false)
}

// peakRun is the highest rate any replica sustained over PeakRun minutes in
// a row, each queued when queued is set, and the mean prompt length, whole
// prompt and hit rate of that run's minutes.
func peakRun(ks *keyState, queued bool) (busy, bool) {
	var best busy
	found := false
	for _, name := range ks.podNames() {
		run, prompts, minutes := []float64{}, []float64{}, []sample{}
		var last time.Time
		for _, s := range ks.pods[name].samples {
			if (queued && !s.queued) || !s.rateOK || !(s.rate > 0) {
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
				best = busy{rate: sustained, prompt: meanPrompt(prompts[len(prompts)-PeakRun:]), at: s.at}
				best.whole, best.hit = meanWhole(minutes[len(minutes)-PeakRun:])
				best.lo, best.hi = promptRange(prompts[len(prompts)-PeakRun:])
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
