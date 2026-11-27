package whatif

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
)

// This file is a variant of the experimental rank-mixture TTFT model
// (rank_mixture.go), QueueRankMixtureNLLine. The queue and the service line
// are v2's: limited processor sharing per prefill rank on S0 + x/V, mixed
// over the ranks' shares, with the fitted KV-deferral wait. What differs is
// how the inputs are fitted.
//
// Fit steps. v2's two fit-step fixes and its stratified moments are always
// on (RankMixtureOptions CleanFitSteps, SharesAtLoad, StratifiedMoments;
// rank_mixture_fitsteps.go, rank_mixture_stratified.go): the KV-deferral
// lines and Z's parts are fitted on the steps under MaxCleanFitRho, the
// shares are read at the priced load, and each rank's service moments are
// integrated over the computed-token law.
//
// Non-queue part, measured part by part (ZParts). Z is the engine's work
// before its queue (the ranks' TTFT less their prefill and queue time), the
// decode engine's first token and the router's remainder (its mean less both
// engines'), each a line in the prompt with a non-negative intercept and
// slope fitted on that part's own series (zSeries, as FitBeta reads them).
// Z's constant A0 is the sum of the intercepts, so it is a time by
// construction, and Z is not calibrated: Spread is 1. v2's calibration of
// the spread to the fit window's p90, with A0 refitted to hold the mean,
// reads load-dependent misfit as prompt cost: a spread that grows moves A0
// below zero.
//
// Abstention. With no location or spread calibrated to the router's TTFT,
// the fit window's median ratio of the model's mean and p90 to the router's
// measured ones (RankMixtureFit FitMean, FitP90) are checks. They are in
// sample: the remainder is the router's mean less both engines' means, so
// FitMean reads mostly the prefill engine's queue and service against its
// measured TTFT, and FitP90 the spreads. The backtest abstains outside [MinNLFitMean, MaxNLFitMean] and
// [MinNLFitP90, MaxNLFitMean]. Those checks mean something only on a window
// that holds the loads and prompts it will be asked about: on a fit window
// under MinSpreadFitWindow it abstains too.

// QueueRankMixtureNLLine is the experimental TTFT model with v2's queue and
// service line, and Z measured part by part (rank_mixture_nl.go).
// Experimental; only a caller that asks prices it.
const QueueRankMixtureNLLine QueueModel = "rank-mixture-nl-line"

// MinNLFitMean and MaxNLFitMean bound the fit window's median mean ratio
// (RankMixtureFit.FitMean) at which QueueRankMixtureNLLine prices, and
// MinNLFitP90 and MaxNLFitMean its median p90 ratio (FitP90). The p90's
// floor is tighter: a tail short on the window it was fitted on reads
// shorter where the load is higher.
const (
	MinNLFitMean = 0.8
	MinNLFitP90  = 0.9
	MaxNLFitMean = 1.25
)

// ZPart is one part of Z as a line in the whole prompt: A seconds plus B
// seconds a prompt token, both non-negative. Identified is false where the
// prompts did not spread (B is then 0 and A the part's median). Sigma is
// the part's spread: each request's part is lognormal about the line's mean
// with this sigma (mean-preserving); 0 is the line itself. Shape, where set,
// draws the part from a measured law in place of Sigma's lognormal
// (remainder spread, rank_mixture_modes.go): Shape is that law's quantiles
// over its mean on shapeGrid's mid-points, and ShapeMean its mean, seconds.
// Server, where set on the remainder (remainder server,
// rank_mixture_server.go), draws the part from the prefill pods' API
// servers and the router's own work on Shape in place of the line.
type ZPart struct {
	A, B       float64
	Identified bool
	Sigma      float64
	Shape      []float64
	ShapeMean  float64
	Server     *ZServer
}

// ZParts is Z measured part by part.
type ZParts struct {
	Overhead, Decode, Remainder ZPart
}

