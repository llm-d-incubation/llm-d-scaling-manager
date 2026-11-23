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
- **What the operator gets:** no docker, no checkout, and an install that
  lives in git as plain kustomizations, which Argo CD or Flux can sync.
- **What it costs:** more commands than `smt`. One namespace is about 11
  `kubectl` commands, plus a `kubectl diff` before each apply, and 11 small
  kustomization files, against `smt`'s 3 commands. A `kubectl` plugin can
  write the files and bring it back to 3 commands.
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
| model-server monitoring | a kustomize component with a decode PodMonitor, `recipes/modelserver/components/monitoring` (`decode-podmonitor.yaml`) | [the component](https://github.com/llm-d/llm-d/tree/main/guides/recipes/modelserver/components/monitoring) |
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

Not confirmed: which version the `v0` chart tag resolves to, the contents
of llm-d's decode PodMonitor (only its file name was read), and the full
router release notes. The GitHub REST API refused unauthenticated requests;
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
installs with Helm, so Helm is the closest match to it. It is still not the
primary design: it has [its own section](#alternative-helm-needs-a-maintainer-decision).

## Candidate designs

1. **Kustomize bases and components.** The release ships `config/install`,
   rendered with the image digest, as a signed tarball. The operator writes
   small kustomizations over it, one per part.
2. **Jobs for the procedural steps.** The preflight (exists), the KEDA
   certificate copy and the ScaledObject plan, run from the controller image.
3. **A `kubectl` plugin**, `kubectl scaling-manager`: a static Go binary,
   krew-installable, that writes the kustomizations of design 1 and runs the
   Jobs of design 2.
4. **Helm (an OCI chart).** Alternative only.

| | 1. kustomize | 1 + 2 | 1 + 2 + 3 | 4. Helm |
| --- | --- | --- | --- | --- |
| commands, one namespace | about 7, plus the KEDA step by hand | about 11 | 3, as `smt` | 2 `helm install` (admin chart, owner chart), plus the preflight |
| files the operator keeps | 8 small kustomizations | 11 | 11, written by the plugin | 2 values files |
| admin/owner split | separate directories | the same; the Jobs' grants in a third, temporary one | as `smt` | two releases |
| config surface | a settings generator, list-valued patches, components | the same | flags, as `smt` | values |
| upgrade | point at the new tarball, `diff`, apply | the same, plus the versioned Jobs | the new binary, same commands | `helm upgrade` |
| preflight | no | the Job, as today | the Job | a hook Job |
| multi-namespace | one small kustomization per namespace | the same | `--model-namespace` | a list in values |
| OpenShift | the `openshift` component | the same | `--platform` | a values switch |
| GitOps | yes: the steady-state directories | yes; the Jobs stay out of the sync | yes, from the written files | yes, through Flux helm-controller or Argo CD |
| effort | 2 weeks | 5 weeks | 7 weeks | 2 weeks, plus the rule |

## Recommendation

**Designs 1 and 2 as the base, design 3 for convenience.** The gain over
`smt` is no docker, no checkout, and an install that is plain files under
version control. It is not fewer commands; the plugin is for that.

What follows is written as the future guide would read. `team-a` and
`team-b` are placeholder namespaces; `v0.13.0` is a placeholder release.

### You need

- llm-d model servers, and the router installed with its monitoring values
  (`MONITORING_VALUES` in the llm-d guides). For scale from zero, its flow
  control values too (`featureGates: [flowControl]`).
- The router v1.0 or later; InferencePool from `inference.networking.k8s.io`
  (v1) or `inference.networking.x-k8s.io` (v1alpha2). The release notes state
  the tested range of each release.
- KEDA 2.18 or later (2.21 recommended), and the Prometheus Operator.
- For a DisaggregatedSet: LeaderWorkerSet v0.10.0 or later.
- Your GPU product: the `GPU.PRODUCT` column of
  `kubectl get nodes -L nvidia.com/gpu.product`.
- `kubectl` with kustomize components and `labelSelector` patch targets. The
  walkthroughs were rendered with kubectl v1.31 (kustomize v5.4.2); phase 1
  sets the oldest supported version.
- `cosign` 3.0 or later, to verify the release.
- A cluster admin and a namespace owner.

### Get the release

```bash
V=v0.13.0
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V
curl -fsSLO $URL/scaling-manager-kustomize-$V.tar.gz
curl -fsSLO $URL/scaling-manager-kustomize-$V.tar.gz.sigstore.json
cosign verify-blob scaling-manager-kustomize-$V.tar.gz \
  --bundle scaling-manager-kustomize-$V.tar.gz.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/$V"
tar -xzf scaling-manager-kustomize-$V.tar.gz          # unpacks to scaling-manager-v0.13.0/
```

Verify before unpacking. The tarball pins every image by digest. A git ref
(`github.com/llm-d-incubation/llm-d-scaling-manager//config/install/...?ref=<commit>`)
works too, for development, pinned to a commit: a tag can move, and the
tree at a tag cannot carry the digest of the image built from it.

### What the release ships

`config/install/` replaces `config/overlays/` as the one render tree. The
release files (`deploy/release-manifests.sh`), `make deploy-scaling-manager`
and the tarball all render from it.

```text
scaling-manager-v0.13.0/
  controller-admin/        part=admin, controller namespace: ServiceAccount, Roles, controller
                           ServiceMonitor, scaler NetworkPolicy (KEDA in "keda"), the empty marker
  cluster-admin/           the controller's ClusterRoles and ClusterRoleBindings
  model-namespace/         one model namespace: the controller's Role and RoleBinding
  owner/                   the settings ConfigMap, the Deployment, the Services
  keda-auth-setup/         in KEDA's namespace: KEDA's Secret with no data, the ClusterTriggerAuthentication
  epp-monitoring/          one EPP nothing scrapes yet: ServiceMonitor, ServiceAccount, token Secret,
                           /metrics ClusterRoleBinding
  steps/
    preflight/             the preflight Job and all its RBAC (temporary)
    keda-auth/keda/        the keda-auth Job, its ServiceAccount and Role, in KEDA's namespace (temporary)
    keda-auth/controller/  its Role in the controller namespace (temporary)
  components/
    openshift/             the service CA, openshift-keda
    modelserver-monitoring/  the model-server PodMonitor (leave it out where one already scrapes)
    prometheus-ca/         mounts Secret scaling-manager-prometheus-ca, sets prometheusCACertPath
    allow-http/            plain-HTTP Prometheus: no token, prometheusAllowHTTP
    quota/                 limiter quota (the GPUs go in the operator's generator)
    kueue/                 the Kueue readers
    cluster-pods/          list pods cluster-wide, for the gpu-inventory limiter
    dset-role-writer-admin/  get, list, patch on disaggregatedsets (one model namespace)
    dset-role-writer/      DSET_ROLE_WRITER=true on the controller
    activate/              the pod-writer grant (one model namespace)
    argocd-hooks/          runs the preflight as an Argo CD sync hook, for those who want it
  switches/                the scaling policy: passive, sharing off. Created once, never re-applied
  scaledobject/external/   a ScaledObject overlay with the external trigger, for a model added by hand
```

Every object keeps `scaling-manager.llmd.ai/part` (admin or controller) and
the `app.kubernetes.io` labels of today's release files. Bindings whose
subject is the controller's ServiceAccount also carry
`scaling-manager.llmd.ai/subject: controller`, which the operator's
`controller-namespace` component selects.

### One namespace

The controller runs beside the models, in `team-a`. The admin keeps
`sm-admin/` and `sm-steps/`; the owner keeps `sm-owner/`. Paths are
relative to the directory that holds the unpacked release. That is 11 small
files; `kubectl scaling-manager init --namespace team-a --out .` writes
them.

| directory | `namespace:` | holds |
| --- | --- | --- |
| `sm-admin/controller-namespace/` | | a component: the controller's namespace, the one place it is written besides `namespace:` |
| `sm-admin/` | | `resources:` the five directories below |
| `sm-admin/controller/` | `team-a` | `controller-admin`, plus the component |
| `sm-admin/cluster/` | `team-a` | `cluster-admin`, `nameSuffix: -team-a`, plus the component |
| `sm-admin/team-a/` | `team-a` | `model-namespace` and `modelserver-monitoring`, plus the component |
| `sm-admin/team-a-epp/` | `team-a` | `epp-monitoring`, `nameSuffix: -team-a` (only where nothing scrapes the EPP) |
| `sm-admin/keda/` | `keda` | `keda-auth-setup`, `nameSuffix: -team-a` |
| `sm-steps/preflight/` | `team-a` | `steps/preflight`, `nameSuffix: -team-a` |
| `sm-steps/keda-auth/keda/` | `keda` | `steps/keda-auth/keda`, `nameSuffix: -team-a` |
| `sm-steps/keda-auth/controller/` | `team-a` | `steps/keda-auth/controller`, and a patch naming the Job's ServiceAccount in `keda` |
| `sm-owner/` | `team-a` | `owner`, and the settings generator |

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
# sm-admin/cluster/kustomization.yaml
namespace: team-a
nameSuffix: -team-a
resources:
- ../../scaling-manager-v0.13.0/cluster-admin
components:
- ../controller-namespace
```

```yaml
# sm-admin/team-a/kustomization.yaml: a model namespace
namespace: team-a
resources:
- ../../scaling-manager-v0.13.0/model-namespace
components:
- ../controller-namespace
- ../../scaling-manager-v0.13.0/components/modelserver-monitoring
```

```yaml
# sm-owner/kustomization.yaml
namespace: team-a
resources:
- ../scaling-manager-v0.13.0/owner
configMapGenerator:
- name: scaling-manager-settings
  behavior: merge
  options:
    disableNameSuffixHash: true
  literals:
  - prometheusURL=https://prometheus.monitoring.svc:9090
```

**Why the component.** A kustomization's `namespace:` rewrites a binding's
subject only when that ServiceAccount is in the same build. The controller's
ServiceAccount is in `sm-admin/controller/`, so in every other directory the
subject would keep the base's namespace. This was rendered with kubectl
v1.31 (kustomize v5.4.2) on a scratch copy of the layout, with a controller
namespace `llmsm-ctrl`:

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

The same run shows that `nameSuffix` renames each ClusterRole and the
`roleRef` that names it, while a binding to a stock role such as
`system:auth-delegator` keeps its `roleRef`. It also shows that a generator
with `behavior: merge` and `disableNameSuffixHash: true` keeps the name
`scaling-manager-settings`.

The directories whose ServiceAccount is in the same build are
`steps/preflight`, `steps/keda-auth/keda` and `epp-monitoring`. There,
`nameSuffix` renames the ServiceAccount, and kustomize follows it into the
Job's `serviceAccountName`. It follows it into a binding's subject only when
the base gives the ServiceAccount the same namespace as the subject. A second
scratch render showed both cases. With the ServiceAccount carrying no
namespace, the ClusterRoleBinding subject kept `scaling-manager-preflight` in
`scaling-manager-system`. With `namespace: scaling-manager-system` on it, the
subject became `scaling-manager-preflight-llmsm-ctrl` in `llmsm-ctrl`. So
every base ServiceAccount carries the namespace its subjects name.

The same render showed a kustomize `configurations:` entry carrying
`nameSuffix` into a ClusterTriggerAuthentication's `secretTargetRef`. It
also showed kustomize setting `metadata.namespace` on that cluster-scoped
object, because kustomize does not know the CRD's scope. Phase 1 checks
that `kubectl apply` ignores the field, or gives kustomize the CRD's scope.
Phase 1 renders the real bases the same way.

**Monitoring.** Leave `team-a-epp` out where the router was installed with
its monitoring values, and `modelserver-monitoring` out where llm-d's
monitoring component already scrapes the model servers: a second scrape
doubles every rate. The preflight FAILs on a double scrape either way.
`--modelserver-port` becomes a strategic-merge patch on the PodMonitor in
`sm-admin/team-a/`.

**The steps.** Each person runs `kubectl diff -k` before every apply.

```bash
kubectl apply -k sm-steps/preflight/                               # admin: the Job and its rights
kubectl logs -f -n team-a job/scaling-manager-preflight-v0-13-0-team-a   # PASS, WARN, FAIL or UNKNOWN, and the fix
kubectl diff -k sm-admin/ ; kubectl apply -k sm-admin/             # admin
kubectl diff -k sm-owner/ ; kubectl apply -k sm-owner/             # owner: the settings and the controller
kubectl create -k scaling-manager-v0.13.0/switches -n team-a       # owner, once: passive, no sharing
kubectl apply -k sm-steps/keda-auth/controller/                    # admin: the keda-auth Job's read in team-a
kubectl apply -k sm-steps/keda-auth/keda/                          # admin: the Job, in KEDA's namespace
kubectl logs -f -n keda job/scaling-manager-keda-auth-v0-13-0-team-a     # ends with "mutual TLS"
kubectl delete -k sm-steps/keda-auth/keda/ ; kubectl delete -k sm-steps/keda-auth/controller/
kubectl delete -k sm-steps/preflight/                              # admin: the temporary rights go
```

The first preflight runs before any admin object exists, so it can refuse
what `admin-setup` refuses today (see [what smt does that the bases must
cover](#what-smt-does-that-the-bases-must-cover)). Run it again after the
owner's apply (delete its Job first): checks that need the controller are
UNKNOWN until then.

`sm-steps/` is applied by hand and never kept in a GitOps sync: a sync would
bring the temporary rights back. Only `sm-admin/` and `sm-owner/` are steady
state.

### Several namespaces, one budget

The controller runs in `scaling-manager-system`; models in `team-a` and
`team-b`. Changes from one namespace:

- Every directory that names the controller's namespace names
  `scaling-manager-system`: `controller-namespace`, `controller`, `cluster`,
  `sm-steps/`, `sm-owner/`; the suffix is `-scaling-manager-system`.
- `sm-admin/kustomization.yaml` lists `team-a` and `team-b`, one directory
  each, written as `sm-admin/team-a/` above with its own `namespace:`, and an
  EPP directory per model namespace where one is needed. Kustomize has no
  loop. The preflight's reads in each model namespace come the same way, one
  `sm-steps/preflight-team-a/` each.
- `sm-admin/cluster/` gets a list-valued patch with the namespaces the
  controller reads. Kustomize replaces the list whole:

```yaml
# sm-admin/cluster/model-namespaces.yaml, listed under patches: in sm-admin/cluster/kustomization.yaml
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

- The owner's generator names the namespaces (comma-separated) and the
  budget:

```yaml
  literals:
  - prometheusURL=https://prometheus.monitoring.svc:9090
  - watchNamespaces=team-a,team-b
  - limiter=quota
  - quotas=NVIDIA-H100-80GB-HBM3=16
  - quotaScope=cluster
```

```bash
kubectl create namespace scaling-manager-system                   # admin
# then the steps of one namespace, with -n scaling-manager-system
```

With Kueue (`kueue=true`), the Kueue reader gets the same list-valued patch.
`kubectl scaling-manager init --namespace scaling-manager-system
--model-namespace team-a --model-namespace team-b --out .` writes all of
these files.

**Removing a namespace.** `kubectl apply` does not prune. Delete it first,
then drop it from the lists:

```bash
kubectl delete -k sm-admin/team-b/                                 # admin
# remove team-b from sm-admin/kustomization.yaml, model-namespaces.yaml and watchNamespaces
kubectl apply -k sm-admin/ ; kubectl apply -k sm-owner/
kubectl rollout restart deployment/scaling-manager-controller-manager -n scaling-manager-system   # watchNamespaces needs a restart
```

### Passive, shadow, active

The install's settings declare what the install is: Prometheus, the
budget, the namespaces. The live switches (`passive` and the sharing mode)
are not in them. They live in the scaling policy's `default` entry, in
`switches/`, which is created once and never re-applied, so an upgrade or a
sync never turns sharing back off. The settings file of the release leaves
`passive` and `utilizationShare` out; the controller already takes them from
the policy when the settings do not set them
([where a value comes from](../reference/configuration.md#where-a-value-comes-from)).
The Deployment sets `LLMSM_PASSIVE=true`, so a missing policy is passive
too.

The steps above create it (`kubectl create -k .../switches`). Shadow sharing, once the budget is in the settings:

```bash
kubectl edit configmap scaling-manager-scaling-policy-config -n team-a
# in the default entry:
#   optimizer:
#     type: utilizationShare
#     utilizationShare:
#       shadow: true
```

Going active needs the admin's one grant of `patch` on pods per model
namespace, a separate admin step as today
([decision 3 of install-from-release](install-from-release.md#decisions-2026-11-10)):

```yaml
# sm-admin/team-a/kustomization.yaml: add under components:
- ../../scaling-manager-v0.13.0/components/activate
```

```bash
kubectl apply -k sm-admin/                                         # admin
kubectl edit configmap scaling-manager-scaling-policy-config -n team-a   # owner: passive: false, shadow: false
```

**Undo sharing.** In the policy's `default` entry, set `shadow: true` and
wait one cycle, so no transfer marks are left on pods. Then remove the
`optimizer` block and the `activate` component, and apply `sm-admin/`. The
budget stays; each model is scaled on its own.

### Register the models

The controller publishes a suggested plan: one ScaledObject per model
server, and per role on a DisaggregatedSet. It writes it to ConfigMap
`scaling-manager-scaledobjects-plan` in its namespace, one key per model
namespace. You review it, keep what you want in git, and apply it. Applying
ScaledObjects stays an explicit step for a person or a GitOps tool.

```bash
kubectl get configmap scaling-manager-scaledobjects-plan -n team-a \
  -o jsonpath='{.data.team-a}' > scaledobjects-team-a.yaml           # edit the bounds and the modelID
kubectl apply -f scaledobjects-team-a.yaml --dry-run=server
kubectl apply -f scaledobjects-team-a.yaml
```

The plan is read from workloads the model namespace's owner controls, so it
is untrusted input. The controller writes only `ScaledObject` objects into
it, each with the external trigger at this controller's scaler and the
`app.kubernetes.io/managed-by: llm-d-scaling-manager` label, and the
plugin's `plan check` refuses a file with anything else. Read it before
applying.

For a model you add by hand, the release also ships a checked-in overlay,
`scaledobject/external/`, in the style of the llm-d guides: one
`scaledobject.yaml` with the external trigger, applied with
`kubectl kustomize ... | envsubst | kubectl apply -f -`.

### A DisaggregatedSet

First see how its roles are scaled:

```bash
SET=<your DisaggregatedSet>
kubectl get disaggregatedset $SET -n team-a -o jsonpath='{.spec.roles[*].scaling.mode}{"\n"}'
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
kubectl annotate disaggregatedset $SET -n team-a \
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
kubectl get configmap scaling-manager-controller-status -n team-a \
  -o jsonpath='{.data.scalerTLS} | {.data.limiter} {.data.utilizationShare}{"\n"}'   # mutual TLS | quota/... shadow
kubectl get events -n team-a --field-selector reason=PassiveRecommendation
```

In Prometheus, `llmsm_utilization_share_passive_shadow` is 1 while sharing
only reports, and `llmsm_desired_replicas` is what each model would run. The
[README's step 4](../../README.md#4-watch-what-it-would-do) has the rest.

### Upgrade and rollback

Unpack the new release beside the old one and change the version in the
paths (or let the plugin do it). Then the preflight of the new release,
`diff`, and the admin and owner applies. The Jobs are named by version, so a
new release never hits a Job's immutable template.

Rollback is the same with the old path. Two cautions: a settings key the
newer release added must come out of the generator first (phase 1 confirms
whether an older controller rejects or ignores it), and learned state
written by the newer controller is read back as the
[upgrade notes](../reference/upgrade-and-uninstall.md) say.

Settings that are read at start (`prometheusURL`, `scaleToZero`,
`watchNamespaces`, `logVerbosity`, ...) need a restart after a change. The
controller logs `Restart required` and sends a `SettingsRestartRequired`
event ([configuration](../reference/configuration.md)):

```bash
kubectl rollout restart deployment/scaling-manager-controller-manager -n team-a
```

### Uninstall

In this order:

```bash
# 1. Owner: sharing to shadow, wait a cycle (no transfer marks left), then the ScaledObjects.
kubectl delete -f scaledobjects-team-a.yaml
# 2. Owner: the controller. The scaler TLS Secrets are kept (below).
kubectl delete -k sm-owner/
kubectl delete configmap scaling-manager-scaling-policy-config -n team-a
kubectl delete secret scaling-manager-prometheus-ca -n team-a --ignore-not-found
# 3. Admin: the admin objects, KEDA's side, any temporary grant still there.
kubectl delete -k sm-admin/
kubectl delete -k sm-steps/keda-auth/keda/ --ignore-not-found
kubectl delete -k sm-steps/keda-auth/controller/ --ignore-not-found
kubectl delete -k sm-steps/preflight/ --ignore-not-found
# 4. Nothing left but the scaler CA:
kubectl get all,configmap,secret,role,rolebinding,servicemonitor,podmonitor,networkpolicy -A \
  -l app.kubernetes.io/part-of=scaling-manager
kubectl get clusterrole,clusterrolebinding,clustertriggerauthentication \
  -l app.kubernetes.io/part-of=scaling-manager
```

The scaler TLS Secrets are not in any base: the controller creates them
([below](#what-the-controller-and-the-release-need)). They carry the
`scaling-manager.llmd.ai/scaler-tls` label. The CA, Secret
`scaling-manager-external-scaler-ca`, is kept on purpose: a reinstall signs
with it, so KEDA's open connection keeps working. When you will not
reinstall:

```bash
kubectl delete secret -n team-a -l scaling-manager.llmd.ai/scaler-tls
```

`kubectl delete -k sm-admin/` removes the KEDA-namespace Secret, the
ClusterTriggerAuthentication and the marker, because the bases create them
and the Job only fills them. It removes the EPP monitor's ServiceAccount,
token Secret and binding with `sm-admin/team-a-epp/`. No base holds a
Namespace, so none is deleted. The scaling policy is deleted by name in
step 2 because only `kubectl create` made it.

### Air-gapped

Mirror the images by digest (`skopeo copy --all`), verify them with
`cosign verify` on a connected machine first, and override them in each
kustomization:

```yaml
images:
- name: ghcr.io/llm-d-incubation/llm-d-scaling-manager
  newName: registry.internal/llm-d-scaling-manager
```

The controller, the preflight, the `keda-auth` Job and the plan all run the
one controller image, under one image name, so one entry covers them. The
render-parity check asserts that no other image is in the bases.

## For developers

### The Jobs

| Job | runs | where | rights |
| --- | --- | --- | --- |
| preflight | `/manager preflight check` (exists) | controller namespace | its Role and ClusterRole (exist), in `steps/preflight` beside the Job |
| keda-auth | new `/manager keda-auth apply` | **KEDA's namespace** | see below |
| plan | none: the controller writes the plan | | `update` by name on one ConfigMap in its own namespace |

Each Job is named with its version, then the operator's suffix
(`scaling-manager-preflight-v0-13-0-team-a`).
`ttlSecondsAfterFinished` stays, so a finished Job goes away; a re-run of the
same release deletes it first. Argo CD and Flux must not own the Jobs: they
would re-create one after its TTL. A component, `components/argocd-hooks`,
adds `argocd.argoproj.io/hook: Sync` for those who want the preflight run on
each sync.

### The KEDA certificate step, and why it runs in KEDA's namespace

KEDA presents its client certificate through a ClusterTriggerAuthentication,
which reads Secrets only in KEDA's own namespace. The copy never goes to a
model namespace or any namespace a tenant owns: whoever can read it can pose
as KEDA (`deploy/lib/scaler_tls.sh`, the KEDA section, and the
[security model](../reference/security-model.md)). Today the admin makes the
copy with their own kubeconfig.

The Job must not run in the controller namespace. There the owner holds
`admin`: they can run a pod as the Job's ServiceAccount or create a token for
it, and so use any right it has. So:

- **`keda-auth-setup` (admin, steady state, in `sm-admin/keda/`)**
  pre-creates, in KEDA's namespace, the target Secret
  `scaling-manager-external-scaler-<ctrl>` and, cluster-wide, the
  ClusterTriggerAuthentication that names it. Their content is known in
  advance. The Secret is `Opaque` with no `data` field, so a later apply or
  sync never empties what the Job wrote into it. The base ships a kustomize
  `configurations:` entry so that `nameSuffix` reaches the
  ClusterTriggerAuthentication's `secretTargetRef` names.
- **The marker** is in `controller-admin`, also with no `data`. The Job
  fills it. The owner can edit it, so like the stamp it is a convenience,
  not a control. A marker set early only breaks the owner's own install.
- **`steps/keda-auth` (admin, temporary, `sm-steps/keda-auth/`)** runs the
  Job in KEDA's namespace, as a ServiceAccount there. Its rights, all by
  `resourceNames`:
  - `get` on Secret `scaling-manager-external-scaler-client-tls` in the
    controller namespace;
  - `get`, `update` on the pre-created Secret in KEDA's namespace;
  - `get`, `update` on the marker ConfigMap;
  - `get`, `patch` on Deployment `scaling-manager-controller-manager`, for
    the restart annotation.

  No `create`, nothing cluster-wide, no right the owner can reach.
- The Job waits for the controller to fill the client Secret, copies it,
  sets the marker's `ready` key (a controller change: the marker counts only
  with it), and restarts the controller through a pod-template annotation,
  since the controller reads the client CA once at start. It then reads
  `scalerTLS` until `mutual TLS`, and checks KEDA's connection as
  `admin-finish` does.

**Rotation.** KEDA keeps one connection per scaler address until it
restarts. When the CA changes, the Job reports a stale connection and prints
the restart of KEDA's operator; it never restarts KEDA, which is shared.
Re-run the Job after a CA change.

A namespaced TriggerAuthentication is not used. If it ever is, it needs
KEDA 2.21 or later.

### The scaler TLS Secrets

The release file ships them with empty keys and is `kubectl create`d once:
applying it again would empty the certificates the controller wrote. Under
kustomize every file is applied each time, so the owner base cannot hold
them. The controller creates them instead, as it already fills them. That
needs `create` on Secrets in its own namespace, which cannot be limited by
name: a compromised controller could create any Secret there, including a
ServiceAccount token Secret for any ServiceAccount in that namespace. The
controller namespace holds only the scaling manager's own ServiceAccounts,
none with more rights than the controller's, so this is accepted here. It
is open question 2.

### Install settings and live switches

The settings base and the operator's generator are applied every time;
whatever they hold, the file wins over a live `kubectl patch`. So they hold
what the install declares, and nothing an operator flips day to day.
`passive` and the sharing mode move to the scaling policy (`switches/`),
which no apply touches. The same holds under Helm or any other renderer.

### What smt does that the bases must cover

| `smt` does | in this design |
| --- | --- |
| re-wires an HPA KEDA left on the CPU default (`scaledobjects-apply`) | KEDA 2.20 and later does not leave it. Below, the controller reports an unwired HPA in its status and as an Event with the fix; the plugin's `rewire` runs it |
| EPP ServiceMonitor only where nothing scrapes the EPP | `components/epp-monitoring`, left out where the router's monitoring values are on; the preflight FAILs on a double scrape |
| model-server PodMonitor, `--modelserver-port` | `components/modelserver-monitoring`; the port is a strategic-merge patch on the PodMonitor. Reuse llm-d's monitoring component instead where it is applied; the preflight FAILs on a double scrape |
| `--prometheus-ca FILE` | `components/prometheus-ca` plus a `secretGenerator` (`disableNameSuffixHash: true`) in `sm-owner/` |
| `--allow-http` | `components/allow-http`: an empty token path and `prometheusAllowHTTP=true` |
| `--quota`, `--set KEY=VALUE` | generator literals; the controller checks the values and reports a rejected key in its status |
| `--dset-role-writer` | the two `dset-role-writer` components |
| the weights labeller's watch list | stays in `smt` and the plugin; the preflight WARNs with the `kubectl patch` that adds the namespace |
| removing a model namespace | delete its directory first, then the lists ([above](#several-namespaces-one-budget)) |
| refuses a namespace another scaling manager manages, a namespaced install beside a cluster-wide one, a re-run that drops namespaces | preflight FAILs, run before the admin apply. A dropped namespace shows in `kubectl diff -k sm-admin/` |
| the stamp ConfigMap (the record of the last `admin-setup`) | not needed: the preflight reads the live admin objects. `smt` keeps it |
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

- The controller creates its scaler TLS Secrets (above).
- The marker counts only with its `ready` key.
- `/manager keda-auth apply`, a port of `llmsm_scaler_keda_auth_apply`
  (`deploy/lib/scaler_tls.sh`).
- The plan writer, a port of the discovery in `deploy/lib/scaledobject.sh`,
  and the unwired-HPA report.
- The release attaches `scaling-manager-kustomize-<release>.tar.gz`, in
  `SHA256SUMS` and signed. From phase 3, the plugin binaries and a krew
  manifest generated from `SHA256SUMS`, each binary with a cosign signature.
- A proposal to llm-d: `SCALING_MANAGER_VERSION` in `guides/env.sh`,
  beside `ROUTER_CHART_VERSION`, since their guides policy asks guides to
  take versions from `env.sh` rather than redefine them. It would name a
  release; the guide then fetches that release's tarball.

### Words used here

- **the marker**: ConfigMap `scaling-manager-scaler-keda-auth` in the
  controller namespace; the controller serves mutual TLS only when it says
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
   sm-admin/`. Expected: the version label, the `step` label, and the
   `keda-auth-setup` objects, which already exist.
2. The owner copies the live settings into the generator, leaving out
   `passive` and `utilizationShare`, and copies those two into the scaling
   policy's `default` entry first. Then `kubectl diff -k sm-owner/`. A key
   missing from the generator takes the shipped default on apply, so the copy
   is required.
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
  equal in count, and that the bases carry only the controller image. Its
  negative control: a base with one rule removed must fail.
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
- **The llm-d dry run.** If an llm-d guide ever installs the scaling manager
  this way, its overlay falls under that repository's kustomize dry-run CI
  job, as a tier-2 guide.

## Phases

| phase | what | effort |
| --- | --- | --- |
| 1 | `config/install` (replacing `config/overlays`), the components, `release-manifests.sh` from it, the parity check, the tarball; the controller creates its TLS Secrets; the switches move to the policy | 2 weeks |
| 2 | `/manager keda-auth apply`, the marker's `ready` key, the plan writer, the unwired-HPA report, the versioned Jobs; `smt` calls them; the kind e2e; the guide from a `guide.yaml` | 3 weeks |
| 3 | the plugin (`init`, `run`, `plan check`, `rewire`), krew manifest, release binaries | 2 weeks |
| 4 | the README leads with the kubectl path; `smt` stays documented beside it | 2 days |

Phases 1 and 2 can run in parallel: about seven weeks of work, five elapsed.

## Open questions

1. **Kustomize bases as a release asset.** Install-from-release ruled out a
   remote kustomize base on 2026-11-10. llm-d's own guides install an
   autoscaler controller as an overlay over a remote git-ref base. This
   proposal ships the bases as a signed, digest-pinned tarball, and keeps the
   git ref for development only. Is that acceptable?
2. **`create` on Secrets for the controller**, in its own namespace,
   unscoped by name ([above](#the-scaler-tls-secrets)). Accept it, or keep a
   `create`-only step outside the bases?
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
against Helm. The created-once Secrets go away once the controller creates
them. Settings drift is the same under kustomize.

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
