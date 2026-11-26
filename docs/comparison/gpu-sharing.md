# Sharing a GPU budget between models

llm-d's side was checked on 2026-11-18, against llm-d commit
[`fbc9d5c7`][recipe]. Line numbers below are in that commit's recipe README.

## The short answer

When several models in several namespaces share a fixed set of GPUs, and the
GPUs should follow the load, llm-d's Kueue recipe is the wrong tool on its own.
Kueue is a Kubernetes add-on that holds a pod back until there is quota for
it. llm-d's recipe, "Kueue-Based Replica Rebalancing", uses it to give each
model a guaranteed share of the GPUs and to lend idle GPUs to the others. It
enforces a budget well. It was not built to move GPUs from a quiet model to a
busy one. The scaling manager's utilization share does that, and the plan is
to run it on top of Kueue, with Kueue enforcing the budget underneath.

Why the recipe alone falls short:

- **It is built for one namespace.** Its first prerequisite is "Two or more
  model server Deployments in one namespace" ([README:33][r33]), and its Kueue
  settings name a single namespace ([kueue.values.yaml:48-53][values]).
- **It does not move GPUs by load.** Each model gets a guaranteed floor.
  Once every model holds its floor, the rule is "you may preempt to reach your
  own floor, never to go beyond it. Above your floor you can only take what is
  idle" ([README:416-417][r416]). A quiet model keeps its GPUs until its own
  autoscaler lets them go, however busy the other model is.
- **The autoscalers cannot see the budget.** "The HPA still makes every
  scaling decision" ([README:390][r390]). Each model asks for the replicas it
  wants, and what does not fit waits as Pending pods. Alerts on "replicas !=
  desired" "now fire by design" ([README:395-397][r395]).
- **It does not cover P/D models.** In a P/D model (prefill and decode on
  separate pods) one replica is several pods. The recipe covers plain
  Deployments only ([README:33][r33]). Kueue admits each pod on its own
  ([README:9-10][r9]), so we infer that half of a P/D replica could start
  while the other half waits. Nobody has tested this.
- **There is no safe way to try it first.** Opting in models that already
  serve restarts every model server ([README:295-296][r295]), and the budget
  is enforced from that moment. The recipe has no mode that only reports what
  it would do.

## At a glance

| What the business needs | llm-d Kueue recipe | Scaling manager |
| --- | --- | --- |
| Models in different namespaces share one GPU budget | Not covered. Written for one namespace. | Yes. One budget across namespaces. Run several times on GPU clusters, including a user's own run; not yet enough runs to call it production-ready. |
| GPUs follow the load when the budget is full | No. A busy model gets only idle GPUs once every model has its floor. | Yes. GPUs move from the model with the most to spare to the model that needs them, by load and by each model's weight. |
| A guaranteed minimum per model | Yes. A model takes its floor back at once. | Yes. A model's floor is never taken. Getting GPUs back takes minutes. |
| The budget is never exceeded | Yes, enforced. A pod over budget never gets a GPU, batch jobs on the same queues included. | Not on its own. It plans inside the budget, but needs a quota or Kueue underneath to enforce it. |
| Planning knows the budget | No. Demand over budget turns into Pending pods. | Yes. Replicas are planned inside the budget, so few pods wait. |
| P/D and multi-pod models | Not covered. | Planned as one model. Multi-pod replicas are funded whole. Tested only on a local test cluster (kind). |
| A safe way to turn it on | No. Every serving model server restarts and the budget applies at once. | Yes. Watch only, then report what it would move, then act, one model at a time if wanted. |
| How fast GPUs move | At once for a model taking back its floor. Otherwise only when the quiet model's own autoscaler scales down. | Minutes: the giving model scales down and drains, then the new pod starts. A model that just gave GPUs waits about 12 minutes to take them back, unless the imbalance is large. |
| Cost | Holds no spare GPUs. | Keeps spare GPUs running ahead of need: 36% to 43% more GPU time in the P/D runs. |
| Maturity | Experimental. | Experimental. One or two benchmark runs per configuration, small models on 1 GPU each. |

