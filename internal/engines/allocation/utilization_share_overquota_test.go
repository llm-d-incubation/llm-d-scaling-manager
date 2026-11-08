package allocation

import (
	"testing"
	"time"
)

// overQuotaReq is shareReq with replicas of gpus GPUs each.
func overQuotaReq(id, ns string, demand float64, replicas, gpus int) ModelScalingRequest {
	req := shareReq(id, ns, demand, replicas)
	for i := range req.VariantStates {
		req.VariantStates[i].GPUsPerReplica = gpus
	}
	return req
}

// overQuotaGroup builds the one group two models A and B form under cons,
// holding a and b replicas of gpus GPUs each, both short of their need.
func overQuotaGroup(t *testing.T, cons []*ResourceConstraints, opts ShareGroupOptions, ns string, a, b, gpus int) ShareGroup {
	t.Helper()
	reqs := []ModelScalingRequest{
		overQuotaReq("A", ns, 8000, a, gpus),
		overQuotaReq("B", ns, 6000, b, gpus),
	}
	groups := BuildShareGroups(reqs, cons, opts)
	if len(groups) != 1 || len(groups[0].Roles) != 2 {
		t.Fatalf("setup: groups %+v, want one group of two roles", groups)
	}
	return groups[0]
}

// overQuotaPlan plans g once, as actuation does, and returns the plan and the
// GPUs the idle fill could place after it.
func overQuotaPlan(g ShareGroup) (SharePlan, int) {
	l := NewShareLedger()
	in := SharePlanInput{
		Roles: g.Roles, Held: g.Committed, Thresholds: g.Thresholds, DownThresholds: g.DownThresholds,
		Budget: g.Budget, Tolerance: 0.15, Give: g.Give, Grow: g.Grow, PhysicalFree: g.ClusterFree,
		Headroom: ShareHeadroom{Capped: true, Replicas: 1, ReturnLoad: 0.6}, Origins: g.Origins,
	}
	p := PlanShareTransfers(l, in, time.Unix(100000, 0), simTimings())
	return p, shareIdle(l, in, l.Committed(g.Committed))
}

