package collector

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/prometheus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// poolSeries is one series of pool with extra labels.
func poolSeries(pool string, value float64, kv ...string) source.MetricValue {
	labels := map[string]string{poolLabel: pool}
	for i := 0; i+1 < len(kv); i += 2 {
		labels[kv[i]] = kv[i+1]
	}
	return source.MetricValue{Labels: labels, Value: value}
}

// collectPools runs CollectFlowControlPools for model-a against results,
// with a learner recording on.
func collectPools(t *testing.T, results map[string]*source.MetricResult, err error) ([]domain.FlowControlPool, map[string]string) {
	t.Helper()
	var params map[string]string
	src := &mockMetricsSource{refreshFunc: func(_ context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
		params = spec.Params
		return results, err
	}}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
	c.BeginRecordingCycle(true)
	defer c.EndCycle()
	return c.CollectFlowControlPools(context.Background(), "model-a", "ns"), params
}

// The pools joined to a model are those the queue gauge names with that model
// alone; each figure is read per pool, and the result is in pool-name order
// with no names. The saturation stages are the router's three, prefill,
// decode and effective; any other stage is not read as one of them.
func TestCollectFlowControlPools_JoinsAPoolServingTheModelAlone(t *testing.T) {
	results := map[string]*source.MetricResult{
		registration.QueryFlowControlPoolModels: {Values: []source.MetricValue{
			poolSeries("pool-b", 0, seriesTargetModelLabel, "model-a"),
			poolSeries("pool-a", 3, seriesModelLabel, "model-a"),
			// Shared: joined to neither model.
			poolSeries("pool-shared", 0, seriesTargetModelLabel, "model-a"),
			poolSeries("pool-shared", 0, seriesTargetModelLabel, "model-b"),
			// Another model's pool.
			poolSeries("pool-other", 0, seriesTargetModelLabel, "model-b"),
			// A series naming no model joins nothing.
			poolSeries("pool-a", 0),
		}},
		registration.QueryFlowControlPoolSaturation: {Values: []source.MetricValue{
			poolSeries("pool-a", 0.4, "stage", "prefill"),
			poolSeries("pool-a", 0.9, "stage", "decode"),
			poolSeries("pool-a", 0.9, "stage", "effective"),
			poolSeries("pool-b", 1, "stage", "effective"),
			poolSeries("pool-b", 0.3),
			poolSeries("pool-b", 0.5, "stage", "other"),
			poolSeries("pool-b", math.NaN(), "stage", "decode"),
		}},
		registration.QueryFlowControlOutcomes: {Values: []source.MetricValue{
			poolSeries("pool-a", 5, "outcome", "Dispatched"),
			poolSeries("pool-a", 0.5, "outcome", "RejectedCapacity"),
			poolSeries("pool-a", 0.25, "outcome", "RejectedOther"),
			poolSeries("pool-a", 0.1, "outcome", "EvictedTTL"),
			poolSeries("pool-a", 0.05, "outcome", "Something"),
			poolSeries("pool-shared", 7, "outcome", "Dispatched"),
		}},
	}
	got, params := collectPools(t, results, nil)
	assert.Equal(t, map[string]string{source.ParamNamespace: "ns"}, params)
	require.Len(t, got, 2, "pool-a and pool-b serve model-a alone")

	assert.Equal(t, domain.FlowControlPool{
		WindowSeconds:            registration.FlowControlWindowSeconds,
		EffectiveSaturation:      0.9,
		EffectiveSaturationKnown: true,
		PrefillSaturation:        0.4,
		PrefillSaturationKnown:   true,
		DecodeSaturation:         0.9,
		DecodeSaturationKnown:    true,
		DispatchedRate:           5,
		RejectedRate:             0.75,
		EvictedRate:              0.1,
		OtherOutcomeRate:         0.05,
		OutcomesKnown:            true,
	}, got[0])
	// pool-b: no outcome series is absent, not zeros; a stage-less and an
	// unknown stage are not read; a NaN stage is not read.
	assert.Equal(t, domain.FlowControlPool{
		WindowSeconds:            registration.FlowControlWindowSeconds,
		EffectiveSaturation:      1,
		EffectiveSaturationKnown: true,
	}, got[1])
}

// No pool joined is nil: flow control off (no gauge), a failed query, a
// source error, and a pool shared with another model all read as absent.
func TestCollectFlowControlPools_AbsentIsNil(t *testing.T) {
	shared := &source.MetricResult{Values: []source.MetricValue{
		poolSeries("p", 0, seriesTargetModelLabel, "model-a"),
		poolSeries("p", 0, seriesTargetModelLabel, "model-b"),
	}}
	served := &source.MetricResult{Values: []source.MetricValue{poolSeries("p", 0, seriesTargetModelLabel, "model-a")}}
	for name, tc := range map[string]struct {
		results map[string]*source.MetricResult
		err     error
	}{
		"flow control off":  {map[string]*source.MetricResult{}, nil},
		"shared pool":       {map[string]*source.MetricResult{registration.QueryFlowControlPoolModels: shared}, nil},
		"failed join query": {map[string]*source.MetricResult{registration.QueryFlowControlPoolModels: {Error: errors.New("timeout"), Values: served.Values}}, nil},
		"source error":      {nil, errors.New("prometheus unreachable")},
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := collectPools(t, tc.results, tc.err)
			assert.Nil(t, got)
		})
	}

	// Joined, with every per-pool query failed: the pool is there and every
	// figure is absent.
	failed := &source.MetricResult{Error: errors.New("timeout"), Values: []source.MetricValue{
		poolSeries("p", 1, "stage", "decode", "outcome", "Dispatched")}}
	got, _ := collectPools(t, map[string]*source.MetricResult{
		registration.QueryFlowControlPoolModels:     served,
		registration.QueryFlowControlPoolSaturation: failed,
		registration.QueryFlowControlOutcomes:       failed,
	}, nil)
	require.Len(t, got, 1)
	assert.Equal(t, domain.FlowControlPool{WindowSeconds: registration.FlowControlWindowSeconds}, got[0])
}

