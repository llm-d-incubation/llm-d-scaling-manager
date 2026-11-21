package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The PinnedDefaultB hint offers --assume-itl-b floor only where the floor
// gives an intercept the line refits through; elsewhere it offers a stated
// B and says why the floor gives none, instead of offering what the planner
// would then decline (issue llm-d-incubation/llm-d-scaling-manager#277).
func TestPinnedDefaultBHint_OffersTheFloorOnlyWhereItWorks(t *testing.T) {
	readings := []itl.Observation{{K: 0.30, ITLSec: 0.020}, {K: 0.40, ITLSec: 0.022}}
	pv := pricedVariant{state: learnreplay.VariantState{Tier: whatif.TierPinnedB, Refitted: true,
		Line: itl.Model{A: 0.04, B: itl.DefaultBaselineSec}, LineReadings: readings}}
	for name, c := range map[string]struct {
		floorITL, floorFill float64
		why, stated, api    string
	}{
		"no floor below the band": {0, 0, "no floor below the band", "--assume-itl-b 12ms states one", "assume.itlB in seconds ("},
		// The readings' mean fill, 0.35, is under twice the floor's; a stated
		// B is offered under the floor, which caps it.
		"no intercept": {0.0055, 0.20, "gives no intercept with the line's readings",
			"--assume-itl-b with a duration at most the floor's 5.5 ms states one", "assume.itlB in seconds, at most the floor's 0.0055"},
	} {
		t.Run(name, func(t *testing.T) {
			pv := pv
			pv.state.BelowBandITL, pv.state.BelowBandFill = c.floorITL, c.floorFill
			pv.api = false
			h := pv.pinnedDefaultBHint()
			assert.NotContains(t, h, "--assume-itl-b floor", "the floor is not offered where it gives no B")
			assert.Contains(t, h, c.stated)
			assert.Contains(t, h, "the floor gives none here: ")
			assert.Contains(t, h, c.why)
			pv.api = true
			h = pv.pinnedDefaultBHint()
			assert.NotContains(t, h, "itlBFromFloor")
			assert.Contains(t, h, c.api)
			assert.Contains(t, h, c.why)
		})
	}

	// A line the replay does not reproduce takes no stated B at all.
	pv.state.Refitted = false
	h := pv.pinnedDefaultBHint()
	assert.NotContains(t, h, "--assume-itl-b")
	assert.Contains(t, h, "no stated B can price it")
}

// Where the floor works, the hint still offers it and names the B it gives.
func TestPinnedDefaultBHint_NamesTheFloorWhereItWorks(t *testing.T) {
	out, _ := runCmd(t, "replicas-to-slo", "--recording", pinnedRecording, "-o", "json")
	r := role(t, decodeOutput(t, out), roleDecode)
	require.NotNil(t, r.Results.Knee.Refused)
	assert.Contains(t, r.Results.Knee.Refused.Detail, "--assume-itl-b floor takes it from the floor below the band (here ")
	assert.Contains(t, r.Results.Knee.Refused.Detail, "or --assume-itl-b with a duration at most the floor's ", "a stated B is capped by the floor")
	assert.NotContains(t, r.Results.Knee.Refused.Detail, "12ms", "no B the floor would refuse")
}

// The self-check names a switch the answer set off its default as run
// without, not one given at its default; --imbalance-across-counts, which
// the fits do price with, is replayed and outside.
func TestSelfCheckStatedSwitches(t *testing.T) {
	f := answerFlags{psOnsets: true, priceRouterWait: false, imbalanceAcrossCounts: !whatif.DefaultPlannerImbalanceAcrossCounts,
		set: map[string]bool{"ps-onsets": true, "price-router-wait": true, "imbalance-across-counts": true}}
	st, err := f.stated()
	require.NoError(t, err)
	assert.Equal(t, []string{"--ps-onsets=true"}, st.Other)
	require.Len(t, st.Replayed, 1)
	assert.Contains(t, st.Replayed[0], "--imbalance-across-counts=")
	assert.True(t, st.Outside)

	// The router's flow control and wait replay, as backtest takes them.
	f = answerFlags{flowControl: "on", routerWaitInPrefill: true, set: map[string]bool{"flow-control": true, "router-wait-in-prefill": true}}
	st, err = f.stated()
	require.NoError(t, err)
	assert.Empty(t, st.Other)
	assert.Equal(t, "on", st.Assume.FlowControl)
	assert.Equal(t, whatifapi.RouterWaitInPrefill, st.Assume.RouterWait)
}
