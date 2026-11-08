package variantmeta

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// The in-flight ledger releases a replica's node hole once its pods hold a
// node: scheduled, or nominated for one after a preemption, as the usage walk
// counts them. A draining pod is leaving, and an unscheduled one holds nothing.
func TestPlacedReplicasCountsScheduledAndNominatedPods(t *testing.T) {
	sel := map[string]string{"app": "m"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: sel}}},
	}
	nominated := heldPod("nominated", sel, "", corev1.PodPending, false)
	nominated.Status.NominatedNodeName = "n3"
	draining := heldPod("draining", sel, "n1", corev1.PodRunning, true)
	now := metav1.Now()
	draining.DeletionTimestamp = &now // a zero time does not survive the fake client
	c := heldClient(t,
		heldPod("running", sel, "n1", corev1.PodRunning, false),
		heldPod("starting", sel, "n2", corev1.PodPending, false),
		nominated,
		draining,
		heldPod("unscheduled", sel, "", corev1.PodPending, false),
	)
	placed, ok := PlacedReplicas(context.Background(), c, "ns", scaletarget.NewDeploymentAccessor(d))
	if !ok || placed != 3 {
		t.Fatalf("placed = %d (ok %t), want 3: running, starting and nominated", placed, ok)
	}
	if _, ok := PlacedReplicas(context.Background(), nil, "ns", nil); ok {
		t.Fatal("without a client the count must be unknown")
	}
}
