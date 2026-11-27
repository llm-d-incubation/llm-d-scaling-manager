package whatif

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// This file fits the experimental rank-mixture model (rank_mixture.go) from
// a recording's steps: each a few minutes of one fleet, with what its ranks
// and its router measured.

// MixtureRank is one prefill rank's means over a step.
type MixtureRank struct {
	// Rate is its requests a second.
	Rate float64
	// PrefillTime, QueueTime and TTFT are its mean prefill time (queue
	// excluded), queue time and TTFT, seconds.
	PrefillTime, QueueTime, TTFT float64
	// Computed and Prompt are its mean computed and whole prompt, tokens.
	Computed, Prompt float64
	// Frontend is its pod's API-server time outside the engine on a
	// generation request (domain.ReplicaMetrics.AvgFrontendTime), Render
	// the pod's mean /render handler time, seconds, and RenderRate the
	// rank's share of the pod's /render requests a second; each 0 where not
	// recorded (a measured frontend of exactly 0 reads as not recorded).
	// Only RankMixtureOptions.RemainderServer reads them.
	Frontend, Render, RenderRate float64
}

// MixtureStep is one step of a fleet, as the model fits and prices it.
type MixtureStep struct {
	At time.Time
	// Rate is the prefill ranks' requests a second.
	Rate float64
	// Ranks is the prefill ranks that served in the step.
	Ranks []MixtureRank
	// Computed, Prompt and Queue are the prefill ranks' pooled cumulative
	// histograms: computed tokens, whole prompt tokens and queue time
	// (seconds), upper bound to rate.
	Computed, Prompt, Queue map[float64]float64
	// PrefillTTFTHist and DecodeTTFTHist are the prefill and the decode
	// engines' pooled TTFT histograms (upper bound to rate); nil where not
	// read.
	PrefillTTFTHist, DecodeTTFTHist map[float64]float64
	// PrefillTTFT and DecodeTTFT are the prefill and the decode engines'
	// mean TTFT, seconds; PromptMean the prefill ranks' mean whole prompt.
	PrefillTTFT, DecodeTTFT, PromptMean float64
	// RouterMean and RouterP90 are the router's measured TTFT mean and p90,
	// seconds; 0 is not measured.
	RouterMean, RouterP90 float64
	// RouterProcessingHist is the router's request-processing histogram
	// (seconds, upper bound to rate) pooled over the step, and
	// RouterProcessingMean its mean, seconds; nil and 0 where the
	// recording does not hold them. Only RankMixtureOptions.RemainderSpread
	// reads them.
	RouterProcessingHist map[float64]float64
	RouterProcessingMean float64
	// RouterRenderMean is the router's mean render call (its token
	// producer's /render request to a prefill pod), seconds, inside its
	// request processing; 0 where not recorded. Only
	// RankMixtureOptions.RemainderServer reads it.
	RouterRenderMean float64
}

// minRankRate is the least rate a rank's means are read at: below it a
// step's means rest on a handful of requests.
const minRankRate = 0.05

// usable is the rank's means are all read.
func (r MixtureRank) usable() bool {
	return r.Rate > minRankRate && r.PrefillTime > 0 && r.QueueTime >= 0 && Finite(r.QueueTime) && r.Computed > 0
}

// xbar is the step's mean computed prompt; 0 with no rank usable.
func (s MixtureStep) xbar() float64 {
	var c, n float64
	for _, r := range s.Ranks {
		if r.Rate > 0 && r.Computed > 0 {
			c += r.Rate * r.Computed
			n += r.Rate
		}
	}
	if n == 0 {
		return 0
	}
	return c / n
}

// olsLine is the least-squares line y = a + b x; ok is false with fewer than
// two distinct x.
func olsLine(x, y []float64) (a, b float64, ok bool) {
	n := float64(len(x))
	if len(x) < 2 || len(x) != len(y) {
		return 0, 0, false
	}
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	mx /= n
	my /= n
	var sxx, sxy float64
	for i := range x {
		sxx += (x[i] - mx) * (x[i] - mx)
		sxy += (x[i] - mx) * (y[i] - my)
	}
	if !(sxx > 0) {
		return 0, 0, false
	}
	b = sxy / sxx
	return my - b*mx, b, true
}

