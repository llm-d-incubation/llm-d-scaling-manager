package slo

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// pricedMixture is a rank mixture that prices: two ranks sharing the load,
// a service line, and a pooled prompt of 512 to 2048 tokens.
func pricedMixture() whatif.MixtureWindowFit {
	hist := map[float64]float64{512: 1, 1024: 2, 2048: 3, math.Inf(1): 3}
	m := whatif.RankMixture{S0: 0.02, V: 40000, Shares: []float64{0.5, 0.5}, A0: 0.03, Beta: 2e-5, Spread: 1, Slots: 4}
	return whatif.MixtureWindowFit{Fitted: &whatif.RankMixtureFitted{M: m, Report: whatif.RankMixtureReport{Slots: 4}},
		Pooled: whatif.MixtureStep{Computed: hist, Prompt: hist}, Top: 10}
}

// pricedTPOT is a TPOT distribution that prices.
func pricedTPOT() whatif.TPOTWindowFit {
	return whatif.TPOTWindowFit{Fitted: &whatif.TPOTDistFitted{C: 0.5}, Dist: whatif.TPOTDist{GroupCV2: 0.01, C: 0.5}, Inv: 0.01, Top: 5}
}

// ttftPolicy is a policy with a TTFT objective of target at the p90.
func ttftPolicy(target time.Duration) *config.ScalingPolicy {
	return &config.ScalingPolicy{SLO: &config.SLOPolicy{TTFT: &config.SLOObjective{Target: config.SLODuration{Duration: target}, Percentile: 90}}}
}

// The two v2 learners are on the forget list and report under their own
// names, one per objective.
func TestModelLearners_AreOnTheForgetList(t *testing.T) {
	require.Len(t, modelLearners, 2)
	assert.Equal(t, learn.TTFTMixture, modelLearners[0].name)
	assert.Equal(t, learn.TPOTDistribution, modelLearners[1].name)
	for _, l := range modelLearners {
		assert.Contains(t, learn.Learners(), l.name)
		assert.Contains(t, learn.Reporters(), l.name)
		_, _, handled := New(nil, nil).forgetLearnerLocked(l.name, "ns", "m", "v")
		assert.True(t, handled, l.name)
	}
}

// The prefill ask is searched outward from the serving pods; it is the
// least count a linear search from one finds, at every rate, and none where
// even maxPrefillAskFactor times the pods miss the target.
func TestModelPrefillAsk_IsTheLeastCountThatMeetsTheTarget(t *testing.T) {
	a := New(nil, nil)
	tt := &ttftLearner{fit: pricedMixture(), prefill: 2}
	for _, rate := range []float64{1, 5, 20, 60} {
		for _, target := range []float64{0.08, 0.15, 0.4} {
			linear, linearOK := 0, false
			for n := 1; n <= maxPrefillAskFactor*2; n++ {
				_, v, ok := tt.fit.Price(rate, 2/float64(n), 0.9)
				if ok && v[0] <= target {
					linear, linearOK = n, true
					break
				}
			}
			atPods := math.Inf(1)
			if _, at, okAt := tt.fit.Price(rate, 1, 0.9); okAt {
				atPods = at[0]
			}
			got, ok := a.modelPrefillAsk(tt, rate, 2, 0.9, target, atPods)
			assert.Equal(t, linearOK, ok, "rate %g target %g", rate, target)
			assert.Equal(t, linear, got, "rate %g target %g", rate, target)
		}
	}
}

// One set of draws prices every statistic: the mean and each quantile equal
// the model's own Price, call by call.
func TestMixtureWindowFit_PriceIsTheModelsPrice(t *testing.T) {
	w := pricedMixture()
	mean, qs, ok := w.Price(6, 1, 0.9, 0.95)
	require.True(t, ok)
	m := w.Fitted.M
	for i, q := range []float64{0, 0.9, 0.95} {
		v, _, u := m.Price(6, w.Pooled.Computed, w.Pooled.Prompt, q)
		require.Nil(t, u)
		want := mean
		if i > 0 {
			want = qs[i-1]
		}
		assert.Equal(t, v, want, "q %g", q)
	}
}

