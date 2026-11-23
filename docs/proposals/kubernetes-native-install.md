# Install with kubectl alone, like the router

- **Status:** proposed. Nothing in it is built.
- **Audience:** contributors and maintainers. Operators install today from
  [install in a namespace](../guides/install-in-namespace/) and the
  [install commands](../reference/tools-install.md).
- **Builds on:** [Install from the release](install-from-release.md) (the
  signed release files, the settings ConfigMap, the preflight Job) and
  [Install in two commands](two-command-install.md) (`smt admin-setup`,
  `install`, `admin-finish`).
- **Needs a maintainer decision on:** a remote kustomize base, which
  [install-from-release](install-from-release.md) ruled out on 2026-11-10
  ("No Helm, no remote kustomize base, no operator"). See
  [Open questions](#open-questions).

## The ask

Install, configure, upgrade and uninstall the scaling manager the way an
operator installs the llm-d router: with Kubernetes tools, no docker and no
checkout. Today the short path runs a container (`smt`), and the path without
a container is about 20 `kubectl`, `curl` and `sed` lines.

## How the router installs today

The router (the endpoint picker, EPP) installs with **Helm**, from llm-d's own
OCI charts. Its CRDs are a separate `kubectl apply` of release YAML. There is
no operator, and no kustomize base for the EPP.

| question | what the router does | source |
| --- | --- | --- |
| mechanism | Helm. Two charts on ghcr.io: `oci://ghcr.io/llm-d/charts/llm-d-router-standalone` (EPP with an Envoy or agentgateway proxy) and `oci://ghcr.io/llm-d/charts/llm-d-router-gateway` (EPP, InferencePool, optionally an HTTPRoute), on a shared library chart `routerlib` | [charts README](https://github.com/llm-d/llm-d-router/blob/main/config/charts/README.md) |
| publishing | the release workflow pushes both charts; the chart version is stamped at release | [ci-build-images.yaml](https://github.com/llm-d/llm-d-router/blob/main/.github/workflows/ci-build-images.yaml) |
| config | values under `router.epp.*`: `pluginsConfig` (a structured EndpointPickerConfig written into a ConfigMap file, which replaces the built-in one), `pluginsCustomConfig` (raw ConfigMap entries), `pluginsConfigFile`, `flags` | [routerlib values.yaml](https://raw.githubusercontent.com/llm-d/llm-d-router/main/config/charts/routerlib/values.yaml) |
| layered values | the llm-d guides pass a base values file, then a monitoring and a per-guide one | [base.values.yaml](https://raw.githubusercontent.com/llm-d/llm-d/main/guides/recipes/router/base.values.yaml) |
| RBAC | namespaced Roles for pods, InferencePool, InferenceObjective, leases and events. ClusterRoles for TokenReview, SubjectAccessReview and `/metrics` by default; `router.monitoring.prometheus.auth.enabled=false` gives a namespace-only install | [charts README](https://github.com/llm-d/llm-d-router/blob/main/config/charts/README.md) |
| CRDs | not in the chart. `kubectl apply -f` of the Gateway API Inference Extension `v1-manifests.yaml` (InferencePool), and of the router release's `manifests.yaml` (InferenceObjective, InferenceModelRewrite) | [DEVELOPMENT.md](https://github.com/llm-d/llm-d-router/blob/main/DEVELOPMENT.md), [release v1.0.0](https://github.com/llm-d/llm-d-router/releases/tag/v1.0.0) |
| upgrade | `helm upgrade`. The chart README covers some in-place cases and says to gate on `kubectl wait` rather than `--wait`. CRDs: re-apply the release YAML | [charts README](https://github.com/llm-d/llm-d-router/blob/main/config/charts/README.md) |
| release assets | CRD YAML and SBOMs only; no EPP deployment YAML | [release v1.0.0](https://github.com/llm-d/llm-d-router/releases/tag/v1.0.0) |

The operator's commands, from the
[optimized-baseline guide](https://github.com/llm-d/llm-d/blob/main/guides/optimized-baseline/README.md)
(variables from [guides/env.sh](https://raw.githubusercontent.com/llm-d/llm-d/main/guides/env.sh),
where `ROUTER_CHART_VERSION=v0` is a floating major tag):

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/${GAIE_URL}/v1-manifests.yaml
kubectl create namespace ${NAMESPACE} --dry-run=client -o yaml | kubectl apply -f -
helm install ${GUIDE_NAME} oci://ghcr.io/llm-d/charts/llm-d-router-standalone \
  -f ${ROUTER_BASE_VALUES} ${MONITORING_VALUES} -f ${ROUTER_VALUES} \
  -n ${NAMESPACE} --version ${ROUTER_CHART_VERSION}
kubectl apply -n ${NAMESPACE} -k ${REPO_ROOT}/guides/${GUIDE_NAME}/modelserver/...   # the model servers: kustomize
helm uninstall ${GUIDE_NAME} -n ${NAMESPACE}                                           # cleanup
```

What makes it feel Kubernetes-native is not Helm itself. It is four things:

1. **One command per piece**, run with the operator's own kubeconfig. No
   container, no local scripts.
2. **Config is data**: one values file, rendered into one ConfigMap (the
   EndpointPickerConfig). The operator keeps that file in git.
3. **Upgrade is the same command with a new version.**
4. **Uninstall is one command**, and removes only what it installed.

It has no admin/owner split and no preflight; the chart installs what it
renders, with whatever rights the caller has.

Not confirmed: which version the `v0` chart tag resolves to, whether the
gateway chart renders RBAC differently from the standalone one, and the full
v1.0.0 release notes. The GitHub REST API refused unauthenticated requests;
the pages and the registry tag lists were read instead.

## How we install today

Three `smt` commands, from the tools image
([README](../../README.md#install), [install commands](../reference/tools-install.md)).
`smt` is a shell function around `docker run` of
`ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools@sha256:...`. The
entry point is `deploy/scaling-manager-tools.sh`; the install logic is
`deploy/lib/release_install.sh`, about 2,100 lines of bash over `kubectl`,
`jq` and `yq`.

### What each step does, in Kubernetes terms

| step | who | creates or changes | checks |
| --- | --- | --- | --- |
| `smt admin-setup -n CTRL [--model-namespace M ...]` | cluster admin | the `part=admin` objects of `scaling-manager-namespaced.yaml`: the ServiceAccount, the ClusterRoles and bindings named per install (node reader, Kueue reader, Namespace reader with `resourceNames`, metrics auth), the Roles, the controller ServiceMonitor, the scaler NetworkPolicy with KEDA's namespace filled in. The preflight's ServiceAccount, Role and ClusterRole and their bindings. In each model namespace, the model-namespace file: the controller's Role and RoleBinding and the model-server PodMonitor (port from `--modelserver-port`). An EPP ServiceMonitor, ServiceAccount, token Secret and `/metrics` binding where nothing scrapes the EPP. With `--dset-role-writer`, the `disaggregatedsets` grant. A stamp ConfigMap `scaling-manager-admin-setup`. The weights labeller's watch list | the namespace exists; KEDA's namespace (the live NetworkPolicy, else the external-metrics APIService); no other scaling manager manages a model namespace; no namespaced install beside a cluster-wide one; a re-run keeps the model namespaces the Namespace reader records |
| `smt install -n CTRL --prometheus-url URL [--quota ...] [--utilization-share ...]` | namespace owner, with the stock `admin` ClusterRole bound in CTRL | the settings ConfigMap `scaling-manager-settings` (created once, never overwritten). The Prometheus CA Secret. The preflight Job. The three empty scaler TLS Secrets (created once; the controller fills them). The `part=controller` objects: the Deployment (with `LLMSM_WATCH_NAMESPACE` set to the model namespaces), the Services, the controller-side config. The preflight Job again. With `--utilization-share active`, the pod-writer grant of `scaling-manager-activate.yaml` | `SHA256SUMS` of the release files and their release version; every flag against the keys the controller accepts; the stamp matches the release's admin objects (else exit 3, "ask the admin"); the http/https scheme against the live settings; the preflight Job's PASS/WARN/FAIL/UNKNOWN (a FAIL stops before the controller); the rollout |
| `smt admin-finish -n CTRL` | cluster admin | copies the client certificate Secret the controller minted into KEDA's namespace, applies `scaling-manager-keda-auth.yaml` (KEDA's ClusterTriggerAuthentication and the marker ConfigMap `scaling-manager-scaler-keda-auth`), restarts the controller when KEDA's side changed, deletes the preflight's ClusterRoleBinding | waits for the client certificate; KEDA's namespace from the stamp, agreeing with the NetworkPolicy; refuses a Secret of that name it did not write; waits for `scalerTLS: mutual TLS` in `scaling-manager-controller-status`; KEDA's stale connection (restart with `--restart-keda`); the preflight once more |
| `smt scaledobjects-plan -n M --controller-namespace CTRL --out plan.yaml` | either | nothing: writes a commented YAML plan of one ScaledObject per model server (per role on a DisaggregatedSet) | discovers the model servers and their InferencePools |
| `smt scaledobjects-apply -n M --plan plan.yaml` | either | the ScaledObjects in the plan; re-wires an HPA KEDA left on the CPU default | the plan parses; no warm pool set to apply |
| `smt workload-patch -n M [--apply]` | namespace owner | with `--apply`, patches the model servers (drain hook, caches) | reports what each model server still needs |
| `smt release-files --out DIR` | anyone | nothing: writes the release files the image carries | |

The preflight itself is not shell: it is the controller binary
(`/manager preflight check`) run as a Job from `scaling-manager-preflight.yaml`
([preflight](../reference/preflight.md)). The certificates are not shell
either: the controller mints the scaler CA and leaves at start
(`--external-scaler-tls-bootstrap`). What is shell is the order, the
substitutions, the checks between steps, and the KEDA copy.

### Why docker is needed

The tools image carries two things that must match:

- **the release files**, signed, with `SHA256SUMS`, so one `cosign verify` of
  the image covers them ([where the files come from](two-command-install.md#where-the-files-come-from));
- **the logic**: bash, `kubectl`, `jq`, `yq`, `openssl`, `column` and
  `diffutils` at pinned versions, so the commands behave the same on every
  machine.

Docker is the delivery vehicle for those, not a need of the install. It also
costs: no kubeconfig `exec` plugins inside the container (token kubeconfigs
only), `--network host` for a `127.0.0.1` API server, and a read-write mount
of the working directory.

### Without docker today

| path | what it needs | what is missing |
| --- | --- | --- |
| [By hand from the release files](../guides/install-in-namespace/#install-by-hand-from-the-release-files) | `kubectl`, `curl`, `sed` | about 20 commands; `sed` on `__NAMESPACE__` and `__MODEL_NAMESPACE__`; the settings edited in a file and created once; the KEDA step copied out of `scalerTLSAction`; no multi-namespace grant without editing the Namespace reader by hand |
| The tools tarball `scaling-manager-tools-<release>.tar.gz` ([prepare model servers](../guides/prepare-workloads/)) | bash, `kubectl`, `jq`, `yq`, `openssl` and `column` installed locally, at versions close to the image's | not signed as one unit with the files it installs; local tool versions vary; not tested as an install path |
| `make deploy-scaling-manager` | a checkout, `make`, `helm`, `yq`, `jq` | not a release install |

## Goal and constraint

**Goal.** Install, configure, upgrade and uninstall with `kubectl` only,
optionally `kustomize` or a `kubectl` plugin. No docker, no checkout. Config in
one place the operator keeps in git. The admin/owner split, the passive first
install and the preflight stay.

**Constraint.** [AGENTS.md](../../AGENTS.md) says: "the helm chart has been
removed. Do not re-introduce it or add helm chart features." The router
installs with Helm, so Helm is the closest match to it. It is still not the
primary design here: it is in its [own section](#alternative-helm-needs-a-maintainer-decision),
for the maintainers to decide on.

## Candidate designs

### 1. Release YAML and kustomize bases

Ship the install as kustomize bases and components, and let the operator
write a small `kustomization.yaml` per install, like a values file:

```text
config/install/
  namespaced/admin/          the part=admin objects of the namespaced file
  namespaced/owner/          the part=controller objects
  namespaced/preflight/      the preflight's RBAC (admin) and Job (owner)
  model-namespace/           one model namespace: Role, RoleBinding, PodMonitor
  cluster/                   the cluster-wide install
  components/
    params/                  replacements: the namespace into names, subjects, the NetworkPolicy, the KEDA marker
    openshift/               the OpenShift differences (service CA, openshift-keda)
    quota/                   settings: limiter quota, quotas
    passive/                 settings: passive "true" (the default)
    utilization-share/       settings: utilizationShare
    activate/                the pod-writer grant (one model namespace)
    dset-role-writer/        the disaggregatedsets grant (admin) and DSET_ROLE_WRITER (owner)
    multi-namespace/         the Namespace reader names and the watch list, from params
    kueue/                   the Kueue readers
```

The operator's one input file is a local-config ConfigMap (dropped from the
output by kustomize) that the `params` component reads:

```yaml
# params.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: scaling-manager-install
  annotations:
    config.kubernetes.io/local-config: "true"
data:
  namespace: team-a
  kedaNamespace: keda
  modelNamespaces: team-a         # space-separated; the controller's own when it runs beside them
```

The settings are a `configMapGenerator` with `behavior: merge` over the
shipped `scaling-manager-settings`. That is the router's EndpointPickerConfig
pattern: config is a file in the operator's repository, rendered into one
ConfigMap.

Two ways to get the bases:

- **A git ref:** `https://github.com/llm-d-incubation/llm-d-scaling-manager//config/install/namespaced/owner?ref=vX.Y.Z`.
  Needs `git` on the machine (kubectl's kustomize runs it). The image is
  pinned by tag, as a release checkout pins it today.
- **A release asset:** `scaling-manager-kustomize-<release>.tar.gz`, the same
  tree with the image pinned by digest, signed with the other files. Unpack it
  and point the kustomization at the local directory. Works air-gapped.

What it cannot do alone: run checks, wait for a step, or copy KEDA's
certificate. Design 2 covers those.

### 2. In-cluster Jobs for the procedural steps

Move each step that `smt` runs as shell into the controller binary, run as a
Job from the release files:

| Job | from | rights | replaces |
| --- | --- | --- | --- |
| `preflight` | `/manager preflight check` (exists) | its Role and ClusterRole (exist) | the `smt` preflight calls; already a Job |
| `keda-auth` | new `/manager keda-auth apply` | a Role in CTRL (get the client Secret, create the marker ConfigMap, patch the Deployment for the restart), a Role in KEDA's namespace (create, and get/update by name, one Secret), get/update by name on one ClusterTriggerAuthentication. Admin objects, deleted after the run like the preflight's binding | `admin-finish`'s copy, `llmsm_scaler_keda_auth_apply` in `deploy/lib/scaler_tls.sh` |
| `scaledobjects-plan` | new `/manager scaledobjects plan` | read of Deployments, LeaderWorkerSets, DisaggregatedSets and InferencePools in the model namespace; write of one ConfigMap | `deploy/lib/scaledobject.sh` discovery. Writes the plan, as ready-to-apply ScaledObject YAML with its comments, to ConfigMap `scaling-manager-scaledobjects-plan` in that namespace. The operator extracts it, edits it, and `kubectl apply -f` it |

The admin applies the `keda-auth` Job once the controller runs; it waits
for the client certificate itself, so the order between owner and admin
stops mattering. A controller-side alternative for the plan: the controller
writes the plan ConfigMap on each discovery, with no Job at all. It already
lists the model servers for supply; it would need write on one ConfigMap per
model namespace.

### 3. A kubectl plugin

`kubectl scaling-manager admin-setup|install|admin-finish|plan|apply ...`: a
static Go binary per platform, attached to the release with `SHA256SUMS` and
a signature, and installable with krew. It uses the operator's kubeconfig,
including `exec` plugins, so cloud logins work. It embeds the kustomize API
and the release's `config/install` tree, renders from the same bases as
design 1, and starts the Jobs of design 2 rather than carrying their logic.
The flags and exit codes of `smt` carry over.

### 4. Helm (an OCI chart)

The router's way. An alternative only:
[see its section](#alternative-helm-needs-a-maintainer-decision).

### Compared

| | 1. kustomize | 2. Jobs | 3. plugin | 4. Helm |
| --- | --- | --- | --- | --- |
| operator steps, one namespace | admin: 1 apply. Owner: 1 apply. Admin: the KEDA step by hand | admin: 2 applies (objects, then the `keda-auth` Job). Owner: 1 apply. Preflight: one Job, logs | 3 commands, as `smt` | admin: 1 `helm install` of an admin chart. Owner: 1 `helm install` |
| admin/owner split | two kustomizations: `admin` and `owner` bases | admin Jobs and owner Jobs | as `smt` | not natural: one release renders both, unless split into two charts |
| config surface | one `params.yaml` and a settings generator in the operator's repo | none of its own | flags, as `smt` | one values file |
| upgrade | change `?ref=` (or the tarball), apply again | re-run the Jobs of the new release | the new binary, same commands | `helm upgrade` |
| preflight | not alone | yes: the Job, as today | yes: starts the Job | a `helm test` hook, or a pre-install hook Job |
| multi-namespace | one model-namespace kustomization per namespace (kustomize has no loop); the plugin generates them | the Jobs take the list | `--model-namespace`, as `smt` | a list in values; a `range` renders per namespace |
| OpenShift | the `openshift` component | same Jobs | same | a values switch |
| effort | about 2 weeks | about 3 weeks (two Go ports) | about 2 weeks on top of 1 and 2 | about 2 weeks, plus the rule |

## Recommendation

**Designs 1 and 2 as the base, design 3 for convenience.** Kustomize gives
the router's "config is a file, upgrade is a new version" with plain
`kubectl`. The Jobs keep what makes our install safe, the preflight and the
KEDA step, inside the cluster, run by the controller binary that already
carries the preflight. The plugin then becomes a thin wrapper that a person
can skip.

The examples use a git ref and a release `v0.13.0`. With the release
tarball, each URL becomes a path into the unpacked directory. The tenant
names (`team-a`, `team-b`) are placeholders.

### One namespace

The admin's directory, `sm-admin/` (kept by the admin):

```yaml
# sm-admin/kustomization.yaml
resources:
- params.yaml
- https://github.com/llm-d-incubation/llm-d-scaling-manager//config/install/namespaced/admin?ref=v0.13.0
- https://github.com/llm-d-incubation/llm-d-scaling-manager//config/install/namespaced/preflight/admin?ref=v0.13.0
- https://github.com/llm-d-incubation/llm-d-scaling-manager//config/install/model-namespace?ref=v0.13.0
- https://github.com/llm-d-incubation/llm-d-scaling-manager//config/install/namespaced/keda-auth?ref=v0.13.0
components:
- https://github.com/llm-d-incubation/llm-d-scaling-manager//config/install/components/params?ref=v0.13.0
```

The owner's directory, `sm-owner/` (kept by the namespace owner, the same
`params.yaml`):

```yaml
# sm-owner/kustomization.yaml
resources:
- params.yaml
- https://github.com/llm-d-incubation/llm-d-scaling-manager//config/install/namespaced/owner?ref=v0.13.0
components:
- https://github.com/llm-d-incubation/llm-d-scaling-manager//config/install/components/params?ref=v0.13.0
configMapGenerator:
- name: scaling-manager-settings
  behavior: merge
  literals:
  - prometheusURL=https://prometheus.monitoring.svc:9090
```

Then:

`sm-admin/` also lists `namespaced/keda-auth`, the `keda-auth` Job and its
RBAC. Each step's objects carry a new label, `scaling-manager.llmd.ai/step`,
so one directory can be applied a step at a time. Then:

```bash
S=scaling-manager.llmd.ai/step
kubectl apply -k sm-admin/ -l "$S!=keda-auth"                                    # admin: RBAC, monitors, NetworkPolicy, preflight RBAC
kubectl apply -k sm-owner/ -l "$S=preflight"                                     # owner: the settings and the preflight Job
kubectl logs -f -n team-a job/scaling-manager-preflight                          # PASS, WARN, FAIL or UNKNOWN per check, and the fix
kubectl apply -k sm-owner/                                                       # owner: the controller
kubectl apply -k sm-admin/ -l "$S=keda-auth"                                     # admin: the keda-auth Job; it waits for the client certificate
kubectl logs -f -n team-a job/scaling-manager-keda-auth                          # ends with "mutual TLS"
kubectl delete -k sm-admin/ -l "$S=keda-auth,scaling-manager.llmd.ai/part=admin" # admin: drop the Job's rights
kubectl delete clusterrolebinding scaling-manager-preflight-team-a               # admin: the preflight's cluster reads, as today
```

The preflight Job is created again for each run, as today: delete it first
(`kubectl delete job scaling-manager-preflight -n team-a`), because a Job's
template cannot change.

### Several namespaces, one budget

The controller runs in `scaling-manager-system`; models in `team-a` and
`team-b`. `params.yaml` sets `namespace: scaling-manager-system` and
`modelNamespaces: team-a team-b`; the `multi-namespace` component writes
them into the Namespace reader's `resourceNames` and the controller's watch
list. `sm-admin/kustomization.yaml` drops `model-namespace` (the
controller's namespace holds no model servers). The admin adds one small
kustomization per model namespace instead, because kustomize cannot loop.
Each holds a copy of `params.yaml`, since kustomize does not read files
above a kustomization's own directory:

```yaml
# sm-admin/team-a/kustomization.yaml (and the same for team-b)
namespace: team-a
resources:
- params.yaml
- https://github.com/llm-d-incubation/llm-d-scaling-manager//config/install/model-namespace?ref=v0.13.0
components:
- https://github.com/llm-d-incubation/llm-d-scaling-manager//config/install/components/params?ref=v0.13.0
```

The owner's settings add the shared budget:

```yaml
configMapGenerator:
- name: scaling-manager-settings
  behavior: merge
  literals:
  - prometheusURL=https://prometheus.monitoring.svc:9090
  - limiter=quota
  - quotas=NVIDIA-H100-80GB-HBM3=16
  - quotaScope=cluster
```

```bash
kubectl create namespace scaling-manager-system                                  # admin
kubectl apply -k sm-admin/ && kubectl apply -k sm-admin/team-a/ && kubectl apply -k sm-admin/team-b/   # admin
# then the owner's preflight, controller, and the admin's keda-auth Job, as above
```

`kubectl scaling-manager admin-setup -n scaling-manager-system --model-namespace team-a --model-namespace team-b`
writes these directories for the admin, instead of applying them, with
`--out DIR`.

### Passive utilization share on plain Kubernetes with KEDA

The install is passive by default. Shadow sharing is two more settings
literals in the owner's file:

```yaml
  - limiter=quota
  - quotas=NVIDIA-H100-80GB-HBM3=16
  - utilizationShare=shadow
```

Going active is a change to the same file, plus the admin's one grant of
`patch` on pods per model namespace, which stays a separate admin step
([decision 3 of install-from-release](install-from-release.md#decisions-2026-11-10)):

```bash
# admin, per model namespace: the activate component
kubectl apply -k sm-admin/team-a-activate/
# owner: passive=false, utilizationShare=active in sm-owner/kustomization.yaml, then
kubectl apply -k sm-owner/
```

The live `kubectl patch` of the settings still works, but the next apply
writes the file's value back. In this path the file is the source of truth,
as the router's values file is.

### A DisaggregatedSet

Roles with `scaling.mode: Static` need the role writer. The admin adds the
`dset-role-writer` component to `sm-admin/` (the `disaggregatedsets` grant
in each model namespace); the owner adds the same component to `sm-owner/`
(`DSET_ROLE_WRITER=true` on the controller). The plan then has one
ScaledObject per role:

The `model-namespace` base carries the plan Job and its read-only RBAC,
labelled `step=scaledobjects-plan`:

```bash
kubectl apply -k sm-admin/team-a/ -l "$S=scaledobjects-plan"                     # the plan Job
kubectl logs -f -n team-a job/scaling-manager-scaledobjects-plan                 # the table of what it found
kubectl get configmap scaling-manager-scaledobjects-plan -n team-a \
  -o jsonpath='{.data.plan\.yaml}' > plan-team-a.yaml                            # edit the bounds
kubectl apply -f plan-team-a.yaml
```

### Admin manifests and owner manifests

The split is by base, not by label, so each person applies only what they
own:

| applied by | bases |
| --- | --- |
| cluster admin | `namespaced/admin`, `namespaced/preflight/admin`, `model-namespace` (one per namespace), `keda-auth`, the `activate` and `dset-role-writer` grants, `kueue` |
| namespace owner | `namespaced/owner` (the settings, the Deployment, the Services, the empty scaler TLS Secrets), `namespaced/preflight/owner` (the Job) |

The `scaling-manager.llmd.ai/part` label stays on every object, so the release
files and the `smt` path keep working, and `kubectl apply -l` still selects
either part from a single file.

### What moves out of `smt`, and what stays

| | moves | stays in `smt` |
| --- | --- | --- |
| rendering the objects | into `config/install` bases; `release-manifests.sh` renders the release files from them | `smt` applies the release files, now rendered from the same bases |
| namespace substitution | the `params` component | `sed` on the release files, unchanged |
| preflight | already a Job | `smt` keeps running it and gating on it |
| KEDA client certificate | the `keda-auth` Job (`/manager keda-auth apply`) | `admin-finish` runs the same Job, then waits for `mutual TLS` |
| ScaledObject plan | the plan Job | `scaledobjects-plan` runs the same Job and copies the ConfigMap out |
| stamp, exit codes, re-run safety, `--dry-run` | the stamp becomes optional: the Jobs check the live admin objects | `smt` keeps them |
| `prepull`, `weights`, `engine-cache`, `workload-patch` | nothing | unchanged |

Tools-image parity: one set of release files, rendered from one tree, feeds
the tools image, the release assets and the kustomize tarball. `smt` keeps
every command and flag.

### What the controller and the release need

- **The scaler TLS Secrets under `apply`.** The release file ships them with
  empty `data` keys and is created once, because a second `kubectl apply`
  would empty the certificates the controller wrote. Under kustomize they are
  applied every time. Either the controller creates them itself (`create` on
  secrets in its own namespace, `get`/`update` by name as today), or they move
  out of the owner base into a `create`-only step. The first is simpler for
  GitOps; it widens the controller's Role by one verb.
- **Settings applied, not created once.** The controller already reads them
  live; nothing changes in it. The guides change: edit the file and apply.
- **Two new subcommands** of `/manager`: `keda-auth apply` and
  `scaledobjects plan`, ports of `deploy/lib/scaler_tls.sh` and
  `deploy/lib/scaledobject.sh`.
- **The release** attaches `scaling-manager-kustomize-<release>.tar.gz` and,
  from phase 3, the plugin binaries, all in `SHA256SUMS` and signed.

## Migration

An install made with `smt` or by hand from the release files carries the same
names, namespaces and labels (`app.kubernetes.io/part-of`, `instance`,
`version`, `scaling-manager.llmd.ai/part`). Applying the new bases over it
updates the objects in place:

1. The admin writes `sm-admin/` with the same namespace, KEDA namespace and
   model namespaces, and runs `kubectl diff -k sm-admin/` first. The diff
   should show only the version label.
2. The owner copies the live settings into the generator (one line:
   `kubectl get configmap scaling-manager-settings -n NS -o yaml`), then runs
   `kubectl diff -k sm-owner/`. A key missing from the file keeps the shipped
   default on apply, so the copy is required, and the guide says so.
3. Apply admin, then owner. The scaler TLS Secrets, the marker and KEDA's
   copy are untouched, so mutual TLS stays up and KEDA needs no restart.
4. The stamp ConfigMap stays; `smt` can still run against the install.

With client-side `kubectl apply`, the first apply over an object `smt`
created with `kubectl apply` reuses its last-applied annotation. Over an
object `smt` created with `kubectl create` (the settings), the first apply
warns once about the missing annotation and adds it.

An install made with `make deploy-scaling-manager` is not adopted, as for
`smt` today.

**Upgrade.** Change `?ref=` (or the tarball) in both directories; run the
preflight, the admin apply, the owner apply. CRDs: none of ours. The router's
and KEDA's stay the operator's, as today.

**Uninstall.** `kubectl delete -k sm-owner/`, then `kubectl delete -k
sm-admin/`. No base holds a Namespace or the policy ConfigMap, so neither is
deleted, and every ClusterRole is named per install. The keep-list of the
[cleanup](../guides/install-in-namespace/#cleanup) (the scaler CA for a
reinstall) becomes a base the delete leaves out.

## Verification

- **Render parity, in `make lint-deploy-scripts`.** A new
  `hack/check-kustomize-install.sh`: for a fixed namespace, model-namespace
  list and KEDA namespace, `kubectl kustomize` of the admin and owner bases
  must equal the release files after `sed`, object for object
  (`yq`-normalised). It asserts both sides are non-empty and counts the
  objects, so two empty renders cannot pass. Its negative control: a base
  with one rule removed must fail.
- **An e2e for the kubectl-only path, in kind.** `make
  test-e2e-kustomize-install`, beside `test-e2e-release-manifests` and
  `test-e2e-tools-install`, on the same `test-e2e-release-manifests-setup`
  prerequisites ([testing](../developer-guide/testing.md)). It applies the
  bases from the local tree (a `file://` ref, so a PR tests its own bases),
  runs the preflight Job, the controller, the `keda-auth` Job, the plan Job,
  and the smoke suite, then the uninstall and an object-leftover check. One
  leg per scope, and a second run that upgrades from the previous release's
  tarball. Images: the locally built controller loaded into kind, the
  prerequisites from `registry.k8s.io` and `quay.io` as today; no docker.io
  image. Run with `-v` and fail on a 0-spec run.
- **Release asset checks.** `hack/check-release-manifests.sh` gains the
  tarball: in `SHA256SUMS`, signed, the image pinned by digest, the same
  digest as the YAML files. `hack/check-tools-image.sh` checks the image
  still carries the same files. From phase 3, the plugin binaries: checksums,
  signatures, and `kubectl scaling-manager version` printing the release.

## Phases

| phase | what | effort |
| --- | --- | --- |
| 1 | `config/install` bases and components; `release-manifests.sh` renders from them; the parity check; the tarball asset; the guides gain a kustomize tab. The scaler TLS Secret change | 2 weeks |
| 2 | `/manager keda-auth apply` and `/manager scaledobjects plan` as Jobs; `smt` calls them; the kind e2e | 3 weeks |
| 3 | the `kubectl scaling-manager` plugin, krew manifest, release binaries | 2 weeks |
| 4 | the README leads with the kubectl path; `smt` stays documented beside it | 2 days |

Phases 1 and 2 are independent and can run in parallel. Seven weeks of work
in all, about five elapsed.

## Open questions

1. **The remote kustomize base.** Install-from-release ruled it out on
   2026-11-10. This proposal brings it back as one of two ways to get the
   bases, beside a signed tarball. Keep both, the tarball only, or neither?
2. **The image pin on the git ref.** A tag ref pins the image by tag (the
   digest is not known when the tag is cut). Acceptable as on a release
   checkout, or should the guides lead with the digest-pinned tarball?
3. **The scaler TLS Secrets.** Let the controller create them (one more verb
   on its own namespace's secrets), or keep a `create`-only step?
4. **Kustomize ordering.** The `params` component sets names and namespaces
   by `replacements`. Confirm, in phase 1, that the kustomize bundled with
   the oldest `kubectl` we support applies a remote component's replacements
   as the parity check expects.
5. **The plan: a Job or the controller?** A controller that writes the plan
   ConfigMap on each discovery needs no Job, but gains one write per model
   namespace.
6. **Settings drift.** Under apply, a live `kubectl patch` is reverted by the
   next apply. Is that the behaviour we want to document, or should the
   settings stay out of the owner base and be created once, as today?

## Alternative: Helm (needs a maintainer decision)

AGENTS.md forbids a Helm chart, so this is not recommended. It is here
because the router installs with Helm, and the evidence for it is real:

- every llm-d operator already runs `helm` for the router, so a chart adds
  no tool;
- the router's values file is the closest match to "config in one file";
- `helm upgrade` and `helm uninstall` give upgrade and removal for free;
- a `range` over the model namespaces is simpler than one kustomization per
  namespace.

Against it, and why the native design is still preferred:

- **The admin/owner split.** One release renders both halves with the
  caller's rights. It needs two charts, or one chart installed twice with
  `--set part=admin`, which is design 1 with a template language.
- **Created-once objects.** Helm owns every object it renders; the settings
  and the scaler TLS Secrets need `helm.sh/resource-policy: keep` and
  lookups, which `helm template` and GitOps renderers do not run.
- **The preflight and KEDA steps** become hook Jobs; hooks do not run under
  `helm template`, which Argo CD and Flux's post-renderers commonly use.
- **The rule.** A chart was removed once; bringing it back is a maintainer
  call, not a design detail.

If the maintainers lift the rule, the chart should be a thin wrapper over the
same `config/install` bases (rendered into `templates/` at release), pushed
as `oci://ghcr.io/llm-d-incubation/charts/llm-d-scaling-manager`, with two
charts for the two halves, the preflight and `keda-auth` as hook Jobs, and the
same render-parity check against the release files.
