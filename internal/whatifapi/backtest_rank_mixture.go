package whatifapi

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// This file prices a backtest's TTFT with the experimental rank-mixture model
// (whatif.QueueRankMixture), where the request asks for it
// (BacktestRequest.TTFTModel). The model is fitted on the fit window's steps,
// each step's inputs and its router-measured mean and p90, and every test
// step is priced from its own inputs: its rate and its prefill pods'
// computed-token and prompt-length histograms. Nothing else in the answer
// moves: TPOT, the exclusions and the scoring are the planner's. Every TTFT
// it prices is flagged whatif.RankMixtureTTFT, and reads OUTSIDE VALIDATED
// REGION. With the prefix hit at count on (InputsRequest.PrefillHitAtCount),
// a test step on another prefill count than the fit's draws the fit
// window's computed-token histogram at that count's hit instead of its own
// (rankMixtureQ.computedAt).

// RankMixtureReport is the experimental model's fit, as a backtest's answer
// carries it (BacktestResponse.RankMixture): whatif.RankMixtureReport, the
// report the slo analyzer's v2 TTFT learner fills from the same fit.
type RankMixtureReport = whatif.RankMixtureReport

// RankMixtureKV is the fitted KV-deferral wait (whatif.RankMixtureKV).
type RankMixtureKV = whatif.RankMixtureKV

// RankMixtureCheck is what one step's data says of the model's assumptions
// (whatif.RankMixtureChecks): the busiest rank's utilization, its limited-PS
// admission, its relaxation time against the step, and whether the KV lines
// were read outside their fit.
type RankMixtureCheck struct {
	BusiestRho        float64 `json:"busiestRho"`
	RelaxationSeconds float64 `json:"relaxationSeconds"`
	KVOutside         bool    `json:"kvOutside,omitempty"`
	// AdmissionShare is the share of requests that wait for one of a
	// rank's K places (sum p_i rho_i^K); BusiestRhoK the busiest rank's
	// rho^K and AdmissionWait its mean wait, seconds; TailIndex its sojourn
	// tail index (Nair, Wierman and Zwart) from ServiceTailIndex, the
	// service's; HeavyTraffic a rank above whatif.MaxLPSRho
	// (whatif.RankMixtureChecks).
	AdmissionShare   float64 `json:"admissionShare"`
	BusiestRhoK      float64 `json:"busiestRhoK"`
	AdmissionWait    float64 `json:"admissionWait"`
	TailIndex        float64 `json:"tailIndex,omitempty"`
	ServiceTailIndex float64 `json:"serviceTailIndex,omitempty"`
	HeavyTraffic     bool    `json:"heavyTraffic,omitempty"`
	// Refused is why the model did not price the step, "" where it did.
	// Abstained says the model abstained on the whole backtest
	// (RankMixtureReport.Abstained).
	Refused   string `json:"refused,omitempty"`
	Abstained bool   `json:"abstained,omitempty"`
}

// rankMixtureQ is a backtest's fitted rank mixture.
type rankMixtureQ struct {
	m      whatif.RankMixture
	report RankMixtureReport
	// fitPrefill is the prefill pods the fit window served (the median of
	// its cycles), the count the ranks' shares were read at; 0 unknown.
	fitPrefill int
	// hitAtCount is the fit source's PrefillHitAtCount: a test step on
	// another prefill count than fitPrefill draws the fit window's
	// computed-token histogram, fitPooled, moved to that count's prefix hit
	// (computedAt), with hitModel where the fit source gave one.
	hitAtCount bool
	hitModel   *prefill.HitModel
	fitPooled  whatif.MixtureStep
}

// withHitAtCount turns the prefix hit at the test step's count on, from the
// fit window of fit at fitIdx, where fit was read with PrefillHitAtCount.
func (rq *rankMixtureQ) withHitAtCount(fit *inputsEntry, fitIdx []int) *rankMixtureQ {
	if !fit.doc.PrefillHitAtCount || len(fitIdx) == 0 || len(fit.mixture) != len(fit.series) {
		return rq
	}
	first, last := fit.series[fitIdx[0]].At, fit.series[fitIdx[len(fitIdx)-1]].At
	rq.hitAtCount, rq.hitModel = true, fit.doc.PrefillHitModel
	rq.fitPooled = whatif.MixtureStepOf(mixtureWindow(fit, fitIdx, first.Add(-time.Nanosecond), last), last)
	return rq
}