// MinComputedCV is the least spread of the rank-steps' mean computed prompts
// (their coefficient of variation) from which FitPSInversion fits a line:
// below it every rank-step sits at about one prompt, and S0 and V cannot be
// told apart.
const MinComputedCV = 0.05

// FitPSInversion is S0 and V by processor-sharing inversion per rank: for
// M/G/1-PS, E[T] = E[S]/(1 - lambda E[S]), so E[S] = E[T]/(1 + lambda E[T]);
// the rank-steps' E[S] are then regressed on their mean computed prompt,
// E[S] = S0 + E[x]/V. S0 is held at 0 or above.
//
// Where the prompts do not spread (under MinComputedCV, a staircase at one
// prompt length) or the line does not rise, the line is not identified: the
// service is then taken proportional to the prompt, S0 0 and V the
// rank-steps' computed tokens over their service time (proportional true).
// It prices the prompts it was read at; it says nothing of a prompt far
// from them.
func FitPSInversion(steps []MixtureStep) (s0, v float64, proportional bool, err error) {
	var xs, es []float64
	for _, st := range steps {
		for _, r := range st.Ranks {
			if !r.usable() {
				continue
			}
			xs = append(xs, r.Computed)
			es = append(es, r.PrefillTime/(1+r.Rate*r.PrefillTime))
		}
	}
	if len(xs) == 0 {
		return 0, 0, false, errors.New("no rank read its request rate, prefill time and computed prompt")
	}
	a, b, ok := olsLine(xs, es)
	if ok && b > 0 && cvOf(xs) >= MinComputedCV {
		return max(a, 0), 1 / b, false, nil
	}
	var sx, se float64
	for i := range xs {
		sx += xs[i]
		se += es[i]
	}
	if !(se > 0) {
		return 0, 0, false, errors.New("the ranks' service times read 0")
	}
	return 0, sx / se, true, nil
}

// ShareProfile is the ranks' shares of the requests, busiest first, the
// mean over the steps where the most ranks served and carried at least one
// request a second; cv is the busiest share's coefficient of variation over
// those steps (the shares' stability).
func ShareProfile(steps []MixtureStep) (shares []float64, cv float64, err error) {
	n := 0
	for _, st := range steps {
		n = max(n, len(st.Ranks))
	}
	if n == 0 {
		return nil, 0, errors.New("no step has a rank")
	}
	sum := make([]float64, n)
	var tops []float64
	for _, st := range steps {
		if len(st.Ranks) != n {
			continue
		}
		var tot float64
		r := make([]float64, n)
		for i, rk := range st.Ranks {
			r[i] = max(rk.Rate, 0)
			tot += r[i]
		}
		if !(tot > 1) {
			continue
		}
		slices.SortFunc(r, func(a, b float64) int { return cmp.Compare(b, a) })
		for i := range r {
			sum[i] += r[i] / tot
		}
		tops = append(tops, r[0]/tot)
	}
	if len(tops) == 0 {
		return nil, 0, errors.New("no step has every rank serving")
	}
	for i := range sum {
		sum[i] /= float64(len(tops))
	}
	return sum, cvOf(tops), nil
}

func cvOf(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var m, v float64
	for _, x := range xs {
		m += x
	}
	m /= float64(len(xs))
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	v /= float64(len(xs))
	if !(m > 0) {
		return 0
	}
	return math.Sqrt(v) / m
}

// KV deferral edges, seconds: the queue-time histogram's bounds the fit reads.
const (
	kvEdgeLo = 0.3
	kvEdgeHi = 1.0
)

