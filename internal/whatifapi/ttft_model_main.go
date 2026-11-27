package whatifapi

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// This file prices the main view's TTFT (POST /v1/plan, /v1/size and
// /v1/report) with the experimental rank-mixture model, where the server
// (Config.TTFTModel) or the inputs request (InputsRequest.TTFTModel) asks for
// it. The model is fitted on the source's own window when the source is
// resolved, as a backtest fits it on its fit window (fitRankMixture), and
// abstains where its identifiability checks fail (identified): the
// planner's own answer then stands at every rate. Where it prices, it prices
// every rate of the TTFT curve up to the planner's own lower knee, and no
// further: the knee the rank mixture would read from its fitted service
// rate is a processor-sharing inversion, which read 1.06 to 1.56 times the
// measured capacity on the lab recordings (issue #406), so the curve never
// runs past the knee the planner itself answers. A rate above the highest
// step rate the fit window served is marked Figure.Extrapolated.

// The main view's words for the experimental model.
const (
	// TTFTModelLabel names the model on the TTFT figure and its curve.
	TTFTModelLabel = "Experimental TTFT model"
	// TTFTBeyondMeasured labels the curve past the highest load measured.
	TTFTBeyondMeasured = "beyond the highest load measured"
	// TTFTRightEndEstimate says the curve's right end, under a knee that is
	// a lower bound or refused, is the model's estimate.
	TTFTRightEndEstimate = "the curve's right end is an estimate from the fitted prefill service rate"
)

// TTFTModelInfo is the experimental TTFT model as fitted on a source
// (InputsResponse.TTFTModel).
type TTFTModelInfo struct {
	// Model is whatif.QueueRankMixture; Experimental is always true; Label
	// is TTFTModelLabel.
	Model        string `json:"model"`
	Experimental bool   `json:"experimental"`
	Label        string `json:"label"`
	// Abstained is why the model prices nothing, "" where it prices.
	Abstained string `json:"abstained,omitempty"`
	// HeldOut is the model's mean TTFT scored on the fit window's
	// self-check folds (whatif.RankMixtureHeldOut), and Evidence that score
	// in words; both absent where nothing was scored.
	HeldOut  *HeldOutScore `json:"heldOut,omitempty"`
	Evidence string        `json:"evidence,omitempty"`
	// MeasuredUpTo is the highest step rate the fit window served,
	// requests a second, at the recorded fleet.
	MeasuredUpTo *float64 `json:"measuredUpTo,omitempty"`
	// Fit is the model's fit, as a backtest's answer carries it.
	Fit *RankMixtureReport `json:"fit,omitempty"`
}

// HeldOutScore is the experimental model scored on the source's held-out
// self-check folds (whatif.RankMixtureHeldOut): at Statistic (the mean; the
// p90 calibrates the model's spread on those same folds, so it is not
// held out), Within of Steps were within BacktestTolerance of the measured
// figure and Low read more than that low; MedianRatio is predicted over
// measured. RateFrom and RateTo are the rates the steps scored ran at, and
// Prefill and Decode the recorded fleet: the range the score covers.
type HeldOutScore struct {
	Statistic   string  `json:"statistic"`
	Within      int     `json:"within"`
	Low         int     `json:"low"`
	Steps       int     `json:"steps"`
	MedianRatio float64 `json:"medianRatio"`
	RateFrom    float64 `json:"rateFrom"`
	RateTo      float64 `json:"rateTo"`
	Prefill     int     `json:"prefill"`
	Decode      int     `json:"decode"`
}

