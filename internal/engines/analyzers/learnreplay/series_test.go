package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// seriesInput is one cycle of a model with a prefill variant p1, two decode
// variants d1 and d2, and the rows given.
func seriesInput(rate float64, rows ...domain.ReplicaMetrics) domain.AnalyzerInput {
	return domain.AnalyzerInput{ModelID: "m", Namespace: "ns", ArrivalRate: rate, ReplicaMetrics: rows,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "p1", Role: domain.RolePrefill, GPUsPerReplica: 2},
			{VariantName: "d1", Role: domain.RoleDecode, GPUsPerReplica: 4},
			{VariantName: "d2", Role: domain.RoleDecode, GPUsPerReplica: 8},
		}}
}

// seriesDecodeRow is a ready decode row of variant v serving rate r/s at the
// lengths given.
func seriesDecodeRow(v string, rate, in, out, hit float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{VariantName: v, Ready: true, RequestRate: rate, RequestRateKnown: true,
		AvgInputTokens: in, AvgOutputTokens: out, PrefixCacheHitRate: hit}
}

// A cycle counts the pods, GPUs and lengths of the named variants only: the
// rows of another variant of a role are OtherPods, a row not ready or from
// the warm pool is no pod, and the lengths are weighted by each row's rate.
func TestSampleOfCountsTheNamedVariants(t *testing.T) {
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	in := seriesInput(9,
		domain.ReplicaMetrics{VariantName: "p1", Ready: true},
		domain.ReplicaMetrics{VariantName: "p1", Ready: false},
		domain.ReplicaMetrics{VariantName: "p1", Ready: true, FromWarmPool: true},
		seriesDecodeRow("d1", 3, 1000, 100, 0.5),
		seriesDecodeRow("d1", 1, 2000, 300, 0.1),
		seriesDecodeRow("d2", 5, 9000, 900, 0.9),
	)
	s := sampleOf(in, at, SeriesVariants{Prefill: "p1", Decode: "d1"})
	assert.Equal(t, at, s.At)
	assert.Equal(t, 9.0, s.ArrivalRate)
	assert.True(t, s.RateKnown)
	assert.Equal(t, 1, s.PrefillPods, "the row not ready and the warm-pool bridge are no pods")
	assert.Equal(t, 2, s.DecodePods)
	assert.Equal(t, 1, s.OtherPods, "d2's row")
	assert.Equal(t, 2, s.PrefillGPUs)
	assert.Equal(t, 4, s.DecodeGPUs, "d1's, not d2's")
	assert.True(t, s.HasShape)
	assert.InDelta(t, (3*1000+1*2000)/4.0, s.PromptTokens, 1e-9, "weighted by rate, d1 alone")
	assert.InDelta(t, (3*100+1*300)/4.0, s.OutputTokens, 1e-9)
	assert.InDelta(t, (3*0.5+1*0.1)/4.0, s.PrefixHitRate, 1e-9)

	// Every variant counted where none is named.
	all := sampleOf(in, at, SeriesVariants{})
	assert.Equal(t, 3, all.DecodePods)
	assert.Zero(t, all.OtherPods)
	assert.Equal(t, 8, all.DecodeGPUs)
}

// With no router rate the rate is every decode row's of the model, as the
// router's would be, whatever variant is counted; a cycle with no rate
// anywhere is idle, a known 0.
func TestSampleOfRateWithoutTheRouter(t *testing.T) {
	in := seriesInput(0, seriesDecodeRow("d1", 3, 1000, 100, 0), seriesDecodeRow("d2", 5, 1000, 100, 0))
	s := sampleOf(in, time.Time{}, SeriesVariants{Prefill: "p1", Decode: "d1"})
	assert.False(t, s.RateKnown)
	assert.Equal(t, 8.0, s.ArrivalRate, "the model's rate, not d1's alone")

	idle := sampleOf(seriesInput(0, domain.ReplicaMetrics{VariantName: "d1", Ready: true}), time.Time{}, SeriesVariants{})
	assert.True(t, idle.RateKnown, "an idle cycle is no fall-back")
	assert.Zero(t, idle.ArrivalRate)
	assert.False(t, idle.HasShape)
}

// A cycle counts the replicas asked for and not serving of the named
// variants: those that exist and are not ready (PendingReplicas). A healthy
// fleet, every replica ready, counts none, and a decision recorded above
// the replicas the workload has counts none either: the recording holds no
// replica its controller never created.
func TestSampleOfCountsTheReplicasNotServing(t *testing.T) {
	in := seriesInput(1)
	in.VariantStates[0].CurrentReplicas, in.VariantStates[0].PendingReplicas = 2, 1
	in.VariantStates[1].CurrentReplicas, in.VariantStates[1].PendingReplicas = 3, 2
	in.VariantStates[2].CurrentReplicas, in.VariantStates[2].PendingReplicas = 3, 1
	s := sampleOf(in, time.Time{}, SeriesVariants{Prefill: "p1", Decode: "d1"})
	assert.Equal(t, 1, s.PrefillNotServing, "one of two starting")
	assert.Equal(t, 2, s.DecodeNotServing, "d1's alone")
	assert.Equal(t, 2+1, sampleOf(in, time.Time{}, SeriesVariants{}).DecodeNotServing, "every decode variant")

	healthy := seriesInput(1)
	for i := range healthy.VariantStates {
		healthy.VariantStates[i].CurrentReplicas, healthy.VariantStates[i].DesiredReplicas = 2, 5
	}
	h := sampleOf(healthy, time.Time{}, SeriesVariants{})
	assert.Zero(t, h.PrefillNotServing, "every replica ready")
	assert.Zero(t, h.DecodeNotServing, "a decision above what it has is not a replica asked of the cluster")
}
