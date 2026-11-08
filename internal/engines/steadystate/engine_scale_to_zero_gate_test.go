package steadystate

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// soleEngineFor decides which request counter measures a model's idleness, and
// whether one counter can measure it at all.
//
// These specs replace the ones for scaleToZeroSupportedForEngines, and the
// expectation for SGLang is deliberately INVERTED. That gate refused to park any
// non-vLLM model because the enforcer asked for vllm:request_success_total
// whatever the model ran, so an SGLang model looked permanently idle. The
// engine-specific query already existed — sglang:num_requests_total, registered
// beside the vLLM one — and simply was not reached. Now the engine is detected and
// passed to the enforcer, so a single-engine SGLang model parks on its own counter.
//
// What is still refused is a model running BOTH engines: one counter would see
// only half its traffic and could park a model still serving through the other.
var _ = Describe("soleEngineFor", func() {
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
	vllm := target("vllm/vllm-openai:latest")
	sglang := target("lmsysorg/sglang:latest")

	It("resolves vLLM for an all-vLLM model", func() {
		engine, ok := soleEngineFor(map[string]scaletarget.ScaleTargetAccessor{"a": vllm, "b": vllm})
		Expect(ok).To(BeTrue())
		Expect(engine).To(Equal(inferenceengine.EngineVLLM))
	})

	// The behaviour change. This used to be BeFalse().
	It("resolves SGLang for an all-SGLang model, which may now park", func() {
		engine, ok := soleEngineFor(map[string]scaletarget.ScaleTargetAccessor{"a": sglang, "b": sglang})
		Expect(ok).To(BeTrue(), "an SGLang model has its own request counter and can be measured")
		Expect(engine).To(Equal(inferenceengine.EngineSGLang),
			"and the enforcer must ask for sglang:num_requests_total, not the vLLM counter")
	})

	It("defaults to vLLM for an empty or nil target set", func() {
		engine, ok := soleEngineFor(nil)
		Expect(ok).To(BeTrue())
		Expect(engine).To(Equal(inferenceengine.EngineVLLM))

		engine, ok = soleEngineFor(map[string]scaletarget.ScaleTargetAccessor{})
		Expect(ok).To(BeTrue())
		Expect(engine).To(Equal(inferenceengine.EngineVLLM))
	})

	// Not a guess-one-and-hope: with two counters in play, asking for either sees
	// part of the traffic, and the part it cannot see may be the part still serving.
	It("refuses a mixed vLLM+SGLang model, because no single counter measures it", func() {
		_, ok := soleEngineFor(map[string]scaletarget.ScaleTargetAccessor{"a": vllm, "b": sglang})
		Expect(ok).To(BeFalse())
	})
})
