package whatif

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expHist is a cumulative histogram of an exponential of mean m on a fine
// grid (bound to cumulative share), with its +Inf bucket.
func expHist(m float64) map[float64]float64 {
	h := map[float64]float64{}
	for x := m / 64; x < 40*m; x *= 1.08 {
		h[x] = 1 - math.Exp(-x/m)
	}
	h[math.Inf(1)] = 1
	return h
}

// pointHist is every request at x (one narrow bucket at it).
func pointHist(x float64) map[float64]float64 {
	return map[float64]float64{x * 0.999: 0, x: 1, math.Inf(1): 1}
}

// Kleinrock: for M/G/1-PS the mean sojourn is E[S]/(1 - rho) whatever the
// service law. One rank, no KV wait, no prompt term: the draws' mean is that.
func TestRankMixture_PSMeanIsInsensitive(t *testing.T) {
	for _, h := range []map[float64]float64{expHist(2000), pointHist(2000)} {
		m := RankMixture{S0: 0.1, V: 10000, Shares: []float64{1}}
		hx, ok := newCumHist(h)
		require.True(t, ok)
		var es float64
		for i := range 20000 {
			es += m.S0 + hx.ppf((float64(i)+0.5)/20000)/m.V
		}
		es /= 20000
		for _, rho := range []float64{0.2, 0.5, 0.8} {
			rate := rho / es
			d, u := m.Draws(rate, h, pointHist(1000), 40000)
			require.Nil(t, u)
			mean, _ := d.Stats(0, 0)
			assert.InDelta(t, es/(1-rho), mean, 0.03*es/(1-rho), "rho %.1f", rho)
			assert.InDelta(t, rho, d.Checks.Rho[0], 0.02)
		}
	}
}

// Poisson splitting: each rank is its own M/G/1-PS queue at p_i lambda, and the
// population's mean is sum p_i E[S]/(1 - rho_i): an uneven split prices
// above the even one at the same rate.
func TestRankMixture_MixtureWeighsShares(t *testing.T) {
	h := pointHist(1000)
	even := RankMixture{S0: 0, V: 1000, Shares: []float64{0.5, 0.5}}
	uneven := RankMixture{S0: 0, V: 1000, Shares: []float64{0.7, 0.3}}
	rate := 1.0 // E[S] 1 s: rho 0.5 each, or 0.7 and 0.3
	d, u := even.Draws(rate, h, h, 20000)
	require.Nil(t, u)
	me, _ := d.Stats(0, 0)
	d, u = uneven.Draws(rate, h, h, 20000)
	require.Nil(t, u)
	mu, _ := d.Stats(0, 0)
	assert.InDelta(t, 2.0, me, 0.04)
	assert.InDelta(t, 0.7/0.3+0.3/0.7, mu, 0.06)
	assert.Equal(t, []float64{0.7, 0.3}, roundAll(d.Checks.Rho, 3))
}

func roundAll(xs []float64, d int) []float64 {
	p := math.Pow(10, float64(d))
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = math.Round(x*p) / p
	}
	return out
}

// The KV-deferral wait adds p_d m_d to the mean, and its tail is the
// exponential's: P(q > t) = p_d exp(-t/m_d).
func TestRankMixture_KVTail(t *testing.T) {
	h := pointHist(1)
	m := RankMixture{S0: 0, V: 1e9, Shares: []float64{1}, KV: KVDeferral{P0: 0.2, M0: 0.5, Known: true, RhoLo: 0, RhoHi: 1}}
	d, u := m.Draws(1e-6, h, h, 50000)
	require.Nil(t, u)
	mean, q := d.Stats(0, 0, 0.9, 0.95)
	assert.InDelta(t, 0.2*0.5, mean, 0.005)
	// P(q > t) = 0.2 exp(-t/0.5): the 0.9 quantile is 0.5 ln 2, the 0.95 one 0.5 ln 4.
	assert.InDelta(t, 0.5*math.Log(2), q[0], 0.02)
	assert.InDelta(t, 0.5*math.Log(4), q[1], 0.03)
	off := m
	off.KV.Known = false
	d, _ = off.Draws(1e-6, h, h, 1000)
	mean, _ = d.Stats(0, 0)
	assert.Less(t, mean, 1e-3, "an unfitted KV wait adds nothing")
}