// TTFTModelView is the experimental model at a plan's fleet
// (TTFTView.Model).
type TTFTModelView struct {
	Model        string `json:"model"`
	Experimental bool   `json:"experimental"`
	Label        string `json:"label"`
	// Priced says the model priced this fleet's TTFT; NotPriced is why not
	// where it did not (the model abstained, or the fleet is one it does
	// not price), and the planner's own TTFT stands.
	Priced    bool   `json:"priced"`
	NotPriced string `json:"notPriced,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
	// MeasuredUpTo is the highest load the fit window served, at this
	// fleet's prefill count; a curve point above it is Extrapolated and
	// drawn as Beyond says.
	MeasuredUpTo *float64 `json:"measuredUpTo,omitempty"`
	Beyond       string   `json:"beyond,omitempty"`
	// RightEnd says the curve's right end is the model's estimate, where
	// the prefill knee it stops at is a lower bound or refused.
	RightEnd string `json:"rightEnd,omitempty"`
}

// mainMixture is the experimental model fitted on a source for the main
// view.
type mainMixture struct {
	rq *rankMixtureQ
	// pooled is the source window's cycles pooled into one step: the
	// computed-token and prompt histograms every rate is priced at.
	pooled whatif.MixtureStep
	// top is the highest step rate the fit served; prefill the recorded
	// prefill replicas.
	top     float64
	prefill int
	info    TTFTModelInfo
	// hitAtCount is InputsRequest.PrefillHitAtCount, as the source was
	// read: the computed-token histogram is priced at each prefill count's
	// prefix hit (computedAt), with hitModel where the source gave one
	// (InputsResponse.PrefillHitModel).
	hitAtCount bool
	hitModel   *prefill.HitModel
	// fit is the shape the fit steps served: what a day-plan bucket's own
	// shape is checked against before the model prices it (outsideFit).
	fit mixtureRange
	// bucket says pooled is a day-plan bucket's own histograms
	// (experimentalModels.atBucket), not the window's: the bucket's lengths
	// on the fleet's knobs are then its own, not stated ones.
	bucket bool
	// prices is every TTFT priced so far on a day-plan bucket's copy
	// (atBucket), by prefill count, rate and statistic, and atTarget every
	// highest rate at a target searched (sharedRate): its sizer prices the
	// same rates at many decode counts, and one plan prices on one
	// goroutine. Both nil on the window's model, which a server shares
	// between requests.
	prices   map[mixturePriceKey]mixturePrice
	atTarget map[mixturePriceKey]mixturePrice
}

// mixturePriceKey is one TTFT the model priced: at the computed-token
// histogram of n prefill replicas (mixtureFleet.histogramCount), the rate
// one recorded replica's worth of ranks takes, and the statistic.
type mixturePriceKey struct {
	n    int
	x, q float64
}

// mixturePrice is a TTFT the model priced, ok false where it refused.
type mixturePrice struct {
	v  float64
	ok bool
}

// computedAt is the computed-token histogram the model draws from at n
// prefill replicas, the flag its figures carry and the note that says so.
// With PrefillHitAtCount on and n not the recorded count, the pooled
// window's hit h, 1 - E[computed] / E[prompt], is priced at n and the
// histogram moved to it (whatif.ComputedAtCount); otherwise the pooled
// histogram as it is.
func (mm *mainMixture) computedAt(n int) (map[float64]float64, whatif.Flag, string) {
	computed := mm.pooled.Computed
	if !mm.hitAtCount || n == mm.prefill {
		return computed, "", ""
	}
	r, moved, ok := whatif.ComputedAtCount(computed, mm.pooled.Prompt, mm.prefill, n, mm.hitModel)
	if !ok {
		return computed, "", ""
	}
	flag := whatif.HitAtCountFlag(r)
	if !r.Moved() {
		return computed, flag, ""
	}
	how := fmt.Sprintf("its histogram scaled by %.3g, capped at the prompt", r.ComputedScale)
	if r.Hit < r.FitHit {
		how = fmt.Sprintf("a miss mixture: a share of the requests computing their whole prompt, %.3g times the computed tokens",
			r.ComputedScale)
	}
	note := fmt.Sprintf("experimental TTFT model's computed tokens at the prefix hit %.3g on %d prefill replicas, from %.3g on %d (%s): %s",
		r.Hit, n, r.FitHit, mm.prefill, r.Rule, how)
	return moved, flag, note
}

// TTFTModelNLLineLabel names the variant whatif.QueueRankMixtureNLLine on
// the TTFT figure and its curve, so the page says which model priced.
const TTFTModelNLLineLabel = TTFTModelLabel + " v2nl-line"

// ttftModelOf is the model a source is resolved with: the request's, else
// the server's. An unknown model is the caller's error.
func ttftModelOf(req, server string) (string, error) {
	m := req
	if m == "" {
		m = server
	}
	if m != "" && m != string(whatif.QueueRankMixture) && m != string(whatif.QueueRankMixtureNLLine) {
		return "", InputErrorf("ttftModel %q, want %q, %q or none", m, whatif.QueueRankMixture, whatif.QueueRankMixtureNLLine)
	}
	return m, nil
}

// TTFTModelOptions is how the main view's experimental TTFT model is
// fitted, as a backtest's request fits it (BacktestRequest TTFTModel and
// its rank-mixture switches): Model is whatif.QueueRankMixture or
// whatif.QueueRankMixtureNLLine, and Stratified prices v2 with the
// stratified service moments (the variant always does).
type TTFTModelOptions struct {
	Model      string
	Stratified bool
	// KVHold and RemainderSpread are the variant's modes
	// (BacktestRequest RankMixtureKVHold, RankMixtureRemainderSpread), and
	// MinFitWindow the shortest fit window either model prices from
	// (BacktestRequest.RankMixtureMinFitWindow), 0 the default.
	KVHold, RemainderSpread bool
	MinFitWindow            time.Duration
	// RemainderServer is the variant's remainder server mode
	// (BacktestRequest.RankMixtureRemainderServer), on top of
	// RemainderSpread.
	RemainderServer bool
}

// switches is the backtest's switches the options turn on.
func (o TTFTModelOptions) switches() rankMixtureSwitches {
	return rankMixtureSwitches{stratified: o.Stratified, nlLine: o.Model == string(whatif.QueueRankMixtureNLLine),
		kvHold: o.KVHold, remainderSpread: o.RemainderSpread, remainderServer: o.RemainderServer, minFitWindow: o.MinFitWindow}
}

// moved says the options fit the model other than its defaults do, apart
// from Stratified: a mode on, or another shortest fit window.
func (o TTFTModelOptions) moved() bool {
	return o.KVHold || o.RemainderSpread || o.RemainderServer || o.MinFitWindow > 0
}

// Check refuses options the model does not take: the modes are the
// variant's only, and the remainder server needs the remainder spread.
func (o TTFTModelOptions) Check() error {
	if (o.KVHold || o.RemainderSpread) && o.Model != string(whatif.QueueRankMixtureNLLine) {
		return InputErrorf("rankMixtureKVHold and rankMixtureRemainderSpread are modes of ttftModel %q: give them with that model",
			whatif.QueueRankMixtureNLLine)
	}
	return checkRemainderServer(o.RemainderServer, o.RemainderSpread, o.Model)
}

// fitMainMixture fits the model sw names on e's cycles up to the cycle
// answered, cut into DefaultBacktestStep steps, with the prefill engine's
// max-num-seqs read from the source, as a backtest fits it on its fit
// window (fitRankMixtureSteps).
func fitMainMixture(e *inputsEntry, sw rankMixtureSwitches) *mainMixture {
	label := TTFTModelLabel
	if sw.nlLine {
		label = TTFTModelNLLineLabel
	}
	mm := &mainMixture{info: TTFTModelInfo{Model: string(sw.model()), Experimental: true, Label: label}}
	if p := e.doc.Fleet.Prefill; p != nil {
		mm.prefill = p.Replicas
	}
	mm.hitAtCount, mm.hitModel = e.doc.PrefillHitAtCount, e.doc.PrefillHitModel
	abstain := func(why string) *mainMixture {
		mm.rq = abstainedMixture(why)
		mm.info.Abstained = why
		return mm
	}
	switch {
	case e.doc.Fleet.Prefill == nil:
		return abstain("the source has no prefill role")
	case len(e.mixture) != len(e.series) || len(e.series) == 0:
		return abstain("the source did not give the per-rank series")
	}
	answered := e.series[len(e.series)-1].At
	if c := e.doc.Source.Cycle; c > 0 && c <= len(e.series) {
		answered = e.series[c-1].At
	}
	var idx []int
	for i, s := range e.series {
		if !s.At.After(answered) {
			idx = append(idx, i)
		}
	}
	// The slo analyzer's v2 TTFT learner fits and prices its own cycles with
	// the same call.
	w := whatif.FitMixtureWindow(fitCycles(e, idx), DefaultBacktestStep, e.engine.PrefillSlots, BacktestTolerance, sw.shared())
	if w.FitErr != nil {
		return abstain(w.FitErr.Error())
	}
	mm.rq = &rankMixtureQ{m: w.Fitted.M, report: w.Fitted.Report}
	r := mm.rq.report
	mm.info.Fit = &r
	if r.Abstained != "" {
		mm.info.Abstained = r.Abstained
		return mm
	}
	mm.top = w.Top
	mm.fit = mixtureRangeOf(w.Steps)
	if mm.top > 0 {
		mm.info.MeasuredUpTo = ptr(mm.top)
	}
	mm.pooled = w.Pooled
	if h := w.HeldOut; h.Steps > 0 {
		mm.info.HeldOut = &HeldOutScore{Statistic: statisticMean, Within: h.Within, Low: h.Low, Steps: h.Steps, MedianRatio: h.MedianRatio,
			RateFrom: h.RateLo, RateTo: h.RateHi, Prefill: mm.prefill, Decode: e.doc.Fleet.Decode.Replicas}
		mm.info.Evidence = heldOutWords(h.Within, h.Steps)
	}
	return mm
}

// heldOutWords is a held-out score in the page's words.
func heldOutWords(within, n int) string {
	return fmt.Sprintf("held-out steps: %d of %d within %.0f%%", within, n, 100*BacktestTolerance)
}

// priced says the model prices.
func (mm *mainMixture) priced() bool {
	return mm != nil && mm.rq != nil && mm.rq.report.Abstained == ""
}

// notPricedAt is why the model does not price the fleet at k, "" where it
// does: it prices the source's own traffic, so a stated shape or term, a
// stated arrival burstiness, or a fleet with no prefill replica is the
// planner's alone.
func (mm *mainMixture) notPricedAt(k Knobs) string {
	switch {
	case !mm.priced():
		return "the model abstained: " + mm.info.Abstained
	case mm.prefill <= 0 || k.Prefill <= 0:
		return "the fleet has no prefill replica"
	case (!mm.bucket && (k.PromptTokens != 0 || k.OutputTokens != 0)) || (k.PrefixHit != nil && !k.PrefixHitRead) || k.CachedTokenCost != nil:
		return "the model prices the source's own request shape, not a stated one"
	case statesTTFTTerm(k.Assume) || k.ArrivalCV2 != nil:
		return "the model prices the source's own terms and random arrivals, not stated ones"
	}
	return ""
}

// statesTTFTTerm says a states a term the planner's TTFT is priced from:
// V_P, a, the prompt spread or the router hop. The decode terms and the
// router's flow control are not the model's.
func statesTTFTTerm(a Assume) bool {
	return a.VP != nil || a.A != nil || a.PromptCV != nil || a.RouterHop != nil
}

// mixtureFleet is a Fleet whose TTFT the experimental model prices, up to
// the planner's own lower knee (curveCap); every other figure is the
// planner's.
type mixtureFleet struct {
	Fleet
	mm *mainMixture
	// q is the TTFT statistic, 0 the mean; scale is the recorded prefill
	// replicas over the fleet's: the rate one recorded prefill replica's
	// worth of ranks takes; n the fleet's prefill replicas.
	q, scale float64
	n        int
	// knee is the highest rate priced (curveCap), kneeOK false with no
	// knee answered; lowerBound says the prefill knee is a lower bound or
	// refused.
	knee       float64
	kneeOK     bool
	lowerBound bool
	// endAtLeast says the curve's end is a prefill knee that is only a
	// lower bound: a limit it sets is "at least" that rate.
	endAtLeast bool
	floor      float64
	// computed is the computed-token histogram priced at the fleet's
	// prefill replicas (mainMixture.computedAt), hitFlag the flag its
	// figures carry and hitNote its assumed line; "" where nothing moved.
	computed map[float64]float64
	hitFlag  whatif.Flag
	hitNote  string
}

// fleet is inner with the model's TTFT, or inner where the model does not
// price at k.
func (mm *mainMixture) fleet(inner Fleet, k Knobs) Fleet {
	if mm == nil || mm.notPricedAt(k) != "" {
		return inner
	}
	f := &mixtureFleet{Fleet: inner, mm: mm, q: k.TTFTPercentile, scale: float64(mm.prefill) / float64(k.Prefill), n: k.Prefill}
	f.computed, f.hitFlag, f.hitNote = mm.computedAt(k.Prefill)
	pk, dk := inner.PrefillKnee(), inner.DecodeKnee()
	f.lowerBound = isRefusal(pk) || slices.Contains(figureOf(pk, unitRate, false).Flags, string(whatif.VPLowerBound))
	pv, pok := valueOf(pk)
	dv, dok := valueOf(dk)
	f.knee, f.kneeOK = curveCap(pv, pok, f.lowerBound, dv, dok, mm.top/f.scale)
	f.endAtLeast = endAtLeast(pv, pok, f.lowerBound, dv, dok, mm.top/f.scale)
	// The floor: the TTFT with no queue, at a rate whose utilization is
	// negligible.
	if v, ok := f.price(1e-6); ok {
		f.floor = v
	}
	return f
}

// price is the model's TTFT at the fleet's rate; ok false where it refused.
func (f *mixtureFleet) price(rate float64) (float64, bool) {
	key := mixturePriceKey{n: f.histogramCount(), x: rate * f.scale, q: f.q}
	if p, ok := f.mm.prices[key]; ok {
		return p.v, p.ok
	}
	v, _, u := f.mm.rq.m.Price(key.x, f.computed, f.mm.pooled.Prompt, f.q)
	p := mixturePrice{v: v, ok: u == nil && whatif.Finite(v) && !math.IsNaN(v)}
	if !p.ok {
		p.v = 0
	}
	if f.mm.prices != nil {
		f.mm.prices[key] = p
	}
	return p.v, p.ok
}

// curveCap is the highest rate the model prices: the lower of the
// planner's prefill knee (pv, pok) and decode knee (dv, dok), where a
// prefill knee that is only a lower bound (lowerBound) is raised to served,
// the highest load the source served at this fleet: the fleet served it, so
// the knee is at least there. ok is false with neither knee answered. A
// refused prefill knee leaves decode's alone.
func curveCap(pv float64, pok, lowerBound bool, dv float64, dok bool, served float64) (float64, bool) {
	if pok && lowerBound && served > pv {
		pv = served
	}
	switch {
	case pok && dok:
		return min(pv, dv), true
	case pok:
		return pv, true
	case dok:
		return dv, true
	}
	return 0, false
}

// endAtLeast says the curve's end (curveCap of the same arguments) is a
// prefill knee that is only a lower bound, raised or not to served: not
// decode's knee, and not a refused prefill knee, which leaves decode's. On a
// tie with decode's knee the end is decode's: the fleet stops there.
func endAtLeast(pv float64, pok, lowerBound bool, dv float64, dok bool, served float64) bool {
	return pok && lowerBound && (!dok || max(pv, served) < dv)
}

// curveEnd is the highest rate the model prices: the curve is drawn up to
// it, past the planner's lower knee where the fleet served past a knee that
// is only a lower bound.
func (f *mixtureFleet) curveEnd() float64 { return f.knee }

// within says the model prices rate: up to curveCap.
func (f *mixtureFleet) within(rate float64) bool {
	return f.kneeOK && rate > 0 && rate <= f.knee*(1+1e-9)
}

// TTFTAt is the model's TTFT at rate, up to the planner's lower knee; past
// it, or where the model refuses, the planner's.
func (f *mixtureFleet) TTFTAt(rate float64) whatif.Result {
	if !f.within(rate) {
		return f.Fleet.TTFTAt(rate)
	}
	v, ok := f.price(rate)
	if !ok {
		return f.Fleet.TTFTAt(rate)
	}
	return whatif.TTFT{Seconds: v, Independent: v, Pooled: v, Floor: f.floor, Flags: f.flags()}
}

// flags are the flags of a TTFT the model prices: RankMixtureTTFT, the fit
// window's (RankMixtureReport.shortFlags), and the prefix hit's at another
// prefill count (mainMixture.computedAt).
func (f *mixtureFleet) flags() []whatif.Flag {
	out := f.mm.modelFlags()
	if f.hitFlag != "" {
		out = append(out, f.hitFlag)
	}
	return out
}

// modelFlags is the flags every figure the model sets carries:
// RankMixtureTTFT, and RankMixtureShortFitWindow where it was fitted on a
// window under the default (RankMixtureReport.ShortFitWindow).
func (mm *mainMixture) modelFlags() []whatif.Flag {
	if mm == nil || mm.rq == nil {
		return []whatif.Flag{whatif.RankMixtureTTFT}
	}
	return append([]whatif.Flag{whatif.RankMixtureTTFT}, mm.rq.report.ShortFlags()...)
}

// TTFTPoint is TTFTAt with the planner's notes.
func (f *mixtureFleet) TTFTPoint(rate float64) (whatif.Result, []string) {
	r, notes := f.Fleet.TTFTPoint(rate)
	if !f.within(rate) {
		return r, notes
	}
	if v, ok := f.price(rate); ok {
		return whatif.TTFT{Seconds: v, Independent: v, Pooled: v, Floor: f.floor, Flags: f.flags()}, notes
	}
	return r, notes
}

// Assumed is the planner's assumed inputs and the model's.
func (f *mixtureFleet) Assumed() []string {
	out := slices.Clone(f.Fleet.Assumed())
	mix := "the source window's pooled prompt mix"
	if f.mm.bucket {
		mix = "the bucket's own prompt and computed-token histograms"
	}
	out = append(out, f.mm.rq.report.Assumed+"; TTFT priced at "+mix+", up to the planner's own lower knee")
	if f.scale != 1 {
		out = append(out, fmt.Sprintf("experimental TTFT model at %d prefill replicas, %d recorded: each replica's ranks take the recorded "+
			"share profile, at the rate per replica", int(math.Round(float64(f.mm.prefill)/f.scale)), f.mm.prefill))
	}
	if f.hitNote != "" {
		out = append(out, f.hitNote)
	}
	return out
}

// extrapolated says the model's TTFT at rate is above the highest load the
// fit window served, at this fleet.
func (f *mixtureFleet) extrapolated(rate float64) bool {
	return f.within(rate) && f.mm.top > 0 && rate*f.scale > f.mm.top*(1+1e-9)
}

// extrapolating is a Fleet that marks the rates it prices beyond what was
// measured.
type extrapolating interface {
	extrapolated(rate float64) bool
}

// markExtrapolated sets f.Extrapolated where fl prices rate beyond what was
// measured and f is answered.
func markExtrapolated(fl Fleet, rate float64, f Figure) Figure {
	if x, ok := fl.(extrapolating); ok && f.Value != nil && x.extrapolated(rate) {
		f.Extrapolated = true
	}
	return f
}

// markExtrapolatedRate is markExtrapolated for a rate: f's own value is
// the rate.
func markExtrapolatedRate(fl Fleet, f Figure) Figure {
	if f.Value == nil || !slices.Contains(f.Flags, string(whatif.RankMixtureTTFT)) {
		return f
	}
	if x, ok := fl.(*mixtureFleet); ok && x.mm.top > 0 && *f.Value*x.scale > x.mm.top*(1+1e-9) {
		f.Extrapolated = true
	}
	return f
}

// view is the model at the fleet at k, for a plan's TTFT view.
func (mm *mainMixture) view(fl Fleet, k Knobs) *TTFTModelView {
	if mm == nil {
		return nil
	}
	v := &TTFTModelView{Model: mm.info.Model, Experimental: true, Label: mm.info.Label, Evidence: mm.info.Evidence}
	f, ok := fl.(*mixtureFleet)
	if !ok {
		v.NotPriced = mm.notPricedAt(k)
		if v.NotPriced == "" {
			v.NotPriced = "no knee answered"
		}
		return v
	}
	v.Priced = f.kneeOK
	if !f.kneeOK {
		v.NotPriced = "no knee answered to stop the curve at"
	}
	if f.mm.top > 0 {
		v.MeasuredUpTo = ptr(f.mm.top / f.scale)
		v.Beyond = TTFTBeyondMeasured
	}
	if f.lowerBound {
		v.RightEnd = TTFTRightEndEstimate
	}
	return v
}

// applyToReport prices the report's TTFT tables with the model, where it
// prices: the measured shape's rows at the recorded or asked fleet, each
// rate up to the shape's lower knee. The verdict and the summary cells are
// the planner's own, and the shape's assumed lines say so.
func (mm *mainMixture) applyToReport(out *ReportResponse, q ReportQuery) {
	if !mm.priced() || out.Fleet.Prefill <= 0 || statesTTFTTerm(q.Assume) || q.ArrivalCV2 != nil {
		return
	}
	scale := float64(mm.prefill) / float64(out.Fleet.Prefill)
	computed, hitFlag, hitNote := mm.computedAt(out.Fleet.Prefill)
	var flags []string
	for _, fl := range mm.modelFlags() {
		flags = append(flags, string(fl))
	}
	if hitFlag != "" {
		flags = append(flags, string(hitFlag))
	}
	for i := range out.Shapes {
		sh := &out.Shapes[i]
		if sh.Label != shapeMeasured {
			continue
		}
		pk, dk := sh.Capacity.PrefillKnee, sh.Capacity.DecodeKnee
		pv, dv := 0.0, 0.0
		if pk.Value != nil {
			pv = *pk.Value
		}
		if dk.Value != nil {
			dv = *dk.Value
		}
		knee, ok := curveCap(pv, pk.Value != nil, pk.LowerBound || pk.Refused != nil, dv, dk.Value != nil, mm.top/scale)
		if !ok {
			continue
		}
		moved := false
		for j := range sh.Table {
			row := &sh.Table[j]
			if !(row.Rate > 0) || row.Rate > knee*(1+1e-9) {
				continue
			}
			for s := range row.TTFT {
				st := &row.TTFT[s]
				p, err := parseStatistic("ttft", st.Statistic)
				if err != nil {
					continue
				}
				v, _, u := mm.rq.m.Price(row.Rate*scale, computed, mm.pooled.Prompt, p)
				if u != nil || !whatif.Finite(v) {
					continue
				}
				st.Figure = Figure{Value: ptr(v), Unit: unitSeconds, Flags: slices.Clone(flags),
					Extrapolated: mm.top > 0 && row.Rate*scale > mm.top*(1+1e-9)}
				moved = true
			}
		}
		if moved {
			// The shape's flags are every flag its figures carry: the
			// short fit window's and the prefix hit's as well.
			for _, fl := range flags {
				if !slices.Contains(sh.Flags, fl) {
					sh.Flags = append(sh.Flags, fl)
				}
			}
			line := TTFTModelLabel + " (" + mm.info.Model + ") prices the table's TTFT up to the lower knee"
			if mm.info.Evidence != "" {
				line += " (" + mm.info.Evidence + ")"
			}
			sh.Assumed = append(sh.Assumed, line+"; the verdict and the summary cells are the planner's own")
			if hitNote != "" {
				sh.Assumed = append(sh.Assumed, hitNote)
			}
		}
	}
}

// ApplyTTFTModel fits the experimental TTFT model on res, a source resolved
// with InputsRequest.TTFTModel, and prices out's TTFT tables with it as
// POST /v1/report does: at-replicas --experimental-ttft=v2 or =v2nl-line,
// fitted as o says (o.Stratified its --rank-mixture-stratified). It
// returns the model as fitted, abstained or not.
func ApplyTTFTModel(res *Resolved, out *ReportResponse, q ReportQuery, o TTFTModelOptions) *TTFTModelInfo {
	mm := fitMainMixture(&inputsEntry{doc: res.Inputs, series: res.Series, latency: res.Latency, mixture: res.Mixture, engine: res.Engine}, o.switches())
	mm.applyToReport(out, q)
	info := mm.info
	return &info
}

// The model's limits. Where the model prices a fleet's TTFT it also sets
// the prefill role's limit: the highest rate whose TTFT holds the target,
// read off the model's own curve (rateAtTarget), and the fleet's: the
// lowest of headroom times the curve's end (curveCap), that rate and
// decode's limit, as the planner composes its own. The curve's end is a
// knee the fleet is known to reach, not one it is known to stop at, where
// the prefill knee is only a lower bound or refused: a limit it sets is
// then "at least" (flagged VPLowerBound), and one past the highest load
// served is Extrapolated on the wire.

// rateAtTarget is the highest rate up to the curve's end whose TTFT the
// model prices at or under target, and whether the curve's end set it.
// ok is false where even the lowest rate misses it, or nothing is priced.
func (f *mixtureFleet) rateAtTarget(target float64) (rate float64, atEnd, ok bool) {
	if !f.kneeOK || !(target > 0) {
		return 0, false, false
	}
	if f.mm.atTarget != nil {
		return f.rateAtTargetShared(target)
	}
	meets := func(r float64) bool {
		v, ok := f.price(r)
		return ok && v <= target
	}
	if meets(f.knee) {
		return f.knee, true, true
	}
	// The search starts at a rate that meets the target, so that it never
	// answers 0 r/s where the curve is not monotone.
	lo, hi := 1e-6*f.knee, f.knee
	if !meets(lo) {
		return 0, false, false
	}
	for range 40 {
		mid := (lo + hi) / 2
		if meets(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, false, true
}

// histogramCount is the prefill count f's computed-token histogram is
// priced at (mainMixture.computedAt): the fleet's with PrefillHitAtCount,
// else 0, since every count then draws from the same histogram, and a
// rate one recorded replica's worth of ranks takes prices the same TTFT at
// any count.
func (f *mixtureFleet) histogramCount() int {
	if f.mm.hitAtCount {
		return f.n
	}
	return 0
}

// rateAtTargetShared is rateAtTarget on a day-plan bucket's model
// (mainMixture.atTarget), whose sizer asks it at many fleets of one prefill
// count: the fleet's rate is read off one search a computed-token
// histogram (sharedRate), capped at its curve's end, and refused where the model's
// TTFT with no queue (the floor) is over target. The TTFT is taken to rise
// with the rate, as rateAtTarget takes it.
func (f *mixtureFleet) rateAtTargetShared(target float64) (rate float64, atEnd, ok bool) {
	if v, priced := f.price(1e-6); !priced || v > target {
		return 0, false, false
	}
	x := f.sharedRate(target)
	if !(x > 0) {
		return 0, false, false
	}
	if x >= f.knee*f.scale {
		return f.knee, true, true
	}
	return x / f.scale, false, true
}

// sharedRate is the highest rate, as one recorded prefill replica's worth
// of ranks takes it, whose TTFT the model prices at or under target at the
// fleet's computed-token histogram: searched once a histogram
// (histogramCount), statistic and target (mainMixture.atTarget). It doubles from the highest step rate
// served until the target is missed, then bisects to a millionth of the
// rate: a replica count is decided far coarser.
func (f *mixtureFleet) sharedRate(target float64) float64 {
	key := mixturePriceKey{n: f.histogramCount(), x: target, q: f.q}
	if p, ok := f.mm.atTarget[key]; ok {
		return p.v
	}
	meets := func(x float64) bool {
		v, ok := f.price(x / f.scale)
		return ok && v <= target
	}
	lo, hi := 0.0, max(f.mm.top, 1e-9)
	for range 64 {
		if !meets(hi) {
			break
		}
		lo, hi = hi, 2*hi
	}
	for hi-lo > 1e-6*hi {
		mid := (lo + hi) / 2
		if meets(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	f.mm.atTarget[key] = mixturePrice{v: lo, ok: true}
	return lo
}

// prefillLimit is the prefill role's limit at target, as an answer, or a
// refusal where the model meets the target at no rate.
func (f *mixtureFleet) prefillLimit(target float64) whatif.Result {
	r, atEnd, ok := f.rateAtTarget(target)
	if !ok {
		return whatif.Unanswerable{Reason: whatif.TargetBelowFloor, Flags: f.mm.modelFlags(),
			Detail: fmt.Sprintf("the experimental TTFT model prices a TTFT over %.3g s at every rate", target)}
	}
	a := whatif.Answer{Value: r, Flags: f.limitFlags(atEnd)}
	a.Basis.BoundBy = []whatif.Bound{whatif.Bound("TTFT, " + f.mm.info.Model)}
	if atEnd {
		a.Basis.BoundBy = []whatif.Bound{whatif.Bound("the highest rate the experimental TTFT model prices")}
	}
	return a
}

// limitFlags is a model limit's flags: RankMixtureTTFT, and VPLowerBound
// where the curve's end set it and is a prefill knee that is only a lower
// bound (endAtLeast).
func (f *mixtureFleet) limitFlags(atEnd bool) []whatif.Flag {
	if atEnd && f.endAtLeast {
		return append(f.mm.modelFlags(), whatif.VPLowerBound)
	}
	return f.mm.modelFlags()
}

// fleetLimit is the fleet's limit at headroom: the lowest of headroom
// times the curve's end, the prefill limit and decode's.
func (f *mixtureFleet) fleetLimit(headroom float64, decode, prefill whatif.Result) whatif.Result {
	if u, ok := decode.(whatif.Unanswerable); ok {
		return u
	}
	p, ok := prefill.(whatif.Answer)
	if !ok {
		return prefill
	}
	type bound struct {
		rate  float64
		by    []whatif.Bound
		flags []whatif.Flag
	}
	bounds := []bound{{headroom * f.knee, []whatif.Bound{whatif.BoundOperatingPoint}, f.limitFlags(true)}, {p.Value, p.Basis.BoundBy, p.Flags}}
	if d, ok := decode.(whatif.Answer); ok {
		// Named as the planner's fleet limit names decode's bound.
		by := make([]string, len(d.Basis.BoundBy))
		for i, b := range d.Basis.BoundBy {
			by[i] = string(b)
		}
		bounds = append(bounds, bound{d.Value, []whatif.Bound{whatif.Bound("decode " + strings.Join(by, ", "))}, d.Flags})
	}
	lowest := bounds[0].rate
	for _, b := range bounds[1:] {
		lowest = min(lowest, b.rate)
	}
	a := whatif.Answer{Value: lowest}
	for _, b := range bounds {
		if b.rate != lowest {
			continue
		}
		a.Basis.BoundBy = append(a.Basis.BoundBy, b.by...)
		for _, fl := range b.flags {
			if !slices.Contains(a.Flags, fl) {
				a.Flags = append(a.Flags, fl)
			}
		}
	}
	for _, fl := range f.mm.modelFlags() {
		if !slices.Contains(a.Flags, fl) {
			a.Flags = append(a.Flags, fl)
		}
	}
	return a
}

// Limits is the planner's decode limit, and the model's prefill and fleet
// limits.
func (f *mixtureFleet) Limits(ttftSeconds, tpotSeconds, headroom float64) LimitSet {
	ls := f.Fleet.Limits(ttftSeconds, tpotSeconds, headroom)
	if !f.kneeOK {
		return ls
	}
	d1, _, _ := f.Fleet.PlainLimits(ttftSeconds, tpotSeconds, 1)
	ls.Prefill = f.prefillLimit(ttftSeconds)
	ls.Fleet = f.fleetLimit(headroom, ls.Decode, ls.Prefill)
	ls.WithoutHeadroom = f.fleetLimit(1, d1, ls.Prefill)
	return ls
}

// PlainLimits is Limits with no span: the model's limits have none.
func (f *mixtureFleet) PlainLimits(ttftSeconds, tpotSeconds, headroom float64) (decode, prefill, fleet whatif.Result) {
	decode, prefill, fleet = f.Fleet.PlainLimits(ttftSeconds, tpotSeconds, headroom)
	if !f.kneeOK {
		return decode, prefill, fleet
	}
	prefill = f.prefillLimit(ttftSeconds)
	return decode, prefill, f.fleetLimit(headroom, decode, prefill)
}
