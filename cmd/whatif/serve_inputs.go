package main

import (
	"fmt"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The refusals an inputs document carries where the source has no figure.
const (
	// reasonNotRecorded is a figure the replayed state does not carry.
	reasonNotRecorded = "NotRecorded"
	// reasonNoPerRequestLengths is a mix with no lengths per request.
	reasonNoPerRequestLengths = "NoPerRequestLengths"
)

// sideLine is a decode side priced by the decode variant's line
// (pricedFleet.decodeSide).
const sideLine = "line"

// inputsDoc is the inputs document of a fleet read at the recorded counts:
// what the source saw, the policy's SLO, and every term with its source, in
// the words and figures the CLI's JSON gives them (wireTTFT, wireInputs). A
// term the fleet did not reach before a refusal is absent; the refusal names
// the term it stopped at.
func inputsDoc(f *answerFlags, st *learnreplay.State, pv *pricedVariant, ps *learnreplay.PrefillState, pf *pricedFleet,
	info whatifapi.SourceInfo) whatifapi.InputsResponse {
	doc := whatifapi.InputsResponse{Model: st.ModelID, Namespace: st.Namespace, Source: info,
		Fleet:   whatifapi.FleetInfo{Decode: whatifapi.RoleInfo{Variant: pv.state.Variant, Replicas: pv.decode.Replicas}},
		Assumed: append([]string{}, pf.in.Assumed...)}
	if ps != nil {
		doc.Fleet.Prefill = &whatifapi.RoleInfo{Variant: ps.Variant, Replicas: ps.Pods}
	}
	doc.Observed = observedOf(pv, pf)
	doc.Policy = policyOf(f, st, pf)
	doc.Terms = termsOf(f, st, pv, pf)
	return doc
}

// figure is an answered figure in unit.
func figure(v float64, unit string) whatifapi.Figure { return whatifapi.Figure{Value: &v, Unit: unit} }

// refused is a refused figure.
func refused(reason, detail string) whatifapi.Figure {
	return whatifapi.Figure{Refused: &whatifapi.Refusal{Reason: reason, Detail: detail}}
}

// refusedBy is the figure u refuses.
func refusedBy(u whatif.Unanswerable) whatifapi.Figure {
	return whatifapi.Figure{Refused: &whatifapi.Refusal{Reason: string(u.Reason), Detail: u.Detail, Flags: flagStrings(u.Flags),
		AssumedTermDecides: u.AssumedTermDecides}}
}

// observedOf is the traffic the source saw: the prompt the prefill pods
// computed (else decode's), the output, and the prompt's spread.
func observedOf(pv *pricedVariant, pf *pricedFleet) whatifapi.Observed {
	o := whatifapi.Observed{
		Rate: refused(reasonNotRecorded, "the replayed state carries no request rate per model; "+
			"a plan's rate defaults to the operating point, headroom times the lower knee"),
		OutputTokens: figure(pv.shape.AvgOutputTokens, "tokens"),
		Buckets:      whatifapi.Buckets(),
		Mix: whatifapi.ObservedMix{Refused: &whatifapi.Refusal{Reason: reasonNoPerRequestLengths,
			Detail: "the source carries the mean prompt and its spread, not a length per request: no share per bucket, " +
				"so the \"if all traffic were bucket b\" view shows every bucket"}},
	}
	prompt := pv.shape.AvgInputTokens
	if pf.refused == nil && pf.in.PromptTokens > 0 {
		prompt = pf.in.PromptTokens
	}
	o.PromptTokens = figure(prompt, "tokens")
	o.PromptBucket = whatifapi.BucketOf(prompt)
	if pf.refused != nil {
		o.PromptCV = refusedBy(*pf.refused)
	} else {
		o.PromptCV = figure(pf.in.PromptCV, "")
		if pf.fleet.Prefill.PromptCVAssumed {
			o.PromptCV.Flags = []string{string(whatif.AssumedPromptSpread)}
		}
	}
	return o
}

// policyOf is the policy's SLO, the headroom the CLI reads, and the
// flow-control state the fleet was priced at.
func policyOf(f *answerFlags, st *learnreplay.State, pf *pricedFleet) whatifapi.PolicyInfo {
	p := whatifapi.PolicyInfo{Headroom: f.headroomFor(st.Policy), FlowControl: pf.in.FlowControl}
	if p.FlowControl == "" {
		p.FlowControl = "not read"
	}
	if st.Policy == nil || st.Policy.SLO == nil {
		return p
	}
	if t := st.Policy.SLO.TTFT; t != nil {
		p.TTFT = &whatifapi.PolicyObjective{Statistic: fmt.Sprintf("p%d", st.Policy.SLO.TTFTPercentile()), Seconds: t.Target.Seconds()}
	}
	if t := st.Policy.SLO.TPOT; t != nil {
		p.TPOT = &whatifapi.PolicyObjective{Statistic: "mean", Seconds: t.Target.Seconds()}
	}
	return p
}

// termsOf is every term the fleet was read from, as the CLI's JSON names its
// source.
func termsOf(f *answerFlags, st *learnreplay.State, pv *pricedVariant, pf *pricedFleet) []whatifapi.Term {
	var terms []whatifapi.Term
	add := func(name, source string, fig whatifapi.Figure) {
		terms = append(terms, whatifapi.Term{Name: name, Source: source, Figure: fig})
	}
	in := pf.in
	switch {
	case pf.refused == nil:
		add("vp", in.VPSource+" "+in.VPOrigin, figure(in.VP, "tokens/s"))
		add("a", in.FixedLatencySource, figure(in.FixedLatencySeconds, "s"))
		add("imbalance", in.ImbalanceSource, figure(in.Imbalance, ""))
		add("promptTokens", in.PromptSource, figure(in.PromptTokens, "tokens"))
		add("promptCV", in.PromptCVSource, figure(in.PromptCV, ""))
		add("transferSecondsPerToken", in.TransferSource, figure(in.TransferSecondsPerToken, "s"))
		if in.DecodeSide == sideLine {
			add("routerHop", in.RouterHopSource, figure(in.RouterHopBaseSeconds, "s"))
		}
	case pf.refused.Reason == whatif.NoFixedLatency:
		add("vp", in.VPSource+" "+in.VPOrigin, figure(in.VP, "tokens/s"))
		add("a", "", refusedBy(*pf.refused))
	case pf.refused.Reason == whatif.NoPrefillCapacity:
		add("vp", "", refusedBy(*pf.refused))
	default:
		add("prefill", "", refusedBy(*pf.refused))
	}
	if knee := whatif.KneeRate(pv.decode); isRefused(knee) {
		add("itlLine", "", refusedBy(pv.qualify(knee).(whatif.Unanswerable)))
	} else {
		add("itlA", pv.state.Tier, figure(pv.decode.Line.A, "s"))
		add("itlB", pv.state.Tier, figure(pv.decode.Line.B, "s"))
	}
	add("capacityTokens", "", figure(pv.decode.CapacityTokens, "tokens"))
	add("kvPerRequest", "", figure(pv.decode.KVPerRequest, "tokens"))
	add("batchCap", pv.state.BatchCapSource, figure(float64(pv.perRank), "sequences per rank"))
	// R_q at p90, a TPOT percentile's usual ask; a plan reads its own.
	r := f.ratioFor(st, 0.9)
	if r.refused != nil {
		add("tpotRatio", "p90", refusedBy(*r.refused))
	} else {
		fig := figure(r.ratio.Value, "")
		fig.Flags = flagStrings(r.flags)
		add("tpotRatio", "p90 "+sourceLearned, fig)
	}
	return terms
}

// isRefused reports whether r is a refusal.
func isRefused(r whatif.Result) bool {
	_, ok := r.(whatif.Unanswerable)
	return ok
}
