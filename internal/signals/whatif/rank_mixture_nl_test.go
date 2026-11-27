package whatif

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fitZPart holds a coefficient the free line reads negative at 0 and refits
// the other; a feasible line is returned as is.
func TestFitZPart_NonNegative(t *testing.T) {
	x := []float64{1, 2, 3, 4}
	p := fitZPart(x, []float64{0.5, 2.5, 4.5, 6.5}) // free line: -1.5 + 2 x
	assert.True(t, p.Identified)
	assert.Zero(t, p.A)
	assert.InDelta(t, (0.5+5+13.5+26)/30, p.B, 1e-12, "the line through the origin")
	p = fitZPart(x, []float64{3, 2.5, 2, 1.5}) // free line: 3.5 - 0.5 x
	assert.Zero(t, p.B)
	assert.InDelta(t, 2.25, p.A, 1e-12, "the mean")
	p = fitZPart(x, []float64{3, 5, 7, 9})
	assert.InDelta(t, 1, p.A, 1e-12)
	assert.InDelta(t, 2, p.B, 1e-12)
	p = fitZPart(x, []float64{-3, -2, -1, -0.5})
	assert.Zero(t, p.A)
	assert.Zero(t, p.B)
	assert.Equal(t, ZPart{}, fitZPart(nil, nil))
}

// Z's parts are lines with non-negative intercepts and slopes; a part whose
// prompts do not spread is its median, not identified.
func TestFitZParts(t *testing.T) {
	steps := make([]MixtureStep, 0, 10)
	for i := range 10 {
		l := 20000 + 5000*float64(i)
		rs := make([]MixtureRank, 0, 3)
		for j := range 3 {
			lr := l + 1000*float64(j)
			rs = append(rs, MixtureRank{Rate: 1, PrefillTime: 0.5, QueueTime: 0.1, Computed: 2000, Prompt: lr,
				TTFT: 0.5 + 0.1 + 0.3 + 4e-6*lr})
		}
		dec := 0.4 + 3e-6*l
		pre := 0.9 + 4e-6*l
		// The remainder's line through the data would cross 0 below the
		// prompts read: -0.05 + 2e-6 l.
		steps = append(steps, MixtureStep{Ranks: rs, PromptMean: l, DecodeTTFT: dec, PrefillTTFT: pre,
			RouterMean: pre + dec - 0.05 + 2e-6*l})
	}
	p, un, err := FitZParts(steps)
	require.NoError(t, err)
	assert.Empty(t, un)
	assert.InDelta(t, 0.3, p.Overhead.A, 1e-9)
	assert.InDelta(t, 4e-6, p.Overhead.B, 1e-14)
	assert.InDelta(t, 0.4, p.Decode.A, 1e-9)
	assert.InDelta(t, 3e-6, p.Decode.B, 1e-14)
	assert.Zero(t, p.Remainder.A, "a negative intercept is held at 0")
	assert.Positive(t, p.Remainder.B)
	for i := range steps {
		steps[i].PromptMean = 50000
		for j := range steps[i].Ranks {
			steps[i].Ranks[j].Prompt = 50000
		}
	}
	p, un, err = FitZParts(steps)
	require.NoError(t, err)
	assert.Equal(t, []string{"overhead", "decode", "remainder"}, un)
	ov := make([]float64, 0, 30)
	for _, st := range steps {
		for _, r := range st.Ranks {
			ov = append(ov, r.TTFT-r.PrefillTime-r.QueueTime)
		}
	}
	assert.InDelta(t, medianOf(ov), p.Overhead.A, 1e-12, "the median of the part")
	assert.Zero(t, p.Overhead.B)
	_, _, err = FitZParts([]MixtureStep{{}})
	assert.Error(t, err)
}

