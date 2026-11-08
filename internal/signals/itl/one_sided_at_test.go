package itl

import (
	"math"
	"testing"
	"time"
)

// Issue ev-shindin/llm-scaling-manager#467: the one-sided price at the cap with a stated prior on B
// (Admission.BPrior), judged where the line is priced: at a fill
// (Admission.OneSidedAt, the saturation line's pricing k) and at a target
// ITL (Admission.OneSidedTarget, the slo line's TPOT target).

// capCycles is how many cycles capWindow runs: past DefaultMinSamples
// readings, four of each fill.
const capCycles = 12

// capWindow is capCycles cycles of three replicas at their cap, on line ITL
// = a k + b, at fills cycling over kCap - 0.01, kCap and kCap + 0.01; noise,
// when set, scales each reading by (1 + noise(c, i)).
func capWindow(tr *Tracker, adm Admission, a, b, kCap float64, noise func(c, i int) float64) Reading {
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var r Reading
	for c := range capCycles {
		k := []float64{kCap - 0.01, kCap, kCap + 0.01}[c%3]
		rows := cappedRows(k, a, b, start)
		if noise != nil {
			for i := range rows {
				rows[i].AvgITL *= 1 + noise(c, i)
			}
		}
		r = tr.Observe("k", "v", rows, adm, start.Add(time.Duration(c)*15*time.Second))
	}
	return r
}

// priorGrid is issue ev-shindin/llm-scaling-manager#467's bias grid, B at or under DefaultBPrior.
var priorGrid = []struct{ a, b, kCap float64 }{
	{0.05, 0.010, 0.97}, {0.05, 0.030, 0.97}, {0.01, 0.020, 0.97}, {0.01, 0.030, 0.97},
	{0.05, 0.010, 0.92}, {0.01, 0.030, 0.92},
}

// boundAt is the most the prior's line can price at k, worked by hand from
// the readings the pinned fit read (pinnedReadings): through the mean at-cap
// point (kbar, ITLbar) with intercept the uncapped prior, DefaultBPrior +
// max(0, ITLbar - DefaultBPrior) k / kbar. With the slope above zero it is
// the truth plus (BPrior - B)(1 - k/kbar); floored at zero it is the prior
// itself. The window's cap (priorCap) only lowers the intercept toward B,
// which can only tighten it, so the rule is held to at most this, not to it.
func boundAt(tr *Tracker, k float64) float64 {
	rs := tr.Readings("k")
	read, _ := pinnedReadings(rs.All, rs.Steady)
	var sumK, sumITL float64
	n := 0
	for _, o := range read {
		if atOrPastKSat(o.K) {
			sumK, sumITL, n = sumK+o.K, sumITL+o.ITLSec, n+1
		}
	}
	kbar, itlbar := sumK/float64(n), sumITL/float64(n)
	return DefaultBPrior + max(0, itlbar-DefaultBPrior)*k/kbar
}

// atPrice is an admission priced one-sided at the default pricing k (0.68)
// with the prior DefaultBPrior.
func atPrice() Admission {
	return Admission{MaxNumSeqs: 64, OneSidedAboveKSat: true, OneSidedAt: 0.68, BPrior: DefaultBPrior}
}

