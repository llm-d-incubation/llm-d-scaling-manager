# Metrics the capacity planner needs

The [capacity planner](../proposals/capacity-planner.md) answers from a window
of your Prometheus: what the model servers, the router and kube-state-metrics
reported while the fleet ran. This page lists every series it reads, so you can
check your Prometheus keeps them, or save them before they age out.

To record a window, follow
[Record a window for the capacity planner](../guides/record-for-capacity-planner/).

## What your Prometheus must keep

- **Scrape every 15 s,** 30 s at most. The planner reads one-minute rates and
  the least queue in each minute; at 60 s a minute holds one sample and both
  read nothing.
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

The [developer page](../developer-guide/whatif-capture.md#where-each-input-comes-from)
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
| `vllm:num_requests_running` | gauge | | running requests: the batch cap, where no flag gives it | optional |
| `vllm:num_requests_waiting` | gauge | | queue length, and the least queue in a minute: prefill busy minutes (`V_P`) | TTFT |
| `vllm:num_requests_waiting_by_reason` | gauge | `reason="capacity"` | requests waiting for KV space (vLLM 0.20 and later) | optional |
| `vllm:request_prompt_tokens_sum` | histogram sum | | mean prompt length: KV per request | TPOT |
| `vllm:request_prompt_tokens_count` | histogram count | | mean prompt length | TPOT |
| `vllm:request_prompt_tokens_bucket` | histogram buckets | `le` | prompt lengths, recorded | optional |
| `vllm:request_generation_tokens_sum` | histogram sum | | mean output length: KV per request, the rate per replica | TPOT |
| `vllm:request_generation_tokens_count` | histogram count | | mean output length; request rate | TPOT |
| `vllm:request_generation_tokens_bucket` | histogram buckets | `le` | output-length spread `D`: KV per request | optional |
| `vllm:prefix_cache_hits_total` | counter | | prefix-cache hit rate: the prompt tokens a request computes | optional |
| `vllm:prefix_cache_queries_total` | counter | | prefix-cache hit rate | optional |
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
| `vllm:nixl_xfer_time_seconds_sum` | histogram sum | | KV transfer time between prefill and decode | optional |
| `vllm:nixl_xfer_time_seconds_count` | histogram count | | KV transfer time | optional |
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
| `llm_d_epp_scheduler_attempts_total` | counter | `status="success"` (no `model_name`) | arrival rate, with flow control off | optional |
| `inference_extension_scheduler_attempts_total` | counter | `status="success"` (no `model_name`) | as above, older routers | optional |
| `llm_d_epp_flow_control_request_queue_duration_seconds_bucket` | histogram buckets | `le`, `outcome="Dispatched"` | flow-control wait | optional |
| `llm_d_epp_flow_control_pool_saturation` | gauge | `inference_pool`, `stage` (no model labels) | pool saturation | optional |
| `llm_d_epp_flow_control_requests_total` | counter | `inference_pool`, `outcome` (no model labels) | flow-control outcomes | optional |

## kube-state-metrics

Read with `--kube-state-metrics`. They say which Pods were Ready, which
Deployment and role each belongs to, and its GPUs. Labels: `namespace` and
`pod` unless the row says otherwise. Without them, give `--role-of` per variant
and `--gpus-per-replica`/`--accelerator`: every Pod then reads as Ready, and the
capture says so.

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
| `kube_node_labels` | gauge | `node`, `label_nvidia_com_gpu_product` (no `namespace`, no `pod`) | the accelerator | optional |
| `kube_deployment_spec_replicas` | gauge | `deployment` (no `pod`) | desired replicas; variants parked at zero | optional |

## Counts

| Source | Series | required | TPOT | TTFT | optional |
| --- | --- | --- | --- | --- | --- |
| vLLM | 33 | 1 | 8 | 4 | 20 |
| SGLang | 23 | 1 | 8 | 0 | 14 |
| Router (EPP) | 17 | 0 | 1 | 2 | 14 |
| kube-state-metrics | 13 | 0 | 0 | 0 | 13 |
| All | 86 | 2 | 17 | 6 | 61 |

A capture reads one engine's series, vLLM's or SGLang's, never both.
