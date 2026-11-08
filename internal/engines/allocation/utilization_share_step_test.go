package allocation

import (
	"slices"
	"testing"
	"time"
)

// A need counts as a step once it has stayed more than ShareStepSpread from
// its mean, on one side, for the latency given and for longer than any
// earlier run within the window; a need that crosses its mean does not.
func TestShareLedgerSteppedAway(t *testing.T) {
	tm := ShareTimings{SwingWindow: 2 * time.Hour}
	t0 := time.Unix(1_000_000, 0)
	record := func(needs ...float64) *ShareLedger {
		l := NewShareLedger()
		for i, n := range needs {
			l.RecordNeeds(map[string]float64{"R": n}, t0.Add(time.Duration(i)*time.Minute), tm)
		}
		return l
	}
	flat := make([]float64, 60)
	for i := range flat {
		flat[i] = 2
	}
	step := append(slices.Clone(flat), 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3)
	if l := record(step...); !l.steppedAway("R", 12*time.Minute) || l.steppedAway("R", 13*time.Minute) {
		t.Error("a need 50% above its mean for 12 minutes is a step at 12 minutes and not at 13")
	}
	if l := record(append(slices.Clone(flat), 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1)...); !l.steppedAway("R", 12*time.Minute) {
		t.Error("a need 50% below its mean for 12 minutes is not a step")
	}
	var swing []float64
	for i := range 80 {
		if i/6%2 == 0 {
			swing = append(swing, 1)
		} else {
			swing = append(swing, 3)
		}
	}
	if record(swing...).steppedAway("R", 12*time.Minute) {
		t.Error("a need crossing its mean every 6 minutes is a step at 12 minutes")
	}

	// A swing slower than the latency is still a swing: its run under way
	// has stayed on one side for 17 minutes, past the 12 given, but no longer
	// than the runs before it.
	var slow []float64
	for i := range 108 {
		if i/18%2 == 0 {
			slow = append(slow, 1)
		} else {
			slow = append(slow, 3)
		}
	}
	l := record(slow...)
	s, mean := l.needs["R"], l.MeanNeed("R", 0)
	if slices.ContainsFunc(s[len(s)-13:], func(x shareNeedSample) bool { return x.need <= mean*(1+ShareStepSpread) }) {
		t.Fatal("setup: the slow swing's last 13 readings are not all above its mean by ShareStepSpread")
	}
	if l.steppedAway("R", 12*time.Minute) {
		t.Error("a need swinging every 18 minutes is a step at 12 minutes: its run is no longer than the ones before it")
	}
	// A run that outlasts the swing's own is a step, once it has.
	ones := func(n int) []float64 {
		out := make([]float64, n)
		for i := range out {
			out[i] = 1
		}
		return out
	}
	if record(append(slices.Clone(slow), ones(18)...)...).steppedAway("R", 12*time.Minute) {
		t.Error("a run of 17 minutes after runs of 17 is a step")
	}
	if !record(append(slices.Clone(slow), ones(25)...)...).steppedAway("R", 12*time.Minute) {
		t.Error("a run of 24 minutes after runs of 17 is not a step")
	}
	if record(append(slices.Clone(flat), 2.2, 2.2, 2.2, 2.2, 2.2, 2.2, 2.2, 2.2, 2.2, 2.2, 2.2, 2.2, 2.2)...).steppedAway("R", time.Minute) {
		t.Error("a need within ShareStepSpread of its mean is a step")
	}
}
