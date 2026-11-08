package allocation

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// heldVariant is the variant whose replica the restart tests hold.
const heldVariant = "ns/m"

// restarted is the ledger a new process holds once it read back what l
// persisted at persistAt, as of now.
func restarted(t *testing.T, l *InFlightLedger, persistAt, now time.Time) *InFlightLedger {
	t.Helper()
	raw, err := l.MarshalInFlight(persistAt)
	if err != nil {
		t.Fatal(err)
	}
	p, err := UnmarshalInFlight(raw)
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewInFlightLedger()
	fresh.RestoreInFlight(p, now, InFlightTTL)
	return fresh
}

// eightGPUNodes is 8-GPU nodes of famHBM3, by name, with the GPUs used on each.
func eightGPUNodes(used map[string]int) map[string]decision.NodeGPU {
	out := map[string]decision.NodeGPU{}
	for n, u := range used {
		out[n] = decision.NodeGPU{Accelerator: famHBM3, Capacity: 8, Used: u}
	}
	return out
}

// PR ev-shindin/llm-scaling-manager#187's known limit: after a restart, a replica already Unschedulable --
// waiting for a preempted borrower to leave the node it was ordered into --
// got no hold. The first cycle sees it at target = current with an unplaced
// replica (ExistingOnly, Unplaced), which only updates an entry the ledger
// has, and the ledger was empty: a wake took the node. The persisted ledger
// keeps the entry, on its own clock.
func TestARestartKeepsAnUnschedulableReplicaNodeHeld(t *testing.T) {
	t0 := time.Unix(100_000, 0)
	nodes := eightGPUNodes(map[string]int{"n1": 8, "n2": 0}) // the replica placed on n1; n2 was its hole
	shape := ReplicaShape{Variant: heldVariant, Accelerator: famHBM3, PodGPUs: []int{8}, GPUs: 8}
	firstCycle := []InFlightAdmission{{Key: heldVariant, Shape: shape, Want: 2, PlacedNow: 1, ExistingOnly: true, Unplaced: true}}
	wakeFits := func(l *InFlightLedger, at time.Time) bool {
		v := newNodeViewAt(nodes, at)
		l.holdOn(v, at)
		return v.PlaceReplicas([]ReplicaShape{{Variant: "cold/c", Accelerator: famHBM3, GPUs: 8}}, false)
	}

	before := NewInFlightLedger()
	before.Admit(heldVariant, shape, 2, 1, nil, t0)
	if wakeFits(before, t0.Add(time.Minute)) {
		t.Fatal("setup: the growth's hole is not held before the restart")
	}

	// Control: the restart of PR ev-shindin/llm-scaling-manager#187, nothing persisted.
	empty := NewInFlightLedger()
	empty.Commit(nil, firstCycle, t0.Add(2*time.Minute))
	if !wakeFits(empty, t0.Add(2*time.Minute)) {
		t.Fatal("control: an empty ledger after a restart already held the node")
	}

	after := restarted(t, before, t0.Add(time.Minute), t0.Add(2*time.Minute))
	after.Commit(nil, firstCycle, t0.Add(2*time.Minute))
	if wakeFits(after, t0.Add(2*time.Minute)) {
		t.Fatal("after a restart, a wake took the node an Unschedulable replica holds")
	}
	// The restart kept the clock: the hold ends InFlightTTL after the growth.
	if !after.Holds(heldVariant, t0.Add(InFlightTTL-time.Second)) || after.Holds(heldVariant, t0.Add(InFlightTTL)) {
		t.Fatal("the restored hold does not end InFlightTTL after the growth was admitted")
	}
}

// PR ev-shindin/llm-scaling-manager#187's other known limit: a LeaderWorkerSet group whose leader holds a
// node and whose worker does not lost the worker's hold in the leader's
// domain on a restart. A 16-GPU group of two 8-GPU pods, its leader on a1 in
// rack a: restored, the worker's 8 GPUs are held in rack a (a2), not the
// group anew elsewhere, and not nothing.
func TestARestartKeepsAHalfPlacedGroupsWorkerHeldInItsDomain(t *testing.T) {
	t0 := time.Unix(100_000, 0)
	nodes := racks([]string{"a", "b"})
	a1 := nodes["a1"]
	a1.Used, a1.LWSGroup = 8, true // the leader, in every snapshot
	nodes["a1"] = a1
	shape := ReplicaShape{Variant: "ns/g", Accelerator: famHBM3, PodGPUs: []int{8, 8}, GPUs: 16, DomainKey: "rack"}
	placed := func() (Placed, bool) {
		return Placed{Partial: []domain.PartialReplica{{Node: "a1", PlacedGPUs: []int{8}}}}, true
	}
	held := func(l *InFlightLedger, at time.Time) map[string]int {
		v := newNodeViewAt(nodes, at)
		l.holdOn(v, at)
		return v.DebitsExcept(nil)
	}

	before := NewInFlightLedger()
	before.Admit("ns/g", shape, 1, 0, placed, t0)
	if got := held(before, t0.Add(time.Minute)); len(got) != 1 || got["a2"] != 8 {
		t.Fatalf("setup: before the restart the worker holds %v, want a2: 8", got)
	}

	after := restarted(t, before, t0.Add(time.Minute), t0.Add(2*time.Minute))
	// The first cycle: the group is at its target with a pod unplaced.
	after.Commit(nil, []InFlightAdmission{{Key: "ns/g", Shape: shape, Want: 1, ExistingOnly: true, Unplaced: true}},
		t0.Add(2*time.Minute))
	if got := held(after, t0.Add(2*time.Minute)); len(got) != 1 || got["a2"] != 8 {
		t.Fatalf("after a restart the worker holds %v, want a2: 8, in its leader's rack", got)
	}
}

