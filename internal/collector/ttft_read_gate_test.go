package collector

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// The TTFT pricing's inputs (Q1, docs/proposals/slo-capacity-planner.md) are
// read every cycle in a namespace with a TTFT target, cost nothing elsewhere,
// and a series that is missing reads unknown.

var ttftReadNames = []string{
	registration.QueryQueueMin, registration.QueryComputedTokensBuckets, registration.QueryAvgKVTransferTime,
}

var ttftRecordOnlyNames = []string{
	registration.QueryPromptTokensBuckets, registration.QueryTTFTBuckets, registration.QueryAvgQueueTime,
	registration.QueryQueueTimeBuckets, registration.QueryAvgPrefillTime, registration.QueryTPOTBuckets,
}

// askedIn runs one cycle over namespaces, two models each, reading the TTFT
// inputs for targets, and returns how often each query was asked.
func askedIn(c *ReplicaMetricsCollector, src *askingSource, targets map[string]bool, namespaces ...string) map[string]int {
	src.asked = nil
	c.BeginCycle()
	defer c.EndCycle()
	c.ReadTTFTInputsIn(targets)
	for _, ns := range namespaces {
		for _, m := range []string{"model-a", "model-b"} {
			_, _, _ = c.queryReplicaSeries(context.Background(), m, ns, nil)
		}
	}
	return src.asked
}

func TestTTFTReadQueries_OncePerNamespaceWithATarget(t *testing.T) {
	src := &askingSource{}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)

	base := askedIn(c, src, nil, "with-target")
	for _, q := range ttftReadNames {
		assert.Zero(t, base[q], "no target: %s was asked", q)
	}

	// The queries are namespace-scoped, so one execution serves both models,
	// and the namespace with no target is not asked.
	asked := askedIn(c, src, map[string]bool{"with-target": true}, "with-target")
	for _, q := range ttftReadNames {
		assert.Equal(t, 1, asked[q], "%s once for the namespace", q)
	}
	for _, q := range ttftRecordOnlyNames {
		assert.Zero(t, asked[q], "%s stays record-only", q)
	}
	asked = askedIn(c, src, map[string]bool{"with-target": true}, "other")
	for _, q := range ttftReadNames {
		assert.Zero(t, asked[q], "%s asked for a namespace with no target", q)
	}
}

// The budget: what a target adds to a namespace's replica queries a cycle, by
// engine. Three on vLLM, and on a mixed namespace one more, SGLang's variant
// of the queue minimum. Recording adds to the scaling queries, as ever.
func TestTTFTReadQueries_TheBudget(t *testing.T) {
	vllm := []inferenceengine.Engine{inferenceengine.EngineVLLM}
	sglang := []inferenceengine.Engine{inferenceengine.EngineSGLang}
	mixed := []inferenceengine.Engine{inferenceengine.EngineVLLM, inferenceengine.EngineSGLang}
	count := func(engines []inferenceengine.Engine, recording, ttft bool) int {
		spec, agnostic := replicaQueryLists(recording, ttft)
		return len(buildEngineQueryList(engines, spec, agnostic))
	}
	for _, tc := range []struct {
		name    string
		engines []inferenceengine.Engine
		scaling int
		ttft    int
		record  int
	}{
		{"vLLM", vllm, 26, 29, 35},
		{"SGLang", sglang, 26, 29, 35},
		{"mixed", mixed, 45, 49, 59},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.scaling, count(tc.engines, false, false), "scaling")
			assert.Equal(t, tc.ttft, count(tc.engines, false, true), "with a TTFT target")
			assert.Equal(t, tc.record, count(tc.engines, true, false), "recording")
			assert.Equal(t, tc.record, count(tc.engines, true, true), "recording holds every one already")
		})
	}
}

