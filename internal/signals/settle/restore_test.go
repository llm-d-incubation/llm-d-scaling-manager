package settle

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// First sight through a controller restart (analyzer-learner.md, PR 5,
// change E): what the snapshot keeps, and when a row takes it back.

// readyAt is when busy's replicas became Ready.
var readyAt = t0.Add(-time.Hour)

// oneSighting is a tracker's sighting of a replica Ready for an hour, first
// seen 40 min before t0.
func oneSighting(pod string) Sighting {
	return Sighting{PodName: pod, VariantName: "dec", FirstSeen: t0.Add(-40 * time.Minute), ReadySince: readyAt,
		ReadySeen: t0.Add(-40 * time.Minute)}
}

// restored is a new tracker, as after a controller restart, holding the
// sightings of pods.
func restored(t *testing.T, pods ...string) *Tracker {
	t.Helper()
	list := make([]Sighting, 0, len(pods))
	for _, p := range pods {
		list = append(list, oneSighting(p))
	}
	tr := NewTracker()
	if got := tr.Restore(map[string][]Sighting{model: list}, t0); got != len(pods) {
		t.Fatalf("Restore took %d of %d", got, len(pods))
	}
	return tr
}

func key(pod string) Key { return Key{Pod: pod, Variant: "dec"} }

// The rule: the first row, Ready with the same non-zero Ready time, takes
// the sighting. The replica keeps its first sight, no settle begins and no
// verdict is counted; it is the replica the old controller saw.
func TestRestore_TheSameReadyTimeKeepsTheSighting(t *testing.T) {
	tr := restored(t, "d")
	tr.Observe(model, []Row{busy("d")}, t0)
	r, ok := tr.Replica(model, key("d"))
	if !ok {
		t.Fatal("the replica is not known")
	}
	s := oneSighting("d")
	if !r.Restored || !r.FirstSeen.Equal(s.FirstSeen) || !r.ReadySeen.Equal(s.ReadySeen) || !r.ReadySince.Equal(readyAt) {
		t.Fatalf("not the stored sighting: %+v", r)
	}
	if !r.Since.Equal(s.FirstSeen) || r.Reason != Joined {
		t.Fatalf("settle: since %v reason %s", r.Since, r.Reason)
	}
	if got := r.ReadyFrom(); !got.Equal(s.ReadySeen) {
		t.Fatalf("ReadyFrom = %v", got)
	}
	if got := tr.Verdicts()[Joined]; got != 0 {
		t.Fatalf("a restored sighting counted %d join verdicts", got)
	}
	if got := tr.RestoredCount(); got != 1 {
		t.Fatalf("RestoredCount = %d", got)
	}
	if !r.LastSeen.Equal(t0) {
		t.Fatalf("LastSeen = %v", r.LastSeen)
	}
}

// A replica that had restarted under the old controller is dated from when
// that controller read the new Ready time, not from its first sight.
func TestRestore_TheSettleRunsFromTheLaterOfTheTimes(t *testing.T) {
	s := oneSighting("d")
	s.ReadySeen = t0.Add(-30 * time.Second)
	tr := NewTracker()
	tr.Restore(map[string][]Sighting{model: {s}}, t0)
	tr.Observe(model, []Row{busy("d")}, t0)
	r, _ := tr.Replica(model, key("d"))
	if !r.Since.Equal(s.ReadySeen) {
		t.Fatalf("since = %v, want the Ready time's first reading %v", r.Since, s.ReadySeen)
	}
}

// Every other first row is a replica first seen now: it waits as with the
// snapshot off. An engine restart or an LWS group restart under the same
// name moves the Ready time; a first row not Ready, or with no Ready time,
// proves nothing.
func TestRestore_AnythingElseWaitsAsAReplicaFirstSeen(t *testing.T) {
	cases := map[string]func(*Row){
		"an engine restart under the same name": func(r *Row) { r.ReadySince = t0.Add(-20 * time.Second) },
		"an LWS group restart, the group's latest Ready time": func(r *Row) {
			r.ReadySince = t0.Add(-5 * time.Second)
		},
		"one second off":     func(r *Row) { r.ReadySince = readyAt.Add(time.Second) },
		"not Ready":          func(r *Row) { r.Ready = false },
		"not Ready, no time": func(r *Row) { r.Ready, r.ReadySince = false, time.Time{} },
		"no Ready time":      func(r *Row) { r.ReadySince = time.Time{} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			tr := restored(t, "d")
			row := busy("d")
			change(&row)
			tr.Observe(model, []Row{row}, t0)
			r, _ := tr.Replica(model, key("d"))
			if r.Restored || !r.FirstSeen.Equal(t0) || !r.Since.Equal(t0) || r.Reason != Joined {
				t.Fatalf("not first seen now: %+v", r)
			}
			if got := tr.Verdicts()[Joined]; got != 1 {
				t.Fatalf("join verdicts = %d", got)
			}
			if got := tr.RestoredCount(); got != 0 {
				t.Fatalf("RestoredCount = %d", got)
			}
		})
	}
}

