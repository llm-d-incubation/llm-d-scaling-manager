package itl

import (
	"fmt"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

const (
	latchA, latchB = 0.03, 0.0025
	latchKPrice    = 0.68
	latchKey       = "k"
)

var latchStart = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// latchRig drives one key. A pod is named for its k, so a k it saw on the
// first cycle stays steady, and a spread falls by leaving pods out.
type latchRig struct {
	tr   *Tracker
	adm  Admission
	a, b float64
}

func newLatchRig() *latchRig {
	return &latchRig{tr: NewTracker(), adm: Admission{TierHysteresis: true, OneSidedAt: latchKPrice}, a: latchA, b: latchB}
}

// rows is one row per k, every ITL scale times the true line's. A scale of
// zero offers no ITL: the pod is seen, and steady from then on, with no reading.
func (l *latchRig) rows(ks []float64, scale float64) []domain.ReplicaMetrics {
	rows := make([]domain.ReplicaMetrics, 0, len(ks))
	for _, k := range ks {
		rows = append(rows, domain.ReplicaMetrics{PodName: fmt.Sprintf("p%.3f", k), VariantName: "v", Ready: true,
			KvUsageInstant: k, AvgITL: scale * (l.a*k + l.b)})
	}
	return rows
}

// see offers one cycle at ks.
func (l *latchRig) see(at time.Duration, ks []float64) Reading {
	return l.tr.Observe(latchKey, "v", l.rows(ks, 1), l.adm, latchStart.Add(at))
}

// latchPods is the pods of every test with a k in [lo, hi]: 24 of them, from
// 0.40 every 0.015. A cycle's rows must be ten for any fit.
func latchPods(lo, hi float64) []float64 {
	var ks []float64
	for i := range 24 {
		if k := 0.40 + 0.015*float64(i); k >= lo-1e-9 && k <= hi+1e-9 {
			ks = append(ks, k)
		}
	}
	return ks
}

func latchFleet() []float64 { return latchPods(0, 1) }

// narrow is ten pods whose spread is far under the clear value.
func narrow() []float64 { return latchPods(0.55, 0.685) }

// atOLS is a rig whose key stands at the full fit, since latchStart.
func atOLS(t *testing.T) *latchRig {
	t.Helper()
	l := newLatchRig()
	if r := l.see(0, latchFleet()); r.Tier != TierOLS {
		t.Fatalf("the fleet's first cycle fitted %q, want the full fit", r.Tier)
	}
	return l
}

// after is one cycle whose windows hold exactly ks, read a cycle earlier: the window a key at the full fit has aged to, with the evidence it
// stands on. ClearWindow would be a forget, which unlatches.
func (l *latchRig) after(at time.Duration, ks []float64, scale float64) Reading {
	l.tr.mu.Lock()
	for _, w := range []*Window{l.tr.windows[latchKey], l.tr.steady[latchKey]} {
		w.Clear()
		for _, k := range ks {
			w.Add(k, scale*(l.a*k+l.b), latchStart.Add(at-15*time.Second))
		}
	}
	l.tr.mu.Unlock()
	return l.see(at, nil)
}

// Inside the clear values a key at the full fit stays on it, where the set
// values alone would leave it: the window here is Ready at neither.
func TestDwellTier_ClearValuesKeepTheFullFit(t *testing.T) {
	spread := atOLS(t)
	r := spread.after(15*time.Second, latchPods(0.46, 0.73), 1)
	if r.Ready || r.Tier != TierOLS || r.TierHeld {
		t.Fatalf("spread %.2f: Ready %v tier %q held %v, want the full fit kept", r.SteadySpread, r.Ready, r.Tier, r.TierHeld)
	}

	rows := atOLS(t)
	r = rows.after(15*time.Second, []float64{0.40, 0.46, 0.52, 0.58, 0.64, 0.73, 0.70}, 1)
	if r.SteadyHeld != 7 || r.Tier == TierOLS {
		t.Fatalf("under the clear rows: %d rows tier %q", r.SteadyHeld, r.Tier)
	}
	rows = atOLS(t)
	r = rows.after(15*time.Second, []float64{0.40, 0.46, 0.52, 0.58, 0.64, 0.73, 0.70, 0.55}, 1)
	if r.SteadyHeld != 8 || r.Ready || r.Tier != TierOLS {
		t.Fatalf("at the clear rows: %d rows, Ready %v tier %q, want the full fit kept", r.SteadyHeld, r.Ready, r.Tier)
	}
}

// A spread that collapses is past any clear value: inside the dwell the key's
// line is the full fit's, unchanged, and the dwell ends the hold.
func TestDwellTier_ACollapsedSpreadHoldsTheLineForTheDwell(t *testing.T) {
	l := atOLS(t)
	var want Model
	{
		probe := atOLS(t)
		b, _ := probe.tr.Baseline(latchKey)
		want = Model{A: probe.tr.Slopes()[latchKey].A, B: b}
	}
	for at := 15 * time.Second; at < DefaultTierDwell; at += 15 * time.Second {
		r := l.after(at, narrow(), 1)
		if !r.TierHeld || !r.SlopeHeld || r.Tier != TierPinnedB || r.Model != want {
			t.Fatalf("at %v: held %v slope %v tier %q line %+v, want %+v", at, r.TierHeld, r.SlopeHeld, r.Tier, r.Model, want)
		}
	}
	r := l.after(DefaultTierDwell, narrow(), 1)
	if r.TierHeld || r.SlopeHeld || r.Tier != TierPinnedB {
		t.Fatalf("at the dwell: held %v slope %v tier %q, want the pinned fit", r.TierHeld, r.SlopeHeld, r.Tier)
	}
	if got := l.tr.latches[latchKey]; got.tier != TierPinnedB || !got.since.Equal(latchStart.Add(DefaultTierDwell)) {
		t.Fatalf("the latch after the dwell: %+v", got)
	}
}

// Without the admission the same cycles change tier at once: the control on
// the test above.
func TestDwellTier_OffChoosesEachCycleAlone(t *testing.T) {
	l := atOLS(t)
	l.adm.TierHysteresis = false
	r := l.after(15*time.Second, narrow(), 1)
	if r.TierHeld || r.SlopeHeld || r.Tier != TierPinnedB {
		t.Fatalf("off: held %v slope %v tier %q", r.TierHeld, r.SlopeHeld, r.Tier)
	}
}

// A line that prices the margin more changes the tier inside the dwell, and
// one just under it waits: how far a hold can price under what it refused.
func TestDwellTier_OnlyTheMarginGoesThroughTheDwell(t *testing.T) {
	for _, tc := range []struct {
		scale float64
		held  bool
	}{{0.90, true}, {1.00, true}, {1.029, true}, {1.031, false}, {1.20, false}} {
		l := atOLS(t)
		r := l.after(15*time.Second, latchPods(0.61, 0.745), tc.scale)
		if r.TierHeld != tc.held {
			t.Errorf("readings at %.3fx the line: held %v, want %v", tc.scale, r.TierHeld, tc.held)
		}
		if !tc.held && (r.Tier != TierPinnedB || r.SlopeHeld) {
			t.Errorf("readings at %.3fx the line: tier %q slope held %v, want the pinned fit", tc.scale, r.Tier, r.SlopeHeld)
		}
	}
}

// A key on the pinned fit, B learned, stays there inside the dwell when the
// full fit would not price the margin more, and the refused fit writes no
// intercept. One that prices the margin more goes at once.
func TestDwellTier_AFullFitThatPricesLessWaits(t *testing.T) {
	setup := func(a, b, learned float64, pods []float64) *latchRig {
		l := newLatchRig()
		l.a, l.b = a, b
		l.tr.SetBaseline(latchKey, learned)
		// Seen with no reading, then read: the pods are steady at once.
		l.tr.Observe(latchKey, "v", l.rows(pods, 0), l.adm, latchStart)
		l.tr.mu.Lock()
		l.tr.latches[latchKey] = tierLatch{tier: TierPinnedB, since: latchStart}
		l.tr.mu.Unlock()
		return l
	}
	wide := latchPods(0, 1)
	// A pinned B under the true one: the pinned fit is steeper past the pods.
	l := setup(0.02, 0.012, 0.002, wide)
	r := l.see(15*time.Second, wide)
	if r.Tier != TierPinnedB || !r.TierHeld || !r.Ready {
		t.Fatalf("a full fit pricing less: tier %q held %v Ready %v", r.Tier, r.TierHeld, r.Ready)
	}
	if b, _ := l.tr.Baseline(latchKey); b != 0.002 {
		t.Fatalf("a refused full fit wrote its intercept: %v", b)
	}
	r = l.see(DefaultTierDwell, wide)
	if r.Tier != TierOLS || r.TierHeld {
		t.Fatalf("at the dwell: tier %q held %v, want the full fit", r.Tier, r.TierHeld)
	}

	// A pinned B over the true one: the pinned fit prices less.
	l = setup(0.03, 0.0025, 0.012, wide)
	r = l.see(15*time.Second, wide)
	if r.Tier != TierOLS || r.TierHeld {
		t.Fatalf("a full fit pricing more: tier %q held %v, want it at once", r.Tier, r.TierHeld)
	}
}

// No dwell delays a key's first full fit: with no B learned the pinned fit
// rests on a constant, and only the full fit can replace it.
func TestDwellTier_TheFirstFullFitDoesNotWait(t *testing.T) {
	l := newLatchRig()
	l.a, l.b = 0.02, 0.012
	wide := latchPods(0, 1)
	l.tr.Observe(latchKey, "v", l.rows(wide, 0), l.adm, latchStart)
	l.tr.mu.Lock()
	l.tr.latches[latchKey] = tierLatch{tier: TierPinnedB, since: latchStart}
	l.tr.mu.Unlock()
	if r := l.see(15*time.Second, wide); r.Tier != TierOLS || r.TierHeld {
		t.Fatalf("the first full fit: tier %q held %v", r.Tier, r.TierHeld)
	}
}

// The latch is not persisted: a restored key stands at the pinned tier, so
// the first full fit after a restart prices no less than the pinned fit
// unless it waits out the dwell (analyzer-learner.md, 3.5).
func TestDwellTier_ARestoredKeyStartsOnTheSafeSide(t *testing.T) {
	l := newLatchRig()
	l.tr.RestoreBaseline(latchKey, latchB)
	l.tr.RestoreSlope(latchKey, Slope{A: latchA * 1.5, FittedAt: latchStart})
	r := l.see(0, latchFleet())
	if r.Tier != TierPinnedB || !r.TierHeld {
		t.Fatalf("a restored key's first cycle: tier %q held %v, want the pinned fit", r.Tier, r.TierHeld)
	}
	if got := l.tr.latches[latchKey]; got.tier != TierPinnedB {
		t.Fatalf("the latch: %+v", got)
	}
	// With nothing restored there is no side to err to: a cold key takes the
	// full fit on its first cycle.
	if r := newLatchRig().see(0, latchFleet()); r.Tier != TierOLS {
		t.Fatalf("a cold key's first cycle: %q", r.Tier)
	}
}

// A slope the admission may not hold is no reason to refuse: other engine
// arguments, or one past its age, end the hold as they do on a short window.
func TestDwellTier_ASlopeItMayNotHoldEndsTheDwell(t *testing.T) {
	args := atOLS(t)
	args.tr.mu.Lock()
	s := args.tr.slopes[latchKey]
	s.args = "before"
	args.tr.slopes[latchKey] = s
	args.tr.mu.Unlock()
	args.adm.Args = "after"
	if r := args.after(15*time.Second, narrow(), 1); r.TierHeld || r.SlopeHeld {
		t.Fatalf("other arguments: held %v slope %v", r.TierHeld, r.SlopeHeld)
	}
	age := atOLS(t)
	age.adm.SlopeHoldFor = time.Minute
	if r := age.after(2*time.Minute, narrow(), 1); r.TierHeld || r.SlopeHeld {
		t.Fatalf("a slope past its age: held %v slope %v", r.TierHeld, r.SlopeHeld)
	}
}

// A key swept or forgotten loses its latch, and a refit of a latched key
// judges the steady readings by the clear values.
func TestDwellTier_TheSweepDropsTheLatchAndARefitReadsIt(t *testing.T) {
	l := atOLS(t)
	if !l.tr.Readings(latchKey).OLSLatched {
		t.Fatal("a key at the full fit reads no latch")
	}
	l.tr.Sweep(latchStart, func(string) bool { return true })
	if n := len(l.tr.latches); n != 0 {
		t.Fatalf("%d latches left after the sweep", n)
	}

	pods := latchPods(0.46, 0.73)
	steady := make([]Observation, 0, len(pods))
	for _, k := range pods {
		steady = append(steady, Observation{K: k, ITLSec: latchA*k + latchB, Timestamp: latchStart})
	}
	if got := Refit(steady, steady, latchB, 0, Admission{}); got.Tier != TierPinnedB {
		t.Fatalf("no latch: refit %q", got.Tier)
	}
	if got := Refit(steady, steady, latchB, 0, Admission{OLSLatched: true}); got.Tier != TierOLS {
		t.Fatalf("latched: refit %q", got.Tier)
	}
}

// A pinned line is judged as Observe would take it: under the one-sided rule
// the higher line, which here prices the margin over the held slope and goes
// through. Judged by the fit through the default B, it would have been held.
func TestDwellTier_APinnedLineIsPricedOneSided(t *testing.T) {
	l := newLatchRig()
	l.a, l.b = 0.01, 0.08
	l.adm.MaxK, l.adm.OneSidedAboveKSat = 0.95, true
	const heldA = (0.08 - DefaultBaselineSec) / latchKPrice
	l.tr.RestoreSlope(latchKey, Slope{A: heldA, FittedAt: latchStart})
	l.tr.mu.Lock()
	l.tr.latches[latchKey] = tierLatch{tier: TierOLS, since: latchStart, seen: latchStart}
	l.tr.mu.Unlock()
	ks := make([]float64, 12)
	for i := range ks {
		ks[i] = 0.86 + 0.002*float64(i)
	}
	r := l.see(15*time.Second, ks)
	if r.TierHeld || !r.PinnedAtLeastITL {
		t.Fatalf("held %v, one-sided %v, line %+v: want the one-sided line through the dwell", r.TierHeld, r.PinnedAtLeastITL, r.Model)
	}
}

// A target can clamp both lines to the band's top, where their fills agree and
// the replica count still goes as the ITL there: the margin is held on the
// rate, not the fill. Readings at 1.5x the line clamp to the same 0.80.
func TestDwellTier_AClampedFillStillComparesTheRate(t *testing.T) {
	for _, tc := range []struct {
		scale float64
		held  bool
	}{{1.02, true}, {1.05, false}, {1.5, false}} {
		l := atOLS(t)
		l.adm.OneSidedAt, l.adm.OneSidedTarget = 0, 1
		r := l.after(15*time.Second, latchPods(0.61, 0.745), tc.scale)
		if got := l.adm.pricedFill(r.Model); got != DefaultMaxObservableK {
			t.Fatalf("the fill did not clamp: %v", got)
		}
		if r.TierHeld != tc.held {
			t.Errorf("readings at %.3fx at a clamped fill: held %v, want %v", tc.scale, r.TierHeld, tc.held)
		}
	}
}

// The batch cap lowers the fill a target prices and caps the sequences, as
// slo's ReplicaTokenRate does: the rate falls with the ITL at that fill.
func TestAdmission_PricedRateReadsTheBatchCap(t *testing.T) {
	adm := Admission{OneSidedTarget: 1, MaxK: 0.80,
		TierPrice: TierPrice{CapacityTokens: 1000, KVPerRequest: 10, MaxSeqs: 50}}
	line := Model{A: 0.03, B: 0.0025}
	want := 50 / line.ITLAt(0.50)
	if got := adm.PricedRate(line); got != want {
		t.Fatalf("priced rate %v, want %v at the cap's fill 0.50", got, want)
	}
}

// The latch stands on its evidence: after any of these, spread 0.20 on 8 rows
// (inside the clear values) earns nothing, and a new B is not written.
func TestDwellTier_TheLatchGoesWithItsEvidence(t *testing.T) {
	inside := []float64{0.46, 0.49, 0.52, 0.55, 0.58, 0.61, 0.64, 0.66}
	// A fleet past the steady window's size holds one cycle of readings: that
	// is not lost evidence.
	wide := atOLS(t)
	many := make([]float64, 0, 48)
	for i := range 48 {
		many = append(many, 0.40+0.007*float64(i))
	}
	for i := 1; i <= 3; i++ {
		wide.see(time.Duration(i)*15*time.Second, many)
	}
	if got := wide.tr.latches[latchKey].tier; got != TierOLS {
		t.Errorf("48 rows a cycle: the latch stands at %q", got)
	}
	for name, drop := range map[string]func(l *latchRig){
		"ClearWindow":     func(l *latchRig) { l.tr.ClearWindow(latchKey) },
		"ForgetSlope":     func(l *latchRig) { l.tr.ForgetSlope(latchKey) },
		"ForgetSlopes":    func(l *latchRig) { l.tr.ForgetSlopes() },
		"ForgetBaselines": func(l *latchRig) { l.tr.ForgetBaselines() },
	} {
		l := atOLS(t)
		drop(l)
		if r := l.after(15*time.Second, inside, 1); r.Tier == TierOLS {
			t.Errorf("%s: the full fit was kept on the clear values", name)
		}
		if got := l.tr.latches[latchKey].tier; got != TierPinnedB {
			t.Errorf("%s: the latch stands at %q", name, got)
		}
	}
	// Past the carry with no steady reading: only this cycle's remain.
	l := atOLS(t)
	l.tr.mu.Lock()
	l.tr.windows[latchKey].Clear()
	l.tr.steady[latchKey].Clear()
	l.tr.mu.Unlock()
	if r := l.see(2*time.Hour, inside); r.Tier == TierOLS {
		t.Error("a window that aged out kept the full fit on the clear values")
	}
	if got := l.tr.latches[latchKey].tier; got != TierPinnedB {
		t.Errorf("aged out: the latch stands at %q", got)
	}
	// An empty window goes the same way, with no row to offer.
	l = atOLS(t)
	l.tr.mu.Lock()
	l.tr.windows[latchKey].Clear()
	l.tr.steady[latchKey].Clear()
	l.tr.mu.Unlock()
	l.see(15*time.Second, nil)
	if got := l.tr.latches[latchKey].tier; got != TierPinnedB {
		t.Errorf("emptied: the latch stands at %q", got)
	}
}

// Resting between the clear and set values, a pinned fit that prices the
// margin more takes the line at once, inside the dwell; one that does not
// leaves the full fit where it is. Either way a fit at that spread teaches
// neither B nor the held slope.
func TestDwellTier_RestingLeavesForAHigherPinnedFit(t *testing.T) {
	l := atOLS(t)
	l.a, l.b = 0.02, 0.012
	r := l.after(15*time.Second, latchPods(0.46, 0.67), 1)
	if r.Ready || r.Tier != TierPinnedB || r.TierHeld || r.SlopeHeld {
		t.Fatalf("Ready %v tier %q held %v slope %v, want the pinned fit", r.Ready, r.Tier, r.TierHeld, r.SlopeHeld)
	}
	if got := l.tr.latches[latchKey]; got.tier != TierPinnedB || !got.since.Equal(latchStart.Add(15*time.Second)) {
		t.Fatalf("the latch: %+v", got)
	}
	l = atOLS(t)
	b, _ := l.tr.Baseline(latchKey)
	a := l.tr.Slopes()[latchKey].A
	l.a, l.b = 0.0295, 0.0028
	if r := l.after(15*time.Second, latchPods(0.46, 0.67), 1); r.Tier != TierOLS {
		t.Fatalf("near the line: tier %q, want the full fit kept", r.Tier)
	}
	if got, _ := l.tr.Baseline(latchKey); got != b {
		t.Errorf("a resting fit moved B: %v, was %v", got, b)
	}
	if got := l.tr.Slopes()[latchKey].A; got != a {
		t.Errorf("a resting fit moved the held slope: %v, was %v", got, a)
	}
}

// A key that had a full fit with B at or under zero remembers no B, and still
// waits out the dwell: it is past its first full fit.
func TestDwellTier_AFullFitWithNoPositiveBStillDwells(t *testing.T) {
	ks := make([]float64, 12)
	for i := range ks {
		ks[i] = 0.49 + 0.028*float64(i)
	}
	l := newLatchRig()
	l.a, l.b = 0.06, -0.008
	if r := l.see(0, ks); r.Tier != TierOLS {
		t.Fatalf("the first cycle fitted %q", r.Tier)
	}
	if _, ok := l.tr.Baseline(latchKey); ok {
		t.Fatal("a B under zero was remembered")
	}
	l.tr.mu.Lock()
	got := l.tr.latches[latchKey]
	got.tier = TierPinnedB
	l.tr.latches[latchKey] = got
	l.tr.mu.Unlock()
	if r := l.after(15*time.Second, ks, 1); r.Tier != TierPinnedB || !r.TierHeld {
		t.Fatalf("tier %q held %v, want the full fit refused", r.Tier, r.TierHeld)
	}
}

// A forget takes the key back to before its first full fit: with no B and no
// fit behind it, the next full fit does not wait, though it prices less.
func TestDwellTier_AForgetRestoresTheFirstFullFit(t *testing.T) {
	l := atOLS(t)
	l.tr.ForgetBaselines()
	l.a, l.b = 0.02, 0.012
	if r := l.after(15*time.Second, latchFleet(), 1); r.Tier != TierOLS || r.TierHeld {
		t.Fatalf("tier %q held %v, want the full fit at once", r.Tier, r.TierHeld)
	}
}