// The saturation line's case, at the pricing k (0.68). Off, through the
// default B, the price is under the truth by the derived term (B0 - B)(1 -
// k/kbar). On, with the prior at DefaultBPrior, at or above the truth for
// every B at or under the prior, over by exactly the derived bound
// (boundAt): (BPrior - B)(1 - k/kbar), kbar the at-cap readings' mean fill,
// or, where their mean ITL is under the prior and the slope floors at 0, the
// prior itself; less where the window's cap lowers the prior. The truth
// exactly where B is the prior. The line returned passes through the default
// B with A above zero.
func TestBPrior_ThePricingKIsAtOrAboveTheTruthAndBounded(t *testing.T) {
	const kPrice = 0.68
	for _, g := range priorGrid {
		truth := g.a*kPrice + g.b
		off := capWindow(NewTracker(), Admission{MaxNumSeqs: 64}, g.a, g.b, g.kCap, nil)
		tr := NewTracker()
		on := capWindow(tr, atPrice(), g.a, g.b, g.kCap, nil)
		atK := on.Model.ITLAt(kPrice)
		errOff, errOn := 100*(off.Model.ITLAt(kPrice)-truth)/truth, 100*(atK-truth)/truth
		// kbar is the mean fill of the readings the pinned fit took: the
		// steady ones, whose mix of the three fills is near even, not even.
		want := 100 * (DefaultBaselineSec - g.b) * (1 - kPrice/g.kCap) / truth
		bound := 100 * (boundAt(tr, kPrice) - truth) / truth
		t.Logf("A %.2f B %.0f ms k_cap %.2f: off %+.2f%% (derived %+.2f%%), on %+.2f%% (bound %+.2f%%)",
			g.a, 1000*g.b, g.kCap, errOff, want, errOn, bound)
		if math.Abs(errOff-want) > 0.1 || !(errOff < 0) {
			t.Errorf("negative control: off priced %+.3f%%, want the derived term %+.3f%%, under the truth", errOff, want)
		}
		if !on.PinnedAtBPrior || on.PinnedAtLeastITL || atK < truth*(1-1e-12) {
			t.Errorf("on: priced %.5f s against the truth %.5f s (at the prior %v)", atK, truth, on.PinnedAtBPrior)
		}
		if errOn > bound+1e-9 {
			t.Errorf("on: over by %+.4f%%, past the bound %+.4f%%", errOn, bound)
		}
		if on.Model.B != DefaultBaselineSec || !(on.Model.A > 0) {
			t.Errorf("on: the line %+v, want one through the default B with A above zero", on.Model)
		}
	}
}

// A true B above the prior is under-priced, by at most (B - BPrior)(1 -
// k/kbar): bounded, and less than the default B alone. The cap does not
// apply (every reading is above the prior), so the price is the bound.
func TestBPrior_ATrueBAboveThePriorIsUnderByTheBound(t *testing.T) {
	const kPrice, a, b, kCap = 0.68, 0.01, 0.040, 0.97
	truth := a*kPrice + b
	tr := NewTracker()
	on := capWindow(tr, atPrice(), a, b, kCap, nil)
	off := capWindow(NewTracker(), Admission{MaxNumSeqs: 64}, a, b, kCap, nil)
	errOn := 100 * (on.Model.ITLAt(kPrice) - truth) / truth
	errOff := 100 * (off.Model.ITLAt(kPrice) - truth) / truth
	bound := 100 * (boundAt(tr, kPrice) - truth) / truth
	t.Logf("B 40 ms: on %+.2f%% (bound %+.2f%%), off %+.2f%%", errOn, bound, errOff)
	if math.Abs(errOn-bound) > 1e-9 || !(errOn < 0) || !(errOn > errOff) {
		// The window's cap is the least ITL, at least B + A k: above the
		// 30 ms prior, so it does not bind and the price is the bound.
		t.Errorf("on %+.4f%%, want the bound %+.4f%%, under the truth and over the default B's %+.4f%%", errOn, bound, errOff)
	}
}

// With the gauge's noise (2 %, a fixed pattern of signs) the price at the
// pricing k is within known truth's 5 % on every case.
func TestBPrior_WithNoiseItIsWithinEpsilon(t *testing.T) {
	const kPrice = 0.68
	noise := func(c, i int) float64 { return 0.02 * float64((c*7+i*3)%5-2) / 2 }
	for _, g := range priorGrid {
		truth := g.a*kPrice + g.b
		on := capWindow(NewTracker(), atPrice(), g.a, g.b, g.kCap, noise)
		if p := on.Model.ITLAt(kPrice); p < truth/1.05 {
			t.Errorf("A %.2f B %.0f ms k_cap %.2f: priced %.5f s, under the truth %.5f s past epsilon", g.a, 1000*g.b, g.kCap, p, truth)
		}
	}
}