// computedAt is the computed-token histogram a test step on testPrefill
// pods draws from: its own, unless the switch is on and the step is on
// another count than the fit's, where it is the fit window's at the step's
// prefix hit (whatif.ComputedAtCount), as the planner prices E[I_eff] and
// the main view its model there, its bounds then scaled by the step's mean
// prompt over the fit window's: the step's prompt is its load's, an input
// the step states, and the hit is the share of it the cache serves. The
// step's own histogram holds the hit it was measured at, which a count
// transfer is to predict.
func (rq *rankMixtureQ) computedAt(st whatif.MixtureStep, testPrefill int) map[float64]float64 {
	if !rq.hitAtCount || rq.fitPrefill <= 0 || testPrefill <= 0 || testPrefill == rq.fitPrefill {
		return st.Computed
	}
	_, moved, ok := whatif.ComputedAtCount(rq.fitPooled.Computed, rq.fitPooled.Prompt, rq.fitPrefill, testPrefill, rq.hitModel)
	if !ok {
		return st.Computed
	}
	if ep, okp := whatif.CumulativeMean(st.Prompt); okp {
		if fp, okf := whatif.CumulativeMean(rq.fitPooled.Prompt); okf && fp > 0 {
			moved = whatif.ComputedAtScale(moved, st.Prompt, ep/fp)
		}
	}
	return moved
}

// rateScale is what a test step's fleet rate is multiplied by before the
// fitted shares split it: the fit's prefill pods over the step's, so each
// fitted rank takes the rate one of the step's ranks takes, as the answer
// path prices another count (mixtureFleet.scale). The shares hold one entry
// per fitted rank, so a step on twice the pods split over them priced each
// rank at twice its rate. 1 where either count is unknown.
func (rq *rankMixtureQ) rateScale(testPrefill int) float64 {
	if rq.fitPrefill <= 0 || testPrefill <= 0 {
		return 1
	}
	return float64(rq.fitPrefill) / float64(testPrefill)
}

// mixtureWindow is the cycles of e at idx whose time is in (from, to].
func mixtureWindow(e *inputsEntry, idx []int, from, to time.Time) []whatif.MixtureCycle {
	var out []whatif.MixtureCycle
	for _, i := range idx {
		if at := e.series[i].At; at.After(from) && !at.After(to) {
			out = append(out, e.mixture[i])
		}
	}
	return out
}

// fitCycles is the cycles of e at idx, in idx's order, as the experimental
// models' shared fits read them (whatif.FitCycle): each cycle's rates, its
// latency where e carries one, and its mixture and decode cycles where e
// carries those series.
func fitCycles(e *inputsEntry, idx []int) []whatif.FitCycle {
	out := make([]whatif.FitCycle, len(idx))
	for j, i := range idx {
		s := e.series[i]
		c := whatif.FitCycle{At: s.At, Rate: s.Rate, RateKnown: s.RateKnown, EngineRate: s.EngineRate}
		if len(e.mixture) == len(e.series) {
			c.Mixture = e.mixture[i]
		}
		if len(e.decode) == len(e.series) {
			c.Decode = e.decode[i]
		}
		if i < len(e.latency) {
			// The engines' TPOT histogram is the source's reading of the
			// cycle, however the source summed it.
			c.Latency, c.Router = true, e.latency[i].Router
			c.Decode.EngineTPOT = e.latency[i].EngineTPOT
		}
		out[j] = c
	}
	return out
}

// MaxMixtureBacklog is the largest backlog at which the rank mixture reads a
// step as stationary (whatif.MaxMixtureBacklog).
const MaxMixtureBacklog = whatif.MaxMixtureBacklog

// backlogged is why a step whose backlog is over MaxMixtureBacklog is not
// stationary, or "" where it is (or the rates are not read): the cycles of e
// at idx in (from, to], as whatif.MixtureBacklogged reads them.
func backlogged(e *inputsEntry, idx []int, from, to time.Time) string {
	return whatif.MixtureBacklogged(fitCycles(e, idx), from, to)
}