// A cycle whose inputs are all read reads window-short until the window
// spans what each model fits on; a fit in use prices, a refit that no
// longer passes holds it for ModelHoldFor and then drops it, and the
// reports say which.
func TestModelLearners_WindowShortThenPricedThenHeldThenDropped(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	clk := clocktesting.NewFakePassiveClock(t0)
	a := New(nil, nil).WithClock(clk).WithModelRefits(ModelRefitEvery, false)
	in, router := modelInputsCycle()
	in.ArrivalRate = 2
	in.Config = ttftPolicy(500 * time.Millisecond)
	fillAllInputs(&in, router)

	a.beginCycle()
	got := a.ObserveModelInputs(in, router)
	assert.Equal(t, constants.SLOModelWindowShort, got[whatif.ObjectiveTTFT].Reason)
	assert.Equal(t, constants.SLOModelWindowShort, got[whatif.ObjectiveTPOT].Reason)

	// A fit that passed, installed as a refit would.
	a.mu.Lock()
	w := a.models[reportModel("ns", "m")]
	w.ttft = ttftLearner{fit: pricedMixture(), fittedAt: t0, prefill: 1}
	w.tpot = tpotLearner{fit: pricedTPOT(), fittedAt: t0, defaultRatio: [2]float64{1.5, 1.8}}
	a.mu.Unlock()
	clk.SetTime(t0.Add(time.Minute))
	a.beginCycle()
	got = a.ObserveModelInputs(in, router)
	assert.Equal(t, constants.SLOModelPriced, got[whatif.ObjectiveTTFT].Reason)
	assert.Equal(t, constants.SLOModelPriced, got[whatif.ObjectiveTPOT].Reason)
	ask, ok := a.ModelPrefillAsk("ns", "m")
	require.True(t, ok)
	assert.Equal(t, "p1", ask.Variant)
	assert.GreaterOrEqual(t, ask.Pods, 1)
	reports := a.modelReports("ns", "m")
	require.Len(t, reports, 2)
	assert.Equal(t, modelSourceFitted, reports[0].Source)

	// The refit due (the first was at t0) fails on a two-cycle window: the
	// fits are held.
	clk.SetTime(t0.Add(ModelRefitEvery + time.Minute))
	a.beginCycle()
	got = a.ObserveModelInputs(in, router)
	assert.Equal(t, constants.SLOModelPriced, got[whatif.ObjectiveTTFT].Reason)
	reports = a.modelReports("ns", "m")
	assert.Equal(t, modelSourceHeld, reports[0].Source)
	assert.Equal(t, modelSourceHeld, reports[1].Source)

	// Past the hold, the next refit drops them.
	clk.SetTime(t0.Add(ModelHoldFor + 2*ModelRefitEvery))
	a.beginCycle()
	got = a.ObserveModelInputs(in, router)
	assert.NotEqual(t, constants.SLOModelPriced, got[whatif.ObjectiveTTFT].Reason)
	assert.NotEqual(t, constants.SLOModelPriced, got[whatif.ObjectiveTPOT].Reason)
	_, ok = a.ModelPrefillAsk("ns", "m")
	assert.False(t, ok)
}

// The window is forgotten on the forget annotation through either learner's
// name, by its prefill or its decode variant, not by another; and when the
// model's variant pair changes, with a forgotten report.
func TestModelLearners_Forget(t *testing.T) {
	a := New(nil, nil)
	in, router := modelInputsCycle()
	a.beginCycle()
	a.ObserveModelInputs(in, router)
	n, forgotten := a.forgetVariant("ns", "m", "other")
	assert.Zero(t, n)
	assert.Empty(t, forgotten)
	assert.Contains(t, a.models, reportModel("ns", "m"))
	a.TakeLearnerReports()
	n, _ = a.forgetVariant("ns", "m", "d1")
	assert.Equal(t, 1, n)
	assert.NotContains(t, a.models, reportModel("ns", "m"))
	assert.Equal(t, map[string]learn.By{learn.TTFTMixture: learn.ByOperator, learn.TPOTDistribution: learn.ByOperator},
		forgottenBy(a.TakeLearnerReports()))

	a.ObserveModelInputs(in, router)
	in.VariantStates[1].VariantName = "d2"
	in.ReplicaMetrics[1].VariantName = "d2"
	a.TakeLearnerReports()
	a.ObserveModelInputs(in, router)
	assert.Equal(t, map[string]learn.By{learn.TTFTMixture: learn.ByArguments, learn.TPOTDistribution: learn.ByArguments},
		forgottenBy(a.TakeLearnerReports()))
	assert.Equal(t, "d2", a.models[reportModel("ns", "m")].decode)
}

