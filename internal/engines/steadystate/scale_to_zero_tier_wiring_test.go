package steadystate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/locator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// replicaSource is a metrics source that reports one vLLM replica of the model
// per pod, so a cycle gets past collection and reaches the scale-to-zero stage.
type replicaSource struct {
	modelID string
	pods    []string
}

func (s replicaSource) QueryList() *source.QueryList { return source.NewQueryList() }

func (s replicaSource) Refresh(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
	now := time.Now()
	sample := func(v float64) *source.MetricResult {
		r := &source.MetricResult{}
		for i, pod := range s.pods {
			labels := map[string]string{"model_name": s.modelID, "pod": pod, "instance": fmt.Sprintf("10.0.0.%d:8000", i+1)}
			r.Values = append(r.Values, source.MetricValue{Labels: labels, Value: v, Timestamp: now})
		}
		return r
	}
	return map[string]*source.MetricResult{
		"kv_cache_usage":        sample(0.1),
		"kv_usage_instant":      sample(0.1),
		"generation_token_rate": sample(10),
		"request_rate":          sample(0.1),
	}, nil
}

func (s replicaSource) Get(string, map[string]string) *source.CachedValue { return nil }

// podSuffix is the suffix every test pod name adds to its variant's name.
const podSuffix = "-0"

// variantOfPodLocator attributes pod "<variant>-0" to that variant.
type variantOfPodLocator struct{}

func (variantOfPodLocator) Locate(_ context.Context, namespace, pod string) (*locator.ManagedScaler, error) {
	return &locator.ManagedScaler{Namespace: namespace, Name: strings.TrimSuffix(pod, podSuffix)}, nil
}

func (variantOfPodLocator) ResolveScaleTarget(context.Context, string, string) (autoscalingv2.CrossVersionObjectReference, bool, error) {
	return autoscalingv2.CrossVersionObjectReference{}, false, nil
}

func (variantOfPodLocator) GetPodLabels(context.Context, string, string) map[string]string {
	return nil
}

// variantTier is one variant and the policy tier it selects.
type variantTier struct {
	variant, tier string
}

// tierWiringCase is one case of TestOptimizeV2ResolvesScaleToZeroFromTheVariantsTier.
type tierWiringCase struct {
	name     string
	policies map[string]config.ScalingPolicy
	// tiers lists the model's variants in the order the cycle receives them.
	tiers       []variantTier
	wantBlocked bool
	// wantWindow is the retention the idle check must span; 0 skips it.
	wantWindow time.Duration
}

// inBothOrders returns each case as given and, when it has more than one
// variant, again with the variants reversed. A bug that lets the first
// variant decide then fails in one of the two runs, whatever the order.
func inBothOrders(cases []tierWiringCase) []tierWiringCase {
	out := make([]tierWiringCase, 0, 2*len(cases))
	for _, tc := range cases {
		out = append(out, tc)
		if len(tc.tiers) < 2 {
			continue
		}
		reversed := tc
		reversed.name += ", variants reversed"
		reversed.tiers = slices.Clone(tc.tiers)
		slices.Reverse(reversed.tiers)
		out = append(out, reversed)
	}
	return out
}

