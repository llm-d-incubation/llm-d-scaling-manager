# Install with kubectl and kustomize, without docker

- **Status:** proposed. Nothing in it is built.
- **Audience:** contributors and maintainers. Operators install today from
  [install in a namespace](../guides/install-in-namespace/) and the
  [install commands](../reference/tools-install.md). The walkthroughs below
  are the draft of a future guide, `docs/guides/kubectl-install/`, written
  from a `guide.yaml`. The [For developers](#for-developers) part is
  developer-facing.
- **Builds on:** [Install from the release](install-from-release.md) (the
  signed release files, the settings ConfigMap, the preflight Job) and
  [Install in two commands](two-command-install.md) (`smt admin-setup`,
  `install`, `admin-finish`).
- **Needs a maintainer decision on:** shipping kustomize bases as a release
  asset ([install-from-release](install-from-release.md) ruled out a remote
  kustomize base on 2026-11-10), and the Helm rule in
  [AGENTS.md](../../AGENTS.md). See [Open questions](#open-questions).

## Summary

- **The ask:** install, configure, upgrade and uninstall with Kubernetes
  tools only, as llm-d's own components install. No docker, no checkout.
- **What the operator gets:** no docker, no checkout (for an operator
  outside the llm-d guides), and an install that lives in git as plain
  kustomizations, which Argo CD or Flux can sync.
- **What it costs:** more commands than `smt`.
  - One namespace is about 16 `kubectl` commands, plus a `kubectl diff`
    before each apply, and 11 small kustomization files, against `smt`'s 3
    commands.
  - Several namespaces with a DisaggregatedSet and shadow sharing is about
    27 commands and 16 files.
  - A `kubectl` plugin can write the files and bring it back to 3 commands.
- **The recommendation:**
  - kustomize bases and components, shipped as a signed release asset;
  - the procedural steps run as Jobs from the controller image;
  - a `kubectl` plugin for convenience;
  - `smt` keeps working on the same release files.

## How llm-d installs its pieces today

### The router

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

### An autoscaler, and the model servers

The same guides install everything else with kustomize.

| piece | how the llm-d guides install it | source |
| --- | --- | --- |
| an autoscaler controller | a kustomize overlay in the guides repository over a **remote git-ref base** in the controller's repository (`github.com/llm-d/llm-d-workload-variant-autoscaler/config/overlays/namespace-scoped/kubernetes?ref=release-0.9`), with `namespace:`, a Deployment patch and an `images:` pin | [wva/controller/platform/k8s/kustomization.yaml](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/wva/controller/platform/k8s/kustomization.yaml) |
| ScaledObjects | checked-in overlays, each with a full `scaledobject.yaml` (KEDA's Prometheus trigger), applied with `kubectl kustomize <overlay> \| envsubst ... \| kubectl apply -f -` | [keda-epp README](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/keda-epp/README.md) |
| model-server monitoring | kustomize components with PodMonitors: `recipes/modelserver/components/monitoring` (decode, `decode-podmonitor.yaml`), `monitoring-pd` (P/D) and `monitoring-epd` | [the component](https://github.com/llm-d/llm-d/tree/main/guides/recipes/modelserver/components/monitoring) |
| guide CI | a kustomize dry-run job covers every overlay of a tier-2 guide | [GUIDES-POLICY.md](https://github.com/llm-d/llm-d/blob/main/guides/GUIDES-POLICY.md) |

So llm-d is Helm for the router and kustomize for the rest, with a remote
git-ref base already the precedent for a controller.

### What makes these installs feel native

1. One command per piece, run with the operator's own kubeconfig.
2. Config is data: a values file or a kustomization in the operator's repo.
3. Upgrade is the same command with a new version.
4. Uninstall is one command.

Neither has an admin/owner split or a preflight. Ours has both, and keeps
both.

llm-d's decode PodMonitor, read in full, selects `llm-d.ai/role: decode`
and scrapes port `modelserver` at `/metrics` every 30s; `monitoring-pd`
and `monitoring-epd` sit beside it. Not confirmed: which version the `v0`
chart tag resolves to, and the full router release notes. The GitHub REST API refused unauthenticated requests;
the pages, raw files and registry tag lists were read instead.

## How we install today

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

## Goal and constraint

**Goal.** `kubectl` only, optionally `kustomize` or a `kubectl` plugin. No
docker, no checkout. Config in files the operator keeps in git. The
admin/owner split, the passive first install and the preflight stay.

**Constraint.** [AGENTS.md](../../AGENTS.md): "the helm chart has been
removed. Do not re-introduce it or add helm chart features." The router
installs with Helm; llm-d's autoscaler recipe installs with kustomize. The
design here follows the second, and Helm has
[its own section](#alternative-helm-needs-a-maintainer-decision).

## Candidate designs

1. **Kustomize bases and components.** The release ships `config/install`,
   rendered with the image digest, as a signed tarball. The operator writes
   small kustomizations over it, one per part.
2. **Jobs for the procedural steps.** The preflight (exists), the KEDA
   certificate copy, run from the controller image; the controller publishes
   the ScaledObject plan.
3. **A `kubectl` plugin**, `kubectl scaling-manager`: a static Go binary,
   krew-installable, that writes the kustomizations of design 1 and runs the
   Jobs of design 2.
4. **Helm (an OCI chart).** Alternative only.

| | 1. kustomize | 1 + 2 | 1 + 2 + 3 | 4. Helm |
| --- | --- | --- | --- | --- |
| commands, one namespace | about 10, plus the KEDA step by hand | about 16 | 3, as `smt` | 2 `helm install` (admin chart, owner chart), plus the preflight |
| files the operator keeps | 8 small kustomizations | 11 | 11, written by the plugin | 2 values files |
| admin/owner split | separate directories | the same; the Jobs' grants in a third, temporary one | as `smt` | two releases |
| config surface | a settings generator, list-valued patches, components | the same | flags, as `smt` | values |
| upgrade | point at the new tarball, `diff`, apply | the same, plus the versioned Jobs | the new binary, same commands | `helm upgrade` |
| preflight | no | the Job, as today | the Job | a hook Job |
| multi-namespace | one small kustomization per namespace | the same | `--model-namespace` | a list in values |
| OpenShift | the `openshift` component | the same | `--platform` | a values switch |
| GitOps | yes: the steady-state directories | yes; the Jobs stay out of the sync | yes, from the written files | yes, through Flux helm-controller or Argo CD |
| effort | 2 weeks | 6 weeks | 8 weeks | 2 weeks, plus the rule |

## Recommendation

**Designs 1 and 2 as the base, design 3 for convenience.** The gain over
`smt` is no docker, no checkout (for an operator outside the llm-d guides),
and an install that is plain files under version control. It is not fewer
commands; the plugin is for that.

What follows is written as the future guide would read. `team-a` and
`team-b` are placeholder namespaces; `v0.13.0` is a placeholder release.

### You need

- llm-d model servers, and the router. Decide how the EPP is scraped
  ([monitoring](#monitoring)). For scale from zero, the router's flow
  control values too (`featureGates: [flowControl]`).
- The router v1.0 or later; InferencePool from `inference.networking.k8s.io`
  (v1) or `inference.networking.x-k8s.io` (v1alpha2). The release notes state
  the tested range of each release.
- KEDA 2.18 or later (2.21 recommended), and the Prometheus Operator. On GKE
  with Managed Prometheus, llm-d uses `PodMonitoring` instead; our monitors
  are Prometheus Operator objects, so leave the monitoring components out
  and scrape with GMP.
- For a DisaggregatedSet: LeaderWorkerSet v0.10.0 or later.
- Your GPU product: the `GPU.PRODUCT` column of
  `kubectl get nodes -L nvidia.com/gpu.product`.
- `kubectl` with kustomize components and `labelSelector` patch targets.
  The examples were rendered with kubectl v1.31 (kustomize v5.4.2); phase 1
  sets the oldest supported version.
- `cosign` 3.0 or later.
- A cluster admin and a namespace owner.

The commands use these variables:

```bash
V=v0.13.0
CTRL=team-a                        # where the controller runs
MODEL_NAMESPACES="team-a"          # every namespace with model servers
KEDA_NS=keda                       # where KEDA runs
```

### Get the release

```bash
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V
curl -fsSLO $URL/scaling-manager-kustomize-$V.tar.gz
curl -fsSLO $URL/scaling-manager-kustomize-$V.tar.gz.sigstore.json
cosign verify-blob scaling-manager-kustomize-$V.tar.gz \
  --bundle scaling-manager-kustomize-$V.tar.gz.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/$V"
tar -xzf scaling-manager-kustomize-$V.tar.gz          # unpacks to scaling-manager-$V/
```

Verify before unpacking. The tarball pins every image by digest. A git ref
pinned to a release commit is the other way in: for development, and for an
llm-d recipe ([the llm-d path](#the-llm-d-path)).

### What the release ships

`config/install/` replaces `config/overlays/` as the one render tree. The
release files (`deploy/release-manifests.sh`), `make deploy-scaling-manager`
and the tarball all render from it.

```text
scaling-manager-v0.13.0/
  controller-admin/        part=admin, controller namespace: ServiceAccount, Roles, controller
                           ServiceMonitor, scaler NetworkPolicy (KEDA in "keda"), the empty marker,
                           the empty plan ConfigMap
  cluster-admin/           the controller's ClusterRoles and ClusterRoleBindings
  model-namespace/         one model namespace: the controller's Role and RoleBinding
  owner-settings/          the settings ConfigMap, with the shipped defaults
  owner/                   the Deployment and the Services
  keda-auth-setup/         KEDA's Secret with no data (KEDA's namespace), the ClusterTriggerAuthentication
  scaler-tls/              the three scaler TLS Secrets, created once, never applied
  switches/                the scaling policy: passive, sharing off. Created once, never applied
  steps/
    preflight/             the preflight Job and all its RBAC (temporary)
    keda-auth/keda/        the keda-auth Job, its ServiceAccount and Role, in KEDA's namespace (temporary)
    keda-auth/controller/  its Role in the controller namespace (temporary)
  components/
    openshift/             the service CA, openshift-keda
    modelserver-monitoring/  the model-server PodMonitor, port "modelserver"
    epp-monitoring/        an EPP ServiceMonitor scraping with a token, its ServiceAccount, Secret, binding
    prometheus-ca/         mounts Secret scaling-manager-prometheus-ca, sets prometheusCACertPath
    allow-http/            plain-HTTP Prometheus: no token, prometheusAllowHTTP
    quota/                 limiter quota (the GPUs go in the operator's generator)
    kueue/                 the Kueue readers
    cluster-pods/          list pods cluster-wide, for the gpu-inventory limiter
    dset-role-writer-admin/  get, list, patch on disaggregatedsets (one model namespace)
    dset-role-writer/      DSET_ROLE_WRITER=true on the controller
    activate/              the pod-writer grant (one model namespace)
  scaledobject/external/   a ScaledObject overlay with the external trigger, for a model added by hand
```

Every object keeps `scaling-manager.llmd.ai/part` (admin or controller) and
the `app.kubernetes.io` labels of today's release files.

### One namespace

The controller runs beside the models: `CTRL=team-a`,
`MODEL_NAMESPACES=team-a`. The admin keeps `sm-admin/` and `sm-steps/`; the
owner keeps `sm-owner/`. Paths are relative to the directory that holds the
unpacked release. That is 11 small files;
`kubectl scaling-manager init --namespace team-a --out .` writes them.

| directory | `namespace:` | holds |
| --- | --- | --- |
| `sm-admin/controller-namespace/` | | a component: sets the controller's namespace in binding subjects |
| `sm-admin/` | | `resources:` the four directories below |
| `sm-admin/controller/` | `team-a` | `controller-admin`, plus the component |
| `sm-admin/cluster/` | `team-a` | `cluster-admin`, `nameSuffix: -team-a`, plus the component |
| `sm-admin/team-a/` | `team-a` | `model-namespace`, the monitoring components you need, plus the component |
| `sm-admin/keda/` | `keda` | `keda-auth-setup`, `nameSuffix: -team-a` |
| `sm-steps/preflight/` | `team-a` | `steps/preflight`, `nameSuffix: -team-a` |
| `sm-steps/keda-auth/keda/` | `keda` | `steps/keda-auth/keda`, `nameSuffix: -team-a` |
| `sm-steps/keda-auth/controller/` | `team-a` | `steps/keda-auth/controller`, and a patch naming the Job's ServiceAccount in `keda` |
| `sm-owner/settings/` | `team-a` | `owner-settings`, and the settings generator |
| `sm-owner/` | `team-a` | `settings/` and `owner` |

Four of them written out:

```yaml
# sm-admin/controller-namespace/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
patches:
- target: {kind: RoleBinding, labelSelector: scaling-manager.llmd.ai/subject=controller}
  patch: |-
    - {op: replace, path: /subjects/0/namespace, value: team-a}
- target: {kind: ClusterRoleBinding, labelSelector: scaling-manager.llmd.ai/subject=controller}
  patch: |-
    - {op: replace, path: /subjects/0/namespace, value: team-a}
```

```yaml
# sm-admin/team-a/kustomization.yaml: a model namespace
namespace: team-a
resources:
- ../../scaling-manager-v0.13.0/model-namespace
components:
- ../controller-namespace
- ../../scaling-manager-v0.13.0/components/modelserver-monitoring   # see Monitoring
```

```yaml
# sm-owner/settings/kustomization.yaml
namespace: team-a
resources:
- ../../scaling-manager-v0.13.0/owner-settings
configMapGenerator:
- name: scaling-manager-settings
  behavior: merge
  options:
    disableNameSuffixHash: true
  literals:
  - prometheusURL=https://prometheus.monitoring.svc:9090
```

```yaml
# sm-owner/kustomization.yaml
namespace: team-a
resources:
- settings
- ../scaling-manager-v0.13.0/owner
```

The `controller-namespace` component is needed because a kustomization's
`namespace:` does not move a binding's subject to another build's
ServiceAccount. The scratch renders that show it are in
[For developers](#what-the-renders-showed).

**The steps.** Run `kubectl diff -k` before every `kubectl apply -k`.

```bash
kubectl apply -k sm-owner/settings/                                     # owner: the settings first
kubectl create -k scaling-manager-$V/scaler-tls -n $CTRL                # owner, once: the empty scaler TLS Secrets
kubectl create -k scaling-manager-$V/switches -n $CTRL                  # owner, once: passive, no sharing
kubectl apply -k sm-steps/preflight/                                    # admin: preflight run 1
kubectl logs -f -n $CTRL job/scaling-manager-preflight-${V//./-}-$CTRL  # podmonitor.port, metrics.model FAIL: expected on run 1
kubectl apply -k sm-admin/                                              # admin
kubectl apply -k sm-owner/                                              # owner: the controller
kubectl apply -k sm-steps/keda-auth/controller/                         # admin: the keda-auth Job's reads in $CTRL
kubectl apply -k sm-steps/keda-auth/keda/                               # admin: the Job, in KEDA's namespace
kubectl logs -f -n $KEDA_NS job/scaling-manager-keda-auth-${V//./-}-$CTRL   # ends with "mutual TLS"
kubectl delete job -n $CTRL scaling-manager-preflight-${V//./-}-$CTRL
kubectl apply -k sm-steps/preflight/                                    # admin: preflight run 2, everything checked
kubectl logs -f -n $CTRL job/scaling-manager-preflight-${V//./-}-$CTRL
kubectl delete -k sm-steps/keda-auth/keda/ ; kubectl delete -k sm-steps/keda-auth/controller/
kubectl delete -k sm-steps/preflight/                                   # admin: the temporary rights go
```

**Preflight run 1** is before any admin object and before the controller.
Expected then:

- UNKNOWN: `rbac.controller` (no ServiceAccount yet), `keda.mtls`,
  `scaler.tls`, and `settings` when `watchNamespaces` is set (no Deployment
  to compare with).
- WARN: `keda.networkpolicy` and `rbac.namespaces` (not applied yet).
- FAIL, unless something else already scrapes the model servers:
  `podmonitor.port` and `metrics.model`. The admin apply adds the
  PodMonitor. Phase 2 adds a preflight mode for run 1 that reports these
  as WARN.

The run-1 checks that matter are the refusals: another scaling manager in a
model namespace, or a namespaced install beside a cluster-wide one. Those
checks are new preflight work ([below](#the-preflight-conflict-checks)).
Run 2 checks everything.

`sm-steps/` is applied by hand and never kept in a GitOps sync: a sync would
bring the temporary rights back. `sm-admin/` and `sm-owner/` are the steady
state.

### Monitoring

Each scrape must exist once. Pick per source:

| source | where llm-d already scrapes it | where nothing scrapes it |
| --- | --- | --- |
| model servers | llm-d's kustomize components [`monitoring`](https://github.com/llm-d/llm-d/tree/main/guides/recipes/modelserver/components/monitoring) (decode: selects `llm-d.ai/role: decode`, port `modelserver`, `/metrics`, every 30s), `monitoring-pd` (P/D, which the pd-disaggregation guide applies) and `monitoring-epd`. Leave ours out | `components/modelserver-monitoring` (port `modelserver`; another port is a strategic-merge patch on the PodMonitor) |
| the EPP | the router installed with llm-d's [`monitoring.values.yaml`](https://raw.githubusercontent.com/llm-d/llm-d/main/guides/recipes/router/features/monitoring.values.yaml): its own ServiceMonitor, with Prometheus auth off. Leave ours out | the router installed with its auth default (on) and no monitoring values: `components/epp-monitoring`, which scrapes with a token |

A second scrape doubles every rate; a missing one leaves the analyzers
blind. Today's preflight catches a missing model-server scrape
(`podmonitor.port`, `metrics.model`). Checks for a double scrape, for the
EPP scrape, and for the EPP metric names this release reads are new work.
The last matters because llm-d's `env.sh` tracks `ROUTER_EPP_VERSION=main`.

`epp-monitoring` goes in the model namespace's directory without a
`nameSuffix`. Its token Secret's ServiceAccount annotation and its
ServiceMonitor's credentials reference are not followed by kustomize
renames. Only its ClusterRoleBinding needs a per-namespace name, given by a
targeted patch, as llm-d's autoscaling recipes do too:

```yaml
# sm-admin/team-a/kustomization.yaml, when the EPP needs our scrape
components:
- ../../scaling-manager-v0.13.0/components/epp-monitoring
patches:
- target: {kind: ClusterRoleBinding, name: scaling-manager-epp-metrics-scraper}
  patch: |-
    - {op: replace, path: /metadata/name, value: scaling-manager-epp-metrics-scraper-team-a}
```

### Several namespaces, one budget

`CTRL=scaling-manager-system`, `MODEL_NAMESPACES="team-a team-b"`. Changes
from one namespace:

- Every directory that names the controller's namespace names
  `scaling-manager-system`: `controller-namespace`, `controller`, `cluster`,
  `keda`, `sm-steps/`, `sm-owner/`. The suffix is `-scaling-manager-system`.
- `sm-admin/kustomization.yaml` lists `team-a` and `team-b`, one directory
  each, written as `sm-admin/team-a/` with its own `namespace:`. Kustomize
  has no loop. The preflight's reads in each model namespace come the same
  way, one `sm-steps/preflight-team-a/` each.
- `sm-admin/cluster/` gets a list-valued patch with the namespaces the
  controller reads. Kustomize replaces the list whole:

```yaml
# sm-admin/cluster/model-namespaces.yaml, under patches: in sm-admin/cluster/kustomization.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: scaling-manager-policy-namespace-reader-role    # the name before nameSuffix
rules:
- apiGroups: [""]
  resources: ["namespaces"]
  verbs: ["get"]
  resourceNames: ["scaling-manager-system", "team-a", "team-b", "scaling-manager-policy"]
```

- The controller must be told which namespaces it manages. Its Deployment
  sets `LLMSM_WATCH_NAMESPACE` to `$(POD_NAMESPACE)`, and `watchNamespaces`
  in the settings can only narrow what the Deployment names. So
  `sm-owner/` patches the Deployment, as `smt` does:

```yaml
# sm-owner/watch.yaml, under patches: in sm-owner/kustomization.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: scaling-manager-controller-manager
spec:
  template:
    spec:
      containers:
      - name: manager
        env:
        - name: LLMSM_WATCH_NAMESPACE
          value: team-a,team-b
```

  Rendered: the merge replaces that one entry by name and keeps the others.

- The settings carry the budget:

```yaml
  literals:
  - prometheusURL=https://prometheus.monitoring.svc:9090
  - limiter=quota
  - quotas=NVIDIA-H100-80GB-HBM3=16
  - quotaScope=cluster
```

```bash
kubectl create namespace $CTRL                                          # admin
kubectl create rolebinding scaling-manager-owner -n $CTRL \
  --clusterrole=admin --user=<the owner>                                # admin: the stock admin ClusterRole, in $CTRL only
# then the steps of one namespace, unchanged: they use $CTRL
```

With Kueue (`kueue=true`), the Kueue reader gets the same list-valued patch.
This path, with a DisaggregatedSet and shadow sharing, is about 27 commands
and 16 files. `kubectl scaling-manager init --namespace $CTRL
--model-namespace team-a --model-namespace team-b --out .` writes the files.

**Removing a namespace.** `kubectl apply` does not prune. Delete it first,
then drop it from every list:

```bash
NS=team-b
kubectl delete -k sm-admin/$NS/                                         # admin
# remove $NS from sm-admin/kustomization.yaml, sm-admin/cluster/model-namespaces.yaml and sm-owner/watch.yaml
kubectl apply -k sm-admin/ ; kubectl apply -k sm-owner/                 # the Deployment rolls with the new list
```

### Passive, shadow, active

The install's settings declare what the install is: Prometheus, the
budget. The live switches (`passive` and the sharing mode) are not in them.
They live in the scaling policy, created once from `switches/` and never
applied again, so an upgrade or a sync never turns sharing back off. The
release's settings leave `passive` and `utilizationShare` out; the
controller then takes them from the policy
([where a value comes from](../reference/configuration.md#where-a-value-comes-from)).
The Deployment sets `LLMSM_PASSIVE=true`, so a missing policy is passive.

Which policy to edit:

| case | `passive` | the sharing mode (`optimizer:`) |
| --- | --- | --- |
| no policy namespace | `scaling-manager-scaling-policy-config` in `$CTRL`, `default` entry | the same |
| a policy namespace resolves (`scaling-manager-policy`, or the label) | `$CTRL`'s map | the map in the policy namespace: `optimizer:` and `limiters:` in `$CTRL`'s map no longer count ([scaling policy](../reference/scaling-policy.md#configmap-structure)) |
| a model namespace has its own map of that name | that namespace's map: it replaces the global one whole for its models | that namespace's map |

A new preflight WARN, `policy.source`, names which map wins for each
managed namespace.

Shadow sharing, with no policy namespace:

```bash
kubectl edit configmap scaling-manager-scaling-policy-config -n $CTRL
# in the default entry:
#   optimizer:
#     type: utilizationShare
#     utilizationShare:
#       shadow: true
```

Going active needs the admin's grant of `patch` on pods in each model
namespace, a separate admin step as today
([decision 3 of install-from-release](install-from-release.md#decisions-2026-11-10)).
Add `- ../../scaling-manager-v0.13.0/components/activate` under
`components:` in each `sm-admin/<ns>/`, then:

```bash
kubectl apply -k sm-admin/                                              # admin
kubectl edit configmap scaling-manager-scaling-policy-config -n $CTRL   # owner: passive: false, shadow: false
```

**Undo sharing.** Set `shadow: true` and wait one cycle, so no transfer
marks are left on pods. Then remove the `optimizer` block and the
`activate` components, and apply `sm-admin/`. The budget stays; each model
is scaled on its own.

### Register the models

The controller publishes a suggested plan: one ScaledObject per model
server, and per role on a DisaggregatedSet. It writes it to the ConfigMap
`scaling-manager-scaledobjects-plan` in `$CTRL`, one key per model
namespace. `controller-admin` creates that ConfigMap empty, and the
controller has `get` and `update` on it by name. It holds every managed
namespace's plan, so only `$CTRL`'s owner reads it.

That owner extracts each namespace's file. The model namespace's owner
reviews and applies it, since they own the workloads; in one namespace that
is the same person.

```bash
for NS in $MODEL_NAMESPACES; do                                         # owner of $CTRL
  kubectl get configmap scaling-manager-scaledobjects-plan -n $CTRL \
    -o jsonpath="{.data.$NS}" > scaledobjects-$NS.yaml
done
for NS in $MODEL_NAMESPACES; do                                         # owner of each $NS
  kubectl apply -f scaledobjects-$NS.yaml -n $NS --dry-run=server && kubectl apply -f scaledobjects-$NS.yaml -n $NS
done
```

The plan is read from workloads the model namespace's owner controls, so it
is untrusted input. The controller writes only `ScaledObject` objects into
it, each with the external trigger at this controller's scaler and the
`app.kubernetes.io/managed-by: llm-d-scaling-manager` label. The plugin's
`plan check` refuses a file with anything else. Read it before applying.

For a model you add by hand, `scaledobject/external/` is a checked-in
overlay in the style of the llm-d guides: one `scaledobject.yaml` with the
external trigger, applied with `kubectl kustomize ... | envsubst | kubectl
apply -f -`.

### A DisaggregatedSet

First see how its roles are scaled:

```bash
NS=team-a SET=<your DisaggregatedSet>
kubectl get disaggregatedset $SET -n $NS -o jsonpath='{.spec.roles[*].scaling.mode}{"\n"}'
```

**External roles** are scaled through their `DisaggregatedSetRoleScaler`
(`<set>-<role>`). The plan has one ScaledObject per role; nothing else is
needed.

**Static roles** have no `/scale` for KEDA, so the role writer reads and
writes them. Add the two components:

```yaml
# sm-admin/team-a/kustomization.yaml, under components:
- ../../scaling-manager-v0.13.0/components/dset-role-writer-admin
# sm-owner/kustomization.yaml
components:
- ../scaling-manager-v0.13.0/components/dset-role-writer
```

Apply both directories, then opt the set in:

```bash
kubectl annotate disaggregatedset $SET -n $NS \
  scaling-manager.llmd.ai/role-writer=true \
  scaling-manager.llmd.ai/model-id=<the model it serves> \
  scaling-manager.llmd.ai/min-replicas=1 \
  scaling-manager.llmd.ai/max-replicas=<max groups per role>
```

Use `force` instead of `true` if the set was created with `kubectl create`,
plain `kubectl apply` or Helm
([ownership](../reference/disaggregatedset.md#ownership-of-replicas)).
While the model is passive the writer writes nothing.

### How you know it worked

```bash
kubectl get configmap scaling-manager-controller-status -n $CTRL \
  -o jsonpath='{.data.scalerTLS} | {.data.limiter} {.data.utilizationShare}{"\n"}'   # mutual TLS | quota/... shadow
for NS in $MODEL_NAMESPACES; do
  kubectl get events -n $NS --field-selector reason=PassiveRecommendation
done
```

In Prometheus, `llmsm_utilization_share_passive_shadow` is 1 while sharing
only reports, and `llmsm_desired_replicas` is what each model would run. The
[README's step 4](../../README.md#4-watch-what-it-would-do) has the rest.

### Upgrade and rollback

Unpack the new release beside the old one and change the version in the
paths (or let the plugin do it). Then the new release's preflight, `diff`,
and the admin and owner applies. The Jobs are named by version, so a new
release never hits a Job's immutable template.

Rollback is the same with the old path, with three cautions:

- a settings key the newer release added must come out of the generator
  first (phase 1 confirms whether an older controller rejects or ignores it);
- the scaling policy was created once and may carry fields the newer
  release added; remove them by hand;
- a release without a kustomize tarball (before this ships) rolls back with
  `smt` or its release files only.

Settings that are read at start (`prometheusURL`, `scaleToZero`,
`watchNamespaces`, `logVerbosity`, ...) need a restart. The controller logs
`Restart required` and sends a `SettingsRestartRequired` event
([configuration](../reference/configuration.md)):

```bash
kubectl rollout restart deployment/scaling-manager-controller-manager -n $CTRL
```

**Under GitOps.** Keep `sm-admin/` and `sm-owner/` in the sync. Keep
`sm-steps/`, `scaler-tls/` and `switches/` out: syncing `scaler-tls/` would
empty the certificates the controller wrote, and syncing `switches/` would
reset the live switches. To upgrade:

1. Run the new release's `sm-steps/preflight` by hand.
2. Merge the path bump.
3. Nobody runs `rollout restart` under auto-sync, so `sm-owner/` carries a
   pod-template annotation, `scaling-manager.llmd.ai/restart: "<n>"`. Bump
   it in the same change as a restart key.

Argo CD must track resources by annotation
(`application.resourceTrackingMethod: annotation`). Label tracking rewrites
`app.kubernetes.io/instance`, which `smt` and the migration rely on.

### Uninstall

In this order:

```bash
# 1. Sharing to shadow and wait a cycle (no transfer marks left). Then each model namespace's owner:
for NS in $MODEL_NAMESPACES; do kubectl delete -f scaledobjects-$NS.yaml -n $NS --ignore-not-found; done
# 2. Owner of $CTRL: the controller. The scaler TLS Secrets are kept (below).
kubectl delete -k sm-owner/
kubectl delete configmap scaling-manager-scaling-policy-config -n $CTRL
kubectl delete secret scaling-manager-prometheus-ca -n $CTRL --ignore-not-found
# 3. Admin: the admin objects, KEDA's side, any temporary rights still there.
kubectl delete -k sm-admin/
kubectl delete -k sm-steps/keda-auth/keda/ --ignore-not-found
kubectl delete -k sm-steps/keda-auth/controller/ --ignore-not-found
kubectl delete -k sm-steps/preflight/ --ignore-not-found
# 4. Nothing left but the scaler CA:
for L in app.kubernetes.io/part-of=scaling-manager app.kubernetes.io/managed-by=llm-d-scaling-manager; do
  kubectl get all,configmap,secret,role,rolebinding,servicemonitor,podmonitor,networkpolicy,scaledobject -A -l $L
  kubectl get clusterrole,clusterrolebinding,clustertriggerauthentication -l $L
done
```

The scaler TLS Secrets are not in any applied base; `scaler-tls/` created
them once. The CA, Secret `scaling-manager-external-scaler-ca`, is kept on
purpose: a reinstall signs with it, so KEDA's open connection keeps working.
When you will not reinstall:

```bash
kubectl delete secret -n $CTRL -l scaling-manager.llmd.ai/scaler-tls
```

`kubectl delete -k sm-admin/` removes:

- the KEDA-namespace Secret, the ClusterTriggerAuthentication, the marker
  and the plan ConfigMap, because the bases create them and the controller
  or the Job only fills them;
- the EPP monitor's objects, with each model namespace's directory.

No base holds a Namespace, so none is deleted. The policy is deleted by name
in step 2, because only `kubectl create` made it.

### Air-gapped

Mirror the images by digest (`skopeo copy --all`), verify them with
`cosign verify` on a connected machine first, and override them in each
kustomization:

```yaml
images:
- name: ghcr.io/llm-d-incubation/llm-d-scaling-manager
  newName: registry.internal/llm-d-scaling-manager
```

The controller, the preflight and the `keda-auth` Job all run the one
controller image, under one image name, so one entry covers them. The
render-parity check asserts that no other image is in the bases.

### The llm-d path

llm-d's guides build overlays from their own repository: its CI dry-run job
builds in-repo directories, so an overlay over a downloaded tarball cannot
resolve there. An llm-d recipe, `guides/recipes/scaling-manager/`, would
follow the pattern its autoscaler recipe already uses:

- a git-ref base pinned to this release's commit;
- an `images:` pin of the controller by digest;
- `SCALING_MANAGER_VERSION` in llm-d's `guides/env.sh` feeding both, as
  `ROUTER_CHART_VERSION` feeds the router.

## For developers

### The Jobs

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

### The preflight conflict checks

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

### The KEDA certificate step, and why it runs in KEDA's namespace

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

### The scaler TLS Secrets

The release file ships them with empty keys (type `kubernetes.io/tls`
requires the key fields, and a Secret's type cannot change). Applying them
again would empty the certificates the controller wrote. So they stay out of
every applied base. The owner creates them once with `kubectl create -k
scaler-tls/`, as with `switches/`, and the controller keeps `get` and
`update` by name only. The controller never needs `create` on Secrets.

### Install settings and live switches

The settings and the operator's generator are applied every time, so the
file wins over a live `kubectl patch`. They hold what the install
declares, and nothing an operator flips day to day. `passive` and the
sharing mode move to the scaling policy, which no apply touches. The same
holds under Helm or any other renderer.

### What the renders showed

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

### What smt does that the bases must cover

| `smt` does | in this design |
| --- | --- |
| sets `LLMSM_WATCH_NAMESPACE` on the Deployment to the model namespaces | the env patch in `sm-owner/` ([several namespaces](#several-namespaces-one-budget)) |
| re-wires an HPA KEDA left on the CPU default (`scaledobjects-apply`) | KEDA 2.20 and later does not leave it. Below that, the controller reports an unwired HPA in its status and as an Event with the fix; the plugin's `rewire` runs it |
| EPP ServiceMonitor only where nothing scrapes the EPP | `components/epp-monitoring`, by the rule in [Monitoring](#monitoring) |
| model-server PodMonitor, `--modelserver-port` | `components/modelserver-monitoring`, and a patch for the port |
| `--prometheus-ca FILE` | `components/prometheus-ca`, plus a `secretGenerator` (`disableNameSuffixHash: true`) in `sm-owner/settings/` |
| `--allow-http` | `components/allow-http`: an empty token path and `prometheusAllowHTTP=true` |
| `--quota`, `--set KEY=VALUE` | generator literals; the controller checks the values and reports a rejected key in its status |
| `--dset-role-writer` | the two `dset-role-writer` components |
| the weights labeller's watch list | stays in `smt` and the plugin; the preflight WARNs with the `kubectl patch` that adds the namespace |
| removing a model namespace | delete its directory first, then the lists |
| refuses a namespace another scaling manager manages, or a namespaced install beside a cluster-wide one | the new preflight conflict checks, in run 1 |
| refuses a re-run that drops namespaces | `kubectl diff -k sm-admin/` shows the deletion |
| the stamp ConfigMap | not needed: the preflight reads the live admin objects. `smt` keeps it |
| checks `SHA256SUMS` and the release version | `cosign verify-blob` of the tarball |

### What moves out of `smt`, and what stays

| | moves | stays in `smt` |
| --- | --- | --- |
| rendering the objects | `config/install`; `release-manifests.sh` renders the release files from it | `smt` applies the release files, now rendered from it |
| preflight | already a Job | `smt` runs and gates on it |
| KEDA certificate | the `keda-auth` Job | `admin-finish` runs the same Job |
| ScaledObject plan | the controller's plan ConfigMap | `scaledobjects-plan` copies it out |
| `prepull`, `weights`, `engine-cache`, `workload-patch` | nothing | unchanged |

One tree feeds the tools image, the release YAML files and the tarball.
`smt` keeps every command and flag.

### What the controller and the release need

New controller work:

- the plan writer (a port of the discovery in `deploy/lib/scaledobject.sh`)
  into the pre-created plan ConfigMap, and the unwired-HPA report;
- the marker's `ready` key, the live re-read of the client CA, and
  `kedaConnection` and `kedaCopyStale` in the status;
- `/manager keda-auth apply` (a port of `llmsm_scaler_keda_auth_apply`).

New preflight checks:

- the conflict checks;
- `policy.source`;
- a double scrape of the model servers or the EPP;
- a missing EPP scrape;
- the EPP metric names;
- a run-1 mode.

The release:

- attaches `scaling-manager-kustomize-<release>.tar.gz`, in `SHA256SUMS`
  and signed;
- from phase 3, attaches the plugin binaries, each with a cosign bundle, and
  a krew manifest generated from `SHA256SUMS`. To check a binary before krew
  installs it:

  ```bash
  cosign verify-blob kubectl-scaling-manager-linux-amd64.tar.gz \
    --bundle kubectl-scaling-manager-linux-amd64.tar.gz.sigstore.json \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/$V"
  ```

- is the version that a proposal to llm-d, `SCALING_MANAGER_VERSION` in
  `guides/env.sh`, would name ([the llm-d path](#the-llm-d-path)).

### Words used here

- **the marker**: ConfigMap `scaling-manager-scaler-keda-auth` in the
  controller namespace; the controller serves mutual TLS only once it says
  KEDA's side is in place.
- **the stamp**: ConfigMap `scaling-manager-admin-setup`, which `smt
  admin-setup` writes so `smt install` can tell the admin step ran.
- **`scalerTLSAction`**: the key in `scaling-manager-controller-status` that
  prints what to run to give KEDA its certificate.

## Migration

An install made with `smt` or by hand from the release files has the same
names, namespaces and labels. The new directories take it over in place:

1. The admin writes `sm-admin/` for the same namespaces and KEDA namespace
   (or runs `kubectl scaling-manager init`), then `kubectl diff -k
   sm-admin/`. Expected: the version label, the
   `scaling-manager.llmd.ai/subject` label, the empty plan ConfigMap, and the
   `keda-auth-setup` objects, which already exist.
2. The owner copies the live settings into the generator, leaving out
   `passive` and `utilizationShare`, and copies those two into the policy
   that wins for each namespace first ([which policy](#passive-shadow-active)).
   For several namespaces, the live `LLMSM_WATCH_NAMESPACE` goes into
   `sm-owner/watch.yaml`. Then `kubectl diff -k sm-owner/`. A key missing
   from the generator takes the shipped default on apply, so the copy is
   required.
3. Apply admin, then owner. The scaler TLS Secrets, KEDA's copy and the
   marker keep their content, so mutual TLS stays up.
4. The first `kubectl apply` over an object made with `kubectl create` (the
   settings) warns once about the missing last-applied annotation, and adds
   it.
5. The stamp stays; `smt` still works against the install.

An install made with `make deploy-scaling-manager` is not adopted, as for
`smt` today.

## Verification

- **Render parity**, in `make lint-deploy-scripts` (a new
  `hack/check-kustomize-install.sh`): for fixed namespaces, `kubectl
  kustomize` of the walkthrough's directories must equal the release files
  after `sed`, object for object. It asserts both sides are non-empty and
  equal in count, and that the bases carry only the controller image. It
  also checks every case in [What the renders showed](#what-the-renders-showed),
  including the EPP references a rename does not follow. Its negative
  control: a base with one rule removed must fail.
- **A kubectl-only e2e in kind**, `make test-e2e-kustomize-install`, beside
  `test-e2e-release-manifests` and `test-e2e-tools-install`, on
  `test-e2e-release-manifests-setup` ([testing](../developer-guide/testing.md)).
  It writes the walkthrough's directories over the local `config/install`,
  runs every step of one namespace, registers a model from the plan, checks
  the controller status and the smoke suite, uninstalls, and runs the
  nothing-left check. Legs: one namespace, several namespaces, and an upgrade
  from the previous release's tarball. Images: the locally built controller
  loaded into kind, the prerequisites from `registry.k8s.io` and `quay.io` as
  today, never docker.io. Ginkgo runs with `-v`; a 0-spec run fails.
- **Release asset checks.** `hack/check-release-manifests.sh` gains the
  tarball: in `SHA256SUMS`, signed, every image by digest and the same digest
  as the YAML files. From phase 3, the plugin binaries and the krew manifest.
- **The llm-d dry run.** An llm-d recipe ([the llm-d path](#the-llm-d-path))
  falls under that repository's kustomize dry-run CI job, as a tier-2
  guide. It builds in-repo directories over the git ref.

## Phases

| phase | what | effort |
| --- | --- | --- |
| 1 | `config/install` (replacing `config/overlays`), the components, `scaler-tls/` and `switches/`, `release-manifests.sh` from it, the parity check, the tarball. `epp-monitoring` has fixed names, so two unscraped EPPs in one model namespace would collide: name its objects per EPP | 2 weeks |
| 2 | `/manager keda-auth apply`, the marker's `ready` key and the live CA re-read, `kedaConnection`, the plan writer, the unwired-HPA report, the versioned Jobs; the new preflight checks; `smt` calls them; the kind e2e; the guide from a `guide.yaml` | 4 weeks |
| 3 | the plugin (`init`, `run`, `plan check`, `rewire`), krew manifest, release binaries | 2 weeks |
| 4 | the README leads with the kubectl path; `smt` stays documented beside it | 2 days |

Phases 1 and 2 can run in parallel: about eight weeks of work, six elapsed.

## Open questions

1. **The git ref: development only, or also the llm-d path?**
   Install-from-release ruled out a remote kustomize base on 2026-11-10.
   llm-d's own guides install an autoscaler controller as an overlay over a
   remote git-ref base, and their CI dry-run can only build that form. This
   proposal makes the signed tarball the operator's path. It allows the git
   ref, pinned to a release commit with the image pinned by digest, for
   development and for an llm-d recipe. Is that acceptable?
2. **The live CA re-read.** With it, the `keda-auth` Job needs no right on
   the Deployment. Build it in phase 2, or keep the restart and its `patch`
   by name?
3. **Kustomize floor.** Which oldest `kubectl` do we support, given that
   components and `labelSelector` patch targets are required?
4. **The switches in the policy.** Moving `passive` and the sharing mode
   out of the settings changes where operators flip them. Keep the settings
   keys as an override for one more release?
5. **The weights labeller.** It stays an `smt` and plugin step. Is a
   preflight WARN enough on the kubectl path?

## Alternative: Helm (needs a maintainer decision)

AGENTS.md forbids a Helm chart, so this is not the recommendation. It is
here because the router installs with Helm, and the case for it is real:

- every llm-d operator already runs `helm` for the router;
- a values file is the closest match to "config in one file";
- `helm upgrade` and `helm uninstall` give upgrade and removal;
- a `range` over the model namespaces is simpler than a directory each;
- hooks work under GitOps: Flux's helm-controller runs them, and Argo CD
  maps Helm hooks to its sync hooks.

Several concerns are the same for any renderer, so they do not count
against Helm. The created-once objects (`scaler-tls/`, `switches/`) are
created outside the chart, as here. Settings drift is the same under
kustomize.

What remains against it is two things:

- **The admin/owner split.** One release renders both halves with the
  caller's rights. It needs two charts, or two releases of one chart.
- **The rule itself.** A chart was removed once; bringing it back is a
  maintainer decision.

If the rule is lifted, the chart should be a thin wrapper generated in CI
from the same `config/install` bases. It would be pushed as
`oci://ghcr.io/llm-d-incubation/charts/llm-d-scaling-manager` and
`...-admin`, with the preflight and `keda-auth` as hook Jobs, and the same
render-parity check against the release files.
