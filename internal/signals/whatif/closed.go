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
	// HeldBefore is the seconds a request holds its KV on the decode replica
	// before its first output token: on a P/D fleet the KV pull and the
	// first step, the decode engine's own TTFT (issue
	// llm-d-incubation/llm-d-scaling-manager#301). Residence is then
	// HeldBefore + (OL - 1) * ITL(k), not OL * ITL(k): the TTFT ends at the
	// first output token, so that step is in it. At 8k prompts and 26 output
	// tokens the pull is about a third of the residence. 0, as the controller
	// prices, leaves it out. It must be a TTFT read with no decode backlog: a
	// request waiting for decode capacity holds no KV.
	//
	// Two simplifications. HeldBefore is the TTFT read at the recorded load
	// and held at every rate priced: the pull's own wait grows with the rate,
	// so above the recorded load the residence reads short (the knee high),
	// below it long. And the same residence times the batch slots, where a
	// request pulling its KV holds cache but not yet a running-batch slot
	// (vLLM's WAITING_FOR_REMOTE_KVS): a knee the slots bind (BoundBatchCap)
	// is under-priced, conservatively, by up to HeldBefore over the
	// residence.
	HeldBefore float64
	// ArrivalCV2 is Ca^2, the squared coefficient of variation of the fleet's
	// interarrival times, where the caller prices how clustered arrivals
	// load the fill a request runs at (issue
	// llm-d-incubation/llm-d-scaling-manager#291): 0 or 1 is Poisson, as the
	// controller prices, and below 1 prices as Poisson too (burstFill).
	ArrivalCV2 float64
	// AssumedB says the caller stated Line's intercept B and refitted the
	// slope through it (the capacity planner's --assume-itl-b): a line on the
	// default intercept is then priced, not refused (PinnedDefaultB), and the
	// caller flags every figure as assumed. The controller never sets it.
	AssumedB bool
	// Batch is the ITL the step adds past the roofline (BatchTerm, issue
	// llm-d-incubation/llm-d-scaling-manager#309), read on top of Line at
	// every fill (itlAt). The zero value prices Line alone. The capacity
	// planner sets it from a term it fitted; the controller never does.
	Batch BatchTerm
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
	// LeastSequences is the fewest sequences a replica ran in the readings
	// LeastITL was read from, and ByCount the floor kept by the sequences it
	// was read at, as the line is kept by fill: the line ITL = A n + B
	// through the floors of those readings grouped by their sequences n
	// (itl.Model, n in place of the fill). A replica running fewer sequences
	// steps faster, so LeastITL stands for LeastSequences and more; a TPOT
	// priced at fewer is held to the floor ByCount reads at its own count,
	// never above LeastITL, and flagged ExtrapolatedBelowFloor (issue
	// llm-d-incubation/llm-d-scaling-manager#286). LeastSequences 0 is
	// unknown, and LeastITL then stands for every count; a ByCount that is
	// not a valid line (itl.ValidModel) leaves LeastITL, the lowest-count
	// floor, flagged all the same. The flag is set only where the floor
	// holds the figure up: a TPOT the line reads above it is the line's.
	LeastSequences float64
	ByCount        itl.Model
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
	case !d.Batch.IsZero() && !d.Batch.usable(d.Line):
		u = refuse(InvalidInput, fmt.Sprintf("batch term %g s per token past %g tokens per rank, on a line with B=%g",
			d.Batch.Slope, d.Batch.Roof, d.Line.B))
	case d.onDefaultB():
		u = refuse(PinnedDefaultB, fmt.Sprintf("a %s line on the default intercept %g s", d.Tier, itl.DefaultBaselineSec))
	case !Finite(d.CapacityTokens) || !(d.CapacityTokens > 0):
		u = refuse(NoCapacity, fmt.Sprintf("KV capacity %g tokens", d.CapacityTokens))
	case d.ReplicaCap <= 0:
		u = refuse(NoBatchCap, fmt.Sprintf("replica cap %d", d.ReplicaCap))
	case !Finite(d.OutputTokens) || !(d.OutputTokens > 0) || !Finite(d.KVPerRequest) || !(d.KVPerRequest > 0):
		u = refuse(NoTraffic, fmt.Sprintf("output %g tokens, KV per request %g tokens", d.OutputTokens, d.KVPerRequest))
	case !Finite(d.HeldBefore) || d.HeldBefore < 0:
		u = refuse(InvalidInput, fmt.Sprintf("held before the first token %g s, want a finite value of at least 0", d.HeldBefore))
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
	if !readsFloor(tier, line, leastITL) {
		return band.Lo
	}
	return min(max((leastITL-line.B)/line.A, 0), band.Lo)
}

