package whatifsource

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/prometheus/common/model"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// Tier says what an optional series is for, in the words the operator
// pages use.
type Tier string

// The tiers of OptionalSeries. A series the capture replays is "required
// today" (docs/guides/capacity-planner/metrics.md); the optional series are
// one of the other two.
const (
	// TierCachedFleet is a series the planner needs to price TTFT on a
	// fleet whose prompts hit a cache tier, an offload tier or a peer.
	TierCachedFleet Tier = "needed for TTFT on cached fleets"
	// TierDiagnostic is a series that explains an answer, and is not
	// priced.
	TierDiagnostic Tier = "diagnostic"
	// TierOutsideEngines is a diagnostic series that explains the TTFT
	// spent outside the engines: in the router, its plugins, the P/D
	// sidecar, or the engine's HTTP front end before the scheduler.
	TierOutsideEngines Tier = "diagnostic: TTFT outside the engines"
	// TierPrefillCapacity is a series that measures prefill capacity
	// without a queue: the tokens each engine step takes, and the spread
	// of per-request prefill time.
	TierPrefillCapacity Tier = "needed for prefill capacity"
)

// Where an optional series comes from (OptionalSeries.Source).
const (
	SourceVLLM    = "vllm"
	SourceSGLang  = "sglang"
	SourceLMCache = "lmcache"
	SourceRouter  = "router"
	SourceSidecar = "sidecar"
	SourceKSM     = "kube-state-metrics"
	// SourceDCGM is the NVIDIA DCGM exporter. Its series carry the
	// exporter's own namespace in namespace, and the GPU's Pod in
	// exported_namespace and exported_pod: they are selected by
	// exported_namespace.
	SourceDCGM = "dcgm"
)

// OptionalSeries is one series a capture checks for but does not replay:
// the cache-tier, offloading and KV-transfer series an operator collects
// for TTFT on cached fleets, and the router, sidecar and engine front-end
// series that explain the TTFT spent outside the engines. The capture records whether the window held
// it, and a counter's increase, in the window line
// (learnreplay.CaptureWindow.Optional); an absent one is recorded as absent
// and never fails the capture.
type OptionalSeries struct {
	// Name is the series as Prometheus stores it: a counter's _total name,
	// a histogram's _sum, _count or _bucket.
	Name string
	// Source is where the series comes from: SourceVLLM, SourceRouter
	// and the other Source constants.
	// A capture with --engine vllm checks no SGLang series, and the other
	// way round.
	Source string
	Tier   Tier
	// By is the label a counter's increase is broken down by; empty for
	// none.
	By string
	// Match is more label matchers, comma-separated, that select the
	// series; empty for none.
	Match string
	// Prefix, when set, makes Name a name prefix: every series whose name
	// starts with it is checked, as one, for a family whose names vary
	// across releases.
	Prefix bool
}

// counter reports whether the series accumulates: a counter, or a
// histogram's _sum or _count, whose increase over the window is recorded.
func (s OptionalSeries) counter() bool {
	return !s.Prefix && (strings.HasSuffix(s.Name, "_total") || strings.HasSuffix(s.Name, "_sum") ||
		strings.HasSuffix(s.Name, "_count"))
}

// namespaceLabel is the label that holds the namespace of the Pod a series
// describes.
func (s OptionalSeries) namespaceLabel() string {
	if s.Source == SourceDCGM {
		return "exported_namespace"
	}
	return "namespace"
}

