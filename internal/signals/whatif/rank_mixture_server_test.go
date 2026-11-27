package whatif

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serverTruth is the remainder server the synthetic fit windows are drawn
// from: render 0.2 s + 1 us a token, frontend 10 ms + 0.5 us a token, the
// router's work outside render 70 ms, the rest 50 ms.
var serverTruth = ZServer{R0: 0.2, R1: 1e-6, F0: 0.01, F1: 5e-7, Other: 0.07, N0: 0.05, HasRender: true, HasFrontend: true}

// serverSteps is n 5-minute steps of four pods, each at its own prompt and
// rate, their API servers drawn from truth, and the router's remainder their
// rate-weighted sum with O and N.
func serverSteps(truth ZServer, n int) []MixtureStep {
	at := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	steps := make([]MixtureStep, n)
	for i := range steps {
		st := MixtureStep{At: at.Add(time.Duration(i) * 5 * time.Minute), PrefillTTFT: 1, DecodeTTFT: 0.5}
		var lsum, ren, fp float64
		for j := range 4 {
			l := 2000 + 4000*float64((i*7+j*3)%11)
			lam := 0.4 + 0.3*float64(j) + 0.05*float64(i%5)
			r := MixtureRank{Rate: lam, Prompt: l, Frontend: truth.frontend(l), Render: truth.render(l), RenderRate: lam}
			st.Ranks = append(st.Ranks, r)
			st.Rate += lam
			lsum += lam * l
			ren += lam * r.Render
			fp += lam * r.Frontend
		}
		st.PromptMean = lsum / st.Rate
		ren, fp = ren/st.Rate, fp/st.Rate
		st.RouterRenderMean = ren
		st.RouterProcessingMean = truth.Other + ren
		st.RouterMean = st.PrefillTTFT + st.DecodeTTFT + truth.Other + ren + fp + truth.N0
		steps[i] = st
	}
	return steps
}

// The fit reads back the lines the pods measured, the router's work outside
// its render call and the rest, and prices the remainder each step measured.
func TestFitZServer_ReadsTheLinesBack(t *testing.T) {
	steps := serverSteps(serverTruth, 24)
	z, off := FitZServer(steps, 300)
	require.Empty(t, off)
	require.NotNil(t, z)
	assert.True(t, z.HasRender)
	assert.True(t, z.HasFrontend)
	assert.InDelta(t, 0.2, z.R0, 1e-6)
	assert.InDelta(t, 1e-6, z.R1, 1e-10)
	assert.InDelta(t, 0.01, z.F0, 1e-6)
	assert.InDelta(t, 5e-7, z.F1, 1e-10)
	assert.InDelta(t, 0.07, z.Other, 1e-9)
	assert.InDelta(t, 0.05, z.N0, 1e-6)
	assert.InDelta(t, 0, z.N1, 1e-9)
	assert.Equal(t, 24, z.Steps)
	assert.Equal(t, 96, z.PodSteps)
	var top float64
	for _, st := range steps {
		// The lines are read at each pod's prompt and priced at the step's
		// mean: lines, so the rate-weighted mean is the same.
		assert.InDelta(t, st.RouterMean-st.PrefillTTFT-st.DecodeTTFT, z.Mean(st.PromptMean), 2e-3)
		top = max(top, st.Rate)
	}
	assert.Equal(t, top, z.TopRate)
	assert.Less(t, z.RestShare, 0.2, "the rest is a small part here")
}

// A pod-step far off its line moves the soft-L1 fit little; least squares
// would move it by its whole weight.
func TestFitZServer_AnOutlierPodStepMovesItLittle(t *testing.T) {
	steps := serverSteps(serverTruth, 24)
	steps[3].Ranks[1].Frontend = 5
	z, off := FitZServer(steps, 300)
	require.Empty(t, off)
	assert.InDelta(t, 0.01, z.F0, 2e-3)
	assert.InDelta(t, 5e-7, z.F1, 1e-7)
}

// The pods' frontend is optional: without it the render and the router's
// work are read as before, the frontend is 0 and its time is in the rest,
// the remainder is priced the same, and the assumed line says so.
func TestFitZServer_FrontendIsOptional(t *testing.T) {
	steps := serverSteps(serverTruth, 24)
	for i := range steps {
		for j := range steps[i].Ranks {
			steps[i].Ranks[j].Frontend = 0
		}
	}
	z, off := FitZServer(steps, 300)
	require.Empty(t, off)
	assert.False(t, z.HasFrontend)
	assert.Zero(t, z.F0)
	assert.Zero(t, z.frontend(10000))
	assert.InDelta(t, 0.2, z.R0, 1e-6)
	assert.Greater(t, z.N0+z.N1*10000, 0.05, "the frontend's time is in the rest")
	for _, st := range steps {
		assert.InDelta(t, st.RouterMean-st.PrefillTTFT-st.DecodeTTFT, z.Mean(st.PromptMean), 3e-3)
	}
	m := RankMixture{RemainderServer: true, Parts: &ZParts{Remainder: ZPart{Server: z}}}
	assert.Contains(t, m.modesAssumed(), "frontend not recorded (in the rest)")
}

