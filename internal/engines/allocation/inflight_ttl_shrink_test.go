package allocation

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// Review 187 minor: an entry's TTL only ever grew. An entry admitted on a
// Kueue flavor for a 30m placement timeout that then grew off the flavor
// kept the 30m TTL: expired after 5m, it stayed in the ledger 30m more, and
// the variant could not start a new hold in that time. The TTL follows the
// admission.
func TestAnEntryThatLeavesItsFlavorTakesTheShorterTTL(t *testing.T) {
	l := NewInFlightLedger()
	t0 := time.Unix(10_000, 0)
	flavored := ReplicaShape{Variant: "ns/m", Accelerator: famHBM3, GPUs: 8,
		Flavor: &kueue.FlavorPlacement{NodeLabels: map[string]string{"pool": "borrowed"}}}
	l.Commit(nil, []InFlightAdmission{{Key: "ns/m", Shape: flavored, Want: 2, PlacedNow: 1, TTL: 30 * time.Minute}}, t0)

	t1 := t0.Add(time.Minute)
	plain := ReplicaShape{Variant: "ns/m", Accelerator: famHBM3, GPUs: 8}
	l.Commit(nil, []InFlightAdmission{{Key: "ns/m", Shape: plain, Want: 3, PlacedNow: 1}}, t1)

	// Expired at t1+5m; dropped once a further InFlightTTL has passed.
	at := t1.Add(2*InFlightTTL + time.Minute)
	l.holdOn(newNodeViewAt(racks([]string{"a"}), at), at)
	if _, kept := l.entries["ns/m"]; kept {
		t.Fatal("an entry off its flavor was kept on the flavor's 30m TTL")
	}
}