// FitKVDeferral fits the KV-deferral wait (KVDeferral) from each step's
// queue-time histogram: read as an atom at 0 and an exponential, the two
// edges give the mean m = (1 - 0.3)/ln(P(q>0.3)/P(q>1)) and the
// probability p = P(q>0.3) exp(0.3/m); each is then a line in the ranks'
// mean utilization rate/n (S0 + xbar/V). FITTED, not derived.
func FitKVDeferral(steps []MixtureStep, s0, v float64, ranks int) (KVDeferral, error) {
	var rhos, ps, ms []float64
	for _, st := range steps {
		total, ok1 := st.Queue[math.Inf(1)]
		lo, ok2 := st.Queue[kvEdgeLo]
		hi, ok3 := st.Queue[kvEdgeHi]
		if !ok1 || !ok2 || !ok3 || !(total > 0) || ranks == 0 {
			continue
		}
		p3, p1 := 1-lo/total, 1-hi/total
		if !(p3 > p1 && p1 > 0) {
			continue
		}
		m := (kvEdgeHi - kvEdgeLo) / math.Log(p3/p1)
		rhos = append(rhos, st.Rate/float64(ranks)*(s0+st.xbar()/v))
		ps = append(ps, min(p3*math.Exp(kvEdgeLo/m), 1))
		ms = append(ms, m)
	}
	p0, p1, ok := olsLine(rhos, ps)
	m0, m1, ok2 := olsLine(rhos, ms)
	if !ok || !ok2 {
		return KVDeferral{}, errors.New("the queue-time histograms do not read the 0.3 s and 1 s edges over two utilizations")
	}
	return KVDeferral{P0: p0, P1: p1, M0: m0, M1: m1, RhoLo: slices.Min(rhos), RhoHi: slices.Max(rhos), Known: true}, nil
}

// FitBeta is the seconds per whole-prompt token outside the prefill queue:
// the slope of the ranks' pre-queue overhead (TTFT less prefill and queue
// time) on their mean prompt, across ranks and steps, and the slopes of the
// decode engine's TTFT and of the router's remainder (router mean less both
// engines' TTFT) on the step's mean prompt. Each is held at 0 or above.
//
// A slope is fitted only where its prompts spread (a coefficient of
// variation of at least MinPromptSpreadCV): at one prompt length a slope
// against the prompt is noise, and it is then 0 and named in unidentified
// ("overhead", "decode", "remainder"); A0 then holds that cost at the
// prompt read.
func FitBeta(steps []MixtureStep) (overhead, decode, remainder float64, unidentified []string, err error) {
	lo, ov, ls, dec, rem := zSeries(steps)
	if len(lo) == 0 && len(ls) == 0 {
		return 0, 0, 0, nil, errors.New("the prompt-length slopes need ranks' TTFT or steps' decode and router means")
	}
	slope := func(name string, x, y []float64) float64 {
		_, b, ok := olsLine(x, y)
		if !ok || cvOf(x) < MinPromptSpreadCV {
			unidentified = append(unidentified, name)
			return 0
		}
		return max(b, 0)
	}
	overhead = slope("overhead", lo, ov)
	decode = slope("decode", ls, dec)
	remainder = slope("remainder", ls, rem)
	return overhead, decode, remainder, unidentified, nil
}

// zSeries is the non-queue part's three series on steps: each usable rank's
// mean prompt (lo) and pre-queue overhead (ov, TTFT less prefill and queue
// time), and each step's mean prompt (ls) with the decode engine's TTFT
// (dec) and the router's remainder (rem, router mean less both engines'
// TTFT).
func zSeries(steps []MixtureStep) (lo, ov, ls, dec, rem []float64) {
	for _, st := range steps {
		for _, r := range st.Ranks {
			if r.usable() && r.TTFT > 0 && r.Prompt > 0 {
				lo = append(lo, r.Prompt)
				ov = append(ov, r.TTFT-r.PrefillTime-r.QueueTime)
			}
		}
		if st.PromptMean > 0 && st.DecodeTTFT > 0 && st.PrefillTTFT > 0 && st.RouterMean > 0 {
			ls = append(ls, st.PromptMean)
			dec = append(dec, st.DecodeTTFT)
			rem = append(rem, st.RouterMean-st.PrefillTTFT-st.DecodeTTFT)
		}
	}
	return lo, ov, ls, dec, rem
}

// MinPromptSpreadCV is the least spread of the prompts (their coefficient of
// variation) a slope of FitBeta is fitted against: a fleet whose traffic
// mixes prompt lengths spreads its steps' mean prompt by more, a staircase
// at one prompt length by well under it.
const MinPromptSpreadCV = 0.03

