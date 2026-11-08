package allocation

import (
	"math"
	"slices"
	"time"
)

const (
	// ShareHistoryMaxSamples is the most need samples a role's stored history
	// keeps. Samples are bucketed (NeedHistory) so that a swing window fits:
	// one per minute up to this many minutes of window, coarser beyond.
	ShareHistoryMaxSamples = 180
	// ShareHistoryMaxGap is the longest unobserved stretch a restored history
	// may end with and still count as covering its window. A restart's own gap
	// -- the pod's restart, the write interval -- is well inside it; a
	// controller down for longer missed whatever peaked meanwhile, so its
	// restored peaks are kept but not taken as the whole window's.
	ShareHistoryMaxGap = 10 * time.Minute
	// shareHistorySkew is how far in the future a stored timestamp may lie
	// and still be read: clocks across controller replicas differ a little.
	shareHistorySkew = time.Minute
)

// ShareNeedSample is one stored need reading.
type ShareNeedSample struct {
	At   time.Time
	Need float64
}

// ShareRoleNeedHistory is one role's stored need history: since when its need
// has been recorded, and its sustained need per bucket over the last swing
// window, oldest first. A zero Since says nothing is known before the oldest
// sample.
type ShareRoleNeedHistory struct {
	Since   time.Time
	Samples []ShareNeedSample
}

// ShareNeedHistory is a ledger's need history by role, as it is stored across
// controller restarts (NeedHistory, RestoreNeedHistory). It is what PeakNeed
// and PeakKnown read: without it a restarted ledger knows no role's peak, and
// the planner takes every peak at its ceiling for a swing window.
type ShareNeedHistory map[string]ShareRoleNeedHistory

// ShareNeedRestore is what RestoreNeedHistory restored.
type ShareNeedRestore struct {
	// Roles is how many roles had samples restored.
	Roles int
	// KnownNow is how many of them have a known peak (PeakKnown) at once.
	KnownNow int
	// KnownAt is the earliest time a restored role's peak becomes known --
	// a swing window after its restored Since -- or zero when no role's
	// Since was restored.
	KnownAt time.Time
}

// shareHistoryBucket is the width one stored sample stands for: a minute, or
// coarser when a swing window holds more minutes than ShareHistoryMaxSamples.
func shareHistoryBucket(window time.Duration) time.Duration {
	b := time.Minute
	if per := (window + ShareHistoryMaxSamples - 1) / ShareHistoryMaxSamples; per > b {
		b = per
	}
	return b
}

// NeedHistory is the ledger's need history over the last swing window, for
// storage, at most ShareHistoryMaxSamples per role. PeakNeed counts a peak
// only once two readings in a row reach it, so what a bucket (a minute, or
// coarser for a long window) stores is its sustained level: the highest
// min(s[i-1], s[i]) over the pairs of consecutive readings ending in it. A
// bucket's highest single reading would turn an isolated spike in each of two
// neighbouring buckets into a sustained peak after a restart; its lowest
// would erase a real two-reading burst inside it. A pair with a reading older
// than the window, or not a usable need, stores nothing, so neither does the
// role's first reading on its own. RestoreNeedHistory reads each stored level
// back as sustained by itself, so PeakNeed after a restore is PeakNeed before.
func (l *ShareLedger) NeedHistory(now time.Time, tm ShareTimings) ShareNeedHistory {
	bucket := shareHistoryBucket(tm.SwingWindow)
	out := make(ShareNeedHistory, len(l.needsSince))
	for role, since := range l.needsSince {
		s := l.needs[role]
		var samples []ShareNeedSample
		for i := 1; i < len(s); i++ {
			a, b := s[i-1], s[i]
			if now.Sub(a.at) > tm.SwingWindow || !shareNeedValid(a.need) || !shareNeedValid(b.need) {
				continue
			}
			level := math.Min(a.need, b.need)
			if n := len(samples); n > 0 && samples[n-1].At.Truncate(bucket).Equal(b.at.Truncate(bucket)) {
				// Stamped with the bucket's last reading: the level ages out
				// no earlier than the pair that set it.
				samples[n-1] = ShareNeedSample{At: b.at, Need: math.Max(samples[n-1].Need, level)}
				continue
			}
			samples = append(samples, ShareNeedSample{At: b.at, Need: level})
		}
		if len(samples) > ShareHistoryMaxSamples {
			samples = samples[len(samples)-ShareHistoryMaxSamples:]
		}
		out[role] = ShareRoleNeedHistory{Since: since, Samples: samples}
	}
	return out
}

