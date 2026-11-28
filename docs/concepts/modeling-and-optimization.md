# Modeling and optimization

llm-d-scaling-manager decides how many replicas each variant of each model
needs, as the request rate and request shape (input and output token counts)
change. It is a *global* autoscaler, not a set of per-Deployment ones. Each
cycle it solves every variant together against one accelerator budget, using
the available accelerators, live load, each model's SLOs, and workload
priority.

This page covers the terms of that decision, what is modelled, and how
per-variant demands become a replica count. For the measurement
path that produces those demands, read
[the steady-state engine](steady-state-engine.md) first.

## Definitions and assumptions

- An **accelerator** is a unit of GPU allocation of one type and count. For example, 2xH100 is one accelerator made of two H100 GPUs, sized for the model's memory or performance needs.

- An **accelerator arrangement** consists of one or more accelerators in parallel (tensor or pipeline parallelism) assigned to a model server.

- A **variant** is a set of model servers (**replicas**) serving one model on the same accelerator arrangement. In the running system it is exactly one KEDA ScaledObject and the workload it scales. A replica belongs to the variant whose managed scaler its `ownerReferences` lead to, and the variant is named after that scaler.

- The **model SLOs** define target values for two metrics:

  1. *TTFT*: queueing time plus waiting for and running prefill.
  2. *ITL*: the decode time for one output token. Congestion stretches it: batching, prefill injected into a long decode cycle, and KV-cache effects such as memory swapping.

- **Workload priority** (or **criticality**) says how important one application's requests are. Other components use it differently: an admission controller to choose which requests to drop, a request scheduler to order its queue. Here it decides which variants get accelerators when there are not enough to meet every model's SLOs.

## What is modelled, and what is not

Everything below is learned from live metrics. There is no performance-profile
store, no offline benchmarking step, and no lookup per model-accelerator pair.
A variant never observed under load gets a conservative estimate. That is a
deliberate trade, and the project proposal states its cost: a shape has to
saturate once before it can be priced well.

**Per-replica capacity, in KV-cache tokens.** A replica's usable capacity is
the smaller of a memory ceiling (its physical KV-cache size scaled by
`kvCacheThreshold`) and a compute ceiling (how many concurrent tokens it can
process before scheduling, not memory, is the limit). A variant's capacity is
the median across its ready replicas. This makes a decision divisible: a
replica count is `ceil(required tokens / per-replica capacity)`, so one cycle
can order three replicas, not just one.

