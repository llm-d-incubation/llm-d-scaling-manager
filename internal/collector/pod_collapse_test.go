package collector

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

func TestCollapseToPods_MergesDPRanksIntoOneReplica(t *testing.T) {
	ts := time.Now()
	// Two DP ranks on one pod, deliberately asymmetric: rank B has twice the
	// cache and three times the traffic, so every merge rule is observable.
	instances := []domain.ReplicaMetrics{
		{
			PodName: "pod-dp", VariantName: "va-1", ModelID: "m", Namespace: "ns",
			NumGpuBlocks: 1000, BlockSize: 16, TotalKvCapacityTokens: 16000, TokensInUse: 4000,
			KvCacheUsage: 0.25, KvUsageInstant: 0.25, KvUsageMean: 0.25, QueueLength: 2,
			RequestRate: 1, GenerationTokenRate: 100,
			AvgInputTokens: 100, AvgOutputTokens: 20, AvgITL: 0.02, AvgServiceTime: 10, PrefixCacheHitRate: 0.4,
			AvgITLKnown: true, RequestRateKnown: true,
			Metadata: &domain.ReplicaMetricsMetadata{CollectedAt: ts, Age: 5 * time.Second, FreshnessStatus: "fresh"},
		},
		{
			PodName: "pod-dp", VariantName: "va-1", ModelID: "m", Namespace: "ns",
			NumGpuBlocks: 2000, BlockSize: 16, TotalKvCapacityTokens: 32000, TokensInUse: 32000,
			KvCacheUsage: 1.0, KvUsageInstant: 1.0, KvUsageMean: 1.0, QueueLength: 5,
			RequestRate: 3, GenerationTokenRate: 300,
			AvgInputTokens: 200, AvgOutputTokens: 40, AvgITL: 0.06, AvgServiceTime: 30, PrefixCacheHitRate: 0.8,
			AvgITLKnown: true, RequestRateKnown: true,
			Metadata: &domain.ReplicaMetricsMetadata{CollectedAt: ts, Age: 30 * time.Second, FreshnessStatus: "stale"},
		},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 1, "both ranks belong to one pod, so they must produce one replica")
	pod := pods[0]

	assert.Equal(t, "pod-dp", pod.PodName)
	assert.Equal(t, "va-1", pod.VariantName)

	// Extensive quantities add up.
	assert.Equal(t, int64(3000), pod.NumGpuBlocks)
	assert.Equal(t, int64(48000), pod.TotalKvCapacityTokens)
	assert.Equal(t, int64(36000), pod.TokensInUse)
	assert.Equal(t, 7, pod.QueueLength)
	assert.Equal(t, 4.0, pod.RequestRate)
	assert.Equal(t, 400.0, pod.GenerationTokenRate)
	assert.Equal(t, pod.NumGpuBlocks*pod.BlockSize, pod.TotalKvCapacityTokens,
		"blocks x block size must still describe the merged capacity")

	// Utilization is tokens over capacity, not the mean of the two fractions
	// (which would be 0.625 and would understate a pod that is three quarters full).
	assert.InDelta(t, 0.75, pod.KvCacheUsage, 1e-9)
	assert.InDelta(t, 0.75, pod.KvUsageInstant, 1e-9)
	assert.InDelta(t, 0.75, pod.KvUsageMean, 1e-9)

	// Request shape is weighted by traffic: rank B serves 3 of every 4 requests.
	assert.InDelta(t, 175.0, pod.AvgInputTokens, 1e-9)
	assert.InDelta(t, 35.0, pod.AvgOutputTokens, 1e-9)
	// Weighted by generated tokens (100 and 300 tok/s, the same 1:3 as the
	// requests here).
	assert.InDelta(t, 0.05, pod.AvgITL, 1e-9)
	// Weighted by request rate, and asymmetric on purpose: a plain
	// mean would give 20, so 25 is what distinguishes the weighting from an
	// average. (1x10 + 3x30) / 4.
	assert.InDelta(t, 25.0, pod.AvgServiceTime, 1e-9)
	// Weighted by the prompt tokens each rank looked up, as vLLM's
	// prefix_cache_hits_total/prefix_cache_queries_total pool them: rank A 1 req/s x 100
	// tokens, rank B 3 x 200. (100x0.4 + 600x0.8) / 700. By request rate alone
	// it would be 0.7.
	assert.InDelta(t, 520.0/700.0, pod.PrefixCacheHitRate, 1e-9)

	// A pod is only as trustworthy as its stalest rank.
	require.NotNil(t, pod.Metadata)
	assert.Equal(t, "stale", pod.Metadata.FreshnessStatus)
	assert.Equal(t, 30*time.Second, pod.Metadata.Age)
}

