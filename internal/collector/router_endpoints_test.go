package collector

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// routerSeries is one router series: the router Pod it was scraped from
// and its other labels.
func routerSeries(router string, v float64, kv ...string) source.MetricValue {
	l := map[string]string{routerPodLabel: router}
	for i := 0; i+1 < len(kv); i += 2 {
		l[kv[i]] = kv[i+1]
	}
	return source.MetricValue{Labels: l, Value: v}
}

// endpointResults is a P/D namespace with three router Pods: epp-a and
// epp-b serve model-a, epp-c serves model-b and also reads model-a's pool.
// decode-0 and decode-1 take model-a's placements (the decode profile is
// the primary one); prefill-0 has none. decode-1 runs two engine ranks.
func endpointResults() map[string]*source.MetricResult {
	ttft := &source.MetricResult{Values: []source.MetricValue{
		{Labels: map[string]string{seriesTargetModelLabel: "model-a", "le": "+Inf"}, Value: 1},
		{Labels: map[string]string{seriesTargetModelLabel: "model-b", "le": "+Inf"}, Value: 1}}}
	place := func(router, model, pod string, v float64) source.MetricValue {
		return routerSeries(router, v, seriesTargetModelLabel, model, "endpoint_name", pod)
	}
	queue := func(router, endpoint string, v float64) source.MetricValue {
		return routerSeries(router, v, "model_server_endpoint", endpoint)
	}
	inflight := func(router, endpoint string, v float64) source.MetricValue {
		return routerSeries(router, v, "endpoint_name", endpoint)
	}
	producer := func(router, plugin, role string, v float64) source.MetricValue {
		return routerSeries(router, v, "plugin_name", plugin, "endpoint_role", role)
	}
	return map[string]*source.MetricResult{
		registration.QuerySLORouterTTFTBuckets: ttft,
		registration.QuerySLORouterEndpointPlacements: {Values: []source.MetricValue{
			place("epp-a", "model-a", "decode-0", 3), place("epp-a", "model-a", "decode-1", 1),
			place("epp-b", "model-a", "decode-0", 2),
			// A rank-suffixed endpoint name: the decode-1 Pod's second
			// rank, folded into the same Pod.
			place("epp-b", "model-a", "decode-1-rank-1", 0.5),
			place("epp-c", "model-b", "other-0", 7),
			// A standby replica: its counter is there, at rate 0.
			place("epp-d", "model-a", "decode-0", 0),
		}},
		registration.QuerySLORouterEndpointQueue: {Values: []source.MetricValue{
			queue("epp-a", "decode-0-rank-0", 4), queue("epp-b", "decode-0-rank-0", 5),
			queue("epp-a", "decode-1-rank-0", 1), queue("epp-a", "decode-1-rank-1", 2),
			queue("epp-a", "prefill-0-rank-0", 6),
			queue("epp-c", "decode-0-rank-0", 50), queue("epp-d", "decode-0-rank-0", 60),
		}},
		registration.QuerySLORouterEndpointInFlightRequests: {Values: []source.MetricValue{
			inflight("epp-a", "decode-0-rank-0", 10), inflight("epp-b", "decode-0-rank-0", 6),
			inflight("epp-a", "prefill-0-rank-0", 2),
			inflight("epp-c", "decode-0-rank-0", 100),
		}},
		registration.QuerySLORouterEndpointInFlightTokens: {Values: []source.MetricValue{
			inflight("epp-a", "prefill-0-rank-0", 9000), inflight("epp-b", "prefill-0-rank-0", 1000),
			inflight("epp-c", "prefill-0-rank-0", 99999),
		}},
		registration.QuerySLORouterPrefixRequests: {Values: []source.MetricValue{
			producer("epp-a", "approx", "prefill", 2), producer("epp-b", "approx", "prefill", 1),
			producer("epp-a", "decode-prefix", "decode", 2), producer("epp-c", "approx", "prefill", 40)}},
		registration.QuerySLORouterPrefixPrompt: {Values: []source.MetricValue{
			producer("epp-a", "approx", "prefill", 2000), producer("epp-b", "approx", "prefill", 1000),
			producer("epp-a", "decode-prefix", "decode", 2000), producer("epp-c", "approx", "prefill", 40000)}},
		registration.QuerySLORouterPrefixPredicted: {Values: []source.MetricValue{
			producer("epp-a", "approx", "prefill", 600), producer("epp-b", "approx", "prefill", 365),
			producer("epp-a", "decode-prefix", "decode", 0), producer("epp-c", "approx", "prefill", 30000)}},
		registration.QuerySLORouterPrefixBestPredicted: {Values: []source.MetricValue{
			producer("epp-a", "approx", "prefill", 610), producer("epp-b", "approx", "prefill", 360),
			producer("epp-a", "decode-prefix", "decode", 0), producer("epp-c", "approx", "prefill", 30000)}},
		registration.QuerySLORouterPrefixBestAvailable: {Values: []source.MetricValue{
			producer("epp-a", "approx", "prefill", 611), producer("epp-b", "approx", "prefill", 360),
			producer("epp-a", "decode-prefix", "decode", 979), producer("epp-c", "approx", "prefill", 31000)}},
	}
}