// RestoreNeedHistory merges a stored need history into the ledger, as read back
// after a restart. A sample that is not a finite, non-negative need, that is
// older than the swing window, or that lies in the future is dropped, and at
// most ShareHistoryMaxSamples of the newest are taken per role. Each sample is
// a bucket's sustained level (NeedHistory), so it is restored as two readings
// at its instant: a level by itself, as PeakNeed counts one.
//
// A role's Since is restored -- making its peak known once a window has passed
// since it -- only when its history runs up to within ShareHistoryMaxGap of
// now: a longer gap was not observed, and a peak in it would be missed. It is
// taken no earlier than the start of the bucket before the oldest sample's: a Since the
// samples do not cover -- written by hand, or kept over a shorter window than
// today's -- would call a peak known that nothing recorded. A zero Since is
// the oldest sample. A role not in the group is restored too; Retain forgets
// it if it does not return.
func (l *ShareLedger) RestoreNeedHistory(h ShareNeedHistory, now time.Time, tm ShareTimings) ShareNeedRestore {
	var res ShareNeedRestore
	bucket := shareHistoryBucket(tm.SwingWindow)
	for role, rh := range h {
		samples := make([]shareNeedSample, 0, len(rh.Samples))
		for _, s := range rh.Samples {
			if !shareNeedValid(s.Need) || s.At.IsZero() || now.Sub(s.At) > tm.SwingWindow || s.At.Sub(now) > shareHistorySkew {
				continue
			}
			// Bounded as a live need is (BuildShareGroups): a stored need
			// out of any budget's range would overflow MeanNeed.
			samples = append(samples, shareNeedSample{at: s.At, need: math.Min(s.Need, MaxShareBudgetGPUs)})
		}
		if len(samples) == 0 {
			continue
		}
		res.Roles++
		slices.SortFunc(samples, func(a, b shareNeedSample) int { return a.at.Compare(b.at) })
		if len(samples) > ShareHistoryMaxSamples {
			samples = samples[len(samples)-ShareHistoryMaxSamples:]
		}
		for _, s := range samples {
			l.needs[role] = append(l.needs[role], s, s)
		}
		slices.SortStableFunc(l.needs[role], func(a, b shareNeedSample) int { return a.at.Compare(b.at) })

		oldest, last := samples[0].at, samples[len(samples)-1].at
		since := rh.Since
		switch {
		case since.IsZero() || since.After(oldest):
			// No honest start: the samples say the need was recorded at
			// least from the first of them.
			since = oldest
		case since.Before(oldest.Truncate(bucket).Add(-bucket)):
			// The oldest sample is stamped with its bucket's last reading,
			// and the bucket before it may have just aged out: a bucket
			// before the oldest sample's own is what the samples cover.
			since = oldest.Truncate(bucket).Add(-bucket)
		}
		if now.Sub(last) > ShareHistoryMaxGap {
			continue
		}
		if cur, ok := l.needsSince[role]; !ok || since.Before(cur) {
			l.needsSince[role] = since
		}
		if l.PeakKnown(role, now, tm) {
			res.KnownNow++
		}
		if at := l.needsSince[role].Add(tm.SwingWindow); res.KnownAt.IsZero() || at.Before(res.KnownAt) {
			res.KnownAt = at
		}
	}
	return res
}

// ForgetNeedHistory drops every role's need history, as when the stored
// history is flushed: each role's peak is unknown again until it has been
// recorded for a swing window.
func (l *ShareLedger) ForgetNeedHistory() {
	clear(l.needs)
	clear(l.needsSince)
}

// shareNeedValid reports whether need is a reading a peak may be taken from.
func shareNeedValid(need float64) bool {
	return !math.IsNaN(need) && !math.IsInf(need, 0) && need >= 0
}
