package allocation

import (
	"strings"
	"testing"
	"time"
)

// A persisted hold names its scale target with a kind and name this
// controller writes; one with any other is dropped, and a restore with a
// count gives each restored hold the count of its own target.
func TestARestoredHoldIsCountedFromItsPersistedScaleTarget(t *testing.T) {
	t0 := time.Unix(100_000, 0)
	entry := func(kind, name string) PersistedInFlightEntry {
		return PersistedInFlightEntry{GPUs: 8, Want: 2, Placed: 1, Expires: t0.Add(time.Minute), TTLSeconds: 300,
			TargetKind: kind, TargetName: name}
	}
	p := PersistedInFlight{Version: InFlightFormat, Entries: map[string]PersistedInFlightEntry{
		"ns/dep": entry("Deployment", "dep"),
		"ns/lws": entry("LeaderWorkerSet", "lws"),
		"ns/old": entry("", ""),
		"ns/bad": entry("StatefulSet", "sts"),
		"ns/big": entry("Deployment", strings.Repeat("x", 254)),
	}}
	asked := map[string]ScaleTargetRef{}
	l := NewInFlightLedger()
	restored, dropped := l.RestoreInFlightWith(p, t0, InFlightTTL, func(key string, ref ScaleTargetRef) func() (Placed, bool) {
		asked[key] = ref
		return func() (Placed, bool) { return Placed{Whole: 2}, true }
	})
	if restored != 3 || dropped != 2 {
		t.Fatalf("restored %d and dropped %d, want 3 and 2 (an unknown kind and an over-long name)", restored, dropped)
	}
	if len(asked) != 2 || asked["ns/dep"] != (ScaleTargetRef{Kind: "Deployment", Name: "dep"}) ||
		asked["ns/lws"] != (ScaleTargetRef{Kind: "LeaderWorkerSet", Name: "lws"}) {
		t.Fatalf("counts were built for %v, want the two holds that name a scale target", asked)
	}
	l.settle(t0.Add(time.Second))
	if l.Holds("ns/dep", t0.Add(time.Second)) || l.Holds("ns/lws", t0.Add(time.Second)) {
		t.Fatal("a restored hold whose pods all hold a node was not ended by its own count")
	}
	if !l.Holds("ns/old", t0.Add(time.Second)) {
		t.Fatal("control: a hold persisted without a scale target was ended")
	}
}
