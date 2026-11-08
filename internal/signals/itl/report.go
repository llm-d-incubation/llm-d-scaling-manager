package itl

import (
	"math"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// RestoredKeys is how many keys match that Restored reports: the lines of a
// model a snapshot brought back, for learner-start.
func (t *Tracker) RestoredKeys(match func(key string) bool) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for key := range t.restored {
		if match(key) {
			n++
		}
	}
	return n
}

// reasonOffered is the Report reason of a row the window was offered.
const reasonOffered learn.Reason = "offered"

// Reasons is a reading's admission outcomes by reason: each row the window was
// offered, counted once under why it was or was not taken, and of those taken,
// the ones kept out of the full fit (unsteady) and those of a replica inside
// its settle.
func (r Reading) Reasons() map[learn.Reason]int64 {
	out := map[learn.Reason]int64{}
	for reason, n := range map[learn.Reason]int{
		reasonOffered: r.Offered, "not-ready": r.NotReady, "no-itl": r.NoITL, "no-k": r.NoK,
		"below-band": r.BelowBand, "above-band": r.AboveBand, "capped": r.Capped,
		"unsteady": r.Unsteady, "settling": r.Settling,
	} {
		if n > 0 {
			out[reason] = int64(n)
		}
	}
	return out
}

// Report is rep, naming a variant, filled from the reading of the key that fit
// its line: the outcomes by reason, the window's size, and Writes, the same
// count as "offered", the readings admitted. The key priced the variant when
// the reading holds a line. A line resting on a held slope is as old as its
// fit once the window has aged out, or holds nothing since a restart; one
// fitted above the band, as its newest reading there. A key with none of them
// that holds an intercept is as old as the intercept: the newest reading of the
// full fit that set it, or the stamp a snapshot carried with it
// (RestoreBaselineAt). Only a key with none of these, or an intercept of
// unknown age, has no stamp.
func (t *Tracker) Report(rep learn.Report, key string, r Reading) learn.Report {
	rep.Counts, rep.Samples, rep.Writes = r.Reasons(), r.Held, int64(r.Offered)
	if r.Tier != "" {
		rep.PricingKey = key
	}
	switch {
	case r.AboveBandFit:
		rep.LastAdmitted = t.newestAbove(key)
	case r.Held > 0:
		rep.LastAdmitted = t.Newest(key)
	case r.SlopeHeld:
		rep.LastAdmitted = r.SlopeFittedAt
	default:
		// The intercept the key holds now, not the one Observe read before
		// its fit: the first full fit that stores B is stamped on its cycle.
		rep.LastAdmitted = t.BaselineAdmittedAt(key)
	}
	return rep
}

// newestAbove is Newest for the above-band window.
func (t *Tracker) newestAbove(key string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if w, ok := t.above[key]; ok && len(w.observations) > 0 {
		return w.observations[len(w.observations)-1].Timestamp
	}
	return time.Time{}
}

// Moved reports whether now is more than band, a fraction of was, from was: a
// learner-write band. A value that is not a number moves when the other is
// one, or the comparison never says so (NaN compares false).
func Moved(now, was, band float64) bool {
	if math.IsNaN(now) || math.IsNaN(was) {
		return math.IsNaN(now) != math.IsNaN(was)
	}
	return math.Abs(now-was) > band*math.Abs(was)
}
