package whatif

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The fit cores of the experimental latency models, shared by their two
// callers: the capacity planner, which fits them on a recording's cycles
// (whatifapi), and the slo analyzer, which fits them on the cycles the
// controller observes (docs/proposals/predicted-latency-in-controller.md).
// Both build the same FitCycle from the same analyzer input and router
// reading (FitCycleOf), and both call the same fits on them
// (FitRankMixtureCycles, FitTPOTDistCycles, FitMixtureWindow, FitTPOTWindow),
// so the two give the same figures on the same cycles.

// DefaultMeasureWindow is the rate window, seconds, a measurement is read over
// when the source does not say: the engines' histograms are over a minute.
const DefaultMeasureWindow = 60.0

// FitCycle is one cycle of one model as the experimental latency models fit
// on it: its arrival and completion rates, the router's latency over its
// window, and the cycle each model reads (MixtureCycleOf, DecodeCycleOf).
type FitCycle struct {
	At time.Time
	// Rate is the model's arrival rate, requests a second; RateKnown false
	// where the input carried none and decode rows served requests, Rate
	// then the decode rows' (ArrivalOf). EngineRate is the decode and unsplit
	// rows' request rate, summed: the completions.
	Rate       float64
	RateKnown  bool
	EngineRate float64
	// Latency says the cycle carries a latency reading at all, Router the
	// router's over its window, nil where it reported none.
	Latency bool
	Router  *domain.RouterLatency
	// Mixture and Decode are the cycle as the TTFT rank mixture and the TPOT
	// distribution read it.
	Mixture MixtureCycle
	Decode  DecodeCycle
}

// FitCycleOf is one cycle of in, read at at, with the router's reading
// router: the prefill ranks of prefillVariant and the decode rows of
// decodeVariant, each every variant of its role where empty. The mixture
// cycle reads the router's processing histogram from router, not from
// in.RouterLatency, which the controller does not set on the input it hands
// the v2 models.
func FitCycleOf(in domain.AnalyzerInput, router *domain.RouterLatency, at time.Time, prefillVariant, decodeVariant string) FitCycle {
	rate, known, engine := ArrivalOf(in)
	in.RouterLatency = router
	return FitCycle{At: at, Rate: rate, RateKnown: known, EngineRate: engine, Latency: true, Router: router,
		Mixture: MixtureCycleOf(in, at, prefillVariant, decodeVariant), Decode: DecodeCycleOf(in, at, decodeVariant)}
}

// ArrivalOf is in's arrival rate and the rate its engines complete: rate is
// the router's (in.ArrivalRate), or, where the input carries none while
// decode rows served requests, the decode rows' request rate summed, known
// false; a cycle with neither is idle, a known 0. engine is the decode and
// unsplit ("both") rows' request rate summed, whatever rate is.
func ArrivalOf(in domain.AnalyzerInput) (rate float64, known bool, engine float64) {
	roles := rolesOf(in)
	var decodeRate, bothRate float64
	for _, rm := range in.ReplicaMetrics {
		role := domain.CanonicalRole(roles[rm.VariantName])
		if role == domain.RoleDecode && rm.RequestRateKnown && rm.RequestRate > 0 {
			decodeRate += rm.RequestRate
		}
		if role == domain.RoleBoth && rm.RequestRateKnown && rm.RequestRate > 0 {
			bothRate += rm.RequestRate
		}
	}
	rate, known = in.ArrivalRate, in.ArrivalRate > 0 || decodeRate == 0
	if !known {
		rate = decodeRate
	}
	return rate, known, decodeRate + bothRate
}

// RouterWindowOf is the window, seconds, the router's reading r was taken
// over: its own, DefaultMeasureWindow where it does not say.
func RouterWindowOf(r *domain.RouterLatency) float64 {
	if r != nil && r.WindowSeconds > 0 {
		return r.WindowSeconds
	}
	return DefaultMeasureWindow
}