// The lines are held where they leave the data: p in [MinKVDeferralP, 1],
// m at least MinKVDeferralMean, rho at most MaxKVRho.
func TestKVDeferral_HeldAtItsBounds(t *testing.T) {
	k := KVDeferral{P0: -1, P1: 10, M0: -1, M1: 1, Known: true}
	p, m := k.at(0)
	assert.Equal(t, MinKVDeferralP, p)
	assert.Equal(t, MinKVDeferralMean, m)
	p, _ = k.at(5)
	assert.Equal(t, 1.0, p)
	// rho is read at MaxKVRho at most: -1 + 0.95 is under the floor.
	_, m = k.at(5)
	assert.Equal(t, MinKVDeferralMean, m)
	k.M0 = 0
	_, m = k.at(5)
	assert.InDelta(t, MaxKVRho, m, 1e-12)
}

// The prompt term is deterministic: Z = A0 + sb L moves the mean by
// A0 + sb E[L] and leaves the queue part alone.
func TestRankMixture_PromptTerm(t *testing.T) {
	m := RankMixture{S0: 0.1, V: 10000, Shares: []float64{1}}
	d, u := m.Draws(1, pointHist(1000), pointHist(50000), 4000)
	require.Nil(t, u)
	m0, _ := d.Stats(0, 0)
	m1, q1 := d.Stats(0.3, 1e-5, 0.9)
	_, q0 := d.Stats(0, 0, 0.9)
	assert.InDelta(t, m0+0.3+0.5, m1, 1e-3)
	assert.InDelta(t, q0[0]+0.3+0.5, q1[0], 2e-3)
}

// The checks: the admission share rho^K, a rank above MaxLPSRho flags
// LimitedPSHeavyTraffic, a relaxation over a tenth of the step RelaxationLong,
// and a rank at rho 1 refuses.
func TestRankMixture_Checks(t *testing.T) {
	h := pointHist(1000)
	m := RankMixture{S0: 0, V: 1000, Shares: []float64{1}, Slots: 4}
	d, u := m.Draws(0.3, h, h, 1000)
	require.Nil(t, u)
	assert.InEpsilon(t, math.Pow(0.3, 4), d.Checks.AdmissionShare, 0.01)
	assert.InEpsilon(t, math.Pow(0.3, 4), d.Checks.BusiestRhoK, 0.01)
	assert.Empty(t, d.Checks.Flags(300))
	d, _ = m.Draws(0.8, h, h, 1000)
	assert.NotContains(t, d.Checks.Flags(300), LimitedPSHeavyTraffic)
	d, _ = m.Draws(0.95, h, h, 1000)
	assert.Contains(t, d.Checks.Flags(300), LimitedPSHeavyTraffic)
	d, _ = m.Draws(0.8, h, h, 1000)
	assert.InEpsilon(t, 1/(0.2*0.2), d.Checks.Relaxation, 0.01)
	assert.Contains(t, d.Checks.Flags(100), RelaxationLong)
	assert.NotContains(t, d.Checks.Flags(300), RelaxationLong)
	_, u = m.Draws(1.01, h, h, 1000)
	require.NotNil(t, u)
	assert.Equal(t, Saturated, u.Reason)
	_, u = m.Draws(0.5, nil, h, 1000)
	require.NotNil(t, u)
}

// PS inversion recovers the service line from ranks that are M/G/1-PS:
// E[T] = E[S]/(1 - lambda E[S]), E[S] = S0 + E[x]/V.
func TestFitPSInversion_RecoversTheLine(t *testing.T) {
	s0, v := 0.25, 20000.0
	steps := make([]MixtureStep, 0, 12)
	for i := range 12 {
		ranks := make([]MixtureRank, 0, 4)
		for j := range 4 {
			x := 1000 + 400*float64(i+j)
			lam := 0.3 + 0.1*float64(j)
			es := s0 + x/v
			ranks = append(ranks, MixtureRank{Rate: lam, PrefillTime: es / (1 - lam*es), QueueTime: 0.01, Computed: x})
		}
		steps = append(steps, MixtureStep{Ranks: ranks})
	}
	gs0, gv, prop, err := FitPSInversion(steps)
	require.NoError(t, err)
	assert.False(t, prop)
	assert.InDelta(t, s0, gs0, 1e-9)
	assert.InDelta(t, v, gv, 1e-6)
	// Negative control: regressing E[T] itself (no inversion) reads a
	// different line on the same ranks.
	xs, ts := make([]float64, 0, 48), make([]float64, 0, 48)
	for _, st := range steps {
		for _, r := range st.Ranks {
			xs, ts = append(xs, r.Computed), append(ts, r.PrefillTime)
		}
	}
	a, b, _ := olsLine(xs, ts)
	assert.Greater(t, math.Abs(a-s0)+math.Abs(1/b-v)/v, 0.05)
}

