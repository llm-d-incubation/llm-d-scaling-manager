package whatif

// DefaultPriceRouterWait is TTFTFleet.PriceRouterWait's default, for the CLI
// (--price-router-wait) and the SLO analyzer (slo.SwitchTTFTRouterWait)
// alike: off. The wait it would price is argued and simulated, not measured.
const DefaultPriceRouterWait = false

// MaxRouterWaitKneeShare is the highest rate, as a share of the prefill knee,
// at which TTFTFleet.PriceRouterWait takes. Against a simulation of the
// gated prefill pods (running caps 1 to 256, the gate read live or every
// 50 ms and 200 ms, prompt cv 0.058 and 0.25, Poisson and Ca^2 4, three
// seeds), the processor-sharing price read no more than 4.8 % short of the
// gated pods up to 0.90 of the knee, inside the 5 % the stage reads short
// of its own simulation. At 0.95 and 0.97 it read up to 34 % short, at large
// caps where the router held least: the stage's own error near the knee, not
// the gate's. Past this share the wait keeps AboveFlowControlOnset
// (TestFlowControlWait_GatedPodsAreNoSlower).
const MaxRouterWaitKneeShare = 0.9

// TTFTMeets is the verdict on a TTFT priced at a rate (TTFTAt) against a
// target, the one the SLO analyzer's shadow pricing publishes and the CLI's
// and the planner's answers are read by. known is false where the price says
// nothing of the target: a refusal for any reason other than the rate being
// past what the fleet serves. A TTFT answered meets the target when its
// figure is at most the target and the router's wait, if it is holding
// requests, is priced: AboveFlowControlOnset is a miss however low the
// figure, FlowControlWaitPriced is read by the figure. A refusal at or past
// the prefill knee (Saturated) or the decode onset (AboveDecodeOnset) is a
// miss.
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
