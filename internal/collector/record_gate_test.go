package collector

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// The record-only queries (analyzer-learner.md, 2.9) cost nothing while no
// learner recording is on, and a failed one is counted once, by the
// collection that issued it, under its own query_type.

// askingSource answers every query with result, and notes how often each
// was asked.
type askingSource struct {
	mu     sync.Mutex
	asked  map[string]int
	result func(query string) *source.MetricResult
}

func (s *askingSource) QueryList() *source.QueryList { return source.NewQueryList() }

func (s *askingSource) Refresh(_ context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.asked == nil {
		s.asked = map[string]int{}
	}
	out := make(map[string]*source.MetricResult, len(spec.Queries))
	for _, q := range spec.Queries {
		s.asked[q]++
		if s.result != nil {
			out[q] = s.result(q)
		} else {
			out[q] = &source.MetricResult{QueryName: q}
		}
	}
	return out, nil
}

func (s *askingSource) Get(string, map[string]string) *source.CachedValue { return nil }

// recordOnlyNames is every query a cycle with no TTFT target issues only while
// recording, per pod and router: the record-only ones and the TTFT readers'.
var recordOnlyNames = []string{
	registration.QueryPromptTokensBuckets, registration.QueryTTFTBuckets, registration.QueryAvgQueueTime,
	registration.QueryQueueTimeBuckets, registration.QueryQueueMin, registration.QueryAvgPrefillTime,
	registration.QueryAvgKVTransferTime, registration.QueryTPOTBuckets, registration.QueryComputedTokensBuckets,
	registration.QuerySLORouterTPOTBuckets, registration.QueryFlowControlWaitBuckets,
	registration.QueryFlowControlPoolModels, registration.QueryFlowControlPoolSaturation, registration.QueryFlowControlOutcomes,
}

// collectAll runs one cycle of every collection that can issue a
// record-only query, for two models of one namespace, recording or not.
func collectAll(c *ReplicaMetricsCollector, record bool) {
	c.BeginRecordingCycle(record)
	defer c.EndCycle()
	for _, m := range []string{"model-a", "model-b"} {
		_, _, _ = c.queryReplicaSeries(context.Background(), m, "ns", nil)
		c.CollectRouterLatency(context.Background(), m, "ns")
		c.CollectFlowControlPools(context.Background(), m, "ns")
	}
}

func TestRecordOnlyQueries_IssuedOnlyWhileRecording(t *testing.T) {
	src := &askingSource{}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)

	collectAll(c, false)
	for _, q := range recordOnlyNames {
		assert.Zero(t, src.asked[q], "recording off: %s was asked", q)
	}
	// What the slo analyzer reads is asked as before R1, once per namespace.
	for _, q := range []string{registration.QuerySLORouterTTFTBuckets, registration.QuerySLORouterTPOTSum,
		registration.QuerySLORouterTPOTCount, registration.QueryQueueLength} {
		assert.Equal(t, 1, src.asked[q], "recording off: %s", q)
	}

	src.asked = nil
	collectAll(c, true)
	for _, q := range recordOnlyNames {
		assert.Equal(t, 1, src.asked[q], "recording on: %s asked once per namespace", q)
	}
	total := 0
	for _, q := range recordOnlyNames {
		total += src.asked[q]
	}
	for _, q := range []string{registration.QuerySLORouterTTFTBuckets, registration.QuerySLORouterTPOTSum,
		registration.QuerySLORouterTPOTCount} {
		total += src.asked[q]
	}
	// The spec's cost (analyzer-learner.md, 2.9): for a vLLM namespace whose
	// models list no slo analyzer, the recording adds these queries a cycle.
	assert.Equal(t, 17, total)
}

// The recording flag is the cycle's: on, then off, then on again, each cycle
// asks exactly what its own flag says, and once a cycle ends nothing outside
// a cycle asks a record-only query, whatever the last cycle did.
func TestRecordOnlyQueries_FlagIsTheCyclesOwn(t *testing.T) {
	src := &askingSource{}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
	askedRecordOnly := func() int {
		n := 0
		for _, q := range recordOnlyNames {
			n += src.asked[q]
		}
		src.asked = nil
		return n
	}
	for i, record := range []bool{true, false, true} {
		collectAll(c, record)
		want := 0
		if record {
			want = len(recordOnlyNames)
		}
		assert.Equal(t, want, askedRecordOnly(), "cycle %d (record %v)", i, record)
	}

	// The last cycle recorded; it has ended, so a caller outside a cycle
	// must not issue a record-only query.
	assert.False(t, c.RecordsTTFTInputs(), "the flag outlived its cycle")
	_, _, _ = c.queryReplicaSeries(context.Background(), "model-a", "ns", nil)
	c.CollectRouterLatency(context.Background(), "model-a", "ns")
	assert.Nil(t, c.CollectFlowControlPools(context.Background(), "model-a", "ns"))
	assert.Zero(t, askedRecordOnly(), "outside a cycle")
}

