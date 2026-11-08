package fixtures

import (
	"context"
	"maps"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// honorPodNodeName makes c's pod List apply a spec.nodeName field selector.
// The fake clientset applies label selectors only and ignores every field
// selector, so without this a List for one node returns the pods of all.
func honorPodNodeName(c *fake.Clientset) {
	c.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		sel := action.(k8stesting.ListAction).GetListRestrictions().Fields
		if sel == nil || sel.Empty() {
			return false, nil, nil
		}
		obj, err := c.Tracker().List(corev1.SchemeGroupVersion.WithResource("pods"),
			corev1.SchemeGroupVersion.WithKind("Pod"), action.GetNamespace())
		if err != nil {
			return true, nil, err
		}
		all := obj.(*corev1.PodList)
		out := &corev1.PodList{}
		for _, p := range all.Items {
			if sel.Matches(fields.Set{"spec.nodeName": p.Spec.NodeName}) {
				out.Items = append(out.Items, p)
			}
		}
		return true, out, nil
	})
}

// A pod whose namespace is being deleted holds its GPUs until the namespace
// controller deletes it, and then frees them: it counts as terminating before
// it has a deletion timestamp of its own, whichever of the namespace's
// deletion timestamp or Terminating phase says so. A finished pod there holds
// no GPU and does not count; a pod in a live namespace counts only by its own
// deletion timestamp; a pod on a node without the product label does not
// count at all.
func TestTerminatingPodsOnNodesCountsPodsOfADeletedNamespace(t *testing.T) {
	const key, product, node, other = "gpu.product", "MI300X", "worker", "cpu-worker"
	now := metav1.Now()
	pod := func(ns, name, on string, deleted bool, phase corev1.PodPhase) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.PodSpec{NodeName: on},
			Status: corev1.PodStatus{Phase: phase}}
		if deleted {
			p.DeletionTimestamp = &now
			p.Finalizers = []string{"test/hold"}
		}
		return p
	}
	c := fake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node, Labels: map[string]string{key: product}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: other}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "live"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "both", DeletionTimestamp: &now,
			Finalizers: []string{"kubernetes"}}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "timestamp-only", DeletionTimestamp: &now,
			Finalizers: []string{"kubernetes"}}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "phase-only"},
			Status: corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "off-product", DeletionTimestamp: &now,
			Finalizers: []string{"kubernetes"}}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating}},
		pod("live", "running", node, false, corev1.PodRunning),
		pod("live", "deleting", node, true, corev1.PodRunning),
		pod("both", "not-reached-yet", node, false, corev1.PodRunning),
		pod("both", "succeeded", node, false, corev1.PodSucceeded),
		pod("timestamp-only", "not-reached-yet", node, false, corev1.PodRunning),
		pod("phase-only", "not-reached-yet", node, false, corev1.PodRunning),
		pod("phase-only", "failed", node, false, corev1.PodFailed),
		pod("off-product", "not-reached-yet", other, false, corev1.PodRunning),
		pod("off-product", "deleting", other, true, corev1.PodRunning),
	)
	honorPodNodeName(c)

	got, err := TerminatingPodsOnNodes(context.Background(), c, key, product)
	if err != nil {
		t.Fatalf("TerminatingPodsOnNodes: %v", err)
	}
	want := map[string]int{"live": 1, "both": 1, "timestamp-only": 1, "phase-only": 1}
	if !maps.Equal(got, want) {
		t.Errorf("terminating by namespace = %v, want %v", got, want)
	}
}