// A scope holding more GPUs than its quota allows has its free figure floored
// at 0, so a budget of free plus what it holds was what it held: under a quota
// of 7, a namespace holding 10 was planned over 10, its overage treated as
// normal. The budget is the quota; the overage is a debt the planner pays back,
// with no fill and no growth.
func TestShareBudgetOverQuota(t *testing.T) {
	nsQuota := func(limit, used int) []*ResourceConstraints {
		return []*ResourceConstraints{{NamespaceScoped: true, NamespacePools: map[string]map[string]ResourcePool{
			"ns": {"A100": {Limit: limit, Used: used}},
		}}}
	}
	clusterQuota := func(limit, used int) []*ResourceConstraints {
		return []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: limit, Used: used}}}}
	}
	cases := []struct {
		name       string
		cons       []*ResourceConstraints
		opts       ShareGroupOptions
		a, b, gpus int
		budget     int
		over       int
	}{
		{name: "namespace quota 7, holding 10", cons: nsQuota(7, 10), a: 6, b: 4, gpus: 1, budget: 7, over: 3},
		{name: "namespace quota 7, holding 6 (control)", cons: nsQuota(7, 6), a: 4, b: 2, gpus: 1, budget: 7},
		{name: "namespace quota 7, holding 7 (control)", cons: nsQuota(7, 7), a: 4, b: 3, gpus: 1, budget: 7},
		{name: "namespace quota 8, holding 12 in 2-GPU replicas", cons: nsQuota(8, 12), a: 4, b: 2, gpus: 2, budget: 8, over: 4},
		{name: "namespace quota 8, holding 6 in 2-GPU replicas (control)", cons: nsQuota(8, 6), a: 2, b: 1, gpus: 2, budget: 8},
		{name: "cluster quota 7, holding 10", cons: clusterQuota(7, 10), opts: ShareGroupOptions{ClusterIsQuota: true},
			a: 6, b: 4, gpus: 1, budget: 7, over: 3},
		{name: "cluster quota 7, holding 6 (control)", cons: clusterQuota(7, 6), opts: ShareGroupOptions{ClusterIsQuota: true},
			a: 4, b: 2, gpus: 1, budget: 7},
		{name: "cluster quota 8, holding 12 in 2-GPU replicas", cons: clusterQuota(8, 12), opts: ShareGroupOptions{ClusterIsQuota: true},
			a: 4, b: 2, gpus: 2, budget: 8, over: 4},
		// The physical inventory is not a quota: what it counts past its
		// capacity is a stale read, not an allowance spent, and is not judged.
		{name: "physical inventory 7, counting 10 (not a quota)", cons: clusterQuota(7, 10), opts: ShareGroupOptions{PhysicalGroups: true},
			a: 6, b: 4, gpus: 1, budget: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := "ns"
			g := overQuotaGroup(t, tc.cons, tc.opts, ns, tc.a, tc.b, tc.gpus)
			if g.Budget != tc.budget || g.OverQuota != tc.over {
				t.Fatalf("budget %d over %d, want budget %d over %d", g.Budget, g.OverQuota, tc.budget, tc.over)
			}
			held := (tc.a + tc.b) * tc.gpus
			p, idle := overQuotaPlan(g)
			if tc.over == 0 {
				if p.ReserveDebt != 0 {
					t.Fatalf("within the quota: reserve debt %d, want 0", p.ReserveDebt)
				}
				return
			}
			if idleClaim(g) != 0 || idle > 0 {
				t.Fatalf("over the quota: idle claim %d, idle after planning %d, want none", idleClaim(g), idle)
			}
			if p.ReserveDebt != held-tc.budget {
				t.Fatalf("reserve debt %d, want the %d held past the quota", p.ReserveDebt, held-tc.budget)
			}
			if p.Refills == 0 {
				t.Fatal("nothing started to give the overage back")
			}
			targets := 0
			for _, v := range p.Evaluation.Roles {
				targets += v.Integer
			}
			if targets > tc.budget {
				t.Fatalf("the targets sum to %d GPUs, past the quota's %d", targets, tc.budget)
			}
			for _, s := range p.Started {
				if s.Receiver != "" {
					t.Fatalf("transfer %+v raises %s while the scope is over its quota", s, s.Receiver)
				}
			}
		})
	}
}

// A namespace that counts cohort borrowing holds borrowed GPUs past its
// guaranteed figure, and those are not over its quota (GuaranteedOnly): only
// what it holds past the borrow cap is.
func TestShareBudgetOverBorrowCap(t *testing.T) {
	borrowing := func(used, cap int) []*ResourceConstraints {
		c := borrowQuota(8, nsBorrow{ns: "x", guaranteed: 8, used: used, cap: cap, wn: 8})
		c.NamespacePools["x"] = map[string]ResourcePool{"A100": {Limit: 8, Used: used}}
		c.NamespaceBorrow["x"] = map[string]NamespaceBorrow{"A100": {Cap: cap, WithinNominal: 8}}
		c.Pools = reachAggregate(reachPools(c.NamespacePools, c.NamespaceBorrow))
		return GuaranteedOnly([]*ResourceConstraints{c})
	}
	for _, tc := range []struct {
		name            string
		used, cap, a, b int
		budget, over    int
	}{
		{name: "12 held, cap 4: all borrowed", used: 12, cap: 4, a: 8, b: 4, budget: 12},
		{name: "12 held, cap 2: 2 past the cap", used: 12, cap: 2, a: 8, b: 4, budget: 10, over: 2},
		{name: "12 held, unlimited cap", used: 12, cap: -1, a: 8, b: 4, budget: 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := overQuotaGroup(t, borrowing(tc.used, tc.cap), ShareGroupOptions{}, "x", tc.a, tc.b, 1)
			if g.Budget != tc.budget || g.OverQuota != tc.over {
				t.Fatalf("budget %d over %d, want budget %d over %d", g.Budget, g.OverQuota, tc.budget, tc.over)
			}
		})
	}
}
