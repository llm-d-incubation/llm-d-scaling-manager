package registry

import (
	"sync"
	"testing"
	"time"
)

const (
	// testTTL is short and round so the expiry arithmetic in these tests reads
	// directly; the clock is driven by hand, so the real duration is irrelevant.
	testTTL   = time.Minute
	testModel = "llama"
)

func newTestRegistry(now *time.Time) *Registry {
	r := New(testTTL)
	r.now = func() time.Time { return *now }
	return r
}

// TestObserveRegistersAndRefreshes is discovery itself: the first call about an
// object is what makes the scaling manager aware of it, and every later call is what keeps it.
func TestObserveRegistersAndRefreshes(t *testing.T) {
	now := time.Now()
	r := newTestRegistry(&now)

	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("a registry nobody has called about must be empty, have %d", len(got))
	}

	r.Observe("chat", "llama-so", map[string]string{ModelIDKey: testModel})
	first := r.Snapshot()
	if len(first) != 1 {
		t.Fatalf("expected the call to register the object, have %d", len(first))
	}
	if first[0].Namespace != "chat" || first[0].Name != "llama-so" {
		t.Fatalf("wrong identity: %+v", first[0])
	}
	if first[0].Metadata[ModelIDKey] != testModel {
		t.Fatalf("metadata not carried: %+v", first[0].Metadata)
	}

	firstSeen := first[0].FirstSeen
	now = now.Add(30 * time.Second)
	r.Observe("chat", "llama-so", map[string]string{ModelIDKey: testModel})

	second := r.Snapshot()
	if len(second) != 1 {
		t.Fatalf("a refresh must not duplicate the entry, have %d", len(second))
	}
	if !second[0].FirstSeen.Equal(firstSeen) {
		t.Error("FirstSeen must date the workload, not the last call about it")
	}
	if !second[0].LastSeen.After(firstSeen) {
		t.Error("LastSeen must advance on a refresh")
	}
}

// TestObserveReplacesMetadata: editing a trigger has to take effect. Merging
// would leave a removed key in place for the life of the process.
func TestObserveReplacesMetadata(t *testing.T) {
	now := time.Now()
	r := newTestRegistry(&now)

	r.Observe("chat", "so", map[string]string{ModelIDKey: testModel, VariantCostKey: "10.0"})
	r.Observe("chat", "so", map[string]string{ModelIDKey: testModel})

	e, ok := r.Get("chat", "so")
	if !ok {
		t.Fatal("entry must still be registered")
	}
	if _, present := e.Metadata[VariantCostKey]; present {
		t.Error("a key removed from the trigger must not survive the refresh")
	}
}

// TestObservedMetadataIsCopied: the map arrives on a protobuf message whose
// lifetime is the request's, and the entry outlives the call.
func TestObservedMetadataIsCopied(t *testing.T) {
	now := time.Now()
	r := newTestRegistry(&now)

	caller := map[string]string{ModelIDKey: testModel}
	r.Observe("chat", "so", caller)
	caller[ModelIDKey] = "mutated-after-the-call"

	e, _ := r.Get("chat", "so")
	if e.Metadata[ModelIDKey] != testModel {
		t.Errorf("the registry must not alias the caller's map, have %q", e.Metadata[ModelIDKey])
	}

	// And the other direction: a reader must not be able to edit the registry.
	e.Metadata[ModelIDKey] = "mutated-by-reader"
	again, _ := r.Get("chat", "so")
	if again.Metadata[ModelIDKey] != testModel {
		t.Errorf("a snapshot must not alias the stored map, have %q", again.Metadata[ModelIDKey])
	}
}

// TestEntryExpiresWhenKEDAStopsAsking: the TTL is what replaces deletion. There
// is no watch and no finalizer, so an object that goes away is one scaling manager stops
// being called about.
func TestEntryExpiresWhenKEDAStopsAsking(t *testing.T) {
	now := time.Now()

	r := newTestRegistry(&now)

	r.Observe("chat", "so", map[string]string{ModelIDKey: testModel})

	now = now.Add(testTTL - time.Second)
	if len(r.Snapshot()) != 1 {
		t.Fatal("an entry inside its TTL must stay; polling is allowed to be slow")
	}

	now = now.Add(2 * time.Second)
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("an entry nobody has asked about for a whole TTL must go, have %d", len(got))
	}
	if r.Len() != 0 {
		t.Error("Snapshot must evict, not merely filter; otherwise the map grows forever")
	}
}

// TestOpenStreamKeepsEntryLive is the case the TTL alone gets wrong. A workload
// parked at zero on a push trigger is called about exactly once — StreamIsActive
// — and KEDA neither polls IsActive nor queries metrics for it after that. Aging
// it out would evict precisely the entries whose purpose is to be woken.
func TestOpenStreamKeepsEntryLive(t *testing.T) {
	now := time.Now()

	r := newTestRegistry(&now)

	release := r.Hold("chat", "so", map[string]string{ModelIDKey: testModel})

	now = now.Add(100 * testTTL)
	live := r.Snapshot()
	if len(live) != 1 {
		t.Fatalf("an entry with an open stream must never expire, have %d", len(live))
	}
	if !live[0].Streaming {
		t.Error("the entry should report that it is being streamed")
	}

	// Closing the stream does not delete: KEDA closes and re-opens streams across
	// its own reconciles, so a reconnect inside the TTL must be a refresh.
	release()
	if len(r.Snapshot()) != 1 {
		t.Fatal("closing a stream must hand the entry to the TTL, not delete it")
	}

	now = now.Add(2 * testTTL)
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("once released and aged out, the entry must go, have %d", len(got))
	}
}