// The first row claims the sighting whatever it says: a Pod first read not
// Ready that then reads Ready with the stored time does not get it back.
func TestRestore_TheFirstRowClaimsTheSighting(t *testing.T) {
	tr := restored(t, "d")
	notReady := busy("d")
	notReady.Ready = false
	tr.Observe(model, []Row{notReady}, t0)
	tr.Observe(model, []Row{busy("d")}, t0.Add(cycle))
	r, _ := tr.Replica(model, key("d"))
	if r.Restored || !r.FirstSeen.Equal(t0) {
		t.Fatalf("a claimed sighting came back: %+v", r)
	}
	if s := tr.Sightings()[model]; len(s) != 1 || !s[0].FirstSeen.Equal(t0) {
		t.Fatalf("exported %+v", s)
	}

	// Nor after the tracker lets the replica go: a claimed sighting is gone,
	// not waiting under the key for the row after.
	tr = restored(t, "d")
	tr.Observe(model, []Row{notReady}, t0)
	tr.Retain(nil)
	at := t0.Add(cycle)
	tr.Observe(model, []Row{busy("d")}, at)
	if r, _ := tr.Replica(model, key("d")); r.Restored || !r.FirstSeen.Equal(at) {
		t.Fatalf("a claimed sighting came back after the replica went: %+v", r)
	}
}

// A bridge is never kept: a lend keeps the Pod's Ready time, and a Pod lent
// again to the same variant keeps its key, so the three times cannot tell a
// new lend from the old one (#283). Lent again across a restart, it starts
// its grace: the row with the stored Ready time is a bridge first seen.
func TestRestore_ABridgeIsNotKeptSoANewLendStartsAgain(t *testing.T) {
	lent := busy("pool-0")
	lent.Bridge, lent.LentSince = true, t0.Add(-10*time.Minute)
	before := NewTracker()
	before.Observe(model, []Row{lent}, t0.Add(-9*time.Minute))
	before.Observe(model, []Row{lent}, t0.Add(-9*time.Minute+cycle))
	if s := before.Sightings(); s != nil {
		t.Fatalf("a bridge was kept: %+v", s)
	}

	// A hand-made document naming the Pod reaches no bridge row either.
	tr := restored(t, "pool-0")
	lent.LentSince = t0.Add(-time.Minute)
	tr.Observe(model, []Row{lent}, t0)
	r, _ := tr.Replica(model, Key{Pod: "pool-0", Variant: "dec", Bridge: true})
	if r.Restored || r.Reason != Lent || !r.Since.Equal(t0) || !r.LentSeen.Equal(t0) {
		t.Fatalf("the new lend does not start again: %+v", r)
	}
}

// The idle run is not kept: load returning across the restart is missed.
// Named here because it is a known cost of the change, not a guarantee.
func TestRestore_AnIdleRunIsNotKept(t *testing.T) {
	before := NewTracker()
	for i := range 3 {
		before.Observe(model, []Row{idle("d")}, t0.Add(time.Duration(i)*cycle))
	}
	if r, _ := before.Replica(model, key("d")); r.IdleRows != 3 {
		t.Fatalf("precondition: idle rows %d", r.IdleRows)
	}
	back := t0.Add(5 * time.Minute)
	tr := NewTracker()
	tr.Restore(before.Sightings(), back)
	tr.Observe(model, []Row{busy("d")}, back)
	r, _ := tr.Replica(model, key("d"))
	if !r.Restored || r.IdleRows != 0 || len(r.Returns) != 0 {
		t.Fatalf("%+v", r)
	}
}

// A settle after the restart ends the restored mark, and a restart under
// the same name waits from then, as it does live.
func TestRestore_ALaterRestartSettlesAsUsual(t *testing.T) {
	tr := restored(t, "d")
	tr.Observe(model, []Row{busy("d")}, t0)
	row := busy("d")
	row.ReadySince = t0.Add(cycle)
	at := t0.Add(2 * cycle)
	tr.Observe(model, []Row{row}, at)
	r, _ := tr.Replica(model, key("d"))
	if r.Restored || r.Reason != Restarted || !r.Since.Equal(at) {
		t.Fatalf("%+v", r)
	}
}

