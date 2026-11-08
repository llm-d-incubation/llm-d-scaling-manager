package capacity

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// vLLM's --data-parallel-size-local is read beside the global size, and an
// unreadable one (an LWS expression) is left unstated without marking the
// read incomplete or withholding sharing. It is not hashed, and two groups
// that split one global size differently do not price each other.
func TestVLLMReadsTheLocalDataParallelSize(t *testing.T) {
	p := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer([]string{
		"vllm", "serve", "org/m", "--data-parallel-size", "16", "--data-parallel-size-local", "8",
	})))
	assert.Equal(t, 16, p.DataParallelSize)
	assert.Equal(t, 8, p.DataParallelSizeLocal)
	assert.True(t, p.Complete(), "unresolved: %v", p.Unresolved)

	q := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer([]string{
		"/bin/sh", "-c", "vllm serve org/m --data-parallel-size 16 --data-parallel-size-local $(DP_SIZE_LOCAL)",
	})))
	assert.Equal(t, 0, q.DataParallelSizeLocal)
	assert.True(t, q.Complete(), "unresolved: %v", q.Unresolved)
	assert.Empty(t, q.SharingUnresolved)
	assert.Equal(t, p.ArgsFingerprint(), q.ArgsFingerprint(), "the local size is not hashed")
	assert.False(t, p.IsCapacityCompatible(&q), "but compared for capacity")
	assert.True(t, p.IsCapacityCompatible(&p))

	// Compared as the engine runs it: a local size equal to the global one,
	// or above it, is the same engine as one left unstated.
	for _, local := range []int{16, 32} {
		s := q
		s.DataParallelSizeLocal = local
		assert.True(t, s.IsCapacityCompatible(&q), "local %d against unstated", local)
		assert.True(t, q.IsCapacityCompatible(&s), "unstated against local %d", local)
	}

	r := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer([]string{
		"vllm", "serve", "org/m", "--data-parallel-size", "8",
	})))
	assert.Equal(t, 0, r.DataParallelSizeLocal, "not stated")
}
