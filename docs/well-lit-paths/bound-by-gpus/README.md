# Bound a fleet by real GPUs

Stops the scaling manager from asking for more GPUs than the cluster has. For
any shared cluster: where more than one team scales on the same accelerators,
or where `maxReplicaCount` is a safety limit rather than a real ceiling.

**Maturity:** Stable.

## When not to take it

- You need enforcement. This limiter is advisory: it stops the controller
  *asking* for replicas that cannot be placed. The boundary that holds is a
  GPU `ResourceQuota` ([bound at admission](../../guides/admin-cluster-setup/README.md#bound-the-namespaces-gpus-at-admission)).
  Use both.
- Your question is "is this tenant within its allowance?", not "is the
  hardware there?". Take [cap what each tenant may take](../tenant-gpu-quotas/).

## Start here

You need a cluster admin, and controllers installed by
[the saturation path](../scale-on-saturation/#start-here). Every step is in
[Bound every install by real GPUs](../../guides/admin-gpu-bounding/).

1. Cluster admin: [check accelerators resolve](../../guides/admin-gpu-bounding/README.md#prerequisites). If they do not, the failure has no symptom.
2. Cluster admin: run [`make enable-physical-limiter`](../../guides/admin-gpu-bounding/README.md#installation-instructions). Read the list it prints first: some controllers will be restarted ([details](#what-the-command-restarts)).
3. [Check it works](../../guides/admin-gpu-bounding/README.md#verification): each controller reports the limiter `inventory/<name>`, not `none`.

To undo, the admin runs `make disable-limiter`
([Cleanup](../../guides/admin-gpu-bounding/README.md#cleanup)). It removes only
the physical limiter; a quota in the same policy stays. To bound one
install instead, its namespace owner adds `LLMSM_LIMITER=gpu-inventory` to
`make deploy-scaling-manager` ([configuration](../../reference/configuration.md#core-configuration));
the same owner can edit it away.

## What it costs

- No accelerators.
- Latency when the budget binds: a model held at its budget queues what an
  extra replica would have served. That shows up as TTFT (time to first
  token), not as an error.
- An owner: "why did we not scale" becomes a declared policy somebody answers for.

## Evidence

No benchmark run. The end-to-end suites under
[How it is tested](#how-it-is-tested) cover it.

## Details

### What it is

Without a limiter, each controller scales its workloads up to their
`maxReplicaCount`, and nothing checks that against the accelerators that
exist. A limiter gives the optimizer a budget: the GPU inventory discovered on
the cluster (`gpu-inventory`), or per-accelerator caps an operator declares
(`quota`). A limiter without a quota bounds a well-behaved autoscaler, not the
cluster.

### What it needs

- Cluster-admin rights, once. The policy must live where a tenant cannot edit
  it, and every controller that follows it must be able to read it.
- Every controller able to **list nodes**. A controller that cannot resolve a
  variant's accelerator gives it no budget and never scales it up. That is why
  the guide checks `reason=AcceleratorNotResolved` first.
- On **Kubernetes**, a namespace-scoped controller needs `get` on its own
  Namespace, which `make setup-prereqs` grants ([older installs](#details-older-installs)).
- A **namespace-scoped** controller also needs `list` on Pods **cluster-wide**.
  Neither `make setup-prereqs` nor `make enable-physical-limiter` grants it.
  Without it the controller counts only the GPUs its own namespace holds, so
  GPUs other tenants hold on the same nodes read as free and it orders
  replicas that stay Pending. It reports this (`llmsm_pod_access_denied` = 1 and
  an error log), but keeps scaling. Grant the list, or bound that controller
  with a quota instead ([details](../../reference/gpu-limiter.md#when-policy-demands-a-limiter-the-controller-cannot-serve)).
- **A scheduler that packs GPU pods**, when replicas of different sizes share
  nodes. The scaling manager plans each pod into the smallest hole that holds
  it. The default kube-scheduler scores `LeastAllocated` and spreads instead,
  so a 4-GPU pod can land on the free 8-GPU node a planned 8-GPU replica
  needed, which then stays Pending. Score GPU nodes with `MostAllocated` and a
  GPU resource weight ([how](../../reference/gpu-limiter.md#the-scheduler-must-pack)).
  With one replica size, or one replica per node, this does not matter.

### What the command restarts

`make enable-physical-limiter` restarts every namespace-scoped controller whose
policy lives outside the namespace it manages (marked `WILL BE RESTARTED`), on
this run and every later one. Leave a namespace out of `LLMSM_LIMITER_TARGETS`
where that restart is not acceptable now. A later `make deploy-scaling-manager` without
`LLMSM_LIMITER` keeps the limiter and says so; `LLMSM_LIMITER=none` removes it
([why](../../reference/scaling-policy.md#re-running-the-installer-resets-the-default-entry)).
`make disable-limiter` leaves the node-read RBAC in place.

### Which limiter

**You can declare both.** Every limiter in the `limiters:` list is built, and a
model must fit all of them. Enabling one kind keeps the other. Declare both when
the allowance can exceed the GPUs that exist, for example a Kueue quota on a
shared cluster.

| | `gpu-inventory` | `quota` |
| --- | --- | --- |
| Question | is the hardware there? | is this tenant within its allowance? |
| Counts | every GPU-holding Pod on the cluster | only what **the scaling manager's own variants** hold |
| Declared | nothing to declare: discovered | per accelerator type, at cluster or namespace scope |

The shipped policy declares no limiter. `gpu-inventory` is only the default of
`LLMSM_LIMITER_TYPE`, which `make enable-physical-limiter` reads. What a quota
counts, why a cap is a ceiling and not a reservation, and why a missing entry
denies are in [capping what each tenant may take](../tenant-gpu-quotas/).

### Verifying it

The guide's [Verification](../../guides/admin-gpu-bounding/README.md#verification)
has the commands. Read them like this:

- `GPU limiter (re)built from config` with `limiterTypes` `inventory` (after
  a restart, `GPU limiter constructed` with that `limiterTypes`), and no
  `Invalid saturation scaling`. `GPU limiter constructed` alone proves nothing:
  every controller prints it at start, `limiterTypes` `none` included.
- `llmsm_model_scaling_blocked` carries the reason as a label. A model that
  wanted more and did not get it is a normal, visible outcome here.
- `scaling-decision` lines show what the optimizer decided, with a reason code
  per entry.

Admin-owned controllers read their own ConfigMap by design; see
[the GPU limiter](../../reference/gpu-limiter.md#what-an-admin-sets-and-where).

### How it is tested

- End-to-end: `test/e2e/limiter_test.go` (labelled `full`), and
  `test/e2e/warm_pool_quota_test.go` for the refusal when a pool asks for
  accelerators the quota will not give.
- The accounting, and where a GPU budget can still over-state free capacity,
  are in [gpu-capacity-accounting.md](../../concepts/gpu-capacity-accounting.md).

### Tuning it

The controller reads only the `limiters:` list on the scaling-policy
ConfigMap's `default` entry. It is dynamic, so the engine rebuilds it without a
restart. The exception is a namespace-scoped controller reading it from
outside its namespace, which reads it at startup; `make enable-physical-limiter`
restarts those.

The installer writes that list: `LLMSM_LIMITER=quota` with `LLMSM_QUOTAS='H200=8'`
for one install's own policy, and `LLMSM_LIMITER_TYPE` with the same
`LLMSM_QUOTAS` for the cluster policy (read by the controllers listed in
[the guide](../../guides/admin-gpu-bounding/#installation-instructions)). The
quota form **requires** a budget: an entry naming no accelerator is a budget
of zero, not unlimited. Where Kueue already holds the numbers,
`LLMSM_QUOTA_KUEUE=true` reads them from its ClusterQueues:
[bound tenants by the quotas Kueue already holds](../kueue-bounded-quotas/)
(experimental). Schema and validation: [quota limiter](../../reference/quota-limiter.md).
Where the policy lives and who may edit it: [the GPU limiter](../../reference/gpu-limiter.md).

### Details: older installs

A namespace-scoped controller set up before `make setup-prereqs` granted `get`
on its Namespace (`scaling-manager-policy-namespace-reader`) does not see `scaling-manager-policy`. It
keeps reading its own ConfigMap and says so: `Cannot tell whether a cluster
admin published policy` or `Could not read the managed Namespace` in its log,
`llmsm_policy_namespace_unverified` = 1, and alert
`LLMSMPolicyNamespaceUnverified`. A cluster admin re-runs
`NAMESPACE=<ns> make setup-prereqs`, or `make enable-physical-limiter`, which
also restarts it ([upgrading](../../reference/upgrade-and-uninstall.md#upgrading-from-before-v0100-rc1);
[grant it by hand](../../guides/admin-cluster-setup/README.md#grant-the-namespace-read-by-hand)
only where neither can run).
