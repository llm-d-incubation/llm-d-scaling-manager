package whatif

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The rank-mixture TTFT model's fit on cycles (cycle_fits.go): the steps cut
// from the cycles, the fit, its identifiability checks, and the window it is
// priced on. The capacity planner calls it on a recording's cycles, the slo
// analyzer on the controller's own.

// RankMixtureReport is the experimental model's fit, as a backtest's answer
// carries it (whatifapi.BacktestResponse.RankMixture) and the slo analyzer
// logs it.
type RankMixtureReport struct {
	// Model is QueueRankMixture; Experimental is always true.
	Model        string `json:"model"`
	Experimental bool   `json:"experimental"`
	// Abstained is why the model prices nothing, "" where it prices: an
	// identifiability check failed (a prompt slope not identified, the
	// spread not calibrated, the service line not identified, the ranks'
	// own rates or K not read) or the fit failed. Every TTFT is then the
	// planner's own, flagged RankMixtureAbstained. The fitted figures
	// below, where there are any, are diagnostics.
	Abstained string `json:"abstained,omitempty"`
	// Assumed is the fitted parameters in words, also in the answer's
	// assumed lines.
	Assumed string `json:"assumed"`
	// S0 and V are the service line, seconds and tokens a second.
	S0 float64 `json:"s0"`
	V  float64 `json:"v"`
	// ServiceProportional says the prompts did not spread enough to fit
	// S0 and V apart: S0 is 0 and V the computed tokens over the service
	// time (FitPSInversion).
	ServiceProportional bool `json:"serviceProportional,omitempty"`
	// Shares is each rank's share of the requests, busiest first;
	// ShareCV the busiest share's coefficient of variation over the fit's
	// steps (the shares' stability); EvenWithinRows the ranks whose own
	// rate the recording does not carry, split evenly from their row.
	Shares         []float64 `json:"shares"`
	ShareCV        float64   `json:"shareCv"`
	EvenWithinRows int       `json:"evenWithinRows"`
	// KV is the fitted KV-deferral wait (fitted, not derived); nil where
	// the queue-time histograms did not read it.
	KV *RankMixtureKV `json:"kv,omitempty"`
	// A0, Beta and Spread are the non-queue part Z = A0 + Spread Beta L,
	// seconds and seconds per prompt token; Beta is the sum of the three
	// slopes beside it. Spread is a calibration: 1 is what the model
	// expects, and its distance from 1 is what Beta misses.
	A0            float64 `json:"a0"`
	Beta          float64 `json:"beta"`
	BetaOverhead  float64 `json:"betaOverhead"`
	BetaDecode    float64 `json:"betaDecode"`
	BetaRemainder float64 `json:"betaRemainder"`
	// BetaUnidentified names the slopes the fit window's prompts did not
	// spread enough to fit: each is 0 (FitBeta).
	BetaUnidentified []string `json:"betaUnidentified,omitempty"`
	Spread           float64  `json:"spread"`
	// FoldSpread is the spread each self-check fold fits alone;
	// SpreadAtBound says the folds could not reach a p90 ratio of 1;
	// FitP90 is the fit window's median p90 ratio at the spread; FitSteps
	// the fit's steps.
	FoldSpread    []float64 `json:"foldSpread"`
	SpreadAtBound bool      `json:"spreadAtBound,omitempty"`
	// SpreadDefaulted says the fit window is shorter than
	// MinSpreadFitWindow: the spread is 1, not calibrated.
	// SpreadFloored says the folds fitted a spread under MinSpread, and it
	// is priced at that floor.
	SpreadDefaulted bool    `json:"spreadDefaulted,omitempty"`
	SpreadFloored   bool    `json:"spreadFloored,omitempty"`
	FitP90          float64 `json:"fitP90"`
	FitSteps        int     `json:"fitSteps"`
	// FitStepsBacklogged is the fit window's steps left out because they
	// were not stationary: their arrivals ran ahead of their completions
	// by more than MaxMixtureBacklog (MixtureBacklog).
	FitStepsBacklogged int `json:"fitStepsBacklogged,omitempty"`
	// Queue is each rank's queue model (QueueLimitedProcessorSharing, or
	// processor sharing with Slots unknown); Slots its K, the prefill
	// engine's max-num-seqs (1 without chunked prefill), 0 not known.
	Queue string `json:"queue"`
	Slots int    `json:"slots"`
	// ZParts is Z measured part by part and FitMean the fit window's
	// median ratio of priced to measured mean: the variant's
	// (QueueRankMixtureNLLine) only, which calibrates nothing to the
	// router (identifiedNL).
	ZParts  *RankMixtureZParts `json:"zParts,omitempty"`
	FitMean float64            `json:"fitMean,omitempty"`
	// Stratified says each rank's mean service and cs^2 were read from the
	// computed-token histogram itself, not from the rank's own draws
	// (RankMixtureSwitches.Stratified; the planner's
	// BacktestRequest.RankMixtureStratified, on by default).
	Stratified bool `json:"stratified,omitempty"`
	// MinFitWindow is the shortest fit window the model was allowed to
	// price from, where the switches set one (RankMixtureSwitches
	// MinFitWindow; the planner's BacktestRequest.RankMixtureMinFitWindow),
	// absent at the default (MinSpreadFitWindow). ShortFitWindow says the
	// fit window was under the default and the model was fitted on it all
	// the same: every figure it prices is flagged RankMixtureShortFitWindow.
	MinFitWindow   string `json:"minFitWindow,omitempty"`
	ShortFitWindow bool   `json:"shortFitWindow,omitempty"`
	// fitWindow is MinFitWindow as a duration, 0 the default.
	fitWindow time.Duration
}

