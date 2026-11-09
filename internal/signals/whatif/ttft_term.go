package whatif

import (
	"fmt"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// MaterialPrefixHitShare is the share of the whole prompt a prefix cache must
// spare for DecodeTermPrefixHits: D is fitted on the whole prompt, which the KV
// pull moves, and priced at E[I_eff]. Stated, not measured: no committed run has
// prefix hits.
const MaterialPrefixHitShare = 0.1

// PromptLine is a term in the prompt, Base + PerToken*I, in seconds. With
// HeldPromptTokens the prompts it was read at could not separate the two, so
// Base stands for the whole term at that prompt and is priced as
// DecodeTTFT.at is: held for a shorter prompt and scaled up for a longer one.
// Both read high.
type PromptLine struct {
	Base, PerToken, HeldPromptTokens float64
}

// split is the line at a prompt of i tokens, as a fixed part and a part per
// token: the second goes in the request's own latency, as T_xfer does, so it
// follows the prompt's quantile.
func (l PromptLine) split(i float64) (fixed, perToken float64) {
	if l.HeldPromptTokens > 0 {
		if i > l.HeldPromptTokens {
			return 0, l.Base / l.HeldPromptTokens
		}
		return l.Base, 0
	}
	return l.Base, l.PerToken
}

// DecodeTerm is decode's part of a TTFT on a decode line
// (docs/proposals/slo-capacity-planner-ttft.md, "As built: the decode term"),
// in place of T_xfer + ITL(k):
//
//	D(I, ITL) + R = Engine(I) + PerStep*max(ITL(k) - FromITL, 0) + Hop(I)
//
// ITL(k) is the line at the fill the rate runs at, with no clamp to the band.
// No published figure falls under the old sum's own, on any path (the
// headline, the floor, Independent and Pooled): where one would, the old sum
// stands, and on the headline or the floor it is flagged DecodeTermFloored.
// TermFromEstimate builds the term from what the variant learned, flagged when
// assumed.
type DecodeTerm struct {
	// Engine is D at no step time, and PerStep its seconds of TTFT per second
	// of ITL. FromITL is 0 on a fit. On a measured D held at the ITL it was
	// measured at, FromITL is that ITL: only a step time above it adds.
	Engine           PromptLine
	PerStep, FromITL float64
	// Hop is R, the router's mean less the engines'.
	Hop PromptLine
	// StepAssumed is a D with no fit: Engine is the decode engine's TTFT on
	// quiet rows and PerStep AssumedDecodeStepSlope. EngineOmitted is no D at
	// all: the old sum stands for it.
	StepAssumed, EngineOmitted bool
	// HopAssumed is an R not learned: AssumedRouterHopSeconds, or the
	// operator's.
	HopAssumed bool
	// EngineRefused and HopRefused are why no fit stands; "" with one.
	EngineRefused, HopRefused prefill.Refusal
	// WholePromptTokens is the decode rows' mean whole prompt: well above the
	// E[I_eff] priced, D reads low (DecodeTermPrefixHits). 0 is unread.
	WholePromptTokens float64
}

// TermFromEstimate is the term a decode variant supports. D is the fit; with
// none, the decode engine's TTFT on the window's quiet rows, held, climbing from
// the ITL it was read at by AssumedDecodeStepSlope, flagged
// DecodeStepSlopeAssumed; with no quiet row, omitted, flagged DecodeTermOmitted.
// Busy rows are never held: their TTFT holds the slot wait the model prices
// apart as W_decode, and a load the question may not ask about. R is the fit, or
// else AssumedRouterHopSeconds, flagged RouterHopAssumed. An unknown input takes
// the value that asks for more, and is flagged, as the callers' other inputs do
// (slo.ttftFleet); each refusal travels with the term, for the caller to name.
func TermFromEstimate(est prefill.DecodeTermEstimate) DecodeTerm {
	t := DecodeTerm{WholePromptTokens: est.WholePromptTokens}
	switch f, h := est.Decode, est.Held; {
	case f.Refused == "" && f.PerStep > 0:
		t.Engine = PromptLine{Base: f.Base, PerToken: f.PerToken, HeldPromptTokens: f.HeldPromptTokens}
		t.PerStep = f.PerStep
	case h.Seconds > 0 && h.PromptTokens >= MinDecodeTTFTPrompt:
		t.Engine = PromptLine{Base: h.Seconds, HeldPromptTokens: h.PromptTokens}
		t.PerStep, t.FromITL, t.StepAssumed = AssumedDecodeStepSlope, h.ITLSeconds, true
		t.EngineRefused = refusalOf(f.Refused, prefill.TooFewDecodeMinutes)
	default:
		t.EngineOmitted = true
		t.EngineRefused = refusalOf(f.Refused, prefill.TooFewDecodeMinutes)
	}
	if h := est.Hop; h.Refused == "" && h.Windows > 0 {
		t.Hop = PromptLine{Base: h.Base, PerToken: h.PerToken, HeldPromptTokens: h.HeldPromptTokens}
	} else {
		t.Hop, t.HopAssumed = PromptLine{Base: AssumedRouterHopSeconds}, true
		t.HopRefused = refusalOf(h.Refused, prefill.NoRouterMean)
	}
	return t
}

// refusalOf is r, or fallback where no window gave one.
func refusalOf(r, fallback prefill.Refusal) prefill.Refusal {
	if r == "" {
		return fallback
	}
	return r
}

// check refuses a term with a figure no reading gives.
func (t DecodeTerm) check() *Unanswerable {
	for _, x := range []float64{t.Engine.Base, t.Engine.PerToken, t.Engine.HeldPromptTokens, t.Hop.Base, t.Hop.PerToken,
		t.Hop.HeldPromptTokens, t.PerStep, t.FromITL, t.WholePromptTokens} {
		if !Finite(x) {
			u := refuse(InvalidInput, fmt.Sprintf("decode term %+v", t))
			return &u
		}
	}
	if t.PerStep < 0 || t.FromITL < 0 || t.Engine.PerToken < 0 || t.Hop.PerToken < 0 ||
		t.Engine.HeldPromptTokens < 0 || t.Hop.HeldPromptTokens < 0 || t.WholePromptTokens < 0 {
		u := refuse(InvalidInput, fmt.Sprintf("decode term %+v", t))
		return &u
	}
	return nil
}

// withoutAssumed is the term with every assumed part taken out: an assumed hop
// to none, an assumed D to the old sum. False when nothing in it is assumed.
func (t DecodeTerm) withoutAssumed() (DecodeTerm, bool) {
	if !t.HopAssumed && !t.StepAssumed {
		return t, false
	}
	if t.HopAssumed {
		t.Hop, t.HopAssumed = PromptLine{}, false
	}
	if t.StepAssumed {
		t.Engine, t.PerStep, t.FromITL = PromptLine{}, 0, 0
		t.StepAssumed, t.EngineOmitted = false, true
	}
	return t, true
}

// WithoutAssumedTerms is the fleet with the decode term's assumed parts taken
// out, and whether it has any: what a caller compares an answer against to say
// that an assumed term alone decided it.
func WithoutAssumedTerms(fl TTFTFleet) (TTFTFleet, bool) {
	if fl.DecodeTerm == nil {
		return fl, false
	}
	t, ok := fl.DecodeTerm.withoutAssumed()
	fl.DecodeTerm = &t
	return fl, ok
}

// termPlan is a checked term at the question's prompt.
type termPlan struct {
	// engine and hop are the fixed parts, seconds.
	engine, hop float64
	perStep     float64
	fromITL     float64
	// omitted is no D at all: the step is the old sum's own first step.
	omitted bool
	// oldTransfer is the T_xfer per token of the old sum, which floors the
	// priced figure.
	oldTransfer float64
}

// termOn fits the term to the plan's prompt, moves the per-token parts to the
// own latency and raises the term's flags.
func termOn(t DecodeTerm, pf *prefillPlan) *termPlan {
	i := pf.p.PromptTokens
	engine, engineToken := t.EngineAt(i, pf.pull)
	hop, hopToken := t.Hop.split(i)
	tp := &termPlan{engine: engine, hop: hop, perStep: t.PerStep, fromITL: t.FromITL, omitted: t.EngineOmitted, oldTransfer: pf.xfer}
	if t.EngineOmitted {
		pf.xfer += hopToken
	} else {
		// With the pull its own stage, it is priced there and in the own
		// time beside D net of it.
		pf.xfer = engineToken + hopToken + pf.pull
		pf.flags = slices.DeleteFunc(pf.flags, func(f Flag) bool { return f == TransferOmitted })
	}
	pf.setVP(pf.vp)
	switch {
	case t.EngineOmitted:
		pf.flags = append(pf.flags, DecodeTermOmitted)
	case t.StepAssumed:
		pf.flags = append(pf.flags, DecodeStepSlopeAssumed)
	}
	if t.HopAssumed {
		pf.flags = append(pf.flags, RouterHopAssumed)
	}
	if !t.EngineOmitted && i < (1-MaterialPrefixHitShare)*t.WholePromptTokens {
		pf.flags = append(pf.flags, DecodeTermPrefixHits)
	}
	return tp
}

// EngineAt is D's fixed and per-token parts as priced at a prompt of i
// tokens, with the KV pull priced as its own stage at pull seconds a token
// (TTFTFleet.ProcessorSharing with a decode line; 0 otherwise): the fit read
// at i, net of the pull (netOfPull) unless D is omitted.
func (t DecodeTerm) EngineAt(i, pull float64) (fixed, perToken float64) {
	fixed, perToken = t.Engine.split(i)
	if !t.EngineOmitted && pull > 0 {
		fixed, perToken = netOfPull(fixed, perToken, pull, i)
	}
	return fixed, perToken
}

// netOfPull is D's fixed and per-token parts at a prompt of i tokens with the
// KV pull, pull seconds a token, taken out: D is the decode engine's TTFT, the
// pull inside it, fitted on windows whose pull was measured as it ran, so with
// the pull priced as its own processor-sharing stage (TTFTFleet.ProcessorSharing)
// D keeps only the rest. The pull comes out of the per-token part, and what
// that part cannot hold out of the fixed one (a held D has no per-token part);
// the step term is not touched.
func netOfPull(fixed, perToken, pull, i float64) (float64, float64) {
	if perToken >= pull {
		return fixed, perToken - pull
	}
	return fixed - (pull-perToken)*i, 0
}

// at is the term's D and R at step time itl, the line at the actual fill.
// oldStep is the line at the clamped fill, which an omitted D prices.
func (t termPlan) at(itl, oldStep float64) (engine, hop float64) {
	engine = t.engine + t.perStep*max(itl-t.fromITL, 0)
	if t.omitted {
		engine = oldStep
	}
	return max(engine, 0), max(t.hop, 0)
}

// floorFlags are the flags that say what a floor rests on, the decode term and
// the prompt's own prefill: a refusal keeps them. Nothing that sets only a wait
// is here: a floor has none.
var floorFlags = []Flag{DecodeTermOmitted, DecodeStepSlopeAssumed, RouterHopAssumed, DecodeTermFloored, DecodeTermPrefixHits,
	AssumedVP, VPLowerBound, VPAboveSlopeBound, VPScaledToPrompt, VPShapeMoved, AssumedPromptSpread}

// belowFloor is the refusal of a target at or under the floor, with the floor's
// flags. Where the term's assumed parts alone put the floor over the target, it
// says so (Unanswerable.AssumedTermDecides): that is no reading of the fleet.
func (pl ttftPlan) belowFloor(fl TTFTFleet, t TTFTTarget, detail string, raised []Flag) Unanswerable {
	u := refuse(TargetBelowFloor, detail)
	for _, f := range floorFlags {
		if slices.Contains(pl.flags, f) || slices.Contains(raised, f) {
			u.Flags = append(u.Flags, f)
		}
	}
	plain, ok := WithoutAssumedTerms(fl)
	if !ok {
		return u
	}
	p, bad := plain.plan()
	if bad != nil {
		return u
	}
	zero := p.price(0, t.Percentile, true, false)
	if zero.u == nil && zero.out.Floor < t.Seconds {
		u.AssumedTermDecides = true
		u.Detail += fmt.Sprintf("; an assumed term decides this: without it the floor is %.4g s", zero.out.Floor)
	}
	return u
}
