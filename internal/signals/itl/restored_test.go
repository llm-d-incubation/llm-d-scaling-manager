package itl

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

func restoredRow(k float64) []domain.ReplicaMetrics {
	return []domain.ReplicaMetrics{{PodName: "p", VariantName: "v", Ready: true, KvUsageInstant: k, AvgITL: 0.03}}
}

// A key is restored from a snapshot's intercept or slope until its window
// keeps a reading. One turned away leaves it restored; what was learned here
// is not overwritten and not marked.
func TestTracker_RestoredUntilAReadingIsKept(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tr := NewTracker()
	if tr.Restored("k") {
		t.Fatal("an unknown key is not restored")
	}
	tr.RestoreBaseline("k", 0.01)
	if !tr.Restored("k") {
		t.Fatal("a restored intercept marks the key")
	}
	if r := tr.Observe("k", "v", restoredRow(0.01), Admission{}, now); r.Offered != 0 {
		t.Fatalf("premise: a k under the band is turned away, Offered %d", r.Offered)
	}
	if !tr.Restored("k") {
		t.Fatal("a reading turned away leaves the key restored")
	}
	if r := tr.Observe("k", "v", restoredRow(0.4), Admission{}, now); r.Offered != 1 {
		t.Fatalf("premise: a k in the band is kept, Offered %d", r.Offered)
	}
	if tr.Restored("k") {
		t.Fatal("the first reading kept clears the key")
	}

	tr.SetBaseline("live", 0.01)
	tr.RestoreBaseline("live", 0.02)
	if tr.Restored("live") {
		t.Fatal("what was learned here wins, and is not restored")
	}
	tr.RestoreSlope("s", Slope{A: 0.1, FittedAt: now})
	if !tr.Restored("s") {
		t.Fatal("a restored slope marks the key")
	}
}

// A mark goes with the last of what it describes, so it cannot outlive the
// key: a sweep, a forget, and the flushes.
func TestTracker_TheRestoredMarkGoesWithWhatItDescribes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	both := func() *Tracker {
		tr := NewTracker()
		tr.RestoreBaseline("k", 0.01)
		tr.RestoreSlope("k", Slope{A: 0.1, FittedAt: now})
		return tr
	}
	tr := both()
	tr.Sweep(now, func(string) bool { return true })
	if len(tr.restored) != 0 {
		t.Fatalf("after a sweep: %v", tr.restored)
	}
	tr = both()
	tr.ForgetKeys(now, "operator", func(string) bool { return true })
	if len(tr.restored) != 0 {
		t.Fatalf("after a forget: %v", tr.restored)
	}
	tr = both()
	tr.ForgetSlope("k")
	if !tr.Restored("k") {
		t.Fatal("the intercept is still the snapshot's")
	}
	tr.ForgetBaselines()
	if tr.Restored("k") {
		t.Fatal("nothing restored remains")
	}
	tr = both()
	tr.ForgetBaselines()
	if !tr.Restored("k") {
		t.Fatal("the slope is still the snapshot's")
	}
	tr.ForgetSlopes()
	if len(tr.restored) != 0 {
		t.Fatalf("after both flushes: %v", tr.restored)
	}
}
