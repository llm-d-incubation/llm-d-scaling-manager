# Two-Variant llm-d-scaling-manager Benchmark

End-to-end guide for the **two-variant efficiency-aware scaling** benchmark: a
single model deployed as two variants of differing `variantCost` under one
shared `InferencePool` / EPP, used to exercise the steady-state engine's cost-aware
optimizer. The optimizer scales the **most efficient** variant — the one with
the best serving-capacity per unit cost — up first, not simply the cheapest.
With the shipped pricing (primary cost 10 / TP=2, secondary cost 5 / TP=1, so
the cost ratio equals the GPU/TP ratio) the **TP=2 primary is the more efficient
variant** and scales up first, while the cheaper TP=1 secondary absorbs
spillover. See the efficiency note in `variants/v2-tp1-cheaper.yaml` for why.

For cluster login, namespace setup, and HuggingFace token configuration, follow
[`benchmark-guide.md`](benchmark-guide.md) Steps 1–4 first; this document picks
up after those.

---

## Topology

One `InferencePool` and one EPP front two `vLLM` `Deployment`s of the same
model. Each `Deployment` has its own KEDA `ScaledObject`, and both carry the
same `modelID` in their trigger metadata, so the steady-state engine groups
them and applies cost-weighted scaling.

```text
            +------------- Gateway --------------+
            |  HTTPRoute -> InferencePool (1 EPP)|
            +-------------------------------------+
                              |
               +--------------+--------------+
               |                             |
     +---------+--------+        +-----------+-----+
     | vLLM decode      |        | vLLM decode     |
     | primary (cost 10)|        | secondary (5)   |
     | ScaledObject     |        | ScaledObject    |
     +------------------+        +-----------------+
                   ^                       ^
                   +--- KEDA ---- scaler --+
                              external scaler
```

### The ScaledObject IS the registration

The scaling manager does not watch or list anything to find these workloads. It learns a
variant exists from the KEDA call the `ScaledObject`'s trigger causes, and
takes the variant's identity from that trigger's metadata:

| key | meaning |
|---|---|
| `modelID` | required; groups the two variants under one model |
| `variantCost` | price; omitted means `10.0`, which is the primary's price here |
| `scalingPolicy` | optional named policy tier |

Two consequences worth knowing before debugging a run:

- A variant with no `ScaledObject` is invisible. Not degraded — absent. The scaling manager
  reports nothing about it because it has never been told it exists.
- A `ScaledObject` scaled by a `prometheus` trigger never contacts the scaling manager at all,
  so it is equally invisible, and the deadlock is silent: nothing errors, the
  workload simply sits at its replica count.

### Label strategy (how both Deployments share one pool)

The `InferencePool` EPP selects pods by two camelCase labels:

```yaml
llm-d.ai/inferenceServing: "true"
llm-d.ai/model:            <model-hash>
```

The primary `Deployment` (managed by the `llm-d-modelservice` chart) adds a
third selector label, kebab-case: `llm-d.ai/inference-serving: "true"`.

The secondary `Deployment` created by `add_variant.py`:

- **Keeps** `llm-d.ai/inferenceServing` + `llm-d.ai/model` so the pool picks
  up its pods.
- **Omits** `llm-d.ai/inference-serving` so the primary `Deployment` does
  not claim secondary pods.
- **Adds** `scaling-manager.llmd.ai/variant: <suffix>` (default `v2`) as the secondary
  `Deployment`'s own selector discriminator.

No `llm-d.ai/variant` pod label is involved. It used to be documented as
required; no code reads it. The collector keys metrics on the model, and
per-variant the scaling manager gauges are labelled `variant_name` — whose value is the
**ScaledObject's** name, which is why the secondary's `ScaledObject` keeps the
`-v2` suffix (`dump_scaling_manager_full_timeseries.py` buckets on it).

---

## Required pieces

The benchmark only works end-to-end when **all** of these are in place.

### 1. The scaling manager built from this working tree

`make benchmark-standup` installs the scaling manager from this repo's `deploy/` — but with
`IMG` pointing at whatever you give it, defaulting to the published
`ghcr.io/ev-shindin/llm-scaling-manager:main`. **Pass `IMG=<your
build>` or you are benchmarking a registry image, not your changes.**

```bash
make docker-build docker-push IMG=quay.io/<you>/scaling-manager:<tag>
make benchmark-standup ... IMG=quay.io/<you>/scaling-manager:<tag>
```