// No pod render or frontend recorded, too few steps with one, or no router
// processing: nothing is fitted, and the reason says so.
func TestFitZServer_AbstainsWithoutTheSeries(t *testing.T) {
	steps := serverSteps(serverTruth, 24)
	none := make([]MixtureStep, len(steps))
	for i, st := range steps {
		st.Ranks = append([]MixtureRank(nil), st.Ranks...)
		for j := range st.Ranks {
			st.Ranks[j].Frontend, st.Ranks[j].Render, st.Ranks[j].RenderRate = 0, 0, 0
		}
		none[i] = st
	}
	z, off := FitZServer(none, 300)
	assert.Nil(t, z)
	assert.Contains(t, off, "fewer than 12 fit steps")

	z, _ = FitZServer(steps[:MinRemainderServerSteps-1], 300)
	assert.Nil(t, z, "one step short")
	z, _ = FitZServer(steps[:MinRemainderServerSteps], 300)
	assert.NotNil(t, z)

	noProc := make([]MixtureStep, len(steps))
	for i, st := range steps {
		st.RouterProcessingMean = 0
		noProc[i] = st
	}
	z, _ = FitZServer(noProc, 300)
	assert.Nil(t, z, "no router processing: no O")

	// A pod-step under minServerPodRequests is not read.
	z, _ = FitZServer(steps, 1)
	assert.Nil(t, z, "a second-long step reads no pod at its requests")
}

// Without the render handler on most pod-steps the render part is 0, and O
// is the router's whole processing as a line in the prompt; the frontend
// stands alone.
func TestFitZServer_NoRender(t *testing.T) {
	steps := serverSteps(serverTruth, 24)
	for i := range steps {
		for j := range steps[i].Ranks {
			steps[i].Ranks[j].Render, steps[i].Ranks[j].RenderRate = 0, 0
		}
		steps[i].RouterRenderMean = 0
	}
	z, off := FitZServer(steps, 300)
	require.Empty(t, off)
	assert.False(t, z.HasRender)
	assert.Zero(t, z.R0)
	assert.Zero(t, z.render(10000))
	assert.Positive(t, z.Other+z.Other1*10000)
	assert.InDelta(t, 0.01, z.F0, 1e-6)
}

// With the pods' render handler recorded but the router's render series
// not (a failed query, or a router that does not export it), the router's
// work outside render still leaves the render out: the pods' render line
// stands in for the router's render, so the render is not counted twice
// and the remainder's mean is the one measured.
func TestFitZServer_RouterRenderAbsent(t *testing.T) {
	for _, missing := range []int{24, 16} {
		steps := serverSteps(serverTruth, 24)
		for i := range missing {
			steps[i].RouterRenderMean = 0
		}
		z, off := FitZServer(steps, 300)
		require.Empty(t, off)
		assert.True(t, z.HasRender)
		assert.InDelta(t, 0.07, z.Other, 0.01, "%d steps without the router's render", missing)
		for _, st := range steps {
			assert.InDelta(t, st.RouterMean-st.PrefillTTFT-st.DecodeTTFT, z.Mean(st.PromptMean), 0.01,
				"%d steps without the router's render", missing)
		}
	}
}

// The rest the model cannot see: its share of the fit window's remainder is
// reported and named in the assumed line, and where it is over
// MaxRemainderRestShare a figure priced past the fit window's top rate is
// flagged; at or under that rate, or with a small rest, it is not.
func TestZServer_RestPastFit(t *testing.T) {
	big := serverTruth
	big.N0 = 1.5
	z, off := FitZServer(serverSteps(big, 24), 300)
	require.Empty(t, off)
	assert.Greater(t, z.RestShare, MaxRemainderRestShare)
	assert.True(t, z.restPastFit(1.01*z.TopRate))
	assert.False(t, z.restPastFit(z.TopRate))
	m := RankMixture{RemainderServer: true, Parts: &ZParts{Remainder: ZPart{Server: z}}}
	assert.Contains(t, m.modesAssumed(), "unmeasured rest")
	assert.Contains(t, m.modesAssumed(), string(RemainderRestPastFit))
	assert.Contains(t, RankMixtureChecks{RemainderRestPastFit: true}.Flags(0), RemainderRestPastFit)

	small, _ := FitZServer(serverSteps(serverTruth, 24), 300)
	assert.False(t, small.restPastFit(10*small.TopRate))
	ms := RankMixture{RemainderServer: true, Parts: &ZParts{Remainder: ZPart{Server: small}}}
	assert.Contains(t, ms.modesAssumed(), "unmeasured rest")
	assert.NotContains(t, ms.modesAssumed(), string(RemainderRestPastFit))
	var none *ZServer
	assert.False(t, none.restPastFit(100))
}

