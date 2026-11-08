package steadystate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// fragFleet is a whole-node receiver G (one replica is one 8-GPU pod) beside
// two idle small models S1 and S2 of 1-GPU pods, five each, on 8-GPU nodes.
// G fills n1. Where the small pods run is the layout under test; every other
// GPU of their nodes is held by pods the scaling manager does not plan. The
// quota (18) is spent, and covers a second G replica once S1 and S2 are at
// their floors of one.
type fragFleet struct {
	held  map[string]int
	nodes map[string]allocation.ShareNode
	// units is each small model's unmarked pods, as the engine offers them.
	units map[string][]allocation.ShareUnit
	// marked is each started transfer's pod, until it is released.
	marked map[string]allocation.SharePod
	owner  map[string]string
}

// newFragFleet lays S1's and S2's five pods out on the nodes named, in order.
func newFragFleet(s1, s2 []string) *fragFleet {
	f := &fragFleet{
		held:   map[string]int{"G": 8, "S1": 5, "S2": 5},
		nodes:  map[string]allocation.ShareNode{"n1": {}, "n2": {}, "n3": {}, "n4": {}},
		units:  map[string][]allocation.ShareUnit{},
		marked: map[string]allocation.SharePod{},
		owner:  map[string]string{},
	}
	for role, on := range map[string][]string{"S1": s1, "S2": s2} {
		for i, n := range on {
			f.units[role] = append(f.units[role], allocation.ShareUnit{Pods: []allocation.SharePod{
				{Name: fmt.Sprintf("ns/%s-%d", strings.ToLower(role), i), Node: n, GPUs: 1}}})
		}
	}
	return f
}

func (f *fragFleet) group() allocation.ShareGroup {
	v := func(name string, gpus int) allocation.ShareVariant {
		return allocation.ShareVariant{Name: name, GPUs: gpus, PodGPUs: []int{gpus}}
	}
	return allocation.ShareGroup{
		Roles: []allocation.ShareRole{
			{Key: "G", Weight: 1, Need: 16, Floor: 8, MinFloor: 8, Ceiling: 16, ReplicaGPUs: 8},
			{Key: "S1", Weight: 1, Floor: 1, MinFloor: 1, Ceiling: 8, ReplicaGPUs: 1},
			{Key: "S2", Weight: 1, Floor: 1, MinFloor: 1, Ceiling: 8, ReplicaGPUs: 1},
		},
		Origins: map[string]allocation.ShareRoleOrigin{
			"G": {Namespace: "ns", ModelID: "g"}, "S1": {Namespace: "ns", ModelID: "s1"}, "S2": {Namespace: "ns", ModelID: "s2"}},
		Committed:  f.held,
		Budget:     18,
		Thresholds: map[string]float64{"G": 0.8, "S1": 0.8, "S2": 0.8},
		Give:       map[string]allocation.ShareVariant{"G": v("g", 8), "S1": v("s1", 1), "S2": v("s2", 1)},
		Grow:       map[string]allocation.ShareVariant{"G": v("g", 8), "S1": v("s1", 1), "S2": v("s2", 1)},
	}
}

// mark takes a pod from each transfer started this cycle out of the donor's
// offered units: the planned ones for a node-aware set, else the last.
func (f *fragFleet) mark(started []allocation.ShareTransfer) {
	for _, t := range started {
		if t.Donor == "" || t.Donor == "G" {
			continue
		}
		us := f.units[t.Donor]
		i := len(us) - 1
		if len(t.PlannedPods) > 0 {
			i = slices.IndexFunc(us, func(u allocation.ShareUnit) bool { return u.Pods[0].Name == t.PlannedPods[0] })
		}
		f.marked[t.ID] = us[i].Pods[0]
		f.owner[t.ID] = t.Donor
		f.units[t.Donor] = slices.Delete(us, i, i+1)
	}
}

// release lets each marked pod go once its transfer is a minute old: its GPU
// comes free on its node.
func (f *fragFleet) release(l *allocation.ShareLedger, now time.Time) {
	for _, t := range l.Transfers() {
		p, ok := f.marked[t.ID]
		if !ok || t.State != allocation.ShareReleasing || now.Sub(t.Started) < time.Minute {
			continue
		}
		delete(f.marked, t.ID)
		n := f.nodes[p.Node]
		n.Free += p.GPUs
		f.nodes[p.Node] = n
		f.held[f.owner[t.ID]] -= p.GPUs
	}
}

// pods is each giving role's GPUs by node, marked pods included, as
// shareGroupPodGPUs lists them. G's pod is on n1.
func (f *fragFleet) pods() map[string]map[string]int {
	out := map[string]map[string]int{"G": {"n1": 8}, "S1": {}, "S2": {}}
	for role, us := range f.units {
		for _, u := range us {
			out[role][u.Pods[0].Node] += u.Pods[0].GPUs
		}
	}
	for id, p := range f.marked {
		out[f.owner[id]][p.Node] += p.GPUs
	}
	return out
}

