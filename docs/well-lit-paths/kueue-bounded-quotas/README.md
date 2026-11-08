# Bound tenants by the quotas Kueue already holds

If Kueue already decides your GPU quotas, the scaling manager can read them
instead of keeping a second copy. It caps each namespace and accelerator type
at **the smaller of Kueue's grant and its own static cap**, so it rarely asks
KEDA for a replica Kueue would hold pending. Kueue's grant is first charged
with what other work Kueue admitted on the same ClusterQueue holds (a
cluster-scope entry is the exception; see
[When not to take it](#when-not-to-take-it)). Where it is safe, the grant
includes what the namespace's ClusterQueue can **borrow** from its cohort, only
from lenders Kueue would reclaim for, and Kueue takes borrowed replicas back
when a lender needs them.

**Maturity:** **Experimental.** Borrowing is experimental but on by default:
`countBorrowable: false` on the entry turns it off and keeps the Kueue cap.
What backs it: [Evidence](#evidence).

## What it buys

- One source of truth for tenant GPU numbers: change the ClusterQueue, and the
  scaling manager follows within one `refreshInterval` (30s) plus one
  [optimize cycle](../../reference/cycle-log.md).
- Few replicas ordered that Kueue would hold at its admission gate, so few
  Pending pods a scale-up counted on; the cases that remain are listed below.
- With borrowing: a model grows into GPUs its cohort leaves idle (batch quota
  between jobs, say), and gives them back when Kueue reclaims them.

## What it costs

- **API load.** ClusterQueues, LocalQueues, ResourceFlavors and Cohorts are
  listed once per `refreshInterval`. Workloads come from a cluster-wide
  informer (one watch, every Workload held in memory). ResourceQuotas in
  managed namespaces are cached.
- **A lag**: a change in Kueue takes effect after at most `refreshInterval`
  plus one cycle.
- **With borrowing: lost replicas.** Kueue preempts borrowed replicas when a
  lender asks for its quota, and the scaling manager gives back a borrowed
  replica that did not place within `placementTimeout` (5m). Each loss is a
  cold start.
- **Prerequisites to keep true**: a GPU bound, Kueue-managed GPU pods, and,
  for a namespace-scoped install, a Role in the Kueue namespace and a read of
  every namespace's Kueue Workloads
  ([the guide's prerequisites](../../guides/kueue-borrowing/#prerequisites)).
- **A wider read**: borrowing reads every tenant's Kueue Workloads, pod
  templates included ([security model](../../reference/security-model.md#kueue-borrowing)).

Failures are visible. A failed read freezes borrowing at the last figures
(`llmsm_quota_borrow_withheld{reason="stale"}`, alert `LLMSMKueueReaderStale`, or
`LLMSMKueueReaderStaleNamespace` when only one namespace's read fails).
An entry never read shows as `llmsm_kueue_never_read` (alert `LLMSMKueueNeverRead`):
its finite caps still bound, and a type at `-1` is bounded by the other
limiters.

## When not to take it

- **A cluster-scope Kueue entry over queues that also run other GPU work**
  (training, batch), and no `gpu-inventory` limiter is declared. A
  cluster-scope figure is charged only with the scaling manager's own models,
  so it orders replicas Kueue then holds at its gate. Use namespace-scope
  entries, which are charged with the other work, or add `gpu-inventory`
  ([details](../../reference/quota-limiter.md#what-a-quota-is-charged)).
- **Several namespaces share one ClusterQueue and the shared total must
  hold.** Each namespace gets the queue's grant less what the others hold,
  but all of them see the same free GPUs. Two that grow in one cycle can
  together order past the queue, and Kueue holds the excess at its gate.
  Guaranteed is per ClusterQueue too: a shared queue is a ceiling, not a
  partition. Give each tenant its own ClusterQueue
  ([details](../../reference/quota-limiter.md#what-a-quota-is-charged)).
- **Tenants can create LocalQueues on a queue that admits any namespace**, and
  your static cap is `-1`. They would get that queue's grant
  ([details](../../reference/quota-limiter.md#what-is-read)).
- **The controller must stay bounded when Kueue cannot be read**, with `-1`
  caps only. Until the first read succeeds, an entry that leaves a type at
  `-1` binds nothing on that type, and only the other limiters bound it. Use a finite static
  cap, which Kueue can only tighten.
- **A model must never lose a replica**, and its queue lends to or borrows
  from a cohort. Give it the quota as nominal, or set `countBorrowable: false`.
- **Kueue features borrowing does not support**: hierarchical cohorts, Fair
  Sharing, Topology-Aware Scheduling, admission checks (MultiKueue),
  `borrowWithinCohort` preemption, and `waitForPodsReady` with
  `blockAdmission`. Each but the last withholds borrowing with a reason; the
  last is not detected. `waitForPodsReady` at all is a risk: Kueue evicts a
  model whose pods are not Ready within its timeout (5 minutes by default).
- **Two scaling managers borrow from one cohort.** Run one per cohort, or set
  `countBorrowable: false` on the Kueue entries of all but one.

## Start here

1. Read [capping what each tenant may take](../tenant-gpu-quotas/#start-here);
   everything there still applies.
2. Follow [let models borrow from a Kueue cohort](../../guides/kueue-borrowing/):
   prerequisites, enable, verify, pilot on a few namespaces, read the reasons,
   turn it off. To keep the Kueue cap without borrowing, add
   `LLMSM_QUOTA_KUEUE_BORROW=false` to its enable step.

## Evidence

No benchmark scenario: a bound changes what the optimizer may order, not how
it scales. Two end-to-end suites back it:

- `test/e2e/kueue_quota_test.go` (label `kueue-quota`): the read-and-merge
  chain against Kueue's CRDs, in the full tier.
- `test/e2e/kueue_borrow_test.go` (label `kueue-borrow`), against a live
  Kueue: an unmanaged GPU pod withholds borrowing, a stopped Kueue controller
  stops growth past guaranteed, a `Never` lender's capacity is not counted,
  borrowing with every pod placed, and recovery after Kueue preempts. The live
  e2e runs in CI job `e2e-tests-kueue-borrow`.

Not covered end to end: inactive queues, finalizers and `stopPolicy`
transitions (unit fixtures only), and enforcement past the effective entry,
the same gap as [tenant-gpu-quotas](../tenant-gpu-quotas/). Defaults that may
still move: `refreshInterval` 30s, `placementTimeout` 5m, `borrowCooldown`
10m. The unit suites, by package:
[Kueue borrowing internals](../../developer-guide/kueue-borrowing.md#testing).

## Read next

- Every field, the merge rules, what is read and when borrowing is counted:
  [Kueue as a quota source](../../reference/quota-limiter.md#kueue-as-a-quota-source).
- Why a namespace does not borrow, and what to do:
  [Borrowing is withheld](../../reference/troubleshooting.md#borrowing-is-withheld).
  Replicas dropped:
  [the runbook](../../reference/troubleshooting.md#replicas-dropped-under-kueue-borrowing).
- The signals and alerts: [Prometheus](../../reference/prometheus.md#kueue-borrowing-metrics).
- Upgrading to a release that counts borrowing:
  [upgrade notes](../../reference/upgrade-and-uninstall.md#limiters-and-kueue-borrowing).
- Tenants and Kueue's Workloads: [security model](../../reference/security-model.md#kueue-borrowing).
- Why a quota counts only the scaling manager's own consumption:
  [GPU capacity accounting](../../concepts/gpu-capacity-accounting.md).
