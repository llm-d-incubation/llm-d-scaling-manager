package allocation

import (
	"math"
	"testing"
	"time"
)

// spareRun is the cluster run that motivated ShareCapSpare and the unpaced
// hard imbalance: after its burst, Q held 7 of a quota of 10 at 5% load while
// L, bursting, held 3 against a need of 11. L and Q are one-GPU replicas with
// a floor of one and a ceiling of eight; the spare is capped at one replica.
func spareRun(lNeed, qNeed float64, qHeld int) SharePlanInput {
	roles := []ShareRole{
		{Key: "L", Weight: 1, Need: lNeed, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "Q", Weight: 1, Need: qNeed, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
	}
	// Raw: the planner caps them, and only while the quota is contended.
	return SharePlanInput{Roles: roles, Held: map[string]int{"L": 3, "Q": qHeld},
		Thresholds: map[string]float64{"L": 0.85, "Q": 0.85}, Budget: 10, Tolerance: 0.15,
		PhysicalFree: math.MaxInt, Headroom: ShareHeadroom{Capped: true, Replicas: 1}}
}

// spareTimings are simTimings with a 30s scale-down window: a role above its
// target returns its spare on the third cycle.
func spareTimings() ShareTimings {
	tm := simTimings()
	tm.Window = 30 * time.Second
	return tm
}

// burstHistory makes the ledger remember both models bursting to 8, as in
// the cluster run: at their peaks they would need 16 of the quota's 10, so
// the quota is contended and the spare is capped. Two readings, so the peak
// counts (PeakNeed), and before the scale-down window, so a donor is judged
// calm on what it reads now (donorView).
func burstHistory(l *ShareLedger) {
	for _, at := range []int64{900, 915} {
		l.RecordNeeds(map[string]float64{"L": 8, "Q": 8}, time.Unix(at, 0), spareTimings())
	}
}

// planEach runs n cycles 15s apart on one ledger, which remembers both models'
// bursts (burstHistory), and returns each cycle's plan.
func planEach(l *ShareLedger, in SharePlanInput, n int) []SharePlan {
	burstHistory(l)
	out := make([]SharePlan, 0, n)
	for i := range n {
		out = append(out, PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), spareTimings()))
	}
	return out
}

func qToL(ts []ShareTransfer) int {
	n := 0
	for _, t := range ts {
		if t.Donor == "Q" && t.Receiver == "L" {
			n++
		}
	}
	return n
}

func TestShareCapSpare(t *testing.T) {
	roles := []ShareRole{
		{Key: "a", Weight: 1, Need: 2.3, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "b", Weight: 1, Need: 2.3, ReplicaGPUs: 1},             // unbounded
		{Key: "c", Weight: 1, Need: 2.3, Ceiling: 3, ReplicaGPUs: 1}, // its own ceiling is lower
		{Key: "d", Weight: 1, Need: 9, Ceiling: 64, ReplicaGPUs: 8},  // whole-node replicas
		{Key: "e", Weight: 1, Need: 0.2, Floor: 2, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "f", Weight: 2, Need: 2.3, Ceiling: 16, ReplicaGPUs: 1},   // important: twice the headroom
		{Key: "g", Weight: 0.5, Need: 2.3, Ceiling: 16, ReplicaGPUs: 1}, // best-effort: half, rounded up
		{Key: "h", Weight: 1, Need: 0, Ceiling: 8, ReplicaGPUs: 1},      // no claim, no headroom: limit 0 kept as is
	}
	for spare, want := range map[int]map[string]int{
		1:  {"a": 4, "b": 4, "c": 3, "d": 24, "e": 3, "f": 5, "g": 4, "h": 1},
		2:  {"a": 5, "b": 5, "c": 3, "d": 32, "e": 4, "f": 7, "g": 4, "h": 2},
		0:  {"a": 3, "b": 3, "c": 3, "d": 16, "e": 2, "f": 3, "g": 3, "h": 8},
		-1: {"a": 8, "b": 0, "c": 3, "d": 64, "e": 8, "f": 16, "g": 16, "h": 8},
	} {
		got := ShareCapSpare(roles, spare)
		for i, r := range got {
			if r.Ceiling != want[r.Key] {
				t.Errorf("spare %d: %s ceiling %d, want %d", spare, r.Key, r.Ceiling, want[r.Key])
			}
			if r.Claim() != roles[i].Claim() {
				t.Errorf("spare %d: %s claim %v changed from %v", spare, r.Key, r.Claim(), roles[i].Claim())
			}
		}
	}
	if roles[0].Ceiling != 8 {
		t.Fatal("ShareCapSpare changed its input")
	}
}

