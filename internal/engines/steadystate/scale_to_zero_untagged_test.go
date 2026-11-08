package steadystate

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// A P/D model with only its decode tagged follows the decode's tier for
// parking, as it already did for its thresholds. Folding the untagged prefill
// in as the default entry let a default that disables scale-to-zero hold the
// whole model -- both roles' GPUs -- although the tier the model scales under
// enables it.
func TestUntaggedVariantFollowsTheTaggedTierForParking(t *testing.T) {
	const (
		modelID   = "pd-model"
		namespace = "pd-ns"
	)
	on, off := true, false
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfigForNamespace(namespace, map[string]config.ScalingPolicy{
		"default": {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &off}},
		"pd-gpu":  {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &on, RetentionPeriod: "1m"}},
	})
	decode := llmdvariant.Variant{}
	decode.Spec.ScalingPolicy = "pd-gpu"
	prefill := llmdvariant.Variant{} // names no tier

	if tier, _ := modelPolicy([]llmdvariant.Variant{decode, prefill}); tier != "pd-gpu" {
		t.Fatalf("setup: thresholds resolve tier %q, want pd-gpu", tier)
	}

	vllm := map[string]scaletarget.ScaleTargetAccessor{"a": scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "server", Image: "vllm/vllm-openai:latest"}},
		}}},
	})}
	e := &Engine{
		Config:            cfg,
		lastBlockedModels: make(map[string]blockedModelRef),
		ScaleToZeroEnforcer: allocation.NewEnforcer(
			func(context.Context, inferenceengine.Engine, string, string, time.Duration) (float64, error) {
				return 0, nil // idle
			},
			nil,
		),
	}
	d := []domain.VariantDecision{
		{VariantName: "decode", ModelID: modelID, Namespace: namespace, Cost: 8, CurrentReplicas: 1, TargetReplicas: 1},
		{VariantName: "prefill", ModelID: modelID, Namespace: namespace, Cost: 8, CurrentReplicas: 1, TargetReplicas: 1},
	}
	tiers := variantTiers([]llmdvariant.Variant{decode, prefill})
	if parked := e.applyScaleToZeroEnforcement(context.Background(), modelID, namespace, "v2-saturation",
		tiers, d, vllm, nil); !parked {
		t.Fatalf("model did not park under tiers %v: the untagged prefill folded in the default entry", tiers)
	}
}

// variantTiers: "" stands for the default entry only when no variant names a
// tier; otherwise untagged variants follow the named tiers.
func TestVariantTiersUntaggedHandling(t *testing.T) {
	on := func(tiers ...string) []llmdvariant.Variant {
		out := make([]llmdvariant.Variant, len(tiers))
		for i, tier := range tiers {
			out[i].Spec.ScalingPolicy = tier
		}
		return out
	}
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"no variants", nil, []string{""}},
		{"none tagged", []string{"", ""}, []string{""}},
		{"one tagged, one not", []string{"", "pd-gpu"}, []string{"pd-gpu"}},
		{"distinct and sorted", []string{"zeta", "", "alpha", "zeta"}, []string{"alpha", "zeta"}},
	} {
		got := variantTiers(on(tc.in...))
		if len(got) != len(tc.want) {
			t.Errorf("%s: variantTiers = %q, want %q", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: variantTiers = %q, want %q", tc.name, got, tc.want)
				break
			}
		}
	}
}
