package steadystate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/floor"
)

// floorRaisedResult is the saturation analyzer's result on a P/D fleet whose
// decode the throughput floor raises (issue #495; the saturation package's
// TestFloorRaisesVariants_TheDecodeVariantReadsTheRolesSpare): decode seen
// saturated at 5.4 completions/s, then six decode replicas holding 3 000
// tokens each at 6 req/s. switchOn sets SwitchThroughputFloorRaisesVariants.
func floorRaisedResult(t *testing.T, switchOn bool) *domain.AnalyzerResult {
	t.Helper()
	const kvCapacity = int64(1_162_240)
	clk := clocktesting.NewFakeClock(time.Date(2026, 9, 17, 10, 48, 9, 0, time.UTC))
	a := saturation.NewSaturationAnalyzer(capacity.NewStore()).WithClock(clk)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{saturation.SwitchThroughputFloorRaisesVariants: switchOn}))

	replica := func(pod, variant string, tokensInUse, capacityTokens int64, queue int, out, rate float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName: pod, VariantName: variant, ModelID: "test-model", Namespace: "test-ns",
			KvCacheUsage: float64(tokensInUse) / float64(capacityTokens), QueueLength: queue,
			NumGpuBlocks: capacityTokens / 16, BlockSize: 16, TotalKvCapacityTokens: capacityTokens,
			TokensInUse: tokensInUse, AvgInputTokens: 6000, AvgOutputTokens: out,
			RequestRate: rate, GenerationTokenRate: rate * out, Ready: true,
		}
	}
	input := func(rm []domain.ReplicaMetrics, decodeN int) domain.AnalyzerInput {
		return domain.AnalyzerInput{
			ModelID: "test-model", Namespace: "test-ns", ReplicaMetrics: rm, ArrivalRate: 6,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "decode-v", Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: decodeN, GPUsPerReplica: 1},
				{VariantName: "prefill-v", Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
			Config: &config.ScalingPolicy{KvCacheThreshold: 0.8, QueueLengthThreshold: 5, AnalyzerName: "saturation",
				ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70},
		}
	}
	ctx := context.Background()
	for i := floor.MinThroughputSamplesToOrder - 1; i >= 0; i-- {
		_, err := a.Analyze(ctx, input([]domain.ReplicaMetrics{
			replica("decode-0", "decode-v", 1_158_912, kvCapacity, 10, 1000, 5.4+0.01*float64(i)),
			replica("prefill-0", "prefill-v", 66_183, 1_149_312, 0, 1, 6),
		}, 1))
		require.NoError(t, err)
		clk.Step(saturation.ThroughputSampleSpacing + time.Second)
	}
	rm := make([]domain.ReplicaMetrics, 0, 7)
	for i := range 6 {
		rm = append(rm, replica(string(rune('a'+i)), "decode-v", 3_000, kvCapacity, 0, 1000, 1))
	}
	rm = append(rm, replica("p", "prefill-v", 0, 1_149_312, 0, 1, 6))
	result, err := a.Analyze(ctx, input(rm, 6))
	require.NoError(t, err)
	return result
}

// The warm pool's pressure, read by the real publisher's function off a cycle
// the throughput floor raised decode on: on, the decode variant is as spare as
// its role, 1 - role demand / role supply; off, it reads its own occupancy and
// is far sparer.
func TestVariantPressure_AFlooredRoleIsReadAtTheRolesSpare(t *testing.T) {
	spare := func(res *domain.AnalyzerResult) (float64, float64) {
		var vc domain.VariantCapacity
		for _, v := range res.VariantCapacities {
			if v.VariantName == "decode-v" {
				vc = v
			}
		}
		supply := float64(vc.ReplicaCount) * vc.PerReplicaCapacity
		require.Positive(t, supply)
		for _, p := range variantPressure(allocation.NamedAnalyzerResult{Name: "saturation", Result: res}, 0.85) {
			if p.Variant == "decode-v" {
				return p.SpareFraction, 1 - res.RoleDemand[domain.RoleDecode]/supply
			}
		}
		require.FailNow(t, "no pressure for decode-v")
		return 0, 0
	}
	onSpare, roleSpare := spare(floorRaisedResult(t, true))
	offSpare, offRoleSpare := spare(floorRaisedResult(t, false))

	assert.InDelta(t, roleSpare, onSpare, 1e-12, "on, the warm pool reads decode at its role's spare")
	assert.InDelta(t, roleSpare, offRoleSpare, 1e-12, "the role's figure is the same either way")
	assert.Greater(t, offSpare, onSpare+0.15, "off, it reads the variant's occupancy, far sparer")
	t.Logf("decode spare to the warm pool: %.4f on, %.4f off (role %.4f)", onSpare, offSpare, roleSpare)
}
