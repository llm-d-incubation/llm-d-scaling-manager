package whatifsource

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// Each per-Pod query the collector registers, into the row field a capture
// fills from it: through the Prometheus HTTP API, the client's parse, the
// collector's Prometheus source and its extraction, as the controller reads
// it. Each is checked present, absent (the row of a Pod with its KV and
// queue series alone) and NaN.
//
// A field with a presence bit reads its bit; one without reads its value
// against zero. NaN is the 0/0 of an idle window: a bit stays unset, and the
// value reads zero, except ITL's, whose NaN is the idle state (AvgITLEmpty).

// rowField reads one field of a row: its value, and whether it was read.
type rowField func(r domain.ReplicaMetrics) (value any, known bool)

func scalar(get func(r domain.ReplicaMetrics) float64, known func(r domain.ReplicaMetrics) bool) rowField {
	return func(r domain.ReplicaMetrics) (any, bool) {
		v := get(r)
		if known == nil {
			return v, v != 0
		}
		return v, known(r)
	}
}

func buckets[B domain.LengthBucket | domain.LatencyBucket](get func(r domain.ReplicaMetrics) []B) rowField {
	return func(r domain.ReplicaMetrics) (any, bool) {
		b := get(r)
		return b, len(b) > 0
	}
}

type mappingCase struct {
	query string
	// extra is the series' labels beyond the Pod's (le, the cache shape).
	extra []string
	value float64
	field rowField
	want  any
	// also are series the field needs beside its own: the busiest rank's
	// capacity queue is read only beside the pod's.
	also map[string]float64
	// nan is false for a query whose NaN is not tested (the cache shape's
	// value counts ranks, and its shape is in labels).
	noNaN bool
}

