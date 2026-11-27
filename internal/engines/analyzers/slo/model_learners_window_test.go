package slo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// A window keeps at most one cycle every ModelCycleSpacing: at a 5 s cycle
// it keeps one in three, at the default 15 s every one, through a second of
// jitter. Its cap holds a whole ModelWindow at that spacing, so the TTFT
// model's 12 h calibration window is reachable at any cycle interval.
func TestModelLearners_WindowKeepsOneCycleEverySpacing(t *testing.T) {
	assert.GreaterOrEqual(t, time.Duration(modelMaxCycles-1)*ModelCycleSpacing, ModelWindow,
		"the cap must hold the window at the spacing")
	assert.Greater(t, time.Duration(modelMaxCycles-1)*ModelCycleSpacing, whatif.MinSpreadFitWindow)

	t0 := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	kept := func(offsets ...time.Duration) []time.Duration {
		clk := clocktesting.NewFakePassiveClock(t0)
		a := New(nil, nil).WithClock(clk).WithModelRefits(365*24*time.Hour, false)
		in, router := modelInputsCycle()
		for _, o := range offsets {
			clk.SetTime(t0.Add(o))
			a.beginCycle()
			a.ObserveModelInputs(in, router)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		cycles := a.models[reportModel("ns", "m")].cycles
		out := make([]time.Duration, 0, len(cycles))
		for _, c := range cycles {
			out = append(out, c.At.Sub(t0))
		}
		return out
	}
	s := time.Second
	assert.Equal(t, []time.Duration{0, 15 * s, 30 * s}, kept(0, 5*s, 10*s, 15*s, 20*s, 25*s, 30*s), "5 s cycles: one in three")
	assert.Equal(t, []time.Duration{0, 14 * s, 30 * s, 44 * s}, kept(0, 14*s, 30*s, 44*s), "15 s cycles with jitter: every one")
}

// A model the v2 models stop running for (it turned active, or its policy
// dropped the slo analyzer) no longer observes, and after ModelReleaseAfter
// its window goes to a model waiting for one, filed as evicted; before that
// the model waiting reads model-limit.
func TestModelLearners_UnobservedWindowIsReleasedToAWaitingModel(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	clk := clocktesting.NewFakePassiveClock(t0)
	a := New(nil, nil).WithClock(clk)
	a.modelMaxWindows = 1
	first, router := modelInputsCycle()
	second, _ := modelInputsCycle()
	second.ModelID = "m2"

	a.ObserveModelInputs(first, router)
	clk.SetTime(t0.Add(ModelReleaseAfter))
	got := a.ObserveModelInputs(second, router)
	assert.Equal(t, constants.SLOModelModelLimit, got[whatif.ObjectiveTTFT].Reason, "an hour unobserved: still held")
	a.TakeLearnerReports()

	clk.SetTime(t0.Add(ModelReleaseAfter + time.Second))
	got = a.ObserveModelInputs(second, router)
	assert.NotEqual(t, constants.SLOModelModelLimit, got[whatif.ObjectiveTTFT].Reason)
	assert.NotContains(t, a.models, reportModel("ns", "m"))
	assert.Contains(t, a.models, reportModel("ns", "m2"))
	assert.Equal(t, map[string]learn.By{learn.TTFTMixture: learn.ByEvicted, learn.TPOTDistribution: learn.ByEvicted},
		forgottenBy(a.TakeLearnerReports()))
}

// A cycle whose variant pair or engine argument tags do not read keeps the
// window: it is neither kept nor priced, reads signal-missing, and the next
// cycle that reads them goes on with the same window, nothing forgotten.
func TestModelLearners_UnreadPairKeepsTheWindow(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	clk := clocktesting.NewFakePassiveClock(t0)
	a := New(nil, nil).WithClock(clk).WithModelRefits(365*24*time.Hour, false)
	in, router := modelInputsCycle()
	a.beginCycle()
	a.ObserveModelInputs(in, router)
	a.TakeLearnerReports()

	unread := in
	unread.VariantStates = nil
	clk.SetTime(t0.Add(time.Minute))
	a.beginCycle()
	got := a.ObserveModelInputs(unread, router)
	assert.Equal(t, constants.SLOModelSignalMissing, got[whatif.ObjectiveTTFT].Reason)
	assert.Equal(t, constants.SLOModelSignalMissing, got[whatif.ObjectiveTPOT].Reason)
	a.mu.Lock()
	w := a.models[reportModel("ns", "m")]
	require.NotNil(t, w)
	assert.Equal(t, "p1", w.prefill)
	assert.Len(t, w.cycles, 1, "the unread cycle is not kept")
	a.mu.Unlock()

	clk.SetTime(t0.Add(2 * time.Minute))
	a.beginCycle()
	a.ObserveModelInputs(in, router)
	assert.Empty(t, forgottenBy(a.TakeLearnerReports()), "nothing forgotten")
	a.mu.Lock()
	assert.Len(t, a.models[reportModel("ns", "m")].cycles, 2)
	a.mu.Unlock()

	// Unread for longer than ModelReleaseAfter (the model moved to several
	// variants of a role): the window starts again, forgotten.
	start := t0.Add(3 * time.Minute)
	for _, o := range []time.Duration{0, ModelReleaseAfter, ModelReleaseAfter + time.Minute} {
		clk.SetTime(start.Add(o))
		a.beginCycle()
		a.ObserveModelInputs(unread, router)
	}
	assert.Equal(t, map[string]learn.By{learn.TTFTMixture: learn.ByArguments, learn.TPOTDistribution: learn.ByArguments},
		forgottenBy(a.TakeLearnerReports()))
	a.mu.Lock()
	assert.Empty(t, a.models[reportModel("ns", "m")].prefill, "the window reads the pair as it now is")
	a.mu.Unlock()
}

// The v2 TTFT is priced, and published, at the objective's percentile too,
// under the statistic the objective's own reading names: a p99 objective
// gets a p99 v2 figure beside the mean, p90 and p95.
func TestModelPricing_PublishesTheObjectivesPercentile(t *testing.T) {
	a := New(nil, nil)
	tt := &ttftLearner{fit: pricedMixture(), prefill: 2}
	p := a.priceModel(tt, 5, 2, 0.99, 10)
	require.True(t, p.ok)
	_, want, ok := tt.fit.Price(5, 1, 0.9, 0.95, 0.99)
	require.True(t, ok)
	assert.Equal(t, want[2], p.ttft["p99"])
	assert.Equal(t, want[0], p.ttft[constants.SLOStatisticP90])
	assert.Len(t, p.ttft, 4)
	assert.Len(t, a.priceModel(tt, 5, 2, 0.9, 10).ttft, 3, "a p90 objective is the p90 figure")
	assert.Equal(t, "p99", percentileStatistic(0.99))
	assert.Equal(t, "p90", percentileStatistic(float64(90)/100))
}
