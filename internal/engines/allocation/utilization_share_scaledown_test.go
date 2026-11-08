package allocation

import (
	"math"
	"testing"
	"time"
)

// bandRun is L, needing 2 on lHeld, beside Q, needing 10 on qHeld, every
// GPU of the quota held. No peak is known yet, so the spare is capped and
// each role's target is its claim plus one replica: Q's is 11. At a
// scale-up threshold of 0.85 and a scale-down boundary of 0.65, today's
// scale-down leaves demand a quarter of the way up, at 0.70, which Q's
// reaches on 10 x 0.85 / 0.70 = 12.1 GPUs: it keeps 13.
func bandRun(lHeld, qHeld int, down map[string]float64) SharePlanInput {
	roles := []ShareRole{
		{Key: "L", Weight: 1, Need: 2, Floor: 1, Ceiling: 32, ReplicaGPUs: 1},
		{Key: "Q", Weight: 1, Need: 10, Floor: 1, Ceiling: 32, ReplicaGPUs: 1},
	}
	return SharePlanInput{Roles: roles, Held: map[string]int{"L": lHeld, "Q": qHeld},
		Thresholds: map[string]float64{"L": 0.85, "Q": 0.85}, DownThresholds: down,
		Budget: lHeld + qHeld, Tolerance: 0.15, PhysicalFree: math.MaxInt,
		Headroom: ShareHeadroom{Capped: true, Replicas: 1}}
}

// shareBoundaries are scale-down boundaries of 0.65, below a scale-up
// threshold of 0.85.
var shareBoundaries = map[string]float64{"L": 0.65, "Q": 0.65}

// bandCycles plans in for four cycles 15s apart on a fresh ledger, and
// returns Q's commitment and the scale-down-band withholdings.
func bandCycles(in SharePlanInput) (q, withheld int) {
	l := NewShareLedger()
	for i := range 4 {
		p := PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), spareTimings())
		withheld += p.Withheld[ShareWithheldScaleDownBand]
	}
	return l.Committed(in.Held)["Q"], withheld
}

// A transfer does not keep the band: L, at its need and short of its
// headroom, still gets its replica from Q at 13. A receiver at its need whose
// demand is still rising needs that replica within a move's latency.
func TestShareTransfersIgnoreTheScaleDownBand(t *testing.T) {
	if q, _ := bandCycles(bandRun(2, 13, shareBoundaries)); q >= 13 {
		t.Fatalf("Q kept %d from L's headroom: a transfer must not keep the band", q)
	}
}

// Without a scale-down boundary below the scale-up threshold there is no
// band: a role's boundary missing, or at its threshold, changes nothing.
func TestShareNoBandWithoutABoundaryBelowTheThreshold(t *testing.T) {
	for name, down := range map[string]map[string]float64{
		"missing":          {"L": 0.70},
		"at the threshold": {"L": 0.70, "Q": 0.85},
	} {
		if q, w := bandCycles(bandRun(2, 13, down)); q >= 13 || w != 0 {
			t.Errorf("%s: Q kept %d (withheld %d), want it to give L its headroom", name, q, w)
		}
		// Exactly no band: a missing boundary would divide by zero, and an
		// infinite float has no defined int.
		p := &sharePlanRun{in: bandRun(2, 13, down)}
		if k := p.bandKeep("Q", p.in.Roles[1]); k != 0 {
			t.Errorf("%s: Q's band keeps %d, want 0", name, k)
		}
	}
	p := &sharePlanRun{in: bandRun(2, 13, shareBoundaries)}
	if k := p.bandKeep("Q", p.in.Roles[1]); k != 13 {
		t.Errorf("control: Q's band keeps %d, want 13 (10 x 0.85 / 0.70, rounded up)", k)
	}
}

// The band keeps what today's scale-down keeps, not the boundary's GPUs: a
// role of 8-GPU replicas needing 20, between 0.70 and 0.85, keeps 3 replicas
// (20 x 0.85 / 0.7375 = 23.1), not the 4 that 20 x 0.85 / 0.70 = 24.3 rounds
// up to -- a whole node parked and withheld every cycle.
func TestShareBandKeepsWhatTodaysScaleDownKeeps(t *testing.T) {
	p := &sharePlanRun{in: SharePlanInput{
		Roles:          []ShareRole{{Key: "Q", Need: 20, Floor: 8, Ceiling: 64, ReplicaGPUs: 8}},
		Thresholds:     map[string]float64{"Q": 0.85},
		DownThresholds: map[string]float64{"Q": 0.70}}}
	if k := p.bandKeep("Q", p.in.Roles[0]); k != 24 {
		t.Fatalf("Q's band keeps %d GPUs, want 24: 3 replicas of 8", k)
	}
}

// A return is today's scale-down and goes no lower: at a return load of 0.95
// Q, on 14, would return two replicas to 12; it keeps 13 instead. Control:
// without a boundary it returns both.
func TestShareReturnStopsAtTheScaleDownBand(t *testing.T) {
	returns := func(down map[string]float64) (n, withheld int) {
		in := bandRun(3, 14, down)
		in.Headroom.ReturnLoad = 0.95
		l := NewShareLedger()
		for i := range 4 {
			p := PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), spareTimings())
			withheld += p.Withheld[ShareWithheldScaleDownBand]
			for _, tr := range p.Started {
				if tr.Returned && tr.Donor == "Q" {
					n++
				}
			}
		}
		return n, withheld
	}
	if n, w := returns(shareBoundaries); n != 1 || w == 0 {
		t.Fatalf("Q returned %d (withheld scale-down-band %d), want 1 and withheld: 14 down to its band of 13", n, w)
	}
	if n, _ := returns(nil); n != 2 {
		t.Fatalf("control: with no boundary Q returned %d, want 2", n)
	}
}

// The group carries each role's scale-down boundary from its composite
// signal, beside its scale-up threshold.
func TestShareGroupCarriesTheScaleDownBoundary(t *testing.T) {
	req := shareReq("A", "ns", 4000, 9)
	req.CompositeSignal.ScaleDownBoundary = 0.65
	groups := BuildShareGroups([]ModelScalingRequest{req},
		[]*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 9}}}},
		ShareGroupOptions{ClusterIsQuota: true})
	if len(groups) != 1 || len(groups[0].Roles) != 1 {
		t.Fatalf("groups %+v, want one group of one role", groups)
	}
	g := groups[0]
	if got := g.DownThresholds[g.Roles[0].Key]; got != 0.65 {
		t.Fatalf("scale-down boundary %v, want 0.65", got)
	}
}
