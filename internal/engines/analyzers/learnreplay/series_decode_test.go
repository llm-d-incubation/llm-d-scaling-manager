package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A decode cycle pools the named decode variant's output lengths, reads each
// replica's token time as its step time over the tokens a step emits, and
// counts the running sequences per rank from the group's engines or split
// evenly from the row; prefill rows, other decode variants and rows not
// serving are left out.
func TestDecodeCycleOf_PoolsTheNamedDecodeRows(t *testing.T) {
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	group := domain.ReplicaMetrics{PodName: "d-0", VariantName: "d1", Ready: true, EngineRanks: 2, RequestRate: 2, RequestRateKnown: true,
		AvgITL: 0.04, AvgITLKnown: true, SpecAcceptedPerStep: 1, SpecAcceptedPerStepKnown: true,
		OutputTokensBuckets: []domain.LengthBucket{{UpperBound: 10, Rate: 1}, {UpperBound: 100, Rate: 2}},
		GroupEngines: []domain.EngineReading{{PodName: "d-0", RunningRequests: 3, RunningRequestsKnown: true},
			{PodName: "d-0-1", RunningRequests: 5, RunningRequestsKnown: true}}}
	plain := domain.ReplicaMetrics{PodName: "d-1", VariantName: "d1", Ready: true, EngineRanks: 2, RequestRate: 1, RequestRateKnown: true,
		AvgITL: 0.02, AvgITLKnown: true, RunningRequests: 8, RunningRequestsKnown: true,
		OutputTokensBuckets: []domain.LengthBucket{{UpperBound: 10, Rate: 0.5}, {UpperBound: 100, Rate: 1}}}
	in := seriesInput(3, group, plain,
		domain.ReplicaMetrics{PodName: "x", VariantName: "d1", Ready: false, RequestRate: 9, RequestRateKnown: true,
			OutputTokensBuckets: []domain.LengthBucket{{UpperBound: 10, Rate: 9}}},
		domain.ReplicaMetrics{PodName: "o", VariantName: "d2", Ready: true, RequestRate: 9, RequestRateKnown: true,
			OutputTokensBuckets: []domain.LengthBucket{{UpperBound: 10, Rate: 9}}},
		domain.ReplicaMetrics{PodName: "p", VariantName: "p1", Ready: true, RequestRate: 9, RequestRateKnown: true,
			OutputTokensBuckets: []domain.LengthBucket{{UpperBound: 10, Rate: 9}}},
	)
	c := decodeCycleOf(in, at, SeriesVariants{Prefill: "p1", Decode: "d1"})
	assert.Equal(t, at, c.At)
	assert.Equal(t, map[float64]float64{10: 1.5, 100: 3}, c.Output)
	require.Len(t, c.Groups, 2)
	assert.InDelta(t, 0.02, c.Groups[0].TokenTime, 1e-12, "a 40 ms step emitting 2 tokens")
	assert.True(t, c.Groups[0].AcceptedKnown)
	assert.InDelta(t, 0.02, c.Groups[1].TokenTime, 1e-12, "no speculation: the step is a token")
	assert.False(t, c.Groups[1].AcceptedKnown)
	assert.Equal(t, 4, c.RanksReported)
	assert.InDelta(t, 4, c.MeanRunning, 1e-12, "(3 + 5 + 8) over 4 ranks")
	assert.InDelta(t, 5, c.MostRunning, 1e-12)
}
