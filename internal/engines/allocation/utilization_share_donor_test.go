package allocation

import (
	"fmt"
	"testing"
	"time"
)

// qAfterReadings is Q's commitment after a hard imbalance (L needs 11 on 3,
// Q reads 0.5 on 7) when Q read the given needs at 975 and 990 -- inside its
// 2-minute scale-down window for every cycle -- after 0.5 at 960. L's
// history, when long, holds a completed burst longer than a move, from a
// minute into it (one under way when the history starts is not complete).
func qAfterReadings(readings []float64, lLongBursts bool) int {
	tm := spareTimings()
	tm.Window = 2 * time.Minute // every reading stays inside it for every cycle
	in := spareRun(11, 0.5, 7)
	l := NewShareLedger()
	if lLongBursts {
		start := time.Unix(1000, 0).Add(-tm.SwingWindow * 9 / 10)
		end := start.Add(tm.moveLatency() * 2)
		for at := start; at.Before(time.Unix(950, 0)); at = at.Add(30 * time.Second) {
			need := 1.0
			if !at.Before(start.Add(time.Minute)) && at.Before(end) {
				need = 11
			}
			l.RecordNeeds(map[string]float64{"L": need}, at, tm)
		}
	}
	l.RecordNeeds(map[string]float64{"Q": 0.5}, time.Unix(960, 0), tm)
	for i, n := range readings {
		l.RecordNeeds(map[string]float64{"Q": n}, time.Unix(int64(975+15*i), 0), tm)
	}
	for i := range 3 {
		PlanShareTransfers(l, in, time.Unix(int64(1000+10*i), 0), tm)
	}
	return l.Committed(in.Held)["Q"]
}

// A donor's recent peak, like any peak, needs two readings in a row: one --
// a glitch, or a reading a tenant forges once a scale-down window -- would
// keep its GPUs from a short neighbour for the whole window. Q read 3 once:
// it is drained as calm. Read twice, it keeps at least 5 (3 <= 0.6 x 5).
func TestShareDonorRecentPeakNeedsTwoReadings(t *testing.T) {
	if q := qAfterReadings([]float64{3, 0.5}, false); q >= 5 {
		t.Fatalf("Q, which read 3 once, kept %d: one reading held its GPUs", q)
	}
	if q := qAfterReadings([]float64{3, 3}, false); q < 5 {
		t.Fatalf("control: Q, which read 3 twice in a row, was drained to %d", q)
	}
}

// A receiver whose bursts outlast a move judges its donor on the donor's
// need, not its recent peak: the move lands inside the burst it is made for,
// and holding it back a scale-down window at every handover only made the
// burst wait. Control: the same donor, for a receiver with no completed long
// burst, keeps its recent peak.
func TestShareLongBurstReceiverJudgesTheDonorOnItsNeed(t *testing.T) {
	if q := qAfterReadings([]float64{3, 3}, true); q >= 5 {
		t.Fatalf("Q kept %d for L, whose bursts outlast a move: held back on its recent peak", q)
	}
	if q := qAfterReadings([]float64{3, 3}, false); q < 5 {
		t.Fatalf("control: Q was drained to %d for L, with no long burst", q)
	}
}

func TestShareLongBursts(t *testing.T) {
	tm := spareTimings()
	t0 := time.Unix(10000, 0)
	record := func(l *ShareLedger, needs ...float64) {
		for i, n := range needs {
			l.RecordNeeds(map[string]float64{"R": n}, t0.Add(time.Duration(i)*time.Minute), tm)
		}
	}
	l := NewShareLedger()
	record(l, 1, 9, 9, 9, 9, 1, 1)
	if !l.LongBursts("R", 4*time.Minute) {
		t.Error("a completed burst of 4 minutes is not long at 4 minutes")
	}
	if l.LongBursts("R", 5*time.Minute) {
		t.Error("a completed burst of 4 minutes is long at 5 minutes")
	}
	l = NewShareLedger()
	record(l, 1, 1, 9, 9, 9, 9, 9)
	if !l.LongBursts("R", 4*time.Minute) || l.LongBursts("R", 5*time.Minute) {
		t.Error("a burst 4 minutes under way is long at 4 minutes and not at 5")
	}
	l = NewShareLedger()
	record(l, 5, 5, 5, 5)
	if l.LongBursts("R", time.Minute) {
		t.Error("a flat need has no bursts")
	}
}

