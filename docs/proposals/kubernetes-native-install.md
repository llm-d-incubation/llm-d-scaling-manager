# Install with kubectl and kustomize, without docker

- **Status:** phase 1 built, in [#409](https://github.com/llm-d-incubation/llm-d-scaling-manager/pull/409),
  released in v0.13.0. Phases 2 to 4 are partly built or open
  ([appendix B](#b-what-was-built-and-the-phases)).
- **Audience:** contributors and maintainers. Operators install from the
  [kubectl install guide](../guides/kubectl-install/), which is the source of
  truth for the commands. The commands here are a summary of it.
- **Builds on:** [Install from the release](install-from-release.md) and
  [Install in two commands](two-command-install.md).
- **Changed after review.** The first draft opted a namespace in with a
  label, narrowed by an admin-owned `namespaceSelector`. #409's review
  replaced that with the existing `watchNamespaces` setting, read live. The
  reasons are under [Alternatives](#alternatives).

## Summary

A cluster admin installs the scaling manager the way KEDA, Kueue and
LeaderWorkerSet install: signed release files, applied as downloaded with
`kubectl`. No docker, no checkout, and no file is edited before it is
applied. Two scripts that run `kubectl` only apply the files, one for the
cluster admin and one for a namespace owner; `--print` shows the commands
instead, for GitOps.

The config is the existing settings ConfigMap, and the status is the
existing status ConfigMap. The admin picks the managed namespaces with the
existing `watchNamespaces` setting, which this install reads live. The
controller starts passive, runs its own preflight checks, fills its own
scaler certificates and KEDA's copy, reports each namespace's readiness, and
suggests the ScaledObjects. A person still reviews and applies them. `smt`
keeps working on the same release files.

## Motivation

Before v0.13.0, an install took the tools image (docker) or about 20
commands by hand. The controllers an llm-d cluster already runs install
with one manifest.

**Goals.** Install, configure, upgrade and uninstall with `kubectl` only. No
docker, no checkout, no edit of a signed file. Config that GitOps can hold.
The passive first install and the preflight stay.

**Non-goals.** A Helm chart (forbidden by AGENTS.md; see
[Alternatives](#alternatives)). A new CRD in phase 1. Changing how the
controller decides.

## Proposal

### User stories

- As a cluster admin, I install the signed files and list the namespaces to
  manage.
- As a platform team, I keep the install commands and the settings in git,
  and flip passive, shadow and active with `kubectl`.
- As a namespace owner, I review the ScaledObjects the controller suggests
  before anything scales, and read my namespace's readiness without access to
  the controller's namespace.

### One namespace

The cluster admin downloads and verifies the files, then installs:

```bash
V=v0.13.0
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V
for f in scaling-manager.yaml scaling-manager-keda.yaml scaling-manager-grants.yaml scaling-manager-monitoring.yaml \
    scaling-manager-admin.sh scaling-manager-owner.sh; do
  curl -fsSLO $URL/$f && curl -fsSLO $URL/$f.sigstore.json
  cosign verify-blob $f --bundle $f.sigstore.json \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/$V"
done
bash scaling-manager-admin.sh install --prometheus-url https://prometheus.monitoring.svc:9090 \
  --keda-namespace keda --watch-namespaces team-a
kubectl get configmap scaling-manager-scaledobjects-plan -n scaling-manager-system \
  -o jsonpath='{.data.team-a}' > team-a-scaledobjects.yaml      # handed to the owner
```

`install` applies `scaling-manager.yaml` (the namespace, RBAC, the controller
and the ClusterTriggerAuthentication), applies `scaling-manager-keda.yaml` in
KEDA's namespace with `-n`, labels that namespace
`scaling-manager.llmd.ai/keda=true`, and writes the settings ConfigMap:
`prometheusURL`, `kedaNamespace`, `caFromSecret` and `watchNamespaces`.

The namespace owner reviews the file and registers it:

```bash
bash scaling-manager-owner.sh register team-a team-a-scaledobjects.yaml
bash scaling-manager-owner.sh status team-a                     # readiness, read in team-a
```

### Several namespaces, one budget, a DisaggregatedSet, sharing in shadow

```bash
bash scaling-manager-admin.sh install --prometheus-url https://prometheus.monitoring.svc:9090 \
  --keda-namespace keda --watch-namespaces team-a,team-b
bash scaling-manager-admin.sh set limiter=quota quotas=NVIDIA-H100-80GB-HBM3=16 quotaScope=cluster
# Static DisaggregatedSet roles only: the owner binds the role writer's grant,
# the admin turns the writer on, the owner opts the set in.
bash scaling-manager-owner.sh grant dset-role-writer team-a
bash scaling-manager-admin.sh set dsetRoleWriter=true utilizationShare=shadow   # never in git
kubectl annotate disaggregatedset <set> -n team-a scaling-manager.llmd.ai/role-writer=true \
  scaling-manager.llmd.ai/model-id=<model> scaling-manager.llmd.ai/min-replicas=1 \
  scaling-manager.llmd.ai/max-replicas=<max groups per role>
```

The install is passive: it computes, publishes and moves nothing. To see
whether a role is Static or External:
`kubectl get disaggregatedset <set> -n team-a -o jsonpath='{.spec.roles[*].scaling.mode}'`.
External roles need no grant. The suggested plan covers Deployments only, so
a DisaggregatedSet's ScaledObjects are written by hand
([appendix A](#a-disaggregatedset-step-by-step) has the steps).

### What you get

- **The preflight, inside the controller.** It runs every minute and writes
  each result to the status ConfigMap `scaling-manager-controller-status`:
  one `preflight.<check>` key per check, plus `ready`. A change of a check
  becomes an Event. There is no preflight Job on this install.
- **The managed namespaces, live.** `watchNamespaces` selects them without a
  restart. The cache stays cluster-wide. The scaler refuses, with
  `PermissionDenied`, a ScaledObject in any other namespace, and ends an
  open stream once its namespace is left out. The controller drops that
  namespace's InferencePools.
- **Each namespace's readiness.** A `namespace.<name>` key in the status
  (`ok`, `waiting`, `not ready`, `not managed`, with what to do), and the
  same line as an Event on that namespace's ScaledObjects and InferencePools,
  so an owner reads it without access to the controller's namespace.
- **Its own TLS, with no `create` on Secrets.** The manifest ships the
  scaler's Secrets as `Opaque` with no data, and the controller fills and
  rotates them by name. Over an `smt` install it copies the old CA across.
- **KEDA wired for you.** `scaling-manager-keda.yaml` creates the one Secret
  KEDA reads, in KEDA's namespace, and a Role there that lets the controller
  update that Secret by name. The controller fills it and switches to mutual
  TLS without a restart. KEDA's client certificate never goes to a model
  namespace.
- **A suggested plan.** One ScaledObject per model-server Deployment in each
  managed namespace, in ConfigMap `scaling-manager-scaledobjects-plan`. A
  person reviews and applies it.

### Check it, and switch it

```bash
bash scaling-manager-admin.sh status            # ready, namespacesManaged, namespace.<name>, each check
```

`ready` is `ok` when no check fails; a WARN or UNKNOWN does not block it.
`namespacesManaged` is the list, or `*` for every namespace.
`namespacesRefused` lists the namespaces KEDA called the scaler for that are
not managed. `kubectl wait` cannot wait on ConfigMap data, so a script polls
`ready`.

Passive, then shadow, then active, one key each:

```bash
A="bash scaling-manager-admin.sh set"
$A utilizationShare=shadow                          # sharing computes and publishes, moves nothing
$A passive=false                                    # the controller scales
bash scaling-manager-owner.sh grant pod-writer team-a   # the owner, per sharing namespace
$A utilizationShare=active                          # sharing moves replicas
```

The controller checks each grant a switch needs with a
SelfSubjectAccessReview, and a missing one turns the namespace `not ready`
with the command to run. To undo, set `utilizationShare` back to `shadow`,
wait one cycle (15 s) so no transfer marks are left on pods, then set it to
`off` and delete the pod-writer grants. The budget stays.

**Switch keys are never in git.** `passive`, `utilizationShare` and
`dsetRoleWriter` are set with `kubectl`. Argo CD with server-side apply and
Flux both force their ownership, so a switch left in the synced file
silently resets a live change on every sync ([appendix E](#e-gitops)).

### The trade-offs

- **Cluster-wide reads.** The controller reads cluster-wide, with the same
  reads as today's cluster-wide install ([appendix C](#c-rbac-in-full)).
  `watchNamespaces` limits where it acts, not what it can read.
- **An empty `watchNamespaces` manages every namespace**, as on every
  cluster-wide install. A fresh install without `--watch-namespaces` then
  serves a ScaledObject in any namespace and shares one budget across all of
  them. On a shared cluster, list the namespaces. A rejected value keeps the
  list in force; a rejected value with no list before it manages nothing.
- **Only the cluster admin opts a namespace in.** The setting lives in the
  controller's namespace, which only the cluster admin owns on this install.
- **Active sharing is a grant per namespace.** The namespace owner binds it
  from `scaling-manager-grants.yaml`. It is never cluster-wide.
- **KEDA's namespace is admitted by a label.** The scaler's NetworkPolicy
  admits the namespace labelled `scaling-manager.llmd.ai/keda=true`. The
  label opens the network path only: once KEDA's copy is written the scaler
  serves mutual TLS, so a caller also needs KEDA's client certificate.

| | cluster-wide `smt` install | this install | namespaced `smt` install |
| --- | --- | --- | --- |
| reads | cluster-wide | the same | per namespace |
| which namespaces | `--model-namespace`, at install | `watchNamespaces`, live | its own |
| pod `patch` | per namespace, at activation | per namespace, bound by the owner | the same as cluster-wide |
| KEDA's copy | the admin's kubeconfig | the controller, one Secret by name | the admin's kubeconfig |
| `create` on Secrets | no | no | no |
| commands, one namespace | 3 with `smt`, plus docker setup | the download loop and 1 for the admin; 1 for the owner | 3 with `smt`; about 16 on the [advanced path](#a-the-advanced-path-least-privilege-per-namespace) |

## Design details

The appendices hold the detail:

- [B](#b-what-was-built-and-the-phases): what was built, what is left, and the phases.
- [C](#c-rbac-in-full): RBAC in full.
- [D](#d-monitoring): monitoring.
- [E](#e-gitops): GitOps.
- [F](#f-upgrade-rollback-air-gapped-uninstall): upgrade, rollback, air-gapped use and uninstall.
- [G](#g-migration-from-smt): migration from `smt`.
- [K](#k-verification-and-tests): verification and tests.

## Alternatives

- **A namespace label, narrowed by `namespaceSelector`** (the first draft).
  A namespace opted in with `scaling-manager.llmd.ai/managed=true`, and an
  admin-owned selector, plus an optional ValidatingAdmissionPolicy
  (`scaling-manager-label-guard.yaml`), limited which labelled namespaces
  counted. Replaced in #409 by `watchNamespaces`, because:
  - anyone who can create a namespace can label it, which is why the
    selector and the label guard had to exist;
  - the selector was only ever used as `kubernetes.io/metadata.name in
    (...)`, which is a list of names, and `watchNamespaces` is that list;
  - `watchNamespaces` is the key every other install already has, and
    keeping both would give the admin two lists to keep in step.

  An old `namespaceSelector` key is reported as unknown, with the hint that
  `watchNamespaces` replaced it.
- **Editing the manifest for KEDA's namespace** (a `sed`, or a
  `keda-namespace` kustomize component). Replaced by the `kedaNamespace`
  setting and `scaling-manager-keda.yaml` applied with `-n`, so the signed
  files are applied as downloaded.
- **KEDA's client certificate in each model namespace**, behind a namespaced
  TriggerAuthentication. Rejected: whoever can read it can pose as KEDA, the
  client identity is one SAN and the scaler does not check which namespace a
  call is for, and writing it would need `create` on Secrets in every model
  namespace. The copy stays in KEDA's namespace only. A namespaced
  TriggerAuthentication is worth revisiting only with a per-namespace client
  identity and a server-side check that the caller's namespace matches the
  ScaledObject's.
- **Least privilege per namespace** ([appendix A](#a-the-advanced-path-least-privilege-per-namespace)): kept as the advanced path, not built, because it costs about 16 commands and 11 files per namespace.
- **Helm** ([appendix J](#j-helm)): closest to the router, but forbidden by AGENTS.md.
- **A CRD** ([A CRD later?](#a-crd-later)): deferred.
- **`smt` only**: it keeps working, but needs docker.

## Release notes

Shipped in v0.13.0 (#409): a kubectl-only install from one signed manifest.
New release assets: `scaling-manager.yaml`, `scaling-manager-keda.yaml`,
`scaling-manager-grants.yaml`, `scaling-manager-monitoring.yaml`,
`scaling-manager-admin.sh` and `scaling-manager-owner.sh`, all in
`SHA256SUMS` and signed. New settings keys: `kedaNamespace`, `caFromSecret`,
`dsetRoleWriter`; `watchNamespaces` is live on this install. New status
keys: `ready`, `preflight.<check>`, `namespacesManaged`, `namespacesRefused`,
`namespace.<name>`, `kedaConnection`, `kedaCopyStale`. `smt`, every earlier
release file and `make deploy-scaling-manager` are unchanged.

## Open questions: decisions for the maintainers

Decided, in #409 and its review:

| question | decision |
| --- | --- |
| how a namespace is chosen | the existing `watchNamespaces` setting, live on this install; no label, no `namespaceSelector`, no label guard |
| an empty `watchNamespaces` | every namespace, as on every cluster-wide install. Any change to that meaning goes in a later PR |
| a rejected `watchNamespaces` | keeps the list in force; a rejected first value manages nothing (fails closed, never to every namespace) |
| KEDA in another namespace | the `kedaNamespace` setting, `scaling-manager-keda.yaml` applied with `-n`, the NetworkPolicy admitting KEDA's namespace by label. No edit of a signed file |
| how the files are applied | as downloaded, by two `kubectl`-only scripts; `--print` for GitOps |
| KEDA's client certificate | only in KEDA's namespace, in a Secret the manifest pre-creates; the controller updates it by name. Never in a model namespace. No namespaced TriggerAuthentication |
| the scaler's TLS Secrets | `Opaque`, no data, under new names (`scaling-manager-scaler-{ca,serving,client}`); the `smt` CA copied once. No `create` on Secrets |
| the live switches | stay in the settings ConfigMap, set with `scaling-manager-admin.sh set`, never in git |
| who binds the grants | the namespace owner binds `pod-writer` and `dset-role-writer` in their namespace; the admin binds an EPP's token review (`grant-epp`) |
| the DisaggregatedSet role writer | per namespace, and it scans only the namespaces holding its grant |
| passive by default | yes, from the Deployment (`LLMSM_PASSIVE=true`), below the live `passive` setting |
| the plan | the controller writes it; a person applies it; the admin hands each owner their namespace's part as a file |
| readiness for a namespace owner | `namespace.<name>` in the status, and the same line as an Event in that namespace, recorded on change only |
| a CRD | not in phase 1 ([A CRD later?](#a-crd-later)) |

Still open ([appendix N](#n-open-questions)):

- **Cluster-wide by default.** The main path needs cluster-wide reads, and
  least privilege per namespace is the
  [advanced path](#a-the-advanced-path-least-privilege-per-namespace), not
  built.
- **Helm.** [AGENTS.md](../../AGENTS.md) forbids a chart. The router
  installs with Helm, and the llm-d guides install LeaderWorkerSet and
  Kueue from their Helm OCI charts. A chart over this manifest would be
  thin ([appendix J](#j-helm)).
- **A git ref, or a signed kustomize tarball, for llm-d recipes.**
  Install-from-release ruled out a remote kustomize base on 2026-11-10, but
  an llm-d recipe needs an in-repo kustomization
  ([appendix H](#h-the-llm-d-recipe-path)).
- **The plan's reader.** The plan ConfigMap is in `scaling-manager-system`,
  which a namespace owner cannot read.
- **The EPP NetworkPolicy rule** selects every pod of the namespace on port
  9090, since router charts label their EPP pods differently.

## Appendices

### A. The advanced path: least privilege per namespace

**Not built** (phase 3). Until it is, a namespaced `smt` install is the
least-privilege option.

For a controller per namespace that needs no cluster-wide reads, or a
namespace owner who installs without a cluster admin beyond a first step.
It is the design two review rounds settled. It holds more files and more
commands: about 16 commands and 11 small files for one namespace; about 27
and 16 for several namespaces with a DisaggregatedSet and shadow sharing.
`kubectl scaling-manager init` (phase 4) would write the files.

Here the owner holds `admin` in the controller's namespace, so nothing there
may hold a right the owner must not have. That is why KEDA's copy is made by
a Job in KEDA's namespace, not by the controller as on the main path, where
only the cluster admin owns the controller's namespace. On both paths KEDA's
client certificate goes only to KEDA's namespace.

It differs from the main path in four ways:

- **The bases.** The admin keeps `sm-admin/`, the owner `sm-owner/`, and the
  admin applies the temporary `sm-steps/` by hand. The bases come from the
  release's kustomize tarball, verified with `cosign verify-blob`.
- **The preflight.** A preflight Job, run before and after the install.
- **KEDA's copy.** A `keda-auth` Job, run in KEDA's namespace.
- **The config.** The settings ConfigMap lives in the owner's namespace.

The internals are in [For developers](#the-advanced-paths-internals).

| directory | `namespace:` | holds |
| --- | --- | --- |
| `sm-admin/controller-namespace/` | | a component: sets the controller's namespace in binding subjects |
| `sm-admin/` | | `resources:` the four directories below |
| `sm-admin/controller/` | `$CTRL` | `controller-admin`, plus the component |
| `sm-admin/cluster/` | `$CTRL` | `cluster-admin`, `nameSuffix: -$CTRL`, plus the component |
| `sm-admin/<ns>/` | `<ns>` | `model-namespace`, the monitoring components needed, plus the component |
| `sm-admin/keda/` | `keda` | `keda-auth-setup`, `nameSuffix: -$CTRL` |
| `sm-steps/preflight/` | `$CTRL` | `steps/preflight`, `nameSuffix: -$CTRL` |
| `sm-steps/keda-auth/keda/` | `keda` | `steps/keda-auth/keda`, `nameSuffix: -$CTRL` |
| `sm-steps/keda-auth/controller/` | `$CTRL` | `steps/keda-auth/controller`, and a patch naming the Job's ServiceAccount in `keda` |
| `sm-owner/` | `$CTRL` | `owner`, and the settings ConfigMap |

A model namespace's directory, written out:

```yaml
# sm-admin/team-a/kustomization.yaml
namespace: team-a
resources:
- ../../scaling-manager-v0.13.0/model-namespace
components:
- ../controller-namespace
- ../../scaling-manager-v0.13.0/components/modelserver-monitoring   # only where nothing scrapes
```

The `controller-namespace` component is needed because a kustomization's
`namespace:` does not move a binding's subject to another build's
ServiceAccount ([the renders](#l-the-render-and-kind-evidence)).

The steps, for `CTRL=team-a`, with `kubectl diff -k` before each apply:

```bash
kubectl create rolebinding scaling-manager-owner -n $CTRL \
  --clusterrole=admin --user=<the owner>                                # admin, once
kubectl apply -k sm-steps/preflight/                                    # admin: preflight run 1
kubectl logs -f -n $CTRL job/scaling-manager-preflight-${V//./-}-$CTRL  # podmonitor.port, metrics.model FAIL: expected on run 1
kubectl apply -k sm-admin/                                              # admin
kubectl apply --server-side -k sm-owner/                                # owner: the settings and the controller
kubectl apply -k sm-steps/keda-auth/controller/                         # admin: the keda-auth Job's reads in $CTRL
kubectl apply -k sm-steps/keda-auth/keda/                               # admin: the Job, in KEDA's namespace
kubectl logs -f -n $KEDA_NS job/scaling-manager-keda-auth-${V//./-}-$CTRL   # ends with "mutual TLS"
kubectl delete job -n $CTRL scaling-manager-preflight-${V//./-}-$CTRL
kubectl apply -k sm-steps/preflight/                                    # admin: preflight run 2
kubectl logs -f -n $CTRL job/scaling-manager-preflight-${V//./-}-$CTRL
kubectl delete -k sm-steps/keda-auth/keda/ ; kubectl delete -k sm-steps/keda-auth/controller/
kubectl delete -k sm-steps/preflight/                                   # admin: the temporary rights go
```

**Preflight run 1** comes before any admin object and before the
controller. Expected results:

- UNKNOWN: `rbac.controller`, `keda.mtls`, `scaler.tls`.
- WARN: `keda.networkpolicy`, `rbac.namespaces`.
- FAIL: `podmonitor.port` and `metrics.model`, unless something already
  scrapes the model servers.

What run 1 is for is the refusals: another scaling manager in a namespace,
or a namespaced install beside a cluster-wide one. Those checks are new
preflight work.

**Several namespaces** (`CTRL=scaling-manager-system`). Each model namespace
gets its own `sm-admin/<ns>/`, plus `sm-steps/preflight-<ns>/` for the
preflight's reads there. Two list-valued patches are also needed:

- `sm-admin/cluster/model-namespaces.yaml`: the Namespace reader's
  `resourceNames`;
- `sm-owner/watch.yaml`: `LLMSM_WATCH_NAMESPACE` on the Deployment.
  `watchNamespaces` can only narrow what the Deployment names, and a
  render confirmed the patch replaces that one env entry.

To remove a namespace, delete its directory first, since `kubectl apply`
does not prune, then edit both lists and apply.

The settings ConfigMap lives in `sm-owner/` only, applied server-side, and
the switch keys stay out of it, as on the main path. The scaler TLS Secrets
are made the same way on both paths: `Opaque` with no data, in the applied
base (`owner` here), filled by the controller. A server-side re-apply
keeps their data, so no created-once step is needed.

**Upgrade.** Under GitOps, keep `sm-admin/` and `sm-owner/` in the sync, and
`sm-steps/` out, since a sync would bring the temporary rights back. Run the new release's
`sm-steps/preflight` by hand, then merge the path bump. Argo CD must track
resources by annotation: label tracking rewrites `app.kubernetes.io/instance`,
which `smt` and the migration rely on.

**Uninstall.**

1. Delete the ScaledObjects.
2. `kubectl delete -k sm-owner/`, then `kubectl delete -k sm-admin/`, then
   each `sm-steps/` directory still there.
3. Run the same leftovers check as the main path.

The scaler CA, Secret `scaling-manager-scaler-ca`, is kept for a
reinstall. To delete it: `kubectl delete secret -n $CTRL -l
scaling-manager.llmd.ai/scaler-tls`.

#### A DisaggregatedSet, step by step

On the main path, as built:

```bash
NS=team-a SET=<your DisaggregatedSet>
kubectl get disaggregatedset $SET -n $NS -o jsonpath='{.spec.roles[*].scaling.mode}{"\n"}'
```

- **External roles** are scaled through their `DisaggregatedSetRoleScaler`
  (`<set>-<role>`). Write one ScaledObject per role by hand, with the same
  scaler address and ClusterTriggerAuthentication as the plan's: the plan
  writer covers Deployments only.
- **Static roles** have no `/scale`, so the role writer reads and writes
  them. Three steps:
  1. The namespace owner binds the role writer's grant (`get`, `list`,
     `patch` on `disaggregatedsets`) in the namespace:

     ```bash
     bash scaling-manager-owner.sh grant dset-role-writer $NS
     ```

  2. The cluster admin turns the writer on (a restart key; the controller
     restarts itself):

     ```bash
     bash scaling-manager-admin.sh set dsetRoleWriter=true
     ```

  3. The owner opts the set in:

     ```bash
     kubectl annotate disaggregatedset $SET -n $NS \
       scaling-manager.llmd.ai/role-writer=true \
       scaling-manager.llmd.ai/model-id=<the model it serves> \
       scaling-manager.llmd.ai/min-replicas=1 \
       scaling-manager.llmd.ai/max-replicas=<max groups per role>
     ```

  Use `force` instead of `true` if the set was created with `kubectl
  create`, plain `kubectl apply` or Helm
  ([ownership](../reference/disaggregatedset.md#ownership-of-replicas)).
  While the model is passive the writer writes nothing. The writer scans
  only the managed namespaces that hold its grant.

#### The advanced path's internals

##### The Jobs

| Job | runs | where | rights |
| --- | --- | --- | --- |
| preflight | `/manager preflight check` (exists) | controller namespace | its Role and ClusterRole (exist), plus the conflict checks' reads (new), in `steps/preflight` |
| keda-auth | new `/manager keda-auth apply` | **KEDA's namespace** | see below |

Each Job is named with its version, then the operator's suffix
(`scaling-manager-preflight-v0-13-0-team-a`). `ttlSecondsAfterFinished`
stays; a re-run of the same release deletes the Job first. Argo CD and Flux
must not own the Jobs: they would re-create one after its TTL. There is no
Argo CD hook component: a hook would put the preflight's cluster-wide reads
back into the sync.

##### The preflight conflict checks

`smt admin-setup` refuses a model namespace another scaling manager
manages, and a namespaced install beside a cluster-wide one. On this path
the preflight does it, before the admin apply. The check exists since #409,
as `conflicts` (`internal/preflight/install_checks.go`), reading
RoleBindings and ClusterRoleBindings; what is left is running it as a Job
with these rights:

- `list` on `rolebindings` in each listed model namespace, to find another
  controller's RoleBinding there;
- `list` on `clusterrolebindings`, to find a cluster-wide install's. This
  shows the cluster's RBAC layout to the owner of `$CTRL`, who can run pods
  as the preflight's ServiceAccount, for as long as `sm-steps/preflight`
  exists. It is read-only and temporary;
- never Deployments. A Deployment's env holds that tenant's settings, and
  the owner of `$CTRL` can run pods as the preflight's ServiceAccount.

A dropped namespace on a re-run shows in `kubectl diff -k sm-admin/`.

##### The KEDA certificate step, and why it runs in KEDA's namespace

KEDA presents its client certificate through a ClusterTriggerAuthentication,
which reads Secrets only in KEDA's own namespace. The copy never goes to a
model namespace or any namespace a tenant owns: whoever can read it can pose
as KEDA (`deploy/lib/scaler_tls.sh`, the KEDA section, and the
[security model](../reference/security-model.md)). On an `smt` install the
admin makes the copy with their own kubeconfig; on the main path the
controller writes it, by name, into a Secret the manifest pre-creates.

The Job must not run in the controller namespace. There the owner holds
`admin`: they can run a pod as the Job's ServiceAccount or create a token for
it, and so use any right it has.

- **`keda-auth-setup` (admin, steady state, `sm-admin/keda/`)**
  pre-creates the target Secret `scaling-manager-external-scaler-<ctrl>` in
  KEDA's namespace, and the ClusterTriggerAuthentication that names it.
  - The Secret is `Opaque` with no `data`, so a later apply or sync never
    empties what the Job wrote.
  - The ClusterTriggerAuthentication base carries no `metadata.namespace`.
- **The marker** is in `controller-admin`, also with no `data`. The Job
  fills it. The owner can edit it, so like the stamp it is a convenience,
  not a control. A marker set early only breaks the owner's own install.
- **`steps/keda-auth` (admin, temporary)** runs the Job in KEDA's
  namespace, as a ServiceAccount there. Its rights, all by `resourceNames`:
  - `get` on Secret `scaling-manager-scaler-client` and on
    ConfigMap `scaling-manager-controller-status`, in the controller
    namespace;
  - `get` and `update` on the pre-created Secret in KEDA's namespace;
  - `get` and `update` on the marker.

  No `create`, no `pods/log`, nothing cluster-wide, no right the owner can
  reach.
- The Job waits for the controller to fill the client Secret, copies it, and
  sets the marker's `ready` key. The controller watches the marker and, when
  `ready` changes, re-reads the client CA and serves mutual TLS without a
  restart. The live switch exists since #409 (`--external-scaler-tls-live`);
  reading the marker for it is the new part. The Job reads `scalerTLS`
  until `mutual TLS`.
- **KEDA's connection.** `smt admin-finish` judges KEDA's connection from
  the controller's log and KEDA's pods (`deploy/lib/scaler_tls.sh`), and
  `pods/log` cannot be limited by name. Instead the Job reads the verdict
  the controller publishes since #409: `kedaConnection` (`none`, `current`
  or `stale`) and `kedaCopyStale` (KEDA's copy predates the current client
  certificate).

**Rotation.** KEDA keeps one connection per scaler address until it
restarts. When the CA changes, the Job reports `kedaConnection: stale` and
prints the restart of KEDA's operator; it never restarts KEDA, which is
shared. Re-run the Job after a CA change.

A namespaced TriggerAuthentication is not used, on either path: it would
put KEDA's client certificate in a model namespace
([Alternatives](#alternatives)). If it is ever revisited, it needs a
per-namespace client identity, a server-side namespace check, and KEDA 2.21
or later.

##### The scaler TLS Secrets

The `smt` release files ship them as type `kubernetes.io/tls` with empty
keys. That type requires the key fields, so an apply of that file empties
the certificates the controller wrote; that is why it is created once.
The one manifest instead ships them as `Opaque` with no `data`, under new
names, `scaling-manager-scaler-{ca,serving,client}` (built in #409,
`--external-scaler-tls-secrets=opaque`); the advanced path would do the
same. An `Opaque` Secret with no `data` field keeps what another manager
wrote across a server-side re-apply. So the Secrets sit in the applied
files, and the controller keeps `get` and `update` by name only. It never
needs `create` on Secrets. The type of the existing Secrets cannot change,
hence the new names; on first start the controller copies the old CA
across.

### B. What was built, and the phases

Phase 1 shipped in #409 (merged 2026-11-24, released in v0.13.0). It built
more of the controller than the first phase plan named, and less of the
kustomize work. This appendix was re-checked against `main` at `190a1da6`.

What was built: the release files and scripts (`deploy/release-manifests.sh`,
`deploy/kubectl/scaling-manager-{admin,owner}.sh`, signed beside the
manifests); `watchNamespaces` live (`internal/config/settings_loader.go`,
`internal/managedns`); the scaler's refusal (`internal/scaler/admit.go`);
the in-controller preflight with its new checks (`internal/preflight/monitor.go`,
`internal/preflight/install_checks.go`); the status keys
(`internal/controllerstatus/publisher.go`); its own TLS and KEDA's copy
(`internal/scalertls/{ensure,kedacopy,setup}.go`); the plan writer
(`internal/scaleplan`); the guide (`docs/guides/kubectl-install/`); the
kind e2e (`make test-e2e-manifest-install`, `make test-e2e-manifest-migrate`).

Each item of the original plan, and where it stands:

| item | status | evidence on `main` |
| --- | --- | --- |
| settings keys `kedaNamespace`, `caFromSecret`, `dsetRoleWriter` | done | `internal/config/settings.go`; [configuration](../reference/configuration.md) |
| settings key `namespaceSelector` | dropped, by decision | `watchNamespaces` instead; the old key maps to it as an unknown-key hint (`internal/config/settings.go`, `UnknownSettingHint`) |
| a changed restart key restarts the controller | done | `--restart-on-settings-change=true` in `deploy/release-manifests.sh`; `RestartOnSettingsChange` in `internal/controller/settings.go` |
| status keys `ready`, one per check | done | `KeyReady`, `KeyPreflightPrefix` (`preflight.<check>`) in `internal/controllerstatus/publisher.go` |
| status keys for the managed namespaces | done, and more | `namespacesManaged`, `namespacesRefused`, and `namespace.<name>` per namespace (`internal/controllerstatus/publisher.go`, `internal/managedns/reporter.go`) |
| `kedaConnection`, `kedaCopyStale` | done | `internal/controllerstatus/publisher.go`; set from `internal/scalertls/bootstrap.go` and `internal/preflight/scaler.go` |
| `metricsScraped` | not built as a key | the same question is answered by the preflight checks `metrics.scrape` (a double scrape) and `metrics.model` (a missing one) |
| an Event for each change of a check | done | `recordChanges` in `internal/preflight/monitor.go` |
| the preflight inside the controller, every cycle | done | `Monitor` in `internal/preflight/monitor.go`, `--self-check-interval=1m` |
| check: conflicts, from RoleBindings and ClusterRoleBindings, never Deployments | partly | `conflicts` (`checkConflicts`, `internal/preflight/install_checks.go`). It reports an overlapping install; it does not refuse it or stop managing what overlaps |
| check: which policy wins | done | `policy.source` (`checkPolicySource`) |
| check: a double scrape, a missing EPP scrape, the EPP metric names | done | `metrics.scrape` (`checkScrape`), `metrics.epp` (`checkEPPMetrics`: `llm_d_epp_flow_control_queue_size` and its old name) |
| the selector, and the scaler refusing an unadmitted namespace | done, as `watchNamespaces` | `managedns.Admission`; `PermissionDenied` before registration, and an open stream ended (`internal/scaler/admit.go`); the InferencePool reconciler drops a left-out namespace (`internal/controller/inferencepool_reconciler.go`) |
| the controller refusing an unadmitted namespace's workloads | partly | the scaler, the registry (and so the engines and warm pool), the InferencePools, the plan and the role writer follow `watchNamespaces`; the KEDA HPA repair and the namespace-local policy path still act wherever their own opt-in is |
| the label policy (`scaling-manager-label-guard.yaml`) | dropped, by decision | no label to guard; `hack/check-manifest-install.sh` fails if the file is written |
| its own TLS, without `create` on Secrets | done | `config/release/scaler-tls-opaque-{secret,role,rolebinding}.yaml`; `--external-scaler-tls-secrets=opaque`; the `smt` CA copied once (`internal/scalertls/migrate_test.go`) |
| KEDA's copy, by name, in KEDA's namespace | done | `config/release/keda-copy-{secret,role,rolebinding}.yaml` in `scaling-manager-keda.yaml`; `internal/scalertls/kedacopy.go` |
| mutual TLS and a new CA without a restart | done | `--external-scaler-tls-live=true`; `SetupOptions.Live` in `internal/scalertls/setup.go` |
| on a CA change, name KEDA's restart, never do it | done | `kedaConnection: stale` and `scalerTLSAction` |
| the plan writer | partly | `internal/scaleplan/plan.go`: Deployments whose pods carry `llm-d.ai/role` `decode` or `prefill`. Not LeaderWorkerSets or DisaggregatedSets, no adoption of an existing ScaledObject |
| the unwired-HPA report, for KEDA before 2.20 | partly | not in the plan writer. `keda.version` names KEDA versions with the bug (`internal/preflight/keda.go`), and `internal/hparepair` re-wires such an HPA |
| the release files, in `SHA256SUMS` and signed | done | `RELEASE_FILES` and `RELEASE_SCRIPTS` in `deploy/release-manifests.sh`; `.github/workflows/ci-release.yaml` |
| `scaling-manager-monitoring.yaml` | partly | the model-server PodMonitor and the EPP NetworkPolicy rule. No EPP ServiceMonitor: the controller reads each EPP's `/metrics` itself ([D](#d-monitoring)) |
| passive by default | done | `LLMSM_PASSIVE=true` on the Deployment (`deploy/release-manifests.sh`); `hack/check-manifest-install.sh` `not-passive` |
| the guide | done | `docs/guides/kubectl-install/` |
| the kind e2e | partly | `make test-e2e-manifest-install` and `make test-e2e-manifest-migrate` pass on kind, run locally. Not in CI. No upgrade leg from the previous release, no Argo CD or Flux run, no OpenShift |
| `config/install`, replacing `config/overlays` | open | no `config/install`; the manifest is rendered by `deploy/release-manifests.sh` from `config/release` and `config/base` |
| the kustomize tarball | open | the release has the tools tarball only (`scaling-manager-tools-<release>.tar.gz`) |
| the render-parity check | open | no `hack/check-kustomize-install.sh` |
| narrow the cluster-wide `configmaps` read | open | `configmaps` get, list, watch cluster-wide in `config/base/rbac/manager-clusterrole.yaml` |
| the advanced path: the Jobs, the steps, their e2e | open | no `keda-auth` subcommand in `cmd/main.go`, no `sm-admin`, `sm-owner` or `sm-steps` bases |
| `epp-monitoring` named per EPP | open | only on the advanced path now; the main path has no EPP ServiceMonitor |
| the plugin, a krew manifest, release binaries | open | no plugin under `cmd/`, none in the release |
| the README leads with the manifest | done | `README.md`, Install: "With kubectl, from one manifest" first |

| `smt` does | main path, as built | advanced path |
| --- | --- | --- |
| the managed namespaces | `watchNamespaces`, live (`scaling-manager-admin.sh namespaces`) | `LLMSM_WATCH_NAMESPACE` patch, the Namespace reader's list |
| the preflight | status keys, every minute | the preflight Job |
| refuses an overlapping install | the `conflicts` check reports it; it does not refuse | the conflict check in run 1 |
| KEDA's client certificate | the controller, one Secret by name in KEDA's namespace | the `keda-auth` Job, in KEDA's namespace |
| `--prometheus-*`, `--allow-http`, `--quota`, `--set` | settings keys, written by `install` and `set`; the CA in the fixed Secret | the same |
| `--utilization-share`, `passive`, `--dset-role-writer` | settings keys, never in git; grants bound by the owner | the same |
| PodMonitor, `--modelserver-port`, the EPP monitor | llm-d's components or `scaling-manager-monitoring.yaml`; the controller reads the EPP itself ([D](#d-monitoring)) | the monitoring components |
| `scaledobjects-plan` / `-apply`, the HPA re-wiring | the controller's plan, applied by the owner; the HPA repair | the same |
| the weights labeller's watch list | stays in `smt` | the same |

`smt` keeps every command, on the same release files.

| phase | what was planned | status |
| --- | --- | --- |
| 1 | `config/install`, the manifest and the tarball rendered from it, the parity check; the new settings keys; narrowing the `configmaps` read | **done in #409 (v0.13.0)** for the manifest, the KEDA, grants and monitoring files, the two scripts, signing and the settings keys, plus most of phase 2 below. Not built: `config/install`, the tarball, the parity check, the `configmaps` narrowing |
| 2 | the controller: the preflight as status keys with the new checks; the selector and the scaler refusing unadmitted namespaces; the label policy; its own TLS without `create`; KEDA's copy and the CA re-read; the plan writer; the kind e2e; the guide | **mostly done in #409.** The selector became `watchNamespaces` and the label policy was dropped. Left: the plan writer for LeaderWorkerSets and DisaggregatedSets and adoption; refusing an overlapping install; `watchNamespaces` for the HPA repair and the namespace-local policy; the e2e in CI, with an upgrade leg and GitOps |
| 3 | the advanced path: the Jobs, the steps, their e2e; `epp-monitoring` named per EPP | open |
| 4 | the plugin, krew manifest, release binaries; the README leads with the manifest | open, except the README, which leads with the manifest |

### C. RBAC in full

`watchNamespaces` is a setting, not an RBAC scope, so the controller reads
cluster-wide and acts only in the managed namespaces. The ClusterRole for
passive mode is the controller's `manager-role` of today
([config/base/rbac/manager-clusterrole.yaml](../../config/base/rbac/manager-clusterrole.yaml)):

| API group | resources | verbs |
| --- | --- | --- |
| core | `configmaps`, `namespaces`, `nodes`, `nodes/status`, `pods`, `services` | get, list, watch |
| core | `configmaps/status` | get |
| core | `events` | create, get, list, patch |
| core | `resourcequotas` | get, list |
| apps | `deployments`, `replicasets`, `statefulsets` | get, list, watch |
| apps | `deployments/scale` | get |
| batch | `jobs` | get |
| coordination.k8s.io | `leases` named `c1f6bfd2.kueue.x-k8s.io` | get |
| disaggregatedset.x-k8s.io | `disaggregatedsets`, `disaggregatedsetrolescalers`, `.../scale` | get |
| inference.networking.k8s.io, inference.networking.x-k8s.io | `inferencepools` | get, list, watch |
| keda.sh | `scaledobjects` | get |
| kueue.x-k8s.io | `clusterqueues`, `cohorts`, `localqueues`, `resourceflavors` | get, list |
| kueue.x-k8s.io | `workloads` | get, list, watch |
| leaderworkerset.x-k8s.io | `leaderworkersets` | get, list, watch |
| leaderworkerset.x-k8s.io | `leaderworkersets/scale` | get |
| monitoring.coreos.com | `servicemonitors` | get, list, watch |

What the one manifest adds, as built
([security model](../reference/security-model.md#the-one-manifest-install)):

- **Scaler certificates:** a Role in its namespace, `get` and `update` on the
  three `Opaque` Secrets `scaling-manager-scaler-{ca,serving,client}` by
  name, and `get` on the earlier install's Secrets, to copy their CA once.
  No `create` on Secrets anywhere.
- **KEDA's copy:** a Role in KEDA's namespace, from
  `scaling-manager-keda.yaml`: `get` and `update` on that one Secret by name.
- **The suggested plan:** a Role in its namespace, `get` and `update` on
  ConfigMap `scaling-manager-scaledobjects-plan` by name.
- **The self-check:** a read-only ClusterRole and Role for the in-controller
  preflight, including `list` on RoleBindings and ClusterRoleBindings for the
  conflict check, and so the role writer finds the namespaces that granted
  it. This shows the cluster's RBAC layout to whoever can act as the
  controller, which on this install is the cluster admin alone.
- **Per namespace, bound by the namespace owner:** `pod-writer` (`patch` on
  pods) and `dset-role-writer` (`get`, `list`, `patch` on DisaggregatedSets),
  from `scaling-manager-grants.yaml`. Never cluster-wide.
- **EPP token reviews:** ClusterRole `scaling-manager-epp-tokenreview-*`
  (`create` on tokenreviews and subjectaccessreviews), bound by the cluster
  admin to an EPP's own ServiceAccount, never to the controller's.
- The CA for Prometheus has a fixed Secret name,
  `scaling-manager-prometheus-ca`, read when `caFromSecret` is `true`, so no
  unscoped Secret read is needed.

Its own namespace's Roles (leader election, status, learned state) are as
today. It writes nothing outside its namespace except Events, KEDA's copy,
and what the two per-namespace grants allow once the owner binds them.

| | [cluster-wide `smt` install](../guides/install-cluster-wide/) | this install | namespaced `smt` install |
| --- | --- | --- | --- |
| reads | cluster-wide, the table above | the same | per namespace, plus the cluster-scoped readers |
| pod `patch` | per namespace, at activation | per namespace, bound by the owner | per namespace, at activation |
| KEDA's copy | the admin's kubeconfig, once | the controller, one Secret by name | the admin's kubeconfig, once |
| who installs | cluster admin | cluster admin; the owner registers | admin, then the namespace owner |

So this install holds **the same reads** as the cluster-wide `smt` install,
plus one named Secret in KEDA's namespace instead of the admin copying it.
It holds **more** than a namespaced install. The `configmaps` read across
the cluster is the widest of them. It is there for each namespace's own
policy map; narrowing it to that name is still open. Where least privilege
per namespace is the rule, use a namespaced `smt` install, or the advanced
path once it is built.

### D. Monitoring

Each scrape must exist once: a second doubles every rate, and a missing one
leaves the analyzers blind. The preflight checks `metrics.scrape` (a pod
scraped by more than one job) and `metrics.model` (no model-server series)
report both.

| source | where llm-d already scrapes it | where nothing scrapes it |
| --- | --- | --- |
| model servers | llm-d's kustomize components [`monitoring`](https://github.com/llm-d/llm-d/tree/main/guides/recipes/modelserver/components/monitoring) (decode: selects `llm-d.ai/role: decode`, port `modelserver`, `/metrics`, every 30s), `monitoring-pd` (P/D, which the pd-disaggregation guide applies) and `monitoring-epd`. Nothing to add | `scaling-manager-owner.sh monitor <ns>`: the PodMonitor on port `modelserver` from `scaling-manager-monitoring.yaml` |
| the EPP, for the controller | the controller reads each managed InferencePool's EPP `/metrics` itself, with its own token. The EPP reviews that token; where its router chart did not grant that, the admin runs `scaling-manager-admin.sh grant-epp <ns> <the EPP's ServiceAccount>` | the same |
| the EPP, in Prometheus | the router installed with llm-d's [`monitoring.values.yaml`](https://raw.githubusercontent.com/llm-d/llm-d/main/guides/recipes/router/features/monitoring.values.yaml): its own ServiceMonitor. The `metrics.epp` check looks for the EPP's flow-control queue metric there | not in `scaling-manager-monitoring.yaml` yet. Use the router's monitoring values |

Where a default-deny ingress policy is in place, the owner also runs
`scaling-manager-owner.sh network <ns>`: a NetworkPolicy rule letting the
controller reach port 9090. Router charts label their EPP pods differently,
so the rule selects every pod of the namespace on that port; in a namespace
with no policy it would cut every other ingress, so apply it only where a
default deny exists.

A metrics port not named `modelserver` (what `smt`'s `--modelserver-port`
did) is a patch of the PodMonitor after `monitor`; the next apply of the
file undoes it, so re-run the patch, or keep the PodMonitor in your own
kustomization.

Whether llm-d's `monitoring-pd` selects a DisaggregatedSet's pods was not
confirmed. `metrics.model` reports model servers Prometheus holds no
`vllm:` series for.

On GKE with Managed Prometheus, llm-d uses `PodMonitoring`. Our monitors are
Prometheus Operator objects, so scrape with GMP instead.

### E. GitOps

- **Keep in git:** the `kubectl` commands the scripts print with `--print`,
  over the release files as downloaded, and the settings without their
  switch keys. Both scripts print the same commands they run, so Argo CD or
  Flux applies the same files.
- **Keep out of git:** the switch keys (`passive`, `utilizationShare`,
  `dsetRoleWriter`) and the per-namespace grants. On the advanced path,
  also `sm-steps/`.

```bash
bash scaling-manager-admin.sh --print install --prometheus-url https://prometheus.monitoring.svc:9090 \
  --keda-namespace keda --watch-namespaces team-a,team-b
```

Under plain `kubectl apply --server-side`, a key the file leaves out keeps
what another manager set. A key the file sets, and someone else changed,
makes the apply stop with a conflict. Argo CD with `ServerSideApply=true`
and Flux both force ownership instead. A switch key left in git therefore
silently resets a live change on every sync. Applying client-side, Argo CD
can ignore the keys:

```yaml
# Argo CD Application
spec:
  syncPolicy:
    syncOptions:
    - ServerSideApply=true
    - RespectIgnoreDifferences=true
  ignoreDifferences:
  - kind: ConfigMap
    name: scaling-manager-settings
    namespace: scaling-manager-system
    jsonPointers:
    - /data/passive
    - /data/utilizationShare
    - /data/dsetRoleWriter
```

Flux needs nothing more once the keys are out of git. To leave the whole
ConfigMap alone after it is created, annotate it
`kustomize.toolkit.fluxcd.io/ssa: IfNotPresent`.

`watchNamespaces` is live, so a sync that changes it takes effect without a
restart. Argo CD must track resources by annotation, because label tracking
rewrites `app.kubernetes.io/instance`, which `smt`, the uninstall and the
migration rely on. Nobody runs `rollout restart` under auto-sync, and none is
needed: a changed restart key makes the controller restart itself, and
records an Event saying so. Changed again and again, that puts the Pod into
the kubelet's crash back-off, which grows to five minutes.

Not yet tested: neither Argo CD nor Flux has run this install.

### F. Upgrade, rollback, air-gapped, uninstall

**Upgrade:** download and verify the new release's files, then
`bash scaling-manager-admin.sh upgrade --keda-namespace keda`. The settings
and the ScaledObjects are not in the files, so they stay. The scaler's
certificate Secrets and KEDA's copy carry no data in the files, so a
re-apply keeps what the controller wrote, and KEDA stays connected. A key
the new release adds takes its default. Under GitOps, merge the version
bump.

**Rollback:** the same command with an older release's files that carry
`scaling-manager.yaml` (v0.13.0 or later). The settings are a ConfigMap, so
no schema prunes a newer key. A key the older controller does not know is
listed in the status as unknown, and ignored; remove it.

**KEDA elsewhere.** The `kedaNamespace` setting names KEDA's namespace;
`install` and `upgrade` take `--keda-namespace` and apply
`scaling-manager-keda.yaml` there with `-n`. The scaler's NetworkPolicy
admits the namespace labelled `scaling-manager.llmd.ai/keda=true`, which
`install` sets. No file is edited. On OpenShift (`openshift-keda`) the
install path is not tested yet.

**Air-gapped:** mirror the image by digest (`skopeo copy --all`), verify it
with `cosign verify` first. The controller is the only image. The files
name the release's image by digest, so either the cluster's registry mirror
serves that reference, or a kustomization over the downloaded
`scaling-manager.yaml` adds one `images:` entry. Neither is tested yet.

**Uninstall:**

```bash
bash scaling-manager-admin.sh uninstall --keda-namespace keda
kubectl get clusterrole,clusterrolebinding,clustertriggerauthentication -l app.kubernetes.io/instance=scaling-manager-system   # nothing left
```

`uninstall` sets sharing to shadow first, so no transfer mark stays on a
pod, then removes everything this install made, in every namespace, and what
a moved `smt` install left, by the same instance label. The settings go with
the namespace. The ScaledObjects an owner registered from their own file are
not labelled by this install and stay: each owner deletes theirs first.

### G. Migration from `smt`

**From a cluster-wide install** (`scaling-manager-system`):

```bash
bash scaling-manager-admin.sh move --keda-namespace keda
bash scaling-manager-admin.sh namespaces team-a,team-b      # the namespaces it named with --model-namespace
```

- The settings ConfigMap is the same object and stays. Take its switch keys
  out of any copy kept in git.
- `move` takes the controller Deployment over from `smt` once
  (`--force-conflicts`). The new TLS Secrets take the old CA, so KEDA stays
  connected with no restart of its operator, and KEDA's copy keeps its name
  (`make test-e2e-manifest-migrate`).
- `move` deletes the `smt` install's cluster-wide DisaggregatedSet role
  writer grant; each owner binds it per namespace. What else `smt` made stays
  until `uninstall`.

A cluster-wide install from `scaling-manager-cluster.yaml` has the same
objects and moves the same way; no e2e covers it yet.

**From a namespaced install:** not moved in place. Its scaler runs in that
namespace, so its ScaledObjects name another address and
ClusterTriggerAuthentication. Uninstall it as its guide says, install as
above with the namespace in `watchNamespaces`, and register the ScaledObjects
again with `scaling-manager-owner.sh register`. `move` does not detect a
namespaced install in `scaling-manager-system`. Staying namespaced on the
advanced path, once built, is the other option.

An install made with `make deploy-scaling-manager` is not adopted, as for
`smt` today.

### H. The llm-d recipe path

Not built; it waits on `config/install` (or a signed tarball of the bases)
and on the git-ref question.

llm-d's CI dry-run builds in-repo directories, so an llm-d recipe cannot
point at a downloaded file. A recipe, `guides/recipes/scaling-manager/`,
would carry the one-manifest path the way llm-d's autoscaler recipe carries
its controller:

```yaml
# guides/recipes/scaling-manager/kustomization.yaml (in llm-d)
resources:
- github.com/llm-d-incubation/llm-d-scaling-manager//config/install/cluster?ref=<release commit>
images:
- name: ghcr.io/llm-d-incubation/llm-d-scaling-manager
  digest: sha256:<the release's digest>
```

- **Version:** `SCALING_MANAGER_VERSION` in llm-d's `guides/env.sh` feeds
  both the ref and the digest, as `ROUTER_CHART_VERSION` feeds the router.
  A scratch render confirmed that the `images:` digest entry replaces the
  base's tag with `@sha256:...`.
- **Rendering:** the recipe renders the same objects as the release's
  `scaling-manager.yaml` and `scaling-manager-keda.yaml`, and is applied
  with `kubectl apply --server-side -k`.
- **Platforms:** KEDA's namespace is the `kedaNamespace` setting, so the
  `k8s/` and `ocp/` overlays set that key (`keda`, or `openshift-keda`) and
  the namespace of KEDA's side, as the keda-epp guide does.
- **A cluster prerequisite, not a per-guide install.** Like KEDA, it is
  installed once per cluster. A guide that uses it adds only its namespace
  to `watchNamespaces` and its ScaledObjects. Its cleanup removes only
  those two, as the guides policy asks of shared components.

### I. How the router and the neighbours install

#### The router

The router (the endpoint picker, EPP) installs with **Helm**, from llm-d's
own OCI charts. Its CRDs are a separate `kubectl apply` of release YAML.

| question | what the router does | source |
| --- | --- | --- |
| mechanism | Helm. `oci://ghcr.io/llm-d/charts/llm-d-router-standalone` (EPP with an Envoy or agentgateway proxy) and `oci://ghcr.io/llm-d/charts/llm-d-router-gateway` (EPP, InferencePool, optionally an HTTPRoute), on a library chart `routerlib` | [charts README](https://github.com/llm-d/llm-d-router/blob/main/config/charts/README.md) |
| publishing | the release workflow pushes both charts and stamps the version | [ci-build-images.yaml](https://github.com/llm-d/llm-d-router/blob/main/.github/workflows/ci-build-images.yaml) |
| config | values under `router.epp.*`: `pluginsConfig` (an EndpointPickerConfig, rendered into a ConfigMap; it replaces the built-in one), `pluginsCustomConfig`, `pluginsConfigFile`, `flags` | [routerlib values.yaml](https://raw.githubusercontent.com/llm-d/llm-d-router/main/config/charts/routerlib/values.yaml) |
| layered values | the llm-d guides pass a base values file, then monitoring, then a per-guide file | [base.values.yaml](https://raw.githubusercontent.com/llm-d/llm-d/main/guides/recipes/router/base.values.yaml) |
| RBAC | namespaced Roles for pods, InferencePool, InferenceObjective, leases and events. A ClusterRole for TokenReview and SubjectAccessReview while metrics auth is on (the default), and for `/metrics` when Prometheus monitoring with the Prometheus Operator is enabled. `router.monitoring.prometheus.auth.enabled=false` gives a namespace-only install | [charts README](https://github.com/llm-d/llm-d-router/blob/main/config/charts/README.md) |
| CRDs | not in the chart. The router's development guide applies the Gateway API Inference Extension `manifests.yaml`; the llm-d guides apply its `v1-manifests.yaml` (InferencePool). The router release's own `manifests.yaml` (InferenceObjective, InferenceModelRewrite) is applied only by the guides that need it, such as flow control | [DEVELOPMENT.md](https://github.com/llm-d/llm-d-router/blob/main/DEVELOPMENT.md), [release v1.0.0](https://github.com/llm-d/llm-d-router/releases/tag/v1.0.0), [flow-control guide](https://github.com/llm-d/llm-d/blob/main/guides/flow-control/README.md) |
| upgrade | `helm upgrade`. The chart README says to gate on `kubectl wait` rather than `--wait`. CRDs: re-apply the release YAML | [charts README](https://github.com/llm-d/llm-d-router/blob/main/config/charts/README.md) |
| versions | `guides/env.sh` sets `ROUTER_CHART_VERSION=v0` (a floating major tag), `GAIE_VERSION=latest`, `LWS_VERSION=1.0.0` | [env.sh](https://raw.githubusercontent.com/llm-d/llm-d/main/guides/env.sh) |

The guide's commands
([optimized-baseline](https://github.com/llm-d/llm-d/blob/main/guides/optimized-baseline/README.md)):

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/${GAIE_URL}/v1-manifests.yaml
kubectl create namespace ${NAMESPACE} --dry-run=client -o yaml | kubectl apply -f -
helm install ${GUIDE_NAME} oci://ghcr.io/llm-d/charts/llm-d-router-standalone \
  -f ${ROUTER_BASE_VALUES} ${MONITORING_VALUES} -f ${ROUTER_VALUES} \
  -n ${NAMESPACE} --version ${ROUTER_CHART_VERSION}
kubectl apply -n ${NAMESPACE} -k ${REPO_ROOT}/guides/${GUIDE_NAME}/modelserver/...   # the model servers: kustomize
helm uninstall ${GUIDE_NAME} -n ${NAMESPACE}                                           # cleanup
```

#### An autoscaler, and the model servers

The same guides install everything else with kustomize.

| piece | how the llm-d guides install it | source |
| --- | --- | --- |
| an autoscaler controller | a kustomize overlay in the guides repository over a **remote git-ref base** in the controller's repository (`github.com/llm-d/llm-d-workload-variant-autoscaler/config/overlays/namespace-scoped/kubernetes?ref=release-0.9`), with `namespace:`, a Deployment patch and an `images:` pin | [wva/controller/platform/k8s/kustomization.yaml](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/wva/controller/platform/k8s/kustomization.yaml) |
| ScaledObjects | checked-in overlays, each with a full `scaledobject.yaml` (KEDA's Prometheus trigger), applied with `kubectl kustomize <overlay> \| envsubst ... \| kubectl apply -f -` | [keda-epp README](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/keda-epp/README.md) |
| model-server monitoring | kustomize components with PodMonitors: `recipes/modelserver/components/monitoring` (decode, `decode-podmonitor.yaml`), `monitoring-pd` (P/D) and `monitoring-epd` | [the component](https://github.com/llm-d/llm-d/tree/main/guides/recipes/modelserver/components/monitoring) |
| guide CI | a kustomize dry-run job covers every overlay of a tier-2 guide | [GUIDES-POLICY.md](https://github.com/llm-d/llm-d/blob/main/guides/GUIDES-POLICY.md) |

So llm-d is Helm for the router and kustomize for the rest, with a remote
git-ref base already the precedent for a controller.

#### KEDA, Kueue, LeaderWorkerSet, cert-manager

The controllers an llm-d cluster already runs each document one release
manifest, applied by the cluster admin and removed with `kubectl delete -f`.
The llm-d guides themselves often take the same project's Helm chart
instead.

| project | its documented manifest install | uninstall | what the llm-d guides use | source |
| --- | --- | --- | --- | --- |
| KEDA | `kubectl apply --server-side -f https://github.com/kedacore/keda/releases/download/v2.18.3/keda-2.18.3.yaml` | `kubectl delete -f` of the same URL | an install step in the workload-autoscaling README, or the platform's operator (`openshift-keda`); which form was not confirmed | [KEDA deploy](https://keda.sh/docs/2.18/deploy/), [keda-epp](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/keda-epp/README.md) |
| Kueue | `kubectl apply --server-side -f https://github.com/kubernetes-sigs/kueue/releases/download/v0.20.7/manifests.yaml` | `kubectl delete -f` of the same URL, after deleting Kueue objects | Helm: `helm install kueue oci://registry.k8s.io/kueue/charts/kueue` | [Kueue installation](https://kueue.sigs.k8s.io/docs/getting-started/installation/), [kueue-rebalancing](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/kueue-rebalancing/README.md) |
| LeaderWorkerSet | `kubectl apply --server-side -f https://github.com/kubernetes-sigs/lws/releases/download/$VERSION/manifests.yaml` | `kubectl delete -f` of the same URL | Helm: `helm upgrade --install lws oci://registry.k8s.io/lws/charts/lws` | [LWS installation](https://lws.sigs.k8s.io/docs/installation/), [coord-disaggregation](https://github.com/llm-d/llm-d/blob/main/guides/coord-disaggregation/README.md) |
| cert-manager | `kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.3/cert-manager.yaml` | `kubectl delete -f` of the same URL | | [cert-manager kubectl install](https://cert-manager.io/docs/installation/kubectl/) |

Each manifest carries the namespace, the CRDs, the RBAC and the
controller. Each controller is then configured through its own objects,
not through the install. Kueue and LeaderWorkerSet ship both forms: the
manifest and a thin chart from the same release.

#### What makes these installs feel native

1. One command per piece, run with the operator's own kubeconfig.
2. Config is data: a values file or a kustomization in the operator's repo.
3. Upgrade is the same command with a new version.
4. Uninstall is one command.

None of these has an admin/owner split or a preflight. The main path here
keeps the preflight inside the controller; the advanced path keeps both.

llm-d's decode PodMonitor, read in full, selects `llm-d.ai/role: decode`
and scrapes port `modelserver` at `/metrics` every 30s; `monitoring-pd`
and `monitoring-epd` sit beside it. Not confirmed: which version the `v0`
chart tag resolves to, and the full router release notes. The GitHub REST API refused unauthenticated requests;
the pages, raw files and registry tag lists were read instead.

#### How we installed before v0.13.0

This was the starting point, and `smt` still works this way. Three `smt` commands, from the tools image
([README](../../README.md#install), [install commands](../reference/tools-install.md)).
`smt` is a shell function around `docker run` of
`ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools@sha256:...`. The
logic is `deploy/lib/release_install.sh`, about 2,100 lines of bash over
`kubectl`, `jq` and `yq`.

| step | who | creates or changes | checks |
| --- | --- | --- | --- |
| `smt admin-setup -n CTRL [--model-namespace M ...]` | cluster admin | the `part=admin` objects of the namespaced release file: the ServiceAccount, the per-install ClusterRoles and bindings (node, Kueue and Namespace readers, metrics auth), the Roles, the controller ServiceMonitor, the scaler NetworkPolicy naming KEDA's namespace. The preflight's RBAC. In each model namespace: the controller's Role and RoleBinding and the model-server PodMonitor. An EPP ServiceMonitor with its ServiceAccount, token Secret and `/metrics` binding, where nothing scrapes the EPP. With `--dset-role-writer`, the `disaggregatedsets` grant. A stamp ConfigMap. The weights labeller's watch list | the namespace exists; KEDA's namespace; no other scaling manager manages a model namespace; no namespaced install beside a cluster-wide one; a re-run without the list keeps the namespaces recorded on the Namespace reader |
| `smt install -n CTRL --prometheus-url URL [...]` | namespace owner (stock `admin` ClusterRole in CTRL) | the settings ConfigMap (created once), the Prometheus CA Secret, the empty scaler TLS Secrets (created once; the controller fills them), the Deployment and Services, the preflight Job before and after. With `--utilization-share active`, the pod-writer grant | `SHA256SUMS` and release version of the files; every flag against what the controller accepts; the stamp matches the release's admin objects; the http/https scheme against the live settings; the preflight (a FAIL stops before the controller); the rollout |
| `smt admin-finish -n CTRL` | cluster admin | copies the client certificate Secret the controller minted into KEDA's namespace, applies KEDA's ClusterTriggerAuthentication and the marker ConfigMap, restarts the controller when that changed, removes the preflight's ClusterRoleBinding | waits for the certificate; KEDA's namespace agrees with the NetworkPolicy; refuses a Secret it did not write; waits for `scalerTLS: mutual TLS`; KEDA's stale connection (`--restart-keda`); the preflight again |
| `smt scaledobjects-plan` / `-apply` | either | a commented plan of one ScaledObject per model server (per role on a DisaggregatedSet); then exactly that plan; then re-wires an HPA KEDA left on the CPU default | discovers model servers and InferencePools |
| `smt workload-patch -n M [--apply]` | namespace owner | with `--apply`, patches the model servers (drain hook, caches) | reports what each still needs |
| `smt release-files --out DIR` | anyone | nothing: writes the release files | |

The preflight is already the controller binary (`/manager preflight check`)
run as a Job ([preflight](../reference/preflight.md)), and the controller
already mints the scaler's CA and leaves at start. What is shell is the
order, the substitutions, the checks between steps, and the KEDA copy.

**Why docker.** The tools image carries the release files, signed as one
unit with the image, and the logic, at pinned tool versions. Docker is how
both reach the operator, not a need of the install. It costs: no kubeconfig
`exec` plugins (token kubeconfigs only), `--network host` for a `127.0.0.1`
API server, and a read-write mount of the working directory.

**Without docker, before v0.13.0:**

| path | needs | missing |
| --- | --- | --- |
| [by hand from the release files](../guides/install-in-namespace/#install-by-hand-from-the-release-files) | `kubectl`, `curl`, `sed` | about 20 commands; `sed` on `__NAMESPACE__`; the KEDA step copied out of the controller's status; multi-namespace needs hand edits |
| the tools tarball `scaling-manager-tools-<release>.tar.gz` | bash, `kubectl`, `jq`, `yq`, `openssl`, `column` locally | local tool versions vary; not tested as an install path |
| `make deploy-scaling-manager` | a checkout, `make`, `helm`, `yq`, `jq` | not a release install |

### J. Helm

AGENTS.md forbids a Helm chart, so this is not the recommendation. It is
here because the router installs with Helm:

- every llm-d operator already runs `helm` for the router;
- `helm upgrade` and `helm uninstall` give upgrade and removal;
- Flux's helm-controller runs hooks, and Argo CD maps Helm hooks to its
  sync hooks.

With one manifest, a chart becomes very thin. It would be the same
manifest, cut into `templates/` by CI, with two values: OpenShift, and the
image repository. KEDA's namespace is already a setting. Config stays in the settings
ConfigMap, not in values. That thin chart, generated in CI
beside the manifest, is the form in which the llm-d guides install
LeaderWorkerSet and Kueue today ([neighbours](#keda-kueue-leaderworkerset-cert-manager)).
So a chart would match how llm-d's guides install their prerequisites,
while adding no config surface of its own. Whether to ship one is the
maintainers' call.

What remains against it:

- the rule itself: bringing a chart back is a maintainer decision;
- the advanced path's admin/owner split, which would need two charts.

If the rule is lifted, push it as
`oci://ghcr.io/llm-d-incubation/charts/llm-d-scaling-manager`, generated
from the same render as `scaling-manager.yaml`, and keep a parity check
against the manifest.

### K. Verification and tests

What exists, from #409 ([testing](../developer-guide/testing.md)):

- **The kind e2e for the main path,** `make test-e2e-manifest-install`, run
  after `test-e2e-release-manifests-setup`. It installs with the two scripts,
  polls `ready`, registers a model, runs the smoke suite, uninstalls, and
  checks nothing labelled with the instance is left. Images: the locally
  built controller, and the prerequisites from `registry.k8s.io` and
  `quay.io`; never docker.io. Ginkgo runs with `-v`.
- **The migration e2e,** `make test-e2e-manifest-migrate`: a cluster-wide
  `smt` install over mutual TLS, then `move`, `upgrade` and
  `watchNamespaces`; the CA is copied and KEDA reconnects with no restart of
  its operator.
- **The scripts,** `hack/check-kubectl-scripts.sh` in `make
  lint-deploy-scripts`: `--print` pinned against
  `hack/testdata/kubectl-scripts/`, run mode equal to `--print` against a
  stub `kubectl` with nothing else on `PATH`, and the refusals.
- **The manifest,** `hack/check-manifest-install.sh`: passive by default,
  KEDA's side by name and in its own file, the TLS Secrets with no data, no
  label guard; and `hack/check-release-manifests.sh` for the sums and the
  signing of both scripts.
- **Unit tests** for each controller change, among them
  `TestWatchNamespacesLive`, `TestAdmissionFollowsTheSettingLive`,
  `TestAdmittedStreamRefusesOnceTheNamespaceIsLeftOut`,
  `TestReporterReadinessAndEvents`, `TestReporterGrants` and
  `TestPassScansOnlyTheGrantedNamespaces`.

Still to build:

- **CI.** Neither kind e2e runs in CI.
- **An upgrade leg** from the previous release, and passive to shadow to
  active on this path, with the grant.
- **GitOps:** apply the settings without the switch keys, flip them, and
  re-apply: they hold. Run Argo CD and Flux, and assert the reset case: a
  switch left in git is overwritten.
- **OpenShift.**
- **The advanced path:** `make test-e2e-kustomize-install`, and the
  render-parity check `hack/check-kustomize-install.sh` in `make
  lint-deploy-scripts`.
  - It compares `kubectl kustomize` with the release files, object for
    object, on non-empty, equal-count sides.
  - It covers the [renders](#l-the-render-and-kind-evidence), including the
    EPP references a rename does not follow.
  - Its negative control is a base with one rule removed.
- **llm-d:** a recipe falls under that repository's kustomize dry-run CI
  job, as a tier-2 guide.

### L. The render and kind evidence

These are scratch renders with kubectl v1.31 (kustomize v5.4.2), of a
copy of the layout with a controller namespace `llmsm-ctrl`, for the
advanced path. The parity check, not built yet, would render the real bases
the same way.

```text
kubectl kustomize sm-admin/cluster    (namespace: llmsm-ctrl, nameSuffix: -llmsm-ctrl)
  without the component:
    scaling-manager-policy-namespace-reader-rolebinding-llmsm-ctrl  roleRef ...-role-llmsm-ctrl  subject namespace scaling-manager-system
    scaling-manager-auth-delegator-llmsm-ctrl                       roleRef system:auth-delegator   subject namespace scaling-manager-system
  with it:
    scaling-manager-policy-namespace-reader-rolebinding-llmsm-ctrl  roleRef ...-role-llmsm-ctrl  subject namespace llmsm-ctrl
    scaling-manager-auth-delegator-llmsm-ctrl                       roleRef system:auth-delegator   subject namespace llmsm-ctrl
kubectl kustomize sm-admin/team-b     (namespace: team-b, with the component)
  RoleBinding  namespace team-b  subject namespace llmsm-ctrl
kubectl kustomize single              (namespace: team-a, the ServiceAccount in the same build)
  RoleBinding  namespace team-a  subject namespace team-a
```

- **Subjects.** A kustomization's `namespace:` moves a binding's subject
  only when that ServiceAccount is in the same build.
- **Renames.** `nameSuffix` renames each ClusterRole and the `roleRef` that
  names it. A binding to a stock role such as `system:auth-delegator` keeps
  its `roleRef`.
- **Settings name.** A generator with `behavior: merge` and
  `disableNameSuffixHash: true` keeps the name `scaling-manager-settings`.
- **Env patch.** A strategic-merge patch of the Deployment's env replaces
  `LLMSM_WATCH_NAMESPACE` by name and keeps the other entries. It moves the
  patched entry first, which is harmless for a literal value.
- **ServiceAccounts in the same build** (`steps/preflight`,
  `steps/keda-auth/keda`). `nameSuffix` renames the ServiceAccount, and
  kustomize follows it into the Job's `serviceAccountName`. It follows it
  into a binding's subject only when the base gives the ServiceAccount the
  subject's namespace:
  - ServiceAccount with no namespace: the subject kept
    `scaling-manager-preflight` in `scaling-manager-system`;
  - ServiceAccount with `namespace: scaling-manager-system`: the subject
    became `scaling-manager-preflight-llmsm-ctrl` in `llmsm-ctrl`.

  So every base ServiceAccount carries the namespace its subjects name.
- **Not followed by a rename.** With `nameSuffix` on the EPP objects, the
  token Secret's `kubernetes.io/service-account.name` annotation still named
  `epp-metrics`, and the ServiceMonitor's `authorization.credentials.name`
  still named `epp-metrics-token`, while both objects had been renamed. A
  targeted patch of the ClusterRoleBinding's name alone renders correctly.
  The parity check includes this case.
- **The ClusterTriggerAuthentication.** A kustomize `configurations:`
  nameReference carries `nameSuffix` into its `secretTargetRef`. Kustomize
  also sets `metadata.namespace` on it, because kustomize does not know the
  CRD's scope. Still to check: that `kubectl apply` ignores the field, or
  give kustomize the CRD's scope.

On a throwaway kind cluster (v1.31.2), with plain `kubectl`: a Secret applied
server-side with no data, filled by another field manager, kept its data on
a second apply. A ConfigMap key the applied file omitted, patched by another
manager, survived the re-apply. A key the file set, patched the same way,
made the next apply fail with a conflict. A scratch render confirmed that an
`images:` digest entry replaces the base's tag with `@sha256:...`.

### M. Words used here

- **the marker**: ConfigMap `scaling-manager-scaler-keda-auth` in the
  controller namespace; the controller serves mutual TLS only once it says
  KEDA's side is in place.
- **the stamp**: ConfigMap `scaling-manager-admin-setup`, which `smt
  admin-setup` writes so `smt install` can tell the admin step ran.
- **`scalerTLSAction`**: the key in `scaling-manager-controller-status` that
  prints what to run to give KEDA its certificate.
- **managed namespace**: a namespace `watchNamespaces` names, or any
  namespace when it is empty. The scaler serves ScaledObjects only there.
- **KEDA's copy**: Secret `scaling-manager-external-scaler-<controller
  namespace>` in KEDA's namespace, the client certificate KEDA presents to
  the scaler through the ClusterTriggerAuthentication.

### N. Open questions

Decided in #409 and its review: see the table under
[Open questions](#open-questions-decisions-for-the-maintainers). The first
draft's questions, the review's (items 7 to 11), and where they stand:

1. **Cluster-wide by default,** with the advanced path for least privilege?
   Open. The main path is cluster-wide as built; the advanced path is not
   built.
2. **A git ref for llm-d recipes,** pinned to a release commit with the
   image by digest, although install-from-release ruled out a remote base?
   Open. The review leaned to a signed, digest-pinned tarball of the
   kustomize bases instead, with the git ref kept for development; neither
   is built.
3. **Helm:** lift the rule for a thin chart generated in CI, as the llm-d
   guides use for LeaderWorkerSet and Kueue? Open; a maintainers' call.
4. **The cluster-wide `configmaps` read:** narrow it to the policy map's
   name, if every read allows it? Open; not checked yet.
5. **The kustomize floor** for the advanced path: which oldest `kubectl`?
   Open, and only matters once the advanced path is built.
6. **The weights labeller:** it stays an `smt` step. Is a status key naming
   the patch enough? Open; no such key exists.
7. **The namespace selection.** Decided: `watchNamespaces`, live. The label
   and `namespaceSelector` are gone ([Alternatives](#alternatives)).
8. **Should the controller create the scaler TLS Secrets, and where does
   KEDA's copy go?** Decided: it fills Secrets the manifest pre-creates, with
   `get` and `update` by name, and KEDA's copy goes only to KEDA's
   namespace. Never a model namespace.
9. **Kustomize replacements.** Moot for the main path: the parameters are
   settings the controller reads at runtime (`kedaNamespace`,
   `watchNamespaces`), so the files need no substitution.
10. **The model plan: a Job or the controller?** Decided: the controller.
11. **A settings re-apply reverting a live switch.** Decided: the switch
    keys stay out of git, and the scripts set them with `set`.

New, from #409:

12. **The plan's reader.** The plan ConfigMap is in `scaling-manager-system`,
    so `scaling-manager-owner.sh register <ns>` without a file works only for
    someone who can read it. Today the admin hands each owner a file. Should
    the controller publish each namespace's plan where its owner can read it?
13. **The EPP NetworkPolicy rule** selects every pod of the namespace on
    port 9090. Select the EPP by a label the router sets, once there is one?
14. **Overlapping installs** are reported by `conflicts`, not refused.
    Should this install stop managing a namespace another scaling manager
    manages?
15. **The rest of the controller following `watchNamespaces`:** the KEDA HPA
    repair and the namespace-local policy path still act wherever their own
    opt-in is.

### A CRD later?

Phase 1 keeps the ConfigMaps. A `ScalingManagerConfig` CRD would add:

- validation at admission (types, enums, ranges);
- a status subresource with conditions, which only the controller writes;
- `kubectl wait --for=condition=Ready`, `-o wide` printer columns and
  `kubectl explain`.

Without it, the project avoids:

- an API to version and, later, convert, with a conversion webhook that
  needs its own certificate;
- `storedVersions` migrations;
- older CRDs silently pruning newer fields on rollback;
- the code for a new type.

It also works on today's controller, which already reads the settings and
writes the status ConfigMaps. Revisit once the keys settle.
