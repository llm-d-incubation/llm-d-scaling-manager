package allocation

import (
	"math"
	"testing"
	"time"
)

// qAfterHardImbalance runs spareRun(11, 0.5, 7) -- L bursting on 3, Q calm on
// 7 -- for three cycles 10 s apart, and returns what Q is left committed.
// recent is a need Q read 5 s before the first cycle, inside its 30 s
// scale-down window throughout; 0 reads none.
func qAfterHardImbalance(recent float64) int {
	tm := spareTimings()
	in := spareRun(11, 0.5, 7)
	l := NewShareLedger()
	burstHistory(l)
	if recent > 0 {
		l.RecordNeeds(map[string]float64{"Q": recent}, time.Unix(995, 0), tm)
	}
	for i := range 3 {
		PlanShareTransfers(l, in, time.Unix(int64(1000+10*i), 0), tm)
	}
	return l.Committed(in.Held)["Q"]
}

// A donor is judged calm on the higher of its need and its peak over its
// scale-down window, as a scale-down is: Q reads 0.5 now but read 3 within its
// window, and a hard imbalance -- which takes a donor down to where it would
// stop being calm -- leaves it at least 5 (3 <= 0.6 x 5). Control: judged on
// the reading alone, it is drained lower.
func TestShareHardImbalanceJudgesTheDonorOnItsRecentPeak(t *testing.T) {
	if q := qAfterHardImbalance(3); q < 5 {
		t.Fatalf("Q, which read 3 within its window, was drained to %d", q)
	}
	if q := qAfterHardImbalance(0); q >= 5 {
		t.Fatalf("control: Q, calm on its reading alone, kept %d", q)
	}
}

// setRun is R -- a replica of two 8-GPU pods, reading 40 on 16 -- beside D,
// reading 2 on 64 in 1x8 replicas, and C, at its floor and calm. No single D
// replica fits R's pods, so only a donor set can fund it.
type setRun struct {
	// dRecent is a need D read twice in a row, 75 and 45 s before the first
	// cycle, inside its 300 s scale-down window; 0 reads none. Once would be
	// a glitch (RecentPeak).
	dRecent float64
	// dHeld: D received within its reversal hold, so only a hard imbalance
	// may take from it.
	dHeld bool
	// rSwinging plans R on its mean over a swing window of readings of 4.
	rSwinging bool
	// noC leaves C out of the group, and its GPUs out of the quota.
	noC bool
	// nodes: D's replicas each run alone on a node of one zone, and R's pods
	// must share a zone, so only the node-aware set (shareNodeSet) can fund R.
	nodes  bool
	cycles int
}

// drained plans the run's cycles 30 s apart and returns the first (from 1) in
// which a set drained D, or 0.
func (s setRun) drained() int {
	tm := simTimings()
	t0 := time.Unix(100000, 0)
	v8 := ShareVariant{Name: "v", GPUs: 8, PodGPUs: []int{8}}
	lws := ShareVariant{Name: "l", GPUs: 16, PodGPUs: []int{8, 8}}
	rHistory := 40.0
	if s.rSwinging {
		rHistory = 4
	}
	l := NewShareLedger()
	for i := 60; i > 0; i-- {
		l.RecordNeeds(map[string]float64{"D": 2, "R": rHistory, "C": 1}, t0.Add(-time.Duration(30*i)*time.Second), tm)
	}
	if s.dRecent > 0 {
		for _, ago := range []time.Duration{75 * time.Second, 45 * time.Second} {
			l.RecordNeeds(map[string]float64{"D": s.dRecent}, t0.Add(-ago), tm)
		}
	}
	if s.dHeld {
		l.recordMove("D", true, "x", t0.Add(-10*time.Second), tm)
	}
	if s.rSwinging {
		l.swingUntil["R"] = t0.Add(time.Hour)
	}
	held := map[string]int{"R": 16, "D": 64, "C": 16}
	roles := []ShareRole{
		{Key: "R", Weight: 1, Need: 40, Floor: 16, Ceiling: 96, ReplicaGPUs: 16},
		{Key: "D", Weight: 1, Need: 2, Floor: 8, Ceiling: 96, ReplicaGPUs: 8},
		{Key: "C", Weight: 1, Need: 1, Floor: 16, Ceiling: 96, ReplicaGPUs: 8},
	}
	budget := 96
	if s.noC {
		roles, budget = roles[:2], 80
		delete(held, "C")
	}
	vs := map[string]ShareVariant{"R": lws, "D": v8, "C": v8}
	in := SharePlanInput{Roles: roles, Held: held, Thresholds: map[string]float64{"R": 0.8, "D": 0.8, "C": 0.8},
		Budget: budget, Tolerance: 0.15, Give: vs, Grow: vs, PhysicalFree: math.MaxInt,
		Headroom: ShareHeadroom{Capped: true, Replicas: 1}}
	if s.nodes {
		in.Nodes = map[string]ShareNode{}
		units := make([]ShareUnit, 0, 8)
		for i := range 8 {
			n := "n" + string(rune('0'+i))
			in.Nodes[n] = ShareNode{Labels: map[string]string{"zone": "z"}}
			units = append(units, ShareUnit{Pods: []SharePod{{Name: "ns/d-" + n, Node: n, GPUs: 8}}})
		}
		in.DonorUnits = map[string][]ShareUnit{"D": units}
		in.DomainKey = map[string]string{"R": "zone"}
	}
	for i := range s.cycles {
		p := PlanShareTransfers(l, in, t0.Add(time.Duration(30*i)*time.Second), tm)
		for _, tr := range p.Started {
			if tr.Donor == "D" {
				return i + 1
			}
		}
	}
	return 0
}