// The slo line's case: priced at the fill where the line meets its TPOT
// target, a lower fill being more replicas. On, the fill is at or under the
// true line's for every B at or under the prior, and inside the band
// wherever the prior's line meets the target there; where the prior's line
// is past the target at the band's bottom (its error over B larger than the
// target's headroom) it abstains. Off, it is above the truth's.
func TestBPrior_ATargetPricesAFillAtOrUnderTheTruths(t *testing.T) {
	for _, g := range priorGrid {
		// A target the true line meets at 0.5, inside the band.
		target := g.a*0.5 + g.b
		adm := Admission{MaxNumSeqs: 64, OneSidedAboveKSat: true, OneSidedTarget: target, BPrior: DefaultBPrior}
		lo := capWindow(NewTracker(), Admission{MaxNumSeqs: 64}, g.a, g.b, g.kCap, nil)
		on := capWindow(NewTracker(), adm, g.a, g.b, g.kCap, nil)
		offFill, onFill := adm.pricedFill(lo.Model), adm.pricedFill(on.Model)
		t.Logf("A %.2f B %.0f ms k_cap %.2f: fill truth 0.5000, off %.4f, on %.4f", g.a, 1000*g.b, g.kCap, offFill, onFill)
		if !(offFill > 0.5) {
			t.Errorf("negative control: off priced the fill %.4f, want above the truth's 0.5", offFill)
		}
		if !on.PinnedAtBPrior || onFill > 0.5+1e-12 {
			t.Errorf("on: priced the fill %.4f, above the truth's 0.5 (at the prior %v)", onFill, on.PinnedAtBPrior)
		}
		// Abstains (the band's bottom) only where the prior's line itself is
		// past the target there: the prior's error is larger than the
		// target's headroom over B.
		bandLo, _ := adm.band()
		tr := NewTracker()
		capWindow(tr, Admission{MaxNumSeqs: 64}, g.a, g.b, g.kCap, nil)
		rs := tr.Readings("k")
		read, _ := pinnedReadings(rs.All, rs.Steady)
		p, _, _ := priorLine(read, DefaultBPrior, adm.priceAnchor(lo.Model))
		if feasible := p.ITLAt(bandLo) < target; feasible != (onFill > bandLo) {
			t.Errorf("on: fill %.4f, but the prior's line is %.5f s at the band's bottom against the target %.5f s",
				onFill, p.ITLAt(bandLo), target)
		}
	}
}

// The held A is the A of the line returned, so a later hold, through the
// default B, prices what the line at the prior did at the pricing k.
func TestBPrior_TheHeldSlopeKeepsThePrice(t *testing.T) {
	const kPrice = 0.68
	tr := NewTracker()
	on := capWindow(tr, atPrice(), 0.05, 0.010, 0.97, nil)
	if !on.PinnedAtBPrior {
		t.Fatal("premise: not priced at the prior")
	}
	held := Model{A: tr.Slopes()["k"].A, B: DefaultBaselineSec}
	if math.Abs(held.ITLAt(kPrice)-on.Model.ITLAt(kPrice)) > 1e-15 {
		t.Errorf("held line %.6f s at k %.2f, the line at the prior %.6f s", held.ITLAt(kPrice), kPrice, on.Model.ITLAt(kPrice))
	}
}