// fitRankMixture fits the model on the fit entry's cycles at fitIdx, cut into
// steps of step (whatif.FitRankMixtureCycles): each step's inputs pooled over
// its anchor's measurement window, its mean and p90 the router's at the
// anchor. A step that is not stationary (backlogged) is left out.
func fitRankMixture(fit *inputsEntry, fitIdx []int, step time.Duration, slots int, sw rankMixtureSwitches) (*rankMixtureQ, error) {
	rq, _, err := fitRankMixtureSteps(fit, fitIdx, step, slots, sw)
	return rq, err
}

// rankMixtureSwitches are the rank mixture's switches, each independent of
// the other.
type rankMixtureSwitches struct {
	// fitSteps turns on the fit-step fixes (whatif.RankMixtureOptions
	// CleanFitSteps and SharesAtLoad): BacktestRequest.RankMixtureFitSteps,
	// off by default.
	fitSteps bool
	// stratified prices with the stratified service moments
	// (whatif.RankMixtureOptions.StratifiedMoments):
	// BacktestRequest.RankMixtureStratified, on by default.
	stratified bool
	// nlLine fits the variant whatif.QueueRankMixtureNLLine in place of v2
	// (BacktestRequest.TTFTModel); it fits with both switches above on.
	nlLine bool
	// kvHold and remainderSpread are the variant's modes
	// (whatif.RankMixtureOptions KVHold and RemainderSpread):
	// BacktestRequest RankMixtureKVHold and RankMixtureRemainderSpread.
	kvHold, remainderSpread bool
	// remainderServer is the variant's remainder server mode
	// (whatif.RankMixtureOptions.RemainderServer):
	// BacktestRequest.RankMixtureRemainderServer.
	remainderServer bool
	// minFitWindow is the shortest fit window the model prices from
	// (BacktestRequest.RankMixtureMinFitWindow): 0 is
	// whatif.MinSpreadFitWindow, the default. A model priced from a window
	// under the default is flagged whatif.RankMixtureShortFitWindow.
	minFitWindow time.Duration
}

// rankMixtureSwitchesOf is the switches req turns on. The server has set
// an absent rankMixtureStratified to its own default; the CLI's backtest
// always sets it.
func rankMixtureSwitchesOf(req BacktestRequest) rankMixtureSwitches {
	return rankMixtureSwitches{fitSteps: req.RankMixtureFitSteps,
		stratified:      stratifiedOf(req.RankMixtureStratified, whatif.DefaultRankMixtureStratified),
		nlLine:          req.TTFTModel == string(whatif.QueueRankMixtureNLLine),
		kvHold:          req.RankMixtureKVHold,
		remainderSpread: req.RankMixtureRemainderSpread,
		remainderServer: req.RankMixtureRemainderServer,
		minFitWindow:    minFitWindowOf(req.RankMixtureMinFitWindow)}
}

// model is the model sw fits.
func (sw rankMixtureSwitches) model() whatif.QueueModel {
	if sw.nlLine {
		return whatif.QueueRankMixtureNLLine
	}
	return whatif.QueueRankMixture
}

// options is the fit's options sw turns on, with slots the prefill
// engine's max-num-seqs: the backtest's fit and the main view's
// (fitMainMixture) read the same.
func (sw rankMixtureSwitches) options(slots int) whatif.RankMixtureOptions {
	return sw.shared().Options(slots)
}

// shared is sw as the shared fit reads it (whatif.RankMixtureSwitches): the
// slo analyzer's v2 TTFT learner fits with the same switches.
func (sw rankMixtureSwitches) shared() whatif.RankMixtureSwitches {
	return whatif.RankMixtureSwitches{FitSteps: sw.fitSteps, Stratified: sw.stratified, NLLine: sw.nlLine,
		KVHold: sw.kvHold, RemainderSpread: sw.remainderSpread, RemainderServer: sw.remainderServer, MinFitWindow: sw.minFitWindow}
}

// stratifiedOf is a request's rankMixtureStratified: the value where it
// names one, def where it is absent.
func stratifiedOf(asked *bool, def bool) bool {
	if asked == nil {
		return def
	}
	return *asked
}

