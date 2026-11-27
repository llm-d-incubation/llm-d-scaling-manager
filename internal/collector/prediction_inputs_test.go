package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/prometheus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// The slo analyzer's v2 models' inputs
// (docs/proposals/predicted-latency-in-controller.md): issued where a cycle
// names the namespace, read into rows the slo analyzer's v2 models alone read, and never
// seen by an analyzer.

// Each prediction query is a record-only query or a TTFT reader's, never a
// scaling query, and a replica query the collector knows.
func TestPredictionQueries_AreRecordOnlyOrTTFTReaders(t *testing.T) {
	require.NotEmpty(t, predictionReadReplicaQueries)
	all := map[string]bool{}
	for _, q := range append(append([]string{}, engineSpecificReplicaQueries...), agnosticReplicaQueries...) {
		all[q] = true
	}
	for q := range predictionReadReplicaQueries {
		assert.True(t, all[q], "%s is not a replica query", q)
		assert.False(t, isScalingQuery(q), "%s is a scaling query: clearing it would change what the analyzers read", q)
		assert.True(t, recordOnlyReplicaQueries[q] || ttftReadReplicaQueries[q], q)
	}
}

// The budget: what --slo-v2-inputs adds to a namespace's logical replica
// queries a cycle. Six beside the scaling queries, five beside a TTFT
// target's (it issues the computed-token histogram already), none beside a
// recording's, and each listed once.
func TestPredictionQueries_TheBudget(t *testing.T) {
	require.Len(t, predictionReadReplicaQueries, 6)
	count := func(recording, ttft, predict bool) int {
		spec, agnostic := replicaQueryListsFor(recording, ttft, predict)
		seen := map[string]bool{}
		for _, q := range append(append([]string{}, spec...), agnostic...) {
			assert.False(t, seen[q], "%s listed twice", q)
			seen[q] = true
		}
		return len(seen)
	}
	scaling, ttft, record := count(false, false, false), count(false, true, false), count(true, false, false)
	assert.Equal(t, scaling+len(predictionReadReplicaQueries), count(false, false, true))
	assert.Equal(t, ttft+len(predictionReadReplicaQueries)-1, count(false, true, true))
	assert.Equal(t, record, count(true, false, true))
	assert.Equal(t, record, count(true, true, true))
}

// askedPredicting runs one cycle over namespaces, two models each, reading
// the prediction inputs for named, and returns how often each query was
// asked.
func askedPredicting(c *ReplicaMetricsCollector, src *askingSource, named map[string]bool, namespaces ...string) map[string]int {
	src.asked = nil
	c.BeginCycle()
	defer c.EndCycle()
	c.ReadPredictionInputsIn(named)
	for _, ns := range namespaces {
		for _, m := range []string{"model-a", "model-b"} {
			_, _, _ = c.queryReplicaSeries(context.Background(), m, ns, nil)
		}
	}
	return src.asked
}

// Once per named namespace through the memo, never for another, and the
// record-only queries the models do not read stay record-only.
func TestPredictionQueries_OncePerNamedNamespace(t *testing.T) {
	src := &askingSource{}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)

	base := askedPredicting(c, src, nil, "ns")
	for q := range predictionReadReplicaQueries {
		assert.Zero(t, base[q], "off: %s was asked", q)
	}
	asked := askedPredicting(c, src, map[string]bool{"ns": true}, "ns", "other")
	for q := range predictionReadReplicaQueries {
		assert.Equal(t, 1, asked[q], "%s once for the named namespace", q)
	}
	for _, q := range []string{registration.QueryTTFTBuckets, registration.QueryTPOTBuckets, registration.QueryQueueMin,
		registration.QueryAvgKVTransferTime} {
		assert.Zero(t, asked[q], "%s is not a model input", q)
	}
	assert.False(t, c.readsPredictionInputs("ns"), "EndCycle must clear them")
	c.ReadPredictionInputsIn(map[string]bool{"ns": true})
	assert.False(t, c.readsPredictionInputs("ns"), "outside a cycle nothing is held")
}

