package capacity

// The engine flags the experimental latency models read
// (docs/proposals/predicted-latency-in-controller.md): the capacity planner
// reads them from a recording's variants, the slo analyzer from the
// variants it prices, both through these.

// PrefillSlots is K, the prompts one prefill rank runs at once, as the
// rank-mixture TTFT model prices it: the max-num-seqs the flags state
// (ExplicitMaxNumSeqs), 1 where chunked prefill is off. 0 is not known: no
// params, or no max-num-seqs stated.
func (p *EngineParams) PrefillSlots() int {
	if p == nil {
		return 0
	}
	k := int(p.ExplicitMaxNumSeqs())
	if k > 0 && !p.ChunkedPrefillEnabled {
		return 1
	}
	return k
}

// DecodeSlots is the decode engine's max-num-seqs as the flags state it and
// its num_speculative_tokens, as the TPOT distribution reads them; 0 and 0
// with no params.
func (p *EngineParams) DecodeSlots() (slots, spec int) {
	if p == nil {
		return 0, 0
	}
	return int(p.ExplicitMaxNumSeqs()), p.SpeculativeTokens()
}
