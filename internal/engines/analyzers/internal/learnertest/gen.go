// Package learnertest holds what the analyzers' learner tests share
// (docs/proposals/analyzer-learner.md, 2.5 and 2.6): the seeded generators,
// the property-case judgement against a known-failures ledger, and the
// closed-loop fleet simulator. Only tests import it; a test in this package
// holds that no other file does.
package learnertest

import (
	"math"
	"math/rand/v2"
	"time"
)

// Cycle is the controller's cycle: inputs are sampled at it.
const Cycle = 15 * time.Second

// Scrape is Prometheus's scrape interval: a gauge's avg_over_time[1m]
// averages the four samples it took in the minute, not every second of it.
const Scrape = 15 * time.Second

// Start is the synthetic clock's origin.
var Start = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// Rand is the generators' source for seed: a seed reproduces a case.
func Rand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, 0x0b3)) }

// ARNoise is n relative deviations of an AR(1) process with standard
// deviation sigma and lag-one correlation rho: the gauges' noise is
// autocorrelated (one-minute windows share 45 s), and iid noise over-counts
// flips (analyzer-learner.md, 3.5).
func ARNoise(r *rand.Rand, n int, sigma, rho float64) []float64 {
	out := make([]float64, n)
	innov := sigma * math.Sqrt(1-rho*rho)
	x := r.NormFloat64() * sigma
	for i := range out {
		out[i] = x
		x = rho*x + innov*r.NormFloat64()
	}
	return out
}

// TimeBase is the time-base input family: a per-second path, sampled at a
// Cycle cycle as Prometheus serves it to the collector.
type TimeBase struct {
	// Instant is the path at the cycle's instant; Rate1m its mean over the
	// minute before (rate[1m]); Peak1m its max over that minute
	// (max_over_time[1m]); Avg1m the mean of the samples Prometheus scraped
	// of it in that minute, one every Scrape (avg_over_time[1m] of a gauge).
	// Consecutive cycles' minutes share 45 s.
	Instant, Rate1m, Peak1m, Avg1m []float64
}

// SampleTimeBase samples path, one value per second, at every Cycle from the
// first full minute.
func SampleTimeBase(path []float64) TimeBase {
	var tb TimeBase
	step := int(Cycle / time.Second)
	for t := 60; t < len(path); t += step {
		sum, peak := 0.0, math.Inf(-1)
		for s := t - 59; s <= t; s++ {
			sum += path[s]
			peak = math.Max(peak, path[s])
		}
		scraped, n := 0.0, 0
		for s := t; s > t-60; s -= int(Scrape / time.Second) {
			scraped += path[s]
			n++
		}
		tb.Instant = append(tb.Instant, path[t])
		tb.Rate1m = append(tb.Rate1m, sum/60)
		tb.Peak1m = append(tb.Peak1m, peak)
		tb.Avg1m = append(tb.Avg1m, scraped/float64(n))
	}
	return tb
}

// RelErr is |got - want| / |want|, and +Inf when either is NaN, so a NaN
// never passes a tolerance.
func RelErr(got, want float64) float64 {
	if math.IsNaN(got) || math.IsNaN(want) {
		return math.Inf(1)
	}
	if want == 0 {
		return math.Abs(got)
	}
	return math.Abs(got-want) / math.Abs(want)
}

// Reversals counts how often a boolean verdict changed along a trajectory.
func Reversals(v []bool) int {
	n := 0
	for i := 1; i < len(v); i++ {
		if v[i] != v[i-1] {
			n++
		}
	}
	return n
}
