package capacity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

func deploymentWithContainers(containers ...corev1.Container) scaletarget.ScaleTargetAccessor {
	return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: containers},
		}},
	})
}

// The image is the engine's, chosen by the rule the flags are read with: a
// routing sidecar beside the engine must not lend it its image, or two engines
// on different releases behind the same router would hash alike.
func TestEngineImageIsTheEngineContainers(t *testing.T) {
	st := deploymentWithContainers(
		corev1.Container{Name: "router", Image: "ghcr.io/example/router:v1", Command: []string{"router"}},
		corev1.Container{Name: "vllm", Image: "vllm/vllm-openai:v0.11.0", Command: []string{"vllm", "serve", "m"}},
	)
	p := ParseEngineArgs(inferenceengine.EngineVLLM, st)
	assert.Equal(t, "vllm/vllm-openai:v0.11.0", p.Image)
}

// Two engines identical in every flag and different only in release are
// different engines, and the fingerprint has to say so: a canary on a new
// release would otherwise borrow the old release's latency line as its own.
func TestFingerprintSeparatesEngineReleases(t *testing.T) {
	old := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(
		corev1.Container{Name: "vllm", Image: "vllm/vllm-openai:v0.8.5", Command: []string{"vllm", "serve", "m"}}))
	canary := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(
		corev1.Container{Name: "vllm", Image: "vllm/vllm-openai:v0.11.0", Command: []string{"vllm", "serve", "m"}}))

	assert.NotEqual(t, old.Fingerprint(), canary.Fingerprint())
	assert.True(t, old.IsCapacityCompatible(&canary),
		"the image is not a capacity input: a rollout must not refuse every capacity record")
}
