# Let models borrow from a Kueue cohort

## Overview

With a Kueue-bounded quota, a model may grow past its ClusterQueue's own quota
into GPUs its cohort lends, and Kueue takes them back when the lender needs
them. This guide checks the prerequisites, turns borrowing on, pilots it on a
few namespaces, reads why it is withheld, and turns it off.

Borrowing is **experimental, but on by default** for every namespace-scope
quota entry with `kueue.enabled`. Where a condition does not hold, the
namespace is capped at its nominal quota and a reason says why.

Whether to take it at all (what it buys, what it costs, when not to):
[the Kueue path](../../well-lit-paths/kueue-bounded-quotas/).

### Terms

| Term | Meaning |
| --- | --- |
| **nominal** | a ClusterQueue's `nominalQuota` |
| **guaranteed** | the part of nominal the queue can count on now. It is per ClusterQueue: namespaces that share one queue share it |
| **lender** | a ClusterQueue in the cohort whose unused nominal others borrow |
| **`reclaimWithinCohort`** | whether a lender takes lent capacity back by preemption: `Any`, `LowerPriority` or `Never` |
| **borrow cap** | how far past guaranteed a namespace may go: nominal plus `borrowingLimit`, bounded by the static entry and the namespace's ResourceQuota |
| **flavor** | a Kueue ResourceFlavor: which nodes a quota is on |

## Prerequisites

