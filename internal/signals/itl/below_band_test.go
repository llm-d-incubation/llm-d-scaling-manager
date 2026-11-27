package itl

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The floor is the BelowBandQuantile quantile, so with ten readings the
// least is passed over; with fewer than BelowBandMinReadings there is none.
func TestBelowBandFloor(t *testing.T) {
	xs := []float64{0.001, 0.012, 0.013, 0.014, 0.015, 0.016, 0.017, 0.018, 0.019, 0.020}
	if got := BelowBandFloor(xs); got != 0.012 {
		t.Errorf("floor %g, want 0.012: the outlier is passed over", got)
	}
	if got := BelowBandFloor(xs[:BelowBandMinReadings-1]); got != 0 {
		t.Errorf("floor %g on %d readings, want none", got, BelowBandMinReadings-1)
	}
	var kept []Observation
	for i := range BelowBandWindow + 5 {
		kept = keepBelowBand(kept, Observation{K: 0.05, ITLSec: float64(i)})
	}
	if len(kept) != BelowBandWindow || kept[0].ITLSec != 5 {
		t.Errorf("kept %d from %g, want the latest %d", len(kept), kept[0].ITLSec, BelowBandWindow)
	}
	p, ok := belowBandPoint([]Observation{{K: 0.01, ITLSec: 0.001}, {K: 0.02, ITLSec: 0.012}, {K: 0.03, ITLSec: 0.013},
		{K: 0.04, ITLSec: 0.014}, {K: 0.05, ITLSec: 0.015}, {K: 0.06, ITLSec: 0.016}, {K: 0.07, ITLSec: 0.017},
		{K: 0.08, ITLSec: 0.018}, {K: 0.09, ITLSec: 0.019}, {K: 0.10, ITLSec: 0.020}}, 0.15)
	if !ok || p.ITLSec != 0.012 || p.K != 0.02 {
		t.Errorf("point %+v, want 0.012 at its own fill 0.02", p)
	}
}

// floorRows is three replicas at fill k on the line ITL = a k + b, Ready an
// hour before start.
func floorRows(k, a, b float64, start time.Time) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, 3)
	for i := range 3 {
		out = append(out, domain.ReplicaMetrics{PodName: fmt.Sprintf("pod-%d", i), VariantName: "v", Ready: true,
			ReadySince: start.Add(-time.Hour), KvCacheUsage: k, KvUsageInstant: k, AvgITL: a*k + b})
	}
	return out
}

// floorTrue is the line the floor tests' engines run on: a B of 12 ms, twice
// the default.
var floorTrue = Model{A: 0.05, B: 0.012}

// floorRun observes 20 cycles at fill below, under the band, then 4 in it at
// 0.30 to 0.40, too narrow a spread for a full fit, and returns the last
// reading.
func floorRun(tr *Tracker, adm Admission, below func(c int) float64, start time.Time) Reading {
	var r Reading
	c := 0
	for ; c < 20; c++ {
		r = tr.Observe("k", "v", floorRows(below(c), floorTrue.A, floorTrue.B, start), adm, start.Add(time.Duration(c)*15*time.Second))
	}
	for i, k := range []float64{0.30, 0.35, 0.40, 0.35} {
		r = tr.Observe("k", "v", floorRows(k, floorTrue.A, floorTrue.B, start), adm, start.Add(time.Duration(c+i)*15*time.Second))
	}
	return r
}

