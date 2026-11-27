package whatif

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// labComputed and labPrompt are cumulative histograms of the shape the
// rank mixture reads: computed tokens spread from 1,000 to 16,000 and whole
// prompts from 8,000 to 32,000.
func labComputed() map[float64]float64 {
	return map[float64]float64{1000: 0, 2000: 10, 4000: 30, 8000: 70, 16000: 100, math.Inf(1): 100}
}

func labPrompt() map[float64]float64 {
	return map[float64]float64{8000: 0, 16000: 60, 32000: 100, math.Inf(1): 100}
}

// The mean is the one the draws give: log-uniform inside each bucket.
func TestCumulativeMean_IsTheDrawsMean(t *testing.T) {
	m, ok := CumulativeMean(map[float64]float64{100: 0, 200: 50, 400: 100, math.Inf(1): 100})
	require.True(t, ok)
	want := 0.5*100/math.Log(2) + 0.5*200/math.Log(2)
	assert.InDelta(t, want, m, 1e-9)
	h, _ := newCumHist(map[float64]float64{100: 0, 200: 50, 400: 100, math.Inf(1): 100})
	var sum float64
	const n = 200000
	for i := range n {
		sum += h.ppf((float64(i) + 0.5) / n)
	}
	assert.InDelta(t, want, sum/n, 0.01, "the draws' mean")
	_, ok = CumulativeMean(map[float64]float64{100: 0, math.Inf(1): 0})
	assert.False(t, ok, "nothing counted")
}

// Scaled by s, every bound moves by s and the mean with it, until the cap:
// no share of the scaled histogram reads a length over the prompt's at the
// same share. The total is kept, and a scale of 1, or one not a positive
// finite number, leaves the histogram as it is.
func TestComputedAtScale_ScalesAndCapsAtThePrompt(t *testing.T) {
	c, p := labComputed(), labPrompt()
	m0, _ := CumulativeMean(c)
	half := ComputedAtScale(c, p, 0.5)
	m, ok := CumulativeMean(half)
	require.True(t, ok)
	assert.InDelta(t, m0/2, m, 1e-6*m0, "below the prompt no cap binds")
	assert.Equal(t, 100.0, half[math.Inf(1)])

	up := ComputedAtScale(c, p, 3)
	mUp, ok := CumulativeMean(up)
	require.True(t, ok)
	mp, _ := CumulativeMean(p)
	assert.Less(t, mUp, 0.99*3*m0, "the cap binds at the top")
	assert.LessOrEqual(t, mUp, mp)
	hu, _ := newCumHist(up)
	hp, _ := newCumHist(p)
	for _, u := range []float64{0.1, 0.3, 0.5, 0.7, 0.9, 0.99} {
		assert.LessOrEqual(t, hu.ppf(u), hp.ppf(u)*(1+1e-9), "share %g", u)
	}

	for _, s := range []float64{1, 0, -1, math.NaN(), math.Inf(1)} {
		assert.Equal(t, c, ComputedAtScale(c, p, s), "scale %g", s)
	}
	assert.Equal(t, c, ComputedAtScale(c, nil, 2), "no prompt histogram")
}

// The flag a figure carries: PrefixHitAtCount where the hit moved by more
// than half of MaterialPrefixHitMove, PrefixHitHeld where a hit above that
// is held at more pods, none otherwise.
func TestHitAtCountFlag(t *testing.T) {
	at := func(h float64, n, np int) prefill.HitAtCount {
		r, ok := prefill.HitAtCountOf(h, 1000, n, np, nil)
		require.True(t, ok)
		return r
	}
	assert.Equal(t, PrefixHitAtCount, HitAtCountFlag(at(0.5, 2, 1)))
	assert.Equal(t, Flag(""), HitAtCountFlag(at(0.08, 2, 1)), "moved by 0.04: under half the material move")
	assert.Equal(t, PrefixHitHeld, HitAtCountFlag(at(0.5, 2, 4)))
	assert.Equal(t, Flag(""), HitAtCountFlag(at(0.04, 2, 4)), "no material hit to hold")
	assert.Equal(t, Flag(""), HitAtCountFlag(at(0.5, 2, 2)))
}
