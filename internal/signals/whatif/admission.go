package whatif

import (
	"fmt"
	"math"
	"slices"
)

// Admission is decode admission on one replica: a birth-death chain on the
// requests the replica holds, with the line read at each state's own fill
// (docs/proposals/slo-capacity-planner-ttft.md, "Decode admission").
//
// A replica holding n requests completes them at
//
//	mu_n = m / (OL * ITL(m*KVreq/C)),   m = min(n, c)
//
// with c the replica's slots. Above c the chain is geometric, so a request
// that finds every slot taken is admitted after exactly Exp(mu_c - lambda_rep),
// and the wait probability is the chain's mass at n >= c (arrivals see the
// time average: Poisson arrivals). Residence is treated as exponential.
type Admission struct {
	// WaitProbability is the share of requests that wait for a slot.
	WaitProbability float64
	// Decay is mu_c - lambda_rep, per second: the wait of a request that
	// finds every slot taken is exponential at this rate.
	Decay float64
	// Slots is c: the replica cap, or the whole requests the band's top holds
	// where that is fewer (flagged KVBoundSlots).
	Slots int
	// Basis carries the mean KV fill the chain spends its time at (Fill), the
	// knee it saturates at (KneeRate), what set the slots (BoundBy) and the
	// ITL line's tier (Tier).
	Basis Basis
	Flags []Flag
}

func (Admission) result() {}

// HasFlag reports whether the admission carries f.
func (a Admission) HasFlag(f Flag) bool {
	return slices.Contains(a.Flags, f)
}

// MeanWait is the mean admission wait over all requests, in seconds.
func (a Admission) MeanWait() float64 {
	if a.WaitProbability == 0 {
		return 0
	}
	return a.WaitProbability / a.Decay
}

// WaitExceeds is P(W > t): the share of requests that wait longer than t
// seconds.
func (a Admission) WaitExceeds(t float64) float64 {
	if t < 0 {
		return 1
	}
	return a.WaitProbability * math.Exp(-a.Decay*t)
}

// WaitQuantile is the q-th percentile of the admission wait, in seconds: 0
// when fewer than 1-q of requests wait at all. NaN for a q outside (0, 1).
func (a Admission) WaitQuantile(q float64) float64 {
	if !(q > 0 && q < 1) {
		return math.NaN()
	}
	if a.WaitProbability <= 1-q {
		return 0
	}
	return math.Log(a.WaitProbability/(1-q)) / a.Decay
}

// maxSlots bounds the admission chain, which holds one state per slot. A
// million sequences on one replica is three orders of magnitude above any
// engine's batch cap; a cap or capacity read that large is an error in the
// reading, refused (TooManySlots) before anything is allocated.
const maxSlots = 1_000_000

// DecodeAdmission prices decode admission at rate requests per second over the
// fleet, shared evenly across the replicas. It saturates exactly where the
// knee does (KneeRate): both are chainMu at the slots, so at or above the knee
// the chain has no stationary state (Saturated).
//
// The slots are the replica's saturation point (the same as the knee's): the
// replica cap, or the whole requests the band's top holds where that is fewer,
// an approximation the answer flags (KVBoundSlots): KV-bound admission is not
// a fixed number of slots.
//
// The line is read at each state's own fill, clamped to the band the line
// was fitted over: below it at the band's bottom (the higher, safer ITL, as
// ReachableFill reads it), and the slots never fill above its top. A fill
// below the band moves only the few least-loaded states, so it is not
// flagged; the knee's own read below the band is (Admission.Basis carries the
// knee, and the flags its BelowFittedBand).
func DecodeAdmission(d Decode, rate float64) Result {
	if u := d.check(); u != nil {
		return *u
	}
	if u := checkRate(rate); u != nil {
		return *u
	}
	p := d.full()
	if p.slots > maxSlots {
		return refuse(TooManySlots, fmt.Sprintf("%g slots on one replica, above the %d the chain holds", p.slots, maxSlots))
	}
	// Before the chain allocates: past the knee there is nothing to solve.
	knee := d.kneeRate()
	if rate >= knee {
		return refuse(Saturated, fmt.Sprintf("%g r/s at or above the knee %g r/s", rate, knee))
	}
	c := int(p.slots)
	ch, ok := birthDeath(rate/float64(d.Replicas), func(m int) float64 { return d.chainMu(float64(m)) }, c)
	if !ok {
		return refuse(Saturated, fmt.Sprintf("%g r/s per replica: the chain has no stationary state", rate/float64(d.Replicas)))
	}
	flags := p.flags()
	if p.by == BoundBandTop {
		flags = append(flags, KVBoundSlots)
	}
	return Admission{
		WaitProbability: ch.wait, Decay: ch.decay, Slots: c, Flags: flags,
		Basis: Basis{Fill: ch.seqs * d.KVPerRequest / d.CapacityTokens, KneeRate: knee, BoundBy: []Bound{p.by}, Tier: d.Tier},
	}
}

// chain is a solved birth-death chain: the wait probability, the decay of the
// wait above the slots, and the mean of min(n, c), in sequences.
type chain struct{ wait, decay, seqs float64 }

// birthDeath solves the chain with arrival rate lam, completion rate mu(m)
// with m requests in service for m in 1..c, and mu(c) above c. ok is false when
// lam >= mu(c), a rate is unusable, or c is outside [1, maxSlots], the bound
// that keeps the one state per slot it allocates finite.
//
// The stationary weights are products of lam/mu(m), which overflow long before
// c reaches a large engine's batch cap, so they are summed in logs.
func birthDeath(lam float64, mu func(m int) float64, c int) (chain, bool) {
	if c < 1 || c > maxSlots || !Finite(lam) || lam < 0 {
		return chain{}, false
	}
	muC := mu(c)
	if !Finite(muC) || !(muC > lam) {
		return chain{}, false
	}
	if lam == 0 {
		return chain{decay: muC}, true
	}
	logW := make([]float64, c+1)
	for m := 1; m <= c; m++ {
		mm := mu(m)
		if !Finite(mm) || !(mm > 0) {
			return chain{}, false
		}
		logW[m] = logW[m-1] + math.Log(lam/mm)
	}
	rho := lam / muC
	// The tail n >= c is geometric: w_c * rho^j, j >= 0, summing to
	// w_c / (1 - rho); its mean of min(n, c) is c.
	logTail := logW[c] - math.Log1p(-rho)
	hi := logTail
	for _, w := range logW[:c] {
		hi = max(hi, w)
	}
	var z, seqs float64
	for m, w := range logW[:c] {
		e := math.Exp(w - hi)
		z += e
		seqs += float64(m) * e
	}
	tail := math.Exp(logTail - hi)
	z += tail
	seqs += float64(c) * tail
	return chain{wait: tail / z, decay: muC - lam, seqs: seqs / z}, true
}