// A key with no learned B is pinned through the floor read below the band,
// at its fill, with BaselineFromFloor: on readings of a true line, its own B,
// so the line prices the truth past its readings. Without it, as before, at
// the default B, and the fit through that B prices ITL high there. Pinned at
// the floor's ITL as if read at no fill, B would be high by A times the
// floor's fill and the line under the truth past its readings (the negative
// control below).
func TestTracker_BaselineFromFloorPinsAnUnlearnedKey(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	steadyLow := func(int) float64 { return 0.05 }
	on := floorRun(NewTracker(), Admission{BaselineFromFloor: true}, steadyLow, start)
	off := floorRun(NewTracker(), Admission{}, steadyLow, start)
	if on.Tier != TierPinnedB || off.Tier != TierPinnedB {
		t.Fatalf("premise: a pinned fit both ways; got %q on, %q off", on.Tier, off.Tier)
	}
	wantFloor := floorTrue.ITLAt(0.05)
	if !on.PinnedAtFloor || math.Abs(on.Floor-wantFloor) > 1e-12 || on.FloorK != 0.05 {
		t.Errorf("on: floor %g at %g (pinned %v), want %g at 0.05", on.Floor, on.FloorK, on.PinnedAtFloor, wantFloor)
	}
	if math.Abs(on.Model.B-floorTrue.B) > 1e-9 || math.Abs(on.Model.A-floorTrue.A) > 1e-9 {
		t.Errorf("on: line %+v, want the truth %+v", on.Model, floorTrue)
	}
	// Negative control: the fit through the floor's ITL itself.
	naive, ok := FitPinnedB(spanReadings(floorTrue, 12, 0.30, 0.40), wantFloor)
	if !ok || !(naive.ITLAt(0.8) < floorTrue.ITLAt(0.8)) {
		t.Errorf("negative control: through the floor's ITL at no fill, %+v prices %.4f s at k 0.8, want under the truth %.4f s",
			naive, naive.ITLAt(0.8), floorTrue.ITLAt(0.8))
	}
	if off.PinnedAtFloor || off.Floor != 0 || off.Model.B != DefaultBaselineSec {
		t.Errorf("off: B %g (floor %g, pinned %v), want the default", off.Model.B, off.Floor, off.PinnedAtFloor)
	}
	truth := floorTrue.ITLAt(0.8)
	onErr, offErr := on.Model.ITLAt(0.8)-truth, off.Model.ITLAt(0.8)-truth
	if math.Abs(onErr) > 1e-9 || !(offErr > 0) {
		t.Errorf("at k 0.8, truth %.4f s: on errs %+.4f s, off %+.4f s; on should be exact, off high", truth, onErr, offErr)
	}
	t.Logf("at k 0.8, truth %.4f s: through the floor %+.4f s, default B %+.4f s, the floor's ITL at no fill %+.4f s",
		truth, onErr, offErr, naive.ITLAt(0.8)-truth)
}

// A learned B ends the floor: the key pins its own.
func TestTracker_ALearnedBaselineEndsTheFloor(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	tr.SetBaseline("k", 0.02)
	r := floorRun(tr, Admission{BaselineFromFloor: true}, func(int) float64 { return 0.05 }, start)
	if r.PinnedAtFloor || r.Model.B != 0.02 {
		t.Errorf("B %g (pinned at the floor %v), want the learned 0.02", r.Model.B, r.PinnedAtFloor)
	}
}

// A reading below the band from a replica whose fill is moving is a step,
// not the floor, and is not kept (only each replica's first, which has no
// history to move against); and the switch off keeps none at all.
func TestTracker_TheFloorReadsSteadyRowsOnly(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	jumping := func(c int) float64 { return []float64{0.02, 0.14}[c%2] }
	tr := NewTracker()
	r := floorRun(tr, Admission{BaselineFromFloor: true}, jumping, start)
	if kept := len(tr.belowBand["k"].readings); r.PinnedAtFloor || r.Floor != 0 || kept > 3 {
		t.Errorf("a moving fill set the floor: %g (%d kept)", r.Floor, kept)
	}
	off := NewTracker()
	floorRun(off, Admission{}, func(int) float64 { return 0.05 }, start)
	if len(off.belowBand) != 0 {
		t.Errorf("off: %d keys kept readings below the band", len(off.belowBand))
	}
}

// A sweep forgets a key's readings below the band with its window.
func TestTracker_SweepForgetsTheFloor(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	floorRun(tr, Admission{BaselineFromFloor: true}, func(int) float64 { return 0.05 }, start)
	if tr.belowBand["k"] == nil || len(tr.belowBand["k"].readings) == 0 {
		t.Fatal("premise: readings kept")
	}
	tr.Sweep(start.Add(time.Hour), func(string) bool { return true })
	if len(tr.belowBand) != 0 {
		t.Errorf("%d keys left after the sweep", len(tr.belowBand))
	}
}

// revisionRows is floorRows under a revision, with pod names from prefix.
func revisionRows(prefix, rev string, k float64, m Model, start time.Time) []domain.ReplicaMetrics {
	rows := floorRows(k, m.A, m.B, start)
	for i := range rows {
		rows[i].PodName, rows[i].Revision = fmt.Sprintf("%s-%d", prefix, i), rev
	}
	return rows
}

