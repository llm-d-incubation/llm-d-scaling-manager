// Package settlerow reads the collector's replica rows as the settle
// tracker's (internal/signals/settle). The engine observes these, and a
// replay of a learner recording observes the same, so the two cannot read a
// row differently. It is apart from settle so that settle itself imports
// only the standard library.
package settlerow

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
)

// Of is one cycle's rows as the settle tracker reads them: one row per
// replica, with its role read from rolesByVariant (a variant it does not name
// is aggregated).
func Of(rows []domain.ReplicaMetrics, rolesByVariant map[string]string) []settle.Row {
	out := make([]settle.Row, 0, len(rows))
	for _, rm := range rows {
		out = append(out, Row(rm, domain.CanonicalRole(rolesByVariant[rm.VariantName]) == domain.RolePrefill))
	}
	return out
}

// Row is the settle tracker's row of rm; prefill says it serves the prefill
// role.
func Row(rm domain.ReplicaMetrics, prefill bool) settle.Row {
	itl := settle.ITLAbsent
	switch rm.ITLState() {
	case domain.ITLFinite:
		itl = settle.ITLFinite
	case domain.ITLEmpty:
		itl = settle.ITLEmpty
	}
	return settle.Row{
		Key:                 Key(rm),
		Prefill:             prefill,
		Ready:               rm.Ready,
		ReadySince:          rm.ReadySince,
		LentSince:           rm.LentSince,
		Revision:            rm.Revision,
		KV:                  rm.KvCacheUsage,
		KVKnown:             rm.KvCacheUsageKnown,
		ITL:                 itl,
		GenerationRate:      rm.GenerationTokenRate,
		GenerationRateKnown: rm.GenerationTokenRateKnown,
		RequestRate:         rm.RequestRate,
		RequestRateKnown:    rm.RequestRateKnown,
	}
}

// Key is the settle tracker's key of rm.
func Key(rm domain.ReplicaMetrics) settle.Key {
	return settle.Key{Pod: rm.PodName, Variant: rm.VariantName, Bridge: rm.FromWarmPool}
}

// ReadyAgain is the first Ready row after the last row of trace that was not
// Ready, zero when every row is Ready or none after it is. A Pod that left
// the rotation and came back with no Ready time to say so has restarted as
// far as anything can tell: k2's joined test and the ITL line both age it
// from here.
func ReadyAgain(trace []settle.Sample) time.Time {
	for i := len(trace) - 1; i >= 0; i-- {
		if trace[i].Row.Ready {
			continue
		}
		if i+1 < len(trace) {
			return trace[i+1].At
		}
		return time.Time{}
	}
	return time.Time{}
}

// ObserveOwn observes one cycle of a model's rows into a tracker an analyzer
// keeps when the engine gave it none (a test that drives Analyze alone). The
// analyzer alone stands for a controller that has watched the fleet for as
// long as the caller says, not for one that has just started: on the
// model's first observation (first) it observes the same rows backdate
// earlier too, so a replica on that cycle is not a joiner. A controller
// restart is the engine's tracker's first cycle; a test of one hands the
// analyzer a tracker of its own making instead.
//
// The backdate must be at most the tracker's absence tolerance
// (settle.Tolerance): past it the second observation reads the gap as an
// absence, and every replica settles again at now (settle.Rejoined). A
// longer backdate is ObserveOwnSince's.
func ObserveOwn(t *settle.Tracker, model string, rows []settle.Row, first bool, backdate time.Duration, now time.Time) {
	if first {
		t.Observe(model, rows, now.Add(-backdate))
	}
	t.Observe(model, rows, now)
}

// ObserveOwnSince is ObserveOwn for a backdate of any length: on the
// model's first observation (first) it observes the rows once, at now, each
// replica taken as seen since backdate earlier, or since its Ready time (a
// bridge's lend time) if later (settle.Tracker.ObserveSince), so it reads
// settle.Joined (settle.Lent for a bridge) with that Since, whatever the
// tracker's absence tolerance. Its trace and idle run hold only
// the rows read at now. On any other observation it is Observe.
func ObserveOwnSince(t *settle.Tracker, model string, rows []settle.Row, first bool, backdate time.Duration, now time.Time) {
	if first {
		t.ObserveSince(model, rows, now.Add(-backdate), now)
		return
	}
	t.Observe(model, rows, now)
}