// OptionalSeriesList is every optional series, in the order the operator
// page lists them (docs/guides/capacity-planner/metrics.md, "Cache tiers,
// offloading and KV transfer", "TTFT outside the engines" and "Prefill
// capacity, load and health"). A test keeps the page, the record guide's
// CACHE_METRICS list and this list in step. The versions and flags that
// produce each are on that page.
var OptionalSeriesList = []OptionalSeries{
	// vLLM: tokens served by a KV connector, and the tier that served them.
	{Name: "vllm:external_prefix_cache_queries_total", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:external_prefix_cache_hits_total", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:prompt_tokens_by_source_total", Source: SourceVLLM, Tier: TierCachedFleet, By: "source"},
	{Name: "vllm:prompt_tokens_cached_total", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:prompt_tokens_cached_by_source_total", Source: SourceVLLM, Tier: TierCachedFleet, By: "source"},
	// vLLM: KV offloading (the OffloadingConnector, --kv-offloading-size).
	{Name: "vllm:kv_offload_load_bytes_total", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:kv_offload_load_time_total", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:kv_offload_store_bytes_total", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_store_time_total", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_total_bytes_total", Source: SourceVLLM, Tier: TierCachedFleet, By: "transfer_type"},
	{Name: "vllm:kv_offload_total_time_total", Source: SourceVLLM, Tier: TierCachedFleet, By: "transfer_type"},
	{Name: "vllm:kv_offload_tiering_chunk_queries_total", Source: SourceVLLM, Tier: TierCachedFleet, By: "tier"},
	{Name: "vllm:kv_offload_tiering_chunk_hits_total", Source: SourceVLLM, Tier: TierCachedFleet, By: "tier"},
	{Name: "vllm:kv_offload_tiering_read_bytes_total", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:kv_offload_tiering_read_time_total", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:kv_offload_config_info", Source: SourceVLLM, Tier: TierDiagnostic},
	// vLLM: the load time from a secondary tier (a peer) that the read
	// time above leaves out, and where the wait for cached KV goes.
	{Name: "vllm:kv_offload_tiering_promotion_latency_seconds_sum", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:kv_offload_tiering_promotion_latency_seconds_count", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:kv_offload_tiering_promotion_latency_seconds_bucket", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_lookup_sync_delay_seconds_bucket", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_lookup_sync_delay_seconds_sum", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_lookup_sync_delay_seconds_count", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_lookup_async_delay_seconds_bucket", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_lookup_async_delay_seconds_sum", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_lookup_async_delay_seconds_count", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_tiering_lookup_sync_delay_seconds_bucket", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_tiering_lookup_sync_delay_seconds_sum", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_tiering_lookup_sync_delay_seconds_count", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_tiering_lookup_async_delay_seconds_bucket", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_tiering_lookup_async_delay_seconds_sum", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_tiering_lookup_async_delay_seconds_count", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_load_size_bucket", Source: SourceVLLM, Tier: TierDiagnostic},
	// vLLM: how long a cached prefix lives in the GPU cache
	// (--kv-cache-metrics). The reuse-gap and idle-before-evict buckets are
	// also recorded: the capacity planner prices the prefix hit at another
	// prefill count from them (prefill.FitHitModel).
	{Name: "vllm:kv_block_lifetime_seconds_bucket", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_block_lifetime_seconds_sum", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_block_lifetime_seconds_count", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_block_idle_before_evict_seconds_bucket", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:kv_block_idle_before_evict_seconds_sum", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_block_idle_before_evict_seconds_count", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_block_reuse_gap_seconds_bucket", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:kv_block_reuse_gap_seconds_sum", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_block_reuse_gap_seconds_count", Source: SourceVLLM, Tier: TierDiagnostic},
	// vLLM: the decode host cache, every series of the family, whose names
	// vary across releases.
	{Name: "vllm:hisparse_", Source: SourceVLLM, Tier: TierDiagnostic, Prefix: true},
	// vLLM: P/D transfer over NIXL, beyond the transfer time a capture
	// replays.
	{Name: "vllm:nixl_bytes_transferred_sum", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:nixl_bytes_transferred_count", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:nixl_xfer_time_seconds_bucket", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:nixl_post_time_seconds_sum", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:nixl_post_time_seconds_count", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:nixl_num_failed_transfers_total", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:nixl_num_kv_expired_reqs_total", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:nixl_num_failed_notifications_total", Source: SourceVLLM, Tier: TierDiagnostic},
	// vLLM: speculative decoding (MTP, EAGLE), the acceptance at each draft
	// position and the drafts proposed. With them a TPOT spread priced from
	// the tokens a step emits reads the acceptance law, not one assumed from
	// the mean accepted tokens a step.
	{Name: "vllm:spec_decode_num_accepted_tokens_per_pos_total", Source: SourceVLLM, Tier: TierDiagnostic, By: "position"},
	{Name: "vllm:spec_decode_num_drafts_total", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:spec_decode_num_draft_tokens_total", Source: SourceVLLM, Tier: TierDiagnostic},
	// LMCache, multi-process server (its own port).
	{Name: "lmcache_mp_lookup_requested_tokens_total", Source: SourceLMCache, Tier: TierCachedFleet},
	{Name: "lmcache_mp_lookup_hit_tokens_total", Source: SourceLMCache, Tier: TierCachedFleet},
	{Name: "lmcache_mp_lookup_hit_l1_tokens_total", Source: SourceLMCache, Tier: TierCachedFleet},
	{Name: "lmcache_mp_lookup_hit_l2_tokens_total", Source: SourceLMCache, Tier: TierCachedFleet},
	{Name: "lmcache_mp_lookup_prompt_tokens_total", Source: SourceLMCache, Tier: TierDiagnostic},
	{Name: "lmcache_mp_lookup_request_hit_tokens_total", Source: SourceLMCache, Tier: TierDiagnostic},
	// LMCache, in-process connector (on the engine's own /metrics).
	{Name: "lmcache:num_requested_tokens_total", Source: SourceLMCache, Tier: TierCachedFleet},
	{Name: "lmcache:num_hit_tokens_total", Source: SourceLMCache, Tier: TierCachedFleet},
	{Name: "lmcache:num_lookup_tokens_total", Source: SourceLMCache, Tier: TierDiagnostic},
	{Name: "lmcache:num_lookup_hits_total", Source: SourceLMCache, Tier: TierDiagnostic},
	{Name: "lmcache:time_to_retrieve_sum", Source: SourceLMCache, Tier: TierDiagnostic},
	{Name: "lmcache:time_to_retrieve_count", Source: SourceLMCache, Tier: TierDiagnostic},
	// llm-d router: cached tokens as the engine reported them, and the
	// prefix producer's prediction.
	{Name: "llm_d_epp_request_cached_tokens_sum", Source: SourceRouter, Tier: TierCachedFleet},
	{Name: "llm_d_epp_request_cached_tokens_count", Source: SourceRouter, Tier: TierCachedFleet},
	{Name: "llm_d_epp_request_cached_tokens_bucket", Source: SourceRouter, Tier: TierCachedFleet},
	{Name: "llm_d_epp_prefix_predicted_cached_tokens_sum", Source: SourceRouter, Tier: TierCachedFleet},
	{Name: "llm_d_epp_prefix_predicted_cached_tokens_count", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_best_predicted_cached_tokens_sum", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_best_available_cached_tokens_sum", Source: SourceRouter, Tier: TierCachedFleet},
	{Name: "llm_d_epp_prefix_prompt_tokens_sum", Source: SourceRouter, Tier: TierDiagnostic},
	// llm-d router: how much cache affinity the routing gives up under
	// load, and the prefix indexer.
	{Name: "llm_d_epp_prefix_best_predicted_cached_tokens_count", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_best_available_cached_tokens_count", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_prompt_tokens_count", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_predicted_cached_tokens_bucket", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_best_available_cached_tokens_bucket", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_cache_affinity_filter_decisions_total", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_indexer_size", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_indexer_hit_ratio_bucket", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_indexer_hit_ratio_sum", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_indexer_hit_ratio_count", Source: SourceRouter, Tier: TierDiagnostic},
	// SGLang: the hit tier of each prefill token.
	{Name: "sglang:prefill_effective_tokens_total", Source: SourceSGLang, Tier: TierCachedFleet, By: "mode"},
	{Name: "sglang:uncached_prompt_tokens_histogram_sum", Source: SourceSGLang, Tier: TierCachedFleet},
	{Name: "sglang:uncached_prompt_tokens_histogram_count", Source: SourceSGLang, Tier: TierCachedFleet},
	{Name: "sglang:uncached_prompt_tokens_histogram_bucket", Source: SourceSGLang, Tier: TierDiagnostic},
	// TTFT outside the engines: the router's own time, its plugins and
	// render calls, the P/D sidecar's prefill leg, and the engine's time
	// before and around its scheduler.
	{Name: "llm_d_epp_request_processing_duration_seconds_sum", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_request_processing_duration_seconds_count", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_request_processing_duration_seconds_bucket", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_token_producer_render_duration_seconds_sum", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_token_producer_render_duration_seconds_count", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_plugin_duration_seconds_sum", Source: SourceRouter, Tier: TierOutsideEngines, By: "extension_point"},
	{Name: "llm_d_epp_plugin_duration_seconds_count", Source: SourceRouter, Tier: TierOutsideEngines, By: "extension_point"},
	{Name: "llm_d_epp_request_size_bytes_sum", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_request_size_bytes_count", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_request_duration_seconds_sum", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_request_duration_seconds_count", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_disagg_sidecar_prefill_duration_seconds_sum", Source: SourceSidecar, Tier: TierOutsideEngines},
	{Name: "llm_d_disagg_sidecar_prefill_duration_seconds_count", Source: SourceSidecar, Tier: TierOutsideEngines},
	{Name: "vllm:e2e_request_latency_seconds_sum", Source: SourceVLLM, Tier: TierOutsideEngines},
	{Name: "vllm:e2e_request_latency_seconds_count", Source: SourceVLLM, Tier: TierOutsideEngines},
	{Name: "vllm:request_render_duration_seconds_sum", Source: SourceVLLM, Tier: TierOutsideEngines},
	{Name: "vllm:request_render_duration_seconds_count", Source: SourceVLLM, Tier: TierOutsideEngines},
	{Name: "http_request_duration_seconds_sum", Source: SourceVLLM, Tier: TierOutsideEngines, By: "handler"},
	{Name: "http_request_duration_seconds_count", Source: SourceVLLM, Tier: TierOutsideEngines, By: "handler"},
	{Name: "llm_d_epp_scheduler_e2e_duration_seconds_bucket", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_scheduler_e2e_duration_seconds_sum", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_scheduler_e2e_duration_seconds_count", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_response_processing_duration_seconds_sum", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_response_processing_duration_seconds_count", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_flow_control_request_queue_duration_seconds_sum", Source: SourceRouter, Tier: TierOutsideEngines, By: "outcome"},
	{Name: "llm_d_epp_flow_control_request_queue_duration_seconds_count", Source: SourceRouter, Tier: TierOutsideEngines, By: "outcome"},
	// Prefill capacity without a queue: the tokens each engine step takes,
	// the spread of prefill time, and the compute each GPU does
	// (--enable-mfu-metrics) and the DCGM exporter sees. The tokens-per-iteration
	// buckets are also recorded: the planner reads from them whether the
	// step budget binds (capacity.ReadStepBudgetBinding).
	{Name: "vllm:iteration_tokens_total_bucket", Source: SourceVLLM, Tier: TierPrefillCapacity},
	{Name: "vllm:iteration_tokens_total_sum", Source: SourceVLLM, Tier: TierPrefillCapacity},
	{Name: "vllm:iteration_tokens_total_count", Source: SourceVLLM, Tier: TierPrefillCapacity},
	{Name: "vllm:request_prefill_time_seconds_bucket", Source: SourceVLLM, Tier: TierPrefillCapacity},
	{Name: "vllm:estimated_flops_per_gpu_total", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:estimated_read_bytes_per_gpu_total", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:estimated_write_bytes_per_gpu_total", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "DCGM_FI_DEV_GPU_UTIL", Source: SourceDCGM, Tier: TierDiagnostic},
	{Name: "DCGM_FI_PROF_SM_ACTIVE", Source: SourceDCGM, Tier: TierDiagnostic},
	{Name: "DCGM_FI_PROF_PIPE_TENSOR_ACTIVE", Source: SourceDCGM, Tier: TierDiagnostic},
	{Name: "DCGM_FI_PROF_DRAM_ACTIVE", Source: SourceDCGM, Tier: TierDiagnostic},
	{Name: "DCGM_FI_DEV_POWER_USAGE", Source: SourceDCGM, Tier: TierDiagnostic},
	{Name: "DCGM_FI_DEV_SM_CLOCK", Source: SourceDCGM, Tier: TierDiagnostic},
	// The same six as the DCGM exporter names them from 4.9.0, with units.
	{Name: "DCGM_FI_DEV_GPU_UTIL_RATIO", Source: SourceDCGM, Tier: TierDiagnostic},
	{Name: "DCGM_FI_PROF_SM_UTIL_RATIO", Source: SourceDCGM, Tier: TierDiagnostic},
	{Name: "DCGM_FI_PROF_TENSOR_UTIL_RATIO", Source: SourceDCGM, Tier: TierDiagnostic},
	{Name: "DCGM_FI_PROF_DRAM_UTIL_RATIO", Source: SourceDCGM, Tier: TierDiagnostic},
	{Name: "DCGM_FI_DEV_BOARD_POWER_WATTS", Source: SourceDCGM, Tier: TierDiagnostic},
	{Name: "DCGM_FI_DEV_SM_CLOCK_HERTZ", Source: SourceDCGM, Tier: TierDiagnostic},
	// vLLM: decode time and per-token latency per request, and how
	// requests ended.
	{Name: "vllm:request_time_per_output_token_seconds_sum", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:request_time_per_output_token_seconds_count", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:request_decode_time_seconds_bucket", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:request_decode_time_seconds_sum", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:request_decode_time_seconds_count", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:request_success_total", Source: SourceVLLM, Tier: TierDiagnostic, By: "finished_reason"},
	// llm-d router: the build that ran, the endpoints and load it saw, its
	// errors, request sizes, and admission.
	{Name: "llm_d_epp_info", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_ready_endpoints", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_inflight_requests", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_inflight_tokens", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_request_total", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_request_error_total", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_request_input_tokens_bucket", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_request_input_tokens_sum", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_request_input_tokens_count", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_request_output_tokens_bucket", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_request_output_tokens_sum", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_request_output_tokens_count", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_flow_control_dispatch_cycle_duration_seconds_bucket", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_flow_control_dispatch_cycle_duration_seconds_sum", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_flow_control_dispatch_cycle_duration_seconds_count", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_flow_control_detector_saturation", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_flow_control_capacity_utilization_requests", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_flow_control_capacity_utilization_bytes", Source: SourceRouter, Tier: TierDiagnostic},
	// kube-state-metrics: restarts, and why a container last ended
	// (OOMKilled among them).
	{Name: "kube_pod_container_status_restarts_total", Source: SourceKSM, Tier: TierDiagnostic, By: "container"},
	{Name: "kube_pod_container_status_last_terminated_reason", Source: SourceKSM, Tier: TierDiagnostic},
}

// optionalFor is the optional series a capture of engine checks: every
// one but the other engine's.
func optionalFor(engine inferenceengine.Engine) []OptionalSeries {
	skip := SourceSGLang
	if engine == inferenceengine.EngineSGLang {
		skip = SourceVLLM
	}
	var out []OptionalSeries
	for _, s := range OptionalSeriesList {
		if s.Source != skip {
			out = append(out, s)
		}
	}
	return out
}

// OptionalQuery is the PromQL that checks s in namespace: a counter's sum
// (by its breakdown label), whose steps give its increase; any other
// series' count, and a prefix's count of every series it starts. All are
// instant vectors, answered at every step of the window like the capture's
// own queries. A DCGM series is selected by the namespace of the GPU's Pod,
// not the exporter's.
func OptionalQuery(s OptionalSeries, namespace string) string {
	match := s.namespaceLabel() + `="` + source.EscapePromQLValue(namespace) + `"`
	if s.Match != "" {
		match += "," + s.Match
	}
	sel := s.Name + "{" + match + "}"
	if s.Prefix {
		sel = `{__name__=~"` + source.EscapePromQLValue(regexp.QuoteMeta(s.Name)) + `.+",` + match + "}"
	}
	if !s.counter() {
		return "count(" + sel + ")"
	}
	if s.By != "" {
		return "sum by (" + s.By + ") (" + sel + ")"
	}
	return "sum(" + sel + ")"
}

// checkOptional checks every optional series of the capture's engine over
// the window, on an API of its own: one range query each, at the
// capture's step, in the parts the capture's own queries take. A query
// that fails is recorded as unchecked and the rest go on; only a
// cancelled ctx stops them. It returns the series as found, and the range
// queries and samples it took.
func checkOptional(ctx context.Context, opts Options, maxSamples int) ([]learnreplay.CaptureSeries, int, int, error) {
	api := newWindowAPI(opts.Prometheus, opts.From, opts.To, opts.Step, opts.RangeQueryPoints, nil)
	var out []learnreplay.CaptureSeries
	queries, held := 0, 0
	for _, s := range optionalFor(opts.Engine) {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		// Each answer is read and dropped: the bound is per query, not on
		// the sum, so one large series does not stop the checks after it.
		api.mu.Lock()
		held += api.samples
		api.samples, api.maxSamples = 0, maxSamples
		api.mu.Unlock()
		queries++
		ns := opts.Namespace
		if strings.HasPrefix(s.Name, "lmcache_mp_") && opts.LMCacheNamespace != "" {
			ns = opts.LMCacheNamespace
		}
		byTime, _, err := api.fetch(ctx, OptionalQuery(s, ns))
		if err != nil {
			if ctx.Err() != nil {
				return nil, 0, 0, ctx.Err()
			}
			out = append(out, learnreplay.CaptureSeries{Name: s.Name, Err: err.Error()})
			continue
		}
		out = append(out, summarize(s, byTime))
	}
	return out, queries, held + api.held(), nil
}

// summarize is what a series' answer over the window says: whether it had
// a sample, and a counter's increase, read from its sums step by step as
// rate() reads a counter: a fall is a restart, and the value after it
// counts in full.
func summarize(s OptionalSeries, byTime map[model.Time]model.Vector) learnreplay.CaptureSeries {
	c := learnreplay.CaptureSeries{Name: s.Name, Present: len(byTime) > 0}
	if !c.Present || !s.counter() {
		return c
	}
	last := map[string]float64{}
	by := map[string]float64{}
	for _, at := range slices.Sorted(maps.Keys(byTime)) {
		for _, sample := range byTime[at] {
			key := string(sample.Metric[model.LabelName(s.By)])
			v := float64(sample.Value)
			prev, seen := last[key]
			switch {
			case !seen:
			case v >= prev:
				by[key] += v - prev
			default:
				by[key] += v
			}
			last[key] = v
		}
	}
	for _, v := range by {
		c.Increase += v
	}
	if s.By != "" && len(by) > 0 {
		c.By = by
	}
	return c
}

// OptionalText is one line per optional series checked: present, with a
// counter's increase, absent, or unchecked and why. present and absent
// count them.
func OptionalText(found []learnreplay.CaptureSeries) (lines []string, present, absent int) {
	for _, c := range found {
		switch {
		case c.Err != "":
			lines = append(lines, fmt.Sprintf("unchecked %s: %s", c.Name, c.Err))
		case !c.Present:
			absent++
			lines = append(lines, "absent    "+c.Name)
		default:
			present++
			var line strings.Builder
			line.WriteString("present   " + c.Name)
			if c.Increase != 0 || c.By != nil {
				fmt.Fprintf(&line, "  increase %.4g", c.Increase)
			}
			for _, k := range slices.Sorted(maps.Keys(c.By)) {
				name := k
				if name == "" {
					name = `""`
				}
				fmt.Fprintf(&line, "  %s=%.4g", name, c.By[k])
			}
			lines = append(lines, line.String())
		}
	}
	return lines, present, absent
}
