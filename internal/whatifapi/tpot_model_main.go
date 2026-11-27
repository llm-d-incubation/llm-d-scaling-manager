package whatifapi

import (
	"fmt"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// This file prices the main view's TPOT percentiles (POST /v1/plan, /v1/size
// and /v1/report) with the experimental per-request TPOT distribution
// (whatif.TPOTRenewalLognormal), where the server (Config.TPOTModel) or the
// inputs request (InputsRequest.TPOTModel) asks for it, as the experimental
// TTFT model prices the TTFT (ttft_model_main.go). The distribution is
// fitted on the source's own window when the source is resolved, as a
// backtest fits it on its fit window (fitTPOTDist), and abstains where that
// fit does: the planner's own percentile then stands at every rate.
//
// The mean TPOT stays the planner's. The distribution is lognormal around
// it, its log-variance the decode replicas' spread plus C E[1/(L-1)] over
// the window's pooled output lengths, so a percentile is the mean times a
// ratio that does not move with the rate: the fleet prices its percentile
// curve, its point and decode's limit at a TPOT percentile target at that
// ratio in place of its own R_q (TPOTRatioFleet). Only p90 and p95 are
// priced: the two the self-check folds score. A rate above the highest
// step rate the fit window served, per decode replica, is marked
// Figure.Extrapolated.

// TPOTModelLabel names the experimental TPOT model on the TPOT figure and
// its curve.
const TPOTModelLabel = "Experimental TPOT model"

// tpotModelSource is the plan's TPOT ratio's source where the model sets it.
const tpotModelSource = "experimental TPOT model"

// TPOTModelInfo is the experimental TPOT model as fitted on a source
// (InputsResponse.TPOTModel).
type TPOTModelInfo struct {
	// Model is whatif.TPOTRenewalLognormal; Experimental is always true;
	// Label is TPOTModelLabel.
	Model        string `json:"model"`
	Experimental bool   `json:"experimental"`
	Label        string `json:"label"`
	// Abstained is why the model prices nothing, "" where it prices.
	Abstained string `json:"abstained,omitempty"`
	// HeldOut is the model scored on the fit window's self-check folds, and
	// Evidence that score in words; both absent where nothing was scored.
	HeldOut  *TPOTHeldOut `json:"heldOut,omitempty"`
	Evidence string       `json:"evidence,omitempty"`
	// MeasuredUpTo is the highest step rate the fit window served,
	// requests a second, at the recorded fleet.
	MeasuredUpTo *float64 `json:"measuredUpTo,omitempty"`
	// P90Ratio and P95Ratio are the p90 and p95 over the mean the model
	// prices at every rate; absent where it abstained.
	P90Ratio *float64 `json:"p90Ratio,omitempty"`
	P95Ratio *float64 `json:"p95Ratio,omitempty"`
	// Fit is the model's fit, as a backtest's answer carries it.
	Fit *TPOTDistReport `json:"fit,omitempty"`
}

// TPOTHeldOut is the experimental TPOT model scored on the source's
// self-check folds, each fold's C fitted on the steps before its cut alone
// and placed at each later step's measured mean: of Steps steps, P90Inside
// and P95Inside put the predicted p90 and p95 in the measured one's joint
// bracket (TPOTDistScore). No percentile is fitted, so both are
// held out. Decode is the recorded decode replicas.
type TPOTHeldOut struct {
	Steps     int `json:"steps"`
	P90Inside int `json:"p90Inside"`
	P95Inside int `json:"p95Inside"`
	Decode    int `json:"decode"`
}

// TPOTModelView is the experimental TPOT model at a plan's fleet
// (TPOTView.Model).
type TPOTModelView struct {
	Model        string `json:"model"`
	Experimental bool   `json:"experimental"`
	Label        string `json:"label"`
	// Priced says the model priced this fleet's TPOT percentile; NotPriced
	// is why not where it did not, and the planner's own stands.
	Priced    bool   `json:"priced"`
	NotPriced string `json:"notPriced,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
	// MeasuredUpTo is the highest load the fit window served, at this
	// fleet's decode count; a curve point above it is Extrapolated and
	// drawn as Beyond says.
	MeasuredUpTo *float64 `json:"measuredUpTo,omitempty"`
	Beyond       string   `json:"beyond,omitempty"`
}

// mainTPOT is the experimental TPOT model fitted on a source for the main
// view.
type mainTPOT struct {
	tq *tpotDistQ
	// dist is the fitted distribution and inv the source window's
	// E[1/(L-1)], pooled over its cycles: what every rate is priced at.
	dist whatif.TPOTDist
	inv  float64
	// top is the highest step rate the fit served; decode the recorded
	// decode replicas.
	top    float64
	decode int
	info   TPOTModelInfo
	// bucket says the model prices a day-plan bucket at its own shape
	// (experimentalModels.atBucket): the bucket's output length on the
	// fleet's knobs is then its own, not a stated one, and the ratio stays
	// the window's.
	bucket bool
}

// tpotModelOf is the TPOT model a source is resolved with: the request's,
// else the server's. An unknown model is the caller's error.
func tpotModelOf(req, server string) (string, error) {
	m := req
	if m == "" {
		m = server
	}
	if m != "" && !whatif.IsTPOTDistModel(m) {
		return "", InputErrorf("tpotModel %q, want %q, %q or none", m, whatif.TPOTRenewalLognormal, whatif.TPOTPerLengthMixture)
	}
	return m, nil
}

// fitMainTPOT fits the model on e's cycles up to the cycle answered, cut
// into DefaultBacktestStep steps, with the decode engine's max-num-seqs and
// num_speculative_tokens read from the source, on at least minSteps steps
// (0 MinTPOTDistFitSteps), as a backtest fits it (fitTPOTDist). model is
// the distribution (whatif.TPOTRenewalLognormal, or
// whatif.TPOTPerLengthMixture; "" the former). With the per-length mixture
// every rate is priced at the source window's pooled lengths, as the one
// lognormal is at their pooled E[1/(L-1)].
func fitMainTPOT(e *inputsEntry, model string, minSteps int) *mainTPOT {
	if model == "" {
		model = whatif.TPOTRenewalLognormal
	}
	mt := &mainTPOT{decode: e.doc.Fleet.Decode.Replicas,
		info: TPOTModelInfo{Model: model, Experimental: true, Label: TPOTModelLabel}}
	abstain := func(why string) *mainTPOT {
		mt.info.Abstained = why
		return mt
	}
	switch {
	case mt.decode <= 0:
		return abstain("the source has no decode replica")
	case len(e.decode) != len(e.series) || len(e.series) == 0:
		return abstain("the source did not give the decode series")
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
	// The slo analyzer's v2 TPOT learner fits and prices its own cycles with
	// the same call.
	w := whatif.FitTPOTWindow(fitCycles(e, idx), DefaultBacktestStep, e.engine.DecodeSlots, e.engine.SpecTokens, model, minSteps)
	tq := tpotDistQOf(w.Fitted)
	r := tq.report
	mt.info.Fit = &r
	if w.Abstained != "" {
		return abstain(w.Abstained)
	}
	mt.tq, mt.inv, mt.dist = tq, w.Inv, w.Dist
	mt.info.P90Ratio, mt.info.P95Ratio = ptr(mt.ratio(0.9)), ptr(mt.ratio(0.95))
	mt.top = w.Top
	if mt.top > 0 {
		mt.info.MeasuredUpTo = ptr(mt.top)
	}
	var h TPOTHeldOut
	for _, f := range r.Fold {
		h.Steps += f.Steps
		h.P90Inside += f.P90Inside
		h.P95Inside += f.P95Inside
	}
	if h.Steps > 0 {
		h.Decode = mt.decode
		mt.info.HeldOut = &h
		mt.info.Evidence = tpotHeldOutWords(h)
	}
	return mt
}

// tpotHeldOutWords is a held-out score in the page's words.
func tpotHeldOutWords(h TPOTHeldOut) string {
	return fmt.Sprintf("held-out steps, at the measured mean: p90 in the measured p90's joint bracket on %d of %d, p95 on %d",
		h.P90Inside, h.Steps, h.P95Inside)
}

// priced says the model prices.
func (mt *mainTPOT) priced() bool {
	return mt != nil && mt.tq != nil && mt.info.Abstained == ""
}

// notPricedAt is why the model does not price the fleet at k, "" where it
// does: it prices p90 and p95, the statistics its folds score, at the
// source's own output lengths, so a stated output length or a stated R_q
// is the planner's alone.
func (mt *mainTPOT) notPricedAt(k Knobs) string {
	switch {
	case !mt.priced():
		return "the model abstained: " + mt.info.Abstained
	case k.TPOTPercentile != 0.9 && k.TPOTPercentile != 0.95:
		return "the model prices p90 and p95, not " + statisticName(k.TPOTPercentile)
	case k.Decode <= 0:
		return "the fleet has no decode replica"
	case k.OutputTokens != 0 && !mt.bucket:
		return "the model prices the source's own output lengths, not a stated one"
	case k.Assume.TPOTRatio != nil:
		return "the ratio stated (assume.tpotRatio) prices the percentile"
	}
	return ""
}

// ratio is the percentile over the mean at q: the same at every rate.
func (mt *mainTPOT) ratio(q float64) float64 { return mt.dist.Quantile(1, mt.inv, q) }

// fleet is inner with its TPOT percentile priced at the model's ratio, or
// inner where the model does not price at k or inner takes no ratio.
func (mt *mainTPOT) fleet(inner Fleet, k Knobs) Fleet {
	if mt == nil || mt.notPricedAt(k) != "" {
		return inner
	}
	x, ok := inner.(TPOTRatioFleet)
	if !ok {
		return inner
	}
	return x.WithTPOTRatio(ModelRatio{Value: mt.ratio(k.TPOTPercentile), Flag: whatif.TPOTDistribution, More: mt.tq.report.Flags()[1:], Source: tpotModelSource,
		Assumed: mt.assumed(k)})
}

// assumed is the fleet's assumed line for the model at k.
func (mt *mainTPOT) assumed(k Knobs) string {
	s := mt.tq.report.Assumed + fmt.Sprintf("; TPOT %s priced at %.3g times the planner's mean, at the source window's pooled output "+
		"lengths and decode spread", statisticName(k.TPOTPercentile), mt.ratio(k.TPOTPercentile))
	if k.Decode != mt.decode {
		s += fmt.Sprintf(", read at %d decode replicas and taken at %d", mt.decode, k.Decode)
	}
	return s
}

// extrapolated says rate, at k's decode replicas, is above the highest
// load the fit window served at the recorded ones.
func (mt *mainTPOT) extrapolated(k Knobs, rate float64) bool {
	return mt.notPricedAt(k) == "" && mt.top > 0 && rate*float64(mt.decode)/float64(k.Decode) > mt.top*(1+1e-9)
}

// mark sets f.Extrapolated where the model priced f (flagged
// whatif.TPOTDistribution) at a rate beyond what was measured: rate, or
// f's own value where f is a rate (rate 0).
func (mt *mainTPOT) mark(k Knobs, rate float64, f Figure) Figure {
	if mt == nil || f.Value == nil || !slices.Contains(f.Flags, string(whatif.TPOTDistribution)) {
		return f
	}
	if rate == 0 {
		rate = *f.Value
	}
	if mt.extrapolated(k, rate) {
		f.Extrapolated = true
	}
	return f
}

// markPoints is pts with mark on each, in a copy.
func (mt *mainTPOT) markPoints(k Knobs, pts []Point) []Point {
	if mt == nil {
		return pts
	}
	out := slices.Clone(pts)
	for i := range out {
		out[i].Figure = mt.mark(k, out[i].Rate, out[i].Figure)
	}
	return out
}

// view is the model at the fleet at k, for a plan's TPOT view.
func (mt *mainTPOT) view(fl Fleet, k Knobs) *TPOTModelView {
	if mt == nil {
		return nil
	}
	v := &TPOTModelView{Model: mt.info.Model, Experimental: true, Label: TPOTModelLabel, Evidence: mt.info.Evidence}
	v.NotPriced = mt.notPricedAt(k)
	if v.NotPriced == "" {
		if r := fl.TPOTRatio(); !slices.Contains(r.Flags, string(whatif.TPOTDistribution)) {
			v.NotPriced = "the fleet takes no ratio from the model"
		}
	}
	if v.NotPriced != "" {
		return v
	}
	v.Priced = true
	if mt.top > 0 {
		v.MeasuredUpTo = ptr(mt.top * float64(k.Decode) / float64(mt.decode))
		v.Beyond = TTFTBeyondMeasured
	}
	return v
}

// applyToReport prices the report's TPOT percentiles with the model, where
// it prices: the measured shape's rows at the recorded or asked fleet, each
// row's p90 or p95 the model's ratio times the row's own mean. The verdict,
// the capacity and the summary cells are the planner's own, and the
// shape's assumed lines say so.
func (mt *mainTPOT) applyToReport(out *ReportResponse, q ReportQuery) {
	if !mt.priced() || out.Fleet.Decode <= 0 || q.Assume.TPOTRatio != nil {
		return
	}
	k := Knobs{Decode: out.Fleet.Decode}
	for i := range out.Shapes {
		sh := &out.Shapes[i]
		if sh.Label != shapeMeasured {
			continue
		}
		moved := false
		for j := range sh.Table {
			row := &sh.Table[j]
			st, mean := row.TPOTPercentile, row.TPOTMean.Value
			if st == nil || mean == nil || !(*mean > 0) || !(row.Rate > 0) {
				continue
			}
			p, err := parseStatistic("tpot", st.Statistic)
			if err != nil {
				continue
			}
			k.TPOTPercentile = p
			if mt.notPricedAt(k) != "" {
				continue
			}
			v := mt.ratio(p) * *mean
			if !whatif.Finite(v) {
				continue
			}
			st.Figure = Figure{Value: ptr(v), Unit: unitSeconds, Flags: flagStrings(mt.tq.report.Flags()),
				Extrapolated: mt.extrapolated(k, row.Rate)}
			moved = true
		}
		if moved {
			for _, fl := range flagStrings(mt.tq.report.Flags()) {
				if !slices.Contains(sh.Flags, fl) {
					sh.Flags = append(sh.Flags, fl)
				}
			}
			line := TPOTModelLabel + " (" + mt.info.Model + ") prices the table's TPOT percentile around its mean"
			if mt.info.Evidence != "" {
				line += " (" + mt.info.Evidence + ")"
			}
			sh.Assumed = append(sh.Assumed, line+"; the verdict, the capacity and the summary cells are the planner's own")
		}
	}
}

// ApplyTPOTModel fits the experimental TPOT model on res, a source resolved
// with InputsRequest.TPOTModel, and prices out's TPOT percentiles with it as
// POST /v1/report does: at-replicas --experimental-tpot=v2, on at least
// minSteps fit steps (0 MinTPOTDistFitSteps). It returns the model as
// fitted, abstained or not.
func ApplyTPOTModel(res *Resolved, out *ReportResponse, q ReportQuery, minSteps int) *TPOTModelInfo {
	mt := fitMainTPOT(&inputsEntry{doc: res.Inputs, series: res.Series, latency: res.Latency, decode: res.Decode, engine: res.Engine}, res.TPOTModel, minSteps)
	mt.applyToReport(out, q)
	info := mt.info
	return &info
}

// experimentalModels is the experimental models a source was resolved
// with, either nil: what a plan and a size price with, and a day plan
// asked for them (DayPlanRequest.ExperimentalModels; sizer.models).
type experimentalModels struct {
	ttft *mainMixture
	tpot *mainTPOT
}

// models is e's experimental models, nil where it was resolved with none.
func (e *inputsEntry) models() *experimentalModels {
	if e.ttftModel == nil && e.tpotModel == nil {
		return nil
	}
	return &experimentalModels{ttft: e.ttftModel, tpot: e.tpotModel}
}

// fleet is fl with the models: the TPOT model's ratio first, since it
// replaces the fleet the TTFT model then wraps; fl itself where m is nil.
func (m *experimentalModels) fleet(fl Fleet, k Knobs) Fleet {
	if m == nil {
		return fl
	}
	return m.ttft.fleet(m.tpot.fleet(fl, k), k)
}

// markLimit is markExtrapolatedRate for a limit fl priced at k, with the
// TPOT model's mark beside the TTFT model's: a size's step and a plan's
// one-more-replica step are marked as the plan's own limits are. With m
// nil, f is unchanged.
func (m *experimentalModels) markLimit(fl Fleet, k Knobs, f Figure) Figure {
	if m == nil {
		return f
	}
	return m.tpot.mark(k, 0, markExtrapolatedRate(fl, f))
}

// modelFleet is fl with the experimental models e was resolved with.
func (e *inputsEntry) modelFleet(fl Fleet, k Knobs) Fleet { return e.models().fleet(fl, k) }

// markLimit is experimentalModels.markLimit with e's models.
func (e *inputsEntry) markLimit(fl Fleet, k Knobs, f Figure) Figure {
	return e.models().markLimit(fl, k, f)
}
