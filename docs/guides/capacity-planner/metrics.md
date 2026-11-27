# Capacity planner: the metrics it needs

The [capacity planner](README.md) answers from a window of your Prometheus:
what the model servers, the router and kube-state-metrics reported while the
fleet ran. This page lists every series it reads, so you can check your
Prometheus keeps them, or save them before they age out.

To save a window, follow [Record a window](../record-for-capacity-planner/).

## What your Prometheus must keep

- **Scrape every 15 s,** 30 s at most. The planner reads one-minute rates and
  the least queue in each minute. At 60 s a minute holds one sample: the
  rates read nothing, and the least queue is that one reading, so a minute
  with a queue at the scrape reads as busy throughout and prefill capacity
  `V_P` is learned from minutes that were not.
- **Keep the histograms.** Many setups drop `_bucket` series, or keep them for
  days rather than weeks. Without them the TTFT answers fall back to assumed
  figures.
- **Keep the labels.** Engine series need `namespace` and `pod` (your
  PodMonitor or ServiceMonitor adds them), plus the `model_name` the engine
  sets. A relabeling that drops `pod` makes every replica look like one.
- **Cover a range of load.** The decode latency line is fitted from at least
  10 readings in 30 minutes whose KV-cache use spans 0.30 or more, between
  0.15 and 0.80 full. Prefill capacity needs at least 5 minutes in 30 when a
  prefill pod always had a queue. A flat, quiet hour teaches neither: record
  a window of 30 minutes or more in which load rises.
- **Start 5 minutes early.** Some inputs are 5-minute rates. Save, or keep,
  at least 5 minutes before the start of the window you want answered.

## How to read the tables

- **Need**:
  - **required**: without it the capture finds no replica and stops (exit 2).
  - **TPOT** or **TTFT**: the planner reads it for that answer. Without it,
    that answer is refused or rests on an assumed figure, which the output
    names.
  - **optional**: it refines an answer, or raises a flag, or is recorded
    for checking. The answer stands without it.
- **Labels** are the labels the planner's queries select on or group by. A
  series without them is not read.
- One series per row: a histogram is listed by the parts the planner reads
  (`_bucket`, `_sum`, `_count`), a counter by its `_total` name.