// A rollout clears the floor with the window: a floor read under the old
// engine (B 12 ms) never pins the new one (B 4 ms), which it priced 27 %
// under the truth at k 0.8 (the round-1 review's repro). The new engine is
// priced as with the switch off until its own floor is read.
func TestTracker_ARolloutClearsTheFloor(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	oldE, newE := Model{A: 0.05, B: 0.012}, Model{A: 0.05, B: 0.004}
	run := func(adm Admission) (*Tracker, Reading) {
		tr := NewTracker()
		c := 0
		for ; c < 20; c++ {
			rows := tr.RolloutRows("k", "v", revisionRows("old", "r1", 0.05, oldE, start), false).Rows
			tr.Observe("k", "v", rows, adm, start.Add(time.Duration(c)*15*time.Second))
		}
		if st := tr.belowBand["k"]; adm.BaselineFromFloor && (st == nil || len(st.readings) < BelowBandMinReadings) {
			t.Fatal("premise: the old engine's floor was read")
		}
		var r Reading
		ks := []float64{0.30, 0.35, 0.40, 0.35}
		// The new pods settle for DefaultSteadyFor before their readings fit.
		for i := range 20 {
			rows := tr.RolloutRows("k", "v", revisionRows("new", "r2", ks[i%4], newE, start), false)
			if i == 0 && !rows.Moved {
				t.Fatal("premise: the revision moved")
			}
			r = tr.Observe("k", "v", rows.Rows, adm, start.Add(time.Duration(c+i)*15*time.Second))
		}
		return tr, r
	}
	tr, on := run(Admission{BaselineFromFloor: true})
	_, off := run(Admission{})
	if on.Tier == "" {
		t.Fatal("premise: the new engine's readings fit a line")
	}
	if on.PinnedAtFloor || on.Floor != 0 || tr.belowBand["k"] != nil {
		t.Errorf("the old engine's floor %.4f s survived the rollout: line %+v", on.Floor, on.Model)
	}
	if on.Model != off.Model {
		t.Errorf("the new engine's line %+v, want the switch-off line %+v", on.Model, off.Model)
	}
}

// A shape move (ClearWindow) clears the floor with the window too.
func TestTracker_AShapeMoveClearsTheFloor(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	floorRun(tr, Admission{BaselineFromFloor: true}, func(int) float64 { return 0.05 }, start)
	if tr.belowBand["k"] == nil {
		t.Fatal("premise: readings kept")
	}
	tr.ClearWindow("k")
	if _, ok := tr.belowBand["k"]; ok {
		t.Error("the floor outlived the window it was read beside")
	}
}

// A floor read longer ago than DefaultObservationMaxAge is not read: after
// it, the line is pinned at the default again until a new floor is read.
func TestTracker_TheFloorAgesOut(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	adm := Admission{BaselineFromFloor: true}
	for c := range 20 {
		tr.Observe("k", "v", floorRows(0.05, floorTrue.A, floorTrue.B, start), adm, start.Add(time.Duration(c)*15*time.Second))
	}
	later := start.Add(DefaultObservationMaxAge + 20*15*time.Second)
	var r Reading
	for i, k := range []float64{0.30, 0.35, 0.40, 0.35} {
		r = tr.Observe("k", "v", floorRows(k, floorTrue.A, floorTrue.B, start), adm, later.Add(time.Duration(i)*15*time.Second))
	}
	if r.PinnedAtFloor || r.Floor != 0 || r.Model.B != DefaultBaselineSec {
		t.Errorf("a floor %s old pinned B %g (floor %g)", DefaultObservationMaxAge, r.Model.B, r.Floor)
	}
}

