package allocation

import (
	"maps"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// heldPrefillGroup is issue ev-shindin/llm-scaling-manager#222's contended P/D quota of 16 1-GPU A100s, all
// in use. Model pd runs 10 prefill replicas and 4 decode; decode needs its 4.
// Prefill's demand before the hold is 2800, a need of 3.5 GPUs; with held set,
// saturation holds it at the bottom of the band (0.70 x 10 x 1000 = 7000).
// Model B holds 2 GPUs and needs 20.
func heldPrefillGroup(t *testing.T, held bool) ShareGroup {
	t.Helper()
	prefillDemand := 2800.0
	if held {
		prefillDemand = 0.70 * 10 * 1000
	}
	pd := withSatEntry(&satEntryFixture{
		ModelID:     "pd",
		Namespace:   "ns",
		TotalDemand: prefillDemand + 3200,
		RoleDemand:  map[string]float64{domain.RolePrefill: prefillDemand, domain.RoleDecode: 3200},
		VariantCapacities: []vcFixture{
			{VariantName: "pd-p", AcceleratorName: "A100", Role: domain.RolePrefill, Cost: 5, ReplicaCount: 10, PerReplicaCapacity: 1000},
			{VariantName: "pd-d", AcceleratorName: "A100", Role: domain.RoleDecode, Cost: 5, ReplicaCount: 4, PerReplicaCapacity: 1000},
		},
		RoleCapacities: map[string]domain.RoleCapacity{
			domain.RolePrefill: {Role: domain.RolePrefill, TotalDemand: prefillDemand},
			domain.RoleDecode:  {Role: domain.RoleDecode, TotalDemand: 3200},
		},
	}, ModelScalingRequest{
		ModelID:       "pd",
		Namespace:     "ns",
		Disaggregated: true,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "pd-p", CurrentReplicas: 10, GPUsPerReplica: 1, Role: domain.RolePrefill, MinReplicas: ptrInt(1)},
			{VariantName: "pd-d", CurrentReplicas: 4, GPUsPerReplica: 1, Role: domain.RoleDecode, MinReplicas: ptrInt(1)},
		},
	})
	pd.CompositeSignal.ScaleUpThreshold = 0.8
	if held {
		pd.CompositeSignal.Result.HeldRoleDemand = map[string]float64{domain.RolePrefill: 2800}
	}
	reqs := []ModelScalingRequest{pd, shareReq("B", "ns", 16000, 2)}
	groups := BuildShareGroups(reqs, []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 16}}}},
		ShareGroupOptions{ClusterIsQuota: true})
	if len(groups) != 1 || len(groups[0].Roles) != 3 {
		t.Fatalf("setup: groups %+v, want one group of three roles", groups)
	}
	return groups[0]
}

// planTargets plans g once, as actuation does, and returns each role's
// whole-replica target and the transfers started.
func planTargets(g ShareGroup) (map[string]int, []ShareTransfer) {
	p := PlanShareTransfers(NewShareLedger(), SharePlanInput{
		Roles: g.Roles, Held: g.Committed, Thresholds: g.Thresholds, DownThresholds: g.DownThresholds,
		Budget: g.Budget, Tolerance: 0.15, Give: g.Give, Grow: g.Grow, PhysicalFree: g.ClusterFree,
		Headroom: ShareHeadroom{Capped: true, Replicas: 1, ReturnLoad: 0.6}, Origins: g.Origins,
	}, time.Unix(100000, 0), simTimings())
	out := map[string]int{}
	for _, v := range p.Evaluation.Roles {
		out[v.Key] = v.Integer
	}
	return out, p.Started
}

// unreachable is the GPUs the targets promise receivers beyond what the group
// can give them: its free GPUs plus what every role that can give holds above
// its own target. A role with no Give keeps what it holds.
func unreachable(g ShareGroup, targets map[string]int) int {
	free := g.Budget
	for _, c := range g.Committed {
		free -= c
	}
	supply, promised := max(free, 0), 0
	for k, n := range targets {
		c := g.Committed[k]
		switch {
		case n > c:
			promised += n - c
		case n < c:
			if _, ok := g.Give[k]; ok {
				supply += c - n
			}
		}
	}
	return max(promised-supply, 0)
}

