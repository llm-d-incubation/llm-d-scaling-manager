package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A cycle keeps what the rows say beside the router's rate: every decode
// row's request rate summed, whatever variants are counted, and the counted
// variants' replicas as their states give them (issue
// llm-d-incubation/llm-d-scaling-manager#331).
func TestSampleOfKeepsTheEngineRateAndTheReplicas(t *testing.T) {
	at := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	in := seriesInput(36, seriesDecodeRow("d1", 3, 1000, 100, 0), seriesDecodeRow("d1", 1, 1000, 100, 0),
		seriesDecodeRow("d2", 5, 1000, 100, 0))
	in.VariantStates[0].CurrentReplicas = 4
	in.VariantStates[1].CurrentReplicas = 8
	in.VariantStates[2].CurrentReplicas = 3
	s := sampleOf(in, at, SeriesVariants{Prefill: "p1", Decode: "d1"})
	assert.Equal(t, 36.0, s.ArrivalRate)
	assert.Equal(t, 9.0, s.EngineRate, "every decode row's, d2's too")
	assert.Equal(t, 2, s.DecodePods)
	assert.Equal(t, 8, s.DecodeReplicas, "d1's")
	assert.Equal(t, 4, s.PrefillReplicas)

	all := sampleOf(in, at, SeriesVariants{})
	assert.Equal(t, 11, all.DecodeReplicas, "d1's and d2's")

	// A replica still starting is not a serving one; a row of an unsplit
	// variant serves the router's traffic too; a queue is noted.
	in.VariantStates[1].PendingReplicas = 3
	in.ReplicaMetrics = append(in.ReplicaMetrics, domain.ReplicaMetrics{VariantName: "b1", Ready: true, RequestRate: 2, RequestRateKnown: true})
	in.VariantStates = append(in.VariantStates, domain.VariantReplicaState{VariantName: "b1", Role: domain.RoleBoth, CurrentReplicas: 1})
	in.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 4}
	s = sampleOf(in, at, SeriesVariants{Prefill: "p1", Decode: "d1"})
	assert.Equal(t, 5, s.DecodeReplicas, "8 less the 3 starting")
	assert.Equal(t, 11.0, s.EngineRate, "the unsplit row's 2 r/s too")
	assert.True(t, s.RouterQueued)

	in.VariantStates[0].Role = domain.RoleDecode
	assert.Zero(t, sampleOf(in, at, SeriesVariants{Prefill: "p1", Decode: "d1"}).PrefillReplicas, "no prefill variant stated")
}
