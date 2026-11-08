package utils

import (
	"context"
	"testing"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// TestEventTargetCarriesTheScaledObjectUID follows a ScaledObject through
// enrichment and variant synthesis to the object events are recorded against.
// kubectl describe matches events to an object by involvedObject.uid, so a
// target without the UID put every Warning event out of its sight.
func TestEventTargetCarriesTheScaledObjectUID(t *testing.T) {
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Name: "chat-so", Namespace: "ns1", UID: "5b0c1d8e-uid"},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: "chat-deploy"},
		},
	}
	reg := registry.New(time.Minute)
	reg.Observe("ns1", "chat-so", map[string]string{registry.ModelIDKey: "meta/llama-3-8b"})
	reg.SetTarget("ns1", "chat-so", registry.TargetFromScaledObject(so))

	vas := readyVariants(context.Background(), reg)
	if len(vas) != 1 {
		t.Fatalf("want 1 variant, got %d", len(vas))
	}
	target := variant.EventTarget(&vas[0])
	if target.UID != so.UID {
		t.Errorf("event target UID = %q, want the ScaledObject's %q", target.UID, so.UID)
	}
	if target.Name != so.Name || target.Namespace != so.Namespace || target.Kind != "ScaledObject" {
		t.Errorf("event target = %s %s/%s, want ScaledObject ns1/chat-so", target.Kind, target.Namespace, target.Name)
	}
}