// fitZPart is one part's line on (x prompt, y seconds): the least-squares
// line with a non-negative intercept and slope, the slope only where the
// prompts spread (MinPromptSpreadCV); else the median, not identified. With
// two coefficients the constrained optimum is the free line where it is
// feasible, else the better of the line through the origin and the mean.
func fitZPart(x, y []float64) ZPart {
	if len(y) == 0 {
		return ZPart{}
	}
	a, b, ok := olsLine(x, y)
	if !ok || cvOf(x) < MinPromptSpreadCV {
		return ZPart{A: max(medianOf(y), 0)}
	}
	if a >= 0 && b >= 0 {
		return ZPart{A: a, B: b, Identified: true}
	}
	var sy, sxx, sxy float64
	for i := range x {
		sy += y[i]
		sxx += x[i] * x[i]
		sxy += x[i] * y[i]
	}
	sse := func(a, b float64) float64 {
		var e float64
		for i := range x {
			r := y[i] - a - b*x[i]
			e += r * r
		}
		return e
	}
	best := ZPart{Identified: true}
	bestE := sse(0, 0)
	if m := sy / float64(len(y)); m > 0 && sse(m, 0) < bestE {
		best, bestE = ZPart{A: m, Identified: true}, sse(m, 0)
	}
	if sxx > 0 {
		if s := sxy / sxx; s > 0 && sse(0, s) < bestE {
			best = ZPart{B: s, Identified: true}
		}
	}
	return best
}

// FitZParts is Z measured part by part on steps (zSeries): the ranks'
// pre-queue overhead on their mean prompt, and the decode engine's TTFT and
// the router's remainder on the step's mean prompt. unidentified names the
// parts whose slope the prompts did not identify.
func FitZParts(steps []MixtureStep) (p ZParts, unidentified []string, err error) {
	lo, ov, ls, dec, rem := zSeries(steps)
	if len(lo) == 0 || len(ls) == 0 {
		return ZParts{}, nil, errors.New("the non-queue parts need the ranks' TTFT and the steps' decode and router means")
	}
	p = ZParts{Overhead: fitZPart(lo, ov), Decode: fitZPart(ls, dec), Remainder: fitZPart(ls, rem)}
	for _, part := range []struct {
		name string
		z    ZPart
	}{{"overhead", p.Overhead}, {"decode", p.Decode}, {"remainder", p.Remainder}} {
		if !part.z.Identified {
			unidentified = append(unidentified, part.name)
		}
	}
	return p, unidentified, nil
}

// Spreads of Z's parts. Z's parts are not constants per prompt: the front
// end, the decode engine's first token (its own queue and the KV transfer)
// and the router vary request to request. Each part is drawn lognormal about
// its line with a sigma read from that part's own measured series, so the
// spread moves the tail and not the mean. The decode part's sigma is read
// from the decode engines' TTFT histogram (its mean and p90); the overhead's
// is the one at which the model's prefill-engine TTFT (overhead + queue +
// prefill) reads the prefill engines' measured TTFT p90 (median ratio 1 over
// the clean fit steps). The router's remainder has no histogram of its own:
// its sigma is 0. A lognormal's p90 over its mean rises with sigma only up
// to z90 (1.28), so the search runs over [0, MaxPartSigma].

// MaxPartSigma bounds a part's sigma.
const MaxPartSigma = 1.2

// z90 is the standard normal's 0.9 quantile.
const z90 = 1.2815515655446004

// zSeed fixes the parts' draws.
const zSeed = 0x5eed + 99

// draws is each request's Z at its prompt l: every part lognormal about its
// line, or drawn from its shape, and the remainder from its server where it
// has one; nil where p is nil or no part is spread (Z is then the lines'
// sum, priced as a0 + beta L).
func (p *ZParts) draws(l []float64) []float64 {
	if p == nil || (!p.Overhead.spread() && !p.Decode.spread() && !p.Remainder.spread()) {
		return nil
	}
	if s := p.Remainder.Server; s != nil && p.Remainder.Shape != nil {
		out := drawParts(l, []ZPart{p.Overhead, p.Decode})
		for i, r := range s.draws(l, p.Remainder.Shape) {
			out[i] += r
		}
		return out
	}
	return drawParts(l, []ZPart{p.Overhead, p.Decode, p.Remainder})
}

// spread says the part is drawn, not read as its line.
func (z ZPart) spread() bool {
	return z.Sigma > 0 || z.Shape != nil
}

// drawParts is each request's sum of parts at its prompt l. A part with a
// shape is its line less the shape's mean, held at 0, plus a draw of the
// shape; its mean is the line's wherever the line is at or above the
// shape's mean.
func drawParts(l []float64, parts []ZPart) []float64 {
	rng := rand.New(rand.NewPCG(zSeed, uint64(len(l))))
	out := make([]float64, len(l))
	for i, li := range l {
		for _, z := range parts {
			mu := max(z.A+z.B*li, 0)
			switch {
			case z.Shape != nil:
				mu = max(mu-z.ShapeMean, 0) + z.ShapeMean*shapeAt(z.Shape, rng.Float64())
			case z.Sigma > 0:
				mu *= math.Exp(z.Sigma*rng.NormFloat64() - z.Sigma*z.Sigma/2)
			}
			out[i] += mu
		}
	}
	return out
}

