# Install with kubectl and kustomize, without docker

- **Status:** proposed. Nothing in it is built.
- **Audience:** contributors and maintainers. Operators install today from
  [install in a namespace](../guides/install-in-namespace/) and the
  [install commands](../reference/tools-install.md). The commands below
  are the draft of a future guide, `docs/guides/kubectl-install/`.
- **Builds on:** [Install from the release](install-from-release.md) and
  [Install in two commands](two-command-install.md).

## Summary

A cluster admin installs the scaling manager the way KEDA, Kueue and
LeaderWorkerSet install: one signed release manifest, applied with
`kubectl apply --server-side`. No docker, no checkout. The config is the
existing settings ConfigMap, and the status is the existing status
ConfigMap, so both live as plain objects a GitOps tool can sync. A
namespace opts in with a label. The controller checks its own
prerequisites, mints its own TLS, wires KEDA, and suggests the
ScaledObjects. A person still reviews and applies them. `smt` keeps
working on the same release files, and kustomize stays available for
customising: the manifest is rendered from the same bases.

## Motivation

Installing today takes the tools image (docker) or about 20 commands by hand.
The controllers an llm-d cluster already runs install with one manifest.

**Goals.** Install, configure, upgrade and uninstall with `kubectl` only. No
docker, no checkout. Config that GitOps can hold. The passive first
install and the preflight stay.

