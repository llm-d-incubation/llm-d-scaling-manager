# Bound every install by real GPUs

## Overview

One command publishes a cluster policy with a GPU limiter, and gives every
install on the cluster the access it then needs. Without it, each controller
scales up to its workloads' `maxReplicaCount`, whatever GPUs exist.

## Prerequisites

- [ ] Cluster-admin rights.
- [ ] **Every variant's accelerator resolves.** The limiter gives no budget to a
  variant whose accelerator it cannot resolve, and that variant never scales up.
  Nothing else warns you, so check first:

<!-- guide:prerequisites.accelerators start -->
```bash
# List workloads whose accelerator could not be resolved. Empty is good.
kubectl get events -A --field-selector reason=AcceleratorNotResolved
# The same, from the controller log (logged once per change, so it may have rotated)
kubectl logs -n <scaling-manager-namespace> deploy/scaling-manager-controller-manager --tail=-1 | grep -i "Accelerator not resolved"
```
<!-- guide:prerequisites.accelerators end -->

To fix one, give the workload a GPU key in its `nodeSelector`. The scaling
manager also learns the accelerator from the nodes its running pods are on.

## Installation Instructions

<!-- guide:deploy.enable start -->
```bash
# Grant every controller the node read, then publish the limiter. Read the list it prints first.
make enable-physical-limiter
```
<!-- guide:deploy.enable end -->

What it does:

- **Finds every controller** on the cluster. You do not list them: the risk is
  the install you forgot. The list it prints marks each one it will restart and
  each it will skip.
- **Grants first, then publishes.** Declaring the limiter before the grants
  would stop every install that cannot read Nodes.
- **Publishes where each controller reads policy**: `scaling-manager-policy` (or the
  namespace a `scaling-manager.llmd.ai/policy-namespace` label names) for a controller
  that runs in the namespace it manages; its own ConfigMap for an
  out-of-reach controller, one that runs in an admin-owned namespace apart
  from the models it manages (`LLMSM_WATCH_NS`).
- **Restarts namespace-scoped controllers** whose policy lives outside the
  namespace they manage, on every run, because they read it once at startup.
  Cluster-scoped controllers reload it live.
- **Exits non-zero** on any failed step and names it. It never reports a limiter
  in force for a controller that does not read it yet.

A controller installed later gets the same grants from `make setup-prereqs`.

## Verification

<!-- guide:verify.controllers start -->
```bash
# Every controller should be Ready.
kubectl get pods -A -l app.kubernetes.io/name=scaling-manager
```
<!-- guide:verify.controllers end -->

<!-- guide:verify.source start -->
```bash
# Print each controller's policy namespace, limiter and whether it accepted the policy.
for ns in $(kubectl get deploy -A -l app.kubernetes.io/name=scaling-manager,control-plane=controller-manager \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{end}'); do
  echo "== $ns"
  kubectl get cm scaling-manager-controller-status -n "$ns" \
    -o jsonpath='{.data.policyNamespace} {.data.limiter} {.data.policyAccepted}{"\n"}' 2>/dev/null \
    || kubectl logs -n "$ns" deploy/scaling-manager-controller-manager --tail=-1 \
    | grep -E 'Cluster policy comes from|Could not read the managed Namespace|Cannot tell whether' \
    || echo "   (no status and no startup line: restart the controller to print it)"
done
```
<!-- guide:verify.source end -->

Expect a line such as `scaling-manager-policy inventory/gpu-inventory true`. With a quota
declared beside it, the limiter reads `inventory+quota/gpu-inventory,install-quota`:
the limiter types joined by `+`, then every entry's name.

Trust this over what the enable command printed. If a controller has no
`scaling-manager-controller-status`, its startup log answers instead:

- `Cluster policy comes from the well-known namespace` or `... the namespace
  this one is labelled with`: a policy namespace governs it.
- `Cluster policy comes from this controller's own namespace`: it reads its own
  ConfigMap.
- `Could not read the managed Namespace` or `Cannot tell whether`: it never saw
  `scaling-manager-policy`.

<!-- guide:verify.budget start -->
```bash
# Show the published limiter, then what the controller built from it.
POLICY_NS=$(kubectl get cm scaling-manager-controller-status -n <scaling-manager-namespace> \
  -o jsonpath='{.data.policyNamespace}' 2>/dev/null)
kubectl get configmap scaling-manager-scaling-policy-config -n "${POLICY_NS:-scaling-manager-policy}" \
  -o jsonpath='{.data.default}' | grep -A12 limiters
kubectl logs -n <scaling-manager-namespace> deploy/scaling-manager-controller-manager --tail=-1 \
  | grep -E 'GPU limiter constructed|Invalid saturation scaling'
```
<!-- guide:verify.budget end -->

