package decision

import (
	"testing"
	"time"
)

func TestGPUUsageGetBeforeAnyPublish(t *testing.T) {
	// "Unknown" must be distinguishable from "nothing is in use": the consumer
	// skips its capacity check on the former and would wrongly believe the whole
	// cluster free on the latter.
	if snap, ok := NewGPUUsageStore().Get(); ok || snap != nil {
		t.Fatalf("Get() = (%v, %v), want (nil, false) before any publish", snap, ok)
	}
}

func TestGPUUsagePublishDeepCopies(t *testing.T) {
	// The doc promises the caller may keep mutating its own maps. The saturation
	// engine hands over the very maps it accumulates each cycle, so a shallow
	// copy would let the next cycle mutate a snapshot the scale-from-zero engine
	// is concurrently reading.
	s := NewGPUUsageStore()

	byType := map[string]int{"H100": 4}
	byNS := map[string]map[string]int{"chat": {"H100": 4}}
	s.Publish(byType, byNS)

	byType["H100"] = 99
	byType["L4"] = 7
	byNS["chat"]["H100"] = 99
	byNS["other"] = map[string]int{"L4": 1}

	snap, ok := s.Get()
	if !ok {
		t.Fatal("Get() reported no snapshot after Publish")
	}
	if got := snap.ByType["H100"]; got != 4 {
		t.Fatalf("ByType[H100] = %d, want 4 (caller mutation must not leak in)", got)
	}
	if _, leaked := snap.ByType["L4"]; leaked {
		t.Fatal("a key added to the caller's map after Publish leaked into the snapshot")
	}
	if got := snap.ByNamespace["chat"]["H100"]; got != 4 {
		t.Fatalf("ByNamespace[chat][H100] = %d, want 4 (inner map must be copied too)", got)
	}
	if _, leaked := snap.ByNamespace["other"]; leaked {
		t.Fatal("a namespace added to the caller's map after Publish leaked into the snapshot")
	}
}

func TestGPUUsagePublishStampsAndReplaces(t *testing.T) {
	now := time.Now()
	s := NewGPUUsageStore()
	s.now = func() time.Time { return now }

	s.Publish(map[string]int{"H100": 1}, nil)
	first, _ := s.Get()
	if !first.TakenAt.Equal(now) {
		t.Fatalf("TakenAt = %v, want %v", first.TakenAt, now)
	}

	// A later cycle replaces the snapshot wholesale rather than accumulating:
	// usage is an absolute reading, not a delta.
	now = now.Add(time.Minute)
	s.Publish(map[string]int{"L4": 2}, nil)
	second, _ := s.Get()
	if _, stale := second.ByType["H100"]; stale {
		t.Fatal("a key from the previous snapshot survived; Publish must replace, not merge")
	}
	if got := second.ByType["L4"]; got != 2 {
		t.Fatalf("ByType[L4] = %d, want 2", got)
	}
	if !second.TakenAt.Equal(now) {
		t.Fatalf("TakenAt = %v, want the later stamp %v", second.TakenAt, now)
	}
}

// TestGPUUsageEmptySnapshotIsStillASnapshot pins the distinction the parked-fleet
// case depends on: a cycle over an empty population publishes an EMPTY snapshot,
// which is a real answer ("the scaling manager's variants hold no GPUs") and must report ok=true,
// not be mistaken for "no snapshot yet".
func TestGPUUsageEmptySnapshotIsStillASnapshot(t *testing.T) {
	s := NewGPUUsageStore()
	s.Publish(map[string]int{}, map[string]map[string]int{})

	snap, ok := s.Get()
	if !ok {
		t.Fatal("an empty snapshot must still report ok=true")
	}
	if len(snap.ByType) != 0 || len(snap.ByNamespace) != 0 {
		t.Fatalf("snapshot = %+v, want empty maps", snap)
	}
}

func TestGPUUsageConcurrentPublishAndGet(t *testing.T) {
	// Run with -race: the store is written by the saturation loop and read by the
	// scale-from-zero loop.
	s := NewGPUUsageStore()
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			s.Publish(map[string]int{"H100": i}, map[string]map[string]int{"chat": {"H100": i}})
		}
	}()
	for i := 0; i < 200; i++ {
		if snap, ok := s.Get(); ok {
			_ = snap.ByType["H100"]
			_ = snap.ByNamespace["chat"]["H100"]
		}
	}
	<-done
}