// RankMixtureOptions are a fit's settings: K, the prompts a rank runs at
// once (Slots, 0 unknown: plain PS), the shortest fit window the spread is
// calibrated on (MinSpreadSpan, 0 any), and the parts an ablation drops.
// Each part dropped is priced as absent: the ranks at the mean share
// (NoMixture), no KV-deferral wait (NoKV), Beta 0 (NoPrompt), Spread 1
// (NoSpread).
type RankMixtureOptions struct {
	Slots         int
	MinSpreadSpan time.Duration
	// SpreadFloor is the least spread priced: a fitted spread under it
	// is raised to it (RankMixtureFit.SpreadFloored); 0 none.
	SpreadFloor                         float64
	NoMixture, NoKV, NoPrompt, NoSpread bool
	// CleanFitSteps fits the KV-deferral lines and the prompt slopes on the
	// steps under MaxCleanFitRho only; SharesAtLoad prices the share profile
	// of the fit steps nearest the priced rate (rank_mixture_fitsteps.go).
	// Both default off.
	CleanFitSteps, SharesAtLoad bool
	// StratifiedMoments prices with stratified service moments
	// (RankMixture.Stratified). The zero value is off; the planner sets it
	// from DefaultRankMixtureStratified unless the caller says otherwise.
	StratifiedMoments bool
	// KVHold holds the KV-deferral lines at the fit window's
	// KVHoldQuantile rank utilization, and RemainderSpread draws the
	// router's remainder from its measured request processing
	// (rank_mixture_modes.go). Both are modes of FitRankMixtureNL, each
	// off by default; FitRankMixture does not read them.
	KVHold, RemainderSpread bool
	// RemainderServer draws the router's remainder from the pods' API
	// servers and the router's own work on the remainder spread's shape
	// (rank_mixture_server.go); a mode of FitRankMixtureNL on top of
	// RemainderSpread, off by default, and nothing without it.
	RemainderServer bool
}

// MinSpreadFitWindow is the shortest fit window the planner calibrates the
// spread on (RankMixtureOptions.MinSpreadSpan); on a shorter one the spread
// is 1, the model's own expectation where Beta holds the prompt's whole
// effect, flagged (RankMixtureFit.SpreadDefaulted). The self-check folds
// of a short window are a few hours each and track that window's own tail,
// so the spread fitted on one moves with the hours it happens to cover.
const MinSpreadFitWindow = 12 * time.Hour

// MinSpread is the least spread the planner prices on a window it
// calibrates on (RankMixtureOptions.SpreadFloor): a fitted spread under 1
// would read the tail narrower than Beta's slopes do.
const MinSpread = 1.0

// FitCore fits every parameter but A0 and Spread on steps (Spread is set to
// 1), and the busiest share's cv (ShareProfile).
func FitCore(steps []MixtureStep, o RankMixtureOptions) (RankMixture, float64, error) {
	s0, v, proportional, err := FitPSInversion(steps)
	if err != nil {
		return RankMixture{}, 0, err
	}
	shares, cv, err := ShareProfile(steps)
	if err != nil {
		return RankMixture{}, 0, err
	}
	if o.NoMixture {
		for i := range shares {
			shares[i] = 1 / float64(len(shares))
		}
	}
	m := RankMixture{S0: s0, V: v, ServiceProportional: proportional, Shares: shares, Spread: 1, Slots: o.Slots,
		Stratified: o.StratifiedMoments}
	if o.SharesAtLoad && !o.NoMixture {
		m.ShareRows = shareRowsOf(steps, len(shares))
	}
	fitOn := steps
	if o.CleanFitSteps {
		fitOn, m.CleanSteps, m.OverloadedSteps = cleanFitSteps(steps, s0, v)
	}
	if !o.NoKV {
		if kv, err := FitKVDeferral(fitOn, s0, v, len(shares)); err == nil {
			m.KV = kv
		}
	}
	if !o.NoPrompt {
		if m.BetaOverhead, m.BetaDecode, m.BetaRemainder, m.BetaUnidentified, err = FitBeta(fitOn); err != nil {
			return RankMixture{}, 0, err
		}
		m.Beta = m.BetaOverhead + m.BetaDecode + m.BetaRemainder
	}
	return m, cv, nil
}