// The window is evicted when the model goes unseen past the state timeout.
func TestModelLearners_EvictedPastTheStateTimeout(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	clk := clocktesting.NewFakePassiveClock(t0)
	a := New(nil, nil).WithClock(clk).WithStateTimeout(time.Hour)
	in, router := modelInputsCycle()
	a.ObserveModelInputs(in, router)
	assert.Zero(t, a.evictStale(t0.Add(time.Hour)).Models)
	assert.Equal(t, 1, a.evictStale(t0.Add(time.Hour+time.Second)).Models)
	assert.Empty(t, a.models)
}

// A model observed this cycle publishes both models' figures side by side:
// v2 TTFT by statistic, the default TPOT mean at the line's current fill
// and its percentiles at the default R_q, and the v2 TPOT percentiles at the
// distribution's ratio around the same mean. A model not observed this
// cycle (a replay) publishes none.
func TestModelPredictions_SideBySide(t *testing.T) {
	a := New(nil, nil)
	in, router := modelInputsCycle()
	in.ArrivalRate = 2
	in.ReplicaMetrics[1].KvUsageInstant, in.ReplicaMetrics[1].KvUsageInstantKnown = 0.5, true
	lines := map[string]itl.Model{"d1": {A: 0.02, B: 0.01}}
	assert.Nil(t, a.modelPredictions(in, lines), "never observed")

	a.beginCycle()
	a.ObserveModelInputs(in, router)
	a.mu.Lock()
	w := a.models[reportModel("ns", "m")]
	w.now.ttft = map[string]float64{constants.SLOStatisticMean: 0.1, constants.SLOStatisticP90: 0.2, constants.SLOStatisticP95: 0.3}
	w.tpot = tpotLearner{fit: pricedTPOT(), fittedAt: time.Now(), defaultRatio: [2]float64{1.5, 1.8}}
	a.mu.Unlock()
	got := a.modelPredictions(in, lines)
	byKey := map[[3]string]float64{}
	for _, p := range got {
		byKey[[3]string{p.Objective + "/" + p.Variant, p.Statistic, p.Learner}] = p.Seconds
	}
	mean := 0.02*0.5 + 0.01
	v2 := pricedTPOT()
	assert.Equal(t, map[[3]string]float64{
		{"ttft/p1", "mean", learn.TTFTMixture}:     0.1,
		{"ttft/p1", "p90", learn.TTFTMixture}:      0.2,
		{"ttft/p1", "p95", learn.TTFTMixture}:      0.3,
		{"tpot/d1", "mean", learn.SLO}:             mean,
		{"tpot/d1", "p90", learn.SLO}:              mean * 1.5,
		{"tpot/d1", "p95", learn.SLO}:              mean * 1.8,
		{"tpot/d1", "p90", learn.TPOTDistribution}: mean * v2.Ratio(0.9),
		{"tpot/d1", "p95", learn.TPOTDistribution}: mean * v2.Ratio(0.95),
	}, byKey)

	a.beginCycle()
	assert.Nil(t, a.modelPredictions(in, lines), "not observed on this cycle")
}

// forgottenBy is what forgot each learner's keys in reports.
func forgottenBy(reports map[string][]learn.Report) map[string]learn.By {
	out := map[string]learn.By{}
	for _, reps := range reports {
		for _, r := range reps {
			for _, f := range r.Forgotten {
				out[r.Learner] = f.By
			}
		}
	}
	return out
}

// fillAllInputs gives the cycle every input the v2 models read.
func fillAllInputs(in *domain.AnalyzerInput, router *domain.RouterLatency) {
	p := &in.ReplicaMetrics[0]
	p.AvgPrefillTime, p.AvgPrefillTimeKnown, p.AvgQueueTime, p.AvgQueueTimeKnown = 0.2, true, 0.01, true
	p.PromptTokensBuckets = []domain.LengthBucket{{UpperBound: 1000, Rate: 1}}
	p.QueueTimeBuckets = []domain.LatencyBucket{{UpperBound: 0.1, Rate: 1}}
	router.TPOTBuckets = []domain.LatencyBucket{{UpperBound: 0.05, Rate: 2}}
}
