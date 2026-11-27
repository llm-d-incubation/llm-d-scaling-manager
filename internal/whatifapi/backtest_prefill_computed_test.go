package whatifapi

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A step whose prefill pods computed 6000 tokens of a 24000-token prompt
// (a 0.75 prefix hit) states both to the fleet priced: the whole prompt as
// PromptTokens and the computed one as PrefillComputedTokens, so the planner
// can price the work its prefill pods did and the cached context they
// attended over (issue llm-d-incubation/llm-d-scaling-manager#311). Before,
// the step stated the whole prompt alone, and a fit whose rows read no hit
// priced all 24000 tokens as computed. A step whose prefill rows read no
// computed prompt states none.
func TestBacktest_StepCarriesItsPrefillComputedPrompt(t *testing.T) {
	r := &stubResolver{}
	var mu sync.Mutex
	var knobs []Knobs
	r.onFleet = func(k Knobs) { mu.Lock(); knobs = append(knobs, k); mu.Unlock() }
	fitS, fitL := btEarlier(btSeries(240, 0, 1))
	fit := btEntry(t, r, "fit", fitS, fitL, btConfig("H100"))

	s, l := btSeries(240, 0, 1)
	for i := range s {
		s[i].PromptTokens = 24000
		s[i].PromptRequestRate, s[i].PromptComputedRate = s[i].Rate, 6000*s[i].Rate
	}
	btRun(t, fit, btEntry(t, r, "hit", s, l, btConfig("H100")), BacktestRequest{})
	require.NotEmpty(t, knobs, "fleets were priced")
	for _, k := range knobs {
		assert.InDelta(t, shapeKnob(24000), k.PromptTokens, 1e-9, "the whole prompt")
		assert.InDelta(t, shapeKnob(6000), k.PrefillComputedTokens, 1e-9, "the prompt the prefill pods computed")
	}

	knobs = nil
	s, l = btSeries(240, 0, 1)
	btRun(t, fit, btEntry(t, r, "none", s, l, btConfig("H100")), BacktestRequest{})
	require.NotEmpty(t, knobs, "fleets were priced")
	for _, k := range knobs {
		assert.Zero(t, k.PrefillComputedTokens, "no computed prompt read")
	}
}

// A computed prompt read above the whole one (two rows' readings, on their
// grids) is stated at the whole prompt, never above it.
func TestBacktest_StepComputedPromptNeverAboveTheWhole(t *testing.T) {
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
		assert.InDelta(t, k.PromptTokens, k.PrefillComputedTokens, 1e-9, "computed 8000 of a 1000-token prompt, held at 1000")
	}
}
