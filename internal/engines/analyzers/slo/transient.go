package slo

import (
	"maps"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// minTransientGrace is the least time after a transient ends that its requests
// may still be completing into the router's window. The router records a
// request at end of stream, so a request queued through a wake finishes, and
// is recorded, up to its whole duration later; the grace grows with the
// requests the fleet is serving (transientGrace).
const minTransientGrace = 120 * time.Second

// maxTransientGrace bounds the grace. A mean request of twenty minutes is not
// one the router window can judge anyway, and a bogus service time must not
// leave a model unjudged for hours (or overflow time.Duration).
const maxTransientGrace = 30 * time.Minute

// startingMaxAge is how old a not-Ready Pod may be and still be starting. An
// older one was Ready and failed its probe: a flap, not a new replica, and not
// a reason to stop judging the fleet it belongs to.
const startingMaxAge = 30 * time.Minute

// transientMemo is one model's last transient cycle per objective scope, and
// when the model was last seen.
type transientMemo struct {
	// all is the last transient over every variant; generating the last over
	// the decode and aggregated variants alone.
	all, generating transientMark
	// seenAt is when the model was last analyzed or reported present, and the
	// memo is evicted on it.
	seenAt time.Time
	// analyzedCycle is the engine cycle (BeginCycle) the model was last
	// analyzed in. Presence does not touch it, or a parked model would look
	// as if it had been watched all along.
	analyzedCycle uint64
	// pendingSince is when each variant with replicas pending began to have
	// them, for boundPending.
	pendingSince map[string]time.Time
	// cyclesSinceAnalyzed is how many engine cycles passed between the last
	// two analyses (1 when none was skipped, 0 on first sight), for the log.
	cyclesSinceAnalyzed uint64
}

type transientMark struct {
	at     time.Time
	reason string
}

// starting counts a variant's replicas that are starting rather than flapping.
// PendingReplicas is discovery's status.replicas less ready, so it counts a
// Ready Pod that failed its probe as well as a new one; the Pods' ages tell
// them apart. Without the ages, pending less the Pods that will never become
// Ready is the estimate.
func starting(vs domain.VariantReplicaState) int {
	if vs.PendingAges == nil {
		return max(vs.PendingReplicas-vs.StuckReplicas, 0)
	}
	n := 0
	for _, age := range vs.PendingAges {
		if age >= 0 && age < startingMaxAge.Seconds() {
			n++
		}
	}
	return n
}

// transientOf names what makes this cycle transient over a set of variants, or
// "". The set is waking when none of it has a ready replica but some of it is
// starting (CurrentReplicas is status.replicas, Pods not Ready included, so
// ready is Current - Pending). A set with nothing at all is parked, and a
// parked model is not analyzed; within a model, a variant at zero beside one
// that serves is not a wake. Nor is a set at zero whose Pods are stuck -- one
// that cannot be scheduled is a shortfall, and its misses are judged. A
// replica starting anywhere in the set is a rise in progress, so a rise needs
// no reason of its own.
func transientOf(states []domain.VariantReplicaState, in func(domain.VariantReplicaState) bool) string {
	var ready, starts int
	for _, vs := range states {
		if !in(vs) {
			continue
		}
		ready += max(vs.CurrentReplicas-vs.PendingReplicas, 0)
		starts += starting(vs)
	}
	switch {
	case ready == 0 && starts > 0:
		return constants.SLOExcludedZeroReplicas
	case starts > 0:
		return constants.SLOExcludedStarting
	}
	return ""
}

// transientGrace is the grace after a transient: the router window clears the
// requests that began in it only once the longest of them has finished, so it
// is the longer of minTransientGrace and one and a half times the longest mean
// request duration among the model's ready replicas, at most maxTransientGrace.
func transientGrace(rows []domain.ReplicaMetrics) time.Duration {
	longest := 0.0
	for _, rm := range rows {
		if rm.Ready && !rm.FromWarmPool && whatif.Finite(rm.AvgServiceTime) && rm.AvgServiceTime > longest {
			longest = rm.AvgServiceTime
		}
	}
	// Clamped in seconds, before the conversion can overflow.
	grace := min(1.5*longest, maxTransientGrace.Seconds())
	return max(minTransientGrace, time.Duration(grace*float64(time.Second)))
}

// noteTransient records this cycle and returns, per objective, why the router's
// current window must not be judged ("" when it may). A window is judged only
// once the fleet has been steady for the whole of it and the grace after it: a
// wake from zero or a replica starting inside it would otherwise read as an SLO
// miss that no amount of steady-state capacity would have prevented.
//
// TTFT is excluded on a transient of any variant: a decode short of capacity
// back-pressures prefill, so it delays first tokens too. TPOT is excluded only
// on a transient of a variant that generates (decode or aggregated): a prefill
// replica starting does not change how fast tokens come out.
func (a *Analyzer) noteTransient(key string, states []domain.VariantReplicaState, window, grace time.Duration,
	now time.Time) (ttft, tpot string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.transients[key]
	fresh := m == nil
	if fresh {
		m = &transientMemo{}
		a.transients[key] = m
	}
	// A model the analyzer did not analyze on the previous engine cycle was
	// skipped -- a wake whose Pods had no metrics yet, which the engine does
	// not analyze, a parked model, a cycle its policy did not list slo -- or
	// this process has just started and holds none of its history. Either way
	// the window may hold requests from a transient no cycle here saw, so it
	// is not judged until window+grace from now. Counted in cycles, not read
	// from timing: a slow cycle is not a skipped one.
	unobserved := fresh || a.cycle > m.analyzedCycle+1
	if !fresh {
		m.cyclesSinceAnalyzed = a.cycle - m.analyzedCycle
	}
	m.seenAt, m.analyzedCycle = now, a.cycle
	if unobserved {
		// Nothing here saw the variants pending through the gap, so it cannot
		// say a group has been pending continuously: a group that started a
		// minute ago would be judged as a stuck one.
		m.pendingSince = nil
	}
	states = boundPending(m, states, now)

	if unobserved {
		mark := transientMark{at: now, reason: constants.SLOExcludedUnobserved}
		m.all, m.generating = mark, mark
	}
	if r := transientOf(states, func(domain.VariantReplicaState) bool { return true }); r != "" {
		m.all = transientMark{at: now, reason: r}
	}
	if r := transientOf(states, func(vs domain.VariantReplicaState) bool { return isGenerating(vs.Role) }); r != "" {
		m.generating = transientMark{at: now, reason: r}
	}
	held := func(t transientMark) string {
		if !t.at.IsZero() && now.Sub(t.at) < window+grace {
			return t.reason
		}
		return ""
	}
	return held(m.all), held(m.generating)
}

// transientView is a copy of one model's transient memo, for the log.
type transientView struct {
	all, generating     transientMark
	cyclesSinceAnalyzed uint64
	pendingSince        map[string]time.Time
}

// transientDetail returns the model's transient memo as it stands, or the zero
// view when there is none.
func (a *Analyzer) transientDetail(key string) transientView {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.transients[key]
	if m == nil {
		return transientView{}
	}
	return transientView{all: m.all, generating: m.generating, cyclesSinceAnalyzed: m.cyclesSinceAnalyzed,
		pendingSince: maps.Clone(m.pendingSince)}
}

// boundPending applies the flap rule to a variant whose Pods have no ages. A
// multi-Pod LeaderWorkerSet group lists no ages (discovery cannot tell which
// group a Pod is in), so its pending replicas were all "starting" for as long as they
// stayed pending -- a group that cannot be scheduled, unjudged for ever, when
// that shortfall is exactly an SLO miss worth seeing. Pending continuously for
// startingMaxAge, such a variant's pending replicas stop counting as starting,
// as a Pod older than startingMaxAge does.
//
// Returns states itself when nothing changes, else a copy.
func boundPending(m *transientMemo, states []domain.VariantReplicaState, now time.Time) []domain.VariantReplicaState {
	since := make(map[string]time.Time, len(states))
	var out []domain.VariantReplicaState
	for i, vs := range states {
		if vs.PendingReplicas <= 0 {
			continue
		}
		at, ok := m.pendingSince[vs.VariantName]
		if !ok {
			at = now
		}
		since[vs.VariantName] = at
		if vs.PendingAges == nil && now.Sub(at) >= startingMaxAge {
			if out == nil {
				out = slices.Clone(states)
			}
			// Ages read and none of them young: starting() counts none.
			out[i].PendingAges = []float64{}
		}
	}
	m.pendingSince = since
	if out == nil {
		return states
	}
	return out
}
