package steadystate

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// gpuTemplate is a pod template requesting gpus GPUs at the priority class pc.
func gpuTemplate(gpus int64, pc string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		PriorityClassName: pc,
		Containers: []corev1.Container{{Name: "vllm", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(gpus, resource.DecimalSI)},
		}}},
	}}
}

func gpuDeployment(name string, gpus int64, pc string) scaletarget.ScaleTargetAccessor {
	return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       appsv1.DeploymentSpec{Template: gpuTemplate(gpus, pc)},
	})
}

// A whole-node receiver with no PriorityClass is named: a node freed for it is
// anyone's while it opens GPU by GPU (issue ev-shindin/llm-scaling-manager#234). One with a class, or one
// whose pod fits beside others, is not.
func TestShareWholeNodeUnprioritized(t *testing.T) {
	g := allocation.ShareGroup{
		AcceleratorType: "H100",
		Origins: map[string]allocation.ShareRoleOrigin{
			"G": {Namespace: "ns", ModelID: "g"}, "P": {Namespace: "ns", ModelID: "p"},
			"S": {Namespace: "ns", ModelID: "s"}, "L": {Namespace: "ns", ModelID: "l"},
		},
		Variants: map[string][]allocation.ShareVariant{
			"G": {{Name: "g-decode"}}, "P": {{Name: "p-decode"}},
			"S": {{Name: "s-decode"}}, "L": {{Name: "l-decode"}},
		},
	}
	leader := gpuTemplate(8, "share-receiver")
	targets := map[string]scaletarget.ScaleTargetAccessor{
		"ns/g-decode": gpuDeployment("g-decode", 8, ""),
		"ns/p-decode": gpuDeployment("p-decode", 8, "share-receiver"),
		"ns/s-decode": gpuDeployment("s-decode", 1, ""),
		// A LeaderWorkerSet whose leader has a class and whose workers do not.
		"ns/l-decode": scaletarget.NewLWSAccessor(&lwsv1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "l-decode", Namespace: "ns"},
			Spec: lwsv1.LeaderWorkerSetSpec{LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
				Size: ptr.To[int32](2), LeaderTemplate: &leader, WorkerTemplate: gpuTemplate(8, ""),
			}},
		}),
	}
	accessor := func(_, variant string) scaletarget.ScaleTargetAccessor { return targets["ns/"+variant] }
	nodes := map[string]decision.NodeGPU{
		"n1": {Accelerator: "H100", Capacity: 8},
		"n2": {Accelerator: "H100", Capacity: 4},
		"a1": {Accelerator: "A100", Capacity: 16},
	}

	got := shareWholeNodeUnprioritized(g, nodes, accessor)
	if want := []string{"ns/g-decode", "ns/l-decode"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// The nodes spell the accelerator as their label does, the group as its
	// budget does: they are still the group's nodes.
	labelled := map[string]decision.NodeGPU{
		"n1": {Accelerator: "NVIDIA-H100", Capacity: 8},
		"a1": {Accelerator: "NVIDIA-A100", Capacity: 16},
	}
	if got := shareWholeNodeUnprioritized(g, labelled, accessor); !slices.Equal(got, []string{"ns/g-decode", "ns/l-decode"}) {
		t.Fatalf("nodes labelled NVIDIA-H100: got %v", got)
	}
	// Nodes of the group's accelerator are larger than any pod: no pod needs
	// a whole node.
	nodes["n1"] = decision.NodeGPU{Accelerator: "H100", Capacity: 16}
	if got := shareWholeNodeUnprioritized(g, nodes, accessor); len(got) != 0 {
		t.Fatalf("no whole-node pod, got %v", got)
	}
	// Without a node picture the optimizer frees no node.
	if got := shareWholeNodeUnprioritized(g, nil, accessor); got != nil {
		t.Fatalf("no nodes, got %v", got)
	}
}

