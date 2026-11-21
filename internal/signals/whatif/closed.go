package whatif

import (
	"fmt"
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// Decode is one decode role as the planner prices it: Replicas identical
// replicas, each running the fitted line over its band, sharing arrivals
// evenly (docs/proposals/slo-capacity-planner.md, "Decode: TPOT from the ITL
// line").
//
// A replica at fill k holds n(k) = min(k*C/KVreq, ReplicaCap) sequences, each
// emitting a token every ITL(k), so its request rate is
// lambda_rep(k) = n(k) / (ITL(k) * OL).
type Decode struct {
	// Line is the variant's fitted ITL line, ITL(k) = A*k + B in seconds.
	Line itl.Model
	// Band is the fill range the line is fitted over; prices are clamped to
	// it. It must meet FillBand's precondition.
	Band FillBand
	// CapacityTokens is C, the replica's whole KV capacity in tokens
	// (CapacityTokens).
	CapacityTokens float64
	// KVPerRequest is KVreq, the KV tokens one in-flight request holds on
	// average (shape.Shape.KVreq).
	KVPerRequest float64
	// OutputTokens is OL, the mean output length a request rate is divided
	// by.
	OutputTokens float64
	// ReplicaCap is the most sequences one replica runs: one engine's batch
	// cap times the data-parallel ranks the replica runs.
	ReplicaCap int64
	// Replicas is N_d, the decode replicas sharing the arrivals.
	Replicas int
	// Tier is where Line came from (TierOLS, TierPinnedB, TierHeld,
	// TierBorrowed), carried into every Basis; "" when the caller does not
	// know it. A pinned-B or held line on the default intercept is refused
	// (PinnedDefaultB).
	Tier string
	// BelowBand says how the line is read below the band; the zero value
	// reads it at the band's bottom, as the controller prices.
	BelowBand BelowBand
	// AssumedB says the caller stated Line's intercept B and refitted the
	// slope through it (the capacity planner's --assume-itl-b): a line on the
	// default intercept is then priced, not refused (PinnedDefaultB), and the
	// caller flags every figure as assumed. The controller never sets it.
	AssumedB bool
}

// BelowBand is how a Decode reads its line at a fill under the band's bottom.
type BelowBand struct {
	// Read reads the line at the fill itself, down to the floor LowestFill
	// sets, for a line fitted on the variant's own readings
	// (MeasuredIntercept) with a floor read below the band. The planner sets
	// it; the controller does not.
	Read bool
	// LeastITL is the floor below the band, in seconds: a low percentile of
	// the mean ITLs the variant's replicas read at a fill under the band's
	// bottom, readings the line's fit drops as idle. The line is never read
	// below it. 0 when there were too few such readings to trust one.
	LeastITL float64
}

// check refuses a Decode the planner cannot price, naming the first input
// that fails. The order is the order an operator fixes them in: the question
// first, then the line, the capacity, the cap and the shape.
func (d Decode) check() *Unanswerable {
	var u Unanswerable
	switch {
	// The band is the policy's, so an invalid one is a configuration error;
	// everything below it is read from the cluster, so a bad value is data.
	case !d.Band.valid():
		u = refuse(InvalidInput, fmt.Sprintf("band [%g, %g], want 0 < lo < hi < 1", d.Band.Lo, d.Band.Hi))
	case d.Replicas < 1:
		u = refuse(NoDecodeReplicas, fmt.Sprintf("decode replicas %d", d.Replicas))
	// B must be positive as well as A: with B <= 0 the request rate does not
	// rise with fill, so no fill answers a rate and the line describes no
	// engine (every step has a fixed cost).
	case !itl.ValidModel(d.Line.A, d.Line.B) || !Finite(d.Line.B) || !(d.Line.B > 0):
		u = refuse(NoLine, fmt.Sprintf("line A=%g B=%g", d.Line.A, d.Line.B))
	case d.onDefaultB():
		u = refuse(PinnedDefaultB, fmt.Sprintf("a %s line on the default intercept %g s", d.Tier, itl.DefaultBaselineSec))
	case !Finite(d.CapacityTokens) || !(d.CapacityTokens > 0):
		u = refuse(NoCapacity, fmt.Sprintf("KV capacity %g tokens", d.CapacityTokens))
	case d.ReplicaCap <= 0:
		u = refuse(NoBatchCap, fmt.Sprintf("replica cap %d", d.ReplicaCap))
	case !Finite(d.OutputTokens) || !(d.OutputTokens > 0) || !Finite(d.KVPerRequest) || !(d.KVPerRequest > 0):
		u = refuse(NoTraffic, fmt.Sprintf("output %g tokens, KV per request %g tokens", d.OutputTokens, d.KVPerRequest))
	// The band's top must hold one whole request: a replica that cannot
	// serves none, and the closed form would price a fraction of one.
	case math.Floor(d.Band.Hi*d.CapacityTokens/d.KVPerRequest) < 1:
		u = refuse(NoCapacity, fmt.Sprintf("KV capacity %g tokens holds no whole request of %g at the band top %g",
			d.CapacityTokens, d.KVPerRequest, d.Band.Hi))
	default:
		return nil
	}
	return &u
}

// defaultBTolerance is how near itl.DefaultBaselineSec, relative to it, an
// intercept reads as that default rather than a measurement: a pinned fit
// holds B exactly, but a line that travelled through a snapshot or a borrow
// may not hold every bit of it.
const defaultBTolerance = 1e-9

// onDefaultB reports a line whose slope was fitted against the default
// intercept rather than a measured one: a pinned-B, held or above-band fit
// pins B, and a borrowed line is its donor's, so one of those on the
// default B rests on no measured floor (PinnedDefaultB). An ols line fits B
// itself and is never refused for where it lands. A B the caller stated
// (AssumedB) is not the default's.
func (d Decode) onDefaultB() bool {
	return !d.AssumedB && OnDefaultB(d.Tier, d.Line)
}

// OnDefaultB reports a line of tier on the default intercept, the line a
// Decode refuses as PinnedDefaultB (Decode.onDefaultB).
func OnDefaultB(tier string, line itl.Model) bool {
	switch tier {
	case TierPinnedB, TierHeld, TierBorrowed:
		return math.Abs(line.B-itl.DefaultBaselineSec) <= defaultBTolerance*itl.DefaultBaselineSec
	}
	return false
}

// MeasuredIntercept reports a line whose intercept B is not a guess: an ols
// fit's own B; a pinned-B or held line's B, which a full fit stores (the
// tracker's baseline) or the floor below the band sets, unless it is the
// default, itl.DefaultBaselineSec, which no fit measured; or an above-band line's B, the least ITL read above
// the band, which is an upper bound on the engine's floor by construction,
// so reading below the band from it errs high, never low. A borrowed line is
// another variant's, and a line of unknown tier has no known provenance:
// neither is read below the band.
func MeasuredIntercept(tier string, line itl.Model) bool {
	switch tier {
	case TierOLS:
		return true
	case TierPinnedB, TierHeld, TierAboveBand:
		return math.Abs(line.B-itl.DefaultBaselineSec) > defaultBTolerance*itl.DefaultBaselineSec
	}
	return false
}

// LowestFill is the lowest fill the line is read at, below band, when the
// reading below the band is asked for (BelowBand.Read):
//
//   - band.Lo, the band's bottom, for an intercept that is a guess
//     (MeasuredIntercept), or when no floor was read below the band
//     (leastITL 0): with no evidence there, the line is not extrapolated;
//   - otherwise the fill at which the line reaches leastITL, the floor read
//     below the band, clamped to [0, band.Lo]: the line is read at the fill
//     itself, never below what the engines were seen to do there.
//
// A fill below the band is flagged BelowFittedBand either way. Measured on
// the Qwen3-32B P/D staircases at fills of 0.02 to 0.06, a held line read at
// the fill came within -5% to +1% of the mean TPOT the clients saw; read at
// the band's bottom, 0.15, it was 29% to 46% high.
func LowestFill(band FillBand, tier string, line itl.Model, leastITL float64) float64 {
	if !MeasuredIntercept(tier, line) || !(line.A > 0) || !(leastITL > 0) || !Finite(leastITL) {
		return band.Lo
	}
	return min(max((leastITL-line.B)/line.A, 0), band.Lo)
}

// lowestFill is the lowest fill d reads its line at: LowestFill when the
// reading below the band is asked for, the band's bottom otherwise.
func (d Decode) lowestFill() float64 {
	if !d.BelowBand.Read {
		return d.Band.Lo
	}
	return LowestFill(d.Band, d.Tier, d.Line, d.BelowBand.LeastITL)
}

// fullPoint is where a replica saturates: the sequences it runs there, the
// fill those occupy, and the fill the line is read at.
type fullPoint struct {
	// slots is c, a whole number of sequences: the replica cap, or the
	// sequences the band's top holds, floor(band.Hi*C/KVreq), when that is
	// fewer. Kept as a float: a cap read as MaxInt64 is a number here, and
	// only the admission chain needs it as a count (maxSlots bounds that).
	slots float64
	// slotFill is the fill the slots occupy, c*KVreq/C: never above band.Hi.
	slotFill float64
	// fill is k_full, the fill the line is read at: slotFill, raised to
	// lowestFill when the slots fill less than that (a line without a
	// measured intercept is read no lower than the band's bottom).
	fill float64
	// by is what set the slots.
	by Bound
	// belowBand is slotFill < band.Lo: the knee sits below the band the line
	// was fitted over.
	belowBand bool
}

// full is the replica's saturation point. The knee and the admission chain
// both saturate at it, so the two never disagree about where a rate stops
// being carried.
func (d Decode) full() fullPoint {
	p := fullPoint{slots: math.Floor(d.Band.Hi * d.CapacityTokens / d.KVPerRequest), by: BoundBandTop}
	if cp := float64(d.ReplicaCap); cp <= p.slots {
		p.slots, p.by = cp, BoundBatchCap
	}
	p.slotFill = p.slots * d.KVPerRequest / d.CapacityTokens
	p.fill = min(p.slotFill, d.Band.Hi) // a rounding above Hi reads Hi, as chainMu does
	if p.slotFill < d.Band.Lo {
		p.fill, p.belowBand = max(p.slotFill, d.lowestFill()), true
	}
	return p
}

// flags is BelowFittedBand when the knee sits below the band.
func (p fullPoint) flags() []Flag {
	if p.belowBand {
		return []Flag{BelowFittedBand}
	}
	return nil
}

// replicaRate is lambda_rep(k), one replica's request rate at fill k: the
// sequences resident there, capped at the slots (CappedSequences), each
// advancing a token every ITL(k), over the output length.
func (d Decode) replicaRate(k float64) float64 {
	seqs := CappedSequences(k, d.CapacityTokens, d.KVPerRequest, d.full().slots)
	itlSec := d.Line.ITLAt(k)
	if !(seqs > 0) || !(itlSec > 0) {
		return 0
	}
	return seqs / (itlSec * d.OutputTokens)
}

// chainMu is the rate a replica holding m sequences completes them:
// m / (OL * ITL(k_m)), with k_m = m*KVreq/C clamped to [lowestFill, band.Hi]
// (below the band only a measured intercept is read at the fill itself). The
// admission chain's state m completes at chainMu(min(m, c)), and the knee is
// N_d * chainMu(c): one formula, so the chain and the knee saturate at the
// same rate by construction.
func (d Decode) chainMu(m float64) float64 {
	k := min(max(m*d.KVPerRequest/d.CapacityTokens, d.lowestFill()), d.Band.Hi)
	return m / (d.OutputTokens * d.Line.ITLAt(k))
}

// kneeRate is the fleet's knee, N_d * chainMu(c): N_d * c / (OL * ITL(k_full)).
func (d Decode) kneeRate() float64 {
	return float64(d.Replicas) * d.chainMu(d.full().slots)
}

// fillAtReplicaRate is the closed-form fill one replica runs at while it
// carries lam requests a second (Little's law, residence OL*ITL(k)):
//
//	k(lam) = lam*OL*B / (C/KVreq - lam*OL*A)
//
// It is valid while the denominator is positive and the sequences are under
// the slots (the fill under slotFill); ok is false past that, where the
// replica is saturated.
func (d Decode) fillAtReplicaRate(lam float64) (k float64, ok bool) {
	den := d.CapacityTokens/d.KVPerRequest - lam*d.OutputTokens*d.Line.A
	if !(den > 0) {
		return 0, false
	}
	k = lam * d.OutputTokens * d.Line.B / den
	if !Finite(k) || k > d.full().slotFill {
		return 0, false
	}
	return k, true
}

// KneeRate is the rate, in requests per second over the fleet, at which the
// decode replicas saturate, N_d * c / (OL * ITL(k_full)) (question 2, step 1):
// the knee. Basis.KneeRate repeats it, Basis.Fill is k_full, Basis.BoundBy
// names whether the band's top or the batch cap set the slots, and
// BelowFittedBand flags a knee read at the band's bottom because the slots
// fill less than it.
func KneeRate(d Decode) Result {
	if u := d.check(); u != nil {
		return *u
	}
	return d.kneeAnswer()
}

func (d Decode) kneeAnswer() Answer {
	p := d.full()
	knee := d.kneeRate()
	return Answer{Value: knee, Flags: p.flags(), Basis: Basis{Fill: p.fill, KneeRate: knee, BoundBy: []Bound{p.by}, Tier: d.Tier}}
}

// checkRate refuses a rate that is not a finite, non-negative number of
// requests per second.
func checkRate(rate float64) *Unanswerable {
	if Finite(rate) && rate >= 0 {
		return nil
	}
	u := refuse(InvalidInput, fmt.Sprintf("rate %g, want a finite rate of at least 0", rate))
	return &u
}

// FillAtRate is the KV fill each decode replica runs at while the fleet
// carries rate requests a second, shared evenly: the closed form k(lambda_rep)
// with lambda_rep = rate / N_d. Saturated at or above the knee.
func FillAtRate(d Decode, rate float64) Result {
	if u := d.check(); u != nil {
		return *u
	}
	return d.fillAtRate(rate)
}

func (d Decode) fillAtRate(rate float64) Result {
	if u := checkRate(rate); u != nil {
		return *u
	}
	knee := d.kneeRate()
	if rate >= knee {
		return refuse(Saturated, fmt.Sprintf("rate %.4g r/s at or above the knee %.4g r/s", rate, knee))
	}
	k, ok := d.fillAtReplicaRate(rate / float64(d.Replicas))
	if !ok {
		return refuse(Saturated, fmt.Sprintf("no fill carries %.4g r/s", rate))
	}
	return Answer{Value: k, Basis: Basis{Fill: k, KneeRate: knee, Tier: d.Tier}}
}

// TPOTTarget is a TPOT service level: the mean, or a percentile over requests
// of each request's own TPOT, (e2e - TTFT)/(output tokens - 1).
type TPOTTarget struct {
	// Seconds is the target.
	Seconds float64
	// Percentile is q in (0, 1) for a per-request percentile, or 0 for the
	// mean.
	Percentile float64
	// OverTokenGaps asks for a percentile over individual token gaps (ITL)
	// rather than over requests. It is refused: nothing models the spread of
	// single steps (TokenGapPercentile).
	OverTokenGaps bool
	// Ratio is R_q, the q-th percentile of per-request TPOT over its mean.
	// Required for a percentile; ignored for the mean.
	Ratio TPOTRatio
}

// TPOTRatio is R_q: learned from a TPOT histogram (TPOTRatioFromWindows), or
// assumed by the caller. A zero Value is "no ratio".
type TPOTRatio struct {
	// Value is the global R_q, read from every window.
	Value float64
	// Assumed marks a ratio the caller supplied rather than learned; answers
	// priced with it carry AssumedTPOTRatio.
	Assumed bool
	// ByFill is R_q by KV-fill bucket, rising with the fill; nil prices Value
	// at every fill. A percentile is priced at the R_q of the fill it is read
	// at (At).
	ByFill []FillRatio
}

// one is the multiplier of the mean: R_q = 1 at every fill.
func one() TPOTRatio { return TPOTRatio{Value: 1} }

// ratio resolves the multiplier a target applies to the mean ITL: 1 for the
// mean, R_q for a percentile, with the flags it brings.
func (t TPOTTarget) ratio() (TPOTRatio, []Flag, *Unanswerable) {
	var u Unanswerable
	curve := t.Ratio.check()
	switch {
	case t.OverTokenGaps:
		u = refuse(TokenGapPercentile, "percentiles over token gaps are not modelled; use a per-request TPOT percentile")
	case t.Percentile == 0:
		return one(), nil, nil
	case !(t.Percentile > 0 && t.Percentile < 1):
		u = refuse(InvalidInput, fmt.Sprintf("TPOT percentile %g, want 0 < q < 1", t.Percentile))
	case t.Ratio.Value == 0:
		u = refuse(NoTPOTHistogram, fmt.Sprintf("no R_q for the p%g TPOT: no histogram and none assumed", 100*t.Percentile))
	case !Finite(t.Ratio.Value) || !(t.Ratio.Value >= 1):
		u = refuse(InvalidInput, fmt.Sprintf("R_q %g, want a finite ratio of at least 1", t.Ratio.Value))
	case curve != nil:
		u = *curve
	default:
		var flags []Flag
		if t.Ratio.Assumed {
			flags = append(flags, AssumedTPOTRatio)
		}
		return t.Ratio, flags, nil
	}
	return TPOTRatio{}, nil, &u
}

// tpotAtFill is ITL(k) times R_q at k, and whether the global R_q stood in
// for a thin fill bucket there.
func (d Decode) tpotAtFill(k float64, r TPOTRatio) (float64, bool) {
	rk, global := r.At(k)
	return d.Line.ITLAt(k) * rk, global
}

// fillForTPOT is the fill k* at which ITL(k) times R_q(k) reaches t. With
// one ratio it is the closed form (t/R_q - B)/A. With R_q by fill the product
// rises strictly in k (A and B are positive and R_q never falls), so k* is
// found by bisection on [lo, 1]; past a full cache R_q is flat at its last
// bucket, and the closed form at that ratio is exact there. R_q steps up at a
// bucket's edge, where the product can jump past t: the bisection returns the
// side below t, so the fill answered never prices a TPOT above the target.
// The flag says the global R_q stood in for a thin bucket at k*.
func (d Decode) fillForTPOT(t float64, r TPOTRatio, lo float64) (float64, bool) {
	if len(r.ByFill) == 0 {
		return (t/r.Value - d.Line.B) / d.Line.A, false
	}
	if at, _ := d.tpotAtFill(1, r); at < t {
		r1, global := r.At(1)
		return (t/r1 - d.Line.B) / d.Line.A, global
	}
	hi := 1.0
	for range 100 {
		mid := (lo + hi) / 2
		if mid == lo || mid == hi {
			break
		}
		if at, _ := d.tpotAtFill(mid, r); at < t {
			lo = mid
		} else {
			hi = mid
		}
	}
	_, global := r.At(lo)
	return lo, global
}

// RateQuery is question 1's input beyond the fleet.
type RateQuery struct {
	// Target is the TPOT service level.
	Target TPOTTarget
	// Headroom places the operating-point cap at Headroom times the knee: the
	// scaling policy's scaleUpThreshold, 0.85 by default. Valid in (0, 1]; 1
	// lifts the cap, and the knee alone bounds the rate. Anything else is
	// InvalidInput.
	Headroom float64
}

// RateForTPOT answers question 1 for decode: the largest rate, in requests per
// second over the fleet, at which the replicas hold the TPOT target
// (docs/proposals/slo-capacity-planner.md, "Question 1").
//
// It inverts ITL(k)*R_q(k) = t (fillForTPOT; k* = (t/R_q - B)/A with one R_q,
// and R_q = 1 for the mean), refuses with TargetBelowFloor when the floor,
// ITL(lowestFill)*R_q, already reaches t, clamps k* to k_full, and prices
// N_d * lambda_rep(k*). The rate is then capped at the operating point,
// headroom times the knee, because past it the admission wait is no longer
// small; a headroom of 1 lifts the cap. A final fill below the band is flagged
// BelowFittedBand.
//
// Basis.Uncapped is the rate the target alone allows, Basis.Fill the fill the
// answer runs at, and Basis.BoundBy what set the rate.
func RateForTPOT(d Decode, q RateQuery) Result {
	if u := d.check(); u != nil {
		return *u
	}
	t := q.Target
	if !Finite(t.Seconds) || !(t.Seconds > 0) {
		return refuse(InvalidInput, fmt.Sprintf("TPOT target %g s", t.Seconds))
	}
	r, flags, u := t.ratio()
	if u != nil {
		return *u
	}
	if !(q.Headroom > 0 && q.Headroom <= 1) {
		return refuse(InvalidInput, fmt.Sprintf("headroom %g, want 0 < headroom <= 1 for question 1", q.Headroom))
	}
	floor, _ := d.tpotAtFill(d.lowestFill(), r)
	if floor >= t.Seconds {
		return refuse(TargetBelowFloor, fmt.Sprintf("target %.4g ms at or below the floor %.4g ms", 1000*t.Seconds, 1000*floor))
	}
	p := d.full()
	k, global := d.fillForTPOT(t.Seconds, r, d.lowestFill())
	by := []Bound{BoundTPOT}
	// below is whether the answer's fill sits under the band: set once per
	// place k is set, and flagged once, at the end.
	below := k < d.Band.Lo
	if k > p.fill {
		k, by, below = p.fill, []Bound{p.by}, p.belowBand
	}
	knee := d.kneeRate()
	uncapped := float64(d.Replicas) * d.replicaRate(k)
	if !Finite(uncapped) || !(uncapped > 0) {
		return refuse(NoLine, fmt.Sprintf("the line prices no rate at fill %g", k))
	}
	rate := uncapped
	// A headroom of 1 is no cap: the knee bounds the rate, and the
	// operating point is not named.
	if op := q.Headroom * knee; q.Headroom < 1 && rate >= op {
		if rate > op {
			// The cap's own fill, from the closed form. Below the knee it
			// exists; if it did not, the answer would have no fill to stand
			// on, so it is refused rather than reported at the uncapped one.
			kop, ok := d.fillAtReplicaRate(op / float64(d.Replicas))
			if !ok {
				return refuse(Saturated, fmt.Sprintf("no fill carries the operating point %.4g r/s", op))
			}
			rate, k, by, below = op, kop, nil, kop < d.Band.Lo
		}
		by = append([]Bound{BoundOperatingPoint}, by...)
	}
	if below {
		flags = append(flags, BelowFittedBand)
	}
	// The thin bucket's stand-in matters only where the target sets the rate:
	// k* is then the answer's fill.
	if global && slices.Contains(by, BoundTPOT) {
		flags = append(flags, GlobalTPOTRatio)
	}
	return Answer{Value: rate, Flags: flags, Basis: Basis{
		Fill: k, KneeRate: knee, Floor: floor, Uncapped: uncapped, BoundBy: by, Tier: d.Tier,
	}}
}

// ReplicasQuery is question 2's input beyond the fleet: where to operate, and
// which TPOT percentile to report besides the mean. Set exactly one of
// Headroom and Rate; both set, or neither, is InvalidInput.
type ReplicasQuery struct {
	// Headroom places the operating point at Headroom times the knee: the
	// scaling policy's scaleUpThreshold, 0.85 by default. Valid in (0, 1):
	// unlike question 1, where 1 lifts the cap, question 2 refuses 1, because
	// the knee itself is a point no fill carries (ask KneeRate for it).
	Headroom float64
	// Rate is the operating rate in requests per second over the fleet, in
	// place of Headroom times the knee (--rate). Valid when finite and
	// positive; at or above the knee it is Saturated.
	Rate float64
	// PercentileTarget, when not nil, asks for the TPOT at that per-request
	// percentile as well as the mean (its Percentile, Ratio and
	// OverTokenGaps; Seconds is ignored). Nil asks for the mean only.
	PercentileTarget *TPOTTarget
}

// DecodeSLO is question 2's decode answer. Each field is its own Result, so a
// percentile refused for want of a histogram leaves the mean answered.
type DecodeSLO struct {
	// KneeRate is the saturation rate, requests per second over the fleet.
	KneeRate Result
	// Rate is the operating rate the TPOTs are priced at.
	Rate Result
	// MeanTPOT is the mean TPOT there, in seconds.
	MeanTPOT Result
	// PercentileTPOT is the per-request TPOT percentile there, in seconds.
	//
	// NIL WHEN NO PERCENTILE WAS ASKED (ReplicasQuery.PercentileTarget nil):
	// neither an answer nor a refusal. Check it before a type switch;
	// ExitCode skips it.
	PercentileTPOT Result
}

// SLOAtReplicas answers question 2 for decode: the knee, the operating rate
// below it, and the TPOT held there (docs/proposals/slo-capacity-planner.md,
// "Question 2"). The TPOT is the line at the closed-form fill, as the mean and,
// when asked, times R_q; it never beats the floor, ITL(lowestFill) (times
// R_q). A fill below the band is flagged BelowFittedBand and read at the fill
// itself for a measured intercept, at the band's bottom otherwise.
func SLOAtReplicas(d Decode, q ReplicasQuery) DecodeSLO {
	// every refuses each asked-for field with u.
	every := func(out DecodeSLO, u Unanswerable) DecodeSLO {
		out.Rate, out.MeanTPOT = u, u
		if q.PercentileTarget != nil {
			out.PercentileTPOT = u
		}
		return out
	}
	if u := d.check(); u != nil {
		return every(DecodeSLO{KneeRate: *u}, *u)
	}
	out := DecodeSLO{KneeRate: d.kneeAnswer()}
	knee := d.kneeRate()

	var rate float64
	var rateBy []Bound
	switch {
	case q.Rate != 0 && q.Headroom != 0:
		return every(out, refuse(InvalidInput, fmt.Sprintf("rate %g and headroom %g both set; set one", q.Rate, q.Headroom)))
	case q.Rate != 0:
		rate = q.Rate // fillAtRate refuses a negative or non-finite one
	// Question 2 operates below the knee: at a headroom of 1 the operating
	// point is the knee itself, where no fill carries the rate and the
	// admission wait is unbounded.
	case !(q.Headroom > 0 && q.Headroom < 1):
		return every(out, refuse(InvalidInput, fmt.Sprintf("headroom %g, want 0 < headroom < 1 for question 2", q.Headroom)))
	default:
		rate, rateBy = q.Headroom*knee, []Bound{BoundOperatingPoint}
	}
	res := d.fillAtRate(rate)
	if u, refused := res.(Unanswerable); refused {
		return every(out, u)
	}
	k := res.(Answer).Value
	out.Rate = Answer{Value: rate, Basis: Basis{Fill: k, KneeRate: knee, BoundBy: rateBy, Tier: d.Tier}}
	out.MeanTPOT = d.tpotAt(k, knee, one(), nil)
	if p := q.PercentileTarget; p != nil {
		if p.Percentile == 0 && !p.OverTokenGaps {
			// A percentile asked with no percentile is a caller's mistake,
			// not a request for the mean twice.
			out.PercentileTPOT = refuse(InvalidInput, "TPOT percentile 0 asked for")
			return out
		}
		r, flags, u := p.ratio()
		if u != nil {
			out.PercentileTPOT = *u
		} else {
			out.PercentileTPOT = d.tpotAt(k, knee, r, flags)
		}
	}
	return out
}

// tpotAt is the TPOT at fill k, times R_q at that fill, never below the floor.
func (d Decode) tpotAt(k, knee float64, r TPOTRatio, flags []Flag) Answer {
	lowest := d.lowestFill()
	floor, _ := d.tpotAtFill(lowest, r)
	at := max(k, lowest)
	if k < d.Band.Lo {
		flags = append(flags, BelowFittedBand)
	}
	v, global := d.tpotAtFill(at, r)
	if global {
		flags = append(flags, GlobalTPOTRatio)
	}
	return Answer{Value: v, Flags: flags, Basis: Basis{Fill: k, KneeRate: knee, Floor: floor, Tier: d.Tier}}
}