// The rule compares the two lines where the line is priced (OneSidedAt),
// not at kSat. A window with a replica in the band (k 0.75, too close to the
// cap for a full fit) beside the capped ones steepens the fit through the
// default B until it crosses the prior's line between the pricing k and
// kSat; the price at the pricing k must be the higher of the two there.
func TestBPrior_TheLinesAreComparedAtThePricingK(t *testing.T) {
	const kPrice = 0.68
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	crossed := 0
	for _, bandITL := range []float64{0.050, 0.053, 0.056, 0.059, 0.062, 0.065} {
		run := func(adm Admission) (*Tracker, Reading) {
			tr := NewTracker()
			var r Reading
			for c := range 6 {
				rows := cappedRows(0.97, 0, 0.050, start)
				rows[2].KvCacheUsage, rows[2].KvUsageInstant, rows[2].AvgITL = 0.75, 0.75, bandITL
				r = tr.Observe("k", "v", rows, adm, start.Add(time.Duration(c)*15*time.Second))
			}
			return tr, r
		}
		tr, _ := run(Admission{MaxNumSeqs: 64})
		rs := tr.Readings("k")
		m := Refit(rs.All, rs.Steady, DefaultBaselineSec, 0, Admission{}).Model
		p, _, ok := priorLine(rs.All, DefaultBPrior, kPrice)
		if !ok || m.IsZero() {
			t.Fatalf("band ITL %.3f: premise: no lines (%+v, %v)", bandITL, m, ok)
		}
		if (m.ITLAt(kPrice) > p.ITLAt(kPrice)) == (m.ITLAt(DefaultKSat) > p.ITLAt(DefaultKSat)) {
			continue
		}
		crossed++
		_, on := run(atPrice())
		want := max(m.ITLAt(kPrice), p.ITLAt(kPrice))
		t.Logf("band ITL %.3f: default-B fit %.5f s, prior's line %.5f s at k %.2f; priced %.5f s",
			bandITL, m.ITLAt(kPrice), p.ITLAt(kPrice), kPrice, on.Model.ITLAt(kPrice))
		if math.Abs(on.Model.ITLAt(kPrice)-want) > 1e-12 {
			t.Errorf("band ITL %.3f: priced %.6f s at k %.2f, want the higher line's %.6f s", bandITL, on.Model.ITLAt(kPrice), kPrice, want)
		}
	}
	if crossed == 0 {
		t.Fatal("premise: no window had the two lines cross between the pricing k and kSat")
	}
}

// The prior's line passes through the mean of the at-cap readings at or
// past the fill priced, its intercept the prior capped at the window's least
// ITL at any k; readings under kSat set the cap but not the anchor. With
// fewer than three at or past the fill, the three of highest k are the
// anchor; with the priced fill past even their mean, beyond is set.
func TestBPrior_ThePriorLineIsThroughTheAnchor(t *testing.T) {
	obs := []Observation{{K: 0.95, ITLSec: 0.060}, {K: 0.90, ITLSec: 0.054}}
	m, beyond, ok := priorLine(obs, 0.030, 0.68)
	if !ok || beyond || math.Abs(m.A-(0.057-0.030)/0.925) > 1e-12 || m.B != 0.030 {
		t.Errorf("%+v (beyond %v), want B 0.030 through the mean (0.925, 0.057)", m, beyond)
	}
	// A reading under kSat at 0.010 s caps the prior at it, and is not in
	// the anchor.
	capped := append([]Observation{{K: 0.20, ITLSec: 0.010}}, obs...)
	if m, _, _ := priorLine(capped, 0.030, 0.68); m.B != 0.010 || math.Abs(m.A-(0.057-0.010)/0.925) > 1e-12 {
		t.Errorf("capped: %+v, want B 0.010 through the mean (0.925, 0.057)", m)
	}
	if m, _, _ := priorLine([]Observation{{K: 0.90, ITLSec: 0.020}}, 0.030, 0.68); m.A != 0 || m.B != 0.020 {
		t.Errorf("a mean under the prior: %+v, want flat at the cap 0.020", m)
	}
	if _, _, ok := priorLine([]Observation{{K: 0.80, ITLSec: 0.05}}, 0.030, 0.68); ok {
		t.Error("a line from readings under kSat alone")
	}
	// Priced at 0.93: three readings at or past it are the anchor.
	five := []Observation{{K: 0.86, ITLSec: 0.050}, {K: 0.88, ITLSec: 0.052}, {K: 0.93, ITLSec: 0.057},
		{K: 0.95, ITLSec: 0.059}, {K: 0.97, ITLSec: 0.061}}
	if m, beyond, _ := priorLine(five, 0.030, 0.93); beyond || math.Abs(m.A-(0.059-0.030)/0.95) > 1e-12 {
		t.Errorf("at 0.93: %+v (beyond %v), want through the mean of the three past it (0.95, 0.059)", m, beyond)
	}
	// Priced at 0.96: one reading past it; the three of highest k, whose
	// mean (0.95) is under 0.96, so beyond.
	if m, beyond, _ := priorLine(five, 0.030, 0.96); !beyond || math.Abs(m.A-(0.059-0.030)/0.95) > 1e-12 {
		t.Errorf("at 0.96: %+v (beyond %v), want the top three's mean and beyond", m, beyond)
	}
}

