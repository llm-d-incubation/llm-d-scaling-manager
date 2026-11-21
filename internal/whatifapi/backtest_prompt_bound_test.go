package whatifapi

import (
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// btPromptSeries is btSeries with a computed-prompt histogram on every
// cycle: 8000-token prompts, all in the (5000, 10000] bucket, so the
// histogram reads cv 0.31 and bounds the prompt at 1.25 times its mean.
func btPromptSeries(n int) ([]SeriesSample, []LatencySample) {
	s, l := btSeries(n, 0, 1)
	for i := range s {
		r := s[i].Rate
		s[i].PromptRequestRate, s[i].PromptComputedRate = r, 8000*r
		s[i].PromptBuckets = map[float64]float64{5000: 0, 10000: r, math.Inf(1): r}
	}
	return s, l
}

// A step whose histogram states its spread carries the bound that histogram
// sets (whatif.PromptSpread.MaxRatio) to the fleet priced, beside the cv: a
// stated cv would otherwise price the gamma past the longest prompt the
// buckets prove. A typed cv (assume.promptCV) carries none.
func TestBacktest_StepCarriesItsPromptBound(t *testing.T) {
	r := &stubResolver{}
	var mu sync.Mutex
	var knobs []Knobs
	r.onFleet = func(k Knobs) { mu.Lock(); knobs = append(knobs, k); mu.Unlock() }
	fitS, fitL := btEarlier(btSeries(240, 0, 1))
	fit := btEntry(t, r, "fit", fitS, fitL, btConfig("H100"))
	s, l := btPromptSeries(240)
	btRun(t, fit, btEntry(t, r, "test", s, l, btConfig("H100")), BacktestRequest{})
	require.NotEmpty(t, knobs, "fleets were priced")
	for _, k := range knobs {
		require.NotNil(t, k.Assume.PromptCV, "the step's spread is stated")
		assert.InDelta(t, 0.3, *k.Assume.PromptCV, 1e-9, "the step's spread, on its grid")
		assert.InDelta(t, 1.25, k.PromptMaxRatio, 1e-9, "the step's bound, 10000 over 8000")
	}

	knobs = nil
	btRun(t, fit, btEntry(t, r, "typed", s, l, btConfig("H100")), BacktestRequest{Assume: &Assume{PromptCV: ptr(0.5)}})
	require.NotEmpty(t, knobs, "fleets were priced")
	for _, k := range knobs {
		assert.Zero(t, k.PromptMaxRatio, "a typed cv carries no histogram's bound")
	}
}

// A step's bound is raised to the grid, never lowered: a lower bound would
// price less of the prompt than the histogram allows.
func TestMaxRatioOnGrid(t *testing.T) {
	for in, want := range map[float64]float64{0: 0, -1: 0, 1.25: 1.25, 1.246: 1.25, 1.2501: 1.3, 1.995: 2, 2.003: 2.05} {
		assert.InDelta(t, want, maxRatioOnGrid(in), 1e-12, "%g", in)
	}
	assert.Zero(t, maxRatioOnGrid(math.NaN()))
}