// fitRankMixtureSteps is fitRankMixture, and the steps it fitted on.
func fitRankMixtureSteps(fit *inputsEntry, fitIdx []int, step time.Duration, slots int, sw rankMixtureSwitches) (
	*rankMixtureQ, []whatif.MixtureStep, error) {
	if len(fit.mixture) != len(fit.series) {
		return nil, nil, InputErrorf("the experimental TTFT model %s needs the recording's per-rank series, which this source did not give",
			sw.model())
	}
	f, steps, err := whatif.FitRankMixtureCycles(fitCycles(fit, fitIdx), step, slots, sw.shared())
	if err != nil {
		return nil, nil, InputError{Err: err}
	}
	return &rankMixtureQ{m: f.M, report: f.Report, fitPrefill: actualOf(seriesAt(fit.series, fitIdx)).Prefill}, steps, nil
}

// abstainedMixture is the model abstaining for why.
func abstainedMixture(why string) *rankMixtureQ {
	f := whatif.AbstainedRankMixture(why)
	return &rankMixtureQ{m: f.M, report: f.Report}
}

// identified is rq, or rq abstaining where an identifiability check fails
// (whatif.RankMixtureFitted.Identified): the model prices only what its
// inputs identify.
func (rq *rankMixtureQ) identified() *rankMixtureQ {
	rq.report = (&whatif.RankMixtureFitted{M: rq.m, Report: rq.report}).Identified().Report
	return rq
}

// mixturePriced says the rank mixture priced the step's TTFT.
func (q *backtestQuestion) mixturePriced(out BacktestStep) bool {
	return q.mixture != nil && out.RankMixture != nil && out.RankMixture.Refused == "" && !out.RankMixture.Abstained
}

// assumedLines is the model's assumed lines for the answer.
func (rq *rankMixtureQ) assumedLines() []string {
	if rq.report.Abstained != "" {
		return []string{fmt.Sprintf("experimental TTFT model %s abstained (%s): every TTFT is the planner's own, flagged %s",
			rq.report.Model, rq.report.Abstained, whatif.RankMixtureAbstained)}
	}
	out := []string{rq.report.Assumed + "; spread fitted on the fit window's self-check folds so that their median p90 ratio is 1 " +
		"(p95 is not fitted); every TTFT is flagged " + string(whatif.RankMixtureTTFT) + " and is OUTSIDE VALIDATED REGION"}
	if rq.report.ZParts != nil {
		out[0] = rq.report.Assumed + fmt.Sprintf("; nothing calibrated to the router (the fit window prices its own mean at %.3g of "+
			"measured); every TTFT is flagged %s and is OUTSIDE VALIDATED REGION", rq.report.FitMean, whatif.RankMixtureTTFT)
	}
	if len(rq.report.BetaUnidentified) > 0 {
		out = append(out, fmt.Sprintf("the fit window's prompts did not spread: the prompt term's %s slope is 0, its cost held in a0 at the "+
			"prompts read", strings.Join(rq.report.BetaUnidentified, ", ")))
	}
	if rq.report.ServiceProportional {
		out = append(out, "the fit window's prompts did not spread: the prefill service is taken proportional to the computed prompt, "+
			"read at the prompts it was measured at")
	}
	if rq.report.SpreadDefaulted {
		out = append(out, fmt.Sprintf("the fit window is shorter than %s: the prompt term's spread is 1, not calibrated", rq.report.Window()))
	}
	if rq.report.ShortFitWindow {
		out = append(out, fmt.Sprintf("the fit window is shorter than %s, priced all the same down to the %s asked: every TTFT the "+
			"model prices is also flagged %s", whatif.MinSpreadFitWindow, rq.report.Window(), whatif.RankMixtureShortFitWindow))
	}
	if rq.report.SpreadFloored {
		out = append(out, fmt.Sprintf("the folds fitted the prompt term's spread under %g: it is priced at %g", whatif.MinSpread, whatif.MinSpread))
	}
	if rq.report.EvenWithinRows > 0 {
		out = append(out, fmt.Sprintf("%d prefill ranks' own request rates are not in the recording: each row's rate is split evenly "+
			"between its ranks, so the share profile is flatter than the fleet's", rq.report.EvenWithinRows))
	}
	return out
}

