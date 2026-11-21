package itl

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// heldAdm is the admission the held floor's tests run: the floor read below
// the band, and held through the cycles that read none.
var heldAdm = Admission{BaselineFromFloor: true, HoldsFloorBaseline: true}

// bandCycle is the fill the in-band stretch reads at cycle c: 0.30 to 0.40,
// too narrow a spread for a full fit.
func bandCycle(c int) float64 { return []float64{0.30, 0.35, 0.40, 0.35}[c%4] }

// inBandFor runs a key through 20 cycles below the band, then n cycles in
// it at bandCycle on line m, from start, and returns every in-band reading.
func inBandFor(tr *Tracker, adm Admission, m Model, n int, start time.Time) []Reading {
	for c := range 20 {
		tr.Observe("k", "v", floorRows(0.05, m.A, m.B, start), adm, start.Add(time.Duration(c)*15*time.Second))
	}
	out := make([]Reading, 0, n)
	for c := range n {
		at := start.Add(time.Duration(20+c) * 15 * time.Second)
		out = append(out, tr.Observe("k", "v", floorRows(bandCycle(c), m.A, m.B, start), adm, at))
	}
	return out
}

// A fleet held inside the band for an hour, none of its replicas below it,
// keeps the B it read from its floor: the line is the truth on every cycle.
// Without the hold (the negative control), the floor's readings age out at
// DefaultObservationMaxAge, the line steps to the default B, and its slope
// to a steeper one, with no change in load (issue #333).
func TestTracker_AFleetHeldInTheBandKeepsItsFloorB(t *testing.T) {
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	const hour = 240 // cycles of 15 s
	on := inBandFor(NewTracker(), heldAdm, floorTrue, hour, start)
	off := inBandFor(NewTracker(), Admission{BaselineFromFloor: true}, floorTrue, hour, start)

	held, first := 0, -1
	for c, r := range on {
		if first < 0 && r.Tier == "" {
			continue // too few readings in the band yet for any line
		}
		if first < 0 {
			first = c
		}
		if r.Tier != TierPinnedB || !r.PinnedAtFloor {
			t.Fatalf("on, cycle %d: tier %q, pinned at the floor %v; want a floor-pinned line", c, r.Tier, r.PinnedAtFloor)
		}
		if math.Abs(r.Model.B-floorTrue.B) > 1e-9 || math.Abs(r.Model.A-floorTrue.A) > 1e-9 {
			t.Fatalf("on, cycle %d (%s in the band): line %+v, want the truth %+v", c, time.Duration(c)*15*time.Second, r.Model, floorTrue)
		}
		if r.FloorBHeld {
			held++
			if r.FloorBAt.IsZero() || !r.FloorBAt.Before(start.Add(time.Duration(20+c)*15*time.Second)) {
				t.Errorf("on, cycle %d: held B stamped %v, want when the floor was read", c, r.FloorBAt)
			}
		}
	}
	if held == 0 || first < 0 || first > 4 {
		t.Fatalf("premise: a line from cycle %d, and %d cycles held past the floor's age", first, held)
	}

	stepAt := -1
	for c, r := range off {
		if r.Model.B == DefaultBaselineSec {
			stepAt = c
			break
		}
	}
	if stepAt < 0 {
		t.Fatalf("negative control: off, the line never left the floor's B in %d cycles", len(off))
	}
	if at := time.Duration(stepAt) * 15 * time.Second; at < 25*time.Minute || at > 31*time.Minute {
		t.Fatalf("negative control: off, the step at %s, want it where the floor ages out (%s)", at, DefaultObservationMaxAge)
	}
	if held != len(on)-stepAt {
		t.Errorf("on: held on %d cycles, want every cycle from the step off (%d)", held, len(on)-stepAt)
	}
	r := off[stepAt]
	if !(r.Model.A > floorTrue.A*1.2) || r.PinnedAtFloor {
		t.Errorf("negative control: off at %s, line %+v (pinned at the floor %v), want the default B and a steeper slope",
			time.Duration(stepAt)*15*time.Second, r.Model, r.PinnedAtFloor)
	}
	t.Logf("off: B %g -> %g and A %g -> %g at %s in the band; on: held through %d of %d cycles",
		off[stepAt-1].Model.B, r.Model.B, off[stepAt-1].Model.A, r.Model.A, time.Duration(stepAt)*15*time.Second, held, len(on))
}

