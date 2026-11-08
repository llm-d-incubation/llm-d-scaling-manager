package settle

import (
	"testing"
	"time"
)

// Issue #511: a first sighting backdated past the absence tolerance. Two
// observations, at since and at now, read the gap as an absence: every
// replica settles again at now (Rejoined). ObserveSince observes once, at
// now, and takes each replica first seen as seen since since: Joined (Lent
// for a bridge) at since, with its Ready time and lend first seen then. The
// trace and the idle run hold only the row read at now.
func TestObserveSinceBacksAFirstSightingPastTheTolerance(t *testing.T) {
	const backdate = 2 * time.Minute
	now := t0
	since := now.Add(-backdate)
	bridge := idle("pool-0")
	bridge.Bridge, bridge.LentSince = true, now.Add(-time.Hour)
	rows := []Row{busy("d"), bridge}

	tr := NewTracker()
	tr.SetInterval(cycle)
	if tr.tolerance >= backdate {
		t.Fatalf("premise: the tolerance %v is not shorter than the backdate %v", tr.tolerance, backdate)
	}
	tr.ObserveSince(model, rows, since, now)

	d := replicaOf(t, tr, "d")
	if d.Reason != Joined || !d.Since.Equal(since) || !d.FirstSeen.Equal(since) || !d.ReadySeen.Equal(since) {
		t.Errorf("d: settle %s at %v, first seen %v, Ready seen %v; want joined, all at %v",
			d.Reason, d.Since, d.FirstSeen, d.ReadySeen, since)
	}
	if !d.LastSeen.Equal(now) {
		t.Errorf("d: last seen %v, want %v", d.LastSeen, now)
	}
	b, ok := tr.Replica(model, bridge.Key)
	if !ok {
		t.Fatal("the tracker does not know the bridge")
	}
	if b.Reason != Lent || !b.Since.Equal(since) || !b.LentSeen.Equal(since) {
		t.Errorf("bridge: settle %s at %v, lend seen %v; want lent, both at %v", b.Reason, b.Since, b.LentSeen, since)
	}
	if b.IdleRows != 1 || !b.IdleFrom.Equal(now) {
		t.Errorf("bridge: idle run of %d rows from %v, want the one row read, at %v", b.IdleRows, b.IdleFrom, now)
	}
	for _, key := range []Key{d.Key, b.Key} {
		if trace := tr.Trace(model, key); len(trace) != 1 || !trace[0].At.Equal(now) {
			t.Errorf("%s: trace %+v, want the one row read, at %v", key.Pod, trace, now)
		}
	}
	if v := tr.Verdicts(); v[Joined] != 1 || v[Lent] != 1 || v[Rejoined] != 0 {
		t.Errorf("verdicts %v, want one join and one lend", v)
	}

	// The two observations the analyzers made before: the same rows at
	// since and at now read as an absence, and every replica settles at now.
	two := NewTracker()
	two.SetInterval(cycle)
	two.Observe(model, rows, since)
	two.Observe(model, rows, now)
	if r := replicaOf(t, two, "d"); r.Reason != Rejoined || !r.Since.Equal(now) {
		t.Errorf("two observations: settle %s at %v; want the gap read as an absence, rejoined at %v", r.Reason, r.Since, now)
	}
}

// ObserveSince changes only replicas first seen: one the tracker holds is
// observed as by Observe, keeping its sighting; and a since after now is now.
func TestObserveSinceLeavesAHeldReplicaAlone(t *testing.T) {
	tr := NewTracker()
	tr.Observe(model, []Row{busy("d")}, t0)
	now := t0.Add(cycle)
	tr.ObserveSince(model, []Row{busy("d"), busy("e")}, now.Add(-time.Hour), now)
	if r := replicaOf(t, tr, "d"); r.Reason != Joined || !r.Since.Equal(t0) || !r.FirstSeen.Equal(t0) {
		t.Errorf("held d: settle %s at %v, first seen %v; want its own join at %v", r.Reason, r.Since, r.FirstSeen, t0)
	}
	if r := replicaOf(t, tr, "e"); !r.Since.Equal(now.Add(-time.Hour)) {
		t.Errorf("new e: settle at %v, want %v", r.Since, now.Add(-time.Hour))
	}

	ahead := NewTracker()
	ahead.ObserveSince(model, []Row{busy("d")}, t0.Add(time.Minute), t0)
	if r := replicaOf(t, ahead, "d"); !r.Since.Equal(t0) || !r.FirstSeen.Equal(t0) {
		t.Errorf("since after now: settle at %v, first seen %v; want %v", r.Since, r.FirstSeen, t0)
	}
}

// A first sighting is never backdated before the Pod was Ready, nor a
// bridge before it was lent: a Pod Ready 10 s ago begins 10 s ago, one Ready
// an hour ago at since, a bridge lent 30 s ago 30 s ago, and a Ready time
// ahead of the controller's clock at now.
func TestObserveSinceNeverStartsBeforeTheReadyTime(t *testing.T) {
	now := t0
	since := now.Add(-2 * time.Minute)
	fresh := busy("fresh")
	fresh.ReadySince = now.Add(-10 * time.Second)
	old := busy("old")
	old.ReadySince = now.Add(-time.Hour)
	ahead := busy("ahead")
	ahead.ReadySince = now.Add(5 * time.Second)
	bridge := busy("pool-0")
	bridge.Bridge, bridge.LentSince = true, now.Add(-30*time.Second)

	tr := NewTracker()
	tr.ObserveSince(model, []Row{fresh, old, ahead, bridge}, since, now)
	for _, tc := range []struct {
		key  Key
		want time.Time
	}{
		{fresh.Key, fresh.ReadySince},
		{old.Key, since},
		{ahead.Key, now},
		{bridge.Key, bridge.LentSince},
	} {
		r, ok := tr.Replica(model, tc.key)
		if !ok {
			t.Fatalf("the tracker does not know %s", tc.key.Pod)
		}
		if !r.Since.Equal(tc.want) || !r.FirstSeen.Equal(tc.want) || !r.ReadySeen.Equal(tc.want) {
			t.Errorf("%s: settle at %v, first seen %v, Ready seen %v; want all at %v",
				tc.key.Pod, r.Since, r.FirstSeen, r.ReadySeen, tc.want)
		}
		if tc.key.Bridge && !r.LentSeen.Equal(tc.want) {
			t.Errorf("%s: lend seen %v, want %v", tc.key.Pod, r.LentSeen, tc.want)
		}
	}
}