func mappingCases() []mappingCase {
	f := scalar
	return []mappingCase{
		{query: registration.QueryKvCacheUsage, value: 0.25, want: 0.25,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.KvCacheUsage }, func(r domain.ReplicaMetrics) bool { return r.KvCacheUsageKnown })},
		{query: registration.QueryQueueLength, value: 7, want: 7.0,
			field: f(func(r domain.ReplicaMetrics) float64 { return float64(r.QueueLength) }, func(r domain.ReplicaMetrics) bool { return r.QueueLengthKnown })},
		{query: registration.QueryMaxRankKvCacheUsage, value: 0.75, want: 0.75,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.MaxRankKvCacheUsage }, nil)},
		{query: registration.QueryMaxRankQueueLength, value: 4, want: 4.0,
			field: f(func(r domain.ReplicaMetrics) float64 { return float64(r.MaxRankQueueLength) }, nil)},
		{query: registration.QueryCapacityQueueLength, value: 3, want: 3.0,
			field: f(func(r domain.ReplicaMetrics) float64 { return float64(r.CapacityQueueLength) }, func(r domain.ReplicaMetrics) bool { return r.CapacityQueueKnown })},
		{query: registration.QueryMaxRankCapacityQueueLength, value: 2, want: 2.0, also: map[string]float64{registration.QueryCapacityQueueLength: 3},
			field: f(func(r domain.ReplicaMetrics) float64 { return float64(r.MaxRankCapacityQueueLength) }, nil)},
		{query: registration.QueryCacheConfigInfo, extra: []string{"num_gpu_blocks", "1000", "block_size", "16"}, value: 2, want: 16000.0, noNaN: true,
			field: f(func(r domain.ReplicaMetrics) float64 { return float64(r.TotalKvCapacityTokens) }, func(r domain.ReplicaMetrics) bool {
				return r.NumGpuBlocks == 1000 && r.BlockSize == 16 && r.EngineRanks == 2 && r.TokensInUse == 8000
			})},
		{query: registration.QueryAvgOutputTokens, value: 300, want: 300.0,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.AvgOutputTokens }, nil)},
		{query: registration.QueryAvgOutputTokensRecent, value: 280, want: 280.0,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.AvgOutputTokensRecent }, nil)},
		{query: registration.QueryAvgInputTokens, value: 900, want: 900.0,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.AvgInputTokens }, nil)},
		{query: registration.QueryAvgInputTokensRecent, value: 850, want: 850.0,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.AvgInputTokensRecent }, nil)},
		{query: registration.QueryPrefixCacheHitRate, value: 0.3, want: 0.3,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.PrefixCacheHitRate }, nil)},
		{query: registration.QueryAvgITL, value: 0.02, want: 0.02,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.AvgITL }, func(r domain.ReplicaMetrics) bool { return r.AvgITLKnown })},
		{query: registration.QueryAvgTTFT, value: 0.4, want: 0.4,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.AvgTTFT }, func(r domain.ReplicaMetrics) bool { return r.AvgTTFTKnown })},
		{query: registration.QueryAvgServiceTime, value: 3, want: 3.0,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.AvgServiceTime }, func(r domain.ReplicaMetrics) bool { return r.AvgServiceTimeKnown })},
		{query: registration.QueryPrefillComputedTokenRate, value: 5000, want: 5000.0,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.PrefillComputedTokenRate }, func(r domain.ReplicaMetrics) bool { return r.PrefillComputedTokenRateKnown })},
		{query: registration.QueryGenerationTokenRate, value: 1200, want: 1200.0,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.GenerationTokenRate }, func(r domain.ReplicaMetrics) bool { return r.GenerationTokenRateKnown })},
		{query: registration.QueryKvUsageInstant, value: 0.45, want: 0.45,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.KvUsageInstant }, func(r domain.ReplicaMetrics) bool { return r.KvUsageInstantKnown })},
		{query: registration.QueryKvUsageMean, value: 0.42, want: 0.42,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.KvUsageMean }, func(r domain.ReplicaMetrics) bool { return r.KvUsageMeanKnown })},
		{query: registration.QueryRequestRate, value: 2.5, want: 2.5,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.RequestRate }, func(r domain.ReplicaMetrics) bool { return r.RequestRateKnown })},
		{query: registration.QueryRunningRequests, value: 30, want: 30.0,
			field: f(func(r domain.ReplicaMetrics) float64 { return float64(r.RunningRequests) }, func(r domain.ReplicaMetrics) bool { return r.RunningRequestsKnown })},
		{query: registration.QueryPreemptionRate, value: 0.1, want: 0.1,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.PreemptionRate }, func(r domain.ReplicaMetrics) bool { return r.PreemptionRateKnown })},
		{query: registration.QuerySpecAcceptedPerStep, value: 1.5, want: 1.5,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.SpecAcceptedPerStep }, func(r domain.ReplicaMetrics) bool { return r.SpecAcceptedPerStepKnown })},
		// The TTFT model's inputs (R1), recorded and read by nothing yet.
		{query: registration.QueryAvgQueueTime, value: 0.05, want: 0.05,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.AvgQueueTime }, func(r domain.ReplicaMetrics) bool { return r.AvgQueueTimeKnown })},
		{query: registration.QueryAvgPrefillTime, value: 0.2, want: 0.2,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.AvgPrefillTime }, func(r domain.ReplicaMetrics) bool { return r.AvgPrefillTimeKnown })},
		{query: registration.QueryAvgKVTransferTime, value: 0.01, want: 0.01,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.AvgKVTransferTime }, func(r domain.ReplicaMetrics) bool { return r.AvgKVTransferTimeKnown })},
		{query: registration.QueryQueueMin, value: 1, want: 1.0,
			field: f(func(r domain.ReplicaMetrics) float64 { return r.QueueMin }, func(r domain.ReplicaMetrics) bool { return r.QueueMinKnown })},
		{query: registration.QueryOutputTokensBuckets, extra: []string{"le", "512"}, value: 1.5,
			want:  []domain.LengthBucket{{UpperBound: 512, Rate: 1.5}},
			field: buckets(func(r domain.ReplicaMetrics) []domain.LengthBucket { return r.OutputTokensBuckets })},
		{query: registration.QueryPromptTokensBuckets, extra: []string{"le", "1024"}, value: 2,
			want:  []domain.LengthBucket{{UpperBound: 1024, Rate: 2}},
			field: buckets(func(r domain.ReplicaMetrics) []domain.LengthBucket { return r.PromptTokensBuckets })},
		{query: registration.QueryTTFTBuckets, extra: []string{"le", "0.5"}, value: 1.25,
			want:  []domain.LatencyBucket{{UpperBound: 0.5, Rate: 1.25}},
			field: buckets(func(r domain.ReplicaMetrics) []domain.LatencyBucket { return r.TTFTBuckets })},
		{query: registration.QueryQueueTimeBuckets, extra: []string{"le", "0.1"}, value: 0.75,
			want:  []domain.LatencyBucket{{UpperBound: 0.1, Rate: 0.75}},
			field: buckets(func(r domain.ReplicaMetrics) []domain.LatencyBucket { return r.QueueTimeBuckets })},
		{query: registration.QueryTPOTBuckets, extra: []string{"le", "0.05"}, value: 0.5,
			want:  []domain.LatencyBucket{{UpperBound: 0.05, Rate: 0.5}},
			field: buckets(func(r domain.ReplicaMetrics) []domain.LatencyBucket { return r.TPOTBuckets })},
	}
}

