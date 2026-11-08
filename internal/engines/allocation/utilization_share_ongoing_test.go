package allocation

import (
	"math"
	"slices"
	"testing"
	"time"
)

// A burst under way counts as long once it has lasted long enough: read on
// completed bursts alone, a model in its first long burst was not a long
// burster until the burst ended. R has burst 4 minutes and is still in it.
func TestShareBurstUnderWayCountsAsLong(t *testing.T) {
	l := burstLedger(nil, "R", 1, 1, 9, 9, 9, 9, 9)
	if !l.LongBursts("R", 4*time.Minute) {
		t.Error("a burst 4 minutes under way is not long at 4 minutes")
	}
	if l.LongBursts("R", 5*time.Minute) {
		t.Error("a burst 4 minutes under way is long at 5 minutes")
	}
	if l := burstLedger(nil, "R", 0.5, 0.5, 8, 8, 8, 8, 8); !l.BurstsLong("R", 4*time.Minute) {
		t.Error("a burst from 0.5 to 8, 4 minutes under way, does not burst long at 4 minutes")
	}
}

// A role whose short bursts made it a fast swinger is not one once the burst
// under way outlasts a move: it kept reading as a fast swinger for as long as
// its first long burst lasted. Control: one under way for less than the move
// leaves it a fast swinger.
func TestShareBurstUnderWayEndsAFastSwing(t *testing.T) {
	if l := burstLedger(nil, "R", 2, 2, 6, 6, 2, 2, 6, 6, 2, 2, 6, 6, 6, 6, 6); l.SwingsFast("R", 3*time.Minute) {
		t.Error("two 2-minute bursts and one 4 minutes under way swing fast at 3 minutes")
	}
	if l := burstLedger(nil, "R", 2, 2, 6, 6, 2, 2, 6, 6, 2, 2, 6, 6); !l.SwingsFast("R", 3*time.Minute) {
		t.Error("control: two 2-minute bursts and one 1 minute under way do not swing fast at 3 minutes")
	}
}

// ongoingSwingMeanCapped reports whether A is capped on its mean beside B, in
// a contended quota of 10 (SharePlan.MeanCapped). A swings between 2 and 6
// every 2 minutes, far faster than a move, and is at its crest (6, holding 3). B has been at 0.5 and is in its FIRST burst
// to 8, under way for bFor and still running (holding 5) -- it has no
// completed burst at all.
func ongoingSwingMeanCapped(bFor time.Duration) bool {
	tm := simTimings()
	t0 := time.Unix(100000, 0)
	l := NewShareLedger()
	for at := t0.Add(-100 * time.Minute); !at.After(t0); at = at.Add(30 * time.Second) {
		a := 2.0
		if int(at.Sub(t0).Minutes()+100)/2%2 == 0 {
			a = 6
		}
		b := 0.5
		if t0.Sub(at) < bFor {
			b = 8
		}
		l.RecordNeeds(map[string]float64{"A": a, "B": b}, at, tm)
	}
	in := SharePlanInput{Roles: []ShareRole{
		{Key: "A", Weight: 1, Need: 6, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "B", Weight: 1, Need: 8, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
	}, Held: map[string]int{"A": 3, "B": 5}, Thresholds: map[string]float64{"A": 1, "B": 1}, Budget: 10,
		Tolerance: 0.15, PhysicalFree: math.MaxInt, Headroom: ShareHeadroom{Capped: true, Replicas: 1}}
	p := PlanShareTransfers(l, in, t0, tm)
	if !p.HeadroomCapped {
		panic("setup: the peaks (6 + 8 of 10) do not fit; the cap must be on")
	}
	return slices.Contains(p.MeanCapped, "A")
}

// A fast swinger is capped on its mean beside a model in its first long
// burst, as beside one whose long burst has ended: the GPUs A takes at each
// crest are the ones B's burst waits a release for, and B's burst is already
// longer than a move. Control: B 3 minutes into its burst, shorter than a
// move, does not cap A.
func TestShareFastSwingerCappedBesideAFirstLongBurst(t *testing.T) {
	if !ongoingSwingMeanCapped(30 * time.Minute) {
		t.Error("A is not capped on its mean beside B 30 minutes into its first burst, longer than a move")
	}
	if ongoingSwingMeanCapped(3 * time.Minute) {
		t.Error("control: A is capped on its mean beside B 3 minutes into its first burst, shorter than a move")
	}
}