// Priced above the mean at-cap fill (here at 0.95, against readings at 0.85
// to 0.97), the line is anchored at the readings at or past it, so it is not
// under the truth for B at or under the prior, without noise. Anchored at the
// mean of all of them (kbar about 0.91) it would be: (b - B)(1 - 0.95/0.91)
// under. The true B (3 ms) is under the default 6 ms, so the fit through the
// default B is under the truth past kbar too, by (B0 - B)(1 - k/kbar), and
// does not cover it; with B over the default, that fit is over the truth
// past kbar, and the higher of the two is not under either way.
func TestBPrior_PricedAboveTheMeanFillIsNotUnder(t *testing.T) {
	const kPrice, a, b = 0.95, 0.01, 0.003
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	adm := Admission{MaxNumSeqs: 64, OneSidedAboveKSat: true, OneSidedAt: kPrice, BPrior: DefaultBPrior}
	tr := NewTracker()
	var on Reading
	ks := []float64{0.85, 0.87, 0.89, 0.91, 0.93, 0.95, 0.96, 0.97}
	for c := range 12 {
		rows := cappedRows(ks[c%len(ks)], a, b, start)
		on = tr.Observe("k", "v", rows, adm, start.Add(time.Duration(c)*15*time.Second))
	}
	truth := a*kPrice + b
	// The line through the mean of every at-cap reading, for contrast.
	rs := tr.Readings("k")
	read, _ := pinnedReadings(rs.All, rs.Steady)
	var sumK, sumITL float64
	for _, o := range read {
		sumK, sumITL = sumK+o.K, sumITL+o.ITLSec
	}
	kbar, itlbar := sumK/float64(len(read)), sumITL/float64(len(read))
	b0 := priorCap(read, DefaultBPrior)
	mean := Model{A: (itlbar - b0) / kbar, B: b0}
	t.Logf("priced at %.2f: truth %.5f s, on %.5f s, through every reading's mean (kbar %.3f) %.5f s",
		kPrice, truth, on.Model.ITLAt(kPrice), kbar, mean.ITLAt(kPrice))
	if !on.PinnedAtBPrior || on.Model.ITLAt(kPrice) < truth*(1-1e-12) {
		t.Errorf("on: priced %.5f s, under the truth %.5f s (at the prior %v)", on.Model.ITLAt(kPrice), truth, on.PinnedAtBPrior)
	}
	if on.PriorBeyondReadings {
		t.Error("flagged beyond the readings with readings at or past the priced fill")
	}
	if !(mean.ITLAt(kPrice) < truth) {
		t.Errorf("premise: the mean of every reading priced %.5f s, not under the truth %.5f s", mean.ITLAt(kPrice), truth)
	}
	if fit := Refit(rs.All, rs.Steady, DefaultBaselineSec, 0, Admission{}).Model; !(fit.ITLAt(kPrice) < truth) {
		t.Errorf("premise: the fit through the default B priced %.5f s, not under the truth %.5f s", fit.ITLAt(kPrice), truth)
	}
}