// Issue ev-shindin/llm-scaling-manager#222: a role saturation holds gives nothing, so the share must not
// plan its GPUs for another model. Prefill holds 10 GPUs and needs 3.5; sized
// on that need with a floor of one replica, its target was 3 and B was
// promised the 7 GPUs above it, which no transfer could take. Its floor is
// what it holds, as for a scale-down hold, so B is promised nothing the quota
// cannot give.
func TestShareHeldRoleKeepsWhatItHoldsInThePlan(t *testing.T) {
	g := heldPrefillGroup(t, true)
	prefill := "ns/pd/" + domain.RolePrefill
	var role ShareRole
	for _, r := range g.Roles {
		if r.Key == prefill {
			role = r
		}
	}
	if role.Need < 3.4 || role.Need > 3.6 {
		t.Fatalf("held prefill needs %v, want 3.5: the demand before the hold", role.Need)
	}
	if role.Floor != 10 {
		t.Errorf("held prefill's floor is %d, want the 10 GPUs it holds", role.Floor)
	}
	if role.MinFloor != 1 {
		t.Errorf("held prefill's MinFloor is %d, want the 1 its minReplicas holds", role.MinFloor)
	}
	if _, ok := g.Give[prefill]; ok {
		t.Fatal("held prefill can give")
	}
	targets, started := planTargets(g)
	if targets[prefill] < g.Committed[prefill] {
		t.Errorf("held prefill targets %d of the %d GPUs it keeps", targets[prefill], g.Committed[prefill])
	}
	if n := unreachable(g, targets); n != 0 {
		t.Errorf("targets %v promise %d GPUs no role can give", targets, n)
	}
	for _, tr := range started {
		if tr.Donor == prefill {
			t.Errorf("transfer %+v takes from the held role", tr)
		}
	}
}

// Control: the same prefill, not held, sizes on its need of 3.5 and gives the
// GPUs above it to B.
func TestShareUnheldRoleStillReleases(t *testing.T) {
	g := heldPrefillGroup(t, false)
	prefill := "ns/pd/" + domain.RolePrefill
	for _, r := range g.Roles {
		if r.Key == prefill && r.Floor != 1 {
			t.Errorf("unheld prefill's floor is %d, want one replica", r.Floor)
		}
	}
	if _, ok := g.Give[prefill]; !ok {
		t.Fatal("unheld prefill cannot give")
	}
	targets, started := planTargets(g)
	if targets[prefill] >= g.Committed[prefill] {
		t.Errorf("unheld prefill targets %d of %d: it should release", targets[prefill], g.Committed[prefill])
	}
	if b := "ns/B/" + domain.RoleBoth; targets[b] <= g.Committed[b] {
		t.Errorf("B targets %d holding %d: it should be given prefill's spare", targets[b], g.Committed[b])
	}
	if n := unreachable(g, targets); n != 0 {
		t.Errorf("targets %v promise %d GPUs no role can give", targets, n)
	}
	gave := false
	for _, tr := range started {
		gave = gave || tr.Donor == prefill
	}
	if !gave {
		t.Errorf("no transfer from unheld prefill: %+v", started)
	}
}

// heldAggregatedGroup is model A holding 10 1-GPU replicas with a need of 3.5
// beside B holding 2 and needing 2, in a quota of 16 with 4 GPUs free. A is
// held by saturation (HeldRoleDemand) or by the composite (NoScaleDown).
func heldAggregatedGroup(t *testing.T, noScaleDown bool) ShareGroup {
	t.Helper()
	a := shareReq("A", "ns", 2800, 10)
	if noScaleDown {
		a.CompositeSignal.Holds = map[string]RoleHold{domain.RoleBoth: {NoScaleDown: true}}
	} else {
		a = shareReq("A", "ns", 0.70*10*1000, 10)
		a.CompositeSignal.Result.RoleDemand = map[string]float64{domain.RoleBoth: 0.70 * 10 * 1000}
		a.CompositeSignal.Result.HeldRoleDemand = map[string]float64{domain.RoleBoth: 2800}
	}
	groups := BuildShareGroups([]ModelScalingRequest{a, shareReq("B", "ns", 1600, 2)},
		[]*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 12}}}},
		ShareGroupOptions{ClusterIsQuota: true})
	if len(groups) != 1 || len(groups[0].Roles) != 2 {
		t.Fatalf("setup: groups %+v, want one group of two roles", groups)
	}
	return groups[0]
}

// A held role keeps what it holds and is not raised past it while its need
// fits inside it, however the hold is expressed: the two plan alike.
func TestShareHeldRoleIsNotRaisedByItsHold(t *testing.T) {
	a := "ns/A/" + domain.RoleBoth
	var first map[string]int
	for _, nsd := range []bool{false, true} {
		g := heldAggregatedGroup(t, nsd)
		targets, _ := planTargets(g)
		if targets[a] != g.Committed[a] {
			t.Errorf("noScaleDown=%v: held A targets %d holding %d with a need of 3.5", nsd, targets[a], g.Committed[a])
		}
		if first == nil {
			first = targets
		} else if !maps.Equal(first, targets) {
			t.Errorf("HeldRoleDemand plans %v, NoScaleDown plans %v", first, targets)
		}
	}
}