// captureOne captures one step of one Pod with its two defining series, the
// case's series at value, and the case's companions.
func captureOne(t *testing.T, c mappingCase, value *float64) domain.ReplicaMetrics {
	t.Helper()
	srv := promtest.New()
	defer srv.Close()
	baseFleet(t, srv, testPod)
	for q, v := range c.also {
		srv.Set(promQL(t, q), promtest.Const(podLabels(testPod), v))
	}
	if value != nil {
		srv.Set(promQL(t, c.query), promtest.Const(podLabels(testPod, c.extra...), *value))
	}
	d, _ := capture(t, roleOfOpts(t, srv))
	return onlyRow(t, d)
}

func TestCapture_EachQueryFillsItsField(t *testing.T) {
	cases := mappingCases()
	// Every per-Pod query the collector issues while recording, so a query
	// added to it is added here.
	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.query] = true
	}
	for _, q := range collectorReplicaQueries() {
		assert.True(t, covered[q], "the per-Pod query %s has no mapping case", q)
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			v := c.value
			got, known := c.field(captureOne(t, c, &v))
			assert.True(t, known, "present: not read")
			assert.Equal(t, c.want, got, "present")

			// Absent: the field is not read. KV and queue define the row,
			// so their absence is the case's own series replaced.
			if c.query != registration.QueryKvCacheUsage && c.query != registration.QueryQueueLength {
				_, known = c.field(captureOne(t, c, nil))
				assert.False(t, known, "absent: read anyway")
			}
			if c.noNaN {
				return
			}
			nan := math.NaN()
			row := captureOne(t, c, &nan)
			got, known = c.field(row)
			assert.False(t, known, "NaN: read as a value %v", got)
			if c.query == registration.QueryAvgITL {
				assert.True(t, row.AvgITLEmpty, "NaN ITL is an idle replica")
			}
		})
	}
}