// A persisted ledger that is expired, corrupt, of another version, or holds
// figures this controller does not write restores nothing.
func TestAnExpiredOrCorruptPersistedLedgerIsIgnored(t *testing.T) {
	t0 := time.Unix(100_000, 0)
	shape := ReplicaShape{Variant: heldVariant, Accelerator: famHBM3, GPUs: 8}
	l := NewInFlightLedger()
	l.Admit(heldVariant, shape, 2, 1, nil, t0)

	if r := restarted(t, l, t0, t0.Add(InFlightTTL)); r.Holds(heldVariant, t0.Add(InFlightTTL)) || len(r.entries) != 0 {
		t.Fatal("an entry whose hold ended while the controller was down was restored")
	}
	if r := restarted(t, l, t0, t0.Add(time.Minute)); !r.Holds(heldVariant, t0.Add(time.Minute)) {
		t.Fatal("control: an entry within its hold was not restored")
	}

	for name, raw := range map[string]string{
		"not JSON":        `{"version":1,"entries":`,
		"another version": `{"version":2,"entries":{"ns/m":{"gpus":8,"want":2,"placed":1,"expires":"2100-01-01T00:00:00Z","ttlSeconds":300}}}`,
		"no version":      `{"entries":{"ns/m":{"gpus":8,"want":2,"placed":1,"expires":"2100-01-01T00:00:00Z","ttlSeconds":300}}}`,
	} {
		if _, err := UnmarshalInFlight(raw); err == nil {
			t.Errorf("%s: parsed without an error", name)
		}
	}

	far := t0.Add(time.Hour).UTC().Format(time.RFC3339)
	entry := func(fields string) string {
		return `{"version":1,"entries":{"ns/m":{` + fields + `,"expires":"` + far + `"}}}`
	}
	for name, raw := range map[string]string{
		"want at its placed count": entry(`"gpus":8,"want":1,"placed":1,"ttlSeconds":300`),
		"negative placed":          entry(`"gpus":8,"want":1,"placed":-1,"ttlSeconds":300`),
		"want out of bounds":       entry(`"gpus":8,"want":100001,"placed":0,"ttlSeconds":300`),
		"TTL under InFlightTTL":    entry(`"gpus":8,"want":2,"placed":1,"ttlSeconds":10`),
		"more partials than held":  entry(`"gpus":8,"want":2,"placed":1,"ttlSeconds":300,"partial":[{"Node":"a"},{"Node":"b"}]`),
	} {
		p, err := UnmarshalInFlight(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		fresh := NewInFlightLedger()
		if restored, dropped := fresh.RestoreInFlight(p, t0, InFlightTTL); restored != 0 || dropped != 1 {
			t.Errorf("%s: restored %d, dropped %d; want 0 and 1", name, restored, dropped)
		}
	}

	// An expiry further than the TTL from now is brought back to it.
	p, err := UnmarshalInFlight(entry(`"gpus":8,"want":2,"placed":1,"ttlSeconds":300`))
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewInFlightLedger()
	fresh.RestoreInFlight(p, t0, InFlightTTL)
	if fresh.Holds(heldVariant, t0.Add(InFlightTTL)) {
		t.Fatal("a restored hold outlasts its TTL from the restore")
	}
}

// The persisted ledger stays bounded: at most maxInFlightPersisted entries
// and maxInFlightBytes of them, the holds that end last kept first; a ledger
// read back past the bound restores no more.
func TestThePersistedLedgerStaysBounded(t *testing.T) {
	t0 := time.Unix(100_000, 0)
	l := NewInFlightLedger()
	n := maxInFlightPersisted + 88
	for i := range n {
		// Later keys admitted later: their holds end last.
		l.Admit(fmt.Sprintf("ns/v%04d", i), ReplicaShape{Accelerator: famHBM3, GPUs: 16, PodGPUs: []int{8, 8}}, 2, 1, nil,
			t0.Add(time.Duration(i)*time.Millisecond))
	}
	raw, err := l.MarshalInFlight(t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	p, err := UnmarshalInFlight(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != maxInFlightPersisted {
		t.Fatalf("persisted %d entries, want %d", len(p.Entries), maxInFlightPersisted)
	}
	if _, ok := p.Entries[fmt.Sprintf("ns/v%04d", n-1)]; !ok {
		t.Fatal("the hold that ends last was left out")
	}
	if _, ok := p.Entries["ns/v0000"]; ok {
		t.Fatal("the hold that ends first was kept past the bound")
	}

	// Large entries: bounded by bytes.
	big := NewInFlightLedger()
	tol := make([]corev1.Toleration, 40)
	for i := range tol {
		tol[i] = corev1.Toleration{Key: strings.Repeat("k", 60) + strconv.Itoa(i), Operator: corev1.TolerationOpExists}
	}
	for i := range 200 {
		big.Admit(fmt.Sprintf("ns/b%04d", i), ReplicaShape{Accelerator: famHBM3, GPUs: 8, Tolerations: tol}, 2, 1, nil, t0)
	}
	raw, err = big.MarshalInFlight(t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxInFlightBytes+1024 {
		t.Fatalf("the persisted ledger is %d bytes, past its %d-byte bound", len(raw), maxInFlightBytes)
	}

	// Read back past the bound: no more than maxInFlightPersisted restored,
	// the holds that end last, as MarshalInFlight keeps them -- not the first
	// keys by name. Entries out of bounds are dropped before the bound, so
	// they do not take a valid entry's place.
	over := PersistedInFlight{Version: InFlightFormat, Entries: map[string]PersistedInFlightEntry{}}
	for i := range n {
		over.Entries[fmt.Sprintf("ns/v%04d", i)] = PersistedInFlightEntry{GPUs: 8, Want: 2, Placed: 1,
			Expires: t0.Add(time.Minute + time.Duration(i)*time.Millisecond), TTLSeconds: 300}
	}
	for i := range 10 {
		over.Entries[fmt.Sprintf("ns/a-bad%d", i)] = PersistedInFlightEntry{GPUs: 8, Want: 1, Placed: 1,
			Expires: t0.Add(time.Hour), TTLSeconds: 300}
	}
	back := NewInFlightLedger()
	restored, dropped := back.RestoreInFlight(over, t0, InFlightTTL)
	if restored != maxInFlightPersisted || dropped != n-maxInFlightPersisted+10 {
		t.Fatalf("restored %d and dropped %d of %d, want %d and %d", restored, dropped, n+10, maxInFlightPersisted, n-maxInFlightPersisted+10)
	}
	if !back.Holds(fmt.Sprintf("ns/v%04d", n-1), t0) || back.Holds("ns/v0000", t0) {
		t.Fatal("a ledger read back past its bound kept the first keys by name, not the holds that end last")
	}
}

// A persisted TTL is cut to the longest hold the engine starts: an edited
// ledger cannot pin a node for a day.
func TestARestoredHoldIsCutToTheLongestTTL(t *testing.T) {
	t0 := time.Unix(100_000, 0)
	p := PersistedInFlight{Version: InFlightFormat, Entries: map[string]PersistedInFlightEntry{
		heldVariant: {GPUs: 8, Want: 2, Placed: 1, Expires: t0.Add(20 * time.Hour), TTLSeconds: 86400}}}
	l := NewInFlightLedger()
	l.RestoreInFlight(p, t0, 15*time.Minute)
	if !l.Holds(heldVariant, t0.Add(14*time.Minute)) || l.Holds(heldVariant, t0.Add(15*time.Minute)) {
		t.Fatal("a restored hold with a day's TTL was not cut to the 15-minute maximum")
	}
}

// A restored entry counts no pods. The first fitted cycle after the restart
// ends those it does not admit: a variant deleted while the controller was
// down, or no longer asking, would otherwise hold its node until the hold
// expires. One the cycle admits, or a wake admitted since, stays.
func TestTheFirstCycleEndsTheRestoredHoldsItDoesNotAdmit(t *testing.T) {
	t0 := time.Unix(100_000, 0)
	shape := ReplicaShape{Accelerator: famHBM3, PodGPUs: []int{8}, GPUs: 8}
	before := NewInFlightLedger()
	for _, k := range []string{heldVariant, "ns/gone", "cold/woken"} {
		before.Admit(k, shape, 2, 1, nil, t0)
	}
	after := restarted(t, before, t0.Add(time.Minute), t0.Add(2*time.Minute))
	// A wake admits cold/woken before the first cycle.
	after.Commit(nil, []InFlightAdmission{{Key: "cold/woken", Shape: shape, Want: 2, PlacedNow: 1}}, t0.Add(2*time.Minute))
	first := []InFlightAdmission{{Key: heldVariant, Shape: shape, Want: 2, PlacedNow: 1, ExistingOnly: true, Unplaced: true}}
	after.Commit(nil, first, t0.Add(2*time.Minute))
	after.DropRestoredExcept(map[string]bool{heldVariant: true})
	at := t0.Add(2 * time.Minute)
	if after.Holds("ns/gone", at) {
		t.Fatal("a restored hold the first cycle did not admit still holds its node")
	}
	if !after.Holds(heldVariant, at) || !after.Holds("cold/woken", at) {
		t.Fatal("control: a restored hold the cycle admitted, or a wake updated, was ended")
	}
}
