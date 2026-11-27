package whatifapi

import (
	"context"
	"fmt"
	"math"
	"time"
)

// CachedTokenCost is phi, what a cached prompt token costs a request on a
// prefill pod over what a computed one costs: the request's own latency
// carries phi x cached / V_P beside its computed prompt. A token served
// from a host tier or a peer is not computed, but its KV is loaded before
// the request's prefill can use it. phi is one figure per fleet, fitted on
// the source's own steps (FitCachedTokenCost), and 0 where it is not fitted:
// Why says why, and nothing the fit would move moves. The replay switch
// slo.SwitchTTFTCachedTokenCost turns it on.
type CachedTokenCost struct {
	// Phi is in [0, 1].
	Phi float64 `json:"phi"`
	// Fitted says Phi was fitted on Steps steps of the source; false, Phi is
	// 0 and Why says why.
	Fitted bool `json:"fitted"`
	Steps  int  `json:"steps"`
	// CachedShare is the prefill variant's computed-token hit: the share of
	// the whole prompt the engines did not compute, over every tier.
	CachedShare float64 `json:"cachedShare"`
	// FirstHalf and SecondHalf are phi fitted the same way on the earlier
	// and the later half of the steps, for how stable it is; absent where
	// not fitted.
	FirstHalf  *float64 `json:"firstHalf,omitempty"`
	SecondHalf *float64 `json:"secondHalf,omitempty"`
	Why        string   `json:"why,omitempty"`
}

// MinCachedTokenCostSteps is the fewest steps phi is fitted on: an hour of
// five-minute steps. Fewer leave phi at 0.
const MinCachedTokenCostSteps = 12

// MinCachedTokenCostShare is the least cached share phi is fitted at: below
// it the load the term prices is within what a minute's counters resolve,
// whatif.MaterialPrefixHitMove.
const MinCachedTokenCostShare = 0.1

// The reasons phi is not fitted.
const (
	CachedCostNoQueueVP    = "V_P was not learned under a queue"
	CachedCostNoShare      = "no material cached share"
	CachedCostTooFewSteps  = "too few steps"
	CachedCostNoStepPriced = "no step priced"
)

// Words is the cost as the assumed row names it.
func (c CachedTokenCost) Words() string {
	if !c.Fitted {
		return "cached-token cost phi 0, not fitted: " + c.Why
	}
	s := fmt.Sprintf("cached-token cost phi %.3f, fitted on %d steps at a cached share of %.3f", c.Phi, c.Steps, c.CachedShare)
	if c.FirstHalf != nil && c.SecondHalf != nil {
		s += fmt.Sprintf(" (first half %.3f, second half %.3f)", *c.FirstHalf, *c.SecondHalf)
	}
	return s
}

// NotFittedCachedTokenCost is phi 0 with the reason it was not fitted.
func NotFittedCachedTokenCost(share float64, why string) CachedTokenCost {
	return CachedTokenCost{CachedShare: share, Why: why}
}

// FitCachedTokenCost fits phi on res's own steps up to until, the cycle res
// was read at, as a backtest prices and scores them: each step judged, its
// mean TTFT scored (measured, not refused, settled) and below the knee, is
// priced at phi 0 and at phi 1, and its implied phi is the measured mean
// TTFT less the first over the second less the first. The prices are linear
// in phi (the load is a fixed part of the request's own latency), so that is
// the phi at which the step's mean TTFT is priced as measured. phi is the
// median of the steps' implied phi, held to [0, 1]: a step the model reads
// high for another reason pulls phi to 0, never under it. The rule was fixed
// before any recording was scored with it.
//
// res's pricer must take Knobs.CachedTokenCost, and share is the cached
// share it prices (CachedTokenCost.CachedShare). Fewer than
// MinCachedTokenCostSteps steps, or a share under MinCachedTokenCostShare,
// leave phi at 0 with the reason.
func FitCachedTokenCost(ctx context.Context, res *Resolved, until time.Time, share float64) (CachedTokenCost, error) {
	if !(share >= MinCachedTokenCostShare) {
		return NotFittedCachedTokenCost(share, fmt.Sprintf("%s (%.3f, under %.2f)", CachedCostNoShare, share, MinCachedTokenCostShare)), nil
	}
	if len(res.Series) == 0 || len(res.Latency) != len(res.Series) {
		return NotFittedCachedTokenCost(share, CachedCostNoStepPriced+": the source carries no measured latency"), nil
	}
	doc := res.Inputs
	doc.InputsID = contentID("inputs", res.Content)
	e := &inputsEntry{id: doc.InputsID, doc: doc, pricer: res.Pricer, series: res.Series, latency: res.Latency, config: res.Config}
	// The test is the source's own cycles up to until: a cutoff before its
	// first cycle, so none is the fit's.
	q, err := questionOf(e, e, BacktestRequest{AllowConfigMismatch: true}, res.Series[0].At.Add(-time.Nanosecond), until, "", false)
	if err != nil {
		return NotFittedCachedTokenCost(share, fmt.Sprintf("%s: %v", CachedCostNoStepPriced, err)), nil
	}
	zero, one := 0.0, 1.0
	q.cachedCost = &zero
	at0, err := q.run(ctx)
	if err != nil {
		return CachedTokenCost{}, err
	}
	q.cachedCost = &one
	_, at1, err := q.predictSteps(ctx)
	if err != nil {
		return CachedTokenCost{}, err
	}
	implied := impliedPhi(at0.Steps, at1)
	out := CachedTokenCost{CachedShare: share, Steps: len(implied)}
	if len(implied) < MinCachedTokenCostSteps {
		out.Why = fmt.Sprintf("%s (%d, under %d)", CachedCostTooFewSteps, len(implied), MinCachedTokenCostSteps)
		return out, nil
	}
	out.Fitted, out.Phi = true, heldPhi(median(implied))
	h := len(implied) / 2
	first, second := heldPhi(median(implied[:h])), heldPhi(median(implied[h:]))
	out.FirstHalf, out.SecondHalf = &first, &second
	return out, nil
}

// impliedPhi is each step's implied phi, in time order: scored is the
// steps priced at phi 0 and scored, at1 the same steps priced at phi 1.
func impliedPhi(scored, at1 []BacktestStep) []float64 {
	var out []float64
	for i, s := range scored {
		if i >= len(at1) || s.Excluded != "" || s.Unsettled || s.Load == "" || s.Load == LoadPastKnee ||
			len(s.TTFT) == 0 || len(at1[i].TTFT) == 0 {
			continue
		}
		p := s.TTFT[0]
		p1 := at1[i].TTFT[0].Predicted.Value
		// A TTFT priced from a lower bound of V_P is scored apart
		// (UnscoredVPLowerBound), but it is a price beside a measurement, as
		// the fit needs: phi is read from it as from any other.
		unscored := p.Unscored != "" && p.Unscored != UnscoredVPLowerBound
		if p.Statistic != statisticMean || unscored || p.Measured == nil || p.Predicted.Value == nil || p1 == nil {
			continue
		}
		d := *p1 - *p.Predicted.Value
		if !(d > 0) || math.IsInf(d, 0) {
			continue
		}
		out = append(out, (*p.Measured-*p.Predicted.Value)/d)
	}
	return out
}

// heldPhi is x held to [0, 1].
func heldPhi(x float64) float64 { return min(max(x, 0), 1) }