This is the failure the current wiring exists to prevent: the scenario used to
carry a `scaling-manager:` block pinning chart and image to `v0.8.0-rc5`, so every run
measured a released binary while appearing to test the branch.

### 2. KEDA on the cluster

The `ScaledObject` is the registration, so no KEDA means no discovery and no
scaling. `deploy/install.sh` installs KEDA if the cluster has none, and leaves
a platform-managed one alone.

### 3. Scaling policy for the run

The shipped `scaling-manager-scaling-policy-config` already selects the V2 (token/capacity)
analyzer. [`make benchmark-enable-v2-saturation`](#step-3--set-the-scaling-policy)
overwrites it with the benchmark's thresholds at
[`hack/benchmark/scenarios/scaling_manager_threshold/saturation_v2_config.yaml`](../../hack/benchmark/scenarios/scaling_manager_threshold/saturation_v2_config.yaml)
and restarts the controller. Run it to pin the thresholds a comparison depends
on, not to turn V2 on.

### 4. Newer vLLM image

The scenario yaml pins `docker.io/vllm/vllm-openai:v0.14.0`. This is required
because the default llm-d ships `v0.9.2` which does **not** emit
`vllm:cache_config_info` at all.

---

## How to run

Set `NS` to your namespace, then walk the steps below from the repo root.

### Step 0 — Install the llm-d-benchmark CLI (one-time)

The make targets shell out to `llmdbenchmark` from a checkout of
[`llm-d-benchmark`](https://github.com/llm-d/llm-d-benchmark). On a fresh
clone of this repo:

```bash
make benchmark-install
```

Idempotent — re-running checks out the pinned `BENCHMARK_REPO_REF` and
re-installs the CLI.

### No prometheus-adapter

`BENCHMARK_SKIP_PROMETHEUS_ADAPTER` now defaults to `true`, and should stay
that way: prometheus-adapter and KEDA both register the
`v1beta1.external.metrics.k8s.io` APIService, and a cluster has exactly one.
Installing it takes that group away from the metrics server every
`ScaledObject`'s HPA queries. It was here to serve `llmsm_desired_replicas` to
hand-written HPAs — the path the KEDA external scaler replaced.

The flag stubs a `prometheus-adapter-resource-reader` ClusterRole annotated
with Helm release metadata, which makes `llmdbenchmark`'s existing-PA probe
pass so its install is skipped. Override the release-namespace annotation with
`LLMSM_MONITORING_NAMESPACE=<ns>` if you have a PA elsewhere.

### Step 1 — Stand up the primary variant

```bash
make benchmark-standup BENCHMARK_NAMESPACE=$NS \
                       BENCHMARK_SPEC=guides/two-variant-scaling-manager \
                       BENCHMARK_MODEL_ID=unsloth/Meta-Llama-3.1-8B-Instruct \
                       IMG=quay.io/<you>/scaling-manager:<tag> \
                       ENVIRONMENT=openshift
```

`ENVIRONMENT` picks the install path (`openshift` → `deploy-scaling-manager-on-openshift`,
anything else → `deploy-scaling-manager-on-k8s`). `IMG` decides what is measured — see
[Required pieces #1](#1-the-scaling-manager-built-from-this-working-tree).

> **Model requirement — must be chat-template-bearing.** Use an
> instruct/chat-tuned model (the scenario default is
> `unsloth/Meta-Llama-3.1-8B-Instruct`). On llm-d-benchmark ≥ v0.7.0 the
> decode image ships transformers ≥ 4.44, which **rejects a model whose
> tokenizer defines no chat template** (i.e. base models such as
> `unsloth/Meta-Llama-3.1-8B`) with `ChatTemplateResolutionError`, and every
> request errors. To use a base model anyway, supply `--chat-template` or
> pin a transformers-< 4.44 decode image.

`benchmark-standup` copies two files into the `llm-d-benchmark` checkout:

- [`hack/benchmark/scenarios/guides/two-variant-scaling-manager.yaml`](../../hack/benchmark/scenarios/guides/two-variant-scaling-manager.yaml)
  — the scenario values (replicas, image pins, decode shape).
- [`hack/benchmark/scenarios/guides/two-variant-scaling-manager.yaml.j2`](../../hack/benchmark/scenarios/guides/two-variant-scaling-manager.yaml.j2)
  — the specification wrapper that the `--spec` flag actually loads.

Standup then installs the `llm-d-infra`, `inferencepool-gaie` and
`modelservice` Helm releases for the chosen model with primary `tensor: 2`.
`BENCHMARK_MODEL_ID` is required — without it the standup defaults to a
placeholder dummy model.

When that returns, the target installs the scaling manager itself, in two steps you can also
run separately as `make benchmark-deploy-scaling-manager`:

1. `deploy-scaling-manager-on-{openshift,k8s}` with `LLMSM_NS=$NS LLMSM_SCOPE=namespace` — the
   controller lands *in the benchmark namespace* and manages only it.
2. `scaledobjects-apply` — discovers the model servers in that namespace and
   creates one `ScaledObject` each, at `BENCHMARK_KEDA_MIN_REPLICAS` /
   `BENCHMARK_KEDA_MAX_REPLICAS` (default 1/10) and the default
   `variantCost` of 10.0.

Check that the primary registered before going further — an empty result here
is the whole benchmark silently measuring nothing:

```bash
kubectl get scaledobject -n $NS
kubectl logs -n $NS deploy/scaling-manager-controller-manager | grep -i "scaling-decision"
```

### Step 2 — Add the secondary variant

```bash
make benchmark-add-variant BENCHMARK_NAMESPACE=$NS
```

This invokes `hack/benchmark/add_variant.py` against the variant config at
`hack/benchmark/scenarios/guides/variants/v2-tp1-cheaper.yaml` (default —
override with `VARIANT_CONFIG=<path>`), creating a secondary `Deployment` and
a `ScaledObject` cloned from the primary's, both named with the `v2` suffix,
with `variantCost: "5.0"` in the trigger metadata.

The clone inherits the scaler address, polling interval, cooldown and
`advanced` behaviour the standup chose, and changes only four things: the
name, what it scales, its replica bounds, and its price. The scale target is
never copied: each variant's target is read from its own ScaledObject's
`scaleTargetRef`, so a clone cannot point the new variant at the original's
`Deployment`.

Verify both variants registered:

```bash
kubectl get scaledobject,hpa -n $NS
kubectl get pods -n $NS -l 'llm-d.ai/inferenceServing=true,llm-d.ai/model=unsloth--6b24a594-instruct'
```

The `hpa` rows are KEDA's — it creates one per `ScaledObject`. Nothing in this
flow writes an HPA by hand.

### Step 3 — Set the scaling policy

The shipped config already selects V2. Apply the benchmark's thresholds and
restart the controller so a comparison is not run against whatever the install
defaults happen to be:

```bash
make benchmark-enable-v2-saturation BENCHMARK_NAMESPACE=$NS
```

Confirm the analyzer in use:

```bash
kubectl logs -n $NS -l app.kubernetes.io/name=scaling-manager \
  --tail=200 | grep -i "analyzer-result"
```

### Step 4 — (Optional) Tune the scale-up envelope

Damping now lives on the `ScaledObject`, not on a hand-written HPA — KEDA
renders `spec.advanced.horizontalPodAutoscalerConfig.behavior` into the HPA it
owns, and patching that HPA directly is undone the next time KEDA reconciles.

```bash
for so in $(kubectl get scaledobject -n $NS -o name); do
  kubectl patch -n $NS "$so" --type=merge -p '{"spec":{"advanced":
    {"horizontalPodAutoscalerConfig":{"behavior":{"scaleUp":
    {"stabilizationWindowSeconds":120}}}}}}'
done
```

### Step 5 — Run the benchmark

The default workload is `test/benchmark/scenarios/prefill_heavy.yaml.in`.
Edit the file (`rate`, `max_seconds`, `prompt_tokens`, `output_tokens`)
before invoking — `make benchmark-run` copies the file at run-time, so the
value on disk at invocation is what gets used.

For multi-run comparisons, restart the controller between runs to flush k2
history (otherwise stale per-replica capacity estimates from the previous
run can poison the next):

`BENCHMARK_MODEL_ID` must be passed to `benchmark-run` as well — without it
the CLI defaults to `e2e-scaling-manager/dummy-model` and fails model verification.

Pass `BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX=v2` to enable two-variant
post-processing (per-variant replica rows, weighted cost column, and a
full-pipeline PNG plot). The GPU counts for the cost formula are read automatically
from the scenario and variant config YAMLs (`tensor:` field).

```bash
make benchmark-restart-controller BENCHMARK_NAMESPACE=$NS
make benchmark-run BENCHMARK_NAMESPACE=$NS BENCHMARK_SPEC=guides/two-variant-scaling-manager \
                   BENCHMARK_MODEL_ID=unsloth/Meta-Llama-3.1-8B-Instruct \
                   BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX=v2
# Override workload:
make benchmark-run BENCHMARK_NAMESPACE=$NS BENCHMARK_SPEC=guides/two-variant-scaling-manager \
                   BENCHMARK_MODEL_ID=unsloth/Meta-Llama-3.1-8B-Instruct \
                   BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX=v2 \
                   BENCHMARK_WORKLOAD=symmetrical
```

Each run produces a workspace under `$REPO/$USER-<timestamp>/...` with raw
metrics, logs, and processed timeseries.

### Step 5a — Post-run output

When `BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX=v2` is set, `benchmark-run`
automatically produces two outputs after the run completes:

**Run `post_run_analyze.sh` promptly** (within a few minutes of run completion —
The scaling manager controller pod's log buffer rotates and the window for extracting
controller decisions closes). Its k1/k2 report reads lines the controller logs only at
`--v=3`. `benchmark-run` installs the controller at that level
(`BENCHMARK_LLMSM_VERBOSITY=3`); a controller installed another way needs
`LLMSM_VERBOSITY=3` on its install, or the report comes back empty:

```bash
bash hack/benchmark/post_run_analyze.sh <results-dir> $NS
# e.g. bash hack/benchmark/post_run_analyze.sh \
#   biran-20260704-135514-081/results/guidellm-1783162554-04wm0f_1 biran
```

This runs five steps: dumps the scaling manager controller decisions + saturation analysis
numbers from pod logs, computes capacity/demand estimates from raw vLLM/EPP
scrapes, extracts EPP throughput and llm-d-scaling-manager Prometheus timeseries, and renders the
full-pipeline PNG plot into `<results-dir>/metrics/graphs/`.

It exits 1 when the run window holds no controller line of the level the
controller ran at: `k2-decision` at `--v=3` or more, `replica-capacity-summary`
at `--v=2`. Below 2 the controller logs neither, and the analysis prints "Not
checked" instead. It reads that level from the Running controller Pods' args, so
a controller reinstalled since the run gives the wrong one; set
`CONTROLLER_VERBOSITY=<n>` to name the run's level. It also exits 1 when it
cannot read the level.

**Markdown table** (printed to stdout, copy-paste into `docs/developer-guide/benchmark-results.md`):

```text
| Metric                                | Run 1 |
|---------------------------------------|-------|
| P99 TTFT (ms)                         | 601   |
| P99 ITL (ms/token)                    | 6.2   |
| Avg primary replicas                  | 5.84  |
| Max primary replicas                  | 10    |
| Avg secondary replicas                | 2.31  |
| Max secondary replicas                | 8     |
| Avg KV cache utilization              | 71.3% |
| Avg queue depth (EPP)                 | 12.4  |
| Error count                           | 0     |
| Avg pod startup (s)                   | 118   |
| Cost (weighted avg replicas × GPU/hr) | 14.31 |
```

The cost is `(primary_avg × 2 + secondary_avg × 1)` — weighted by TP (GPU)
count, read from the scenario and variant config YAMLs so no manual input
is needed.

**Full-pipeline PNG plot** saved to `<results-dir>/metrics/graphs/two_variant_v2_full_pipeline.png`.
Panels (up to 7, optional panels appear when data is present):
1. Replica count — ready (solid) + the scaling manager desired (dashed) per variant
2. Estimated demand (stacked: in-use / vLLM waiting / EPP queue) vs capacity
3. KV cache utilisation (avg per variant)
4. Requests running (sum per variant)
5. vLLM requests waiting (sum per variant)
6. EPP queue metrics (flow-control queue, pool average, per-variant per-pod sum)
7. Gateway throughput (requests/s and errors/s from EPP counters)

To regenerate either output against an older results directory:

```bash
# Markdown table:
python3 hack/benchmark/postprocess.py \
    --secondary-suffix v2 \
    --scenario-yaml hack/benchmark/scenarios/guides/two-variant-scaling-manager.yaml \
    --variant-config hack/benchmark/scenarios/guides/variants/v2-tp1-cheaper.yaml \
    <results-dir>

# Plot:
python3 hack/benchmark/plot_two_variant_pipeline.py <results-dir>
```

### Step 6 — Teardown

```bash
make benchmark-teardown BENCHMARK_NAMESPACE=$NS \
                        BENCHMARK_SPEC=guides/two-variant-scaling-manager
```

`BENCHMARK_SPEC` must be passed (the make target's default is the
`guides/workload-autoscaling` scenario, which the CLI can't find for
two-variant teardown).

Teardown removes the scaling manager **first**, then the Helm releases: a namespace-scoped
install still creates cluster-scoped RBAC, which deleting the namespace would
leave behind. Pass the same `ENVIRONMENT` you installed with.

The secondary `Deployment` is created by `benchmark-add-variant` outside any
Helm release; the llm-d-benchmark teardown explicitly deletes orphaned
Deployments in the namespace, and `add_variant.py` sets an `ownerReference` on
the secondary `ScaledObject` pointing at that `Deployment` so it cascades with
it. That matters beyond tidiness — an orphaned `ScaledObject` keeps
registering a workload that no longer exists.

---

## Verifying efficiency-aware behavior

In the controller log during sustained load you should see, ordered by
priority:

1. The **more efficient** variant scaling up first — the TP=2 primary at the
   shipped 10/5 pricing.
2. The less efficient variant joining only when the efficient one's
   `maxReplicas` cannot absorb demand alone.
3. On scale-down, the less efficient variant shrinking first.

Two structured lines carry it. `analyzer-result`, once per model per cycle
(`steadystate/engine_v2.go`), with short keys — `supply`, `demand`, `util`,
`rc` (required capacity), `sc` (spare capacity) — and a `variants` list:

```text
steadystate/engine_v2.go:995  analyzer-result
  {"modelID":"unsloth/Meta-Llama-3.1-8B-Instruct","analyzer":"saturation",
   "supply":2503400,"demand":2229945,"util":0.89,"rc":120065,"sc":0,...}
```

and `scaling-decision`, which names each variant and its current/target count.

`supply` should track the realized capacity (sum of `cache_config_info` across
ready pods) — typical values for Llama-3.1-8B / H100 are ~315k tokens per pod,
so ~3M tokens at 10+0 replicas. Numbers near 13k mean the collector fell back
to the per-step batch budget: check the model server emits
`vllm:cache_config_info` ([Required pieces #4](#4-newer-vllm-image)).

---

## Files involved

| Path | Role |
|---|---|
| `hack/benchmark/scenarios/guides/two-variant-scaling-manager.yaml` | Scenario / values for the primary stack (decode shape, TP=2, image pins). Installs **no** autoscaler: `wva.enabled: false`. Copied into the `llm-d-benchmark` checkout automatically by `make benchmark-standup`. |
| `hack/benchmark/scenarios/guides/variants/v2-tp1-cheaper.yaml` | Default secondary-variant config (suffix `v2`, cost 5.0, TP=1) consumed by `make benchmark-add-variant`. Override path with `VARIANT_CONFIG=<path>`. |
| `hack/benchmark/add_variant.py` | Creates the secondary `Deployment` and clones the primary's `ScaledObject` onto it, with the kebab-label trick. |
| `deploy/lib/scaledobject.sh` | Discovers llm-d model servers and renders their `ScaledObject`s. Driven by `make scaledobjects-apply`, which `benchmark-deploy-scaling-manager` calls. |
| `hack/benchmark/post_run_analyze.sh` | Wraps the five post-run dump+plot steps. Must run promptly after `benchmark-run` — the scaling manager controller log buffer rotates. Usage: `bash hack/benchmark/post_run_analyze.sh <results-dir> [namespace]`. |
| `hack/benchmark/dump_scaling_manager_target_timeseries.py` | Extracts the scaling manager controller decisions and saturation analysis numbers (supply, demand, utilization, required/spare capacity) from pod logs into `metrics/processed/llmsm_target_timeseries.json`. |
| `hack/benchmark/dump_capacity_demand_estimate.py` | Computes per-variant capacity/demand estimate from raw vLLM/EPP scrapes into `metrics/processed/capacity_demand_estimate.json`. |
| `hack/benchmark/dump_epp_throughput.py` | Derives request rate from EPP counters into `metrics/processed/epp_throughput.json`. |
| `hack/benchmark/dump_scaling_manager_full_timeseries.py` | Extracts llm-d-scaling-manager Prometheus metrics timeseries into `metrics/processed/llmsm_metrics_timeseries.json`. |
| `hack/benchmark/postprocess.py` | Generates a markdown results table (matching `docs/developer-guide/benchmark-results.md`) from a results directory. Called automatically by `make benchmark-report`. Pass `--secondary-suffix v2` for per-variant replica rows and weighted cost. |
| `hack/benchmark/plot_two_variant_pipeline.py` | Generates the full-pipeline PNG (up to 7 panels: replicas, capacity/demand, KV cache, requests running/waiting, EPP queue, gateway throughput, the scaling manager saturation utilization). Called automatically by `make benchmark-plot-two-variant`. |
| `hack/benchmark/scenarios/scaling_manager_threshold/saturation_v2_config.yaml` | The `scaling-manager-scaling-policy-config` ConfigMap for the run: `analyzerName: saturation` plus the thresholds. Applied by `make benchmark-enable-v2-saturation`. |
| `test/benchmark/scenarios/prefill_heavy.yaml.in` | Default workload for `make benchmark-run`. Edit `rate`/`max_seconds` here — `make benchmark-run` copies this file at run-time, overriding any stale defaults in the benchmark repo. |

---

## Tuning knobs

| Knob | Where | Effect |
|---|---|---|
| `IMG` | `make benchmark-standup` / `benchmark-deploy-scaling-manager` | **What is measured.** Defaults to the published `:latest`, not your build |
| primary cost | trigger metadata on the primary's `ScaledObject` | Omitted = 10.0. `kubectl patch` it, or supply `LLMSM_DEFAULT_SO_TEMPLATE=<file>` to `scaledobjects-apply` |
| `variantCost` field | `variants/v2-tp1-cheaper.yaml` (or other `VARIANT_CONFIG`) | Secondary cost (default 5) |
| `suffix` field | variant config yaml | Secondary `Deployment`/`ScaledObject` name suffix (default `v2`; keep the `-v2` ending — the timeseries dump buckets on it) |
| `BENCHMARK_KEDA_MIN_REPLICAS` / `_MAX_REPLICAS` | `make benchmark-standup` | Primary's bounds (default 1/10) |
| `minReplicas` / `maxReplicas` | variant config | Secondary's bounds |
| `spec.advanced.horizontalPodAutoscalerConfig.behavior` | the `ScaledObject` | Scale-up/down damping. Patching KEDA's HPA directly is reverted on its next reconcile |
| `rate`, `max_seconds`, `prompt_tokens`, `output_tokens` | `prefill_heavy.yaml.in` | Workload shape |

---

## Common failure modes

- **Nothing in the controller log mentions the variant at all**
  → It has no `ScaledObject`, or its `ScaledObject` has no
  `external`/`external-push` trigger. The scaling manager is never called about it and so has
  nothing to say. `kubectl get scaledobject -n $NS -o yaml` and check the
  trigger type; `make scaledobjects-apply LLMSM_NS=$NS LLMSM_DEFAULT_SO_NS=$NS`
  creates the missing ones.
- **`Default ScaledObjects: 0 created` / `No llm-d model servers found`**
  → The discovery scan looks for `llm-d.ai/inferenceServing=true` on the pod
  template. If the model servers are up and labelled and it still finds
  nothing, check the namespace it scanned: a namespace-scoped install scans
  only its own, which is why the benchmark installs the scaling manager *into* `$NS`.
- **Both variants scale to `maxReplicas` immediately under modest load**
  → The analyzer read fallback capacity, not real KV. Check the model server
  image emits `vllm:cache_config_info` ([Required pieces #4](#4-newer-vllm-image)).
- **Primary scales up while the secondary stays at one replica**
  → Expected at the shipped 10/5 pricing: the TP=2 primary is the more
  efficient variant (cost ratio == GPU/TP ratio), so the optimizer scales it
  first and the cheaper TP=1 secondary only joins once the primary hits its
  `maxReplicas`. To make the secondary the preferred-first variant instead,
  price it *below* its proportional GPU share (cost ratio > TP ratio) — see
  the efficiency note in `variants/v2-tp1-cheaper.yaml`.
- **Stale capacity estimates after a previous run**
  → k2 history persists for the controller's lifetime. Run
  `make benchmark-restart-controller BENCHMARK_NAMESPACE=$NS` between runs
  to flush it ([Step 5](#step-5--run-the-benchmark)).
- **Standup fails at `[03] workload_monitoring` with**
  `APIService "v1beta1.external.metrics.k8s.io" exists and cannot be imported into the current release`
  → Something set `BENCHMARK_SKIP_PROMETHEUS_ADAPTER=false`. It defaults to
  `true` and should stay there: that APIService belongs to KEDA
  ([No prometheus-adapter](#no-prometheus-adapter)).
- **The results look like a released build, because they are**
  → `IMG` was left at its default. There is no way to tell after the fact from
  the metrics alone; the standup prints the image it installs, so check the
  standup log ([Required pieces #1](#1-the-scaling-manager-built-from-this-working-tree)).