// A failed prediction query is counted once, under slo_model_inputs, and a
// query a TTFT target issues anyway keeps its own type.
func TestPredictionQueries_FailureCountedUnderItsOwnType(t *testing.T) {
	failing := func(q string) *source.MetricResult {
		return &source.MetricResult{QueryName: q, Error: errors.New("query timed out")}
	}
	run := func(ttft bool) *promclient.Registry {
		registry := promclient.NewRegistry()
		require.NoError(t, metrics.InitMetrics(registry))
		src := &askingSource{result: func(q string) *source.MetricResult {
			if isPredictionQuery(q) || ttftReadReplicaQueries[q] {
				return failing(q)
			}
			return &source.MetricResult{QueryName: q}
		}}
		c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
		c.BeginCycle()
		defer c.EndCycle()
		if ttft {
			c.ReadTTFTInputsIn(map[string]bool{"ns": true})
		}
		c.ReadPredictionInputsIn(map[string]bool{"ns": true})
		for _, m := range []string{"model-a", "model-b"} {
			_, _, _ = c.queryReplicaSeries(context.Background(), m, "ns", nil)
		}
		return registry
	}
	alone := run(false)
	assert.Equal(t, float64(len(predictionReadReplicaQueries)), errorsOfType(t, alone, constants.QueryTypeSLOModelInputs))
	assert.Zero(t, errorsOfType(t, alone, constants.QueryTypeTTFTInputs))
	assert.Zero(t, errorsOfType(t, alone, constants.QueryTypeLearnerRecording))

	withTarget := run(true)
	assert.Equal(t, float64(len(predictionReadReplicaQueries)-1), errorsOfType(t, withTarget, constants.QueryTypeSLOModelInputs))
	assert.Equal(t, 3.0, errorsOfType(t, withTarget, constants.QueryTypeTTFTInputs))
}

// collectPredicting is collectParsedCycle that also returns the rows the
// cycle kept for the v2 models, read before the cycle ends.
func collectPredicting(t *testing.T, series map[string]model.Value, begin func(*ReplicaMetricsCollector)) (
	rows []domain.ReplicaMetrics, kept []domain.ReplicaMetrics, keptOK bool) {
	t.Helper()
	reg := promclient.NewRegistry()
	require.NoError(t, metrics.InitMetrics(reg))
	all := map[string]model.Value{}
	for name, v := range series {
		all[name] = v
	}
	if _, ok := all[registration.QueryQueueLength]; !ok {
		all[registration.QueryQueueLength] = presenceSample(1)
	}
	cfg := prometheus.DefaultPrometheusSourceConfig()
	cfg.QueryTimeout = 100 * time.Millisecond
	src := prometheus.NewPrometheusSource(context.Background(), &promStub{series: all}, cfg)
	t.Cleanup(src.Close)
	c := NewReplicaMetricsCollector(parsedSource{src}, fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build(), nil, nil,
		scalerLocator(map[string]string{presencePod: "va-1"}))
	begin(c)
	defer c.EndCycle()
	rows, err := c.CollectReplicaMetrics(context.Background(), "test-model", "test-ns",
		make(map[string]scaletarget.ScaleTargetAccessor), make(map[string]*llmdvariant.Variant), nil)
	require.NoError(t, err)
	kept, keptOK = c.PredictionRows("test-ns", "test-model")
	return rows, kept, keptOK
}

// The point of the split: with every model input exported, the rows the
// analyzers get with --slo-v2-inputs on equal the rows they got with it
// off, with and without a TTFT target, and the v2 models' rows carry the
// inputs. A recording issues them anyway, so its rows keep them.
func TestPredictionRows_AnalyzersReadTheRowsOfACollectionWithoutIt(t *testing.T) {
	series := map[string]model.Value{
		registration.QueryAvgTTFT:               presenceSample(0.4),
		registration.QueryRequestRate:           presenceSample(2),
		registration.QueryAvgPrefillTime:        presenceSample(0.2),
		registration.QueryAvgQueueTime:          presenceSample(0.01),
		registration.QueryQueueMin:              presenceSample(0),
		registration.QueryAvgKVTransferTime:     presenceSample(0.03),
		registration.QueryQueueTimeBuckets:      ttftBucketSamples("test-model", map[string]float64{"0.1": 1, "+Inf": 2}),
		registration.QueryPromptTokensBuckets:   ttftBucketSamples("test-model", map[string]float64{"1000": 1, "+Inf": 2}),
		registration.QueryComputedTokensBuckets: ttftBucketSamples("test-model", map[string]float64{"500": 1, "+Inf": 2}),
		registration.QueryTTFTBuckets:           ttftBucketSamples("test-model", map[string]float64{"1": 1, "+Inf": 2}),
		registration.QueryTPOTBuckets:           ttftBucketSamples("test-model", map[string]float64{"0.05": 1, "+Inf": 2}),
	}
	cycle := func(ttft, predict bool) func(*ReplicaMetricsCollector) {
		return func(c *ReplicaMetricsCollector) {
			c.BeginCycle()
			if ttft {
				c.ReadTTFTInputsIn(map[string]bool{"test-ns": true})
			}
			if predict {
				c.ReadPredictionInputsIn(map[string]bool{"test-ns": true})
			}
		}
	}
	for _, ttft := range []bool{false, true} {
		off, _, offKept := collectPredicting(t, series, cycle(ttft, false))
		on, kept, ok := collectPredicting(t, series, cycle(ttft, true))
		require.Len(t, off, 1)
		assert.False(t, offKept, "off: no rows kept")
		assert.Equal(t, untimed(off), untimed(on), "ttft target %v: the analyzers' rows moved", ttft)
		require.True(t, ok)
		require.Len(t, kept, 1)
		k := kept[0]
		assert.True(t, k.AvgPrefillTimeKnown)
		assert.True(t, k.AvgQueueTimeKnown)
		assert.NotNil(t, k.QueueTimeBuckets)
		assert.NotNil(t, k.PromptTokensBuckets)
		assert.NotNil(t, k.ComputedTokensBuckets)
		assert.Nil(t, k.TTFTBuckets, "not a model input")
		assert.Equal(t, ttft, k.QueueMinKnown, "a TTFT reader's, issued only with the target")
	}

	recorded, _, _ := collectPredicting(t, series, func(c *ReplicaMetricsCollector) { c.BeginRecordingCycle(true) })
	both, kept, ok := collectPredicting(t, series, func(c *ReplicaMetricsCollector) {
		c.BeginRecordingCycle(true)
		c.ReadPredictionInputsIn(map[string]bool{"test-ns": true})
	})
	assert.Equal(t, untimed(recorded), untimed(both), "a recording's rows are its own")
	require.True(t, ok)
	assert.Equal(t, untimed(recorded), untimed(kept))
}

