package steadystate

import (
	"math"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// Two LeaderWorkerSet receivers with an exclusive topology, each short by
// one 16-GPU group of two 8-GPU pods, and one rack with 32 free GPUs: the
// idle fill raised both into the rack, and the second group stayed Pending
// on the exclusive-topology anti-affinity. One group per domain, as the node
// view places them. Control: without the topology key both are filled.
func TestTheIdleFillPlacesOneGroupPerExclusiveRack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		domains map[string]string
		want    int
	}{
		{"exclusive topology: one group per rack", map[string]string{"B": "rack", "C": "rack"}, 1},
		{"no topology key (control)", nil, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{}
			e.utilizationShare.desired = map[string]int{}
			gang := allocation.ShareVariant{GPUs: 16, PodGPUs: []int{8, 8}}
			b, c := gang, gang
			b.Name, c.Name = "b", "c"
			g := allocation.ShareGroup{
				AcceleratorType: "A100", Budget: 32, ClusterFree: math.MaxInt,
				Roles: []allocation.ShareRole{
					{Key: "B", Weight: 1, Need: 16, Ceiling: 32, ReplicaGPUs: 16},
					{Key: "C", Weight: 1, Need: 16, Ceiling: 32, ReplicaGPUs: 16},
				},
				Grow: map[string]allocation.ShareVariant{"B": b, "C": c},
			}
			ev := allocation.ShareEvaluation{Roles: []allocation.ShareRoleVerdict{{Key: "B", Integer: 16}, {Key: "C", Integer: 16}}}
			nodes := map[string]allocation.ShareNode{}
			for _, n := range []string{"n1", "n2", "n3", "n4"} {
				nodes[n] = allocation.ShareNode{Free: 8, Labels: map[string]string{"rack": "r1"}}
			}
			key := func(role, variant string) string { return role + "/" + variant }
			e.fillIdleShare(logr.Discard(), allocation.NewShareLedger(), g, ev, map[string]int{},
				shareFit{nodes: nodes, domains: tc.domains}, key, time.Unix(0, 0), allocation.ShareTimings{FillTimeout: time.Minute})
			if got := e.utilizationShare.desired["B/b"] + e.utilizationShare.desired["C/c"]; got != tc.want {
				t.Fatalf("filled %d groups, want %d", got, tc.want)
			}
		})
	}
}

// The share's node picture marks a node a LeaderWorkerSet group's pod is on,
// so the fit refuses its domain to a new group.
func TestTheShareNodePictureMarksGroupNodes(t *testing.T) {
	snap := map[string]decision.NodeGPU{
		"n1": {Accelerator: "A100", Capacity: 8, Used: 8, LWSGroup: true, LWSGroups: []string{"ns/z-0"}},
		"n2": {Accelerator: "A100", Capacity: 8},
	}
	nodes := shareFreeNodes(snap, "A100", 0)
	if len(nodes["n1"].Groups) != 1 || nodes["n1"].Groups[0] != "ns/z-0" || len(nodes["n2"].Groups) != 0 {
		t.Fatalf("group marks %v, want n1 only", nodes)
	}
}
