package steadystate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// TestOptimizeV2ClearsTheSignalReasonOnEverySkipPath: a model the cycle builds
// no composite for does not keep a no-composite-signal reason from an earlier
// cycle. reportSignalReason is tested on its own; this drives the optimize
// loop, so removing the call on a skip path -- not just emptying the helper --
// fails too.
func TestOptimizeV2ClearsTheSignalReasonOnEverySkipPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		collect func() *collector.ReplicaMetricsCollector
	}{
		{"nothing collected", func() *collector.ReplicaMetricsCollector {
			return collector.NewReplicaMetricsCollector(emptyMetricsSource{}, nil, nil, record.NewFakeRecorder(10), nil)
		}},
		{"collection failed", func() *collector.ReplicaMetricsCollector {
			return collector.NewReplicaMetricsCollector(failingMetricsSource{}, nil, nil, record.NewFakeRecorder(10), nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, registry := engineConfigEngine(t)
			e.Config = config.NewTestConfig()
			e.Config.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{config.GlobalDefaultsKey: {}})
			e.optimizer = allocation.NewCostAwareOptimizer()
			e.client = fake.NewClientBuilder().WithObjects(parkedTarget()).Build()
			e.Recorder = record.NewFakeRecorder(10)
			e.ReplicaMetricsCollector = tc.collect()
			reasons := func() []string {
				return seriesLabels(t, registry, constants.LLMSMModelScalingBlocked, constants.LabelReason)
			}
			metrics.SetModelScalingBlockedReasons("ns", "m", constants.ScalingBlockedReasonsSignal,
				[]string{constants.ScalingBlockedNoCompositeSignal})
			require.Equal(t, []string{constants.ScalingBlockedNoCompositeSignal}, reasons())

			e.optimizeV2(context.Background(), map[string][]llmdvariant.Variant{
				"ns/m": {parkedVA()},
			}, nil)

			assert.Empty(t, reasons(), "a cycle with no composite for the model clears the reason")
		})
	}
}