// price prices one test step's TTFT at each of backtestStats from the
// cycles of its window, over a step of stepSeconds, on testPrefill prefill
// pods (rateScale). ok is false where the model refused.
func (rq *rankMixtureQ) price(cycles []whatif.MixtureCycle, at time.Time, stepSeconds float64, testPrefill int) ([]Figure, RankMixtureCheck, bool) {
	st := whatif.MixtureStepOf(cycles, at)
	d, u := rq.m.Draws(st.Rate*rq.rateScale(testPrefill), rq.computedAt(st, testPrefill), st.Prompt, whatif.DefaultMixtureDraws)
	if u != nil {
		return nil, RankMixtureCheck{Refused: u.Error()}, false
	}
	c := d.Checks
	chk := RankMixtureCheck{RelaxationSeconds: c.Relaxation, KVOutside: c.KVOutside, AdmissionShare: c.AdmissionShare,
		BusiestRhoK: c.BusiestRhoK, AdmissionWait: c.AdmissionWait, TailIndex: c.TailIndex, ServiceTailIndex: c.ServiceTailIndex,
		HeavyTraffic: c.HeavyTraffic}
	if len(c.Rho) > 0 {
		chk.BusiestRho = slices.Max(c.Rho)
	}
	flags := append(append([]whatif.Flag{whatif.RankMixtureTTFT}, rq.report.ShortFlags()...), c.Flags(stepSeconds)...)
	qs := []float64{}
	for _, q := range backtestStats {
		if q != 0 {
			qs = append(qs, q)
		}
	}
	mean, quant := d.Stats(rq.m.A0, rq.m.Spread*rq.m.Beta, qs...)
	out := make([]Figure, len(backtestStats))
	j := 0
	for i, q := range backtestStats {
		v := mean
		if q != 0 {
			v = quant[j]
			j++
		}
		out[i] = figureOf(whatif.Answer{Value: v, Flags: flags}, unitSeconds, true)
	}
	return out, chk, true
}

// priceMixture sets a step's TTFT from the rank mixture: its figures in
// pred's place, no bound, and the step's relaxation and settling from the
// model's busiest rank. Where the model refused, the planner's figures
// stand, flagged with the experimental model. It refuses a step that is not
// stationary (backlogged): the model prices a steady state, and a step whose
// backlog grows has none.
func (q *backtestQuestion) priceMixture(pred *prediction, out *BacktestStep, st stepOf) {
	if q.mixture.report.Abstained != "" {
		out.RankMixture = &RankMixtureCheck{Abstained: true}
		for i := range pred.ttft {
			if !slices.Contains(pred.ttft[i].Flags, string(whatif.RankMixtureAbstained)) {
				pred.ttft[i].Flags = append(pred.ttft[i].Flags, string(whatif.RankMixtureAbstained))
			}
		}
		return
	}
	at := q.test.series[st.anchor].At
	var figs []Figure
	var chk RankMixtureCheck
	ok := false
	if why := backlogged(q.test, st.window, st.windowStart, at); why != "" {
		chk.Refused = why
	} else {
		testPrefill := actualOf(seriesAt(q.test.series, st.window)).Prefill
		figs, chk, ok = q.mixture.price(mixtureWindow(q.test, st.window, st.windowStart, at), at, st.windowSeconds, testPrefill)
	}
	out.RankMixture = &chk
	if !ok {
		for i := range pred.ttft {
			if !slices.Contains(pred.ttft[i].Flags, string(whatif.RankMixtureTTFT)) {
				pred.ttft[i].Flags = append(pred.ttft[i].Flags, string(whatif.RankMixtureTTFT))
			}
		}
		return
	}
	for i := range pred.ttft {
		pred.ttft[i] = validate(figs[i], whatif.FigureTTFT, backtestStats[i], whatif.FigureClass{}, 0)
		pred.ttftBound[i] = nil
	}
	out.RelaxationSeconds = ptr(chk.RelaxationSeconds)
	out.Unsettled = !settles(chk.RelaxationSeconds, st.windowSeconds)
}