**The saturated completion rate, per role.** The demand floor is priced
against mu: the completion rate one replica sustained when last seen
saturated, recorded per output-length bucket. Occupancy belongs to the fleet
and falls as replicas are added. mu belongs to the hardware and the request
shape, and does not move with the fleet. A fleet needs `lambda / mu` replicas
to keep up. Prefill's mu is in prompt tokens: the tokens one replica computes
per second, at the prompt length arriving. A prefill that has only ever queued
behind a full decode has a lower bound on it instead. See
[the throughput floor](steady-state-engine.md#the-throughput-floor).

The controller sizes replicas from these two. The decode and prefill models
below price latency. The [capacity planner](../guides/capacity-planner/) uses
them to answer what-if questions from a recording. In the controller the
[`slo` analyzer](../reference/scaling-policy.md#slo-observes-bounds-decode-packing)
runs them too. On this release it reports and does not decide, except that a
`tpot` target bounds decode packing. How it is built is on the
[`slo` analyzer developer page](../developer-guide/slo-analyzer.md).

### Decode: a shared step, and the loop it closes

A decode replica runs every sequence it holds in one step, so the sequences
share it (processor sharing). The step time is the inter-token latency, and it
rises with the KV fill `k`. The scaling manager fits it per variant as a line,
`ITL(k) = A*k + B`, over the fills it has seen. `B` is the step's fixed cost.
`A*k` is the cost of the KV in the batch.

The fill and the ITL set each other. More requests a second means more
sequences held, a fuller cache, a slower step, longer requests, and so more
sequences held again. At a rate `lambda` per replica, with `S` output tokens a
request, `KVreq` KV tokens a request and `C` KV tokens of capacity, the fill
that settles is

    k = lambda*S*B / (C/KVreq - lambda*S*A)

and the TPOT there is `ITL(k)`. For Poisson arrivals this is the exact mean
of the processor-sharing queue, not an approximation. The decode knee is
where the fill reaches the top of the fitted band (0.8 by default) or the
batch cap. A
small birth-death chain on the sequences held gives the wait for a free slot,
which is small until close to the knee.

**The loop gain.** `g = lambda*S*KVreq/C * A` is the share of each step that
the fill adds back. It rises with the rate, and the fill above is finite only
while `g < 1`. It also says how far to trust a figure. A relative error `e`
in the slope `A` moves the fill, and the TPOT read there, by about
`e * g/(1 - g)`:

| `g` | a 5% slope error moves the TPOT by |
| --- | --- |
| 0.3 | about 2% |
| 0.6 | about 8% |
| 0.8 | about 25% |
| 0.9 | about 80% |

Where this bites: an engine with a small `B` beside `A`. A small model on a
fast GPU has a step of about 1 ms, and reaches a gain of 0.8 or more before
its knee. A 32B model, with a step near 10 ms, stays under about 0.7. The
reverse question, the rate that meets a TPOT target, is not amplified: a slope
error moves that rate by at most its own size.

The full derivation, and what each term reads, is in
[decode: TPOT from the ITL line](../proposals/slo-capacity-planner.md#decode-tpot-from-the-itl-line).

### Prefill: a queue on prompt tokens

TTFT is the wait before prefill, the request's own prefill, the KV transfer to
decode, and decode's admission wait and first step. Each prefill pod is
modelled as a queue that serves prompt tokens (processor sharing by default).
It takes its share of the arrivals, skewed by the measured imbalance `f`
between pods.

`V_P` is a prefill pod's prompt-token throughput while it has work. A
request's own prefill is `a + I/V_P`, with `I` its prompt tokens and `a` a
fixed cost. `V_P` sets the prefill knee: the rate at which the busiest pod's
utilization reaches its limit. It is read, in order of preference:

1. from minutes the pods were busy;
2. from a peak seen under a queue;
3. as a lower bound, where prefill only ever queued behind a full decode. The
   knee is then "at least" that rate, and the TTFT figures say so
   (`VPLowerBound`);
4. from `--assume-vp`, in the planner. Otherwise the TTFT is refused.

Above the rate where the router's flow control starts holding requests, a
router queue is added. Below it, nothing changes. The whole model is in
[end-to-end TTFT](../proposals/slo-capacity-planner-ttft.md).

Known gap: a chunked-prefill pod serves faster as its batch fills, so `V_P`
is not constant with the load. A single `V_P` reads the knee low and the
stretch near it in the wrong shape.

### How sure a figure is: bands

In the capacity planner, decode figures carry a band: the 10th to 90th
percentile of the figure re-priced on 200 resampled ITL lines. The
resampling is by blocks of cycles,
with a fixed seed, so the same recording gives the same band. Each resampled
line goes through the same fill loop, so the band widens where the gain is
high.

What a band covers: the noise in the readings the line was fitted on.

What it does not cover:

- a line that is wrong for the shape asked, as when fitted at one request shape
  and priced at another;
- `V_P` and `f`, which get a sensitivity span of plus or minus 15% instead;
- the TPOT percentile ratio, the KV per request and the arrivals' burstiness;
- the model's own form.

Bands are not calibrated yet: how often a measured figure lands inside one has
not been validated per model class. Read a band as the least uncertainty a
figure has, not the most. The measured accuracy per figure is on
[how accurate each figure is](../guides/capacity-planner/accuracy.md).

### What kind of engine it is: model-class detection

The models above apply to any LLM only if the scaling manager knows what kind
of engine it is pricing. It reads that from the engine's own configuration and
metrics, never from the model's name:

| Class | Read from | What it changes today |
| --- | --- | --- |
| Attention kind: full, sliding window, MLA, hybrid | the model's `config.json` (the [`model-config` annotation](../reference/configuration.md) in the controller; `--model-config` in the planner) | reported; MLA and hybrid put figures outside the validated region |
| MoE class: dense, MoE with or without expert parallelism | the same `config.json`, and the engine's flags | reported beside the engine |
| Whether the step budget binds | vLLM's `vllm:iteration_tokens_total` | reported; the planner also caps the batch per rank at the budget itself |
| Host-bound decode step | the measured intercept and floor, under 5 ms | the planner refuses a TPOT percentile read off coarse router buckets |
| Role (prefill or decode) | the engine's own signals | behind a controller switch, off by default: labels decide |

An unknown class is named "unknown" and prices as before. To give the planner
the model's config, see `--model-config` in
[recording for the capacity planner](../guides/record-for-capacity-planner/).

### What is not modelled

- **No forecasting.** The decision uses what is measured now: an operator can
  read and argue with it, but it cannot arrive before the load.
- **No parameter tuner.** Thresholds are set in the
  [scaling policy](../reference/scaling-policy.md), not searched for.
- **No batch-size term by default.** ITL is a line in the KV fill. An opt-in
  batch term exists, but is off: no recording backs it yet. Where a load runs
  more sequences a pod than the line was fitted on, the figures are flagged
  `BatchBeyondFit`.
- **No second axis where fill cannot see the load.** MLA, fp8 KV and
  sliding-window models, and small models on large-memory GPUs, can run under
  the fitted band at every load. They get no line, and their TPOT is refused.

## Optimization

The optimizer turns each analyzer's per-variant demand into a replica count for
every variant of every model, inside whatever accelerator budget the limiter
declares. Two things decide the outcome.

**Cost, when there is room.** Variants are ranked by serving capacity per unit
of cost (the relative `variantCost` key in the ScaledObject trigger). The *most
efficient* variant grows first, which is not always the cheapest. A TP=2
variant at twice the cost of a TP=1 variant is the better buy whenever it
serves more than twice as much. See `cost_aware_optimizer.go` and
`greedy_score_optimizer.go`.

**Priority, when there is not.** When the budget cannot meet every model's
demand, allocation is a priority-weighted fair share: a variant's `priority`,
which normally comes from its [policy tier](../well-lit-paths/workload-classes/),
is the weight in that split (`fairShareValue`). The optimizer records what it
could not give: which variant was limited, by what, and how many GPUs it got.
That shows up as `llmsm_model_scaling_blocked` with a reason, not as silence.

**Constraints, not modes.** Limiters are ordinary constraints on the optimizer:
GPU inventory, declared quota, or none. The `limiters:` list on the scaling
policy chooses them; there is no mode switch. See
[bounding a fleet by real GPUs](../well-lit-paths/bound-by-gpus/) and
[GPU capacity accounting](gpu-capacity-accounting.md).