// TestCollectReplicaMetrics_DPRanksCollapseToOneReplica drives the collapse
// through the whole collection path: a data-parallel pod is scraped as two
// series on different ports, and must come out as one replica carrying the
// pod's combined capacity — otherwise counting the result would give a DP-rank
// count where every consumer expects a scale-target replica count.
func TestCollectReplicaMetrics_DPRanksCollapseToOneReplica(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	scheme := runtime.NewScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	ts := time.Now()
	rank := func(port string) map[string]string {
		return namespaceSeriesLabels("test-model", "pod-dp", "10.0.0.1:"+port)
	}
	cacheConfig := func(port string) map[string]string {
		labels := rank(port)
		labels["num_gpu_blocks"] = "1000"
		labels["block_size"] = "16"
		return labels
	}

	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {Values: []source.MetricValue{
					{Labels: rank("8000"), Value: 0.5, Timestamp: ts},
					{Labels: rank("8001"), Value: 0.5, Timestamp: ts},
				}},
				"cache_config_info": {Values: []source.MetricValue{
					{Labels: cacheConfig("8000"), Value: 1, Timestamp: ts},
					{Labels: cacheConfig("8001"), Value: 1, Timestamp: ts},
				}},
			}, nil
		},
	}

	collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil, scalerLocator(map[string]string{"pod-dp": "va-1"}))
	results, err := collector.CollectReplicaMetrics(
		context.Background(), "test-model", "test-ns",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdvariant.Variant),
		nil,
	)
	require.NoError(t, err)

	require.Len(t, results, 1, "two DP ranks on one pod are one replica")
	assert.Equal(t, "pod-dp", results[0].PodName)
	assert.Equal(t, int64(32000), results[0].TotalKvCapacityTokens, "the pod's capacity is both ranks'")
	assert.InDelta(t, 0.5, results[0].KvCacheUsage, 1e-9)
}

func TestCollapseToPods_IdlePodFallsBackToPlainMean(t *testing.T) {
	// No traffic anywhere on the pod: there is nothing to weigh the shape ratios
	// by, so they must not collapse to zero.
	instances := []domain.ReplicaMetrics{
		{PodName: "pod-idle", AvgInputTokens: 100, AvgOutputTokens: 10, PrefixCacheHitRate: 0.2},
		{PodName: "pod-idle", AvgInputTokens: 300, AvgOutputTokens: 30, PrefixCacheHitRate: 0.6},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 1)
	assert.InDelta(t, 200.0, pods[0].AvgInputTokens, 1e-9)
	assert.InDelta(t, 20.0, pods[0].AvgOutputTokens, 1e-9)
	assert.InDelta(t, 0.4, pods[0].PrefixCacheHitRate, 1e-9)
}

func TestCollapseToPods_NoCapacityFallsBackToPlainMean(t *testing.T) {
	// cache_config_info unavailable: with no capacity to weigh by, the usage
	// fractions average rather than divide by zero.
	instances := []domain.ReplicaMetrics{
		{PodName: "pod-nocap", KvCacheUsage: 0.2, KvUsageInstant: 0.4},
		{PodName: "pod-nocap", KvCacheUsage: 0.6, KvUsageInstant: 0.8},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 1)
	assert.InDelta(t, 0.4, pods[0].KvCacheUsage, 1e-9)
	assert.InDelta(t, 0.6, pods[0].KvUsageInstant, 1e-9)
	assert.Zero(t, pods[0].TotalKvCapacityTokens)
}

func TestCollapseToPods_DistinctPodsPassThroughInOrder(t *testing.T) {
	// The common case — one instance per pod — must be left exactly as it was,
	// in input order.
	instances := []domain.ReplicaMetrics{
		{PodName: "pod-b", KvCacheUsage: 0.5, TotalKvCapacityTokens: 100},
		{PodName: "pod-a", KvCacheUsage: 0.9, TotalKvCapacityTokens: 200},
		{PodName: "pod-c", KvCacheUsage: 0.1, TotalKvCapacityTokens: 300},
	}

	pods := collapseToPods(instances)

	assert.Equal(t, instances, pods)
}

