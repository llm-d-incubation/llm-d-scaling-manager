package saturation

import (
	"math"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// SwitchK2BatchAtITLTarget is the replay switch for the TPOT objective's
// bound on a decode replica's k2 (issue
// llm-d-incubation/llm-d-scaling-manager#258).
//
// k2 is how many tokens a replica holds when it is busy, and nothing in it
// prices latency: the derived k2 (P3) holds a rank at the batch its token
// budget sustains in steady state, N_steady (steadyBatch), and a measured k2
// (P1, P2) at the fill a replica was seen to queue at. The more a decode
// replica holds, the slower each token. With a decode's budget charged only
// the prompt it computes (SwitchK2DecodeBudgetAtComputedPrompt), N_steady is
// large: on a GPU run one decode replica carried the batch three had, and
// TPOT p90 rose from 12 to 15-16 ms.
//
// On, where the policy sets a TPOT objective (slo.tpot, the target the SLO
// analyzer prices; config.SLOPolicy) and lists the SLO analyzer, a decode
// replica's k2, whichever priority priced it, is held at the KV its variant
// holds where its own decode ITL line reaches the target: a batch of
// min(N_steady, N_itl) on the derived k2. The ITL line is in KV fill,
// ITL(k) = A x k + B, per token (SwitchITLPerToken, which reads the step
// time over the tokens a step emits at the measured speculative
// acceptance). The fill it reaches the target at, k_itl, held to the ITL
// band, holds a rank at k_itl x C / KVreq requests, C the rank's KV; so the
// bound on k2 is k_itl x the replica's KV, and the k2-itl-bound line carries
// N_itl a rank at the context, I + D x O/2. The line's k is the pod's mean
// fill, so the bound holds the mean ITL at the target; the busiest rank's
// requests run above it. With no TPOT objective there is no bound: the
// band's pricing point is a fill, not a latency, and nothing measured put
// the latency cost there.
//
// The bound applies only on a decode variant's line it learned itself
// (k2ITLDistrust): not one borrowed from a sibling, priced one-sided at a
// prior, fitted above the band, or pinned at the default intercept
// (itl.DefaultBaselineSec) rather than a B the key measured or read through
// its floor below the band. A prefill or aggregated variant is not bound. A
// line in step time (SwitchITLPerToken off on a speculating engine) is not
// set against a per-token target, and a target at or under the line's
// intercept is met at no fill. In each of these cases k2 is the chain's, as
// before, and the k2-itl-bound line says why. A slope held from the key's
// own earlier fit does bound it (k2ITLDistrust).
//
// It only lowers k2, after the chain and its lower bound: the figure priced
// and the one the capacity store keeps are both held, so a parked variant's
// zero-replica estimate and a sibling's lookup inherit it. A measured window
// is not changed; it is held when read.
//
// On from the constructor: a policy that states a TPOT objective is not
// sized to pack decode past it. A replay turns it off (SetSwitchesForReplay).
const SwitchK2BatchAtITLTarget = "k2-batch-held-at-itl-target"

// Which bound held k2 (k2BatchBound on the k2-itl-bound line): the ITL
// target, or none, the chain's figure standing.
const (
	k2BoundNone = domain.DecodePackingBoundNone
	k2BoundITL  = domain.DecodePackingBoundITL
)

// Where the ITL target k2 is held at came from (itlBatchBound.source).
const (
	// itlTargetSLO is the policy's TPOT objective.
	itlTargetSLO = "slo-tpot"
)

// Why k2 is not held at an ITL target (itlBatchBound.why), with the switch
// on.
const (
	itlBoundNoTarget  = "no-tpot-target"
	itlBoundNoLine    = "no-line"
	itlBoundBorrowed  = "borrowed-line"
	itlBoundOneSided  = "one-sided-line"
	itlBoundAboveBand = "above-band-line"
	itlBoundDefaultB  = "default-intercept"
	itlBoundPerStep   = "per-step-line"
	itlBoundUnreach   = "target-unreachable"
	itlBoundBadBand   = "bad-band"
	itlBoundNotDecode = "not-decode"
)

// itlBoundNotBinding is why a bound that applies held nothing: the chain's
// k2 was already at or under the KV the bound allows. It is published with
// bound none (decodePackingOf), and is not an inert bound: the bound is in
// force, and would hold k2 if the chain's figure rose past it.
const itlBoundNotBinding = domain.DecodePackingNotBinding

// itlBoundNoKV is why a bound that applies held nothing because the replica
// reported no KV capacity to hold k2 at: inert, not k2-under-bound.
const itlBoundNoKV = "no-kv-capacity"

// Where the fill the line reaches the target at was held to the ITL band
// (itlBatchBound.clamp): the line was not fitted past it.
const (
	// itlClampLow is a fill under the band's lower edge: held at the edge.
	itlClampLow = "below-band"
	// itlClampHigh is a fill over the band's upper edge: held at the edge.
	itlClampHigh = "above-band"
)

// itlBatchBound is the KV fill at which a replica's ITL line reaches its
// target, the bound on its k2 (SwitchK2BatchAtITLTarget). The zero value is
// the switch off. With it on, fill is 0 where there is no bound, and why
// says why.
type itlBatchBound struct {
	// on is SwitchK2BatchAtITLTarget.
	on bool
	// fill is the KV fill the line reaches the target at, held to the ITL
	// band.
	fill float64
	// target is the latency target, seconds per token.
	target float64
	// source is itlTargetSLO.
	source string
	// clamp says the fill was held to the band's edge (itlClampLow,
	// itlClampHigh), and is empty where it was not.
	clamp string
	// why is empty with a bound, and otherwise why there is none.
	why string
}

// applies reports whether b bounds k2.
func (b itlBatchBound) applies() bool {
	return b.on && b.why == "" && b.fill > 0 && b.fill <= 1
}

// k2ITLDistrust says why the line r is not one k2 may be held at
// (SwitchK2BatchAtITLTarget), or "" when it is: a line the key fitted, with
// an intercept it measured or read through its floor. A slope held from the
// key's own earlier fit, or a full fit resting inside the tier latch, is the
// key's own measurement and bounds k2: dropping the bound while the bound
// itself keeps the fleet at a low fill, where a line is often held, would
// flap. A B read through the floor may err high and the slope low; the line
// then reads ITL low past its readings and high before them, so a target
// above the readings' ITL gives a looser bound and one below it a tighter.
func k2ITLDistrust(r itl.Reading) string {
	switch {
	case r.Model.IsZero() || r.Tier == "":
		return itlBoundNoLine
	case r.PinnedAtBPrior || r.PinnedAtLeastITL:
		return itlBoundOneSided
	case r.AboveBandFit:
		return itlBoundAboveBand
	case r.Tier == itl.TierPinnedB && !r.BaselineLearned && !r.PinnedAtFloor:
		return itlBoundDefaultB
	}
	return ""
}

// k2ITLBound is the bound on k2 for variant this cycle
// (SwitchK2BatchAtITLTarget): the fill at which its line reaches the TPOT
// objective, and none with no objective set.
func (c *cycle) k2ITLBound(variant string) itlBatchBound {
	if !c.a.k2AtITLTarget {
		return itlBatchBound{}
	}
	tpot := sloTPOTTarget(c.cfg)
	if !(tpot > 0) || math.IsInf(tpot, 0) {
		return itlBatchBound{on: true, why: itlBoundNoTarget}
	}
	if domain.CanonicalRole(c.rolesByVariant[variant]) != domain.RoleDecode {
		return itlBatchBound{on: true, why: itlBoundNotDecode}
	}
	why, ok := c.itlDistrust[variant]
	if !ok {
		why = itlBoundNoLine
	}
	if why != "" {
		return itlBatchBound{on: true, why: why}
	}
	params := engineParamsFor(c.a, c.input.Namespace, c.input.ModelID, variant)
	if params.SpeculatesPerStep() && !c.a.itlPerToken {
		return itlBatchBound{on: true, why: itlBoundPerStep}
	}
	lo, hi := c.cfg.ITLBand()
	return itlBoundFor(c.itlModels[variant], tpot, lo, hi)
}

// sloTPOTTarget is the policy's TPOT objective in seconds, or 0 with none.
// Both halves are needed, as for the TTFT pricing (config.PricesTTFT): the
// target, and the SLO analyzer that reads it listed. A target with the
// analyzer unlisted is read by nothing.
func sloTPOTTarget(cfg *config.ScalingPolicy) float64 {
	if cfg == nil || cfg.SLO == nil || cfg.SLO.TPOT == nil || !cfg.SLOAnalyzerListed() {
		return 0
	}
	return cfg.SLO.TPOT.Target.Seconds()
}

// itlBoundFor is the bound line m gives at a TPOT target of tpot seconds,
// with the switch on. No target (0) is no bound. A target at or under the
// line's intercept B is met at no fill: more replicas would not meet it, so
// it is no bound either (target-unreachable), where holding it at the band
// would ask for 1/lo times the replicas. A target the line reaches inside
// (0, lo) is held at lo, the band's lower edge, where the line was fitted;
// one past hi at hi. Between the intercept and hi a tighter target never
// asks for fewer replicas than a looser one, and at most 1/lo times those a
// full cache would need.
func itlBoundFor(m itl.Model, tpot, lo, hi float64) itlBatchBound {
	if !(tpot > 0) || math.IsInf(tpot, 0) {
		return itlBatchBound{on: true, why: itlBoundNoTarget}
	}
	if !itl.ValidModel(m.A, m.B) {
		return itlBatchBound{on: true, why: itlBoundNoLine}
	}
	if !(lo > 0) || !(hi >= lo) || hi > 1 {
		return itlBatchBound{on: true, why: itlBoundBadBand}
	}
	b := itlBatchBound{on: true, fill: (tpot - m.B) / m.A, target: tpot, source: itlTargetSLO}
	switch {
	case !(b.fill > 0):
		return itlBatchBound{on: true, target: tpot, source: itlTargetSLO, why: itlBoundUnreach}
	case b.fill < lo:
		b.fill, b.clamp = lo, itlClampLow
	case b.fill > hi:
		b.fill, b.clamp = hi, itlClampHigh
	}
	return b
}

// itlBoundedK2 is k2 held at the KV the replica holds at the bound's fill,
// kvTokens of it in all, and whether the bound held it there.
func itlBoundedK2(k2 int64, b itlBatchBound, kvTokens int64) (int64, bool) {
	if !b.applies() || kvTokens <= 0 {
		return k2, false
	}
	if at := int64(b.fill * float64(kvTokens)); at > 0 && at < k2 {
		return at, true
	}
	return k2, false
}

// decodePackingOf is what the bound made of a decode replica's k2, as its
// capacity row carries it (capacity.ReplicaCapacity.PackingBound and
// PackingReason): the bound that held k2 (k2BoundITL or k2BoundNone) and
// why. ok is false wherever the k2-itl-bound line is not logged either --
// the switch off, no TPOT objective, a prefill or aggregated replica -- so
// nothing is published for a variant the bound can never apply to.
//
// The reason is the target's source (itlTargetSLO) where the bound held k2,
// itlBoundNotBinding where it applies and the chain's k2 was already under
// it, and otherwise why there is no bound (itlBatchBound.why). Every value is
// one of those constants, so a label carrying it is bounded.
func decodePackingOf(b itlBatchBound, held bool) (bound, reason string, ok bool) {
	if !b.on || b.why == itlBoundNoTarget || b.why == itlBoundNotDecode {
		return "", "", false
	}
	switch {
	case held:
		return k2BoundITL, b.source, true
	case b.applies():
		return k2BoundNone, itlBoundNotBinding, true
	case b.why != "":
		return k2BoundNone, b.why, true
	}
	// A bound with no reason that does not apply: a fill out of (0, 1]
	// itlBoundFor never returns. Named, not left empty.
	return k2BoundNone, itlBoundBadBand, true
}

// logITLBound logs, with SwitchK2BatchAtITLTarget on and a TPOT objective,
// what the bound made of a decode replica's k2: the chain's figure (k2Unbounded, from priority) and the
// one priced, which bound held it (k2BatchBound), the target, the fill and
// any clamp to the band, why there was no bound where there was none, and
// the batch a rank under each: nSteady, the token budget's (steadyBatch),
// and nITL, the bound's fill of the rank's KV over I + D x O/2.
func logITLBound(logger logr.Logger, modelID, namespace, variant string, priority capacity.K2Source,
	params *capacity.EngineParams, batchCap pricedBatchCap, avgInput, avgOutput, dispersion float64,
	ranks int, kvTokens, unbounded, k2 int64, held bool) {
	b := batchCap.itl
	if !b.on || b.why == itlBoundNoTarget || b.why == itlBoundNotDecode {
		// No line for a model with no TPOT objective, nor for a prefill or
		// aggregated replica: the bound never applies there.
		return
	}
	bound := k2BoundNone
	if held {
		bound = k2BoundITL
	}
	kv := []any{"modelID", modelID, "namespace", namespace, "variant", variant,
		"priority", priority.String(), "k2Unbounded", unbounded, "k2", k2, "k2BatchBound", bound,
		"itlBoundWhy", b.why, "nSteady", steadyBatchAt(params, batchCap.perRank, avgInput, avgOutput, batchCap.budget)}
	if b.applies() {
		if !(dispersion >= 1) || math.IsInf(dispersion, 0) {
			dispersion = 1
		}
		nITL := 0.0
		if kvReq := avgInput + dispersion*avgOutput/2; kvReq > 0 {
			nITL = b.fill * float64(kvTokens) / float64(max(ranks, 1)) / kvReq
		}
		kv = append(kv, "nITL", nITL, "itlTargetSource", b.source, "itlTargetFill", b.fill,
			"itlTargetSec", b.target, "itlTargetClamp", b.clamp)
	}
	logger.V(perReplicaLogLevel).Info("k2-itl-bound", kv...)
}

// steadyBatchAt is the requests one engine rank holds in steady state at
// batch cap batchCap under params, its token budget spent as budget says
// (steadyBatch): the cap where params state no budget, 0 where the derived
// k2 has nothing to price (estimateCapacityAtCapBudget).
func steadyBatchAt(params *capacity.EngineParams, batchCap int64, avgInput, avgOutput float64, budget stepBudget) float64 {
	if batchCap <= 0 || avgOutput <= 0 || (params != nil && params.EffectiveMaxBatchedTokens <= 0) {
		return 0
	}
	if params == nil {
		return float64(batchCap)
	}
	return steadyBatch(float64(params.EffectiveMaxBatchedTokens), float64(batchCap), avgInput, avgOutput, budget)
}

// DecodePackingInertReasons is every reason a TPOT target can bound nothing
// for a decode variant: domain.DecodePacking.Reason with bound none, other
// than domain.DecodePackingNotBinding. The engine's Warning says what each
// means (DecodePackingBoundInert).
func DecodePackingInertReasons() []string {
	return []string{itlBoundNoLine, itlBoundBorrowed, itlBoundOneSided, itlBoundAboveBand, itlBoundDefaultB,
		itlBoundPerStep, itlBoundUnreach, itlBoundBadBand, itlBoundNoKV}
}

// noteDecodePacking keeps what a TPOT objective made of each decode
// variant's k2 on this analysis of the model, replacing the last, for the
// engine to take (TakeDecodePacking). An empty map is kept too: it says the
// model was analyzed and no variant is bound.
func (a *SaturationAnalyzer) noteDecodePacking(namespace, modelID string, packing map[string]domain.DecodePacking) {
	a.packingMu.Lock()
	defer a.packingMu.Unlock()
	if a.packing == nil {
		a.packing = map[string]map[string]domain.DecodePacking{}
	}
	a.packing[namespace+"/"+modelID] = packing
}

// TakeDecodePacking returns what a TPOT objective made of each decode
// variant's k2 on the model's last analysis, keyed by variant, and forgets
// it. ok is false when the model was not analyzed since the last take, so
// the caller keeps what it published rather than reading the model's
// variants as unbound.
func (a *SaturationAnalyzer) TakeDecodePacking(namespace, modelID string) (map[string]domain.DecodePacking, bool) {
	a.packingMu.Lock()
	defer a.packingMu.Unlock()
	key := namespace + "/" + modelID
	p, ok := a.packing[key]
	delete(a.packing, key)
	return p, ok
}
