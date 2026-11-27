package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// An engine of a LeaderWorkerSet group keeps its own prefill readings
// (EngineReadingOf), which the merged row holds only summed or averaged; an
// engine's row for the ITL line (rowOf) does not carry them.
func TestEngineReadingOf_KeepsThePrefillReadings(t *testing.T) {
	rm := ReplicaMetrics{PodName: "g-0-1", RequestRate: 1.5, RequestRateKnown: true, AvgTTFT: 1.2, AvgTTFTKnown: true,
		AvgQueueTime: 0.1, AvgQueueTimeKnown: true, AvgPrefillTime: 0.6, AvgPrefillTimeKnown: true, AvgInputTokens: 50000,
		PrefillComputedTokenRate: 3000, PrefillComputedTokenRateKnown: true}
	e := EngineReadingOf(rm)
	assert.Equal(t, EngineReading{PodName: "g-0-1", RequestRate: 1.5, RequestRateKnown: true, AvgTTFT: 1.2, AvgTTFTKnown: true,
		AvgQueueTime: 0.1, AvgQueueTimeKnown: true, AvgPrefillTime: 0.6, AvgPrefillTimeKnown: true, AvgInputTokens: 50000,
		PrefillComputedTokenRate: 3000, PrefillComputedTokenRateKnown: true}, e)
	assert.Equal(t, ReplicaMetrics{PodName: "g-0-1"}, e.rowOf(ReplicaMetrics{}))
}