// The draws keep the server's mean on a mean-1 shape, the router part
// carries the shape's tail, and the same inputs draw the same requests.
func TestZServer_DrawsKeepTheMean(t *testing.T) {
	z, off := FitZServer(serverSteps(serverTruth, 24), 300)
	require.Empty(t, off)
	shape, _ := vincentShape([]map[float64]float64{processingHist()})
	l := make([]float64, 40000)
	for i := range l {
		l[i] = 2000 + float64(i%9)*2500
	}
	var lb float64
	for _, x := range l {
		lb += x
	}
	lb /= float64(len(l))
	d := z.draws(l, shape)
	var mean float64
	for _, x := range d {
		mean += x
	}
	mean /= float64(len(d))
	// The shape's tail is heavy: 40000 draws hold its mean to about 2 %.
	assert.InDelta(t, z.Mean(lb), mean, 0.025*z.Mean(lb))
	router := z.other(lb) + z.render(lb)
	assert.Greater(t, sampleQuantile(d, 0.95), z.Mean(lb)+router, "the router part is drawn on the shape's tail")
	assert.Equal(t, d, z.draws(l, shape))
}

// The remainder server on a fitted variant: drawn from the pods where the fit
// window recorded them, the spread itself where it did not (with the reason
// in the assumed line), and nothing at all without the spread. The fit
// window's checks never move; priced past the fit window's top rate with a
// rest over MaxRemainderRestShare, a draw is flagged.
func TestFitRankMixtureNL_RemainderServer(t *testing.T) {
	steps := nlSteps(t)
	srv := serverSteps(serverTruth, len(steps))
	for i := range steps {
		steps[i].RouterProcessingHist = processingHist()
		steps[i].RouterProcessingMean = srv[i].RouterProcessingMean
		steps[i].RouterRenderMean = srv[i].RouterRenderMean
	}
	o := nlOptions()
	o.RemainderSpread = true
	spread, spreadDiag, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	require.NotNil(t, spread.Parts.Remainder.Shape)

	// No pod render or frontend recorded: the plain spread, figure for figure.
	o.RemainderServer = true
	absent, absentDiag, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	assert.Nil(t, absent.Parts.Remainder.Server)
	assert.Contains(t, absent.RemainderServerOff, "fewer than 12 fit steps")
	assert.Contains(t, absent.Assumed(), "router remainder server not applied")
	assert.Equal(t, spreadDiag, absentDiag)
	comp, prom := expHist(2000), expHist(40000)
	ds, u := spread.Draws(steps[0].Rate, comp, prom, 4000)
	require.Nil(t, u)
	da, u := absent.Draws(steps[0].Rate, comp, prom, 4000)
	require.Nil(t, u)
	assert.Equal(t, ds.Z, da.Z, "abstained, the remainder is the spread's draws")

	// The pods' API servers recorded: the server draws the remainder.
	for i := range steps {
		for j := range steps[i].Ranks {
			if j < len(srv[i].Ranks) {
				r := srv[i].Ranks[j]
				steps[i].Ranks[j].Frontend, steps[i].Ranks[j].Render, steps[i].Ranks[j].RenderRate = r.Frontend, r.Render, r.RenderRate
			}
		}
	}
	on, onDiag, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	s := on.Parts.Remainder.Server
	require.NotNil(t, s, on.RemainderServerOff)
	assert.Empty(t, on.RemainderServerOff)
	assert.Contains(t, on.Assumed(), "router remainder from the pods' API servers")
	assert.Equal(t, spreadDiag, onDiag, "the mode never moves the fit window's checks")
	dn, u := on.Draws(steps[0].Rate, comp, prom, 4000)
	require.Nil(t, u)
	assert.NotEqual(t, ds.Z, dn.Z)
	assert.Equal(t, ds.QP, dn.QP, "the queue part is the same draws")
	dn, u = on.Draws(1.05*s.TopRate, comp, prom, 4000)
	require.Nil(t, u)
	assert.NotContains(t, dn.Checks.Flags(0), RemainderRestPastFit, "a small rest: past the top rate, no flag")

	// A router mean that carries far more than the pods measure: the rest is
	// most of the remainder, and a draw past the top rate is flagged.
	more := make([]MixtureStep, len(steps))
	for i, st := range steps {
		st.RouterMean += 2
		more[i] = st
	}
	big, _, err := FitRankMixtureNL(more, o)
	require.NoError(t, err)
	bs := big.Parts.Remainder.Server
	require.NotNil(t, bs, big.RemainderServerOff)
	require.Greater(t, bs.RestShare, MaxRemainderRestShare)
	db, u := big.Draws(1.05*bs.TopRate, comp, prom, 4000)
	require.Nil(t, u)
	assert.Contains(t, db.Checks.Flags(0), RemainderRestPastFit)
	db, u = big.Draws(bs.TopRate, comp, prom, 4000)
	require.Nil(t, u)
	assert.NotContains(t, db.Checks.Flags(0), RemainderRestPastFit, "at the top rate, no flag")
	ds, u = spread.Draws(1.05*bs.TopRate, comp, prom, 4000)
	require.Nil(t, u)
	assert.NotContains(t, ds.Checks.Flags(0), RemainderRestPastFit, "only the server flags it")

	// Asked without the spread: nothing.
	o.RemainderSpread = false
	alone, _, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	assert.False(t, alone.RemainderServer)
	assert.Nil(t, alone.Parts.Remainder.Server)
	assert.NotContains(t, alone.Assumed(), "remainder server")

	// Asked with the spread on a fit with no prompt recorded: no spread to
	// draw on, and the assumed line says the server was not applied rather
	// than saying nothing.
	o.RemainderSpread, o.NoPrompt = true, true
	blind, _, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	assert.True(t, blind.RemainderServer)
	assert.Nil(t, blind.Parts.Remainder.Server)
	assert.Equal(t, "no remainder spread to draw it on", blind.RemainderServerOff)
	assert.Contains(t, blind.Assumed(), "router remainder server not applied")
}

