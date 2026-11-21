package whatifapi

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// btT0 is the synthetic window's first cycle.
var btT0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

// btCycle is the synthetic window's cycle.
const btCycle = 15 * time.Second

// btRate is the synthetic window's rate at cycle i: up from 2 r/s by 0.05 a
// cycle, then back to 2.
func btRate(i int) float64 { return 2 + 0.05*float64(i%240) }

// btRouter is a router histogram for n cycles of rate r: half the requests
// within scale, nine tenths within 2 x scale, every one within 4 x scale,
// and the mean at 1.2 x scale.
func btRouter(r, scale float64) *domain.RouterLatency {
	b := []domain.LatencyBucket{{UpperBound: scale, Rate: 0.5 * r}, {UpperBound: 2 * scale, Rate: 0.9 * r},
		{UpperBound: 4 * scale, Rate: r}, {UpperBound: math.Inf(1), Rate: r}}
	return &domain.RouterLatency{WindowSeconds: 300, TTFTBuckets: b, TTFTSumRate: 1.2 * scale * r, TTFTCountRate: r,
		TPOTBuckets: []domain.LatencyBucket{{UpperBound: scale / 10, Rate: 0.5 * r}, {UpperBound: scale / 5, Rate: 0.9 * r},
			{UpperBound: math.Inf(1), Rate: r}}, TPOTSumRate: 0.012 * r, TPOTCountRate: r}
}

// btSeries is n synthetic cycles of 2 prefill and 2 decode pods, 1000
// tokens in and 100 out, the decode pods 3 from cycle scaleAt on (never
// with scaleAt 0), with the router's latency scaled by latencyX.
func btSeries(n, scaleAt int, latencyX float64) ([]SeriesSample, []LatencySample) {
	s := make([]SeriesSample, n)
	l := make([]LatencySample, n)
	for i := range n {
		at := btT0.Add(time.Duration(i) * btCycle)
		d := 2
		if scaleAt > 0 && i >= scaleAt {
			d = 3
		}
		r := btRate(i)
		s[i] = SeriesSample{At: at, Rate: r, RateKnown: true, Prefill: 2, Decode: d, PromptTokens: 1000, OutputTokens: 100, HasShape: true}
		l[i] = LatencySample{At: at, Router: btRouter(r, 0.1*latencyX)}
	}
	return s, l
}

// btEarlier is s and l a day earlier: a fit that does not overlap a test of
// the same synthetic window.
func btEarlier(s []SeriesSample, l []LatencySample) ([]SeriesSample, []LatencySample) {
	for i := range s {
		s[i].At = s[i].At.Add(-24 * time.Hour)
		l[i].At = l[i].At.Add(-24 * time.Hour)
	}
	return s, l
}

// btConfig is a run's configuration, with the decode accelerator given.
func btConfig(accelerator string) RunConfig {
	return RunConfig{Fields: []ConfigField{{Name: "model", Value: "model-01"}, {Name: "decode accelerator", Value: accelerator},
		{Name: "flow-control series", Value: "absent (off)"}}}
}

// btEntry is an inputs entry of the stub's model over series and latency.
func btEntry(t *testing.T, r *stubResolver, id string, s []SeriesSample, l []LatencySample, c RunConfig) *inputsEntry {
	t.Helper()
	res, err := r.Resolve(context.Background(), InputsRequest{})
	require.NoError(t, err)
	return &inputsEntry{id: id, doc: res.Inputs, pricer: res.Pricer, series: s, latency: l, config: c}
}

// btRun is a backtest of fit against test.
func btRun(t *testing.T, fit, test *inputsEntry, req BacktestRequest) *BacktestResponse {
	t.Helper()
	q, err := backtestQuestionOf(fit, test, req, time.Time{}, "")
	require.NoError(t, err)
	out, err := q.run(context.Background())
	require.NoError(t, err)
	return out
}

// predictedOf is every step's predicted figures, as JSON.
func predictedOf(t *testing.T, out *BacktestResponse) string {
	t.Helper()
	p := make([]any, 0, 18*len(out.Steps))
	for _, s := range out.Steps {
		for _, x := range append(append([]BacktestPoint{}, s.TTFT...), s.TPOT...) {
			p = append(p, x.Statistic, x.Predicted, x.Bound)
		}
		p = append(p, s.Excluded, s.KneeShare)
	}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	return string(b)
}

