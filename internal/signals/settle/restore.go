package settle

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// SwitchFirstSightRestored is the replay switch for learner PR 5d's change E
// (analyzer-learner.md, PR 5): a replica's first sight survives a controller
// restart in the learned-state snapshot. On, the first row of a replica the
// snapshot holds takes its stored sighting when the row is Ready with the
// same non-zero Ready time, so the replica does not wait again. Off, every
// replica is first seen on the new controller's first cycle and waits (4.4).
// On from the constructor; a replay turns it off (SetSwitchesForReplay).
const SwitchFirstSightRestored = "settle-first-sight-restored"

// Sighting is what the snapshot keeps of one replica: its key and the three
// times its wait is read from.
//
// Only a replica with a Ready time is kept: without one, nothing could tell
// the replica on the far side of a restart from a new one. A warm-pool bridge
// is never kept: a lend keeps the Ready time the Pod had in the pool, and a
// Pod handed back and lent again to the same variant keeps its key too, so
// these times cannot tell a new lend from the old one. A bridge waits after
// a restart, as with the snapshot off. The idle run and the returns of load
// are not kept: a return of load during the restart is missed.
type Sighting struct {
	PodName     string    `json:"pod"`
	VariantName string    `json:"variant"`
	FirstSeen   time.Time `json:"firstSeen"`
	ReadySince  time.Time `json:"readySince"`
	ReadySeen   time.Time `json:"readySeen"`
}

// key is the replica the sighting names.
func (s Sighting) key() Key { return Key{Pod: s.PodName, Variant: s.VariantName} }

// usable says the sighting can be installed at now: it names a replica, with
// all three times, none of them ahead of now. A hand-edited or
// skewed time costs the replica its wait, never a wait it should have had.
func (s Sighting) usable(now time.Time) bool {
	return s.PodName != "" && s.VariantName != "" &&
		!s.FirstSeen.IsZero() && !s.ReadySince.IsZero() && !s.ReadySeen.IsZero() &&
		!s.FirstSeen.After(now) && !s.ReadySeen.After(now)
}

// sightingOf is what the snapshot keeps of r, and whether it keeps it.
func sightingOf(r *replica) (Sighting, bool) {
	if r.Bridge || r.ReadySince.IsZero() {
		return Sighting{}, false
	}
	return Sighting{PodName: r.Pod, VariantName: r.Variant, FirstSeen: r.FirstSeen,
		ReadySince: r.ReadySince, ReadySeen: r.ReadySeen}, true
}