// Window is the shortest fit window the model was allowed to price from.
func (r RankMixtureReport) Window() time.Duration {
	if r.fitWindow > 0 {
		return r.fitWindow
	}
	return MinSpreadFitWindow
}

// ShortFlags is the flags every figure the model prices carries for its fit
// window: RankMixtureShortFitWindow where the window was under the default,
// none otherwise.
func (r RankMixtureReport) ShortFlags() []Flag {
	if r.ShortFitWindow {
		return []Flag{RankMixtureShortFitWindow}
	}
	return nil
}

// RankMixtureZParts is Z measured part by part, seconds and seconds a
// prompt token, and the parts' spreads (ZParts).
type RankMixtureZParts struct {
	OverheadA  float64 `json:"overheadA"`
	OverheadB  float64 `json:"overheadB"`
	DecodeA    float64 `json:"decodeA"`
	DecodeB    float64 `json:"decodeB"`
	RemainderA float64 `json:"remainderA"`
	RemainderB float64 `json:"remainderB"`
	// OverheadSigma and DecodeSigma are the parts' lognormal spreads
	// (ZPart.Sigma); 0 is none.
	OverheadSigma float64 `json:"overheadSigma"`
	DecodeSigma   float64 `json:"decodeSigma"`
	// RemainderShapeMean is the router's measured request-processing mean
	// the remainder is drawn from, seconds, and RemainderShapeP95 that
	// law's 95th percentile over its mean (RankMixtureSwitches
	// RemainderSpread); both absent where the remainder is its line.
	RemainderShapeMean float64 `json:"remainderShapeMean,omitempty"`
	RemainderShapeP95  float64 `json:"remainderShapeP95,omitempty"`
	// RemainderServer is the remainder drawn from the prefill pods' API
	// servers (RankMixtureSwitches.RemainderServer; the planner's
	// BacktestRequest.RankMixtureRemainderServer), absent where it was not
	// asked or not applied; RemainderServerOff is why it was asked and not
	// applied.
	RemainderServer    *RankMixtureServer `json:"remainderServer,omitempty"`
	RemainderServerOff string             `json:"remainderServerOff,omitempty"`
}

// RankMixtureServer is the remainder server's fit (ZServer), seconds and
// seconds a prompt token: the render handler's line, the generation
// frontend's (absent where the pods did not record it), the router's work
// outside its render call, the unmeasured rest's line and its share of the
// fit window's remainder, the fit window's top rate past which a figure is
// flagged where that share is over MaxRemainderRestShare, and what it was
// read on.
type RankMixtureServer struct {
	RenderA      float64 `json:"renderA"`
	RenderB      float64 `json:"renderB"`
	HasRender    bool    `json:"hasRender"`
	FrontendA    float64 `json:"frontendA"`
	FrontendB    float64 `json:"frontendB"`
	HasFrontend  bool    `json:"hasFrontend"`
	RouterOther  float64 `json:"routerOther"`
	RouterOtherB float64 `json:"routerOtherB,omitempty"`
	RestA        float64 `json:"restA"`
	RestB        float64 `json:"restB"`
	RestShare    float64 `json:"restShare"`
	TopRate      float64 `json:"topRate"`
	FitSteps     int     `json:"fitSteps"`
	PodSteps     int     `json:"podSteps"`
}