// lnSigma is the sigma of the lognormal with this mean and p90, the smaller
// root, at most z90; ok is false where either is not positive.
func lnSigma(mean, p90 float64) (float64, bool) {
	if !(mean > 0) || !(p90 > 0) {
		return 0, false
	}
	disc := z90*z90 - 2*math.Log(p90/mean)
	if disc <= 0 {
		return z90, true
	}
	return max(z90-math.Sqrt(disc), 0), true
}

// histP90 is the 0.9 quantile of a cumulative histogram (upper bound to
// rate, +Inf the total), linear inside a bucket and the lower edge in the
// +Inf one; ok is false where it holds nothing.
func histP90(h map[float64]float64) (float64, bool) {
	const q = 0.9
	tot := h[math.Inf(1)]
	if !(tot > 0) {
		return 0, false
	}
	les := make([]float64, 0, len(h))
	for le := range h {
		les = append(les, le)
	}
	slices.Sort(les)
	target, prevLe, prevC := q*tot, 0.0, 0.0
	for _, le := range les {
		c := h[le]
		if c >= target {
			if math.IsInf(le, 1) {
				return prevLe, true
			}
			if c == prevC {
				return le, true
			}
			return prevLe + (le-prevLe)*(target-prevC)/(c-prevC), true
		}
		prevLe, prevC = le, c
	}
	return 0, false
}

// poolHist is out with h added at weight 1/n; nil while neither holds any.
func poolHist(out, h map[float64]float64, n float64) map[float64]float64 {
	for b, v := range h {
		if out == nil {
			out = map[float64]float64{}
		}
		out[b] += v / n
	}
	return out
}

// fitSigmas sets the parts' sigmas on m (spreads of Z's parts, above) from
// the clean steps.
func (m *RankMixture) fitSigmas(clean []MixtureStep) {
	p := m.Parts
	var ds []float64
	for _, st := range clean {
		if q, ok := histP90(st.DecodeTTFTHist); ok {
			if s, ok := lnSigma(st.DecodeTTFT, q); ok {
				ds = append(ds, s)
			}
		}
	}
	if len(ds) > 0 {
		p.Decode.Sigma = min(medianOf(ds), MaxPartSigma)
	}
	type obs struct {
		d    RankMixtureDraws
		meas float64
	}
	var os []obs
	for _, st := range clean {
		q, ok := histP90(st.PrefillTTFTHist)
		if !ok || !(q > 0) {
			continue
		}
		d, u := m.Draws(st.Rate, st.Computed, st.Prompt, DefaultMixtureDraws)
		if u != nil {
			continue
		}
		os = append(os, obs{d, q})
	}
	if len(os) == 0 {
		return
	}
	ratio := func(sigma float64) float64 {
		part := p.Overhead
		part.Sigma = sigma
		r := make([]float64, 0, len(os))
		for _, o := range os {
			d := o.d
			d.Z = drawParts(d.L, []ZPart{part})
			_, q := d.Stats(0, 0, 0.9)
			r = append(r, q[0]/o.meas)
		}
		return medianOf(r)
	}
	lo, hi := 0.0, MaxPartSigma
	if ratio(lo) >= 1 {
		return
	}
	if ratio(hi) < 1 {
		p.Overhead.Sigma = hi
		return
	}
	for range spreadIterations {
		mid := (lo + hi) / 2
		if ratio(mid) < 1 {
			lo = mid
		} else {
			hi = mid
		}
	}
	p.Overhead.Sigma = (lo + hi) / 2
}

