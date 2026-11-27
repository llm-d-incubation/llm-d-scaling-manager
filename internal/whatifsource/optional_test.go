package whatifsource

import (
	"slices"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// optionalByName is the optional series named name.
func optionalByName(t *testing.T, name string) OptionalSeries {
	t.Helper()
	i := slices.IndexFunc(OptionalSeriesList, func(s OptionalSeries) bool { return s.Name == name })
	require.GreaterOrEqualf(t, i, 0, "%s is not an optional series", name)
	return OptionalSeriesList[i]
}

// foundSeries is what the capture recorded of name.
func foundSeries(t *testing.T, all []learnreplay.CaptureSeries, name string) learnreplay.CaptureSeries {
	t.Helper()
	i := slices.IndexFunc(all, func(c learnreplay.CaptureSeries) bool { return c.Name == name })
	require.GreaterOrEqualf(t, i, 0, "%s was not checked", name)
	return all[i]
}

// rising is a series that holds base at t0 and gains per every 15 s step.
func rising(labels map[string]string, base, per float64) promtest.Series {
	return promtest.Series{Labels: labels, Value: func(at time.Time) (float64, bool) {
		return base + per*float64(at.Sub(t0)/(15*time.Second)), true
	}}
}

// The new series land in the window line and the summary: a present
// counter with its increase, one broken down by its label, a present
// gauge, and every series the fleet does not export recorded as absent.
// None of them fails the capture.
func TestCapture_RecordsWhichOptionalSeriesTheWindowHeld(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	baseFleet(t, srv, testPod)
	hits := optionalByName(t, "vllm:external_prefix_cache_hits_total")
	bySource := optionalByName(t, "vllm:prompt_tokens_cached_by_source_total")
	info := optionalByName(t, "vllm:kv_offload_config_info")
	lmc := optionalByName(t, "lmcache_mp_lookup_hit_tokens_total")
	srv.Set(OptionalQuery(hits, testNS), rising(nil, 1000, 50))
	srv.Set(OptionalQuery(bySource, testNS),
		rising(map[string]string{"source": "device"}, 0, 30), rising(map[string]string{"source": "host"}, 10, 20))
	srv.Set(OptionalQuery(info, testNS), promtest.Const(nil, 2))
	srv.Set(OptionalQuery(lmc, testNS), rising(nil, 0, 7))
	opts := roleOfOpts(t, srv)
	opts.To = t0.Add(4 * 15 * time.Second)
	d, sum := capture(t, opts)

	require.Equal(t, d.window.Optional, sum.Optional, "the summary is the window line's")
	require.Len(t, sum.Optional, len(optionalFor(inferenceengine.EngineVLLM)), "every optional series of the engine is checked")
	got := foundSeries(t, sum.Optional, hits.Name)
	assert.True(t, got.Present)
	assert.InDelta(t, 4*50, got.Increase, 1e-9, "four steps of 50")
	got = foundSeries(t, sum.Optional, bySource.Name)
	assert.True(t, got.Present)
	assert.Equal(t, map[string]float64{"device": 120, "host": 80}, got.By)
	assert.InDelta(t, 200, got.Increase, 1e-9)
	got = foundSeries(t, sum.Optional, info.Name)
	assert.True(t, got.Present)
	assert.Zero(t, got.Increase, "a gauge has no increase")
	assert.True(t, foundSeries(t, sum.Optional, lmc.Name).Present, "LMCache's series are checked on a vLLM capture")

	present := 0
	for _, c := range sum.Optional {
		assert.Empty(t, c.Err, "%s: an absent series is not an error", c.Name)
		if c.Present {
			present++
		}
	}
	assert.Equal(t, 4, present, "only the four series the fleet exports read present")
	assert.False(t, foundSeries(t, sum.Optional, "vllm:kv_offload_tiering_chunk_hits_total").Present)

	lines, nPresent, nAbsent := OptionalText(sum.Optional)
	assert.Equal(t, 4, nPresent)
	assert.Equal(t, len(sum.Optional)-4, nAbsent)
	assert.Contains(t, lines, "present   vllm:prompt_tokens_cached_by_source_total  increase 200  device=120  host=80")
	assert.Contains(t, lines, "absent    vllm:kv_offload_tiering_chunk_hits_total")

	// Every request, the checks' included, is a range query at the step.
	for _, r := range srv.Requests() {
		assert.Equal(t, "/api/v1/query_range", r.Path)
		assert.Equal(t, 15*time.Second, r.Step)
	}
}

// A check whose query fails is recorded with its error and the capture
// goes on: the optional series are never required. The capture's own
// queries still are (TestCapture_AnyFailedQueryFailsTheCaptureAtOnce).
func TestCapture_AFailedOptionalCheckDoesNotFailTheCapture(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	baseFleet(t, srv, testPod)
	hits := optionalByName(t, "vllm:external_prefix_cache_hits_total")
	srv.Fail(OptionalQuery(hits, testNS), "query processing would load too many samples into memory")
	d, sum := capture(t, roleOfOpts(t, srv))

	require.Len(t, d.cycles, 1, "the capture went on")
	got := foundSeries(t, sum.Optional, hits.Name)
	assert.False(t, got.Present)
	assert.Contains(t, got.Err, "too many samples")
	lines, _, _ := OptionalText(sum.Optional)
	assert.Contains(t, lines[slices.IndexFunc(lines, func(l string) bool { return len(l) > 9 && l[:9] == "unchecked" })], hits.Name)
}

// A capture checks its own engine's series and the router's, LMCache's,
// kube-state-metrics' and DCGM's, never the other engine's.
func TestOptionalFor_ChecksTheCapturedEngineOnly(t *testing.T) {
	for engine, other := range map[inferenceengine.Engine]string{
		inferenceengine.EngineVLLM: SourceSGLang, inferenceengine.EngineSGLang: SourceVLLM} {
		sources := map[string]bool{}
		for _, s := range optionalFor(engine) {
			sources[s.Source] = true
		}
		assert.False(t, sources[other], "%s checks %s series", engine, other)
		assert.True(t, sources[SourceRouter] && sources[SourceLMCache] && sources[SourceSidecar] &&
			sources[SourceKSM] && sources[SourceDCGM],
			"%s skips the router's, LMCache's, the sidecar's, kube-state-metrics' or DCGM's series", engine)
	}
}

// The series that explain TTFT outside the engines are checked and filed
// under their own tier: the router's own time, the sidecar's prefill leg,
// the engine's front end.
func TestOptionalSeries_TTFTOutsideTheEngines(t *testing.T) {
	for _, name := range []string{
		"llm_d_epp_request_processing_duration_seconds_bucket", "llm_d_epp_token_producer_render_duration_seconds_sum",
		"llm_d_epp_plugin_duration_seconds_count", "llm_d_epp_request_size_bytes_sum", "llm_d_epp_request_duration_seconds_sum",
		"llm_d_disagg_sidecar_prefill_duration_seconds_sum", "vllm:e2e_request_latency_seconds_sum",
		"vllm:request_render_duration_seconds_count", "http_request_duration_seconds_count",
	} {
		assert.Equal(t, TierOutsideEngines, optionalByName(t, name).Tier, name)
	}
	assert.Equal(t, Tier("diagnostic: TTFT outside the engines"), TierOutsideEngines)
}

// A counter's increase is read across restarts, as rate() reads it: a
// fall is a restart, and the value after it counts in full.
func TestSummarize_ReadsACounterAcrossARestart(t *testing.T) {
	at := func(i int) model.Time {
		return model.TimeFromUnixNano(t0.Add(time.Duration(i) * 15 * time.Second).UnixNano())
	}
	sample := func(v float64) model.Vector {
		return model.Vector{&model.Sample{Metric: model.Metric{}, Value: model.SampleValue(v)}}
	}
	byTime := map[model.Time]model.Vector{at(0): sample(100), at(1): sample(160), at(2): sample(5), at(3): sample(25)}
	got := summarize(OptionalSeries{Name: "x_total"}, byTime)
	assert.True(t, got.Present)
	assert.InDelta(t, 60+5+20, got.Increase, 1e-9)

	got = summarize(OptionalSeries{Name: "x_bucket"}, byTime)
	assert.True(t, got.Present)
	assert.Zero(t, got.Increase, "a histogram's buckets are not summed")

	assert.False(t, summarize(OptionalSeries{Name: "x_total"}, map[model.Time]model.Vector{}).Present)
}

// The check of a series selects it by its name in the capture's
// namespace, escaped, and sums a counter by its breakdown label.
func TestOptionalQuery(t *testing.T) {
	assert.Equal(t, `sum by (source) (vllm:prompt_tokens_cached_by_source_total{namespace="a\"b"})`,
		OptionalQuery(optionalByName(t, "vllm:prompt_tokens_cached_by_source_total"), `a"b`))
	assert.Equal(t, `sum(vllm:external_prefix_cache_hits_total{namespace="llm"})`,
		OptionalQuery(optionalByName(t, "vllm:external_prefix_cache_hits_total"), "llm"))
	assert.Equal(t, `count(vllm:kv_offload_config_info{namespace="llm"})`,
		OptionalQuery(optionalByName(t, "vllm:kv_offload_config_info"), "llm"))
	assert.Equal(t, `count(llm_d_epp_request_cached_tokens_bucket{namespace="llm"})`,
		OptionalQuery(optionalByName(t, "llm_d_epp_request_cached_tokens_bucket"), "llm"))
	// The engine's HTTP time is read for every handler, broken down by it.
	assert.Equal(t, `sum by (handler) (http_request_duration_seconds_sum{namespace="llm"})`,
		OptionalQuery(optionalByName(t, "http_request_duration_seconds_sum"), "llm"))
	// A DCGM series is selected by the GPU's Pod namespace, not the
	// exporter's.
	assert.Equal(t, `count(DCGM_FI_PROF_SM_ACTIVE{exported_namespace="llm"})`,
		OptionalQuery(optionalByName(t, "DCGM_FI_PROF_SM_ACTIVE"), "llm"))
	// A prefix checks every series of its family, as one.
	assert.Equal(t, `count({__name__=~"vllm:hisparse_.+",namespace="llm"})`,
		OptionalQuery(optionalByName(t, "vllm:hisparse_"), "llm"))
	assert.Equal(t, `sum by (extension_point) (llm_d_epp_plugin_duration_seconds_sum{namespace="llm"})`,
		OptionalQuery(optionalByName(t, "llm_d_epp_plugin_duration_seconds_sum"), "llm"))
}