// remainderShape sets the remainder's measured law, where it has one.
func (z *RankMixtureZParts) remainderShape(r ZPart) {
	if r.Shape == nil {
		return
	}
	z.RemainderShapeMean, z.RemainderShapeP95 = r.ShapeMean, r.ShapeQuantile(0.95)
}

// remainderServer sets the remainder server's fit, or why it was not
// applied, where m was asked for it.
func (z *RankMixtureZParts) remainderServer(m RankMixture) {
	if !m.RemainderServer || m.Parts == nil {
		return
	}
	s := m.Parts.Remainder.Server
	if s == nil {
		z.RemainderServerOff = m.RemainderServerOff
		return
	}
	z.RemainderServer = &RankMixtureServer{RenderA: s.R0, RenderB: s.R1, HasRender: s.HasRender, FrontendA: s.F0, FrontendB: s.F1,
		HasFrontend: s.HasFrontend, RouterOther: s.Other, RouterOtherB: s.Other1, RestA: s.N0, RestB: s.N1, RestShare: s.RestShare,
		TopRate: s.TopRate, FitSteps: s.Steps, PodSteps: s.PodSteps}
}

// RankMixtureKV is the fitted KV-deferral wait.
type RankMixtureKV struct {
	P0    float64 `json:"p0"`
	P1    float64 `json:"p1"`
	M0    float64 `json:"m0"`
	M1    float64 `json:"m1"`
	RhoLo float64 `json:"rhoLo"`
	RhoHi float64 `json:"rhoHi"`
	// RhoHold is the utilization the lines are held at past it
	// (RankMixtureSwitches.KVHold); absent, not held.
	RhoHold float64 `json:"rhoHold,omitempty"`
}

// RankMixtureSwitches are the rank mixture's fit switches, each independent
// of the others. The zero value is every switch off; the planner turns
// Stratified on unless asked not to (DefaultRankMixtureStratified).
type RankMixtureSwitches struct {
	// FitSteps turns on the fit-step fixes (RankMixtureOptions
	// CleanFitSteps and SharesAtLoad).
	FitSteps bool
	// Stratified turns on the stratified service moments
	// (RankMixtureOptions.StratifiedMoments).
	Stratified bool
	// NLLine fits the variant QueueRankMixtureNLLine (FitRankMixtureNL) in
	// place of v2; it fits with both switches above on.
	NLLine bool
	// KVHold and RemainderSpread are the variant's modes
	// (RankMixtureOptions KVHold and RemainderSpread); the planner's
	// BacktestRequest RankMixtureKVHold and RankMixtureRemainderSpread.
	KVHold, RemainderSpread bool
	// RemainderServer is the variant's remainder server mode
	// (RankMixtureOptions.RemainderServer); the planner's
	// BacktestRequest.RankMixtureRemainderServer.
	RemainderServer bool
	// MinFitWindow is the shortest fit window the model prices from (the
	// planner's BacktestRequest.RankMixtureMinFitWindow): 0 is
	// MinSpreadFitWindow, the default. A model priced from a window under
	// the default is flagged RankMixtureShortFitWindow.
	MinFitWindow time.Duration
}

// FitWindow is the shortest fit window sw prices from.
func (sw RankMixtureSwitches) FitWindow() time.Duration {
	if sw.MinFitWindow > 0 {
		return sw.MinFitWindow
	}
	return MinSpreadFitWindow
}

// Options is the fit's options sw turns on, with slots the prefill engine's
// max-num-seqs: the backtest's fit, the main view's and the slo analyzer's
// read the same.
func (sw RankMixtureSwitches) Options(slots int) RankMixtureOptions {
	return RankMixtureOptions{Slots: slots, MinSpreadSpan: sw.FitWindow(),
		SpreadFloor: MinSpread, CleanFitSteps: sw.FitSteps, SharesAtLoad: sw.FitSteps,
		StratifiedMoments: sw.Stratified, KVHold: sw.KVHold, RemainderSpread: sw.RemainderSpread,
		RemainderServer: sw.RemainderServer}
}

