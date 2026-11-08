package allocation

import (
	"math"
	"strings"
	"testing"
)

// A need above a role's ceiling is judged against the ceiling, all its target
// can cover: R needs 9 under a ceiling of 8 and holds 7, one replica short of
// that ceiling. Judged against the 9, it was in band -- the target of 8 is
// below the need -- and never took its last replica. Control: at 8 it is.
func TestShareNeedAboveTheCeilingIsJudgedAtTheCeiling(t *testing.T) {
	roles := []ShareRole{
		{Key: "R", Weight: 1, Need: 9, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "O", Weight: 1, Need: 1, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
	}
	th := map[string]float64{"R": 0.85, "O": 0.85}
	verdict := func(held int) ShareRoleVerdict {
		ev := EvaluateShare(roles, map[string]int{"R": held, "O": 10 - held}, th, 10, 0.15)
		for _, v := range ev.Roles {
			if v.Key == "R" {
				return v
			}
		}
		t.Fatal("no verdict for R")
		return ShareRoleVerdict{}
	}
	if v := verdict(7); v.Integer != 8 || v.InBand || !v.Actionable {
		t.Fatalf("R at 7 of its ceiling 8, needing 9: integer %d, in band %v, actionable %v; want 8, false, true",
			v.Integer, v.InBand, v.Actionable)
	}
	if v := verdict(8); !v.InBand || v.Actionable {
		t.Fatalf("control: R at its ceiling: in band %v, actionable %v; want true, false", v.InBand, v.Actionable)
	}
}

// A need that is not a finite number freezes its model -- every share sum
// would take it -- and a finite one larger than any group is clamped to the
// largest budget, so two such needs cannot sum past the largest float and
// turn every target NaN.
func TestShareNeedsAreBoundedWhenGroupsAreBuilt(t *testing.T) {
	full16 := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 16}}}}
	huge := func(id string) ModelScalingRequest {
		req := shareReq(id, "ns", 1e305, 4)
		// A per-replica capacity of 1e-3: a need of 1.25e308 GPUs, finite.
		req.CompositeSignal.Result.VariantCapacities[0].PerReplicaCapacity = 1e-3
		return req
	}
	for _, c := range []struct {
		name   string
		reqs   []ModelScalingRequest
		frozen []string
	}{
		{"an infinite need", []ModelScalingRequest{shareReq("I", "ns", math.MaxFloat64, 4), shareReq("A", "ns", 4000, 12)}, []string{"ns/I"}},
		{"two needs past any group", []ModelScalingRequest{huge("H1"), huge("H2"), shareReq("A", "ns", 4000, 8)}, nil},
	} {
		groups := BuildShareGroups(c.reqs, full16, ShareGroupOptions{ClusterIsQuota: true})
		if len(groups) != 1 {
			t.Fatalf("%s: %d groups, want 1", c.name, len(groups))
		}
		g := groups[0]
		for _, want := range c.frozen {
			found := false
			for _, f := range g.Frozen {
				found = found || strings.Contains(f, want)
			}
			if !found {
				t.Errorf("%s: %s not frozen: frozen %v", c.name, want, g.Frozen)
			}
		}
		for _, r := range g.Roles {
			if math.IsNaN(r.Need) || math.IsInf(r.Need, 0) || r.Need > MaxShareBudgetGPUs {
				t.Errorf("%s: %s planned on a need of %v, want at most %d", c.name, r.Key, r.Need, MaxShareBudgetGPUs)
			}
		}
		for k, v := range ContinuousShareTargets(g.Roles, float64(g.Budget)) {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Errorf("%s: %s has target %v", c.name, k, v)
			}
		}
	}
}