// A sighting that cannot be read as one is not taken: each costs the replica
// its wait, none gives it one it should not have.
func TestRestore_AnUnusableSightingIsNotTaken(t *testing.T) {
	future := t0.Add(time.Second)
	for name, change := range map[string]func(*Sighting){
		"no pod":              func(s *Sighting) { s.PodName = "" },
		"no variant":          func(s *Sighting) { s.VariantName = "" },
		"no first sight":      func(s *Sighting) { s.FirstSeen = time.Time{} },
		"no Ready time":       func(s *Sighting) { s.ReadySince = time.Time{} },
		"no Ready sight":      func(s *Sighting) { s.ReadySeen = time.Time{} },
		"first sight ahead":   func(s *Sighting) { s.FirstSeen = future },
		"Ready sight ahead":   func(s *Sighting) { s.ReadySeen = future },
		"first sight at last": func(s *Sighting) { s.FirstSeen = future.Add(time.Hour) },
	} {
		t.Run(name, func(t *testing.T) {
			s := oneSighting("d")
			change(&s)
			tr := NewTracker()
			if got := tr.Restore(map[string][]Sighting{model: {s}}, t0); got != 0 {
				t.Fatalf("took %d", got)
			}
		})
	}
	// At now exactly is not ahead.
	s := oneSighting("d")
	s.FirstSeen, s.ReadySeen = t0, t0
	if got := NewTracker().Restore(map[string][]Sighting{model: {s}}, t0); got != 1 {
		t.Fatalf("a sighting at now: took %d", got)
	}
	// A list that names a replica twice takes it once.
	if got := NewTracker().Restore(map[string][]Sighting{model: {oneSighting("d"), oneSighting("d")}}, t0); got != 1 {
		t.Fatalf("a replica named twice: took %d", got)
	}
}

// A read that worked only after some cycles (a store that was unreadable)
// finds the replicas already seen: what this process saw stands.
func TestRestore_AReplicaAlreadyObservedIsNotOverwritten(t *testing.T) {
	tr := NewTracker()
	tr.Observe(model, []Row{busy("d")}, t0)
	if got := tr.Restore(map[string][]Sighting{model: {oneSighting("d")}}, t0.Add(cycle)); got != 0 {
		t.Fatalf("took %d", got)
	}
	r, _ := tr.Replica(model, key("d"))
	if r.Restored || !r.FirstSeen.Equal(t0) {
		t.Fatalf("%+v", r)
	}
}

// A sighting no row has claimed is exported, so a second restart inside the
// first loses nothing, and goes past the tolerance, as a replica missing that
// long settles again. An unclaimed one is not lost on a cycle that holds or
// sees other replicas.
func TestRestore_UnclaimedSightingsAreKeptThenExpire(t *testing.T) {
	tr := restored(t, "d", "e")
	tr.Observe(model, []Row{busy("d")}, t0)
	want := []string{"d", "e"}
	got := func() []string {
		list := tr.Sightings()[model]
		out := make([]string, 0, len(list))
		for _, s := range list {
			out = append(out, s.PodName)
		}
		return out
	}
	if !reflect.DeepEqual(got(), want) {
		t.Fatalf("exported %v", got())
	}
	if s := tr.Sightings()[model]; s[1] != oneSighting("e") {
		t.Fatalf("an unclaimed sighting changed: %+v", s[1])
	}

	// At the tolerance exactly it is claimed.
	tr.Observe(model, []Row{busy("d"), busy("e")}, t0.Add(Period))
	if r, _ := tr.Replica(model, key("e")); !r.Restored {
		t.Fatalf("at the tolerance exactly it is still claimed: %+v", r)
	}

	// Past it, it is gone: the replica is first seen.
	tr = restored(t, "d", "e")
	tr.Observe(model, []Row{busy("d")}, t0)
	at := t0.Add(Period + time.Second)
	tr.Observe(model, []Row{busy("d"), busy("e")}, at)
	if r, _ := tr.Replica(model, key("e")); r.Restored || !r.FirstSeen.Equal(at) {
		t.Fatalf("past the tolerance: %+v", r)
	}
}

// A replica already absent past the tolerance when the document is written
// is not kept: back in this process it would settle again, so a restart in
// between must not spare it. At the tolerance exactly it is kept, as a row
// then would keep it. A held cycle is no absence.
func TestSightings_AReplicaAbsentPastTheToleranceIsNotKept(t *testing.T) {
	gone := func(after time.Duration) []Sighting {
		tr := NewTracker()
		tr.Observe(model, []Row{busy("d"), busy("e")}, t0)
		tr.Observe(model, []Row{busy("e")}, t0.Add(after))
		return tr.Sightings()[model]
	}
	if got := gone(Period); len(got) != 2 {
		t.Fatalf("at the tolerance: %+v", got)
	}
	got := gone(Period + time.Second)
	if len(got) != 1 || got[0].PodName != "e" {
		t.Fatalf("past the tolerance: %+v", got)
	}

	held := NewTracker()
	held.Observe(model, []Row{busy("d")}, t0)
	held.Hold(model, t0.Add(10*time.Minute/5))
	if s := held.Sightings()[model]; len(s) != 1 {
		t.Fatalf("a held cycle: %+v", s)
	}
}

