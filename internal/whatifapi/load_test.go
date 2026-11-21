package whatifapi

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// poissonMinutes is n one-minute rates of a steady Poisson stream of rate r.
func poissonMinutes(rng *rand.Rand, n int, r float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		// Arrivals in a minute: exponential gaps summed to 60 s.
		var t float64
		k := 0
		for {
			t += rng.ExpFloat64() / r
			if t > RateWindowSeconds {
				break
			}
			k++
		}
		out[i] = float64(k) / RateWindowSeconds
	}
	return out
}

// meanP95 is the mean and the nearest-rank p95 of xs.
func meanP95(xs []float64) (float64, float64) {
	s := slices.Clone(xs)
	slices.Sort(s)
	var sum float64
	for _, x := range s {
		sum += x
	}
	return sum / float64(len(s)), s[int(math.Ceil(0.95*float64(len(s))))-1]
}

// A steady Poisson stream's one-minute p95 is its rate plus counting noise:
// at 1 r/s about 21% over, which the queueing model prices already. With
// the noise out it is the rate, within the sampling of a day.
func TestModulationP95SteadyPoissonIsItsRate(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, r := range []float64{0.5, 1, 4, 12.5} {
		rates := poissonMinutes(rng, 1440, r)
		mean, p95 := meanP95(rates)
		mod := ModulationP95(rates, mean, p95)
		assert.Greater(t, p95/r, 1+1.2/math.Sqrt(60*r), "r %g: the p95 holds the noise", r)
		assert.InDelta(t, r, mod, 0.06*r, "r %g: the p95 less its noise is the rate (p95 %.3f, mean %.3f)", r, p95, mean)
	}
}

// A load that swings far more than its counting noise keeps its p95: a day
// from 2 to 16 r/s.
func TestModulationP95KeepsASwing(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	rates := make([]float64, 0, 1440)
	for i := range 1440 {
		r := 9 + 7*math.Sin(2*math.Pi*float64(i)/1440)
		rates = append(rates, poissonMinutes(rng, 1, r)[0])
	}
	mean, p95 := meanP95(rates)
	mod := ModulationP95(rates, mean, p95)
	assert.Less(t, mod, p95)
	assert.InDelta(t, p95, mod, 0.01*p95, "the swing is the p95's excess, not the noise")
}

// The edges give the p95 back: one rate, rates all alike, a p95 at the mean.
func TestModulationP95Edges(t *testing.T) {
	assert.Equal(t, 3.0, ModulationP95([]float64{3}, 3, 3))
	assert.Equal(t, 2.0, ModulationP95([]float64{2, 2, 2}, 2, 2))
	assert.Equal(t, 1.0, ModulationP95([]float64{1, 1, 1, 1}, 2, 1), "a p95 under the mean")
	// Variance all noise: the mean.
	assert.InDelta(t, 1.0, ModulationP95([]float64{0.95, 1.05}, 1, 1.05), 1e-12)
}

// A shape off the load's cycles by more than 20% in a length is a mismatch.
func TestShapeLoadMismatch(t *testing.T) {
	load := ReportLoadShape{InputTokens: 1000, OutputTokens: 560}
	assert.False(t, ShapeLoadMismatch(1100, 600, load))
	assert.True(t, ShapeLoadMismatch(4569, 1855, load), "the guide's sample: priced at 4569/1855, the peak ran at about 1000/560")
	assert.True(t, ShapeLoadMismatch(1000, 700, load), "output 25% long")
	assert.False(t, ShapeLoadMismatch(1000, 700, ReportLoadShape{}), "no load shape to compare")
}

// "load" asks the load's own shape, and a load statistic may be the p95
// less its noise.
func TestParseLoadShapeAndStatistic(t *testing.T) {
	sh, err := ParseReportShape(ShapeLoad)
	require.NoError(t, err)
	assert.True(t, sh.Load)
	assert.False(t, sh.Measured)
	require.NoError(t, CheckLoadStatistic(LoadP95Modulation))
	require.Error(t, CheckLoadStatistic("p99"))
}
