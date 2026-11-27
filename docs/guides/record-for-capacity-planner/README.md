# Capacity planner: record a window

This page is part of the [capacity planner guide](../capacity-planner/).

## Overview

The capacity planner learns its answers from a past window of your
Prometheus: what the model servers, the router and kube-state-metrics reported
while the fleet ran.

This guide saves such a window. There are two ways:

1. **With `capacity-planner`.** One command reads the window from Prometheus and writes
   a recording. Use it when you can run the tool (the release binary) and
   reach Prometheus.
2. **With only curl and jq.** Save the raw series to files and send them.
   Anyone with this repository then loads them into a local Prometheus and
   runs step 1 there. Use it when you cannot run `capacity-planner` where Prometheus is.

[Which series, and what each one feeds](../capacity-planner/metrics.md)
is the reference for both.

## What to send us

For an operator collecting a window for someone else to plan from.

1. **Record with Path 2** ([curl and jq](#path-2-record-with-curl-and-jq)).
   It saves the raw series, the cache-tier ones included. A Path 1 capture
   keeps only whether each cache-tier series was present and its total.
2. **Take 24 hours**, the 24 hours before now if Prometheus still holds them.
   Set `FROM` 5 minutes before the start.
3. **Engine arguments.** The describe step writes them to `window.txt`.
   Check it holds the full arguments, `--kv-transfer-config` and any
   offloading flags (`--kv-offloading-size`) included.
4. **Router version:** the image of the router (EPP) Deployment, for example
   `kubectl -n <namespace> get deploy <router deployment> -o jsonpath='{..image}'`.
5. **LMCache, if used:** its config file or `LMCACHE_*` environment, and,
   for the multi-process server, its namespace (set `LMCACHE_NAMESPACE` before
   the fetch when it differs) and that Prometheus scrapes its
   `--prometheus-port`.
6. **Check before you send.** The check step prints `bad=0`, then one
   `present:` or `absent:` line per cache-tier series. `absent` is fine for a
   feature you do not run; `present` should cover the ones you do. An
   `EMPTY:` line names a kube-state-metrics series that came back empty, the
   kube-state-metrics setting that turns it on, and what is lost without it.
7. Send `planner-export.tgz`, and the router version and LMCache config
   beside it.

## Prerequisites

- [ ] Your Prometheus scrapes the model servers every 15 s (30 s at most),
  keeps their histograms, and still holds the window
  ([what it must keep](../capacity-planner/metrics.md#what-your-prometheus-must-keep)).
- [ ] A window of 30 minutes or more in which load rose. A flat, quiet hour
  teaches the planner little.
- [ ] Optional, to check TPOT p90 and p95: TPOT histogram edges about 15%
  apart across the range you serve. With the default router edges (5 to
  10 ms, 10 to 20 ms) the check cannot place a TPOT percentile and leaves it
  unscored. Set `metrics.requestTPOTBuckets` in the router's
  EndpointPickerConfig (an llm-d-router build after v1.0.0,
  [llm-d-router#3539](https://github.com/llm-d/llm-d-router/pull/3539)), or
  vLLM's `--custom-histogram-buckets` (v0.32.0 and later), before the window
  you record ([why](../capacity-planner/check.md#read-the-answer)). For
  vLLM the family is `inter_token_latency`: it sets the per-request TPOT
  histogram too. For example:
  `--custom-histogram-buckets '{"inter_token_latency":[0.005,0.00575,0.0066,0.0076,0.0087,0.01,0.0115,0.0132,0.0152,0.0175,0.02,0.023,0.0265,0.0305,0.035,0.04,0.046,0.053,0.075,0.1,0.25,1.0]}'`
  ([what it sets](../capacity-planner/check.md#vllms-tpot-buckets)).
- [ ] `kubectl` access to Prometheus' namespace (a port-forward), or its URL.
- [ ] For Path 1: the `capacity-planner` binary of the release you run
  ([Get the planner](../capacity-planner/README.md#get-the-planner)), or a
  checkout of that release and Go 1.26 or later (`make capacity-planner`).
  Record with the binary, not the image: the image is for serving.
- [ ] For Path 2: bash, `jq`, and `curl` 7.76 or later (for `--fail-with-body`;
  `curl --version` says which), plus `kubectl get` on the model workloads
  (Deployments, LeaderWorkerSets, or their Pods).
- [ ] kube-state-metrics in the same Prometheus, for readiness and roles,
  with `--metric-labels-allowlist=pods=[llm-d.ai/role],nodes=[nvidia.com/gpu.product]`
  so it exports each Pod's role and each node's accelerator, and, optionally,
  `--metric-annotations-allowlist=deployments=[scaling-manager.llmd.ai/vllm-version]`
  so it exports the [vLLM release](#the-vllm-release) annotation.
  For a LeaderWorkerSet or DisaggregatedSet fleet, add
  `leaderworkerset.sigs.k8s.io/name`, `leaderworkerset.sigs.k8s.io/group-index`,
  `leaderworkerset.sigs.k8s.io/worker-index`, `disaggregatedset.x-k8s.io/name`
  and `disaggregatedset.x-k8s.io/role` to the `pods=[...]` list: they say which
  Pods form one replica, and which role each serves.
  Without it, see [Without kube-state-metrics](#without-kube-state-metrics).

<!-- guide:prerequisites start -->
```bash
# Reach Prometheus from your machine. Use the Service your Prometheus has; plain http here.
# For a Prometheus behind a token, see "Prometheus with a token" below.
export PROM_NS=<prometheus namespace>
export PROM_SVC=<prometheus service>
kubectl -n ${PROM_NS} port-forward svc/${PROM_SVC} 9090:9090 &
export PROM_URL=http://localhost:9090
curl -s ${PROM_URL}/api/v1/status/buildinfo | jq -r .data.version
```
<!-- guide:prerequisites end -->

## Path 1: record with capacity-planner

<!-- guide:record start -->
```bash
# The binary of the release you run, at bin/capacity-planner, as the capacity planner guide's Get the planner
# downloads and checks it. Record with the binary: the image is for serving. From a checkout instead, run
# make capacity-planner (Go 1.26 or later; on v0.11.1 and earlier make whatif, which builds bin/whatif).
bin/capacity-planner version
bin/capacity-planner capture -h 2>&1 | head -3

# The namespace your model servers run in, and the window to answer from (UTC).
export NAMESPACE=<model namespace>
export FROM=2026-11-01T12:00:00Z
export TO=2026-11-01T13:00:00Z

# The engine arguments the workloads ran with. kube-state-metrics does not export them.
# A workload is deploy/NAME, lws/NAME (a LeaderWorkerSet), dset/NAME (a DisaggregatedSet: the role whose
# llm-d.ai/role label is decode, or prefill), or pod/NAME (a leader Pod). ENGINE_CONTAINER is the engine's container.
# For a P/D fleet, set PREFILL_WORKLOAD; leave it empty and prefill takes ENGINE_ARGS.
export ENGINE_CONTAINER=vllm
export DECODE_WORKLOAD=deploy/<decode deployment>
export PREFILL_WORKLOAD=
engine_args() {
  obj=$1
  case "${obj}" in dset/*) obj=disaggregatedsets.disaggregatedset.x-k8s.io/${obj#dset/} ;; esac
  out=$(kubectl -n "${NAMESPACE}" get "${obj}" -o json | jq -r --arg c "${ENGINE_CONTAINER}" --arg role "$2" '
    def role_of: (.metadata.labels // {})["llm-d.ai/role"];
    (if .kind == "DisaggregatedSet" then
       {spec: ([.spec.roles[] | select(role_of == $role
           or (.spec.leaderWorkerTemplate.leaderTemplate // {} | role_of) == $role
           or (.spec.leaderWorkerTemplate.workerTemplate // {} | role_of) == $role)][0].spec // {})}
     else . end)
    | [(.spec.template.spec // .spec.leaderWorkerTemplate.leaderTemplate.spec
      // .spec.leaderWorkerTemplate.workerTemplate.spec // .spec)
     | (.containers // [])[] | select(.name == $c) | (.command // []) + (.args // []) | .[]
     | gsub("\n"; " ")
     | if test("^[[{]") then (try (fromjson | tojson) catch .) else . end]
    | join(" ")')
  [ -n "${out}" ] || { echo "no container ${ENGINE_CONTAINER} with a command or args in $1" \
    "(or, for a DisaggregatedSet, no role labelled llm-d.ai/role=$2)" >&2; return 1; }
  echo "${out}"
}
ENGINE_ARGS=$(engine_args "${DECODE_WORKLOAD}" decode) && export ENGINE_ARGS && echo "decode: ${ENGINE_ARGS}"
unset PREFILL_ENGINE_ARGS
if [ -n "${PREFILL_WORKLOAD}" ]; then
  PREFILL_ENGINE_ARGS=$(engine_args "${PREFILL_WORKLOAD}" prefill) && export PREFILL_ENGINE_ARGS \
    && echo "prefill: ${PREFILL_ENGINE_ARGS}"
fi

# Replays the window through the controller's own queries. Exit 0 done, 2 your input (a flag, nothing found), 1 a failed query.
# It also checks the optional cache-tier series and prints which were present; an absent one never fails it.
bin/capacity-planner capture --prometheus ${PROM_URL} --namespace ${NAMESPACE} --kube-state-metrics \
  --from ${FROM} --to ${TO} --engine vllm --engine-args "${ENGINE_ARGS}" \
  ${PREFILL_ENGINE_ARGS+--prefill-engine-args "${PREFILL_ENGINE_ARGS}"} \
  -o run.capture.jsonl.gz

# Plays the capture through the analyzers. The result is an ordinary learner recording.
bin/capacity-planner generate -in run.capture.jsonl.gz -o run.jsonl.gz

# Ask the planner's two questions. Exit 0 answered, 3 a role refused (the output says why).
bin/capacity-planner replicas-to-slo --recording run.jsonl.gz --cycle last
bin/capacity-planner slo-to-rate --recording run.jsonl.gz --cycle last --tpot 20ms@p90
```
<!-- guide:record end -->

`capture` prints what it found, then `generate` what it played:

```
captured 57 steps of demo from 2026-11-12T19:48:04Z to 2026-11-12T20:02:04Z every 15s: 57 cycles, 138 rows, 1 variants, 59 range queries, 12272 samples
  Qwen/Qwen3-0.6B/demo-decode  decode  1 GPU NVIDIA-H100-80GB-HBM3
  flag: engine parameters from flags, which may differ from the window's workloads
  flag: rollouts not seen: every scale target filed as not rolling out
optional series (cache tiers, offloading, KV transfer, TTFT outside the engines): 2 present, 59 absent, 0 unchecked
  present   vllm:external_prefix_cache_queries_total  increase 1.832e+06
  present   vllm:external_prefix_cache_hits_total  increase 6.14e+05
  absent    vllm:prompt_tokens_by_source_total
  ...
wrote run.capture.jsonl.gz
generated 57 steps into run.jsonl.gz: 72688 bytes, analyzes map[saturation.Analyze:57]
  caveat: engine parameters from flags, which may differ from the window's workloads
  caveat: rollouts not seen: every scale target filed as not rolling out
```

A `flag:` or `caveat:` line names something the window could not show, such
as readiness without kube-state-metrics. Read them before the answers.

The `optional series` lines are the
[cache-tier, offloading and KV-transfer series](../capacity-planner/metrics.md#cache-tiers-offloading-and-kv-transfer):
which ones the window held, and a counter's increase over it. `absent` is
normal on an engine without that feature or release. `unchecked` means the
query failed (the line says why); the capture still completes.

Exit codes: `0` done; `2` your input (a bad flag, no replica with a KV-cache
series in the namespace and window, a window too large without `--force`);
`1` anything else, usually a failed Prometheus query, which the message names.
The answer commands add `3`: a role refused, with the reason printed.
`130` or `143`: stopped by Ctrl-C or SIGTERM, and the output may be incomplete.
[What to do about each](../capacity-planner/troubleshooting.md#recording-a-window).

`replicas-to-slo` and `slo-to-rate` also take the [capture flags](#capture-flags)
in place of `--recording`, and answer from the window without writing a file.
How to read their answers: [Read the command-line answers](#read-the-command-line-answers).

To look at the recording in the planner's page, put `run.jsonl.gz` (the
recording, not `run.capture.jsonl.gz`) in a directory and serve it:
[Start the planner](../capacity-planner/install-options.md#start-the-planner).

**Go easy on Prometheus.** A capture asks one range query per planner query
over the whole window: about 56 for one vLLM model. An hour is light; a day
returns millions of samples, and the usual failure is Prometheus'
`query.max-samples`. Capture off-peak, or from a copy
([Load on Prometheus](#load-on-prometheus)).

## Path 2: record with curl and jq

This saves the raw samples of every series the planner reads, nothing else,
for one namespace. It needs read access to Prometheus' query API, and to read
the model workloads; no checkout of this repository, no Go.

<!-- guide:manual start -->
```bash
# The namespace, and the window in Unix seconds. Start 5 minutes before the window you want answered.
# GNU date shown; on macOS use date -j -u -f %Y-%m-%dT%H:%M:%SZ <time> +%s.
export PROM_URL=http://localhost:9090
export NAMESPACE=<model namespace>
export FROM=$(date -u -d 2026-11-01T11:55:00Z +%s)
export TO=$(date -u -d 2026-11-01T13:00:00Z +%s)
export CHUNK=3600
export CURL_OPTS=""

# Saves the raw samples of every series the planner reads, one file per series and CHUNK seconds.
# Each is one instant query for a range, metric{namespace=...}[1h]: it returns stored samples, not a resampled grid.
# A failed query saves its error, so the check below and the converter refuse it. Needs bash, jq and curl 7.76 or later.
# Lower CHUNK if a query fails with "too many samples".
# CACHE_METRICS are the optional series: cache tiers, offloading, KV transfer, the TTFT spent outside the engines,
# prefill capacity, router load, restarts, and the GPUs as the DCGM exporter sees them.
# Older releases lack many of them, and an absent one saves an empty answer, not a failure. Set LMCACHE_NAMESPACE first if the LMCache server runs in another namespace.
# DCGM series are selected by the label that names the GPU's Pod, not by the exporter's namespace: DCGM_SELECTOR
# defaults to exported_namespace='<NAMESPACE>'; set it first to select otherwise, for example DCGM_SELECTOR="exported_pod=~'my-model-.*'".
# If your Prometheus honors the exporter's labels (honorLabels: true), set DCGM_SELECTOR="namespace='<NAMESPACE>'".
# A name ending in _ (vllm:hisparse_) saves every series whose name starts with it.
export METRICS="
vllm:kv_cache_usage_perc vllm:cache_config_info vllm:inter_token_latency_seconds_sum
vllm:inter_token_latency_seconds_count vllm:generation_tokens_total vllm:num_requests_running
vllm:num_requests_waiting vllm:num_requests_waiting_by_reason vllm:num_requests_kv_fetch_by_stage
vllm:request_prompt_tokens_sum
vllm:request_prompt_tokens_count vllm:request_prompt_tokens_bucket vllm:request_generation_tokens_sum
vllm:request_generation_tokens_count vllm:request_generation_tokens_bucket vllm:prefix_cache_hits_total
vllm:prefix_cache_queries_total vllm:external_prefix_cache_hits_total vllm:external_prefix_cache_queries_total
vllm:prompt_tokens_cached_by_source_total vllm:request_prefill_kv_computed_tokens_sum
vllm:request_prefill_kv_computed_tokens_bucket vllm:request_inference_time_seconds_sum
vllm:request_inference_time_seconds_count vllm:time_to_first_token_seconds_sum
vllm:time_to_first_token_seconds_count vllm:time_to_first_token_seconds_bucket
vllm:request_queue_time_seconds_sum vllm:request_queue_time_seconds_count
vllm:request_queue_time_seconds_bucket vllm:request_prefill_time_seconds_sum
vllm:request_prefill_time_seconds_count vllm:request_time_per_output_token_seconds_bucket
vllm:kv_block_reuse_gap_seconds_bucket vllm:kv_block_idle_before_evict_seconds_bucket
vllm:nixl_xfer_time_seconds_sum vllm:nixl_xfer_time_seconds_count vllm:nixl_bytes_transferred_sum
vllm:kv_offload_load_bytes_total vllm:kv_offload_load_time_total vllm:kv_offload_tiering_read_bytes_total
vllm:kv_offload_tiering_read_time_total vllm:num_preemptions_total
vllm:spec_decode_num_accepted_tokens_total vllm:e2e_request_latency_seconds_sum
vllm:e2e_request_latency_seconds_count http_request_duration_seconds_sum http_request_duration_seconds_count
sglang:token_usage sglang:max_total_num_tokens sglang:inter_token_latency_seconds_sum
sglang:inter_token_latency_seconds_count sglang:generation_tokens_total sglang:num_running_reqs
sglang:num_queue_reqs sglang:prompt_tokens_histogram_sum sglang:prompt_tokens_histogram_count
sglang:prompt_tokens_histogram_bucket sglang:generation_tokens_histogram_sum
sglang:generation_tokens_histogram_count sglang:generation_tokens_histogram_bucket
sglang:cached_tokens_total sglang:prompt_tokens_total sglang:e2e_request_latency_seconds_sum
sglang:e2e_request_latency_seconds_count sglang:queue_time_seconds_sum sglang:queue_time_seconds_count
sglang:queue_time_seconds_bucket sglang:time_to_first_token_seconds_sum
sglang:time_to_first_token_seconds_count sglang:time_to_first_token_seconds_bucket
llm_d_epp_request_streaming_tpot_seconds_bucket llm_d_epp_request_streaming_tpot_seconds_sum
llm_d_epp_request_streaming_tpot_seconds_count llm_d_epp_request_ttft_seconds_bucket
llm_d_epp_request_ttft_seconds_sum llm_d_epp_request_ttft_seconds_count
llm_d_epp_flow_control_queue_size inference_extension_flow_control_queue_size
llm_d_epp_flow_control_queue_bytes inference_extension_flow_control_queue_bytes
llm_d_epp_flow_control_request_enqueue_duration_seconds_count
inference_extension_flow_control_request_enqueue_duration_seconds_count
llm_d_epp_scheduler_attempts_total inference_extension_scheduler_attempts_total
llm_d_epp_flow_control_request_queue_duration_seconds_bucket llm_d_epp_flow_control_pool_saturation
llm_d_epp_flow_control_requests_total llm_d_epp_request_processing_duration_seconds_bucket
llm_d_epp_request_processing_duration_seconds_sum llm_d_epp_request_processing_duration_seconds_count
llm_d_epp_token_producer_render_duration_seconds_sum llm_d_epp_token_producer_render_duration_seconds_count
llm_d_epp_prefix_predicted_cached_tokens_sum llm_d_epp_prefix_best_available_cached_tokens_sum
llm_d_epp_prefix_best_predicted_cached_tokens_sum llm_d_epp_prefix_prompt_tokens_sum
llm_d_epp_prefix_predicted_cached_tokens_count llm_d_epp_per_endpoint_queue_size
llm_d_epp_inflight_requests llm_d_epp_inflight_tokens
kube_pod_info kube_pod_owner kube_replicaset_owner kube_pod_labels kube_pod_status_phase
kube_pod_status_ready kube_pod_status_ready_time kube_pod_created kube_pod_start_time
kube_pod_deletion_timestamp kube_pod_container_resource_requests kube_node_labels
kube_deployment_spec_replicas kube_pod_container_info kube_deployment_annotations
"
export CACHE_METRICS="
vllm:external_prefix_cache_queries_total vllm:external_prefix_cache_hits_total
vllm:prompt_tokens_by_source_total vllm:prompt_tokens_cached_total vllm:prompt_tokens_cached_by_source_total
vllm:kv_offload_load_bytes_total vllm:kv_offload_load_time_total vllm:kv_offload_store_bytes_total
vllm:kv_offload_store_time_total vllm:kv_offload_total_bytes_total vllm:kv_offload_total_time_total
vllm:kv_offload_tiering_chunk_queries_total vllm:kv_offload_tiering_chunk_hits_total
vllm:kv_offload_tiering_read_bytes_total vllm:kv_offload_tiering_read_time_total vllm:kv_offload_config_info
vllm:kv_offload_tiering_promotion_latency_seconds_sum vllm:kv_offload_tiering_promotion_latency_seconds_count
vllm:kv_offload_tiering_promotion_latency_seconds_bucket
vllm:kv_offload_lookup_sync_delay_seconds_bucket vllm:kv_offload_lookup_sync_delay_seconds_sum
vllm:kv_offload_lookup_sync_delay_seconds_count vllm:kv_offload_lookup_async_delay_seconds_bucket
vllm:kv_offload_lookup_async_delay_seconds_sum vllm:kv_offload_lookup_async_delay_seconds_count
vllm:kv_offload_tiering_lookup_sync_delay_seconds_bucket vllm:kv_offload_tiering_lookup_sync_delay_seconds_sum
vllm:kv_offload_tiering_lookup_sync_delay_seconds_count vllm:kv_offload_tiering_lookup_async_delay_seconds_bucket
vllm:kv_offload_tiering_lookup_async_delay_seconds_sum vllm:kv_offload_tiering_lookup_async_delay_seconds_count
vllm:kv_offload_load_size_bucket
vllm:kv_block_lifetime_seconds_bucket vllm:kv_block_lifetime_seconds_sum vllm:kv_block_lifetime_seconds_count
vllm:kv_block_idle_before_evict_seconds_bucket vllm:kv_block_idle_before_evict_seconds_sum
vllm:kv_block_idle_before_evict_seconds_count vllm:kv_block_reuse_gap_seconds_bucket
vllm:kv_block_reuse_gap_seconds_sum vllm:kv_block_reuse_gap_seconds_count
vllm:hisparse_
vllm:nixl_bytes_transferred_sum vllm:nixl_bytes_transferred_count vllm:nixl_xfer_time_seconds_bucket
vllm:nixl_post_time_seconds_sum vllm:nixl_post_time_seconds_count vllm:nixl_num_failed_transfers_total
vllm:nixl_num_kv_expired_reqs_total vllm:nixl_num_failed_notifications_total
vllm:spec_decode_num_accepted_tokens_per_pos_total vllm:spec_decode_num_drafts_total
vllm:spec_decode_num_draft_tokens_total
lmcache_mp_lookup_requested_tokens_total lmcache_mp_lookup_hit_tokens_total
lmcache_mp_lookup_hit_l1_tokens_total lmcache_mp_lookup_hit_l2_tokens_total
lmcache_mp_lookup_prompt_tokens_total lmcache_mp_lookup_request_hit_tokens_total
lmcache:num_requested_tokens_total lmcache:num_hit_tokens_total lmcache:num_lookup_tokens_total
lmcache:num_lookup_hits_total lmcache:time_to_retrieve_sum lmcache:time_to_retrieve_count
llm_d_epp_request_cached_tokens_sum llm_d_epp_request_cached_tokens_count
llm_d_epp_request_cached_tokens_bucket llm_d_epp_prefix_predicted_cached_tokens_sum
llm_d_epp_prefix_predicted_cached_tokens_count llm_d_epp_prefix_best_predicted_cached_tokens_sum
llm_d_epp_prefix_best_available_cached_tokens_sum llm_d_epp_prefix_prompt_tokens_sum
llm_d_epp_prefix_best_predicted_cached_tokens_count llm_d_epp_prefix_best_available_cached_tokens_count
llm_d_epp_prefix_prompt_tokens_count llm_d_epp_prefix_predicted_cached_tokens_bucket
llm_d_epp_prefix_best_available_cached_tokens_bucket llm_d_epp_prefix_cache_affinity_filter_decisions_total
llm_d_epp_prefix_indexer_size llm_d_epp_prefix_indexer_hit_ratio_bucket
llm_d_epp_prefix_indexer_hit_ratio_sum llm_d_epp_prefix_indexer_hit_ratio_count
sglang:prefill_effective_tokens_total sglang:uncached_prompt_tokens_histogram_sum
sglang:uncached_prompt_tokens_histogram_count sglang:uncached_prompt_tokens_histogram_bucket
llm_d_epp_request_processing_duration_seconds_sum llm_d_epp_request_processing_duration_seconds_count
llm_d_epp_request_processing_duration_seconds_bucket llm_d_epp_token_producer_render_duration_seconds_sum
llm_d_epp_token_producer_render_duration_seconds_count llm_d_epp_plugin_duration_seconds_sum
llm_d_epp_plugin_duration_seconds_count llm_d_epp_request_size_bytes_sum llm_d_epp_request_size_bytes_count
llm_d_epp_request_duration_seconds_sum llm_d_epp_request_duration_seconds_count
llm_d_disagg_sidecar_prefill_duration_seconds_sum llm_d_disagg_sidecar_prefill_duration_seconds_count
vllm:e2e_request_latency_seconds_sum vllm:e2e_request_latency_seconds_count
vllm:request_render_duration_seconds_sum vllm:request_render_duration_seconds_count
http_request_duration_seconds_sum http_request_duration_seconds_count
llm_d_epp_scheduler_e2e_duration_seconds_bucket llm_d_epp_scheduler_e2e_duration_seconds_sum
llm_d_epp_scheduler_e2e_duration_seconds_count llm_d_epp_response_processing_duration_seconds_sum
llm_d_epp_response_processing_duration_seconds_count llm_d_epp_flow_control_request_queue_duration_seconds_sum
llm_d_epp_flow_control_request_queue_duration_seconds_count
vllm:iteration_tokens_total_bucket vllm:iteration_tokens_total_sum vllm:iteration_tokens_total_count
vllm:request_prefill_time_seconds_bucket vllm:estimated_flops_per_gpu_total
vllm:estimated_read_bytes_per_gpu_total vllm:estimated_write_bytes_per_gpu_total
DCGM_FI_DEV_GPU_UTIL DCGM_FI_PROF_SM_ACTIVE DCGM_FI_PROF_PIPE_TENSOR_ACTIVE DCGM_FI_PROF_DRAM_ACTIVE
DCGM_FI_DEV_POWER_USAGE DCGM_FI_DEV_SM_CLOCK DCGM_FI_DEV_GPU_UTIL_RATIO DCGM_FI_PROF_SM_UTIL_RATIO
DCGM_FI_PROF_TENSOR_UTIL_RATIO DCGM_FI_PROF_DRAM_UTIL_RATIO DCGM_FI_DEV_BOARD_POWER_WATTS DCGM_FI_DEV_SM_CLOCK_HERTZ
vllm:request_time_per_output_token_seconds_sum vllm:request_time_per_output_token_seconds_count
vllm:request_decode_time_seconds_bucket vllm:request_decode_time_seconds_sum
vllm:request_decode_time_seconds_count vllm:request_success_total
llm_d_epp_info llm_d_epp_ready_endpoints llm_d_epp_inflight_requests llm_d_epp_inflight_tokens
llm_d_epp_request_total llm_d_epp_request_error_total
llm_d_epp_request_input_tokens_bucket llm_d_epp_request_input_tokens_sum llm_d_epp_request_input_tokens_count
llm_d_epp_request_output_tokens_bucket llm_d_epp_request_output_tokens_sum llm_d_epp_request_output_tokens_count
llm_d_epp_flow_control_dispatch_cycle_duration_seconds_bucket
llm_d_epp_flow_control_dispatch_cycle_duration_seconds_sum
llm_d_epp_flow_control_dispatch_cycle_duration_seconds_count llm_d_epp_flow_control_detector_saturation
llm_d_epp_flow_control_capacity_utilization_requests llm_d_epp_flow_control_capacity_utilization_bytes
kube_pod_container_status_restarts_total kube_pod_container_status_last_terminated_reason
"
export LMCACHE_NAMESPACE=${LMCACHE_NAMESPACE:-${NAMESPACE}}
[ -n "${DCGM_SELECTOR:-}" ] || export DCGM_SELECTOR="exported_namespace='${NAMESPACE}'"
rm -rf planner-export
mkdir -p planner-export
failed=0
for m in ${METRICS} ${CACHE_METRICS}; do
  sel="${m}{namespace='${NAMESPACE}'}"
  [ "${m}" = kube_node_labels ] && sel="${m}"
  case "${m}" in
    lmcache_mp_*) sel="${m}{namespace='${LMCACHE_NAMESPACE}'}" ;;
    DCGM_*) sel="${m}{${DCGM_SELECTOR}}" ;;
    *_) sel="{__name__=~'${m}.+',namespace='${NAMESPACE}'}" ;;
  esac
  t=${FROM}
  while [ "${t}" -lt "${TO}" ]; do
    end=$(( t + CHUNK )); [ "${end}" -gt "${TO}" ] && end=${TO}
    out="planner-export/${m//:/_}.${t}.json"
    curl -sS --fail-with-body ${CURL_OPTS} -G "${PROM_URL}/api/v1/query" -o "${out}" \
      --data-urlencode "query=${sel}[$(( end - t ))s]" --data-urlencode "time=${end}" \
      || { echo "FAILED: ${m} at ${t}"; failed=1; }
    t=${end}
  done
done
echo "failed=${failed}"

# What loading the export needs and Prometheus does not hold: the window, the engine, and the engine
# arguments of the decode and prefill workloads (leave PREFILL_WORKLOAD empty without one).
# A workload is deploy/NAME, lws/NAME (a LeaderWorkerSet), dset/NAME (a DisaggregatedSet: the role whose
# llm-d.ai/role label is decode, or prefill, on the role or its leader or worker template), or pod/NAME (a leader Pod).
# ENGINE is vllm or sglang; ENGINE_CONTAINER is the engine's container name.
# It stops, and writes no window.txt, when a workload or its container is not found.
export ENGINE=vllm
export ENGINE_CONTAINER=vllm
export DECODE_WORKLOAD=deploy/<decode deployment>
export PREFILL_WORKLOAD=
engine_args() {
  obj=$1
  case "${obj}" in dset/*) obj=disaggregatedsets.disaggregatedset.x-k8s.io/${obj#dset/} ;; esac
  out=$(kubectl -n "${NAMESPACE}" get "${obj}" -o json | jq -r --arg c "${ENGINE_CONTAINER}" --arg role "$2" '
    def role_of: (.metadata.labels // {})["llm-d.ai/role"];
    (if .kind == "DisaggregatedSet" then
       {spec: ([.spec.roles[] | select(role_of == $role
           or (.spec.leaderWorkerTemplate.leaderTemplate // {} | role_of) == $role
           or (.spec.leaderWorkerTemplate.workerTemplate // {} | role_of) == $role)][0].spec // {})}
     else . end)
    | [(.spec.template.spec // .spec.leaderWorkerTemplate.leaderTemplate.spec
      // .spec.leaderWorkerTemplate.workerTemplate.spec // .spec)
     | (.containers // [])[] | select(.name == $c) | (.command // []) + (.args // []) | .[]
     | gsub("\n"; " ")
     | if test("^[[{]") then (try (fromjson | tojson) catch .) else . end]
    | join(" ")')
  [ -n "${out}" ] || { echo "no container ${ENGINE_CONTAINER} with a command or args in $1" \
    "(or, for a DisaggregatedSet, no role labelled llm-d.ai/role=$2)" >&2; return 1; }
  echo "${out}"
}
rm -f planner-export/window.txt
(
  [ -n "${DECODE_WORKLOAD}" ] || { echo "set DECODE_WORKLOAD" >&2; exit 1; }
  decode_args=$(engine_args "${DECODE_WORKLOAD}" decode) || exit 1
  prefill_args=
  if [ -n "${PREFILL_WORKLOAD}" ]; then prefill_args=$(engine_args "${PREFILL_WORKLOAD}" prefill) || exit 1; fi
  {
    echo "NAMESPACE=${NAMESPACE}"
    echo "FROM=${FROM}"
    echo "TO=${TO}"
    echo "FROM_ISO=$(jq -rn --argjson t "${FROM}" '$t | todate')"
    echo "TO_ISO=$(jq -rn --argjson t "${TO}" '$t | todate')"
    echo "CAPTURE_FROM=$(( FROM + 300 ))"
    echo "ENGINE=${ENGINE}"
    echo "DECODE_ARGS=${decode_args}"
    if [ -n "${PREFILL_WORKLOAD}" ]; then echo "PREFILL_ARGS=${prefill_args}"; fi
  } > planner-export/window.txt
) && cat planner-export/window.txt || echo "window.txt NOT written: fix the above, then run this step again"
if grep -E '^[A-Z]+_ARGS=.*([$]|sh -c)' planner-export/window.txt 2>/dev/null; then
  echo "EDIT planner-export/window.txt: the line above is not expanded (see the guide)"
fi

# Every chunk saved and a success, then how many series each metric had. It prints bad=0 when nothing is missing.
# Zero series is fine for a metric your fleet does not export. Then, which cache-tier series it holds.
# Last, an EMPTY: line for each kube-state-metrics series the planner reads that came back empty: why, and what is lost.
want=$(( (TO - FROM + CHUNK - 1) / CHUNK ))
bad=0
for m in ${METRICS} ${CACHE_METRICS}; do
  have=$(ls planner-export/"${m//:/_}".*.json 2>/dev/null | wc -l)
  [ "${have}" -eq "${want}" ] || { echo "MISSING: ${m} has ${have} of ${want} chunks"; bad=1; }
done
for f in planner-export/*.json; do
  jq -e '.status == "success"' "${f}" >/dev/null 2>&1 || { echo "FAILED: ${f}"; bad=1; }
done
echo "bad=${bad}"
for f in planner-export/*.json; do
  echo "$(jq '.data.result | length' "${f}" 2>/dev/null || echo 0) $(basename "${f}" | cut -d. -f1)"
done | awk '{n[$2] += $1} END {for (m in n) print n[m], m}' | sort -n
for m in ${CACHE_METRICS}; do
  n=$(cat planner-export/"${m//:/_}".*.json 2>/dev/null | jq -s '[.[].data.result | length] | add // 0')
  if [ "${n:-0}" -gt 0 ]; then echo "present: ${m}"; else echo "absent: ${m}"; fi
done
for m in kube_node_labels kube_pod_created kube_pod_deletion_timestamp kube_deployment_annotations; do
  n=$(cat planner-export/"${m}".*.json 2>/dev/null | jq -s '[.[].data.result | length] | add // 0')
  [ "${n:-0}" -gt 0 ] && continue
  case "${m}" in
    kube_node_labels) echo "EMPTY: kube_node_labels: kube-state-metrics exports node labels only with" \
      "--metric-labels-allowlist=nodes=[nvidia.com/gpu.product]. Without it the accelerator is unknown:" \
      "give --accelerator when the export is loaded." ;;
    kube_pod_created) echo "EMPTY: kube_pod_created: disabled or denylisted in kube-state-metrics" \
      "(--metric-denylist, --resources). Without it no Pod has a creation time, so start-up time" \
      "(creation to Ready) is not read." ;;
    kube_pod_deletion_timestamp) echo "EMPTY: kube_pod_deletion_timestamp: normal when no Pod was deleted in" \
      "the window; otherwise disabled or denylisted in kube-state-metrics. Without it a Pod being deleted" \
      "counts as a replica until its series stop." ;;
    kube_deployment_annotations) echo "EMPTY: kube_deployment_annotations: off unless kube-state-metrics has" \
      "--metric-annotations-allowlist=deployments=[scaling-manager.llmd.ai/vllm-version], or denylisted." \
      "Without it the vLLM release comes from the image tag, or is unknown (vLLM defaults then unknown)." ;;
  esac
done

# One file to send. It holds your metric labels (Pod, node, model names) and engine arguments, no request content.
# COPYFILE_DISABLE=1 keeps macOS tar from adding a ._ file beside each answer.
COPYFILE_DISABLE=1 tar czf planner-export.tgz planner-export
```
<!-- guide:manual end -->

The check names every chunk that is missing or failed, then prints `bad=0`
when there is none, then a count of series per metric. Fetch a failed chunk
again before you pack. A metric your fleet does not export reads `0`: the
SGLang series on a vLLM fleet, say, or the `inference_extension_` twins on a
current router. `0` for `vllm:kv_cache_usage_perc` (or `sglang:token_usage`)
means the namespace or the window is wrong: nothing can be captured from it.

**Check `window.txt` before you pack.** The describe step copies each
engine container's command and arguments as written in the Deployment. It
does not expand them: a `$(VAR)` or `${VAR}` stays as typed, and an
entrypoint like `/bin/sh -c "vllm serve ... --max-num-seqs ${SEQS}"` stays one
script. Where you see either, edit `DECODE_ARGS` (and `PREFILL_ARGS`) in
`planner-export/window.txt` to the flags the engine really ran with, for
example `DECODE_ARGS=--max-num-seqs 256 --tensor-parallel-size 2`. `capacity-planner`
splits these on whitespace: a JSON value is written compact (no spaces between
its tokens), but a space inside one of its strings still splits it.

**A LeaderWorkerSet or DisaggregatedSet fleet.** For a DisaggregatedSet, set
both `DECODE_WORKLOAD` and `PREFILL_WORKLOAD` to `dset/<name>`: each reads the
role whose `llm-d.ai/role` label is `decode` or `prefill` (on the role, or on
its leader or worker template), from its leader template. If no role carries
the label, the step stops and says so; name the role's LeaderWorkerSet
instead (`kubectl -n <namespace> get lws` lists them). For a LeaderWorkerSet,
`lws/<name>` reads its leader template; a leader Pod, `pod/<name>`, reads the
arguments that Pod ran with. Path 1's engine-arguments step takes the same
forms. Where the
arguments are built at start, in a script or from environment variables,
neither shows them: copy them from the engine's startup log instead (vLLM
prints them on its `non-default args` line) into `window.txt`. Each Pod's
own rank flags, such as `--data-parallel-rank`, can stay as the leader's.
Prometheus must scrape every rank's engine, not only the leader's: the
planner refuses a recording whose engine rows count fewer replicas, or a
lower request rate, than the fleet served
([troubleshooting](../capacity-planner/troubleshooting.md#a-recording-is-refused-engine-rows-do-not-stand-for-the-fleet)).

Send `planner-export.tgz`. It holds metric labels (Pod, node and model names)
and `window.txt`: the window, the engine and the workloads' engine
arguments, which loading it needs and Prometheus does not hold. No request
content.

### Loading a manual export

On a machine with a checkout of this repository, Go, Python 3, and a
`prometheus` binary on `PATH` (from the Prometheus release, or the
`quay.io/prometheus/prometheus` image). The window, the namespace and the
engine come from the export's `window.txt`. A `STOP:` line means
`window.txt` is not what the describe step writes: do not go on.

<!-- guide:load start -->
```bash
# In a checkout of this repository, with Python 3 and a prometheus binary on PATH (the Prometheus release,
# or the quay.io/prometheus/prometheus image). Builds capacity-planner and installs promtool.
make capacity-planner promtool
export PROMTOOL=$(ls bin/promtool-* | sort -V | tail -1)
${PROMTOOL} --version | head -1

# The export came from someone else: look before you extract. This lists any entry that is not a plain
# file name under planner-export/ (a path that climbs out, a link); it should print nothing.
tar tzvf planner-export.tgz | awk '$1 !~ /^[-d]/ || $NF !~ /^planner-export\/[A-Za-z0-9_.:-]*$/'

# Merges the answers into one OpenMetrics file, and prints the lookback to serve them with.
# It reads only the *.json answers, and skips macOS ._ files. window.txt is read as data, never sourced, and checked.
tar xzf planner-export.tgz --no-same-owner
python3 hack/promjson-to-openmetrics.py -o planner-export.om planner-export/*.json
unset PREFILL_ARGS
while IFS='=' read -r k v; do
  case "${k}" in NAMESPACE|FROM|TO|FROM_ISO|TO_ISO|CAPTURE_FROM|ENGINE|DECODE_ARGS|PREFILL_ARGS) export "${k}=${v}" ;; esac
done < planner-export/window.txt
[ -n "${PREFILL_ARGS:-}" ] || unset PREFILL_ARGS
case "${FROM}:${TO}:${CAPTURE_FROM}" in *[!0-9:]*|:*|*::*|*:) echo "STOP: FROM, TO and CAPTURE_FROM in window.txt must be Unix seconds" ;; esac
case "${ENGINE}" in vllm|sglang) ;; *) echo "STOP: ENGINE in window.txt must be vllm or sglang" ;; esac
echo "namespace ${NAMESPACE}, engine ${ENGINE}, decode arguments: ${DECODE_ARGS}"
echo "prefill arguments: ${PREFILL_ARGS-none, prefill takes the decode arguments}"

# Writes TSDB blocks.
${PROMTOOL} tsdb create-blocks-from openmetrics planner-export.om planner-tsdb

# Serve the blocks. A retention longer than their age, or Prometheus deletes them on start.
# LOOKBACK is the one the converter printed: four scrape intervals of the slowest metric, often kube-state-metrics'.
# A deleted Pod stays visible that long (see Limits).
export LOOKBACK=<the lookback the converter printed, e.g. 240s>
touch empty.yml
prometheus --config.file=empty.yml --storage.tsdb.path=planner-tsdb \
  --storage.tsdb.retention.time=10y --query.lookback-delta="${LOOKBACK}" \
  --web.listen-address=127.0.0.1:9091 &
until curl -sf http://127.0.0.1:9091/-/ready >/dev/null; do sleep 1; done

# Then capture as in the first path, against the local copy, from 5 minutes into the export.
# Each role is priced with its own engine arguments: DECODE_ARGS, and PREFILL_ARGS when window.txt has them.
# With no PREFILL_ARGS line or an empty one (an older export wrote one empty), prefill takes DECODE_ARGS.
bin/capacity-planner capture --prometheus http://127.0.0.1:9091 --namespace "${NAMESPACE}" --kube-state-metrics \
  --from "${CAPTURE_FROM}" --to "${TO}" --engine "${ENGINE}" --engine-args "${DECODE_ARGS}" \
  ${PREFILL_ARGS+--prefill-engine-args "${PREFILL_ARGS}"} \
  -o run.capture.jsonl.gz
```
<!-- guide:load end -->

The converter refuses a file whose query failed, a chunk one metric has and
another lacks, native histograms (which it cannot write), and an export with no
sample at all, so a broken export is not read as a window with less load.

### Limits of a manual export

- **A series' end is blurred by up to the lookback.** Prometheus marks a series
  stale when its target goes away; the query API does not return those marks.
  The local copy instead holds the last value for `--query.lookback-delta`:
  four times the slowest metric's scrape interval, which the converter prints
  (`240s` with kube-state-metrics at 60 s). The slowest, because a lookback
  sized for engines at 5 s (`20s`) loses kube-state-metrics, scraped every
  60 s, at three steps in four. The cost: a deleted Pod's series stays
  visible for that long, so a fleet that scales down shows phantom replicas
  for up to the lookback the converter printed.
- **The window must start 5 minutes before the one you want answered.** Some
  inputs are 5-minute rates.

Where your Prometheus allows its admin API, a TSDB snapshot keeps everything,
staleness marks included: [From a Prometheus snapshot](#from-a-prometheus-snapshot).

## From a Prometheus snapshot

`_bucket` series are often kept for days, not weeks. To keep a window worth
having, snapshot Prometheus while it still holds it, and record from the
snapshot later. A snapshot keeps everything, and capturing from it puts no
load on your Prometheus.

1. Snapshot Prometheus. The snapshot API needs Prometheus started with
   `--web.enable-admin-api`; the snapshot is written under its data
   directory. `PROM_POD` is the Prometheus Pod:

   ```bash
   kubectl -n "$PROM_NS" port-forward "svc/$PROM_SVC" 9090:9090 &
   # The answer is {"status":"success","data":{"name":"20261101T120000Z-..."}}.
   NAME=$(curl -s -XPOST http://localhost:9090/api/v1/admin/tsdb/snapshot | jq -r .data.name)
   test -n "$NAME" && test "$NAME" != null || echo "no snapshot: is the admin API on?"
   kubectl -n "$PROM_NS" cp -c prometheus "$PROM_POD:/prometheus/snapshots/$NAME" ./snapshot
   ```

   `kubectl cp` runs `tar` inside the container. A distroless Prometheus
   image has none, and the copy fails with `executable file not found`. Then
   copy from a helper Pod that has `tar` (an image from a registry you trust)
   and mounts the Prometheus data volume's claim read-only at `/prometheus`:
   `kubectl -n "$PROM_NS" cp "$HELPER_POD:/prometheus/snapshots/$NAME" ./snapshot`.
   A ReadWriteOnce claim mounts only on Prometheus' own node, so schedule the
   helper there. An ephemeral container works as well where the cluster
   allows them:
   `kubectl debug -n "$PROM_NS" "$PROM_POD" --target=prometheus --image=IMAGE-WITH-TAR -c snap -- sleep 3600`,
   then `kubectl cp -c snap "$PROM_POD:/proc/1/root/prometheus/snapshots/$NAME" ./snapshot`.

   Without the admin API, use [Path 2](#path-2-record-with-curl-and-jq).

2. Serve the snapshot with a local Prometheus. Give it a retention longer
   than the snapshot's age, or it deletes the old blocks on start:

   ```bash
   prometheus --storage.tsdb.path=./snapshot --storage.tsdb.retention.time=10y \
     --web.listen-address=127.0.0.1:9091 --config.file=/dev/null
   ```

3. Record as in [Path 1](#path-1-record-with-capacity-planner), with
   `--prometheus http://127.0.0.1:9091`.

## Prometheus with a token

`capacity-planner` sends a token only over https, to Prometheus' own host. A plain
`kubectl port-forward` gives an http URL, so forward to Prometheus' https port
and pass the CA and the token:

```bash
bin/capacity-planner capture --prometheus https://localhost:9091 --ca-file ca.crt \
  --bearer-token-file token ...
```

The certificate must name `localhost`, or add `--insecure-skip-verify` knowing
what it gives up. For curl, put the header in a file and use
`export CURL_OPTS="--cacert ca.crt -H @auth-header.txt"`, the file holding
`Authorization: Bearer <token>`.

A query frontend that aligns a range query to its step (Thanos' and Mimir's
do) answers off the capture's grid, and the capture stops naming it. Query
Prometheus itself, or use Path 2, which asks no range queries.

## Without kube-state-metrics

Drop `--kube-state-metrics`, and say which Pods serve which role, one variant
each, with their GPUs:

```bash
bin/capacity-planner capture ... --role-of decode=app=<decode pod label> --role-of prefill=app=<prefill pod label> \
  --gpus-per-replica 1 --accelerator <accelerator name>
```

Every Pod then reads as Ready, and the capture says so.

## Capture flags

```
capacity-planner capture  --prometheus URL --namespace NS (--kube-state-metrics | --role-of ROLE=SELECTOR ...)
                          --from T0 --to T1 -o run.capture.jsonl.gz
                          [--step 15s] [--policy FILE]
                          [--engine vllm|sglang] [--engine-args "ARGS"] [--batch-cap N]
                          [--gpus-per-replica 1] [--accelerator NAME]
                          [--vllm-version 0.34] [--engine-image IMAGE] [--model-config config.json]
                          [--bearer-token-file FILE] [--ca-file FILE] [--insecure-skip-verify]
                          [--timeout 2m30s] [--force] [--lmcache-namespace NS]
capacity-planner generate -in run.capture.jsonl.gz -o run.jsonl.gz
```

| Flag | What it does |
| --- | --- |
| `--prometheus` | The Prometheus base URL, http or https. Required. An error message never quotes its userinfo, path, query or fragment: they read `REDACTED`. |
| `--namespace` | The namespace whose models to capture. Required. |
| `--kube-state-metrics` | Read Pods, owners, readiness, GPUs and nodes from kube-state-metrics in the same Prometheus. |
| `--role-of ROLE=SELECTOR` | The Pods a label selector picks serve ROLE (`prefill`, `decode`, `both`), as one variant. Repeat per variant. Two selectors that pick the same Pod, or whose name prefixes nest, are refused. |
| `--from`, `--to` | The window, RFC 3339 or Unix seconds, truncated to the millisecond. Required. |
| `--step` | The interval between cycles, taken as the run's optimize interval. Whole milliseconds. Default 15s. |
| `--policy` | A scaling-policy entry (YAML), applied as the default entry. Default: the defaults. |
| `--engine` | The inference engine the variants run, `vllm` or `sglang`. Default `vllm`. |
| `--engine-args` | The engine's arguments, space-separated, parsed for its parameters (kube-state-metrics has none). |
| `--batch-cap` | The engine's batch cap (`--max-num-seqs`, or `--max-running-requests`). Warned about when `--engine-args` sets it too. |
| `--gpus-per-replica`, `--accelerator` | Stand-ins where kube-state-metrics gives no GPU request or node label. |
| `--vllm-version`, `--engine-image` | The vLLM release the engines run, as `0.34`, or their image, whose `vllm-openai` tag names it. Stand-ins where kube-state-metrics shows neither; see [The vLLM release](#the-vllm-release). |
| `--model-config` | The served model's `config.json`, or the checkpoint directory holding it. Its attention kind (full, sliding window, MLA, hybrid) and MoE class (dense, MoE with or without expert parallelism) are recorded with every variant and named beside the engine in the answers. Reported, not priced. The window's variants must all serve that one model. An answer from `--recording` takes the same flag, and reads a `config.json` beside the recording when it is not given. It moves no figure's value, but the `MLAAttention` and `HybridAttention` flags then follow the config, so a figure can read outside the validated region where the name said nothing (or inside, where only an MLA name match put it outside). A sliding window is read as vLLM serves it: on every layer unless `layer_types` or `max_window_layers` say otherwise. |
| `--bearer-token-file` | A file holding a bearer token. Sent over https only, to Prometheus' own host only, never on a redirect elsewhere. Refused with an http URL, or when the file is empty. |
| `--ca-file` | A PEM file of the CAs that sign an https Prometheus' certificate, trusted in place of the system's. Refused with an http URL. |
| `--insecure-skip-verify` | Do not verify the certificate. Warned about on stderr. Refused with an http URL. |
| `--timeout` | How long one HTTP request may take. Default 2m30s, a little over Prometheus' own 2-minute query timeout. |
| `--force` | Capture past the load and memory bounds in [Load on Prometheus](#load-on-prometheus). |
| `--lmcache-namespace` | Where LMCache's multi-process server runs, when not in `--namespace`: its `lmcache_mp_` series are checked there. |
| `-o` | Where to write the capture. Written beside it and renamed over it when whole, so a failed run leaves the file that was there. |

`generate` takes `-in` (the capture, gzipped or not) and `-o` (the recording;
a name ending in `.gz` is gzipped). Both commands write their file readable by
its owner only (mode 0600).

What a capture cannot know, and flags: engine parameters (always from flags),
rollouts (never seen), readiness without kube-state-metrics (every Pod reads as
Ready), and LeaderWorkerSets (use `--role-of`). The `flag:` lines name each.

## The vLLM release

vLLM changes the defaults of `--max-num-batched-tokens`,
`--gpu-memory-utilization` and `--block-size` between releases. The planner
records which release each variant runs and shows the values those defaults
resolve to, the ones the controller uses. They key the learned state, and
the answers report them for reference: no figure of an answer reads them
yet. See [vLLM versions](../../reference/vllm-versions.md) for the values.

With `--kube-state-metrics` the capture reads the release from what the
workload ran: the Deployment's `scaling-manager.llmd.ai/vllm-version`
annotation, else a `vllm-openai` image tag. kube-state-metrics exports the
image by default; the annotation needs
`--metric-annotations-allowlist=deployments=[scaling-manager.llmd.ai/vllm-version]`.
kube-state-metrics exports no LeaderWorkerSet annotations, so an annotated
LeaderWorkerSet reads as unknown.

Where neither is there (llm-d's own images, a digest, no kube-state-metrics),
give the release yourself:

```bash
bin/capacity-planner capture ... --vllm-version 0.34
```

What the workload shows wins over the flags, as in the controller. The
capture prints the release and where it came from on each variant's line,
and every answer repeats it:

```
engine          vLLM 0.34 (from image tag); step budget 8192 (default), priced as a cap on the batch per rank; for reference, not priced yet: GPU memory 1 (default), block size 64 (default)
```

The capture adds a `flag:` line when:

- a flag names another release than the workload shows (the workload's is used);
- a variant ran more than one release in the window (the one most Pods ran is used);
- no annotation, tag or flag names the release (`vLLM release unknown`): give
  `--vllm-version`, or allowlist the annotation.

A recording made before v0.12.0, the first release that reads it, says `vLLM release not recorded`.

A capture now keys learned state by the engine's real image, as the
controller does (for SGLang too, though SGLang has no release to read). Learned
state from a capture made before this release is not reused.

## Load on Prometheus

A capture turns every query the controller asks into one range query over the
whole window, at the step. Each is asked once; every later step reads its
answer from memory. Every query is required: one that fails stops the capture
at once and names the query.

One namespace with a single vLLM model asks about 56 queries. For a fleet of
10 engine Pods that is roughly 1,000 series. Rough figures, to be read as
orders of magnitude:

| Window at 15 s | Steps | Range requests | Samples returned and held |
| --- | --- | --- | --- |
| 1 hour | 241 | about 56 | about 240,000 |
| 1 day | 5,761 | about 56 | about 5.8 million |
| 3 days | 17,281 | about 112 (two parts each) | about 17 million; past the step bound, so it needs `--force` |

Prometheus reads far more raw samples than it returns: each `rate(...[1m])`
reads every scrape in its minute at every step.

- **Capture off-peak**, or better, **from a snapshot** served by a local
  Prometheus ([From a Prometheus snapshot](#from-a-prometheus-snapshot)),
  never by loading a production server with a long window.
- **The usual failure is `query.max-samples`**: a histogram over many steps
  loads more samples than the server allows in one query. It stops the
  capture with exit 1 and names the query; shorten the window or lengthen
  the step.
- **Bounds.** Steps times queries is bounded at 1,000,000 (a little over two
  days at 15 s), and the samples held at 20 million (a little over a
  gigabyte). Past either, the capture stops with exit 2 unless `--force`.

## Read the command-line answers

`replicas-to-slo` and `slo-to-rate` price a P/D fleet's TTFT when a TTFT flag
is given, from a recording or from the capture flags:

```
capacity-planner replicas-to-slo --recording run.jsonl.gz --ttft-percentile 90 [--assume-vp TOKENS] [--assume-a D] [--prefill N]
capacity-planner slo-to-rate     --recording run.jsonl.gz --tpot 4ms --ttft 200ms@p95 [--assume-vp TOKENS] [--assume-a D] [--prefill N]
```

- **Treat every TTFT figure as unvalidated.** Each prints `UNVALIDATED`, and it
  is not a bound. At the assumed prompt `cv` 2, which is what the CLI prints
  for rows with no computed-token histogram, it read 1.1x to 4.1x the router's
  percentile on the validation recordings; at the spread the load generator
  sent (`cv` 0.06) up to 33% under it. Do not size from it yet.
- **Read the `assumed` row first.** It names each input the recording did not
  give. `V_P ASSUMED` means `--assume-vp` supplied `V_P`, and `a ASSUMED` that
  `--assume-a` supplied `a`. It says so when an assumed term alone refuses a
  target, or moves the answer by 25%. `--assume-router-hop 8ms` supplies the
  hop between the router and the engines.
- **`--assume-transfer-base 20ms`** states the fixed part of the KV
  transfer per request; the rest of the measured transfer scales with the
  tokens moved. The recording holds one mean transfer, not its split, so
  the default is 0: all of it scales. A base above the measured transfer is
  held at it, and with no transfer measured it is ignored. It is named on
  the `assumed` row, not raised as a flag, and puts the figures outside the
  validated region. `replicas-to-slo` and `slo-to-rate` only: `at-replicas`
  refuses it.
- **`--assume-vp` overrides,** as every `--assume-*` flag does. Give it when
  `NoPrefillCapacity` is refused, or to ask what another `V_P` would do. A
  learned figure prints beside it: `V_P 130000 ASSUMED, learned 115621`.
- **A decode row can rest on `V_P` too.** Where the prefill knee is the lower,
  it sets the operating rate, and the rate and TPOT rows say so.
- **`NoFixedLatency`** means the fleet ran one prompt length. Give
  `--assume-a 11ms` for a flagged answer.
- **Read the band as a sweep.** The knee, each TTFT and each TTFT rate print the
  figure at `V_P` and `f` 15% either way. It is not a confidence interval. A
  `Saturated` in it means the rate has no 15% to spare.
- **`Saturated` means the rate is at or above the knee.** It is a refusal, exit
  `3`, not a figure.
- **`slo-to-rate` with no `--tpot` still needs a decode line.** Decode's knee
  is then all that bounds decode. When the recording has no usable line
  (`NoLine`, `PinnedDefaultB`), the `decode` and `fleet` rows are refused, exit
  `3`; the `prefill` row is still priced.
- **`PinnedDefaultB`** means the line's intercept B was never measured. Give
  `--assume-itl-b floor` (B from the low-fill latency the decode Pods read) or
  `--assume-itl-b 12ms` for a flagged answer. The `ITL B` row says which B
  was used.
- **The `line check` row** compares the token rate the latency line predicts
  with the rate the decode rows reported. `MISMATCH` (a median error above
  15%, flagged `LineMismatch`): do not size from it. `at another shape`: the
  check ran on rows whose KV per request is more than 20% from the one priced.
  `not run`: under 5 rows to compare; it is not a pass.

- **Aggregated fleets and per-bucket answers are refused.** TTFT is priced
  for P/D fleets only.

### The joint TTFT model

```bash
capacity-planner joint-ttft --recording run.jsonl.gz [--cycle N|last] [--model M] [--prefill N] [--decode N] [--rate R] [--prompt I] [--hop D] [-o json]
```

Prices the joint prefill and decode model's mean TTFT at a fleet, from what
the recording's rows taught it by the cycle asked for. Each of `--prefill`,
`--decode` and `--rate` defaults to 0, which means the cycle's own: the fleet
and the rate that ran. `--prompt 0` prices at the prompt the work was read at.
`--hop 0`, the default, assumes a router hop of 20 ms and flags it
`RouterHopAssumed`; give `--hop` in seconds to state it.

At the 120th cycle of a staircase run, with a third decode Pod:

```console
$ capacity-planner joint-ttft --recording ttft-staircase-b-off.jsonl.gz --cycle 120 --decode 3
joint TTFT, ns-01/model-02 at cycle 120: 2 prefill + 3 decode at 29.80 req/s (ran 2 + 2 at 29.80)
  mean TTFT 145.0 ms (engines 125.0 ms, hop 20.0 ms)
    overhead a(rho_p) 23.6 ms, prefill T_P 39.5 ms at rho_p 0.371
    KV pull T_X 33.0 ms, unshared 24.8 ms at 0.247 of the decode pod
    decode m_d*ITL(k) + d 28.9 ms, batch k 7.2, ITL 3.6 ms
    flags: [OverheadMeasured RouterHopAssumed]
  inputs: S 24.9 ms at 8001 tokens (232 readings); a0 14.8 ms (232); pull table 3 buckets
          ITL(k) 1.944 ms + 0.2322 ms*k (232); decode own 5.03*ITL + 10.7 ms (232); n_out 200
  note: UNVALIDATED beyond the staircase's 2+2 phases; ...
```

It exits `3` with a refusal, which is printed: `no-prefill-capacity` until a
prefill Pod has read an empty engine queue, `saturated` at a prefill or pull
knee, `batch-unheld` where the decode batch cannot hold the rate. With `-o json`
the answer is JSON (`schemaVersion` 1, keys lowerCamel).

It reads the rows as the slo analyzer does: only for a model with one prefill
and one decode variant; new engine arguments or a new accelerator start the
inputs over; on SGLang the prompt is the prompt length. One thing differs: the
analyzer leaves out a replica's first two minutes after it starts or comes back
from idle, while it settles, and this command reads them, so its inputs can
lean on a warm-up the analyzer never saw.

Exit codes for all of these: `0` answered, `3` a refusal, `2` a usage error,
`1` anything else. `130` or `143`: stopped by Ctrl-C or SIGTERM, and the output
may be incomplete. The full CLI reference is in
[the planner's design](../../proposals/slo-capacity-planner.md#cli-reference).

## See also

- [The metrics it needs](../capacity-planner/metrics.md) and
  [Troubleshooting](../capacity-planner/troubleshooting.md#recording-a-window)
- [Capacity planner capture and generate](../../developer-guide/capacity-planner-capture.md): how capture is built
- [The capacity planner's design](../../proposals/capacity-planner.md), and its
  [CLI](../../proposals/slo-capacity-planner.md#cli-reference)