func TestCollapseToPods_MixedPodsMergeOnlyWhereShared(t *testing.T) {
	// One DP pod alongside a single-instance pod: the counts must come out as
	// two replicas, not three.
	instances := []domain.ReplicaMetrics{
		{PodName: "pod-dp", TotalKvCapacityTokens: 1000, RequestRate: 1},
		{PodName: "pod-single", TotalKvCapacityTokens: 4000, RequestRate: 7},
		{PodName: "pod-dp", TotalKvCapacityTokens: 1000, RequestRate: 1},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 2)
	assert.Equal(t, "pod-dp", pods[0].PodName)
	assert.Equal(t, int64(2000), pods[0].TotalKvCapacityTokens)
	assert.Equal(t, 2.0, pods[0].RequestRate)
	assert.Equal(t, "pod-single", pods[1].PodName)
	assert.Equal(t, int64(4000), pods[1].TotalKvCapacityTokens)
}

func TestCollapseToPods_InstantKvKnownOnlyIfEveryRankReportedIt(t *testing.T) {
	// A rank with no instant series would be averaged in as an empty cache;
	// the pod's instant reading is then not one the analyzer may charge at.
	instances := []domain.ReplicaMetrics{
		{PodName: "pod-a", TotalKvCapacityTokens: 100, KvUsageInstant: 0.8, KvUsageInstantKnown: true},
		{PodName: "pod-a", TotalKvCapacityTokens: 100},
		{PodName: "pod-b", TotalKvCapacityTokens: 100, KvUsageInstant: 0.2, KvUsageInstantKnown: true},
		{PodName: "pod-b", TotalKvCapacityTokens: 100, KvUsageInstant: 0.4, KvUsageInstantKnown: true},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 2)
	assert.False(t, pods[0].KvUsageInstantKnown, "pod-a: one rank had no instant series")
	assert.True(t, pods[1].KvUsageInstantKnown, "pod-b: both ranks reported one")
}

func TestCollapseToPods_MeanKvKnownOnlyIfEveryRankReportedIt(t *testing.T) {
	// The same for the k an ITL(k) reading is fitted from: a rank with no
	// series would be averaged in as an empty cache.
	instances := []domain.ReplicaMetrics{
		{PodName: "pod-a", TotalKvCapacityTokens: 100, KvUsageMean: 0.8, KvUsageMeanKnown: true},
		{PodName: "pod-a", TotalKvCapacityTokens: 100},
		{PodName: "pod-b", TotalKvCapacityTokens: 100, KvUsageMean: 0.2, KvUsageMeanKnown: true},
		{PodName: "pod-b", TotalKvCapacityTokens: 100, KvUsageMean: 0.4, KvUsageMeanKnown: true},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 2)
	assert.False(t, pods[0].KvUsageMeanKnown, "pod-a: one rank had no mean series")
	assert.True(t, pods[1].KvUsageMeanKnown, "pod-b: both ranks reported one")
	assert.InDelta(t, 0.3, pods[1].KvUsageMean, 1e-9)
}