// untimed is rows with the wall-clock instants of their metadata taken out:
// two collections a few milliseconds apart differ there and nowhere else.
func untimed(rows []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, len(rows))
	for i, rm := range rows {
		if rm.Metadata != nil {
			md := *rm.Metadata
			md.CollectedAt, md.Age = time.Time{}, 0
			rm.Metadata = &md
		}
		out[i] = rm
	}
	return out
}

// Clearing a row clears its group's engines too, on a copy: the rows the
// v2 models keep share nothing it writes.
func TestClearPredictionOnly_GroupEnginesOnACopy(t *testing.T) {
	row := domain.ReplicaMetrics{PodName: "g-0", AvgPrefillTime: 0.2, AvgPrefillTimeKnown: true, AvgQueueTime: 0.1, AvgQueueTimeKnown: true,
		ComputedTokensBuckets: []domain.LengthBucket{{UpperBound: 1, Rate: 1}},
		GroupEngines: []domain.EngineReading{{PodName: "g-0", AvgPrefillTime: 0.2, AvgPrefillTimeKnown: true,
			AvgQueueTime: 0.1, AvgQueueTimeKnown: true, RequestRate: 1, RequestRateKnown: true}}}
	kept := []domain.ReplicaMetrics{row}
	out := []domain.ReplicaMetrics{row}
	clearPredictionOnly(out, true)
	assert.False(t, out[0].AvgPrefillTimeKnown)
	assert.False(t, out[0].GroupEngines[0].AvgPrefillTimeKnown)
	assert.False(t, out[0].GroupEngines[0].AvgQueueTimeKnown)
	assert.True(t, out[0].GroupEngines[0].RequestRateKnown, "a scaling field stays")
	assert.NotNil(t, out[0].ComputedTokensBuckets, "a TTFT target reads it anyway")
	assert.True(t, kept[0].GroupEngines[0].AvgPrefillTimeKnown, "the kept row was written through")
	clearPredictionOnly(out, false)
	assert.Nil(t, out[0].ComputedTokensBuckets)
}

// The router's reading for the v2 models carries the TPOT histogram and the
// TTFT mean; the slo analyzer's reading in the same cycle is what it was,
// and the histogram it alone asked for is counted under slo_model_inputs.
func TestCollectPredictionLatency_ItsOwnReading(t *testing.T) {
	src := &askingSource{}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
	c.BeginCycle()
	c.CollectPredictionLatency(context.Background(), "model-a", "ns")
	c.CollectRouterLatency(context.Background(), "model-a", "ns")
	c.EndCycle()
	for _, q := range []string{registration.QuerySLORouterTTFTBuckets, registration.QuerySLORouterTPOTSum,
		registration.QuerySLORouterTPOTCount, registration.QuerySLORouterTTFTSum, registration.QuerySLORouterTTFTCount,
		registration.QuerySLORouterTPOTBuckets} {
		assert.Equal(t, 1, src.asked[q], "%s once through the memo", q)
	}
	assert.Zero(t, src.asked[registration.QueryFlowControlWaitBuckets], "a recording's alone")

	registry := promclient.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	failing := &askingSource{result: func(q string) *source.MetricResult {
		return &source.MetricResult{QueryName: q, Error: errors.New("prometheus unreachable")}
	}}
	assert.Nil(t, NewReplicaMetricsCollector(failing, nil, nil, nil, nil).CollectPredictionLatency(context.Background(), "model-a", "ns"))
	assert.Equal(t, 1.0, errorsOfType(t, registry, constants.QueryTypeSLOModelInputs), "the TPOT histogram")
	assert.Equal(t, 2.0, errorsOfType(t, registry, constants.QueryTypeTTFTInputs), "the TTFT mean")
	assert.Equal(t, 3.0, errorsOfType(t, registry, constants.QueryTypeRouterLatency))
}