// The prediction is the fit's and the test's inputs alone: the test's
// latency, tripled, moves every measured figure and no predicted one.
func TestBacktest_MeasuredLatencyDoesNotReachThePrediction(t *testing.T) {
	r := &stubResolver{}
	var mu sync.Mutex
	var knobs []Knobs
	r.onFleet = func(k Knobs) { mu.Lock(); knobs = append(knobs, k); mu.Unlock() }
	fitS, fitL := btEarlier(btSeries(240, 0, 1))
	fit := btEntry(t, r, "fit", fitS, fitL, btConfig("H100"))
	s1, l1 := btSeries(240, 0, 1)
	s3, l3 := btSeries(240, 0, 3)
	a := btRun(t, fit, btEntry(t, r, "a", s1, l1, btConfig("H100")), BacktestRequest{})
	knobsA := append([]Knobs{}, knobs...)
	knobs = nil
	b := btRun(t, fit, btEntry(t, r, "b", s3, l3, btConfig("H100")), BacktestRequest{})
	require.NotEmpty(t, knobsA, "fleets were priced")
	assert.Equal(t, knobsA, knobs, "the fleets priced read no latency")
	assert.Equal(t, predictedOf(t, a), predictedOf(t, b), "a measured latency reached a prediction")
	moved := 0
	for i := range a.Steps {
		for j := range a.Steps[i].TTFT {
			x, y := a.Steps[i].TTFT[j].Measured, b.Steps[i].TTFT[j].Measured
			if x != nil && y != nil {
				assert.InDelta(t, 3**x, *y, 1e-9, "step %d %s", i, a.Steps[i].TTFT[j].Statistic)
				moved++
			}
		}
	}
	assert.Greater(t, moved, 20, "too few measured figures compared")
}