func TestCollapseToPods_ITLMeanCoversFiniteRanksOnly(t *testing.T) {
	// The pod's ITL is the generation-weighted mean over the ranks that
	// measured one (ev-shindin/llm-scaling-manager#341 4a). An Empty rank that generated tokens
	// (max_tokens=1 requests) has no interval: weighed in, it read pod-a as
	// 0.02 x 100 / 150 = 13.3ms and Absent. pod-b: an Absent rank still makes
	// the pod Absent, but the value is the Finite rank's. pod-c: with no
	// generation rate the fallback weighs the Finite ranks by request rate,
	// and the busy Empty rank is left out of that too, (0.02 + 3 x 0.06) / 4,
	// not (0.02 + 3 x 0.06) / 14.
	finite := func(pod string, itl, gen, req float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, AvgITL: itl, AvgITLKnown: true,
			GenerationTokenRate: gen, RequestRate: req}
	}
	instances := []domain.ReplicaMetrics{
		finite("pod-a", 0.02, 100, 1),
		{PodName: "pod-a", AvgITLEmpty: true, GenerationTokenRate: 50, RequestRate: 50},
		finite("pod-b", 0.02, 100, 1),
		{PodName: "pod-b", GenerationTokenRate: 200, RequestRate: 2},
		finite("pod-c", 0.02, 0, 1),
		finite("pod-c", 0.06, 0, 3),
		{PodName: "pod-c", AvgITLEmpty: true, RequestRate: 10},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 3)
	assert.Equal(t, domain.ITLFinite, pods[0].ITLState(), "pod-a: the Empty rank that generated blocked Finite")
	assert.InDelta(t, 0.02, pods[0].AvgITL, 1e-12, "pod-a: the Empty rank diluted the mean")
	assert.Equal(t, 150.0, pods[0].GenerationTokenRate, "the rate still sums every rank")
	assert.Equal(t, domain.ITLAbsent, pods[1].ITLState(), "pod-b: an Absent rank hides its own mean")
	assert.InDelta(t, 0.02, pods[1].AvgITL, 1e-12, "pod-b: the Absent rank diluted the mean")
	assert.Equal(t, domain.ITLFinite, pods[2].ITLState())
	assert.InDelta(t, 0.05, pods[2].AvgITL, 1e-12, "pod-c: the request-rate fallback counted the Empty rank")
}

func TestCollapseToPods_SpecAcceptedKnownOnlyIfEveryRankReportedIt(t *testing.T) {
	// The tokens a speculative step emits, per rank: weighted by the steps
	// each ran, its tokens over 1 + E, and known only when every Finite rank
	// was read (ev-shindin/llm-scaling-manager#274). pod-b: 300/2 = 150 steps at 1 and 100/3 at 2.
	finite := func(pod string, gen float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, GenerationTokenRate: gen, AvgITL: 0.03, AvgITLKnown: true}
	}
	counted := func(m domain.ReplicaMetrics, e float64) domain.ReplicaMetrics {
		m.SpecAcceptedPerStep, m.SpecAcceptedPerStepKnown = e, true
		return m
	}
	instances := []domain.ReplicaMetrics{
		counted(finite("pod-a", 100), 1.5),
		finite("pod-a", 100),
		counted(finite("pod-b", 300), 1),
		counted(finite("pod-b", 100), 2),
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 2)
	assert.False(t, pods[0].SpecAcceptedPerStepKnown, "pod-a: one rank had no series")
	assert.Zero(t, pods[0].SpecAcceptedPerStep, "an unknown pod carries no figure")
	assert.True(t, pods[1].SpecAcceptedPerStepKnown, "pod-b: both ranks reported one")
	assert.InDelta(t, (150*1+100.0/3*2)/(150+100.0/3), pods[1].SpecAcceptedPerStep, 1e-9)
}

func TestCollapseToPods_AnIdleRankDoesNotHideTheSpecAcceptedCount(t *testing.T) {
	// A rank with no decode step in the window reads its count as 0/0 and its
	// ITL as empty: it adds nothing, as it adds nothing to the ITL mean, and
	// must not make the busy rank's count unknown, which would withhold the
	// pod's ITL under speculative flags (ev-shindin/llm-scaling-manager#274). A pod of idle ranks only has
	// no count.
	instances := []domain.ReplicaMetrics{
		{PodName: "pod-a", GenerationTokenRate: 300, AvgITL: 0.03, AvgITLKnown: true,
			SpecAcceptedPerStep: 2, SpecAcceptedPerStepKnown: true},
		{PodName: "pod-a", AvgITLEmpty: true},
		{PodName: "pod-b", AvgITLEmpty: true},
		{PodName: "pod-b", AvgITLEmpty: true},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 2)
	assert.True(t, pods[0].SpecAcceptedPerStepKnown, "pod-a: the idle rank hid the busy rank's count")
	assert.InDelta(t, 2, pods[0].SpecAcceptedPerStep, 1e-9)
	assert.False(t, pods[1].SpecAcceptedPerStepKnown, "pod-b: no rank read a count")
}