// FitRankMixtureNL: v2's service line, v2's fit-step and stratified switches
// on, Z as the sum of its parts at spread 1, and a fit window that prices
// its own router mean.
func TestFitRankMixtureNL(t *testing.T) {
	truth := RankMixture{S0: 0.1, V: 8000, Shares: []float64{0.3, 0.25, 0.25, 0.2}, Spread: 1, Slots: 4,
		BetaOverhead: 3e-6, BetaDecode: 2e-6, Beta: 5e-6}
	steps := syntheticSteps(t, truth, 48)
	// syntheticSteps prices Z at A0 0: its parts' constants (0.3 s
	// overhead, 0.4 s decode) are in the engines' means, not the router's,
	// so the router mean here gets them added.
	for i := range steps {
		steps[i].RouterMean += 0.7
		steps[i].RouterP90 += 0.7
	}
	m, diag, err := FitRankMixtureNL(steps, RankMixtureOptions{Slots: 4, MinSpreadSpan: MinSpreadFitWindow})
	require.NoError(t, err)
	s0, v, _, err := FitPSInversion(steps)
	require.NoError(t, err)
	assert.InDelta(t, s0, m.S0, 1e-12, "v2's service line")
	assert.InDelta(t, v, m.V, 1e-9)
	assert.True(t, m.Stratified, "v2's stratified moments")
	assert.NotNil(t, m.ShareRows, "v2's shares at the load")
	assert.Equal(t, 48, m.CleanSteps+m.OverloadedSteps, "v2's clean fit steps")
	assert.Equal(t, 1.0, m.Spread)
	require.NotNil(t, m.Parts)
	assert.InDelta(t, m.Parts.Overhead.A+m.Parts.Decode.A+m.Parts.Remainder.A, m.A0, 1e-12)
	assert.InDelta(t, m.Parts.Overhead.B+m.Parts.Decode.B+m.Parts.Remainder.B, m.Beta, 1e-15)
	assert.GreaterOrEqual(t, m.A0, 0.0)
	assert.InDelta(t, 0.3, m.Parts.Overhead.A, 0.02)
	assert.InDelta(t, 0.4, m.Parts.Decode.A, 0.02)
	assert.InDelta(t, 1, diag.FitMean, 0.1)
	assert.Equal(t, 48, diag.Steps)
	assert.Equal(t, 4*time.Hour, diag.Span, "48 steps of 5 min")
	assert.True(t, diag.SpreadDefaulted, "a 4 h window is under MinSpreadFitWindow")
	assert.Contains(t, m.Assumed(), string(QueueRankMixtureNLLine))
	assert.Contains(t, m.Assumed(), "KV-deferral and prompt slopes fitted on", "the fit-step switches say what they read")
	_, _, err = FitRankMixtureNL(steps[:7], RankMixtureOptions{})
	assert.Error(t, err)
}

// A lognormal's sigma is read back from its mean and p90; the histogram's
// quantile interpolates inside a bucket.
func TestLnSigmaAndHistQuantile(t *testing.T) {
	sigma := 0.6
	p90 := math.Exp(-sigma*sigma/2 + z90*sigma) // mean 1
	got, ok := lnSigma(1, p90)
	require.True(t, ok)
	assert.InDelta(t, sigma, got, 1e-9)
	_, ok = lnSigma(0, 1)
	assert.False(t, ok)
	h := map[float64]float64{1: 0.5, 2: 0.8, 4: 1, math.Inf(1): 1}
	q, ok := histP90(h)
	require.True(t, ok)
	assert.InDelta(t, 3, q, 1e-12)
	_, ok = histP90(map[float64]float64{})
	assert.False(t, ok)
}

