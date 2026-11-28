package slo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// emptyGateFits has the refit fit a TTFT model whose held-out folds scored no
// step and a TPOT model whose self-check folds scored none, both otherwise
// passing, and records the switches the TTFT fit was asked with.
func emptyGateFits(t *testing.T) *[]whatif.RankMixtureSwitches {
	t.Helper()
	origTTFT, origTPOT := fitMixtureWindow, fitTPOTWindow
	t.Cleanup(func() { fitMixtureWindow, fitTPOTWindow = origTTFT, origTPOT })
	var asked []whatif.RankMixtureSwitches
	fitMixtureWindow = func(_ []whatif.FitCycle, _ time.Duration, _ int, _ float64, sw whatif.RankMixtureSwitches) whatif.MixtureWindowFit {
		asked = append(asked, sw)
		return pricedMixture()
	}
	fitTPOTWindow = func([]whatif.FitCycle, time.Duration, int, int, string, int) whatif.TPOTWindowFit {
		return pricedTPOT()
	}
	return &asked
}

// refitWithAFitInUse observes one cycle on a, installs a passing fit in use
// for both learners at t0, and refits a minute later. It returns the window.
func refitWithAFitInUse(t *testing.T, a *Analyzer, clk *clocktesting.FakePassiveClock, t0 time.Time) *modelWindow {
	t.Helper()
	in, router := modelInputsCycle()
	in.ArrivalRate = 2
	in.Config = ttftPolicy(500 * time.Millisecond)
	fillAllInputs(&in, router)
	a.beginCycle()
	a.ObserveModelInputs(in, router)

	a.mu.Lock()
	w := a.models[reportModel("ns", "m")]
	require.NotNil(t, w)
	in2 := pricedMixture()
	in2.Fitted.M.A0 = 0.05
	w.ttft = ttftLearner{fit: in2, fittedAt: t0, prefill: 1}
	w.tpot = tpotLearner{fit: pricedTPOT(), fittedAt: t0}
	a.mu.Unlock()

	clk.SetTime(t0.Add(time.Minute))
	_, ok := a.RefitModel("ns", "m")
	require.True(t, ok)
	return w
}

// A refit whose held-out folds scored no step: off from the constructor, as
// before, it passes and replaces the fit in use; with
// SwitchV2EmptyGateAbstains it does not pass, and the fit in use is held as
// after any refit that does not pass.
func TestModelLearners_AnEmptyGateAbstainsWithTheSwitch(t *testing.T) {
	emptyGateFits(t)
	t0 := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

	assert.False(t, New(nil, nil).v2EmptyGateAbstains, "off from the constructor")
	assert.Contains(t, ReplaySwitchNames(), SwitchV2EmptyGateAbstains)

	for _, on := range []bool{false, true} {
		clk := clocktesting.NewFakePassiveClock(t0)
		a := New(nil, nil).WithClock(clk).WithModelRefits(ModelRefitEvery, false)
		if on {
			require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchV2EmptyGateAbstains: true}))
		}
		w := refitWithAFitInUse(t, a, clk, t0)
		a.mu.Lock()
		assert.Equal(t, on, w.ttft.held, "switch %v: the TTFT fit in use held", on)
		assert.Equal(t, on, w.tpot.held, "switch %v: the TPOT fit in use held", on)
		wantAt := t0.Add(time.Minute)
		if on {
			wantAt = t0
		}
		assert.Equal(t, wantAt, w.ttft.fittedAt, "switch %v", on)
		assert.Equal(t, wantAt, w.tpot.fittedAt, "switch %v", on)
		a.mu.Unlock()
	}

	// The gates alone: a score with steps is judged by its share whatever
	// the switch.
	assert.True(t, HeldOutPasses(whatif.HeldOutScore{}))
	a := New(nil, nil)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchV2EmptyGateAbstains: true}))
	assert.False(t, a.heldOutPassesFor(whatif.HeldOutScore{}))
	assert.True(t, a.heldOutPassesFor(whatif.HeldOutScore{Within: 5, Steps: 10}))
	assert.False(t, a.heldOutPassesFor(whatif.HeldOutScore{Within: 4, Steps: 10}))
	assert.True(t, tpotFoldsPass(whatif.TPOTDistReport{}, false))
	assert.False(t, tpotFoldsPass(whatif.TPOTDistReport{}, true))
}

// The TTFT learner asks its fit for the folds' own spread only with
// SwitchV2HeldOutFoldSpread, off from the constructor; its other switches
// are the ones it always fitted with.
func TestModelLearners_TheFoldSpreadSwitchReachesTheFit(t *testing.T) {
	assert.False(t, New(nil, nil).v2HeldOutFoldSpread, "off from the constructor")
	assert.Contains(t, ReplaySwitchNames(), SwitchV2HeldOutFoldSpread)
	t0 := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	for _, on := range []bool{false, true} {
		asked := emptyGateFits(t)
		clk := clocktesting.NewFakePassiveClock(t0)
		a := New(nil, nil).WithClock(clk).WithModelRefits(ModelRefitEvery, false)
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchV2HeldOutFoldSpread: on}))
		refitWithAFitInUse(t, a, clk, t0)
		require.NotEmpty(t, *asked)
		want := modelMixtureSwitches
		want.HeldOutFoldSpread = on
		for _, sw := range *asked {
			assert.Equal(t, want, sw, "switch %v", on)
		}
	}
}
