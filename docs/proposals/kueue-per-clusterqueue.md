# Model Kueue per ClusterQueue, not per namespace

- **Status:** proposed, revised after review (see [Review](#review)).
- **Problem:** Kueue admits against a ClusterQueue. The scaling manager reads
  Kueue per namespace, taking the smallest of the queues a namespace has a
  LocalQueue on. Where a namespace maps to more than one queue, or a queue
  serves more than one namespace, the figures are a guess.
- **Decision:** bind each variant to the ClusterQueue its pods are admitted
  through, and compute every Kueue figure per queue, spent once per pass by
  every variant bound to it. A variant that cannot be bound keeps today's
  per-namespace figure, with a reason.
- **Shape:** four stages. The first only reads and reports the binding.
- **Replaces:** #186's `multi-queue-namespace` withholding and its
  per-(namespace, queue) charge, for bound variants.

## Summary

Today a Kueue-bounded quota gives each **namespace** a figure: the smallest
grant among the ClusterQueues its LocalQueues point to. Kueue does not work
that way. A workload names one LocalQueue (`kueue.x-k8s.io/queue-name`), that
LocalQueue names one ClusterQueue, and Kueue admits the workload against that
queue alone.

This proposal makes the scaling manager do the same. Nominal, guaranteed,
reach, the charge of other workloads, co-resources and managed usage are all
computed per queue. A queue shared by two namespaces is one budget, spent
once.

For an operator: a namespace on two queues stops being capped at the smaller
one, a P/D model whose roles use different queues gets each role's own queue,
a shared queue stops being over-ordered, and a variant whose queue cannot be
told says so with a reason.

## Terms

As in [Kueue borrowing, and limiters that
compose](kueue-borrowing-and-limiter-composition.md#terms): nominal,
guaranteed, within nominal, cohort, lender, flavor. Plus:

- **Binding**: the ClusterQueue a variant's pods are admitted through.
- **Charge**: what Kueue has reserved in a queue for workloads that are not
  this controller's managed replicas (a training job, another tenant).

## Motivation

All figures are GPUs; every replica is 8 GPUs.

### What #186 fixed, and how

#186 fixed three findings of the Kueue review on the per-namespace model:

1. **Other workloads' reservations were ignored** (KU1). Fixed by
   `chargeNonManaged` (`allocation/quota_charge.go`): each namespace's figures
   are charged with the queue's `Reserved` less what that namespace's managed
   variants hold, with gated pods not cancelling it (`SetGatedSource`).
2. **A namespace on two queues was granted their sum** (KU2). Fixed in
   `namespaceCaps` (`kueue/reader.go`): the smallest single queue's figure per
   type, and borrowing of a type two queues grant withheld with
   `multi-queue-namespace`.
3. **Growth into own lent-out nominal preempted own borrowers** (KU3). Fixed by
   `yieldReclaimed` (`allocation/borrow_reclaim_yield.go`).

What the reader already gets right, and this proposal keeps: a LocalQueue
whose ClusterQueue declares no GPU resource is skipped (a CPU batch queue
says nothing about GPUs), and one whose ClusterQueue's `namespaceSelector`
does not select the namespace is skipped too (`kueue/reader.go`). A
ClusterQueue with `stopPolicy` `Hold` or `HoldAndDrain` is read as inactive.

### What is still wrong

The per-namespace model cannot be made exact: it lacks which queue a replica
goes to.

1. **The interim min under-spends.** CQ-S nominal 16, CQ-B nominal 8, one
   namespace. Every serving variant labels CQ-S. The namespace is capped at 8
   for the type, so serving stops at one replica with 8 GPUs of CQ-S free.
2. **P/D roles on different queues.** Prefill labels CQ-P (nominal 16),
   decode labels CQ-D (nominal 32), same namespace and type. Both roles are
   capped at 16 and neither borrows that type. When the utilization share
   moves GPUs from decode to prefill, the nominal it frees is CQ-D's, which is
   no headroom for prefill on CQ-P; the share's transfer accounting does not
   know that (`utilization_share_ledger.go`).
3. **A shared queue is a ceiling, not a partition.** CQ-T, nominal 16, has
   LocalQueues in namespaces A and B; Kueue has 8 reserved. Each namespace
   sees 8 free, and in one pass both order a replica: 16 ordered against 8
   admissible. The second waits at the gate until the placement timeout.
4. **Co-resources per namespace on a shared queue.** CQ-T has 64 CPUs; each
   replica asks 48. A and B each fit one: 96 ordered against 64.
5. **Usage is keyed per namespace.** Managed usage (`SetManagedUsage`) and
   gated GPUs (`GatedGPUs`) are per namespace and accelerator. During a
   rollout that changes a variant's queue, old pods still charge the old
   queue, and a namespace key cannot say so.
6. **The label is never read.** `kueue.QueueNameLabel` is declared and used
   by nothing (`kueue/kueue_constants.go`).

## Goals

- Bind every Kueue-bounded variant to one ClusterQueue, by the rules Kueue
  itself uses, and say with a reason when that cannot be done.
- Compute nominal, guaranteed, reach, charge, co-resource headroom and managed
  usage per queue; spend each queue's headroom once per pass.
- Keep figures per queue for P/D roles on different queues, and in the share.
- Remove `multi-queue-namespace` and the per-(namespace, queue) charge for
  bound variants.

## Non-goals

- Changing cohort arithmetic. The cohort's free pool is already per cohort,
  flavor and resource (`SharedPools`).
- **Two controllers on one queue.** Two namespace-scoped controllers whose
  namespaces share a ClusterQueue each spend the whole queue. This proposal
  does not solve that; it detects and reports it (below). The fix is the
  multi-tenant Lease.
- Flavor choice inside a queue (`PinRequired` stays).
- Writing the label. The scaling manager never edits templates.

## Design

### Binding a variant

For each variant whose namespace a Kueue-bounded quota entry governs:

1. **Read the queue name.**
   - Above zero replicas, the truth is what Kueue saw: the `queue-name` label
     on the variant's pods, or the Workloads' `spec.queueName`. A variant
     whose pods name two queues (a rollout in progress) is bound to the
     template's queue for new growth; each pod's usage stays on its own queue.
   - At zero, read the scale target's own `metadata.labels` first (for a
     Deployment and a LeaderWorkerSet, Kueue's integration reads the label on
     the object and its webhook copies it into the pod templates), then the
     pod templates: the leader template, or the worker template when the
     LeaderWorkerSet has no leader template (as `scaletarget/lws.go` does).
     Object and templates must agree; if they do not, the variant is unbound
     with `queue-label-mismatch`. One LeaderWorkerSet group is one Workload,
     so leader and worker cannot be admitted through different queues.
2. **Look up the LocalQueue** in the variant's namespace. The reader already
   lists LocalQueues; keep `namespace/name -> spec.clusterQueue`.
3. **Check Kueue would admit through it**, with the reader's existing checks
   kept:
   - the ClusterQueue exists and declares the variant's GPU resource;
   - its `namespaceSelector` selects the namespace;
   - neither the ClusterQueue's nor the LocalQueue's `spec.stopPolicy` holds
     it, and the LocalQueue's `Active` condition is true;
   - the ClusterQueue covers every resource the replica requests (a resource
     it does not cover is never admitted).
4. The binding is that ClusterQueue.

Outcomes other than a binding:

| Case | Kueue's behaviour | Here | Reason |
|---|---|---|---|
| No label, a LocalQueue named `default`, `LocalQueueDefaulting` known on | admits through `default` | bind to it | none |
| No label otherwise | not managed (`manageJobsWithoutQueueName` false), or managed with no queue and never admitted | unbound: today's per-namespace min | `queue-unbound` (info) |
| Label names no LocalQueue, or its ClusterQueue is not read | pending forever | grows by nothing Kueue-backed | `queue-not-found` |
| LocalQueue or ClusterQueue stopped or inactive, GPU or other resource not covered, or `namespaceSelector` excludes the namespace | never admitted | grows by nothing Kueue-backed | `queue-not-admitting` |
| Object and template labels disagree | depends on the webhook | unbound: today's per-namespace min | `queue-label-mismatch` |

"Exactly one GPU queue in the namespace" is **not** treated as a binding: a
pod with no label is not admitted through that queue.

The `LocalQueueDefaulting` gate is read from the `kueue-controller-manager`
Deployment's arguments, which the reader already reads for the version (only
when a cohort exists today; it would now be read whenever a variant has no
label). Whether `manageJobsWithoutQueueName` is set is in Kueue's
Configuration ConfigMap, which needs a new grant (see [RBAC](#rbac)); without
it the answer is unknown and the variant stays on the fallback.

The binding is read every cycle with the variant's other template fields, so
a template change rebinds on the next cycle.

### One entry per queue

```go
type QueueBudget struct {
    Queue      string
    Cohort     string
    Flavors    []FlavorFigures // nominal, guaranteed, within nominal, reach
    Charge     map[string]int  // per flavor/resource: Reserved - managed admitted
    CoHeadroom map[string]int  // CPU, memory, ... per flavor, as Kueue charges
    Managed    map[string]int  // admitted managed usage, by the queue each pod carries
    Gated      map[string]int  // managed pods at the gate, by the queue each pod carries
    Variants   []string        // bound variants, any namespace
}
```

- **Managed usage and gated GPUs are keyed per queue**, by the queue label each
  pod carries, not the variant's current binding. `SetManagedUsage` and
  `GatedGPUs` change key from namespace to queue. During a rollout that moves a
  variant to another queue, its old pods count against the old queue.
- **Charge** is the queue's `Reserved` less the managed replicas admitted in
  it. Gated replicas do not cancel it.
- **Co-resources** are what Kueue charges a pod: the larger of the sum of its
  containers and its largest init container, plus `overhead`, after the
  Configuration's `resources.transformations` and less
  `resources.excludeResourcePrefixes`. Where the Configuration is not
  readable, a template with transformations or excluded prefixes in play is
  read conservatively (no exclusions applied).

The optimizer spends a queue entry like a shared pool: each replica of a bound
variant debits it, so two namespaces on CQ-T see one 8-GPU headroom between
them. Within a pass, headroom on a shared queue goes by model score, reusing
the ordering `SharedPools` uses across models. Namespace figures
(`NamespacePools`) are still built, as each namespace's share, for reporting
and for the namespace ResourceQuota bound, which stays per namespace.

Unbound variants keep a per-namespace min, but taken after bound spending:
the min, over the namespace's GPU queues, of what each queue still has once
its bound variants' usage and this pass's bound orders are debited. Today's
min computed as now would over-order next to bound variants: with LocalQueues
on CQ-S (16) and CQ-B (8), a bound variant spends 16 on CQ-S and an unbound
one still sees 8, so 24 are ordered, and if the unbound replica lands on
CQ-S it finds nothing left. Debited, the unbound variant sees min(0, 8) = 0. With that, the fallback never
over-orders, and unlike holding unbound variants at zero it does not block
scale from zero for installs that rely on a namespace default. A test covers
the CQ-S/CQ-B case.

### P/D roles and the share

Roles of one model bound to different queues are sized against their own
queues. The utilization share keeps its transfers per queue: a transfer from
a donor on CQ-D to a receiver on CQ-P is allowed only up to the receiver's
headroom on CQ-P (within nominal, or borrowable from the cohort), and its
promise is held on CQ-P. GPUs a donor frees on CQ-D return to CQ-D.

### A shared queue

When a variant gets less than its queue's free figure because other variants
bound to the same queue took it in this pass, it reports `queue-spent`. The
reason names the queue, never the other namespace or variant: the tenant on
the other side is not this namespace's business.

### Two controllers on one queue

Not solved here. Detected, where LocalQueues can be listed cluster-wide (a
cluster-scoped install does so already; a reader restricted to one namespace
lists only that one, tries a cluster-wide list, and reports nothing on
Forbidden): a GPU ClusterQueue that a bound variant uses and that also has LocalQueues
in namespaces this controller does not manage is reported with reason
`queue-shared-unmanaged` and a log line naming the queue (not the other
namespaces). It is
informational: the controller cannot tell a second controller from a training
job, so it changes nothing and the charge still applies.

### Removed for bound variants

- `namespaceCaps` and `minCaps` (kept for unbound variants until stage 4).
- `ambiguousType` and `ReasonMultiQueueNamespace`.
- `namespaceCharge` and the namespace fold in `chargeNonManaged`.
- The per-namespace co-resource check on the nominal path.

### Reasons and metrics

- `queue-unbound`, `queue-label-mismatch`, `queue-not-found`,
  `queue-not-admitting`, `queue-spent` and `queue-shared-unmanaged` are
  scaling-blocked reasons: on `llmsm_model_scaling_blocked{reason}` for the
  model, and per variant. They are the second variant-scoped condition, the
  point at which `constants/metrics.go` says `llmsm_variant_at_max_replicas`
  becomes `llmsm_variant_scaling_blocked{variant_name, reason}`; they go there,
  not on `llmsm_quota_borrow_withheld`.
- `multi-queue-namespace` stays on `llmsm_quota_borrow_withheld` for unbound
  variants until stage 4, then is accepted for one release, never emitted.
- New series `llmsm_kueue_queue_limit_gpus{cluster_queue, accelerator, tier}`.
  No `cluster_queue` label on `llmsm_quota_limit_gpus`: on a shared queue the
  per-namespace series would then double-count. The namespace series becomes
  the namespace's share.
- `llmsm_kueue_shadow_mismatch_total{field}` while shadowed (stage 2).
- The controller status lists each variant's binding and reason.

### RBAC

The binding needs nothing new: LocalQueues, ClusterQueues, pods, Workloads
and the `kueue-controller-manager` Deployment are read already. Reading Kueue's
Configuration ConfigMap (`manageJobsWithoutQueueName`, transformations,
excluded prefixes) needs a new `get` on one ConfigMap by name in Kueue's
namespace. The two-controller detection needs `list` on LocalQueues
cluster-wide, which a namespace-scoped install may not have. Both are
optional: Forbidden means unknown, never "not set", and the conservative path
is taken.

## Alternatives considered

- **Keep the targeted fixes.** #186 never over-orders on a multi-queue
  namespace, but under-spends (scenarios 1, 2) and leaves a shared queue a
  ceiling (3, 4). Fine where every tenant has its own ClusterQueue. It stays
  the fallback for unbound variants.
- **Hold an unbound variant at zero Kueue growth.** Safer against a wrong
  guess, but blocks scale from zero for installs that never label. One
  reviewer preferred it; see [Decisions](#decisions).
- **Partition a shared queue by namespace weight.** Invents a split Kueue does
  not make.
- **Templates only.** Unreliable: the label lives on the object and the
  webhook copies it, and running pods are the truth once there are any.

## Staged rollout

1. **Read and report.** Resolve bindings; publish the reasons, the new queue
   series and the status list. No decision changes. Shows operators which
   variants would be unbound before anything bites.
2. **Per-queue figures and usage, shadowed.** Compute `QueueBudget`, with
   managed usage and gated GPUs keyed per queue, beside today's figures; a
   difference increments `llmsm_kueue_shadow_mismatch_total`.
3. **Spend per queue.** The optimizer, the wake and the share read queue
   entries for bound variants. Gated by the quota-entry field
   `kueue.perQueue: true`, off in the release that adds it, on in the next.
4. **Remove the per-namespace path** for everything but the unbound fallback,
   and the opt-out. Lands with the claims ledger's package move.

## Test strategy

- Binding tables: label on pods, on Workloads, on the object only, on the
  template only, object and template disagreeing; no label with and without a
  `default` LocalQueue and the gate on or unknown; LocalQueue missing,
  stopped, inactive; ClusterQueue without the GPU resource, without a
  co-resource, `namespaceSelector` excluding; LeaderWorkerSet with and
  without a leader template; a rollout with pods on two queues.
- One test per motivation scenario, in 8-GPU units, asserting ordered
  replicas: CQ-S 16 + CQ-B 8 serves two; a bound variant spending CQ-S
  leaves an unbound one in the namespace 0; prefill on CQ-P and decode on CQ-D
  each sized on their own queue; a share transfer from CQ-D to CQ-P bounded by
  CQ-P; shared CQ-T orders one between A and B, and the second reports
  `queue-spent` without naming A; 64-CPU queue fits one 48-CPU replica.
- Co-resources: init container larger than the containers, pod overhead.
- The #186 regression tests stay, rewritten only where they assert the
  per-namespace min for a variant that is now bound.
- The Kueue kind e2e suite gains a shared-queue case with two namespaces and
  a P/D case with two queues.

## Risks

- **Templates without labels.** Installs that rely on
  `manageJobsWithoutQueueName` stay on the fallback, as today. Stage 1
  measures how many.
- **A wrong binding over-orders.** Pods and Workloads above zero, and object
  and template agreeing at zero, make that unlikely; the opt-out takes it back.
- **Ordering on a shared queue.** By model score; checked by the shared-queue
  test, not assumed.

## Decisions

1. **Unbound variant on several GPU queues?** Keep today's per-namespace min
   as the fallback, with the informational reason `queue-unbound`. *Dissent:*
   the whole-node reviewer preferred holding it at zero Kueue growth; that
   blocks scale from zero for unlabelled installs.
2. **`LocalQueueDefaulting`?** Not inferred. Above zero, use the queue on
   pods and Workloads. At zero, read the gate from the
   `kueue-controller-manager` Deployment's arguments; `manageJobs*` needs the
   optional ConfigMap grant.
3. **New series or a label?** A new series,
   `llmsm_kueue_queue_limit_gpus{cluster_queue, accelerator, tier}`; the
   namespace series becomes the share.
4. **Shared queue order?** By model score, reusing `SharedPools` ordering.
5. **Before or after the claims ledger?** Stages 1 to 3 before the ledger's
   stages 2 to 4, with per-queue usage keying done here, since it does not
   need the ledger. *Dissent:* the Kueue maintainer would land stages 2 and 3
   after the ledger, so claims are keyed per queue first; stage 1 first and
   alone either way.

## Landing order

Agreed across both proposals (see the [claims
ledger](claims-ledger.md#landing-order)):

1. Stage 1 of both: ledger golden traces, and this read-and-report.
2. Stages 2 and 3 here, with per-queue usage keying.
3. Ledger stages 2 to 4, claims keyed per queue.
4. Kueue seam and package move, and stage 4 here.

## Review

Reviewed by a whole-node P/D and SRE reviewer, an architect and a Kueue
maintainer. This revision: drops "one GPU queue means bound" (an unlabelled
pod is not admitted through it); keeps the reader's GPU-resource and
`namespaceSelector` checks and adds the LocalQueue's `stopPolicy` and
`Active`; reads the object label first and pods and Workloads above zero;
keys usage and gated GPUs per queue; adds P/D on two queues and the share's
cross-queue transfers; adds `queue-spent`, a plain statement that two
controllers on one queue stay unsolved with detection, scaling-blocked
reasons, a new queue series, the ConfigMap RBAC note, and co-resources as
Kueue charges them.

## Related

- [Kueue borrowing, and limiters that compose](kueue-borrowing-and-limiter-composition.md)
- [One claims ledger](claims-ledger.md)
- [Quota limiter reference](../reference/quota-limiter.md)
- [Kueue-bounded quotas well-lit path](../well-lit-paths/kueue-bounded-quotas/)
- [Kueue borrowing (developer guide)](../developer-guide/kueue-borrowing.md)
