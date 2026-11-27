package learnreplay

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A LeaderWorkerSet group whose engines recorded their own rates gives one
// rank per engine, each with its own means; a row of several ranks without
// them is split evenly, each rank the row's means, and counted as such. The
// histograms are summed over the prefill rows, the decode rows give the
// decode TTFT, and rows of other variants or not serving are left out.
func TestMixtureCycleOf_RanksFromEnginesOrSplitEvenly(t *testing.T) {
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	engine := func(pod string, rate, pf, q, ttft, computed, prompt float64) domain.EngineReading {
		return domain.EngineReading{PodName: pod, RequestRate: rate, RequestRateKnown: true, AvgPrefillTime: pf, AvgPrefillTimeKnown: true,
			AvgQueueTime: q, AvgQueueTimeKnown: true, AvgTTFT: ttft, AvgTTFTKnown: true, PrefillComputedTokenRate: computed,
			PrefillComputedTokenRateKnown: true, AvgInputTokens: prompt}
	}
	group := domain.ReplicaMetrics{PodName: "g-0", VariantName: "p1", Ready: true, EngineRanks: 2, RequestRate: 3, RequestRateKnown: true,
		AvgTTFT: 1.5, AvgTTFTKnown: true,
		ComputedTokensBuckets: []domain.LengthBucket{{UpperBound: 1000, Rate: 2}, {UpperBound: 2000, Rate: 3}},
		QueueTimeBuckets:      []domain.LatencyBucket{{UpperBound: 0.3, Rate: 2.5}, {UpperBound: 1, Rate: 3}},
		GroupEngines: []domain.EngineReading{
			engine("g-0", 1, 0.5, 0.1, 1.2, 1500, 40000),
			engine("g-0-1", 2, 0.6, 0.2, 1.4, 4000, 50000),
		}}
	plain := domain.ReplicaMetrics{PodName: "p", VariantName: "p1", Ready: true, EngineRanks: 2, RequestRate: 4, RequestRateKnown: true,
		AvgPrefillTime: 0.7, AvgPrefillTimeKnown: true, PrefillComputedTokenRate: 8000, PrefillComputedTokenRateKnown: true,
		AvgInputTokens: 30000, AvgTTFT: 1, AvgTTFTKnown: true,
		ComputedTokensBuckets: []domain.LengthBucket{{UpperBound: 1000, Rate: 1}, {UpperBound: 2000, Rate: 4}}}
	in := seriesInput(7, group, plain,
		domain.ReplicaMetrics{PodName: "x", VariantName: "p1", Ready: false, RequestRate: 9, RequestRateKnown: true},
		domain.ReplicaMetrics{PodName: "d", VariantName: "d1", Ready: true, RequestRate: 7, RequestRateKnown: true, AvgTTFT: 0.4, AvgTTFTKnown: true},
		domain.ReplicaMetrics{PodName: "e", VariantName: "d2", Ready: true, RequestRate: 7, RequestRateKnown: true, AvgTTFT: 9, AvgTTFTKnown: true},
	)
	c := mixtureCycleOf(in, at, SeriesVariants{Prefill: "p1", Decode: "d1"})
	require.Len(t, c.Ranks, 4)
	assert.Equal(t, "g-0", c.Ranks[0].Key)
	assert.Equal(t, 1.0, c.Ranks[0].Rate)
	assert.Equal(t, 1500.0, c.Ranks[0].Computed)
	assert.Equal(t, 0.1, c.Ranks[0].QueueTime)
	assert.Equal(t, 2000.0, c.Ranks[1].Computed, "4000 tokens a second over 2 requests")
	assert.Equal(t, "p#0", c.Ranks[2].Key)
	assert.Equal(t, 2.0, c.Ranks[2].Rate, "the row's rate split over its 2 ranks")
	assert.Equal(t, 2000.0, c.Ranks[2].Computed)
	assert.Equal(t, -1.0, c.Ranks[2].QueueTime, "no queue time read")
	assert.Equal(t, 2, c.EvenWithinRows)
	assert.Equal(t, map[float64]float64{1000: 3, 2000: 7}, c.Computed)
	assert.Equal(t, map[float64]float64{0.3: 2.5, 1: 3}, c.Queue)
	assert.InDelta(t, (3*1.5+4*1.0)/7, c.PrefillTTFT, 1e-12)
	assert.Equal(t, 0.4, c.DecodeTTFT, "d1 only")
	assert.Nil(t, c.RouterProcessingHist, "no router latency recorded")
	assert.Zero(t, c.RouterProcessingMean)
	// A group with an engine whose rate was not recorded is split evenly.
	group.GroupEngines[1].RequestRateKnown = false
	c = mixtureCycleOf(seriesInput(7, group), at, SeriesVariants{})
	require.Len(t, c.Ranks, 2)
	assert.Equal(t, 1.5, c.Ranks[0].Rate)
	assert.Equal(t, 2, c.EvenWithinRows)
}