// A hold pairs a slope with the B it was fitted through: a slope fitted
// through the default is held through the default though a floor is read
// now, as with the switch off, and one fitted through the floor's B through
// that B. The round-1 review's repro priced the mixed pair, the default's
// slope through the floor's B, 22 % under the truth.
func TestTracker_AHoldKeepsItsSlopesB(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	truth := Model{A: 0.05, B: 0.002}
	aDefault, _ := pinnedSlope([]Observation{{K: 0.35, ITLSec: truth.ITLAt(0.35)}}, DefaultBaselineSec)
	for _, c := range []struct {
		name   string
		held   heldSlope
		wantB  float64
		atPrev bool
	}{
		{"through the default", heldSlope{a: aDefault, fittedAt: start}, DefaultBaselineSec, false},
		{"through the floor", heldSlope{a: truth.A, fittedAt: start, floorB: truth.B}, truth.B, true},
	} {
		tr := NewTracker()
		adm := Admission{BaselineFromFloor: true}
		for i := range 12 {
			tr.belowBand["k"] = &belowBandState{readings: keepBelowBand(func() []Observation {
				if st := tr.belowBand["k"]; st != nil {
					return st.readings
				}
				return nil
			}(), Observation{K: 0.05, ITLSec: truth.ITLAt(0.05), Timestamp: start.Add(time.Duration(i) * time.Second)})}
		}
		tr.slopes["k"] = c.held
		// A pod seen first, then three that join: their readings are
		// settling, too few to fit, so the slope holds.
		tr.Observe("k", "v", revisionRows("first", "", 0.35, truth, start), adm, start)
		r := tr.Observe("k", "v", revisionRows("joined", "", 0.35, truth, start), adm, start.Add(15*time.Second))
		if !r.SlopeHeld {
			t.Fatalf("%s: premise: the slope holds; got %+v", c.name, r)
		}
		if r.Model.B != c.wantB || r.PinnedAtFloor != c.atPrev {
			t.Errorf("%s: held line %+v (pinned at the floor %v), want B %g", c.name, r.Model, r.PinnedAtFloor, c.wantB)
		}
		if mixed := (Model{A: c.held.a, B: r.Floor}); c.held.floorB == 0 && !(mixed.ITLAt(0.8) < r.Model.ITLAt(0.8)) {
			t.Errorf("%s: premise: the mixed pair %+v prices under the held line %+v", c.name, mixed, r.Model)
		}
	}
}

// A slope fitted through the floor's B is exported with that B, and a
// default-B slope without one, so a snapshot written with the switch off is
// as it was; restored, each pairs its A with its own B.
func TestTracker_AFloorSlopeIsExportedWithItsB(t *testing.T) {
	tr := NewTracker()
	tr.slopes["floor"] = heldSlope{a: 0.05, floorB: 0.012}
	tr.slopes["default"] = heldSlope{a: 0.05}
	got := tr.Slopes()
	if got["floor"].FloorB != 0.012 || got["default"].FloorB != 0 || len(got) != 2 {
		t.Errorf("exported %v, want both, the floor's with B 0.012", got)
	}
	back := NewTracker()
	for k, s := range got {
		back.RestoreSlope(k, s)
	}
	if back.slopes["floor"].floorB != 0.012 || back.slopes["default"].floorB != 0 {
		t.Errorf("restored %v", back.slopes)
	}
	back = NewTracker()
	back.RestoreSlope("bad", Slope{A: 0.05, FloorB: math.Inf(1)})
	if back.slopes["bad"].floorB != 0 {
		t.Errorf("an infinite FloorB restored as %g", back.slopes["bad"].floorB)
	}
}

// After a restart under light load a key whose slope was fitted through the
// floor's B still holds a line (the round-2 review's repro): its slope is
// exported with its B and restored paired with it. Before, it was not
// exported, and the key had no line at all.
func TestTracker_ARestoredFloorSlopeHoldsItsLine(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	truth := Model{A: 0.05, B: 0.012}
	adm := Admission{BaselineFromFloor: true, SlopeHoldFor: 3 * time.Hour}
	tr := NewTracker()
	c := 0
	for ; c < 20; c++ {
		tr.Observe("k", "v", floorRows(0.05, truth.A, truth.B, start), adm, start.Add(time.Duration(c)*15*time.Second))
	}
	var before Reading
	for i, k := range []float64{0.30, 0.35, 0.40, 0.35, 0.30, 0.35, 0.40, 0.35} {
		before = tr.Observe("k", "v", floorRows(k, truth.A, truth.B, start), adm, start.Add(time.Duration(c+i)*15*time.Second))
	}
	if !before.PinnedAtFloor {
		t.Fatalf("premise: the line was pinned through the floor; got %+v", before)
	}
	restored := NewTracker()
	for k, s := range tr.Slopes() {
		restored.RestoreSlope(k, s)
	}
	at := start.Add(time.Duration(c+8)*15*time.Second + time.Minute)
	var after Reading
	for i := range 20 {
		after = restored.Observe("k", "v", revisionRows("q", "", 0.05, truth, start), adm, at.Add(time.Duration(i)*15*time.Second))
	}
	if after.Tier == "" || !after.SlopeHeld {
		t.Fatalf("after the restart under light load: no line (%+v)", after)
	}
	if after.Model != before.Model || !after.PinnedAtFloor {
		t.Errorf("held line %+v (pinned at the floor %v), want the line before the restart %+v", after.Model, after.PinnedAtFloor, before.Model)
	}
}