// A held role's headroom is counted above its need, never below what it
// holds: above its floor, every cycle a hold lasts would raise it a replica.
func TestShareCapSpareHeldRole(t *testing.T) {
	roles := []ShareRole{
		{Key: "low", Weight: 1, Need: 3.5, Floor: 10, ReplicaGPUs: 1, Held: true},   // keeps 10, no headroom above
		{Key: "high", Weight: 1, Need: 12, Floor: 10, ReplicaGPUs: 1, Held: true},   // grows: need + a replica
		{Key: "node", Weight: 1, Need: 9, Floor: 16, ReplicaGPUs: 8, Held: true},    // whole-node replicas
		{Key: "free", Weight: 1, Need: 3.5, Floor: 10, ReplicaGPUs: 1, Held: false}, // a minReplicas floor: as before
	}
	want := map[string]int{"low": 10, "high": 13, "node": 24, "free": 11}
	for _, r := range ShareCapSpare(roles, 1) {
		if r.Ceiling != want[r.Key] {
			t.Errorf("%s ceiling %d, want %d", r.Key, r.Ceiling, want[r.Key])
		}
	}
}

// Capped, the targets leave the spare unassigned instead of spreading it.
func TestShareCapSpareLeavesTheSpareUnassigned(t *testing.T) {
	in := spareRun(2, 0.4, 3)
	integ := IntegerShareTargets(ShareCapSpare(in.Roles, 1), in.Budget)
	if integ["L"] != 3 || integ["Q"] != 2 {
		t.Fatalf("capped targets %v, want L=3 (need 2 + one spare), Q=2 (floor 1 + one spare)", integ)
	}
	uncapped := IntegerShareTargets([]ShareRole{
		{Key: "L", Weight: 1, Need: 2, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "Q", Weight: 1, Need: 0.4, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
	}, in.Budget)
	if uncapped["L"]+uncapped["Q"] != in.Budget {
		t.Fatalf("control: uncapped targets %v should spread the whole budget", uncapped)
	}
}

// A hard imbalance moves at the pace on its first cycle, and takes every
// replica the receiver is still short on the next: neither the per-cycle pace
// nor the concurrency limit holds it to two.
func TestShareHardImbalanceTakesTheWholeShortfall(t *testing.T) {
	plans := planEach(NewShareLedger(), spareRun(11, 0.4, 7), 2)
	first, second := qToL(plans[0].Started), qToL(plans[1].Started)
	if first < 1 || first > ShareMaxReplicasPerCycle {
		t.Fatalf("Q -> L moved %d in the first cycle, want 1..%d: one reading moves at the pace", first, ShareMaxReplicasPerCycle)
	}
	if first+second != 5 {
		t.Fatalf("Q -> L moved %d+%d over two cycles, want 5 (L from 3 to its ceiling of 8)", first, second)
	}
	for _, p := range plans {
		for _, tr := range p.Started {
			if !tr.SkippedWaits {
				t.Errorf("%s did not skip the waits", tr.ID)
			}
		}
	}
	// Two ordinary moves in flight -- the concurrency limit -- hold the first
	// cycle, not the second.
	l := NewShareLedger()
	in := spareRun(11, 0.4, 7)
	for range ShareMaxConcurrentTransfers {
		tr := l.Start(ShareTransfer{Donor: "Q", Receiver: "L", GPUs: 1, DonorGPUs: 1}, in.Held, time.Unix(900, 0), simTimings())
		l.ConfirmStarted(tr.ID, nil)
	}
	moved := 0
	for _, p := range planEach(l, in, 2) {
		moved += qToL(p.Started)
	}
	if moved != 3 {
		t.Fatalf("with %d in flight, Q -> L moved %d over two cycles, want the 3 still short", ShareMaxConcurrentTransfers, moved)
	}
}

// One cycle's reading -- a scrape glitch, a forged metric -- moves at most the
// pace, and the next cycle, the reading gone, cancels what it moved.
func TestShareOneReadingMovesAtThePace(t *testing.T) {
	l := NewShareLedger()
	p := PlanShareTransfers(l, spareRun(11, 0.4, 7), time.Unix(1000, 0), simTimings())
	if n := qToL(p.Started); n < 1 || n > ShareMaxReplicasPerCycle {
		t.Fatalf("the spike moved %d, want 1..%d", n, ShareMaxReplicasPerCycle)
	}
	// The reading gone, nothing more skips the waits. (With L's need back at
	// 1 the peaks fit the quota, so the spare is spread by weight again, and
	// what moves now is an ordinary, confirmed headroom move.)
	p = PlanShareTransfers(l, spareRun(1, 0.4, 7), time.Unix(1015, 0), simTimings())
	for _, tr := range p.Started {
		if tr.SkippedWaits {
			t.Fatalf("skipped the waits again after the reading went: %+v", tr)
		}
	}
}

// Control: waits not skipped, the same imbalance moves at the pace.
func TestShareOrdinaryMovesKeepThePace(t *testing.T) {
	in := spareRun(11, 0.4, 7)
	in.SkipWaits = ShareSkipWaits{Off: true}
	total := 0
	for i, p := range planEach(NewShareLedger(), in, 4) {
		n := qToL(p.Started)
		if n > ShareMaxReplicasPerCycle {
			t.Errorf("cycle %d moved %d, above the pace of %d", i+1, n, ShareMaxReplicasPerCycle)
		}
		total += n
	}
	if total == 0 || total > ShareMaxConcurrentTransfers {
		t.Fatalf("moved %d over four cycles, want 1..%d: the concurrency limit holds until releases land",
			total, ShareMaxConcurrentTransfers)
	}
}

// A receiver the idle fill can fund this cycle is not funded from a donor.
func TestShareLeavesTheIdleToTheFill(t *testing.T) {
	// L needs 4 holding 3: its target is 5. Two GPUs are unassigned.
	in := spareRun(4, 0.4, 5)
	for i, p := range planEach(NewShareLedger(), in, 3) {
		if n := qToL(p.Started); n > 0 {
			t.Fatalf("cycle %d: Q gave L %d replicas the idle fill funds", i+1, n)
		}
	}
	// Control: with no GPU physically free there is no idle, and Q gives.
	in.PhysicalFree = 0
	moved := 0
	for _, p := range planEach(NewShareLedger(), in, 3) {
		moved += qToL(p.Started)
	}
	if moved == 0 {
		t.Fatal("control: Q did not give L anything with nothing free")
	}
}

// With nobody short, a role holding more than its target for its scale-down
// window returns all of it to the quota at once, as a scale-down would, and
// not below its target.
func TestShareReturnsSpareWhenNobodyIsShort(t *testing.T) {
	l := NewShareLedger()
	plans := planEach(l, spareRun(2, 0.4, 7), 3)
	if plans[0].Returns != 0 {
		t.Fatalf("cycle 1 returned before the confirm cycles: %v", plans[0].Started)
	}
	p := plans[1]
	if p.Returns != 5 {
		t.Fatalf("returned %d, want 5 at once (7 down to its target of 2): %v", p.Returns, p.Started)
	}
	for _, tr := range p.Started {
		if tr.Donor != "Q" || tr.Receiver != "" || !tr.Returned || !tr.Entitled {
			t.Errorf("not a return from Q: %+v", tr)
		}
	}
	// Q's target is 2: across every cycle it returns 5 at most, and L, at
	// its target, returns nothing.
	l = NewShareLedger()
	burstHistory(l)
	returned := map[string]int{}
	held := map[string]int{"L": 3, "Q": 7}
	in := spareRun(2, 0.4, 7)
	for i := range 40 {
		now := time.Unix(int64(1000+15*i), 0)
		l.Observe(held, now, simTimings())
		l.TakeReleased()
		p := PlanShareTransfers(l, in, now, simTimings())
		for _, tr := range p.Started {
			if tr.Returned {
				returned[tr.Donor]++
				held[tr.Donor]-- // released at once
			}
		}
		in.Held = held
	}
	if returned["Q"] != 5 || returned["L"] != 0 {
		t.Fatalf("returned %v, want Q=5 (7 down to its target of 2) and L=0", returned)
	}
}

// The target bounds a return within a cycle where the calm load would not: Q,
// with a floor of 3 and a need of 1.15, has a target of 4 (its floor and one
// spare) yet is calm down to 3 (1.15 <= 0.6 x (4 - 2)). Holding 5, out of band
// at a threshold of 0.95, it returns one replica.
func TestShareReturnsDownToTheTargetOnly(t *testing.T) {
	in := spareRun(2, 1.15, 5)
	in.Roles = []ShareRole{
		{Key: "L", Weight: 1, Need: 2, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "Q", Weight: 1, Need: 1.15, Floor: 3, Ceiling: 8, ReplicaGPUs: 1},
	}
	in.Thresholds["Q"] = 0.95
	if integ := IntegerShareTargets(ShareCapSpare(in.Roles, 1), in.Budget); integ["Q"] != 4 {
		t.Fatalf("setup: Q's target %d, want 4", integ["Q"])
	}
	returned := 0
	for _, p := range planEach(NewShareLedger(), in, 4) {
		returned += p.Returns
	}
	if returned != 1 {
		t.Fatalf("Q returned %d, want 1: from 5 down to its target of 4, not below", returned)
	}
}

func TestShareDoesNotReturnWhileARoleIsShort(t *testing.T) {
	// L short of its need (7, holding 3), and nothing free: Q's surplus goes
	// to L by transfers, and nothing is returned while L is still short.
	in := spareRun(7, 0.4, 7)
	in.SkipWaits = ShareSkipWaits{Off: true}
	for i, p := range planEach(NewShareLedger(), in, 3) {
		if p.Returns != 0 {
			t.Fatalf("cycle %d returned %d while L was short", i+1, p.Returns)
		}
	}
}

// A role returns only while it would stay calm a replica below what it
// keeps: Q at a need of 3 holds 7 against a target of 4, yet returns one
// replica only -- at 5, a need of 3 is more than the calm load of the 4
// below it.
func TestShareReturnsOnlyWhileCalm(t *testing.T) {
	in := spareRun(2, 3, 7)
	if integ := IntegerShareTargets(ShareCapSpare(in.Roles, 1), in.Budget); integ["Q"] != 4 {
		t.Fatalf("setup: Q's target %d, want 4", integ["Q"])
	}
	total := 0
	for _, p := range planEach(NewShareLedger(), in, 6) {
		total += p.Returns
	}
	if total != 1 {
		t.Fatalf("Q returned %d replicas, want 1: 3 <= 0.6 x (6 - 1), and 3 > 0.6 x (5 - 1)", total)
	}
}

// A return is called off, while it is free, when its role's demand comes
// back: draining the pod would only make the fill start another.
func TestShareCancelsAReturnWhenDemandComesBack(t *testing.T) {
	l := NewShareLedger()
	in := spareRun(2, 0.4, 7)
	plans := planEach(l, in, 2)
	if plans[1].Returns == 0 {
		t.Fatal("setup: Q did not return")
	}
	returns := map[string]bool{}
	for _, tr := range plans[1].Started {
		returns[tr.ID] = tr.Returned
	}
	// Q's demand is back in full -- a need of 8 against the 2 the returns
	// leave it: every return is called off.
	p := PlanShareTransfers(l, spareRun(2, 8, 7), time.Unix(1030, 0), spareTimings())
	if len(p.Cancelled) != plans[1].Returns {
		t.Fatalf("cancelled %v, want all %d returns", p.Cancelled, plans[1].Returns)
	}
	for _, id := range p.Cancelled {
		if !returns[id] {
			t.Errorf("cancelled %s, not a return", id)
		}
	}
	// Back in part -- a need of 1.5, a target of 3: one return is called off,
	// and Q, at 3, is calm again (1.5 <= 0.6 x 3), so the others go on.
	l = NewShareLedger()
	plans = planEach(l, spareRun(2, 0.4, 7), 2)
	if p := PlanShareTransfers(l, spareRun(2, 1.5, 7), time.Unix(1030, 0), spareTimings()); len(p.Cancelled) != 1 {
		t.Fatalf("cancelled %v, want one of the %d returns", p.Cancelled, plans[1].Returns)
	}
	// Control: with Q's demand where it was, nothing is cancelled.
	l = NewShareLedger()
	planEach(l, spareRun(2, 0.4, 7), 3)
	if p := PlanShareTransfers(l, spareRun(2, 0.4, 7), time.Unix(1030, 0), spareTimings()); len(p.Cancelled) != 0 {
		t.Fatalf("control: cancelled %v with demand unchanged", p.Cancelled)
	}
}