// The list a collection issues holds each query once, and every replica query
// is in exactly one class: a query added to neither is a scaling query.
func TestTTFTReadQueries_TheClassesAreDisjoint(t *testing.T) {
	for q := range ttftReadReplicaQueries {
		assert.False(t, recordOnlyReplicaQueries[q], "%s is in both classes", q)
	}
	for _, recording := range []bool{false, true} {
		for _, ttft := range []bool{false, true} {
			spec, agnostic := replicaQueryLists(recording, ttft)
			seen := map[string]bool{}
			for _, q := range append(append([]string{}, spec...), agnostic...) {
				assert.False(t, seen[q], "%s listed twice", q)
				seen[q] = true
			}
		}
	}
}

func TestTTFTReadQueries_TheNamespacesAreTheCyclesOwn(t *testing.T) {
	src := &askingSource{}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
	targets := map[string]bool{"ns": true}

	askedIn(c, src, targets, "ns")
	assert.False(t, c.readsTTFTInputs("ns"), "EndCycle must clear them")

	// A new cycle starts with none, whatever the last one named.
	c.BeginCycle()
	assert.False(t, c.readsTTFTInputs("ns"))
	c.ReadTTFTInputsIn(targets)
	c.BeginRecordingCycle(true)
	assert.False(t, c.readsTTFTInputs("ns"), "a cycle opened over one must not inherit its namespaces")
	c.EndCycle()

	// Outside a cycle nothing is held, so nothing can leak into the next.
	c.ReadTTFTInputsIn(targets)
	assert.False(t, c.readsTTFTInputs("ns"))
}

// A failed query of the three is counted once per query sent, not per model
// reading it from the memo, under its own type and with no recording on.
func TestTTFTReadQueries_FailureCountedOnceUnderItsOwnType(t *testing.T) {
	registry := promclient.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	src := &askingSource{result: func(q string) *source.MetricResult {
		return &source.MetricResult{QueryName: q, Error: errors.New("query timed out")}
	}}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
	askedIn(c, src, map[string]bool{"ns": true}, "ns")
	assert.Equal(t, 3.0, errorsOfType(t, registry, constants.QueryTypeTTFTInputs))
	assert.Zero(t, errorsOfType(t, registry, constants.QueryTypeLearnerRecording))
}

// A namespace with a target pays no log line: at the shipped verbosity a
// collection logs the same with the three queries on as with them off, and
// with each of them failing.
func TestTTFTReadQueries_AddNoLogLine(t *testing.T) {
	lines := func(targets map[string]bool, fail bool) int {
		n := 0
		ctx := logr.NewContext(context.Background(), funcr.New(func(string, string) { n++ }, funcr.Options{Verbosity: 2}))
		registry := promclient.NewRegistry()
		require.NoError(t, metrics.InitMetrics(registry))
		src := &askingSource{}
		if fail {
			src.result = func(q string) *source.MetricResult {
				if ttftReadReplicaQueries[q] {
					return &source.MetricResult{QueryName: q, Error: errors.New("query timed out")}
				}
				return &source.MetricResult{QueryName: q}
			}
		}
		c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
		c.BeginCycle()
		defer c.EndCycle()
		c.ReadTTFTInputsIn(targets)
		for _, m := range []string{"model-a", "model-b"} {
			_, _, err := c.queryReplicaSeries(ctx, m, "ns", nil)
			require.NoError(t, err)
		}
		return n
	}
	target := map[string]bool{"ns": true}
	assert.Equal(t, lines(nil, false), lines(target, false), "with the queries on")
	assert.Equal(t, lines(nil, false), lines(target, true), "with the queries failing")
}

