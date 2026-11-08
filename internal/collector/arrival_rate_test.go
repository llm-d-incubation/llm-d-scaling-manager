package collector

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// A failed arrival query is counted once, by the model whose call sent it.
// The second model in the namespace reads the failed result from the cycle's
// memo and does not count it again: counted per model, one failure in a
// namespace of N models read as N. The next cycle sends the query again, and
// counts it again.
func TestCollectModelArrivalRate_FailedQueryCountedOnce(t *testing.T) {
	registry := promclient.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	refreshes := 0
	src := &mockMetricsSource{refreshFunc: func(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
		refreshes++
		return map[string]*source.MetricResult{
			registration.QueryModelArrivalRate: {Error: errors.New("query timed out")},
		}, nil
	}}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)

	c.BeginCycle()
	assert.Zero(t, c.CollectModelArrivalRate(context.Background(), "model-a", "ns"))
	assert.Zero(t, c.CollectModelArrivalRate(context.Background(), "model-b", "ns"))
	c.EndCycle()
	assert.Equal(t, 1, refreshes, "the second model reads the memo")
	assert.Equal(t, 1.0, errorsOfType(t, registry, constants.QueryTypeArrivalRate))

	c.BeginCycle()
	assert.Zero(t, c.CollectModelArrivalRate(context.Background(), "model-a", "ns"))
	c.EndCycle()
	assert.Equal(t, 2, refreshes)
	assert.Equal(t, 2.0, errorsOfType(t, registry, constants.QueryTypeArrivalRate))
}

// A refresh that fails as a whole memoizes nothing, so each model's call
// sends the query again and each is counted, as a query that really ran.
func TestCollectModelArrivalRate_RefreshErrorCountedPerCall(t *testing.T) {
	registry := promclient.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	src := &mockMetricsSource{refreshFunc: func(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
		return nil, errors.New("prometheus unreachable")
	}}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
	c.BeginCycle()
	defer c.EndCycle()
	assert.Zero(t, c.CollectModelArrivalRate(context.Background(), "model-a", "ns"))
	assert.Zero(t, c.CollectModelArrivalRate(context.Background(), "model-b", "ns"))
	assert.Equal(t, 2.0, errorsOfType(t, registry, constants.QueryTypeArrivalRate))
}

// The shape the Prometheus source returns when the arrival query, the only
// query in its refresh, fails: the failed result and an error, since no query
// succeeded. The first model counts the failed refresh; the result is
// memoized, so the second model reads it from the memo, sends nothing, and
// does not count it again.
func TestCollectModelArrivalRate_FailedRefreshWithResultsCountedOnce(t *testing.T) {
	registry := promclient.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	refreshes := 0
	src := &mockMetricsSource{refreshFunc: func(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
		refreshes++
		return map[string]*source.MetricResult{
			registration.QueryModelArrivalRate: {Error: errors.New("query timed out")},
		}, errors.New("all 1 Prometheus queries failed: query timed out")
	}}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
	c.BeginCycle()
	defer c.EndCycle()
	assert.Zero(t, c.CollectModelArrivalRate(context.Background(), "model-a", "ns"))
	assert.Zero(t, c.CollectModelArrivalRate(context.Background(), "model-b", "ns"))
	assert.Equal(t, 1, refreshes, "the second model reads the memo")
	assert.Equal(t, 1.0, errorsOfType(t, registry, constants.QueryTypeArrivalRate))
}

// arrivalSeriesLogged runs CollectModelArrivalRate for model-a and returns
// the rate and the series count its debug line reports.
func arrivalSeriesLogged(t *testing.T, c *ReplicaMetricsCollector) (float64, float64) {
	t.Helper()
	series := math.NaN()
	logger := funcr.NewJSON(func(obj string) {
		var line map[string]any
		require.NoError(t, json.Unmarshal([]byte(obj), &line))
		if line["msg"] != "Collected model arrival rate" {
			return
		}
		n, ok := line["series"].(float64)
		require.True(t, ok, "the line reports the series it read: %s", obj)
		series = n
	}, funcr.Options{Verbosity: logging.DEBUG})
	rate := c.CollectModelArrivalRate(logr.NewContext(context.Background(), logger), "model-a", "ns")
	return rate, series
}

// A NaN or infinite arrival series, through the real Prometheus parse path
// (model.Vector), is skipped: the source stores it as 0 with a flag set, and
// a check of the raw value read it as a series of rate 0.
func TestCollectModelArrivalRate_NaNSeriesThroughPrometheusParse(t *testing.T) {
	c := parsedRouterCollector(t, map[string]model.Value{
		registration.QueryModelArrivalRate: model.Vector{
			fcSample(1.5, seriesTargetModelLabel, "model-a"),
			fcSample(math.NaN(), seriesTargetModelLabel, "model-a"),
			fcSample(math.Inf(1), seriesTargetModelLabel, "model-a"),
			fcSample(9, seriesTargetModelLabel, "model-b"),
		},
	})
	rate, series := arrivalSeriesLogged(t, c)
	assert.Equal(t, 1.5, rate)
	assert.Equal(t, 1.0, series, "the NaN and the infinite series are not read")

	// Only NaN: no series read, not one read at 0.
	c = parsedRouterCollector(t, map[string]model.Value{
		registration.QueryModelArrivalRate: model.Vector{fcSample(math.NaN(), seriesTargetModelLabel, "model-a")},
	})
	rate, series = arrivalSeriesLogged(t, c)
	assert.Zero(t, rate)
	assert.Zero(t, series)
}

// A NaN scheduler queue series, through the real Prometheus parse path, adds
// nothing to the sum but still counts as present. A non-nil result is what
// AnalyzerInput.FlowControlSeries reads as the router publishing its queue
// series, so a NaN-only series must not read as no series: that would flip
// the slo analyzer's inferred gate from on to off.
func TestCollectSchedulerQueueMetrics_NaNSeriesThroughPrometheusParse(t *testing.T) {
	c := parsedRouterCollector(t, map[string]model.Value{
		registration.QuerySchedulerQueueSize:  model.Vector{fcSample(math.NaN(), seriesTargetModelLabel, "model-a")},
		registration.QuerySchedulerQueueBytes: model.Vector{fcSample(math.Inf(1), seriesTargetModelLabel, "model-a")},
	})
	nanOnly := c.CollectSchedulerQueueMetrics(context.Background(), "model-a", "ns")
	require.NotNil(t, nanOnly, "a NaN series is a published series")
	assert.Equal(t, int64(0), nanOnly.QueueSize)
	assert.Equal(t, int64(0), nanOnly.QueueBytes)

	c = parsedRouterCollector(t, map[string]model.Value{
		registration.QuerySchedulerQueueSize: model.Vector{
			fcSample(3, seriesTargetModelLabel, "model-a"),
			fcSample(math.NaN(), seriesTargetModelLabel, "model-a"),
		},
	})
	got := c.CollectSchedulerQueueMetrics(context.Background(), "model-a", "ns")
	require.NotNil(t, got)
	assert.Equal(t, int64(3), got.QueueSize)
	assert.Zero(t, got.QueueBytes)
}