// Model is the model sw fits.
func (sw RankMixtureSwitches) Model() QueueModel {
	if sw.NLLine {
		return QueueRankMixtureNLLine
	}
	return QueueRankMixture
}

// RankMixtureFitted is the model fitted on cycles, and its report.
type RankMixtureFitted struct {
	M      RankMixture
	Report RankMixtureReport
}

// FitRankMixtureCycles fits the model on cycles, cut into steps of step:
// each step's inputs pooled over its anchor's measurement window (the
// router's, RouterWindowOf), its mean and p90 the router's at the anchor
// (RouterTTFTOf). A step that is not stationary (MixtureBacklogged) is left
// out: no queue model's steady state is what it measured, and its TTFT would
// pass into the fit as cost. slots is the prefill engine's max-num-seqs (0
// not known). It returns the steps it fitted on.
func FitRankMixtureCycles(cycles []FitCycle, step time.Duration, slots int, sw RankMixtureSwitches) (*RankMixtureFitted, []MixtureStep, error) {
	cycles = sortedByTime(cycles)
	order, anchors := stepAnchors(cycles, step)
	var steps []MixtureStep
	even, noRouter, noRanks, backlog := 0, 0, 0, 0
	for _, k := range order {
		a := anchors[k]
		if !cycles[a].Latency {
			continue
		}
		at := cycles[a].At
		from := at.Add(-time.Duration(RouterWindowOf(cycles[a].Router) * float64(time.Second)))
		window := MixtureCyclesIn(cycles, from, at)
		if len(window) == 0 {
			continue
		}
		if MixtureBacklogged(cycles, from, at) != "" {
			backlog++
			continue
		}
		st := MixtureStepOf(window, at)
		mean, p90, ok := RouterTTFTOf(cycles[a].Router)
		switch {
		case !ok:
			noRouter++
			continue
		case !(st.Rate > 0):
			noRanks++
			continue
		}
		st.RouterMean, st.RouterP90 = mean, p90
		steps = append(steps, st)
		even = max(even, window[len(window)-1].EvenWithinRows)
	}
	if len(steps) == 0 {
		return nil, nil, fmt.Errorf("the experimental TTFT model %s fits against the router's measured TTFT mean and p90 and the prefill "+
			"ranks' own rates on stationary steps: of the fit window's %d steps, %d read no router TTFT, %d no prefill rank's rate "+
			"and %d were not stationary", sw.Model(), len(order), noRouter, noRanks, backlog)
	}
	fitter := FitRankMixture
	if sw.NLLine {
		fitter = FitRankMixtureNL
	}
	m, diag, err := fitter(steps, sw.Options(slots))
	if err != nil {
		return nil, nil, fmt.Errorf("the experimental TTFT model %s did not fit: %w", sw.Model(), err)
	}
	r := RankMixtureReport{Model: string(sw.Model()), Experimental: true, Assumed: m.Assumed(), S0: m.S0, V: m.V,
		ServiceProportional: m.ServiceProportional, Shares: m.Shares, ShareCV: diag.ShareCV, EvenWithinRows: even, A0: m.A0,
		Beta: m.Beta, BetaOverhead: m.BetaOverhead, BetaDecode: m.BetaDecode, BetaRemainder: m.BetaRemainder,
		BetaUnidentified: m.BetaUnidentified, Spread: m.Spread, FoldSpread: diag.FoldSpread, SpreadAtBound: diag.SpreadAtBound,
		SpreadDefaulted: diag.SpreadDefaulted, SpreadFloored: diag.SpreadFloored, FitP90: diag.FitP90, FitSteps: diag.Steps,
		Slots: slots, FitStepsBacklogged: backlog, Queue: string(QueueLimitedProcessorSharing), Stratified: sw.Stratified}
	if sw.MinFitWindow > 0 {
		r.fitWindow, r.MinFitWindow = sw.MinFitWindow, sw.MinFitWindow.String()
		r.ShortFitWindow = !diag.SpreadDefaulted && diag.Span < MinSpreadFitWindow
	}
	if slots <= 0 {
		r.Queue = string(QueueProcessorSharing)
	}
	if r.FoldSpread == nil {
		r.FoldSpread = []float64{}
	}
	if m.KV.Known {
		r.KV = &RankMixtureKV{P0: m.KV.P0, P1: m.KV.P1, M0: m.KV.M0, M1: m.KV.M1, RhoLo: m.KV.RhoLo, RhoHi: m.KV.RhoHi,
			RhoHold: m.KV.RhoHold}
	}
	if p := m.Parts; p != nil {
		r.FitMean = diag.FitMean
		r.ZParts = &RankMixtureZParts{OverheadA: p.Overhead.A, OverheadB: p.Overhead.B, DecodeA: p.Decode.A, DecodeB: p.Decode.B,
			RemainderA: p.Remainder.A, RemainderB: p.Remainder.B, OverheadSigma: p.Overhead.Sigma, DecodeSigma: p.Decode.Sigma}
		r.ZParts.remainderShape(p.Remainder)
		r.ZParts.remainderServer(m)
	}
	return &RankMixtureFitted{M: m, Report: r}, steps, nil
}