// Priced past every reading's fill, the line is flagged
// PriorBeyondReadings: its bound no longer holds there. Past the readings
// the fit through the default B is the higher line whenever the true B is
// over the default (its error, (B0 - B)(1 - k/kbar), turns positive past
// kbar), so the prior's line is chosen there only against a weaker pinned
// fit: here one of A 0.01, as a fit taught mostly in the band might be.
func TestBPrior_PricedPastEveryReadingIsFlagged(t *testing.T) {
	read := make([]Observation, 0, 3)
	for _, k := range []float64{0.91, 0.92, 0.93} {
		read = append(read, Observation{K: k, ITLSec: 0.05*k + 0.010})
	}
	c := fitChoice{model: Model{A: 0.01, B: DefaultBaselineSec}, tier: TierPinnedB, read: read}
	past := Admission{OneSidedAboveKSat: true, OneSidedAt: 0.99, BPrior: DefaultBPrior}
	m, _, raised, beyond := oneSidedLine(c, DefaultBaselineSec, past)
	if !raised || !beyond || m.B != DefaultBaselineSec {
		t.Errorf("priced at 0.99, past readings at 0.91 to 0.93: %+v raised %v beyond %v, want raised and beyond", m, raised, beyond)
	}
	// Through Observe and Refit, the flag reaches the Reading and the
	// Refitted. Two replicas at the cap at 0.92 beside one in the band at
	// 0.65 (close enough for no full fit), on a line flatter there than at
	// the cap: the pinned fit through the default B, read over all three, is
	// shallower than the prior's line through the cap, which is chosen at
	// 0.99, past every reading.
	adm := Admission{MaxNumSeqs: 64, OneSidedAboveKSat: true, OneSidedAt: 0.99, BPrior: DefaultBPrior}
	tr := NewTracker()
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var on Reading
	for c := range 12 {
		rows := cappedRows(0.92, 0.05, 0.010, start)
		rows[2].KvCacheUsage, rows[2].KvUsageInstant, rows[2].AvgITL = 0.65, 0.65, 0.012
		on = tr.Observe("k", "v", rows, adm, start.Add(time.Duration(c)*15*time.Second))
	}
	if !on.PinnedAtBPrior || !on.PriorBeyondReadings {
		t.Errorf("Observe at 0.99: at the prior %v, beyond %v, want both", on.PinnedAtBPrior, on.PriorBeyondReadings)
	}
	rs := tr.Readings("k")
	if got := Refit(rs.All, rs.Steady, DefaultBaselineSec, 0, adm); !got.PriorBeyondReadings {
		t.Errorf("Refit at 0.99: beyond %v, want it", got.PriorBeyondReadings)
	}
	under := Admission{OneSidedAboveKSat: true, OneSidedAt: 0.68, BPrior: DefaultBPrior}
	if _, _, raised, beyond := oneSidedLine(c, DefaultBaselineSec, under); !raised || beyond {
		t.Errorf("priced at 0.68, under every reading: raised %v beyond %v, want raised and not beyond", raised, beyond)
	}
}

// An admission that names no price (OneSidedTarget 0, as the slo line's with
// no TPOT target, or OneSidedAt out of range) still returns the line through
// the default B when it states a prior: never the prior's own line, which can
// be flat (one reading, or the cap's ITL at every reading).
func TestBPrior_NoPriceStillReturnsALineThroughTheDefaultB(t *testing.T) {
	for name, adm := range map[string]Admission{
		"no target":           {MaxNumSeqs: 64, OneSidedAboveKSat: true, BPrior: DefaultBPrior},
		"OneSidedAt past 1":   {MaxNumSeqs: 64, OneSidedAboveKSat: true, OneSidedAt: 1.5, BPrior: DefaultBPrior},
		"OneSidedAt negative": {MaxNumSeqs: 64, OneSidedAboveKSat: true, OneSidedAt: -0.5, BPrior: DefaultBPrior},
	} {
		t.Run(name, func(t *testing.T) {
			if adm.namesItsPrice() {
				t.Fatal("premise: the admission names its price")
			}
			// Every ITL at the cap the same: the prior's line is flat.
			on := capWindow(NewTracker(), adm, 0, 0.040, 0.95, nil)
			if !on.PinnedAtBPrior {
				t.Fatalf("premise: not raised (%+v)", on.Model)
			}
			if on.Model.B != DefaultBaselineSec || !ValidModel(on.Model.A, on.Model.B) {
				t.Errorf("the line %+v, want one through the default B that ValidModel takes", on.Model)
			}
		})
	}
}

