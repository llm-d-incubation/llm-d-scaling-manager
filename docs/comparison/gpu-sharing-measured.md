# Sharing a GPU budget: the measured comparison

> **Status, 2026-11-26: one valid pair. It is a pointer, not the verdict.**
> One arm of each kind ran to the end and passed the run's checks: arm A
> (llm-d's recipes) first, then arm B (the scaling manager). The verdict rule,
> fixed before the run, needs the full A-B-B-A order and both pairs agreeing.
> A full A-B-B-A re-run, on the router llm-d documents for arm A's signal, is
> in progress.

[Sharing a GPU budget between models](gpu-sharing.md) explains, from the
documents, how llm-d's recipes and the scaling manager share GPUs. This page is
the experiment that tests it on real GPUs: what ran, what the first pair
showed, and where arm A is not exactly what llm-d documents.

## In short

- **The question.** Two models share 6 GPUs. When the load moves from one to
  the other, do the GPUs follow it?
- **What the first pair showed.** With the scaling manager (B), the busy model
  got its GPUs sooner and more requests met the SLO: 30.6% of all requests
  against 11.7%. No pods waited at Kueue's gate, against 80 pod-minutes under
  llm-d's recipes (A). B used 1.15 times A's GPU-hours.
- **What tempers it.**
  - It is one pair, and A ran first, on cold caches.
  - Both arms were overloaded in every peak, so most requests missed the SLO
    in both.
  - Arm A's router is not the one llm-d documents for its default autoscaling
    signal (see [Where arm A differs](#where-arm-a-differs-from-llm-ds-recipes)).
    That slowed A's scale-up by an amount this run cannot separate out.
- **Next.** The full A-B-B-A re-run. Both arms now use llm-d's
  optimized-baseline router without flow control, as the `inflight` path is
  documented (see [Open questions](#open-questions)).

## The question

Several models share a fixed set of GPUs, and their peaks come at different
times. llm-d today scales each model on its own load, and leaves the budget to
Kueue, a Kubernetes add-on that holds a pod back until there is quota for it.
The scaling manager plans all the models together inside the budget. The run
tests four claims from [gpu-sharing.md](gpu-sharing.md):

| | Claim | Expected to favour |
| --- | --- | --- |
| H1 | GPUs do not follow the load under A: once each model holds its floor, a busy model gets only idle GPUs. | B |
| H2 | A's autoscalers cannot see the budget, so demand over it waits as Pending pods. | B |
| H3 | Kueue gives a model its floor back faster (by preemption). | A |
| H4 | B costs more GPU-hours, because it keeps spare GPUs running. | A |

## What ran

| | Arm A: llm-d's recipes | Arm B: the scaling manager |
| --- | --- | --- |
| Who sets the replica counts | One KEDA ScaledObject per model, from llm-d's [KEDA + EPP guide][keda-guide], default `inflight` signal ([ScaledObject][keda-so]) | The [utilization-share optimizer][ushare], acting (`shadow: false`), through one KEDA ScaledObject per model with an `external-push` trigger |
| Signal | `sum(llm_d_epp_inflight_requests{producer_name="inflight-load-producer"})` per model, `AverageValue` 16 | Saturation analyzer; scale-up 0.85, scale-down 0.70 (shipped defaults) |
| KEDA polling, cooldown | 15 s, 300 s | 5 s, 30 s |
| HPA scale-up | 300 s stabilization window, then up to 100% per 15 s | No window, up to 100% per 5 s |
| HPA scale-down | 300 s window, up to 100% per 15 s | 300 s window, up to 100% per 120 s |
| Replicas per model | 1 to 8 | 1 to 8 |
| Budget | llm-d's [Kueue rebalancing recipe][kueue-recipe] (experimental): one ClusterQueue per model with a floor (`nominalQuota`) of 3 GPUs, one cohort, `reclaimWithinCohort: Any`, `withinClusterQueue: LowerOrNewerEqualPriority`, no borrowing or lending limits; queue label on the pod template; `maxSurge: 0` | The same Kueue objects, which still admit every pod. The scaling manager plans inside a namespace quota of `H200: 6`; it does not read Kueue's quota |
| Version | llm-d v1.0.0 guides; Kueue recipe at commit `fbc9d5c7` | [Scaling manager v0.12.4][v0124] (commit `bb647fbe`), image pinned by digest |

The same in both arms:

- **Models:** two separately named deployments of `Qwen/Qwen3-32B` (BF16) on
  vLLM v0.26.0, 1 H200 GPU per replica, at most 32 sequences at once, weights
  on the node's local disk. They stand in for two different models of the same
  size, so the result says nothing about a small model next to a large one.
- **Router:** llm-d router v1.0.0 endpoint picker, InferencePool chart v1.5.0,
  the same plugin config in both arms. In the first pair that config had flow
  control on (below). In the re-run it is llm-d v1.0.0's
  [optimized-baseline config][ob-router], byte for byte, with no flow control.
  Gateway: agentgateway v1.2.1, one of the gateways the KEDA guide names.
- **KEDA v2.18.1 and Kueue v0.20.2**, as already installed on the shared
  cluster.
- **Budget:** 6 GPUs, a floor of 3 per model. Up to 8 replicas each, so the
  two together can ask for more than the budget.
- **Load:** the same 65-minute schedule and seeds in every arm (see
  [Method](#method)).

B's ScaledObject is the one the scaling manager ships: it decides when to
scale, so its HPA adds no scale-up window of its own. That is part of what is
being compared, not a setting tuned for this run.

## Where arm A differs from llm-d's recipes

We checked every value of arm A against llm-d's files at v1.0.0 (the
in-flight ScaledObject is the same on `main`). The differences:

1. **In the first pair, the router had flow control on.** The re-run removes
   this difference: arm A, and arm B with it, run the optimized-baseline
   router config. This is how the first pair differed. llm-d documents the
   `inflight` signal as needing no flow control, on the optimized-baseline
   router config (a prefix-cache affinity filter and a token-load scorer). Both
   arms of the first pair ran
   the router config of llm-d's KEDA flow-control overlay
   ([router.values.yaml][keda-router]): the `flowControl` gate and the queue,
   KV-cache, prefix-cache and no-hit-LRU scorers, with the
   `inflight-load-producer` added so A's metric exists. Flow control is on
   because the scaling manager's benchmark setup turns it on: the scaling
   manager reads the router's queue as demand (it strictly needs it only to
   scale from zero). Keeping one router for both arms kept routing equal, but
   it changed two things for A:
   - **A's signal stopped at about 37 requests per ready replica.** Measured:
     X's in-flight count was 37 at 1 replica, 73 at 2, 110 at 3 and 185 at 5;
     Y's was 148 at 4. That is vLLM's 32 running sequences and a few more. The
     requests waiting in the router's queue were not counted. At a target of
     16, A's HPA could ask for at most about 2.3 times the replicas it had
     ready, and each step waited out the 300 s scale-up window. In P1, X's HPA
     asked for 2 and then 3 replicas 5 to 6 minutes into the phase, and for 7
     about 6 minutes after that. Without flow control those requests would
     wait in vLLM instead, where we expect the producer to count them, so A
     would ask for more at once. This is not measured.
   - **Every failed request was a flow-control rejection.** All 14,444 failed
     requests in A and all 12,034 in B returned HTTP 429. Each came back 60 to
     61 s after it was sent: the router held it in its queue for that long,
     then shed it. Without flow control these requests would wait at the
     model server instead. How many would then meet the SLO is not measured.
2. **Versions.** The Kueue recipe pins Kueue 0.19.1; the cluster runs v0.20.2.
   The KEDA guide names no version and links the 2.20 docs; the cluster runs
   v2.18.1. Both are shared, so they were used as installed.
3. **A smaller budget.** The recipe's example is 5 + 5 of 10 GPUs; here it is
   3 + 3 of 6, with the same queue settings.
4. **Tenant safety.** Our own queue, flavor and cohort names, and each
   ClusterQueue admits pods from the run's namespace only (the recipe admits
   every namespace).
5. **One more label in the query.** Both models share one namespace, as the
   Kueue recipe requires, so the query also selects the model's router.
6. **8 replicas per model, the guide's value, above a 6-GPU budget.** The
   Kueue recipe says `maxReplicaCount` should be a sane physical ceiling. With
   8, A's HPAs can ask for pods that can never be admitted, and those count in
   A's gated pod-minutes. B had the same maximum.

The recipe's own 300 s scale-up window is not a deviation, but it matters: in
P1, A took 5.0 minutes longer than B to give X its 5 replicas, the length of
that window.

## The first valid pair

Each peak needs 5 replicas for the busy model. P3 needs 9, more than the
budget.

| | Arm A | Arm B |
| --- | --- | --- |
| **Requests meeting the SLO** | | |
| X alone at peak (P1) | 0.1% | 39.4% |
| Y after the swap (P2) | 0.1% | 0.1% |
| Both at peak (P3): X / Y / together | 0.5% / 0.0% / 0.2% | 0.2% / 41.9% / 23.7% |
| Whole arm, both models | 11.7% | 30.6% |
| Requests meeting the SLO per GPU-hour | 1,537 | 3,489 |
| **Time to GPUs** | | |
| Until X holds its 5 replicas (P1) | 12.2 min | 7.2 min |
| Until Y holds its 5 replicas (P2) | not reached (at most 4) | 13.7 min |
| Until Y is back at its floor of 3 (P2) | 6.5 min | 7.7 min |
| **Budget** | | |
| Pod-minutes waiting at Kueue's gate, P3 | 80.1 | 0 |
| Kueue preemptions | 16 | 0 |
| **Failed requests (all HTTP 429)** | 20.9% | 17.4% |
| P1 X / P2 Y / P3 X / P3 Y | 5,442 / 5,409 / 1,078 / 2,515 | 0 / 6,313 / 4,952 / 769 |
| **GPU-hours** | | |
| Whole arm | 5.26 | 6.06 (1.15x) |
| Quiet phases (P0 + P4) | 1.32 | 1.58 |

SLO: time to first token at most 2 s and time per output token at most 50 ms.
A failed request counts as a miss.

### How to read it

- **Both arms were overloaded in every peak.** Adding replicas took 7 to 14
  minutes of a 15-minute phase, and the 90th-percentile time to first token
  was about 60 s in every peak of both arms. The SLO numbers mostly say how
  soon each arm added replicas.
- **In the swap (P2) neither arm served Y on time** (0.1% in both). B got Y
  its 5 replicas 1.3 minutes before the phase ended. A never did: X came
  down to 2 replicas, not 1, and kept them, which left Y at most 4 of the 6
  GPUs. B failed
  more of Y's requests in P2 (6,313 against 5,409).
- **With both at peak (P3), B gave Y more and X less.** Y met the SLO 41.9% of
  the time against 0.0%, but X's failures rose from 1,078 to 4,952. Taken
  together, 23.7% against 0.2%.
- **Kueue gives a floor back faster.** Y was back at its floor 1.2 minutes
  sooner under A, by preemption (H3 favours A, as expected).
- **B's extra GPU-hours** came mostly from the warm-up (0.58 against 0.33) and
  P1 (1.48 against 0.96): B keeps spare GPUs running and grew X sooner. Per
  GPU-hour, B delivered 2.3 times as many requests inside the SLO. That is one
  pair, not a claim about cost.
- **A ran first, on cold caches.** The first replica to start on a node
  compiles its engine: about 100 s, against about 60 s once the cache is warm
  (measured earlier on the same hardware). This run skipped calibration, so
  more of those first starts fell in arm A. They were not timed per pod.
- **By the rule's numbers, this pair would count for H1** (Y in P3, 41.9
  points better) **and for H2** (80.1 against 0 pod-minutes). The rule needs
  two agreeing pairs, so it gives no verdict yet. H4 is 1.15, under the 1.5
  flag.

### Validity

Both arms passed the checks fixed before the run: no engine error or node
failure; the load generator kept up (90th-percentile send delay at most
2.2 ms in every phase); each autoscaler asked for more than one replica when
the load needed it, and scaled on its intended metric. An arm is void if
more than 2% of requests fail for reasons outside the arms (the load
generator or the gateway). The 429s are the router turning away requests the
arm's replicas could not take, so they count as misses, not as faults.

## Method

**Load.** Poisson arrivals with 1,000 input and 250 output tokens per request,
fixed seeds per model, so every arm replays the same requests. Rates are in
units of c, the rate one replica serves inside the SLO: c = 4.0 requests per
second. c came from one replica with no autoscaler on a staircase of rates:
4.0 passed (time to first token 0.64 s and time per output token 27.5 ms, at
the 90th percentile); 5.0 failed (time to first token 20.9 s).

| Phase | Minutes | X | Y | Replicas needed (X + Y) | Tests |
| --- | --- | --- | --- | --- | --- |
| P0 | 10 | 2 rps | 2 rps | 1 + 1 | warm-up |
| P1 | 15 | 18 rps | 2 rps | 5 + 1 | X alone at peak, 2 above its floor |
| P2 | 15 | 2 rps | 18 rps | 1 + 5 | the swap (H1, H3) |
| P3 | 15 | 14 rps | 18 rps | 4 + 5 = 9, over 6 | both at peak (H1, H2) |
| P4 | 10 | 2 rps | 2 rps | 1 + 1 | idle GPUs held (H4) |

**Order: A, B, B, A,** so each arm type gets an early and a late slot. Between
arms both models go back to 1 replica, then 5 quiet minutes with no pod
waiting in Kueue. B keeps what it has learned from one B arm to the next, as a
long-running controller would; this pair's B arm was the first, so it carried
nothing from an earlier arm.

**Decision rule** (fixed before the run; both pairs must agree, else the
result is "mixed"):

- **H1** holds if, in both pairs, B gives Y its replicas in P2 at least 2
  minutes sooner than A, or B meets the SLO for the busier model in P2 or P3
  at least 10 points more often.
- **H2** holds if A has gated pods in P3 in both A arms and B has at most 20%
  of A's pod-minutes.
- **H3** is reported as measured.
- **H4** is B's GPU-hours over A's; above 1.5 it is flagged as a cost to weigh.

**An arm is void** if a model server hits an engine error or a node fails;
more than 2% of requests fail outside the arms; a model's autoscaler never
asks for a second replica when the load needs one; or the load generator falls
behind (90th-percentile send delay over 1 s in any phase). Before each arm,
the run checks that the arm's signal exists: Prometheus has A's in-flight
series, and B's HPAs scale on the scaling manager's metric. An arm is never
re-run on its own.

**Recorded per arm:** every request (latency and outcome), each model's
replicas every 15 s (running, ready, waiting at the gate), Kueue events, and
the objects as applied. The raw data is kept with the run, not in this
repository.

## Open questions

What an llm-d maintainer would fairly ask us to change. The re-run in progress
takes up the first; the others are still open.

1. **The router for arm A.** Taken up in the re-run: A runs on the
   optimized-baseline router without flow control, as the `inflight` path is
   documented. B runs on the same router. With flow control off, B loses only
   scale from zero, which this run does not use, so the autoscaler stays the
   only difference between the arms. The other way, flow control with the
   `queue` signal that the guide pairs with it, is not tested.
2. **The scale-up window.** 300 s is the guide's value, chosen to damp
   overshoot while new replicas load. A run with a shorter window would show
   how much of the P1 gap is the window.
3. **The replica ceiling.** A maximum of 6 per model (the whole budget) would
   cut the pods A asks for and can never get.
4. **More than one A-B-B-A**, and pod start times recorded per arm, to size
   the order effect.
5. **Models of different sizes**, which this run does not cover.

## How we got here

- **Runs 1 and 2 gave no result.** Run 1's 8B models were too fast to
  calibrate: one replica met the SLO at every rate tried. In run 2 neither
  arm's signal worked: A's in-flight metric did not exist (the router did not
  list its producer), and B's HPAs were built on CPU.
- **Run 3 gave this pair.** It then stopped itself at the start of arm 3 (B),
  because B's HPAs came up on CPU again. The likely cause is a start-up race
  between KEDA's connection to the scaling manager and the controller
  restarting. The fix waits, and checks that the HPAs use the scaling
  manager's metric, before B starts.
- **Run 4 gave no result.** Its load generator never started. Its pod needs a
  CPU-only node, and those were full, so no request was sent. The run now checks
  for room before it takes any GPU.
- **The re-run** is the full A-B-B-A with these fixes and with llm-d's
  documented router for both arms. It takes about 6.5 hours and at most 38
  GPU-hours.

## Links

- llm-d [v1.0.0][llmd-v1]; its [KEDA + EPP guide][keda-guide], the
  [in-flight ScaledObject][keda-so] and the
  [flow-control router config][keda-router]; the
  [optimized-baseline router config][ob-router]
- llm-d's [Kueue rebalancing recipe][kueue-recipe] and its
  [ClusterQueues][kueue-cqs], at the commit compared against
- llm-d router [v1.0.0](https://github.com/llm-d/llm-d-router/releases/tag/v1.0.0)
- Kueue [v0.20.2](https://github.com/kubernetes-sigs/kueue/releases/tag/v0.20.2);
  KEDA [v2.18.1](https://github.com/kedacore/keda/releases/tag/v2.18.1) and its
  [Prometheus scaler](https://keda.sh/docs/2.18/scalers/prometheus/)
- vLLM [v0.26.0](https://github.com/vllm-project/vllm/releases/tag/v0.26.0)
- Scaling manager [v0.12.4][v0124]; utilization share:
  [when to use it][ushare] and [how to turn it on](../guides/utilization-share/)

[llmd-v1]: https://github.com/llm-d/llm-d/releases/tag/v1.0.0
[keda-guide]: https://github.com/llm-d/llm-d/tree/v1.0.0/guides/workload-autoscaling/keda-epp
[keda-so]: https://github.com/llm-d/llm-d/blob/v1.0.0/guides/workload-autoscaling/keda-epp/optimized-baseline/overlays/k8s/inflight/scaledobject.yaml
[keda-router]: https://github.com/llm-d/llm-d/blob/v1.0.0/guides/workload-autoscaling/keda-epp/optimized-baseline/router.values.yaml
[ob-router]: https://github.com/llm-d/llm-d/blob/v1.0.0/guides/optimized-baseline/router/optimized-baseline.values.yaml
[kueue-recipe]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md
[kueue-cqs]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/optimized-baseline/base/clusterqueues.yaml
[v0124]: https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/tag/v0.12.4
[ushare]: ../well-lit-paths/utilization-share/
