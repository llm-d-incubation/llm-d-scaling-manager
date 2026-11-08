package steadystate

import (
	"context"
	"slices"
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

// The scale-to-zero policy must come from the tier the model's variants
// selected, the same tier-aware resolution its thresholds use. It used to be
// resolved with no tier at all, so a default that enabled scale-to-zero parked a
// model whose tier forbade it, and a tier that enabled it never parked anything.
func TestScaleToZeroHonoursTheVariantsPolicyTier(t *testing.T) {
	const (
		modelID   = "tier-model"
		namespace = "tier-ns"
	)
	vllm := map[string]scaletarget.ScaleTargetAccessor{"a": scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "server", Image: "vllm/vllm-openai:latest"}},
		}}},
	})}
	variantsOn := func(tiers ...string) []llmdvariant.Variant {
		out := make([]llmdvariant.Variant, 0, len(tiers))
		for _, tier := range tiers {
			va := llmdvariant.Variant{}
			va.Spec.ScalingPolicy = tier
			out = append(out, va)
		}
		return out
	}
	envelope := func(enabled bool, retention string) *config.ScaleToZeroEnvelope {
		return &config.ScaleToZeroEnvelope{Enabled: &enabled, RetentionPeriod: retention}
	}

	cases := []struct {
		name     string
		policies map[string]config.ScalingPolicy
		tiers    []string
		parks    bool
	}{
		{
			name: "default enables, the selected tier forbids: must not park",
			policies: map[string]config.ScalingPolicy{
				"default":     {ScaleToZero: envelope(true, "1m")},
				"interactive": {ScaleToZero: envelope(false, "")},
			},
			tiers: []string{"interactive"},
			parks: false,
		},
		{
			name: "default forbids, the selected tier enables: parks",
			policies: map[string]config.ScalingPolicy{
				"default": {ScaleToZero: envelope(false, "1m")},
				"batch":   {ScaleToZero: envelope(true, "2m")},
			},
			tiers: []string{"batch"},
			parks: true,
		},
		{
			name: "variants disagree, one tier forbids: the model must not park",
			policies: map[string]config.ScalingPolicy{
				"default":     {ScaleToZero: envelope(false, "1m")},
				"batch":       {ScaleToZero: envelope(true, "2m")},
				"interactive": {ScaleToZero: envelope(false, "")},
			},
			tiers: []string{"batch", "interactive"},
			parks: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewTestConfig()
			cfg.UpdateScalingPolicyConfigForNamespace(namespace, tc.policies)
			e := &Engine{
				Config:            cfg,
				lastBlockedModels: make(map[string]blockedModelRef),
				ScaleToZeroEnforcer: allocation.NewEnforcer(
					func(context.Context, inferenceengine.Engine, string, string, time.Duration) (float64, error) {
						return 0, nil // idle: parks unless the policy forbids it
					},
					nil,
				),
			}
			d := []domain.VariantDecision{
				{VariantName: "v1", ModelID: modelID, Namespace: namespace, Cost: 1, CurrentReplicas: 2, TargetReplicas: 2},
			}
			parked := e.applyScaleToZeroEnforcement(context.Background(), modelID, namespace, "v2-saturation",
				variantTiers(variantsOn(tc.tiers...)), d, vllm, nil)
			if parked != tc.parks {
				t.Fatalf("parked = %v, want %v (target now %d)", parked, tc.parks, d[0].TargetReplicas)
			}
		})
	}
}

// Retention is the longest any selected tier asks for, so a model is never
// parked sooner than one of its variants' tiers allows.
//
// The longest is placed first, last and in the middle of the sorted tiers, so
// a fold that keeps the first or the last tier's value fails one of the cases.
func TestScaleToZeroPolicyTakesTheLongestRetention(t *testing.T) {
	on := true
	retention := func(r string) config.ScalingPolicy {
		return config.ScalingPolicy{ScaleToZero: &config.ScaleToZeroEnvelope{RetentionPeriod: r}}
	}
	cases := []struct {
		name     string
		policies map[string]config.ScalingPolicy
		tiers    []string
		want     time.Duration
	}{
		{
			name: "longest in the last tier",
			policies: map[string]config.ScalingPolicy{
				"default": {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &on, RetentionPeriod: "1m"}},
				"batch":   retention("7m"),
			},
			tiers: []string{"", "batch"},
			want:  7 * time.Minute,
		},
		{
			name: "longest in the first tier",
			policies: map[string]config.ScalingPolicy{
				"default": {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &on, RetentionPeriod: "1m"}},
				"alpha":   retention("9m"),
				"zeta":    retention("3m"),
			},
			tiers: []string{"alpha", "zeta"},
			want:  9 * time.Minute,
		},
		{
			name: "longest in the middle tier",
			policies: map[string]config.ScalingPolicy{
				"default": {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &on, RetentionPeriod: "1m"}},
				"alpha":   retention("2m"),
				"beta":    retention("8m"),
				"gamma":   retention("4m"),
			},
			tiers: []string{"alpha", "beta", "gamma"},
			want:  8 * time.Minute,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := scaleToZeroPolicy(config.NewTestConfig(), tc.policies, "m", "ns", tc.tiers)
			if r := config.ResolveScaleToZeroRetention(&got); r != tc.want {
				t.Fatalf("retention = %v, want %v", r, tc.want)
			}
			if !config.ResolveScaleToZeroEnabled(nil, &got) {
				t.Fatal("every tier inherits enabled from the default; the fold must keep it enabled")
			}
		})
	}
}

// With mixed tiers, policy-forbids-zero must be traceable to the tier that
// disabled it: the fold names every tier whose resolution disables it, and the
// enforcer reports that set once per change.
func TestScaleToZeroPolicyNamesTheTiersThatDisableIt(t *testing.T) {
	on, off := true, false
	policies := map[string]config.ScalingPolicy{
		"default":     {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &on}},
		"batch":       {},
		"interactive": {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &off}},
		"realtime":    {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &off}},
	}
	got, disabledBy := scaleToZeroPolicy(config.NewTestConfig(), policies, "m", "ns",
		[]string{"batch", "interactive", "realtime"})
	if config.ResolveScaleToZeroEnabled(nil, &got) {
		t.Fatal("two tiers disable scale-to-zero; the fold must disable it")
	}
	if want := []string{"interactive", "realtime"}; !slices.Equal(disabledBy, want) {
		t.Fatalf("disabledBy = %v, want %v", disabledBy, want)
	}
	if _, disabledBy := scaleToZeroPolicy(config.NewTestConfig(), policies, "m", "ns", []string{"batch"}); disabledBy != nil {
		t.Fatalf("an enabled tier reported disabledBy = %v", disabledBy)
	}
	if got := tierNames([]string{"", "batch"}); !slices.Equal(got, []string{"default", "batch"}) {
		t.Fatalf("tierNames = %v, want the default entry spelled out", got)
	}

	e := &Engine{}
	if !e.recordForbiddenBy("ns", "m", []string{"interactive"}) {
		t.Fatal("first set must be reported")
	}
	if e.recordForbiddenBy("ns", "m", []string{"interactive"}) {
		t.Fatal("an unchanged set must stay quiet")
	}
	// The blocked-reason bookkeeping shares the entry and must not reset it.
	e.recordBlockedModel("ns", "m", []string{"policy-forbids-zero"})
	if e.recordForbiddenBy("ns", "m", []string{"interactive"}) {
		t.Fatal("recording the reasons forgot the tiers")
	}
	if !e.recordForbiddenBy("ns", "m", []string{"interactive", "realtime"}) {
		t.Fatal("a second tier disabling it is a change")
	}
}