// A test that ran another configuration is refused, naming the field; asked
// to, it is priced with the difference as a caveat.
func TestBacktest_RefusesAnotherConfiguration(t *testing.T) {
	r := &stubResolver{}
	s, l := btSeries(120, 0, 1)
	fs, fl := btEarlier(btSeries(120, 0, 1))
	fit := btEntry(t, r, "fit", fs, fl, btConfig("H100"))
	test := btEntry(t, r, "test", s, l, btConfig("H200"))
	_, err := backtestQuestionOf(fit, test, BacktestRequest{}, time.Time{}, "")
	require.Error(t, err)
	assert.True(t, IsInputError(err))
	assert.Contains(t, err.Error(), "decode accelerator is H100 in the fit and H200 in the test")
	assert.Contains(t, err.Error(), "allowConfigMismatch")

	out := btRun(t, fit, test, BacktestRequest{AllowConfigMismatch: true})
	assert.False(t, out.Config.Match)
	assert.True(t, out.Config.Allowed)
	assert.Equal(t, []ConfigMismatch{{Field: "decode accelerator", Fit: "H100", Test: "H200"}}, out.Config.Differences)
	assert.Contains(t, strings.Join(out.Notes, " "), "CONFIGURATION DIFFERS")

	// A field one side does not record is not compared, and is no mismatch.
	unrecorded := btEntry(t, r, "u", s, l, btConfig(""))
	q, err := backtestQuestionOf(fit, unrecorded, BacktestRequest{}, time.Time{}, "")
	require.NoError(t, err)
	assert.True(t, q.config.Match)
	assert.Contains(t, q.config.NotCompared, "decode accelerator (not read on both sides)")
	assert.Contains(t, q.config.NotCompared[0], "router's version")

	// The test is the fit's own source, or overlaps it in time: refused.
	_, err = backtestQuestionOf(fit, fit, BacktestRequest{}, time.Time{}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the fit's own source")
	_, err = backtestQuestionOf(btEntry(t, r, "other", s, l, btConfig("H100")), test, BacktestRequest{AllowConfigMismatch: true}, time.Time{}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not end before the test starts")
}

// The test's first minutes, a step near a change of replicas and a step
// with no traffic are not judged, each counted by its reason.
func TestBacktest_ExcludesWarmUpScalingAndIdle(t *testing.T) {
	r := &stubResolver{}
	fitS, fitL := btEarlier(btSeries(240, 0, 1))
	s, l := btSeries(240, 160, 1) // the decode pods go to 3 at 10:40
	for i := 200; i < 220; i++ {  // 10:50 to 10:55 is idle
		s[i].Rate = 0
	}
	out := btRun(t, btEntry(t, r, "fit", fitS, fitL, btConfig("H100")), btEntry(t, r, "t", s, l, btConfig("H100")), BacktestRequest{})
	reason := map[string]string{}
	for _, st := range out.Steps {
		reason[st.Start.Format("15:04")] = st.Excluded
	}
	assert.Equal(t, ExcludedWarmUp, reason["10:00"])
	assert.Equal(t, "", reason["10:05"])
	assert.Equal(t, ExcludedScaling, reason["10:40"], "the replicas changed in the step")
	assert.Equal(t, "", reason["10:45"], "the change is more than the settle before")
	assert.Equal(t, ExcludedNoTraffic, reason["10:50"])
	e := out.Summary.Excluded
	assert.Equal(t, BacktestExcluded{WarmUp: 1, Scaling: 1, NoTraffic: 1}, e)
	assert.Equal(t, 12, out.Summary.Steps)
	assert.Equal(t, 9, out.Summary.Judged)
	assert.Contains(t, out.Excluded, "Judged 9 of 12 five-minute steps; not judged: 1 warm-up, 1 replicas changing, 1 no traffic.")
}

// The measured figures are the histogram's: the median at its first
// bound, the p90 at its second, the mean from its sum and count; a ratio
// is predicted over measured, and the summary scores it.
func TestBacktest_ScoresAgainstTheHistogram(t *testing.T) {
	r := &stubResolver{}
	s, l := btSeries(240, 0, 1)
	fs, fl := btEarlier(btSeries(240, 0, 1))
	out := btRun(t, btEntry(t, r, "fit", fs, fl, btConfig("H100")), btEntry(t, r, "t", s, l, btConfig("H100")), BacktestRequest{})
	st := out.Steps[3]
	require.Empty(t, st.Excluded)
	want := map[string]float64{"mean": 0.12, "p50": 0.1, "p90": 0.2, "p95": 0.3}
	for _, p := range st.TTFT {
		require.NotNil(t, p.Measured, p.Statistic)
		assert.InDelta(t, want[p.Statistic], *p.Measured, 1e-9, p.Statistic)
		if p.Ratio != nil {
			// Outside a bucket too wide to place the figure, the ratio is
			// over the bucket's nearer edge.
			ref := *p.Measured
			switch p.OutsideBucket {
			case OutsideUnder:
				ref = p.MeasuredBucket.Lo
			case OutsideOver:
				ref = p.MeasuredBucket.Hi
			}
			assert.InDelta(t, *p.Predicted.Value/ref, *p.Ratio, 1e-12, p.Statistic)
			require.NotNil(t, p.Bound)
			assert.InDelta(t, 1.1**p.Predicted.Value, *p.Bound, 1e-12, "the bound is the top of the span")
		}
	}
	// The stub prices no TPOT percentile: refused, and counted so.
	for _, sc := range out.Summary.TPOT {
		if sc.Statistic != "mean" {
			assert.Zero(t, sc.All.Steps, sc.Statistic)
			assert.Positive(t, sc.Refused["NoTPOTHistogram"], sc.Statistic)
		}
	}
	mean := out.Summary.TTFT[0]
	require.Equal(t, "mean", mean.Statistic)
	assert.Equal(t, out.Summary.Judged, mean.All.Steps+mean.Unscored[UnscoredRefused])
	require.NotNil(t, mean.All.Bias)
	assert.Zero(t, mean.InRegion.Steps, "the stub's class is in no region")
	assert.Contains(t, out.Verdict, "the planner did not see")
	assert.Contains(t, out.Verdict, "TTFT: within 20% of measured in")
	assert.Equal(t, out.Verdict, strings.Join(out.VerdictLines, " "))
	// Every step is banded by the stub's knee, 20 r/s at 1000 tokens, a
	// little less at the prompt on the shape grid.
	for _, st := range out.Steps {
		if st.KneeShare != nil {
			assert.InEpsilon(t, st.Rate/20, *st.KneeShare, 0.05)
			assert.Equal(t, loadBand(*st.KneeShare), st.Load)
		}
	}
}

// btResolver is the stub's resolver with a window: every source carries
// the synthetic series and latency, and it remembers the cycles asked.
type btResolver struct {
	stubResolver
	mu     sync.Mutex
	cycles []int
	// folds, when set, reads the self-check's folds where asked, and
	// lastSelfCheck is what the last request asked.
	folds         bool
	lastSelfCheck *bool
	// n is the window's cycles; 0 is 240.
	n int
}

func (r *btResolver) Resolve(ctx context.Context, req InputsRequest) (*Resolved, error) {
	res, err := r.stubResolver.Resolve(ctx, req)
	if err != nil {
		return nil, err
	}
	cycle := 0
	if req.Source != nil {
		cycle = req.Source.Cycle
	}
	r.mu.Lock()
	r.cycles = append(r.cycles, cycle)
	r.mu.Unlock()
	res.Series, res.Latency = btSeries(max(r.n, 240), 0, 1)
	res.Config = btConfig("H100")
	r.mu.Lock()
	r.lastSelfCheck = req.SelfCheck
	r.mu.Unlock()
	if r.folds && req.SelfCheck != nil && *req.SelfCheck {
		for _, c := range SelfCheckCycles(len(res.Series)) {
			res.Folds = append(res.Folds, FoldFit{Cycle: c, At: res.Series[c-1].At, Pricer: &r.stubResolver})
		}
	}
	res.Content += " cycle " + time.Duration(cycle).String()
	res.Inputs.Source.Cycle = cycle
	return res, nil
}

// POST /v1/backtest splits one source: the fit is the source read at the
// split's cycle, through the resolver, and the test every cycle after it.
func TestBacktest_SplitsOneSourceOverHTTP(t *testing.T) {
	r := &btResolver{}
	h := New(Config{Resolver: r, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	code, body := post(t, h, "/v1/backtest", BacktestRequest{InputsID: id, Split: "50%", Step: "5m"})
	require.Equal(t, http.StatusOK, code, string(body))
	var out BacktestResponse
	require.NoError(t, json.Unmarshal(body, &out))
	assert.Equal(t, []int{0, 120}, r.cycles, "the fit is read at the split's cycle")
	assert.Equal(t, "50%", out.Split)
	assert.Equal(t, btT0.Add(119*btCycle), out.Fit.To)
	assert.Equal(t, btT0.Add(120*btCycle), out.Test.From, "the test starts after the split")
	assert.Equal(t, 120, out.Test.Cycles)
	assert.Equal(t, SchemaVersion, out.SchemaVersion)
	assert.NotEmpty(t, out.BacktestID)

	for name, c := range map[string]struct {
		req  BacktestRequest
		want string
	}{
		"split and test":   {BacktestRequest{InputsID: id, TestInputsID: id, Split: "50%"}, "without testInputsId"},
		"split too large":  {BacktestRequest{InputsID: id, Split: "100%"}, "between 0% and 100%"},
		"split before":     {BacktestRequest{InputsID: id, FitUntil: "2026-11-20T09:00:00Z"}, "before the source's first cycle"},
		"split after":      {BacktestRequest{InputsID: id, FitUntil: "2026-11-20T12:00:00Z"}, "nothing to test on"},
		"bad step":         {BacktestRequest{InputsID: id, Step: "10s"}, "step"},
		"bad measured":     {BacktestRequest{InputsID: id, Measured: "client"}, "measured"},
		"bad target":       {BacktestRequest{InputsID: id, TTFT: &Objective{Statistic: "p90"}}, "ttft.target"},
		"unknown test":     {BacktestRequest{InputsID: id, TestInputsID: "nope"}, "unknown"},
		"both split forms": {BacktestRequest{InputsID: id, Split: "50%", FitUntil: "2026-11-20T10:30:00Z"}, "give one"},
	} {
		t.Run(name, func(t *testing.T) {
			code, body := post(t, h, "/v1/backtest", c.req)
			assert.Contains(t, []int{http.StatusBadRequest, http.StatusNotFound}, code, string(body))
			assert.Contains(t, strings.ToLower(string(body)), strings.ToLower(c.want))
		})
	}
}

// A TTFT target is judged at each step, predicted and measured, and the
// costly disagreement, predicted met and measured missed, is counted.
func TestBacktest_JudgesATTFTTarget(t *testing.T) {
	r := &stubResolver{}
	s, l := btSeries(240, 0, 1)
	fs, fl := btEarlier(btSeries(240, 0, 1))
	target := 0.15
	out := btRun(t, btEntry(t, r, "fit", fs, fl, btConfig("H100")), btEntry(t, r, "t", s, l, btConfig("H100")),
		BacktestRequest{TTFT: &Objective{Statistic: "p90", Target: &target}})
	a := out.Summary.Target
	require.NotNil(t, a)
	assert.Equal(t, out.Summary.Judged, a.Steps)
	assert.Equal(t, a.Steps, a.BothMet+a.BothMissed+a.MissedMisses+a.FalseAlarms+a.NotJudged)
	// The measured p90 is in the 0.1 to 0.2 s bucket everywhere, which holds
	// the target: no step can say which side of it the fleet was.
	assert.Equal(t, a.Steps, a.NotJudged, "%+v", *a)
	// Under the bucket, every judged step measured a miss.
	target = 0.09
	out = btRun(t, btEntry(t, r, "fit", fs, fl, btConfig("H100")), btEntry(t, r, "t", s, l, btConfig("H100")),
		BacktestRequest{TTFT: &Objective{Statistic: "p90", Target: &target}})
	a = out.Summary.Target
	require.NotNil(t, a)
	assert.Zero(t, a.BothMet)
	assert.Zero(t, a.FalseAlarms)
	assert.Positive(t, a.MissedMisses+a.BothMissed, "%+v", *a)
}

// A source with no prefill variant is refused: a backtest prices a P/D
// fleet. A step in which another variant also served is not judged: the
// rate and the latency are the whole model's.
func TestBacktest_PricesAPDFleetOfItsOwnVariants(t *testing.T) {
	r := &stubResolver{noPrefillRole: true}
	s, l := btSeries(120, 0, 1)
	fs, fl := btEarlier(btSeries(120, 0, 1))
	_, err := backtestQuestionOf(btEntry(t, r, "fit", fs, fl, btConfig("H100")), btEntry(t, r, "t", s, l, btConfig("H100")), BacktestRequest{},
		time.Time{}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no prefill variant")

	pd := &stubResolver{}
	s, l = btSeries(240, 0, 1)
	for i := 120; i < 140; i++ { // 10:30 to 10:35
		s[i].OtherPods = 1
	}
	fs, fl = btEarlier(btSeries(240, 0, 1))
	out := btRun(t, btEntry(t, pd, "fit", fs, fl, btConfig("H100")), btEntry(t, pd, "t", s, l, btConfig("H100")), BacktestRequest{})
	assert.Positive(t, out.Summary.Excluded.OtherVariants)
	for _, st := range out.Steps {
		if st.Start.Format("15:04") == "10:30" {
			assert.Equal(t, ExcludedOtherVariants, st.Excluded)
		}
	}
	assert.Contains(t, out.Excluded, "other variants serving")
}

// A backtest refuses an assumed term no CLI flag takes (assume.flowControl,
// assume.routerWait, a cv past the CLI's range, a V_P under 0), as the
// caller's error, before any step is priced: the pricer would refuse it on
// every step, and the answer would judge none.
func TestBacktest_RefusesAnUnknownRouterWait(t *testing.T) {
	r := &stubResolver{}
	s, l := btSeries(120, 0, 1)
	fs, fl := btEarlier(btSeries(120, 0, 1))
	fit := btEntry(t, r, "fit", fs, fl, btConfig("H100"))
	test := btEntry(t, r, "test", s, l, btConfig("H100"))
	for field, a := range map[string]Assume{"assume.flowControl": {FlowControl: "maybe"}, "assume.routerWait": {RouterWait: "both"},
		"assume.promptCV": {PromptCV: ptr(5.0)}, "assume.vp": {VP: ptr(-1.0)}} {
		_, err := backtestQuestionOf(fit, test, BacktestRequest{Assume: &a}, time.Time{}, "")
		require.Error(t, err)
		assert.True(t, IsInputError(err), "%v", err)
		assert.Contains(t, err.Error(), field)
	}
	ok := Assume{FlowControl: FlowControlOn, RouterWait: RouterWaitInPrefill}
	_, err := backtestQuestionOf(fit, test, BacktestRequest{Assume: &ok}, time.Time{}, "")
	require.NoError(t, err)
}
