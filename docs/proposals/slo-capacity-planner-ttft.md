# What-if capacity: end-to-end TTFT

**Status:** proposal, 2026-11-02. The model is built as a library (P3,
2026-11-04: [as built](#as-built-p3)), UNVALIDATED and on no switch; in the
tree only the what-if CLI calls it
([TTFT, as built](slo-capacity-planner.md#ttft-as-built)). Repeat 1 of the
staircase ran, and every flag stays ([as built, M2](#as-built-m2)). The second
staircase's first repeat failed bucket B on the shape of the prefill side, which
is now priced as processor sharing ([as built](#as-built-processor-sharing)). Joint
prefill and decode sizing is [proposed](#proposed-joint-prefill-and-decode-ttft-sizing),
not built. The companion to
[what-if capacity](slo-capacity-planner.md), which owns the questions, the
decode model, the CLI and the PR plan. This page owns the TTFT model: what it
is made of, the router's flow-control queue, how the parts combine into a
percentile, and the staircase run that validates it.

**Audience:** contributors.

## In one paragraph

TTFT is end to end, as the client sees it. It is the wait before prefill
(router queue and engine queue), the request's own prefill, the KV transfer and
the decode side's admission wait and first step. Prefill capacity is a token
throughput `V_P`, taken only from sources that cannot over-state it. Below the
point where EPP flow control starts holding requests, the router queue is empty
and each pod queues for itself, so the model is per pod, with the measured
imbalance. The parts are combined into a percentile by convolution where they
are independent and by an optimised union bound where they are not; never by
adding percentiles.

## The terms

    TTFT = W_router + W_engine + own_prefill + W_decode + D(I, ITL(k_d)) + R

The design priced decode's part as `T_xfer + ITL(k_d)`. The staircase run
showed that sum 13 to 101 ms short, so `D + R` replaced it on a decode line
([the decode term](#as-built-the-decode-term)). `T_xfer` and `ITL(k_d)` below
are the old sum, which stands only as a flagged fall-back.

| Term | What it is | Model |
| --- | --- | --- |
| `W_router` | EPP flow-control queue: requests held before dispatch | Almost always 0 below the gate's onset. Above the prefill onset it has no formula of its own: the per-pod wait covers `W_router + W_engine` together. Above the decode onset, not priced ([the router queue](#the-router-queue-flow-control)) |
| `W_engine` | The prefill pod's own queue | [Per pod, with the imbalance `f`](#the-engine-queue). As built, the pod shares itself between its prompts instead, and `W_engine` is the stretch ([processor sharing](#as-built-processor-sharing)) |
| `own_prefill` | `a + I/V_P`, this request's own prefill | [Prefill capacity](#prefill-capacity) |
| `T_xfer` | KV transfer to decode (P/D) | `I·κ/BW` once `nixl_xfer_time_seconds` is collected; omitted and flagged until then. As built, a processor-sharing stage on the decode pod, out of `D` ([processor sharing](#as-built-processor-sharing)) |
| `W_decode` | Waiting for a decode slot | [The birth-death chain](#decode-admission) |
| `ITL(k_d)` | Decode's first step, which streams the first token (P/D) | The decode line at its fill |
| `D(I, ITL)` | The decode engine's TTFT: the KV pull and the steps a request waits on (P/D) | `base + perToken·I + perStep·ITL`, learned per variant |
| `R` | The hop between the router and the engines | The router's mean less the engines', learned per variant |

| | Flow control off (the upstream default) | Flow control on |
| --- | --- | --- |
| **P/D** | `W_router` = 0; every other term | Below the onsets: as off, approximately. Above the prefill onset: `W_router` holds the excess, engines hold about the detector's threshold. Above the decode onset: not priced |
| **Aggregated** | `W_router` = 0; `W_engine` is the pod's queue, which also holds decode admission; `own_prefill` produces the first token, so no `T_xfer`, `W_decode` or `ITL(k_d)` | As P/D: as off below the onset |

Flow control is off by default upstream, but this repository's scale-to-zero
installs turn it on (`deploy/lib/epp-flow-control.values.yaml`), so both
columns matter. With it off, the router admits everything; sheddable requests
(priority below 0) are rejected under saturation rather than queued.

## What is derivable today

The inputs are inventoried in [one table](slo-capacity-planner.md#what-exists).
Three derivations matter here:

- **On a P/D prefill pod**, the sidecar asks for one token, so
  `AvgServiceTime` is the prefill time and `AvgTTFT − AvgServiceTime` is the
  engine wait. That difference holds a constant overhead (tokenization,
  scheduling, the HTTP path): on `ols-sweep` it is never below 2.5 ms with no
  queue. Its low-load minimum is added to `a`. The wait derivation works for
  vLLM and SGLang (SGLang's service time is e2e minus `queue_time`).
- **On an aggregated pod** it does not work: service time includes decode, and
  `AvgServiceTime − (OL − 1)·ITLPerToken` subtracts two large numbers. The
  per-stage `request_prefill_time_seconds` is needed.
- **`ols-sweep`** has one prefill pod that never queued. It supports `a`
  and the low-load slope, not `V_P` or `f`. `a` is 11.04 ms: an
  intercept of 8.36 ms, fitted against the effective prompt over low-load
  minutes, plus the least overhead, 2.68 ms. An earlier
  revision said 10.1 ms: that was an intercept fitted against
  `AvgInputTokens`, the long-window prompt mean, not the minute the service
  time covers, and it left the overhead out.
- **Long prompts bend the line.** The per-token prefill cost rises with the
  prompt, from attention. On the TTFT track's run 3 (Qwen3-0.6B at TP1 on an
  H200, low load at 8k, 30k and 37k prompts) the medians cost 8.3e-6 s a
  token from 8k to 30k and 1.33e-5 from 30k to 37k. A straight line through
  the 86 low-load rows meets the axis at −52 ms, and P1 refused `a`
  (`NonPositiveIntercept`). So `a` is the intercept of a convex curve
  `s0 + b·I + c·I²` (`c > 0`) where the minutes can carry it, and of the
  line otherwise (`prefill/fixed.go`, `Estimate.AModel`):
  - **Clusters.** The minutes are sorted by prompt length. A minute within
    10% (`MaxPromptDrift`) of the one before it joins its cluster, and no
    cluster is wider than 1.2x (`MaxClusterWidth`). Without the cap,
    neighbours a few percent apart chain into one cluster: run 3's 86 rows
    from 29k to 38k do, and so does a slow drift from 8k to 37k.
  - **The curve.** It is fitted when at least 4 clusters of at least 3
    minutes each are left, so a fourth length checks the curve the other
    three set. The longest cluster's median prompt must be at least 3x the
    shortest's. Fewer clusters, or a narrower spread, keep the line.
  - **The fit.** The curve goes through each cluster's median prompt and
    median service time, weighted by relative error (1/s²), because the
    noise grows with the service time. Medians, because one slow minute no
    state identifies (run 3's 0.375 s at 38k, against 0.30 s for the other
    minutes at that length) moves a fit through the minutes. One minute 25%
    off, anywhere in runs 3 and 4 pooled, moves the fit through the minutes
    by up to 0.55 ms and the fit through the medians by up to 0.14 ms.
  - **Refusals.** A fitted `c` or `b` that is not above zero keeps the
    line (`ConcaveFit`, `NegativeSlopeFit`), as does a solve the clusters
    cannot carry (`SingularFit`). An intercept that is not above zero is
    refused. The curve's intercept carries a standard error, and one above
    half of the intercept is refused (`AUncertain`). The error has two
    parts: the minutes' relative noise within their clusters, pooled over
    the clusters and at least 1% (identical minutes would otherwise read
    none), and noise a whole cluster shares, read from the curve's lack of
    fit beyond what the first part explains. On 2,000 seeded pods it is
    conservative by about 1.2 times: one standard error covers 78% of the
    intercepts, against 68% for an exact one.
  - **Extrapolation.** `a` is not observed below the shortest prompts the
    fit reads; it is extrapolated there. If the true curve is flat or
    concave below them, the extrapolated `a` reads low, which is the unsafe
    side: it prices TTFT low and asks for fewer replicas. So an intercept
    whose shortest prompts are over 2,500 tokens (`MaxAShortestPrompt`) is
    refused (`NoShortPrompts`, `AExtrapolated`), for the curve and the line
    alike. The curve's shortest is its shortest counted cluster's median;
    the line's is its second shortest minute, as its spread test reads it.
    So with the switch on, a deployment whose prompts are all long has `a`
    refused even where it keeps the plain line, where P1 shipped published
    the line's intercept.

  Three recordings agree on 8 to 9 ms, and an intercept extrapolated from
  long prompts reads low:

  | Recording | Prompts | Model | Intercept | SE |
  |---|---|---|---|---|
  | ols-sweep | 1k, 6k | line | 8.36 ms | |
  | run 4 | 1k, 2k, 4k, 16k | curve, 4 clusters | 8.67 ms | 0.52 ms |
  | runs 3 and 4 pooled | 1k to 38k | curve, 7 clusters | 7.95 ms | 0.47 ms |
  | run 3 | 8k, 30k, 37k | line, 3 clusters | refused, `NoShortPrompts` | |

  Over all 185 low-load rows of runs 3 and 4, a relative-error fit gives
  7.98 ms (SE 0.10). The curve through pooled minutes from a shortest
  cluster at 1k gives 7.98 ms, at 2k 6.91 ms, and at 4k 5.55 ms; a
  straight line on prompts under 3k gives 8.86 ms, under 6k 7.53 ms, and
  under 8k 5.10 ms. The 2,500-token cutoff admits a shortest cluster at 2k,
  at most about 14% low, and refuses one at 4k, about 30% low. Run 3 alone,
  fitted through its three medians, gave 3.69 ms: half the agreed value.
  An earlier revision said the medians of run 3's 86 rows give 2.8 ms; that
  came from grouping the rows by sweep level, not from this code, which
  admits 15 of those rows.

  Run 4 also shows the line P1 shipped reading low inside its own spread:
  over 1k to 16k it published 0.84 ms. With the curve on, the line is held
  to the same 2,500-token cutoff. The replay switch is
  `prefill-a-quadratic`.

## Prefill capacity

A vLLM prefill pod schedules prompts first come, first served, under a token
budget per step (`--max-num-batched-tokens`). It is a work-conserving server of
prompt tokens at rate `V_P`. A prompt's own prefill duration stretches when
other prompts share its steps (`constants.VLLMRequestPrefillTimeSeconds` says
so), so it is not a property of the pod.

**The fixed cost `a` enters only the request's own latency.** Measured at low
load, where a prompt has a step to itself, most of it is paid per step under
load, and busy-minute `V_P` is net of it. Utilization and the queue use work
alone: `S_work = I_eff/V_P`, `ρ = λ·E[I_eff]/V_P`.

**`V_P` comes only from sources that cannot over-state it**, in this order:

1. **Busy minutes.** The pod's queue was above zero on every sample in the
   minute (`min_over_time` of `num_requests_waiting` positive), and decode was
   not back-pressuring it (the test `computeK2` applies,
   `k2ReasonObsDownstream`). `V_P` is the median of `PrefillComputedTokenRate`
   over at least 5 such minutes. On P/D this is rare: saturation has never
   recorded a non-back-pressured prefill saturation there. As built, the
   minimum is the per-pod `QueueMin`, read in a namespace with a TTFT target
   ([Q1](slo-tiers.md#as-built-q1)) and while a learner recording is on. A minute on a KV-full prefill pod is not busy, and only
   the last 30 minutes are read. A busy minute reads at or below capacity:
   data-parallel pods (flagged a lower bound) and stalls under-read
   ([P1 and busy minutes](slo-capacity-planner.md#pr-plan)).
2. **Observed peak under a queue.** The highest `PrefillComputedTokenRate`
   sustained over 3 minutes in which the pod held a queue on some sample.
   Throughput never exceeds capacity, so this is a lower bound. Flagged. A peak
   with no queue is only the offered load and is not used.
3. **Back-pressure bound**: saturation's prefill lower-bound window
   (`prefill_bound.go`). Flagged.
4. Otherwise `NoPrefillCapacity`. SGLang always lands here (no computed-token
   series).

The **low-load slope** of service time against prompt length is an upper bound,
not a source. A lone prompt runs as `T(I) = s0 + c·I`, and a busy pod's steps of
`B` tokens run at `B/(s0 + c·B) < 1/c`, so `1/slope ≥ V_P`: on `ols-sweep` about
520k tok/s against an offered peak of 30k. It is a validation signal only.
On a convex pod the bound is the curve's marginal rate `1/(b + c·I)` at the
prompt length `I` the busy pod was fed. A straight line's slope is a chord
of the curve, steeper than the curve at the short end of the fit, so
`1/slope` reads under the bound at the long lengths it spans. On run 3 the
line's 1/slope is 105k tok/s, under the curve's 147k at 30k, and a true
`V_P` there would be flagged `VPAboveSlopeBound` wrongly. So with the curve,
the bound is `1/(b + c·I)` at `VPPromptTokens`, the mean prompt length of
the minutes `V_P` rests on. With no such length (the back-pressure bound
records none) there is no bound and no flag.

**What the curve means for `own_prefill`.** P3 prices a request's own
prefill as `a + I/V_P`, which is linear in `I`. The curve is used for `a`
only, and `own_prefill` stays linear. That is consistent for a request at
the prompt length `V_P` was measured at: a busy pod's steps cost what that
length costs, so `V_P` there already includes the attention cost. A `V_P`
measured on shorter prompts reads high on long ones and over-promises.
With P1's curve, P3 now rescales `V_P` to the prompt asked, and without one it
flags a `V_P` whose prompt length
differs from the current mean by more than 1.2x either way (`VPShapeMoved`,
[S1 decisions, 2](slo-tiers.md#2-v_p-in-production); [after
P2](#as-built-p3-after-p2)).

**A limit of P3: a mix of lengths.** On a convex curve, a mix of prompt
lengths costs more than its mean length does (Jensen's inequality). P3
draws `I` from a gamma of the prompt spread ([as built](#the-model-as-built)),
but prices each length linearly at one `V_P`, so its mean own prefill is
`E[I]/V_P`: the cost at the mean. `VPShapeMoved` compares means, so it does
not see the spread either. On the pooled curve, an even mix of 8k and 52k
prompts (mean 30k) priced at a `V_P` read at 30k is 32% under its true
prefill time, and a single 52k request 40% under. Both over-promise TTFT.
This PR does not change P3; the row in [where it is most
wrong](#where-it-is-most-wrong) records it. Per-bucket `V_P` is [open
question 4](#open-questions).

`--assume-vp` supplies `V_P` by hand for an illustration or for hardware not yet
measured; every answer built on it is flagged ASSUMED.

## The engine queue

The design below is a first-come queue. The staircase showed a prefill pod
under chunked prefill does not queue at all: it shares itself between its
prompts, and the model now prices that ([processor sharing](#as-built-processor-sharing)).
The first-come queue stands with the switch off.

The router sends each request to one pod, which queues it. One run measured
439 waiting with 3 of 4 prefill pods idle: the prefill profile had one usable
scorer, so one pod won every request in a refresh window. So each pod is an
M/G/1 queue on prefill work, and the worst pod sees `λ_w = f·λ/N_p`:

    W̄ = λ_w · E[S_work²] / (2·(1 − ρ_w))                    (Pollaczek-Khinchine)
    θ = E[S_work]·(1 + Cs²) / (2·(1 − ρ_w)),   W̄ = ρ_w·θ
    P(W_engine > t) ≈ ρ_w · e^(−t/θ)

with `Cs² = E[I_eff²]/E[I_eff]² − 1`. For bursty arrivals `1 + Cs²` becomes
`Ca² + Cs²` (Kingman). The worst pod is stricter than the router's histogram,
which mixes pods; the mixture `Σ share_i·P(W_i > t)` may be used instead. The
pooled M/G/c (Allen-Cunneen) is printed as an optimistic bound only.

**Measuring `f`**: the busiest pod's share of prefill work over the mean share,
from per-pod `PrefillComputedTokenRate`, over minutes with at least 100
requests a pod on average, minus the expected max of `N_p` Poisson counts.
That de-bias errs both ways ([where it is most wrong](#where-it-is-most-wrong)):
low on a real imbalance at few requests, high on equal pods with variable
prompts. Sticky routing
(one pod wins a whole refresh window `w`) averages back toward 1 per minute; the
worst pod then gets `Ca² ≈ 1 + λ_w·w`, uncapped.

## Decode admission

A decode pod holding `n` requests completes them at

    μ_n = m / ( OL · ITL(m·KVreq/C) ),   m = min(n, c),   c = batchCap·ranks

a birth-death chain using the line at each state's own fill, clamped to the
fitted band (a fill below it reads the band's bottom). Above `c` the chain
is geometric, so the wait of a request that waits is exactly
`Exp(μ_c − λ_rep)`, and the wait probability is the chain's mass at `n ≥ c`. A
constant ITL (Erlang C at the mean fill) under-states the wait near the knee: in
the worked example 0.0007 against 0.032 at 0.85 of the knee. When the band's
top holds fewer requests than the cap, `c = floor(band.Hi·C/KVreq)`, flagged
approximate. Either way `μ_c` is the decode knee per replica, so the chain and
the knee saturate at the same rate. Residence is treated as exponential.

**One rule, flow control on or off:** `W_decode` is this chain, on the decode
pods. With the batch cap binding (the worked example's cap fill is 0.732), the
detector's KV term (0.8) never trips, so requests wait for slots on the decode
pods, not in the router. What flow control adds is the router hold above the
decode gate's onset, below.

## The router queue (flow control)

**What EPP flow control does.** Sources: upstream
`pkg/epp/flowcontrol/README.md` and the dispatch cycle in
`pkg/epp/flowcontrol/controller/internal/processor.go` (llm-d router,
gateway-api-inference-extension), the detectors' `config.go` under
`pkg/epp/framework/plugins/flowcontrol/saturationdetector/`, and this repo's
[router configuration](../reference/router-configuration.md).

- Off unless the `flowControl` feature gate is on.
- Requests wait in per-flow queues in the EPP. Each dispatch cycle releases at
  most one request.
- **The gate.** The default `utilization-detector` scores each endpoint
  `max(queueDepth/5, kvUsage/0.8)` and takes the unweighted **mean**. On P/D it
  scores the prefill and decode stages apart (unlabelled pods count as decode,
  interleaved pods in both) and gates on the worse. At a score at or above the
  band's ceiling (1.0 under the default static policy) the cycle stops for every
  band: head-of-line blocking. The `concurrency-detector` reads in-flight
  requests against `maxConcurrency` (100 per endpoint); in its default aggregate
  mode it compares totals, so it has the same mean problem, and only its
  `hybrid` mode bounds each pod.
- **Order.** Bands highest first; within a band the default fairness is
  `global-strict-fairness-policy`, a greedy global order across flows (with one
  flow, first come first served); within a flow, first come first served.
- **TTL.** Version dependent: v0.9.0 resolves `DefaultRequestTTL: 0s` (no
  expiry); newer EPPs default to 60 s; this repo recommends 30 s. Read the
  resolved `FlowControlConfig` from the EPP log.
- **Metrics** (`llm_d_epp_` prefix; upstream GIE also exports
  `inference_extension_` as a deprecated alias): `flow_control_queue_size` and
  `_bytes` (collected); `flow_control_request_queue_duration_seconds`
  (histogram, `priority` and `outcome`); `flow_control_pool_saturation`
  (`inference_pool`, `stage`); `flow_control_requests_total` (`outcome`,
  `priority`, `inference_pool`). The last two carry no model label: they are
  recorded per pool, and joined to a model only when the pool serves one, the
  guard the arrival-rate query already uses.

**When the router holds anything.** The gate stays open while the mean endpoint
score is below 1, so the engines fill first. A prefill pod's mean waiting count
is `ρ²(1 + Cs²)/(2(1 − ρ))`; at `Cs²` = 1 it reaches 5 at `ρ` = 0.854. A decode
pod's mean waiting count comes from the chain. The **onset** of each stage is
the rate at which its mean score reaches 1; the router queue is **almost
always** empty below it. The onset is a time average, while the gate reads the
current score, so below the onset the gate still closes for the share of time
`P(score ≥ 1)`, and every request waits while it does. In the worked example,
with one decode pod, that is 0.8%, 1.4% and 3.4% of the time at 11.10, 11.32 and
11.64 r/s (about `ρ⁶`, 0.3%, on the prefill side). The tool prints this closed
fraction in the basis. The union bound adds block 1 and `W_decode` rather than
overlapping them, so the answer is expected to stay conservative; the staircase
checks it.

**The model, in order of rate:**

1. **Below both onsets** (where every operating point sits in the worked
   example): flow control changes nothing. The per-pod engine model with `f`,
   and `W_decode` on the decode pods. `W_router` = 0.
2. **Above the prefill onset**: the router holds the excess, the engines hold
   about the threshold. The answer stays the per-pod model, flagged
   "above flow-control onset". Late binding can only reduce imbalance while the
   gate is closed, but one-per-cycle dispatch and stale scores can add delay,
   so the per-pod model is not proven conservative there; the pooled M/G/c is
   printed as the optimistic bound.
3. **Above the decode onset**: a closed decode gate stops dispatch for every
   request, prefill included, and a released request leaves its decode slot
   idle through prefill and transfer, so the gate is not work conserving. The
   tool does not price TTFT here. The decode onset is an extra knee under flow
   control: question 1 caps at it; question 2 flags an operating point above
   it. Decode admission is counted once, in `W_decode`, never also in
   `W_router`.

In the worked example the decode onset is 12.50 r/s (0.94 of the knee) and the
prefill onset 25.6 r/s, so both answers there are the same with flow control on
or off.

**Detecting it.** The EPP publishes the flow-control series with the gate on,
and the queue series is the signal. Its absence means off, flagged "flow control
inferred off (no queue series)" (a scrape gap looks the same). An on gate publishes
it at 0 while the queue is empty, once the router has had a request, and not before
([as built, M2](#as-built-m2)). The other series are not the signal: wait buckets
at rate 0 were recorded within a run's first 3 to 4 minutes on runs `goldens.yaml`
describes as off. On the staircase they follow an on phase; the non-zero rates fit
its 5-minute window, and the rest of the cause is not known, here or on the earlier
runs. `FlowControlUnknown` is refused only when it is on and the detector's
thresholds are neither readable nor given (`--fc-queue-threshold`,
`--fc-kv-threshold`, `--fc-max-concurrency`).

**Bands.** The model is for one band under the static policy. With several bands
or a holdback policy, the answer is for the highest band, flagged.

## Combining the parts into a percentile

**Percentiles do not add.** In the worked example at p95, adding the parts'
p95s gives 224 ms, the optimised bound 231 ms, full independence 210 ms.

Two blocks:

1. **The prefill side**: the pre-service wait and the request's own work,
   `a + I/V_P + I·κ/BW`. `T_xfer` scales with the prompt like prefill does, so
   it is folded into the same function of `I` rather than convolved as an
   independent part. The wait is independent of the request's own work for
   first-come service with Poisson arrivals, so the two are convolved exactly.
   That fails for `edf` or `slo` ordering. The tool does not flag it: no recording
   says which ordering the router runs
   ([cut](slo-capacity-planner.md#ttft-as-built)).
2. **The decode side**: `W_decode`, driven by the same arrivals, so not
   independent of block 1.

For any split `α + β = 1 − q`:

    TTFT_q ≤ Q_{1−α}(block 1) + Q_{1−β}(W_decode) + ITL(k_d)

holds whatever the dependence between the blocks. The tool **chooses `β` by a
one-dimensional search** to minimise the sum. When decode's wait probability is
below `1 − q`, the best `β` is about that probability and the decode term is 0;
an even split would instead switch the term on abruptly where the probability
crosses `(1 − q)/2`. The figure for independent blocks is printed beside it.

The bound is an upper bound on the quantile of the sum of the **modelled**
parts, never below their independent combination. It does not correct a part
that is itself wrong (the exponential tail, Poisson arrivals, an omitted
`T_xfer`).

## Where it is most wrong

| Limit | Direction |
| --- | --- |
| Wait tail at prompt cv above 1: `ρ_w·e^(−γt)`, `γ` the Cramér-Lundberg root ([as built](#the-model-as-built)) | Under-promises against a Lindley simulation (cv 2 and 3, `ρ` 0.3 and 0.6): by 3–28% at p90, 4–16% at p99 and 3–12% at p999; not a proven bound. Kingman's `1/θ` alone over-promised by up to 10% here |
| Wait tail at prompt cv below 1: Taylor's bound `min(ρ_w, C·e^(−γt))` | Under-promises, provably: by 8–18% at p90, 4–12% at p99 and 4–10% at p999 against a simulation (cv 0 and 0.5), and 1–17% against Erlang's exact M/D/1 wait. The exponential body it replaced read an M/D/1 p90 sojourn 10% short at `ρ` 0.2 |
| Prompt cv above 3 | Either way, and not monotone: the gamma's p90 can fall as cv rises at very low load, since most of its mass sits below the mean. S1 passes no cv above 3, assumed or measured (it assumes 2); nothing here is tested above it |
| Wait tail with bursty arrivals (`Ca²` ≠ 1): the Poisson tail stretched in time by `(Ca² + Cs²)/(1 + Cs²)`, never decaying faster than the work's own tail rate | Not checked against a simulation of a bursty queue. It is continuous at `Ca²` 1, never falls as `Ca²` rises (`TestProperty_TTFTNeverFallsWithCa2`), and tends to Kingman's `1/θ` in heavy traffic; below that it is an interpolation. Kingman's `1/θ` alone is no conservative side: above `cv` 1 it decays faster than Poisson's root, so `Ca²` just over 1 priced better than Poisson (5.53 against 5.26 r/s on the worked fleet at 300 ms p95, `cv` 2) |
| Poisson arrivals on bursty traffic | Over-promises. The CLI's `--arrival-cv2` states `Ca²` for the prefill queue; decode admission stays Poisson |
| Per-pod model above the flow-control onset | Either way; the pooled bound printed beside it over-promises |
| Dispatch-gate delays (scores up to 200 ms stale, a 50 ms candidate cache) | Over-promises, slightly, near the onset |
| One band, static ceiling | Over-promises for low bands, under-promises for the highest |
| TTL eviction (only where a TTL exists) | Evicted requests are not in the router histogram: the ground truth reads high near the TTL |
| Proxy hop before the EPP | Over-promises by that hop |
| `T_xfer` omitted (not collected) | Over-promises |
| `D` fitted on the whole prompt and priced at `E[I_eff]` | Over-promises with prefix hits, by `perToken·(I − I_eff)`, down to the old sum, which floors it: the pull moves the whole prompt. Flagged `DecodeTermPrefixHits` past 10% of the prompt. No committed run has hits, so this is not measured |
| `D` and `R` added at the mean to every percentile, but for their per-token parts | Over-promises at the tail. On the staircase run, bucket A's p90 and p95 read 9% and 13% under on average with the mean 8% over |
| `D` read past the prompts or step times it was fitted on | Either way, and not flagged: the plane is a line in each. `MaxDecodeQueue` keeps the behind-on-pulls minutes out of the fit, so `D` does not reach them, and they read far above it |
| `AssumedDecodeStepSlope`, `AssumedRouterHopSeconds` (no fit) | Under-promises if they stand above the fleet's true figures, which is the point. Each is flagged, and a refusal or a move of 25% that one alone decides says so. A long prompt may hop further than 20 ms |
| `V_P` from a lower bound | Under-promises |
| A mix of prompt lengths priced at its mean on a convex prefill curve ([a limit of P3](#prefill-capacity)) | Over-promises: an even 8k/52k mix at a `V_P` read at 30k is priced 32% under its prefill time. The rescale ([after P2](#as-built-p3-after-p2)) moves `V_P` to the mean, not the mix, and `VPShapeMoved`, comparing means, does not flag it |
| `V_P` rescaled along the curve with no fixed cost per step, `s(I_vp)/s(I)` | Under-promises for a longer prompt only if P1's low-load `b` and `c` are the busy step's per-token cost, which is not measured: on run 2 at 37k it read 1.5% above the served `V_P` (over-promises), covered by the margin only from `ρ` 0.6. No-act (`VPScaledToPrompt`) until a second point confirms it. Held (not raised) for a shorter prompt |
| The decode engine's TTFT added at its mean to every percentile | Over-promises at the tail by `(r − 1)·D`, `r` decode's p90 over its mean, 1.1–1.8 on the P2 runs (coarse buckets): up to about 20% of the router's p90 at `r` 1.8 (37k: about 127 ms of 0.795 s, 16%; 1k: about 10 ms of 0.047 s, 21%). At each level's own measured `r` it is less: 37k `r` 1.48, about 76 ms (10%); 1k `r` 1.54, about 7 ms (14%) |
| The decode engine's TTFT held at the rate it was measured at | Either way: `RateForTTFT` extrapolates to other rates with `D` frozen, and decode's own queue at a higher rate is not in it |
| `V_P` from the peak under a queue: a max over up to 358 overlapping 3-minute windows' minimums of a one-minute `rate()` | Over-promises, by about the noise of a one-minute rate |
| `f` de-biased for Poisson arrivals, on a real imbalance | Over-promises: the busiest pod's share holds little chance excess, but the equal pods' excess is subtracted anyway. Four pods at 2:1:1:1 (`f` 1.6) read about 1.50 at 100 requests a pod (6% low) and about 1.57 at 1500 |
| `f` from computed-token shares with variable prompts | Under-promises: a pod's token count varies about `√(1+Cs²)` times its request count, and only the request count's excess is subtracted. Four equal pods with `Cs²` = 1 read about 1.03 at 100 requests a pod |
| Worst-pod percentile | Under-promises |
| The mean TTFT is the worst pod's mean | Under-promises the fleet's mean, which mixes in the quieter pods |
| Question 1 capped at the decode onset | Over-promises: the onset is where the mean score reaches 1, and the gate reads the current score; `E[max] ≥ max E`, so the gate already closes for part of the time below it |
| The union bound | Under-promises against the true combination of the modelled parts; the independent figure beside it over-promises |

## Ground truth and the staircase

**The ground truth is the router's TTFT histogram**,
`llm_d_epp_request_ttft_seconds`. It starts at the EPP's request headers,
before flow control, and misses only the proxy hop and requests that never
finish. The mean router TTFT is also checked against the sum of the parts'
means, each from its own series ([inventory](slo-capacity-planner.md#what-exists));
the residual is the proxy hop and scheduling.

The staircase is the TTFT pass mark (decode is validated against the goldens).
**The prediction is made before the run**, from a separate warm-up recording.

| Item | Specification |
| --- | --- |
| Fleet | Fixed `N_p` = 2, `N_d` = 2; ScaledObjects paused (`autoscaling.keda.sh/paused`) |
| Load | [`hack/benchmark/staircase_load.py`](../../hack/benchmark/staircase_load.py), behind `make benchmark-staircase-run` ([as built](#as-built-the-staircase-tooling)). Streamed, Poisson on A and B: guidellm has no bucket C |
| Buckets | A: prompts 1000, answers 1000 (decode-bound). B: prompts 8000, answers 200 (prefill-bound). C (bursty): B's shape with arrivals at an interarrival `Ca²` of 4, the squared coefficient of variation P3's `ArrivalCV2` takes. The arrivals are a renewal process: i.i.d. gaps from a balanced two-phase hyperexponential with mean `1/λ` and SCV 4, which is on/off traffic with exponential phase lengths. The fallback is batches at Poisson epochs: a batch of `k` simultaneous requests gives gaps of SCV `2k − 1`, so `k` = 2 (3) and `k` = 3 (5) bracket 4. The harness measures `Ca²` per step from the request send times and records it; a step outside 3 to 5 is rerun. guidellm's built-in rate profiles are constant or Poisson, which is why bucket C has a driver of its own |
| Flow control | Each bucket twice: off, then on (utilization detector, one band, static ceiling). Bucket C is predicted with the measured `Ca²`, not Poisson |
| Start | 40% of the predicted knee |
| Steps | +10% to 70% of the knee, then +3% to 110% |
| Hold | 10 minutes per step; the first 3 discarded |
| Measured | Router TTFT p90 and p95, mean TPOT and per-request TPOT p90 (router TPOT histogram, and the driver per request) over the last 7 minutes; with flow control on, `pool_saturation` per stage, the direct test of the onset |
| Runs away | TTFT p90 above twice the target for the whole window |
| Repeats | 3; the statistic is the median over runs, per bucket |
| Recorded | A learner recording on a build with the [TTFT recorder fields](analyzer-learner.md#29-recorder-fields-for-ttft-and-capture); per-pod `num_requests_waiting` |

**Measured rate at SLO**: the highest step rate, interpolated between steps,
whose TTFT percentile and TPOT (mean, and per-request percentile) meet the
target. **Pass**: at the operating point, a predicted rate more than 10% above
the measured one fails; more than 25% below fails. The same mark is applied
separately to the mean TPOT and to the per-request TPOT percentile. **Noise floor**: an M/G/1 queue at `ρ` 0.85 relaxes in about
`E[S](1 + Cs²)/(1 − √ρ)²`, about 330 `E[S]` at `Cs²` = 1, 90 s for an 8000-token
prefill at 30k tok/s; a step whose window holds fewer than 4 such times is
flagged and lengthened. Repeat 1 failed the form before the decode term, and
it chose the term, so it cannot validate that either. Validation is a fresh
frozen run: until one passes, every TTFT figure prints UNVALIDATED.

**Which bucket lifts which flag.** The SLO analyzer's S2 does not size on a
figure carrying a flag below until the bucket named passes the mark above,
in all its repeats ([S1 decisions](slo-tiers.md#s1-decisions)):

| Flag | Lifted by |
| --- | --- |
| `UnvalidatedTTFT` | Buckets A and B, flow control off and on, on a fresh frozen run of the current form |
| `AssumedPoissonArrivals` | Bucket C, flow control off and on, predicted at the measured `Ca²` |
| `AboveFlowControlOnset` (on a rate answer) | Buckets B and C with flow control on, with steps above the prefill onset |
| `FlowControlInferredOff` | Never by the staircase: it is a missing series, fixed by scraping it |

## As built (P3)

`internal/signals/whatif`, files `ttft.go`, `ttft_queue.go`, `ttft_result.go`
and `ttft_prefill.go`. A library: no switch, no caller in the controller, no
golden moves. The CLI wiring came later:
[TTFT, as built](slo-capacity-planner.md#ttft-as-built).

**Built before its validation.** The plan has P3 depend on P2, the
prefill-stress recordings kept as goldens. Those runs were blocked on cluster
access when P3 was built, so P3 was built against known truth and synthetic
fleets instead.
Every TTFT figure carries the flag `UnvalidatedTTFT`, and nothing here has
been compared with a measured TTFT.

### The API

| Call | Answers |
| --- | --- |
| `TTFTAt(fleet, rate, q)` | Question 2's TTFT at a rate, at percentile `q`, or the mean for `q` = 0. Returns a `TTFT`: the union bound (`Seconds`), the independent figure, the pooled optimistic figure, the floor, the decode share `β`, the parts' means, `ρ_w`, `θ`, both knees and the onsets |
| `RateForTTFT(fleet, target)` | Question 1: the largest rate whose TTFT meets the target. An `Answer` whose `Basis.TTFT` carries the lower knee, the TTFT floor and the prefill side's own rate (`PrefillOnlyRate`); `BoundBy` says what set the rate. The TPOT fields of `Basis` (`KneeRate`, `Floor`, `Uncapped`) stay 0 |
| `PrefillKnee(prefill)` | `N_p·V_P(I)·(1 − m)/(f·E[I_eff])`, in `Basis.TTFT.KneeRate`: `V_P` rescaled to the prompt and less the margin ([after P2](#as-built-p3-after-p2)) |
| `FlowControlOnsets(fleet)` | Each stage's onset; zero with flow control off |
| `PrefillFromEstimate(est)` | Maps P1's `prefill.Estimate` to `V_P`, `a`, `f`, the prompt `V_P` was read at and, with the curve, its `b` and `c`; sets the margin `DefaultVPMargin`; carries P1's refusals |

A `TTFTFleet` is a `Prefill`, an optional `*Decode`, a `DecodeTTFT` and a
`FlowControl`. With no decode line, `W_decode` is not priced, and decode's
part is the decode engine's measured TTFT, `DecodeTTFT`, flagged
`DecodeEngineMeasured` ([after P2](#as-built-p3-after-p2)); unmeasured, it
is left out and flagged `DecodeSideOmitted` and `DecodeTTFTUnmeasured`.
 This is the shape S1
needs to price one prefill variant in shadow; the rate per replica is the
answer over `N_p`, priced at the actual `N_p` with `f` held. With flow control on and no decode role, the
decode onset cannot be placed: the answer is flagged `DecodeOnsetUnchecked`,
not refused, and may stand above the onset. The operating-point cap is not
applied: as on the decode side, the caller applies it to the smaller of the
roles' rates.

### The model as built

    ρ_w   = f·λ·E[I]/(N_p·V_P)
    θ     = (E[I]/V_P)·(Ca² + Cs²) / (2(1 − ρ_w))          Cs² = cv²
    γ     : λ_w·(M_S(γ) − 1) = γ        (Cramér-Lundberg root; M_S the work's MGF)
    s     = (Ca² + Cs²)/(1 + Cs²)                            1 at Poisson arrivals
    P(W_engine > t) = ρ_w·e^(−rt/s),  r = min(1/θ₁, γ)       cv ≥ 1, θ₁ the θ at Ca² 1
                    = min(ρ_w, C·e^(−γt/s)),  C = 1 − γ·cv²·E[S]  cv < 1 (Taylor's bound at s = 1)
                    = ρ_w·e^(−rt),  r = min(1/θ, 1/(cv²·E[S]))  bursty arrivals (Kingman)
    own   = a + I·(1/V_P + κ/BW),   I ~ Gamma(mean E[I], cv); constant below cv 1e-4
    block 1 = W_engine + own        (convolved exactly, closed form via the incomplete gamma)
    TTFT_q = min over β of  Q_{1−(1−q−β)}(block 1) + Q_{1−β}(W_decode) + ITL(k_d)
    mean  = ρ_w·θ + a + E[I]·(1/V_P + κ/BW) + E[W_decode] + ITL(k_d)
    floor = a + I_q·(1/V_P + κ/BW) + ITL(band.lo)

    with a decode line, the term replaces κ/BW and ITL(k_d) (after M2):
    κ/BW     → perToken_D + perToken_R          in the own latency, so at I_q
    ITL(k_d) → base_D + perStep·ITL(k) + base_R, k the actual fill, no clamp
    the floor reads it at k = 0

    with no decode line (after P2):  W_decode = 0, κ/BW = 0 when D is measured, and
    ITL(k_d) → D·max(1, E[I]/I_D)      D the decode engine's mean TTFT, measured at I_D
    V_P → V_P(I)·(1 − m(ρ₀))           rescaled to the prompt, less the margin near the knee

- **The wait tail errs only toward more replicas.** At cv 1 the two decays
  agree and the tail is exact (M/M/1). Above 1, the Cramér-Lundberg root is
  the slower decay: Kingman's `1/θ` alone read a p99 6–11% short of a
  Lindley simulation (cv 2, 3), and the root now reads it 4–16% long. Below 1,
  an exponential body behind the atom under-states the wait just past it.
  The tail there is Taylor's bound on an M/G/1 wait,
  `C = sup_x P(S > x)/E[e^(γ(S−x)); S > x]`. For gamma work at cv ≤ 1 (an
  increasing failure rate) the sup is the limit, `C = 1 − γ/b`. That is 1
  for constant work, which is the Lundberg inequality, and `ρ` at cv 1, so
  the tail meets the exact M/M/1 one there and TTFT does not jump as cv
  crosses 1. For bursty arrivals there is no root without the arrival
  law, so the tail keeps Kingman's decay and never decays faster than one
  request's work. The mean wait stays Pollaczek-Khinchine's (Kingman's)
  `ρ_w·θ`, exact for M/G/1, rather than the tail's own mean.
- **The mean is the worst pod's**, like every figure here: the mean TTFT a
  request on the busiest pod sees, not the fleet's.
- A prompt cv below 1e-4 is priced as constant: the gamma's shape is then
  above 1e8, where the incomplete gamma cancels (cv 1e-7 read 8% high).
- `W_decode` is D2's admission chain (`DecodeAdmission`), bounded at 1e6
  slots. `ITL(k_d)` is the line at the closed-form decode fill, read at the
  band's bottom below it.
- The search over `β` is a log grid on `(0, min(P_wait, 1 − q)]` followed by
  a golden-section refinement. The even split is a candidate as well, so the
  answer is never above it.
- The independent figure convolves block 1 with `W_decode` (a two-rate
  hypoexponential). The pooled figure is Allen-Cunneen M/G/`N_p` with no
  imbalance. It describes a router that holds requests and dispatches each
  to the first free pod, which flow control does only above the prefill
  onset. Below the onset each pod queues for itself, and the pooled figure
  is an optimistic bound, not an estimate.
- **Flow control.** The prefill onset assumes the worst pod is at `f` and the
  rest split the remainder evenly. Each pod scores `Lq/threshold`, with
  `Lq = ρ²(Ca² + Cs²)/(2(1 − ρ))`, and the onset is where the mean score
  reaches 1. Prefill KV is not scored. The decode onset is where a balanced
  decode pod's `max(Lq/threshold, fill/KVthreshold)` reaches 1, with `Lq`
  and the fill taken from the chain. At or above the decode onset TTFT is
  refused (`AboveDecodeOnset`), and question 1 caps at it. At or above the
  prefill onset the answer is flagged `AboveFlowControlOnset`. Below both,
  flow control changes nothing. The gate's closed fraction `P(score ≥ 1)` is
  **not built**.
- **The question-1 cap at the decode onset is not conservative.** The onset
  is where the mean endpoint score reaches 1, while the gate acts on the
  current score. Since `E[max] ≥ max E`, the gate already closes for part of
  the time below the onset.
- **The gate rule** (the mean over endpoints of
  `max(queue/threshold, kv/KVthreshold)`) is the newer llm-d router's
  utilization detector, not GIE v1.2.1's. On a v1.2.1 EPP the onsets are not
  these.
- **Refusals**: `NoPrefillCapacity`, `NoFixedLatency`, `NoImbalance` (more
  than one pod with no `f`; `f` = 1 is never assumed silently),
  `NoPrefillReplicas`, `FlowControlUnknown` and `AboveDecodeOnset`, plus
  `Saturated` at the prefill knee, and `NoTraffic`, `TargetBelowFloor` and
  `InvalidInput` as on decode.
- **Flags**: `UnvalidatedTTFT` (always), `AssumedVP`, `VPLowerBound`,
  `VPAboveSlopeBound`, `AssumedImbalance`, `AssumedPromptSpread`,
  `TransferOmitted`, `DecodeSideOmitted`, `FlowControlInferredOff`,
  `AboveFlowControlOnset`, `HeldByDecodeAdmission` and
  `DecodeOnsetUnchecked`; after P2, `DecodeTTFTUnmeasured`,
  `DecodeEngineMeasured`, `VPScaledToPrompt` and `VPShapeMoved`
  ([after P2](#as-built-p3-after-p2)). When decode
  admission holds the TTFT rate below what the prefill side alone allows,
  the answer is bound by `TTFT` and then `decode admission`. The worked
  example's 11.10 r/s at p95 is such a case: decode takes `β` ≈ 0.03 of the
  tail.

### What the tests check

- **Known truth.** M/M/1 sojourn (exact at cv 1, `f` = 1, one pod), the
  Pollaczek-Khinchine mean, Allen-Cunneen against an independent Erlang C,
  the Cramér-Lundberg root against its equation, and the incomplete gamma
  against its closed forms.
- **Never below the reference.**
  - At cv 0 the wait's quantiles are compared with Erlang's exact M/D/1
    waiting-time distribution, summed in 1024-bit floats. The model is at
    or above it at every `ρ` from 0.1 to 0.95 and `q` of 0.9, 0.99 and 0.999.
  - The sojourn at p90, p99 and p999 is compared with frozen figures from a
    Lindley simulation outside the code (numpy; 3M arrivals; seeds 20261104,
    7, 99 and 4242). Each figure is the least of the four seeds, and the
    model must reach it less 1%. The seeds spread up to 1.8% at p99 and
    6.5% at p999 (cv 3, `ρ` 0.6). The cases are cv 0, 0.5, 1, 2 and 3 at
    several `ρ`.
  - The model over the least seed:

    | cv | `ρ` | p90 | p99 | p999 |
    | --- | --- | --- | --- | --- |
    | 2 | 0.3 | 1.150 | 1.099 | 1.083 |
    | 2 | 0.6 | 1.076 | 1.039 | 1.048 |
    | 3 | 0.3 | 1.278 | 1.159 | 1.118 |
    | 3 | 0.6 | 1.102 | 1.050 | 1.032 |
    | 1 | 0.6 | 1.002 | 1.003 | 1.004 |
    | 0.5 | 0.2 | 1.106 | 1.089 | 1.069 |
    | 0.5 | 0.3 | 1.115 | 1.080 | 1.057 |
    | 0.5 | 0.6 | 1.076 | 1.042 | 1.037 |
    | 0 | 0.2 | 1.175 | 1.124 | 1.100 |
    | 0 | 0.3 | 1.152 | 1.120 | 1.085 |
    | 0 | 0.6 | 1.109 | 1.063 | 1.056 |

    The pre-round-1 tail read the cv 2 / 3 p99s at 0.937 / 0.944 and
    0.895 / 0.907, and fails this test.
  - TTFT never falls as the prompt cv rises from 0 to 3 (p90, p95 and p99,
    at 0.3 to 0.85 of the knee), across the change of form at cv 1.
- **The worked example** is reproduced to 1e-6: 160 / 154 ms at p90, 231 / 210 ms at p95, floor
  91.6 ms, 11.64 / 11.10 r/s for 200 ms, onsets 12.50 and 25.6 r/s. Six
  cases with no closed form (cv 0.5 and 2, bursty arrivals with `f` 1.5
  over four pods, a transfer term, prefill alone, constant prompts) are
  checked against a scipy reference that integrates a different form of the
  convolution.
- **Properties** on seeded random fleets. TTFT rises with the rate and with
  the prompt, and falls with either role's replicas. Question 1's rate
  rises with the target and the prefill pods, and round-trips to 1e-6. The
  model refuses at the knees and at the decode onset. The union bound is
  never below the independent figure or above the even split, and the
  pooled figure is never above the per-pod one. NaN and infinite inputs are
  refused.
- **Mutants.** Each mutant listed in the P3 commit messages is killed by a
  named test, including one that reverts each part of the tail rule.

### What is UNVALIDATED, and what P2 must check

1. **`V_P` as a throughput.** Does a pod under a queue serve prompt tokens
   at the busy-minute `V_P`, and does it stay constant with prompt length
   (open question 4)?
2. **The wait tail.** How far above per-pod `request_queue_time_seconds`
   (R1's histograms) does the tail rule sit, at the real prompt spread? It
   is at or above the references it is checked against (Erlang's exact
   M/D/1 wait and a Lindley simulation of M/G/1, [the tests](#what-the-tests-check)),
   which is not the same as conservative on a real pod: a real pod's chunked
   prefill is not the M/G/1 server it assumes, and on P2's goldens the whole
   prediction reads up to about 33% under the router's p90 at light load
   ([after P2](#as-built-p3-after-p2)).
3. **The gamma prompt.** Is a gamma with the measured cv a fair stand-in for
   the prompt histogram? P2 should compare the floor and the low-load
   quantiles.
4. **`f` and the worst pod.** P1's `f` is biased both ways
   ([where it is most wrong](#where-it-is-most-wrong)). P2 must show
   whether the worst-pod quantile, the per-pod mixture or the pooled figure
   tracks the router's histogram.
5. **The mean is the worst pod's.** Compare it with the busiest pod's
   `AvgTTFT`, not the router's mean.
6. **`a` at load.** `a` is measured at low load and added to every request.
   Check whether it grows under a queue.
7. **The union bound's conservatism.** Compare the end-to-end p90 and p95
   against `llm_d_epp_request_ttft_seconds` across the step rates.
8. **Flow-control onsets.** Do `pool_saturation` per stage and the router
   queue start where the onsets say? This is the staircase's flow-control-on
   phase (M2), not P2.
9. **`T_xfer`**, once `nixl_xfer_time_seconds` is collected (open question 3).

Until P2 and the staircase pass, no TTFT figure drives a replica count. S1
publishes in shadow only.

## As built (P2)

P2 commits the four prefill-stress runs as learner goldens and prices P3 on
them. **P3 as built before P2 does not pass.** At the prompt spread the runs
sent, it under-predicted the router's p90 and p95 TTFT at 11 of 14
light-load points, by 19% to 49%, and by about 90% at the one saturated
level it priced. P2 did not tune P3. The three model changes these
findings led to, and the table after them, are in [As built (P3 after
P2)](#as-built-p3-after-p2): there 9 of 14 light-load points are still
under. The tables in this section are P3 before that change; the frozen
table in `ttft_validation_test.go` is P3 after it.

### The goldens

All four are Qwen3-0.6B at TP 1 on H200, the P/D scenario, replicas pinned,
flow control off, the recorder on with every R1 field. Each was added at the
commit that recorded it and carried over; no earlier golden's output moved.
The [developer guide](../developer-guide/testing.md#learner-goldens) lists
them.

| Golden | Run | Fleet and load | Compressed |
| --- | --- | --- | --- |
| `ttft-three-pods` | 1 | 3 prefill + 3 decode, closed loop at 30k | 1.06 MB, whole |
| `ttft-saturated` | 2 | 1 prefill + 2 decode, closed loop to prefill saturation | 275 KB |
| `ttft-open-loop-long` | 3 | 1 + 1, Poisson at light load, 8k, 30k, 37k | 88 KB |
| `ttft-open-loop-short` | 4 | 1 + 1, Poisson at light load, 1k, 2k, 4k, 16k | 111 KB |

None needed trimming: the largest is a quarter of the 4 MiB limit.

### What P1 learns on main

`TestTTFTValidation_P1OnTheGoldens` replays each golden and reads P1's last
`prefill-capacity-observed` line.

| Run | `V_P` | `a` | `f` |
| --- | --- | --- | --- |
| 2 | **134,775 tok/s**, busy minutes, 28 | refused, `TooFewLowLoadMinutes` | `OnePod` |
| 3 | refused, `NoPrefillCapacity` | refused, `NoShortPrompts` (shortest prompts 8k) | `OnePod` |
| 4 | refused, `NoPrefillCapacity` | **11.58 ms**, the curve | `OnePod` |
| 1 | 115,621 tok/s, peak under a queue (lower bound) | refused, `NoPromptSpread` | **1.00**, 45 minutes |

No run alone gives P3 a fleet it can price: each lacks `V_P` or `a`. The
validation therefore takes **`V_P` from run 2** for every level, and **`a`
from run 4** (P1's curve, 11.58 ms) for run 4's levels. P1 refuses `a` on
runs 3 and 2, so their levels take **`a` = 10.86 ms**, the curve over runs 3
and 4 pooled (its 7.95 ms intercept plus run 4's least overhead 2.91 ms,
`TestQuadratic_Runs3And4PooledGiveA`): no golden replays the two runs
together, so the figure is taken from that test.

Before ev-shindin/llm-scaling-manager#475 (the curved `a`) merged, run 4's `a` was the line's 3.74 ms and
run 3's refusal `NonPositiveIntercept`. The rows priced at run 4's `a` moved
with it, by its 7.84 ms: 12 of 14 light-load points were under, 7 beyond the
bucket; now 11, 4 beyond the bucket. No other row moved.

### How it is priced

`TestTTFTValidation_AgainstTheRouter`, in `learnreplay`. For each level:

- **The rate and the prompt** are the means over the level's cycles of the
  prefill row's request rate and effective prompt (computed tokens over
  requests), from a minute after the level starts.
- **The fleet** is one prefill pod at `V_P` and `a`, the KV transfer per
  token measured from decode's `AvgKVTransferTime`, Poisson arrivals, flow
  control off, and **no decode side** (`DecodeSideOmitted`). No run fitted a
  decode ITL line under load: decode's KV stayed near 0.01. This is what S1
  prices with flow control off.
- **Two spreads.** `cv` 0.058 is the spread the load generator sent (±10%
  uniform). `cv` 2 is S1's assumption with no histogram.
- **The observation** is the router's TTFT histogram over its 300 s window
  at the level's last cycle, at p90 and p95, read as `histogram_quantile`
  does. Its buckets are coarse (25 to 50 ms, up to 200 ms wide at 0.4 s), so
  each verdict says whether it holds wherever in its bucket the true
  quantile lies ("beyond the bucket").
- **The mark** is S1's: more than 10% below the observed fails (UNDER), more
  than 25% above fails (over).

Run 2's first three levels (two in flight, no queue, the pod near
utilization 1) are not a Poisson queue, and its levels 4 and 5 are shorter
than the router's window, so only its last three levels are compared.

### The result table

Predicted against the router's TTFT, in seconds. `a` is run 4's own on run 4
and the pooled curve's on runs 3 and 2.

| Run | Level | Rate r/s | Router p90 | P3 p90, cv 0.058 | | P3 p90, cv 2 | | Router p95 | P3 p95, cv 0.058 | | P3 p95, cv 2 | |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 4 | 1k | 7.74 | 0.047 | 0.024 | **UNDER** | 0.051 | pass | 0.049 | 0.028 | **UNDER** | 0.074 | over\* |
| 4 | 2k | 5.35 | 0.048 | 0.036 | **UNDER** | 0.091 | over\* | 0.049 | 0.045 | pass | 0.135 | over\* |
| 4 | 4k | 3.09 | 0.092 | 0.059 | **UNDER** | 0.172 | over\* | 0.096 | 0.077 | **UNDER** | 0.259 | over\* |
| 4 | 16k | 0.48 | 0.316 | 0.184 | **UNDER** | 0.571 | over\* | 0.358 | 0.248 | **UNDER** | 0.892 | over\* |
| 3 | 8k | 1.75 | 0.098 | 0.126 | over\* | 0.332 | over\* | 0.110 | 0.142 | over | 0.504 | over\* |
| 3 | 30k | 0.20 | 0.570 | 0.330 | **UNDER**\* | 1.017 | over\* | 0.585 | 0.347 | **UNDER**\* | 1.603 | over\* |
| 3 | 37k | 0.19 | 0.795 | 0.413 | **UNDER**\* | 1.300 | over\* | 0.915 | 0.547 | **UNDER**\* | 2.043 | over\* |
| 2 | 30k, 32 in flight | 4.50 | 7.80 | refused, `Saturated` | | refused | | 7.90 | refused | | refused | |
| 2 | 30k, 96 in flight | 4.52 | 29.0 | refused, `Saturated` | | refused | | 29.5 | refused | | refused | |
| 2 | 37k, 64 in flight | 3.14 | 28.9 | 2.57 | **UNDER**\* | 11.39 | **UNDER**\* | 29.5 | 3.22 | **UNDER**\* | 14.90 | **UNDER**\* |

\* beyond the bucket: the verdict holds wherever in the router's bucket the
true quantile lies.

### Findings

1. **P3 under-predicts at light load, beyond the mark.** At the spread the
   runs sent, 11 of the 14 light-load points are more than 10% under, 4 of
   them below even the router bucket's lower bound. The ratio is 0.512 to
   0.805. **The cause is a term P3 leaves out.** The router's TTFT is close to
   the two engines' TTFTs added: the prefill engine's (`a` + `I/V_P` + its
   own overhead) and then the decode engine's (the KV pull and the first
   step: 12 ms at 1k, 127 ms at 30k, 159 ms at 37k).
   `TestTTFTValidation_TheResidual` finds the router's median less the two
   at −7 to +15 ms at 6 of 7 levels and 62 ms at 30k. P3 with no decode
   side omits the decode engine's part entirely, and the measured transfer
   (`T_xfer`) is only part of it. So this is the `DecodeSideOmitted` case
   failing, not the queue model. S1 publishes exactly this case with flow
   control off.
2. **`cv` 2 hides it by overcharging.** At S1's assumed spread, 13 of 14
   light-load points are over the mark, up to 4.6x. They are over only
   because a gamma at `cv` 2 puts a long tail on a prompt that had none. The
   1k p90 "pass" is that coincidence. The assumption spends replicas on a
   spread these runs did not have.
3. **At saturation, P3 cannot price, or under-predicts by 10x.** At 30k
   (32 and 96 in flight), the measured rate is 0.3% and 0.5% above the knee
   P3 computes from run 2's own `V_P`, so it refuses `Saturated`. S1 reads
   that as "unmet", which is the right side. At 37k the pod serves only
   116k tok/s, 14% under the `V_P` learned at 30k, so P3 reads utilization
   0.86 where the pod is saturated: it prices 2.6 s against 28.9 s. This is
   `V_P` depending on the prompt (open question 4); S1's `VPShapeMoved`
   (1.23x here, above its 1.2x) would flag it. A closed loop is also not
   Poisson: its queue is set by the in-flight count, which no M/G/1 model
   prices.
4. **8k over-predicts** (1.29x), the safe side. `V_P` learned at 30k
   charges an 8k prompt 59 ms of prefill where the pod took 28 ms at light
   load (289k tok/s at 8k), which outweighs the missing decode part there.
   The same over-charge is in run 4's points, which are under the mark
   regardless.

### What P2 checks, item by item

| Item | Finding | Test |
| --- | --- | --- |
| 1. `V_P` as a throughput | **Holds at the prompt it was learned on, not at another.** At 30k the pod serves 135,122 and 135,451 tok/s under a queue, within 0.5% of busy-minute `V_P`. At 37k it serves 115,953, 14% under | `WhatTheRunsShow` |
| 2. The wait tail | **Direction only.** At light load no request queued past the engine histogram's first bound (0.3 s), and P3's tail there is 1e-76 to 8e-3: at or above, as it must be. The bound is too coarse to say how far above. Under a queue the per-pod histogram is not comparable to an M/G/1 tail (closed loop) | `WhatTheRunsShow` |
| 3. The gamma prompt | **Not testable against a histogram:** vLLM's prompt buckets (1-2-5) put a level in one or two buckets, and every prompt above 20k in `+Inf`. The known spread, ±10% uniform, is cv 0.058, and `cv` 2 over-predicts by up to 4.6x (finding 2) | `AgainstTheRouter` |
| 4. `f` and the worst pod | **`f` = 1.00 at three pods** over 45 minutes. The busiest pod's raw share per cycle has median 1.03 and maximum 1.33 over 197 loaded cycles, so S1's floor of 1.5 holds every cycle and is conservative here. Untestable: an imbalanced fleet, and the router's view of a worst pod (run 1 recorded no busy minute) | `P1OnTheGoldens`, `WhatTheRunsShow` |
| 5. The mean is the worst pod's | **One pod**, so the worst pod is the pod. Its engine `AvgTTFT` is 0.34 to 0.71 of the router's median: the decode part again | `TheResidual` |
| 6. `a` at load | **It grows.** The prefill engine's TTFT beyond its queue and service time is 3 to 83 ms at light load (rising with the prompt) and 224 to 277 ms under a queue. P3 adds a constant `a` of 3.7 to 10.9 ms | `WhatTheRunsShow` |
| 7. The union bound's conservatism | **Not testable:** with no decode side there is no union bound. Needs a decode ITL line, which these runs did not fit | — |
| 8. Flow-control onsets | **Not testable:** flow control was off in every run (M2's staircase) | — |
| 9. `T_xfer` | **Measured, and not purely per token.** Decode's KV transfer is 3.9 µs a token at 1k, 3.0 at 4k and flat at 2.4 from 16k: a fixed cost per transfer plus a per-token one. A per-token figure from long prompts reads a short prompt's transfer low. Per request, not per chunk, at these lengths | `WhatTheRunsShow` |
| S1 item 5. The mean residual | The router's median less both engines' mean TTFTs: −7 to +15 ms at 6 of 7 light levels, +62 ms at 30k (median +7 ms). Within S1's 10 ms budget at the median; above it at 4k (+15 ms) and 30k (+62 ms). The router records no TTFT sum, so the median stands in for its mean | `TheResidual` |

### Open items

- **Price the decode engine's part on a P/D fleet without a decode line.**
  The measured decode `AvgTTFT` (or `AvgKVTransferTime` plus a first step)
  is the missing term. Until then, `DecodeSideOmitted` under-promises by
  19% to 49% here, and S2's no-act list already holds it out. S1 should
  publish it flagged as it does. *Done in [P3 after P2](#as-built-p3-after-p2),
  item 1, from the decode engine's mean TTFT.*
- **`a` under load.** The engine's own overhead grows from milliseconds to
  about 250 ms under a queue on these pods; P3's constant `a` misses it.
- **`V_P` by prompt length** (open question 4): 14% lower at 37k than at 30k.
  *Done in [P3 after P2](#as-built-p3-after-p2), item 2: `V_P` is rescaled
  along P1's curve to the question's prompt.*
- **A Poisson run near the knee** at a fixed prompt, to test the wait tail
  where it matters. A closed loop does not test it.
- **The decode side and the union bound** need a run that fits a decode ITL
  line (decode KV above 0.1).
- **PR ev-shindin/llm-scaling-manager#477** (per-pod-count `f`) changes nothing here: every compared level
  is one pod. P1 still refuses `a` on runs 3 and 2 alone, so their levels
  rest on the pooled figure.

## As built (P3 after P2)

[P2](#as-built-p2) (PR ev-shindin/llm-scaling-manager#482) priced P3 on the four prefill-stress goldens
and found it under the router's TTFT at 11 of 14 light-load points (12
before ev-shindin/llm-scaling-manager#475 raised run 4's `a`), and at a tenth of it at run 2's 37k
level. This change makes three model changes. Each errs toward
more replicas, and none is fitted to those recordings.
`UnvalidatedTTFT` stays on every figure.

### 1. The decode engine's TTFT with no decode line

On P/D the router's TTFT is the prefill engine's TTFT and then the decode
engine's: the KV pull and decode's first token. P2's residual (the router's
median less the two engines' mean TTFTs) is −7 to +15 ms at 6 of 7 levels.
P3 with `Decode == nil` priced prefill alone and so left out 12 ms at 1k
and 159 ms at 37k.

- **With a decode line**, nothing changes: `T_xfer + W_decode + ITL(k_d)`.
- **With no decode line**, decode's part is `TTFTFleet.DecodeTTFT`, the
  decode engine's mean TTFT, priced at `D·max(1, E[I]/I_D)`. `I_D` is the
  mean prompt it was measured at. Scaling a fixed cost by the prompt makes
  it high, so the term scales up for a longer prompt. For a shorter prompt
  it is held: the pull grows with the prompt, so a held term is also high.
  It is added at its mean to every percentile, which is the one part that
  under-states the tail, by `(r − 1)·D` with `r` decode's p90 over its mean
  (1.1–1.8 on these runs): up to about 20% of the router's p90. `D` is also
  held at the rate it was measured at, so `RateForTTFT` extrapolates with it
  frozen. `Seconds` and `PromptTokens` travel as a pair from one cycle, and
  `I_D` is `E[I_eff]` while the pull moves the whole prompt, prefix hits
  included.
- **The transfer is in it.** The decode engine's TTFT includes the KV pull,
  so `TransferSecondsPerToken` is not priced beside it, and `TransferOmitted`
  is not raised.
- **Flags.** Measured, the answer is flagged `DecodeEngineMeasured`, which
  is informational, not a hold; `DecodeSideOmitted` is not raised.
  Unmeasured (`Seconds` 0), the term is left out and the answer flagged
  `DecodeSideOmitted` and `DecodeTTFTUnmeasured`, which always coincide; no
  caller acts on them. It is never a silent zero. A negative or non-finite
  figure, a measured figure with a prompt under one token
  (`MinDecodeTTFTPrompt`; `PromptTokens` is required when `Seconds` > 0), or
  one whose term overflows at the prompt asked, is refused (`InvalidInput`).

- **Where it is learned:** S1, per decode variant, from the R1 field
  `domain.ReplicaMetrics.AvgTTFT` on the variant's Ready decode rows
  ([S1 decisions, 3](slo-tiers.md#3-what-s1-passes-to-ttftat-and-rateforttft)).
  `AvgTTFT` is a scaling query, collected every cycle (not record-only).

### 2. `V_P` at the question's prompt

At 37k the pod served 115,953 tok/s, 14% under the `V_P` of 134,775
learned at 31.8k. P3 held `V_P` and read utilization 0.86 where the pod was
saturated.

- **The rescale.** A busy pod's step of `B` tokens of prompts of length `I`
  costs about `s0 + (b + c·I)·B` ([prefill capacity](#prefill-capacity)), so
  `V_P(I)/V_P(I_vp) = (h + s(I_vp))/(h + s(I))`, with `s(I) = b + c·I` and
  `h = s0/B ≥ 0` unknown. P3 takes `h = 0`:
  `V_P(I) = V_P(I_vp)·s(I_vp)/s(I)`. For `I > I_vp` that is at or below the
  true ratio for any `h` **only if P1's low-load `b` and `c` are the busy
  step's per-token cost**, which is not measured; `V_P` is rescaled down and
  flagged `VPScaledToPrompt`, which is on S2's no-act list until a second
  measured point confirms it. For `I < I_vp` it would over-read the gain, so `V_P`
  is held at the learned figure, which the true one is at or above.
  `TestVPRescaleNeverAboveTheStepModel` checks both over `h` from 0 to
  1e-5 s a token, inside the step model only.
- **Its inputs** are P1's, through `PrefillFromEstimate`:
  `Estimate.VPPromptTokens` (`I_vp`) and, with `AModel` quadratic,
  `ASlope` and `ACurve` (`b`, `c`), into `Prefill.VPPromptTokens`,
  `CurveSlope` and `CurveCurvature`. On run 2 with the curve of runs 3 and
  4 pooled, the rescale reads 117,645 tok/s at 37k, 1.5% above the 115,953
  served (`TestVPRescaleOnRun2At37k` holds the sign): an over-read the step
  model says cannot happen. With the margin below it is 3.6% under, but the
  margin is whole only from `ρ` 0.6, so below that the over-read stands.
- **Fallback, with no curve** (the line, a refused fit, or no
  `VPPromptTokens`): `V_P` is held, and flagged `VPShapeMoved` when the mean
  prompt is more than `MaxVPShapeRatio` (1.2x) from `I_vp` either way, or
  `I_vp` is unknown. This is S1's rule, moved into P3 so that every caller
  gets it. An assumed `V_P` is neither rescaled nor flagged.

### 3. A margin on `V_P` near the knee

The wait grows as `ρ/(1 − ρ)`, so near the knee a small error in `V_P` is a
large error in TTFT. On the M/M/1 sojourn, a `V_P` read `(1 + e)` high puts
TTFT at `(1 − ρ)/((1 + e)(1 − ρ) + e·ρ)` of the truth. At `e` = 5% that
is 9.1% short at `ρ` 0.5, 10% at 0.55, 14% at 0.7 and 25% at 0.85.

- **Chosen: a margin on `V_P`, not a reported sensitivity.** A sensitivity
  figure beside the answer would leave the answer itself on the unsafe
  side, and S1 and S2 read the answer. The margin `m` = `DefaultVPMargin`,
  5%, is set by `PrefillFromEstimate`. It is ten times the 0.5% by which
  run 2's pod under a queue served away from busy-minute `V_P` at the prompt
  it was learned on. **It covers that measurement noise only, at the prompt
  length `V_P` was learned at.** It does not cover an error in the rescale
  or in pricing a prompt mix at its mean: an even 8k/52k mix is 32% under
  its prefill time ([where it is most wrong](#where-it-is-most-wrong)),
  six times the margin.
- **Where:** none below `MarginRampStart` (`ρ₀` 0.5, the worst pod's
  utilization at the unmargined `V_P`), all of it from `MarginRampFull`
  (0.6), and linear between, so TTFT has no jump in the rate. The ramp
  straddles 0.55, where a 5% error crosses the S1 mark's 10%
  (`TestVPMarginRampIsWhereTTFTTurnsSensitive`). Below it the error is
  within the mark on its own, and no replica is spent on it.
- **The knee** is `(1 − m)` of the unmargined one. `VPMargin` is at most
  `MaxVPMargin` = 1 − `MarginRampFull`, so the margin is whole before the
  margined pod saturates. `TTFT.VPMarginApplied` reports the share taken at
  the rate. The floor is at the unmargined `V_P`, since there is no queue
  at no load.

### The validation table after the change

`TestTTFTValidation_AgainstTheRouter`, priced as [P2](#how-it-is-priced) prices it
(`V_P` from run 2, `a` from run 4 or ev-shindin/llm-scaling-manager#475, flow control off, no decode
line), plus three inputs:

- the decode engine's mean `AvgTTFT` over the level, at the level's prompt;
- the curve: run 4's own fit (main's P1 fits a quadratic on run 4), and
  ev-shindin/llm-scaling-manager#475's pooled `b` 7.1815e-7, `c` 2.0280e-10 for runs 3 and 2, where main
  fits none;
- `V_P`'s prompt: run 2's `VPPromptTokens`, 31,816.

The mark is S1's: more than 10% under fails (UNDER), more than 25% over fails
(over). \* is beyond the bucket.

| Run | Level | Router p90 | p90, cv 0.058 | | p90, cv 2 | | Router p95 | p95, cv 0.058 | | p95, cv 2 | |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 4 | 1k | 0.047 | 0.032 | **UNDER** | 0.052 | pass | 0.049 | 0.036 | **UNDER** | 0.068 | over\* |
| 4 | 2k | 0.048 | 0.045 | pass | 0.088 | over\* | 0.049 | 0.055 | pass | 0.123 | over\* |
| 4 | 4k | 0.092 | 0.070 | **UNDER** | 0.160 | over\* | 0.096 | 0.090 | pass | 0.231 | over\* |
| 4 | 16k | 0.316 | 0.213 | **UNDER** | 0.523 | over\* | 0.358 | 0.282 | **UNDER** | 0.785 | over\* |
| 3 | 8k | 0.098 | 0.146 | over\* | 0.308 | over\* | 0.110 | 0.161 | over | 0.453 | over\* |
| 3 | 30k | 0.570 | 0.380 | **UNDER** | 0.923 | over\* | 0.585 | 0.393 | **UNDER** | 1.396 | over\* |
| 3 | 37k | 0.795 | 0.523 | **UNDER**\* | 1.373 | over\* | 0.915 | 0.714 | **UNDER**\* | 2.088 | over\* |
| 2 | 30k, 32 in flight | 7.80 | refused, `Saturated` | | refused | | 7.90 | refused | | refused | |
| 2 | 30k, 96 in flight | 29.0 | refused, `Saturated` | | refused | | 29.5 | refused | | refused | |
| 2 | 37k, 64 in flight | 28.9 | refused, `Saturated` | | refused | | 29.5 | refused | | refused | |

At the sent spread (cv 0.058), the light-load points under the mark fall
from 11 of 14 in [P2's table](#the-result-table) (12 at 602054e98, before
ev-shindin/llm-scaling-manager#475 raised run 4's `a` from 3.74 to 11.58 ms) to 9, and those beyond the
bucket from 4 (7) to 2. The ratios of the points still under rise from
0.51–0.81 (0.35–0.75) to 0.66–0.79, and the three
that now pass read 0.94 to 1.11. Run 2's 37k level, priced at 2.6 s
against 28.9 s, is now refused `Saturated`: the rescaled, margined knee is
below its rate, and S1 reads that as unmet, the right side. At cv 2 nothing
moves from over to under. The 8k level moves further over (1.29x to 1.50x),
because the decode term is added where `V_P` was already high for an 8k
prompt.

### What still fails, and why it is not tuned away

Nine light-load points remain under at the sent spread, and the change does
not close them. Two reasons, both outside what these three changes model:

1. **The router's p90 is not the sum of the two engines' means.** The
   engines' own TTFT histograms put decode's p90 at 1.1–1.8 times its mean
   and prefill's at 1.2–2.2 on these runs, read from coarse buckets. The
   model adds decode's part at its mean, and its
   prefill part (`a + I/V_P` at cv 0.058) has almost no spread. Adding the
   two engines' p90s reads 0.039 against the router's 0.047 at 1k, and
   0.710 against 0.795 at 37k: closer, still under. Pricing decode's spread
   needs its histogram (the R1 field `TTFTBuckets` on decode rows), not a
   constant.
2. **The prefill engine's overhead at light load is larger than `a`.** P2
   measured 3 to 83 ms beyond queue and service time on the prefill engine,
   growing with the prompt (P2's check 6). P3's `a` is a constant 3.7 to
   10.9 ms. At 30k and 37k that gap, about 40 to 70 ms, is most of what is
   left.

Both would add a term fitted from these same recordings, which this change
does not do. They are open items for S1's recorder-backed inputs (decode's
TTFT histogram) and for P1 (an overhead that grows with the prompt).

### As built: the decode term

The staircase run (M2) showed why light-load figures read low on a decode line.
The model priced decode's part as `T_xfer + ITL(k)`, and measured decode-engine
TTFT is several steps long. Now, on a decode line:

    decode's part = D(I, ITL(k)) + R
    D(I, ITL)     = base + perToken·I + perStep·ITL
    ITL(k)          the line at the actual fill, not clamped up to the band

The clamp had to go: at `perStep` of about 7 it would overshoot bucket A's floor
by about 28 ms. `BelowFittedBand` still stands below the band.

**Where the coefficients come from.** Each is learned per decode variant
(`prefill.DecodeTermWindow`), as `a` and `V_P` are, and flagged when assumed. No
coefficient of this run is in the code, except as the fixture of a test.

| Coefficient | Learned from | Read every cycle? | With no fit |
| --- | --- | --- | --- |
| `base`, `perToken`, `perStep` | Per-pod `AvgTTFT`, `AvgITL`, input tokens, request rate and queues | Yes: all are scaling queries. Nothing here needs Q1's always-on work | `D` from the last 5 minutes' quiet rows held, rising by `AssumedDecodeStepSlope` (10) from the ITL it was read at, flagged `DecodeStepSlopeAssumed`. With no quiet row, the old sum, flagged `DecodeTermOmitted` |
| `R` | The router's TTFT `_sum` and `_count` over 5 minutes, less the engines' `AvgTTFT` over the same cycles | Only with this change: it adds the two queries, for a model whose policy lists the SLO analyzer | `AssumedRouterHopSeconds` (20 ms), flagged `RouterHopAssumed`. The CLI takes `--assume-router-hop` |

The window is the variant's, under its accelerator and engine arguments: a change
of either sweeps it, as it does the line. Why no fit stands (`NoStepSpread`,
`NonPositiveDecodePlane`, `NoRouterMean` and so on) is carried with the term to the
CLI's `assumed` row and JSON and to the `slo-ttft-variant` log, beside the term as
priced.

- **Why the router needed new queries.** The collector read only the histogram's
  buckets, 25 to 100 ms wide around a 5 to 15 ms figure. Read from the buckets,
  the warm-up's residual fell with the prompt, the wrong sign. Read from `_sum`
  and `_count` over a steady 5-minute window, R's median was 4.6 and 4.9 ms on
  bucket A, 11.4 and 11.6 ms on B, 12.5 and 12.5 ms on C, spread 0.6 ms on A and
  about 4 ms on B.
- **What a router window needs.** Steady rate and prompt, and no cycle with the
  router holding requests: the router's mean starts before flow control, and a
  held window would count the queue in R. A line needs every median above 0.
- **What admits a minute.** A row with a read mean of both, at least 0.05 r/s,
  its prompt steady to 15%, no speculation, its slot queue read and empty (an
  unknown queue is not an empty one, so SGLang, which publishes none, takes the
  fall-back), at most `MaxDecodeQueue` (9) waiting at all, and no more than 0.4 s
  of them at its own request rate. Past that the pod is behind on KV pulls: of
  3,300 staircase rows, 3% at 6 to 9 waiting read over 30 ms above the fit, against
  44% at 10 to 14 and 81% at 15 to 24, and none under 0.4 s against 49% at 0.4 to
  0.8 s. Both gates are stated from one model.
- **What a slope needs.** Step time must span 1.5x. A fleet at flat load stays
  refused (`NoStepSpread`) and prices the assumed slope. One prompt length gives
  `base` as the whole prompt's part, held at the shortest prompt and scaled up
  only. A plane must stand above 0 at the shortest prompt and step it read; its
  intercept may not, since nothing reads it there.
- **Nothing prices under the old sum,** fit or fall-back. Each figure (the
  headline, the floor, the independent and the pooled one) is compared with the
  old sum's at its own rate and percentile, and the old sum stands where it is
  higher, flagged `DecodeTermFloored`
  (`TestTerm_NoFitPricesUnderTheOldSum`: 100 random fits at five percentiles and
  the rate answered). Before this a fit with a negative `base` gave 5.4x the rate,
  unflagged.
- **A latch keeps the choice from flapping.** A fit is priced after 30 cycles in a
  row and given up after 15 refused ones, and holds its last value until then. Its
  noise is assumed: phi 0.9 is not measured on the fit's own series.
  See [3.5](analyzer-learner.md#35-hysteresis).
- **The held `D` is quiet.** A busy row's TTFT holds the slot wait, which the model
  prices apart as `W_decode`, and a load the question may not ask about: a 400 ms
  reading gave a 509 ms floor. So only the rows that admit a minute are held.
- **A refusal keeps what it rests on.** `TargetBelowFloor` carries the term's flags.
  Where an assumed part alone puts the floor over the target, it says so, and
  `llmsm_slo_ttft_unmet` is absent: that says nothing of the fleet.
- **What the warm-up teaches.** `D` = −2.12 ms + 4.09 ms per 1000 tokens + 6.78
  ITL, near the −2.98, 4.06 and 7.02 fitted outside the code. Its intercept is
  under 0 and it stands above 0 wherever it was read. Of the 171 cycles a plane
  was fitted it priced on 142; the first 29 were the latch. The tables below are
  that learned `D`, with R the warm-up's Prometheus fit, since no recording holds
  the router's mean.
- **Measured versus assumed.** `perStep` fits 7.05, 6.68, 6.61, 6.44, 7.46 and
  8.09 on the six phases alone, each from one prompt length, all on one small
  model. Both assumed constants are stated, not measured in general, and neither
  has been run on another model. The learned R has run on no build: no recording
  holds the new fields, so every golden prices the 20 ms. Its estimator was checked
  against the run's Prometheus series offline, and by unit tests.

**What moved.** On the staircase's fleet, with the warm-up's learned `D` and R
(`TestLadder_BeforeAndAfterTheTerm`, which also reproduces the old figures):

| Bucket | Floor, mean, ms | Floor, p90, ms | First level, mean, ms | First level, p90, ms | Client at that level, mean / p90 |
| --- | --- | --- | --- | --- | --- |
| A | 30.1 → 36.7 | 31.0 → 37.3 | 30.2 → 43.7 | 31.1 → 44.4 | 39.5 / 47.3 |
| B | 75.9 → 93.2 | 80.2 → 98.0 | 81.7 → 107.1 | 106.7 → 132.3 | 118.7 / 147.2 |
| C | 75.9 → 93.2 | 80.2 → 98.0 | 98.7 → 124.2 | 159.7 → 185.2 | 132.3 / 182.6 |

Against all 31 levels, predicted over the client's figure, old → new:

| | Mean error | Range | Within 10% |
| --- | --- | --- | --- |
| Mean | −35.4% → −7.3% | −47.4…−23.6% → −17.2…+10.7% | 0 → 18 of 31 |
| p90 | −28.0% → −7.3% | −54.5…+1.8% → −22.5…+18.1% | 8 → 8 of 31 |
| p95 | −23.4% → −4.9% | −56.0…+14.3% → −24.6…+28.4% | 10 → 1 of 31 |

**This is a consistency check, not a validation.** The form was chosen from M2's
own data, so M2 cannot validate it, whichever split was fitted on: repeat 1
failed the old form, and only a fresh frozen run can score this one.
`UnvalidatedTTFT` stays. The p90
and p95 errors are the tail this term does not touch: `Ca²` stays as given (no
thinning), so C's tail still reads high, and B's and A's low. The service-time
change is not in this work.

S1 stays shadow. `slo.SwitchTTFTDecodeTerm` off replays the old sum unflagged. It
moves no committed golden, since no recording with `slo.Analyze` prices a line.
The CLI has no switch, and its goldens carry the fall-back: their recordings read
no decode TTFT, so `DecodeTermOmitted`, `RouterHopAssumed` and 20 ms. A refusal, or
a move of 25%, that an assumed term alone decides is named on the `assumed` row.

### What ev-shindin/llm-scaling-manager#482 took from this change

ev-shindin/llm-scaling-manager#482 merged main with this change and updated `ttft_validation_test.go`
(its frozen table is this section's) and the [P2](#as-built-p2) text:

1. `p1Line` gains `vpPromptTokens`, `aModel`, `aSlope` and `aCurve`, and
   `estimate()` maps them to `VPPromptTokens`, `AModel`, `ASlope` and
   `ACurve`.
2. Constants `pr475B = 7.1815e-07` and `pr475C = 2.0280e-10` (ev-shindin/llm-scaling-manager#475's pooled
   curve, `TestQuadratic_Runs3And4PooledGiveA`). `ttftPrefill` takes the
   fixture's own curve where its P1 fits a quadratic (run 4), else these.
3. `ttftFleetAt` takes the level's decode-engine TTFT and passes
   `DecodeTTFT{Seconds: o.decodeTTFT, PromptTokens: o.prompt}`.
4. The 40 rows of `ttftFrozen` are replaced by the figures in the table
   above, and the counts become 9 (under), 2 (under beyond the bucket),
   13 (cv 2 over, unchanged) and 0 (run 2 under beyond the bucket; all six
   of its rows are refused).
5. `WhatTheRunsShow`'s bound on P3's light-load tail past 0.3 s rises from
   0.01 to 0.02. The 37k level reads 0.0186 with the decode term in the
   fleet (it was 0.0082). The direction check is unchanged.
6. Independently of this change, `P1OnTheGoldens` already fails on main
   since ev-shindin/llm-scaling-manager#475 merged: run 3's `a` refusal is `NoShortPrompts`, not
   `NonPositiveIntercept`, and run 4's `a` is 11.58 ms, not 3.74 ms. ev-shindin/llm-scaling-manager#482
   must take these whatever happens to this change. Run 4's levels are then
   priced at 11.58 ms.

## As built (M2)

The first staircase, one repeat of all six phases, against a prediction frozen
before its first step.

**This is repeat 1 only. Every flag stays, and no operator page is written.**
The documented criteria ask for 3 repeats, and a flag lifts only if its bucket
passes in all of them. A fail cannot be undone by a later repeat, so repeats 2
and 3 run only after a clean repeat 1. This one is not clean: A and B fail on
TTFT, so `UnvalidatedTTFT` stays and repeats 2 and 3 were not run. C passes
here, and that lifts nothing: `AssumedPoissonArrivals` needs all its repeats.
`AboveFlowControlOnset` cannot be lifted by this ladder: its steps stop below
every predicted onset (decode 42.4 r/s and prefill 54.6 r/s in B).

### The run

Qwen3-0.6B at TP 1 on H200, 2 prefill and 2 decode replicas, one warm-up with
flow control off, then each bucket off and on.

Four phases have a committed golden: `ttft-staircase-a-off`, `-b-off`, `-b-on` and
`-c-on` ([Learner Goldens](../developer-guide/testing.md#learner-goldens)). A on and
C off are in this section's tables only. Their figures rest on the run's own
request logs, Prometheus captures and raw recordings, which are outside the
repository, and no test here replays them. Every figure below was derived the same
way for all six phases, and [the tooling](#as-built-the-staircase-tooling) now derives
them from the tree.

Where it departs from [the specification](#ground-truth-and-the-staircase):

| Item | As run | Why |
| --- | --- | --- |
| Replicas | Pinned by ScaledObject min = max = 2, not `paused`. 911 samples per role, all 2 | The earlier runs' pin |
| Load | One driver for all buckets, streamed, Poisson on A and B | guidellm has no bucket C. Poisson is what A and B are predicted at |
| Steps | 0.40, 0.55, 0.70, then +0.06 to 1.12 of the predicted knee: 10 steps | 100 minutes a phase, not 3 hours |
| Warm-up | One, flow control off | The model's inputs do not depend on the gate |
| Stop | A, B on and C on stopped on the frozen client p90 rule. B off and C off stopped on the driver's own cap, `--max-inflight 4000`, at 4535 and 4157 in flight, 330 s into step 7 and 150 s into its 420 s window | The frozen criteria do not name the cap. It ended the two runs before the p90 rule could read a full window |
| Targets | Proposed, none being documented: `TTFTAt` at 0.85 of the knee, flow control off, frozen. TPOT: the mean priced there, and that mean times the warm-up's `R_q` (2.39 at p90, 2.44 at p95, flagged `CoarseTPOTBucket`) | |
| Router figures | Prometheus' 1-minute rates, summed over the window. No raw EPP scrape | The EPP's metrics were not readable through the API proxy |

The prediction is the file frozen before the first step. Run again at the
recording's commit, the predictor writes the same 15 lines, byte for byte.
Bucket C is priced at the `Ca²` each phase measured (mean 4.03 off, 4.01 on,
every step 3.87 to 4.23 but the one cut short), targets held, as the criteria
ask. Decode binds all three buckets: the prefill knees are 459 and 57.4 r/s
against decode's 42.7 and 44.0, so B's "prefill-bound" did not hold.

Measured rate is interpolated between step rates. "Below the ladder" is a first
step that already misses the target, so the ratio is a lower bound. The mark is
the page's: a ratio above 1.10 over-promises, below 0.75 under-promises. Reading
the achieved rate in place of the step rate changes no mark.

Validity. No engine error, and both roles stayed at 2 Ready. Send lag and `Ca²`
were in bounds on every step. Step 3 of C off ran 3.3% under its rate, past the
proposed 3%, and is kept: no crossing falls there. Step 7 of B off and C off ran
64.1% and 65.7% under, because the in-flight cap stopped them 150 s into the
window. A crossing that lands on one of those steps is reported as a bound, 38.7
to 41.3 r/s, and not interpolated. C on's step 7 failed 54%, and counts as a miss.

Noise floor. A step's window must hold 4 relaxation times, where a relaxation time
is `E[S](Ca² + Cs²)/(1 − √ρ)²`: the page's formula, with `Ca²` kept for bucket C.
On prefill the inputs are measured (`E[S]` is `I/V_P`, `Cs²` the prompt spread) and
it never binds: 4 relaxation times are 17.3 s at most. Decode has no measured
`E[S]` or `Cs²` for a request in the queue, so this is an estimate: `E[S]` is 2 over
the frozen knee, `ρ` the step rate over that knee, and `Cs²` is assumed to be 1.

| Phase | Step | Rate | Decode rho | 4T, s | Window, s | Window over 4T | Flagged |
| --- | --- | --- | --- | --- | --- | --- | --- |
| B-off | 6 | 38.7 | 0.88 | 95 | 420 | 4.42 | no |
| B-off | 7 | 41.3 | 0.94 | 392 | 150 | 0.38 | yes |
| B-on | 6 | 38.7 | 0.88 | 95 | 420 | 4.42 | no |
| B-on | 7 | 41.3 | 0.94 | 392 | 420 | 1.07 | no |
| C-off | 6 | 38.7 | 0.88 | 247 | 420 | 1.70 | no |
| C-off | 7 | 41.3 | 0.94 | 1113 | 150 | 0.13 | yes |
| C-on | 6 | 38.7 | 0.88 | 240 | 420 | 1.75 | no |
| C-on | 7 | 41.3 | 0.94 | 968 | 420 | 0.43 | yes |

- Steps 1 to 5 clear it by at least 4.13 times. B's step 6 clears it by 4.42, and
  C's by 1.70 and 1.75, a thin margin on an assumed `Cs²`.
- Three step 7s are flagged, and B on's clears it by 1.07. With the knee where
  [the readings](#readings) put it, at or under 41.3 r/s, `ρ` at step 7 is at or
  above 1 and no window holds it, B on's included.
- The steps were not lengthened: the hold was fixed at 10 minutes, and the proposed
  exception starts above `ρ` 0.95. At 0.94 this departs from the page.
- No mark moves. A crossing on a flagged step is a bound, which adds C on's client
  TPOT p90 to the four the 3% rule catches. B on's TPOT crossings rest on step 7,
  and as a bound would read 0.90 to 0.97, still in the mark.

The window's own drift agrees. Client p90 in the second half of the window over the
first is 0.94 to 1.09 on every step through 6, except C on's step 6 at 1.50, and it
is 1.18 on B on's step 7. C on's TTFT crossings end on that step 6: read as the
whole bracket, 36.0 to 38.7 r/s, they give 0.96 to 1.04, still in the mark.

### TTFT, frozen

| Bucket | Flow control | q | Target | Predicted r/s | Measured r/s, router | Ratio | Mark | Measured r/s, client | Ratio | Mark |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| A | off | p90 | 31.8 ms | 36.3 | below the ladder (17.1) | > 2.12 | FAIL, over-promises | below the ladder (17.1) | > 2.12 | FAIL, over-promises |
| A | off | p95 | 33.1 ms | 36.3 | below the ladder (17.1) | > 2.12 | FAIL, over-promises | below the ladder (17.1) | > 2.12 | FAIL, over-promises |
| A | on | p90 | 31.8 ms | 36.3 | below the ladder (17.1) | > 2.12 | FAIL, over-promises | below the ladder (17.1) | > 2.12 | FAIL, over-promises |
| A | on | p95 | 33.1 ms | 36.3 | below the ladder (17.1) | > 2.12 | FAIL, over-promises | below the ladder (17.1) | > 2.12 | FAIL, over-promises |
| B | off | p90 | 164.3 ms | 37.4 | below the ladder (17.6) | > 2.13 | FAIL, over-promises | 20.8 | 1.80 | FAIL, over-promises |
| B | off | p95 | 189.6 ms | 37.4 | below the ladder (17.6) | > 2.13 | FAIL, over-promises | 21.5 | 1.74 | FAIL, over-promises |
| B | on | p90 | 164.3 ms | 37.4 | below the ladder (17.6) | > 2.13 | FAIL, over-promises | 18.3 | 2.04 | FAIL, over-promises |
| B | on | p95 | 189.6 ms | 37.4 | below the ladder (17.6) | > 2.13 | FAIL, over-promises | 19.8 | 1.89 | FAIL, over-promises |
| C | off | p90 | 394.1 ms | 37.3 | 36.2 | 1.03 | in the mark | 37.2 | 1.00 | in the mark |
| C | off | p95 | 510.3 ms | 37.3 | 37.5 | 0.99 | in the mark | 38.7 to 41.3 (bound) | 0.90 to 0.96 | in the mark, a bound |
| C | on | p90 | 394.1 ms | 37.3 | 36.1 | 1.03 | in the mark | 36.3 | 1.03 | in the mark |
| C | on | p95 | 510.3 ms | 37.3 | 36.5 | 1.02 | in the mark | 36.8 | 1.01 | in the mark |

A and B fail in both flow-control states, by both witnesses. C is in the mark in
both, one figure of it only as a bound. In this repeat the client's rate was lower
with flow control on by 1.7 to 2.5 r/s in B and by 0.9 r/s in C at p90. At p95 in C
it was lower by 1.9 r/s or more: C off's figure there is a bound, 38.7 to 41.3 r/s,
so 1.9 is a difference from the bound's low end. That is one repeat with the phases
run in sequence, and the differences are about a ladder step (2.6 r/s at C, 6.6 r/s
at B), so they say nothing of the gate. The frozen prediction is the same rate on
and off.

### TPOT, frozen

The predicted rate is the operating rate: each target was set there.

| Bucket | Flow control | Reading | Target | Predicted r/s | Measured r/s, router | Ratio | Mark | Measured r/s, client | Ratio | Mark |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| A | off | mean | 7.0 ms | 36.3 | at least 29.9 | at most 1.21 | no mark | at least 29.9 | at most 1.21 | no mark |
| A | off | p90 | 16.7 ms | 36.3 | at least 29.9 | at most 1.21 | no mark | at least 29.9 | at most 1.21 | no mark |
| A | off | p95 | 17.1 ms | 36.3 | at least 29.9 | at most 1.21 | no mark | at least 29.9 | at most 1.21 | no mark |
| A | on | mean | 7.0 ms | 36.3 | at least 29.9 | at most 1.21 | no mark | at least 29.9 | at most 1.21 | no mark |
| A | on | p90 | 16.7 ms | 36.3 | at least 29.9 | at most 1.21 | no mark | at least 29.9 | at most 1.21 | no mark |
| A | on | p95 | 17.1 ms | 36.3 | at least 29.9 | at most 1.21 | no mark | at least 29.9 | at most 1.21 | no mark |
| B | off | mean | 9.5 ms | 37.4 | 36.1 | 1.03 | in the mark | 36.1 | 1.03 | in the mark |
| B | off | p90 | 22.7 ms | 37.4 | 38.7 to 41.3 (bound) | 0.90 to 0.97 | in the mark, a bound | 38.7 to 41.3 (bound) | 0.90 to 0.97 | in the mark, a bound |
| B | off | p95 | 23.2 ms | 37.4 | 38.7 to 41.3 (bound) | 0.90 to 0.97 | in the mark, a bound | 38.7 to 41.3 (bound) | 0.90 to 0.97 | in the mark, a bound |
| B | on | mean | 9.5 ms | 37.4 | 36.4 | 1.03 | in the mark | 36.3 | 1.03 | in the mark |
| B | on | p90 | 22.7 ms | 37.4 | 39.3 | 0.95 | in the mark | 40.7 | 0.92 | in the mark |
| B | on | p95 | 23.2 ms | 37.4 | 39.2 | 0.95 | in the mark | 40.7 | 0.92 | in the mark |
| C | off | mean | 9.5 ms | 37.4 | 34.6 | 1.08 | in the mark | 35.2 | 1.06 | in the mark |
| C | off | p90 | 22.7 ms | 37.4 | 37.0 | 1.01 | in the mark | 38.7 to 41.3 (bound) | 0.90 to 0.97 | in the mark, a bound |
| C | off | p95 | 23.2 ms | 37.4 | 36.3 | 1.03 | in the mark | 38.2 | 0.98 | in the mark |
| C | on | mean | 9.5 ms | 37.4 | 34.8 | 1.07 | in the mark | 34.7 | 1.08 | in the mark |
| C | on | p90 | 22.7 ms | 37.4 | 37.2 | 1.00 | in the mark | 38.7 to 41.3 (bound) | 0.90 to 0.97 | in the mark, a bound |
| C | on | p95 | 23.2 ms | 37.4 | 36.8 | 1.02 | in the mark | 38.3 | 0.97 | in the mark |

- **B and C are in the mark** on the mean and on both percentiles, by both
  witnesses. Ratios run 0.92 to 1.08, or 0.90 at the far end of a bound.
- **A's TPOT is "at least".** A TTFT runaway stopped A at step 3, while mean TPOT
  was 4.4 ms against a 7.0 ms target. The rate where TPOT meets its target was
  never reached, so A has no TPOT mark, only a bound: the ratio is at most 1.21.

### What the router's buckets can say

The router's buckets are wide where the targets sit, and `histogram_quantile`
interpolates inside them:

| Bucket | q | Target | Router bucket | Width | Width over target |
| --- | --- | --- | --- | --- | --- |
| A | p90 | 31.8 ms | 0.025 to 0.05 s | 25 ms | 79% |
| A | p95 | 33.1 ms | 0.025 to 0.05 s | 25 ms | 75% |
| B | p90 | 164.3 ms | 0.1 to 0.2 s | 100 ms | 61% |
| B | p95 | 189.6 ms | 0.1 to 0.2 s | 100 ms | 53% |
| C | p90 | 394.1 ms | 0.2 to 0.4 s | 200 ms | 51% |
| C | p95 | 510.3 ms | 0.4 to 0.6 s | 200 ms | 39% |

So a router figure near a target is a position inside a bucket, not a
measurement. Below the ladder on B at p90 rests on 189 ms read in a bucket from
100 to 200 ms; the client read 147 ms there, below the target. C's p90 target,
394 ms, is 6 ms under an edge. The client's column is the finer one, and B and
A fail on it too.

### Readings

**A's target sits on the model's floor, and the gap grows with load.** The frozen
p90 target, 31.8 ms, is 1.03 times the model's floor, 31.0 ms. The client's p90 is
above the model's by 16.2, 23.6 and 37.4 ms over steps 1 to 3 off, and 19.9, 26.3
and 35.9 ms on. An error in the floor alone would not grow. The runaway rule, twice
the target, stopped A at 0.70 of the knee, so A's queue was never reached.

**The capacity knee reads high, and the result does not split by flow-control
state.** B and C predict 44.0 r/s. A phase held a step if completions over the
judged window came within 3% of the step rate and nothing failed. By that rule all
four held step 6, 38.7 r/s (38.5, 38.5, 38.8 and 38.8 r/s). At step 7, 41.3 r/s:

| Phase | Completions, first minute, r/s | Then, r/s | Peak in flight | Stopped by |
| --- | --- | --- | --- | --- |
| B off | 39.8 | 13.8 to 36.8 | 4535 | the in-flight cap |
| C off | 37.8 | 13.3 to 32.9 | 4157 | the in-flight cap |
| C on | 39.7 | 14.9 to 26.9 | 1657, with the router's queue peaking at 1455 | the p90 rule |
| B on | 41.2 | 40.4 to 42.9 | 234 | the p90 rule, held |

That is two collapses off, one collapse on and one hold on, at one rate in one
repeat. In B off and C off the knee lies between 38.7 and 41.3 r/s, and the
prediction reads 6% to 14% high. In B on it is at or above 41.3 r/s, and the
prediction reads at most 6% high.

C on's step 6 held by that rule and no more. Inside its window the client's p90 went
from 424 ms to 636 ms, 1.50 times, in flight from 90 to 136, and the residual is 32.7
ms against 17.8 at step 5. So C on's knee may be below 38.7 r/s, and there 14% is a
lower bound on how high the prediction reads: from step 5, which was steady, it
could be as much as 22%.

The one hold is not evidence for the gate: C on collapsed with it, and B on and C on
differ in `Ca²`. Why the fleet collapses below the priced knee is not known.

**C on's step 7 is a 429 storm.** Of the 17503 requests due in the judged window,
9527 failed, 54%. Over the whole step 10731 of 24823 did, 43%. All were 429
`ResourceExhausted`. The router's p90 of 45.6 s covers only requests that
finished; the client's is infinite. The router's effective saturation peaked at
15.3 and its queue at 1455.

**Where the time goes.** The terms below are means, in ms: the model's own terms
against the client's mean and the engines' counters over the same window.

| Phase | Step, r/s | Client mean | Model mean | Prefill, measured | Prefill, priced | Decode and hop, measured | Decode and hop, priced | Residual | Client p90 | Model p90 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| A off | 1, 17.1 | 39.5 | 30.2 | 13.2 | 15.6 | 26.3 | 14.6 | 5.1 | 47.3 | 31.1 |
| A off | 3, 29.9 | 57.5 | 30.2 | 15.6 | 15.6 | 42.0 | 14.6 | 8.0 | 68.7 | 31.3 |
| B off | 1, 17.6 | 118.7 | 81.7 | 49.8 | 45.5 | 69.0 | 36.1 | 10.9 | 147.2 | 106.7 |
| B off | 5, 36.0 | 188.5 | 103.8 | 72.5 | 65.8 | 115.9 | 38.0 | 13.9 | 261.0 | 156.7 |
| B on | 5, 36.0 | 185.7 | 103.8 | 71.1 | 65.8 | 114.6 | 38.0 | 13.3 | 255.6 | 156.7 |
| C off | 1, 17.6 | 132.3 | 98.7 | 57.2 | 62.6 | 75.1 | 36.1 | 12.3 | 182.6 | 159.7 |
| C off | 5, 36.0 | 233.0 | 177.2 | 101.4 | 139.2 | 131.6 | 38.0 | 13.9 | 359.7 | 366.2 |
| C on | 5, 36.0 | 246.3 | 177.2 | 105.4 | 139.2 | 141.0 | 38.0 | 17.8 | 380.1 | 366.2 |

Measured:

- Prefill is priced close on A and B through step 6: within 2.4 ms on A and 4 to
  9 ms low on B.
- Decode and the hop are where A and B are lost. On A off they grow from 26.3 to
  42.0 ms against 14.6 priced, on B off from 69.0 to 115.9 ms against 36 to 38
  priced.
- The residual, the client's mean less the two engines' TTFT, is 5.1 to 15.2 ms on
  every phase through step 6, but for C on's steps 5 and 6 (17.8 and 32.7 ms).
  Nothing in the model prices it.
- B's gap is not a fixed offset. Its gap at step 1 is 34% to 44% of the gap at
  steps 3 to 6 off, and 46% to 61% on.

Fitted, on the warm-up alone, 305 decode rows: decode TTFT is -3.0 ms, plus 4.06 ms
per 1000 tokens, plus 7.02 times ITL, rms 4.15 ms. Applied to the staircase, which
it never saw, its mean error is -2.2 ms on A, -6.5 ms on B and -14.0 ms on C. Fitted
on the staircase itself, the slope on the client's TPOT is 6.81 on A and 6.95 on B
and C.

Inferred:

- The decode side scales with step time, at about 7 steps, where the model prices
  one first step and the transfer. That is why A's and B's gaps grow with load.
- The residual is the hop between the router and the engines.

**C passes by cancelling errors: measured, no longer a hypothesis.** At 36 r/s on C
off the model has prefill 37.8 ms over, 139.2 against 101.4, and decode and hop
93.6 ms under, 38.0 against 131.6. C on gives 33.8 ms over and 103.0 ms under. Its
mean is 55.8 ms low, while its p90 reads 366.2 ms against the client's 359.7 off and
380.1 on. The over side is the bursts: from B to C at 36 r/s the prefill engine
gained 28.9 ms off and 34.3 ms on, where the model adds 73.4 ms.

| Reading | p90 | p95 |
| --- | --- | --- |
| Model, Ca2 4 over Ca2 1, at the operating rate | 2.40 | 2.69 |
| Measured C over B, client, off, steps 1 to 6 | 1.22 to 1.49 | 1.18 to 1.52 |
| Measured C over B, router, off, steps 1 to 6 | 1.02 to 1.40 | 1.02 to 1.61 |
| Measured C over B, client, on, steps 1 to 6 | 1.14 to 1.80 | 1.13 to 2.04 |
| Measured C over B, router, on, steps 1 to 6 | 1.02 to 1.51 | 1.08 to 1.79 |
| A off, step 1 at 17.1 r/s: model, then client | 31.1, 47.3 ms (1.52x) | 31.6, 50.3 ms (1.59x) |
| A on, step 1 at 17.1 r/s: model, then client | 31.1, 51.0 ms (1.64x) | 31.6, 54.6 ms (1.73x) |
| B off, step 1 at 17.6 r/s: model, then client | 106.7, 147.2 ms (1.38x) | 115.9, 170.2 ms (1.47x) |
| B on, step 1 at 17.6 r/s: model, then client | 106.7, 161.7 ms (1.52x) | 115.9, 181.5 ms (1.57x) |
| C off, step 1 at 17.6 r/s: model, then client | 159.7, 182.6 ms (1.14x) | 214.0, 201.0 ms (0.94x) |
| C on, step 1 at 17.6 r/s: model, then client | 159.7, 183.6 ms (1.15x) | 214.0, 204.2 ms (0.95x) |

The model says going from `Ca²` 1 to 4 multiplies the TTFT 2.40 times at p90 and 2.69
at p95 at the operating rate, and the client measured 1.14 to 1.80 and 1.13 to 2.04.
So C's pass says nothing of the model at any other `Ca²`, and the two errors would
not cancel at the same place at `Ca²` near 2. That step was not run, so C's pass is
not evidence that the model prices bursts.

These predictions are the frozen run's, and stay: they were priced at Kingman's
`1/θ`. The bursty tail has since become the Poisson tail stretched by
`(Ca² + Cs²)/(1 + Cs²)`, so a fresh freeze prices bucket C differently. On the old
sum, at 17.6 r/s, 159.7 becomes 197.2 ms at p90 and 214.0 becomes 233.8 ms at p95.
The gap closes toward the knee: 424.7 becomes 444.6 ms at p90 at 38.7 r/s. At
41.3 r/s the p95 falls, 1087 to 1058 ms, because at `cv` under 1 the stretched root
decays a little faster than Kingman there. Means do not move. Buckets A and B are
Poisson, and move by nothing (`TestLadder_BeforeAndAfterTheTerm`).

### S2's gate and the flow-control series

**No recording holds S2's gate on one cycle.** The gate is `V_P`, `a`, `slo` and
a decode line that prices, at once. Replayed at this commit, with `slo` listed on
every cycle:

| Phase | Cycles | With `V_P` | With `a` | With a pricing decode line | `V_P` and decode | All | Priced |
| --- | --- | --- | --- | --- | --- | --- | --- |
| A off | 128 | 0 | 0 | 0 | 0 | 0 | 0 |
| A on | 130 | 0 | 0 | 0 | 0 | 0 | 0 |
| B off | 289 | 29 | 0 | 0 | 0 | 0 | 0 |
| B on | 289 | 0 | 0 | 34 | 0 | 0 | 0 |
| C off | 286 | 30 | 0 | 0 | 0 | 0 | 0 |
| C on | 292 | 36 | 0 | 0 | 0 | 0 | 0 |

`a` is the wall: a phase sends one prompt length, so P1 refuses it
(`NoPromptSpread`, `TooFewLowLoadMinutes`). The warm-up has the other half: 44 of
317 cycles hold `V_P` and `a` together, and it ends with a decode line, but it
ran before `slo` was listed and is not a golden. A on and C off are read from the
run's raw recordings; the other four rows hold on the goldens too. S2 still needs one run with a
prompt sweep and `slo` listed from its start.
`TestGoldens_TTFTStaircaseHoldsNoS2GateCycle` freezes the four committed rows.

**An on gate publishes the queue gauge at 0.** `slo-tiers.md` left this open.
From the recordings:

| Phase | Cycles | Gauge at 0 | Gauge above 0 | No gauge | Wait histogram |
| --- | --- | --- | --- | --- | --- |
| A off | 128 | 0 | 0 | 128 | 0 |
| A on | 130 | 126 | 0 | 4 | 125 |
| B off | 289 | 0 | 0 | 289 | 15 |
| B on | 289 | 281 | 4 | 4 | 284 |
| C off | 286 | 0 | 0 | 286 | 14 |
| C on | 292 | 245 | 43 | 4 | 288 |

- With the gate on and the queue empty, the gauge reads 0: 652 cycles over the
  three on phases, 526 of them in the two committed goldens, none absent once it
  has appeared. It still reads 0 at the end
  of each recording, 70 to 82 s after the last completion. Longer idle was not
  measured. The test's count of 3 cycles with no arrivals, in every on phase, is
  not all idle: in A on and B on one of the 3 is at 60 s, 11 to 12 s after the first
  request, before the arrival rate reads. After the last completion it is 2, 2 and
  3.
- Before a router's first request the gauge does not exist. Each on phase's first
  4 cycles hold none. Prometheus agrees: the pool saturation series reads from
  its first sample, 28 to 31 s into the run, and the gauge first at 58 to 61 s,
  after the first request at 46 to 49 s. So a router with the gate on that has
  served nothing still reads as no series.
- The wait histogram is no signal of the gate. B off and C off, each after an on
  phase, hold it for the first 195 to 210 s, at a non-zero rate for the first
  60 s. The non-zero part fits a 5-minute rate window: the on phase's last send and
  last completion are 231 and 227 s before B off's recording starts, 226 and 222 s
  before C off's, so the window empties them at 69 to 78 s, and the next cycle, at
  75 s, reads 0. Why the buckets then stay at rate 0 until 195 to 210 s is not known.
  It would take the old router's last two scrapes to end before the config patch
  that restarted it, and no file records them. A off follows an off warm-up and
  holds none.
- B's gate hardly worked: B on's effective saturation, a 5-minute mean, peaked at
  0.83, under 1, and the queue was above 0 on 4 cycles. C on is the only phase
  that queued for long.

`TestGoldens_TTFTStaircaseFlowControlSeries` freezes the table's four committed
rows; A on and C off are read from the run's raw recordings. The golden check
holds it too: `slo-published.csv` reads `no-decode-side` on the on phases' cycles
with a gauge and no decode line, a reason only the on state reaches.

### Not done

The onsets (check 8), the mean residual against its 10 ms budget, the union bound
and the domain check per step are report-only, and are not in this section: they
were not derived from this run. The run's files are outside the repository. Its
driver and the derivation of these tables are not:
[as built](#as-built-the-staircase-tooling).

### As built: the staircase tooling

The run is reproducible from the tree. How to run it is in
[Testing](../developer-guide/testing.md#the-ttft-staircase), and what the next run
must hold out and freeze is in
[the plan](../plans/analyzers/ttft-staircase-repeat.md).

| Piece | What it does |
| --- | --- |
| `whatif freeze` | The prediction: a warm-up recording and a spec in, one JSON file out, the same bytes for the same inputs. It reads the fleet as `slo-to-rate` does. It refuses to replace a file, and exits 3 when a bucket refuses |
| `staircase_spec.yaml` | The buckets, steps, warm-up, stop rule and falsifiers, hashed into the frozen file |
| `staircase_freeze.py` | The ladder, with each step's hold lengthened until its window holds 4 relaxation times, and the GPU-hours. It refuses a warm-up that fails S2's gate |
| `staircase_load.py` | The open-loop driver. A `RUNAWAY` line names its rule, `client-p90` or `max-inflight` |
| `staircase_analyze.py` | One row a step, then `report.md`. The judged window is cut at the stop, and each row carries `stopped_by`. A void step reads as a bound, and a phase that did not finish is excluded and named, so a bucket with a repeat missing cannot pass |
| `staircase.sh` | The run: deadline, standup, warm-up, phases, the gate check after repeat 1, captures, teardown |
| `hack/check-staircase.sh` | From `make lint-deploy-scripts`: the exit paths it lists against a stub kubectl, on a PATH with no other kubectl, and the arithmetic against hand-worked figures |

**Checked against M2.** Run over M2's files, the analysis gives every mark of the
tables above, and the knee table's holds and collapses. A step's window now starts at
its first request, not at the log's stamp, and is cut at the stop, so five client
figures move in their last digit (B off p95 1.74 to 1.73, B on p95 19.8 to 19.9 r/s,
and three mean TPOT rates by 0.1 r/s). No mark moves. The two steps the in-flight cap
cut short, step 7 of B off and C off, used to be read over a window past the stop. On
M2's warm-up `whatif freeze` gives the knees, onsets, `R_q` and mean TPOT of the frozen
file. Its targets differ, as they should: the frozen file predates the decode term.

**A saturated transfer floors the price only once it is the truth.** S1 and the CLI
read `T_xfer` through one window (`prefill.TransferWindow`, `slo.SwitchTTFTQuietTransfer`),
the same 20-cycle mean in both. A cycle is quiet when every row with a reading has an
empty queue, so one quiet pod beside busy ones says nothing. What S1 does at each edge:

| The fleet | `T_xfer` priced | Flag |
| --- | --- | --- |
| Quiet this cycle | The mean of the quiet cycles of the last 5 minutes | |
| Busy, or no reading, for up to 5 minutes (`HeldDecodeAge`) | The same, held | `TransferHeld` |
| Busy past 5 minutes | Every row's mean: the ev-shindin/llm-scaling-manager#494 floor rises again | |
| No reading past 5 minutes | None | `TransferOmitted` |
| Accelerator or engine arguments changed | The window starts over | |

Before this, M2's warm-up, which ends saturated, read 0.34 s at 8000 tokens, which
floored the priced figure at 414 ms through `DecodeTermFloored`, and it still does on
that recording: 11 saturated minutes is past the age. So the next warm-up ends on a
quiet stage. The frozen file stores the priced figure and every row's. It moves no
golden.

**Where a freeze departs from the CLI.** Each is recorded in the file: the prompt's
spread, the arrivals and the flow-control state are the spec's, and the prefix hit
rate is 0, since the driver's prompts share no prefix.

**Not checked.** `staircase.sh` has run only against stubs and a local fake API: no
cluster has run this tree. M2 ran its scratch form, and changing replicas between phases, which M2 never
did, has not run at all.

## As built: processor sharing

Repeat 1 of the second staircase (frozen at 0.85 of each bucket's knee) failed
the gate on bucket B: the model promised 28.3 r/s where the client held its
p90 and p95 targets at 23.2 to 24.7 r/s, a ratio of 1.15 to 1.22. The mean
TTFT agreed to 3 %, but only because three wrong inputs cancelled: the p90 read
11 % short at the operating rate. The model's shape was wrong, not a figure.

**What the run showed.** The vLLM histograms per pod, over each step:

- The prefill pod never queues. Its queue time was 0 ms at every step. Under
  chunked prefill the pod admits every prompt and splits each step between
  them, so a prompt's prefill time stretches while the pod is shared, as
  `S/(1 − ρ)` with `S` 24.9 to 26.0 ms at 8000 tokens. That is processor sharing,
  not a first-come queue.
- The decode pod's queue time is its KV pull, and it stretches the same way,
  `S/(1 − ρ)` with `S` 27 to 29 ms.

The model priced the first as a first-come (Kingman) wait on the busiest pod,
with near-constant work (`Cs²` 0.0034), and the second as a fixed `I·κ` inside
`D`. A first-come queue of near-constant work has a thin tail; processor
sharing does not. A tandem processor-sharing simulation calibrated on B's first
step reproduced the client's mean, p50, p90 and p95 at all 8 of B's steps
within about 2 %; the first-come shape read p90 18 % short.

**What changed.** Behind `TTFTFleet.ProcessorSharing`, on with the replay
switch `slo.SwitchTTFTProcessorSharing` (`slo-ttft-processor-sharing`) and
always in the CLI:

    TTFT = a + T_P(I) + T_X(I) + I·(perToken − κ) + R + D_rest(ITL) + W_decode

- `T_P` is the prefill pod as an M/G/1 processor-sharing stage. The busiest pod
  takes `f/N_p` of the arrivals and the others fall linearly from it, down to
  the least share that keeps the mean (`(2 − f)/N_p`, or 0 past `f` 2); at two
  pods that is the rest. A request is on a pod by its share, so the percentile
  is the population's, a request on any pod, not the busiest pod's. `f` fixes
  only the busiest pod's share: at two pods that fixes both, and the shares are
  exact. From three pods on, spreading the others evenly is the least spread
  they can have, and the stretch is convex in a pod's share, so it is the most
  optimistic reading (about 10 % low at three pods in review); the worst
  spread `f` allows reads far higher. The linear fall is a middle estimate,
  less optimistic than even spreading and not a bound, until the router's
  per-pod shares are read: at `f` 1.5 and 0.8 of the knee it prices the mean
  stretch 4.6 % above the even spread at three pods and 9.6 % at four. There is
  no prefill wait: the stretch is the wait.
- `T_X` is the KV pull, work `I·κ`, as a processor-sharing stage on the decode
  pod it lands on, the `N_d` pods sharing the arrivals evenly. A pull that
  saturates its pods is refused (`Saturated`).
- `D`'s per-token part comes net of the pull, since `D` was fitted with the pull
  inside it. On the staircase's warm-up that is 5.616e-6 less 2.541e-6, 3.074e-6
  s a token: at 8000 tokens `D`'s per-token part falls from 44.9 to 24.6 ms, and
  the 20.3 ms of pull work moves into `T_X`, where it stretches with load (28.5
  ms at B's operating rate). A held `D` with no per-token part gives the pull up
  from its fixed part. The step term and `R` are not touched.
- The parts: `engineWait` is the prefill pods' mean stretch (the busiest pod's
  and the others' by their shares), the new `kvPullWait` the pull's, and
  `waitScale` and `waitOffset`, which describe a first-come wait, are 0. The
  CLI's JSON is `schemaVersion` 3 for this.

**How the percentile is priced.** For one stage at utilization `ρ` and a request
of work `x`, the time past its work, `E = T(x) − x`:

| | Value | Exact where |
| --- | --- | --- |
| `P(E = 0)` | `(1 − ρ)·e^(−λx)`: the pod was empty and nothing arrived while it ran | Poisson arrivals |
| `E[E]` | `x·sρ/(1 − ρ)`, `s = (Ca² + Cs²)/(1 + Cs²)` past `Ca²` 1, `(1 + Ca²)/2` below it | `Ca²` 1, any work: the M/G/1-PS sojourn `x/(1 − ρ)` |
| `Var[E]` | `2vρ/(1 − ρ)²·(xk/(1 − ρ) − k²(1 − e^(−(1 − ρ)x/k))/(1 − ρ)²)`, `k = E[S]·(Ca² + max(1, Cs²))/2`, `v = s²` past `Ca²` 1, `s` below it | `Ca²` 1 and exponential work: the M/M/1-PS conditional variance. Any work, `Ca²` 1: its small-`x` limit `ρx²/(1 − ρ)²` |

Given `E > 0`, `E` is a gamma of those moments. Each stage then has two outcomes
(alone or shared), the two stages four, independent given the prompt. The
prompt is a gamma, and the prefill work, the pull and `D`'s per-token rest all
scale with it, so each outcome's first two moments over the prompt have a
closed form: every term is `c·u^n·e^(−a·u)`, `u` the prompt over its mean. Each
outcome is priced as a gamma of those moments, mixed by its weight and by the
pod's share: at most eight gammas. The decode admission wait joins it as before,
by the optimised union bound.

**Burstier than Poisson.** `s` past `Ca²` 1 is Kingman's ratio, the first-come
wait's at that `Ca²` over its Poisson one, and the variance scales as `s²`: a
burst stretches every request in it by about its size. It is a stated rule,
checked against simulation, not derived. The first version priced
`s = (1 + Ca²)/2` with the variance at `s`, and on one pod with near-constant
prompts that read 15 to 49 % short at `Ca²` 2 to 4 and `ρ` 0.6 to 0.85. Each
stage is priced at its own pods' `Ca²`: a pod taking share `p` of a stream sees
`p·Ca² + 1 − p`, the prefill pods at their shares and the pull at `1/N_d`.
That is exact for a renewal stream split at random (Bernoulli routing). A
load-aware router spreads a burst wider, so there it errs safe. It is
optimistic where a burst stays on one pod: prefix-affinity routing that keeps
a session's or a shared prefix's requests together, and bursts that are not
renewal (a modulated stream whose rate swings for minutes, which a split does
not thin).

Against a one-pod M/G/1-PS simulation at `Ca²` 2 and 4, `ρ` 0.3 to 0.93 and
prompt cv 0.058 and 1, p90 and p95 read −1.1 % to +117 %, the mean −3.8 % to
+45 %: never more than 5 % short at a percentile. A first-come queue
(Kingman's mean, an exponential tail) read at or below the rule at every one
of those points, so it is no floor for burst.

**It reads high at light load, and that is real.** `s` is the heavy-traffic
burst factor, applied at every load. On one pod at `Ca²` 4 with near-constant
prompts (cv 0.058) p95 reads +118 % at `ρ` 0.3, +48 % at 0.6 and +10 % at
0.85. On the rate side, a 250 ms p95 target is met at 0.68 of the rate that
truly meets it. The CLI's note says so on every answer at a stated `Ca²` above
1. Follow-up: blend `s` from 1 toward Kingman's ratio as `ρ` rises (the
review's suggestion; `s^(1 + ρ)` on the variance read p90 and p95 0.3 % to
53 % high over the same grid), and check it against the same simulation
before it replaces the rule.

**Past a wide prompt spread.** The staircase sent prompts at cv 0.058. vLLM's
chunked prefill need not share a prompt longer than its step budget
(`max_num_batched_tokens`) as processor sharing does, and may serve long
prompts closer to first come. Past a prompt cv of 0.25 (`whatif.MaxPSPromptCV`,
about four times the spread measured) the TTFT is the larger of processor
sharing's and the first-come queue's, flagged `SpreadBeyondProcessorSharing`,
until a run at a wide spread validates processor sharing there. S1's assumed
cv 2 is always past it.

**Checked against a simulation** of the same tandem stages
(`TestPS_AgainstATandemSimulation`, 180,000 requests a point, `V_P` unmargined):

| Bucket | Steps | Mean | p90 | p95 |
| --- | --- | --- | --- | --- |
| B, 2 + 2 | 13.3 to 31.6 r/s | within 0.1 % | 0.6 to 0.8 % short | 3.3 to 4.4 % short |
| B-onset, 2 + 4 | 39.1 to 53.6 r/s | 0.4 to 2.2 % short | −1.8 to +2.4 % | 0.7 to 2.1 % short |
| C, `Ca²` 4, 2 + 2 | 13.3 to 31.6 r/s | +8 to +12 % | +25 to +27 % | +26 to +32 % |
| One pod, `Ca²` 2 | `ρ` 0.3 to 0.85 | +1 to +8 % | +3 to +19 % | −1.1 to +20 % |
| One pod, `Ca²` 4 | `ρ` 0.3 to 0.85 | +8 to +23 % | +16 to +61 % | +16 to +71 % |

At Poisson the p95 reads 3.3 to 4.4 % short, inside the test's band of 5 %:
the two-moment gamma per part has a lighter tail than the simulated sum.
Burstier than Poisson the test's bands are one-sided, at most 5 % short.

At no load the stages are the old own time to the bit; toward the knee they
run away as `1/(1 − ρ)`; the TTFT never falls as the rate rises
(`TestPS_AtNoLoadIsTheOwnWork`, `TestPS_DivergesAtTheKnee`,
`TestPS_MonotoneInTheRate`).

**Against the staircase.** `TestStaircaseR1_*` replays the frozen fleet (the
frozen file's targets to 1e-9) against the client's ladder. Bucket B, both
phases, the step's p90 and p95 against the client's:

| Step, r/s | Client p90 / p95, ms | Processor sharing | First-come |
| --- | --- | --- | --- |
| 13.3 | 128.1 to 130.1 / 143.6 to 152.1 | +7.0 to +8.6 % / −1.9 to +3.9 % | +1.6 to +3.2 % / −7.9 to −2.5 % |
| 20.0 | 160.0 to 161.3 / 180.4 to 180.9 | −0.9 to −0.1 % / −4.3 to −4.1 % | −9.8 to −9.0 % / −13.9 to −13.6 % |
| 25.0 | 183.2 to 184.7 / 204.0 to 204.5 | −0.9 to −0.1 % / −2.5 to −2.3 % | −12.4 to −11.7 % / −14.7 to −14.5 % |
| 26.6 | 190.6 to 196.9 / 213.7 to 218.9 | −1.5 to +1.7 % / −3.4 to −1.0 % | −13.6 to −10.7 % / −16.1 to −14.0 % |
| 28.3, operating | 203.6 / 224.5 to 225.6 | +2.3 % / +0.8 to +1.2 % | −10.7 % / −13.0 to −12.5 % |
| 30.0 | 213.9 to 214.7 / 239.1 to 239.9 | +7.3 to +7.7 % / +4.8 to +5.2 % | −6.5 to −6.2 % / −9.7 to −9.4 % |
| 31.6 | 221.3 to 223.1 / 249.2 to 250.7 | +28 to +29 % / +31 to +32 % | +11.5 to +12.4 % / +12.9 to +13.6 % |

At 31.6 r/s, 0.95 of the knee, the decode admission wait comes in (38.8 ms
mean), and the union bound over it reads high on either shape. This change
does not touch it.

The gate, the predicted rate over the client's crossing of the target:

| Phase | p90, first-come | p90, processor sharing | p95, first-come | p95, processor sharing |
| --- | --- | --- | --- | --- |
| A off | 1.016 | 1.008 | 1.072 | 1.066 |
| A on | 1.033 | 1.025 | **1.616** | **1.609** |
| B off | **1.162** | 0.974 | **1.217** | 0.989 |
| B on | **1.147** | 0.976 | **1.212** | 0.993 |
| B-onset on | 0.936 | 0.850 to 0.876 | 0.922 | 0.850 to 0.876 |
| C off | 0.850 to 0.944 | 0.850 to 0.944 | 0.850 to 0.944 | 0.850 to 0.944 |

Bold is out of the mark (0.75 to 1.10). Every phase is in it on the new shape
but A on's p95, which a one-minute transient at its second step decides
(64.6 ms between 52.9 and 56.4) on either shape. C (`Ca²` 4) reads high under
the burst rule: its targets at the operating rate are 334.6 ms at p90 and
397.7 ms at p95 (282.7 and 307.9 under the first version's `s`), above the
client's ladder at the fourth step, so its ratio is the interval past it, as on
the first-come shape.

**Where it reads high.** At B-onset's operating rate, 47.5 r/s, the busiest
prefill pod is at `ρ` 0.81 and the 5 % `V_P` margin is whole, which takes it to
0.85. The stretch is `1/(1 − ρ)`, so the margin weighs more here than on the old
shape: p90 449 ms against the client's 299 (+50 %), p95 557 against 356. Without
the margin the model reads 356 and 432 ms, 0.90 and 0.88 on the gate. The
target then sits past every step the client held, so the gate's ratio is the
interval up to the knee, 0.85 to 0.88: inside the mark, on the side of
under-promising. The margin is unchanged here; whether processor sharing still
needs it is open.

Follow-up: the 5 % `V_P` margin was sized for the first-come shape, where it
moved the wait as `1/(1 − ρ)` once; under processor sharing the stretch moves
the same way but on every request, so the margin at B-onset reads p90 +50 %
high. Re-size it against a frozen run on the processor-sharing shape (or price
it by `ρ` alone, not by a fixed share of `V_P`) before the next staircase.

**What it does not change.** The flow-control onsets still read a first-come
queue length on the prefill pods (`meanScore`). Under processor sharing the
vLLM queue is near empty below saturation (the run's router queue peaked at 0
at the B-onset step above the predicted onset), so the prefill onset is likely
placed too low. Not addressed here.

**P2, re-frozen.** S1 prices with the switch on, so the router comparison of
[P3 after P2](#as-built-p3-after-p2) (`TestTTFTValidation_AgainstTheRouter`)
now prices processor sharing, and its table is re-frozen. One prefill pod, no
decode line, so the prefill stage is the only one. At the spread sent (cv
0.058) the rows moved:

| Level | p | Router, ms | First come, ms and ratio | Processor sharing, ms and ratio | Verdict |
| --- | --- | --- | --- | --- | --- |
| 1k | p90 | 47.0 | 32.1, 0.682 | 32.4, 0.689 | UNDER |
| 1k | p95 | 48.5 | 36.4, 0.750 | 35.5, 0.732 | UNDER |
| 2k | p90 | 47.9 | 44.8, 0.935 | 48.3, 1.009 | pass |
| 2k | p95 | 49.2 | 54.5, 1.109 | 53.9, 1.096 | pass |
| 4k | p90 | 92.1 | 70.4, 0.765 | 79.7, 0.865 | UNDER |
| 4k | p95 | 96.0 | 90.3, 0.940 | 90.1, 0.938 | pass |
| 16k | p90 | 316.3 | 212.8, 0.673 | 217.9, 0.689 | UNDER |
| 16k | p95 | 358.1 | 281.7, 0.787 | 267.8, 0.748 | UNDER |
| 8k | p90 | 97.6 | 146.4, 1.499 | 141.9, 1.454 | over\* |
| 8k | p95 | 109.6 | 160.6, 1.464 | 162.2, 1.479 | over |
| 30k | p90 | 569.7 | 379.9, 0.667 | 384.4, 0.675 | UNDER |
| 30k | p95 | 584.9 | 392.5, 0.671 | 452.2, 0.773 | UNDER |
| 37k | p90 | 794.7 | 522.5, 0.657 | 540.1, 0.680 | UNDER (was UNDER\*) |
| 37k | p95 | 915.0 | 713.7, 0.780 | 679.0, 0.742 | UNDER\* |

\* is beyond the bucket. Still under at 9 of 14, now 1 beyond the bucket (was
2), worst 32.5 % under (was 34 %). At S1's assumed cv 2, past
`MaxPSPromptCV`, the price is the larger of the two queues, and that is first
come's at every row: those 14 rows did not move (13 over, worst 3.15x at p90 and
4.13x at p95). Processor sharing alone would read them lower (2.48x and 3.31x
at worst in review), but no run has validated it at that spread.
`constants.SLOTTFTPredictionCaveat` and the CLI's note carry the new figures.

**Goldens.** No replay golden moves: no committed recording's cycle holds an
`a`, so S1 stops before it prices a fleet. The CLI's TTFT goldens move.

## Measured: where TTFT goes, and why prefill sizing (S2) is not built yet

The question was whether S2, sizing prefill replicas against a TTFT target,
could ship as an opt-in. It could only help if a prefill-side term is what
grows when the target is missed, and if prefill capacity can be learned
without a prefill queue (there never is one, [above](#as-built-processor-sharing)).
The second holds. The first does not on a fleet with as many decode as prefill
pods, so S2 is not built.

**The split.** Second staircase, Qwen3-0.6B, one H200 a pod, 8000-token
prompts, 200 out, `--max-num-batched-tokens 65536` on prefill, p90 target
200 ms. Per step, means in ms, from the per-pod vLLM histograms the controller
recorded (`AvgQueueTime`, `AvgPrefillTime`, `AvgKVTransferTime`, `AvgTTFT`) and
the router's `llm_d_epp_request_ttft_seconds`. The load generator records no
pod per request, so the split is of the step's mean, not of its p90. The
client's mean TTFT is the router's within 1-3 ms at every step, so the gateway
hop is negligible.

| Fleet, arrivals | req/s | client p90 | router mean | prefill pod TTFT (of it: prefill compute) | decode pod TTFT (of it: KV pull, first token) | rest |
| --- | --- | --- | --- | --- | --- | --- |
| 2+2, Poisson | 14.3 | 135 | 110 | 47 (30) | 53 (30, 5) | 9 |
| 2+2, Poisson | 26.5 | 193 | 141 | 58 (37) | 73 (37, 10) | 10 |
| 2+2, Poisson, **missed** | 28.3 | 204 | 148 | 60 (39) | 77 (38, 11) | 11 |
| 2+2, Poisson, **missed** | 35.7 | 254 | 182 | 71 (46) | 100 (44, 18) | 11 |
| 2+2, bursty (`Ca²` 4) | 14.0 | 159 | 118 | 52 (33) | 57 (32, 6) | 10 |
| 2+2, bursty, **missed** | 21.3 | 204 | 143 | 64 (41) | 70 (38, 8) | 10 |
| 2+2, bursty, **missed** | 35.4 | 381 | 242 | 105 (70) | 122 (55, 21) | 15 |
| 2+4, Poisson, **missed** | 41.2 | 242 | 159 | 80 (53) | 68 (39, 7) | 11 |
| 2+4, Poisson, **missed** | 61.5 | 574 | 327 | 193 (137) | 117 (64, 14) | 17 |

"Prefill pod TTFT" is the prefill engine's own TTFT: its queue (0 ms at every
step before overload), the prompt's compute, and 17-25 ms around it on 2+2
(27-55 ms on 2+4).
"Decode pod TTFT" is the decode engine's: its queue time, which holds the KV
pull, and the first token's step. "Rest" is the router's mean less both.

What it says:

- **On 2+2 the decode pod is the larger part and grows faster.** From the
  first step to the last, the prefill pod adds 23 ms and the decode pod 47 ms
  (KV pull +15, the decode queue beside the pull +13, the first token +13). At
  the first step that missed, the decode pod is 52 % of the router's mean and
  the prefill pod 41 %. The repeat run, and the flow-control-on run of the
  same load, agree within 4 ms a part at every step.
- **The same holds at other prompt lengths and fleet sizes**, from the
  Prometheus split (prefill pod growth vs decode pod growth over each
  staircase): 3000 tokens +3 vs +31 ms, 16000 tokens +18 vs +54, 3+3 at 8000
  tokens +38 vs +51. 3+3 missed from 13.2 req/s a pod of each role, 2+2 from
  14.1.
- **Prefill dominates only where prefill pods carry more per pod than decode
  pods**: 2+4 at twice the per-pod rate on prefill. There the prefill pod is
  the larger part and grows 114 ms against 48.
- **The KV pull is not a decode-only stage.** Unshared pull work,
  `S = T/(1 + λT)` with `λ` the decode pod's rate, is 24.4-24.8 ms at every
  step of 2+2, a clean processor-sharing stage. On 2+4 it rises from 27.7 to
  32.3 ms with the prefill pods' load, at the same decode per-pod rate as 2+2.
  The model prices the pull on the decode pods only.

**A queue-free prefill capacity exists.** The same inversion on the prefill
pod, `S = T/(1 + λT)` with `T` the mean prefill time and `λ` the pod's
completion rate, reads `S` 24.8-25.3 ms over every Poisson step of 2+2 (7 to
18 req/s a pod) and 25.4-26.2 ms on 2+4 (21 to 31 req/s a pod): `V_P` 38-40
req/s a pod, about 310,000 prompt tokens/s, within 5 % across a fourfold load
range, with the engine queue empty throughout. Bursty arrivals stretch `T`
more than processor sharing at `Ca²` 1 predicts, so the inversion reads `S`
27-31 ms there, `V_P` 7-19 % low: the conservative side.

**S2 replayed.** A throwaway build of main (`8affb8c`) supplied that `V_P`
where P1 refused one, took `a` as the prefill pods' `AvgTTFT` less queue and
prefill time, and searched the fewest prefill pods whose predicted p90 met
200 ms, the decode side as priced. Nothing reached the composite; the search
was printed only.

| Phase | Steps that held (ran 2 prefill) | Steps that missed | Predicted p90 at the fleet that ran, vs the client |
| --- | --- | --- | --- |
| 2+2, Poisson, flow control off | ask 1, 2, 2 | step 4 (28.3 req/s, client 204 ms): ask **2**; steps 5-8: ask 3, 3, 4, 5 | 1 % over at step 1, 6-8 % under at steps 2-8 (187 vs 204 at step 4) |
| 2+2, bursty, flow control on | not priced: `no-decode-side` | not priced | - |
| same, with that gate bypassed | ask 1 | step 2 (21.3 req/s): ask **2**; steps 3-6: 4 to 7; steps 7-11: no count meets it (`target-below-floor`) on 54-100 % of cycles | 7-11 % under |
| 2+4, flow control on, gate bypassed | - | steps 1-4: 4, 6, 10, 19 prefill; steps 5-7: no count meets it | 1 % under to 8 % over at steps 1-6 |

Against the bar set for an opt-in (at least one prefill more than ran at every
step that missed, at most one more where it held, nothing moving with it off):
it holds where 2+2 held, and fails at the first missed step of each phase
(asks the 2 that ran), at every bursty step past the sixth (no count meets the
target, because the decode side alone is over it), and at 2+4's steps 5-7 (no
count meets it). Where 2+4 does price, it asks 4 to 19 prefill pods: more than
any measurement supports, because it blames the pull's growth on decode. Bursty
phases are not priced at all on main without a
decode line (`no-decode-side`).

**Why it falls short, in order:**

1. On a fleet with as many decode as prefill pods, adding prefill alone buys
   little: at 2+2's highest step the model's best case, 24 prefill pods,
   predicts 184 ms against 237 at 2. The decode side would have to grow too,
   and nothing sizes decode against TTFT either.
2. The model reads p90 6-11 % under on 2+2 once loaded, so its knee comes
   one step late. A margin would move it, but tuned on these two phases only.
3. The pull is priced as sharing the decode pod; on 2+4 it shares with the
   prefill pods' load as well, so the model blames decode for a prefill cost
   and asks for many prefill pods to win back a little.
4. With flow control on, no decode line means no price (`no-decode-side`),
   which is every flow-control-on phase here.

**What S2 needs before it is built:** a joint search over prefill and decode
counts, not prefill alone; the pull priced with the prefill pods' load in it;
the inversion `V_P` as a P1 source, flagged and validated against a staircase
at another prompt length and arrival burstiness; and a price with flow control
on and no decode line. Until then the operator advice in
[the known limitation](../reference/scaling-policy.md#known-limitation-ttft-targets-with-long-prompts)
stands, with one correction it should carry: on 2+2 and 3+3 the per-pod rate
that held is a rate for both roles, so a prefill floor alone does not hold the
target at a higher rate unless decode grows with it.

## Proposed: joint prefill and decode TTFT sizing

**Status:** J1 is built, in shadow ([as built](#as-built-j1)); J2 to J4 are
design only. It answers
[what S2 needs](#measured-where-ttft-goes-and-why-prefill-sizing-s2-is-not-built-yet):
size both roles together, against a TTFT percentile, from inputs that exist
before any pod queues.

- **What you set:** `slo.mode: enforce` and `slo.ttft.sizing: joint`, with
  `slo.ttft` holding the target and its percentile.
- **What you see:** a prefill and a decode ask per variant on new
  `llmsm_slo_ttft_*` series, and in the composite once enforced.
- **Why it may not act:** `llmsm_slo_ttft_reason` and `llmsm_slo_ttft_flag`.

### First, does the model explain the split?

The measured split says the decode pod grows faster than the prefill pod on
2+2 (+47 ms against +23 ms from the first step to the last of B). The
suggestion was that the KV pull, priced with the prefill pods' load in it,
explains that. It does not. Checked against the per-pod histograms of the
three recorded phases in this data set (B-off-r1 2+2 Poisson, B-onset-on-r1
2+4 Poisson, C-on-r1 2+2 bursty):

- **On 2+2 the pull does not depend on prefill load.** Its unshared work
  (`S = T/(1 + λT)` on the decode pod) is 24.4-24.8 ms at every step while
  the prefill pods' utilization goes from 0.18 to 0.45. A pull priced on the
  decode pods alone, at that `S`, reproduces the pull to 1 ms. The pull is
  +15 ms of the decode pod's +47.
- **The rest of the decode side is decode steps.** At every step of B-off and
  B-onset, within 0.1 ms: the decode pod's queue beside the pull is
  `2.03 x ITL − 0.8 ms`, and its first token `2.01 x ITL − 0.2 ms`. What is
  left of the decode pod's TTFT is `1.14 x ITL + 12.5 ms` (within −2.7 to
  +5.0 ms; +6.1 ms over B-off). ITL is a line in the running batch,
  `ITL = α + β·k` with `α` 2.09 ms and `β` 0.218 ms (within about 0.3 ms).
  The batch closes the loop: `k = n_out·λ_d·α / (1 − n_out·λ_d·β)`, with
  `λ_d = λ/N_d` the rate per decode pod and `n_out` the output length. At
  B's last step `n_out·λ_d·β` is 0.78: the decode batch is near its own knee,
  and that is why the decode pod grows faster.
- **The prefill load does reach the pull on 2+4.** There the pull's unshared
  work rises from 27.7 to 32.3 ms, and a decode-only pull reads 6-25 ms
  short. Closed forms that fit the level of both fleets within 10 % miss the
  growth on 2+2:

| Pull model, fitted on B-off and B-onset together | 2+2 error | 2+4 error | 2+2 growth, measured +15 ms |
| --- | --- | --- | --- |
| Decode pods only | +10 to +14 % | −31 to −6 % | +17.5 ms |
| One stage, decode load plus 0.43 x prefill utilization (`S` 20.8 ms) | −9 to +8 % | −4 to +1 % | +21.1 ms |
| Two stages in a row, decode receive and prefill send | −9 to +9 % | −6 to +10 % | +8.5 ms |

That is why the model below learns the prefill load's effect on the pull as
a table rather than a formula.

- **The prefill pod's overhead stretches too.** Around its compute the
  prefill pod spends 17 to 25 ms on 2+2. That overhead times `1 − ρ_p` (the
  prefill pod's utilization) is flat: 13.5-14.1 ms over B-off, 14.9-15.6 ms
  over C-on. On 2+4 it falls from 13.0 to 10.2 ms, so a flat figure reads
  high there. A line in `ρ_p` also fits B-off (within 0.3 ms), but its slope
  is 28, 54 and 94 ms on the three phases: it does not carry across fleets.

The model built from per-pod rates alone (prefill as processor sharing at
`S` 25.0 ms, the pull on decode at 24.6 ms, the decode term as
`5.18 x ITL(k) + 11.5 ms`; this check holds the overhead fixed):

| Phase, first step to last | Decode pod, measured | Decode pod, model | Prefill compute, measured | Prefill compute, model |
| --- | --- | --- | --- | --- |
| B-off-r1, 2+2 | +46.9 ms | +47.2 ms | +16.0 ms | +14.7 ms |
| B-onset-on-r1, 2+4 | +48.0 ms | +19.8 ms | +83.8 ms | +56.7 ms |
| C-on-r1, 2+2 bursty | +64.2 ms | +46.2 ms | +36.7 ms | +14.6 ms |

On 2+2 Poisson the split is reproduced, each step's decode pod within +1 to
+5.3 ms (the model reads high). On 2+4 the miss is the pull (above) and the
prefill stage near its knee, where a 4 % error in `S` moves the stretch a
lot. The bursty phase reads short on both sides. On prefill, this check is of
means at `Ca²` 1, and the processor-sharing burst rule
([above](#as-built-processor-sharing)) is what prices bursts. On decode, the
batch itself runs larger than the closed loop says (running 38.5 against
32.8 at C's last step, `4 x ITL` 42.5 against 37.0 ms), and nothing in the
model prices that yet (below).

So the model has three parts that matter, in this order on a fleet with as
many decode as prefill pods: the decode steps, the pull, the prefill stage.
The figures are per-step means of the per-pod vLLM histograms in each
phase's controller recording, after the step's first 180 s.

### The model

    TTFT(N_p, N_d) = a(ρ_p) + T_P(λ/N_p) + T_X(λ/N_d, λ/N_p) + m_d·ITL(k(λ/N_d)) + d + R

- `T_P` is the prefill pod as processor sharing, as built. Its capacity
  `V_P` comes from a new P1 source, the queue-free inversion
  `S = T/(1 + λT)` on each prefill pod, read where the engine queue is 0
  ([measured](#measured-where-ttft-goes-and-why-prefill-sizing-s2-is-not-built-yet):
  within 5 % over a fourfold load range). It is published as the
  `llmsm_slo_ttft_vp_source` value `inverted`, and the existing sources win
  when the pod does queue.
- `T_X` is the pull as processor sharing on the decode pod, its unshared work
  learned by the same inversion on the decode pods. The prefill load enters
  as a learned table, not a formula: unshared pull work by prefill
  utilization, in buckets of 0.1. The pull grows with prefill utilization,
  and a smaller prefill fleet raises it, so a count that puts prefill past
  the highest bucket seen cannot be shown to meet the target. It is treated
  as not meeting it and flagged `PullUnseen`. The running count's own bucket
  is always seen, so it is priced at its measured pull. Below the lowest
  bucket seen the lowest bucket's figure is used, which errs high.
- `m_d·ITL(k) + d` is the decode pod's own part: the wait for a step, the
  first token and the rest of the decode pod. `ITL(k) = α + β·k` is fitted
  from the decode pods' (running, ITL) pairs, not from the ITL learner's
  band, which needs a KV fill the decode pods here never reach (the reason S1
  refused as `no-decode-side`). `m_d` and `d` are fitted from the decode
  pod's own histograms (queue less pull, first token, TTFT). `k` comes from
  the closed loop above, and a count where `n_out·λ_d·β ≥ 1` cannot hold the
  batch and is refused.
- `a(ρ_p) = a₀/(1 − ρ_p)` is the prefill pod's overhead around its compute,
  not P1's `a`. P1 refuses `a` on long prompts (`MaxAShortestPrompt`, 2500
  tokens, in `internal/signals/prefill/fixed.go`), and where it has one it is
  8-11 ms, not the 17-25 ms the prefill pods showed on 2+2. `a₀` is the
  highest of `(AvgTTFT − AvgQueueTime − AvgPrefillTime)·(1 − ρ_p)` seen on
  the prefill pods, flagged `OverheadMeasured`: the stretched form carries
  across fleets where a line does not (above), and the highest value errs
  high. A flat figure at the lowest load seen would read 7.5 ms short on
  B-off, the unsafe side.
- `R` is as built. The percentile is priced as built (the gammas of
  processor sharing), at the policy's percentile. The decode term is
  near-constant at a given load, so it shifts the distribution and adds no
  spread.

This assumes one variant per role. Several variants per role are not
designed here (below).

### The count search

Find the cheapest `(N_p, N_d)` whose predicted TTFT at the policy's
percentile meets the target. Cost is replicas times each role's accelerator
count, or the share's GPU units under utilization share.

The search only considers pairs legal under the **no-trade rule**: no cycle
trades one role down for the other up. A role's ask goes below its running
count only if that lower count, with the other role at its running count,
meets the target. A partial grant (a limiter, quota, the share optimizer,
`maxReplicas`) then cannot scale the fleet into a miss.

In this order:

1. **`target-below-floor`.** The fixed parts alone (`a`, `d`, `R` and the
   work with no stretch) are over the target. No count helps: abstain.
2. **`target-past-max-replicas`.** The target is reachable, but only past a
   role's `maxReplicas`. The ask is each role at its `maxReplicas`, flagged:
   the most this variant is allowed, and still short.
3. **`diminishing-return`.** The cheapest pair that meets the target costs
   more than `slo.ttft.maxCostRatio` (default 2) times the larger of
   saturation's ask and the running fleet. Abstain: the target is not worth
   that fleet without an operator saying so. Measuring against the running
   fleet too keeps a low-load cycle, where saturation asks little or
   nothing, from abstaining into a miss.
4. **The box.** Each role moves at most `slo.ttft.maxStep` replicas a cycle
   from where it runs, a new knob (a count, by default `max(1, half the
   running count)`). The box is bounded below by `minReplicas` and above by
   `maxReplicas`. The TTFT falls as either count rises, so for each `N_p` in
   the box a bisection finds the least `N_d`. Ties go to fewer replicas in
   all, then to the shape that runs.
5. **`OutOfReachThisStep`.** The cheapest pair overall is worth it (3 did not
   abstain) but lies outside the box. The ask is one corner: the role whose
   full step lowers the predicted TTFT more goes to the top of its step, and
   the other stays at its running count. Each role's step is judged with the
   other at its running count, as the no-trade rule judges. Only one role
   moves, because the pair after one step is priced next cycle from the
   fleet that ran, not from a guess about the second role. The next cycle
   asks again from there.

### How the SLO analyzer acts

- **Opt-in.** `slo.mode: enforce` ([SLO tiers](slo-tiers.md), phase 1, not
  parsed yet) plus a new `slo.ttft.sizing: joint`. Observe stays the default
  mode. J3 also needs SLO tiers' step 2c (TTFT enforce). `joint` without
  `enforce` is shadow: the ask is published, nothing acts on it. Without
  either, nothing changes.
- **Per role.** The result carries a `RoleDemand` for prefill and for decode,
  each the count it asks, in its own frame. Today `RoleDemand` carries decode
  only (`internal/engines/analyzers/slo/analyzer.go`, where it is set). The
  composite takes the frame needing most per role, as it does for every
  analyzer. Saturation still sizes for throughput; the SLO analyzer can only
  raise a role above it.
- **Scale-down.** Its ask is honest at lower counts too, so it agrees to a
  scale-down only to a count it predicts meets the target, and only under the
  no-trade rule.
- **Utilization share.** The ask enters the share optimizer as per-role
  demand, as any composite-raised role demand does, priced in the share's
  GPU units, and the SLO count is a floor for the share
  ([SLO tiers, item 7](slo-tiers.md#what-the-composite-needs-before-phase-1)).
  A partial grant is safe under the no-trade rule, and the next cycle prices
  the fleet that ran.
- **Abstain.** A NaN need for both roles, its reason on
  `llmsm_slo_ttft_reason`, comes from either of two things:
  - a missing input: `no-prefill-capacity` (no `V_P`), `no-overhead` (no
    `a`), `no-pull` (no pull figure), `no-itl-line` (no `ITL(k)` fit),
    `too-few-cycles`;
  - the self-check, `self-check-under` or `self-check-over`. It compares the
    model's mean TTFT at the running fleet with the router's
    (`llm_d_epp_request_ttft_seconds` sum over count, within 1-3 ms of the
    client's). The router's p90 cannot be used: near the target it sits in a
    histogram bucket 51-79 % wide. The check is asymmetric: it abstains after
    3 cycles with the model's mean more than 5 % under the router's, or more
    than 15 % over, and re-enters after 3 cycles back inside that band.
    Bursty traffic is expected to trip `self-check-under` (the decode batch,
    above).

  `diminishing-return` and `target-past-max-replicas` are on the same
  series; the flags (`PullUnseen`, `OverheadMeasured`, `OutOfReachThisStep`)
  are on `llmsm_slo_ttft_flag`.
- **What abstaining does.** Today the engine keeps `slo` out of the
  composite (`eligibleEntries`) and out of the scale-down grace
  (`listedInGrace`, both in `internal/engines/steadystate/engine_v2.go`).
  J3 lifts both exclusions only for a policy with `slo.mode: enforce` and
  `slo.ttft.sizing: joint`; every other policy, and every replay of one,
  keeps them (`eligibleEntries` is also what the replay uses). The NaN
  result carries `VariantCapacities` for both roles, since the composite's
  hold covers only the roles a result covers. A NaN abstain then holds
  scale-down of both roles until 3 intervals after its last informative
  result, as for any live analyzer whose result is not informative
  (`composite.go`); after that, saturation sizes alone.

### Validation against the staircase

Replay every phase that has a controller recording (A-off-r1, B-off-r1,
C-on-r1, B-onset-on-r1, C-onset-on-r1, and the goldens' ttft-staircase
fixtures) with the joint mode on, priced at the fleet that ran. The bar:

| Check | Pass |
| --- | --- |
| Predicted TTFT at the policy's percentile, at the fleet that ran, against the client's | −5 to +10 % at every step below the knee |
| A step that missed the target | at least one more replica than ran, of the role the split blames (decode on 2+2, prefill on 2+4) |
| A step that held | at most one more replica in all |
| The mode off | every replay golden byte for byte |
| Mutants | without the `ITL(k)` term 2+2 fails the missed-step bar; with a decode-only pull 2+4 fails it |

C (bursty) is expected to fail the model row until a burst term on `k`
exists (below), and it is reported, not tuned away. The phases with no
recording (B-off-r2, B-on, B-onset-on-r2, C-off, H-*) are checked on the
Prometheus split only: the predicted growth per part against the measured.
A live staircase with the mode on comes after the replays pass.

### Phases

1. **J1, inputs in shadow.** The inverted `V_P`, `a₀`, the pull table, the
   `ITL(k)` fit and `m_d`, `d`, published on the SLO status; the what-if CLI
   prices the joint model. Gate: the model row of the bar.
2. **J2, the search in shadow.** `slo.ttft.sizing: joint` without `enforce`.
   The ask per role is published on a new `llmsm_slo_ttft_*` series, not on
   `llmsm_analyzer_demand`, where dashboards would read it as a decision.
   Gate: the whole bar on the replays.
3. **J3, opt-in.** Under `slo.mode: enforce` with SLO tiers' step 2c,
   `slo.ttft.sizing: joint` enters the composite. Gate: a kind e2e with the
   emulator and one live staircase with the mode on.
4. **Joint by default.** `joint` becomes the default value of
   `slo.ttft.sizing` under `enforce`, only after a second shape passes:
   another prompt length and bursty arrivals. Observe stays the default
   mode.

### What it leaves out

- Bursts on decode: the batch runs larger than the closed loop says (C's
  last step, running 38.5 against 32.8), so bursty fleets read short on the
  decode side. A burst term on `k` is open.
- Several variants per role: the search assumes one prefill and one decode
  variant per model.
- The pull past the prefill utilization seen is not priced: such counts are
  not asked for.
- The burst rule reads high at light load, as built: the cost side.

## As built (J1)

J1 learns the joint model's inputs and prices its mean, in shadow. Nothing
acts on either: no ask, nothing in the composite, and the engine still keeps
`slo` out of the composite and the scale-down grace. It runs for a model with
one prefill and one decode variant under a TTFT target, behind the replay
switch `slo.SwitchTTFTJointInputs` (`slo-ttft-joint-inputs`), on by default.

**What it learns** (`prefill.JointWindow`, `internal/signals/prefill/joint.go`),
from the per-pod histograms the collector already reads, over the last hour:

| Input | How | Notes |
| --- | --- | --- |
| `S`, a prefill pod's unshared work | `T/(1 + λT)` on a pod whose engine queue was empty, median | "Empty" is a mean queue time of at most 1 ms: vLLM reads 5 to 10 µs on a pod that does not queue. |
| `V_P` (`inverted`) | the prompt over `S` | Stands only where P1 refused V_P. A pod that queues gives P1 a source, and P1 wins. |
| `a₀` | `(AvgTTFT − AvgQueueTime − AvgPrefillTime)·(1 − ρ_p)`, its 0.9 quantile | Not the highest: one scrape read 17.4 ms where its step read 13.6 to 14.6. |
| The pull table | `T/(1 + λT)` of the KV transfer on the decode pods, median per 0.1 of prefill utilization | Below the lowest bucket the lowest; in a gap the next bucket up; past the highest the highest, flagged `PullUnseen`. |
| `ITL(k) = α + β·k` | least squares on the decode pods' (k, ITL) | `k` is the minute's mean batch by Little's law: the generated-token rate × ITL, or completions × the last minute's `n_out` × ITL, every figure a one-minute one, not the running gauge. The gauge is an instant, and its noise against a minute's ITL flattens the slope: on B, β 0.167 ms from the gauge, 0.219 ms from the minute (0.218 above). Refused with α below 0. |
| `m_d`, `d` | least squares of the decode pod's TTFT less its pull against ITL | B reads 4.96 and 11.2 ms, against 5.18 and 11.5 above. Refused where the line reads the decode pod's own part at or below 0 over the ITL it was read at. |

Neither line is fitted over less than 10 % spread of its x ((max − min)/mean):
flat load gives least squares no slope to read. A-off's first step is that
flat, so it is not priced.

A decode pod with more waiting requests than 4 × (1 + λ·pull) is behind on
its batch and reads nothing: B's pods held 1 to 5 below the knee and 61 to 106
past it.

**How it prices** (`whatif.JointTTFTMean`): the mean of
`a₀/(1 − ρ_p) + S/(1 − ρ_p) + X/(1 − λ_d·X) + m_d·ITL(k) + d + R`, arrivals
split evenly over each role's pods, `k` from the closed loop, refused as
`batch-unheld` when `n_out·λ_d·β ≥ 1`. `R` is the decode term's hop where it is
fitted, and the assumed 20 ms (`RouterHopAssumed`) otherwise. The mean only:
the percentile is J2's.

**What you see:** `llmsm_slo_ttft_joint_input`, `llmsm_slo_ttft_joint_mean_seconds`,
`llmsm_slo_ttft_joint_reason` and `llmsm_slo_ttft_joint_flag` (`OverheadMeasured`,
`PullUnseen`, `RouterHopAssumed`), with no `statistic` label since they are
means; `inverted` on `llmsm_slo_ttft_vp_source`; and a `slo-ttft-joint` line in
the cycle log with the parts and what the engines measured beside them.
`whatif joint-ttft --recording FILE [--prefill N] [--decode N] [--rate R]`
prices any fleet from a recording, and exits 3 on a refusal.

### The model row

`TestGoldens_TTFTJointModelRow` replays the four staircase phases committed as
goldens and compares, per step, after its first 180 s, the joint mean at the
fleet that ran with what the engines measured. The recordings hold no router
mean (they predate `TTFTSumRate`), so both sides leave `R` out: the prefill
pods' mean TTFT plus the decode pods', which on these phases is the router's
mean less 9 to 11 ms. "Frozen" prices each later step from the inputs as they
stood after the second step: the extrapolation J2's search will lean on.

| Phase | Step, r/s | Model, ms | Measured, ms | Error | Frozen |
| --- | --- | --- | --- | --- | --- |
| A off, 2+2, 1k prompts | 17.0 | not priced | 34.3 | | |
| | 23.5 | 39.5 | 39.5 | +0.0 % | |
| | 29.9 | 46.7 | 48.7 | −4.1 % | −9.0 % |
| B off, 2+2, 8k | 17.7 | 108.7 | 107.8 | +0.8 % | |
| | 24.1 | 124.6 | 124.6 | +0.1 % | |
| | 30.5 | 149.0 | 148.4 | +0.4 % | −1.1 % |
| | 33.1 | 160.7 | 158.6 | +1.3 % | +1.0 % |
| | 35.7 | 176.3 | 174.7 | +0.9 % | +2.3 % |
| | 38.6 | 202.0 | 199.0 | +1.5 % | +6.9 % |
| B on, 2+2, 8k | 18.1 | 110.1 | 111.0 | −0.8 % | |
| | 24.4 | 125.4 | 123.8 | +1.3 % | |
| | 30.9 | 149.2 | 147.6 | +1.1 % | −1.3 % |
| | 34.0 | 163.7 | 158.9 | +3.1 % | +0.9 % |
| | 36.1 | 175.6 | 172.4 | +1.9 % | +0.1 % |
| | 38.4 | 196.5 | 195.6 | +0.5 % | −1.9 % |
| | 39.6 | 235.5 | 260.2 | **−9.5 %** | −13.2 % |
| C on, 2+2, 8k, `Ca²` 4 | 17.4 | 121.4 | 119.8 | +1.3 % | |
| | 24.1 | 145.9 | 146.0 | −0.1 % | |
| | 30.5 | 176.8 | 178.2 | −0.8 % | −0.3 % |
| | 33.1 | 195.0 | 195.6 | −0.3 % | +2.3 % |
| | 36.2 | 222.7 | 225.3 | −1.1 % | +6.6 % |
| | 39.0 | 260.4 | 285.1 | −8.7 % | +19.9 % |

Each phase's last step overloaded (the engines' mean went to seconds) and is
past the knee; it is left out.

On 2+2 Poisson, 14 of the 15 steps priced below the knee are inside the bar's
−5 to +10 %, from −4.1 to +3.1 %. The miss is B on's last step, 9.5 % under: the
decode batch stands at 0.86 of its own knee there (`n_out·λ_d·β`), where the
closed loop is steepest and the measured ITL rises faster than the line fitted
below it. That is the SLO side: a count priced there would be too small. It is
held as a known miss, not tuned away. C reads within 1.3 % to its fifth step,
better than the bar expects, because `m_d` and `d` are fitted on C's own
bursty steps; its last step reads 8.7 % under, on the same side.

The in-sample row is the easy one: every input is read from the same pods
it is compared with. The frozen column is harder. Up to three steps past what
it saw it reads −9.0 to +6.9 %. Further out C reads 20 % high (the cost side),
and B on 13 % low at its last step (the SLO side). Most of those cycles are
past the pull table's highest bucket (`PullUnseen`).

**Not checked here.** B-onset (2+4) has no recording in the repository, so the
2+4 miss on the pull (above) is not replayed, and nothing here tests whether
the pull table carries the prefill load's effect there. No recording has the
router's mean, so `R` and the self-check against the router are not checked
either.

**Golden changes.** On the replays with S1 on, the six fixtures with one
prefill and one decode variant under a TTFT target gain the `Joint` field on
every row of `slo-published.csv`; the staircases price its mean on 84 of 128
(A off), 269 and 274 of 289 (B off, B on) and 276 of 292 (C on) rows. On the four staircases S1's `vp_source` reads
`inverted` where it read `none`, and its reason moves from
`no-prefill-capacity` to `no-fixed-latency`: no cycle holds P1's `a`, so S1
prices none either way. The open-loop prefill-stress runs, which are not
`slo` goldens, now have `V_P` too: S1 prices the short one on 63 of 105
cycles, unchecked against the router (`TestS1_OnThePrefillStressGoldensAsP1LearnsThem`).

**Left for J2 and J3:** the count search, the percentile, the self-check,
`slo.ttft.sizing`, `maxStep`, `maxCostRatio`, per-role `RoleDemand`, and
enforce mode.

## Open questions

1. **Is the per-pod model right above the prefill onset?** *Recommend:* keep it,
   flagged, until the flow-control-on phase scores it.
2. **Model the dispatch-gate delay?** *Recommend:* not until the staircase shows
   a residual at the onset larger than the proxy hop.
3. **Is `nixl_xfer_time_seconds` per request or per chunk?** (carried from
   [SLO tiers](slo-tiers.md) open question 4). Neither: it is per NIXL handle.
   From vLLM's source, not measured: [Q1, as built](slo-tiers.md#as-built-q1).
   A request posts its handles together, so they overlap and do not add. The
   mean reads under the slowest of them, which is what a request waits for.
4. **Does `V_P` vary with prompt length?** It does: run 3's low-load medians
   cost about 60% more per token from 30k to 37k than from 8k to 30k. `a`
   takes the curve (above), and P3 rescales `V_P` along it to the prompt
   asked, or, with no curve, flags a `V_P` learned at another mean
   length (`VPShapeMoved`; [after P2](#as-built-p3-after-p2)). A mix of
   lengths at one mean is under-priced
   ([a limit of P3](#prefill-capacity)), and no flag sees it. *Recommend:*
   key `V_P` by input bucket; add a term to `own_prefill` only if bucket B
   misses by more than 15%.
5. **Burstiness.** *Recommend:* estimate `Ca²` from the arrival rate's variance,
   cap that estimate at 4; the sticky-routing term is not capped.
6. **Several priority bands.** *Recommend:* the highest band only, flagged. Cut
   from the CLI: the collector sums over priority, so a recording cannot say
   ([as built](slo-capacity-planner.md#ttft-as-built)).
7. **Which imbalance signal for `f`?** *Recommend:* the computed-token share in
   busy minutes, de-biased; queue length when no busy minute exists.