// A newer floor replaces the held B: the fleet drops below the band again on
// a faster engine's line, and the line is pinned through that floor's B.
func TestTracker_ANewerFloorReplacesTheHeldB(t *testing.T) {
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	rs := inBandFor(tr, heldAdm, floorTrue, 160, start)
	if !rs[len(rs)-1].FloorBHeld {
		t.Fatalf("premise: the B is held; got %+v", rs[len(rs)-1])
	}
	faster := Model{A: floorTrue.A, B: 0.009}
	later := start.Add(3 * time.Hour)
	var r Reading
	for c := range 20 {
		tr.Observe("k", "v", floorRows(0.05, faster.A, faster.B, start), heldAdm, later.Add(time.Duration(c)*15*time.Second))
	}
	for c := range 8 {
		r = tr.Observe("k", "v", floorRows(bandCycle(c), faster.A, faster.B, start), heldAdm, later.Add(time.Duration(20+c)*15*time.Second))
	}
	if r.FloorBHeld || !r.PinnedAtFloor || math.Abs(r.Model.B-faster.B) > 1e-9 {
		t.Errorf("after a new floor: line %+v (held %v, pinned at the floor %v), want B %g from the new floor",
			r.Model, r.FloorBHeld, r.PinnedAtFloor, faster.B)
	}
}

// What contradicts the held B ends it: a pinned reading under it, which no
// line through it passes, or a slope fitted under other engine arguments.
// Either way the line is back on the default B, as without the hold.
func TestTracker_EvidenceAgainstTheHeldBEndsIt(t *testing.T) {
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	held := func(adm Admission) *Tracker {
		tr := NewTracker()
		rs := inBandFor(tr, adm, floorTrue, 160, start)
		if !rs[len(rs)-1].FloorBHeld {
			t.Fatalf("premise: the B is held; got %+v", rs[len(rs)-1])
		}
		return tr
	}
	later := start.Add(time.Duration(180) * 15 * time.Second)

	// Readings at 10.5 to 11 ms, under the held 12 ms: an engine the B is
	// not.
	tr := held(heldAdm)
	under := Model{A: 0.005, B: 0.009}
	var r Reading
	for c := range 12 {
		r = tr.Observe("k", "v", floorRows(bandCycle(c), under.A, under.B, start), heldAdm, later.Add(time.Duration(c)*15*time.Second))
	}
	if r.FloorBHeld || r.PinnedAtFloor || r.Model.B != DefaultBaselineSec {
		t.Errorf("readings under the held B: line %+v (held %v), want the default B", r.Model, r.FloorBHeld)
	}

	argsA := heldAdm
	argsA.Args = "a"
	tr = held(argsA)
	argsB := heldAdm
	argsB.Args = "b"
	r = tr.Observe("k", "v", floorRows(0.35, floorTrue.A, floorTrue.B, start), argsB, later)
	if r.FloorBHeld || r.Model.B != DefaultBaselineSec {
		t.Errorf("other engine arguments: line %+v (held %v), want the default B", r.Model, r.FloorBHeld)
	}
}