// The KV-deferral lines are recovered from queue-time histograms drawn from
// them: an atom at 0 and an exponential, read at the 0.3 s and 1 s edges.
func TestFitKVDeferral_RecoversTheLines(t *testing.T) {
	s0, v, ranks := 0.2, 10000.0, 4
	steps := make([]MixtureStep, 0, 10)
	for i := range 10 {
		rate := 2 + float64(i)
		rho := rate / float64(ranks) * (s0 + 2000/v)
		p, m := 0.05+0.3*rho, 0.4+0.6*rho
		q := map[float64]float64{kvEdgeLo: 1 - p*math.Exp(-kvEdgeLo/m), kvEdgeHi: 1 - p*math.Exp(-kvEdgeHi/m), math.Inf(1): 1}
		rs := make([]MixtureRank, 0, ranks)
		for range ranks {
			rs = append(rs, MixtureRank{Rate: rate / float64(ranks), Computed: 2000})
		}
		steps = append(steps, MixtureStep{Rate: rate, Ranks: rs, Queue: q})
	}
	k, err := FitKVDeferral(steps, s0, v, ranks)
	require.NoError(t, err)
	assert.InDelta(t, 0.05, k.P0, 1e-9)
	assert.InDelta(t, 0.3, k.P1, 1e-9)
	assert.InDelta(t, 0.4, k.M0, 1e-9)
	assert.InDelta(t, 0.6, k.M1, 1e-9)
	// No histogram with both edges: not fitted.
	for i := range steps {
		delete(steps[i].Queue, kvEdgeHi)
	}
	_, err = FitKVDeferral(steps, s0, v, ranks)
	assert.Error(t, err)
}

// Beta is the three slopes against the prompt, each held at 0 or above.
func TestFitBeta_RecoversTheSlopes(t *testing.T) {
	steps := make([]MixtureStep, 0, 10)
	for i := range 10 {
		l := 20000 + 5000*float64(i)
		rs := make([]MixtureRank, 0, 3)
		for j := range 3 {
			lr := l + 1000*float64(j)
			rs = append(rs, MixtureRank{Rate: 1, PrefillTime: 0.5, QueueTime: 0.1, Computed: 2000, Prompt: lr, TTFT: 0.5 + 0.1 + 0.3 + 4e-6*lr})
		}
		dec := 0.4 + 3e-6*l
		pre := 0.9 + 4e-6*l
		steps = append(steps, MixtureStep{Ranks: rs, PromptMean: l, DecodeTTFT: dec, PrefillTTFT: pre,
			RouterMean: pre + dec + 0.6 - 1e-6*l})
	}
	o, d, r, un, err := FitBeta(steps)
	require.NoError(t, err)
	assert.Empty(t, un)
	assert.InDelta(t, 4e-6, o, 1e-12)
	assert.InDelta(t, 3e-6, d, 1e-12)
	assert.Zero(t, r, "a falling remainder is held at 0")
	// At one prompt length no slope is fitted.
	for i := range steps {
		steps[i].PromptMean = 50000 + float64(i)
		for j := range steps[i].Ranks {
			steps[i].Ranks[j].Prompt = 50000 + float64(i)
		}
	}
	o, d, r, un, err = FitBeta(steps)
	require.NoError(t, err)
	assert.Equal(t, []string{"overhead", "decode", "remainder"}, un)
	assert.Zero(t, o+d+r)
}