// readsFloor reports whether LowestFill reads line down to the floor
// leastITL, rather than stopping at the band's bottom.
func readsFloor(tier string, line itl.Model, leastITL float64) bool {
	return MeasuredIntercept(tier, line) && line.A > 0 && leastITL > 0 && Finite(leastITL)
}

// lowestFill is the lowest fill d reads its line at: LowestFill when the
// reading below the band is asked for, the band's bottom otherwise.
func (d Decode) lowestFill() float64 {
	if !d.BelowBand.Read {
		return d.Band.Lo
	}
	return LowestFill(d.Band, d.Tier, d.Line, d.BelowBand.LeastITL)
}

// lowestFillAt is the lowest fill d reads its line at for a TPOT at fill k:
// lowestFill where k holds at least the sequences the floor was read at
// (BelowBand.LeastSequences); under them, LowestFill at the floor kept by
// count (BelowBand.ByCount) at k's own sequences, never above LeastITL; a
// floor by count that reads no positive ITL there holds nothing up, and the
// line is read from its intercept, fill 0, as it is under any floor below B.
// extrapolated reports the second: the floor is read below every count it
// saw. A line whose intercept is a guess is never read below the band's
// bottom (LowestFill), so it is never extrapolated either.
func (d Decode) lowestFillAt(k float64) (fill float64, extrapolated bool) {
	least := d.BelowBand.LeastSequences
	if !d.BelowBand.Read || !(least > 0) || !Finite(least) || !readsFloor(d.Tier, d.Line, d.BelowBand.LeastITL) {
		return d.lowestFill(), false
	}
	n := CappedSequences(k, d.CapacityTokens, d.KVPerRequest, float64(d.ReplicaCap))
	if !(n < least) {
		return d.lowestFill(), false
	}
	floor := d.BelowBand.LeastITL
	if m := d.BelowBand.ByCount; itl.ValidModel(m.A, m.B) {
		f := m.A*n + m.B
		if !(f > 0) {
			return 0, true
		}
		floor = min(floor, f)
	}
	return LowestFill(d.Band, d.Tier, d.Line, floor), true
}

// readFill is the fill d reads its line at for a replica at fill k: k,
// raised to the floor at k's own count (lowestFillAt) where the line reads
// under it. Every reader of the floor goes through it, so the knee, the
// admission chain, the TPOT-target inversion, the TTFT's decode step and the
// TPOT itself read one floor (issue
// llm-d-incubation/llm-d-scaling-manager#485). It rises with k: the line and
// the floor by count both do (itl.ValidModel needs A > 0). extrapolated
// reports the floor read below every count it saw, at a fill the old
// lowest-count floor would have held up (lowestFill above k), the cases
// ExtrapolatedBelowFloor names.
func (d Decode) readFill(k float64) (fill float64, extrapolated bool) {
	lowest, ext := d.lowestFillAt(k)
	return max(k, lowest), ext && d.lowestFill() > k
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
	// the floor at the slots' own count (readFill) when the slots fill less
	// than that (a line without a measured intercept is read no lower than
	// the band's bottom).
	fill float64
	// by is what set the slots.
	by Bound
	// belowBand is slotFill < band.Lo: the knee sits below the band the line
	// was fitted over.
	belowBand bool
	// extrapolated is the knee read at the floor by count below every count
	// the floor saw (readFill).
	extrapolated bool
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
		p.fill, p.extrapolated = d.readFill(p.slotFill)
		p.belowBand = true
	}
	return p
}

// flags is BelowFittedBand when the knee sits below the band, and
// ExtrapolatedBelowFloor when it is read at the floor by count below every
// count the floor saw.
func (p fullPoint) flags() []Flag {
	var flags []Flag
	if p.belowBand {
		flags = append(flags, BelowFittedBand)
	}
	if p.extrapolated {
		flags = append(flags, ExtrapolatedBelowFloor)
	}
	return flags
}

