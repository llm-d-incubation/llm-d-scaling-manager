package whatif

// TTFTMeets is the verdict on a TTFT priced at a rate (TTFTAt) against a
// target. The SLO analyzer's met/unmet is this function. RateForTTFT, which
// the CLI's and the planner API's rate answers and the analyzer's published
// rate all come from, answers only a rate this function calls met: it is
// capped under the prefill onset with flow control on (BoundPrefillOnset).
// The CLI and the API publish no verdict of their own; they carry the flags
// this function reads.
//
// known is false where the price says nothing of the target: a refusal for
// any reason other than the rate being past what the fleet serves. A TTFT
// answered meets the target when its figure is at most the target and the
// router's wait, if it is holding requests, is priced: AboveFlowControlOnset
// is a miss however low the figure, FlowControlWaitPriced is read by the
// figure. A refusal at or past the prefill knee (Saturated) or the decode
// onset (AboveDecodeOnset) is a miss.
func TTFTMeets(r Result, t TTFTTarget) (met, known bool) {
	switch v := r.(type) {
	case TTFT:
		return v.Seconds <= t.Seconds && !v.HasFlag(AboveFlowControlOnset), true
	case Unanswerable:
		if v.Reason == Saturated || v.Reason == AboveDecodeOnset {
			return false, true
		}
	}
	return false, false
}