// The tiers a model's variants name must reach the scale-to-zero stage of a
// real cycle. scaleToZeroPolicy is correct on its own (scale_to_zero_tier_test.go),
// but optimizeV2 collects the tiers in stage 1 and reads them back in stage 3;
// dropping either half resolves the default entry alone, and every function-level
// test still passes.
//
// With variants on different tiers the model parks only if every tier allows
// it, and the idle check spans the longest retention among them.
func TestOptimizeV2ResolvesScaleToZeroFromTheVariantsTier(t *testing.T) {
	const (
		modelID   = "m"
		namespace = "chat"
	)
	on, off := true, false

	for _, tc := range inBothOrders([]tierWiringCase{
		{
			name: "default allows, the variant's tier forbids",
			policies: map[string]config.ScalingPolicy{
				config.GlobalDefaultsKey: {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &on, RetentionPeriod: "1m"}},
				"interactive":            {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &off}},
			},
			tiers:       []variantTier{{"chat-a", "interactive"}},
			wantBlocked: true,
		},
		{
			name: "default forbids, the variant's tier allows",
			policies: map[string]config.ScalingPolicy{
				config.GlobalDefaultsKey: {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &off}},
				"batch":                  {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &on, RetentionPeriod: "1m"}},
			},
			tiers:       []variantTier{{"chat-a", "batch"}},
			wantBlocked: false,
			wantWindow:  time.Minute,
		},
		{
			// One sibling's tier allowing parking must not park a variant
			// whose tier forbids it: parking takes the whole model down.
			name: "one variant's tier allows, its sibling's forbids",
			policies: map[string]config.ScalingPolicy{
				config.GlobalDefaultsKey: {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &on, RetentionPeriod: "1m"}},
				"batch":                  {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &on, RetentionPeriod: "1m"}},
				"interactive":            {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &off}},
			},
			tiers:       []variantTier{{"chat-a", "batch"}, {"chat-b", "interactive"}},
			wantBlocked: true,
		},
		{
			name: "both tiers allow, with different retentions",
			policies: map[string]config.ScalingPolicy{
				config.GlobalDefaultsKey: {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &off}},
				"batch":                  {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &on, RetentionPeriod: "1m"}},
				"overnight":              {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &on, RetentionPeriod: "7m"}},
			},
			tiers:       []variantTier{{"chat-a", "batch"}, {"chat-b", "overnight"}},
			wantBlocked: false,
			wantWindow:  7 * time.Minute,
		},
	}) {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]client.Object, 0, len(tc.tiers))
			pods := make([]string, 0, len(tc.tiers))
			vas := make([]llmdvariant.Variant, 0, len(tc.tiers))
			for _, vt := range tc.tiers {
				variant, tier := vt.variant, vt.tier
				objs = append(objs, &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: variant, Namespace: namespace},
					Spec: appsv1.DeploymentSpec{
						Replicas: utils.Ptr(int32(1)),
						Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "server", Image: "vllm/vllm-openai:latest"}},
						}},
					},
					Status: appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1},
				})
				pods = append(pods, variant+podSuffix)
				vas = append(vas, llmdvariant.Variant{
					ObjectMeta: metav1.ObjectMeta{Name: variant, Namespace: namespace},
					Spec: llmdvariant.Spec{
						ModelID:        modelID,
						ScalingPolicy:  tier,
						MaxReplicas:    4,
						ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: variant, APIVersion: "apps/v1"},
					},
				})
			}
			k8s := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()

			cfg := config.NewTestConfig()
			cfg.UpdateScalingPolicyConfigForNamespace(namespace, tc.policies)

			src := replicaSource{modelID: modelID, pods: pods}
			registry := source.NewSourceRegistry()
			registry.MustRegister("prometheus", src)
			e := NewEngine(k8s, k8s, clientgoscheme.Scheme, nil, registry, cfg, allocation.NewNoOpLimiter("test"))
			e.ReplicaMetricsCollector = collector.NewReplicaMetricsCollector(src, k8s, nil, nil, variantOfPodLocator{})
			var windows []time.Duration
			e.ScaleToZeroEnforcer = allocation.NewEnforcer(
				func(_ context.Context, _ inferenceengine.Engine, _, _ string, window time.Duration) (float64, error) {
					windows = append(windows, window)
					return 0, nil // idle
				},
				nil,
			)

			decisions := e.optimizeV2(context.Background(), map[string][]llmdvariant.Variant{
				utils.GetNamespacedKey(namespace, modelID): vas,
			}, nil)

			ref, seen := e.lastBlockedModels[utils.GetNamespacedKey(namespace, modelID)]
			if !seen {
				t.Fatalf("the cycle never reached the scale-to-zero stage (decisions: %+v)", decisions)
			}
			blocked := ref.reasons == constants.ScalingBlockedPolicyForbidsZero
			if blocked != tc.wantBlocked {
				t.Errorf("blocked reasons = %q, want policy-forbids-zero = %v", ref.reasons, tc.wantBlocked)
			}
			if len(decisions) != len(tc.tiers) {
				t.Fatalf("the cycle produced %d decisions, want one per variant (%d): %+v", len(decisions), len(tc.tiers), decisions)
			}
			// Parked means the whole model is at zero. One variant at zero
			// beside a sibling still serving is the optimizer consolidating the
			// load, not parking.
			parked := true
			for _, d := range decisions {
				parked = parked && d.TargetReplicas == 0
			}
			if parked == tc.wantBlocked {
				t.Errorf("model parked = %v, want %v (decisions: %+v)", parked, !tc.wantBlocked, decisions)
			}
			if tc.wantWindow > 0 {
				if len(windows) != 1 || windows[0] != tc.wantWindow {
					t.Errorf("idle check windows = %v, want one over %v", windows, tc.wantWindow)
				}
			}
		})
	}
}
