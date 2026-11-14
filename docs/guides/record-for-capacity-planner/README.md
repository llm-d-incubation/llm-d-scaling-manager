# Record a window for the capacity planner

## Overview

The [capacity planner](../../proposals/capacity-planner.md) answers two
questions about a model: how many replicas a latency target needs, and what
rate a fleet can serve. It learns the answers from a past window of your
Prometheus: what the model servers, the router and kube-state-metrics reported
while the fleet ran.

This guide saves such a window. There are two ways:

1. **With `capacity-planner`.** One command reads the window from Prometheus and writes
   a recording. Use it when you can build the tool and reach Prometheus.
2. **With only curl and jq.** Save the raw series to files and send them.
   Anyone with this repository then loads them into a local Prometheus and
   runs step 1 there. Use it when you cannot run `capacity-planner` where Prometheus is.

[Which series, and what each one feeds](../../reference/capacity-planner-metrics.md)
is the reference for both.

## Prerequisites

- [ ] Your Prometheus scrapes the model servers every 15 s (30 s at most),
  keeps their histograms, and still holds the window
  ([what it must keep](../../reference/capacity-planner-metrics.md#what-your-prometheus-must-keep)).
- [ ] A window of 30 minutes or more in which load rose. A flat, quiet hour
  teaches the planner little.
- [ ] `kubectl` access to Prometheus' namespace (a port-forward), or its URL.
- [ ] For Path 1: Go and a checkout of the release you run.
- [ ] For Path 2: bash, `jq`, and `curl` 7.76 or later (for `--fail-with-body`;
  `curl --version` says which), plus `kubectl get` on the model Deployments.
- [ ] kube-state-metrics in the same Prometheus, for readiness and roles,
  with `--metric-labels-allowlist=pods=[llm-d.ai/role],nodes=[nvidia.com/gpu.product]`
  so it exports each Pod's role and each node's accelerator.
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
# From a checkout of the release you run. Needs Go. There is no released capacity-planner binary or image. On v0.11.x and earlier the target is make whatif and the binary bin/whatif.
make capacity-planner
bin/capacity-planner capture -h 2>&1 | head -3

# The namespace your model servers run in, and the window to answer from (UTC).
export NAMESPACE=<model namespace>
export FROM=2026-11-01T12:00:00Z
export TO=2026-11-01T13:00:00Z

# The engine arguments the Deployment ran with. kube-state-metrics does not export them.
kubectl -n ${NAMESPACE} get deploy <decode deployment> \
  -o jsonpath='{.spec.template.spec.containers[0].args}{"\n"}'
export ENGINE_ARGS="<those arguments, space-separated>"

# Replays the window through the controller's own queries. Exit 0 done, 2 your input (a flag, nothing found), 1 a failed query.
bin/capacity-planner capture --prometheus ${PROM_URL} --namespace ${NAMESPACE} --kube-state-metrics \
  --from ${FROM} --to ${TO} --engine vllm --engine-args "${ENGINE_ARGS}" \
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
wrote run.capture.jsonl.gz
generated 57 steps into run.jsonl.gz: 72688 bytes, analyzes map[saturation.Analyze:57]
  caveat: engine parameters from flags, which may differ from the window's workloads
  caveat: rollouts not seen: every scale target filed as not rolling out
```

A `flag:` or `caveat:` line names something the window could not show, such
as readiness without kube-state-metrics. Read them before the answers.

Exit codes: `0` done; `2` your input (a bad flag, no replica with a KV-cache
series in the namespace and window, a window too large without `--force`);
`1` anything else, usually a failed Prometheus query, which the message names.
The answer commands add `3`: a role refused, with the reason printed.

`replicas-to-slo` and `slo-to-rate` also take the capture flags in place of
`--recording`, and answer from the window without writing a file.

**Go easy on Prometheus.** A capture asks one range query per planner query
over the whole window: about 56 for one vLLM model. An hour is light; a day
returns millions of samples, and the usual failure is Prometheus'
`query.max-samples`. Capture off-peak, or from a copy
([Load on Prometheus](../../developer-guide/capacity-planner-capture.md#load-on-prometheus)).

## Path 2: record with curl and jq

This saves the raw samples of every series the planner reads, nothing else,
for one namespace. It needs read access to Prometheus' query API, and to read
the model Deployments; no checkout of this repository, no Go.

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
export METRICS="
vllm:kv_cache_usage_perc vllm:cache_config_info vllm:inter_token_latency_seconds_sum
vllm:inter_token_latency_seconds_count vllm:generation_tokens_total vllm:num_requests_running
vllm:num_requests_waiting vllm:num_requests_waiting_by_reason vllm:num_requests_kv_fetch_by_stage
vllm:request_prompt_tokens_sum
vllm:request_prompt_tokens_count vllm:request_prompt_tokens_bucket vllm:request_generation_tokens_sum
vllm:request_generation_tokens_count vllm:request_generation_tokens_bucket vllm:prefix_cache_hits_total
vllm:prefix_cache_queries_total vllm:request_prefill_kv_computed_tokens_sum
vllm:request_prefill_kv_computed_tokens_bucket vllm:request_inference_time_seconds_sum
vllm:request_inference_time_seconds_count vllm:time_to_first_token_seconds_sum
vllm:time_to_first_token_seconds_count vllm:time_to_first_token_seconds_bucket
vllm:request_queue_time_seconds_sum vllm:request_queue_time_seconds_count
vllm:request_queue_time_seconds_bucket vllm:request_prefill_time_seconds_sum
vllm:request_prefill_time_seconds_count vllm:request_time_per_output_token_seconds_bucket
vllm:nixl_xfer_time_seconds_sum vllm:nixl_xfer_time_seconds_count vllm:num_preemptions_total
vllm:spec_decode_num_accepted_tokens_total
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
llm_d_epp_flow_control_requests_total
kube_pod_info kube_pod_owner kube_replicaset_owner kube_pod_labels kube_pod_status_phase
kube_pod_status_ready kube_pod_status_ready_time kube_pod_created kube_pod_start_time
kube_pod_deletion_timestamp kube_pod_container_resource_requests kube_node_labels
kube_deployment_spec_replicas
"
rm -rf planner-export
mkdir -p planner-export
failed=0
for m in ${METRICS}; do
  sel="${m}{namespace='${NAMESPACE}'}"
  [ "${m}" = kube_node_labels ] && sel="${m}"
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
# arguments of the decode and prefill Deployments (leave PREFILL_DEPLOYMENT empty without one).
# ENGINE is vllm or sglang; ENGINE_CONTAINER is the engine's container name.
# It stops, and writes no window.txt, when a Deployment or its container is not found.
export ENGINE=vllm
export ENGINE_CONTAINER=vllm
export DECODE_DEPLOYMENT=<decode deployment>
export PREFILL_DEPLOYMENT=
engine_args() {
  out=$(kubectl -n "${NAMESPACE}" get deploy "$1" -o json | jq -r --arg c "${ENGINE_CONTAINER}" '
    [.spec.template.spec.containers[] | select(.name == $c) | (.command // []) + (.args // []) | .[]
     | gsub("\n"; " ")
     | if test("^[[{]") then (try (fromjson | tojson) catch .) else . end]
    | join(" ")')
  [ -n "${out}" ] || { echo "no container ${ENGINE_CONTAINER} with a command or args in Deployment $1" >&2; return 1; }
  echo "${out}"
}
rm -f planner-export/window.txt
(
  [ -n "${DECODE_DEPLOYMENT}" ] || { echo "set DECODE_DEPLOYMENT" >&2; exit 1; }
  decode_args=$(engine_args "${DECODE_DEPLOYMENT}") || exit 1
  prefill_args=
  if [ -n "${PREFILL_DEPLOYMENT}" ]; then prefill_args=$(engine_args "${PREFILL_DEPLOYMENT}") || exit 1; fi
  {
    echo "NAMESPACE=${NAMESPACE}"
    echo "FROM=${FROM}"
    echo "TO=${TO}"
    echo "FROM_ISO=$(jq -rn --argjson t "${FROM}" '$t | todate')"
    echo "TO_ISO=$(jq -rn --argjson t "${TO}" '$t | todate')"
    echo "CAPTURE_FROM=$(( FROM + 300 ))"
    echo "ENGINE=${ENGINE}"
    echo "DECODE_ARGS=${decode_args}"
    echo "PREFILL_ARGS=${prefill_args}"
  } > planner-export/window.txt
) && cat planner-export/window.txt || echo "window.txt NOT written: fix the above, then run this step again"
if grep -E '^[A-Z]+_ARGS=.*([$]|sh -c)' planner-export/window.txt 2>/dev/null; then
  echo "EDIT planner-export/window.txt: the line above is not expanded (see the guide)"
fi

# Every chunk saved and a success, then how many series each metric had. It prints bad=0 when nothing is missing.
# Zero series is fine for a metric your fleet does not export.
want=$(( (TO - FROM + CHUNK - 1) / CHUNK ))
bad=0
for m in ${METRICS}; do
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

# One file to send. It holds your metric labels (Pod, node, model names) and engine arguments, no request content.
tar czf planner-export.tgz planner-export
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

Send `planner-export.tgz`. It holds metric labels (Pod, node and model names)
and `window.txt`: the window, the engine and the Deployments' engine
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
# It reads only the *.json answers. window.txt is read as data, never sourced, and checked.
tar xzf planner-export.tgz --no-same-owner
python3 hack/promjson-to-openmetrics.py -o planner-export.om planner-export/*.json
while IFS='=' read -r k v; do
  case "${k}" in NAMESPACE|FROM|TO|FROM_ISO|TO_ISO|CAPTURE_FROM|ENGINE|DECODE_ARGS|PREFILL_ARGS) export "${k}=${v}" ;; esac
done < planner-export/window.txt
case "${FROM}:${TO}:${CAPTURE_FROM}" in *[!0-9:]*|:*|*::*|*:) echo "STOP: FROM, TO and CAPTURE_FROM in window.txt must be Unix seconds" ;; esac
case "${ENGINE}" in vllm|sglang) ;; *) echo "STOP: ENGINE in window.txt must be vllm or sglang" ;; esac
echo "namespace ${NAMESPACE}, engine ${ENGINE}, decode arguments: ${DECODE_ARGS}"

# Writes TSDB blocks.
${PROMTOOL} tsdb create-blocks-from openmetrics planner-export.om planner-tsdb

# Serve the blocks. A retention longer than their age, or Prometheus deletes them on start.
# LOOKBACK is the one the converter printed, four scrape intervals: it ends a series about when the original did (see Limits).
export LOOKBACK=<the lookback the converter printed, e.g. 60s>
touch empty.yml
prometheus --config.file=empty.yml --storage.tsdb.path=planner-tsdb \
  --storage.tsdb.retention.time=10y --query.lookback-delta="${LOOKBACK}" \
  --web.listen-address=127.0.0.1:9091 &
until curl -sf http://127.0.0.1:9091/-/ready >/dev/null; do sleep 1; done

# Then capture as in the first path, against the local copy, from 5 minutes into the export.
# capacity-planner takes one set of engine arguments: the decode Deployment's. PREFILL_ARGS is there to read.
bin/capacity-planner capture --prometheus http://127.0.0.1:9091 --namespace "${NAMESPACE}" --kube-state-metrics \
  --from "${CAPTURE_FROM}" --to "${TO}" --engine "${ENGINE}" --engine-args "${DECODE_ARGS}" \
  -o run.capture.jsonl.gz
```
<!-- guide:load end -->

The converter refuses a file whose query failed, a chunk one metric has and
another lacks, native histograms (which it cannot write), and an export with no
sample at all, so a broken export is not read as a window with less load.

### Limits of a manual export

- **A series' end is blurred by up to the lookback.** Prometheus marks a series
  stale when its target goes away; the query API does not return those marks.
  The local copy instead holds the last value for `--query.lookback-delta`, so
  keep it short: four scrape intervals, which the converter prints from the
  samples (`60s` at 15 s, `120s` at 30 s). In a test of
  this path, the copy's capture matched the original's on every step but the
  three, 45 s, after a Pod left, where the copy still listed it; with the
  default `5m` lookback, 19 steps differed.
- **The window must start 5 minutes before the one you want answered.** Some
  inputs are 5-minute rates.

Where your Prometheus allows its admin API, a TSDB snapshot keeps everything,
staleness marks included:
[Capture a past window](../../developer-guide/testing.md#capture-a-past-window).

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

## See also

- [Metrics the capacity planner needs](../../reference/capacity-planner-metrics.md)
- [Capacity planner capture and generate](../../developer-guide/capacity-planner-capture.md): how capture works, its flags, its load
- [The capacity planner](../../proposals/capacity-planner.md), and its
  [CLI](../../proposals/slo-capacity-planner.md#cli-reference)