// The warning is logged once per change, and again when the set changes.
func TestReportWholeNodeUnprioritizedLogsOncePerChange(t *testing.T) {
	var lines []string
	logger := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
	var st utilizationShareState
	st.reportWholeNodeUnprioritized(logger, []string{"ns/g-decode"})
	st.reportWholeNodeUnprioritized(logger, []string{"ns/g-decode"})
	if len(lines) != 1 || !strings.Contains(lines[0], "priorityClassName") || !strings.Contains(lines[0], "ns/g-decode") {
		t.Fatalf("want one warning naming ns/g-decode, got %q", lines)
	}
	st.reportWholeNodeUnprioritized(logger, nil)
	st.reportWholeNodeUnprioritized(logger, []string{"ns/g-decode"})
	if len(lines) != 2 {
		t.Fatalf("want the warning again after the set cleared, got %q", lines)
	}
}

// Through the evaluation: on the node fleet B's pod needs a whole 2-GPU node
// ("shared" is the largest). Without a class B is named; with one, or with
// no node picture, nothing is.
func TestUtilizationShareWarnsOfAWholeNodeReceiverWithNoPriorityClass(t *testing.T) {
	for _, tc := range []struct {
		name      string
		class     string
		withNodes bool
		want      bool
	}{
		{"no class", "", true, true},
		{"a class (control)", "share-receiver", true, false},
		{"no node picture (control)", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nf := newNodeFleet(t)
			ctx, logs := observe(t)
			nf.se.ctx = ctx
			nf.se.clock = nf.se.clock.Add(30 * time.Second)
			if tc.withNodes {
				nf.se.e.utilizationShare.nodes.Publish(nf.nodes, nf.se.clock)
			}
			out := nf.f.requests()
			for i := range out {
				st := &out[i].VariantStates[0]
				st.PodGPUs = []int{1}
				if out[i].ModelID == "B" {
					st.GPUsPerReplica, st.PodGPUs = 2, []int{2}
				}
			}
			targets := nf.f.scaleTargets()
			targets["ns/B-v"] = gpuDeployment("B-decode", 2, tc.class)
			quota := []*allocation.ResourceConstraints{{Pools: map[string]allocation.ResourcePool{
				"A100": {Limit: 16, Used: 16}}}}
			nf.se.e.evaluateUtilizationShare(nf.se.ctx, out, quota, targets)
			warned := logs.FilterMessageSnippet("set no priorityClassName").All()
			if got := len(warned) == 1 && strings.Contains(fmt.Sprint(warned[0].ContextMap()["variants"]), "ns/B-v"); got != tc.want {
				t.Fatalf("warned naming ns/B-v = %v, want %v; lines %v", got, tc.want, warned)
			}
		})
	}
}

// A node picture that lapses and returns does not log the warning again: the
// set did not change, only whether it could be read.
func TestUtilizationShareWholeNodeWarningSurvivesALapsedNodePicture(t *testing.T) {
	nf := newNodeFleet(t)
	ctx, logs := observe(t)
	nf.se.ctx = ctx
	cycle := func(publish bool) {
		nf.se.clock = nf.se.clock.Add(3 * time.Minute)
		if publish {
			nf.se.e.utilizationShare.nodes.Publish(nf.nodes, nf.se.clock)
		}
		out := nf.f.requests()
		for i := range out {
			st := &out[i].VariantStates[0]
			st.PodGPUs = []int{1}
			if out[i].ModelID == "B" {
				st.GPUsPerReplica, st.PodGPUs = 2, []int{2}
			}
		}
		targets := nf.f.scaleTargets()
		targets["ns/B-v"] = gpuDeployment("B-decode", 2, "")
		quota := []*allocation.ResourceConstraints{{Pools: map[string]allocation.ResourcePool{
			"A100": {Limit: 16, Used: 16}}}}
		nf.se.e.evaluateUtilizationShare(nf.se.ctx, out, quota, targets)
	}
	cycle(true)
	cycle(false) // the snapshot is now older than NodeGPUsMaxAge
	cycle(true)
	if n := len(logs.FilterMessageSnippet("set no priorityClassName").All()); n != 1 {
		t.Fatalf("want the warning once across a lapsed node picture, got %d", n)
	}
}