// collectorReplicaQueries is the per-Pod queries a recording collector
// issues for a vLLM fleet (collector engine_queries.go), from the
// registrations: every query whose template groups by pod.
func collectorReplicaQueries() []string {
	return []string{
		registration.QueryKvCacheUsage, registration.QueryQueueLength, registration.QueryMaxRankKvCacheUsage,
		registration.QueryMaxRankQueueLength, registration.QueryCacheConfigInfo, registration.QueryAvgOutputTokens,
		registration.QueryAvgOutputTokensRecent, registration.QueryOutputTokensBuckets, registration.QueryAvgInputTokens,
		registration.QueryAvgInputTokensRecent, registration.QueryPrefixCacheHitRate, registration.QueryAvgITL,
		registration.QueryAvgTTFT, registration.QueryAvgServiceTime, registration.QueryGenerationTokenRate,
		registration.QueryKvUsageInstant, registration.QueryKvUsageMean, registration.QueryRequestRate,
		registration.QueryRunningRequests, registration.QueryPromptTokensBuckets, registration.QueryTTFTBuckets,
		registration.QueryAvgQueueTime, registration.QueryQueueTimeBuckets, registration.QueryQueueMin,
		registration.QueryPrefillComputedTokenRate, registration.QueryPreemptionRate, registration.QuerySpecAcceptedPerStep,
		registration.QueryCapacityQueueLength, registration.QueryMaxRankCapacityQueueLength,
		registration.QueryAvgPrefillTime, registration.QueryAvgKVTransferTime, registration.QueryTPOTBuckets,
	}
}

func TestCapture_AsksEveryReplicaQueryTheRecordingCollectorAsks(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	baseFleet(t, srv, testPod)
	capture(t, roleOfOpts(t, srv))
	asked := map[string]bool{}
	for _, q := range srv.Queries() {
		asked[q] = true
	}
	for _, name := range collectorReplicaQueries() {
		assert.True(t, asked[promQL(t, name)], "%s was not asked", name)
	}
	for _, name := range []string{registration.QuerySLORouterTTFTBuckets, registration.QuerySLORouterTPOTSum,
		registration.QuerySLORouterTPOTCount, registration.QuerySLORouterTPOTBuckets, registration.QueryFlowControlWaitBuckets,
		registration.QueryFlowControlPoolModels, registration.QueryFlowControlPoolSaturation, registration.QueryFlowControlOutcomes,
		registration.QuerySchedulerQueueSize, registration.QuerySchedulerQueueBytes, registration.QueryModelArrivalRate} {
		assert.True(t, asked[promQL(t, name)], "the model-level %s was not asked", name)
	}
}

