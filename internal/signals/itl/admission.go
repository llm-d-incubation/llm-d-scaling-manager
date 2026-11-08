package itl

import (
	"math"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// Admission is what decides, per Observe, which replica rows are readings:
// the KV band and the sequence-cap exception above it; and how long a key's
// last A may stand in for a fit when there are too few readings for one. The
// zero value is the defaults with no cap known, which admits nothing above the
// band, and holds an A without limit.
type Admission struct {
	// MinK and MaxK are the band's edges; zero takes DefaultMinObservableK
	// and DefaultMaxObservableK. The policy keys analyzerTuning.itlBand.minObservableK and
	// analyzerTuning.itlBand.maxObservableK set them (config.ScalingPolicy.ITLBand), which
	// validates 0 < MinK < MaxK < 1.
	MinK, MaxK float64
	// MaxNumSeqs is the engine's batch cap, per rank, as something states it:
	// its flags (max_num_seqs, lowered by --max-num-active-seqs when that is
	// set), or where they state none the llm-d.ai/max-num-seqs annotation
	// (capacity.EngineParams.StatedBatchCap). Zero when it is unknown or
	// only a default, which admits nothing above MaxK.
	MaxNumSeqs int64
	// MaxNumSeqsSource is where MaxNumSeqs came from, for the log only
	// (flags, declared, MaxNumSeqsSourceNone): nothing here reads it.
	MaxNumSeqsSource string
	// CapMaxPreemptionRate is the most preemptions per second a capped
	// replica may show over the ITL's window and still be read above MaxK.
	// The policy key analyzerTuning.itlBand.capMaxPreemptionRate sets it; zero, the default,
	// requires none.
	CapMaxPreemptionRate float64
	// Steady is the steady-state gate: which readings teach the full fit and
	// the pinned one. The policy's analyzerTuning.steadyGate sets it; a zero
	// field takes its default (DefaultSteadiness).
	Steady Steadiness
	// SlopeHoldFor is how long after it was fitted a key's last A may be
	// held when the window cannot fit (Tracker.Observe). The analyzers pass
	// their bucket retention, the horizon a finished fit is kept on
	// elsewhere; zero holds it without limit.
	SlopeHoldFor time.Duration
	// Args is the engine arguments the rows were produced under
	// (capacity.EngineParams.ArgsFingerprint), empty when unknown. An A
	// fitted under other known arguments is not held: it describes another
	// engine.
	Args string

	// Settle, when set, is the settle tracker a replica's settle is read
	// from (settle.go). Nil keeps the key's own trace of each replica, the
	// rule before the tracker.
	Settle *Settle
	// WaitsAfterRestart makes a replica already serving on the key's first
	// Observe wait its settle too: a controller restart is a settle, and the
	// line does not refit from it for Steadiness.For. Without it, as before,
	// such a replica is taken as settled.
	WaitsAfterRestart bool
	// KeepsThroughMissingRow keeps a replica's settle through a cycle it is
	// missing from, up to the absence tolerance: one missed scrape is not a
	// replica that left and came back. On the tracker the tracker's
	// tolerance rules; on the key's own trace, AbsenceTolerance. Without
	// it, as before, a replica missing from one cycle starts over.
	KeepsThroughMissingRow bool
	// AbsenceTolerance is how long a replica may go without a row and keep
	// its trace on the key's own trace (KeepsThroughMissingRow): the settle
	// tracker's tolerance at the controller's interval
	// (settle.Tolerance). At or below settle.Period, settle.Period.
	AbsenceTolerance time.Duration
	// RestartUnderName, on the tracker (Settle), starts a settle when a
	// replica's Ready time changes under the same Pod name
	// (settle.Restarted), whether or not a row that was not Ready came
	// between. Without it, as on the key's own trace, only the row that was
	// not Ready shows the restart.
	RestartUnderName bool
	// IdleOnEvidence, on the tracker, reads load returning after idleRows
	// idle rows from the tracker, idle on evidence (settle.Row.Activity: a
	// known KV of 0, and a known empty ITL window or a known generation
	// rate of 0); a scrape that read nothing is not idle. Without it, as on
	// the key's own trace, a row with neither an ITL nor a k is idle.
	IdleOnEvidence bool
	// FiniteOnly reads a row's ITL by its presence bits: a reading needs the
	// Finite state (domain.ReplicaMetrics.ITLState), so a Pod with an Absent
	// rank, whose AvgITL is its Finite ranks' mean, is no reading. Without
	// it, as before, any finite AvgITL above zero is.
	FiniteOnly bool
	// AboveBandFallback keeps the readings above the band that are not
	// admitted (no sequence cap), at a k of at least DefaultKSat, in a
	// window of their own, stamped by the same steady gate and settle as the
	// band's, and fits a line from them, B pinned at their least ITL, when
	// the band gives none and the key holds no slope it may hold
	// (Reading.AboveBandFit). It never prices under the true line at
	// DefaultKSat (fitAboveBand), and never sets the held slope or the
	// intercept. Without it, as before, such a reading is only counted.
	AboveBandFallback bool
	// OneSidedAboveKSat keeps the pinned fit from pricing under the true line
	// at DefaultKSat while the key has no learned B: when the window holds
	// readings at a k of at least DefaultKSat (a replica at its sequence
	// cap), the fit through DefaultBaselineSec is set against a line pinned
	// at the least ITL of those readings, and the higher at DefaultKSat is
	// the line (atLeastITLLine; Reading.PinnedAtLeastITL). A learned B ends
	// it: the pinned fit then pins that. Without it, as before, the fit
	// through DefaultBaselineSec is the line. The throughput analyzer sets
	// it with its cap admission, the saturation and slo analyzers under
	// their own switches (issue ev-shindin/llm-scaling-manager#467).
	OneSidedAboveKSat bool
	// OneSidedAt is the fill the one-sided rule compares the two lines at,
	// and the held A prices the chosen line's ITL at: the k the analyzer
	// prices its line at (the saturation line's pricing k). Zero is
	// DefaultKSat, the throughput line's. On readings at or past kSat alone
	// the two lines meet at the readings' mean k, at or past kSat, and under
	// it the one with the higher B is the higher, so every fill under kSat
	// makes the same choice, and the line through the least ITL is at or
	// above the truth there (fitAboveBand). The held A is what depends on
	// the fill: a held line, through the default B, prices what the chosen
	// one did there only.
	OneSidedAt float64
	// OneSidedTarget, above zero, prices the line at an ITL instead of a
	// fill: the slo line prices the fill in the band where its ITL meets the
	// TPOT target (whatif.PriceTPOT), and a lower fill is more replicas.
	// Each line is then judged at the fill where it meets the target,
	// clamped to the band (pricedFill), the line with the lower fill is the
	// line, and the held A is the one whose line meets the target at the
	// chosen line's fill. OneSidedAt is not read.
	OneSidedTarget float64
	// BPrior, above zero, is the stated upper prior on B the one-sided rule
	// pins instead of the least ITL (issue ev-shindin/llm-scaling-manager#467). The analyzers set BPrior's
	// value: DefaultBPrior, raised, never lowered, by B learned on the
	// variant's physics key. The line set against the fit through
	// DefaultBaselineSec has as intercept BPrior capped at the least ITL of
	// any reading in the window (priorCap), and passes through the mean point
	// (kbar, ITLbar) of the readings at k >= DefaultKSat: A = (ITLbar - b) /
	// kbar (priorLine). On readings of the true line, at a fill k
	// under kbar, it is over the truth by (b - B)(1 - k/kbar). It never prices
	// under the truth while the true B is at most the prior (the cap, which
	// no reading can put under B, keeps that), and it over-prices by the
	// prior's error, not by a line flattened to the cap's ITL. A true B above
	// the prior is under-priced by at most (B - b)(1 - k/kbar). Zero keeps
	// the line through the least ITL (atLeastITLLine), the throughput line's.
	// The saturation and slo analyzers set it (Reading.PinnedAtBPrior).
	BPrior float64
	// AboveBandWaitsAfterRestart stamps the above-band window's readings
	// with the restart's wait too (WaitsAfterRestart), as learner PR 4b-2
	// first built it, before its review exempted them (aboveBandSettling).
	// Only negative controls set it.
	AboveBandWaitsAfterRestart bool
	// TierHysteresis latches the key's tier: the full fit is taken at the
	// set values (DefaultMinSamples, DefaultMinKSpread), kept down to the
	// clear values (DefaultClearMinSamples, DefaultClearKSpread), and within
	// DefaultTierDwell of the last change the tier changes only for a line
	// that prices DefaultTierSafeMargin more (tier_latch.go). Without it, as
	// before, each cycle chooses alone.
	TierHysteresis bool
	// TierPrice is what a targeted line's price reads of the engine
	// (PricedRate). The slo analyzer sets it; a line priced at a fixed fill
	// has no need of it.
	TierPrice TierPrice
	// OLSLatched, on a refit only (Refit), is KeyReadings.OLSLatched.
	OLSLatched bool
}

// MaxNumSeqsSourceNone is Admission.MaxNumSeqsSource when no cap is stated:
// nothing above the band is admitted. The analyzers that log the source
// share this one value.
const MaxNumSeqsSourceNone = "none"

// PolicyReader is what an ITL line reads of a scaling policy: the KV band
// (analyzerTuning.itlBand), the steady-state gate (analyzerTuning.steadyGate)
// and the most preemption a capped replica may show
// (analyzerTuning.itlBand.capMaxPreemptionRate). *config.ScalingPolicy
// implements it, nil included, and asserts so at compile time: an analyzer
// that cannot import config reads the policy through it, and a renamed
// method fails the build instead of reading the defaults.
type PolicyReader interface {
	ITLBand() (lo, hi float64)
	ITLSteadiness() Steadiness
	ITLCapPreemptionRate() float64
}

// mayHold reports whether a key's last A may stand in for a fit at now: it was
// fitted within SlopeHoldFor, and not under other engine arguments. Arguments
// unknown on either side are not a change, as the saturation analyzer's own
// window reset (itlUnderArgs) treats them.
func (a Admission) mayHold(s heldSlope, now time.Time) bool {
	if a.SlopeHoldFor > 0 && now.Sub(s.fittedAt) > a.SlopeHoldFor {
		return false
	}
	return a.Args == "" || s.args == "" || a.Args == s.args
}

// targeted reports whether the one-sided rule prices at a target ITL
// (OneSidedTarget) rather than at a fill (OneSidedAt).
func (a Admission) targeted() bool {
	return a.OneSidedTarget > 0 && !math.IsInf(a.OneSidedTarget, 0)
}

// namesItsPrice reports whether the admission says where its line is
// priced (OneSidedAt or OneSidedTarget) rather than taking DefaultKSat.
func (a Admission) namesItsPrice() bool {
	return a.targeted() || (a.OneSidedAt > 0 && a.OneSidedAt <= 1)
}

// oneSidedAt is OneSidedAt, DefaultKSat when it is not a fill in (0, 1].
func (a Admission) oneSidedAt() float64 {
	if !(a.OneSidedAt > 0) || a.OneSidedAt > 1 {
		return DefaultKSat
	}
	return a.OneSidedAt
}

// priceAnchor is the fill the prior's line must be anchored at or past
// (priorLine): oneSidedAt, or, with a target, the fill where fit, the pinned
// fit through the default B, meets it. The fit's fill is the higher of the
// two lines' (the default B is under the prior), so an anchor at or past it
// is at or past the fill the chosen line is priced at too.
func (a Admission) priceAnchor(fit Model) float64 {
	if a.targeted() {
		return a.pricedFill(fit)
	}
	return a.oneSidedAt()
}

// pricedFill is the fill in the band where line's ITL meets OneSidedTarget,
// as whatif.PriceTPOT clamps it: the band's top for a line that stays under
// the target there, its bottom for one at or past it already.
func (a Admission) pricedFill(line Model) float64 {
	lo, hi := a.band()
	switch {
	case line.ITLAt(lo) >= a.OneSidedTarget:
		return lo
	case !(line.A > 0) || line.ITLAt(hi) <= a.OneSidedTarget:
		return hi
	}
	return min(max((a.OneSidedTarget-line.B)/line.A, lo), hi)
}

// pricesHigher reports whether line prices more replicas than other does
// where the analyzer prices: the higher ITL at oneSidedAt, or, with a
// target, the lower fill where the ITL meets it.
func (a Admission) pricesHigher(line, other Model) bool {
	if a.targeted() {
		return a.pricedFill(line) < a.pricedFill(other)
	}
	k := a.oneSidedAt()
	return line.ITLAt(k) > other.ITLAt(k)
}

// PricedRate is what a replica sustains on line where the analyzer prices it,
// up to a constant for one key: sequences over ITL at the fill. The replica
// count goes as its inverse, so the tier latch compares lines by it. At a fixed
// fill that is the ITL there. With a target the fill is the line's own
// (pricedFill) and may clamp to the band's top for both lines, where only the
// ITL still tells them apart; the batch cap then lowers it as
// whatif.ReachableFill does and caps the sequences as whatif.ReplicaTokenRate
// does (TierPrice). Zero for a line with no positive ITL there.
func (a Admission) PricedRate(line Model) float64 {
	k := a.oneSidedAt()
	if a.targeted() {
		lo, _ := a.band()
		k = a.TierPrice.reachable(a.pricedFill(line), lo)
	}
	lat := line.ITLAt(k)
	if !(lat > 0) || math.IsInf(lat, 0) {
		return 0
	}
	return a.TierPrice.sequences(k) / lat
}

// TierPrice is the engine a targeted line is priced on, for PricedRate: the
// replica's KV in tokens, the KV one request holds, and the sequences the
// replica admits (the batch cap times its ranks). A zero field states nothing:
// the fill is the sequences' measure then, which only a cap could change.
type TierPrice struct {
	CapacityTokens, KVPerRequest float64
	MaxSeqs                      int64
}

// reachable is whatif.ReachableFill: k, unless the batch cap stops the cache
// filling that far, and never below the band.
func (p TierPrice) reachable(k, lo float64) float64 {
	if p.MaxSeqs <= 0 || !(p.CapacityTokens > 0) || !(p.KVPerRequest > 0) {
		return k
	}
	kCap := float64(p.MaxSeqs) * p.KVPerRequest / p.CapacityTokens
	if kCap >= k {
		return k
	}
	return max(kCap, lo)
}

// sequences is the sequences resident at k, capped; k itself with no engine
// stated.
func (p TierPrice) sequences(k float64) float64 {
	seqs := Sequences(k, p.CapacityTokens, p.KVPerRequest)
	if !(seqs > 0) {
		return k
	}
	if p.MaxSeqs > 0 {
		seqs = min(seqs, float64(p.MaxSeqs))
	}
	return seqs
}

// keptA is the A a later hold pairs with baseline so that the held line
// prices what line did: the same ITL at oneSidedAt, or, with a target, the
// same ITL at the fill where line meets it (pricedFill), which gives the
// same fill: inside the band the target itself, and at an edge line's ITL
// there, so a line already past the target at the band's bottom is still
// past it, and one under it at the top still under. For a line whose B is
// baseline it is line's A, up to rounding.
func (a Admission) keptA(line Model, baseline float64) float64 {
	k := a.oneSidedAt()
	if a.targeted() {
		k = a.pricedFill(line)
	}
	return (line.ITLAt(k) - baseline) / k
}

// band is the admission's KV band, with the defaults for an edge left at zero.
func (a Admission) band() (lo, hi float64) {
	lo, hi = a.MinK, a.MaxK
	if !(lo > 0) {
		lo = DefaultMinObservableK
	}
	if !(hi > 0) {
		hi = DefaultMaxObservableK
	}
	return lo, hi
}

// atSequenceCap reports whether the replica's batch is at its sequence cap,
// with no more preemption over the ITL's window than CapMaxPreemptionRate, so
// its reading above the band is a point on its line (issue ev-shindin/llm-scaling-manager#287).
//
// The band stops at 0.80 to keep an engine thrashing out of the fit: the
// readings above it steepen A and push B below zero, and preemption tracks
// them (see DefaultMaxObservableK). A replica whose batch is at max_num_seqs
// is not short of memory. It cannot take another sequence however much cache
// is free, and a fill above the band is its sequences' contexts, not
// pressure. Under uneven routing that replica can be the only one with a k
// in reach: on one run a decode replica held 256 running (its max_num_seqs)
// and 768 waiting at k 0.85-0.89 for 20 minutes while three siblings sat
// below 0.15, and no line was ever fitted.
//
// Every condition is required, and an unknown one admits nothing:
//   - MaxNumSeqs is stated -- by the engine's flag, or by the scale target's
//     llm-d.ai/max-num-seqs annotation -- not a default and not learned: vLLM
//     V1 defaults to 1024 on large accelerators where the parser assumes 256,
//     and a batch of 300 would read as capped while it is still growing. A
//     learned cap may sit below the true one in the same way.
//   - The running batch is EXACTLY the cap on every rank (max_num_seqs bounds
//     one engine, so the cap is it times Ranks). More than the cap means the
//     cap is not what was read; fewer means memory, not the cap, binds.
//   - Requests are waiting: a batch at the cap with nothing queued is not
//     held there by the cap. The full waiting count, also on a P/D decode,
//     where part of it is vLLM's reason="deferred": at the cap the scheduler
//     stops before it looks at a request whose KV pull has landed, so that
//     request stays deferred although only the cap holds it.
//   - The preemption rate was read and is at most CapMaxPreemptionRate: an
//     engine preempting for memory is the case the band exists for.
func (a Admission) atSequenceCap(rm domain.ReplicaMetrics) bool {
	return rm.AtBatchCap(a.MaxNumSeqs) &&
		rm.QueueLength > 0 &&
		rm.PreemptionRateKnown && !math.IsNaN(rm.PreemptionRate) &&
		rm.PreemptionRate <= max(a.CapMaxPreemptionRate, 0)
}