// The share profile is sorted busiest first and averaged over the steps
// where every rank served.
func TestShareProfile(t *testing.T) {
	steps := []MixtureStep{
		{Ranks: []MixtureRank{{Rate: 1}, {Rate: 3}}},
		{Ranks: []MixtureRank{{Rate: 2}, {Rate: 2}}},
		{Ranks: []MixtureRank{{Rate: 5}}},
	}
	s, cv, err := ShareProfile(steps)
	require.NoError(t, err)
	assert.Equal(t, []float64{0.625, 0.375}, s)
	assert.InDelta(t, 0.2, cv, 1e-9)
}

// Synthetic recovery: steps generated from the model with known parameters
// (a spread of 1.5 on beta, a known A0) are fitted back. Each step's measured
// p90 is the model's own at those parameters, and its mean too, but for the
// part of the spread that moves with the step's mean prompt: the spread
// widens Z's tail, while the mean rises with the prompt by Beta alone, which
// is what the three slopes measure.
func TestFitRankMixture_RecoversTheSpread(t *testing.T) {
	truth := RankMixture{S0: 0.25, V: 20000, Shares: []float64{0.3, 0.25, 0.25, 0.2}, A0: 0.4, Spread: 1.5,
		KV: KVDeferral{P0: 0.02, P1: 0.3, M0: 0.3, M1: 1.2, Known: true, RhoLo: 0, RhoHi: 1}}
	truth.BetaOverhead, truth.BetaDecode = 4e-6, 3e-6
	truth.Beta = truth.BetaOverhead + truth.BetaDecode
	steps := syntheticSteps(t, truth, 96)
	m, diag, err := FitRankMixture(steps, RankMixtureOptions{})
	require.NoError(t, err)
	assert.InDelta(t, truth.S0, m.S0, 0.01)
	assert.InDelta(t, truth.V, m.V, 0.02*truth.V)
	assert.InDelta(t, truth.Beta, m.Beta, 0.05*truth.Beta)
	assert.InDelta(t, truth.Shares[0], m.Shares[0], 0.01)
	assert.InDelta(t, truth.Spread, m.Spread, 0.15, "spread")
	assert.InDelta(t, truth.A0, m.A0, 0.05, "A0")
	assert.InDelta(t, 1, diag.FitP90, 0.02)
	require.Len(t, diag.FoldSpread, 2)
	// A fit window under the shortest calibrated span prices Spread 1.
	short, sd, err := FitRankMixture(steps, RankMixtureOptions{MinSpreadSpan: 24 * time.Hour})
	require.NoError(t, err)
	assert.True(t, sd.SpreadDefaulted)
	assert.Equal(t, 1.0, short.Spread)
	assert.False(t, diag.SpreadDefaulted)
	// A floor above the fitted spread prices the floor.
	fl, fd, err := FitRankMixture(steps, RankMixtureOptions{SpreadFloor: 2})
	require.NoError(t, err)
	assert.True(t, fd.SpreadFloored)
	assert.Equal(t, 2.0, fl.Spread)
	assert.False(t, diag.SpreadFloored)
	// Ablation: without the spread the fit window's p90 reads low.
	m1, d1, err := FitRankMixture(steps, RankMixtureOptions{NoSpread: true})
	require.NoError(t, err)
	assert.Equal(t, 1.0, m1.Spread)
	assert.Less(t, d1.FitP90, 0.97)
}

// Where the fit window's p90 lies past what any spread up to MaxSpread
// gives (a backlog the model does not price, say), the calibration ends at
// its bound, and A0, refitted to hold the mean, falls by what the spread
// added to Z's mean: below 0. Both are what the planner's identification
// checks read.
func TestFitRankMixture_AnUnreachableP90EndsAtTheBound(t *testing.T) {
	truth := RankMixture{S0: 0.25, V: 20000, Shares: []float64{0.3, 0.25, 0.25, 0.2}, A0: 0.4, Spread: 1.5,
		KV: KVDeferral{P0: 0.02, P1: 0.3, M0: 0.3, M1: 1.2, Known: true, RhoLo: 0, RhoHi: 1}}
	truth.BetaOverhead, truth.BetaDecode = 4e-6, 3e-6
	truth.Beta = truth.BetaOverhead + truth.BetaDecode
	steps := syntheticSteps(t, truth, 96)
	for i := range steps {
		steps[i].RouterP90 *= 4
	}
	m, diag, err := FitRankMixture(steps, RankMixtureOptions{SpreadFloor: MinSpread})
	require.NoError(t, err)
	assert.True(t, diag.SpreadAtBound)
	assert.Equal(t, MaxSpread, m.Spread)
	assert.Equal(t, []float64{MaxSpread, MaxSpread}, diag.FoldSpread)
	assert.Less(t, m.A0, 0.0)
}

