package registration

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
)

// The per-pod series that say where a prompt's cached tokens came from:
// vllm:prefix_cache_hits counts the GPU prefix cache alone, so a hit from an
// offload tier or a KV connector reads there as computed. They are recorded
// (domain.ReplicaMetrics, "Recorded for the cache tiers") and read by
// nothing yet: a recording made with them says which tier a fleet's hits
// come from. Issued only while recording. vLLM only, registered once under
// a bare name; an engine that does not publish them returns no series, and
// the row reads them as unknown.
const (
	// QueryExternalPrefixCacheHitTokenRate and
	// QueryExternalPrefixCacheQueryTokenRate are the prompt tokens a second
	// a pod's KV connector was asked for and served, over [1m] like
	// QueryPrefillComputedTokenRate (vllm:external_prefix_cache_hits and
	// _queries, v0.11.1 and later; only with a --kv-transfer-config). On a
	// P/D decode pod a pull from prefill counts here.
	QueryExternalPrefixCacheHitTokenRate   = "external_prefix_cache_hit_token_rate"
	QueryExternalPrefixCacheQueryTokenRate = "external_prefix_cache_query_token_rate"

	// QueryCachedTokenRateBySource is the cached prompt tokens a second a
	// pod read from each tier, over [1m], one series per source label
	// (vllm:prompt_tokens_cached_by_source, v0.32.0 and later): device (the
	// GPU prefix cache), host, disk, p2p, external_unspecified. The sources
	// sum to the GPU and the connector hits together.
	QueryCachedTokenRateBySource = "cached_token_rate_by_source"

	// QueryKVReuseGapBuckets and QueryKVIdleBeforeEvictBuckets are a pod's
	// KV residency histograms, over [1m], one series per bucket
	// (vllm:kv_block_reuse_gap_seconds: one observation per prefix-cache
	// hit block, the time since its last use;
	// vllm:kv_block_idle_before_evict_seconds: one per evicted block, the
	// time it sat unused; with --kv-cache-metrics). The capacity planner
	// reads them for the prefix hit at another prefill count
	// (prefill.FitHitModel).
	QueryKVReuseGapBuckets        = "kv_reuse_gap_buckets"
	QueryKVIdleBeforeEvictBuckets = "kv_idle_before_evict_buckets"
)

// perPodRate is the per-pod rate of a vLLM counter over window: each rank
// read once however many times its target is scraped, as
// QueryPrefillComputedTokenRate is, the ranks of a pod summed.
func perPodRate(counter, window string) string {
	return `sum by (model_name, instance, pod) (max by (model_name, instance, pod, engine) (rate(` +
		counter + `{namespace="{{.namespace}}"}[` + window + `])))`
}

// registerCacheTierQueries registers the queries above.
func registerCacheTierQueries(registry *source.QueryList) {
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryExternalPrefixCacheHitTokenRate,
		Type:        source.QueryTypePromQL,
		Template:    perPodRate("vllm:external_prefix_cache_hits_total", TTFTInputWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Prompt tokens a KV connector served per pod (tokens/s, 1m) (vLLM only)",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryExternalPrefixCacheQueryTokenRate,
		Type:        source.QueryTypePromQL,
		Template:    perPodRate("vllm:external_prefix_cache_queries_total", TTFTInputWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Prompt tokens a KV connector was asked for per pod (tokens/s, 1m) (vLLM only)",
	})
	registry.MustRegister(source.QueryTemplate{
		Name: QueryCachedTokenRateBySource,
		Type: source.QueryTypePromQL,
		Template: `sum by (model_name, instance, pod, source) (max by (model_name, instance, pod, engine, source) (rate(` +
			`vllm:prompt_tokens_cached_by_source_total{namespace="{{.namespace}}"}[` + TTFTInputWindow + `])))`,
		Params:      []string{source.ParamNamespace},
		Description: "Cached prompt tokens per pod by the tier that supplied them (tokens/s, 1m, one series per source) (vLLM only)",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryKVReuseGapBuckets,
		Type:        source.QueryTypePromQL,
		Template:    perPodBucketsOnce("vllm:kv_block_reuse_gap_seconds_bucket"),
		Params:      []string{source.ParamNamespace},
		Description: "KV block reuse-gap histogram per pod (1m rate, one series per bucket; --kv-cache-metrics) (vLLM only)",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryKVIdleBeforeEvictBuckets,
		Type:        source.QueryTypePromQL,
		Template:    perPodBucketsOnce("vllm:kv_block_idle_before_evict_seconds_bucket"),
		Params:      []string{source.ParamNamespace},
		Description: "KV block idle-before-evict histogram per pod (1m rate, one series per bucket; --kv-cache-metrics) (vLLM only)",
	})
}
