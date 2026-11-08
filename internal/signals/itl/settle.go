package itl

import (
	"math"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// The ITL line on the settle tracker (internal/signals/settle,
// analyzer-learner.md 4.3, PR 4a). A reading taken while its replica settles
// is kept out of the full fit and the pinned one (DefaultSteadyFor). Before
// the tracker each key kept its own trace of when each replica began to
// settle: it joined after the key's first Observe, or the load came back to
// it after idleRows rows with neither an ITL nor a k, or it was missing from
// a cycle. With Admission.Settle set, the start is read from the engine's
// tracker instead, and the key keeps only each replica's recent k, which the
// drift test reads (DefaultMaxKDrift).
//
// Five more changes ride on their own fields, each a replay switch of the
// analyzers that set them: RestartUnderName and IdleOnEvidence (rules of the
// tracker's), WaitsAfterRestart, KeepsThroughMissingRow and
// FiniteOnly (Admission).

// Settle is the settle tracker Observe reads a replica's settle from, and the
// tracker's name for the model the rows are of (the engine's namespaced
// key). Read only: the engine writes the tracker, once per model per cycle,
// before any analyzer runs.
type Settle struct {
	View  settle.View
	Model string
}

// start is when the replica of rm last began to settle, as the tracker read
// it: the later of the settle the tracker began (it joined, came back after
// its absence tolerance, restarted under its own name, was lent or rolled
// out), the last time load came back after at least idleRows idle rows
// (settle.Replica.ReturnedAfter: idle on positive evidence, settle.Row's
// Activity), and its first Ready row after one that was not
// (settlerow.ReadyAgain). A replica the tracker does not know -- a caller
// that skipped the observation -- has no start: zero, the same on every
// cycle, so the key's first sight of it is its settle.
//
// Two of those are the admission's rules (Admission.RestartUnderName and
// Admission.IdleOnEvidence). Without the first, a settle the tracker began
// because the Ready time changed under the same name is not read: a restart
// shows only as a row that was not Ready, as on the key's own trace. Without
// the second, load returning is the key's own count instead (idleReturn: a
// run of rows with neither an ITL nor a k).
func (s *Settle) start(rm domain.ReplicaMetrics, adm Admission, idleRows int, idleReturn time.Time) time.Time {
	key := settlerow.Key(rm)
	r, ok := s.View.Replica(s.Model, key)
	if !ok {
		return time.Time{}
	}
	start := r.Since
	if r.Reason == settle.Restarted && !adm.RestartUnderName {
		start = time.Time{}
	}
	returned, ok := idleReturn, !idleReturn.IsZero()
	if adm.IdleOnEvidence {
		returned, ok = r.ReturnedAfter(idleRows)
	}
	if ok && returned.After(start) {
		start = returned
	}
	if back := settlerow.ReadyAgain(s.View.Trace(s.Model, key)); back.After(start) {
		start = back
	}
	return start
}

// countIdle is the key's own idle rule on the tracker's path, for
// Admission.IdleOnEvidence off: a row with an ITL after idleRows or more
// rows with neither an ITL nor a k is load returning (idleReturn). It
// starts nothing itself; the settle reads idleReturn (Settle.start).
func (rt *replicaTrace) countIdle(hasITL, hasK bool, now time.Time, s Steadiness) {
	if !hasITL {
		if !hasK {
			rt.idle++
		}
		return
	}
	if rt.idle >= s.IdleRows {
		rt.idleReturn = now
	}
	rt.idle = 0
}

// hasITL is whether the row carries a reading's ITL: with FiniteOnly, a
// Finite state by the presence bits, so a Pod with an Absent rank is no
// reading; without it, as before the bits, any finite AvgITL above zero,
// which also takes the Finite ranks' mean of a Pod whose other rank was
// Absent (domain.ReplicaMetrics.AvgITLKnown).
func (a Admission) hasITL(rm domain.ReplicaMetrics) bool {
	if a.FiniteOnly {
		return rm.ITLState() == domain.ITLFinite && rm.AvgITL > 0 && !math.IsInf(rm.AvgITL, 1)
	}
	return rm.AvgITL > 0 && !math.IsInf(rm.AvgITL, 1)
}

// absenceTolerance is how long a replica may go without a row and keep its
// trace when KeepsThroughMissingRow is set and the key reads no tracker:
// AbsenceTolerance, at least settle.Period.
func (a Admission) absenceTolerance() time.Duration {
	return max(a.AbsenceTolerance, settle.Period)
}

// forgetAfter is how long a replica's trace outlives its last row once
// KeepsThroughMissingRow keeps it through an absence: the tracker's forget
// horizon, so a replica back within it is judged by the tracker, not
// started over by the key.
func (a Admission) forgetAfter() time.Duration {
	return max(settle.TraceHorizon, a.absenceTolerance())
}

// settlingOnTracker reports whether the replica of rm is inside its settle,
// read from the tracker (Admission.Settle).
//
// The key notes the tracker's start the first cycle it reads it, and the
// settle runs from that cycle, on the analyzer's clock: the engine observes
// a little before the analyzer reads its clock, and a replica that joins
// waits from the cycle the key first saw it, as before the tracker.
//
// A start the key first read on its own first Observe began before the key
// knew the replica: a controller start, or a key made anew. It is not a
// settle unless waits is set, and then it runs from the tracker's start, so
// a replica whose settle began long before a key made anew (an operator's
// forget, new engine arguments) does not wait again. A start the key read
// later is a settle whatever waits says.
//
// restartOnly reports that the settle is that wait alone: a start read on
// the key's first Observe, made a settle by waits.
func (rt *replicaTrace) settlingOnTracker(adm Admission, rm domain.ReplicaMetrics, keyFirst time.Time,
	now time.Time, s Steadiness) (settling, restartOnly bool) {
	start := adm.Settle.start(rm, adm, s.IdleRows, rt.idleReturn)
	waits := adm.WaitsAfterRestart
	// Only forward: a start that moves back is an event the tracker no
	// longer holds -- a not-Ready row aged out of its trace (TraceHorizon),
	// or a return evicted from its last few -- not a new one.
	if rt.settleSeen.IsZero() || start.After(rt.settleStart) {
		rt.settleStart, rt.settleSeen = start, now
	}
	if rt.settleSeen.After(keyFirst) {
		return now.Sub(rt.settleSeen) < s.For, false
	}
	settling = waits && !rt.settleStart.IsZero() && now.Sub(rt.settleStart) < s.For
	return settling, settling
}

// aboveBandSettling is the settle stamp of a reading the above-band window
// keeps (Admission.AboveBandFallback): the band's stamp, settling, without
// the wait after a controller restart (restartOnly, Admission.WaitsAfterRestart).
// The fallback pins B at the least ITL among its readings and prices at a k
// of at least DefaultKSat, so it is one-sided (fitAboveBand): a warm-up
// reading of a replica that was serving before the restart sits above the
// true line and can only raise the price. Waiting steadyFor for it after a
// restart bought nothing and kept the line silent under need. A replica that
// became Ready less than steadyFor ago is still kept out (justReady): a
// joiner keeps its stamp.
func aboveBandSettling(settling, restartOnly bool, rm domain.ReplicaMetrics, now time.Time, s Steadiness) bool {
	if restartOnly {
		return justReady(rm, now, s)
	}
	return settling
}

// justReady reports whether the replica of rm became Ready less than s.For
// before now, by its Pod's Ready time: a joiner, whose ITL still covers its
// warm-up. An unknown Ready time is not one.
func justReady(rm domain.ReplicaMetrics, now time.Time, s Steadiness) bool {
	return !rm.ReadySince.IsZero() && now.Sub(rm.ReadySince) < s.For
}