// ForgetFloor drops one key's readings below the band.
func TestTracker_ForgetFloorDropsOneKey(t *testing.T) {
	tr := NewTracker()
	tr.belowBand["a"] = &belowBandState{readings: []Observation{{K: 0.05, ITLSec: 0.01}}}
	tr.belowBand["b"] = &belowBandState{readings: []Observation{{K: 0.05, ITLSec: 0.01}}}
	tr.ForgetFloor("a")
	if _, ok := tr.belowBand["a"]; ok || tr.belowBand["b"] == nil {
		t.Errorf("left %v", tr.belowBand)
	}
}

// The tier dwell compares a new pinned fit with the held line, the held
// slope through the B it was fitted through. A default-B slope against a
// pinned fit through this cycle's lower floor B: through the default, the
// held line prices at least as much, so the dwell holds; paired with this
// cycle's B instead it would price less, and the change would go through.
func TestDwellTier_ComparesTheHeldSlopeThroughItsB(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	tr.slopes["k"] = heldSlope{a: 0.05, fittedAt: now}
	tr.latches["k"] = tierLatch{tier: TierOLS, since: now, seen: now, fitted: true}
	const floorB = 0.001
	c := fitChoice{model: Model{A: 0.055, B: floorB}, tier: TierPinnedB}
	adm := Admission{TierHysteresis: true}
	got, refused := tr.dwellTier("k", c, tierInputs{baseline: floorB, minSettled: DefaultMinSettledSamples}, adm, now.Add(time.Minute))
	if !refused || !got.hold {
		t.Errorf("the change went through (%+v): the held line was paired with this cycle's floor B", got)
	}
}

// Readings kept under an older, higher band bottom are not below this band.
func TestBelowBandPoint_ReadsOnlyUnderTheBandNow(t *testing.T) {
	xs := make([]Observation, 0, 12)
	for i := range 12 {
		xs = append(xs, Observation{K: 0.10, ITLSec: 0.010 + 0.0001*float64(i)})
	}
	if _, ok := belowBandPoint(xs, 0.15); !ok {
		t.Fatal("premise: a floor under a bottom of 0.15")
	}
	if _, ok := belowBandPoint(xs, 0.08); ok {
		t.Error("readings at 0.10 read as below a bottom of 0.08")
	}
}

// The intercept is refused where its gain on the readings' error is above
// one: the mean under twice the floor's fill. A floor at 0.14 against
// readings at 0.16 turned a 0.4 ms error in their mean into 2.8 ms of B (the
// round-1 review's repro); at 0.35 it is taken.
func TestFloorIntercept_NeedsTheGap(t *testing.T) {
	line := Model{A: 0.05, B: 0.006}
	at := func(kbar, e float64) (float64, bool) {
		pinned := make([]Observation, 0, 10)
		for range 10 {
			pinned = append(pinned, Observation{K: kbar, ITLSec: line.ITLAt(kbar) + e})
		}
		return floorIntercept(pinned, Observation{K: 0.14, ITLSec: line.ITLAt(0.14)})
	}
	if _, ok := at(0.16, 0.0004); ok {
		t.Error("a mean at 0.16 against a floor at 0.14 was taken")
	}
	b, ok := at(0.35, 0.0004)
	if !ok || math.Abs(b-line.B) > 0.0004 {
		t.Errorf("at 0.35: B %g (ok %v), want within the 0.4 ms error of %g", b, ok, line.B)
	}
	if _, ok := at(2*0.14, 0); !ok {
		t.Error("a mean at exactly twice the floor's fill was refused")
	}
}

