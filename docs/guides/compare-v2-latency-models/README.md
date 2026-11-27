# Compare the v2 latency models with the default

## Overview

The scaling manager predicts each model's TTFT and TPOT with its own
(default) models. It also runs the capacity planner's v2 models, the
rank-mixture TTFT and the per-request TPOT distribution, wherever it changes
nothing: for a passive model, and for every model while the utilization share
plans in shadow. Both predictions are published side by side, so you can see
which one tracks what your router measures, and what prefill fleet the v2
TTFT model would ask for. No scaling decision reads the v2 figures.

This guide reads those figures for one model. The series are described in
[the metrics reference](../../reference/prometheus.md#slo-analyzer-v2-latency-models);
the design is in [the proposal](../../proposals/predicted-latency-in-controller.md).

## Prerequisites

- [ ] The scaling manager installed in passive mode, with the model
  registered: [try it in passive mode](../try-in-passive-mode/), steps 1 to 3.
  A model you keep active needs the utilization share in shadow instead.
- [ ] The model's [scaling policy](../../reference/scaling-policy.md) lists
  the `slo` analyzer, and sets a TTFT objective for the prefill ask. Without
  the `slo` analyzer the v2 models do not run for the model.
- [ ] Prometheus scraping the controller, reachable from your machine, and
  `curl` and `jq`.
- [ ] For the TTFT model: a prefill/decode model with one prefill variant on
  vLLM. The TPOT model also covers a model with no prefill role.

<!-- guide:prerequisites start -->
```bash
# The namespace and model to read, and a Prometheus you can reach (port-forward it as the capacity planner's recording guide does).
export NAMESPACE=<model namespace>
export MODEL_ID=<the model, as llmsm series name it>
export PROM_URL=http://localhost:9090
q() { curl -sG "${PROM_URL}/api/v1/query" --data-urlencode "query=$1" | jq -r '.data.result[] | [(.metric | del(.__name__) | tostring), .value[1]] | @tsv'; }
```
<!-- guide:prerequisites end -->

## Steps

### 1. Check the v2 models run for the model

<!-- guide:read.passive start -->
```bash
# The model is passive (or the utilization share plans in shadow), so the v2 models run for it.
q "llmsm_passive_mode{exported_namespace=\"${NAMESPACE}\", model_name=\"${MODEL_ID}\", mode!=\"active\"} == 1"
```
<!-- guide:read.passive end -->

### 2. Read where each model stands

<!-- guide:read.coverage start -->
```bash
# Where each model stands, per objective. window-short is normal for the first 12 hours (TTFT) and 2 hours (TPOT).
q "llmsm_slo_model_coverage{exported_namespace=\"${NAMESPACE}\", model_name=\"${MODEL_ID}\"} == 1"
q "llmsm_slo_model_input_missing{exported_namespace=\"${NAMESPACE}\", model_name=\"${MODEL_ID}\"}"
```
<!-- guide:read.coverage end -->

- `priced`: the figures below are published.
- `window-short`: wait. The TTFT model needs 12 hours of traffic, the TPOT
  model 2. A controller restart starts the window again.
- `signal-missing`: the second query names the input your fleet does not
  export. See the signal table in the reference.
- `abstained`: the model's checks refused the window (the same refusal the
  capacity planner gives on a recording of it), for instance prompts of one
  length, or a prefill engine whose `--max-num-seqs` the controller cannot
  read.
- `model-limit`: the controller already runs the v2 models for 16 models,
  the most it holds at once. This one starts when one of them leaves.

### 3. Compare the TTFT

<!-- guide:read.ttft start -->
```bash
# The TTFT by learner and statistic (slo is the default model, ttft-mixture the v2), beside what the router measured.
q "llmsm_slo_predicted_ttft_seconds{exported_namespace=\"${NAMESPACE}\", model_name=\"${MODEL_ID}\"}"
q "llmsm_slo_observed_seconds{exported_namespace=\"${NAMESPACE}\", model_name=\"${MODEL_ID}\", objective=\"ttft\"}"
```
<!-- guide:read.ttft end -->

Each prediction is at the current load. Compare the same statistic of each
learner with the observed one over a day, not one reading: a model that is
right tracks the router as load moves.

### 4. Compare the TPOT

<!-- guide:read.tpot start -->
```bash
# The TPOT by learner and statistic (slo is the default model, tpot-distribution the v2's p90 and p95).
q "llmsm_slo_predicted_tpot_seconds{exported_namespace=\"${NAMESPACE}\", model_name=\"${MODEL_ID}\"}"
```
<!-- guide:read.tpot end -->

Both learners share the mean: the v2 distribution is placed around the
default's. They differ in p90 and p95.

### 5. Read what the v2 TTFT model would ask for

<!-- guide:read.sizing start -->
```bash
# The prefill replicas the analyzers ask for (learner empty) and would ask for on the v2 TTFT model (ttft-mixture).
q "llmsm_replicas_suggested{exported_namespace=\"${NAMESPACE}\", model_name=\"${MODEL_ID}\", role=\"prefill\"}"
```
<!-- guide:read.sizing end -->

The `ttft-mixture` series is the analyzers' ask with the SLO analyzer sizing
prefill on the v2 TTFT model at the policy's TTFT objective. It is never
applied. A model you make active stops publishing it, unless the
utilization share plans in shadow: then every model publishes it, and an
active model's own target is still the one applied.

## What next

To see the same models on a recording, run the capacity planner with
`--experimental-ttft=v2 --experimental-tpot=v2`: it fits them with the same
code, and gives the same figures on the same cycles.