// AbstainedRankMixture is the model abstaining for why.
func AbstainedRankMixture(why string) *RankMixtureFitted {
	return &RankMixtureFitted{Report: RankMixtureReport{Model: string(QueueRankMixture), Experimental: true, Abstained: why,
		Shares: []float64{}, FoldSpread: []float64{}}}
}

// Identified is rq, abstaining where an identifiability check fails: the
// model prices only what its inputs identify.
//
// Two of the checks are on the calibration itself. The spread solves an
// equation, the folds' median p90 ratio equal to 1, by bisection over
// [0, MaxSpread]; a spread, pooled or one fold's, that ends at the upper
// bound has not solved it, and is the bound, not an estimate. A0 is refitted
// at each spread to hold the mean, so a spread that grows moves A0 down by as
// much as it moves the mean of Z up. A0 is Z at a prompt of no tokens, a
// time: a negative A0 is that counterweight, not a location.
func (rq *RankMixtureFitted) Identified() *RankMixtureFitted {
	if rq.Report.Model == string(QueueRankMixtureNLLine) {
		return rq.identifiedNL()
	}
	r := rq.Report
	var why []string
	if len(r.BetaUnidentified) > 0 {
		why = append(why, "prompt slope not identified ("+strings.Join(r.BetaUnidentified, ", ")+")")
	}
	if r.SpreadDefaulted {
		why = append(why, fmt.Sprintf("spread not calibrated (fit window under %s)", r.Window()))
	}
	if r.Spread >= MaxSpread || slices.ContainsFunc(r.FoldSpread, func(s float64) bool { return s >= MaxSpread }) {
		why = append(why, fmt.Sprintf("spread not identified (a self-check fold's p90 ratio does not reach 1 at any spread up to %g)",
			MaxSpread))
	}
	if r.A0 < 0 {
		why = append(why, fmt.Sprintf("non-queue constant negative (a0 %.3g s)", r.A0))
	}
	if r.ServiceProportional {
		why = append(why, "prefill service line not identified (one prompt length)")
	}
	if r.EvenWithinRows > 0 {
		why = append(why, "the prefill ranks' own rates not recorded")
	}
	if r.Slots <= 0 {
		why = append(why, "the prefill engine's max-num-seqs not read")
	}
	if len(why) == 0 {
		return rq
	}
	rq.Report.Abstained = strings.Join(why, "; ")
	return rq
}

// identifiedNL is Identified for the variant QueueRankMixtureNLLine. Its Z
// is measured, not calibrated, so v2's checks on the spread and on A0 do not
// apply (A0 is a sum of non-negative intercepts); in their place the fit
// window must price its own router mean within [MinNLFitMean, MaxNLFitMean]
// of measured and its own p90 within [MinNLFitP90, MaxNLFitMean], since
// nothing was calibrated to either: a model that misses the window it was
// fitted on is not extrapolated from it.
func (rq *RankMixtureFitted) identifiedNL() *RankMixtureFitted {
	r := rq.Report
	var why []string
	if r.ServiceProportional {
		why = append(why, "prefill service not identified (one prompt length)")
	}
	if r.EvenWithinRows > 0 {
		why = append(why, "the prefill ranks' own rates not recorded")
	}
	if r.Slots <= 0 {
		why = append(why, "the prefill engine's max-num-seqs not read")
	}
	if r.SpreadDefaulted {
		why = append(why, fmt.Sprintf("fit window under %s: too short for its own checks", r.Window()))
	}
	if !(r.FitMean >= MinNLFitMean && r.FitMean <= MaxNLFitMean) {
		why = append(why, fmt.Sprintf("the fit window's own mean not priced (median ratio %.3g, outside %g-%g)",
			r.FitMean, MinNLFitMean, MaxNLFitMean))
	}
	if !(r.FitP90 >= MinNLFitP90 && r.FitP90 <= MaxNLFitMean) {
		why = append(why, fmt.Sprintf("the fit window's own p90 not priced (median ratio %.3g, outside %g-%g)",
			r.FitP90, MinNLFitP90, MaxNLFitMean))
	}
	if len(why) == 0 {
		return rq
	}
	rq.Report.Abstained = strings.Join(why, "; ")
	return rq
}

