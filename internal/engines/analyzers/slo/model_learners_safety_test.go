package slo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The analyzer keeps at most modelMaxWindows windows: a model over the bound
// reads model-limit on both objectives, keeps no window and asks for
// nothing, even with an input missing, and takes the room a model evicted
// frees.
func TestModelLearners_WindowsAreBounded(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	clk := clocktesting.NewFakePassiveClock(t0)
	a := New(nil, nil).WithClock(clk).WithStateTimeout(time.Hour).WithModelRefits(ModelRefitEvery, false)
	a.modelMaxWindows = 1
	first, router := modelInputsCycle()
	second, _ := modelInputsCycle()
	second.ModelID = "m2"

	a.beginCycle()
	got := a.ObserveModelInputs(first, router)
	assert.NotEqual(t, constants.SLOModelModelLimit, got[whatif.ObjectiveTTFT].Reason)
	got = a.ObserveModelInputs(second, router)
	assert.Equal(t, constants.SLOModelModelLimit, got[whatif.ObjectiveTTFT].Reason)
	assert.Equal(t, constants.SLOModelModelLimit, got[whatif.ObjectiveTPOT].Reason)
	assert.NotEmpty(t, got[whatif.ObjectiveTTFT].Missing, "the missing inputs are still named")
	assert.Len(t, a.models, 1)
	assert.NotContains(t, a.models, reportModel("ns", "m2"))
	_, ok := a.ModelPrefillAsk("ns", "m2")
	assert.False(t, ok)
	assert.Nil(t, a.modelReports("ns", "m2"))

	// The first model leaves; the second takes its room.
	clk.SetTime(t0.Add(2 * time.Hour))
	a.beginCycle()
	assert.Equal(t, 1, a.evictStale(clk.Now()).Models)
	got = a.ObserveModelInputs(second, router)
	assert.NotEqual(t, constants.SLOModelModelLimit, got[whatif.ObjectiveTTFT].Reason)
	assert.Contains(t, a.models, reportModel("ns", "m2"))
}

// A panic in a fit core fails the refit, not the controller: the refit
// runs to its end, the fits in use are held as after any refit that does not
// pass, the background refit's flag is cleared, and the analyzer's lock is
// free.
func TestModelLearners_APanickingRefitIsHeld(t *testing.T) {
	orig := fitMixtureWindow
	t.Cleanup(func() { fitMixtureWindow = orig })
	fitMixtureWindow = func([]whatif.FitCycle, time.Duration, int, float64, whatif.RankMixtureSwitches) whatif.MixtureWindowFit {
		panic("a fit core's bug")
	}

	t0 := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	clk := clocktesting.NewFakePassiveClock(t0)
	a := New(nil, nil).WithClock(clk)
	in, router := modelInputsCycle()
	in.ArrivalRate = 2
	in.Config = ttftPolicy(500 * time.Millisecond)
	fillAllInputs(&in, router)
	a.beginCycle()
	a.ObserveModelInputs(in, router)
	waitNoRefit(t, a)

	a.mu.Lock()
	w := a.models[reportModel("ns", "m")]
	w.ttft = ttftLearner{fit: pricedMixture(), fittedAt: t0, prefill: 1}
	w.refitAt = time.Time{}
	a.mu.Unlock()

	clk.SetTime(t0.Add(time.Minute))
	a.beginCycle()
	a.ObserveModelInputs(in, router)
	waitNoRefit(t, a)
	a.mu.Lock()
	assert.True(t, w.ttft.held, "the fit in use is held")
	assert.Contains(t, w.ttft.last.Abstained(), "the refit failed")
	assert.False(t, w.fitting)
	a.mu.Unlock()

	clk.SetTime(t0.Add(2 * time.Minute))
	a.beginCycle()
	got := a.ObserveModelInputs(in, router)
	assert.Equal(t, constants.SLOModelPriced, got[whatif.ObjectiveTTFT].Reason, "the held fit still prices")
}

// waitNoRefit waits for a's background refit to finish.
func waitNoRefit(t *testing.T, a *Analyzer) {
	t.Helper()
	require.Eventually(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.modelRefitting
	}, 10*time.Second, 5*time.Millisecond)
}

// The window keeps its own copy of the router's reading: a background refit
// reads it while the caller is free to reuse or change its own.
func TestModelLearners_WindowOwnsTheRouterReading(t *testing.T) {
	a := New(nil, nil).WithModelRefits(ModelRefitEvery, false)
	in, router := modelInputsCycle()
	fillAllInputs(&in, router)
	a.beginCycle()
	a.ObserveModelInputs(in, router)
	router.TTFTBuckets[0].Rate = 99
	router.TPOTBuckets[0].Rate = 99
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := a.models[reportModel("ns", "m")].cycles[0].Router
	require.NotNil(t, kept)
	assert.NotSame(t, router, kept)
	assert.Equal(t, 2.0, kept.TTFTBuckets[0].Rate)
	assert.Equal(t, 2.0, kept.TPOTBuckets[0].Rate)
}