`GPU limiter constructed` is printed at every start, with no limiter too. Read
its `limiterTypes`: `inventory`, `quota` or `inventory+quota` means bounded,
`none` means not. `Invalid saturation scaling` means the controller rejected
the policy. A rejected entry costs the whole default policy and leaves **no**
limiter, while the ConfigMap still looks healthy. The status line above says
the same with `policyAccepted` and `limiter`. Run this for each controller; one
whose `policyNamespace` is its own reads the ConfigMap there.

## Cleanup

<!-- guide:cleanup.disable start -->
```bash
# Remove the physical limiter. A quota in the same policy stays.
make disable-limiter
# a quota instead:
# make disable-limiter LLMSM_LIMITER_TYPE=quota
# every limiter:
# make disable-limiter LLMSM_LIMITER_TYPE=all
```
<!-- guide:cleanup.disable end -->

With no limiter left, scaling is unbounded again. `LLMSM_LIMITER_TYPE=quota`
removes a quota instead. `LLMSM_LIMITER_TYPE=all` removes every limiter; use it
when uninstalling, since it exits 0 even where nothing is declared.

## What it does and does not bound

- **A guardrail, not a security boundary.** Whoever owns a controller's
  Deployment can change what bounds it. Run tenant controllers
  [outside their namespace](../admin-cluster-setup/README.md#keeping-the-controller-out-of-the-tenants-reach),
  and use a `ResourceQuota` for tenants you do not trust.
- **It bounds only what the scaling manager asks for.** A higher
  `maxReplicaCount` or a second KEDA trigger bypasses it.
- **Both kinds bound together.** Declare `gpu-inventory` and `quota` and both
  apply: a model must fit the GPUs that exist and its allowance. Enabling one
  kind on a policy that already declares the other keeps the other.
  `make disable-limiter` removes one kind only; with only the other
  kind declared it removes nothing and exits non-zero. `LLMSM_LIMITER_TYPE=all`
  removes every limiter and exits 0 even where none is declared.
- **Budgets are per controller.** N controllers each enforce their own copy, so
  the cluster permits N times the figure.

## Configuration

| Parameter | Default | Example |
| --- | --- | --- |
| `LLMSM_LIMITER_TYPE` | `gpu-inventory` | `quota` |
| `LLMSM_QUOTAS` | none (**required** by `quota`) | `'H200=8 A100=4'` |
| `LLMSM_QUOTA_SCOPE` | `namespace` | `cluster` |
| `LLMSM_QUOTA_KUEUE` | `false` | `true` |
| `LLMSM_QUOTA_KUEUE_BORROW` | `true` (with `LLMSM_QUOTA_KUEUE=true`) | `false` |
| `KUEUE_NAMESPACE` | the entry's `kueue.controllerNamespace`, else `kueue-system` | `kueue` |
| `LLMSM_LIMITER_TARGETS` | every labelled controller that passes the checks below | `team-a team-b` |
| `LLMSM_CONTROLLER_SA` | `scaling-manager-controller-manager` | `my-scaling-manager-sa` |
| `LLMSM_CONTROLLER_IMAGE_REPOS` | `ghcr.io/ev-shindin/llm-scaling-manager`, plus the repository of `IMG` when it differs | `'registry.local/scaling-manager ghcr.io/ev-shindin/llm-scaling-manager'` |

A quota limiter:

```bash
# Cap each managed namespace at 8 H200s
make enable-physical-limiter LLMSM_LIMITER_TYPE=quota LLMSM_QUOTAS='H200=8'
```

Take the numbers from Kueue instead (experimental, see [bound tenants by the
quotas Kueue already holds](../../well-lit-paths/kueue-bounded-quotas/)):

```bash
# Bound by real GPUs first, then add the Kueue quota beside it
make enable-physical-limiter
make enable-physical-limiter LLMSM_LIMITER_TYPE=quota LLMSM_QUOTAS='H200=-1' LLMSM_QUOTA_KUEUE=true
```

Each namespace gets the smaller of Kueue's grant and `LLMSM_QUOTAS`; `-1` lets
Kueue decide. Cohort borrowing is counted by default, and it needs something
that checks the borrowed GPUs exist: the `gpu-inventory` limiter, or a GPU
`ResourceQuota` in the namespace. Without either, borrowing is withheld as
`no-gpu-bound` and each namespace stays at its nominal quota. Pass
`LLMSM_QUOTA_KUEUE_BORROW=false` to cap at nominal on purpose.

Name exactly the admin-owned namespaces to grant:

```bash
# Grant only these controller namespaces
make enable-physical-limiter LLMSM_LIMITER_TARGETS='scaling-manager-team-a scaling-manager-team-b'
```

Point a controller at another policy namespace by labelling the namespace it
manages, then re-run:

```bash
# Read policy from <policy-namespace> for this managed namespace
kubectl label namespace <managed-namespace> scaling-manager.llmd.ai/policy-namespace=<policy-namespace>
make enable-physical-limiter
```

## Details

### Which controllers are granted

A Deployment with the controller label is granted only when it runs as
`LLMSM_CONTROLLER_SA` and has a `manager` container whose image comes from
`LLMSM_CONTROLLER_IMAGE_REPOS`. Anything else is listed as `SKIPPED` and gets
nothing.

These checks are **signals, not proof**. A tenant can create a ServiceAccount
named `scaling-manager-controller-manager` and run the public image in a container named
`manager`. The checks stop a grant to an account a Deployment merely names,
such as `default`, not a tenant who sets out to pass them. Where tenants are not
trusted, run their controllers in admin-owned namespaces ([the `LLMSM_WATCH_NS`
shape](../../reference/gpu-limiter.md#the-arrangement-where-the-bound-does-hold))
and name those namespaces in `LLMSM_LIMITER_TARGETS`.

A namespace named in `LLMSM_LIMITER_TARGETS` is vouched for by you, so its image is
not checked; the ServiceAccount and `manager` container still are. That is how a
controller built from a mirror gets granted. The other way is to list the mirror
in `LLMSM_CONTROLLER_IMAGE_REPOS`.

### What the summary and the disable command report

- The summary lists every skipped Deployment as **NOT bound**, with the reason.
  A target namespace with no controller is NOT bound too, and nothing is written
  for it.
- A skipped controller keeps the policy it read before.
  `make disable-limiter` does not write it either, and says so if it
  still declares a limiter.
- A policy that declares no limiter is left as it is, with `No limiter declared
  in <ns>/scaling-manager-scaling-policy-config; nothing to remove.`
- Both commands need `kubectl`, `jq` and `yq` on `PATH`, and stop before
  changing anything if one is missing. A failed edit never leaves an emptied
  policy behind.

### The policy namespace

There is no setting for it. A controller in the namespace it manages reads
`scaling-manager-policy` unless that namespace has a `scaling-manager.llmd.ai/policy-namespace` label.
`LLMSM_POLICY_NS`, read by earlier releases, is refused when it names anything
other than `scaling-manager-policy`. Changing the label later needs a re-run of
`make enable-physical-limiter`, which grants the new namespace and restarts the
controller.

Such a controller also gets `get` on Namespaces, limited to the names it reads:
its own namespace, `scaling-manager-policy` and its labelled policy namespace.

### `LLMSM_LIMITER_TYPE` is not `LLMSM_LIMITER`

`LLMSM_LIMITER_TYPE` writes the **cluster** policy, and the own ConfigMap of each
admin-owned controller. `LLMSM_LIMITER` (on `make deploy-scaling-manager`) writes one install's
own policy, which that install's owner can then edit.

### Quotas

- `LLMSM_QUOTAS` has no default. `LLMSM_LIMITER_TYPE=quota` refuses to run without
  it, before it creates or grants anything.
- A quota entry that names no accelerator is **not** unlimited. It is a budget
  of zero for every type, and published here it would stop every managed
  workload from scaling up.
- `LLMSM_QUOTA_SCOPE=namespace` (the default) gives each managed namespace the
  budget. `cluster` caps the sum, **within one controller**. With N admin-owned
  controllers the cluster permits N times the number, in either scope.
- `LLMSM_QUOTA_KUEUE=true` also grants each target controller the Kueue reads:
  - ClusterQueues, Cohorts and ResourceFlavors, and `get` on the namespace it
    manages, by name. A cluster-scoped controller the installer made already
    reads every Namespace. The command warns only when a controller that looks
    cluster-scoped lacks that read, or when it cannot tell (it may not
    impersonate the controller).
  - A Role in the Kueue namespace (`KUEUE_NAMESPACE`) to read the
    `kueue-controller-manager` Deployment and its leader Lease, so the
    controller can tell Kueue runs.
  - While the published policy counts borrowing (the default): Kueue
    Workloads and LocalQueues in **every** namespace, pod templates included.
    `LLMSM_QUOTA_KUEUE_BORROW=false` revokes it on the next run. What that read
    exposes is in [the security model](../../reference/security-model.md#kueue-borrowing).

### Why `GPU budgets available` is not the check

That log line comes only from the scale-from-zero engine while it judges a wake.
A correctly bounded cluster with nothing parked at zero never prints it.

## Next

- [The GPU limiter](../../reference/gpu-limiter.md): why policy lives where it
  does, and the quota form