// Sightings is what the snapshot keeps, per model, each model's replicas
// sorted, so one state always encodes to one document: the replicas the
// tracker holds, and the restored sightings no row has claimed yet, so a
// second restart inside the first does not lose them. A replica already
// absent past the tolerance is not kept: back now it would settle again
// (Rejoined), so a restart must not spare it that. Nil when there is nothing
// to keep, or with SwitchFirstSightRestored off.
func (t *Tracker) Sightings() map[string][]Sighting {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !t.firstSightRestored {
		return nil
	}
	out := map[string][]Sighting{}
	for name, m := range t.models {
		for _, r := range m.replicas {
			if m.last.Sub(r.LastSeen) > t.tolerance {
				continue
			}
			if s, ok := sightingOf(r); ok {
				out[name] = append(out[name], s)
			}
		}
	}
	for name, pending := range t.restored {
		for key, s := range pending {
			if t.replicaOf(name, key) == nil {
				out[name] = append(out[name], s)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	for _, list := range out {
		slices.SortFunc(list, func(a, b Sighting) int {
			return cmp.Or(cmp.Compare(a.PodName, b.PodName), cmp.Compare(a.VariantName, b.VariantName))
		})
	}
	return out
}

// Restore hands the tracker the sightings a snapshot held, read at now, and
// returns how many it took. Each waits for its replica's first row
// (installRestored). One that is not usable is not taken, nor one whose
// replica this process has already observed: nothing it saw itself is
// overwritten. Those left unclaimed go after the tolerance, as a replica
// missing that long would settle again. With SwitchFirstSightRestored off
// nothing is taken. The caller decides whether the document is young enough
// to offer them at all: the tracker cannot see how long the gap was.
func (t *Tracker) Restore(sightings map[string][]Sighting, now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.firstSightRestored {
		return 0
	}
	taken := 0
	for name, list := range sightings {
		for _, s := range list {
			if !s.usable(now) || t.replicaOf(name, s.key()) != nil {
				continue
			}
			pending := t.restored[name]
			if pending == nil {
				pending = map[Key]Sighting{}
				t.restored[name] = pending
			}
			if _, dup := pending[s.key()]; !dup {
				taken++
			}
			pending[s.key()] = s
		}
	}
	t.restoredUntil = now.Add(t.tolerance)
	return taken
}

// DropRestored drops the restored sightings no row has claimed: the snapshot
// they came from was deleted.
func (t *Tracker) DropRestored() {
	t.mu.Lock()
	defer t.mu.Unlock()
	clear(t.restored)
}

// RestoredCount is how many restored sightings the tracker has installed
// since it was made.
func (t *Tracker) RestoredCount() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.restoredCount
}

// expireRestored drops the restored sightings no row claimed in time.
// Callers hold t.mu.
func (t *Tracker) expireRestored(now time.Time) {
	if len(t.restored) > 0 && now.After(t.restoredUntil) {
		clear(t.restored)
	}
}

// installRestored gives r, a replica first seen, the sighting the snapshot
// held for it, and reports whether it did. Only when the row is
// Ready with the same non-zero Ready time: the Pod has then been Ready since
// before the sighting was written, so the wait it served is still good. An
// engine restart under the same name or an LWS group restart moves the Ready
// time, and a replica not Ready on its first row has lost the sighting: each
// waits as a replica first seen does. The row claims the sighting either
// way. No settle begins, so no verdict is counted.
//
// The settle is dated from the later of the first sight and the Ready time's
// first reading: all the three times can say of when the current one began.
// Callers hold t.mu.
func (t *Tracker) installRestored(r *replica, name string, row Row) bool {
	pending := t.restored[name]
	s, ok := pending[row.Key]
	if !ok {
		return false
	}
	delete(pending, row.Key)
	if len(pending) == 0 {
		delete(t.restored, name)
	}
	if !row.Ready || row.ReadySince.IsZero() || !row.ReadySince.Equal(s.ReadySince) {
		return false
	}
	r.Key = row.Key
	r.FirstSeen, r.ReadySince, r.ReadySeen = s.FirstSeen, s.ReadySince, s.ReadySeen
	r.Revision = row.Revision
	r.Since, r.Reason, r.Restored = s.FirstSeen, Joined, true
	if s.ReadySeen.After(r.Since) {
		r.Since = s.ReadySeen
	}
	t.restoredCount++
	return true
}

// switchNames is the package's replay switches.
func switchNames() []string {
	return []string{SwitchFirstSightRestored}
}

// ReplaySwitchNames is every name SetSwitchesForReplay accepts.
func ReplaySwitchNames() []string {
	return switchNames()
}

// SetSwitchesForReplay sets the package's replay switches by name: the test
// seam a replay drives one named change through (analyzer-learner.md, 2.5).
// A name it does not know is an error, and sets nothing. Never called on a
// production path; a test holds that.
func (t *Tracker) SetSwitchesForReplay(switches map[string]bool) error {
	known := switchNames()
	unknown := slices.Sorted(maps.Keys(switches))
	unknown = slices.DeleteFunc(unknown, func(n string) bool { return slices.Contains(known, n) })
	if len(unknown) > 0 {
		return fmt.Errorf("settle: no replay switch %s", strings.Join(unknown, ", "))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if on, ok := switches[SwitchFirstSightRestored]; ok {
		t.firstSightRestored = on
	}
	return nil
}
