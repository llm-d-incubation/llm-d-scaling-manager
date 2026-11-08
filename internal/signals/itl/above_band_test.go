package itl

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// aboveRows is three Ready replicas of variant v, Ready since an hour before
// start, each at k on the line 0.05 k + 0.010.
func aboveRows(k float64, start time.Time) []domain.ReplicaMetrics {
	const n = 3
	out := make([]domain.ReplicaMetrics, 0, n)
	for i := range n {
		out = append(out, domain.ReplicaMetrics{PodName: fmt.Sprintf("pod-%d", i), VariantName: "v", Ready: true,
			ReadySince: start.Add(-time.Hour), KvCacheUsage: k, KvUsageInstant: k, AvgITL: 0.05*k + 0.010})
	}
	return out
}

// With AboveBandFallback, a key that never fitted and whose replicas all sit
// above the band fits from those readings once there are DefaultMinSamples,
// and its A is not remembered as the slope a later hold holds. Without it,
// as before, the key has no line. The first fit in the band replaces it.
func TestTracker_AboveBandFallbackFitsAKeyThatNeverFitted(t *testing.T) {
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	run := func(fallback bool) (first int, last Reading, tr *Tracker) {
		tr = NewTracker()
		adm := Admission{AboveBandFallback: fallback}
		first = -1
		for c := range 8 {
			last = tr.Observe("k", "v", aboveRows(0.90, start), adm, start.Add(time.Duration(c)*15*time.Second))
			if last.Tier != "" && first < 0 {
				first = c
			}
		}
		return first, last, tr
	}
	first, last, tr := run(true)
	if first != 3 {
		t.Fatalf("the fallback's first line on cycle %d, want 3 (the tenth reading, three replicas a cycle)", first)
	}
	if !last.AboveBandFit || last.Tier != TierPinnedB || last.AboveBandHeld != min(24, DefaultWindowMaxSize) || last.Held != 0 {
		t.Fatalf("not the fallback's line: %+v", last)
	}
	// Every reading the same ITL, 55 ms: B pinned at it, a flat line there.
	if want := (Model{A: 0, B: 0.05*0.90 + 0.010}); math.Abs(last.Model.A-want.A) > 1e-12 || math.Abs(last.Model.B-want.B) > 1e-12 {
		t.Errorf("line %+v, want %+v", last.Model, want)
	}
	if _, held := tr.Slopes()["k"]; held {
		t.Error("the fallback's A was remembered as the held slope")
	}
	if _, stored := tr.Baseline("k"); stored {
		t.Error("the fallback set the intercept")
	}

	if first, _, _ := run(false); first >= 0 {
		t.Errorf("negative control: without the fallback the key had a line on cycle %d", first)
	}

	// The band fits: its line replaces the fallback's at once.
	now := start.Add(8 * 15 * time.Second)
	adm := Admission{AboveBandFallback: true}
	var r Reading
	for c := range 4 {
		r = tr.Observe("k", "v", aboveRows(0.40, start), adm, now.Add(time.Duration(c)*15*time.Second))
	}
	if r.AboveBandFit || r.Tier != TierPinnedB {
		t.Fatalf("the band's readings did not replace the fallback: %+v", r)
	}
}

// The above-band window does not wait after a controller restart
// (aboveBandSettling): with WaitsAfterRestart, the fallback still prices on
// the tenth reading, cycle 3, on the key's own trace and on the settle
// tracker. A replica that became Ready less than steadyFor ago is still kept
// out by its Ready time: Ready 15 s before the restart, its readings settle
// until cycle 7, and the fallback prices there.
func TestTracker_AboveBandFallbackDoesNotWaitAfterARestart(t *testing.T) {
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	const cycle = 15 * time.Second
	first := func(tracked bool, ready time.Time) int {
		tr := NewTracker()
		st := settle.NewTracker()
		st.SetInterval(cycle)
		adm := Admission{AboveBandFallback: true, WaitsAfterRestart: true}
		if tracked {
			adm.Settle = &Settle{View: st, Model: "ns/m"}
		}
		for c := range 16 {
			now := start.Add(time.Duration(c) * cycle)
			rows := aboveRows(0.90, start)
			for i := range rows {
				rows[i].ReadySince = ready
			}
			st.Observe("ns/m", settlerow.Of(rows, nil), now)
			if tr.Observe("k", "v", rows, adm, now).Tier != "" {
				return c
			}
		}
		return -1
	}
	for _, tracked := range []bool{false, true} {
		if got := first(tracked, start.Add(-time.Hour)); got != 3 {
			t.Errorf("tracked %v: the fallback's first line on cycle %d, want 3: it waited the restart", tracked, got)
		}
		if got := first(tracked, start.Add(-cycle)); got != 7 {
			t.Errorf("tracked %v: replicas Ready 15 s before the restart: the fallback's first line on cycle %d, want 7", tracked, got)
		}
	}
}