- [ ] The scaling manager installed ([in a namespace](../install-in-namespace/)),
  every variant's accelerator resolving
  ([check](../admin-gpu-bounding/README.md#prerequisites)), Kueue's
  `ClusterQueue`, `LocalQueue`, `ResourceFlavor`, `Cohort` and `Workload`
  kinds served, and a LocalQueue in each managed namespace. `jq` and `yq`
  (mikefarah, v4) on your path.
- [ ] **Kueue runs a tested version**, and each managed ClusterQueue is in a
  cohort. Lenders that must get capacity back from serving use
  `reclaimWithinCohort: Any`, and no queue in the cohort sets
  `borrowWithinCohort` (or sets it to `Never`):

<!-- guide:prerequisites.kueue start -->
```bash
# Kueue runs and its image carries a version tag; then each ClusterQueue's cohort and preemption.
kubectl get deploy kueue-controller-manager -n <kueue-ns> \
  -o jsonpath='{.status.readyReplicas} ready, image {.spec.template.spec.containers[*].image}{"\n"}'
kubectl get clusterqueue -o custom-columns='NAME:.metadata.name,COHORT:.spec.cohortName,RECLAIM:.spec.preemption.reclaimWithinCohort,BORROW_WITHIN:.spec.preemption.borrowWithinCohort'
```
<!-- guide:prerequisites.kueue end -->

  `<kueue-ns>` is where Kueue runs, `kueue-system` by default. On Kueue's
  `v1beta1` API the cohort field is `spec.cohort`, not `spec.cohortName`.
  Kueue v0.19 and v0.20 are accepted; CI runs v0.20.0
  ([the table](../../reference/quota-limiter.md#supported-kueue-versions)).
  Kueue elsewhere than `kueue-system`: set `kueue.controllerNamespace` on the
  entry, and pass `KUEUE_NAMESPACE` to the admin phase below.

- [ ] **The controller can read what borrowing reads.** Without the Role in
  the Kueue namespace, borrowing stops with `kueue-controller-down`; without
  the cluster-wide Workload read, with `lenders-unreadable`:

<!-- guide:prerequisites.rbac start -->
```bash
# Check what the controller may read. Every line must print yes.
SA=system:serviceaccount:<scaling-manager-namespace>:scaling-manager-controller-manager
kubectl auth can-i list workloads.kueue.x-k8s.io --all-namespaces --as="$SA"
kubectl auth can-i list cohorts.kueue.x-k8s.io --as="$SA"
kubectl auth can-i get deployments/kueue-controller-manager -n <kueue-ns> --as="$SA"
kubectl auth can-i get leases.coordination.k8s.io/c1f6bfd2.kueue.x-k8s.io -n <kueue-ns> --as="$SA"
kubectl auth can-i list resourcequotas -n <namespace> --as="$SA"
```
<!-- guide:prerequisites.rbac end -->

  `c1f6bfd2.kueue.x-k8s.io` is Kueue's leader-election Lease. A namespace-scoped
  install usually prints `no` until a cluster admin re-runs the admin phase:

<!-- guide:prerequisites.grant start -->
```bash
# (cluster admin) Any no above. Re-run the admin phase with the limiter the install declares.
KUEUE_NAMESPACE=<kueue-ns> NAMESPACE=<namespace> make setup-prereqs \
  LLMSM_LIMITER=gpu-inventory,quota LLMSM_QUOTAS='<accelerator>=-1' LLMSM_QUOTA_KUEUE=true
```
<!-- guide:prerequisites.grant end -->

  It grants two things: the Role in the Kueue namespace, and the cluster-wide
  Workload read, the latter only because that limiter counts borrowing.
  `KUEUE_NAMESPACE` puts the Role where Kueue runs; unset, the entry's
  `kueue.controllerNamespace` decides.

  If the controller runs in a namespace of its own, apart from the models,
  add `LLMSM_NS=llmsm-<namespace> LLMSM_WATCH_NS=<namespace>`. The Workload read
  lets the controller read every tenant's Kueue Workloads, pod templates
  included ([security model](../../reference/security-model.md#kueue-borrowing)),
  so it is granted only while the install's limiter counts borrowing. Re-run
  with `LLMSM_QUOTA_KUEUE_BORROW=false`, `make disable-limiter
  LLMSM_LIMITER_TYPE=quota`, or an uninstall, and it is revoked. Where the read
  is not acceptable, borrowing cannot be counted safely: set
  `countBorrowable: false`.

  A cluster-scoped install has every read above in its manager ClusterRole;
  after an upgrade, re-apply its manifests:

<!-- guide:prerequisites.grantcluster start -->
```bash
# (cluster admin) A cluster-scoped install instead: re-apply its manifests. Its manager ClusterRole carries every read above.
make setup-prereqs SCOPE=cluster LLMSM_NS=<scaling-manager-namespace>
```
<!-- guide:prerequisites.grantcluster end -->

- [ ] **A GPU bound.** Something must check that borrowed GPUs exist: a
  `gpu-inventory` limiter (it needs the node read `make setup-prereqs`
  grants), or a GPU `ResourceQuota` in the namespace whose `hard` is **at most
  nominal plus `borrowingLimit`**. The installer adds neither. Without one,
  borrowing is withheld as `no-gpu-bound`:

<!-- guide:prerequisites.bound start -->
```bash
# Borrowing needs a GPU bound. Print the queue's nominal and borrowingLimit per flavor, then the namespace's ResourceQuotas: a GPU ResourceQuota must be at most nominal plus borrowingLimit.
kubectl get clusterqueue <cluster-queue> \
  -o jsonpath='{range .spec.resourceGroups[*].flavors[*]}{.name}{":"}{range .resources[*]}{" "}{.name}{" nominal="}{.nominalQuota}{" borrowingLimit="}{.borrowingLimit}{end}{"\n"}{end}'
kubectl get resourcequota -n <namespace> -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.hard}{"\n"}{end}'
```
<!-- guide:prerequisites.bound end -->

  A queue with `nominalQuota: 8` and `borrowingLimit: 8` takes a quota of at
  most 16:

  ```yaml
  apiVersion: v1
  kind: ResourceQuota
  metadata:
    name: gpu
    namespace: <namespace>
  spec:
    hard:
      requests.nvidia.com/gpu: "16"
  ```

- [ ] **Kueue settings borrowing does not support are off**: Fair Sharing, and
  `waitForPodsReady` with `blockAdmission: true`. Fair Sharing is detected and
  withholds borrowing; `blockAdmission` is not detected. `waitForPodsReady` is
  a risk even without it: Kueue evicts a Workload whose pods are not Ready
  within its `timeout` (5 minutes by default), so a model that loads for
  longer is evicted on every start. Raise the timeout, or leave it off:

<!-- guide:prerequisites.kueueconfig start -->
```bash
# Kueue settings borrowing does not support. Expect no fairSharing block, and no waitForPodsReady; if waitForPodsReady is there, its timeout must exceed your slowest replica start.
kubectl get configmap kueue-manager-config -n <kueue-ns> \
  -o jsonpath='{.data.controller_manager_config\.yaml}' | grep -A4 -E 'fairSharing|waitForPodsReady'
```
<!-- guide:prerequisites.kueueconfig end -->

- [ ] **Every GPU pod in the namespace is Kueue-managed**, and tenants cannot
  write Kueue Workloads there. One unmanaged GPU pod withholds borrowing
  (`not-kueue-managed`):

<!-- guide:prerequisites.managed start -->
```bash
# Every GPU pod in the namespace is Kueue-managed: one Workload per replica (or replica group), with quota reserved. Tenants must not write Workloads: expect no.
kubectl get workloads.kueue.x-k8s.io -n <namespace> \
  -o custom-columns='NAME:.metadata.name,OWNER:.metadata.ownerReferences[0].kind,RESERVED:.status.conditions[?(@.type=="QuotaReserved")].status'
kubectl auth can-i create workloads.kueue.x-k8s.io -n <namespace> --as=<tenant>
```
<!-- guide:prerequisites.managed end -->

- [ ] **Serving pods survive a preemption cleanly**: a drain hook (Kueue
  deletes a preempted pod without the Eviction API, so a PodDisruptionBudget
  does not apply), a `terminationGracePeriodSeconds` of at least one engine
  cycle, and a `WorkloadPriorityClass` above batch for models that must keep
  replicas:

<!-- guide:prerequisites.drain start -->
```bash
# Serving pods drain on a preemption. The first command writes the drain patch and shows it; the second applies it, restarting the model servers.
make workload-patch NAMESPACE=<namespace>
make workload-patch NAMESPACE=<namespace> LLMSM_WORKLOAD_PATCH_APPLY=true
```
<!-- guide:prerequisites.drain end -->

- [ ] **Each variant's pod template sets requests for every resource its GPU
  flavor's resource group covers** (CPU, memory), and **pins the flavor's
  `nodeLabels`** when more than one flavor of the queue offers the GPU.
  Otherwise borrowing is withheld (`co-resource-bound`, `flavor-unpinned`).
- [ ] **One scaling manager per cohort**, or `countBorrowable: false` on the
  Kueue entries of all but one. Two controllers that borrow from one cohort
  can count the same free GPUs.

Two things Kueue does that the scaling manager does not change:

- **P/D pairs are not admitted atomically.** A P/D model deployed as two
  workloads is two Kueue Workloads, and Kueue may admit or preempt one role
  without the other. The scaling manager borrows a P/D pair only whole, both
  roles on borrowed capacity, and gives back a half left unplaced after the
  placement timeout.
- **Kueue picks which pod to preempt**, by `WorkloadPriorityClass`.

## Installation Instructions

<!-- guide:deploy.enable start -->
```bash
# Namespace owner. A GPU bound plus a Kueue-backed quota; -1 lets Kueue set the figure.
make deploy-scaling-manager NAMESPACE=<namespace> \
  LLMSM_LIMITER=gpu-inventory,quota LLMSM_QUOTAS='<accelerator>=-1' LLMSM_QUOTA_KUEUE=true
```
<!-- guide:deploy.enable end -->

<!-- guide:deploy.cluster start -->
```bash
# Or, cluster admin: the same quota in the cluster policy every controller reads. A gpu-inventory entry already published there is kept.
make enable-physical-limiter LLMSM_LIMITER_TYPE=quota LLMSM_QUOTAS='<accelerator>=-1' LLMSM_QUOTA_KUEUE=true
```
<!-- guide:deploy.cluster end -->

Borrowing is counted by default. With a GPU `ResourceQuota` in the namespace
as the bound instead of `gpu-inventory`, use `LLMSM_LIMITER=quota`.

`<accelerator>` is the name the controller logs for each model. The entry it
writes, on a namespace-scoped install (a cluster-scoped install, and the
cluster policy, key it on the reserved `default` instead of `<namespace>`, so
every namespace gets the same Kueue-bounded cap):

```yaml
limiters:
  - type: gpu-inventory
  - name: install-quota
    type: quota
    scope: namespace
    kueue:
      enabled: true               # countBorrowable defaults to true
    namespaceQuotas:
      <namespace>:
        <accelerator>: -1         # the TYPE is yours; the NUMBER is Kueue's
```

## Verification

<!-- guide:verify.log start -->
```bash
# The effective caps, then the borrowing figures. Both are logged when they change.
kubectl logs -n <scaling-manager-namespace> deploy/scaling-manager-controller-manager --tail=-1 \
  | grep -E 'quota entry bounded by external caps|Kueue borrowing applied|borrowing frozen at the last figures|external quota source' | tail -5
```
<!-- guide:verify.log end -->

<!-- guide:verify.budgets start -->
```bash
# Per namespace and accelerator: guaranteed, borrowable, used, and the reason borrowable is 0. No reason and borrowable above 0: borrowing is counted.
kubectl get configmap scaling-manager-controller-status -n <scaling-manager-namespace> -o jsonpath='{.data.limiterBudgets}' \
  | jq -c '.entries[] | {namespace, accelerator, guaranteed, borrowable, used, reason}'
```
<!-- guide:verify.budgets end -->

The same in Prometheus, where the model's namespace is the
`exported_namespace` label:

```promql
llmsm_quota_limit_gpus{tier="borrowable"} > 0     # may borrow now
llmsm_quota_borrow_withheld == 1                  # why not, per namespace
llmsm_kueue_borrowed_gpus                         # what this controller holds borrowed, per cohort and flavor
```

A model on borrowed capacity shows `used` above `guaranteed` with no Pending
pod (`kubectl get pods -n <namespace> --field-selector status.phase=Pending`).

## Pilot on a few namespaces

To count borrowing in some namespaces only, set `borrowNamespaces` on the
Kueue-backed quota entry. Every other namespace is capped at its nominal
quota, withheld as `not-selected`:

```yaml
    kueue:
      enabled: true
      borrowNamespaces: [team-a, team-b]   # empty or absent: every governed namespace
```

<!-- guide:pilot.set start -->
```bash
# Count borrowing only in the namespaces listed. Find the policy ConfigMap the controller reads, then set borrowNamespaces on the Kueue-backed quota entry.
STATUS=$(kubectl get cm scaling-manager-controller-status -n <scaling-manager-namespace> -o json)
POLICY_NS=$(printf '%s' "$STATUS" | jq -r '.data.policyNamespace // empty')
POLICY_NS=${POLICY_NS:-<scaling-manager-namespace>}
POLICY_CM=$(printf '%s' "$STATUS" | jq -r '.data.policyConfigMap // empty')
POLICY_CM=${POLICY_CM:-scaling-manager-scaling-policy-config}
kubectl get configmap "$POLICY_CM" -n "$POLICY_NS" -o jsonpath='{.data.default}' \
  | yq '(.limiters[] | select(.type == "quota" and .kueue.enabled == true) | .kueue.borrowNamespaces) = ["<namespace>"]' \
  > policy-default.yaml
kubectl patch configmap "$POLICY_CM" -n "$POLICY_NS" --type=merge \
  -p "$(jq -n --rawfile d policy-default.yaml '{data: {default: $d}}')"
```
<!-- guide:pilot.set end -->

A controller that watches the policy's namespace applies it on its next cycle.
A namespace-scoped controller whose policy lives in another namespace
(`scaling-manager-policy`, or the controller's own namespace when it runs apart from the
models) reads it at startup: restart it with
`kubectl rollout restart deploy/scaling-manager-controller-manager -n <scaling-manager-namespace>`.

<!-- guide:pilot.check start -->
```bash
# Every namespace left out now reads not-selected.
kubectl get configmap scaling-manager-controller-status -n <scaling-manager-namespace> -o jsonpath='{.data.limiterBudgets}' \
  | jq -c '.entries[] | select(.reason == "not-selected") | {namespace, accelerator}'
```
<!-- guide:pilot.check end -->

**The list survives a re-install.** Re-running `make deploy-scaling-manager`, with or
without `LLMSM_LIMITER`, or `make enable-physical-limiter LLMSM_LIMITER_TYPE=quota`,
keeps the entry's whole `kueue:` block: `borrowNamespaces` and every other
setting you wrote there. Only what the variables set is rewritten: `enabled`,
and `countBorrowable` when `LLMSM_QUOTA_KUEUE_BORROW` is given.

Widen the pilot by adding namespaces; remove the key to borrow everywhere.

## Read why borrowing is withheld

<!-- guide:reasons.read start -->
```bash
# Every namespace and accelerator whose borrowing is withheld, frozen, or counted at 0, with its reason.
kubectl get configmap scaling-manager-controller-status -n <scaling-manager-namespace> -o jsonpath='{.data.limiterBudgets}' \
  | jq -c '.entries[] | select(.reason) | {namespace, accelerator, reason}'
```
<!-- guide:reasons.read end -->

Each namespace shows one reason, in three groups:

- **Withheld**: a condition does not hold; borrowable is 0 and the namespace
  is capped at its nominal quota. For example `no-gpu-bound`,
  `not-kueue-managed`, `lenders-unreadable`.
- **Frozen**: Kueue cannot be trusted right now (`stale`,
  `kueue-controller-down`); nothing new is borrowed, and nothing borrowed is
  taken away.
- **Counted, but 0 this cycle**: borrowing is on, there is nothing to draw now
  (`lender-waiting`, `cohort-pool-empty`, ...).

What each reason means and what to do:
[Borrowing is withheld](../../reference/troubleshooting.md#borrowing-is-withheld).

## Optional: give capacity back to lenders Kueue does not reclaim for

By default a lender on `reclaimWithinCohort: Never`, or a `LowerPriority`
lender below your model, lends nothing that counts (`lender-cannot-reclaim`):
Kueue would never take it back for that lender.

`releaseToNonReclaimingLenders: true` counts it, and makes the scaling
manager give it back when the lender waits: it lowers the borrowing models,
lowest priority first, never below `minReplicas` or the namespace's
guaranteed figure. It is **experimental and off by default**: the scaling
manager becomes the lender's reclaim agent. Turn it on only where the lender's
owners agree that serving may borrow their quota.

The setting is on the quota entry: it applies to every namespace the entry
governs in a cohort, and one tenant cannot opt out alone. Run one scaling
manager per cohort with it on: two would hand a lender the same GPUs twice.

<!-- guide:lenders.set start -->
```bash
# Opt in (experimental): count what lenders Kueue does not reclaim for lend, and give it back when they wait. Same policy ConfigMap as the pilot; entries with countBorrowable false are left alone (validation refuses the key there).
STATUS=$(kubectl get cm scaling-manager-controller-status -n <scaling-manager-namespace> -o json)
POLICY_NS=$(printf '%s' "$STATUS" | jq -r '.data.policyNamespace // empty')
POLICY_NS=${POLICY_NS:-<scaling-manager-namespace>}
POLICY_CM=$(printf '%s' "$STATUS" | jq -r '.data.policyConfigMap // empty')
POLICY_CM=${POLICY_CM:-scaling-manager-scaling-policy-config}
kubectl get configmap "$POLICY_CM" -n "$POLICY_NS" -o jsonpath='{.data.default}' \
  | yq '(.limiters[] | select(.type == "quota" and .kueue.enabled == true and .kueue.countBorrowable != false) | .kueue.releaseToNonReclaimingLenders) = true' \
  > policy-default.yaml
kubectl patch configmap "$POLICY_CM" -n "$POLICY_NS" --type=merge \
  -p "$(jq -n --rawfile d policy-default.yaml '{data: {default: $d}}')"
```
<!-- guide:lenders.set end -->

Applied like the pilot: on the next cycle, or after a restart for a
namespace-scoped controller whose policy lives elsewhere. A re-install keeps
the setting.

<!-- guide:lenders.check start -->
```bash
# No namespace reads lender-cannot-reclaim any more; each release to a waiting lender is logged, and recorded on the model's scale target.
kubectl get configmap scaling-manager-controller-status -n <scaling-manager-namespace> -o jsonpath='{.data.limiterBudgets}' \
  | jq -c '.entries[] | select(.reason == "lender-cannot-reclaim") | {namespace, accelerator}'
kubectl logs -n <scaling-manager-namespace> deploy/scaling-manager-controller-manager --tail=-1 \
  | grep 'Releasing borrowed capacity' | grep 'lender-waiting' | tail -5
kubectl get events -n <namespace> --field-selector reason=BorrowedCapacityReleased
kubectl get events -n <namespace> --field-selector reason=LenderReleaseBlocked
```
<!-- guide:lenders.check end -->

A release lands only after the model's KEDA scale-down window; the
lender-waiting alert waits that much longer in that cohort. The model shows
`released-to-lender` while it lands, and borrows nothing new in that cohort
for `borrowCooldown` after. The scaling manager hands back only what admits
the lender's next Workload; when it cannot, it hands back nothing and records
a `LenderReleaseBlocked` Event. How it chooses what to give back:
[the quota limiter](../../reference/quota-limiter.md#lenders-kueue-does-not-reclaim-for).

To stop, or before rolling the controller back to a release without the
setting: what a model borrowed from a lender Kueue never reclaims for stays
borrowed once the setting is off, the lender waits, and the lender-waiting
alert pages at the cooldown. A release in flight keeps its targets for up to
twice its latency. So first lower each borrowing model's `maxReplicaCount` to
what its guaranteed figure holds, then remove the key, then raise
`maxReplicaCount` again. Nothing is evicted by the change itself.

<!-- guide:lenders.unset start -->
```bash
# Turn it off. First cap each borrowing model at what its guaranteed figure holds: what it borrowed from a lender Kueue never reclaims for stays borrowed once the setting is off. Raise maxReplicaCount again once it is off.
kubectl patch scaledobject <scaledobject> -n <namespace> --type=merge \
  -p '{"spec":{"maxReplicaCount":<guaranteed-replicas>}}'
STATUS=$(kubectl get cm scaling-manager-controller-status -n <scaling-manager-namespace> -o json)
POLICY_NS=$(printf '%s' "$STATUS" | jq -r '.data.policyNamespace // empty')
POLICY_NS=${POLICY_NS:-<scaling-manager-namespace>}
POLICY_CM=$(printf '%s' "$STATUS" | jq -r '.data.policyConfigMap // empty')
POLICY_CM=${POLICY_CM:-scaling-manager-scaling-policy-config}
kubectl get configmap "$POLICY_CM" -n "$POLICY_NS" -o jsonpath='{.data.default}' \
  | yq 'del(.limiters[].kueue.releaseToNonReclaimingLenders)' \
  > policy-default.yaml
kubectl patch configmap "$POLICY_CM" -n "$POLICY_NS" --type=merge \
  -p "$(jq -n --rawfile d policy-default.yaml '{data: {default: $d}}')"
```
<!-- guide:lenders.unset end -->

## Cleanup

<!-- guide:cleanup.stop start -->
```bash
# Stop counting borrowing; the Kueue cap stays, at nominal. Nothing is evicted. The rest of the entry's kueue block, borrowNamespaces included, is kept.
make deploy-scaling-manager NAMESPACE=<namespace> \
  LLMSM_LIMITER=gpu-inventory,quota LLMSM_QUOTAS='<accelerator>=-1' LLMSM_QUOTA_KUEUE=true LLMSM_QUOTA_KUEUE_BORROW=false
# the cluster policy instead:
# make enable-physical-limiter LLMSM_LIMITER_TYPE=quota LLMSM_QUOTAS='<accelerator>=-1' LLMSM_QUOTA_KUEUE=true LLMSM_QUOTA_KUEUE_BORROW=false
```
<!-- guide:cleanup.stop end -->

By hand, set `countBorrowable: false` under the entry's `kueue:` block. A
lowered cap only stops growth: models on borrowed capacity keep it until they
scale down, or until Kueue preempts them for a lender. The status lists at
most 200 entries; while `truncated` is above 0, some are not shown, so check
those namespaces by hand:

<!-- guide:cleanup.drain start -->
```bash
# Models move back under the cap only as they scale down. Lists the ones still above it, then how many entries the list left out (truncated); done when both are empty and 0.
BUDGETS=$(kubectl get configmap scaling-manager-controller-status -n <scaling-manager-namespace> -o jsonpath='{.data.limiterBudgets}')
printf '%s' "$BUDGETS" | jq -c '.entries[] | select(.guaranteed >= 0 and .used > .guaranteed) | {namespace, accelerator, guaranteed, used}'
printf '%s' "$BUDGETS" | jq '.truncated // 0'
```
<!-- guide:cleanup.drain end -->

To hand GPUs back sooner, lower the model's `maxReplicaCount` on its
ScaledObject. To roll back to a release without borrowing, turn it off first
and wait for this list to empty, then follow
[upgrade and uninstall](../../reference/upgrade-and-uninstall.md#limiters-and-kueue-borrowing).

To drop the Kueue cap too, **replace** the entry rather than only dropping
`LLMSM_QUOTA_KUEUE`: a type left at `-1` without Kueue has no cap at all. Re-run
`make deploy-scaling-manager` with `LLMSM_LIMITER=quota` and a finite `LLMSM_QUOTAS`, or with
`LLMSM_LIMITER=none`. For the cluster policy, re-run
`make enable-physical-limiter LLMSM_LIMITER_TYPE=quota` with a finite
`LLMSM_QUOTAS`, or remove the quota with
`make disable-limiter LLMSM_LIMITER_TYPE=quota`.

## Configuration

Every setting on the entry's `kueue:` block (`placementTimeout`,
`borrowCooldown`, `lenderDebounce`, `controllerNamespace`,
`assumeTestedVersion`, `borrowNamespaces`, `releaseToNonReclaimingLenders`):
[cohort borrowing](../../reference/quota-limiter.md#cohort-borrowing). The
signals and alerts: [Prometheus](../../reference/prometheus.md#kueue-borrowing-metrics).
How it is built and tested: [Kueue borrowing internals](../../developer-guide/kueue-borrowing.md).