// The step pools each rank's API-server figures: the frontend weighted by
// rate, the render mean by the render rate, the render rate as the rate is
// (absent counts as none); the router's render mean is the mean of the
// cycles that read one.
func TestMixtureStepOf_PoolsTheAPIServer(t *testing.T) {
	// The render rates run against the request rates, so a render mean
	// weighted by the request rate reads 0.35, not 0.28.
	c1 := MixtureCycle{Ranks: []MixtureRankReading{{Key: "p0", MixtureRank: MixtureRank{Rate: 1, Frontend: 0.01, Render: 0.2,
		RenderRate: 3}}}, RouterRenderMean: 0.2}
	c2 := MixtureCycle{Ranks: []MixtureRankReading{{Key: "p0", MixtureRank: MixtureRank{Rate: 3, Frontend: 0.03, Render: 0.4,
		RenderRate: 2}}}}
	s := MixtureStepOf([]MixtureCycle{c1, c2}, time.Time{})
	require.Len(t, s.Ranks, 1)
	r := s.Ranks[0]
	assert.InDelta(t, 0.025, r.Frontend, 1e-12, "the frontend by request rate")
	assert.InDelta(t, 0.28, r.Render, 1e-12, "the render mean by render rate")
	assert.InDelta(t, 2.5, r.RenderRate, 1e-12)
	assert.Equal(t, 0.2, s.RouterRenderMean)

	s = MixtureStepOf([]MixtureCycle{{Ranks: []MixtureRankReading{{Key: "p0", MixtureRank: MixtureRank{Rate: 1}}}}}, time.Time{})
	assert.Zero(t, s.Ranks[0].Frontend)
	assert.Zero(t, s.Ranks[0].Render)
	assert.Zero(t, s.Ranks[0].RenderRate)
	assert.Zero(t, s.RouterRenderMean)
}

// nnLine is the constrained optimum: the free line where it is
// non-negative, else the better boundary.
func TestNNLine(t *testing.T) {
	a, b := nnLine([]float64{1, 2, 3}, []float64{3, 5, 7}, nil)
	assert.InDelta(t, 1, a, 1e-12)
	assert.InDelta(t, 2, b, 1e-12)
	a, b = nnLine([]float64{1, 2, 3}, []float64{3, 2, 1}, nil)
	assert.InDelta(t, 2, a, 1e-12, "a falling line: the mean")
	assert.Zero(t, b)
	a, b = nnLine([]float64{1, 2, 3}, []float64{-1, 1, 3}, nil)
	assert.Zero(t, a, "a negative intercept: through the origin")
	assert.InDelta(t, 10.0/14, b, 1e-12)
	a, b = nnLine([]float64{1, 2}, []float64{-1, -2}, nil)
	assert.Zero(t, a)
	assert.Zero(t, b)
	a, b = nnLine([]float64{1, 2, 3}, []float64{1, 1, 10}, []float64{1, 1, 0})
	assert.InDelta(t, 1, a, 1e-12, "a zero weight reads nothing")
	assert.Zero(t, b)
}
