package scalefromzero

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// TestRepeatedWakeRecordsScaledUpOnce: the loop republishes the same
// activation every 100ms tick until KEDA has scaled the target off zero. The
// ScaledUp Event belongs to the wake, not to the tick, so a second tick with an
// unchanged decision must record nothing new.
func TestRepeatedWakeRecordsScaledUpOnce(t *testing.T) {
	f := newWakeFixture(t, "repeat")
	f.src.values = []source.MetricValue{queueDepth(constants.EPPFlowControlQueueSize, f.model, 3)}
	rec, ok := f.engine.recorder.(*record.FakeRecorder)
	require.True(t, ok, "the fixture's recorder is a FakeRecorder")

	// Three ticks: the target stays at zero replicas, so each one wakes it again.
	for range 3 {
		require.NoError(t, f.engine.optimize(context.Background()))
		replicas, published, _ := f.woken()
		require.True(t, published, "every tick publishes the activation")
		require.Equal(t, int32(1), replicas)
	}

	scaledUp := 0
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, constants.K8SEventScaledUp) {
				scaledUp++
			}
			continue
		default:
		}
		break
	}
	assert.Equal(t, 1, scaledUp, "ScaledUp must be recorded once per wake, not once per tick")
}
