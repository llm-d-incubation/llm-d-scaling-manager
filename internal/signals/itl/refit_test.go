package itl

import (
	"testing"
	"time"
)

// spanReadings is n readings on the line, one per cycle, their k evenly
// from lo to hi.
func spanReadings(line Model, n int, lo, hi float64) []Observation {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	out := make([]Observation, 0, n)
	for i := range n {
		k := lo + (hi-lo)*float64(i)/float64(n-1)
		out = append(out, Observation{K: k, ITLSec: line.ITLAt(k) * (1 + 0.01*float64(i%3-1)), Timestamp: t0.Add(time.Duration(i) * 15 * time.Second)})
	}
	return out
}

// Refit takes the full fit only on steady readings the steady window calls
// ready: DefaultMinSamples of them, spread DefaultMinKSpread. Below the
// spread it is the pinned fit, at the baseline, though Fit alone would take
// the readings.
func TestRefitTakesTheFullFitOnlyWhenReady(t *testing.T) {
	line := Model{A: 0.004, B: 0.0015}
	wide := spanReadings(line, 12, 0.2, 0.7)
	if got := Refit(wide, wide, line.B, 0, Admission{}); got.Tier != TierOLS || len(got.Read) != len(wide) {
		t.Errorf("spread 0.5: tier %q on %d readings, want ols on all %d", got.Tier, len(got.Read), len(wide))
	}
	narrow := spanReadings(line, 12, 0.4, 0.65)
	if _, ok := Fit(narrow); !ok {
		t.Fatal("the full fit refuses the narrow readings; the case tests nothing")
	}
	got := Refit(narrow, narrow, line.B, 0, Admission{})
	if got.Tier != TierPinnedB || got.Model.B != line.B {
		t.Errorf("spread 0.25: %+v, want the pinned fit at B %g", got, line.B)
	}
	few := spanReadings(line, DefaultMinSamples-1, 0.2, 0.7)
	if got := Refit(few, few, line.B, 0, Admission{}); got.Tier != "" || !got.Hold {
		t.Errorf("%d readings: %+v, want a hold", len(few), got)
	}
}

// The pinned fit reads pinnedReadings: every reading of the main window but
// those of a settling replica, when too few are steady.
func TestRefitPinnedReadsWhatTheTrackerReads(t *testing.T) {
	line := Model{A: 0.004, B: 0.0015}
	obs := spanReadings(line, 12, 0.4, 0.5)
	obs[0].settling, obs[1].settling = true, true
	got := Refit(obs, nil, line.B, 0, Admission{})
	if got.Tier != TierPinnedB || len(got.Read) != 10 {
		t.Fatalf("%+v, want the pinned fit on the 10 settled readings", got)
	}
	for _, o := range got.Read {
		if o.settling {
			t.Errorf("read a settling reading %+v", o)
		}
	}
	want, _ := pinnedReadings(obs, nil)
	if m, _ := FitPinnedB(want, line.B); m != got.Model {
		t.Errorf("refit %+v, the tracker's pinned fit %+v", got.Model, m)
	}
}

// The pinned fit's floor on settled readings: one under minSettled holds
// the slope, minSettled itself fits.
func TestRefitSettledFloor(t *testing.T) {
	line := Model{A: 0.004, B: 0.0015}
	for _, minSettled := range []int{DefaultMinSettledSamples, 5} {
		for _, settled := range []int{minSettled - 1, minSettled} {
			obs := spanReadings(line, DefaultMinSamples, 0.4, 0.5)
			for i := range len(obs) - settled {
				obs[i].settling = true
			}
			got := Refit(obs, nil, line.B, minSettled, Admission{})
			wantFit := settled >= minSettled
			if (got.Tier == TierPinnedB) != wantFit || got.Hold == wantFit {
				t.Errorf("minSettled %d, %d settled: %+v, want a fit %v", minSettled, settled, got, wantFit)
			}
		}
	}
}

// RefitAboveBand is the above-band fallback's own fit, on its own readings.
func TestRefitAboveBand(t *testing.T) {
	line := Model{A: 0.05, B: 0.01}
	above := spanReadings(line, 12, 0.86, 0.95)
	got := RefitAboveBand(above, nil, 0)
	m, read, ok := aboveBandFit(above, nil, DefaultMinSettledSamples)
	if !ok || got.Tier != TierPinnedB || got.Model != m || len(got.Read) != len(read) {
		t.Errorf("%+v, want the fallback's %+v on %d readings", got, m, len(read))
	}
	if got := RefitAboveBand(above[:3], nil, 0); got.Tier != "" {
		t.Errorf("three readings: %+v, want none", got)
	}
}
