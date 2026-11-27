package whatifapi

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// btMixture is the rank mixture's cycles beside btSeries's: four prefill
// ranks at fixed shares, each a processor-sharing rank of a service line, a
// KV-deferral wait rising with the rate, and costs per prompt token.
func btMixture(s []SeriesSample) []whatif.MixtureCycle {
	shares := []float64{0.3, 0.25, 0.25, 0.2}
	out := make([]whatif.MixtureCycle, len(s))
	for i, x := range s {
		// The lengths hold for a step (20 cycles) and move between steps.
		xm := 1500 + 100*float64((i/20)%10)
		lm := 20000 + 1000*float64((i/20)%7)
		es := 0.05 + xm/20000
		c := whatif.MixtureCycle{At: x.At, DecodeTTFT: 0.3 + 2e-6*lm,
			Computed: map[float64]float64{1000: 0.2 * x.Rate, 2000: 0.7 * x.Rate, 4000: x.Rate, math.Inf(1): x.Rate},
			Prompt:   map[float64]float64{10000: 0.1 * x.Rate, 30000: 0.8 * x.Rate, 60000: x.Rate, math.Inf(1): x.Rate}}
		rho := x.Rate / 4 * es
		p, m := 0.02+0.2*rho, 0.3+0.5*rho
		c.Queue = map[float64]float64{0.3: x.Rate * (1 - p*math.Exp(-0.3/m)), 1: x.Rate * (1 - p*math.Exp(-1/m)), math.Inf(1): x.Rate}
		for j, sh := range shares {
			lam := x.Rate * sh
			l := lm + 500*float64(j)
			pf := es / (1 - lam*es)
			r := whatif.MixtureRank{Rate: lam, PrefillTime: pf, QueueTime: p * m, Computed: xm, Prompt: l, TTFT: pf + p*m + 0.2 + 4e-6*l}
			c.Ranks = append(c.Ranks, whatif.MixtureRankReading{Key: "r" + strconv.Itoa(j), MixtureRank: r})
			c.PrefillTTFT += sh * r.TTFT
		}
		out[i] = c
	}
	return out
}

// rmFit is a fit window of n cycles a day before the test, with its
// per-rank series. Its router reads a mean TTFT of 1.2 s, above what the
// prefill engines read (btMixture): a router under its engines, as at
// latency 1, is no fleet, and fits a negative non-queue constant.
func rmFit(t *testing.T, r *stubResolver, n int) *inputsEntry {
	t.Helper()
	fitS, fitL := btEarlier(btSeries(n, 0, 10))
	fit := btEntry(t, r, "fit", fitS, fitL, btConfig("H100"))
	fit.mixture = btMixture(fitS)
	fit.doc.Source.Cycle = 0 // the whole fit source, not the stub's one cycle
	return fit
}

// With TTFTModel, on a fit window that identifies the model (13 h, prompts
// that spread, every rank's own rate, K read), the backtest prices every
// TTFT with the rank mixture, flagged RankMixtureTTFT, and names the fit;
// without it the answer carries no trace of the model.
func TestBacktest_RankMixturePricesTTFT(t *testing.T) {
	r := &stubResolver{}
	fit := rmFit(t, r, 3120)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)

	base := btRun(t, fit, test, BacktestRequest{})
	assert.Nil(t, base.RankMixture)
	b, err := json.Marshal(base)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "rankMixture", "the default answer names no part of the model")

	out := btRun(t, fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4})
	require.NotNil(t, out.RankMixture)
	rm := out.RankMixture
	require.Empty(t, rm.Abstained)
	assert.True(t, rm.Experimental)
	// A step pools cycles whose rate rises through it, so the inversion of
	// their pooled means is close to the line, not exact.
	assert.InDelta(t, 0.05, rm.S0, 0.01)
	assert.InEpsilon(t, 20000, rm.V, 0.05)
	assert.InDelta(t, 0.3, rm.Shares[0], 1e-9)
	require.NotNil(t, rm.KV)
	assert.InDelta(t, 0.2, rm.KV.P1, 0.02)
	assert.Equal(t, 4, rm.Slots)
	assert.False(t, rm.SpreadDefaulted)
	assert.Empty(t, rm.BetaUnidentified)
	assert.Contains(t, strings.Join(out.Assumed, "; "), "OUTSIDE VALIDATED REGION")
	assert.NotEqual(t, base.BacktestID, out.BacktestID)
	priced := 0
	for _, st := range out.Steps {
		if st.Excluded != "" {
			continue
		}
		require.NotNil(t, st.RankMixture)
		assert.Empty(t, st.RankMixture.Refused)
		for _, p := range st.TTFT {
			if p.Predicted.Value != nil {
				assert.Contains(t, p.Predicted.Flags, string(whatif.RankMixtureTTFT))
				priced++
			}
		}
		// TPOT is the planner's, untouched.
		for i := range st.TPOT {
			for _, b := range base.Steps {
				if b.Start.Equal(st.Start) {
					assert.Equal(t, b.TPOT[i].Predicted, st.TPOT[i].Predicted)
				}
			}
		}
	}
	assert.Greater(t, priced, 20)
}

