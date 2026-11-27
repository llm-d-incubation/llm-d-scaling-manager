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
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// podSamples is one series for each pod with value v and no model label, as
// the API server's HTTP series are.
func podSamples(v float64, pods ...string) model.Vector {
	vec := make(model.Vector, 0, len(pods))
	for _, p := range pods {
		vec = append(vec, &model.Sample{
			Metric:    model.Metric{"pod": model.LabelValue(p), "instance": "10.0.0.1:8000"},
			Value:     model.SampleValue(v),
			Timestamp: model.TimeFromUnixNano(time.Now().UnixNano()),
		})
	}
	return vec
}

// The API server's figures (registration/frontend.go), through the real
// Prometheus parse path: the frontend is the generation handlers' mean less
// vLLM's end-to-end mean, known only with both; the render mean and rate
// each on its own. The HTTP series carry no model and are attached by
// instance, never adding a Pod the model's own series did not find.
func TestFrontendThroughPrometheusParse(t *testing.T) {
	gen, ren, rate, e2e := registration.QueryAvgHTTPGenerationTime, registration.QueryAvgHTTPRenderTime,
		registration.QueryHTTPRenderRate, registration.QueryAvgE2ELatency
	all := map[string]model.Value{gen: podSamples(1.25, presencePod, "other-pod"), ren: podSamples(0.2, presencePod),
		rate: podSamples(3, presencePod), e2e: presenceSample(1.2)}

	got := collectParsed(t, all, nil, nil)
	assert.True(t, got.AvgFrontendTimeKnown)
	assert.InDelta(t, 0.05, got.AvgFrontendTime, 1e-12)
	assert.True(t, got.AvgRenderTimeKnown)
	assert.Equal(t, 0.2, got.AvgRenderTime)
	assert.True(t, got.RenderRateKnown)
	assert.Equal(t, 3.0, got.RenderRate)
	rows := collectParsedRows(t, all, nil, nil)
	assert.Len(t, rows, 1, "another pod's HTTP series adds no row")

	for _, tc := range []struct {
		name   string
		drop   string
		failed map[string]bool
	}{
		{"no end-to-end mean", e2e, nil},
		{"no generation handler", gen, nil},
		{"a failed end-to-end query", "", map[string]bool{e2e: true}},
	} {
		series := map[string]model.Value{}
		for k, v := range all {
			if k != tc.drop {
				series[k] = v
			}
		}
		got := collectParsed(t, series, tc.failed, nil)
		assert.False(t, got.AvgFrontendTimeKnown, tc.name)
		assert.Zero(t, got.AvgFrontendTime, tc.name)
		assert.True(t, got.RenderRateKnown, "%s: the render figures stand alone", tc.name)
	}

	got = collectParsed(t, map[string]model.Value{gen: podSamples(1.25, presencePod), e2e: presenceSample(math.NaN())}, nil, nil)
	assert.False(t, got.AvgFrontendTimeKnown, "a NaN end-to-end mean is no reading")
	got = collectParsed(t, map[string]model.Value{gen: podSamples(1.1, presencePod), e2e: presenceSample(1.2)}, nil, nil)
	assert.True(t, got.AvgFrontendTimeKnown, "a difference under 0 is a reading")
	assert.InDelta(t, -0.1, got.AvgFrontendTime, 1e-12)
	got = collectParsed(t, map[string]model.Value{e2e: presenceSample(1.2)}, nil, nil)
	assert.False(t, got.RenderRateKnown, "a pod the router does not render on")
	assert.False(t, got.AvgRenderTimeKnown)
}

// Not recording, none of the four is asked.
func TestFrontend_NotAskedWithoutARecording(t *testing.T) {
	src := &askingSource{}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
	collectAll(c, false)
	for _, q := range []string{registration.QueryAvgHTTPGenerationTime, registration.QueryAvgHTTPRenderTime,
		registration.QueryHTTPRenderRate, registration.QueryAvgE2ELatency} {
		assert.Zero(t, src.asked[q], q)
	}
}

