package itl

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

func TestMoved(t *testing.T) {
	nan := math.NaN()
	for _, c := range []struct {
		now, was float64
		want     bool
	}{
		{1.05, 1, false},
		{1.06, 1, true},
		{0.94, 1, true},
		{1, 0, true},
		{0, 0, false},
		{nan, 1, true},
		{1, nan, true},
		{nan, nan, false},
	} {
		if got := Moved(c.now, c.was, 0.055); got != c.want {
			t.Errorf("Moved(%v, %v) = %v, want %v", c.now, c.was, got, c.want)
		}
	}
}

// RestoredKeys counts what Restored reports, among the keys matched, and a
// key that kept a reading is no longer one.
func TestRestoredKeys(t *testing.T) {
	tr := NewTracker()
	tr.RestoreBaseline("ns|m|a", 0.01)
	tr.RestoreBaseline("ns|m|b", 0.01)
	tr.RestoreBaseline("ns|other|a", 0.01)
	tr.SetBaseline("ns|m|c", 0.01)
	if got := tr.RestoredKeys(func(k string) bool { return strings.HasPrefix(k, "ns|m|") }); got != 2 {
		t.Fatalf("RestoredKeys = %d, want 2", got)
	}
	if got := tr.RestoredKeys(func(string) bool { return false }); got != 0 {
		t.Fatalf("RestoredKeys, nothing matched = %d", got)
	}
}

func TestReport(t *testing.T) {
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	tr.windows["k"] = newKeyWindow(DefaultWindowMaxSize)
	tr.above = map[string]*Window{"k": newKeyWindow(DefaultWindowMaxSize)}
	tr.windows["k"].Add(0.4, 0.05, at)
	tr.above["k"].Add(0.9, 0.09, at.Add(time.Minute))

	for _, c := range []struct {
		name string
		r    Reading
		key  string
		when time.Time
	}{
		{"a line from the window", Reading{Tier: TierPinnedB, Held: 1}, "k", at},
		{"a line above the band", Reading{Tier: TierPinnedB, AboveBandFit: true}, "k", at.Add(time.Minute)},
		{"a held slope", Reading{Tier: TierPinnedB, SlopeHeld: true, SlopeFittedAt: at.Add(-time.Hour)}, "k", at.Add(-time.Hour)},
		{"no line", Reading{}, "", time.Time{}},
	} {
		got := tr.Report(learn.Report{Variant: "v"}, "k", c.r)
		if got.Variant != "v" || got.PricingKey != c.key || !got.LastAdmitted.Equal(c.when) {
			t.Errorf("%s: %+v, want key %q at %v", c.name, got, c.key, c.when)
		}
	}
	got := tr.Report(learn.Report{}, "k", Reading{Offered: 3, Settling: 1, Unsteady: 1, Held: 7})
	if got.Writes != 3 || got.Samples != 7 || got.Counts["offered"] != 3 || got.Counts["settling"] != 1 {
		t.Errorf("counts: %+v", got)
	}
	if _, ok := got.Counts["no-itl"]; ok {
		t.Errorf("a reason at zero was filed: %+v", got.Counts)
	}
}
