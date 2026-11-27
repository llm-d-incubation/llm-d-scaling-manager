package whatifapi

import (
	"fmt"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// This file prices a backtest's TPOT percentiles with the experimental
// per-request TPOT distribution (whatif.TPOTRenewalLognormal), where the
// request asks for it (BacktestRequest.TPOTModel). The mean TPOT stays the
// planner's: the distribution is placed around it. Its one free parameter,
// the renewal coefficient C, is fitted on the fit window's steps by
// interval-censored likelihood over every bucket of the router's TPOT
// histogram, and checked on the fit window's two self-check folds; no
// percentile is fitted, so p90 and p95 are both held out. Each step is also
// scored at the histogram's resolution (TPOTDistCheck): the predicted share
// under every bucket edge against the measured one. Every percentile it
// prices is flagged whatif.TPOTDistribution and reads OUTSIDE VALIDATED
// REGION.

// MinTPOTDistFitSteps is the fewest fit steps the distribution is fitted on
// (whatif.MinTPOTDistFitSteps).
const MinTPOTDistFitSteps = whatif.MinTPOTDistFitSteps

// TPOTDistReport is the experimental TPOT distribution's fit, as a
// backtest's answer carries it (BacktestResponse.TPOTDistribution):
// whatif.TPOTDistReport, the report the slo analyzer's v2 TPOT learner fills
// from the same fit.
type TPOTDistReport = whatif.TPOTDistReport

// TPOTDistScore is the distribution scored at the router histogram's
// resolution (whatif.TPOTDistScore).
type TPOTDistScore = whatif.TPOTDistScore

// TPOTDistCheck is the distribution at one test step: its log-variance and
// E[1/(L-1)], and its score at the histogram's resolution where measured.
type TPOTDistCheck struct {
	Sigma2 float64 `json:"sigma2"`
	Inv    float64 `json:"inv"`
	// KS is the largest gap under a bucket edge (TPOTDistScore), at the
	// predicted mean; P90Inside and P95Inside say the predicted percentile
	// lies in the measured one's joint bracket (TPOTDistScore).
	KS        *float64 `json:"ks,omitempty"`
	P90Inside *bool    `json:"p90Inside,omitempty"`
	P95Inside *bool    `json:"p95Inside,omitempty"`
	// Refused is why the step was not priced, "" where it was; Abstained
	// says the model abstained on the whole backtest.
	Refused   string `json:"refused,omitempty"`
	Abstained bool   `json:"abstained,omitempty"`
	// planner is the planner's own p90 and p95 the distribution replaced,
	// for the score's comparison; law the step's output lengths the
	// per-length mixture priced it at.
	planner []*float64
	law     []whatif.LengthGroup
}

// tpotDistQ is a backtest's fitted distribution (whatif.TPOTDistFitted).
type tpotDistQ struct {
	c      float64
	report TPOTDistReport
	// perLength says the distribution is the per-length mixture
	// (whatif.TPOTPerLengthMixture).
	perLength bool
	// stepRates is each fit step's mean arrival rate over the cycles it
	// holds that read one, beside the steps the fit read: the loads the
	// fit window served (tpot_model_main.go).
	stepRates []float64
}

// decodeWindow is the decode cycles of e at idx whose time is in (from, to].
func decodeWindow(e *inputsEntry, idx []int, from, to time.Time) []whatif.DecodeCycle {
	var out []whatif.DecodeCycle
	for _, i := range idx {
		if at := e.series[i].At; at.After(from) && !at.After(to) {
			out = append(out, e.decode[i])
		}
	}
	return out
}

// routerTPOTStep is the step anchored at the cycle a of e
// (whatif.RouterTPOTStepAt): its router TPOT histogram and mean over the
// router's window, the decode cycles of that window among idx, and the
// decode engines' TPOT histogram the cycle read (withEngineTPOT), which the
// step's joint bracket reads (whatif.JointBracket). ok is false where the
// router's TPOT or the output lengths are not read.
func routerTPOTStep(e *inputsEntry, idx []int, a int) (whatif.TPOTDistStep, bool) {
	if a >= len(e.latency) {
		return whatif.TPOTDistStep{}, false
	}
	anchor := fitCycles(e, []int{a})[0]
	return whatif.RouterTPOTStepAt(fitCycles(e, idx), &anchor)
}

// withEngineTPOT is st with the engines' TPOT histogram bs, its rates
// counted over the minute they were read over (whatif.WithEngineTPOT).
func withEngineTPOT(st whatif.TPOTDistStep, bs []domain.LatencyBucket) whatif.TPOTDistStep {
	return whatif.WithEngineTPOT(st, bs)
}

// fitTPOTDist fits the distribution on the fit entry's cycles at fitIdx, cut
// into steps of step (whatif.FitTPOTDistCycles); slots is the decode engine's
// max-num-seqs and spec its num_speculative_tokens. model is the
// distribution (whatif.TPOTRenewalLognormal, or whatif.TPOTPerLengthMixture).
// minSteps is the fewest steps it fits on, 0 MinTPOTDistFitSteps.
func fitTPOTDist(fit *inputsEntry, fitIdx []int, step time.Duration, slots, spec int, model string, minSteps int) *tpotDistQ {
	if len(fit.decode) != len(fit.series) {
		return &tpotDistQ{report: TPOTDistReport{Model: model, Experimental: true, FoldC: []float64{},
			Fold: []TPOTDistScore{}, SpecTokens: spec, DecodeSlots: slots, Abstained: "the fit source did not give the decode series"},
			perLength: model == whatif.TPOTPerLengthMixture}
	}
	return tpotDistQOf(whatif.FitTPOTDistCycles(fitCycles(fit, fitIdx), step, slots, spec, model, minSteps))
}

// tpotDistQOf is the backtest's distribution of the shared fit f.
func tpotDistQOf(f *whatif.TPOTDistFitted) *tpotDistQ {
	return &tpotDistQ{c: f.C, report: f.Report, perLength: f.PerLength, stepRates: f.StepRates}
}

// distAt is the fitted distribution at a step's group spread g2 and
// renewal coefficient c, at the step's lengths law where it is the
// per-length mixture (whatif.TPOTDistFitted.DistAt).
func (tq *tpotDistQ) distAt(g2, c float64, law []whatif.LengthGroup) whatif.TPOTDist {
	return (&whatif.TPOTDistFitted{Report: tq.report, PerLength: tq.perLength}).DistAt(g2, c, law)
}

// assumedLines is the distribution's assumed lines for the answer.
func (tq *tpotDistQ) assumedLines() []string {
	if tq.report.Abstained != "" {
		return []string{fmt.Sprintf("experimental TPOT distribution %s abstained (%s): every TPOT percentile is the planner's own, flagged %s",
			tq.report.Model, tq.report.Abstained, whatif.TPOTDistributionAbstained)}
	}
	return []string{tq.report.Assumed + "; no percentile is fitted (p90 and p95 are held out); every TPOT percentile is flagged " +
		string(whatif.TPOTDistribution) + " and is OUTSIDE VALIDATED REGION"}
}

// priceTPOTDist sets a step's TPOT percentiles from the distribution around
// the step's predicted mean, the step's output lengths and groups read over
// its window. Where it cannot price, the planner's figures stand, flagged.
func (q *backtestQuestion) priceTPOTDist(pred *prediction, out *BacktestStep, st stepOf) {
	flag := func(f whatif.Flag) {
		for i, s := range backtestStats {
			if s != 0 && !slices.Contains(pred.tpot[i].Flags, string(f)) {
				pred.tpot[i].Flags = append(pred.tpot[i].Flags, string(f))
			}
		}
	}
	if q.tpotDist.report.Abstained != "" {
		out.TPOTDist = &TPOTDistCheck{Abstained: true}
		flag(whatif.TPOTDistributionAbstained)
		return
	}
	at := q.test.series[st.anchor].At
	window := decodeWindow(q.test, st.window, st.windowStart, at)
	inv, g2, ok := whatif.DecodeWindow(window)
	mean := pred.tpot[0].Value
	switch {
	case !ok:
		out.TPOTDist = &TPOTDistCheck{Refused: "the step's decode pods read no output-length histogram"}
		flag(whatif.TPOTDistribution)
		return
	case mean == nil || !(*mean > 0):
		out.TPOTDist = &TPOTDistCheck{Refused: "the planner gave no mean TPOT to place the distribution around"}
		flag(whatif.TPOTDistribution)
		return
	}
	var law []whatif.LengthGroup
	if q.tpotDist.perLength {
		law, _ = whatif.DecodeWindowLaw(window)
	}
	d := q.tpotDist.distAt(g2, q.tpotDist.c, law)
	out.TPOTDist = &TPOTDistCheck{Sigma2: d.Sigma2(inv), Inv: inv, law: law}
	for i, s := range backtestStats {
		if s == 0 {
			continue
		}
		if s == 0.9 || s == 0.95 {
			out.TPOTDist.planner = append(out.TPOTDist.planner, pred.tpot[i].Value)
		}
		pred.tpot[i] = validate(figureOf(whatif.Answer{Value: d.Quantile(*mean, inv, s), Flags: q.tpotDist.report.Flags()},
			unitSeconds, true), whatif.FigurePercentileTPOT, s, whatif.FigureClass{}, 0)
		pred.tpotBound[i] = nil
	}
}

// tpotDistScorer pools steps' scores at the histogram's resolution
// (whatif.TPOTDistScorer).
type tpotDistScorer struct {
	whatif.TPOTDistScorer
}

// add is whatif.TPOTDistScorer.Add.
func (s *tpotDistScorer) add(d whatif.TPOTDist, st whatif.TPOTDistStep, mean float64, def []*float64) (float64, bool, bool) {
	return s.Add(d, st, mean, def)
}

// score is the pooled score.
func (s *tpotDistScorer) score() TPOTDistScore { return s.Score() }

// scoreTPOTDist scores every judged, priced step of out at the router
// histogram's resolution, steps[k] the layout of out.Steps[k].
func (q *backtestQuestion) scoreTPOTDist(steps []stepOf, out *BacktestResponse) {
	if q.tpotDist.report.Abstained != "" {
		return
	}
	var sc tpotDistScorer
	for k := range out.Steps {
		s := &out.Steps[k]
		chk := s.TPOTDist
		if chk == nil || chk.Refused != "" || s.Excluded != "" || steps[k].anchor < 0 {
			continue
		}
		st, ok := routerTPOTStep(q.test, steps[k].window, steps[k].anchor)
		mean := s.TPOT[0].Predicted.Value
		if !ok || mean == nil {
			continue
		}
		d := q.tpotDist.distAt(chk.Sigma2-q.tpotDist.c*chk.Inv, q.tpotDist.c, chk.law)
		st.Inv = chk.Inv
		ks, p90, p95 := sc.add(d, st, *mean, chk.planner)
		chk.KS, chk.P90Inside, chk.P95Inside = ptr(ks), &p90, &p95
	}
	t := sc.score()
	q.tpotDist.report.Test = &t
}

// note is the distribution's score at the histogram's resolution in words.
func (tq *tpotDistQ) note() string {
	t := tq.report.Test
	if tq.report.Abstained != "" || t == nil || t.Steps == 0 {
		return fmt.Sprintf("Experimental TPOT distribution %s: not scored at the histogram's resolution (it priced no judged step).",
			tq.report.Model)
	}
	return fmt.Sprintf("Experimental TPOT distribution %s at the router histogram's resolution, over %d judged steps: the largest gap "+
		"between the predicted and the measured share of requests under a bucket edge has median %.3f and p90 %.3f (%.3f and %.3f "+
		"placed at the measured mean: the shape alone); the predicted p90 lies in the measured p90's joint bracket (the router's "+
		"TPOT histogram and, above their first edge, the decode engines', at their sampling noise) on %d steps (the planner's own ratio: %d; "+
		"under it %d, over it %d), the p95 on %d (own: %d; under %d, over %d).",
		tq.report.Model, t.Steps, t.KSMedian, t.KSP90, t.ShapeKSMedian, t.ShapeKSP90, t.P90Inside, deref(t.DefaultP90Inside), t.P90Below,
		t.P90Above, t.P95Inside, deref(t.DefaultP95Inside), t.P95Below, t.P95Above)
}

// deref is *p, 0 for nil.
func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
