package scaler

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// undecidedStaleAfter is how long a ScaledObject stays in the undecided set
// without another undecided GetMetrics answer. KEDA reads the metric on every
// HPA sync (15 s by default) while the target has replicas, so two minutes is
// eight missed reads: the ScaledObject is gone, parked at zero (KEDA stops
// asking), or answered by another replica. It is what keeps the set bounded
// when a target leaves without ever being decided.
const undecidedStaleAfter = 2 * time.Minute

// undecidedMark is one ScaledObject whose latest GetMetrics answer had no
// decision behind it.
type undecidedMark struct {
	reason string
	// last is the most recent undecided answer, for undecidedStaleAfter.
	last time.Time
	// logged records that the transition into this state has been logged.
	// Kept apart from presence so a disabled logger does not consume the line.
	logged bool
}

// undecidedTargets is the set of ScaledObjects the scaler is currently
// answering without a decision. It backs llmsm_scaler_undecided_targets and
// decides when the "no decision" line is logged: once per transition into the
// state (or into another reason), not once per poll, and again after the
// target has been decided in between. The zero value is ready to use.
type undecidedTargets struct {
	mu sync.Mutex
	m  map[types.NamespacedName]undecidedMark
}

// mark records an undecided answer for key at now, and reports whether this
// answer should be logged: the target was not undecided, was undecided for
// another reason, or its transition has not been logged yet. canLog says the
// logger would print; a line it would drop does not count as logged.
func (u *undecidedTargets) mark(key types.NamespacedName, reason string, now time.Time, canLog bool) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.m == nil {
		u.m = make(map[types.NamespacedName]undecidedMark)
	}
	u.pruneLocked(now, nil)
	m, ok := u.m[key]
	transition := !ok || m.reason != reason || !m.logged
	if ok && m.reason != reason {
		m.logged = false
	}
	m.reason = reason
	m.last = now
	if transition && canLog {
		m.logged = true
	}
	u.m[key] = m
	return transition && canLog
}

// clear removes key, because a decided answer was just given for it, and
// reports whether it was undecided until now.
func (u *undecidedTargets) clear(key types.NamespacedName) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.m[key]; !ok {
		return false
	}
	delete(u.m, key)
	return true
}

// counts prunes the set and returns how many ScaledObjects are undecided per
// namespace. live, when non-nil, reports whether a ScaledObject is still
// registered; one that is not is dropped at once rather than after
// undecidedStaleAfter.
func (u *undecidedTargets) counts(now time.Time, live func(namespace, name string) bool) map[string]int {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.pruneLocked(now, live)
	out := make(map[string]int)
	for k := range u.m {
		out[k.Namespace]++
	}
	return out
}

// pruneLocked drops entries not answered within undecidedStaleAfter, and those
// live reports gone. The caller holds u.mu.
func (u *undecidedTargets) pruneLocked(now time.Time, live func(namespace, name string) bool) {
	for k, m := range u.m {
		if now.Sub(m.last) > undecidedStaleAfter || (live != nil && !live(k.Namespace, k.Name)) {
			delete(u.m, k)
		}
	}
}