// The parts' spreads keep Z's mean and lengthen its tail; with no sigma the
// draws carry no Z and price a0 + beta L as before.
func TestZParts_SpreadIsMeanPreserving(t *testing.T) {
	m := RankMixture{S0: 0.1, V: 10000, Shares: []float64{1}, A0: 0.9, Beta: 0, Parts: &ZParts{
		Overhead: ZPart{A: 0.5}, Decode: ZPart{A: 0.4}}}
	d, u := m.Draws(0.5, expHist(2000), pointHist(40000), 8000)
	require.Nil(t, u)
	assert.Nil(t, d.Z)
	m0, q0 := d.Stats(m.A0, 0, 0.9)
	m.Parts.Overhead.Sigma, m.Parts.Decode.Sigma = 0.8, 0.5
	d, u = m.Draws(0.5, expHist(2000), pointHist(40000), 8000)
	require.Nil(t, u)
	require.NotNil(t, d.Z)
	m1, q1 := d.Stats(m.A0, 0, 0.9)
	assert.InDelta(t, m0, m1, 0.02*m0)
	assert.Greater(t, q1[0], q0[0]*1.05)
}

// The decode part's sigma is read from the decode engines' TTFT histogram.
func TestFitSigmas_DecodeFromItsHistogram(t *testing.T) {
	sigma := 0.5
	mean := 0.4
	p90 := mean * math.Exp(-sigma*sigma/2+z90*sigma)
	steps := make([]MixtureStep, 0, 6)
	for range 6 {
		steps = append(steps, MixtureStep{Rate: 1, DecodeTTFT: mean,
			DecodeTTFTHist: map[float64]float64{p90 * 0.5: 0.4, p90: 0.9, p90 * 2: 1, math.Inf(1): 1}})
	}
	m := RankMixture{S0: 0.1, V: 10000, Shares: []float64{1}, Parts: &ZParts{Decode: ZPart{A: mean}}}
	m.fitSigmas(steps)
	assert.InDelta(t, sigma, m.Parts.Decode.Sigma, 1e-9)
	assert.Zero(t, m.Parts.Overhead.Sigma, "no prefill TTFT histogram: no overhead spread")
}

// The overhead's sigma is the one at which the priced prefill-engine TTFT
// (overhead + queue + prefill) reads the engines' measured p90 on the steps
// given: a p90 the line already reaches leaves it at 0, and one no sigma
// reaches leaves it at MaxPartSigma.
func TestFitSigmas_OverheadReadsThePrefillP90(t *testing.T) {
	const want = 0.6
	m := RankMixture{S0: 0.1, V: 10000, Shares: []float64{0.6, 0.4}, Parts: &ZParts{Overhead: ZPart{A: 0.5, B: 2e-6}}}
	stepsAt := func(sigma, scale float64) []MixtureStep {
		steps := make([]MixtureStep, 0, 5)
		for i := range 5 {
			st := MixtureStep{Rate: 1 + float64(i), Computed: expHist(2000), Prompt: expHist(40000)}
			d, u := m.Draws(st.Rate, st.Computed, st.Prompt, DefaultMixtureDraws)
			require.Nil(t, u)
			part := m.Parts.Overhead
			part.Sigma = sigma
			d.Z = drawParts(d.L, []ZPart{part})
			_, q := d.Stats(0, 0, 0.9)
			p90 := q[0] * scale
			st.PrefillTTFTHist = map[float64]float64{p90 / 2: 0.5, p90: 0.9, 2 * p90: 1, math.Inf(1): 1}
			steps = append(steps, st)
		}
		return steps
	}
	m.fitSigmas(stepsAt(want, 1))
	assert.InDelta(t, want, m.Parts.Overhead.Sigma, 0.01)
	assert.Zero(t, m.Parts.Decode.Sigma, "no decode TTFT histogram: no decode spread")
	m.Parts.Overhead.Sigma = 0
	m.fitSigmas(stepsAt(0, 0.95))
	assert.Zero(t, m.Parts.Overhead.Sigma, "the line already reads the p90")
	m.fitSigmas(stepsAt(MaxPartSigma, 1.5))
	assert.Equal(t, MaxPartSigma, m.Parts.Overhead.Sigma, "no sigma reaches the p90: the bound")
}
