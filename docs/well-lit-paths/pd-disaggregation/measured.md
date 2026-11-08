# Scaling a P/D model, measured

[← Scale a P/D-disaggregated model](README.md)

One scenario, a constant 6 req/s whose request shape changes halfway, run on
8 x H200 nodes with Qwen3-0.6B against the controller built at `f5f98761` on
2026-09-23: a cold pass and a warm pass, and the pair repeated a day later on
the same build.

## How it is benchmarked

**Scenario:** `shape_swap_replay_6k1000_1k4000_rps6_20m` in
`test/benchmark/scenarios/`. A GuideLLM trace replay at a constant 6 req/s:
6000 tokens in / 1000 out for 1100s, then 1000 in / 4000 out for 1100s. The
rate never changes, so request shape is the only variable. One H200 decode
replica of Qwen3-0.6B sustains ~5.4 req/s at the first shape and ~2.5 at the
second (from the pod's own completion counters). So the right answer is
**two decode replicas, then three**: a fleet the autoscaler has to hold up,
not one it can let fall to one. `hack/benchmark/gen_shape_trace.py` generates
the trace from `shape_6k1000_1k4000_rps6_20m.params.yaml`; seed 42 makes it
identical every run (13,288 requests: 6693 + 6595).

**Stack:** one pod per role to start, `kvCacheThreshold` 0.8,
`queueLengthThreshold` 5, `scaleUpThreshold` 0.85, `scaleDownBoundary` 0.7, a
15s optimize interval, KEDA min 1 / max 10 per role, and this HPA behaviour on
both ScaledObjects: scale-up 100%/5s with no stabilization, scale-down
50%/120s with a 180s window.

**Deployment:** `hack/benchmark/scenarios/guides/pd-disaggregation.yaml`,
adapted from llm-d's P/D guide, run through
[Benchmark the scaling manager](../../guides/benchmarking/). As written it
names Qwen3-32B with one prefill and two decode pods. The benchmark targets
substitute `MODEL_ID` (Qwen3-0.6B by default) and `BENCHMARK_DECODE_REPLICAS`
(1), so a standup starts from one pod per role, and the accelerator profile is
auto-detected.

Two pins in that file must move together. `patch_harness.sh` pins the
modelservice chart to v0.4.15, which renders the routing sidecar's flags as
`--vllm-port/--connector`. The scenario pins the sidecar to v0.9.0, the build
that takes those flags. Let either float and every decode pod crash-loops on
`unknown flag: --connector` at the next standup. It happened once, and the
file says so.

## Reproducing it

On a Kubernetes cluster with H200 (or similar) nodes, an RDMA resource the
scenario's `networkResource: auto` can find, and a Prometheus that scrapes
PodMonitors from your namespace:

```bash
export BENCHMARK_NAMESPACE=<ns>
export IMG=<the image under test>          # what is measured is what you set here
export KEDA_HELM_INSTALL=true              # only if the cluster has no KEDA

make benchmark-install
make benchmark-standup BENCHMARK_SPEC=guides/pd-disaggregation MODEL_ID=Qwen/Qwen3-0.6B
make prepull-status NAMESPACE=$BENCHMARK_NAMESPACE   # the standup held the engine image on the accelerator nodes; every node should read present

# The HPA behaviour the results below were measured under. deploy/ ships a
# 300s scale-down window with a 100% step; this is the tighter shape.
for so in $(kubectl get scaledobject -n $BENCHMARK_NAMESPACE -o name); do
  kubectl patch -n $BENCHMARK_NAMESPACE $so --type merge -p '{"spec":{"advanced":{"horizontalPodAutoscalerConfig":{"behavior":{"scaleDown":{"stabilizationWindowSeconds":180,"policies":[{"type":"Percent","value":50,"periodSeconds":120}]},"scaleUp":{"stabilizationWindowSeconds":0,"policies":[{"type":"Percent","value":100,"periodSeconds":5}]}}}}}}'
done

# The trace has to sit beside the profile: llm-d-benchmark bundles that whole
# directory into the guidellm-profiles ConfigMap (1MiB cap; the trace is 900KB).
cp test/benchmark/scenarios/shape_6k1000_1k4000_rps6_20m.jsonl llm-d-benchmark/workload/profiles/guidellm/

make benchmark-run BENCHMARK_SPEC=guides/pd-disaggregation MODEL_ID=Qwen/Qwen3-0.6B \
     BENCHMARK_HARNESS=guidellm BENCHMARK_WORKLOAD=shape_swap_replay_6k1000_1k4000_rps6_20m

# Graphs, the k1/k2 decision report and the comparison table. The k1/k2 report
# reads lines the controller logs at --v=3; benchmark-run installs it at that
# level (BENCHMARK_LLMSM_VERBOSITY=3). Over a controller you installed yourself,
# install it with LLMSM_VERBOSITY=3 first.
bash hack/benchmark/post_run_analyze.sh <results dir> $BENCHMARK_NAMESPACE

make benchmark-teardown
```

Pitfalls when you run it: [Running the benchmark: pitfalls](#running-the-benchmark-pitfalls).

## Measured

The scenario, trace, policy and HPA behaviour above, on 8 x H200 nodes (k1 =
930,508 tokens per decode replica), against the controller image built from
this tree at `f5f98761`, on 2026-09-23. Two passes back to back. The cold
pass ran first, on a controller restarted so it had seen nothing. The warm
pass followed, with the saturated throughput of both shapes on record from
the first (runs `guidellm-1790147333-mnjvnc_1` and
`guidellm-1790144180-oz7fvy_1`). The graphs and tables are the ones
`post_run_analyze.sh` writes.

| | this run (warm) | the cold pass before it |
|---|---|---|
| decode replicas ordered | 1 → 2, held through the first phase → 3 at the shape change, held | 1 → 2 → 3 (first ramp) → 2 → 3 → 4 → 3 |
| second decode replica ordered | +53 s after load start | +56 s |
| third decode replica ordered | +1344 s, four minutes into the second phase | +131 s, released at +206 s; again at +1392 s |
| prefill replicas | 1 throughout | 1 throughout |
| decode replicas, mean / max | 2.34 / 3 | 2.47 / 4 |
| TTFT p50 / p95 / p99 | 99 ms / **155 ms** / 229 ms | 99 ms / 204 ms / 3.9 s |
| ITL p50 / p95 | 5.6 / 20.7 ms | 5.3 / 22.1 ms |
| request latency p50 / p95 | 17.2 / 76.8 s | 14.4 / 82.3 s |
| decode GPU-minutes | 90.2 | 95.4 |
| requests / errors | 13,288 / 0 | 13,287 / 0 |

TTFT is time to first token; ITL is inter-token latency. The replica and
GPU-minute rows count decode only, as the harness's report does; prefill's one
replica is on top.

Where the tail is, by five-minute window (p95 TTFT from the engines' own
histograms, over the decode pods; every request also leaves a ~0.4 s prefill
sample, and a fleet-wide figure halves against this one): **0.23 s in
minutes 0-5**, then 0.08 s for the rest of the first phase; **0.23 s in
minutes 20-25**, then 0.04-0.07 s for the rest of the second. On the cold pass
the same two windows read 0.25 s and 4.2 s.

The pair was run twice, a day apart, on the same build. The repeat landed
within a tenth of every figure above (warm p95 167 ms against 155 ms, cold
227 ms against 204 ms, the same replica paths), except in the cold pass's
first five minutes. There, one replica tipping into preemption a cycle
earlier or later moves that window between 0.25 s and 5.6 s. Read the cold
ramp as a range, not a number.

![pipeline: replicas, demand vs capacity, KV, running, waiting, EPP queue](shape-swap-rps6-pipeline.png)

![replica status over the run](shape-swap-rps6-replicas.png)

**The first ramp** (minutes 0-5). One decode replica serves 6 req/s until the
second is Ready. The analyzer knows what one replica of this shape completes
when saturated: 5.66 req/s, priced from +38 s. The arrival rate over that is
1.06 replicas' worth, so it orders the second at +53 s, from the load, not
from a queue, and before the first replica tips into preemption. Two replicas
then carry the whole first phase. The largest backlog the floor ever prices is
4 requests (`backlogRequests` on the `throughput-demand-floor` line), and the
gateway queue is flat after one spike in the first seconds. The cold pass,
with nothing on record, is sized by occupancy for its first cycles: it orders
a third replica at +131 s and releases it at +206 s, once the fleet's own
reading arrives.

Prefill is never ordered: prompts waiting at the scheduler are no longer
charged to it as resident KV. A later cold pass ordered one by a second path,
a prefill saturation recorded while decode was saturated. The analyzer now
declines that, and holds prefill's demand where it neither orders nor releases
while decode is full and queued, which kept prefill at one since. See "A
prefill saturation records only while decode is not saturated" in the
[floor guide](../../developer-guide/saturation-demand-floor.md).

**The first phase, steady** (minutes 5-18). Occupancy is a fraction of one
replica (5-15% KV), the reading that alone would size the fleet to one. The
dashed target holds at two because the analyzer floors each role's demand at
what the load requires in throughput: the arrival rate over the completion
rate one replica sustained when last seen saturated.

**The shape change** (minute 18 on). The third replica is ordered at +1344 s,
the same cycle in which the 4000-token shape's completion rate first comes on
record (3.46 req/s), as the replicas' output length crosses into its bucket.
No queue forms: the waiting panel is flat through the second phase, and the
batch peaks at 62% KV and relaxes. The warm pass never asks for a fourth
replica. The cold pass does, at +1512 s, on one noisy arrival-rate sample
tipping the figure over the three-replica boundary, and asks for three again
75 s later. The HPA's 180 s window decides how long that spare is kept. The
sticky scale-down (`LLMSM_STICKY_SCALE_DOWN`, on by default) holds the published
three against the creep unless demand at three reaches the scale-up threshold.

What still moves: a controller that has never seen a shape saturated has no
throughput for it, and sizes that first ramp from occupancy and the backlog.
That is the cold pass: a spare third replica at +131 s, released at +206 s,
and a peak of four in the second phase. The mechanism is in
[the throughput floor](../../developer-guide/saturation-demand-floor.md); the
analysis behind it in [Sizing a backlog](../../proposals/backlog-sizing.md).

## What the unit specs hold

In `internal/engines/analyzers/saturation/`: `throughput_floor_test.go` and
`throughput_floor_total_test.go` (the throughput floor, and the queue charged
once across the pair), `downstream_saturation_test.go` (a prefill saturation
under a saturated decode), `start_role_test.go` (start time per role), and the
P/D cases in `analyzer_test.go` (per-role demand, prefill charged for input
tokens only). The joint commit, growing both roles together so the fleet keeps
its ratio, is covered by
`internal/engines/allocation/paired_exhausted_role_test.go` and nowhere else:
both passes above held prefill at one replica, so no cluster run has
exercised it.

## Replica start time

Every ramp above is sized by how long the second replica of a role takes to
become Ready. The queue that builds meanwhile counts as demand, so two runs of
one trace can differ by several replicas at the first ramp on pod start alone.
Get the start path right first:
[Preparing a workload](../../reference/workload-preparation.md#the-rest-of-the-start-path),
and for the harness,
[Replica start time in the harness](../../guides/benchmarking/README.md#replica-start-time-in-the-harness).
Sizing a backlog by throughput, so the ramp stops tracking start time, is
open: [Sizing a backlog](../../proposals/backlog-sizing.md).

## Running the benchmark: pitfalls

**Copying results can fail late.** The harness's final `kubectl cp` of the
results directory (500MB of raw scrapes) can die mid-stream with
`read message: %!w(<nil>)` and report the whole run FAILED. The results are
still on the workload PVC; `kubectl exec <access-to-harness-data pod> -- tar czf`
split into 40MB chunks copies fine.

**An unhealthy Kueue webhook blocks every pod.** A cluster whose Kueue pod
webhook is unhealthy denies every Deployment pod with
`Deployment.apps ... not found`. Nothing in this recipe causes it; check that
`kubectl create deployment` works in your namespace first.

**Captures are per run.** `make benchmark-run` starts a log tail and a
replica sampler. Their paths carry the namespace and a run id fixed once per
`make` invocation (`/tmp/llmsm_replica_samples-<ns>-<runid>.json`,
`/tmp/llmsm_controller_tail-<ns>-<runid>.log`, under `BENCHMARK_CAPTURE_DIR`).
So two passes never write to the same file, even in one namespace, or from two
sessions benchmarking one namespace on different clusters. The recipe prints
both paths when it starts them. Two captures writing at once used to produce
an empty controller log for one run and a replica-sample file holding two
concatenated JSON documents for another.

**Stop a stray capture with its own script.** A pass that ends badly can
leave a capture running. Use
`bash hack/benchmark/sample_replicas.sh stop <outfile>` and
`bash hack/benchmark/tail_scaling_manager_logs.sh stop <namespace> <outfile>`, which kill
that capture and nothing else. Do **not** `pkill -f tail_scaling_manager_logs.sh`: it
matches the command line, so it kills every capture on the box, including
another session's. `cat <outfile>.owner` says which namespace and context a
capture belongs to, and `BENCHMARK_KUBE_CONTEXT=<ctx>` names the cluster
instead of inheriting `KUBECONFIG`.

**Count what the captures hold before trusting a report.** Empty is the wrong
test: an artefact that lost its whole capture is `{"snapshots": []}`, which is
non-empty valid JSON, and that shape has cost data here. The workload numbers
are unaffected, but the target path, the ordering times and the dashed line
in the pipeline graph all come from these.

```bash
# one object per sample: a full run is hundreds
python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["snapshots"]))' \
    <results>/metrics/processed/llmsm_replica_samples.json
wc -l <results>/llmsm_controller.log
```

The sampler keeps the lines it assembled that artefact from, next to it as
`<outfile>.snapshots.jsonl`. A count that looks short can be checked against
them, and re-assembled from them if needed.