// collectEndpoints is one recording cycle's (or not) router latency of
// model-a over results.
func collectEndpoints(t *testing.T, results map[string]*source.MetricResult, record bool) *domain.RouterLatency {
	t.Helper()
	src := &mockMetricsSource{refreshFunc: func(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
		return results, nil
	}}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, nil)
	c.BeginRecordingCycle(record)
	defer c.EndCycle()
	return c.CollectRouterLatency(context.Background(), "model-a", "ns")
}

// TestCollectRouterLatency_Endpoints pins the router's per-endpoint series
// as the recording keeps them (#475): read from the router Pods that serve
// the model only, so a second replica routing another model and a standby
// that routes nothing are not read, even where they report the same
// endpoints; an endpoint's ranks are one Pod; the queue, which every
// replica reads from the same model server, is the highest over the
// serving routers, and the in-flight load, which each replica holds for
// itself, their sum; a series the router publishes not for a Pod is
// unknown there, not zero.
func TestCollectRouterLatency_Endpoints(t *testing.T) {
	got := collectEndpoints(t, endpointResults(), true)
	require.NotNil(t, got)
	assert.Equal(t, []domain.RouterEndpoint{
		{Pod: "decode-0", PlacementRate: 5, PlacementKnown: true, QueueSize: 5, QueueSizeKnown: true,
			InFlightRequests: 16, InFlightRequestsKnown: true},
		{Pod: "decode-1", PlacementRate: 1.5, PlacementKnown: true, QueueSize: 3, QueueSizeKnown: true},
		{Pod: "prefill-0", QueueSize: 6, QueueSizeKnown: true, InFlightRequests: 2, InFlightRequestsKnown: true,
			InFlightTokens: 10000, InFlightTokensKnown: true},
	}, got.Endpoints)

	// The prefix producers, per producer and role, summed over the serving
	// routers; epp-c's model-b counts are not model-a's.
	assert.Equal(t, []domain.RouterPrefixProducer{
		{Plugin: "approx", Role: "prefill", RequestRate: 3, PromptTokenRate: 3000, PredictedTokenRate: 965,
			BestPredictedTokenRate: 970, BestAvailableTokenRate: 971},
		{Plugin: "decode-prefix", Role: "decode", RequestRate: 2, PromptTokenRate: 2000, BestAvailableTokenRate: 979},
	}, got.PrefixProducers)
	// The model-level pair joins at the namespace, as before: the router
	// TTFT names two models there, so it joins neither.
	assert.Zero(t, got.PrefixPredictedTokenRate, "two models behind the namespace's routers")
	assert.Zero(t, got.PrefixBestAvailableTokenRate)

	// One router Pod serving two models serves neither.
	both := endpointResults()
	both[registration.QuerySLORouterEndpointPlacements].Values = append(both[registration.QuerySLORouterEndpointPlacements].Values,
		routerSeries("epp-a", 1, seriesTargetModelLabel, "model-b", "endpoint_name", "other-0"))
	got = collectEndpoints(t, both, true)
	require.NotNil(t, got)
	assert.Equal(t, []domain.RouterEndpoint{
		{Pod: "decode-0", PlacementRate: 2, PlacementKnown: true, QueueSize: 5, QueueSizeKnown: true,
			InFlightRequests: 6, InFlightRequestsKnown: true},
		{Pod: "decode-1", PlacementRate: 0.5, PlacementKnown: true},
		{Pod: "prefill-0", InFlightTokens: 1000, InFlightTokensKnown: true},
	}, got.Endpoints, "epp-b alone serves model-a now")

	// A failed gauge query leaves its figure unknown and the rest read.
	failed := endpointResults()
	failed[registration.QuerySLORouterEndpointQueue].Error = errors.New("timeout")
	got = collectEndpoints(t, failed, true)
	require.NotNil(t, got)
	require.Len(t, got.Endpoints, 3)
	for _, e := range got.Endpoints {
		assert.False(t, e.QueueSizeKnown, "%s: a failed query is not a zero", e.Pod)
	}
	assert.Equal(t, 5.0, got.Endpoints[0].PlacementRate)
	assert.True(t, got.Endpoints[2].InFlightTokensKnown, "prefill-0 is still read from its in-flight series")
	assert.NotNil(t, got.PrefixProducers)

	// A producer missing from one of the five queries is left out, not
	// given a zero there; the others stay.
	partial := endpointResults()
	prompt := partial[registration.QuerySLORouterPrefixPrompt]
	prompt.Values = slices.DeleteFunc(prompt.Values, func(v source.MetricValue) bool {
		return v.Labels["plugin_name"] == "decode-prefix"
	})
	got = collectEndpoints(t, partial, true)
	require.NotNil(t, got)
	require.Len(t, got.PrefixProducers, 1)
	assert.Equal(t, "approx", got.PrefixProducers[0].Plugin)

	// A Pod a serving router placed on before but not over the window: its
	// counter stood still, and reads a known zero, not an unknown.
	stale := endpointResults()
	stale[registration.QuerySLORouterEndpointPlacements].Values = append(stale[registration.QuerySLORouterEndpointPlacements].Values,
		routerSeries("epp-a", 0, seriesTargetModelLabel, "model-a", "endpoint_name", "decode-2"))
	got = collectEndpoints(t, stale, true)
	require.NotNil(t, got)
	require.Len(t, got.Endpoints, 4)
	assert.Equal(t, domain.RouterEndpoint{Pod: "decode-2", PlacementKnown: true}, got.Endpoints[2])

	// A failed prefix query: no producer, rather than one that counted none.
	failed = endpointResults()
	failed[registration.QuerySLORouterPrefixBestPredicted].Error = errors.New("timeout")
	got = collectEndpoints(t, failed, true)
	require.NotNil(t, got)
	assert.Nil(t, got.PrefixProducers)
	assert.NotNil(t, got.Endpoints)

	// A failed placement query, or no router that placed this model: no
	// serving router is known, so nothing per endpoint is read.
	failed = endpointResults()
	failed[registration.QuerySLORouterEndpointPlacements].Error = errors.New("timeout")
	got = collectEndpoints(t, failed, true)
	require.NotNil(t, got)
	assert.Nil(t, got.Endpoints)
	assert.Nil(t, got.PrefixProducers)
	idle := endpointResults()
	idle[registration.QuerySLORouterEndpointPlacements] = &source.MetricResult{Values: []source.MetricValue{
		routerSeries("epp-a", 0, seriesTargetModelLabel, "model-a", "endpoint_name", "decode-0")}}
	got = collectEndpoints(t, idle, true)
	require.NotNil(t, got)
	assert.Nil(t, got.Endpoints, "no positive placement: no serving router")

	// Not recording, none of it is asked.
	got = collectEndpoints(t, endpointResults(), false)
	require.NotNil(t, got)
	assert.Nil(t, got.Endpoints)
	assert.Nil(t, got.PrefixProducers)
}

// The per-endpoint series alone keep the router reading: a cycle whose
// router publishes them and nothing else model-level is still recorded.
func TestCollectRouterLatency_EndpointsAloneAreAReading(t *testing.T) {
	all := endpointResults()
	results := map[string]*source.MetricResult{}
	for _, q := range []string{registration.QuerySLORouterEndpointPlacements, registration.QuerySLORouterEndpointQueue} {
		results[q] = all[q]
	}
	got := collectEndpoints(t, results, true)
	require.NotNil(t, got)
	assert.NotNil(t, got.Endpoints)
	assert.Nil(t, got.PrefixProducers, "a missing prefix query is a failed one")
}

func TestEndpointPod(t *testing.T) {
	for in, want := range map[string]string{
		"decode-0-rank-0":        "decode-0",
		"decode-0-rank-12":       "decode-0",
		"decode-0":               "decode-0",
		"my-rank-pod-rank-1":     "my-rank-pod",
		"pod-rank-x":             "pod-rank-x",
		"vllm-7d9f-abcde-rank-3": "vllm-7d9f-abcde",
	} {
		assert.Equal(t, want, endpointPod(in), in)
	}
}