// The fallback never replaces a line the band gives or a slope the key may
// hold: a key that fitted in the band and then went above it keeps fitting
// from the band's old readings until they age out, then holds the last A
// (SlopeHeld), and only once the hold ends (SlopeHoldFor) does the fallback
// price it.
func TestTracker_AboveBandFallbackNeverReplacesAHeldSlope(t *testing.T) {
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	adm := Admission{AboveBandFallback: true, SlopeHoldFor: 40 * time.Minute}
	now := start
	for range 6 {
		tr.Observe("k", "v", aboveRows(0.40, start), adm, now)
		now = now.Add(15 * time.Second)
	}
	reasons := map[string]int{}
	var lastBandFit time.Time
	for now.Before(start.Add(80 * time.Minute)) {
		r := tr.Observe("k", "v", aboveRows(0.90, start), adm, now)
		switch {
		case r.AboveBandFit:
			reasons["above"]++
			if lastBandFit.IsZero() || now.Sub(lastBandFit) <= adm.SlopeHoldFor {
				t.Fatalf("the fallback priced %s after the band's last fit, inside the %s hold", now.Sub(lastBandFit), adm.SlopeHoldFor)
			}
		case r.SlopeHeld:
			reasons["held"]++
		case r.Tier == "":
			reasons["none"]++
		default:
			reasons["band"]++
			lastBandFit = now
		}
		now = now.Add(15 * time.Second)
	}
	if reasons["band"] == 0 || reasons["held"] == 0 || reasons["above"] == 0 || reasons["none"] != 0 {
		t.Fatalf("want the band's fits, then the held slope, then the fallback, and never no line: %v", reasons)
	}
}

// ForgetSlope drops the key's held slope and keeps its windows and
// intercept: with it, a key whose window was cleared has no slope to hold.
func TestTracker_ForgetSlopeDropsOnlyTheSlope(t *testing.T) {
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	for c := range 6 {
		tr.Observe("k", "v", aboveRows(0.40, start), Admission{}, start.Add(time.Duration(c)*15*time.Second))
	}
	tr.SetBaseline("k", 0.007)
	tr.ForgetSlope("k")
	if _, ok := tr.Slopes()["k"]; ok {
		t.Error("the slope was kept")
	}
	if n := tr.WindowLen("k"); n != 18 {
		t.Errorf("the window holds %d readings, want 18", n)
	}
	if b, ok := tr.Baseline("k"); !ok || b != 0.007 {
		t.Errorf("the intercept was dropped: %v %v", b, ok)
	}
}

// The fallback is one-sided: B pinned at the least ITL, readings at k of at
// least DefaultKSat, it prices at DefaultKSat at least the true line's ITL,
// whatever the true B. Negative control: pinned at DefaultBaselineSec, as
// the band's fit pins a key that never fitted, it prices under a true line
// whose B is 10 ms.
func TestFitAboveBand_NeverPricesUnderTheTrueLine(t *testing.T) {
	const a = 0.05
	ks := []float64{0.86, 0.90, 0.94}
	noise := []float64{-0.02, 0.01, 0.02, -0.01, 0}
	for _, b := range []float64{0.004, 0.010} {
		const readings = 15
		obs := make([]Observation, 0, readings)
		for i := range readings {
			k := ks[i%len(ks)]
			obs = append(obs, Observation{K: k, ITLSec: (a*k + b) * (1 + noise[i%len(noise)])})
		}
		truth := a*DefaultKSat + b
		m, ok := fitAboveBand(obs, aboveBandB(obs))
		if !ok {
			t.Fatalf("true B %g: no line", b)
		}
		if p := m.ITLAt(DefaultKSat); p < truth {
			t.Errorf("true B %g: priced %.5f s, under the truth %.5f s", b, p, truth)
		}
		if b > DefaultBaselineSec {
			d, ok := fitAboveBand(obs, DefaultBaselineSec)
			if !ok || d.ITLAt(DefaultKSat) >= truth {
				t.Errorf("negative control: pinned at the default, true B %g priced %.5f s, not under the truth %.5f s",
					b, d.ITLAt(DefaultKSat), truth)
			}
		}
	}
}

// A reading above the band but under DefaultKSat is not kept for the
// fallback: between the band's top and the price, a B pinned above the true
// one would price under the truth.
func TestTracker_AboveBandFallbackKeepsOnlyReadingsAtThePrice(t *testing.T) {
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	var r Reading
	for c := range 6 {
		r = tr.Observe("k", "v", aboveRows(0.82, start), Admission{AboveBandFallback: true}, start.Add(time.Duration(c)*15*time.Second))
	}
	if r.AboveBand != 3 || r.AboveBandHeld != 0 || r.Tier != "" {
		t.Fatalf("readings at k 0.82 were kept for the fallback: %+v", r)
	}
}