// MixtureWindowFit is the model fitted on a window of cycles for pricing at
// any load, as the capacity planner's main view prices it and the slo
// analyzer prices its current load.
type MixtureWindowFit struct {
	// Fitted is the fit, Identified; its Report.Abstained says why it
	// prices nothing, "" where it prices.
	Fitted *RankMixtureFitted
	// Steps is the steps fitted on; Pooled the window's cycles pooled into
	// one step: the computed-token and prompt histograms every rate is
	// priced at; Top the highest step rate fitted, requests a second.
	Steps  []MixtureStep
	Pooled MixtureStep
	Top    float64
	// HeldOut is the model's mean TTFT scored on the window's self-check
	// folds (RankMixtureHeldOut); zero Steps where nothing was scored.
	HeldOut HeldOutScore
	// FitErr is why the fit itself failed, nil where it fitted (and may
	// still abstain on an identifiability check).
	FitErr error
}

// Abstained is why the fit prices nothing, "" where it prices.
func (w MixtureWindowFit) Abstained() string {
	if w.Fitted == nil {
		return "not fitted"
	}
	return w.Fitted.Report.Abstained
}

// FitMixtureWindow fits the model sw names on cycles, cut into steps of
// step, with the prefill engine's max-num-seqs slots, and scores it on its
// self-check folds at tolerance (RankMixtureNLHeldOut for the variant,
// RankMixtureHeldOut otherwise). A fit that fails, or fails an
// identifiability check, abstains.
func FitMixtureWindow(cycles []FitCycle, step time.Duration, slots int, tolerance float64, sw RankMixtureSwitches) MixtureWindowFit {
	rq, steps, err := FitRankMixtureCycles(cycles, step, slots, sw)
	if err != nil {
		return MixtureWindowFit{Fitted: AbstainedRankMixture(err.Error()), FitErr: err}
	}
	w := MixtureWindowFit{Fitted: rq.Identified(), Steps: steps}
	if w.Fitted.Report.Abstained != "" {
		return w
	}
	for _, st := range steps {
		w.Top = max(w.Top, st.Rate)
	}
	sorted := sortedByTime(cycles)
	first, last := sorted[0].At, sorted[len(sorted)-1].At
	w.Pooled = MixtureStepOf(MixtureCyclesIn(cycles, first.Add(-time.Nanosecond), last), last)
	o := sw.Options(rq.M.Slots)
	if sw.NLLine {
		w.HeldOut = RankMixtureNLHeldOut(steps, o, tolerance)
	} else {
		w.HeldOut = RankMixtureHeldOut(steps, o, rq.M.Spread, tolerance)
	}
	return w
}

// Price is the model's TTFT at rate requests a second over the fleet it was
// fitted at scaled by scale (the fitted prefill replicas over the priced
// fleet's), at the pooled window: the mean and each quantile of qs, from
// one set of draws. ok is false where the model abstained or refused the
// rate.
func (w MixtureWindowFit) Price(rate, scale float64, qs ...float64) (mean float64, quantiles []float64, ok bool) {
	if w.Abstained() != "" {
		return 0, nil, false
	}
	m := w.Fitted.M
	d, u := m.Draws(rate*scale, w.Pooled.Computed, w.Pooled.Prompt, DefaultMixtureDraws)
	if u != nil {
		return 0, nil, false
	}
	mean, quantiles = d.Stats(m.A0, m.Spread*m.Beta, qs...)
	if !Finite(mean) {
		return 0, nil, false
	}
	for _, v := range quantiles {
		if !Finite(v) {
			return 0, nil, false
		}
	}
	return mean, quantiles, true
}