// Forget and DropRestored take the unclaimed ones; Retain does not, because
// the first cycles after a restart can see no active model at all.
func TestRestore_WhatDropsTheUnclaimedSightings(t *testing.T) {
	tr := restored(t, "d")
	tr.Retain(nil)
	if tr.Sightings() == nil {
		t.Fatal("Retain took an unclaimed sighting")
	}
	tr.Forget(model)
	if s := tr.Sightings(); s != nil {
		t.Fatalf("Forget left %+v", s)
	}
	tr = restored(t, "d")
	tr.DropRestored()
	if s := tr.Sightings(); s != nil {
		t.Fatalf("DropRestored left %+v", s)
	}
}

// One state encodes to one document: the list is sorted, so the snapshot's
// digest does not move with map order, and a restored sighting exports the
// same bytes it came in as.
func TestSightings_AreSortedAndStable(t *testing.T) {
	first := restored(t, "c", "a", "b")
	first.Observe(model, []Row{busy("a"), busy("b"), busy("c")}, t0)
	want, err := json.Marshal(map[string][]Sighting{model: {oneSighting("a"), oneSighting("b"), oneSighting("c")}})
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		got, err := json.Marshal(first.Sightings())
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("got %s\nwant %s", got, want)
		}
	}
}

// A sighting survives the JSON the snapshot stores it in, and the Ready time
// compares equal to the row's after the round trip.
func TestSightings_SurviveTheDocument(t *testing.T) {
	before := NewTracker()
	row := busy("d")
	row.ReadySince = time.Date(2026, 10, 31, 11, 0, 0, 0, time.FixedZone("x", 3600))
	before.Observe(model, []Row{row}, t0)
	raw, err := json.Marshal(before.Sightings())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{`"pod"`, `"variant"`, `"firstSeen"`, `"readySince"`, `"readySeen"`} {
		if !strings.Contains(string(raw), name) {
			t.Fatalf("%s is not in %s", name, raw)
		}
	}
	var back map[string][]Sighting
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	tr := NewTracker()
	tr.Restore(back, t0.Add(cycle))
	tr.Observe(model, []Row{row}, t0.Add(cycle))
	if r, _ := tr.Replica(model, key("d")); !r.Restored {
		t.Fatalf("%+v", r)
	}
}

// A replica with no Ready time is not kept: nothing could prove it the same
// replica. An empty tracker keeps nothing.
func TestSightings_NoReadyTimeIsNotKept(t *testing.T) {
	tr := NewTracker()
	if s := tr.Sightings(); s != nil {
		t.Fatalf("%+v", s)
	}
	row := busy("d")
	row.ReadySince = time.Time{}
	tr.Observe(model, []Row{row}, t0)
	if s := tr.Sightings(); s != nil {
		t.Fatalf("%+v", s)
	}
}

// The switch: off, nothing is exported, nothing is taken, and the row is a
// replica first seen, as on the parent.
func TestFirstSightSwitch_OffIsTheParent(t *testing.T) {
	tr := NewTracker()
	if err := tr.SetSwitchesForReplay(map[string]bool{SwitchFirstSightRestored: false}); err != nil {
		t.Fatal(err)
	}
	if got := tr.Restore(map[string][]Sighting{model: {oneSighting("d")}}, t0); got != 0 {
		t.Fatalf("took %d", got)
	}
	tr.Observe(model, []Row{busy("d")}, t0)
	if r, _ := tr.Replica(model, key("d")); r.Restored || !r.FirstSeen.Equal(t0) {
		t.Fatalf("%+v", r)
	}
	if s := tr.Sightings(); s != nil {
		t.Fatalf("%+v", s)
	}
}

func TestFirstSightSwitch_ANameItDoesNotKnowSetsNothing(t *testing.T) {
	tr := NewTracker()
	err := tr.SetSwitchesForReplay(map[string]bool{"b": true, "a": false, SwitchFirstSightRestored: false})
	if err == nil || !strings.Contains(err.Error(), "settle: no replay switch a, b") {
		t.Fatalf("err = %v", err)
	}
	if !tr.firstSightRestored {
		t.Fatal("a refused set moved the switch")
	}
	if len(ReplaySwitchNames()) != 1 || ReplaySwitchNames()[0] != SwitchFirstSightRestored {
		t.Fatalf("%v", ReplaySwitchNames())
	}
}
