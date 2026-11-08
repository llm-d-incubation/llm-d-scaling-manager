package allocation

import (
	"math"
	"testing"
	"time"
)

// fastSwingTarget is A's integer target in a contended quota of 10, at the
// crest of its demand (6, holding 3), beside B in its trough (0.5, holding 2).
// A's history swings between 2 and 6 every 2 minutes, far faster than a move
// (12 minutes at simTimings). B's history peaks at 8 for 30 minutes, from 10
// minutes into it, when bLong (a burst under way when the history starts is
// not counted complete), else in 4-minute bursts, so either way the peaks
// (6 + 8) do not fit together.
func fastSwingTarget(bLong bool) int {
	for _, v := range fastSwingPlan(bLong).Evaluation.Roles {
		if v.Key == "A" {
			return v.Integer
		}
	}
	panic("no A")
}

// fastSwingPlan is the plan fastSwingTarget reads A's target from.
func fastSwingPlan(bLong bool) SharePlan {
	tm := simTimings()
	t0 := time.Unix(100000, 0)
	l := NewShareLedger()
	for at := t0.Add(-100 * time.Minute); at.Before(t0); at = at.Add(30 * time.Second) {
		a := 2.0
		if int(at.Sub(t0).Minutes()+100)/2%2 == 0 {
			a = 6
		}
		b := 0.5
		since := at.Sub(t0.Add(-100 * time.Minute))
		if bLong && since >= 10*time.Minute && since < 40*time.Minute || !bLong && since%(20*time.Minute) < 4*time.Minute {
			b = 8
		}
		l.RecordNeeds(map[string]float64{"A": a, "B": b}, at, tm)
	}
	in := SharePlanInput{Roles: []ShareRole{
		{Key: "A", Weight: 1, Need: 6, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "B", Weight: 1, Need: 0.5, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
	}, Held: map[string]int{"A": 3, "B": 2}, Thresholds: map[string]float64{"A": 1, "B": 1}, Budget: 10,
		Tolerance: 0.15, PhysicalFree: math.MaxInt, Headroom: ShareHeadroom{Capped: true, Replicas: 1}}
	p := PlanShareTransfers(l, in, t0, tm)
	if !p.HeadroomCapped {
		panic("setup: the peaks (6 + 8 of 10) do not fit; the cap must be on")
	}
	return p
}

// A model whose demand swings faster than a move is capped on its mean need
// while another bursts for longer than a move: GPUs taken at each crest
// start serving as it ends, and parked on it they are what the long burst
// waits a release for. A, whose mean is just over 4, gets 5 GPUs plus a
// replica of headroom, not its crest's 6 plus one. Control: beside a model
// whose bursts are as short as its own, nothing waits on A's GPUs, and it is
// capped on its reading.
func TestShareFastSwingerCappedOnItsMeanBesideALongBurster(t *testing.T) {
	if a := fastSwingTarget(true); a > 6 {
		t.Fatalf("A, swinging every 2 minutes beside a 30-minute burster, targets %d: capped on its crest", a)
	}
	if a := fastSwingTarget(false); a < 7 {
		t.Fatalf("control: A, beside 4-minute bursts, targets %d: capped below its crest's 6 plus one", a)
	}
}

func TestShareSwingsFastAndBurstsLong(t *testing.T) {
	tm := simTimings()
	t0 := time.Unix(10000, 0)
	record := func(needs ...float64) *ShareLedger {
		l := NewShareLedger()
		for i, n := range needs {
			l.RecordNeeds(map[string]float64{"R": n}, t0.Add(time.Duration(i)*time.Minute), tm)
		}
		return l
	}
	fast := record(2, 2, 6, 6, 2, 2, 6, 6, 2, 2)
	if !fast.SwingsFast("R", 3*time.Minute) {
		t.Error("two completed 2-minute bursts do not swing fast at 3 minutes")
	}
	if fast.SwingsFast("R", 2*time.Minute) {
		t.Error("2-minute bursts swing fast at 2 minutes")
	}
	if record(2, 6, 6, 2, 2).SwingsFast("R", 3*time.Minute) {
		t.Error("one completed burst swings fast")
	}
	if record(5, 5.5, 5.5, 5, 5.5, 5.5, 5).SwingsFast("R", 3*time.Minute) {
		t.Error("a need within a tenth of its peak swings fast")
	}

	long := record(0.5, 8, 8, 8, 8, 0.5, 0.5)
	if !long.BurstsLong("R", 4*time.Minute) {
		t.Error("a completed 4-minute burst from 0.5 to 8 does not burst long at 4 minutes")
	}
	if long.BurstsLong("R", 5*time.Minute) {
		t.Error("a 4-minute burst bursts long at 5 minutes")
	}
	if record(6, 8, 8, 8, 8, 6, 6).BurstsLong("R", 4*time.Minute) {
		t.Error("a need between 6 and 8 bursts long")
	}
	if !record(6, 8, 8, 8, 8, 6, 6).LongBursts("R", 4*time.Minute) {
		t.Error("LongBursts, which takes no spread, changed")
	}

	if m := record(2, 6, 6, 2).MeanNeed("R", 9); m != 4 {
		t.Errorf("MeanNeed = %v, want 4", m)
	}
	if m := NewShareLedger().MeanNeed("R", 9); m != 9 {
		t.Errorf("MeanNeed with no history = %v, want the current 9", m)
	}
}
