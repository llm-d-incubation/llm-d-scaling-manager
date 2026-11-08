package fixtures

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

// A relay wearing ControllerDriverLabels, and a warm-pool Pod with the same
// labels, both started after the controller: neither is the newest controller.
func TestNewestControllerPodSkipsLabelTwins(t *testing.T) {
	const ns = "llm-d-sim"
	now := time.Now()
	pod := func(name, hash, ownerRS string, age time.Duration) *corev1.Pod {
		labels := ControllerDriverLabels()
		if hash != "" {
			labels["pod-template-hash"] = hash
		}
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: name, Labels: labels,
				CreationTimestamp: metav1.NewTime(now.Add(-age)),
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		if ownerRS != "" {
			p.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: ownerRS, Controller: ptr.To(true),
			}}
		}
		return p
	}
	c := fake.NewClientset(
		pod("scaling-manager-controller-manager-7d9f-abcde", "7d9f", "scaling-manager-controller-manager-7d9f", 10*time.Minute),
		pod("e2e-control-driver", "", "", time.Minute),
		pod("e2e-pool-5c6b-qwert", "5c6b", "e2e-pool-5c6b", 2*time.Minute),
	)
	got, err := NewestControllerPod(context.Background(), c, LLMSMController(ns))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "scaling-manager-controller-manager-7d9f-abcde" {
		t.Fatalf("NewestControllerPod = %s, want the controller Pod", got.Name)
	}
}