// ForgetFloors drops every key's readings below the band.
func TestTracker_ForgetFloorsDropsEveryFloor(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	floorRun(tr, Admission{BaselineFromFloor: true}, func(int) float64 { return 0.05 }, start)
	if tr.belowBand["k"] == nil {
		t.Fatal("premise: readings kept")
	}
	tr.ForgetFloors()
	if len(tr.belowBand) != 0 {
		t.Errorf("%d keys left after the forget", len(tr.belowBand))
	}
}

// A held slope pins the B it was fitted through, whatever B this cycle's
// fit would take: a tier-dwell hold (dwellTier) holds while the pinned fit
// through this cycle's floor stands, and must not pair the two.
func TestHeldSlope_PinsTheBItWasFittedThrough(t *testing.T) {
	const thisCycle = 0.004
	if got := (heldSlope{a: 0.05}).pin(thisCycle, false); got != DefaultBaselineSec {
		t.Errorf("a slope fitted through the default pins %g, want the default", got)
	}
	if got := (heldSlope{a: 0.05, floorB: 0.003}).pin(thisCycle, false); got != 0.003 {
		t.Errorf("a slope fitted through a floor's B pins %g, want 0.003", got)
	}
	if got := (heldSlope{a: 0.05, floorB: 0.003}).pin(0.02, true); got != 0.02 {
		t.Errorf("with a learned B, %g, want the learned 0.02", got)
	}
}

// byCountReadings is n pairs of readings below the band, one at itlA on a
// replica running seqA sequences and one at itlB on one running seqB.
func byCountReadings(n int, itlA, itlB, seqA, seqB float64) ([]Observation, []float64) {
	obs := make([]Observation, 0, 2*n)
	seqs := make([]float64, 0, 2*n)
	for range n {
		obs = append(obs, Observation{K: 0.05, ITLSec: itlA}, Observation{K: 0.05, ITLSec: itlB})
		seqs = append(seqs, seqA, seqB)
	}
	return obs, seqs
}

// The floor is kept by the sequences it was read at (issue
// llm-d-incubation/llm-d-scaling-manager#286): readings at 4 sequences of
// 10 ms and at 40 of 20 ms are two groups, each with its floor, and the line
// through them is the floor by count; the fewest sequences read is 4. A count
// not read on one reading leaves the floor standing for every count, a single
// group is no line, and too few readings is no floor.
func TestBelowBandByCount(t *testing.T) {
	obs, seqs := byCountReadings(22, 0.010, 0.020, 4, 40)
	least, byCount := BelowBandByCount(obs, seqs)
	if least != 4 {
		t.Errorf("least %g, want 4", least)
	}
	wantA := 0.010 / 36
	if math.Abs(byCount.A-wantA) > 1e-12 || math.Abs(byCount.B-(0.010-4*wantA)) > 1e-12 {
		t.Errorf("by count %+v, want the line through (4, 10 ms) and (40, 20 ms)", byCount)
	}

	seqs[20] = 0
	if least, byCount := BelowBandByCount(obs, seqs); least != 0 || !byCount.IsZero() {
		t.Errorf("a count not read: %g %+v, want none", least, byCount)
	}
	seqs[20] = math.Inf(1)
	if least, byCount := BelowBandByCount(obs, seqs); least != 0 || !byCount.IsZero() {
		t.Errorf("an infinite count: %g %+v, want none", least, byCount)
	}
	if least, byCount := BelowBandByCount(obs, seqs[:len(seqs)-1]); least != 0 || !byCount.IsZero() {
		t.Errorf("a count short: %g %+v, want none", least, byCount)
	}

	obs, seqs = byCountReadings(22, 0.010, 0.011, 5, 6)
	if least, byCount := BelowBandByCount(obs, seqs); least != 5 || !byCount.IsZero() {
		t.Errorf("one group: %g %+v, want 5 and no line", least, byCount)
	}

	obs, seqs = byCountReadings(BelowBandMinReadings/2-1, 0.010, 0.020, 4, 40)
	if least, byCount := BelowBandByCount(obs, seqs); least != 0 || !byCount.IsZero() {
		t.Errorf("no floor: %g %+v, want none", least, byCount)
	}
}
