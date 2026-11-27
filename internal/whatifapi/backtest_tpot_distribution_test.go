package whatifapi

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// tdC is the renewal coefficient the synthetic router histograms are drawn
// at; the fit window's last quarter is drawn at tdLateC, so that the whole
// window and its self-check folds fit apart.
const (
	tdC     = 3.0
	tdLateC = 5.0
)

// tdOutput is the decode pods' output lengths at rate r: a tenth of two
// tokens, the rest spread to a thousand, moving with the step.
func tdOutput(r float64, i int) map[float64]float64 {
	short := 0.1 + 0.02*float64((i/20)%5)
	return map[float64]float64{1: 0, 2: short * r, 10: (short + 0.2) * r, 100: 0.7 * r, 1000: r, math.Inf(1): r}
}

// tdDecode is the decode cycles beside s: two replicas a fifth apart in
// token time, two drafts accepted a step, four running a rank.
func tdDecode(s []SeriesSample) []whatif.DecodeCycle {
	out := make([]whatif.DecodeCycle, len(s))
	for i, x := range s {
		out[i] = whatif.DecodeCycle{At: x.At, Output: tdOutput(x.Rate, i), MeanRunning: 4, MostRunning: 6, RanksReported: 8,
			Groups: []whatif.DecodeGroup{{Rate: x.Rate / 2, TokenTime: 0.010, Accepted: 2, AcceptedKnown: true},
				{Rate: x.Rate / 2, TokenTime: 0.012, Accepted: 2, AcceptedKnown: true}}}
	}
	return out
}

// tdRouter sets each cycle's router TPOT histogram to the distribution's at
// tdC around a mean of 12 ms, on the router's coarse edges, the output
// lengths pooled over the router's window as the model pools them.
func tdRouter(l []LatencySample, d []whatif.DecodeCycle, late int) {
	edges := []float64{0.0005, 0.00205, 0.005, 0.01, 0.02, 0.04, 0.06, 0.08, 0.1, math.Inf(1)}
	for i := range l {
		var win []whatif.DecodeCycle
		for j := range d {
			if !d[j].At.After(l[i].At) && d[j].At.After(l[i].At.Add(-300e9)) {
				win = append(win, d[j])
			}
		}
		inv, g2, ok := whatif.DecodeWindow(win)
		if !ok {
			continue
		}
		dist := whatif.TPOTDist{GroupCV2: g2, C: tdC}
		if i >= late {
			dist.C = tdLateC
		}
		const mean = 0.012
		r := l[i].Router.TTFTCountRate
		b := make([]domain.LatencyBucket, len(edges))
		for j, e := range edges {
			b[j] = domain.LatencyBucket{UpperBound: e, Rate: r * dist.CDF(mean, inv, e)}
		}
		l[i].Router.TPOTBuckets, l[i].Router.TPOTSumRate, l[i].Router.TPOTCountRate = b, mean*r, r
	}
}

// tdEntries is a fit of n cycles a day before a test of 240, both with their
// decode series and router TPOT histograms drawn from the model.
func tdEntries(t *testing.T, n int) (fit, test *inputsEntry) {
	t.Helper()
	r := &stubResolver{}
	fitS, fitL := btEarlier(btSeries(n, 0, 1))
	fit = btEntry(t, r, "fit", fitS, fitL, btConfig("H100"))
	fit.decode = tdDecode(fitS)
	tdRouter(fitL, fit.decode, n*3/4)
	fit.doc.Source.Cycle = 0
	ts, tl := btSeries(240, 0, 1)
	test = btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.decode = tdDecode(ts)
	tdRouter(tl, test.decode, len(tl))
	return fit, test
}