// FitRankMixtureNL fits QueueRankMixtureNLLine on the fit window's steps
// (in time order): the core with v2's fit-step fixes and stratified moments
// on (FitCore), Z's parts and their spreads on the clean steps, and A0 and
// Beta as Z's sums at Spread 1. o's Slots, MinSpreadSpan, NoMixture, NoKV
// and NoPrompt apply; its spread options do not.
func FitRankMixtureNL(steps []MixtureStep, o RankMixtureOptions) (RankMixture, RankMixtureFit, error) {
	if len(steps) < 8 {
		return RankMixture{}, RankMixtureFit{}, fmt.Errorf("the rank mixture needs at least 8 steps to fit, got %d", len(steps))
	}
	noPrompt := o.NoPrompt
	o.CleanFitSteps, o.SharesAtLoad, o.StratifiedMoments = true, true, true
	// Z is fitted by part below, not by FitBeta.
	o.NoPrompt = true
	m, cv, err := FitCore(steps, o)
	if err != nil {
		return RankMixture{}, RankMixtureFit{}, err
	}
	clean, _, _ := cleanFitSteps(steps, m.S0, m.V)
	diag := RankMixtureFit{Steps: len(steps), ShareCV: cv}
	if len(steps) > 1 {
		diag.Span = steps[len(steps)-1].At.Sub(steps[0].At) + stepLength(steps)
	}
	diag.SpreadDefaulted = diag.Span < o.MinSpreadSpan
	parts := ZParts{}
	if !noPrompt {
		if parts, m.BetaUnidentified, err = FitZParts(clean); err != nil {
			return RankMixture{}, RankMixtureFit{}, err
		}
	}
	m.Parts = &parts
	m.A0 = parts.Overhead.A + parts.Decode.A + parts.Remainder.A
	m.BetaOverhead, m.BetaDecode, m.BetaRemainder = parts.Overhead.B, parts.Decode.B, parts.Remainder.B
	m.Beta = m.BetaOverhead + m.BetaDecode + m.BetaRemainder
	if !noPrompt {
		m.fitSigmas(clean)
	}
	all := drawSteps(m, steps)
	var means []float64
	for _, p := range all {
		if p.priceError || !(p.mean > 0) {
			continue
		}
		mean, _ := p.d.Stats(m.A0, m.Beta)
		means = append(means, mean/p.mean)
	}
	if len(means) == 0 {
		return RankMixture{}, RankMixtureFit{}, errors.New("the fit window prices no step with a measured mean")
	}
	diag.FitMean = medianOf(means)
	diag.FitP90, _ = medianP90(all, m.A0, 1)
	// The modes are set on the fitted model: the fit window's checks above
	// read it without them, so a mode never moves whether the model prices.
	m.KVHold, m.RemainderSpread = o.KVHold, o.RemainderSpread
	if o.KVHold && m.KV.Known {
		m.KV.RhoHold = kvHoldOf(all)
	}
	if o.RemainderSpread && !noPrompt {
		parts.Remainder.Shape, parts.Remainder.ShapeMean = remainderShape(steps)
	}
	if o.RemainderServer && o.RemainderSpread {
		// Asked for, it is named whether or not it applies: with no prompt
		// recorded there is no spread, and the assumed line says so.
		m.RemainderServer = true
		if parts.Remainder.Shape == nil {
			m.RemainderServerOff = "no remainder spread to draw it on"
		} else {
			parts.Remainder.Server, m.RemainderServerOff = FitZServer(steps, stepLength(steps).Seconds())
		}
	}
	return m, diag, nil
}

// assumedNL is Assumed for QueueRankMixtureNLLine.
func (m RankMixture) assumedNL() string {
	busiest := 0.0
	if len(m.Shares) > 0 {
		busiest = m.Shares[0] * float64(len(m.Shares))
	}
	kv := "no KV-deferral wait (not fitted)"
	if m.KV.Known {
		kv = fmt.Sprintf("KV-deferral wait p_d = %.3g + %.3g rho, m_d = %.3g + %.3g rho s (fitted over rho %.3g-%.3g)",
			m.KV.P0, m.KV.P1, m.KV.M0, m.KV.M1, m.KV.RhoLo, m.KV.RhoHi)
	}
	queue := fmt.Sprintf("%s (K = %d)", QueueLimitedProcessorSharing, m.Slots)
	if m.Slots <= 0 {
		queue = string(QueueProcessorSharing) + " (max-num-seqs not read)"
	}
	p := m.Parts
	return fmt.Sprintf("experimental TTFT model %s (OUTSIDE VALIDATED REGION): %s per rank on %d ranks "+
		"(busiest share %.3g of the mean), S(x) = %.3g s + x / %.4g tok/s; %s; Z measured by part, not calibrated: "+
		"overhead %.3g s + %.3g us x L (sigma %.3g), decode %.3g s + %.3g us x L (sigma %.3g), router remainder %.3g s + %.3g us x L%s%s",
		QueueRankMixtureNLLine, queue, len(m.Shares), busiest, m.S0, m.V, kv, p.Overhead.A, p.Overhead.B*1e6, p.Overhead.Sigma,
		p.Decode.A, p.Decode.B*1e6, p.Decode.Sigma, p.Remainder.A, p.Remainder.B*1e6, m.modesAssumed(), m.fitStepsAssumed())
}