// A donor set judges each donor as a single move does, on the higher of its
// need and its peak over its scale-down window: D reads 2 now but 40 within
// its window, so no set -- node-blind, or placed node by node -- takes it
// down past its reversal hold as calm. Control: without the recent reading,
// both do.
func TestShareDonorSetJudgesADonorOnItsRecentPeak(t *testing.T) {
	for _, nodes := range []bool{false, true} {
		if c := (setRun{dRecent: 40, dHeld: true, nodes: nodes, cycles: 2}).drained(); c != 0 {
			t.Errorf("nodes %v: a set drained D, which read 40 within its window, as calm (cycle %d)", nodes, c)
		}
		if (setRun{dHeld: true, nodes: nodes, cycles: 2}).drained() == 0 {
			t.Errorf("nodes %v: control: no set drained D, calm on its reading", nodes)
		}
	}
}

// A donor set judges its receiver as a single move does, on the higher of its
// reading and its mean: R, swinging, reads 40 on 16 but is planned on a mean
// near 5. On its reading it is a hard imbalance beside a calm D, which skips
// D's reversal hold; on its mean, no set -- node-blind, or placed node by
// node -- could take from D until the hold ran out.
func TestShareDonorSetJudgesASwingingReceiverOnItsReading(t *testing.T) {
	for _, nodes := range []bool{false, true} {
		if (setRun{dHeld: true, rSwinging: true, nodes: nodes, cycles: 2}).drained() == 0 {
			t.Errorf("nodes %v: R, reading 40 on 16 beside a calm D, was not funded past D's hold", nodes)
		}
	}
}

// A donor whose recent peak is high makes no hard imbalance, though it reads
// calm now: the set that funds R waits the confirm cycles like any move.
// Control: calm on its recent peak too, D funds R on the first cycle.
func TestShareRecentPeakDonorMakesNoHardImbalance(t *testing.T) {
	if c := (setRun{dRecent: 40, noC: true, cycles: 2}).drained(); c != 2 {
		t.Fatalf("D, which read 40 within its window, was drained on cycle %d, want 2: after the confirm cycles", c)
	}
	if c := (setRun{noC: true, cycles: 2}).drained(); c != 1 {
		t.Fatalf("control: D, calm, was drained on cycle %d, want 1: a hard imbalance waits no confirm", c)
	}
}

// A role that gave and is now at the crest of a burst is exempt from its
// reversal hold on its reading, not its mean: L reads 8 on 3, but its mean
// over the swing window is under 1.5. Judged on the mean, the move that would
// skip the hold is not a hard imbalance, and L waits out the hold beside a
// calm donor.
func TestShareSwingingReceiverSkipsItsReversalHold(t *testing.T) {
	tm := spareTimings()
	in := spareRun(8, 0.5, 7)
	l := NewShareLedger()
	l.recordMove("L", false, "x", time.Unix(990, 0), tm) // L gave: held from receiving
	swingingL(l, 1)
	if !l.ReceivingHeld("L", time.Unix(1000, 0), tm) {
		t.Fatal("setup: L is not held from receiving")
	}
	skipped := 0
	for i := range 3 {
		p := PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), tm)
		for _, tr := range p.Started {
			if tr.Receiver == "L" && tr.SkippedWaits {
				skipped++
			}
		}
	}
	if skipped == 0 {
		t.Fatal("L, reading 8 on 3 beside a calm Q, waited out its reversal hold: judged on its mean")
	}
}
