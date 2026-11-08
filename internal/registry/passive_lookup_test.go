package registry

import (
	"slices"
	"testing"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TiersOf lists the tier every live entry of one model names, "" included,
// and none of another model's or an expired entry's.
func TestTiersOf(t *testing.T) {
	now := time.Unix(1000, 0)
	r := New(time.Minute)
	r.now = func() time.Time { return now }
	r.Observe("ns", "decode", map[string]string{ModelIDKey: "m", ScalingPolicyKey: "gold"})
	r.Observe("ns", "prefill", map[string]string{ModelIDKey: "m"})
	r.Observe("ns", "other", map[string]string{ModelIDKey: "x", ScalingPolicyKey: "lab"})
	r.Observe("elsewhere", "decode", map[string]string{ModelIDKey: "m", ScalingPolicyKey: "lab"})
	got := r.TiersOf("ns", "m")
	slices.Sort(got)
	if !slices.Equal(got, []string{"", "gold"}) {
		t.Fatalf("TiersOf = %q, want [\"\" gold]", got)
	}
	now = now.Add(2 * time.Minute)
	if got := r.TiersOf("ns", "m"); len(got) != 0 {
		t.Fatalf("TiersOf after expiry = %q, want none", got)
	}
}

// ByTargetName finds an entry by its resolved target, whatever its kind.
func TestByTargetName(t *testing.T) {
	r := New(0)
	r.Observe("ns", "so", map[string]string{ModelIDKey: "m"})
	r.SetTarget("ns", "so", Target{Kind: "LeaderWorkerSet", Name: "lws"})
	if e, ok := r.ByTargetName("ns", "lws"); !ok || e.Name != "so" {
		t.Fatalf("ByTargetName = (%+v, %v), want the so entry", e, ok)
	}
	if _, ok := r.ByTargetName("other", "lws"); ok {
		t.Fatal("found an entry in another namespace")
	}
}

// Our trigger is known by its scalerAddress too: another external scaler's
// trigger that happens to carry a modelID is foreign.
func TestOwnTriggerByScalerAddress(t *testing.T) {
	const ours = "scaling-manager-external-scaler.scaling-manager.svc:9090"
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Name: "so", Namespace: "ns"},
		Spec: kedav1alpha1.ScaledObjectSpec{ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: "d"},
			Triggers: []kedav1alpha1.ScaleTriggers{
				{Type: "external-push", Metadata: map[string]string{ModelIDKey: "m", ScalerAddressKey: ours}},
				{Type: "external", Metadata: map[string]string{ModelIDKey: "m", ScalerAddressKey: "someone-else:9090"}},
			},
			Fallback: &kedav1alpha1.Fallback{FailureThreshold: 3, Replicas: 1},
		},
	}
	if got := TargetFromScaledObjectFor(so, ours); got.ForeignTriggers != 1 || !got.HasFallback {
		t.Fatalf("with our address: ForeignTriggers=%d HasFallback=%v, want 1 and true", got.ForeignTriggers, got.HasFallback)
	}
	if got := TargetFromScaledObject(so); got.ForeignTriggers != 0 {
		t.Fatalf("without an address both match our schema: ForeignTriggers=%d, want 0", got.ForeignTriggers)
	}
}