// pricedStep is one step's draws, and its measured figures.
type pricedStep struct {
	d          RankMixtureDraws
	mean, p90  float64
	beta       float64
	priceError bool
}

// drawSteps draws every step at m (A0 and Spread are applied later).
func drawSteps(m RankMixture, steps []MixtureStep) []pricedStep {
	out := make([]pricedStep, 0, len(steps))
	for _, st := range steps {
		d, u := m.Draws(st.Rate, st.Computed, st.Prompt, DefaultMixtureDraws)
		out = append(out, pricedStep{d: d, mean: st.RouterMean, p90: st.RouterP90, beta: m.Beta, priceError: u != nil})
	}
	return out
}

// a0Of is the location: the median over the steps of the measured mean less
// the model's mean at A0 0 and spread s.
func a0Of(ps []pricedStep, s float64) (float64, bool) {
	var d []float64
	for _, p := range ps {
		if p.priceError || !(p.mean > 0) {
			continue
		}
		m, _ := p.d.Stats(0, s*p.beta)
		d = append(d, p.mean-m)
	}
	if len(d) == 0 {
		return 0, false
	}
	return medianOf(d), true
}

// medianP90 is the median over the steps of predicted over measured p90, at
// A0 a0 and spread s.
func medianP90(ps []pricedStep, a0, s float64) (float64, bool) {
	var r []float64
	for _, p := range ps {
		if p.priceError || !(p.p90 > 0) {
			continue
		}
		_, q := p.d.Stats(a0, s*p.beta, 0.9)
		r = append(r, q[0]/p.p90)
	}
	if len(r) == 0 {
		return 0, false
	}
	return medianOf(r), true
}

