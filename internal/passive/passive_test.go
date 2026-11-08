package passive

import (
	"testing"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

func i32(v int32) *int32 { return &v }

// TestModeFromScaledObject reads the shape off the ScaledObject the way the
// enricher does: our trigger alone holds, beside another observes, and a
// scalingModifiers formula that names ours is observe-unsafe.
func TestModeFromScaledObject(t *testing.T) {
	ours := kedav1alpha1.ScaleTriggers{Type: "external-push", Name: "llmsm",
		Metadata: map[string]string{registry.ModelIDKey: "m"}}
	otherExternal := kedav1alpha1.ScaleTriggers{Type: "external", Name: "x",
		Metadata: map[string]string{"scalerAddress": "elsewhere:9090"}}
	prom := kedav1alpha1.ScaleTriggers{Type: "prometheus", Name: "prom"}
	cases := []struct {
		name     string
		triggers []kedav1alpha1.ScaleTriggers
		formula  string
		paused   bool
		passive  bool
		want     Mode
	}{
		{name: "active model", triggers: []kedav1alpha1.ScaleTriggers{ours, prom}, want: Active},
		{name: "alone", triggers: []kedav1alpha1.ScaleTriggers{ours}, passive: true, want: Hold},
		{name: "beside prometheus", triggers: []kedav1alpha1.ScaleTriggers{ours, prom}, passive: true, want: Observe},
		{name: "beside another external scaler", triggers: []kedav1alpha1.ScaleTriggers{ours, otherExternal}, passive: true, want: Observe},
		{name: "formula names ours", triggers: []kedav1alpha1.ScaleTriggers{ours, prom}, formula: "(prom + llmsm) / 2", passive: true, want: ObserveUnsafe},
		{name: "formula names only theirs", triggers: []kedav1alpha1.ScaleTriggers{ours, prom}, formula: "prom * 2", passive: true, want: Observe},
		{name: "a name inside another is not ours", triggers: []kedav1alpha1.ScaleTriggers{ours, prom}, formula: "llmsmx + prom", passive: true, want: Observe},
		{name: "paused is reported, not acted on", triggers: []kedav1alpha1.ScaleTriggers{ours}, paused: true, passive: true, want: Hold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			so := &kedav1alpha1.ScaledObject{
				ObjectMeta: metav1.ObjectMeta{Name: "so", Namespace: "ns"},
				Spec: kedav1alpha1.ScaledObjectSpec{ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: "d"},
					Triggers: tc.triggers},
			}
			if tc.formula != "" {
				so.Spec.Advanced = &kedav1alpha1.AdvancedConfig{ScalingModifiers: kedav1alpha1.ScalingModifiers{Formula: tc.formula}}
			}
			if tc.paused {
				so.Annotations = map[string]string{kedav1alpha1.PausedAnnotation: "true"}
			}
			target := registry.TargetFromScaledObject(so)
			if got := ModeOf(tc.passive, target); got != tc.want {
				t.Errorf("ModeOf = %q, want %q (target %+v)", got, tc.want, target)
			}
			if target.KEDAPaused != tc.paused {
				t.Errorf("KEDAPaused = %v, want %v", target.KEDAPaused, tc.paused)
			}
		})
	}
}

// TestClamp bounds a held count as KEDA and the HPA would.
func TestClamp(t *testing.T) {
	for _, tc := range []struct {
		n        int64
		min, max *int32
		want     int64
	}{
		{n: 3, want: 3},
		{n: 7, min: i32(2), max: i32(4), want: 4},
		{n: 1, min: i32(2), max: i32(4), want: 2},
		{n: 0, min: i32(0), want: 0},
	} {
		if got := Clamp(tc.n, registry.Target{MinReplicas: tc.min, MaxReplicas: tc.max}); got != tc.want {
			t.Errorf("Clamp(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

// TestResolverVariant finds a variant by its ScaledObject or its target name,
// and reads its model's passive mode from the configuration.
func TestResolverVariant(t *testing.T) {
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		"m": {ModelID: "acme/chat", Namespace: "ns", Passive: func() *bool { v := true; return &v }()},
	})
	reg := registry.New(0)
	reg.Observe("ns", "chat-so", map[string]string{registry.ModelIDKey: "acme/chat"})
	reg.SetTarget("ns", "chat-so", registry.Target{Name: "chat-deploy"})
	reg.Observe("ns", "other-so", map[string]string{registry.ModelIDKey: "acme/other"})
	r := &Resolver{Config: cfg, Registry: reg}
	for _, name := range []string{"chat-so", "chat-deploy"} {
		if m, ok := r.Variant("ns", name); !ok || m != Hold {
			t.Errorf("Variant(%q) = (%q, %v), want (hold, true)", name, m, ok)
		}
	}
	if m, ok := r.Variant("ns", "other-so"); !ok || m != Active {
		t.Errorf("Variant(other-so) = (%q, %v), want (active, true)", m, ok)
	}
	var nilResolver *Resolver
	if m, _ := nilResolver.Variant("ns", "chat-so"); m.Passive() {
		t.Error("a nil resolver must answer active")
	}
}