// replicaRate is lambda_rep(k), one replica's request rate at fill k: the
// sequences resident there, capped at the slots (CappedSequences), each
// advancing a token every ITL(seenFill(k)), ITL(k) for Poisson arrivals,
// over the output length.
func (d Decode) replicaRate(k float64) float64 {
	seqs := CappedSequences(k, d.CapacityTokens, d.KVPerRequest, d.full().slots)
	itlSec := d.itlAt(d.seenFill(k))
	if !(seqs > 0) || !(itlSec > 0) {
		return 0
	}
	return seqs / d.residence(itlSec)
}

// burstFill is the fill of the sequences a request finds running beside it
// over what Poisson arrivals give it, from ArrivalCV2 (issue
// llm-d-incubation/llm-d-scaling-manager#291). Little's law gives the time
// average fill k; a request is counted where requests arrive, and clustered
// arrivals put more of them beside it. For a stationary arrival stream with
// interarrival squared coefficient of variation Ca^2, the arrivals expected
// beside a typical one, beyond the rate's share, number Ca^2 - 1 in all (the
// counts' variance over a long window is Ca^2 times their mean); each of N_d
// decode replicas, taking a share 1/N_d of them at random (Bernoulli
// splitting), holds (Ca^2 - 1)/N_d of them. They are taken as all overlapping the request's residence, which
// holds where the residence is long against the clusters (a residence of
// hundreds of steps): where it is not, this reads a request's fill high.
// The line was fitted on what the engines measured at their own arrivals
// (Poisson for every recording that states none), so only the excess over
// Poisson is priced, and a Ca^2 at or below 1 prices none: a smoother
// stream would read a lower TPOT, which no recording has shown.
func (d Decode) burstFill() float64 {
	if !Finite(d.ArrivalCV2) || !(d.ArrivalCV2 > 1) || d.Replicas < 1 {
		return 0
	}
	return (d.ArrivalCV2 - 1) / float64(d.Replicas) * d.KVPerRequest / d.CapacityTokens
}

// seenFill is the fill a request runs at where the time average is k: k and
// the burst's fill (burstFill), never past k_full, where the slots are
// full, nor under k.
func (d Decode) seenFill(k float64) float64 {
	e := d.burstFill()
	if e == 0 {
		return k
	}
	return max(k, min(k+e, d.full().fill))
}

// SeenFill is the fill a request runs at where the replica's time average is
// k (Basis.Fill): k itself for Poisson arrivals, higher where they cluster
// (ArrivalCV2 above 1). The line is read there, so a caller checking a fill
// against what was observed checks this one.
func (d Decode) SeenFill(k float64) float64 {
	return d.seenFill(k)
}

// residence is a request's seconds on the replica at step time itlSec:
// HeldBefore, then the steps after it (steps).
func (d Decode) residence(itlSec float64) float64 {
	return d.HeldBefore + d.steps()*itlSec
}

// steps is the output steps a request takes on the replica after
// HeldBefore: OL with none, OL - 1 with one, since the decode engine's TTFT
// that HeldBefore is read from ends at the first output token, so its step
// is already in it.
func (d Decode) steps() float64 {
	if d.HeldBefore > 0 {
		return max(d.OutputTokens-1, 0)
	}
	return d.OutputTokens
}

// chainMu is the rate a replica holding m sequences completes them:
// m / residence(ITL(k_m)), OL * ITL(k_m) with no HeldBefore, with k_m = m*KVreq/C raised to the floor at m's own
// count (readFill) and clamped to band.Hi
// (below the band only a measured intercept is read at the fill itself). The
// admission chain's state m completes at chainMu(min(m, c)), and the knee is
// N_d * chainMu(c): one formula, so the chain and the knee saturate at the
// same rate by construction.
func (d Decode) chainMu(m float64) float64 {
	k, _ := d.readFill(m * d.KVPerRequest / d.CapacityTokens)
	k = min(k, d.Band.Hi)
	return m / d.residence(d.itlAt(k))
}

// kneeRate is the fleet's knee, N_d * chainMu(c): N_d * c / residence(ITL(k_full)),
// N_d * c / (OL * ITL(k_full)) with no HeldBefore.
func (d Decode) kneeRate() float64 {
	return float64(d.Replicas) * d.chainMu(d.full().slots)
}