func medianOf(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// stepLength is the median time between consecutive steps; 0 with one step.
func stepLength(steps []MixtureStep) time.Duration {
	if len(steps) < 2 {
		return 0
	}
	gaps := make([]float64, len(steps)-1)
	for i := 1; i < len(steps); i++ {
		gaps[i-1] = float64(steps[i].At.Sub(steps[i-1].At))
	}
	return time.Duration(medianOf(gaps))
}

// MaxSpread bounds the spread calibration; the bisection runs over [0, MaxSpread].
const MaxSpread = 8.0

// spreadIterations is the bisection's steps (8/2^14: about 5e-4).
const spreadIterations = 14

// RankMixtureFit is a fit's diagnostics.
type RankMixtureFit struct {
	// Steps is the fit window's steps priced; ShareCV the busiest share's
	// cv over them (ShareProfile).
	Steps   int
	ShareCV float64
	// FoldSpread is the spread each self-check fold alone fits (a fold
	// whose p90 cannot reach 1 inside [0, MaxSpread] reads its bound);
	// FitP90 the fit window's median p90 ratio at the spread chosen.
	FoldSpread []float64
	FitP90     float64
	// FitMean is the fit window's median ratio of the model's mean to the
	// router's measured one, where nothing was calibrated to it
	// (FitRankMixtureNL); 0 for v2, whose A0 holds that mean.
	FitMean float64
	// SpreadAtBound says the folds' p90 did not reach 1 inside
	// [0, MaxSpread], and the spread is that bound.
	SpreadAtBound bool
	// SpreadDefaulted says the fit window was shorter than MinSpreadSpan:
	// the spread is 1, not calibrated. Span is the fit window's length.
	SpreadDefaulted bool
	Span            time.Duration
	// SpreadFloored says the fitted spread was under
	// RankMixtureOptions.SpreadFloor and is priced at it.
	SpreadFloored bool
}

// fold is one self-check fold: its parameters fitted on what came before
// its cut, priced on the steps after it.
type fold struct {
	ps []pricedStep
}

// spreadFor is the spread at which the folds' pooled median p90 ratio is 1,
// A0 refitted over the folds at each spread.
func spreadFor(folds []fold) (s float64, atBound bool, ok bool) {
	n := 0
	for _, f := range folds {
		n += len(f.ps)
	}
	all := make([]pricedStep, 0, n)
	for _, f := range folds {
		all = append(all, f.ps...)
	}
	f := func(s float64) (float64, bool) {
		a0, ok := a0Of(all, s)
		if !ok {
			return 0, false
		}
		return medianP90(all, a0, s)
	}
	lo, hi := 0.0, MaxSpread
	flo, ok1 := f(lo)
	fhi, ok2 := f(hi)
	if !ok1 || !ok2 {
		return 0, false, false
	}
	if fhi < 1 {
		return hi, true, true
	}
	if flo > 1 {
		return lo, true, true
	}
	for range spreadIterations {
		mid := (lo + hi) / 2
		v, ok := f(mid)
		if !ok {
			return 0, false, false
		}
		if v < 1 {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2, false, true
}

// stepsBetween is the steps whose time is in (from, to].
func stepsBetween(steps []MixtureStep, from, to time.Time) []MixtureStep {
	var out []MixtureStep
	for _, s := range steps {
		if s.At.After(from) && !s.At.After(to) {
			out = append(out, s)
		}
	}
	return out
}

// FitRankMixture fits the model on the fit window's steps (in time order):
// the core on all of them (FitCore); the spread on two self-check folds of
// the window, cut at half and three quarters of its span, each fold's
// parameters fitted on the steps before its cut and priced on the steps up
// to the next (half to three quarters, three quarters to the end); and A0
// in-sample on the whole window at that spread.
func FitRankMixture(steps []MixtureStep, o RankMixtureOptions) (RankMixture, RankMixtureFit, error) {
	if len(steps) < 8 {
		return RankMixture{}, RankMixtureFit{}, fmt.Errorf("the rank mixture needs at least 8 steps to fit, got %d", len(steps))
	}
	m, cv, err := FitCore(steps, o)
	if err != nil {
		return RankMixture{}, RankMixtureFit{}, err
	}
	diag := RankMixtureFit{Steps: len(steps), ShareCV: cv}
	start := steps[0].At.Add(-time.Nanosecond)
	span := steps[len(steps)-1].At.Sub(start)
	cut := func(f float64) time.Time { return start.Add(time.Duration(f * float64(span))) }
	var folds []fold
	for _, c := range [][3]float64{{0, 0.5, 0.75}, {0, 0.75, 1}} {
		train := stepsBetween(steps, cut(c[0]), cut(c[1]))
		test := stepsBetween(steps, cut(c[1]), cut(c[2]))
		fm, _, err := FitCore(train, o)
		if err != nil {
			return RankMixture{}, RankMixtureFit{}, fmt.Errorf("self-check fold to %.0f%%: %w", 100*c[1], err)
		}
		folds = append(folds, fold{ps: drawSteps(fm, test)})
	}
	for _, f := range folds {
		s, _, ok := spreadFor([]fold{f})
		if ok {
			diag.FoldSpread = append(diag.FoldSpread, s)
		}
	}
	s, atBound, ok := 1.0, false, true
	// The window's length: its steps' span, plus one step for the first
	// step's own time.
	diag.Span = span + stepLength(steps)
	diag.SpreadDefaulted = diag.Span < o.MinSpreadSpan
	if !o.NoSpread && m.Beta > 0 && !diag.SpreadDefaulted {
		s, atBound, ok = spreadFor(folds)
	}
	if !ok {
		return RankMixture{}, RankMixtureFit{}, errors.New("the self-check folds price no step with a measured mean and p90")
	}
	if s < o.SpreadFloor {
		s, diag.SpreadFloored = o.SpreadFloor, true
	}
	m.Spread, diag.SpreadAtBound = s, atBound
	all := drawSteps(m, steps)
	a0, ok := a0Of(all, s)
	if !ok {
		return RankMixture{}, RankMixtureFit{}, errors.New("the fit window prices no step with a measured mean")
	}
	m.A0 = a0
	diag.FitP90, _ = medianP90(all, a0, s)
	return m, diag, nil
}