// TestOverlappingStreamHoldsDoNotCancelEachOther: KEDA can open a replacement
// stream before closing the old one. Counting holds rather than storing a bool
// stops the stale close from clearing the fresh hold.
func TestOverlappingStreamHoldsDoNotCancelEachOther(t *testing.T) {
	now := time.Now()

	r := newTestRegistry(&now)

	releaseOld := r.Hold("chat", "so", map[string]string{ModelIDKey: testModel})
	releaseNew := r.Hold("chat", "so", map[string]string{ModelIDKey: testModel})

	releaseOld()
	now = now.Add(10 * testTTL)
	if len(r.Snapshot()) != 1 {
		t.Fatal("the surviving stream must still hold the entry live")
	}

	releaseNew()
	now = now.Add(10 * testTTL)
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("with every stream closed the entry must age out, have %d", len(got))
	}
}

// TestReleaseIsIdempotent: the release runs from a deferred call on a stream that
// may also error out, so it must tolerate being run twice without decrementing
// someone else's hold.
func TestReleaseIsIdempotent(t *testing.T) {
	now := time.Now()

	r := newTestRegistry(&now)

	releaseA := r.Hold("chat", "so", map[string]string{ModelIDKey: testModel})
	releaseB := r.Hold("chat", "so", map[string]string{ModelIDKey: testModel})

	releaseA()
	releaseA()
	releaseA()

	now = now.Add(10 * testTTL)
	if len(r.Snapshot()) != 1 {
		t.Fatal("repeated releases must not release a hold they do not own")
	}
	releaseB()
	now = now.Add(10 * testTTL)
	if len(r.Snapshot()) != 0 {
		t.Fatal("the remaining hold should still release normally")
	}
}

// TestGetReportsExpiredAsAbsent without mutating: a lookup has no business
// changing the set, but it must not report a dead entry as live either.
func TestGetReportsExpiredAsAbsent(t *testing.T) {
	now := time.Now()

	r := newTestRegistry(&now)

	r.Observe("chat", "so", map[string]string{ModelIDKey: testModel})
	now = now.Add(2 * testTTL)

	if _, ok := r.Get("chat", "so"); ok {
		t.Error("an expired entry must not be reported live")
	}
	if r.Len() != 1 {
		t.Error("Get must not evict; Snapshot owns that")
	}
}

// TestSnapshotIsOrdered so engine logs and per-cycle iteration are reproducible.
func TestSnapshotIsOrdered(t *testing.T) {
	now := time.Now()
	r := newTestRegistry(&now)

	r.Observe("zeta", "b", nil)
	r.Observe("alpha", "b", nil)
	r.Observe("alpha", "a", nil)

	got := r.Snapshot()
	want := []string{"alpha/a", "alpha/b", "zeta/b"}
	if len(got) != len(want) {
		t.Fatalf("expected %d entries, have %d", len(want), len(got))
	}
	for i, w := range want {
		if k := got[i].Namespace + "/" + got[i].Name; k != w {
			t.Errorf("position %d: want %s, have %s", i, w, k)
		}
	}
}

// TestForgetRemovesOutright covers the case where a call proves the object is
// gone: waiting out the TTL would keep a deleted workload in the fleet the
// optimizer balances against.
func TestForgetRemovesOutright(t *testing.T) {
	now := time.Now()
	r := newTestRegistry(&now)

	r.Observe("chat", "so", nil)
	r.Forget("chat", "so")

	if r.Len() != 0 {
		t.Error("Forget must remove the entry, not expire it")
	}
}

// TestIncompleteRefsAreIgnored: a malformed ref must not create a nameless entry
// the engines would then try to resolve.
func TestIncompleteRefsAreIgnored(t *testing.T) {
	now := time.Now()
	r := newTestRegistry(&now)

	r.Observe("", "so", nil)
	r.Observe("chat", "", nil)
	release := r.Hold("", "", nil)
	release()

	if r.Len() != 0 {
		t.Errorf("expected no entries from malformed refs, have %d", r.Len())
	}
}

// TestConcurrentUse exercises the lock under the shape production has: many gRPC
// handlers writing while the engine loops read. Run with -race.
func TestConcurrentUse(t *testing.T) {
	r := New(time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); r.Observe("chat", "so", map[string]string{ModelIDKey: testModel}) }()
		go func() { defer wg.Done(); r.Hold("chat", "so", map[string]string{ModelIDKey: testModel})() }()
		go func() { defer wg.Done(); _ = r.Snapshot() }()
	}
	wg.Wait()

	if _, ok := r.Get("chat", "so"); !ok {
		t.Error("the entry should have survived the concurrent traffic")
	}
}
