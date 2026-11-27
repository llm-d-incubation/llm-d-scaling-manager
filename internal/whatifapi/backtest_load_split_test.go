package whatifapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadResolver is btResolver that reads only the cycles a source's Cycles
// keeps, as the served resolver's replay does (learnreplay.CyclesOf), and
// remembers how many it kept.
type loadResolver struct {
	btResolver
	kept []int
}

func (r *loadResolver) Resolve(ctx context.Context, req InputsRequest) (*Resolved, error) {
	res, err := r.btResolver.Resolve(ctx, req)
	if err != nil || req.Source == nil || req.Source.Cycles == nil {
		return res, err
	}
	var s []SeriesSample
	var l []LatencySample
	for i, x := range res.Series {
		if req.Source.Cycles.Keeps([]time.Time{x.At}) {
			s, l = append(s, x), append(l, res.Latency[i])
		}
	}
	res.Series, res.Latency = s, l
	res.Content += " load"
	r.mu.Lock()
	r.kept = append(r.kept, len(s))
	r.mu.Unlock()
	return res, nil
}

// The synthetic window's twelve five-minute steps run at 2.475 r/s plus 1 a
// step: p50 is 7.975 r/s and p75 10.725.
const (
	btP50 = 7.975
	btP75 = 10.725
)

