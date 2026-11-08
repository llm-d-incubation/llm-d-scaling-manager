package gpunodes

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Node fit matches a node's own NoSchedule and NoExecute taints against each
// pod's tolerations; the ones that make the node refuse every new pod are
// judged as unschedulable instead, and PreferNoSchedule binds nothing.
func TestPlacementTaintsKeepThePodDecidedOnes(t *testing.T) {
	node := &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{
		{Key: "dedicated", Value: "x", Effect: corev1.TaintEffectNoSchedule},
		{Key: "gpu-pool", Effect: corev1.TaintEffectNoExecute},
		{Key: "soft", Effect: corev1.TaintEffectPreferNoSchedule},
		{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute},
		{Key: "ToBeDeletedByClusterAutoscaler", Effect: corev1.TaintEffectNoSchedule},
	}}}
	got := placementTaints(node)
	if len(got) != 2 || got[0].Key != "dedicated" || got[1].Key != "gpu-pool" {
		t.Fatalf("placement taints %v, want dedicated and gpu-pool", got)
	}
	if placementTaints(&corev1.Node{}) != nil {
		t.Fatal("an untainted node has placement taints")
	}
}