// The fitted shares hold one entry per rank of the fit's prefill pods, so a
// test step on another count is priced at the rate each of its ranks takes:
// the step's fleet rate times the fit's pods over the step's, as the answer
// path prices another count (mixtureFleet.scale). A test on twice the pods
// at twice the rate loads each rank as the fit's count at the base rate does,
// and prices the same TTFT; split over the fit's shares unscaled it priced
// each rank at twice its rate.
func TestBacktest_RankMixturePricesTheTestsPrefillCount(t *testing.T) {
	r := &stubResolver{}
	fit := rmFit(t, r, 3120)
	run := func(name string, pods int) *BacktestResponse {
		ts, tl := btSeries(240, 0, 1)
		x := float64(pods) / 2
		for i := range ts {
			ts[i].Prefill, ts[i].Rate = pods, ts[i].Rate*x
		}
		test := btEntry(t, r, name, ts, tl, btConfig("H100"))
		test.mixture = btMixture(ts)
		return btRun(t, fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4})
	}
	base, twice := run("test2", 2), run("test4", 4)
	require.NotNil(t, base.RankMixture)
	require.Empty(t, base.RankMixture.Abstained)
	require.Len(t, twice.Steps, len(base.Steps))
	compared := 0
	for i, b := range base.Steps {
		w := twice.Steps[i]
		if b.Excluded != "" || w.Excluded != "" || b.RankMixture == nil || b.RankMixture.Refused != "" {
			continue
		}
		require.NotNil(t, w.RankMixture)
		require.Empty(t, w.RankMixture.Refused, "step %d", i)
		for j := range b.TTFT {
			bv, wv := b.TTFT[j].Predicted.Value, w.TTFT[j].Predicted.Value
			if bv == nil {
				continue
			}
			require.NotNil(t, wv)
			assert.InEpsilon(t, *bv, *wv, 1e-9, "step %d stat %d: 4 pods at twice the rate price as 2 at the rate", i, j)
			compared++
		}
	}
	assert.Greater(t, compared, 20)
}

// Where an identifiability check fails the model abstains: every TTFT is the
// planner's own, flagged RankMixtureAbstained, and the answer says why. A
// 2 h fit window does not calibrate the spread.
func TestBacktest_RankMixtureAbstainsWhereItIsNotIdentified(t *testing.T) {
	r := &stubResolver{}
	fit := rmFit(t, r, 480)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)
	base := btRun(t, fit, test, BacktestRequest{})
	out := btRun(t, fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4})
	require.NotNil(t, out.RankMixture)
	assert.Contains(t, out.RankMixture.Abstained, "spread not calibrated")
	assert.Contains(t, strings.Join(out.Assumed, "; "), "abstained")
	compared := 0
	for i, st := range out.Steps {
		require.Equal(t, base.Steps[i].Start, st.Start)
		for j, p := range st.TTFT {
			want := base.Steps[i].TTFT[j].Predicted
			assert.Equal(t, want.Value, p.Predicted.Value, "the planner's own figure")
			if p.Predicted.Value != nil {
				assert.Contains(t, p.Predicted.Flags, string(whatif.RankMixtureAbstained))
				assert.NotContains(t, p.Predicted.Flags, string(whatif.RankMixtureTTFT))
				compared++
			}
		}
	}
	assert.Greater(t, compared, 20)
	// No K read: abstains too.
	out = btRun(t, rmFit(t, r, 3120), test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixture)})
	assert.Contains(t, out.RankMixture.Abstained, "max-num-seqs not read")
}

// An unknown model is the caller's error; a source without the per-rank
// series, or a fit with no router TTFT, makes the model abstain and say why.
func TestBacktest_RankMixtureRefusesWhatItCannotPrice(t *testing.T) {
	r := &stubResolver{}
	fitS, fitL := btEarlier(btSeries(240, 0, 1))
	fit := btEntry(t, r, "fit", fitS, fitL, btConfig("H100"))
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	_, err := backtestQuestionOf(fit, test, BacktestRequest{TTFTModel: "fcfs"}, time.Time{}, "")
	require.Error(t, err)
	assert.True(t, IsInputError(err))
	q, err := backtestQuestionOf(fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixture)}, time.Time{}, "")
	require.NoError(t, err)
	assert.Contains(t, q.mixture.report.Abstained, "per-rank series")
	// A fit with no router TTFT has nothing to fit against, and says so.
	fit.mixture, test.mixture = btMixture(fitS), btMixture(ts)
	fit.doc.Source.Cycle = 0
	for i := range fit.latency {
		fit.latency[i].Router = nil
	}
	q, err = backtestQuestionOf(fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixture)}, time.Time{}, "")
	require.NoError(t, err)
	assert.Contains(t, q.mixture.report.Abstained, "read no router TTFT")
}