// The router's request-processing histogram and mean are the cycle's
// router latency's: the histogram bound to rate, the mean its _sum over its
// _count. A cycle with none, or with no count, reads none.
func TestMixtureCycleOf_RouterProcessing(t *testing.T) {
	at := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	in := seriesInput(7)
	in.RouterLatency = &domain.RouterLatency{WindowSeconds: 300,
		ProcessingBuckets: []domain.LatencyBucket{{UpperBound: 0.05, Rate: 1.5}, {UpperBound: math.Inf(1), Rate: 2.5}},
		ProcessingSumRate: 0.25, ProcessingCountRate: 2.5}
	c := mixtureCycleOf(in, at, SeriesVariants{})
	assert.Equal(t, map[float64]float64{0.05: 1.5, math.Inf(1): 2.5}, c.RouterProcessingHist)
	assert.InDelta(t, 0.1, c.RouterProcessingMean, 1e-12)

	in.RouterLatency.ProcessingSumRate, in.RouterLatency.ProcessingCountRate = 0, 0
	c = mixtureCycleOf(in, at, SeriesVariants{})
	assert.NotNil(t, c.RouterProcessingHist, "the histogram stands without its mean")
	assert.Zero(t, c.RouterProcessingMean)

	in.RouterLatency = &domain.RouterLatency{WindowSeconds: 300, TPOTSumRate: 1, TPOTCountRate: 2}
	c = mixtureCycleOf(in, at, SeriesVariants{})
	assert.Nil(t, c.RouterProcessingHist, "a recording made before the series was recorded")
	assert.Zero(t, c.RouterProcessingMean)
}

// Each rank carries its pod's API-server figures: an LWS group's engine its
// own Pod's, a row's ranks the row's with its render rate split over them.
// A decode Pod's API server is never a rank's, though the collector reads it.
// A figure not known reads 0, and so does the router's render mean without
// its _count; with it, the mean is _sum over _count.
func TestMixtureCycleOf_APIServer(t *testing.T) {
	at := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	group := domain.ReplicaMetrics{PodName: "g-0", VariantName: "p1", Ready: true, EngineRanks: 2, RequestRate: 3, RequestRateKnown: true,
		GroupEngines: []domain.EngineReading{
			{PodName: "g-0", RequestRate: 1, RequestRateKnown: true, AvgFrontendTime: 0.01, AvgFrontendTimeKnown: true,
				AvgRenderTime: 0.2, AvgRenderTimeKnown: true, RenderRate: 1, RenderRateKnown: true},
			{PodName: "g-0-1", RequestRate: 2, RequestRateKnown: true, AvgFrontendTime: 0.03, AvgFrontendTimeKnown: false,
				AvgRenderTime: 0.4, AvgRenderTimeKnown: true, RenderRate: 2, RenderRateKnown: true},
		}}
	plain := domain.ReplicaMetrics{PodName: "p", VariantName: "p1", Ready: true, EngineRanks: 2, RequestRate: 4, RequestRateKnown: true,
		AvgFrontendTime: -0.002, AvgFrontendTimeKnown: true, AvgRenderTime: 0.3, AvgRenderTimeKnown: true, RenderRate: 4,
		RenderRateKnown: true}
	decode := domain.ReplicaMetrics{PodName: "d", VariantName: "d1", Ready: true, RequestRate: 7, RequestRateKnown: true,
		AvgFrontendTime: 0.5, AvgFrontendTimeKnown: true, AvgRenderTime: 0.9, AvgRenderTimeKnown: true, RenderRate: 7,
		RenderRateKnown: true}
	in := seriesInput(7, group, plain, decode)
	in.RouterLatency = &domain.RouterLatency{WindowSeconds: 300, RenderSumRate: 0.9, RenderCountRate: 3}
	c := mixtureCycleOf(in, at, SeriesVariants{})
	require.Len(t, c.Ranks, 4, "the decode row gives no rank")
	for _, r := range c.Ranks {
		assert.NotEqual(t, 0.5, r.Frontend, "the decode pod's frontend on rank %s", r.Key)
	}
	assert.Equal(t, 0.01, c.Ranks[0].Frontend)
	assert.Equal(t, 0.2, c.Ranks[0].Render)
	assert.Equal(t, 1.0, c.Ranks[0].RenderRate)
	assert.Zero(t, c.Ranks[1].Frontend, "not known reads 0")
	assert.Equal(t, 0.4, c.Ranks[1].Render)
	assert.Equal(t, -0.002, c.Ranks[2].Frontend, "a reading under 0 is kept")
	assert.Equal(t, 0.3, c.Ranks[2].Render)
	assert.Equal(t, 2.0, c.Ranks[2].RenderRate, "the row's render rate split over its 2 ranks")
	assert.InDelta(t, 0.3, c.RouterRenderMean, 1e-12)

	in.RouterLatency.RenderCountRate = 0
	c = mixtureCycleOf(in, at, SeriesVariants{})
	assert.Zero(t, c.RouterRenderMean)
	in.RouterLatency = nil
	plain.RenderRateKnown = false
	c = mixtureCycleOf(seriesInput(7, plain), at, SeriesVariants{})
	assert.Zero(t, c.Ranks[0].Render, "a render mean without its rate reads 0")
	assert.Zero(t, c.Ranks[0].RenderRate)
}
