package steadystate

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// publishedGPUs is what the overrides ask the scale targets for, in GPUs.
func publishedGPUs(e *Engine, g allocation.ShareGroup, key func(string, string) string) int {
	gpus := map[string]int{}
	for role, vs := range g.Variants {
		for _, v := range vs {
			gpus[key(role, v.Name)] = v.GPUs
		}
	}
	total := 0
	for k, o := range e.shareOverrides(g, key, "test") {
		total += o.Target * gpus[k]
	}
	return total
}

// A target the share published and a role does not hold -- its Deployment
// scaled down from outside, a raise whose pods are not placed yet -- is not
// idle: the scale target takes those GPUs back as soon as it reaches the
// target. Filling them gives one GPU to two models, and the published
// targets ask for more than the quota.
//
// The shape a benchmark reset produced on a 7-GPU namespace quota: B (1 GPU
// per replica) published at 4 but scaled to 1 from outside, A (2 GPUs per
// replica) at 1 and short. Control: B published at what it holds, and A takes
// the idle GPUs.
func TestShareFillChargesATargetAboveWhatARoleHolds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		targetB int
		wantA   int
	}{
		{"B published at 4, holds 1", 4, 1},
		{"B published at what it holds (control)", 1, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := allocation.ShareGroup{
				AcceleratorType: "H200", Budget: 7, ClusterFree: math.MaxInt,
				Roles: []allocation.ShareRole{
					{Key: "A", Weight: 1, Need: 6, Ceiling: 6, ReplicaGPUs: 2},
					{Key: "B", Weight: 1, Need: 0, Ceiling: 4, ReplicaGPUs: 1},
				},
				Grow: map[string]allocation.ShareVariant{"A": {Name: "a", GPUs: 2}},
				Variants: map[string][]allocation.ShareVariant{
					"A": {{Name: "a", GPUs: 2, Current: 1, Min: 1, Max: 3}},
					"B": {{Name: "b", GPUs: 1, Current: 1, Min: 1, Max: 4}},
				},
			}
			ev := allocation.ShareEvaluation{Roles: []allocation.ShareRoleVerdict{{Key: "A", Integer: 6}, {Key: "B", Integer: 1}}}
			e := &Engine{}
			e.utilizationShare.desired = map[string]int{"A/a": 1, "B/b": tc.targetB}
			key := func(role, variant string) string { return role + "/" + variant }
			e.fillIdleShare(logr.Discard(), allocation.NewShareLedger(), g, ev, map[string]int{"A": 2, "B": 1},
				shareFit{}, key, time.Unix(1000, 0), allocation.ShareTimings{FillTimeout: time.Hour})
			if got := e.utilizationShare.desired["A/a"]; got != tc.wantA {
				t.Errorf("A at %d, want %d", got, tc.wantA)
			}
			if got := publishedGPUs(e, g, key); got > g.Budget {
				t.Fatalf("the overrides ask for %d GPUs on a budget of %d", got, g.Budget)
			}
		})
	}
}

// The same through whole cycles: a fleet at the quota, settled, has one
// model's Deployment scaled down from outside, and while its pods stay gone
// the targets the share publishes never ask for more than the quota.
func TestShareTargetsStayInTheQuotaWhenAModelIsScaledDownFromOutside(t *testing.T) {
	f := newShareFleet()
	// Every model at its need: nothing to move.
	f.demand = map[string]float64{"A": 9 * 800, "B": 5 * 800, "C": 2 * 800}
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	quota := func() []*allocation.ResourceConstraints {
		used := f.current["A"] + f.current["B"] + f.current["C"]
		return []*allocation.ResourceConstraints{{Pools: map[string]allocation.ResourcePool{"A100": {Limit: 16, Used: used}}}}
	}
	cycle := func() map[string]utilizationShareOverride {
		se.clock = se.clock.Add(30 * time.Second)
		return se.e.evaluateUtilizationShare(se.ctx, f.requests(), quota(), f.scaleTargets())
	}
	sum := func(o map[string]utilizationShareOverride) int {
		return targetOf(t, o, "ns/A-v") + targetOf(t, o, "ns/B-v") + targetOf(t, o, "ns/C-v")
	}
	var o map[string]utilizationShareOverride
	for range 20 {
		o = cycle()
	}
	if got := sum(o); got != 16 {
		t.Fatalf("setup: settled targets sum to %d, want the 16 the fleet holds (%v)", got, o)
	}
	for _, l := range se.e.utilizationShare.ledgers {
		if n := len(l.Transfers()); n != 0 {
			t.Fatalf("setup: %d transfers in flight once settled, want none", n)
		}
	}

	// B's Deployment is scaled from 5 to 1 by hand; its target stays 5.
	f.current["B"] = 1
	for i := 1; i < 5; i++ {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: fmt.Sprintf("B-v-%d", i)}}
		if err := c.Delete(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 4 {
		o = cycle()
		if got := sum(o); got > 16 {
			t.Fatalf("cycle %d after the outside scale-down: targets A=%d B=%d C=%d ask for %d GPUs of a 16-GPU quota",
				i+1, targetOf(t, o, "ns/A-v"), targetOf(t, o, "ns/B-v"), targetOf(t, o, "ns/C-v"), got)
		}
	}
}
