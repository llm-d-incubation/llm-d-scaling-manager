package variantmeta

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// Review WN2: a LeaderWorkerSet group some of whose pods hold a node is
// counted apart, with the node and each placed pod's GPUs, so the in-flight
// ledger holds only its other pods, where its placed ones are.
func TestPlacedGroupsReportsHalfPlacedGroups(t *testing.T) {
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "pd", Namespace: "ns"},
		Spec: lwsv1.LeaderWorkerSetSpec{
			Replicas:             ptr.To[int32](3),
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{Size: ptr.To[int32](2)},
		},
	}
	pod := func(name, idx, node string, phase corev1.PodPhase) *corev1.Pod {
		p := heldPod(name, map[string]string{lwsv1.SetNameLabelKey: "pd", lwsv1.GroupIndexLabelKey: idx}, node, phase, false)
		p.Spec.Containers = []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}}}}
		return p
	}
	c := heldClient(t,
		pod("pd-0", "0", "n1", corev1.PodRunning),
		pod("pd-0-1", "0", "n2", corev1.PodRunning),
		// Group 10's leader is bound, its worker not created yet.
		pod("pd-10", "10", "n3", corev1.PodRunning),
		// Group 2's leader is bound, its worker Pending without a node.
		pod("pd-2", "2", "n4", corev1.PodPending),
		pod("pd-2-1", "2", "", corev1.PodPending),
	)
	placed, partial, ok := PlacedGroups(context.Background(), c, "ns", scaletarget.NewLWSAccessor(lws))
	if !ok || placed != 1 {
		t.Fatalf("placed = %d (ok %t), want group 0 alone", placed, ok)
	}
	if len(partial) != 2 || partial[0].Node != "n4" || partial[1].Node != "n3" {
		t.Fatalf("partial = %+v, want group 2 on n4 then group 10 on n3", partial)
	}
	for _, p := range partial {
		if len(p.PlacedGPUs) != 1 || p.PlacedGPUs[0] != 8 {
			t.Fatalf("partial %+v: want one placed pod of 8 GPUs", p)
		}
	}
}
