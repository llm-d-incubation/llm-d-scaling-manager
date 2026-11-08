package allocation

import (
	"testing"
	"time"
)

// podBusyRun plans spareRun(2, qNeed, qHeld) for n cycles 15 s apart, from
// well after burstHistory, on a 2-minute scale-down window, with busy(i) the
// busiest-queue reading in cycle i. It returns the cycle of Q's first return
// (-1: none) and how often each return reason was counted before it.
func podBusyRun(qNeed float64, qHeld int, busy func(i int) map[string]float64, n int) (int, map[string]int) {
	l := NewShareLedger()
	burstHistory(l)
	tm := spareTimings()
	tm.Window = 2 * time.Minute
	withheld := map[string]int{}
	for i := range n {
		in := spareRun(2, qNeed, qHeld)
		in.BusiestQueueLoad = busy(i)
		p := PlanShareTransfers(l, in, time.Unix(int64(2000+15*i), 0), tm)
		for k, v := range p.Withheld {
			withheld[k] += v
		}
		if p.Returns > 0 {
			return i, withheld
		}
	}
	return -1, withheld
}

func always(m map[string]float64) func(int) map[string]float64 {
	return func(int) map[string]float64 { return m }
}

// A role keeps its spare while one of its pods holds a queue above the return
// load of the saturation queue on the confirm cycles in a row, however calm
// the fleet reads -- but only for its donor's scale-down window: a pod queued
// for good, by routing a kept replica does not fix, would otherwise park the
// spare for good. Q holds 7 against a target of 2; unbusy, it returns on the
// second cycle. Busy throughout, it returns once the window has passed
// since the busy run began.
func TestShareReturnWaitsOnTheBusiestPodForAWindowAtMost(t *testing.T) {
	plain, _ := podBusyRun(0.4, 7, always(nil), 20)
	if plain != 1 {
		t.Fatalf("setup: Q returned first in cycle %d, want 1", plain)
	}
	first, withheld := podBusyRun(0.4, 7, always(map[string]float64{"Q": 2}), 20)
	if first < 0 {
		t.Fatal("Q never returned with one pod queued throughout: the spare is parked for good")
	}
	if first < 8 || first > 9 {
		t.Errorf("Q returned in cycle %d with one pod queued throughout, want 8 (120 s after the run began)", first)
	}
	if withheld[ShareWithheldReturnPodBusy] == 0 {
		t.Errorf("the held cycles counted no return-pod-busy: %v", withheld)
	}
}

// The hold needs the busy reading on the confirm cycles in a row: one reading
// -- a scrape, or a queue that cleared -- does not hold a return, and a
// reading every other cycle never builds a run.
func TestShareReturnWaitsOnlyOnAPersistentBusyPod(t *testing.T) {
	alternate := func(i int) map[string]float64 {
		if i%2 == 1 {
			return map[string]float64{"Q": 2}
		}
		return nil
	}
	if first, w := podBusyRun(0.4, 7, alternate, 20); first != 1 || w[ShareWithheldReturnPodBusy] != 0 {
		t.Errorf("Q returned in cycle %d (withheld %v) with a busy reading every other cycle, want cycle 1 and no hold", first, w)
	}
	// Control: busy from the cycle before, a run of two holds it.
	from0 := always(map[string]float64{"Q": 2})
	if first, _ := podBusyRun(0.4, 7, from0, 20); first == 1 {
		t.Error("control: busy on two cycles in a row did not hold the return")
	}
}

// Controls on the threshold and the role: a busiest pod at the return load
// is not busy, and a busy pod on L does not hold Q's return.
func TestShareReturnWaitsOnlyOnItsOwnBusyPod(t *testing.T) {
	if first, _ := podBusyRun(0.4, 7, always(map[string]float64{"Q": 0.6}), 20); first != 1 {
		t.Errorf("Q returned in cycle %d with its busiest pod at the return load, want 1", first)
	}
	if first, _ := podBusyRun(0.4, 7, always(map[string]float64{"L": 5}), 20); first != 1 {
		t.Errorf("Q returned in cycle %d with a busy pod on L, want 1", first)
	}
}

// A return the fleet's load would not let go keeps its own reason: Q, at a
// need of 3 holding 5 against a target of 4, is not calm a replica lower, so
// it counts return-not-calm whether or not a pod is busy, and never
// return-pod-busy.
func TestShareReturnPodBusyOnlyWhereTheFleetIsCalm(t *testing.T) {
	_, w := podBusyRun(3, 5, always(map[string]float64{"Q": 2}), 6)
	if w[ShareWithheldReturnPodBusy] != 0 || w[ShareWithheldReturnNotCalm] == 0 {
		t.Errorf("a return the fleet would not let go was counted %v; want return-not-calm only", w)
	}
}

// The group carries the deepest queue over the role's variants on its own
// accelerator; a role with no reading above zero is absent.
func TestShareGroupCarriesTheBusiestQueue(t *testing.T) {
	reqs := make([]ModelScalingRequest, 0, 2)
	for m, q := range map[string]float64{"A": 2.5, "B": 0} {
		r := shareReq(m, "ns", 1000, 1)
		r.CompositeSignal.Result.VariantBusiestQueueLoad = map[string]float64{m + "-v": q, "elsewhere": 9}
		reqs = append(reqs, r)
	}
	groups := BuildShareGroups(reqs, []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 10, Used: 3}}}},
		ShareGroupOptions{ClusterIsQuota: true})
	if len(groups) != 1 || len(groups[0].Roles) != 2 {
		t.Fatalf("setup: groups %+v, want one group of two roles", groups)
	}
	g := groups[0]
	if len(g.BusiestQueueLoad) != 1 {
		t.Fatalf("BusiestQueueLoad %v, want A's only", g.BusiestQueueLoad)
	}
	for k, v := range g.BusiestQueueLoad {
		if v != 2.5 {
			t.Fatalf("%s carries %v, want A's own variant's 2.5, not another variant's 9", k, v)
		}
	}
}

// A run of busy readings counts the cycles in a row since its first; a cycle
// not busy ends it, and the next busy cycle starts a new one.
func TestShareLedgerNotesARunOfBusyCycles(t *testing.T) {
	l := NewShareLedger()
	t0 := time.Unix(1000, 0)
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * 15 * time.Second) }
	l.NotePodBusy(map[string]bool{"Q": true}, at(0))
	if b := l.NotePodBusy(map[string]bool{"Q": true, "L": false}, at(1))["Q"]; b.cycles != 2 || !b.since.Equal(at(0)) {
		t.Fatalf("two busy cycles read %+v, want 2 since the first", b)
	}
	if _, ok := l.NotePodBusy(map[string]bool{"Q": false}, at(2))["Q"]; ok {
		t.Fatal("a cycle not busy did not end the run")
	}
	if b := l.NotePodBusy(map[string]bool{"Q": true}, at(3))["Q"]; b.cycles != 1 || !b.since.Equal(at(3)) {
		t.Fatalf("the run after a gap reads %+v, want 1 since its own first cycle", b)
	}
	if _, ok := l.NotePodBusy(nil, at(4))["Q"]; ok {
		t.Fatal("a role absent from the reading kept its run")
	}
}