func TestCapture_ModelLevelQueriesFillTheInput(t *testing.T) {
	model := func(extra ...string) map[string]string {
		l := map[string]string{"model_name": testModel, "namespace": testNS}
		for i := 0; i+1 < len(extra); i += 2 {
			l[extra[i]] = extra[i+1]
		}
		return l
	}
	run := func(t *testing.T, set func(srv *promtest.Server)) decodedCycle {
		t.Helper()
		srv := promtest.New()
		defer srv.Close()
		baseFleet(t, srv, testPod)
		set(srv)
		d, _ := capture(t, roleOfOpts(t, srv))
		require.Len(t, d.cycles, 1)
		return d.cycles[0]
	}
	t.Run("absent", func(t *testing.T) {
		c := run(t, func(*promtest.Server) {})
		assert.Nil(t, c.Input.SchedulerQueue)
		assert.Zero(t, c.Input.ArrivalRate)
		assert.Nil(t, c.Input.RouterLatency)
		assert.Nil(t, c.Input.FlowControlPools)
	})
	t.Run("scheduler queue", func(t *testing.T) {
		c := run(t, func(srv *promtest.Server) {
			srv.Set(promQL(t, registration.QuerySchedulerQueueSize), promtest.Const(model(), 6))
			srv.Set(promQL(t, registration.QuerySchedulerQueueBytes), promtest.Const(model(), 4096))
		})
		require.NotNil(t, c.Input.SchedulerQueue)
		assert.Equal(t, domain.SchedulerQueueMetrics{QueueSize: 6, QueueBytes: 4096}, *c.Input.SchedulerQueue)
	})
	t.Run("arrival rate", func(t *testing.T) {
		c := run(t, func(srv *promtest.Server) {
			srv.Set(promQL(t, registration.QueryModelArrivalRate),
				promtest.Const(map[string]string{"namespace": testNS, "target_model_name": testModel}, 3.5))
		})
		assert.Equal(t, 3.5, c.Input.ArrivalRate)
	})
	t.Run("arrival rate NaN", func(t *testing.T) {
		c := run(t, func(srv *promtest.Server) {
			srv.Set(promQL(t, registration.QueryModelArrivalRate),
				promtest.Const(map[string]string{"namespace": testNS, "target_model_name": testModel}, math.NaN()))
		})
		assert.Zero(t, c.Input.ArrivalRate)
	})
	t.Run("router latency", func(t *testing.T) {
		c := run(t, func(srv *promtest.Server) {
			srv.Set(promQL(t, registration.QuerySLORouterTTFTBuckets), promtest.Const(model("le", "0.5"), 1.5), promtest.Const(model("le", "+Inf"), 2))
			srv.Set(promQL(t, registration.QuerySLORouterTPOTSum), promtest.Const(model(), 0.06))
			srv.Set(promQL(t, registration.QuerySLORouterTPOTCount), promtest.Const(model(), 2))
			srv.Set(promQL(t, registration.QuerySLORouterTPOTBuckets), promtest.Const(model("le", "0.05"), 1.75))
			srv.Set(promQL(t, registration.QueryFlowControlWaitBuckets), promtest.Const(model("le", "0.01"), 0.5))
		})
		require.NotNil(t, c.Input.RouterLatency)
		rl := c.Input.RouterLatency
		assert.Equal(t, []domain.LatencyBucket{{UpperBound: 0.5, Rate: 1.5}, {UpperBound: math.Inf(1), Rate: 2}}, rl.TTFTBuckets)
		assert.Equal(t, 0.06, rl.TPOTSumRate)
		assert.Equal(t, 2.0, rl.TPOTCountRate)
		assert.Equal(t, []domain.LatencyBucket{{UpperBound: 0.05, Rate: 1.75}}, rl.TPOTBuckets)
		assert.Equal(t, []domain.LatencyBucket{{UpperBound: 0.01, Rate: 0.5}}, rl.FlowControlWaitBuckets)
		assert.Equal(t, float64(registration.SLORouterWindowSeconds), rl.WindowSeconds)
	})
	t.Run("router TPOT sum NaN", func(t *testing.T) {
		c := run(t, func(srv *promtest.Server) {
			srv.Set(promQL(t, registration.QuerySLORouterTPOTSum), promtest.Const(model(), math.NaN()))
			srv.Set(promQL(t, registration.QuerySLORouterTPOTCount), promtest.Const(model(), 2))
		})
		assert.Nil(t, c.Input.RouterLatency, "a NaN sum is no mean, and nothing else was read")
	})
	t.Run("flow-control pools", func(t *testing.T) {
		pool := func(extra ...string) map[string]string {
			l := map[string]string{"inference_pool": "pool-a"}
			for i := 0; i+1 < len(extra); i += 2 {
				l[extra[i]] = extra[i+1]
			}
			return l
		}
		c := run(t, func(srv *promtest.Server) {
			srv.Set(promQL(t, registration.QueryFlowControlPoolModels), promtest.Const(pool("model_name", testModel), 1))
			srv.Set(promQL(t, registration.QueryFlowControlPoolSaturation),
				promtest.Const(pool("stage", "effective"), 0.8), promtest.Const(pool("stage", "prefill"), 0.8),
				promtest.Const(pool("stage", "decode"), math.NaN()))
			srv.Set(promQL(t, registration.QueryFlowControlOutcomes),
				promtest.Const(pool("outcome", "Dispatched"), 4), promtest.Const(pool("outcome", "Rejected"), 0.5))
		})
		require.Len(t, c.Input.FlowControlPools, 1)
		p := c.Input.FlowControlPools[0]
		assert.True(t, p.EffectiveSaturationKnown)
		assert.Equal(t, 0.8, p.EffectiveSaturation)
		assert.True(t, p.PrefillSaturationKnown)
		assert.False(t, p.DecodeSaturationKnown, "a NaN stage is not read")
		assert.True(t, p.OutcomesKnown)
		assert.Equal(t, 4.0, p.DispatchedRate)
		assert.Equal(t, 0.5, p.RejectedRate)
	})
}