// fillAtReplicaRate is the closed-form fill one replica runs at while it
// carries lam requests a second (Little's law, residence H + S*ITL(k), H
// the HeldBefore, S its steps):
//
//	k(lam) = lam*(H + S*B) / (C/KVreq - lam*S*A)
//
// It is valid while the denominator is positive and the sequences are under
// the slots (the fill under slotFill); ok is false past that, where the
// replica is saturated. With clustered arrivals (burstFill e) a request's
// steps run at ITL(k + e), so its residence is H + S*ITL(k + e):
//
//	k(lam) = lam*(H + S*B + S*A*e) / (C/KVreq - lam*S*A)
//
// while k + e is under k_full, and lam*(H + S*ITL(k_full))*KVreq/C past it.
// With a batch term the rate has no closed form, and the fill is found by
// bisection (fillAtReplicaRateBisected), on the same residence.
func (d Decode) fillAtReplicaRate(lam float64) (k float64, ok bool) {
	if !d.Batch.IsZero() {
		return d.fillAtReplicaRateBisected(lam)
	}
	s := d.steps()
	den := d.CapacityTokens/d.KVPerRequest - lam*s*d.Line.A
	if !(den > 0) {
		return 0, false
	}
	// With no HeldBefore the sum is left out, so the controller's figure is
	// the same to the last bit as before the term; so too with no burst.
	e := d.burstFill()
	switch {
	case e > 0:
		k = lam * (d.HeldBefore + s*d.Line.B + s*d.Line.A*e) / den
		if kf := d.full().fill; k+e > kf {
			k = lam * d.residence(d.Line.ITLAt(kf)) * d.KVPerRequest / d.CapacityTokens
		}
	case d.HeldBefore > 0:
		k = lam * (d.HeldBefore + s*d.Line.B) / den
	default:
		k = lam * s * d.Line.B / den
	}
	if !Finite(k) || k > d.full().slotFill {
		return 0, false
	}
	return k, true
}

// KneeRate is the rate, in requests per second over the fleet, at which the
// decode replicas saturate, N_d * c / residence(ITL(k_full)) (question 2, step 1):
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
	return d.itlAt(k) * rk, global
}