// One at-cap reading, and one that is also the least (so the prior's line
// is flat at it, A 0): the line returned is a valid one through the default
// B, which whatif.PriceTPOT and ValidModel take.
func TestBPrior_OneReadingGivesAValidLine(t *testing.T) {
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	{
		t.Run("one at-cap reading", func(t *testing.T) {
			tr := NewTracker()
			var on Reading
			for c := range 12 {
				rs := cappedRows(0.95, 0.05, 0.010, start)
				// Two replicas in the band, at 0.40 and 0.45; one at the cap.
				rs[0].KvCacheUsage, rs[0].KvUsageInstant, rs[0].AvgITL = 0.40, 0.40, 0.05*0.40+0.010
				rs[1].KvCacheUsage, rs[1].KvUsageInstant, rs[1].AvgITL = 0.45, 0.45, 0.05*0.45+0.010
				if c > 0 {
					// Only the first cycle's at-cap reading is kept at the cap.
					rs[2].KvCacheUsage, rs[2].KvUsageInstant, rs[2].AvgITL = 0.50, 0.50, 0.05*0.50+0.010
				}
				on = tr.Observe("k", "v", rs, atPrice(), start.Add(time.Duration(c)*15*time.Second))
			}
			if on.Tier == "" || !ValidModel(on.Model.A, on.Model.B) {
				t.Errorf("the line %+v, want a valid one", on.Model)
			}
		})
	}
	t.Run("the one reading is the least", func(t *testing.T) {
		obs := []Observation{{K: 0.90, ITLSec: 0.040}}
		m, _, ok := priorLine(obs, DefaultBPrior, 0.68)
		if !ok || math.Abs(m.A-(0.040-0.030)/0.90) > 1e-15 || m.B != 0.030 {
			t.Fatalf("prior line %+v, want B 0.030 through the one reading", m)
		}
		flat, _, _ := priorLine(obs, 0.050, 0.68)
		if flat.A != 0 || flat.B != 0.040 {
			t.Fatalf("a prior above the one reading: %+v, want flat at its ITL (the cap)", flat)
		}
		c := fitChoice{model: Model{A: 0.01, B: DefaultBaselineSec}, tier: TierPinnedB, read: obs}
		adm := atPrice()
		adm.BPrior = 0.050
		got, heldA, raised, _ := oneSidedLine(c, DefaultBaselineSec, adm)
		if !raised || got.B != DefaultBaselineSec || !ValidModel(got.A, got.B) || got.A != heldA {
			t.Errorf("returned %+v (held A %g, raised %v), want a valid line through the default B", got, heldA, raised)
		}
	})
}

// BPrior is DefaultBPrior, raised by a learned B above it, never lowered.
func TestBPrior_ALearnedBOnlyRaisesThePrior(t *testing.T) {
	if got := BPrior(); got != DefaultBPrior {
		t.Errorf("none learned: %g, want %g", got, DefaultBPrior)
	}
	if got := BPrior(0.003, 0.008, 0.015); got != DefaultBPrior {
		t.Errorf("small learned Bs: %g, want the floor %g", got, DefaultBPrior)
	}
	if got := BPrior(0.004, 0.045, math.NaN(), -1, math.Inf(1)); got != 0.045 {
		t.Errorf("a learned B above the floor: %g, want 0.045", got)
	}
}

// An outlier prior (an 80 ms fit on the same physics key) is capped at the
// window's least ITL, which no reading puts under B: the price is bounded by
// the cap, and still at or above the truth.
func TestBPrior_TheWindowCapsAnOutlierPrior(t *testing.T) {
	const kPrice, a, b, kCap = 0.68, 0.01, 0.020, 0.97
	truth := a*kPrice + b
	adm := atPrice()
	adm.BPrior = 0.080
	tr := NewTracker()
	on := capWindow(tr, adm, a, b, kCap, nil)
	rs := tr.Readings("k")
	read, _ := pinnedReadings(rs.All, rs.Steady)
	least, sumK, sumITL, n := math.Inf(1), 0.0, 0.0, 0
	for _, o := range read {
		least = min(least, o.ITLSec)
		if atOrPastKSat(o.K) {
			sumK, sumITL, n = sumK+o.K, sumITL+o.ITLSec, n+1
		}
	}
	// The prior's line at the cap, by hand: intercept the least ITL, through
	// the mean at-cap point. Uncapped, the 80 ms intercept would price 80 ms.
	kbar, itlbar := sumK/float64(n), sumITL/float64(n)
	bound := least + max(0, itlbar-least)*kPrice/kbar
	p := on.Model.ITLAt(kPrice)
	t.Logf("outlier 80 ms: priced %.5f s, truth %.5f s, capped bound %.5f s (cap %.5f s)", p, truth, bound, least)
	if p < truth*(1-1e-12) || math.Abs(p-bound) > 1e-12 || !(p < 0.080) {
		t.Errorf("priced %.5f s, want the capped bound %.5f s, at or above the truth %.5f s", p, bound, truth)
	}
	// The over-price is the cap's error over B, not the outlier's.
	if over, uncapped := (p-truth)/truth, (0.080-truth)/truth; !(over < uncapped/10) {
		t.Errorf("over-priced by %.2f%%, uncapped would be %.2f%%: the cap did not bound it", 100*over, 100*uncapped)
	}
}