// run plans cycles as the engine does -- observe, plan, mark, judge the nodes,
// then the reasons -- and returns G's reasons by cycle and whether a transfer
// to G started.
func (f *fragFleet) run(cycles int) ([][]string, bool) {
	l := allocation.NewShareLedger()
	tm := allocation.ShareTimings{Window: 300 * time.Second, ReleaseTimeout: 6 * time.Minute,
		FillTimeout: 2 * time.Minute, ReversalHold: 10 * time.Minute, SwingWindow: time.Hour}
	t0 := time.Unix(1_000_000, 0)
	seq := make([][]string, 0, cycles)
	funded := false
	for c := range cycles {
		now := t0.Add(time.Duration(c) * 30 * time.Second)
		f.release(l, now)
		g := f.group()
		l.Observe(g.Committed, now, tm)
		l.TakeReleased()
		plan := allocation.PlanShareTransfers(l, allocation.SharePlanInput{
			Roles: g.Roles, Held: g.Committed, Thresholds: g.Thresholds, Budget: g.Budget, Tolerance: 0.15,
			Give: g.Give, Grow: g.Grow, Nodes: f.nodes, DonorUnits: f.units,
			Headroom: allocation.ShareHeadroom{Capped: true, Replicas: 1, ReturnLoad: 0.6}, Origins: g.Origins,
		}, now, tm)
		f.mark(plan.Started)
		funded = funded || slices.ContainsFunc(plan.Started, func(t allocation.ShareTransfer) bool { return t.Receiver == "G" })
		unfunded := markNoNodeFits(plan.Unfunded, shareNoNodeFits(l, g, plan.Evaluation, f.nodes, f.pods(), nil, nil))
		got := shareBlockedReasons(l, g, plan.Evaluation, unfunded, plan.MeanCapped, allocation.ShareSkipWaits{}, now, tm)
		seq = append(seq, got["ns/g"])
	}
	return seq, funded
}

// The issue #236 layout: S1 and S2 hold four pods each on n2 and n3 beside
// four GPUs of other pods, and one each on n4 beside six. No node can be
// emptied for G. S1 and S2 are idle, but G is the only role short and no node
// can hold its replica, so none of their spare is returned: they stay at 5.
// G once reported no-compatible-donor while they held spare, nothing while
// their returns released, then donors-at-floor -- the cause never changed.
// It must carry one reason, the same every cycle.
func TestUtilizationShareUnplaceableReceiverKeepsOneReason(t *testing.T) {
	f := newFragFleet([]string{"n2", "n2", "n2", "n2", "n4"}, []string{"n3", "n3", "n3", "n3", "n4"})
	seq, funded := f.run(12)
	lines := make([]string, 0, len(seq))
	for c, r := range seq {
		lines = append(lines, fmt.Sprintf("c%d %v", c, r))
	}
	t.Logf("G's reasons by cycle: %s", strings.Join(lines, "; "))
	if funded {
		t.Fatal("a transfer to G started: no node can hold its replica")
	}
	if f.held["S1"] != 5 || f.held["S2"] != 5 || len(f.marked) != 0 {
		t.Fatalf("setup: S1=%d S2=%d with %d pods marked, want both at 5 with nothing returned for an unplaceable G",
			f.held["S1"], f.held["S2"], len(f.marked))
	}
	want := []string{constants.ScalingBlockedNoNodeFits}
	for c, r := range seq {
		if !slices.Equal(r, want) {
			t.Errorf("cycle %d: G's reasons %v, want %v every cycle", c, r, want)
		}
	}
}

// Control: S1's and S2's first four pods share n2, so the small models can
// empty it. G is funded by a donor set, and is never reported no-node-fits.
func TestUtilizationShareEmptiableNodeIsNotNoNodeFits(t *testing.T) {
	f := newFragFleet([]string{"n2", "n2", "n2", "n2", "n4"}, []string{"n2", "n2", "n2", "n2", "n4"})
	seq, funded := f.run(12)
	if !funded {
		t.Fatalf("setup: no transfer to G started; reasons by cycle %v", seq)
	}
	for c, r := range seq {
		if slices.Contains(r, constants.ScalingBlockedNoNodeFits) {
			t.Errorf("cycle %d: G reported %v, though n2 can be emptied", c, r)
		}
	}
}

// End to end in the engine: B grows by one 2-GPU pod. With A's pods on nodes
// of their own -- every node holding one GPU -- no node can hold it, and B's
// model is reported no-node-fits from the engine's own pod listing. Control:
// the fixture's layout, where two of A's pods share a node, is never reported.
func TestUtilizationShareEnginePublishesNoNodeFits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apart bool
		want  bool
	}{
		{"every node holds one GPU", true, true},
		{"two pods share a node (control)", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := freshMetrics(t)
			nf := newNodeFleet(t)
			if tc.apart {
				var p corev1.Pod
				if err := nf.c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "A-v-6"}, &p); err != nil {
					t.Fatal(err)
				}
				p.Spec.NodeName = "n-A-v-6"
				if err := nf.c.Update(context.Background(), &p); err != nil {
					t.Fatal(err)
				}
				nf.nodes["shared"] = decision.NodeGPU{Accelerator: "A100", Capacity: 1, Used: 1}
				nf.nodes["n-A-v-6"] = decision.NodeGPU{Accelerator: "A100", Capacity: 1, Used: 1}
			}
			cycles, reported := 0, 0
			for range 12 {
				nf.cycle(true)
				var reasons []string
				for _, m := range family(t, reg, constants.LLMSMModelScalingBlocked) {
					if label(m, constants.LabelModelName) == "B" && m.GetGauge().GetValue() > 0 {
						reasons = append(reasons, label(m, constants.LabelReason))
					}
				}
				if slices.Contains(reasons, constants.ScalingBlockedQuietPeriod) {
					continue // nothing is planned, or judged, in the quiet period
				}
				cycles++
				if slices.Contains(reasons, constants.ScalingBlockedNoNodeFits) {
					reported++
				}
			}
			if cycles == 0 {
				t.Fatal("setup: every cycle was in the quiet period")
			}
			if got := reported > 0; got != tc.want {
				t.Fatalf("no-node-fits on B in %d of %d planned cycles, want reported=%v", reported, cycles, tc.want)
			}
			if tc.want && reported != cycles {
				t.Fatalf("no-node-fits on B in %d of %d planned cycles, want every one", reported, cycles)
			}
		})
	}
}