## What Kueue does well, and how we use it

Kueue is good at enforcement, and better at it than the scaling manager alone:

- **It is a hard limit.** A pod over budget is held at a gate and never
  reaches a GPU. The scaling manager's own budget is advisory: it plans
  inside it, and relies on a quota or Kueue to stop anything else.
- **It takes a floor back at once.** When a model that lent its GPUs needs
  them again, Kueue preempts (stops a running pod to free its GPU) a
  borrowed pod immediately ([README:412][r412]). The scaling manager waits for
  the other model to scale down, minutes.
- **It covers more than model servers.** Training and batch jobs can wait in
  the same queues.
- **It is simple.** A few queue objects and one label per model.

So the plan is to put Kueue underneath the scaling manager: Kueue enforces
the budget, and the scaling manager decides which model gets the GPUs. The
scaling manager can already read its budget from Kueue (see
[With Kueue](#with-kueue)). What is missing, and the order in which we will
add it, is tracked in
[issue #180](https://github.com/llm-d-incubation/llm-d-scaling-manager/issues/180):

1. A supported path for several namespaces, built from the recipe's own queue
   objects, with an end-to-end test.
2. Treat each model's Kueue floor as its floor, and report it when Kueue
   preempts a pod the scaling manager placed, so the two never fight.
3. Read budgets that Kueue keeps on a cohort (a group of queues that lend
   unused quota to each other), which read as zero today, and queues per
   P/D role.
4. Admit a P/D replica whole.
5. Use Kueue preemption to give a model its floor back faster, or document
   the trade-off.
6. Close a gap where any managed namespace can point a queue at another
   model's budget.

Until that lands, the two can work against each other on one cluster. See
[Running both on one cluster](#running-both-on-one-cluster).

## Details for engineers

### What the scaling manager does

llm-d-scaling-manager shares a budget with the experimental
[utilization-share optimizer](../well-lit-paths/utilization-share/):

- **It picks the donor.** Each model is first given the GPUs its demand
  needs, and the rest is shared by weight. GPUs are taken from the model best
  off against its demand and weight, and only down to its share of the budget;
  never below its floor or its last replica. When the budget cannot cover
  every model's need, GPUs go to the model worse off by weight, and a donor
  can go below its need, down to its floor.
- **It plans each move.** The donor is lowered first. The receiver is raised
  only once those GPUs are actually free. A move is called off while it is
  still waiting if the load swings back, and a model that just gave GPUs
  cannot take them back for a while (the reversal hold, about 12 minutes with
  a 300 s scale-down window).
- **It drains through scale-down.** The chosen donor pod is marked to be
  removed first and goes through an ordinary scale-down, so in-flight requests
  are kept as well as the model server drains them.
- **It knows the nodes.** On a cluster-scoped install it opens a hole that fits
  every pod of the receiver's replica, node by node, so a multi-pod replica
  (for example a LeaderWorkerSet group) is funded whole. A P/D model is planned
  as one model, and is held whole when either role cannot be sized. The node
  fit and multi-pod replicas are tested on kind only, not yet on real
  hardware.
- **It shares what is left.** What a warm pool wants and does not hold is
  taken out of the shared budget. The budget can be a declared quota, the
  quota Kueue holds (see [With Kueue](#with-kueue)), or, on a cluster-scoped
  install, the physical GPUs.
- **It rolls out in stages.** Passive (decides, changes nothing), then
  shadow (reports the moves it would make), then active, and one model can be
  handed over first ([start here](../well-lit-paths/utilization-share/README.md#start-here)).

It costs headroom held ahead of need (36% to 43% more GPU-seconds in the P/D
runs). A move takes minutes: the donor's scale-down window and drain, then
the receiver's own start (image, weights, engine ready). When the imbalance is
large (the busier model at 0.9 or more of its scale-up threshold, a donor at
0.6 or less after giving), the move skips every extra wait of its own; a
smaller imbalance first waits two confirmation cycles. The evidence is one or
two runs per configuration, on 1-GPU pods of 8B models. Sharing across
namespaces has been run several times on GPU clusters, including a user's own
run; that is not yet enough to call it production-ready. See
[what it costs](../well-lit-paths/utilization-share/README.md#what-it-costs)
and the [evidence](../well-lit-paths/utilization-share/README.md#evidence).

### What llm-d's recipe does

The recipe is [Kueue-Based Replica Rebalancing][recipe], in
`guides/workload-autoscaling/kueue-rebalancing/`. It was last changed on
2026-09-24 (commit `77f33f16`). llm-d marks it experimental, and its 1.0
documentation lists it as "Share a GPU Budget Across Models (Experimental)".
Each model's HPA, generated by KEDA from EPP or WVA metrics, scales on its
own ([README:39-42][r39]); nothing in the recipe compares one model's load
with another's.

- **Queues.** Each model gets a ClusterQueue whose `nominalQuota` is its
  guaranteed floor, in a shared cohort, and a LocalQueue its pods name
  ([README:161-165][r161]). "Floors should sum to the physical GPU count"
  ([README:181][r181]).
- **Admission.** "Every replica pod becomes its own Kueue `Workload`, and
  Kueue holds a pod at a scheduling gate until GPU quota is free for it. The
  HPA is never touched" ([README:9-10][r9]).
- **Preemption.** The [ClusterQueues][cqs] set only `reclaimWithinCohort: Any`
  and `withinClusterQueue: LowerOrNewerEqualPriority`, so
  `borrowWithinCohort` stays at `Never`: "a borrowing pod never evicts
  anyone — it waits for someone to go idle" ([README:423-425][r423]). When
  both models want more than their floor, "each model sits on its floor and
  the surplus stays gated" ([README:427-430][r427]).
- **Fair sharing**, offered in [Tuning][r432] instead of floors: the whole
  budget on a `Cohort` object, `nominalQuota: 0` and a `fairSharing.weight`
  on each ClusterQueue, "Fully elastic, with no guaranteed floor"
  ([README:444-447][r444]). Kueue then "prefers to preempt Workloads from
  ClusterQueues that have the highest share value first"
  ([Kueue preemption](https://kueue.sigs.k8s.io/docs/concepts/preemption/)):
  the donor is picked by share, not by load.
- **One namespace.** The prerequisite is one namespace ([README:33][r33]);
  `kueue.values.yaml` narrows Kueue's webhooks and controller to it
  ([README:100-123][r100], [kueue.values.yaml:48-53][values]). Two models in
  one namespace are told apart only by their LocalQueues
  ([README:465-466][r465]).
- **Open queues.** Both ClusterQueues set `namespaceSelector: {}`
  ([clusterqueues.yaml:23, 52][cqs]): they accept a LocalQueue from any
  namespace Kueue manages. With one managed namespace that is harmless; it
  matters once the install is widened.
- **Rollouts.** A rolling update needs `maxSurge: 0`, or the surge pod is
  gated and "the rollout waits behind a pod that can never start"
  ([README:266-279][r266]). Rollouts are then terminate-first and slower
  ([README:462-464][r462]).
- **Opt-in.** Labelling the pod template "does change the pod template, so
  every model server restarts once" ([README:295-296][r295]).
- **No ResourceQuota.** The namespace must have no GPU `ResourceQuota`: a
  gated pod still counts against one ([README:43-46][r43]).
- **Nominal accounting.** "Quota above physical capacity produces
  unschedulable pods rather than gated ones" ([README:451-454][r451]). The
  recipe does not mention warm pools, scale-to-zero, LeaderWorkerSet or P/D.
- **Pending pods.** Over budget, "extra pods are created and gated"
  ([README:409][r409]); llm-d's
  [workload autoscaling overview][overview] says "over-budget demand shows up
  as `Pending` pods rather than a lowered `maxReplicas`".

### Side by side

| | Kueue rebalancing (llm-d) | Utilization share (llm-d-scaling-manager) |
| --- | --- | --- |
| Idle GPUs used | Yes: borrowed when a model's HPA asks for more pods. | Yes: spread ahead of need as running headroom, by weight; it is not given back while every model's peak fits. |
| GPUs moved from a quieter model to a busier one when the budget is full | No with the recipe's floors. With fair sharing, from the model furthest over its weighted share, not by load. | Yes, by each model's demand against its share. |
| Who decides a model shrinks | Its own HPA, on its own load. Kueue preempts a borrowed pod when a lender reclaims its floor (or, with fair sharing, by share). | The optimizer picks the donor and how many GPUs it gives. |
| How GPUs move | The HPA deletes pods, or Kueue preempts one; a waiting pod is then admitted. | A planned move: the donor gives first, and the receiver grows once those GPUs are free. The donor gives through an ordinary scale-down, choosing which pod goes first; it drains as any deleted pod does. |
| Namespaces | One ([README:33][r33]). | Several, with a cluster-scope budget (cluster-wide or namespace-scoped install). Run several times on GPU clusters, including a user's own run; not yet enough runs to call it production-ready. |
| Placement | GPUs counted against each ClusterQueue's quota; node placement is left to the scheduler. | Node-aware on a cluster-scoped install: the receiver's replica must fit in the holes the donors open, node by node, and multi-pod replicas are funded whole. Tested on kind only. A namespace-scoped install counts GPUs only. |
| P/D | Not covered; one Workload per pod. | One model, held whole when either role cannot be sized. |
| Signal | The pods each HPA requests. | Each model's demand against its share, with confirmation cycles, a tolerance band and a reversal hold. |
| Enforcement | Hard: an over-budget pod is gated. | Advisory: back it with a GPU `ResourceQuota` or Kueue. |
| Time until the busier model serves more | With floors, until the quieter model's own HPA scales down (its scale-down window), then the new pod's start (image pull, weights load, engine ready), which is minutes for a large model. Preemption skips the scale-down window: with floors only for a model returning to its floor; with fair sharing for any model under its weighted share. A preempted pod drains for its `terminationGracePeriodSeconds` ([README:398-399][r395]). | The donor's scale-down window and the new pod's start; there is no preemption shortcut. With a large imbalance (on by default: the busier model at 0.9 or more of its scale-up threshold, the donor at 0.6 or less after giving) nothing else is added. A smaller imbalance first waits two confirmation cycles, and a model that just gave GPUs waits out the reversal hold (about 12 minutes with a 300 s scale-down window). Headroom held ahead of need serves the model that rises first at once: in the latest aggregated run, median TTFT 0.07 s against 25.6 s with the same fleet planned but not moved. |
| Rollout | Opting in a serving model restarts it; enforced at once. | Passive, shadow, active; one model can go first. |
| Cost and maturity | Experimental. Holds no headroom: idle GPUs stay unused until a model's HPA asks for them. | Experimental. Headroom held ahead of need (36% to 43% more GPU-seconds in the P/D runs). A group freezes for about three minutes when it starts acting, after every restart or upgrade. One or two runs per configuration, 1-GPU pods of 8B models; whole-node and multi-GPU pods not measured. Sharing across namespaces run several times on GPU clusters; not yet enough runs to call it production-ready. |

### Running both on one cluster

Today the two can get in each other's way. Each item is a phase of
[issue #180](https://github.com/llm-d-incubation/llm-d-scaling-manager/issues/180).

- **ResourceQuota.** The recipe forbids a GPU `ResourceQuota` on the
  namespace ([README:43-46][r43]); the utilization-share path recommends one,
  because its own budget is advisory
  ([what it needs](../well-lit-paths/utilization-share/README.md#what-it-needs)).
  With Kueue enforcing, leave the `ResourceQuota` out and declare a
  `gpu-inventory` limiter if the queues also run other work.
- **`lendingLimit: 0`**, offered in the recipe's Tuning section
  ([README:438-440][r438]), keeps a model's floor from being lent, so it stops
  the scaling manager's moves out of that queue.
- **Preemption of a placed pod.** Kueue can preempt a borrowed pod that the
  optimizer placed by a transfer. The optimizer does not report that as a
  transfer outcome. This is an inference; no test covers it.
- **Fair sharing.** The recipe's fair-sharing layout reads as a budget of
  zero (see below).

### With Kueue

The optimizer can take its budget from Kueue. To share GPUs between the models
on a set of ClusterQueues, read Kueue at cluster scope: a quota entry with
`scope: cluster` and `kueue.enabled`. The budget is then the sum of every
ClusterQueue's `nominalQuota`, one budget for every namespace, and Kueue still
admits each pod. This also works for the recipe's own layout, both models in
one namespace on two ClusterQueues: 10 GPUs shared by two models.

The recipe's flavor has no product label, so the entry names the accelerator
type and `-1` lets Kueue give the number
([merge rules](../reference/quota-limiter.md#merge-rules)). Name your nodes'
GPU product in `quotas`, and make sure the models' type resolves to it: the
recipe's model servers set no GPU nodeSelector, so the type comes from the
nodes' `nvidia.com/gpu.product` label (or set a GPU nodeSelector). A model
whose type stays unknown is left out of sharing. The entry goes in the scaling
policy's `default` entry, beside `optimizer:`
([scaling policy](../reference/scaling-policy.md#optimizer-cluster-default-only-live)):

```yaml
limiters:
  - name: cluster-quota
    type: quota
    scope: cluster
    kueue: { enabled: true }
    quotas:
      H100: -1        # Kueue's ClusterQueues give the number
```

The [cluster-wide utilization-share guide](../guides/utilization-share-cluster-wide/)
sets this up (its option C), including the product label check. Its preflight
warns about a namespace with LocalQueues on several ClusterQueues; that rule
is for namespace-scope entries, and a cluster-scope budget is the sum either
way.

What does not work, or is not shown yet:

- **A namespace-scope entry** gives each namespace its own budget, so with one
  model per namespace there is nothing to share.
- **The recipe's fair-sharing layout** sets every `nominalQuota` to 0 and keeps
  the budget on a Cohort object, which the scaling manager does not read, so
  it sees a budget of zero.
- **Queues that also run other GPU work** (training, batch) need a
  `gpu-inventory` limiter beside the entry
  ([Kueue-bounded quotas](../well-lit-paths/kueue-bounded-quotas/README.md#when-not-to-take-it)).
- **Borrowing.** The budget is the nominal quota. Moves between two queues
  rely on Kueue lending a donor's idle floor through the cohort, so
  `lendingLimit: 0` stops them. The optimizer itself starts no borrowing
  beyond the budget.
- **Measurement.** A cluster-wide Kueue quota has not been measured yet.
  Sharing across namespaces has been run several times on GPU clusters, but
  taking that budget from a Kueue ClusterQueue has not been measured on its
  own.

[recipe]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md
[cqs]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/optimized-baseline/base/clusterqueues.yaml
[values]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/kueue.values.yaml#L48-L53
[overview]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/README.md?plain=1#L143-L144
[r9]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L9-L10
[r33]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L33
[r39]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L39-L42
[r43]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L43-L46
[r100]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L100-L123
[r161]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L161-L165
[r181]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L181
[r266]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L266-L279
[r295]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L295-L296
[r390]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L390-L391
[r395]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L395-L399
[r409]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L409
[r412]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L412
[r416]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L416-L417
[r423]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L423-L425
[r427]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L427-L430
[r432]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L432-L447
[r438]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L438-L440
[r444]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L444-L447
[r451]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L451-L454
[r462]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L462-L464
[r465]: https://github.com/llm-d/llm-d/blob/fbc9d5c78bb25d9410f9587bc7e55ddc3db5ad03/guides/workload-autoscaling/kueue-rebalancing/README.md?plain=1#L465-L466