// syntheticSteps is n steps of a fleet that is the model m: four ranks, rates
// rising, prompts and computed tokens varying by step, every mean the model's.
func syntheticSteps(t *testing.T, m RankMixture, n int) []MixtureStep {
	t.Helper()
	t0 := time.Unix(1_000_000, 0)
	out := make([]MixtureStep, 0, n)
	for i := range n {
		rate := 2 + 6*float64(i%12)/11
		xm := 1500 + 100*float64(i%7)
		lm := 40000 + 4000*float64(i%5)
		comp, prom := expHist(xm), expHist(lm)
		ranks := make([]MixtureRank, 0, len(m.Shares))
		for _, p := range m.Shares {
			lam := rate * p
			es := m.S0 + xm/m.V
			rho := lam * es
			pd, md := m.KV.at(rho)
			ranks = append(ranks, MixtureRank{Rate: lam, PrefillTime: es / (1 - rho), QueueTime: pd * md, Computed: xm,
				Prompt: lm, TTFT: es/(1-rho) + pd*md + 0.3 + m.BetaOverhead*lm})
		}
		es := m.S0 + xm/m.V
		rhoMean := rate / float64(len(m.Shares)) * es
		pd, md := m.KV.at(rhoMean)
		queue := map[float64]float64{kvEdgeLo: 1 - pd*math.Exp(-kvEdgeLo/md), kvEdgeHi: 1 - pd*math.Exp(-kvEdgeHi/md), math.Inf(1): 1}
		d, u := m.Draws(rate, comp, prom, DefaultMixtureDraws)
		require.Nil(t, u)
		mean, q := d.Stats(m.A0, m.Spread*m.Beta, 0.9)
		// The mean rises with the mean prompt by Beta, not Spread Beta.
		shift := (m.Spread - 1) * m.Beta * (lm - 48000)
		mean -= shift
		q[0] -= shift
		var pre float64
		for k, r := range ranks {
			pre += m.Shares[k] * r.TTFT
		}
		dec := 0.4 + m.BetaDecode*lm
		out = append(out, MixtureStep{At: t0.Add(time.Duration(i) * 5 * time.Minute), Rate: rate, Ranks: ranks, Computed: comp,
			Prompt: prom, Queue: queue, PrefillTTFT: pre, DecodeTTFT: dec, PromptMean: lm, RouterMean: mean, RouterP90: q[0]})
	}
	return out
}

// MixtureStepOf pools a step's cycles: rates are the mean over the cycles
// (a rank absent from one counts no request there), means weighted by rate,
// histograms the mean of the cycles'.
func TestMixtureStepOf_PoolsCycles(t *testing.T) {
	at := time.Unix(100, 0)
	c1 := MixtureCycle{Ranks: []MixtureRankReading{
		{Key: "a", MixtureRank: MixtureRank{Rate: 1, PrefillTime: 1, QueueTime: 0, TTFT: 2, Computed: 100, Prompt: 1000}},
		{Key: "b", MixtureRank: MixtureRank{Rate: 2, PrefillTime: 2, QueueTime: -1, TTFT: 3, Computed: 200, Prompt: 2000}}},
		Computed: map[float64]float64{10: 2}, PrefillTTFT: 2, DecodeTTFT: 0.5}
	c2 := MixtureCycle{Ranks: []MixtureRankReading{
		{Key: "a", MixtureRank: MixtureRank{Rate: 3, PrefillTime: 2, QueueTime: 1, TTFT: 4, Computed: 300, Prompt: 3000}}},
		Computed: map[float64]float64{10: 4}, PrefillTTFT: 4}
	s := MixtureStepOf([]MixtureCycle{c1, c2}, at)
	require.Len(t, s.Ranks, 2)
	a, b := s.Ranks[0], s.Ranks[1]
	assert.Equal(t, 2.0, a.Rate)
	assert.InDelta(t, (1*1+3*2)/4.0, a.PrefillTime, 1e-12)
	assert.InDelta(t, 0.75, a.QueueTime, 1e-12)
	assert.Equal(t, 1.0, b.Rate)
	assert.Equal(t, -1.0, b.QueueTime, "a queue time never read stays unknown")
	assert.Equal(t, 3.0, s.Rate)
	assert.Equal(t, map[float64]float64{10: 3}, s.Computed)
	assert.Equal(t, 3.0, s.PrefillTTFT)
	assert.Equal(t, 0.5, s.DecodeTTFT)
	assert.InDelta(t, (2*2500+1*2000)/3.0, s.PromptMean, 1e-9)
}