// fcSample is one flow-control series as Prometheus returns it.
func fcSample(v float64, labels ...string) *model.Sample {
	m := model.Metric{}
	for i := 0; i+1 < len(labels); i += 2 {
		m[model.LabelName(labels[i])] = model.LabelValue(labels[i+1])
	}
	return &model.Sample{Metric: m, Value: model.SampleValue(v), Timestamp: model.TimeFromUnixNano(time.Now().UnixNano())}
}

// parsedRouterCollector is a collector reading series through the real
// Prometheus source, in a cycle with a learner recording on, ended when the
// test is.
func parsedRouterCollector(t *testing.T, series map[string]model.Value) *ReplicaMetricsCollector {
	t.Helper()
	cfg := prometheus.DefaultPrometheusSourceConfig()
	cfg.QueryTimeout = 100 * time.Millisecond
	src := prometheus.NewPrometheusSource(context.Background(), &promStub{series: series}, cfg)
	t.Cleanup(src.Close)
	c := NewReplicaMetricsCollector(parsedSource{src}, nil, nil, nil, nil)
	c.BeginRecordingCycle(true)
	t.Cleanup(c.EndCycle)
	return c
}

// The flow-control series through the real Prometheus parse path
// (model.Vector): the wait histogram is read per model (target_model_name,
// else model_name), its +Inf bound parsed and a NaN bucket dropped; the
// pool's stages and outcomes are read per pool, a NaN stage not read.
func TestFlowControlThroughPrometheusParse(t *testing.T) {
	c := parsedRouterCollector(t, map[string]model.Value{
		registration.QuerySLORouterTTFTBuckets: model.Vector{
			fcSample(2, seriesTargetModelLabel, "model-a", "le", "+Inf"),
		},
		registration.QueryFlowControlWaitBuckets: model.Vector{
			fcSample(1.5, seriesTargetModelLabel, "model-a", "le", "0.05"),
			fcSample(0.5, seriesModelLabel, "model-a", "le", "0.05"),
			fcSample(2, seriesTargetModelLabel, "model-a", "le", "+Inf"),
			fcSample(math.NaN(), seriesTargetModelLabel, "model-a", "le", "1"),
			fcSample(9, seriesTargetModelLabel, "model-b", "le", "+Inf"),
		},
		registration.QueryFlowControlPoolModels: model.Vector{
			fcSample(0, poolLabel, "pool-a", seriesTargetModelLabel, "model-a"),
		},
		registration.QueryFlowControlPoolSaturation: model.Vector{
			fcSample(0.25, poolLabel, "pool-a", "stage", "prefill"),
			fcSample(math.NaN(), poolLabel, "pool-a", "stage", "decode"),
			fcSample(0.25, poolLabel, "pool-a", "stage", "effective"),
		},
		registration.QueryFlowControlOutcomes: model.Vector{
			fcSample(2, poolLabel, "pool-a", "outcome", "Dispatched"),
			fcSample(math.NaN(), poolLabel, "pool-a", "outcome", "RejectedCapacity"),
		},
	})

	rl := c.CollectRouterLatency(context.Background(), "model-a", "ns")
	require.NotNil(t, rl)
	assert.Equal(t, []domain.LatencyBucket{{UpperBound: 0.05, Rate: 2}, {UpperBound: math.Inf(1), Rate: 2}},
		rl.FlowControlWaitBuckets)

	pools := c.CollectFlowControlPools(context.Background(), "model-a", "ns")
	assert.Equal(t, []domain.FlowControlPool{{
		WindowSeconds:            registration.FlowControlWindowSeconds,
		EffectiveSaturation:      0.25,
		EffectiveSaturationKnown: true,
		PrefillSaturation:        0.25,
		PrefillSaturationKnown:   true,
		DispatchedRate:           2,
		OutcomesKnown:            true,
	}}, pools)

	// A wait histogram of NaNs, or of another model only, is none.
	c = parsedRouterCollector(t, map[string]model.Value{
		registration.QuerySLORouterTTFTBuckets: model.Vector{fcSample(2, seriesTargetModelLabel, "model-a", "le", "+Inf")},
		registration.QueryFlowControlWaitBuckets: model.Vector{
			fcSample(math.NaN(), seriesTargetModelLabel, "model-a", "le", "+Inf"),
			fcSample(9, seriesTargetModelLabel, "model-b", "le", "+Inf"),
		},
	})
	rl = c.CollectRouterLatency(context.Background(), "model-a", "ns")
	require.NotNil(t, rl)
	assert.Nil(t, rl.FlowControlWaitBuckets)
}