// rmBehind sets e's engine rows to complete every request its router reads,
// except over cycles [from, to), where they complete 1/1.25 of them: a
// backlog adding a quarter of a step to the wait, over MaxMixtureBacklog.
func rmBehind(e *inputsEntry, from, to int) {
	for i := range e.series {
		e.series[i].EngineRate = e.series[i].Rate
		if i >= from && i < to {
			e.series[i].EngineRate /= 1.25
		}
	}
}

// A test step whose arrivals run ahead of its completions by more than
// MaxMixtureBacklog is not stationary, and the model refuses it: the
// planner's own figure stands. The steps around it are priced.
func TestBacktest_RankMixtureRefusesBackloggedSteps(t *testing.T) {
	r := &stubResolver{}
	fit := rmFit(t, r, 3120)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)
	rmBehind(test, 120, 180)
	out := btRun(t, fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4})
	require.NotNil(t, out.RankMixture)
	require.Empty(t, out.RankMixture.Abstained)
	refused, priced := 0, 0
	for _, st := range out.Steps {
		if st.Excluded != "" || st.RankMixture == nil {
			continue
		}
		switch {
		case strings.Contains(st.RankMixture.Refused, "not stationary"):
			refused++
		case st.RankMixture.Refused == "":
			priced++
		}
	}
	assert.Positive(t, refused, "the backlogged steps")
	assert.Positive(t, priced, "the stationary steps")
}

// The fit leaves a backlogged step out: its TTFT is a backlog's, and would
// pass into the fit as cost.
func TestBacktest_RankMixtureFitLeavesBackloggedStepsOut(t *testing.T) {
	r := &stubResolver{}
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)
	req := BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4}
	even := rmFit(t, r, 3120)
	rmBehind(even, 0, 0)
	base := btRun(t, even, test, req).RankMixture
	behind := rmFit(t, r, 3120)
	rmBehind(behind, 600, 1200)
	got := btRun(t, behind, test, req).RankMixture
	require.Empty(t, base.Abstained)
	require.Empty(t, got.Abstained)
	assert.Less(t, got.FitSteps, base.FitSteps-20, "the 150 min behind are about 30 steps")
	b, err := json.Marshal(got)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"fitStepsBacklogged":`)
}

// The calibration's own identification: a spread that ends at its
// bisection's upper bound, pooled or in one self-check fold, has solved
// nothing, and the model abstains. A fold at the lower bound is floored, as
// before, not refused.
func TestRankMixture_SpreadAtItsUpperBoundIsNotIdentified(t *testing.T) {
	fitted := func() *rankMixtureQ {
		return &rankMixtureQ{report: RankMixtureReport{Spread: 1.4, FoldSpread: []float64{1.6, 1.3}, Slots: 4, A0: 0.5, Beta: 1e-5}}
	}
	assert.Empty(t, fitted().identified().report.Abstained)
	pooled := fitted()
	pooled.report.Spread, pooled.report.SpreadAtBound = whatif.MaxSpread, true
	pooled.report.FoldSpread = []float64{whatif.MaxSpread, whatif.MaxSpread}
	assert.Contains(t, pooled.identified().report.Abstained, "spread not identified")
	fold := fitted()
	fold.report.FoldSpread = []float64{1.2, whatif.MaxSpread}
	assert.Contains(t, fold.identified().report.Abstained, "spread not identified")
	low := fitted()
	low.report.FoldSpread, low.report.Spread, low.report.SpreadFloored = []float64{0, 1.1}, whatif.MinSpread, true
	assert.Empty(t, low.identified().report.Abstained)
}

// A0 is Z at a prompt of no tokens, a time. Refitted to hold the mean at
// each spread, it goes negative as the spread outgrows Beta, and the model
// abstains.
func TestRankMixture_NegativeNonQueueConstantAbstains(t *testing.T) {
	rq := &rankMixtureQ{report: RankMixtureReport{Spread: 1.4, FoldSpread: []float64{1.6, 1.3}, Slots: 4, A0: -0.7, Beta: 1e-5}}
	assert.Contains(t, rq.identified().report.Abstained, "non-queue constant negative")
	rq.report.Abstained, rq.report.A0 = "", 0
	assert.Empty(t, rq.identified().report.Abstained)
}