// The draws are fixed: the same inputs price the same figures on every run.
func TestRankMixture_Deterministic(t *testing.T) {
	m := RankMixture{S0: 0.2, V: 20000, Shares: []float64{0.6, 0.4}, A0: 0.5, Beta: 1e-5, Spread: 1.2}
	a, _, u := m.Price(3, expHist(2000), expHist(50000), 0.9)
	require.Nil(t, u)
	b, _, _ := m.Price(3, expHist(2000), expHist(50000), 0.9)
	assert.Equal(t, a, b)
}

// Zhang and Zwart's mean, and the admission wait that splits it: the PS
// sojourn plus rho^K times the wait is the LPS mean; below cs^2 1 the wait
// is 0. At K 1 the mean is Kingman's (Pollaczek-Khinchine at Poisson).
func TestLPSMean_AndTheAdmissionWait(t *testing.T) {
	es, rho, cs2 := 0.5, 0.6, 3.0
	for _, k := range []int{1, 2, 4, 8} {
		w := LPSAdmissionWait(es, rho, cs2)
		assert.InDelta(t, LPSMean(es, rho, cs2, k), es/(1-rho)+math.Pow(rho, float64(k))*w, 1e-12)
	}
	assert.InDelta(t, es+rho*es*(1+cs2)/(2*(1-rho)), LPSMean(es, rho, cs2, 1), 1e-12, "K 1 is P-K")
	assert.InDelta(t, es/(1-rho), LPSMean(es, rho, cs2, 200), 1e-9, "large K is PS")
	assert.Zero(t, LPSAdmissionWait(es, rho, 0.5))
}

// twoPoint is nine in ten prompts of 500 tokens and one in ten of 20000.
func twoPoint() map[float64]float64 {
	return map[float64]float64{499.5: 0, 500: 0.9, 19980: 0.9, 20000: 1, math.Inf(1): 1}
}

// The draws are the LPS mixture: their mean is Zhang and Zwart's, a share
// rho^K of the requests wait for admission, and the percentiles sit above
// plain PS's. With K unknown the rank is plain PS.
func TestRankMixture_LimitedPS(t *testing.T) {
	h := twoPoint()
	lps := RankMixture{S0: 0, V: 2450, Shares: []float64{1}, Slots: 2}
	ps := lps
	ps.Slots = 0
	d1, u := lps.Draws(0.8, h, h, 40000)
	require.Nil(t, u)
	d0, u := ps.Draws(0.8, h, h, 40000)
	require.Nil(t, u)
	m1, q1 := d1.Stats(0, 0, 0.9)
	m0, q0 := d0.Stats(0, 0, 0.9)
	hx, _ := newCumHist(h)
	var m, m2 float64
	for i := range 40000 {
		x := hx.ppf((float64(i)+0.5)/40000) / 2450
		m += x
		m2 += x * x
	}
	m /= 40000
	cs2 := m2/40000/(m*m) - 1
	// Each set's own utilization (its draws' mean service), against which
	// 1/(1 - rho) is read: a 1 % error in it is 4 % of the mean at rho 0.8.
	rho1, rho0 := d1.Checks.Rho[0], d0.Checks.Rho[0]
	assert.InEpsilon(t, LPSMean(rho1/0.8, rho1, cs2, 2), m1, 0.03)
	assert.InEpsilon(t, rho0/0.8/(1-rho0), m0, 0.03)
	assert.InEpsilon(t, rho1*rho1, d1.Checks.AdmissionShare, 1e-9)
	assert.Greater(t, q1[0], q0[0])
	assert.Zero(t, d0.Checks.AdmissionShare)
}

