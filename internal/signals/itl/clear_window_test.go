package itl

import (
	"testing"
	"time"
)

// ClearWindow drops a key's readings, the steady ones too, and keeps its
// intercept and its held slope, so the next fit is made from new readings
// and the slope holds meanwhile. It reports whether there was anything to
// drop, and leaves other keys alone.
func TestTracker_ClearWindowKeepsTheInterceptAndTheSlope(t *testing.T) {
	tr := NewTracker()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	tr.windows["k"] = NewWindow(DefaultWindowMaxSize, DefaultObservationMaxAge, DefaultMinSamples, DefaultMinKSpread, 0, 1)
	tr.steady["k"] = NewWindow(DefaultSteadyWindowMaxSize, DefaultObservationMaxAge, DefaultMinSamples, DefaultMinKSpread, 0, 1)
	tr.windows["other"] = NewWindow(DefaultWindowMaxSize, DefaultObservationMaxAge, DefaultMinSamples, DefaultMinKSpread, 0, 1)
	for i := range 5 {
		tr.windows["k"].Add(0.2+0.1*float64(i), 0.02, now)
		tr.steady["k"].Add(0.2+0.1*float64(i), 0.02, now)
		tr.windows["other"].Add(0.3, 0.02, now)
	}
	tr.SetBaseline("k", 0.006)
	tr.RestoreSlope("k", Slope{A: 0.03, FittedAt: now})

	if !tr.ClearWindow("k") {
		t.Fatal("ClearWindow reported nothing dropped from a window of five readings")
	}
	if n := tr.WindowLen("k"); n != 0 {
		t.Errorf("the window kept %d readings", n)
	}
	if n := tr.steady["k"].Len(); n != 0 {
		t.Errorf("the steady window kept %d readings", n)
	}
	if b, ok := tr.Baseline("k"); !ok || b != 0.006 {
		t.Errorf("the intercept was dropped: %v %v", b, ok)
	}
	if s, ok := tr.Slopes()["k"]; !ok || s.A != 0.03 {
		t.Errorf("the held slope was dropped: %+v %v", s, ok)
	}
	if n := tr.WindowLen("other"); n != 5 {
		t.Errorf("another key's window lost readings: %d", n)
	}
	if tr.ClearWindow("k") {
		t.Error("a second ClearWindow reported readings dropped")
	}
	if tr.ClearWindow("missing") {
		t.Error("ClearWindow of a key with no window reported readings dropped")
	}
}
