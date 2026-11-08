package allocation

import (
	"strings"
	"testing"
	"time"
)

// Review AR2: the no-hole safety net did not survive a restart. The replicas
// ordered without a node hole were known only from the in-memory shared
// record (NoHoleOf) and the governor's own copy, and the persisted ledger
// carried neither. After a restart the record is empty, the namespace is
// within its guaranteed figure, so the borrowed timeout never fires either:
// x's replica Kueue never placed waited Pending for good. The ledger now
// carries the watch, and the restarted governor releases the replica past
// the placement timeout after the order.
func TestNoHoleWatchSurvivesARestart(t *testing.T) {
	t0 := time.Unix(50_000, 0)
	settings := DefaultBorrowSettings()
	world := govWorld{g: 32, used: 24}
	x := unplacedVariant(govModel("x", 3, 8), "x-v", t0)

	before := NewBorrowGovernor(settings)
	c := BorrowCycle{Now: t0, Constraints: world.constraints(), Requests: []ModelScalingRequest{x},
		NoHole: NoHoleOf([]BorrowDraw{{Namespace: "b", Variant: "x-v", GPUs: 8, Replicas: 1, Reclaim: true, NoHole: true, OrderedAt: t0}})}
	before.Observe(c)
	raw, err := before.MarshalLedger()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"noHole"`) {
		t.Fatalf("the persisted ledger %s does not carry the no-hole watch", raw)
	}

	for _, tc := range []struct {
		name    string
		restore bool
		want    int
	}{
		{"restored from the ledger: released past the timeout", true, 2},
		{"control: nothing restored, the record empty: left Pending", false, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := NewBorrowGovernor(settings)
			if tc.restore {
				l, err := UnmarshalBorrowLedger(raw)
				if err != nil {
					t.Fatal(err)
				}
				if dropped := after.Restore(l); dropped != 0 {
					t.Fatalf("restore dropped %d items", dropped)
				}
			}
			// The restarted process: the shared record is empty, the pod is
			// still Pending.
			for _, at := range []time.Time{t0.Add(time.Minute), t0.Add(settings.PlacementTimeout + time.Minute)} {
				c := BorrowCycle{Now: at, Constraints: world.constraints(), Requests: []ModelScalingRequest{x}}
				after.Observe(c)
				ds := decisionsOf(c.Requests)
				after.Releases(c, ds)
				if at.Sub(t0) < settings.PlacementTimeout {
					if got := ds[0].TargetReplicas; got != 3 {
						t.Fatalf("x's target is %d inside the placement timeout, want 3", got)
					}
					continue
				}
				if got := ds[0].TargetReplicas; got != tc.want {
					t.Fatalf("x's target is %d past the placement timeout, want %d", got, tc.want)
				}
			}
		})
	}
}

// Review 187 (AR2 residual): a restored watch was dropped by the first cycle
// that had no state for its variant -- the first cycles after a restart may
// not have read it yet -- and x was left Pending for good again. A restored
// watch waits for a cycle that has the variant.
func TestARestoredNoHoleWatchSurvivesACycleWithoutItsVariant(t *testing.T) {
	t0 := time.Unix(50_000, 0)
	settings := DefaultBorrowSettings()
	world := govWorld{g: 32, used: 24}
	x := unplacedVariant(govModel("x", 3, 8), "x-v", t0)
	after := NewBorrowGovernor(settings)
	after.Restore(BorrowLedger{NoHole: map[string]NoHoleReplicas{"b/x-v": {Replicas: 1, GPUs: 8, Since: t0}}})

	// The first cycle after the restart has not read x.
	after.Observe(BorrowCycle{Now: t0.Add(time.Minute), Constraints: world.constraints()})
	c := BorrowCycle{Now: t0.Add(settings.PlacementTimeout + time.Minute), Constraints: world.constraints(),
		Requests: []ModelScalingRequest{x}}
	after.Observe(c)
	ds := decisionsOf(c.Requests)
	after.Releases(c, ds)
	if got := ds[0].TargetReplicas; got != 2 {
		t.Fatalf("x's target is %d past the placement timeout, want 2: the restored watch was dropped", got)
	}
}

// A persisted no-hole watch past the ledger bounds, or malformed, is dropped.
func TestNoHoleRestoreDropsWhatThisControllerDidNotWrite(t *testing.T) {
	g := NewBorrowGovernor(DefaultBorrowSettings())
	at := time.Unix(50_000, 0)
	dropped := g.Restore(BorrowLedger{NoHole: map[string]NoHoleReplicas{
		"b/huge":   {Replicas: maxLedgerReplicas + 1, GPUs: 8, Since: at},
		"no-slash": {Replicas: 1, GPUs: 8, Since: at},
		"b/zero":   {Replicas: 1, GPUs: 8},
		"b/ok":     {Replicas: 1, GPUs: 8, Since: at},
	}})
	if dropped != 1 {
		t.Fatalf("dropped %d as out of bounds, want 1", dropped)
	}
	got := g.Ledger().NoHole
	if len(got) != 1 || got["b/ok"].Replicas != 1 {
		t.Fatalf("restored %+v, want b/ok alone", got)
	}
}