// The service tail index is read from the histogram's top two edges, and
// the sojourn's is Nair, Wierman and Zwart's min{theta, (theta - 1)(K - floor(K rho))}.
func TestRankMixture_TailIndex(t *testing.T) {
	// P(X > x) = (x/1000)^-2 above 1000.
	h := map[float64]float64{math.Inf(1): 1}
	for _, x := range []float64{1000, 2000, 4000, 8000, 16000} {
		h[x] = 1 - math.Pow(x/1000, -2)
	}
	hx, ok := newCumHist(h)
	require.True(t, ok)
	assert.InDelta(t, 2, hx.tailIndex(), 1e-9)
	m := RankMixture{S0: 0, V: 10000, Shares: []float64{1}, Slots: 4}
	d, u := m.Draws(0.5, h, h, 2000)
	require.Nil(t, u)
	es := d.Checks.Rho[0] / 0.5
	assert.InDelta(t, 2, d.Checks.ServiceTailIndex, 1e-9)
	assert.InDelta(t, 2, d.Checks.TailIndex, 1e-9, "light load: the service's own index")
	// At rho 0.8 one spare place is left of 4: (theta - 1) x 1, heavier than the service.
	d, u = m.Draws(0.8/es, h, h, 2000)
	require.Nil(t, u)
	rho := d.Checks.Rho[0]
	require.InDelta(t, 0.8, rho, 0.05)
	assert.InDelta(t, min(2, 1*(4-math.Floor(4*rho))), d.Checks.TailIndex, 1e-9)
	assert.InDelta(t, 1, d.Checks.TailIndex, 1e-9)
}

// lpsSim simulates one M/G/1-LPS(k) rank at Poisson rate lam: at most k jobs
// share the server equally, the rest wait first come. sizes draws a job's
// service time. It returns the mean buffer wait, the mean time in service
// and the mean sojourn over n jobs after a warm-up of n/10.
func lpsSim(rng *rand.Rand, lam float64, k, n int, size func() float64) (wait, service, sojourn float64) {
	type job struct{ arr, start, rem float64 }
	var running, queue []*job
	t, next := 0.0, rng.ExpFloat64()/lam
	done, warm := 0, n/10
	for done < n+warm {
		// The next completion: the least remaining work, served at 1/len.
		tc := math.Inf(1)
		ic := -1
		for i, j := range running {
			if c := t + j.rem*float64(len(running)); c < tc {
				tc, ic = c, i
			}
		}
		dt := min(tc, next) - t
		for _, j := range running {
			j.rem -= dt / float64(len(running))
		}
		t += dt
		if next <= tc {
			j := &job{arr: t, rem: size()}
			if len(running) < k {
				j.start = t
				running = append(running, j)
			} else {
				queue = append(queue, j)
			}
			next = t + rng.ExpFloat64()/lam
			continue
		}
		j := running[ic]
		running = append(running[:ic], running[ic+1:]...)
		if done >= warm {
			wait += j.start - j.arr
			service += t - j.start
			sojourn += t - j.arr
		}
		done++
		if len(queue) > 0 {
			q := queue[0]
			queue = queue[1:]
			q.start = t
			running = append(running, q)
		}
	}
	return wait / float64(n), service / float64(n), sojourn / float64(n)
}