// A full fit takes over from the held B, as from a floor read this cycle:
// the key learns its own B, and pins it from then on.
func TestTracker_AFullFitOverridesTheHeldB(t *testing.T) {
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	rs := inBandFor(tr, heldAdm, floorTrue, 160, start)
	if !rs[len(rs)-1].FloorBHeld {
		t.Fatalf("premise: the B is held; got %+v", rs[len(rs)-1])
	}
	// The engine's true B is 9 ms, under the held 12: three replicas each
	// held at its own fill across the band, a spread a full fit takes.
	truth := Model{A: 0.05, B: 0.009}
	later := start.Add(3 * time.Hour)
	spread := func() []domain.ReplicaMetrics {
		out := make([]domain.ReplicaMetrics, 0, 6)
		for i, k := range []float64{0.20, 0.30, 0.45, 0.60, 0.75, 0.85} {
			out = append(out, domain.ReplicaMetrics{PodName: fmt.Sprintf("pod-%d", i), VariantName: "v", Ready: true,
				ReadySince: start.Add(-time.Hour), KvCacheUsage: k, KvUsageInstant: k, AvgITL: truth.ITLAt(k)})
		}
		return out
	}
	var r Reading
	for c := range 12 {
		r = tr.Observe("k", "v", spread(), heldAdm, later.Add(time.Duration(c)*15*time.Second))
		if r.Tier == TierOLS {
			break
		}
	}
	if r.Tier != TierOLS || math.Abs(r.Model.B-truth.B) > 1e-9 {
		t.Fatalf("premise: a full fit learns B %g; got %+v (%q)", truth.B, r.Model, r.Tier)
	}
	r = tr.Observe("k", "v", floorRows(0.35, truth.A, truth.B, start), heldAdm, later.Add(15*time.Minute))
	if r.FloorBHeld || r.PinnedAtFloor || !r.BaselineLearned || math.Abs(r.Model.B-truth.B) > 1e-9 {
		t.Errorf("after the full fit: line %+v (held %v, learned %v), want the learned B %g", r.Model, r.FloorBHeld, r.BaselineLearned, truth.B)
	}
}

// The held B goes out with the key's slope, stamped with when its floor was
// read, and a restart puts it back: a fleet still inside the band after the
// restart, with no floor to read, prices the line it priced before. Without
// the hold the restored tracker pins the default B.
func TestTracker_ARestoredHeldBHoldsAfterARestart(t *testing.T) {
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	rs := inBandFor(tr, heldAdm, floorTrue, 160, start)
	before := rs[len(rs)-1]
	if !before.FloorBHeld {
		t.Fatalf("premise: the B is held; got %+v", before)
	}
	exported := tr.Slopes()["k"]
	if exported.FloorB != floorTrue.B || !exported.FloorBAt.Equal(before.FloorBAt) || exported.FloorBAt.IsZero() {
		t.Fatalf("exported %+v, want B %g stamped %v", exported, floorTrue.B, before.FloorBAt)
	}
	at := start.Add(time.Duration(200) * 15 * time.Second)
	for _, c := range []struct {
		name  string
		adm   Admission
		wantB float64
	}{
		{"held", heldAdm, floorTrue.B},
		{"negative control: not held", Admission{BaselineFromFloor: true}, DefaultBaselineSec},
	} {
		back := NewTracker()
		back.RestoreSlope("k", exported)
		var r Reading
		for i := range 12 {
			r = back.Observe("k", "v", floorRows(bandCycle(i), floorTrue.A, floorTrue.B, start), c.adm, at.Add(time.Duration(i)*15*time.Second))
		}
		if r.Tier != TierPinnedB || math.Abs(r.Model.B-c.wantB) > 1e-9 {
			t.Errorf("%s: line %+v (%q), want B %g", c.name, r.Model, r.Tier, c.wantB)
		}
		if c.wantB == floorTrue.B && (!r.FloorBHeld || !r.FloorBAt.Equal(before.FloorBAt)) {
			t.Errorf("%s: held %v stamped %v, want the stamp it was exported with, %v", c.name, r.FloorBHeld, r.FloorBAt, before.FloorBAt)
		}
	}
}

// A sweep of the key forgets the held B with the slope it rides on, as it
// forgets a learned B: the key is gone, and so is what it learned.
func TestTracker_ASweepForgetsTheHeldB(t *testing.T) {
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	rs := inBandFor(tr, heldAdm, floorTrue, 160, start)
	if !rs[len(rs)-1].FloorBHeld || tr.Slopes()["k"].FloorB != floorTrue.B {
		t.Fatalf("premise: the B is held on the key's slope; got %+v", rs[len(rs)-1])
	}
	tr.Sweep(start.Add(4*time.Hour), func(string) bool { return true })
	if _, ok := tr.Slopes()["k"]; ok {
		t.Errorf("the held B outlived the sweep: %+v", tr.Slopes()["k"])
	}
}

