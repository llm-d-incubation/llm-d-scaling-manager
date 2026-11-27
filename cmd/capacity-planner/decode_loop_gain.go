package main

import (
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// --decode-loop-gain reports, on every decode TPOT priced at a rate, how much
// the fill it is priced at amplifies an error in the ITL line's slope (issue
// llm-d-incubation/llm-d-scaling-manager#517; whatif.LoopGainAt).
//
// The fill a rate holds feeds the line's ITL back through Little's law, with
// gain g = lam*S*KVreq/C*A, and a relative slope error e moves the TPOT by
// about e*g/(1-g). An engine whose intercept B is small reaches a high g well
// under its knee: its TPOT there is mostly the slope's. On, each TPOT (the
// mean and the percentile) carries its loop gain, the gain at the knee and
// the TPOT a slope 5% steeper prices; one that slope moves 1.25x or more, or
// saturates, is flagged whatif.DecodeLoopGain, which no envelope row allows,
// so it, and it alone, reads OUTSIDE VALIDATED REGION. No figure's value
// moves. Off by default.

// decodeLoopGainFlag is the flag's name.
const decodeLoopGainFlag = "decode-loop-gain"

// decodeLoopGainUsage is the flag's help, on every command that takes it.
const decodeLoopGainUsage = "opt-in: report each decode TPOT's loop gain, how much the KV fill its rate holds amplifies an error in the ITL " +
	"line's slope (g = rate x output steps x KV per request / KV capacity x slope), and flag DecodeLoopGain, OUTSIDE VALIDATED REGION, " +
	"a TPOT that a slope 5% steeper moves 1.25x or more (g >= 0.8 with no KV pull) or saturates. No figure moves. Off by default"

// withLoopGain is r, a decode TPOT priced at rate, with its loop gain, and
// flagged whatif.DecodeLoopGain where it is high; r as it is with the switch
// off, on a refusal, or where the closed form prices no TPOT at rate.
func (pv pricedVariant) withLoopGain(r whatif.Result, rate float64) whatif.Result {
	a, ok := r.(whatif.Answer)
	if !pv.loopGain || !ok {
		return r
	}
	g, ok := whatif.LoopGainAt(pv.decode, rate)
	if !ok {
		return r
	}
	a.Basis.LoopGain = &g
	if g.High() && !slices.Contains(a.Flags, whatif.DecodeLoopGain) {
		a.Flags = append(slices.Clone(a.Flags), whatif.DecodeLoopGain)
	}
	return a
}
