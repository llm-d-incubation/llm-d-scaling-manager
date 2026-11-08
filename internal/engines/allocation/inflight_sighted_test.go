package allocation

import (
	"testing"
	"time"
)

// Review 187 follow-up: the ledger remembered every variant it ever sighted,
// so a deleted variant stayed for the life of the process. A key no commit
// names for sightedFor is dropped; one named every cycle stays.
func TestTheSightedVariantsArePruned(t *testing.T) {
	l := NewInFlightLedger()
	t0 := time.Unix(10_000, 0)
	shape := ReplicaShape{Variant: "ns/v", Accelerator: famHBM3, GPUs: 8}
	adm := func(key string) InFlightAdmission {
		return InFlightAdmission{Key: key, Shape: shape, Want: 1, PlacedNow: 1}
	}
	l.Commit(nil, []InFlightAdmission{adm("ns/gone"), adm("ns/kept")}, t0)
	for at := t0.Add(time.Minute); !at.After(t0.Add(sightedFor + time.Minute)); at = at.Add(time.Minute) {
		l.Commit(nil, []InFlightAdmission{adm("ns/kept")}, at)
	}
	if _, ok := l.sighted["ns/gone"]; ok {
		t.Fatal("a variant no commit named for sightedFor is still sighted")
	}
	if _, ok := l.sighted["ns/kept"]; !ok {
		t.Fatal("a variant named every cycle was dropped")
	}
}
