# Cap what each tenant may take

Declare how many GPUs of each accelerator type the scaling manager may hold,
for the whole cluster or per namespace, so one team's autoscaling cannot spend
the fleet. For clusters where several teams share accelerators and each has
been promised a number.

**Maturity:** Stable for the control path. No end-to-end spec yet bounds the
default optimizer by a quota ([How it is tested](#how-it-is-tested)).

## When not to take it

- You need to stop a workload from taking GPUs. A quota bounds **only the
  scaling manager**; it is not an admission rule. A Kubernetes GPU
  `ResourceQuota` stops Pods ([bound at admission](../../guides/admin-cluster-setup/README.md#bound-the-namespaces-gpus-at-admission)).
  Where unmanaged workloads share the accelerators, use both.
- Your question is "is the hardware there?". Take
  [bound a fleet by real GPUs](../bound-by-gpus/).
- The numbers already live in Kueue. Take
  [bound tenants by Kueue's quotas](../kueue-bounded-quotas/) (experimental).

## Start here

You need [the saturation path](../scale-on-saturation/#start-here) in place,
and a cluster admin for a budget the tenant cannot edit
([Who owns what](#who-owns-what)). The cluster-wide steps are in the
[admin guide](../../guides/admin-gpu-bounding/); the one-install steps are here.

1. Namespace owner or admin: [check accelerators resolve](../../guides/admin-gpu-bounding/README.md#prerequisites). `kubectl get events -n <ns> --field-selector reason=AcceleratorNotResolved` (admin: `-A`) should print nothing.
2. Declare the quota, one of:
   - Namespace owner, advisory (you can edit it): `LLMSM_LIMITER=quota LLMSM_QUOTAS='<accelerator>=<n>' make deploy-scaling-manager`.
   - Cluster admin, a budget the tenant cannot edit: `make enable-physical-limiter LLMSM_LIMITER_TYPE=quota LLMSM_QUOTAS='<accelerator>=<n>'` ([guide](../../guides/admin-gpu-bounding/README.md#configuration)).
   - Several namespaces named one by one: hand-write the entry ([Declaring it](#declaring-it)).
3. Check it works: a model held at its allowance raises `llmsm_decisions_limited_total` or a `ResourceConstrained` Event. A fully spent quota shows neither ([Verifying it](#verifying-it)).

The `<accelerator>` key must match the accelerator name the controller logs
exactly. To undo, the admin runs
`make disable-limiter LLMSM_LIMITER_TYPE=quota`
([Cleanup](../../guides/admin-gpu-bounding/README.md#cleanup)); without
`LLMSM_LIMITER_TYPE=quota` it removes only the physical limiter and leaves the
quota in force. For one install, re-run `make deploy-scaling-manager` with
`LLMSM_LIMITER=none`.

## What it costs

- No accelerators.
- Latency when a cap binds: a model held at its allowance queues what another
  replica would have served. That shows up as TTFT (time to first token).
- A fairness gap between capped and uncapped tenants
  ([ceilings, not reservations](#things-that-surprise-people)), and an
  owner for the policy.

## Evidence

No benchmark run. Unit tests cover parsing and accounting; end-to-end specs
cover a refusal, the Kueue merge and the utilization-share transfer, but not
the default optimizer under a static quota ([How it is tested](#how-it-is-tested)).

## Details

### Who owns what

A quota only bounds a tenant if the tenant cannot edit it. Which ConfigMap the
controller reads depends on where the controller runs:

| setting | read from | owned by |
| --- | --- | --- |
| `limiters:` and `optimizer:` (the budget) | the policy namespace, when one resolves for this controller (`scaling-manager-policy`, or the namespace a `scaling-manager.llmd.ai/policy-namespace` label names) | the cluster admin |
| `limiters:` and `optimizer:`, when no policy namespace resolves | the controller's own `scaling-manager-scaling-policy-config` | whoever owns the controller's namespace: the admin for a cluster-scoped or out-of-reach controller, **the tenant** for one running in their own namespace |
| thresholds, tiers, per-model entries, utilization-share weights | a `scaling-manager-scaling-policy-config` in the **managed** namespace, if there is one (read as a namespace-local map, in every shape); otherwise the controller's own ConfigMap | the namespace's owner: in the out-of-reach shape the tenant still sets these, never the budget |

A policy namespace resolves only for a controller that manages its own
namespace **and** can read Namespace objects. On **Kubernetes**,
`make setup-prereqs` grants that read. A controller set up before the grant
existed reads its own ConfigMap (the tenant's) and reports
`llmsm_policy_namespace_unverified` = 1 until an admin re-runs
`make setup-prereqs` ([older installs](../bound-by-gpus/#details-older-installs);
[by hand](../../guides/admin-cluster-setup/README.md#grant-the-namespace-read-by-hand)
only where that cannot run).

`LLMSM_LIMITER=quota` writes the controller's own ConfigMap.
`LLMSM_LIMITER_TYPE=quota` with `make enable-physical-limiter` writes the policy
namespace and the own ConfigMap of every admin-owned controller it grants. It
also restarts each namespace-scoped controller that reads its policy outside
the namespace it manages. Where the budget must hold against the tenant, run
the controller
[out of their reach](../../guides/admin-cluster-setup/README.md#keeping-the-controller-out-of-the-tenants-reach)
and back it with a `ResourceQuota`. A controller out of the tenant's reach
reads only its own ConfigMap, and only at start: restart it after an edit
([why](../../reference/scaling-policy.md#limiters-cluster-default-only-live)).

### Declaring it

Both shapes live in the `limiters:` list on the scaling policy's `default`
entry: in `scaling-manager-policy` for the cluster policy, or in the controller's own
namespace for one install. The installer writes it for you, and
`LLMSM_QUOTA_SCOPE=namespace|cluster` picks the shape. A **namespace-scoped**
install keys the entry on the namespace it manages; a **cluster-scoped** one
on the reserved `default` key (below). `LLMSM_QUOTAS` writes one entry;
hand-write it to name several namespaces. One entry has one scope; to cap
both, declare two entries, and both are consulted.

A later `make deploy-scaling-manager` without `LLMSM_LIMITER` keeps the `limiters:` list, a
hand-written entry included. `LLMSM_LIMITER=quota` replaces it and
`LLMSM_LIMITER=none` removes it
([why](../../reference/scaling-policy.md#re-running-the-installer-resets-the-default-entry)).
`make disable-limiter` also clears the limiter it wrote into
admin-owned controllers' own ConfigMaps. It leaves the read-only grants,
except that `LLMSM_LIMITER_TYPE=quota` or `all` revokes the Kueue controller
read and the cluster-wide Kueue Workload read.

The two shapes, with examples, are in the reference:
[cluster scope](../../reference/quota-limiter.md#cluster-scope) and
[namespace scope](../../reference/quota-limiter.md#namespace-scope).

- **Cluster scope** caps a type across every namespace **one controller
  manages**. N controllers each enforce their own copy, so N of them permit N
  times the figure.
- **Namespace scope** caps each tenant, with an optional `exclude` list and a
  fallback for namespaces you did not list. `default` in `namespaceQuotas` is
  that fallback. So you cannot give the namespace called `default` its own
  quota. Prefer a strict allowlist with no `default` key: every unlisted
  namespace is then denied. If you exclude `default` instead, only the
  cluster bounds limit it.
- A value of `-1` means unlimited.

### Things that surprise people

**A quota does not bound the cluster.** It counts only what the scaling
manager's own variants hold, so it can hand out capacity unmanaged workloads
already took. Charging the physical figure would be worse: a 4-GPU allowance
beside an unrelated 4-GPU training job would read as spent while the scaling
manager had placed nothing. "The hardware is full" is the physical limiter's
statement.

**Ceilings, not reservations.** A finite cap guarantees a tenant never
*exceeds* it, never that it can *reach* it. The quota bounds only the
namespaces it governs: those it lists, and with a `default` key every other
namespace it does not exclude. A namespace that is unlimited (`-1`) or excluded is
bounded only by the cluster bounds (a `gpu-inventory` entry or a cluster-scope
quota; nothing, without one), so it can use GPUs a capped peer would have used.
Nobody exceeds their own authorization, but know this before you promise a team
its number.

**An excluded namespace has no quota.**

- The tenants' quota does not apply to it. A full tenant quota does not stop it.
- Only the cluster bounds limit it: a `gpu-inventory` entry and any
  `scope: cluster` quota. A cluster quota denies it a type the quota does not
  list, as it does everyone. With no cluster bound, the scaling manager does
  not limit it at all. Without a `gpu-inventory` limiter, free GPUs are not
  checked: if the cluster is full, new pods stay Pending.
- It can take GPUs your tenants expect to use, because the scaling manager
  reserves nothing. A Kubernetes `ResourceQuota` or Kueue is the real boundary.
- The utilization-share optimizer shares it only when a finite cluster bound
  covers its GPU type: a `scope: cluster` quota, or a `gpu-inventory` entry
  with `physicalGroups: true` on a cluster-scoped controller.

To bound it with a quota, add a `scope: cluster` quota beside the tenants'
quota. Add these two entries to the `limiters:` list of the `default` entry.
Keep the entry's other keys.

```yaml
default: |
  limiters:
    - name: cluster-cap
      type: quota
      scope: cluster
      quotas:
        A100: 20   # 14 for team-a + team-b, plus 6 spare. ns-x may use any of the 20.
    - name: tenants
      type: quota
      scope: namespace
      exclude: [ns-x]
      namespaceQuotas:
        team-a: { A100: 8 }
        team-b: { A100: 6 }
```

This config logs no WARNING. Any namespace not listed, other than ns-x, is
denied.

The cluster quota caps everyone, tenants too, so size it as the tenants' caps
plus what `ns-x` may use. List every GPU type your namespaces use: a type it
does not list is denied to all of them. Like every cluster quota, it is per
controller. It reserves nothing: while team-a is idle, `ns-x` can use team-a's
part. Models take turns by how much they still need. With
`enableRescale: true` on the `default` entry, `ns-x` is sized before the
tenants when GPUs run short. So `ns-x` can take the whole cluster quota, even
while a tenant is below its cap and wants more.

**A missing entry denies.** `0` or no entry for an accelerator type means no
allocation; `-1` means unbounded. A typo in a type name is therefore a denial.

Fairness is per **model**, not per namespace. Two models in one namespace each
get their own fair-share slot; the cap bounds each and their running sum. The
quota clamps *after* the fair-share split: a ceiling on the outcome, not an
input to it.

**Several namespace-scoped controllers under `gpu-inventory` can over-ask
together.** Each computes free capacity from the same nodes without seeing the
others' claims in the same cycle. In sum they can order more than is free, and
the scheduler leaves the excess Pending. The installer warns about this for
side-by-side installs (`deploy/lib/single_install.sh`); it is inferred from the
design, not measured. A per-namespace quota on each, or separate GPU nodes,
removes the overlap.

### Verifying it

Start from the controller's log. It runs in your namespace if you installed
it; in the admin's (for example `scaling-manager-<your-namespace>`) in the out-of-reach
shape, where you need `pods/log` there or the admin reads these two lines.

```bash
# which policy governs this controller, read once at startup
kubectl logs -n <controller-namespace> deploy/scaling-manager-controller-manager \
  | grep -E 'Cluster policy comes from|Could not read the managed Namespace'

# the limiter it built: want limiterTypes "quota". "constructed" prints at
# every start, "none" included; a live policy change logs "(re)built from config".
kubectl logs -n <controller-namespace> deploy/scaling-manager-controller-manager \
  | grep -E 'GPU limiter (constructed|\(re\)built from config)'
```

`Cluster policy comes from this controller's own namespace` (logged at V(1),
which the default `-v=2` prints) means the budget is in the controller's own
`scaling-manager-scaling-policy-config`.
`... the well-known namespace` (or `... labelled with`) names a policy
namespace you normally cannot read: ask the admin for read access or the
figures. The admin can also read
`kubectl get cm scaling-manager-controller-status -n <scaling-manager-namespace> -o jsonpath='{.data.policyNamespace} {.data.limiter} {.data.policyAccepted}'`,
which shows `quota/<name>` (`inventory+quota/gpu-inventory,<name>` when a
`gpu-inventory` entry is declared beside it).

A clamped decision shows in Prometheus (labels `variant_name`,
`limiter_name`; the model's namespace is `exported_namespace`):

```text
increase(llmsm_decisions_limited_total{exported_namespace="<ns>"}[15m])
```

or as an Event on the model's ScaledObject:

```bash
kubectl get events -n <ns> --field-selector reason=ResourceConstrained
```

A clamp is a normal outcome, not an error. `llmsm_model_scaling_blocked` is not
the signal here: its quota reasons come only from the
[utilization-share optimizer](../utilization-share/).

**A fully spent quota is silent.** With nothing left to clamp, neither the
counter nor the Event appears. Compare what runs against the quota directly:

```bash
kubectl get pods -n <ns> -o custom-columns='NAME:.metadata.name,GPUS:.spec.containers[*].resources.limits.nvidia\.com/gpu,PHASE:.status.phase'
```

### How it is tested

- Unit: `internal/config/quota_limiter_test.go` (parsing, scopes, validation,
  the reserved-key rules) and
  `internal/engines/allocation/quota_inventory_test.go` (what the inventory
  reports and charges).
- End-to-end: `test/e2e/warm_pool_quota_test.go` covers a pool refused
  accelerators the quota will not give. `test/e2e/kueue_quota_test.go` asserts
  the quota entry the limiter logs after merging a Kueue grant into a static
  cap (one, then three again once the LocalQueue is gone): the entry, not what
  the optimizer then orders. `test/e2e/utilization_share_test.go` runs two
  models under a spent 4-GPU namespace quota and asserts the
  utilization-share optimizer's transfer (the donor's marked pod leaves before
  the receiver grows): that optimizer, not the default one.
- **No e2e covers the default optimizer's allocation under a static quota.**
  `test/e2e/limiter_test.go` exercises the GPU-inventory limiter only. That is
  the leg to add before treating a tenant cap as something CI protects.

### Tuning it

Every field, the namespace lookup rules, validation and reloads:
[the quota limiter reference](../../reference/quota-limiter.md). What the two
usage bases mean: [GPU capacity accounting](../../concepts/gpu-capacity-accounting.md).
