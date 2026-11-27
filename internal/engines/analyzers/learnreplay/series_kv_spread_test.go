package learnreplay

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A cycle keeps the counted decode pods' KV fill, the busiest pod's and the
// mean, as the load-balance diagnostic reads them: a pod not ready, a bridge, a
// pod of another decode variant and a pod with no finite KV reading are left
// out, and
// a data-parallel pod is read at its busiest rank (issue
// llm-d-incubation/llm-d-scaling-manager#483).
func TestSampleOfKeepsTheDecodeKVSpread(t *testing.T) {
	at := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	hot := seriesDecodeRow("d1", 3, 1000, 100, 0)
	hot.KvCacheUsage = 0.9
	cold := seriesDecodeRow("d1", 1, 1000, 100, 0)
	cold.KvCacheUsage = 0.3
	dp := seriesDecodeRow("d1", 1, 1000, 100, 0)
	dp.KvCacheUsage, dp.MaxRankKvCacheUsage, dp.EngineRanks = 0.2, 0.6, 8
	notReady := seriesDecodeRow("d1", 0, 1000, 100, 0)
	notReady.Ready, notReady.KvCacheUsage = false, 1
	other := seriesDecodeRow("d2", 5, 1000, 100, 0)
	other.KvCacheUsage = 1
	unread := seriesDecodeRow("d1", 1, 1000, 100, 0)
	unread.KvCacheUsage = math.NaN()
	// A warm-pool bridge is ready and routed to, so the load-balance
	// diagnostic counts it; it serves on another engine (prefill.Serving),
	// so the decode fill leaves it out.
	bridge := seriesDecodeRow("d1", 1, 1000, 100, 0)
	bridge.FromWarmPool, bridge.KvCacheUsage = true, 1
	in := seriesInput(9, hot, cold, dp, notReady, other, unread, bridge)

	s := sampleOf(in, at, SeriesVariants{Prefill: "p1", Decode: "d1"})
	assert.Equal(t, 3, s.DecodeKVPods, "d1's three serving pods")
	assert.InDelta(t, 0.9, s.DecodeKVBusiest, 1e-12)
	assert.InDelta(t, (0.9+0.3+0.6)/3, s.DecodeKVMean, 1e-12, "the DP pod at its busiest rank")

	// An unsplit fleet's pods are its decode pods.
	b1, b2 := seriesDecodeRow("b1", 2, 1000, 100, 0), seriesDecodeRow("b1", 2, 1000, 100, 0)
	b1.KvCacheUsage, b2.KvCacheUsage = 0.8, 0.4
	agg := domain.AnalyzerInput{ModelID: "m", Namespace: "ns", ArrivalRate: 4, ReplicaMetrics: []domain.ReplicaMetrics{b1, b2},
		VariantStates: []domain.VariantReplicaState{{VariantName: "b1", Role: domain.RoleBoth}}}
	s = sampleOf(agg, at, SeriesVariants{Decode: "b1"})
	assert.Equal(t, 2, s.DecodeKVPods)
	assert.InDelta(t, 0.8, s.DecodeKVBusiest, 1e-12)
	assert.InDelta(t, 0.6, s.DecodeKVMean, 1e-12)
	assert.Zero(t, sampleOf(agg, at, SeriesVariants{Decode: "d1"}).DecodeKVPods, "another variant named")
	// With no decode variant named, every unsplit variant's pods count.
	b3 := seriesDecodeRow("b2", 2, 1000, 100, 0)
	b3.KvCacheUsage = 0.9
	agg.ReplicaMetrics = append(agg.ReplicaMetrics, b3)
	agg.VariantStates = append(agg.VariantStates, domain.VariantReplicaState{VariantName: "b2", Role: domain.RoleBoth})
	s = sampleOf(agg, at, SeriesVariants{})
	assert.Equal(t, 3, s.DecodeKVPods, "b1's two and b2's one")
	assert.InDelta(t, 0.9, s.DecodeKVBusiest, 1e-12)
	assert.InDelta(t, (0.8+0.4+0.9)/3, s.DecodeKVMean, 1e-12)

	none := sampleOf(seriesInput(9), at, SeriesVariants{Decode: "d1"})
	assert.Zero(t, none.DecodeKVPods)
	assert.Zero(t, none.DecodeKVBusiest)
	assert.Zero(t, none.DecodeKVMean)
}
