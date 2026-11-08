package allocation

import (
	"slices"
	"testing"
	"time"
)

// A return records the shape of the replica it gives back, for returnsCover
// to judge: the give variant's pods, or one pod of the whole replica when
// they are not known.
func TestShareReturnRecordsItsShape(t *testing.T) {
	in := spareRun(2, 0.4, 7)
	in.Give = map[string]ShareVariant{"Q": {Name: "q", GPUs: 1}, "L": {Name: "l", GPUs: 1}}
	in.Grow = map[string]ShareVariant{"Q": {Name: "q", GPUs: 1}, "L": {Name: "l", GPUs: 1}}
	n := 0
	for _, p := range planEach(NewShareLedger(), in, 3) {
		for _, tr := range p.Started {
			if !tr.Returned {
				continue
			}
			n++
			if !slices.Equal(tr.DonorPodGPUs, []int{1}) {
				t.Fatalf("a return recorded pods %v, want [1]: %+v", tr.DonorPodGPUs, tr)
			}
		}
	}
	if n == 0 {
		t.Fatal("setup: Q returned nothing")
	}
}

// lwsReceiverPlan plans one cycle for R, short of its need (40 on 16) with a
// replica of two 8-GPU pods, beside D, which gives 1x8 replicas: no single
// donor replica fits R, so R is funded by a donor set -- unless returns D has
// started are already freeing a replica of R.
func lwsReceiverPlan(returns int) SharePlan {
	return lwsReceiverPlanShaped(returns, 96, []int{8})
}

// lwsReceiverPlanShaped is lwsReceiverPlan with R's ceiling at rCeiling and
// each return's pods shaped as shape.
func lwsReceiverPlanShaped(returns, rCeiling int, shape []int) SharePlan {
	tm := simTimings()
	t0 := time.Unix(100000, 0)
	l := NewShareLedger()
	held := map[string]int{"R": 16, "D": 64}
	roles := []ShareRole{
		{Key: "R", Weight: 1, Need: 40, Floor: 16, Ceiling: rCeiling, ReplicaGPUs: 16},
		{Key: "D", Weight: 1, Need: 2, Floor: 8, Ceiling: 96, ReplicaGPUs: 8},
	}
	th := map[string]float64{"R": 0.8, "D": 0.8}
	for range returns {
		l.Start(ShareTransfer{Donor: "D", DonorGPUs: 8, DonorVariant: "d", Entitled: true, Returned: true, DonorPodGPUs: shape},
			held, t0.Add(-30*time.Second), tm)
	}
	in := SharePlanInput{Roles: roles, Held: held, Thresholds: th, Budget: 80,
		Tolerance: 0.15, Headroom: ShareHeadroom{Capped: true, Replicas: 1},
		Give: map[string]ShareVariant{"D": {Name: "d", GPUs: 8, PodGPUs: []int{8}}},
		Grow: map[string]ShareVariant{"R": {Name: "r", GPUs: 16, PodGPUs: []int{8, 8}}}}
	return PlanShareTransfers(l, in, t0, tm)
}