// The scope reads only keys of the same model, accelerator, GPUs, engine
// arguments and role, the variant's own key always.
func TestBPrior_TheScopeIsThePhysicsKey(t *testing.T) {
	args := map[string]string{
		"ns|m|v1|H200|8": "a1", "ns|m|v2|H200|8": "a1", "ns|m|v3|H200|8": "a2",
		"ns|m|v4|H100|8": "a1", "ns|m|v5|H200|4": "a1", "ns|m2|v6|H200|8": "a1", "ns|m|p1|H200|8": "a1",
		// The namespace alone differs: the same variant name, model,
		// accelerator, GPUs, arguments and role.
		"ns2|m|v2|H200|8": "a1",
	}
	roles := map[string]string{"v1": "decode", "v2": "decode", "v3": "decode", "v4": "decode", "v5": "decode",
		"v6": "decode", "p1": "prefill"}
	s := NewPriorScope("ns|m|v1|H200|8", "a1",
		func(k string) string { return args[k] }, func(v string) string { return roles[v] })
	for key, want := range map[string]bool{
		"ns|m|v1|H200|8": true, "ns|m|v2|H200|8": true,
		"ns|m|v3|H200|8": false, "ns|m|v4|H100|8": false, "ns|m|v5|H200|4": false,
		"ns|m2|v6|H200|8": false, "ns|m|p1|H200|8": false, "ns2|m|v2|H200|8": false, "bad": false,
	} {
		if got := s.In(key); got != want {
			t.Errorf("%s: in scope %v, want %v", key, got, want)
		}
	}
	if NewPriorScope("ns|m|v1|H200|8", "", nil, nil).In("ns|m|v2|H200|8") {
		t.Error("no arguments known: a sibling is in scope")
	}
	if (PriorScope{}).In("ns|m|v1|H200|8") {
		t.Error("the zero scope holds a key")
	}
	tr := NewTracker()
	tr.SetBaseline("ns|m|v2|H200|8", 0.040)
	tr.SetBaseline("ns|m|v4|H100|8", 0.090)
	if got := tr.ScopedBaselines(s); len(got) != 1 || got[0] != 0.040 {
		t.Errorf("scoped baselines %v, want [0.040]", got)
	}
}

// Refit, given the admission's one-sided fields, prices the line Observe
// priced; without them, the fit through the default B alone. The what-if
// bands refit with it (whatif.Refit.OneSided), or price under the line.
func TestRefit_OneSidedPricesTheLineObserveDid(t *testing.T) {
	for _, adm := range []Admission{
		{MaxNumSeqs: 64, OneSidedAboveKSat: true},
		atPrice(),
		{MaxNumSeqs: 64, OneSidedAboveKSat: true, OneSidedTarget: 0.01*0.5 + 0.020, BPrior: DefaultBPrior},
	} {
		tr := NewTracker()
		obs := capWindow(tr, adm, 0.01, 0.020, 0.97, nil)
		rs := tr.Readings("k")
		got := Refit(rs.All, rs.Steady, DefaultBaselineSec, 0, adm)
		raised := got.PinnedAtLeastITL || got.PinnedAtBPrior
		if !raised || got.Model != obs.Model || got.PinnedAtBPrior != obs.PinnedAtBPrior {
			t.Errorf("%+v: refit %+v (%v %v), Observe %+v", adm, got.Model, got.PinnedAtLeastITL, got.PinnedAtBPrior, obs.Model)
		}
		plain := Refit(rs.All, rs.Steady, DefaultBaselineSec, 0, Admission{})
		if plain.PinnedAtLeastITL || plain.PinnedAtBPrior || plain.Model == obs.Model {
			t.Errorf("negative control: refit without the rule gave the one-sided line %+v", plain.Model)
		}
	}
}