// Synthetic recovery from a simulated LPS: ranks simulated as M/G/1-LPS(4)
// with a known service line are fitted back (PS inversion on their time in
// service), and the model's mean sojourn at the fitted line is within the
// approximation's error of the simulated one at moderate load. A larger
// error near the knee is Zhang and Zwart's, not the fit's, and is reported
// here as a bound, not a target.
func TestRankMixture_RecoversASimulatedLPS(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	s0, v, k := 0.2, 20000.0, 4
	h := twoPoint()
	hx, _ := newCumHist(h)
	size := func(scale float64) func() float64 {
		return func() float64 { return s0 + scale*hx.ppf(rng.Float64())/v }
	}
	steps := make([]MixtureStep, 0, 12)
	type sim struct{ lam, es, sojourn float64 }
	sims := make([]sim, 0, 12)
	for _, lam := range []float64{0.5, 1, 1.5, 2} {
		for _, scale := range []float64{0.6, 1, 1.4} {
			w, sv, so := lpsSim(rng, lam, k, 40000, size(scale))
			x := scale * 2450
			steps = append(steps, MixtureStep{Ranks: []MixtureRank{{Rate: lam, PrefillTime: sv, QueueTime: w, Computed: x}}})
			sims = append(sims, sim{lam, s0 + x/v, so})
		}
	}
	gs0, gv, _, err := FitPSInversion(steps)
	require.NoError(t, err)
	assert.InDelta(t, s0, gs0, 0.05, "S0")
	assert.InEpsilon(t, v, gv, 0.15, "V")
	for _, s := range sims {
		rho := s.lam * s.es
		// cs^2 of the service S0 + x/V at this scale (the prompt's mean is 2450).
		var m1, m2 float64
		for i := range 20000 {
			x := s0 + (s.es-s0)*hx.ppf((float64(i)+0.5)/20000)/2450
			m1 += x
			m2 += x * x
		}
		m1 /= 20000
		cs2 := m2/20000/(m1*m1) - 1
		lm := LPSMean(s.es, rho, cs2, k)
		t.Logf("rho %.3f cs2 %.2f: simulated mean sojourn %.4f s, LPS %.4f (%.3f), PS %.4f (%.3f)", rho, cs2, s.sojourn, lm,
			lm/s.sojourn, s.es/(1-rho), s.es/(1-rho)/s.sojourn)
		if rho <= 0.6 {
			assert.InEpsilon(t, s.sojourn, lm, 0.15, "rho %.2f", rho)
		}
	}
}

// Zhang and Zwart's mean against a simulated M/G/1-LPS(4) at a wide service
// spread (cs^2 about 5, as a P/D prefill rank reads): logged per load, and
// held within 20 % up to rho 0.6. Its authors tested it at cs^2 up to 19 in
// heavy traffic; this is the check at moderate load.
func TestLPSMean_AgainstASimulationAtAWideSpread(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	hx, _ := newCumHist(twoPoint())
	v := 10000.0
	size := func() float64 { return 0.02 + hx.ppf(rng.Float64())/v }
	var m1, m2 float64
	for i := range 40000 {
		x := 0.02 + hx.ppf((float64(i)+0.5)/40000)/v
		m1 += x
		m2 += x * x
	}
	es := m1 / 40000
	cs2 := m2/40000/(es*es) - 1
	for _, rho := range []float64{0.3, 0.45, 0.6, 0.75, 0.85} {
		_, _, so := lpsSim(rng, rho/es, 4, 200000, size)
		lm := LPSMean(es, rho, cs2, 4)
		t.Logf("rho %.2f cs2 %.2f: simulated %.4f s, LPS %.4f (%.3f), PS %.4f (%.3f)", rho, cs2, so, lm, lm/so, es/(1-rho), es/(1-rho)/so)
		if rho <= 0.6 {
			assert.InEpsilon(t, so, lm, 0.2, "rho %.2f", rho)
		}
	}
}

// At one prompt length the line is not identified: the service is taken
// proportional to the prompt, through the rank-steps' mean.
func TestFitPSInversion_OnePromptIsProportional(t *testing.T) {
	steps := make([]MixtureStep, 0, 10)
	for i := range 10 {
		// The prompt moves by a few tokens: a line fits exactly, but its
		// prompts spread far under MinComputedCV.
		lam, x := 0.5+0.2*float64(i), 8000+10*float64(i%3)
		es := 0.1 + x/50000
		steps = append(steps, MixtureStep{Ranks: []MixtureRank{{Rate: lam, PrefillTime: es / (1 - lam*es), QueueTime: 0, Computed: x}}})
	}
	s0, v, prop, err := FitPSInversion(steps)
	require.NoError(t, err)
	assert.True(t, prop)
	assert.Zero(t, s0)
	assert.InEpsilon(t, 8010/(0.1+8010/50000.0), v, 1e-3, "E[S] at the prompt read is kept")
}