// A receiver whose replica spans several pods waits on returns that will free
// a replica of it: they land in the unassigned GPUs, where the idle fill
// places its pods, and a donor set now would drain a donor for GPUs already
// on their way. Control: with nothing returning, a set funds it.
func TestShareLWSReceiverWaitsOnReturns(t *testing.T) {
	toR := func(p SharePlan) int {
		n := 0
		for _, tr := range p.Started {
			if tr.Receiver == "R" || (tr.SetID != "" && !tr.Returned) {
				n++
			}
		}
		return n
	}
	// R's ceiling of 32 leaves it one replica short: two returns of 8 cover
	// it, and no set drains D.
	p := lwsReceiverPlanShaped(2, 32, []int{8})
	if n := toR(p); n != 0 {
		t.Fatalf("a donor set started for R while 16 returned GPUs, all it lacks, release: %+v", p.Started)
	}
	if p.Withheld[ShareWithheldAwaitingReturn] == 0 || p.Unfunded["R"] != ShareUnfundedAwaitingReturn {
		t.Fatalf("R waits on the returns and nothing said so: withheld %v, unfunded %v", p.Withheld, p.Unfunded)
	}
	// Short of more than the returns cover, R is credited with them and a set
	// funds the rest: waiting its whole shortfall on one replica of returns
	// held it back a release.
	if p := lwsReceiverPlan(2); p.Withheld[ShareWithheldAwaitingReturn] == 0 || toR(p) == 0 {
		t.Fatalf("R, short past the returns, was not funded for the rest: withheld %v, started %+v", p.Withheld, p.Started)
	}
	// One return frees half a replica of R: not enough to wait on.
	if p := lwsReceiverPlan(1); p.Withheld[ShareWithheldAwaitingReturn] != 0 {
		t.Fatalf("R waited on 8 returned GPUs, half its replica: %v", p.Withheld)
	}
	// Returns of two 4-GPU pods free no node for an 8-GPU pod: nothing to
	// wait on.
	if p := lwsReceiverPlanShaped(2, 32, []int{4, 4}); p.Withheld[ShareWithheldAwaitingReturn] != 0 ||
		p.Unfunded["R"] == ShareUnfundedAwaitingReturn {
		t.Fatalf("R waited on 2x4 returns its 8-GPU pods do not fit: withheld %v, unfunded %v", p.Withheld, p.Unfunded)
	}
	if n := toR(lwsReceiverPlan(0)); n == 0 {
		t.Fatal("control: with nothing returning, no donor set funded R")
	}
}

// singlePodWaitPlan plans one cycle for R, a 1x8 role short of its need (14
// on 8), beside Q, which has one 8-GPU return still releasing, its pods shaped
// as shape (nil when not known).
func singlePodWaitPlan(shape []int) SharePlan {
	tm := simTimings()
	t0 := time.Unix(1000, 0)
	l := NewShareLedger()
	held := map[string]int{"R": 8, "Q": 24}
	roles := []ShareRole{
		{Key: "R", Weight: 1, Need: 14, Floor: 8, Ceiling: 16, ReplicaGPUs: 8},
		{Key: "Q", Weight: 1, Need: 2, Floor: 8, Ceiling: 64, ReplicaGPUs: 8},
	}
	tr := l.Start(ShareTransfer{Donor: "Q", DonorGPUs: 8, DonorVariant: "q", Entitled: true, Returned: true, DonorPodGPUs: shape},
		held, t0.Add(-30*time.Second), tm)
	l.ConfirmStarted(tr.ID, []string{"p"})
	vs := map[string]ShareVariant{"Q": {Name: "q", GPUs: 8, PodGPUs: []int{8}}, "R": {Name: "r", GPUs: 8, PodGPUs: []int{8}}}
	in := SharePlanInput{Roles: roles, Held: held, Thresholds: map[string]float64{"R": 0.8, "Q": 0.8}, Budget: 32,
		Tolerance: 0.15, Give: vs, Grow: vs, Headroom: ShareHeadroom{Capped: true, Replicas: 1}}
	return PlanShareTransfers(l, in, t0, tm)
}

// A single-pod receiver short by a replica waits on a return that will free
// it, as a multi-pod one does, and says so: it is credited, no donor drains
// for GPUs already on their way, and it shows awaiting-return. Controls: a 2x4
// return frees no node for its 1x8 pod, and a return of unknown shape counts
// for nothing; either way a donor funds it.
func TestShareSinglePodReceiverWaitsOnReturns(t *testing.T) {
	p := singlePodWaitPlan([]int{8})
	if p.Unfunded["R"] != ShareUnfundedAwaitingReturn || p.Withheld[ShareWithheldAwaitingReturn] == 0 {
		t.Fatalf("R waits on a 1x8 return and nothing said so: unfunded %v, withheld %v", p.Unfunded, p.Withheld)
	}
	for _, tr := range p.Started {
		if tr.Receiver == "R" {
			t.Fatalf("a donor drained for R while the return that covers it releases: %+v", tr)
		}
	}
	for _, shape := range [][]int{{4, 4}, nil} {
		p := singlePodWaitPlan(shape)
		if p.Unfunded["R"] == ShareUnfundedAwaitingReturn || p.Withheld[ShareWithheldAwaitingReturn] != 0 {
			t.Fatalf("return shaped %v: R waits on GPUs its pod cannot use: unfunded %v, withheld %v", shape, p.Unfunded, p.Withheld)
		}
	}
}

