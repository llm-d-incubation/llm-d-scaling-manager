package main

import (
	"context"
	"fmt"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The cost of a cached prompt token (slo.SwitchTTFTCachedTokenCost): a
// request's own latency on a prefill pod carries phi x cached / V_P beside
// its computed prompt, phi fitted on the source's own steps
// (whatifapi.FitCachedTokenCost). The load is a fixed part of the request's
// own latency, as a is: it never enters the pod's work, so no knee moves.

// cachedCostWords is the assumed row of a fleet with the switch on and no
// fit: an answer that reads no steps (the one-shot commands, an at-replicas
// answer with the self-check off).
const cachedCostWords = "cached-token cost phi 0, not fitted: this answer reads no steps to fit it on"

// queueLearnedVP says V_P was learned under a queue: from busy minutes or the
// peak under a queue, not the back-pressure bound, not a served peak, and
// not raised to the served peak. Only such a V_P prices the steps phi is
// fitted on.
func queueLearnedVP(ps *learnreplay.PrefillState) (bool, string) {
	est := ps.Estimate
	switch {
	case !ps.EstimateRead || !(est.VP > 0) || est.VPRefused != "":
		return false, "no V_P learned"
	case est.VPFlooredAtServed:
		return false, "V_P raised to the served peak"
	case est.VPSource != prefill.SourceBusyMinutes && est.VPSource != prefill.SourcePeakUnderQueue:
		return false, fmt.Sprintf("V_P from %s", est.VPSource)
	}
	return true, ""
}

// fitCachedCost is phi for the fleet pricer prices, fitted on base's series
// up to the cycle ps was read at: 0 with the reason where ps's V_P was not
// learned under a queue or its cached share is not read.
func fitCachedCost(ctx context.Context, ps *learnreplay.PrefillState, base *whatifapi.Resolved, pricer *servedState) whatifapi.CachedTokenCost {
	share, ok := ps.PromptWindow.ComputedHit()
	if !ok {
		return whatifapi.NotFittedCachedTokenCost(0, whatifapi.CachedCostNoShare+" (no computed-token hit read)")
	}
	if ok, why := queueLearnedVP(ps); !ok {
		return whatifapi.NotFittedCachedTokenCost(share, whatifapi.CachedCostNoQueueVP+" ("+why+")")
	}
	r := *base
	r.Pricer = pricer
	c, err := whatifapi.FitCachedTokenCost(ctx, &r, pricer.st.At, share)
	if err != nil {
		return whatifapi.NotFittedCachedTokenCost(share, err.Error())
	}
	return c
}

// fitCachedCosts fits phi for res's pricer and each of its folds' where the
// replay turned slo.SwitchTTFTCachedTokenCost on, each on the steps up to
// its own cycle, and names the answer's in res's inputs.
func (f *answerFlags) fitCachedCosts(ctx context.Context, ps *learnreplay.PrefillState, res *whatifapi.Resolved, pricer *servedState) {
	if ps == nil || !ps.CachedTokenCost {
		return
	}
	c := fitCachedCost(ctx, ps, res, pricer)
	pricer.cachedCost = &c
	f.cachedCost = &c
	res.Inputs.CachedTokenCost = &c
	res.Inputs.Assumed = append(res.Inputs.Assumed, c.Words())
	for _, fold := range res.Folds {
		fp, ok := fold.Pricer.(*servedState)
		if !ok {
			continue
		}
		fps, err := f.prefillState(fp.st)
		if err != nil || fps == nil {
			continue
		}
		fc := fitCachedCost(ctx, fps, res, fp)
		fp.cachedCost = &fc
	}
}

// priceCachedLoad adds phi x cached / V_P to the request's own latency,
// where the replay turned slo.SwitchTTFTCachedTokenCost on, phi is above 0
// and the prefill variant's cached share is at least
// whatif.MaterialPrefixHitMove. The cached tokens are the whole prompt asked
// times the computed-token hit, or, at the measured prompt, the computed
// prompt times hit / (1 - hit): the same tokens, read from the computed
// side. V_P is the learned (or stated) one, before its rescale to the prompt
// and its margin.
func (pf *pricedFleet) priceCachedLoad(f *answerFlags) {
	if !pf.cachedTokenCost {
		return
	}
	phi := f.cachedPhi
	switch {
	case f.cachedPhiSet:
	case f.cachedCost != nil:
		pf.assume("%s", f.cachedCost.Words())
	default:
		pf.assume("%s", cachedCostWords)
		return
	}
	if !(phi > 0) {
		return
	}
	hit, ok := pf.pricedComputedHit()
	if !ok || hit < whatifapi.MinCachedTokenCostShare {
		return
	}
	p := &pf.fleet.Prefill
	cached := p.WholePromptTokens * hit
	if pf.in.PromptSource == shapeMeasured {
		cached = p.PromptTokens * hit / (1 - hit)
	}
	if !(cached > 0) || !(p.VP > 0) {
		return
	}
	load := phi * cached / p.VP
	p.FixedLatency += load
	pf.assume("%s cached prompt tokens a request priced at phi %.3f of a computed token: %.1f ms of load in each request's own latency",
		num(round(cached)), phi, 1000*load)
}