// POST /v1/backtest splits one source by load: the fit is read from the
// steps below fitBelow alone, through the resolver, and only the steps at or
// above testAbove are judged; the steps between are counted as outside the
// test's load.
func TestBacktest_SplitsOneSourceByLoadOverHTTP(t *testing.T) {
	r := &loadResolver{btResolver: btResolver{n: 480}}
	h := New(Config{Resolver: r, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	code, body := post(t, h, "/v1/backtest", BacktestRequest{InputsID: id, FitBelow: "p50", TestAbove: "p75", Step: "5m"})
	require.Equal(t, http.StatusOK, code, string(body))
	var out BacktestResponse
	require.NoError(t, json.Unmarshal(body, &out))
	assert.Equal(t, []int{240}, r.kept, "the fit is read from the twelve steps below p50 alone")
	require.NotNil(t, out.LoadSplit)
	assert.Equal(t, BacktestLoadSplit{FitBelow: "p50", TestAbove: "p75", FitBelowRate: btP50, TestAboveRate: 10.72, FitSteps: 12, TestSteps: 6}, *out.LoadSplit)
	assert.Equal(t, "by load: fit below p50 (7.975 r/s), test at or above p75 (10.72 r/s)", out.Split)
	assert.Equal(t, 240, out.Fit.Cycles)
	assert.Equal(t, btT0.Add(359*btCycle), out.Fit.To, "the fit ends at the last cycle below p50")
	assert.Equal(t, btT0.Add(180*btCycle), out.Test.From, "the test starts at the first step at or above p75")
	assert.Equal(t, 120, out.Test.Cycles)
	judged := 0
	for _, st := range out.Steps {
		switch st.Excluded {
		case "":
			judged++
			assert.GreaterOrEqual(t, st.Rate, btP75*0.9, "a judged step under the test's floor: %s", st.Start)
		case ExcludedLoad:
			assert.Less(t, st.Rate, btP75, "a step over the floor left out: %s", st.Start)
			assert.Contains(t, st.Detail, "under the test's floor")
		}
	}
	assert.Equal(t, 9, out.Summary.Excluded.Load, "the steps of the second ramp under p75 are outside the test's load")
	assert.Equal(t, 1, out.Summary.Excluded.WarmUp, "the test's first step is still its warm-up")
	assert.Equal(t, 5, judged)
	assert.Contains(t, out.Excluded, "9 outside the test's load")

	// The same split asked twice is the same backtest; another test bound is
	// another one.
	_, again := post(t, h, "/v1/backtest", BacktestRequest{InputsID: id, FitBelow: "p50", TestAbove: "p75", Step: "5m"})
	var out2 BacktestResponse
	require.NoError(t, json.Unmarshal(again, &out2))
	assert.Equal(t, out.BacktestID, out2.BacktestID)
	_, other := post(t, h, "/v1/backtest", BacktestRequest{InputsID: id, FitBelow: "p50", TestAbove: "p50", Step: "5m"})
	var out3 BacktestResponse
	require.NoError(t, json.Unmarshal(other, &out3))
	assert.NotEqual(t, out.BacktestID, out3.BacktestID)

	for name, c := range map[string]struct {
		req  BacktestRequest
		want string
	}{
		"fit bound alone":     {BacktestRequest{InputsID: id, FitBelow: "p50"}, "give both"},
		"test bound alone":    {BacktestRequest{InputsID: id, TestAbove: "p75"}, "give both"},
		"with a time split":   {BacktestRequest{InputsID: id, FitBelow: "p50", TestAbove: "p75", Split: "50%"}, "give one"},
		"fit over test":       {BacktestRequest{InputsID: id, FitBelow: "p75", TestAbove: "p50"}, "is above testAbove"},
		"bad percentile":      {BacktestRequest{InputsID: id, FitBelow: "p150", TestAbove: "p75"}, "percentile from p0 to p100"},
		"bad rate":            {BacktestRequest{InputsID: id, FitBelow: "-1", TestAbove: "p75"}, "rate above 0"},
		"nothing below":       {BacktestRequest{InputsID: id, FitBelow: "1", TestAbove: "p75"}, "nothing to fit on"},
		"nothing above":       {BacktestRequest{InputsID: id, FitBelow: "p50", TestAbove: "100"}, "nothing to test on"},
		"rates in r/s":        {BacktestRequest{InputsID: id, FitBelow: "5", TestAbove: "4"}, "is above testAbove"},
		"unknown test inputs": {BacktestRequest{InputsID: id, TestInputsID: "nope", FitBelow: "p50", TestAbove: "p75"}, "unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			code, body := post(t, h, "/v1/backtest", c.req)
			assert.Contains(t, []int{http.StatusBadRequest, http.StatusNotFound}, code, string(body))
			assert.Contains(t, strings.ToLower(string(body)), strings.ToLower(c.want))
		})
	}
}

// Over two sources of one time, the fit's low steps against the test's high
// ones: a test step the fit read a cycle of is not judged, and the fit need
// not end before the test starts.
func TestBacktest_SplitsTwoSourcesByLoad(t *testing.T) {
	r := &stubResolver{}
	s, l := btSeries(240, 0, 1)
	// The fit's own rates run a step behind the test's, so that its steps
	// below 8 r/s reach one step of the test's at or over 8.
	fs, fl := btSeries(240, 0, 1)
	for i := range fs {
		fs[i].Rate--
	}
	req := BacktestRequest{FitBelow: "8", TestAbove: "8"}
	cs, err := LoadFitCycles(&Resolved{Series: fs}, req)
	require.NoError(t, err)
	var ks []SeriesSample
	var kl []LatencySample
	for i, x := range fs {
		if cs.Keeps([]time.Time{x.At}) {
			ks, kl = append(ks, x), append(kl, fl[i])
		}
	}
	req.FitCycles = cs
	fit := btEntry(t, r, "fit", ks, kl, btConfig("H100"))
	fit.doc.Source.Name = "subset.jsonl.gz"
	test := btEntry(t, r, "test", s, l, btConfig("H100"))
	out := btRun(t, fit, test, req)
	require.NotNil(t, out.LoadSplit)
	assert.Equal(t, 7, out.LoadSplit.FitSteps, "the fit's steps under 8 r/s: 1.475 to 7.475")
	assert.Equal(t, 1, out.LoadSplit.FitRead, "the test's 8.475 r/s step is one the fit read")
	assert.Equal(t, 5, out.LoadSplit.TestSteps)
	for _, st := range out.Steps {
		if st.Start.Equal(btT0.Add(30 * time.Minute)) {
			assert.Equal(t, ExcludedLoad, st.Excluded)
			assert.Contains(t, st.Detail, "the fit read a cycle of this step")
		}
	}

	// A fit read from every cycle is no split by load.
	req.FitCycles = nil
	_, err = backtestQuestionOf(fit, test, req, time.Time{}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the fit was read from every cycle")
}

// The bounds: a rate as given, a percentile linear between the two nearest
// steps that served traffic, idle steps left out of it.
func TestLoadBound(t *testing.T) {
	at := func(m int) time.Time { return btT0.Add(time.Duration(m) * time.Minute) }
	rates := map[time.Time]float64{at(0): 0, at(5): 1, at(10): 2, at(15): 3, at(20): 4, at(25): 5}
	for spec, want := range map[string]float64{"p0": 1, "p50": 3, "p75": 4, "p100": 5, "p10": 1.4, "2.5": 2.5} {
		got, err := loadBound("fitBelow", spec, rates)
		require.NoError(t, err, spec)
		assert.InDelta(t, want, got, 1e-12, spec)
	}
	_, err := loadBound("fitBelow", "p50", map[time.Time]float64{at(0): 0})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no step of the source served traffic")
	sr := stepRates([]SeriesSample{{At: at(0), Rate: 1, RateKnown: true}, {At: at(1), Rate: 3, RateKnown: true},
		{At: at(2), Rate: 100}, {At: at(5), Rate: 2, RateKnown: true}}, 5*time.Minute)
	assert.Equal(t, map[time.Time]float64{at(0): 2, at(5): 2}, sr, "a cycle with no known rate is not averaged in")
}

// With no load split asked, a backtest's answer carries neither the split
// nor its exclusion count: the answer is as before.
func TestBacktest_NoLoadSplitAnswersAsBefore(t *testing.T) {
	r := &stubResolver{}
	s, l := btSeries(240, 0, 1)
	fs, fl := btEarlier(btSeries(240, 0, 1))
	out := btRun(t, btEntry(t, r, "fit", fs, fl, btConfig("H100")), btEntry(t, r, "t", s, l, btConfig("H100")), BacktestRequest{})
	b, err := json.Marshal(out)
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"loadSplit"`)
	e, err := json.Marshal(out.Summary.Excluded)
	require.NoError(t, err)
	assert.JSONEq(t, `{"noTraffic":0,"noMeasurement":0,"scaling":0,"warmUp":1,"notPriced":0,"otherVariants":0,"burst":0}`, string(e),
		"the exclusion counts gained a key")
	assert.NotContains(t, out.Excluded, "outside the test's load")
}

// A step the split leaves out says why: read by the fit, under the test's
// floor with its rate, or with no rate read at all, which is not a rate
// under the floor.
func TestLoadSplit_ExcludedSaysWhy(t *testing.T) {
	at := func(m int) time.Time { return btT0.Add(time.Duration(m) * time.Minute) }
	ls := &loadSplit{report: BacktestLoadSplit{TestAbove: "p75"}, testRate: 10,
		outside: map[time.Time]bool{at(0): true, at(5): true, at(10): true}, fitRead: map[time.Time]bool{at(10): true},
		rates: map[time.Time]float64{at(0): 4, at(10): 12}}
	var under, unread, read BacktestStep
	ls.excluded(&under, at(0))
	ls.excluded(&unread, at(5))
	ls.excluded(&read, at(10))
	assert.Equal(t, ExcludedLoad, under.Excluded)
	assert.Contains(t, under.Detail, "under the test's floor of 10")
	assert.Equal(t, 4.0, under.Rate)
	assert.Equal(t, ExcludedLoad, unread.Excluded)
	assert.Contains(t, unread.Detail, "no cycle of it read an arrival rate")
	assert.NotContains(t, unread.Detail, "under the test's floor")
	assert.Contains(t, read.Detail, "the fit read a cycle")
	var in BacktestStep
	ls.excluded(&in, at(15))
	assert.Empty(t, in.Excluded, "a step the split keeps is not marked")
}