The [developer page](../../developer-guide/capacity-planner-capture.md#where-each-input-comes-from)
maps each query to the field it fills. A test
(`internal/whatifsource/metrics_doc_test.go`) fails when a query reads a series
this page does not list, or this page lists one no query reads.

## vLLM

Exported by each vLLM engine Pod. Labels: `namespace`, `pod`, `instance`,
`model_name` on every row; `engine` (the data-parallel rank) where vLLM sets
it.

| Series | Type | Extra labels | Feeds | Need |
| --- | --- | --- | --- | --- |
| `vllm:kv_cache_usage_perc` | gauge | | KV-cache fill: finds the replicas, and is the x axis of the latency line | required |
| `vllm:cache_config_info` | gauge (info) | `num_gpu_blocks`, `block_size` | KV capacity in tokens, which turns fill into tokens | TPOT |
| `vllm:inter_token_latency_seconds_sum` | histogram sum | | mean inter-token latency: the latency line | TPOT |
| `vllm:inter_token_latency_seconds_count` | histogram count | | mean inter-token latency; tokens per step under speculative decoding | TPOT |
| `vllm:generation_tokens_total` | counter | | token rate: the line check | TPOT |
| `vllm:num_requests_running` | gauge | | running requests: the batch cap, where no flag gives it; with the KV fill, the KV a decode request holds (tokens in use over running requests). Without it, decode is priced at the whole context, `IL + D x OL/2`, never at the prefix-hit discount. A stated prompt or output length scales the reading by its context over the measured one, a fixed per-request excess with it, which errs towards more replicas. On SGLang a reading above the context is priced at the context. Since v0.12.3 the controller also prices a reading above the context as read (issue #188); on the decode of a P/D model it also counts the requests that hold KV without running, takes a reading above the context only over 8 or more running requests, and never prices under the v0.12.2 price | optional |
| `vllm:num_requests_waiting` | gauge | | queue length, and the least queue in a minute: prefill busy minutes (`V_P`) | TTFT |
| `vllm:num_requests_waiting_by_reason` | gauge | `reason="capacity"` | requests waiting for KV space or a batch slot (vLLM 0.20 and later). When published, the planner counts only these as a prefill queue for `V_P`; deferred requests, such as those waiting for a cached prefix to load, do not count | optional |
| `vllm:num_requests_kv_fetch_by_stage` | gauge | `stage="completed_waiting"` | P/D decode requests whose KV has landed and that wait for a batch slot (vLLM 0.32 and later) | optional |
| `vllm:request_prompt_tokens_sum` | histogram sum | | mean prompt length: KV per request | TPOT |
| `vllm:request_prompt_tokens_count` | histogram count | | mean prompt length | TPOT |
| `vllm:request_prompt_tokens_bucket` | histogram buckets | `le` | prompt lengths, recorded | optional |
| `vllm:request_generation_tokens_sum` | histogram sum | | mean output length: KV per request, the rate per replica | TPOT |
| `vllm:request_generation_tokens_count` | histogram count | | mean output length; request rate | TPOT |
| `vllm:request_generation_tokens_bucket` | histogram buckets | `le` | output-length spread `D`: KV per request | optional |
| `vllm:prefix_cache_hits_total` | counter | | prefix-cache hit rate: the prompt tokens a request computes (prefill only; a hit does not shrink the KV a decode request holds) | optional |
| `vllm:prefix_cache_queries_total` | counter | | prefix-cache hit rate | optional |
| `vllm:external_prefix_cache_hits_total` | counter | | prompt tokens a KV connector served (an offload tier, LMCache, a P/D pull), which the prefix-cache hits above do not count; recorded (vLLM 0.11.1 and later, with a KV connector) | optional |
| `vllm:external_prefix_cache_queries_total` | counter | | prompt tokens a KV connector was asked for; recorded | optional |
| `vllm:prompt_tokens_cached_by_source_total` | counter | `source` | cached prompt tokens by the tier that supplied them (`device`, `host`, `disk`, `p2p`, `external_unspecified`); recorded (vLLM 0.32 and later) | optional |
| `vllm:request_prefill_kv_computed_tokens_sum` | histogram sum | | computed prompt tokens per second: prefill capacity `V_P` | TTFT |
| `vllm:request_prefill_kv_computed_tokens_bucket` | histogram buckets | `le` | prompt spread `cv` | optional |
| `vllm:request_inference_time_seconds_sum` | histogram sum | | service time: the fixed prefill latency `a` | TTFT |
| `vllm:request_inference_time_seconds_count` | histogram count | | service time | TTFT |
| `vllm:time_to_first_token_seconds_sum` | histogram sum | | the engine's mean TTFT: the decode side of TTFT when no line prices it | optional |
| `vllm:time_to_first_token_seconds_count` | histogram count | | the engine's mean TTFT | optional |
| `vllm:time_to_first_token_seconds_bucket` | histogram buckets | `le` | the engine's TTFT distribution, recorded | optional |
| `vllm:request_queue_time_seconds_sum` | histogram sum | | mean queue time, recorded | optional |
| `vllm:request_queue_time_seconds_count` | histogram count | | mean queue time, recorded | optional |
| `vllm:request_queue_time_seconds_bucket` | histogram buckets | `le` | queue time distribution, recorded | optional |
| `vllm:request_prefill_time_seconds_sum` | histogram sum | | mean prefill time, recorded | optional |
| `vllm:request_prefill_time_seconds_count` | histogram count | | mean prefill time, recorded | optional |
| `vllm:request_time_per_output_token_seconds_bucket` | histogram buckets | `le` | the engine's TPOT distribution, recorded | optional |
| `vllm:kv_block_reuse_gap_seconds_bucket` | histogram buckets | `le` | the time since a cached block's last use, at each prefix-cache hit: the prefix hit at another prefill count (`--prefill-hit-at-count`); recorded (with `--kv-cache-metrics`) | optional |
| `vllm:kv_block_idle_before_evict_seconds_bucket` | histogram buckets | `le` | how long an evicted block sat unused: the cache's horizon, for the same; recorded (with `--kv-cache-metrics`) | optional |
| `vllm:nixl_xfer_time_seconds_sum` | histogram sum | | KV transfer time between prefill and decode | optional |
| `vllm:nixl_xfer_time_seconds_count` | histogram count | | KV transfer time | optional |
| `vllm:nixl_bytes_transferred_sum` | histogram sum | | bytes NIXL moved: with the transfer time above, a peer hit's load time per token; recorded | optional |
| `vllm:kv_offload_load_bytes_total` | counter | | bytes an offload tier loaded into the GPU; recorded (vLLM 0.24 and later, with an `OffloadingConnector`) | optional |
| `vllm:kv_offload_load_time_total` | counter | | seconds those loads took: a host, disk or peer hit's load time per token; recorded | optional |
| `vllm:kv_offload_tiering_read_bytes_total` | counter | | bytes a tiering offload read from a peer or disk tier into its CPU tier; recorded (vLLM 0.28 and later) | optional |
| `vllm:kv_offload_tiering_read_time_total` | counter | | seconds those reads took: added to a disk or peer hit's load time; recorded | optional |
| `vllm:e2e_request_latency_seconds_sum` | histogram sum | | the engine's mean end-to-end time: with the HTTP handler's below, a pod's API-server time outside the engine, `--rank-mixture-remainder-server` | optional |
| `vllm:e2e_request_latency_seconds_count` | histogram count | | as above | optional |
| `http_request_duration_seconds_sum` | histogram sum | `handler` (no `model_name`) | the API server's mean handler time, on the generation endpoints and on their `/render`: `--rank-mixture-remainder-server` | optional |
| `http_request_duration_seconds_count` | histogram count | `handler` (no `model_name`) | as above; the `/render` calls a second | optional |
| `vllm:num_preemptions_total` | counter | | preemptions | optional |
| `vllm:spec_decode_num_accepted_tokens_total` | counter | | accepted draft tokens: TPOT per token under speculative decoding | optional |

## SGLang

Read instead of the vLLM series when you capture with `--engine sglang`. Same
labels as vLLM. SGLang publishes no computed-token series, so prefill capacity
`V_P` is never learned for it: TTFT needs `--assume-vp`.

| Series | Type | Extra labels | Feeds | Need |
| --- | --- | --- | --- | --- |
| `sglang:token_usage` | gauge | | KV-cache fill: finds the replicas, the latency line | required |
| `sglang:max_total_num_tokens` | gauge | | KV capacity in tokens | TPOT |
| `sglang:inter_token_latency_seconds_sum` | histogram sum | | mean inter-token latency: the latency line | TPOT |
| `sglang:inter_token_latency_seconds_count` | histogram count | | mean inter-token latency | TPOT |
| `sglang:generation_tokens_total` | counter | | token rate: the line check | TPOT |
| `sglang:num_running_reqs` | gauge | | running requests: the batch cap, where no flag gives it | optional |
| `sglang:num_queue_reqs` | gauge | | queue length, the least queue in a minute | optional |
| `sglang:prompt_tokens_histogram_sum` | histogram sum | | mean prompt length | TPOT |
| `sglang:prompt_tokens_histogram_count` | histogram count | | mean prompt length | TPOT |
| `sglang:prompt_tokens_histogram_bucket` | histogram buckets | `le` | prompt lengths, recorded | optional |
| `sglang:generation_tokens_histogram_sum` | histogram sum | | mean output length; the token rate where the counter is missing | TPOT |
| `sglang:generation_tokens_histogram_count` | histogram count | | mean output length; request rate | TPOT |
| `sglang:generation_tokens_histogram_bucket` | histogram buckets | `le` | output-length spread `D` | optional |
| `sglang:cached_tokens_total` | counter | | prefix-cache hit rate | optional |
| `sglang:prompt_tokens_total` | counter | | prefix-cache hit rate | optional |
| `sglang:e2e_request_latency_seconds_sum` | histogram sum | | service time (less queue time) | optional |
| `sglang:e2e_request_latency_seconds_count` | histogram count | | service time | optional |
| `sglang:queue_time_seconds_sum` | histogram sum | | mean queue time; service time | optional |
| `sglang:queue_time_seconds_count` | histogram count | | mean queue time; service time | optional |
| `sglang:queue_time_seconds_bucket` | histogram buckets | `le` | queue time distribution, recorded | optional |
| `sglang:time_to_first_token_seconds_sum` | histogram sum | | the engine's mean TTFT | optional |
| `sglang:time_to_first_token_seconds_count` | histogram count | | the engine's mean TTFT | optional |
| `sglang:time_to_first_token_seconds_bucket` | histogram buckets | `le` | the engine's TTFT distribution, recorded | optional |

## Router (EPP)

Exported by the llm-d router (the endpoint picker). Labels: `namespace`, and
`model_name` and `target_model_name` unless the row says otherwise. Where a
series has an `inference_extension_` twin, the planner reads the `llm_d_epp_`
name and falls back to the other: save both, whichever your router version
exports. Without the router's series the decode answers stand; arrival rate,
flow control and the router's own latency go unread.

| Series | Type | Extra labels | Feeds | Need |
| --- | --- | --- | --- | --- |
| `llm_d_epp_request_streaming_tpot_seconds_bucket` | histogram buckets | `le` | per-request TPOT spread: a TPOT percentile from the mean | TPOT |
| `llm_d_epp_request_streaming_tpot_seconds_sum` | histogram sum | | the router's mean TPOT | optional |
| `llm_d_epp_request_streaming_tpot_seconds_count` | histogram count | | the router's mean TPOT | optional |
| `llm_d_epp_request_ttft_seconds_bucket` | histogram buckets | `le`, `streaming="true"` | the router's TTFT: the hop between router and engines | optional |
| `llm_d_epp_request_ttft_seconds_sum` | histogram sum | `streaming="true"` | the router's mean TTFT | optional |
| `llm_d_epp_request_ttft_seconds_count` | histogram count | `streaming="true"` | the router's mean TTFT | optional |
| `llm_d_epp_flow_control_queue_size` | gauge | `inference_pool`, `job` | flow control on or off; scheduler queue; which pool serves a model | TTFT |
| `inference_extension_flow_control_queue_size` | gauge | `job` | as above, older routers | TTFT |
| `llm_d_epp_flow_control_queue_bytes` | gauge | | scheduler queue in bytes | optional |
| `inference_extension_flow_control_queue_bytes` | gauge | | as above, older routers | optional |
| `llm_d_epp_flow_control_request_enqueue_duration_seconds_count` | histogram count | `job` | arrival rate, with flow control on | optional |
| `inference_extension_flow_control_request_enqueue_duration_seconds_count` | histogram count | `job` | as above, older routers | optional |
| `llm_d_epp_scheduler_attempts_total` | counter | `status="success"`, `target_model_name`, `endpoint_name` (no `model_name`) | arrival rate, with flow control off; recorded: requests placed per Pod | optional |
| `inference_extension_scheduler_attempts_total` | counter | `status="success"` (no `model_name`) | as above, older routers | optional |
| `llm_d_epp_flow_control_request_queue_duration_seconds_bucket` | histogram buckets | `le`, `outcome="Dispatched"` | flow-control wait | optional |
| `llm_d_epp_flow_control_pool_saturation` | gauge | `inference_pool`, `stage` (no model labels) | pool saturation | optional |
| `llm_d_epp_flow_control_requests_total` | counter | `inference_pool`, `outcome` (no model labels) | flow-control outcomes | optional |
| `llm_d_epp_request_processing_duration_seconds_bucket` | histogram buckets | `le` (no model labels) | the router's own time: `--rank-mixture-remainder-spread` | optional |
| `llm_d_epp_request_processing_duration_seconds_sum` | histogram sum | (no model labels) | the router's mean own time: `--rank-mixture-remainder-spread` | optional |
| `llm_d_epp_request_processing_duration_seconds_count` | histogram count | (no model labels) | as above | optional |
| `llm_d_epp_token_producer_render_duration_seconds_sum` | histogram sum | (no model labels) | the router's mean render call, inside its own time: `--rank-mixture-remainder-server` | optional |
| `llm_d_epp_token_producer_render_duration_seconds_count` | histogram count | (no model labels) | as above | optional |
| `llm_d_epp_prefix_predicted_cached_tokens_sum` | histogram sum | `plugin_name`, `endpoint_role` (no model labels) | prefix duplication across prefill pods: `--prefill-hit-at-count` with the KV residency histograms | optional |
| `llm_d_epp_prefix_best_available_cached_tokens_sum` | histogram sum | `plugin_name`, `endpoint_role` (no model labels) | as above | optional |
| `llm_d_epp_prefix_best_predicted_cached_tokens_sum` | histogram sum | `plugin_name`, `endpoint_role` (no model labels) | recorded: the prefix producers' counts per role | optional |
| `llm_d_epp_prefix_prompt_tokens_sum` | histogram sum | `plugin_name`, `endpoint_role` (no model labels) | as above | optional |
| `llm_d_epp_prefix_predicted_cached_tokens_count` | histogram count | `plugin_name`, `endpoint_role` (no model labels) | as above | optional |
| `llm_d_epp_per_endpoint_queue_size` | gauge | `model_server_endpoint` (no model labels) | recorded: each Pod's queue as the router read it | optional |
| `llm_d_epp_inflight_requests` | gauge | `endpoint_name`, `producer_name` (no model labels) | recorded: the requests the router holds in flight per Pod | optional |
| `llm_d_epp_inflight_tokens` | gauge | `endpoint_name`, `producer_name` (no model labels) | recorded: their uncached prompt tokens | optional |

The request-processing and render series carry no model. A recording or a
capture gives them to a model only where the router's TTFT names that model
and no other in the namespace; with two models behind one namespace's
routers, neither gets them. The two prefix producer series are joined the
same way. The HTTP handler series carry no model either: they are read per
Pod, for the Pods the model's own series name. A P/D router runs one prefix
producer per profile, and each counts every prompt, so the producers are
not summed: the planner reads the one that predicted the most cached
tokens. The share it could have hit on the best endpoint and sent elsewhere
is how much the routing duplicates a prefix across prefill pods, which the
hit at another prefill count reads; without them the planner takes no
duplication. Behind a P/D router the best available endpoint is chosen
before the role filter, so it spans the decode pods too: a producer that
indexes the decode pods' caches reads a larger loss, and a larger
duplication, than the prefill routing gave up.

A recording also keeps the router's view of each model-server Pod and each
prefix producer's counts per role, each cycle. They are read only from the
router Pods that serve the model: those whose placements in
`llm_d_epp_scheduler_attempts_total` name that model and no other. A
second router replica that routes another model, or routes nothing, is
left out, so these join where the namespace holds two models. The router
counts a placement on the primary profile's Pod only, which in P/D is the
decode Pod: a prefill Pod has no placements, and reads as unknown there,
not zero. The router publishes no TTFT per Pod, and its prefix series per
role, not per Pod. Only the `llm_d_epp_*` names are read: a router older
than v0.9.0, which exports only `inference_extension_scheduler_attempts_total`
(with `pod_name`) and `inference_pool_per_pod_queue_size`, records no
per-Pod view, and the recording holds none. The planner reads none of
these yet.

## kube-state-metrics

Read with `--kube-state-metrics`. They say which Pods were Ready, which
Deployment and role each belongs to, and its GPUs. Labels: `namespace` and
`pod` unless the row says otherwise. Without them, give `--role-of` per variant
and `--gpus-per-replica`/`--accelerator`: every Pod then reads as Ready, and the
capture says so.

kube-state-metrics exports a Pod's or a node's labels only when its
`--metric-labels-allowlist` names them. The planner reads two:
`--metric-labels-allowlist=pods=[llm-d.ai/role],nodes=[nvidia.com/gpu.product]`.
Without the first, and with no `--role-of`, each variant's role is read from
what its engine does: decode computes no prompt tokens or moves KV through
NIXL, an aggregated (both) pod computes prompts and generates, and prefill
makes about 1 output token a request. One token a request is also what an
aggregated pod makes under one-token requests, so prefill is read only beside
a variant of the same model that reads or is labelled decode, or where the
pod's own KV moves through NIXL. The capture names the evidence per Pod, and a
variant whose engine reads no role reads as both. A label or a `--role-of`
wins, and the capture says when the engine reads another role. Without the
second, give `--accelerator`.

| Series | Type | Extra labels | Feeds | Need |
| --- | --- | --- | --- | --- |
| `kube_pod_info` | gauge | `node` | the Pods that existed; their node | optional |
| `kube_pod_owner` | gauge | `owner_kind`, `owner_name` | the ReplicaSet behind each Pod | optional |
| `kube_replicaset_owner` | gauge | `replicaset`, `owner_kind`, `owner_name` (no `pod`) | the Deployment: which variant a Pod is | optional |
| `kube_pod_labels` | gauge | `label_llm_d_ai_role` | prefill or decode; needs kube-state-metrics' `--metric-labels-allowlist=pods=[llm-d.ai/role]` | optional |
| `kube_pod_status_phase` | gauge | `phase="Running"` | Pod running | optional |
| `kube_pod_status_ready` | gauge | `condition="true"` | Pod Ready | optional |
| `kube_pod_status_ready_time` | gauge | | when it became Ready | optional |
| `kube_pod_created` | gauge | | start time | optional |
| `kube_pod_start_time` | gauge | | start time | optional |
| `kube_pod_deletion_timestamp` | gauge | | a Pod being deleted, dropped | optional |
| `kube_pod_container_resource_requests` | gauge | `resource="nvidia_com_gpu"` | GPUs per replica | optional |
| `kube_node_labels` | gauge | `node`, `label_nvidia_com_gpu_product` (no `namespace`, no `pod`) | the accelerator; needs kube-state-metrics' `--metric-labels-allowlist=nodes=[nvidia.com/gpu.product]` | optional |
| `kube_deployment_spec_replicas` | gauge | `deployment` (no `pod`) | desired replicas; variants parked at zero | optional |
| `kube_pod_container_info` | gauge | `container`, `image_spec`, `image` | the vLLM release a `vllm-openai` image tag names | optional |
| `kube_deployment_annotations` | gauge | `deployment`, `annotation_scaling_manager_llmd_ai_vllm_version` (no `pod`) | the vLLM release the annotation states; needs kube-state-metrics' `--metric-annotations-allowlist=deployments=[scaling-manager.llmd.ai/vllm-version]` | optional |

## Cache tiers, offloading and KV transfer

Collect these too when prompts can hit a cache outside the GPU: CPU or disk
offloading, LMCache, a KV connector, or KV pulled from a peer (P/D). The
capture checks each one and prints whether it was present; an absent one never
fails a capture or a manual export. Older engines lack most of them.

Each row has one tier:

- **required today**: the planner reads it now (it is also in the tables
  above). Listed here for the cached-fleet reading it gives.
- **needed for TTFT on cached fleets**: says which tier served a prompt's
  cached tokens, and what loading them cost.
- **diagnostic**: explains an answer; not priced.

**Without the cached-fleet series** the planner still prices prefill from
computed tokens (`vllm:request_prefill_kv_computed_tokens`), which leave out
cached tokens wherever they came from. What it cannot do is say which tier
served them. So it cannot price TTFT when a tier's hit rate or load time
changes (more replicas, a smaller host cache), and it reads the time to load a
hit as compute time.

vLLM flags: `--kv-transfer-config` names the connector (`OffloadingConnector`,
`LMCacheConnectorV1` in-process, `LMCacheMPConnector` for LMCache's
multi-process server, `NixlConnector`, or a `MultiConnector` of them).
`--kv-offloading-size` turns on native CPU offloading with no
`--kv-transfer-config`. LMCache's multi-process server serves its own
`/metrics` on `--prometheus-port` (default 9090): your PodMonitor or
ServiceMonitor must scrape that port too. When that server runs in another
namespace than the engines, give `capture --lmcache-namespace`, or set
`LMCACHE_NAMESPACE` for the manual export. The in-process LMCache connector
exports `lmcache:*` on the engine's own `/metrics`.

| Series | Source and version | Produced with | Tier | Without it |
| --- | --- | --- | --- | --- |
| `vllm:prefix_cache_queries_total` | vLLM, all supported | always | required today | no GPU prefix-cache hit rate |
| `vllm:prefix_cache_hits_total` | vLLM, all supported | always | required today | no GPU prefix-cache hit rate |
| `vllm:request_prefill_kv_computed_tokens_sum` | vLLM | always | required today | no prefill capacity `V_P` |
| `vllm:cache_config_info` | vLLM | always; its labels are the cache config, `kv_offloading_size` and `kv_offloading_backend` among them in releases that have those flags | required today | no KV capacity |
| `vllm:num_preemptions_total` | vLLM | always | required today | preemption recompute not checked |
| `vllm:nixl_xfer_time_seconds_sum` | vLLM 0.11.1+ | `NixlConnector` | required today | no P/D transfer time |
| `vllm:nixl_xfer_time_seconds_count` | vLLM 0.11.1+ | `NixlConnector` | required today | no P/D transfer time |
| `vllm:external_prefix_cache_queries_total` | vLLM 0.11.1+ | any `--kv-transfer-config` connector | needed for TTFT on cached fleets | connector lookups unseen |
| `vllm:external_prefix_cache_hits_total` | vLLM 0.11.1+ | any `--kv-transfer-config` connector | needed for TTFT on cached fleets | connector hits read as GPU misses |
| `vllm:prompt_tokens_by_source_total` | vLLM 0.16.0+; `source` = `local_compute`, `local_cache_hit`, `external_kv_transfer` | always | needed for TTFT on cached fleets | no split of computed, GPU-cached and transferred tokens |
| `vllm:prompt_tokens_cached_total` | vLLM 0.16.0+ | always | needed for TTFT on cached fleets | cached tokens read from the split above, or not at all |
| `vllm:prompt_tokens_cached_by_source_total` | vLLM 0.32.0+; `source` = `device`, `host`, `disk`, `p2p`, `external_unspecified` | always; tiers other than `device` need a connector | needed for TTFT on cached fleets | no hit rate per tier |
| `vllm:kv_offload_load_bytes_total` | vLLM 0.24.0+ | `OffloadingConnector` or `--kv-offloading-size` | needed for TTFT on cached fleets | no offload load volume |
| `vllm:kv_offload_load_time_total` | vLLM 0.24.0+ | `OffloadingConnector` or `--kv-offloading-size` | needed for TTFT on cached fleets | an offload hit's load time read as compute |
| `vllm:kv_offload_store_bytes_total` | vLLM 0.24.0+ | `OffloadingConnector` or `--kv-offloading-size` | diagnostic | none |
| `vllm:kv_offload_store_time_total` | vLLM 0.24.0+ | `OffloadingConnector` or `--kv-offloading-size` | diagnostic | none |
| `vllm:kv_offload_total_bytes_total` | vLLM 0.16.0+, deprecated from 0.24.0; `transfer_type` = `CPU_to_GPU`, `GPU_to_CPU` | `OffloadingConnector` | needed for TTFT on cached fleets | before 0.24.0: no offload load volume |
| `vllm:kv_offload_total_time_total` | vLLM 0.16.0+, deprecated from 0.24.0; `transfer_type` | `OffloadingConnector` | needed for TTFT on cached fleets | before 0.24.0: no offload load time |
| `vllm:kv_offload_tiering_chunk_queries_total` | vLLM 0.28.0+; `tier` | `OffloadingConnector` with a tiering spec | needed for TTFT on cached fleets | no hit rate per secondary tier |
| `vllm:kv_offload_tiering_chunk_hits_total` | vLLM 0.28.0+; `tier` | `OffloadingConnector` with a tiering spec | needed for TTFT on cached fleets | no hit rate per secondary tier |
| `vllm:kv_offload_tiering_read_bytes_total` | vLLM 0.28.0+ | `OffloadingConnector` with a tiering spec | needed for TTFT on cached fleets | no secondary-tier read volume |
| `vllm:kv_offload_tiering_read_time_total` | vLLM 0.28.0+ | `OffloadingConnector` with a tiering spec | needed for TTFT on cached fleets | a disk or peer hit's read into the CPU tier left out of its load time |
| `vllm:kv_offload_config_info` | vLLM 0.33.0+ | `OffloadingConnector` | diagnostic | offload sizes from the engine arguments only |
| `vllm:kv_offload_tiering_promotion_latency_seconds_sum` | vLLM, recent builds | `OffloadingConnector` with a tiering spec | needed for TTFT on cached fleets | a peer-tier hit's load time unseen: the read time above stays empty for that tier |
| `vllm:kv_offload_tiering_promotion_latency_seconds_count` | vLLM, recent builds | as above | needed for TTFT on cached fleets | as above |
| `vllm:kv_offload_tiering_promotion_latency_seconds_bucket` | vLLM, recent builds | as above | diagnostic | no spread of the peer-tier load time |
| `vllm:kv_offload_lookup_sync_delay_seconds_bucket` | vLLM, recent builds | `OffloadingConnector` | diagnostic | where the wait for cached KV goes, unseen |
| `vllm:kv_offload_lookup_sync_delay_seconds_sum` | vLLM, recent builds | `OffloadingConnector` | diagnostic | as above |
| `vllm:kv_offload_lookup_sync_delay_seconds_count` | vLLM, recent builds | `OffloadingConnector` | diagnostic | as above |
| `vllm:kv_offload_lookup_async_delay_seconds_bucket` | vLLM, recent builds | `OffloadingConnector` | diagnostic | as above |
| `vllm:kv_offload_lookup_async_delay_seconds_sum` | vLLM, recent builds | `OffloadingConnector` | diagnostic | as above |
| `vllm:kv_offload_lookup_async_delay_seconds_count` | vLLM, recent builds | `OffloadingConnector` | diagnostic | as above |
| `vllm:kv_offload_tiering_lookup_sync_delay_seconds_bucket` | vLLM, recent builds | `OffloadingConnector` with a tiering spec | diagnostic | as above |
| `vllm:kv_offload_tiering_lookup_sync_delay_seconds_sum` | vLLM, recent builds | as above | diagnostic | as above |
| `vllm:kv_offload_tiering_lookup_sync_delay_seconds_count` | vLLM, recent builds | as above | diagnostic | as above |
| `vllm:kv_offload_tiering_lookup_async_delay_seconds_bucket` | vLLM, recent builds | as above | diagnostic | as above |
| `vllm:kv_offload_tiering_lookup_async_delay_seconds_sum` | vLLM, recent builds | as above | diagnostic | as above |
| `vllm:kv_offload_tiering_lookup_async_delay_seconds_count` | vLLM, recent builds | as above | diagnostic | as above |
| `vllm:kv_offload_load_size_bucket` | vLLM, recent builds | `OffloadingConnector` | diagnostic | no spread of the size of each load |
| `vllm:kv_block_lifetime_seconds_bucket` | vLLM | `--kv-cache-metrics` | diagnostic | how long a cached prefix lives in the GPU cache, unseen |
| `vllm:kv_block_lifetime_seconds_sum` | vLLM | `--kv-cache-metrics` | diagnostic | as above |
| `vllm:kv_block_lifetime_seconds_count` | vLLM | `--kv-cache-metrics` | diagnostic | as above |
| `vllm:kv_block_idle_before_evict_seconds_bucket` | vLLM | `--kv-cache-metrics` | needed for TTFT on cached fleets | the prefix hit at another prefill count falls back to the floor and the held hit |
| `vllm:kv_block_idle_before_evict_seconds_sum` | vLLM | `--kv-cache-metrics` | diagnostic | the mean idle time before an eviction, unseen |
| `vllm:kv_block_idle_before_evict_seconds_count` | vLLM | `--kv-cache-metrics` | diagnostic | as above |
| `vllm:kv_block_reuse_gap_seconds_bucket` | vLLM | `--kv-cache-metrics` | needed for TTFT on cached fleets | as above |
| `vllm:kv_block_reuse_gap_seconds_sum` | vLLM | `--kv-cache-metrics` | diagnostic | the mean reuse gap, unseen |
| `vllm:kv_block_reuse_gap_seconds_count` | vLLM | `--kv-cache-metrics` | diagnostic | as above |
| `vllm:hisparse_` | vLLM, builds with a decode host cache; every series whose name starts so, checked and saved as one | the host cache on | diagnostic | decode host-cache traffic unseen |
| `vllm:nixl_bytes_transferred_sum` | vLLM 0.11.1+ | `NixlConnector` | needed for TTFT on cached fleets | no P/D transfer volume, so no time per byte |
| `vllm:nixl_bytes_transferred_count` | vLLM 0.11.1+ | `NixlConnector` | needed for TTFT on cached fleets | as above |
| `vllm:nixl_xfer_time_seconds_bucket` | vLLM 0.11.1+ | `NixlConnector` | diagnostic | no P/D transfer-time spread |
| `vllm:nixl_post_time_seconds_sum` | vLLM 0.11.1+ | `NixlConnector` | diagnostic | none |
| `vllm:nixl_post_time_seconds_count` | vLLM 0.11.1+ | `NixlConnector` | diagnostic | none |
| `vllm:nixl_num_failed_transfers_total` | vLLM 0.11.1+ | `NixlConnector` | diagnostic | failed transfers unseen |
| `vllm:nixl_num_kv_expired_reqs_total` | vLLM, recent builds | `NixlConnector` | diagnostic | KV that expired before decode pulled it, unseen |
| `vllm:nixl_num_failed_notifications_total` | vLLM, recent builds | `NixlConnector` | diagnostic | lost transfer notifications unseen |
| `vllm:spec_decode_num_accepted_tokens_per_pos_total` | vLLM, v1 engine | speculative decoding (`--speculative-config`: MTP, EAGLE) | diagnostic | the TPOT spread assumes the acceptance at each draft position from the mean accepted tokens a step |
| `vllm:spec_decode_num_drafts_total` | vLLM, v1 engine | speculative decoding | diagnostic | as above |
| `vllm:spec_decode_num_draft_tokens_total` | vLLM, v1 engine | speculative decoding | diagnostic | the draft length from the engine arguments only |
| `lmcache_mp_lookup_requested_tokens_total` | LMCache 0.5.6+ multi-process server (checked to 0.5.8rc3) | the server's `--prometheus-port` scraped | needed for TTFT on cached fleets | no LMCache hit rate |
| `lmcache_mp_lookup_hit_tokens_total` | LMCache 0.5.6+ multi-process server | the server's `--prometheus-port` scraped | needed for TTFT on cached fleets | no LMCache hit rate |
| `lmcache_mp_lookup_hit_l1_tokens_total` | LMCache 0.5.6+ multi-process server | the server's `--prometheus-port` scraped | needed for TTFT on cached fleets | no split of L1 (memory) and L2 (storage) hits |
| `lmcache_mp_lookup_hit_l2_tokens_total` | LMCache 0.5.6+ multi-process server | the server's `--prometheus-port` scraped | needed for TTFT on cached fleets | as above |
| `lmcache_mp_lookup_prompt_tokens_total` | LMCache 0.5.7+ multi-process server | the server's `--prometheus-port` scraped | diagnostic | none |
| `lmcache_mp_lookup_request_hit_tokens_total` | LMCache 0.5.7+ multi-process server | the server's `--prometheus-port` scraped | diagnostic | none |
| `lmcache:num_requested_tokens_total` | LMCache in-process connector (checked 0.3.9 to 0.5.7) | `LMCacheConnectorV1` | needed for TTFT on cached fleets | no LMCache hit rate |
| `lmcache:num_hit_tokens_total` | LMCache in-process connector | `LMCacheConnectorV1` | needed for TTFT on cached fleets | no LMCache hit rate |
| `lmcache:num_lookup_tokens_total` | LMCache in-process connector | `LMCacheConnectorV1` | diagnostic | none |
| `lmcache:num_lookup_hits_total` | LMCache in-process connector | `LMCacheConnectorV1` | diagnostic | none |
| `lmcache:time_to_retrieve_sum` | LMCache in-process connector | `LMCacheConnectorV1` | diagnostic | none |
| `lmcache:time_to_retrieve_count` | LMCache in-process connector | `LMCacheConnectorV1` | diagnostic | none |
| `llm_d_epp_request_cached_tokens_sum` | llm-d router v1.0.0 | the engine reports cached tokens in its response (vLLM: `--enable-prompt-tokens-details`) | needed for TTFT on cached fleets | no fleet-wide cached tokens per request |
| `llm_d_epp_request_cached_tokens_count` | llm-d router v1.0.0 | as above | needed for TTFT on cached fleets | as above |
| `llm_d_epp_request_cached_tokens_bucket` | llm-d router v1.0.0 | as above | needed for TTFT on cached fleets | no spread of cached tokens per request |
| `llm_d_epp_prefix_predicted_cached_tokens_sum` | llm-d router v1.0.0 | an `approx-prefix-cache-producer` or `precise-prefix-cache-producer` plugin | needed for TTFT on cached fleets | no prefix duplication across prefill pods: `--prefill-hit-at-count` takes none |
| `llm_d_epp_prefix_predicted_cached_tokens_count` | llm-d router v1.0.0 | as above | diagnostic | none |
| `llm_d_epp_prefix_best_predicted_cached_tokens_sum` | llm-d router v1.0.0 | as above | diagnostic | none |
| `llm_d_epp_prefix_best_available_cached_tokens_sum` | llm-d router v1.0.0 | as above | needed for TTFT on cached fleets | as above |
| `llm_d_epp_prefix_prompt_tokens_sum` | llm-d router v1.0.0 | as above | diagnostic | none |
| `llm_d_epp_prefix_best_predicted_cached_tokens_count` | llm-d router | as above | diagnostic | how much cache affinity the routing gives up under load, unseen |
| `llm_d_epp_prefix_best_available_cached_tokens_count` | llm-d router | as above | diagnostic | as above |
| `llm_d_epp_prefix_prompt_tokens_count` | llm-d router | as above | diagnostic | as above |
| `llm_d_epp_prefix_predicted_cached_tokens_bucket` | llm-d router | as above | diagnostic | no spread of the predicted cached tokens |
| `llm_d_epp_prefix_best_available_cached_tokens_bucket` | llm-d router | as above | diagnostic | no spread of the best cached tokens on offer |
| `llm_d_epp_prefix_cache_affinity_filter_decisions_total` | llm-d router | a prefix-cache affinity filter | diagnostic | how often the filter kept or dropped cache affinity, unseen |
| `llm_d_epp_prefix_indexer_size` | llm-d router | a prefix indexer; may read empty with other plugins | diagnostic | none |
| `llm_d_epp_prefix_indexer_hit_ratio_bucket` | llm-d router | as above | diagnostic | none |
| `llm_d_epp_prefix_indexer_hit_ratio_sum` | llm-d router | as above | diagnostic | none |
| `llm_d_epp_prefix_indexer_hit_ratio_count` | llm-d router | as above | diagnostic | none |
| `sglang:cached_tokens_total` | SGLang; `cache_source` label 0.5.9+ | `--enable-metrics` | required today | no prefix-cache hit rate |
| `sglang:prompt_tokens_total` | SGLang | `--enable-metrics` | required today | no prefix-cache hit rate |
| `sglang:prefill_effective_tokens_total` | SGLang 0.5.18+; `mode` = `device_hit`, `host_hit`, `storage_hit`, `input` | `--enable-metrics`; host and storage hits need `--enable-hierarchical-cache` | needed for TTFT on cached fleets | no hit rate per tier |
| `sglang:uncached_prompt_tokens_histogram_sum` | SGLang 0.5.12+ | `--enable-metrics` | needed for TTFT on cached fleets | no computed prompt tokens per request |
| `sglang:uncached_prompt_tokens_histogram_count` | SGLang 0.5.12+ | `--enable-metrics` | needed for TTFT on cached fleets | as above |
| `sglang:uncached_prompt_tokens_histogram_bucket` | SGLang 0.5.12+ | `--enable-metrics` | diagnostic | no spread of computed prompt lengths |

The versions are where each name first appears in the upstream source; each
was checked against vLLM v0.34.0, SGLang v0.5.23, LMCache v0.5.8rc3 and llm-d
router v1.0.0. Rows that give no version ("recent builds", "llm-d router")
were added later and are not tied to a first release: an engine or router
that lacks one saves an empty answer.

## TTFT outside the engines

Collect these to explain the part of the router's TTFT that the engines'
own TTFT does not cover: the router's processing and plugins, prompt
rendering, the P/D sidecar, and the engine's HTTP front end. The planner
does not price them, except under an experimental mode the row names; each row is **diagnostic: TTFT outside the engines**. The capture checks
each one and prints whether it was present, like the series above.

| Series | Source and version | Produced with | Tier | Without it |
| --- | --- | --- | --- | --- |
| `llm_d_epp_request_processing_duration_seconds_sum` | llm-d router v1.0.0; no model labels | on by default | diagnostic: TTFT outside the engines | the router's own time before it picks an endpoint unseen |
| `llm_d_epp_request_processing_duration_seconds_count` | llm-d router v1.0.0 | on by default | diagnostic: TTFT outside the engines | as above |
| `llm_d_epp_request_processing_duration_seconds_bucket` | llm-d router v1.0.0 | on by default | diagnostic: TTFT outside the engines | no spread of the router's own time: `--rank-mixture-remainder-spread` draws the remainder as its line |
| `llm_d_epp_token_producer_render_duration_seconds_sum` | llm-d router v1.0.0; `plugin_type`, `plugin_name`, `backend`, `result` | on by default; observed only when a token-producer plugin renders prompts | diagnostic: TTFT outside the engines | time spent rendering prompts for the prefix scorer unseen; `--rank-mixture-remainder-server` cannot take the render call out of the router's own time |
| `llm_d_epp_token_producer_render_duration_seconds_count` | llm-d router v1.0.0 | as above | diagnostic: TTFT outside the engines | as above |
| `llm_d_epp_plugin_duration_seconds_sum` | llm-d router v1.0.0; `extension_point`, `plugin_type`, `plugin_name` | on by default | diagnostic: TTFT outside the engines | no split of the router's time by plugin |
| `llm_d_epp_plugin_duration_seconds_count` | llm-d router v1.0.0 | on by default | diagnostic: TTFT outside the engines | as above |
| `llm_d_epp_request_size_bytes_sum` | llm-d router v1.0.0 | on by default | diagnostic: TTFT outside the engines | no request size, so no check of body-parsing cost |
| `llm_d_epp_request_size_bytes_count` | llm-d router v1.0.0 | on by default | diagnostic: TTFT outside the engines | as above |
| `llm_d_epp_request_duration_seconds_sum` | llm-d router v1.0.0 | on by default | diagnostic: TTFT outside the engines | no end-to-end latency at the router to set the engine's against |
| `llm_d_epp_request_duration_seconds_count` | llm-d router v1.0.0 | on by default | diagnostic: TTFT outside the engines | as above |
| `llm_d_disagg_sidecar_prefill_duration_seconds_sum` | llm-d router v1.0.0 P/D sidecar; no model labels | off by default: start the sidecar with `--metrics-port` and scrape that port | diagnostic: TTFT outside the engines | the P/D prefill leg, as decode's sidecar sees it, unseen |
| `llm_d_disagg_sidecar_prefill_duration_seconds_count` | llm-d router v1.0.0 P/D sidecar | as above | diagnostic: TTFT outside the engines | as above |
| `vllm:e2e_request_latency_seconds_sum` | vLLM, all supported | on by default | diagnostic: TTFT outside the engines | no engine end-to-end latency to set the router's against; with the HTTP handler's, no API-server time per Pod, so `--rank-mixture-remainder-server` draws the remainder as the plain spread |
| `vllm:e2e_request_latency_seconds_count` | vLLM, all supported | on by default | diagnostic: TTFT outside the engines | as above |
| `vllm:request_render_duration_seconds_sum` | vLLM 0.34.0+; `request_type` | on by default | diagnostic: TTFT outside the engines | chat-template and tokenization time read as queue or prefill |
| `vllm:request_render_duration_seconds_count` | vLLM 0.34.0+ | on by default | diagnostic: TTFT outside the engines | as above |
| `http_request_duration_seconds_sum` | vLLM's API server (prometheus-fastapi-instrumentator); `handler`, `method` | on by default; every handler is saved and checked, by `handler` | diagnostic: TTFT outside the engines | the engine's HTTP front end, before the scheduler, unseen: `--rank-mixture-remainder-server` draws the remainder as the plain spread |
| `http_request_duration_seconds_count` | vLLM's API server | as above | diagnostic: TTFT outside the engines | as above |
| `llm_d_epp_scheduler_e2e_duration_seconds_bucket` | llm-d router | on by default | diagnostic: TTFT outside the engines | no spread of the router's scheduling time |
| `llm_d_epp_scheduler_e2e_duration_seconds_sum` | llm-d router | on by default | diagnostic: TTFT outside the engines | the router's scheduling time unseen |
| `llm_d_epp_scheduler_e2e_duration_seconds_count` | llm-d router | on by default | diagnostic: TTFT outside the engines | as above |
| `llm_d_epp_response_processing_duration_seconds_sum` | llm-d router | on by default | diagnostic: TTFT outside the engines | the router's time on the return path unseen |
| `llm_d_epp_response_processing_duration_seconds_count` | llm-d router | on by default | diagnostic: TTFT outside the engines | as above |
| `llm_d_epp_flow_control_request_queue_duration_seconds_sum` | llm-d router; `outcome` | flow control on | diagnostic: TTFT outside the engines | the mean wait in the router's queue, read from its buckets only |
| `llm_d_epp_flow_control_request_queue_duration_seconds_count` | llm-d router | flow control on | diagnostic: TTFT outside the engines | as above |

### Optional: tracing and access logs

Traces, and the gateway's (Envoy's) access-log fields such as upstream
service time, are logs, not metrics: neither path collects them. If you
keep them, a sample from the same window helps place the remainder these
series leave; send it beside the export.

## Prefill capacity, load and health

Collect these to read prefill capacity at light load, without waiting for
queues to form, and to see what the fleet did during the window: how
requests ended, what the router saw, restarts. The planner does not price
them yet. The capture checks each one and prints whether it was present,
like the series above.

The tiers:

- **needed for prefill capacity**: the tokens each engine step takes, and
  the spread of per-request prefill time.
- **diagnostic**: explains an answer; not priced.

DCGM series carry the exporter's namespace in `namespace`, and the GPU's
Pod in `exported_namespace` and `exported_pod`. The capture selects them by
`exported_namespace`; the manual export by `DCGM_SELECTOR`, which you can
set to select by `exported_pod` instead. That holds when Prometheus does not
honor the exporter's labels (`honorLabels: false`, the default of the
exporter's and the GPU Operator's ServiceMonitor). With `honorLabels: true`
the GPU's Pod namespace stays in `namespace`: the capture then reads the
DCGM series as absent, and the manual export needs
`DCGM_SELECTOR="namespace='<NAMESPACE>'"`.