// A rollout or a shape move drops the floor the held B was read from, and
// the B with it: it was read under an engine or a shape the key no longer
// runs. The next pinned fit is on the default B, as without the hold.
func TestTracker_ARolloutOrShapeMoveEndsTheHeldB(t *testing.T) {
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	later := start.Add(time.Duration(180) * 15 * time.Second)
	for name, drop := range map[string]func(*Tracker){
		"shape move (ForgetFloor)": func(tr *Tracker) { tr.ForgetFloor("k") },
		"rollout (ClearWindow)":    func(tr *Tracker) { tr.ClearWindow("k") },
	} {
		tr := NewTracker()
		rs := inBandFor(tr, heldAdm, floorTrue, 160, start)
		if !rs[len(rs)-1].FloorBHeld {
			t.Fatalf("%s: premise: the B is held; got %+v", name, rs[len(rs)-1])
		}
		drop(tr)
		if got := tr.Slopes()["k"]; !got.FloorBGone || got.FloorB != floorTrue.B {
			t.Errorf("%s: exported %+v, want B %g marked gone", name, got, floorTrue.B)
		}
		var r Reading
		for c := range 12 {
			r = tr.Observe("k", "v", floorRows(bandCycle(c), floorTrue.A, floorTrue.B, start), heldAdm, later.Add(time.Duration(c)*15*time.Second))
		}
		if r.FloorBHeld || r.PinnedAtFloor || r.Model.B != DefaultBaselineSec {
			t.Errorf("%s: line %+v (held %v), want the default B", name, r.Model, r.FloorBHeld)
		}
	}
}

// A floor read this cycle that floorIntercept refuses still bounds the held
// B: a floor's ITL is an upper bound on B, so a held B over it ends.
func TestTracker_ARefusedFloorUnderTheHeldBEndsIt(t *testing.T) {
	tr := NewTracker()
	tr.slopes["k"] = heldSlope{a: 0.05, floorB: 0.012}
	pinned := []Observation{{K: 0.35, ITLSec: 0.03}}
	if _, _, ok := tr.heldFloorB("k", pinned, Observation{K: 0.2, ITLSec: 0.011}, true, heldAdm, time.Time{}); ok {
		t.Errorf("held a B of 12 ms over a floor read at 11 ms")
	}
	if b, _, ok := tr.heldFloorB("k", pinned, Observation{K: 0.2, ITLSec: 0.013}, true, heldAdm, time.Time{}); !ok || b != 0.012 {
		t.Errorf("a floor at 13 ms ended the held 12 ms (%g, %v)", b, ok)
	}
}

// One noisy reading under the held B does not end it; the readings' low
// quantile under it does.
func TestHeldFloorContradicted_ReadsAQuantile(t *testing.T) {
	xs := make([]Observation, 0, 21)
	for i := range 20 {
		xs = append(xs, Observation{K: 0.35, ITLSec: 0.03 + float64(i)*1e-4})
	}
	if heldFloorContradicted(append(xs, Observation{K: 0.35, ITLSec: 0.005}), 0.012) {
		t.Errorf("one reading under the B ended it")
	}
	low := make([]Observation, 0, 20)
	for range 20 {
		low = append(low, Observation{K: 0.35, ITLSec: 0.011})
	}
	if !heldFloorContradicted(low, 0.012) {
		t.Errorf("every reading under the B did not end it")
	}
}

// The held B ages out where a held slope does (Admission.SlopeHoldFor),
// counted from when it was read from a floor; one of unknown age is held.
func TestTracker_TheHeldBAgesOutAtTheSlopeHold(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	adm := heldAdm
	adm.SlopeHoldFor = 7 * 24 * time.Hour
	pinned := []Observation{{K: 0.35, ITLSec: 0.03}}
	for name, c := range map[string]struct {
		at   time.Time
		want bool
	}{
		"a day old":      {now.Add(-24 * time.Hour), true},
		"eight days old": {now.Add(-8 * 24 * time.Hour), false},
		"unknown age":    {time.Time{}, true},
	} {
		tr := NewTracker()
		tr.slopes["k"] = heldSlope{a: 0.05, fittedAt: now, floorB: 0.012, floorBAt: c.at}
		if _, _, ok := tr.heldFloorB("k", pinned, Observation{}, false, adm, now); ok != c.want {
			t.Errorf("%s: held %v, want %v", name, ok, c.want)
		}
	}
}