// A return is never redirected to a wake: it has no receiver whose GPUs a wake
// could take. Pinned on the ledger even for a return restored with a receiver,
// which the pod-mark validation rejects before it gets this far.
func TestShareReturnIsNeverRedirected(t *testing.T) {
	l := NewShareLedger()
	l.Restore(ShareTransfer{ID: "r1", Donor: "Q", Receiver: "L", GPUs: 1, DonorGPUs: 1, Entitled: true, Returned: true,
		Started: time.Unix(1000, 0), Deadline: time.Unix(2000, 0)}, 7)
	if _, ok := l.Redirect("r1", time.Minute); ok {
		t.Fatal("a return was redirected to a wake")
	}
	l.Restore(ShareTransfer{ID: "t1", Donor: "Q", Receiver: "L", GPUs: 1, DonorGPUs: 1,
		Started: time.Unix(1000, 0), Deadline: time.Unix(2000, 0)}, 7)
	if _, ok := l.Redirect("t1", time.Minute); !ok {
		t.Fatal("control: a transfer to L was not redirected")
	}
}

// What is released is counted once per receiver replica: a donor set whose two
// members together fund one replica leaves nothing unassigned; a transfer
// already filling has released what it gave; and a member that gives less than
// its receiver's replica takes nothing from another's excess.
func TestShareUnreleasedPerReceiverReplica(t *testing.T) {
	tm := spareTimings()
	t0 := time.Unix(1000, 0)
	held := map[string]int{"A": 8, "B": 8, "R": 8, "Q": 8}

	l := NewShareLedger()
	p := l.Start(ShareTransfer{Donor: "A", Receiver: "R", GPUs: 8, DonorGPUs: 4, SetID: "pending"}, held, t0, tm)
	c := l.Start(ShareTransfer{Donor: "B", DonorGPUs: 4, SetID: "pending"}, held, t0, tm)
	l.LinkSet(p.ID, c.ID)
	if n := l.Unreleased(); n != 0 {
		t.Fatalf("a set of 4 + 4 funding one 8-GPU replica leaves %d unreleased, want 0", n)
	}

	l = NewShareLedger()
	l.Start(ShareTransfer{Donor: "Q", Receiver: "R", GPUs: 4, DonorGPUs: 8}, held, t0, tm)
	if n := l.Unreleased(); n != 4 {
		t.Fatalf("setup: an 8-GPU replica funding a 4-GPU one leaves %d unreleased, want 4", n)
	}
	l.Observe(map[string]int{"A": 8, "B": 8, "R": 8, "Q": 0}, t0.Add(15*time.Second), tm)
	if tr := l.Transfers(); len(tr) != 1 || tr[0].State != ShareFilling {
		t.Fatalf("setup: want the transfer filling, got %+v", tr)
	}
	if n := l.Unreleased(); n != 0 {
		t.Fatalf("a filling transfer counts %d unreleased, want 0: its donor has released", n)
	}

	l = NewShareLedger()
	l.Start(ShareTransfer{Donor: "A", Receiver: "R", GPUs: 8, DonorGPUs: 4, SetID: "pending"}, held, t0, tm)
	l.Start(ShareTransfer{Donor: "Q", DonorGPUs: 4, Entitled: true, Returned: true}, held, t0, tm)
	if n := l.Unreleased(); n != 4 {
		t.Fatalf("unreleased %d, want 4: the return's, which a set primary's shortfall does not offset", n)
	}
}

// A return waits the confirm cycles in force, counted in cycles -- not its
// scale-down window: the release that follows waits that window again. With
// three confirm cycles and a 300 s window, Q returns on the third cycle, 30 s
// in.
func TestShareReturnWaitsTheConfirmCycles(t *testing.T) {
	tm := spareTimings()
	tm.Window = 300 * time.Second
	in := spareRun(2, 0.4, 7)
	in.ConfirmCycles = 3
	l := NewShareLedger()
	burstHistory(l)
	for i := range 3 {
		p := PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), tm)
		if i < 2 && p.Returns != 0 {
			t.Fatalf("cycle %d returned %d before three confirm cycles", i+1, p.Returns)
		}
		if i == 2 && p.Returns == 0 {
			t.Fatal("cycle 3 returned nothing after three confirm cycles")
		}
	}
}
