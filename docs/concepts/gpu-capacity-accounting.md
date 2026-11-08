# GPU Capacity Accounting

How the scaling manager decides whether there are GPUs for a scale-up or a
wake, and where that answer can still be wrong.

It decides this only when the scaling policy declares a `limiters:` list. The
shipped policy declares none: scaling is then bounded only by each workload's
`maxReplicaCount`, and a scale-from-zero wake is not checked. Setting one up is
in [Bound every install by real GPUs](../guides/admin-gpu-bounding/).

The same answer is used by every part that asks for GPUs: the optimizer's
scale-ups, the scale-from-zero wake, the warm pool's growth and the
utilization-share optimizer.

> **Every accelerator must resolve first.** A limiter budgets GPUs per
> accelerator type. A variant whose type the scaling manager cannot tell (no
> GPU `nodeSelector` or `nodeAffinity`, and no running pods to look at) gets no
> budget and **never scales up**, with no error. Check for
> `AcceleratorNotResolved` events before declaring a limiter
> ([how](../reference/gpu-limiter.md#what-has-to-be-true-first-every-accelerator-must-resolve)).

## What each limiter bounds

| limiter | asks | counts as used | bounds |
| --- | --- | --- | --- |
| `gpu-inventory` | is the hardware there? | every GPU a pod holds on the GPU nodes, whoever owns it: other teams, training jobs, system pods, and pods scheduled or nominated but not running yet | the GPUs the nodes advertise, per accelerator type, and each replica to one node (below) |
| `quota` | is this tenant within its allowance? | only what the scaling manager's own replicas and warm pools hold: replicas times GPUs per replica | the caps you declare, per accelerator type, for the cluster or per namespace |
| `quota` with `kueue.enabled` | the same, with Kueue's numbers | the same; for a namespace-scope entry, Kueue's figure is first reduced by what other workloads Kueue admitted hold in the queue | the smaller of your cap and Kueue's grant, plus what the namespace may borrow from its cohort |

The two kinds count differently on purpose. A quota is an allowance: charged
with every GPU on the nodes, a 4-GPU quota beside an unrelated 4-GPU training
job would read as spent while the scaling manager had placed nothing. Whether
the hardware is full is the physical limiter's question. So a quota alone does
not stop the scaling manager asking for GPUs that other work already holds; the
extra replicas wait in `Pending`. Declare both kinds for both guarantees.

Details: [the GPU limiter](../reference/gpu-limiter.md) and
[the quota limiter](../reference/quota-limiter.md).

## How limiters compose

- **Every declared limiter is built, and a decision must fit all of them.** Per
  accelerator type the smallest figure wins, and a type any limiter denies is
  denied.
- **GPU names form families.** `H100` is a family; `NVIDIA-H100-80GB-HBM3` is
  a product in it. A family cap bounds the sum of its products, and a GPU spent
  under one name is spent under the other. A quota that lists only products
  denies the products it does not list.
- **A namespace quota bounds only the namespaces it governs**: those it lists,
  and, with a `default` key, every other namespace it does not exclude. An
  excluded or unlimited (`-1`) namespace is bounded by the cluster-wide limiters
  alone, and by nothing when there are none.
- **A limiter that cannot answer is skipped**, and the others still bound. Its
  usage may not be observed yet, or its read failed; the controller names it
  in `llmsm_constraint_provider_skipped`. Only when every limiter is skipped is a
  decision unchecked. A wake then uses the last answer if it is under 5
  minutes old. A Kueue-backed quota that has never read Kueue is not skipped:
  its finite caps still bound, and `llmsm_kueue_never_read` reports it.
- **Budgets are per controller.** Each controller charges a static quota only
  with what its own models hold. N controllers that read one `scope: cluster` quota
  allow N times the figure.

## Node fit and the in-flight hold

With `gpu-inventory` declared, a replica must also fit **one node**. Sixteen
free GPUs as 4+4+8 hold one 8-GPU replica, not two.

- Each pod is planned into the smallest free hole that holds it, on a node of
  its accelerator that is schedulable (not cordoned, Ready), whose taints it
  tolerates, and whose labels match its `nodeSelector` and required node
  affinity.
- A P/D model grows only when a prefill and a decode replica both find a node.
- A replica just ordered keeps its hole from every later decision until its
  pods hold a node, for up to **5 minutes** (a replica on a Kueue flavor: its
  `placementTimeout`, when that is longer, and three times that while a
  cluster-autoscaler scale-up is pending). Without this hold the next
  decision would see the hole free again while the pod is still being
  scheduled.
- The plan assumes the scheduler packs pods the same way. When replicas of
  different sizes share nodes, configure the scheduler for it
  ([how](../reference/gpu-limiter.md#the-scheduler-must-pack)).

Node fit counts GPUs only, not CPU or memory. The full rules and limits are in
[A replica must fit one node](../reference/gpu-limiter.md#a-replica-must-fit-one-node).

## Kueue borrowing at a glance

With a Kueue-backed quota, a namespace may grow past its ClusterQueue's
nominal quota into capacity its cohort lends. On by default
(`countBorrowable: false` turns it off).

- **Guaranteed** is what the namespace keeps whatever the cohort does.
  **Borrowable** is drawn from the cohort's free GPUs, counted only from
  lenders Kueue would take capacity back for.
- Borrowing is **withheld** (capped at nominal) when a condition for it does
  not hold, and **frozen** (nothing new, last figures kept) when Kueue cannot
  be trusted right now. The reason is published.
- A P/D pair borrows only whole. A wake, the warm pool and rescale never
  borrow.
- The scaling manager gives back a borrowed replica that never placed (after
  `placementTimeout`). When one of its own models grows into nominal its queue
  lent out, it also lowers its own borrower in the same pass (`kueue-reclaim`).
  Kueue preempts the rest when a lender needs it.

The rules and every reason: [Cohort borrowing](../reference/quota-limiter.md#cohort-borrowing).

## What an operator sees

| signal | means |
| --- | --- |
| `limiter` and `policyAccepted` in the `scaling-manager-controller-status` ConfigMap | which limiters this controller runs with, and whether it accepted the policy |
| `ResourceConstrained` Event on the ScaledObject, ending `(limited by <name>)` | a limiter capped this decision; `<name>` is the entry, `gpu-limiter` for the physical one, `node-fit` when the GPUs exist but no node holds a replica |
| `llmsm_decisions_limited_total{limiter_name}` | the same, as a counter |
| `llmsm_model_scaling_blocked{reason}` | why a model is held back |
| `llmsm_constraint_provider_skipped{provider, reason}` | a limiter left out of decisions; the others still bound |
| `llmsm_scale_from_zero_wake_unchecked_total` | wakes made with no capacity check at all |
| `llmsm_unattributed_gpus` | GPUs held by workloads whose accelerator did not resolve |
| `limiterBudgets` in `scaling-manager-controller-status` | each quota's guaranteed, borrowable and used GPUs, per namespace and accelerator |

Runbooks: [GPUs held or quota spent](../reference/troubleshooting.md#gpus-held-or-quota-spent).

## Where the numbers can still be wrong

1. **An unresolved accelerator, under a quota.** A quota charges a model by
   its accelerator type, so the GPUs of a model whose type did not resolve are
   charged to no cap, and the quota looks freer than it is by that much
   (`llmsm_unattributed_gpus`). An explicit `unknown` entry in the quota bounds
   them. The physical limiter counts GPUs by the node they are on, so it is
   not affected. The fix is a GPU `nodeSelector` on the workload.
2. **Cordoned or NotReady nodes** still count toward a pool's GPUs. Node fit
   leaves them out, so they matter only when node fit is off (no node
   snapshot younger than 2 minutes).
3. **A namespace-scoped controller without a cluster-wide Pod list** counts
   only the GPUs its own namespace holds. GPUs other tenants hold on the same
   nodes read as free (`llmsm_pod_access_denied` = 1). Grant `list` on Pods
   cluster-wide, or bound it with a quota
   ([details](../reference/gpu-limiter.md#when-policy-demands-a-limiter-the-controller-cannot-serve)).
4. **Kueue's charge is read, not watched.** A namespace-scope Kueue figure is
   charged with what other workloads Kueue admitted hold in the queue, as of
   the last read. Replicas can still wait at Kueue's gate when:
   - the entry is cluster-scope, which is charged with the managed GPUs only;
   - a variant Kueue does not manage holds GPUs, which makes the charge read
     low;
   - several namespaces on one queue grow in the same cycle, each into the
     same free GPUs;
   - a replica is gated where borrowing is not counted: it lowers the charge,
     and nothing releases it.

   [Details](../reference/quota-limiter.md#what-a-quota-is-charged).
5. **An FMA warm pool** holds GPUs that no pod requests. A launcher keeps its
   engine on a GPU after it unbinds, charged to nobody, so in an FMA namespace
   every figure is a lower bound on use. A `ResourceQuota` has the same blind
   spot. Nothing in the scaling manager can see it
   ([details](../reference/gpu-limiter.md#fma-namespaces-the-gpu-picture-is-a-lower-bound)).

How this is built in the code: [How the limiter set is built](../developer-guide/limiter-composition.md).
