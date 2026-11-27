package whatifapi

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
)

// overlappingMinutes is n one-minute rates of a steady Poisson stream of
// rate r, read every step seconds: consecutive windows share 60 - step
// seconds of arrivals.
func overlappingMinutes(rng *rand.Rand, n int, r, step float64) []float64 {
	span := RateWindowSeconds + float64(n-1)*step
	var at []float64
	for t := rng.ExpFloat64() / r; t < span; t += rng.ExpFloat64() / r {
		at = append(at, t)
	}
	out := make([]float64, n)
	for i := range out {
		lo := float64(i) * step
		k := 0
		for _, t := range at {
			if t >= lo && t < lo+RateWindowSeconds {
				k++
			}
		}
		out[i] = float64(k) / RateWindowSeconds
	}
	return out
}

// The sample variance of 20 one-minute rates read 15 s apart holds about
// 0.86 of the counting variance r/60, as CountingShare says; readings a
// window or more apart hold all of it (#295).
func TestCountingShare(t *testing.T) {
	assert.InDelta(t, 0.855, CountingShare(20, 15), 0.001)
	assert.Equal(t, 1.0, CountingShare(20, 60))
	assert.Equal(t, 1.0, CountingShare(20, 0), "no clock: all of it")
	assert.Equal(t, 1.0, CountingShare(1, 15))
	assert.Equal(t, 1.0, CountingShare(20, math.Inf(1)))

	rng := rand.New(rand.NewPCG(7, 11))
	const r, buckets = 4.0, 3000
	var sum float64
	for range buckets {
		xs := overlappingMinutes(rng, 20, r, 15)
		var mean, ss float64
		for _, x := range xs {
			mean += x / 20
		}
		for _, x := range xs {
			ss += (x - mean) * (x - mean)
		}
		sum += ss / 19
	}
	got := sum / buckets / (r / RateWindowSeconds)
	assert.InDelta(t, CountingShare(20, 15), got, 0.03, "the simulated share")
}

// Overlapping readings take out only the share their variance holds: a
// p95 less counting noise of rates read 15 s apart is at least the one that
// takes out a whole minute's, and the same for readings a minute apart.
func TestModulationP95EveryTakesOutTheShare(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 17))
	for range 200 {
		xs := overlappingMinutes(rng, 20, 2, 15)
		mean, p95 := meanP95(xs)
		assert.GreaterOrEqual(t, ModulationP95Every(xs, mean, p95, 15), ModulationP95(xs, mean, p95))
		assert.Equal(t, ModulationP95(xs, mean, p95), ModulationP95Every(xs, mean, p95, 60))
	}
}
