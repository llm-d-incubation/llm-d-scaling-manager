package whatif

import "fmt"

// The decode loop gain (issue llm-d-incubation/llm-d-scaling-manager#517).
//
// A TPOT priced at a rate reads the line at the fill that rate holds, and
// that fill holds the line's own ITL: by Little's law a replica carrying
// lam requests a second holds k = lam*(H + S*ITL(k))*KVreq/C, so with
// ITL = A*k + B
//
//	k = c*(H/S + B) / (1 - c*A),  c = lam*S*KVreq/C.
//
// g = c*A is the loop gain: the share of a step the fill term feeds back.
// A relative error e in the slope A moves the fill by about e*g/(1-g), and
// the TPOT by as much (with no HeldBefore, ITL = B/(1-g)): the closer g is
// to 1, the more the line's slope error is amplified. g reaches 1 only
// past the knee. A small intercept B makes g large well before it: the
// TPOT is then mostly the fill term, which is the slope's.
//
// The guard asks what a slope LoopGainSlopeError steeper does to the TPOT,
// repriced through the same closed form (so HeldBefore, a burst and a batch
// term are priced as the figure is). Where it moves the TPOT by
// LoopGainMaxMove or more, or saturates the replicas, the slope alone can
// put the figure out of the within-20% band [0.8, 1.25] the planner's
// accuracy is read against, and the figure is flagged DecodeLoopGain. With
// no HeldBefore, no burst and no batch term that is g >= 0.8 exactly:
// (1 - g)/(1 - 1.05*g) >= 1.25.

// LoopGainSlopeError is the slope error the guard prices: 5%, the share by
// which a figure moves before the planner calls it moved (TierUnstable).
const LoopGainSlopeError = 0.05

// LoopGainMaxMove is the most a TPOT may move under LoopGainSlopeError
// before it is flagged: the within-20% band's upper edge, 1/0.8.
const LoopGainMaxMove = 1.25

// DecodeLoopGain is a decode TPOT whose fill amplifies its line's slope:
// a slope LoopGainSlopeError steeper moves it by LoopGainMaxMove or more, or
// saturates the replicas (LoopGain). The figure is priced all the same.
const DecodeLoopGain Flag = "DecodeLoopGain"

// LoopGain is a decode TPOT's loop gain at the rate it is priced at.
type LoopGain struct {
	// Gain is g at the figure's rate: lam*S*KVreq/C times the line's slope
	// (the batch term's too) at the fill a request sees.
	Gain float64
	// KneeGain is g at the knee's rate: the most any rate under the knee
	// reaches. It is the line's, not the rate's: KneeGain under 0.8 is a
	// line no rate flags.
	KneeGain float64
	// Move is the TPOT a slope LoopGainSlopeError steeper prices, over the
	// TPOT: 0 where that slope saturates the replicas (Saturates).
	Move float64
	// Saturates says the steeper slope carries no fill at the rate: at or
	// past its knee.
	Saturates bool
}

// High says the TPOT is flagged DecodeLoopGain.
func (g LoopGain) High() bool {
	return g.Saturates || !(g.Move < LoopGainMaxMove)
}

// String is the loop gain in words.
func (g LoopGain) String() string {
	move := "saturates the replicas"
	if !g.Saturates {
		move = fmt.Sprintf("moves the TPOT %.2fx", g.Move)
	}
	return fmt.Sprintf("loop gain %.2f (%.2f at the knee): a slope %.0f%% steeper %s", g.Gain, g.KneeGain, 100*LoopGainSlopeError, move)
}

// Kappa is the condition number of the gain: g/(1-g).
func (g LoopGain) Kappa() (float64, bool) { return Kappa(g.Gain) }