// With TPOTModel the backtest fits C back from the router's buckets, prices
// every TPOT percentile from the distribution around the planner's own mean
// (the mean untouched), flags each, and scores each step at the histogram's
// resolution; without it the answer carries no trace of the model.
func TestBacktest_TPOTDistributionPricesPercentiles(t *testing.T) {
	fit, test := tdEntries(t, 960)
	base := btRun(t, fit, test, BacktestRequest{})
	assert.Nil(t, base.TPOTDistribution)
	b, err := json.Marshal(base)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "tpotDistribution", "the default answer names no part of the model")

	out := btRun(t, fit, test, BacktestRequest{TPOTModel: whatif.TPOTRenewalLognormal, DecodeSlots: 64, SpecTokens: 3})
	require.NotNil(t, out.TPOTDistribution)
	td := out.TPOTDistribution
	require.Empty(t, td.Abstained)
	assert.Greater(t, td.C, tdC+0.3, "the whole fit window holds the later, wider quarter")
	assert.Less(t, td.C, tdLateC)
	assert.Len(t, td.FoldC, 2)
	for _, c := range td.FoldC {
		assert.InDelta(t, tdC, c, 0.1, "each fold fits on the steps before its cut, inside the first three quarters")
	}
	require.Len(t, td.Fold, 2)
	assert.Positive(t, td.Fold[1].Steps)
	assert.Positive(t, td.Renewal)
	assert.Less(t, td.Renewal, td.C)
	assert.Less(t, td.SlotWait, whatif.MaxSlotWait)
	assert.Contains(t, strings.Join(out.Assumed, "; "), "OUTSIDE VALIDATED REGION")
	assert.NotEqual(t, base.BacktestID, out.BacktestID)
	require.NotNil(t, td.Test)
	assert.Positive(t, td.Test.Steps)
	priced := 0
	for k, st := range out.Steps {
		if st.Excluded != "" {
			continue
		}
		require.NotNil(t, st.TPOTDist)
		assert.Empty(t, st.TPOTDist.Refused)
		assert.Equal(t, base.Steps[k].TPOT[0].Predicted, st.TPOT[0].Predicted, "the mean is the planner's")
		mean := *st.TPOT[0].Predicted.Value
		for i, q := range backtestStats {
			if q == 0 {
				continue
			}
			p := st.TPOT[i].Predicted
			require.NotNil(t, p.Value)
			assert.Contains(t, p.Flags, string(whatif.TPOTDistribution))
			d := whatif.TPOTDist{GroupCV2: st.TPOTDist.Sigma2 - td.C*st.TPOTDist.Inv, C: td.C}
			assert.InEpsilon(t, d.Quantile(mean, st.TPOTDist.Inv, q), *p.Value, 1e-9)
			priced++
		}
		require.NotNil(t, st.TPOTDist.KS)
	}
	assert.Greater(t, priced, 20)
	assert.Contains(t, strings.Join(out.Notes, " "), "histogram's resolution")
}

// The distribution abstains, every percentile the planner's own flagged
// TPOTDistributionAbstained, where max-num-seqs is not read, where a slot
// queue would form, and where the fit window is too short.
func TestBacktest_TPOTDistributionAbstains(t *testing.T) {
	fit, test := tdEntries(t, 960)
	base := btRun(t, fit, test, BacktestRequest{})
	for name, req := range map[string]BacktestRequest{
		"max-num-seqs not read": {TPOTModel: whatif.TPOTRenewalLognormal},
		"slot queue":            {TPOTModel: whatif.TPOTRenewalLognormal, DecodeSlots: 4},
	} {
		out := btRun(t, fit, test, req)
		require.NotNil(t, out.TPOTDistribution, name)
		assert.NotEmpty(t, out.TPOTDistribution.Abstained, name)
		for k, st := range out.Steps {
			if st.Excluded != "" {
				continue
			}
			require.NotNil(t, st.TPOTDist, name)
			assert.True(t, st.TPOTDist.Abstained, name)
			for i := range st.TPOT {
				assert.Equal(t, base.Steps[k].TPOT[i].Predicted.Value, st.TPOT[i].Predicted.Value, name)
			}
		}
	}
	short, test2 := tdEntries(t, 200)
	out := btRun(t, short, test2, BacktestRequest{TPOTModel: whatif.TPOTRenewalLognormal, DecodeSlots: 64})
	assert.Contains(t, out.TPOTDistribution.Abstained, "fit steps")
	_, err := backtestQuestionOf(fit, test, BacktestRequest{TPOTModel: "ratio"}, out.Steps[0].Start, "")
	assert.Error(t, err)
}