// A pod's ranks: the frontend weighted by request rate, the render rate
// summed and its mean weighted by it, each known only where every rank read
// it. An LWS group's engines keep their own.
func TestMergePodInstances_Frontend(t *testing.T) {
	rank := func(rate, fe, ren, rr float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: presencePod, RequestRate: rate, RequestRateKnown: true,
			AvgFrontendTime: fe, AvgFrontendTimeKnown: true, AvgRenderTime: ren, AvgRenderTimeKnown: true,
			RenderRate: rr, RenderRateKnown: true}
	}
	pod := mergePodInstances([]domain.ReplicaMetrics{rank(1, 0.01, 0.2, 1), rank(3, 0.03, 0.4, 3)})
	assert.InDelta(t, 0.025, pod.AvgFrontendTime, 1e-12)
	assert.True(t, pod.AvgFrontendTimeKnown)
	assert.InDelta(t, 4, pod.RenderRate, 1e-12)
	assert.InDelta(t, 0.35, pod.AvgRenderTime, 1e-12)
	unknown := rank(1, 0.01, 0.2, 1)
	unknown.AvgFrontendTimeKnown, unknown.RenderRateKnown = false, false
	pod = mergePodInstances([]domain.ReplicaMetrics{rank(1, 0.01, 0.2, 1), unknown})
	assert.False(t, pod.AvgFrontendTimeKnown)
	assert.Zero(t, pod.AvgFrontendTime)
	assert.False(t, pod.RenderRateKnown)
	e := domain.EngineReadingOf(rank(2, 0.02, 0.3, 2))
	assert.Equal(t, 0.02, e.AvgFrontendTime)
	assert.True(t, e.AvgFrontendTimeKnown)
	assert.Equal(t, 0.3, e.AvgRenderTime)
	assert.Equal(t, 2.0, e.RenderRate)
}

// The router's render _sum and _count are joined as the processing series
// are: the namespace's, given to the one model the router's TTFT names;
// either query failed, neither; not asked while not recording.
func TestCollectRouterLatency_Render(t *testing.T) {
	ttftOf := func(models ...string) *source.MetricResult {
		r := &source.MetricResult{}
		for _, m := range models {
			r.Values = append(r.Values, source.MetricValue{Labels: map[string]string{seriesTargetModelLabel: m, "le": "+Inf"}, Value: 1})
		}
		return r
	}
	sum := &source.MetricResult{Values: []source.MetricValue{{Labels: map[string]string{"result": "success"}, Value: 0.4},
		{Labels: map[string]string{"result": "error"}, Value: 0.2}}}
	count := &source.MetricResult{Values: []source.MetricValue{{Labels: map[string]string{"result": "success"}, Value: 2},
		{Labels: map[string]string{"result": "error"}, Value: 1}}}
	results := func(ttft *source.MetricResult) map[string]*source.MetricResult {
		return map[string]*source.MetricResult{registration.QuerySLORouterTTFTBuckets: ttft,
			registration.QuerySLORouterRenderSum: sum, registration.QuerySLORouterRenderCount: count}
	}
	collect := func(results map[string]*source.MetricResult, record bool) *domain.RouterLatency {
		src := &mockMetricsSource{refreshFunc: func(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return results, nil
		}}
		c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
		c.BeginRecordingCycle(record)
		defer c.EndCycle()
		return c.CollectRouterLatency(context.Background(), "model-a", "ns")
	}

	got := collect(results(ttftOf("model-a")), true)
	require.NotNil(t, got)
	assert.InDelta(t, 0.6, got.RenderSumRate, 1e-12)
	assert.Equal(t, 3.0, got.RenderCountRate)

	got = collect(results(ttftOf("model-a", "model-b")), true)
	require.NotNil(t, got)
	assert.Zero(t, got.RenderCountRate, "two models behind the namespace's routers: the render is neither's")

	r := results(ttftOf("model-a"))
	r[registration.QuerySLORouterRenderCount] = &source.MetricResult{Error: errors.New("timeout"), Values: count.Values}
	got = collect(r, true)
	require.NotNil(t, got)
	assert.Zero(t, got.RenderSumRate, "a failed _count is no mean")
	assert.Zero(t, got.RenderCountRate)

	// The render alone keeps the struct for the recording.
	only := results(ttftOf("model-a"))
	only[registration.QuerySLORouterTTFTBuckets] = &source.MetricResult{Values: []source.MetricValue{
		{Labels: map[string]string{seriesTargetModelLabel: "model-a", "le": "+Inf"}, Value: math.NaN()}}}
	assert.Nil(t, collect(only, true), "no finite router TTFT, no join: nothing read")

	got = collect(results(ttftOf("model-a")), false)
	require.NotNil(t, got)
	assert.Zero(t, got.RenderCountRate, "not recording, the render series is not asked")
}