// The three reach the row through the real parse path with no recording on,
// and the record-only ones do not.
func TestTTFTInputs_ReachTheRowInANamespaceWithATarget(t *testing.T) {
	series := map[string]model.Value{
		registration.QueryQueueMin:              presenceSample(2),
		registration.QueryAvgKVTransferTime:     presenceSample(0.03),
		registration.QueryComputedTokensBuckets: ttftBucketSamples("test-model", map[string]float64{"1000": 1, "+Inf": 2}),
		registration.QueryAvgPrefillTime:        presenceSample(0.5),
		registration.QueryPromptTokensBuckets:   ttftBucketSamples("test-model", map[string]float64{"+Inf": 2}),
	}
	rows := func(targets map[string]bool) domain.ReplicaMetrics {
		got := collectParsedCycle(t, series, nil, nil, func(c *ReplicaMetricsCollector) {
			c.BeginCycle()
			c.ReadTTFTInputsIn(targets)
		})
		require.Len(t, got, 1)
		return got[0]
	}

	on := rows(map[string]bool{"test-ns": true})
	assert.True(t, on.QueueMinKnown)
	assert.Equal(t, 2.0, on.QueueMin)
	assert.True(t, on.AvgKVTransferTimeKnown)
	assert.Equal(t, 0.03, on.AvgKVTransferTime)
	assert.Equal(t, []domain.LengthBucket{{UpperBound: 1000, Rate: 1}, {UpperBound: math.Inf(1), Rate: 2}}, on.ComputedTokensBuckets)
	assert.False(t, on.AvgPrefillTimeKnown, "record-only")
	assert.Nil(t, on.PromptTokensBuckets, "record-only")

	off := rows(map[string]bool{"elsewhere": true})
	assert.False(t, off.QueueMinKnown)
	assert.False(t, off.AvgKVTransferTimeKnown)
	assert.Nil(t, off.ComputedTokensBuckets)
}

// A pod that exports the series not at all, which is every SGLang pod and
// every pod with no NIXL connector, or a query that failed: unknown, never a
// zero. A zero would read as an empty queue, the difference between a busy
// minute and none, and as no spread.
func TestTTFTInputs_AMissingSeriesReadsUnknown(t *testing.T) {
	begin := func(c *ReplicaMetricsCollector) {
		c.BeginCycle()
		c.ReadTTFTInputsIn(map[string]bool{"test-ns": true})
	}
	check := func(name string, rm domain.ReplicaMetrics) {
		t.Helper()
		assert.False(t, rm.QueueMinKnown, name)
		assert.Zero(t, rm.QueueMin, name)
		assert.False(t, rm.AvgKVTransferTimeKnown, name)
		assert.Zero(t, rm.AvgKVTransferTime, name)
		assert.Nil(t, rm.ComputedTokensBuckets, name)
	}
	absent := collectParsedCycle(t, nil, nil, nil, begin)
	require.Len(t, absent, 1)
	check("absent", absent[0])

	nan := collectParsedCycle(t, map[string]model.Value{
		registration.QueryQueueMin:              presenceSample(math.NaN()),
		registration.QueryAvgKVTransferTime:     presenceSample(math.NaN()),
		registration.QueryComputedTokensBuckets: ttftBucketSamples("test-model", map[string]float64{"+Inf": math.NaN()}),
	}, nil, nil, begin)
	require.Len(t, nan, 1)
	check("NaN", nan[0])

	failed := collectParsedCycle(t, nil, map[string]bool{
		registration.QueryQueueMin: true, registration.QueryAvgKVTransferTime: true,
		registration.QueryComputedTokensBuckets: true,
	}, nil, begin)
	require.Len(t, failed, 1)
	check("failed", failed[0])
}

// The query names the failure counter and the log carry are the logical ones
// a mixed namespace splits by engine: SGLang's queue minimum is one of the
// three too.
func TestTTFTReadQueries_AMixedNamespaceTypesBothVariants(t *testing.T) {
	mixed := []inferenceengine.Engine{inferenceengine.EngineVLLM, inferenceengine.EngineSGLang}
	got := buildEngineQueryList(mixed, ttftReadEngineSpecificQueries, ttftReadAgnosticQueries)
	assert.Contains(t, got, registration.QueryQueueMin)
	sglang := registration.EngineQuery(inferenceengine.EngineSGLang, registration.QueryQueueMin)
	assert.Contains(t, got, sglang)
	assert.True(t, strings.Contains(sglang, registration.QueryQueueMin))
	assert.Len(t, got, 4)
}
