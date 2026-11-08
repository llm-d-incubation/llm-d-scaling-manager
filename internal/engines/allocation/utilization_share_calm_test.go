package allocation

import (
	"testing"
	"time"
)

// A swinging role at the crest of a burst is a hard imbalance on its reading:
// judged on its mean, it never skips the waits, and fills one replica a
// confirm at a time while it is short.
func TestShareSwingingReceiverSkipsTheWaits(t *testing.T) {
	in := spareRun(8, 0.5, 7)
	l := NewShareLedger()
	swingingL(l, 1)
	skipped := 0
	for i := range 3 {
		p := PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), spareTimings())
		for _, tr := range p.Started {
			if tr.Receiver == "L" && tr.SkippedWaits {
				skipped++
			}
		}
	}
	if skipped == 0 {
		t.Fatal("L, reading 8 on 3, never skipped the waits: judged on its mean")
	}
	if got := l.Committed(in.Held)["L"]; got < 7 {
		t.Fatalf("L committed %d after three cycles, want at least 7", got)
	}
}

// A donor set judges a swinging donor's calm as a single move does, on the
// higher of its reading and its mean: D reads 40 on a mean of 2, and a single
// move from it is refused, so the set that would take two whole nodes from it
// must be too.
func TestShareDonorSetJudgesASwingingDonorOnItsReading(t *testing.T) {
	tm := simTimings()
	t0 := time.Unix(100000, 0)
	v8 := ShareVariant{Name: "v", GPUs: 8, PodGPUs: []int{8}}
	lws := ShareVariant{Name: "l", GPUs: 16, PodGPUs: []int{8, 8}}
	l := NewShareLedger()
	for i := 60; i > 0; i-- {
		l.RecordNeeds(map[string]float64{"D": 2, "R": 40, "C": 1}, t0.Add(-time.Duration(30*i)*time.Second), tm)
	}
	l.recordMove("D", true, "x", t0.Add(-10*time.Second), tm)
	l.swingUntil["D"] = t0.Add(time.Hour)
	held := map[string]int{"R": 16, "D": 64, "C": 16}
	roles := []ShareRole{
		{Key: "R", Weight: 1, Need: 40, Floor: 16, Ceiling: 96, ReplicaGPUs: 16},
		{Key: "D", Weight: 1, Need: 40, Floor: 8, Ceiling: 96, ReplicaGPUs: 8},
		{Key: "C", Weight: 1, Need: 1, Floor: 16, Ceiling: 96, ReplicaGPUs: 8},
	}
	vs := map[string]ShareVariant{"R": lws, "D": v8, "C": v8}
	th := map[string]float64{"R": 0.8, "D": 0.8, "C": 0.8}
	in := SharePlanInput{Roles: roles, Held: held, Thresholds: th, Budget: 96, Tolerance: 0.15,
		Give: vs, Grow: vs, Headroom: ShareHeadroom{Capped: true, Replicas: 1}}
	for i := range 2 {
		p := PlanShareTransfers(l, in, t0.Add(time.Duration(30*i)*time.Second), tm)
		for _, tr := range p.Started {
			if tr.Donor == "D" {
				t.Fatalf("cycle %d: a set drained D, reading 40, as calm on its mean: %+v", i, tr)
			}
		}
	}
}