// The decode loop's condition number (llm-d-incubation/llm-d-scaling-manager#536).
//
// With no burst or batch term, ln k = ln(c*(H/S + B)) - ln(1 - c*A), with H
// the HeldBefore and S the steps, so
//
//	kappa = d ln k / d ln A = g/(1-g):
//
// a relative slope error e moves the fill by about kappa*e. The TPOT the
// line reads there, A*k + B, moves by A*k/(A*k+B) * (1+kappa) times e: with
// no HeldBefore that is kappa again (the TPOT is B/(1-g)), and with one it
// is more, under kappa+1. With a batch term g holds the term's slope too
// (slopeAt), so kappa is the elasticity to both slopes together. The
// inverse questions are not amplified so: d ln(rate) / d ln A at a fixed
// fill is -g, at most 1 in size. So kappa is reported on the forward
// figures only, the knee and the fill, rate and TPOTs at a rate, and
// changes none of them.

// KappaNoteAbove is the condition number past which the planner's text
// says so: 4, the kappa at g = 0.8, where a slope LoopGainSlopeError
// steeper moves the TPOT by LoopGainMaxMove (DecodeLoopGain's rule with no
// HeldBefore, burst or batch term).
const KappaNoteAbove = 4.0

// Kappa is the decode loop's condition number at loop gain g, g/(1-g).
// ok is false for a gain not in [0, 1): no fill carries a rate there.
func Kappa(g float64) (float64, bool) {
	if !(g >= 0 && g < 1) {
		return 0, false
	}
	k := g / (1 - g)
	return k, Finite(k)
}

// kappaAt is Kappa at rate, requests a second over the fleet: nil where it
// is not defined. At the knee's rate it is the most any rate under the
// knee reaches.
func (d Decode) kappaAt(rate float64) *float64 {
	k, ok := Kappa(d.gainAt(rate))
	if !ok {
		return nil
	}
	return &k
}

// LoopGainAt is the loop gain of d's mean TPOT at rate, requests a second
// over the fleet. ok is false where d does not price that TPOT: a refused
// Decode, or a rate at or past the knee.
func LoopGainAt(d Decode, rate float64) (LoopGain, bool) {
	if d.check() != nil {
		return LoopGain{}, false
	}
	tpot, ok := d.meanTPOTAt(rate)
	if !ok || !(tpot > 0) {
		return LoopGain{}, false
	}
	g := LoopGain{Gain: d.gainAt(rate), KneeGain: d.gainAt(d.kneeRate())}
	steep := d
	steep.Line.A *= 1 + LoopGainSlopeError
	if t, ok := steep.meanTPOTAt(rate); ok {
		g.Move = t / tpot
	} else {
		g.Saturates = true
	}
	return g, true
}

// meanTPOTAt is the mean TPOT SLOAtReplicas prices at rate.
func (d Decode) meanTPOTAt(rate float64) (float64, bool) {
	res := d.fillAtRate(rate)
	a, ok := res.(Answer)
	if !ok {
		return 0, false
	}
	return d.tpotAt(a.Value, d.kneeRate(), one(), nil).Value, true
}

// gainAt is g at rate: lam_rep*S*KVreq/C times dITL/dk at the fill a
// request sees there. It is read from the rate alone, so it is defined at
// the knee too, where no fill carries the rate.
func (d Decode) gainAt(rate float64) float64 {
	lam := rate / float64(d.Replicas)
	return lam * d.steps() * d.KVPerRequest / d.CapacityTokens * d.slopeAt(rate)
}

// slopeAt is dITL/dk where the closed form prices rate: the line's slope,
// and with a batch term the term's slope too, read across one sequence's
// KV at the fill rate holds (the knee's where no fill carries it).
func (d Decode) slopeAt(rate float64) float64 {
	if d.Batch.IsZero() {
		return d.Line.A
	}
	k := d.full().fill
	if kr, ok := d.fillAtReplicaRate(rate / float64(d.Replicas)); ok {
		k = d.seenFill(kr)
	}
	h := d.KVPerRequest / d.CapacityTokens
	return (d.itlAt(k+h) - d.itlAt(k)) / h
}