// fillForTPOT is the fill k* at which ITL(k) times R_q(k) reaches t. With
// one ratio and no batch term it is the closed form (t/R_q - B)/A; with a
// batch term it is bisected (fillForTPOTBisected). With R_q by fill the product
// rises strictly in k (A and B are positive and R_q never falls), so k* is
// found by bisection on [lo, 1]; past a full cache R_q is flat at its last
// bucket, and the closed form at that ratio is exact there. R_q steps up at a
// bucket's edge, where the product can jump past t: the bisection returns the
// side below t, so the fill answered never prices a TPOT above the target.
// The flag says the global R_q stood in for a thin bucket at k*.
func (d Decode) fillForTPOT(t float64, r TPOTRatio, lo float64) (float64, bool) {
	if !d.Batch.IsZero() {
		return d.fillForTPOTBisected(t, r, lo)
	}
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

// tpotRead is the TPOT, times R_q, a request running at fill k prices: the
// line read at readFill(k), held up by the floor at k's own count.
func (d Decode) tpotRead(k float64, r TPOTRatio) float64 {
	f, _ := d.readFill(k)
	v, _ := d.tpotAtFill(f, r)
	return v
}

// fillForTPOTRead is the fill k* at which tpotRead reaches t, the floor read
// at each count searched. At and above lowestFill no floor by count holds
// the line up (lowestFillAt never reads above lowestFill), so a target the
// line meets there is fillForTPOT's, from lowestFill, as before. A target at
// or under the line at lowestFill is met, if at all, at fewer sequences:
// tpotRead rises with k, so k* is bisected on [0, lowestFill] and the side
// below t returned, as fillForTPOT does. Its one caller has refused a
// target at or under tpotRead(0).
func (d Decode) fillForTPOTRead(t float64, r TPOTRatio) (float64, bool) {
	lowest := d.lowestFill()
	if at, _ := d.tpotAtFill(lowest, r); at < t {
		return d.fillForTPOT(t, r, lowest)
	}
	lo, hi := 0.0, lowest
	for range 200 {
		mid := (lo + hi) / 2
		if mid == lo || mid == hi {
			break
		}
		if d.tpotRead(mid, r) < t {
			lo = mid
		} else {
			hi = mid
		}
	}
	f, _ := d.readFill(lo)
	_, global := r.At(f)
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
// and R_q = 1 for the mean), the line read through the floor at each count
// searched (fillForTPOTRead), refuses with TargetBelowFloor when the floor at
// the fewest sequences, tpotRead(0), already reaches t, clamps k* to k_full,
// and prices N_d * lambda_rep(k*). The rate is then capped at the operating
// point, headroom times the knee, because past it the admission wait is no
// longer small; a headroom of 1 lifts the cap. A final fill below the band is
// flagged BelowFittedBand, and one priced at the floor by count below every
// count the floor saw ExtrapolatedBelowFloor, as SLOAtReplicas's TPOT is.
//
// Basis.Uncapped is the rate the target alone allows, Basis.Fill the fill the
// answer runs at, Basis.Floor the floor at its own count, and Basis.BoundBy
// what set the rate.
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
	// The lowest TPOT any rate prices is the one at fill 0, read at the
	// floor of the fewest sequences (readFill): under the floor's counts the
	// floor by count sits below lowestFill's.
	if least := d.tpotRead(0, r); least >= t.Seconds {
		return refuse(TargetBelowFloor, fmt.Sprintf("target %.4g ms at or below the floor %.4g ms", 1000*t.Seconds, 1000*least))
	}
	p := d.full()
	k, global := d.fillForTPOTRead(t.Seconds, r)
	by := []Bound{BoundTPOT}
	e := d.burstFill()
	// Under a burst a target met at k_full itself is met at every fill a
	// request can see, so it is the knee's too (>=, not >): with none the
	// bound stays the target's at k_full, as before.
	clamped := k > p.fill || (e > 0 && k >= p.fill)
	if clamped {
		k, by = p.fill, []Bound{p.by}
	}
	// k is the fill a request runs at (seenFill); the replica's own is the
	// burst's fill (burstFill) under it. A target a request misses at the
	// burst alone has no rate.
	if e > 0 && !clamped {
		if k <= e {
			burstFloor := d.tpotRead(e, r)
			return refuse(TargetBelowFloor, fmt.Sprintf("target %.4g ms at or below the TPOT the arrivals' clustering alone prices, %.4g ms",
				1000*t.Seconds, 1000*burstFloor))
		}
		k -= e
	}
	// below is whether the line is read under the band: at the fill a
	// request sees (seenFill), not the replica's time average. Set once per
	// place k is set, and flagged once, at the end.
	below := d.seenFill(k) < d.Band.Lo
	if clamped {
		below = p.belowBand
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
	// The floor is the one at the answer's own count, as tpotAt reports it.
	seen := d.seenFill(k)
	lowest, _ := d.lowestFillAt(seen)
	floor, _ := d.tpotAtFill(lowest, r)
	if _, extrapolated := d.readFill(seen); extrapolated {
		flags = append(flags, ExtrapolatedBelowFloor)
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
// The line is read at the fill a request sees (seenFill); Basis.Fill is the
// replica's time-average k, and Basis.Floor the line's floor at the lowest
// fill, not the higher floor a burst puts under every request. A TPOT at
// fewer sequences than the floor was read at is held to the floor at its own
// count (lowestFillAt), and says so where the floor read at those counts
// would have held it up; one the line reads above that floor is the line's,
// as before.
func (d Decode) tpotAt(k, knee float64, r TPOTRatio, flags []Flag) Answer {
	seen := d.seenFill(k)
	lowest, extrapolated := d.lowestFillAt(seen)
	if extrapolated && d.lowestFill() > seen {
		flags = append(flags, ExtrapolatedBelowFloor)
	}
	floor, _ := d.tpotAtFill(lowest, r)
	at := max(seen, lowest)
	// Below the band where the line is read: the fill the request sees.
	if seen < d.Band.Lo {
		flags = append(flags, BelowFittedBand)
	}
	v, global := d.tpotAtFill(at, r)
	if global {
		flags = append(flags, GlobalTPOTRatio)
	}
	return Answer{Value: v, Flags: flags, Basis: Basis{Fill: k, KneeRate: knee, Floor: floor, Tier: d.Tier}}
}