**Non-goals.** A Helm chart (forbidden by AGENTS.md; see
[Alternatives](#alternatives)). A new CRD in phase 1. Changing how the
controller decides.

## Proposal

### User stories

- As a cluster admin, I install one signed manifest and label the namespaces
  to manage.
- As a platform team, I keep the install and its settings in git, and flip
  passive, shadow and active with kubectl.
- As a namespace owner, I review the ScaledObjects the controller suggests
  before anything scales.

### One namespace

```bash
V=v0.13.0
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V
curl -fsSLO $URL/scaling-manager.yaml && curl -fsSLO $URL/scaling-manager.yaml.sigstore.json
cosign verify-blob scaling-manager.yaml --bundle scaling-manager.yaml.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/$V"
kubectl apply --server-side -f scaling-manager.yaml               # namespace, RBAC, controller, KEDA's side
kubectl apply --server-side -f settings.yaml                      # the settings ConfigMap, below
kubectl label namespace team-a scaling-manager.llmd.ai/managed=true
```

```yaml
# settings.yaml: the existing scaling-manager-settings ConfigMap. No live switches in it.
apiVersion: v1
kind: ConfigMap
metadata:
  name: scaling-manager-settings
  namespace: scaling-manager-system
data:
  prometheusURL: https://prometheus.monitoring.svc:9090
  namespaceSelector: "scaling-manager.llmd.ai/managed=true,kubernetes.io/metadata.name in (team-a)"
```

### Several namespaces, one budget, a DisaggregatedSet, sharing in shadow

```bash
kubectl apply --server-side -f scaling-manager.yaml               # verified as above
kubectl apply --server-side -f settings.yaml                      # with the budget, below
for NS in team-a team-b; do
  kubectl label namespace $NS scaling-manager.llmd.ai/managed=true
done
# Static DisaggregatedSet roles only: the role writer's grant, the switch, the opt-in.
kubectl apply -n team-a -f scaling-manager-grants.yaml -l scaling-manager.llmd.ai/grant=dset-role-writer
kubectl patch configmap scaling-manager-settings -n scaling-manager-system --type merge \
  -p '{"data":{"dsetRoleWriter":"true","utilizationShare":"shadow"}}'   # live switches, never in git
kubectl annotate disaggregatedset <set> -n team-a scaling-manager.llmd.ai/role-writer=true \
  scaling-manager.llmd.ai/model-id=<model> scaling-manager.llmd.ai/min-replicas=1 \
  scaling-manager.llmd.ai/max-replicas=<max groups per role>
# Register the models: review the controller's plan, then apply it (per namespace).
kubectl get configmap scaling-manager-scaledobjects-plan -n scaling-manager-system \
  -o jsonpath='{.data.team-a}' > so-team-a.yaml && kubectl apply -n team-a -f so-team-a.yaml
```

```yaml
# settings.yaml, data: for a shared budget
  prometheusURL: https://prometheus.monitoring.svc:9090
  namespaceSelector: "scaling-manager.llmd.ai/managed=true,kubernetes.io/metadata.name in (team-a,team-b)"
  limiter: quota
  quotas: NVIDIA-H100-80GB-HBM3=16
  quotaScope: cluster
```

The install is passive: it computes, publishes and moves nothing. To see
whether a role is Static or External:
`kubectl get disaggregatedset <set> -n team-a -o jsonpath='{.spec.roles[*].scaling.mode}'`.
External roles need no grant ([appendix A](#a-the-advanced-path-least-privilege-per-namespace)
has the full DisaggregatedSet steps).

### What you get

- **The preflight, inside the controller.** It checks at start and every
  cycle, and writes the results to the existing status ConfigMap
  `scaling-manager-controller-status`: one key per check (`ok`, `warn` or
  `fail` and a reason), plus `ready`. Changes become Events. There is no
  preflight Job.
- **Its own TLS, with no `create` on Secrets.** The manifest pre-creates
  the scaler's Secrets with no data, and the controller fills and rotates
  them by name.
- **KEDA wired for you.** The manifest pre-creates the one Secret KEDA
  reads, in KEDA's namespace, and the ClusterTriggerAuthentication. The
  controller fills that Secret by name. No Job, no key in a model
  namespace.
- **A suggested plan.** One ScaledObject per model server, per role on a
  DisaggregatedSet, in `scaling-manager-scaledobjects-plan`. You review it
  and apply it.

### Check it, and switch it

```bash
kubectl get configmap scaling-manager-controller-status -n scaling-manager-system \
  -o jsonpath='{.data.ready} {.data.namespacesManaged} {.data.scalerTLS} {.data.utilizationShare}{"\n"}'
kubectl get events -n scaling-manager-system --field-selector involvedObject.name=scaling-manager-controller-status
```

`ready` is `ok` when every check passes; otherwise the failing check's key
names the fix. `kubectl wait` cannot wait on ConfigMap data, so a script
polls `ready`.

Passive, then shadow, then active, one key each:

```bash
P="kubectl patch configmap scaling-manager-settings -n scaling-manager-system --type merge -p"
$P '{"data":{"utilizationShare":"shadow"}}'     # sharing computes and publishes, moves nothing
$P '{"data":{"passive":"false"}}'               # the controller scales
kubectl apply -n team-a -f scaling-manager-grants.yaml -l scaling-manager.llmd.ai/grant=pod-writer   # admin, per sharing namespace
$P '{"data":{"utilizationShare":"active"}}'     # sharing moves replicas
```

To undo, set `utilizationShare` back to `shadow`, wait one cycle (15 s)
so no transfer marks are left on pods, then set it to `off` and delete the
pod-writer grants. The budget stays.

**Switch keys are never in git.** `passive`, `utilizationShare` and
`dsetRoleWriter` are flipped with `kubectl`. Argo CD with server-side apply
and Flux both force their ownership, so a switch left in the synced file
silently resets a live patch on every sync ([appendix E](#e-gitops)).

### The trade-offs

- **Cluster-wide reads.** A label is not an RBAC scope, so the controller
  reads cluster-wide. These are the same reads as today's cluster-wide
  install ([appendix C](#c-rbac-in-full)).
- **The label alone is not consent.** A tenant who can create namespaces
  can set the label. Set `namespaceSelector` to names or a label only the
  admin sets. The scaler and the controller refuse every namespace it does
  not admit, and list the refused ones in the status. Left unset, the
  selector admits every labelled namespace. The status then warns when a
  cluster-scope budget is shared.
- **Active sharing is a grant per namespace.** Patching pods stays the
  admin's explicit act, from the labelled `scaling-manager-grants.yaml`.

| | today's cluster-wide install | this install | today's namespaced install |
| --- | --- | --- | --- |
| reads | cluster-wide | the same | per namespace |
| pod `patch` | per namespace, at activation | the same | the same |
| KEDA's copy | the admin's kubeconfig | the controller, one Secret by name | the admin's kubeconfig |
| `create` on Secrets | no | no | no |
| commands, one namespace | 3 with `smt`, plus docker setup | 5, plus download and verify | 3 with `smt`; about 16 on the [advanced path](#a-the-advanced-path-least-privilege-per-namespace) |

## Design details

The appendices hold the detail:

- [B](#b-what-has-to-be-built-and-the-phases): what the controller must gain, and the phases.
- [C](#c-rbac-in-full): RBAC in full.
- [D](#d-monitoring): monitoring.
- [E](#e-gitops): GitOps.
- [F](#f-upgrade-rollback-air-gapped-uninstall): upgrade, rollback, air-gapped use and uninstall.
- [G](#g-migration-from-smt): migration from `smt`.
- [K](#k-verification-and-tests): verification and tests.

## Alternatives

- **Least privilege per namespace** ([appendix A](#a-the-advanced-path-least-privilege-per-namespace)): kept as the advanced path, not the default, because it costs about 16 commands and 11 files per namespace.
- **Helm** ([appendix J](#j-helm)): closest to the router, but forbidden by AGENTS.md.
- **A CRD** ([A CRD later?](#a-crd-later)): deferred.
- **`smt` only**: it keeps working, but needs docker.

## Release notes

A new install path and release assets (`scaling-manager.yaml`, `scaling-manager-grants.yaml`, `scaling-manager-monitoring.yaml`, the kustomize tarball). New settings keys: `namespaceSelector`, `caFromSecret`, `dsetRoleWriter`. New status keys. `smt` and the existing release files are unchanged.

## Open questions: decisions for the maintainers

- **Cluster-wide by default.** The main path needs today's cluster-wide
  reads, and least privilege per namespace becomes the
  [advanced path](#a-the-advanced-path-least-privilege-per-namespace).
- **Helm.** [AGENTS.md](../../AGENTS.md) forbids a chart. The router
  installs with Helm, and the llm-d guides install LeaderWorkerSet and
  Kueue from their Helm OCI charts. A chart over this manifest would be
  thin ([appendix J](#j-helm)).
- **A git ref for llm-d recipes.** Install-from-release ruled out a remote
  kustomize base on 2026-11-10, but an llm-d recipe needs one, pinned to a
  release commit, with the image pinned by digest
  ([appendix H](#h-the-llm-d-recipe-path)).
- **No CRD in phase 1.** The settings and status ConfigMaps carry the
  config and the result ([A CRD later?](#a-crd-later)).

## Appendices

### A. The advanced path: least privilege per namespace

For a controller per namespace that needs no cluster-wide reads, or a
namespace owner who installs without a cluster admin beyond a first step.
It is the design two review rounds settled. It holds more files and more
commands: about 16 commands and 11 small files for one namespace; about 27
and 16 for several namespaces with a DisaggregatedSet and shadow sharing.
`kubectl scaling-manager init` writes the files.

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

The scaler CA, Secret `scaling-manager-external-scaler-ca`, is kept for a
reinstall. To delete it: `kubectl delete secret -n $CTRL -l
scaling-manager.llmd.ai/scaler-tls`.

#### A DisaggregatedSet, step by step

```bash
NS=team-a SET=<your DisaggregatedSet>
kubectl get disaggregatedset $SET -n $NS -o jsonpath='{.spec.roles[*].scaling.mode}{"\n"}'
```

- **External roles** are scaled through their `DisaggregatedSetRoleScaler`
  (`<set>-<role>`). The plan has one ScaledObject per role.
- **Static roles** have no `/scale`, so the role writer reads and writes
  them. Three steps:
  1. The admin binds the second unbound ClusterRole (`get`, `list`, `patch`
     on `disaggregatedsets`) in the namespace:

     ```bash
     kubectl apply -n $NS -f scaling-manager-grants.yaml -l scaling-manager.llmd.ai/grant=dset-role-writer
     ```

  2. Turn the writer on:

     ```bash
     kubectl patch configmap scaling-manager-settings -n scaling-manager-system --type merge \
       -p '{"data":{"dsetRoleWriter":"true"}}'
     ```

  3. Opt the set in:

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
  While the model is passive the writer writes nothing.

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
the preflight does it, before the admin apply. That is new work: today's
`overlap` reads one cluster-wide controller's Deployment by name and looks at
no other namespace. Its rights:

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
[security model](../reference/security-model.md)). Today the admin makes the
copy with their own kubeconfig.

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
  - `get` on Secret `scaling-manager-external-scaler-client-tls` and on
    ConfigMap `scaling-manager-controller-status`, in the controller
    namespace;
  - `get` and `update` on the pre-created Secret in KEDA's namespace;
  - `get` and `update` on the marker.

  No `create`, no `pods/log`, nothing cluster-wide, no right the owner can
  reach.
- The Job waits for the controller to fill the client Secret, copies it, and
  sets the marker's `ready` key. The controller watches the marker and, when
  `ready` changes, re-reads the client CA and serves mutual TLS without a
  restart (a controller change). The Job reads `scalerTLS` until
  `mutual TLS`. If the live re-read is not built, the Job also needs `get`
  and `patch` by name on the Deployment, to restart it through a
  pod-template annotation.
- **KEDA's connection.** Today `admin-finish` judges KEDA's connection from
  the controller's log and KEDA's pods (`deploy/lib/scaler_tls.sh`), and
  `pods/log` cannot be limited by name. Instead the controller publishes
  the verdict in its status: `kedaConnection` (`none`, `current` or
  `stale`) and `kedaCopyStale` (KEDA's copy predates the current client
  certificate). The Job reads only that.

**Rotation.** KEDA keeps one connection per scaler address until it
restarts. When the CA changes, the Job reports `kedaConnection: stale` and
prints the restart of KEDA's operator; it never restarts KEDA, which is
shared. Re-run the Job after a CA change.

A namespaced TriggerAuthentication is not used. If it ever is, it needs
KEDA 2.21 or later.

##### The scaler TLS Secrets

Today's release file ships them as type `kubernetes.io/tls` with empty
keys. That type requires the key fields, so an apply of that file empties
the certificates the controller wrote; that is why it is created once.
Both paths here instead ship them as `Opaque` with no `data`, under new
names. An `Opaque` Secret with no `data` field keeps what another manager
wrote across a server-side re-apply, which was checked on kind. So the
Secrets sit in the applied bases, and the controller keeps `get` and
`update` by name only. It never needs `create` on Secrets. The type of the
existing Secrets cannot change, hence the new names; on first start the
controller copies the old CA across.

### B. What has to be built, and the phases

What exists today, on `main`:

- the settings ConfigMap `scaling-manager-settings` (`config/release/settings-configmap.yaml`, read by `internal/controller/settings.go`). Its live keys take effect within a cycle; its restart keys after a restart;
- the status ConfigMap `scaling-manager-controller-status` (`internal/controllerstatus/publisher.go`), with `limiter`, `utilizationShare`, `scalerTLS`, `scalerTLSAction`, `scalerUnreachable` and more;
- the preflight, as `/manager preflight check` in a Job (`internal/preflight`), writing its own result ConfigMap;
- the scaler TLS bootstrap, which mints the CA and leaves.

What is new:

- **Settings keys:**
  - `namespaceSelector`, a label-selector string;
  - `caFromSecret`, which reads the CA from the fixed Secret `scaling-manager-prometheus-ca`;
  - `dsetRoleWriter`, today the `DSET_ROLE_WRITER` env var.

  A change to a restart key makes the controller restart itself.
- **Status keys:**
  - one per preflight check, `ok`, `warn` or `fail` and a reason;
  - `ready`;
  - `namespacesManaged`, and the namespaces the selector refused;
  - `kedaConnection` (`none`, `current` or `stale`) and `kedaCopyStale`.

  Each change of a check also becomes an Event.
- **The preflight inside the controller,** at start and every cycle. Its new checks:
  - conflicts, read from RoleBindings and ClusterRoleBindings, never Deployments;
  - which policy wins;
  - a double scrape, a missing EPP scrape, and the EPP metric names this release reads (llm-d's `env.sh` tracks `ROUTER_EPP_VERSION=main`).
- **The label watch with the selector.** The scaler refuses KEDA calls for a namespace the selector does not admit (today it registers any namespace KEDA sends), and the controller refuses its workloads. An optional ValidatingAdmissionPolicy, `scaling-manager-label-guard.yaml`, limits who may set the label.
- **Its own TLS, without `create` on Secrets.**
  - The manifest carries the scaler's Secrets as `Opaque`, with no data, under new names.
  - The controller fills and rotates them with `get` and `update` by name.
  - On first start over an `smt` install, it copies the old CA across.
- **KEDA's copy.**
  - The manifest pre-creates the target Secret in KEDA's namespace (`Opaque`, no data) and the ClusterTriggerAuthentication.
  - A Role in KEDA's namespace gives `get` and `update` on that Secret by name.
  - The controller switches to mutual TLS when the copy is written, without a restart.
  - KEDA keeps one connection per scaler address until it restarts, so on a CA change the status names the KEDA restart. The controller never restarts KEDA.
- **The plan writer,** a port of the discovery in `deploy/lib/scaledobject.sh`, into the pre-created plan ConfigMap. It also reports an unwired HPA, for KEDA before 2.20.
- **The release:**
  - `scaling-manager.yaml`, `scaling-manager-grants.yaml` and `scaling-manager-monitoring.yaml`, all rendered from `config/install`;
  - the kustomize tarball;
  - all of these in `SHA256SUMS` and signed;
  - the plugin binaries and a krew manifest, from phase 4.

| `smt` does | main path | advanced path |
| --- | --- | --- |
| the managed namespaces | the label and `namespaceSelector` | `LLMSM_WATCH_NAMESPACE` patch, the Namespace reader's list |
| the preflight | status keys, continuous | the preflight Job |
| refuses an overlapping install | the conflict check; it manages nothing it overlaps on | the conflict check in run 1 |
| KEDA's client certificate | the controller, one Secret by name | the `keda-auth` Job |
| `--prometheus-*`, `--allow-http`, `--quota`, `--set` | settings keys; the CA in the fixed Secret | the same |
| `--utilization-share`, `passive`, `--dset-role-writer` | settings keys, never in git; grants per namespace | the same |
| PodMonitor, `--modelserver-port`, the EPP monitor | llm-d's components or `scaling-manager-monitoring.yaml` ([D](#d-monitoring)) | the monitoring components |
| `scaledobjects-plan` / `-apply`, the HPA re-wiring | the controller's plan, applied by a person; the unwired-HPA report | the same |
| the weights labeller's watch list | stays in `smt` and the plugin; a status key names the patch | the same |

`smt` keeps every command, now on the same rendered files. One tree,
`config/install`, feeds the tools image, the release files, the manifest and
the tarball.

| phase | what | effort |
| --- | --- | --- |
| 1 | `config/install` (replacing `config/overlays`), the manifest and the tarball rendered from it, the parity check; the new settings keys; whether the cluster-wide `configmaps` read can be narrowed | 2 weeks |
| 2 | the controller: the preflight as status keys with the new checks; the selector and the scaler refusing unadmitted namespaces; the label policy; its own TLS without `create`; KEDA's copy and the CA re-read; the plan writer; the kind e2e; the guide | 4 weeks |
| 3 | the advanced path: the Jobs, the steps, their e2e. `epp-monitoring` has fixed names, so two unscraped EPPs in one model namespace would collide: name its objects per EPP | 2 weeks |
| 4 | the plugin, krew manifest, release binaries; the README leads with the manifest | 2 weeks |

About ten weeks of work; phases 1 and 2 overlap, about seven elapsed.

### C. RBAC in full

A label is not an RBAC scope, so watching labelled namespaces needs
cluster-wide reads. The ClusterRole for passive mode is the controller's
`manager-role` of today ([config/base/rbac/manager-clusterrole.yaml](../../config/base/rbac/manager-clusterrole.yaml)):

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

New on top of it:

- no rights on a CRD: the config and the status stay ConfigMaps, with
  `get` and `update` by name on the status ConfigMap, as today;
- `get` and `update` by name on the one Secret in KEDA's namespace;
- `list` on `rolebindings` and `clusterrolebindings`, for the conflict
  check (another controller in a managed namespace). This shows the
  cluster's RBAC layout to whoever can act as the controller, which on this
  path is the cluster admin alone;
- `get` and `update` by name on its scaler TLS Secrets, and `get` by name on
  Secret `scaling-manager-prometheus-ca`. The CA has a fixed name, so no
  unscoped Secret read is needed (`spec.prometheus.caFromSecret: true`
  points the controller at it).

Its own namespace's Roles (leader election, status, learned state, the
plan ConfigMap by name) are as today. It writes nothing outside its
namespace except Events, KEDA's copy, and the two per-namespace grants once
the admin binds them.

| | today's [cluster-wide install](../guides/install-cluster-wide/) | this path | today's namespaced install |
| --- | --- | --- | --- |
| reads | cluster-wide, the table above | the same | per namespace, plus the cluster-scoped readers |
| pod `patch` | per namespace, at activation | the same | per namespace, at activation |
| KEDA's copy | the admin's kubeconfig, once | the controller, one Secret by name | the admin's kubeconfig, once |
| who installs | cluster admin | cluster admin | admin, then the namespace owner |

So this path holds **the same rights** as today's cluster-wide install,
plus one named Secret in KEDA's namespace instead of the admin copying it.
It holds **more** than a namespaced install. The `configmaps` read across
the cluster is the widest of them. It is there for each namespace's own
policy map, and phase 1 checks whether it can be narrowed to that name.
Where least privilege per namespace is the rule, take the advanced path.

### D. Monitoring

Each scrape must exist once: a second doubles every rate, and a missing one
leaves the analyzers blind. The `metricsScraped` status key reports both.

| source | where llm-d already scrapes it | where nothing scrapes it |
| --- | --- | --- |
| model servers | llm-d's kustomize components [`monitoring`](https://github.com/llm-d/llm-d/tree/main/guides/recipes/modelserver/components/monitoring) (decode: selects `llm-d.ai/role: decode`, port `modelserver`, `/metrics`, every 30s), `monitoring-pd` (P/D, which the pd-disaggregation guide applies) and `monitoring-epd`. Nothing to add | the release's `scaling-manager-monitoring.yaml`, applied in the namespace: a PodMonitor on port `modelserver` |
| the EPP | the router installed with llm-d's [`monitoring.values.yaml`](https://raw.githubusercontent.com/llm-d/llm-d/main/guides/recipes/router/features/monitoring.values.yaml): its own ServiceMonitor, Prometheus auth off. Nothing to add | the router with its auth default (on) and no monitoring values: the `epp-monitoring` objects of the same file, which scrape with a token |

Where nothing scrapes, in each such namespace:

```bash
NS=team-a
kubectl apply -n $NS -f scaling-manager-monitoring.yaml -l scaling-manager.llmd.ai/monitor=modelserver
# a metrics port not named "modelserver" (what smt's --modelserver-port did):
kubectl patch podmonitor scaling-manager-modelserver-metrics -n $NS --type json \
  -p '[{"op":"replace","path":"/spec/podMetricsEndpoints/0/port","value":"<port name>"}]'
# the EPP, only where the router runs with its auth on and no monitoring values:
kubectl apply -n $NS -f scaling-manager-monitoring.yaml -l scaling-manager.llmd.ai/monitor=epp
kubectl create clusterrolebinding scaling-manager-epp-metrics-scraper-$NS \
  --clusterrole=scaling-manager-epp-metrics-reader --serviceaccount=$NS:scaling-manager-epp-metrics-scraper
kubectl label clusterrolebinding scaling-manager-epp-metrics-scraper-$NS \
  app.kubernetes.io/part-of=scaling-manager app.kubernetes.io/instance=scaling-manager-system
```

The EPP's ServiceAccount, token Secret and ServiceMonitor live in the model
namespace, because a ServiceMonitor reads its credentials from its own
namespace. Only the `/metrics` binding is cluster-scoped, one per namespace
by name. A port patch is undone by the next apply of the file: re-run it, or
keep the PodMonitor in a kustomization with the patch.

Whether llm-d's `monitoring-pd` selects a DisaggregatedSet's pods was not
confirmed. The `metricsScraped` status key names any role with no `vllm:`
series.

On GKE with Managed Prometheus, llm-d uses `PodMonitoring`. Our monitors are
Prometheus Operator objects, so scrape with GMP instead.

### E. GitOps

- **Keep in the sync:** `scaling-manager.yaml` (or its kustomization) and the
  settings ConfigMap without its switch keys.
- **Keep out of the sync:** the switch keys (`passive`, `utilizationShare`,
  `dsetRoleWriter`) and the per-namespace grants. On the advanced path,
  also `sm-steps/`.

Under plain `kubectl apply --server-side`, a key the file leaves out keeps
what another manager set. A key the file sets, and someone else changed,
makes the apply stop with a conflict. Argo CD with `ServerSideApply=true`
and Flux both force ownership instead. A switch key left in git therefore
silently resets a live patch on every sync. Applying client-side, Argo CD
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

Argo CD must track resources by annotation, because label tracking rewrites
`app.kubernetes.io/instance`, which `smt`, the uninstall and the migration
rely on. Nobody runs `rollout restart` under auto-sync, and none is needed:
a changed restart key makes the controller restart itself. Changed again
and again, that puts the Pod into the kubelet's crash back-off, which grows
to five minutes. The status says when a restart is pending.

### F. Upgrade, rollback, air-gapped, uninstall

**Upgrade:** `kubectl apply --server-side -f` the new release's manifest,
verified. The settings and the ScaledObjects are not in it, so they stay. A
key the new release adds takes its default. Under GitOps, merge the version
bump.

**Rollback:** apply the old manifest. The settings are a ConfigMap, so no
schema prunes a newer key. A key the older controller does not know is
reported in the status (the `settings` check) and has no effect; remove it.

**KEDA elsewhere.** KEDA's namespace must be known at install: the
NetworkPolicy, KEDA's Secret and its Role name it. The manifest assumes
`keda`. Elsewhere, use the `keda-namespace` kustomize component. Or edit
the file after verifying it, since the signature covers the file as
downloaded. For `keda-system`:

```bash
sed -i 's/^\(  *namespace: \)keda$/\1keda-system/; s/kubernetes.io\/metadata.name: keda$/kubernetes.io\/metadata.name: keda-system/' scaling-manager.yaml
```

Repeat the edit on every upgrade. On OpenShift, use the `openshift`
component (the service CA, `openshift-keda`).

**Air-gapped:** mirror the image by digest (`skopeo copy --all`), verify it
with `cosign verify` first, and add one `images:` entry. The controller is
the only image.

**Uninstall.** Every selector is scoped to this install's
`app.kubernetes.io/instance`:

```bash
I=app.kubernetes.io/instance=scaling-manager-system
P="kubectl patch configmap scaling-manager-settings -n scaling-manager-system --type merge -p"
$P '{"data":{"utilizationShare":"shadow"}}'                  # wait a cycle (15 s): no transfer marks left
kubectl delete scaledobject -A -l "$I,app.kubernetes.io/managed-by=llm-d-scaling-manager"
kubectl delete rolebinding,podmonitor,servicemonitor,serviceaccount,secret -A -l "$I"
kubectl delete clusterrolebinding -l "$I"
kubectl label namespace -l scaling-manager.llmd.ai/managed=true scaling-manager.llmd.ai/managed-
kubectl delete -f scaling-manager.yaml                       # with its namespace, as KEDA's and Kueue's do
kubectl get all,configmap,secret,role,rolebinding,servicemonitor,podmonitor,networkpolicy,scaledobject -A -l "$I"
kubectl get clusterrole,clusterrolebinding,clustertriggerauthentication -l "$I"   # nothing left
```

The label removal assumes one main-path install per cluster. With more
than one, remove the label only from the namespaces this install's
selector admits, which the status lists in `namespacesManaged`.

### G. Migration from `smt`

**From a cluster-wide install** (`scaling-manager-system`):

1. Keep the settings ConfigMap as it is. It is the same object. Take its
   switch keys out of any copy you put in git.
2. Label each namespace the install named with `--model-namespace`, and set
   `namespaceSelector`.
3. Apply `scaling-manager.yaml` server-side. The names, the namespace and
   the labels are those of today's cluster-wide files. The new TLS Secrets
   take the old CA, so KEDA's connection stays up, and KEDA's copy keeps its
   name.

**From a namespaced install,** either:

- uninstall it, keeping the ScaledObjects, and install as above; or
- stay namespaced on the advanced path. The admin writes `sm-admin/` (or
  runs `kubectl scaling-manager init`), and `kubectl diff -k` should show
  only labels, the empty plan ConfigMap and the KEDA objects that exist
  already.

An install made with `make deploy-scaling-manager` is not adopted, as for
`smt` today.

### H. The llm-d recipe path

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
  `scaling-manager.yaml`, and is applied with `kubectl apply --server-side
  -k`.
- **Platforms:** `k8s/` and `ocp/` overlays set KEDA's namespace (`keda`,
  or `openshift-keda`), as the keda-epp guide does.
- **A cluster prerequisite, not a per-guide install.** Like KEDA, it is
  installed once per cluster. A guide that uses it adds only its own entry
  to the config (its namespace in the selector), its namespace label and
  its ScaledObjects. Its cleanup removes only those three, as the guides
  policy asks of shared components.

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

#### How we install today

Three `smt` commands, from the tools image
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

**Without docker today:**

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
manifest, cut into `templates/` by CI, with three values: KEDA's namespace,
OpenShift, and the image repository. Config stays in the settings
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
from `config/install`, and keep the render-parity check against the
manifest.
### K. Verification and tests

- **A kind e2e for the main path,** `make test-e2e-manifest-install`, run on
  `test-e2e-release-manifests-setup` ([testing](../developer-guide/testing.md)).
  - It applies the manifest and the settings, labels the model namespace,
    and polls `ready`.
  - It registers a model from the plan and walks passive, shadow and active
    with the grant.
  - It runs the smoke suite, uninstalls, and runs the leftovers check.
  - A second leg upgrades from the previous release.
  - Images: the locally built controller, and the prerequisites from
    `registry.k8s.io` and `quay.io`; never docker.io.
  - Ginkgo runs with `-v`, and a 0-spec run fails.
- **GitOps:** apply the settings without the switch keys, flip them, and
  re-apply server-side: they hold. Phase 1 also runs Argo CD and Flux, and
  asserts the reset case: a switch left in git is overwritten.
- **The manifest:** `hack/check-release-manifests.sh` gains
  `scaling-manager.yaml`. It checks that the file is in `SHA256SUMS` and
  signed, pins the image by digest, carries the controller image only,
  holds no settings and no ScaledObject, and changes nothing on a re-apply.
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
copy of the layout with a controller namespace `llmsm-ctrl`. Phase 1
renders the real bases the same way, in the parity check.

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
  CRD's scope. Phase 1 checks that `kubectl apply` ignores the field, or
  gives kustomize the CRD's scope.

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

### N. Open questions

1. **Cluster-wide by default,** with the advanced path for least privilege?
2. **A git ref for llm-d recipes,** pinned to a release commit with the
   image by digest, although install-from-release ruled out a remote base?
3. **Helm:** lift the rule for a thin chart generated in CI, as the llm-d
   guides use for LeaderWorkerSet and Kueue?
4. **The cluster-wide `configmaps` read:** narrow it to the policy map's
   name, if every read allows it?
5. **The kustomize floor** for the advanced path: which oldest `kubectl`?
6. **The weights labeller:** it stays an `smt` and plugin step. Is a status
   key naming the patch enough?

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
