package steadystate

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// forbiddenLog collects the "disabled by tier" lines a context's logger writes.
type forbiddenLog struct{ lines []string }

func (f *forbiddenLog) ctx() context.Context {
	return log.IntoContext(context.Background(), funcr.New(func(_, args string) {
		if strings.Contains(args, scaleToZeroForbiddenMessage) {
			f.lines = append(f.lines, args)
		}
	}, funcr.Options{}))
}

// parkable is one variant whose bounds permit zero.
var parkable = []domain.VariantReplicaState{{VariantName: "v"}}

// The enforcer says which tiers disable scale-to-zero once per change of that
// set, only while policy-forbids-zero is what blocks the model, and says where
// to enable it: the per-model entry when that is what disables it, the default
// entry when no variant names a tier.
func TestScaleToZeroForbiddenLogNamesTheTiersOncePerChange(t *testing.T) {
	const ns, nsDefaultOff = "forbid-log", "forbid-log-default"
	on, off := true, false
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfigForNamespace(ns, map[string]config.ScalingPolicy{
		"default":     {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &on}},
		"batch":       {},
		"interactive": {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &off}},
		"realtime":    {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &off}},
		"pinned": {ModelID: "per-model", Namespace: ns,
			ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &off}},
	})
	cfg.UpdateScalingPolicyConfigForNamespace(nsDefaultOff, map[string]config.ScalingPolicy{
		"default": {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &off}},
	})
	for _, m := range [][2]string{{ns, "mixed"}, {ns, "floored"}, {ns, "agrees"}, {ns, "per-model"}, {nsDefaultOff, "untiered"}} {
		t.Cleanup(func() { metrics.ClearModelScalingBlocked(m[0], m[1]) })
	}
	e := &Engine{Config: cfg}
	var f forbiddenLog
	ctx := f.ctx()
	cycle := func(namespace, model string, tiers []string, states []domain.VariantReplicaState) {
		e.applyScaleToZeroEnforcement(ctx, model, namespace, "test", tiers, nil, nil, states)
	}

	cycle(ns, "mixed", []string{"batch", "interactive"}, parkable)
	if len(f.lines) != 1 || !strings.Contains(f.lines[0], `"disabledBy"=["interactive"]`) {
		t.Fatalf("first cycle: want one line naming interactive, got %q", f.lines)
	}
	cycle(ns, "mixed", []string{"batch", "interactive"}, parkable)
	if len(f.lines) != 1 {
		t.Fatalf("an unchanged set was said again: %q", f.lines)
	}
	cycle(ns, "mixed", []string{"batch", "interactive", "realtime"}, parkable)
	if len(f.lines) != 2 || !strings.Contains(f.lines[1], `"disabledBy"=["interactive" "realtime"]`) ||
		!strings.Contains(f.lines[1], "on each tier in disabledBy") {
		t.Fatalf("a changed set: want a second line naming both tiers, got %q", f.lines)
	}

	// Blocked by something else, or a floor that agrees with the policy: no line.
	cycle(ns, "floored", []string{"batch"}, []domain.VariantReplicaState{{VariantName: "v", MinReplicas: ptr.To(1)}})
	cycle(ns, "agrees", []string{"interactive"}, []domain.VariantReplicaState{{VariantName: "v", MinReplicas: ptr.To(1)}})
	if len(f.lines) != 2 {
		t.Fatalf("a model not blocked by policy-forbids-zero was named: %q", f.lines[2:])
	}

	// A per-model enabled: false disables every tier; the fix is that entry.
	cycle(ns, "per-model", []string{"batch"}, parkable)
	if len(f.lines) != 3 || !strings.Contains(f.lines[2], `"disabledByPerModelEntry"=true`) ||
		!strings.Contains(f.lines[2], "on the model's per-model entry\"") {
		t.Fatalf("per-model entry: want the line to send the operator to it, got %q", f.lines[2:])
	}

	// No variant names a tier: the line names the default entry.
	cycle(nsDefaultOff, "untiered", []string{""}, parkable)
	if len(f.lines) != 4 || !strings.Contains(f.lines[3], `"disabledBy"=["default"]`) ||
		!strings.Contains(f.lines[3], "on the default entry") ||
		!strings.Contains(f.lines[3], `"disabledByPerModelEntry"=false`) {
		t.Fatalf("no tier: want the line to name the default entry, got %q", f.lines[3:])
	}
}

// A cycle that skips a model does not resolve its policy, so the record of the
// tiers that disable it is forgotten: when the model is evaluated again the
// line is said again, rather than kept quiet by a record from before the gap.
func TestScaleToZeroForbiddenRecordIsForgottenWhenACycleSkipsTheModel(t *testing.T) {
	// No scaling config loaded: optimizeV2 enumerates the model and skips it.
	e := &Engine{Config: config.NewTestConfig(), optimizer: allocation.NewCostAwareOptimizer()}
	if !e.recordForbiddenBy("chat", "m", []string{"interactive"}) {
		t.Fatal("setup: the first set was not reported")
	}
	e.optimizeV2(context.Background(), map[string][]llmdvariant.Variant{
		"chat/m": {{
			ObjectMeta: metav1.ObjectMeta{Name: "chat-a", Namespace: "chat"},
			Spec:       llmdvariant.Spec{ModelID: "m"},
		}},
	}, nil)
	if !e.recordForbiddenBy("chat", "m", []string{"interactive"}) {
		t.Fatal("after a cycle that skipped the model, the unchanged set stayed quiet")
	}
}
