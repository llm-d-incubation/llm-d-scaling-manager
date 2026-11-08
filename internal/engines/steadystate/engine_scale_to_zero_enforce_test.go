package steadystate

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// applyScaleToZeroEnforcement is the single seam every optimize path (V1, V2,
// queueing-model) routes its enforcement through. These specs drive that seam with
// a real, idle (request count 0) Enforcer and assert each gate's outcome end to end.
//
// A single-engine model parks whether it runs vLLM or SGLang — each has its own
// request counter, and the enforcer asks for the one matching the detected engine.
// What is left untouched is a model running BOTH, where one counter would see only
// part of its traffic. The all-vLLM case is the canary: it proves the refusals are
// not passing merely because the enforcer never ran.
var _ = Describe("applyScaleToZeroEnforcement", func() {
	const (
		modelID   = "test-model"
		namespace = "test-ns"
	)

	var ctx = context.Background()

	target := func(image string) scaletarget.ScaleTargetAccessor {
		return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "server", Image: image}},
					},
				},
			},
		})
	}

	// engineWithIdleEnforcer builds an Engine whose enforcer always reads zero
	// traffic and whose config enables scale-to-zero for the test model — so the
	// enforcer will zero any model it is actually allowed to act on.
	engineWithIdleEnforcer := func() *Engine {
		cfg := config.NewTestConfig()
		// Scale-to-zero policy lives on the scaling entry now, keyed the same way
		// every other per-model setting is.
		cfg.UpdateScalingPolicyConfigForNamespace(namespace, map[string]config.ScalingPolicy{
			"default": {ScaleToZero: &config.ScaleToZeroEnvelope{
				Enabled: ptrTo(true), RetentionPeriod: "10m",
			}},
		})
		return &Engine{
			Config: cfg,
			ScaleToZeroEnforcer: allocation.NewEnforcer(
				func(context.Context, inferenceengine.Engine, string, string, time.Duration) (float64, error) {
					return 0, nil // idle: enforcer would scale to zero unless gated off
				},
				nil,
			),
		}
	}

	decisions := func() []domain.VariantDecision {
		return []domain.VariantDecision{
			{VariantName: "v1", ModelID: modelID, Namespace: namespace, Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 2},
		}
	}

	It("zeroes an idle all-vLLM model (canary: the enforcer does run when ungated)", func() {
		e := engineWithIdleEnforcer()
		d := decisions()
		scaledToZero := e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v1-saturation", nil,
			d, map[string]scaletarget.ScaleTargetAccessor{"a": target("vllm/vllm-openai:latest")}, nil)
		Expect(scaledToZero).To(BeTrue())
		Expect(d[0].TargetReplicas).To(Equal(0))
	})

	It("does NOT zero a model still inside its scale-from-zero retention period", func() {
		// A model just woken from zero has served nothing yet — the request that
		// woke it is still queued in the EPP — so the enforcer's request counter
		// reads idle for exactly the model that has demand waiting on it. Without
		// this gate the wake is undone before it can serve that request.
		decision.DefaultActivations.Mark(namespace, modelID)
		DeferCleanup(func() { decision.DefaultActivations.Clear(namespace, modelID) })

		e := engineWithIdleEnforcer() // configured retention: 10m
		d := decisions()
		scaledToZero := e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation", nil,
			d, map[string]scaletarget.ScaleTargetAccessor{"a": target("vllm/vllm-openai:latest")}, nil)
		Expect(scaledToZero).To(BeFalse())
		Expect(d[0].TargetReplicas).To(Equal(2),
			"a model woken from zero must keep its replicas for the retention period")
	})

	It("zeroes an idle model once its scale-from-zero retention has lapsed", func() {
		// The hold is a grace period, not a permanent exemption: the canary spec
		// above zeroes this same model with no activation recorded at all.
		decision.DefaultActivations.Mark(namespace, modelID)
		decision.DefaultActivations.Clear(namespace, modelID)

		e := engineWithIdleEnforcer()
		d := decisions()
		scaledToZero := e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation", nil,
			d, map[string]scaletarget.ScaleTargetAccessor{"a": target("vllm/vllm-openai:latest")}, nil)
		Expect(scaledToZero).To(BeTrue())
		Expect(d[0].TargetReplicas).To(Equal(0))
	})

	// Deliberately inverted. This spec used to assert an idle SGLang model was NOT
	// zeroed, which was correct only because the enforcer asked for
	// vllm:request_success_total whatever the model ran, so SGLang could not be
	// measured at all. The engine is now detected and passed down, and the SGLang
	// query already existed — so an idle SGLang model parks on its own counter.
	It("zeroes an idle SGLang model, measured by its own request counter", func() {
		e := engineWithIdleEnforcer()
		d := decisions()
		scaledToZero := e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation", nil,
			d, map[string]scaletarget.ScaleTargetAccessor{"a": target("lmsysorg/sglang:latest")}, nil)
		Expect(scaledToZero).To(BeTrue(), "SGLang has sglang:num_requests_total and can be measured")
		Expect(d[0].TargetReplicas).To(Equal(0))
	})

	// Still refused, and this is the case the engine-unsupported reason now names:
	// one counter would see only part of the model's traffic, and the part it cannot
	// see may be the part still serving.
	It("does NOT zero a mixed vLLM+SGLang model", func() {
		e := engineWithIdleEnforcer()
		d := decisions()
		scaledToZero := e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "queueing-model", nil,
			d, map[string]scaletarget.ScaleTargetAccessor{
				"a": target("vllm/vllm-openai:latest"),
				"b": target("lmsysorg/sglang:latest"),
			}, nil)
		Expect(scaledToZero).To(BeFalse())
		Expect(d[0].TargetReplicas).To(Equal(2))
	})

	It("does NOT zero a vLLM model whose variant declares minReplicas > 0", func() {
		e := engineWithIdleEnforcer()
		d := decisions()
		min := 1
		scaledToZero := e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v1-saturation", nil,
			d, map[string]scaletarget.ScaleTargetAccessor{"a": target("vllm/vllm-openai:latest")},
			[]domain.VariantReplicaState{{VariantName: "v1", MinReplicas: &min}})
		Expect(scaledToZero).To(BeFalse())
		Expect(d[0].TargetReplicas).To(Equal(2))
	})

	It("does NOT zero an idle vLLM model when a per-model override disables it", func() {
		// The entry's "default" enables scale-to-zero; the model's own
		// "{modelID}#{namespace}" override disables it and must win. That is the
		// whole resolveScalingPolicy → EnforcePolicyOnDecisions →
		// ResolveScaleToZeroEnabled wiring, end to end.
		//
		// This used to prove something weaker — that an inline setting beat a
		// separate scale-to-zero ConfigMap. With one surface there is no second
		// source to beat, so the meaningful precedence is override over default,
		// and an explicit false must survive an inherited true.
		e := engineWithIdleEnforcer()
		e.Config.UpdateScalingPolicyConfigForNamespace(namespace, map[string]config.ScalingPolicy{
			"default": {ScaleToZero: &config.ScaleToZeroEnvelope{
				Enabled: ptrTo(true), RetentionPeriod: "10m",
			}},
			modelID + "#" + namespace: {
				ModelID:     modelID,
				Namespace:   namespace,
				ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: ptrTo(false)},
			},
		})
		d := decisions()
		scaledToZero := e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation", nil,
			d, map[string]scaletarget.ScaleTargetAccessor{"a": target("vllm/vllm-openai:latest")}, nil)
		Expect(scaledToZero).To(BeFalse())
		Expect(d[0].TargetReplicas).To(Equal(2), "a per-model scaleToZero:false must override the default entry")
	})
})

// ptrTo returns a pointer to v. Local helper so this file stays self-contained.
func ptrTo[T any](v T) *T { return &v }