func TestCollapseToPods_SpecAcceptedCoversTheFiniteRanks(t *testing.T) {
	// The count covers the ranks the ITL mean covers, since 1 + E divides
	// that mean. pod-a: an Empty rank that generated tokens (max_tokens=1)
	// ran no decode step; its missing count does not hide the Finite rank's,
	// as it does not block the pod's ITL. pod-b: an Absent rank is not in the
	// mean, so its count, read or not, is not in E: E is the Finite rank's 1,
	// not a blend with the Absent rank's 3. pod-c: no Finite rank, no count.
	instances := []domain.ReplicaMetrics{
		{PodName: "pod-a", GenerationTokenRate: 300, AvgITL: 0.03, AvgITLKnown: true,
			SpecAcceptedPerStep: 2, SpecAcceptedPerStepKnown: true},
		{PodName: "pod-a", GenerationTokenRate: 50, AvgITLEmpty: true},
		{PodName: "pod-b", GenerationTokenRate: 300, AvgITL: 0.03, AvgITLKnown: true,
			SpecAcceptedPerStep: 1, SpecAcceptedPerStepKnown: true},
		{PodName: "pod-b", GenerationTokenRate: 300, SpecAcceptedPerStep: 3, SpecAcceptedPerStepKnown: true},
		{PodName: "pod-c", GenerationTokenRate: 300, SpecAcceptedPerStep: 1, SpecAcceptedPerStepKnown: true},
		{PodName: "pod-c", GenerationTokenRate: 50, AvgITLEmpty: true},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 3)
	assert.True(t, pods[0].SpecAcceptedPerStepKnown, "pod-a: the Empty rank that generated hid the count")
	assert.InDelta(t, 2, pods[0].SpecAcceptedPerStep, 1e-9)
	assert.True(t, pods[1].SpecAcceptedPerStepKnown, "pod-b: the Absent rank's count is not the mean's")
	assert.InDelta(t, 1, pods[1].SpecAcceptedPerStep, 1e-9, "pod-b: the Absent rank's count was blended in")
	assert.False(t, pods[2].SpecAcceptedPerStepKnown, "pod-c: no Finite rank, so no count")
	assert.Zero(t, pods[2].SpecAcceptedPerStep)
}

func TestCollapseToPods_ITLMeanWeighsByTokens(t *testing.T) {
	// Each Finite rank weighs by the tokens it generated, not by its
	// inter-token intervals (GenerationTokenRate - RequestRate), because
	// RequestRate counts finished requests and bursts when a batch drains.
	// pod-a: 1 req/s of 100 tokens at 20ms beside 50 req/s of 2 tokens at
	// 60ms reads 40ms by tokens. The per-pod PromQL mean divides by intervals
	// and reads (99 x 0.02 + 50 x 0.06) / 149 = 33.4ms; the token weight
	// gives the short-request rank more, here the slower one, so it reads
	// higher, the safe side. pod-b: the slow rank's batch just drained, so
	// its finished requests (150/s) outrun its tokens (100/s). By intervals
	// it would clamp to weight 0 and the pod would read the fast rank's 20ms;
	// by tokens it keeps its share and the pod reads 40ms.
	rank := func(pod string, itl, gen, req float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, AvgITL: itl, AvgITLKnown: true,
			GenerationTokenRate: gen, RequestRate: req, RequestRateKnown: true}
	}
	instances := []domain.ReplicaMetrics{
		rank("pod-a", 0.02, 100, 1),
		rank("pod-a", 0.06, 100, 50),
		rank("pod-b", 0.02, 100, 1),
		rank("pod-b", 0.06, 100, 150),
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 2)
	assert.InDelta(t, 0.04, pods[0].AvgITL, 1e-12, "pod-a: weighed by intervals, not tokens (33.4ms)")
	assert.InDelta(t, 0.04, pods[1].AvgITL, 1e-12, "pod-b: the draining slow rank dropped out of the mean")
}

func TestCollapseToPods_NoFiniteRankReadsNoITL(t *testing.T) {
	// With no Finite rank there is no mean, and AvgITL is 0 whatever the
	// ranks carry, so a reader of AvgITL > 0 sees no reading.
	instances := []domain.ReplicaMetrics{
		{PodName: "pod-a", AvgITL: 0.02, GenerationTokenRate: 100},
		{PodName: "pod-a", AvgITLEmpty: true, GenerationTokenRate: 50},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 1)
	assert.Equal(t, domain.ITLAbsent, pods[0].ITLState())
	assert.Zero(t, pods[0].AvgITL)
}