// The donor-peak guard holds for a donor set as for a single move: while the
// quota holds every peak, a receiver at its need -- here one whose replica
// spans two 8-GPU pods, so only a set funds it -- takes no donor below its own
// recent peak, its peak over its scale-down window. A peak older than that
// window, still inside the swing window, does not hold it -- unless D reached
// it in two bursts further apart than its scale-down window: a peak that
// comes back holds its donor for the window. Control: with D's peak no higher
// than what it keeps, the set starts.
func TestShareDonorSetKeepsTheDonorsPeak(t *testing.T) {
	run := func(recentPeak, oldPeak float64, recurring bool) (started bool, withheld int) {
		t0 := time.Unix(1_000_000, 0)
		tm := ShareTimings{Window: 10 * time.Minute, ReleaseTimeout: 10 * time.Minute, FillTimeout: 3 * time.Minute,
			ReversalHold: 10 * time.Minute, SwingWindow: 60 * time.Minute}
		lws := ShareVariant{Name: "r", GPUs: 16, PodGPUs: []int{8, 8}}
		v8 := ShareVariant{Name: "d", GPUs: 8, PodGPUs: []int{8}}
		l := NewShareLedger()
		// A known history: D at oldPeak from 70 to 40 minutes ago, inside
		// the swing window -- recurring, from 56 to 48 and from 34 to 26
		// minutes ago instead -- and at recentPeak over the last 6 minutes,
		// inside its scale-down window.
		ago := func(at time.Time, from, to time.Duration) bool {
			return !at.Before(t0.Add(-from)) && at.Before(t0.Add(-to))
		}
		for at := t0.Add(-70 * time.Minute); at.Before(t0.Add(-30 * time.Second)); at = at.Add(30 * time.Second) {
			d := 8.0
			switch {
			case recurring && (ago(at, 56*time.Minute, 48*time.Minute) || ago(at, 34*time.Minute, 26*time.Minute)):
				d = oldPeak
			case !recurring && at.Before(t0.Add(-40*time.Minute)):
				d = oldPeak
			case !at.Before(t0.Add(-6 * time.Minute)):
				d = recentPeak
			}
			l.RecordNeeds(map[string]float64{"D": d, "R": 16}, at, tm)
		}
		held := map[string]int{"D": 32, "R": 16}
		in := SharePlanInput{Roles: []ShareRole{
			{Key: "D", Weight: 1, Need: 8, Floor: 8, Ceiling: 48, ReplicaGPUs: 8},
			{Key: "R", Weight: 1, Need: 16, Floor: 16, Ceiling: 48, ReplicaGPUs: 16},
		}, Held: held, Thresholds: map[string]float64{"D": 1, "R": 1}, Budget: 48, Tolerance: 0.15,
			Give: map[string]ShareVariant{"D": v8, "R": lws}, Grow: map[string]ShareVariant{"D": v8, "R": lws},
			PhysicalFree: 1 << 30, Headroom: ShareHeadroom{Capped: true, Replicas: 1}}
		for i := range 6 {
			p := PlanShareTransfers(l, in, t0.Add(time.Duration(i)*30*time.Second), tm)
			if p.HeadroomCapped {
				panic("setup: the peaks (32 + 16 of 48) fit; the cap must be off")
			}
			withheld += p.Withheld[ShareWithheldDonorPeak]
			for _, tr := range p.Started {
				if tr.Donor == "D" {
					started = true
				}
			}
		}
		if b := l.needBurstsJoined("D", tm.Window); recurring != (b.completed >= 2) {
			panic(fmt.Sprintf("setup: D burst %d times within the window, recurring %v", b.completed, recurring))
		}
		return started, withheld
	}
	if started, withheld := run(32, 8, false); started || withheld == 0 {
		t.Fatalf("a set took D below its recent peak of 32 for R, at its need (started %v, withheld %d)", started, withheld)
	}
	if started, withheld := run(8, 32, false); !started || withheld != 0 {
		t.Fatalf("a peak of 32 40 minutes ago kept D from funding R (started %v, withheld %d): held on a peak older than its scale-down window",
			started, withheld)
	}
	if started, withheld := run(8, 32, true); started || withheld == 0 {
		t.Fatalf("D, peaking at 32 twice within the window, funded R below that peak (started %v, withheld %d): a peak that comes back was dropped",
			started, withheld)
	}
	if started, _ := run(8, 8, false); !started {
		t.Fatal("control: with D's peak no higher than it keeps, no set funded R")
	}
}

// Returns credited to a receiver whose replica spans several pods must hold
// its pods: 4-GPU returned pods do not hold an 8-GPU pod, and a return whose
// shape is not known holds nothing.
func TestShareAwaitingReturnCountsOnlyFittingReturns(t *testing.T) {
	plan := func(shape []int) SharePlan {
		tm := simTimings()
		t0 := time.Unix(100000, 0)
		l := NewShareLedger()
		held := map[string]int{"R": 16, "D": 64}
		for range 2 {
			l.Start(ShareTransfer{Donor: "D", DonorGPUs: 8, DonorVariant: "d", Entitled: true, Returned: true,
				DonorPodGPUs: shape}, held, t0.Add(-30*time.Second), tm)
		}
		in := SharePlanInput{Roles: []ShareRole{
			{Key: "R", Weight: 1, Need: 40, Floor: 16, Ceiling: 32, ReplicaGPUs: 16},
			{Key: "D", Weight: 1, Need: 2, Floor: 8, Ceiling: 96, ReplicaGPUs: 8},
		}, Held: held, Thresholds: map[string]float64{"R": 0.8, "D": 0.8}, Budget: 80, Tolerance: 0.15,
			Headroom: ShareHeadroom{Capped: true, Replicas: 1},
			Give:     map[string]ShareVariant{"D": {Name: "d", GPUs: 8, PodGPUs: []int{8}}},
			Grow:     map[string]ShareVariant{"R": {Name: "r", GPUs: 16, PodGPUs: []int{8, 8}}}}
		return PlanShareTransfers(l, in, t0, tm)
	}
	if p := plan([]int{8}); p.Unfunded["R"] != ShareUnfundedAwaitingReturn {
		t.Fatalf("two 1x8 returns, one replica of R, were not credited: unfunded %v", p.Unfunded)
	}
	for _, shape := range [][]int{{4, 4}, nil} {
		if p := plan(shape); p.Withheld[ShareWithheldAwaitingReturn] != 0 {
			t.Errorf("returns shaped %v, which hold no 8-GPU pod, were credited to R: %v", shape, p.Withheld)
		}
	}
}