Every DCGM series needs the exporter's Kubernetes Pod mapping, which puts
the Pod's labels on it. The exporter renamed its fields in 4.9.0, with
units, so each is listed under both names; a fleet exports one set.
`DCGM_FI_PROF_SM_ACTIVE` and `DCGM_FI_PROF_SM_UTIL_RATIO` are in none of
the counter files the exporter ships: add them to a custom one.

| Series | Source and version | Produced with | Tier | Without it |
| --- | --- | --- | --- | --- |
| `vllm:iteration_tokens_total_bucket` | vLLM, all supported | on by default | needed for prefill capacity | prefill capacity read only from minutes with a queue |
| `vllm:iteration_tokens_total_sum` | vLLM, all supported | on by default | needed for prefill capacity | as above |
| `vllm:iteration_tokens_total_count` | vLLM, all supported | on by default | needed for prefill capacity | as above |
| `vllm:request_prefill_time_seconds_bucket` | vLLM, all supported | on by default | needed for prefill capacity | the mean prefill time only, not its spread |
| `vllm:estimated_flops_per_gpu_total` | vLLM, recent builds | `--enable-mfu-metrics` | diagnostic | spare prefill compute not read |
| `vllm:estimated_read_bytes_per_gpu_total` | vLLM, recent builds | `--enable-mfu-metrics` | diagnostic | as above |
| `vllm:estimated_write_bytes_per_gpu_total` | vLLM, recent builds | `--enable-mfu-metrics` | diagnostic | as above |
| `DCGM_FI_DEV_GPU_UTIL` | NVIDIA DCGM exporter, before 4.9.0 | on by default | diagnostic | how busy the GPUs are at light load, unseen |
| `DCGM_FI_PROF_SM_ACTIVE` | NVIDIA DCGM exporter, before 4.9.0 | a custom counters file | diagnostic | as above |
| `DCGM_FI_PROF_PIPE_TENSOR_ACTIVE` | NVIDIA DCGM exporter, before 4.9.0 | on by default, on GPUs with profiling fields | diagnostic | as above |
| `DCGM_FI_PROF_DRAM_ACTIVE` | NVIDIA DCGM exporter, before 4.9.0 | on by default, on GPUs with profiling fields | diagnostic | as above |
| `DCGM_FI_DEV_POWER_USAGE` | NVIDIA DCGM exporter, before 4.9.0 | on by default | diagnostic | as above |
| `DCGM_FI_DEV_SM_CLOCK` | NVIDIA DCGM exporter, before 4.9.0 | on by default | diagnostic | as above |
| `DCGM_FI_DEV_GPU_UTIL_RATIO` | NVIDIA DCGM exporter, 4.9.0 and later | on by default | diagnostic | as above |
| `DCGM_FI_PROF_SM_UTIL_RATIO` | NVIDIA DCGM exporter, 4.9.0 and later | a custom counters file | diagnostic | as above |
| `DCGM_FI_PROF_TENSOR_UTIL_RATIO` | NVIDIA DCGM exporter, 4.9.0 and later | on by default, on GPUs with profiling fields | diagnostic | as above |
| `DCGM_FI_PROF_DRAM_UTIL_RATIO` | NVIDIA DCGM exporter, 4.9.0 and later | on by default, on GPUs with profiling fields | diagnostic | as above |
| `DCGM_FI_DEV_BOARD_POWER_WATTS` | NVIDIA DCGM exporter, 4.9.0 and later | on by default | diagnostic | as above |
| `DCGM_FI_DEV_SM_CLOCK_HERTZ` | NVIDIA DCGM exporter, 4.9.0 and later | on by default | diagnostic | as above |
| `vllm:request_time_per_output_token_seconds_sum` | vLLM, all supported | on by default | diagnostic | the engine's mean per-token latency read from its buckets only |
| `vllm:request_time_per_output_token_seconds_count` | vLLM, all supported | on by default | diagnostic | as above |
| `vllm:request_decode_time_seconds_bucket` | vLLM, all supported | on by default | diagnostic | no decode time per request |
| `vllm:request_decode_time_seconds_sum` | vLLM, all supported | on by default | diagnostic | as above |
| `vllm:request_decode_time_seconds_count` | vLLM, all supported | on by default | diagnostic | as above |
| `vllm:request_success_total` | vLLM, all supported; `finished_reason` | on by default | diagnostic | how requests ended, unseen |
| `llm_d_epp_info` | llm-d router | on by default | diagnostic | the router build that ran, unrecorded |
| `llm_d_epp_ready_endpoints` | llm-d router | on by default | diagnostic | the pods the router could pick, unseen |
| `llm_d_epp_inflight_requests` | llm-d router | on by default | diagnostic | the load the router saw, unseen |
| `llm_d_epp_inflight_tokens` | llm-d router | on by default | diagnostic | as above |
| `llm_d_epp_request_total` | llm-d router | on by default | diagnostic | requests and errors at the router, unseen |
| `llm_d_epp_request_error_total` | llm-d router | on by default | diagnostic | as above |
| `llm_d_epp_request_input_tokens_bucket` | llm-d router | on by default | diagnostic | request sizes at the router, unseen |
| `llm_d_epp_request_input_tokens_sum` | llm-d router | on by default | diagnostic | as above |
| `llm_d_epp_request_input_tokens_count` | llm-d router | on by default | diagnostic | as above |
| `llm_d_epp_request_output_tokens_bucket` | llm-d router | on by default | diagnostic | as above |
| `llm_d_epp_request_output_tokens_sum` | llm-d router | on by default | diagnostic | as above |
| `llm_d_epp_request_output_tokens_count` | llm-d router | on by default | diagnostic | as above |
| `llm_d_epp_flow_control_dispatch_cycle_duration_seconds_bucket` | llm-d router | flow control on | diagnostic | router admission detail unseen |
| `llm_d_epp_flow_control_dispatch_cycle_duration_seconds_sum` | llm-d router | flow control on | diagnostic | as above |
| `llm_d_epp_flow_control_dispatch_cycle_duration_seconds_count` | llm-d router | flow control on | diagnostic | as above |
| `llm_d_epp_flow_control_detector_saturation` | llm-d router | flow control on | diagnostic | as above |
| `llm_d_epp_flow_control_capacity_utilization_requests` | llm-d router | flow control on | diagnostic | as above |
| `llm_d_epp_flow_control_capacity_utilization_bytes` | llm-d router | flow control on | diagnostic | as above |
| `kube_pod_container_status_restarts_total` | kube-state-metrics; `container` | on by default | diagnostic | restarts in the window unseen |
| `kube_pod_container_status_last_terminated_reason` | kube-state-metrics; `reason` | on by default | diagnostic | out-of-memory kills unseen |

## Counts

| Source | Series | required | TPOT | TTFT | optional |
| --- | --- | --- | --- | --- | --- |
| vLLM | 46 | 1 | 8 | 4 | 33 |
| SGLang | 23 | 1 | 8 | 0 | 14 |
| Router (EPP) | 22 | 0 | 1 | 2 | 19 |
| kube-state-metrics | 15 | 0 | 0 | 0 | 15 |
| All | 106 | 2 | 17 | 6 | 81 |

A capture reads one engine's series, vLLM's or SGLang's, never both.
