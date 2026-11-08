package allocation

import (
	"testing"
	"time"
)

// mixedStepRun is a quota of 16 split between D, holding 12 in 4-GPU steps but
// giving from an 8-GPU variant, and R, holding 4 and growing by 8: both
// targets are 8, so D is above its target and R below. One of D's replicas
// is twice its target's step.
func mixedStepRun() SharePlanInput {
	return SharePlanInput{
		Roles: []ShareRole{
			{Key: "D", Weight: 1, Need: 3.5, Floor: 4, Ceiling: 64, ReplicaGPUs: 4},
			{Key: "R", Weight: 1, Need: 6, Floor: 4, Ceiling: 64, ReplicaGPUs: 4},
		},
		Held:       map[string]int{"D": 12, "R": 4},
		Thresholds: map[string]float64{"D": 0.8, "R": 0.8},
		Budget:     16,
		Tolerance:  0.15,
		Give:       map[string]ShareVariant{"D": {Name: "d", GPUs: 8, PodGPUs: []int{8}}},
		Grow:       map[string]ShareVariant{"R": {Name: "r", GPUs: 8, PodGPUs: []int{8}}},
	}
}

// A donor never gives below its whole-replica target: D's 8-GPU replica would
// leave it 4 of its 8, though D, nearly idle, would still read in its
// continuous band. Taken, D is short and R above its own target, and the
// next plan moves the GPUs back.
func TestShareDonorKeepsItsTargetWhenAReplicaIsTwoSteps(t *testing.T) {
	l := NewShareLedger()
	in := mixedStepRun()
	for i := range 6 {
		p := PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), simTimings())
		for _, tr := range p.Started {
			if tr.Donor == "D" {
				t.Fatalf("cycle %d: D gave %d of its 12 against a target of 8: %+v", i, tr.DonorGPUs, tr)
			}
		}
	}
}

// Control for the test above: a donor whose replica is one step of its target
// does give, down to that target.
func TestShareDonorGivesAOneStepReplica(t *testing.T) {
	l := NewShareLedger()
	in := mixedStepRun()
	in.Give = map[string]ShareVariant{"D": {Name: "d", GPUs: 4, PodGPUs: []int{4}}}
	in.Grow = map[string]ShareVariant{"R": {Name: "r", GPUs: 4, PodGPUs: []int{4}}}
	for i := range 6 {
		for _, tr := range PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), simTimings()).Started {
			if tr.Donor == "D" && tr.Receiver == "R" {
				return
			}
		}
	}
	t.Fatal("D, 12 against a target of 8 in 4-GPU steps, never gave R a replica")
}

// landEach plans n cycles, each on a fresh ledger with its moves landed at
// once, so no hold, confirm cycle or release in flight decides anything: only
// the targets, the keep and the admission do. It returns every move started,
// in order, and fails the test on the first that gives back to the role that
// gave to it.
func landEach(t *testing.T, in SharePlanInput, n int) []ShareTransfer {
	t.Helper()
	in.ConfirmCycles = 1
	var all []ShareTransfer
	gave := map[string]string{}
	for i := range n {
		for _, tr := range PlanShareTransfers(NewShareLedger(), in, time.Unix(int64(1000+15*i), 0), simTimings()).Started {
			if tr.Receiver == "" {
				continue
			}
			if gave[tr.Receiver] == tr.Donor {
				t.Fatalf("cycle %d: %s gives back to %s, which gave to it (held %v)", i, tr.Donor, tr.Receiver, in.Held)
			}
			gave[tr.Donor] = tr.Receiver
			all = append(all, tr)
			held := map[string]int{}
			for k, v := range in.Held {
				held[k] = v
			}
			held[tr.Donor] -= tr.DonorGPUs
			held[tr.Receiver] += tr.GPUs
			in.Held = held
		}
	}
	return all
}

// Moves never reverse while demand holds: every move goes from a role above
// its target to one below its own and leaves the donor at or above its
// target, so a receiver is never above its target while its donor is below.
// D, 16 in 4-GPU steps on a need of 3.5, gives R, short on 4 of 6, until both
// sit at their targets, and nothing comes back.
func TestShareMovesDoNotReverseUnderSteadyDemand(t *testing.T) {
	in := mixedStepRun()
	in.Budget = 20
	in.Held = map[string]int{"D": 16, "R": 4}
	in.Give = map[string]ShareVariant{"D": {Name: "d", GPUs: 4, PodGPUs: []int{4}}, "R": {Name: "r", GPUs: 4, PodGPUs: []int{4}}}
	in.Grow = in.Give
	if moves := landEach(t, in, 10); len(moves) == 0 {
		t.Fatal("setup: nothing moved, so nothing could reverse")
	}
}

// A move the targets allow is still refused when it leaves the pair's
// worst-off role no better (§6.2 admission): D, 12 against a ceiling of 8 on a
// need of 2, would give R, 4 on a need of 1, a replica of headroom, and
// both would end where D starts, at a headroom of 3. Moved, the GPUs go to a
// role that does not need them; the next change of demand moves them again.
func TestShareMoveImprovesTheWorstOff(t *testing.T) {
	in := mixedStepRun()
	in.Roles = []ShareRole{
		{Key: "D", Weight: 1, Need: 2, Floor: 4, Ceiling: 8, ReplicaGPUs: 4},
		{Key: "R", Weight: 1, Need: 1, Floor: 4, Ceiling: 64, ReplicaGPUs: 4},
	}
	in.Give = map[string]ShareVariant{"D": {Name: "d", GPUs: 4, PodGPUs: []int{4}}}
	in.Grow = map[string]ShareVariant{"R": {Name: "r", GPUs: 4, PodGPUs: []int{4}}}
	if moves := landEach(t, in, 4); len(moves) != 0 {
		t.Fatalf("a move that leaves the worst-off no better started: %+v", moves[0])
	}
}