// RouterTTFTOf is the router's measured TTFT mean and p90 over its window,
// seconds; ok false where either is not read or the p90 is in the +Inf
// bucket.
func RouterTTFTOf(r *domain.RouterLatency) (mean, p90 float64, ok bool) {
	if r == nil || !(r.TTFTCountRate > 0) {
		return 0, 0, false
	}
	h, hok := LatencyHistOf(r.TTFTBuckets)
	if !hok {
		return 0, 0, false
	}
	mean, p90 = r.TTFTSumRate/r.TTFTCountRate, h.Quantile(0.9)
	if math.IsInf(mean, 0) || math.IsInf(p90, 0) {
		return 0, 0, false
	}
	return mean, p90, true
}

// MixtureCyclesIn is the mixture cycle of each of cycles whose time is in
// (from, to], in cycles' order.
func MixtureCyclesIn(cycles []FitCycle, from, to time.Time) []MixtureCycle {
	var out []MixtureCycle
	for i := range cycles {
		if at := cycles[i].At; at.After(from) && !at.After(to) {
			out = append(out, cycles[i].Mixture)
		}
	}
	return out
}

// DecodeCyclesIn is the decode cycle of each of cycles whose time is in
// (from, to], in cycles' order.
func DecodeCyclesIn(cycles []FitCycle, from, to time.Time) []DecodeCycle {
	var out []DecodeCycle
	for i := range cycles {
		if at := cycles[i].At; at.After(from) && !at.After(to) {
			out = append(out, cycles[i].Decode)
		}
	}
	return out
}

// MaxMixtureBacklog is the largest backlog (MixtureBacklog) at which the rank
// mixture reads a step as stationary. A step of length w whose arrivals A
// run ahead of its completions D leaves (A - D) w requests more waiting at
// its end, which at D adds (A - D)/D of the step to the last request's wait:
// a transient that share of the step. The pointwise-stationary approximation
// takes a transient under MaxRelaxationShare of the step as settled.
const MaxMixtureBacklog = MaxRelaxationShare

// MixtureBacklog is (A - D)/D over the cycles in (from, to]: A the router's
// request rate (the arrivals), D the engine rows' (every request completes on
// a decode or an unsplit row). ok is false where the cycles do not read both.
func MixtureBacklog(cycles []FitCycle, from, to time.Time) (backlog, arrivals, completions float64, ok bool) {
	n := 0
	for i := range cycles {
		s := &cycles[i]
		if s.At.After(from) && !s.At.After(to) && s.RateKnown && s.Rate > 0 && s.EngineRate > 0 {
			arrivals += s.Rate
			completions += s.EngineRate
			n++
		}
	}
	if n == 0 {
		return 0, 0, 0, false
	}
	arrivals, completions = arrivals/float64(n), completions/float64(n)
	return (arrivals - completions) / completions, arrivals, completions, true
}

// MixtureBacklogged is why a step whose backlog is over MaxMixtureBacklog is
// not stationary, or "" where it is (or the rates are not read).
func MixtureBacklogged(cycles []FitCycle, from, to time.Time) string {
	b, a, d, ok := MixtureBacklog(cycles, from, to)
	if !ok || b <= MaxMixtureBacklog {
		return ""
	}
	return fmt.Sprintf("not stationary: arrivals %.3g r/s ran ahead of completions %.3g r/s, a backlog adding %.0f%% of the step "+
		"to the wait (over %.0f%%)", a, d, 100*b, 100*MaxMixtureBacklog)
}

// sortedByTime is cycles in time order, ties in their given order; cycles
// itself where already sorted.
func sortedByTime(cycles []FitCycle) []FitCycle {
	for i := 1; i < len(cycles); i++ {
		if cycles[i].At.Before(cycles[i-1].At) {
			out := make([]FitCycle, len(cycles))
			copy(out, cycles)
			slices.SortStableFunc(out, func(a, b FitCycle) int { return a.At.Compare(b.At) })
			return out
		}
	}
	return cycles
}

// stepAnchors cuts cycles, in time order, into steps of step: the step
// boundaries in order, and each one's anchor, its last cycle.
func stepAnchors(cycles []FitCycle, step time.Duration) (order []time.Time, anchors map[time.Time]int) {
	anchors = map[time.Time]int{}
	for i := range cycles {
		k := cycles[i].At.UTC().Truncate(step)
		if _, ok := anchors[k]; !ok {
			order = append(order, k)
		}
		anchors[k] = i
	}
	return order, anchors
}