// A failed record-only query is counted once per query the collector sent,
// under learner_recording, not under the scaling queries' types, and not
// again by a second model reading it from the memo. The slo analyzer's three
// router queries stay under router_latency.
func TestRecordOnlyQueries_FailureCountedOnceUnderItsOwnType(t *testing.T) {
	for _, recording := range []bool{false, true} {
		registry := promclient.NewRegistry()
		require.NoError(t, metrics.InitMetrics(registry))
		src := &askingSource{result: func(q string) *source.MetricResult {
			return &source.MetricResult{QueryName: q, Error: errors.New("query timed out")}
		}}
		c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
		collectAll(c, recording)

		assert.Equal(t, 3.0, errorsOfType(t, registry, constants.QueryTypeRouterLatency),
			"recording %v: the three slo router queries, once each", recording)
		want, wantRead := 0.0, 0.0
		if recording {
			// 6 per pod, 2 router histograms, 3 pool queries; and the 3 per
			// pod and 2 router queries the TTFT pricing reads, under their
			// own type.
			want, wantRead = 11, 5
		}
		assert.Equal(t, want, errorsOfType(t, registry, constants.QueryTypeLearnerRecording), "recording %v", recording)
		assert.Equal(t, wantRead, errorsOfType(t, registry, constants.QueryTypeTTFTInputs), "recording %v", recording)
	}
}

// A source that fails the whole refresh is counted once per query it was
// asked, by the call that asked: a second model's call asks again (nothing
// was memoized) and is counted again, as a query that really ran.
func TestCollectRouterLatency_RefreshErrorCountedPerIssuedQuery(t *testing.T) {
	registry := promclient.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	src := &mockMetricsSource{refreshFunc: func(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
		return nil, errors.New("prometheus unreachable")
	}}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
	c.BeginRecordingCycle(true)
	assert.Nil(t, c.CollectRouterLatency(context.Background(), "model-a", "ns"))
	c.EndCycle()
	assert.Equal(t, 3.0, errorsOfType(t, registry, constants.QueryTypeRouterLatency))
	assert.Equal(t, 2.0, errorsOfType(t, registry, constants.QueryTypeTTFTInputs))
	assert.Equal(t, 2.0, errorsOfType(t, registry, constants.QueryTypeLearnerRecording))
}

// The router TTFT histogram is sorted by bound, so two identical cycles
// record the same bytes. Built from a map, its order was Go's map order: the
// same input, recorded every cycle, came out in a different order each time.
func TestCollectRouterLatency_IdenticalInputsRecordTheSameBytes(t *testing.T) {
	bounds := []string{"0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "10", "+Inf"}
	src := &askingSource{result: func(q string) *source.MetricResult {
		r := &source.MetricResult{QueryName: q}
		if q == registration.QuerySLORouterTTFTBuckets {
			for i, le := range bounds {
				r.Values = append(r.Values, source.MetricValue{
					Labels: map[string]string{seriesTargetModelLabel: "model-a", "le": le}, Value: float64(i + 1)})
			}
		}
		return r
	}}
	encode := func() string {
		rl := NewReplicaMetricsCollector(src, nil, nil, nil, nil).CollectRouterLatency(context.Background(), "model-a", "ns")
		require.NotNil(t, rl)
		require.Len(t, rl.TTFTBuckets, len(bounds))
		require.True(t, slices.IsSortedFunc(rl.TTFTBuckets, func(a, b domain.LatencyBucket) int {
			if a.UpperBound < b.UpperBound {
				return -1
			}
			return 1
		}), "sorted by bound: %v", rl.TTFTBuckets)
		require.True(t, math.IsInf(rl.TTFTBuckets[len(bounds)-1].UpperBound, 1))
		b, err := learnrec.Marshal(learnrec.InputOf(domain.AnalyzerInput{ModelID: "model-a", Namespace: "ns", RouterLatency: rl}))
		require.NoError(t, err)
		return string(b)
	}
	first := encode()
	for range 20 {
		require.Equal(t, first, encode(), "an identical input recorded different bytes")
	}
}
