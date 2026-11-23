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
- **Needs a maintainer decision on:**
  - a new CRD, `ScalingManagerConfig`;
  - a git ref for llm-d recipes, where install-from-release ruled out a
    remote kustomize base on 2026-11-10;
  - the Helm rule in [AGENTS.md](../../AGENTS.md).

  See [Open questions](#open-questions).

## Summary

- **The ask:** install, configure, upgrade and uninstall with Kubernetes
  tools only, as llm-d's components and its neighbours install. No docker,
  and, outside the llm-d guides, no checkout.
- **The recommendation:** one manifest, applied by the cluster admin, the
  way KEDA, Kueue, LeaderWorkerSet and cert-manager install:
  1. `kubectl apply --server-side -f .../scaling-manager.yaml`: one signed,
     digest-pinned file.
  2. One config object, `ScalingManagerConfig`, holds Prometheus, the
     budget and the switches.
  3. `kubectl label namespace team-a scaling-manager.llmd.ai/managed=true`
     opts a namespace in.
  4. `kubectl get scalingmanagerconfig -o wide` shows whether it works: the
     controller runs the preflight itself and reports conditions.
  5. The controller publishes a ScaledObject plan; the operator reviews it
     and applies it.

  Passive, shadow and active are one field change each.
- **What it costs:**
  - The controller holds cluster-wide read rights, because RBAC cannot be
    scoped by a namespace label. These are the same rights as today's
    cluster-wide install.
  - Patching pods for active sharing stays a grant the admin gives per
    namespace.
  - More of the work moves into the controller.
- **For least privilege per namespace, or no cluster admin at all,** the
  reviewed per-namespace path stays as the
  [advanced path](#advanced-path-least-privilege-per-namespace). It takes
  about 16 commands and 11 small files for one namespace.
- `smt` keeps working on the same release files. Kustomize stays available
  for customising: the manifest is rendered from the same bases.

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

### KEDA, Kueue, LeaderWorkerSet, cert-manager

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

### What makes these installs feel native

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
docker, no checkout. Few commands, config in one object the operator can
keep in git. The passive first install and the preflight stay; the
admin/owner split stays available on the advanced path.

**Constraint.** [AGENTS.md](../../AGENTS.md): "the helm chart has been
removed. Do not re-introduce it or add helm chart features." The router
installs with Helm; KEDA, Kueue, LeaderWorkerSet and cert-manager install
with one manifest, and llm-d's autoscaler recipe with kustomize. The main
path follows the manifest, the llm-d path the recipe, and Helm has
[its own section](#alternative-helm-needs-a-maintainer-decision).

## The recommended install: one manifest

The cluster admin installs once. The controller runs in
`scaling-manager-system` and manages every namespace labelled for it.
Written as the future guide would read; `team-a` and `v0.13.0` are
placeholders.

### You need

- llm-d model servers and the router, scraped by Prometheus. On the llm-d
  guides, that means the router's monitoring values and the model servers'
  `monitoring` component ([Monitoring](#monitoring)). For scale from zero,
  also the router's flow control values (`featureGates: [flowControl]`).
- The router v1.0 or later; InferencePool `inference.networking.k8s.io` (v1)
  or `inference.networking.x-k8s.io` (v1alpha2). The release notes state the
  tested range of each release.
- KEDA 2.18 or later (2.21 recommended), running in namespace `keda` (else
  see [Customising](#customising-with-kustomize)). The Prometheus Operator.
- For a DisaggregatedSet: LeaderWorkerSet v0.10.0 or later.
- Your GPU product: the `GPU.PRODUCT` column of
  `kubectl get nodes -L nvidia.com/gpu.product`.
- Cluster-admin rights, `kubectl`, and `cosign` 3.0 or later.

### 1. Install

```bash
V=v0.13.0
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V
for f in scaling-manager-crds.yaml scaling-manager.yaml; do
  curl -fsSLO $URL/$f && curl -fsSLO $URL/$f.sigstore.json
  cosign verify-blob $f --bundle $f.sigstore.json \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/$V"
done
kubectl apply --server-side -f scaling-manager-crds.yaml
kubectl apply --server-side -f scaling-manager.yaml
```

The CRD is its own file, as cert-manager offers its CRDs apart: an
uninstall or a second install never takes it with the controller. The
manifest holds:

- the namespace `scaling-manager-system`;
- the controller's ServiceAccount, ClusterRole and Roles;
- the scaler's CA and serving Secrets, `Opaque` with no data, which the
  controller fills;
- the Deployment, pinned by digest, and its Services;
- the scaler NetworkPolicy admitting KEDA;
- in KEDA's namespace, the one Secret for KEDA's client certificate (with
  no data) and a Role on it by name;
- the ClusterTriggerAuthentication that names that Secret;
- two unbound ClusterRoles for the per-namespace grants
  ([below](#passive-shadow-active)).

It holds no config and no ScaledObject, so applying it again, as an upgrade
does, changes nothing you set.

### 2. Configure

One object, in the controller's namespace:

```yaml
# scaling-manager-config.yaml
apiVersion: scaling-manager.llmd.ai/v1alpha1
kind: ScalingManagerConfig
metadata:
  name: default
  namespace: scaling-manager-system
spec:
  prometheus:
    url: https://prometheus.monitoring.svc:9090
    # caFromSecret: true                       # Secret scaling-manager-prometheus-ca, key ca.crt
  # namespaceSelector:                         # which labelled namespaces may join (admin-owned)
  #   matchLabels: {scaling-manager.llmd.ai/managed: "true", tenant-tier: gpu}
  quota:
    scope: cluster                             # one budget shared by every managed namespace
    gpus:
      NVIDIA-H100-80GB-HBM3: 16
  # passive: true                              # live switches: defaults shown, see below
  # utilizationShare: "off"                    # off | shadow | active
  # disaggregatedSetRoleWriter: false
```

```bash
kubectl apply --server-side -f scaling-manager-config.yaml
```

Every key the settings ConfigMap has today maps to a field. The API server
validates the object when you apply it: a bad quota is refused there and
then, not found later in a log. A change to a field that is read at start
makes the controller restart itself, so nobody runs `rollout restart`.

**CRD or ConfigMap.** The alternative is to keep today's settings ConfigMap
and status ConfigMap. The recommendation is the CRD:

| | `ScalingManagerConfig` (CRD) | settings and status ConfigMaps |
| --- | --- | --- |
| validation | at admission, from the schema: types, enums, ranges | by the controller after the fact; the `settings` preflight check reports it |
| status | a status subresource with conditions; only the controller writes it | a second ConfigMap; anyone who may edit ConfigMaps there may write it |
| reading it | `kubectl get scalingmanagerconfig -o wide`, `kubectl describe`, `kubectl wait --for=condition=Ready`, `kubectl explain` | `kubectl get configmap -o jsonpath=...` per key |
| defaults | in the schema; visible on the object | in the controller |
| cost | a CRD file; an API to version (v1alpha1 first) and, later, convert; the controller reads the settings ConfigMap too for one release; the docs move | nothing new |

The cost is real, and it lasts: the API must be kept. Every controller an
llm-d cluster already runs is configured this way.

**The versioning rule:**

- Changes within v1alpha1 are additive only: a new optional field with a
  default.
- A new version ships only with a conversion plan and a migration of
  `status.storedVersions`. A conversion webhook needs its own serving
  certificate.
- Applying an older CRD does not refuse newer fields: the API server
  silently prunes the fields that schema does not know, from every object.
- Once a new storage version is listed in `storedVersions`, an older CRD
  that lacks it cannot be applied.

**Live switches under GitOps.** `passive`, `utilizationShare` and
`disaggregatedSetRoleWriter` are fields on the same object, flipped with
`kubectl`. **They are never in the git copy.** Under plain `kubectl apply
--server-side`, a field the file leaves out keeps whatever another manager
set. A field the file sets, and another manager changed, makes the apply
stop with a conflict. GitOps tools do not stop there: Argo CD with
`ServerSideApply=true` and Flux both force their ownership. A switch left
in git therefore silently resets a live patch on every sync. Applying
client-side, Argo CD can also be told to ignore them:

```yaml
# Argo CD Application
spec:
  syncPolicy:
    syncOptions:
    - ServerSideApply=true
    - RespectIgnoreDifferences=true
  ignoreDifferences:
  - group: scaling-manager.llmd.ai
    kind: ScalingManagerConfig
    jsonPointers:
    - /spec/passive
    - /spec/utilizationShare
    - /spec/disaggregatedSetRoleWriter
```

Flux needs no snippet when the git copy leaves the fields out. To keep the
whole object out of Flux's hands after it is created, annotate it
`kustomize.toolkit.fluxcd.io/ssa: IfNotPresent`.

Checked with plain `kubectl` on a throwaway kind cluster (v1.31.2). A
ConfigMap was applied server-side, a key it did not carry was patched by
another field manager, and the same file was applied again: the patched key
stayed. A key the file did carry, patched the same way, made the next apply
fail with a conflict. Phase 1 repeats both on the CRD under Argo CD and
Flux, and asserts the reset case: a switch left in git is overwritten by the
next sync.

A field read at start makes the controller restart itself. Flipped again and
again, that puts the Pod into the kubelet's crash back-off, which grows to
five minutes. The `Ready` condition says when a restart is pending.

### 3. Opt namespaces in

```bash
kubectl label namespace team-a scaling-manager.llmd.ai/managed=true
```

The controller watches namespaces with that label and manages the model
servers in them. Removing the label stops it, at the next cycle: no
directory to delete, no list to edit.

The label alone is not a consent boundary. A tenant who may create
namespaces may label them, and tenant tooling that sets namespace labels,
such as Capsule or Argo CD's `managedNamespaceMetadata`, can join a
cluster-scope shared budget. So it is enforced in two places:

- **`spec.namespaceSelector`** on the config object, which only the admin
  writes, decides which labelled namespaces are managed. Set it to a label
  only the admin sets, or to a list of names (`matchExpressions` on
  `kubernetes.io/metadata.name`).
- **The scaler and the controller refuse** a KEDA call or a workload for a
  namespace the selector does not admit. Today the scaler registers any
  namespace KEDA sends ([security model](../reference/security-model.md)).
  Each refused namespace is listed in the `NamespacesManaged` condition.

Left unset, `namespaceSelector` admits every namespace that carries the
label. That default enforces nothing beyond the label itself, and the
`Ready` condition warns about it under `quota.scope: cluster`. Set the
selector whenever a cluster-scope budget is shared.

Optionally, the release ships a ValidatingAdmissionPolicy,
`scaling-manager-label-guard.yaml`, that lets only named groups set the
label.

```bash
kubectl wait --for=condition=Ready scalingmanagerconfig/default -n scaling-manager-system --timeout=5m
```

### 4. Verify

The controller runs the preflight itself: at start, and then every cycle.
Each check becomes a condition on the config object, and each change of a
check becomes an Event. There is no preflight Job on this path. Illustrative
output:

```text
$ kubectl get scalingmanagerconfig -n scaling-manager-system -o wide
NAME      READY   PASSIVE   SHARING   NAMESPACES   PROMETHEUS                               MTLS          AGE
default   True    true      off       2            https://prometheus.monitoring.svc:9090   mutual TLS    5m

$ kubectl describe scalingmanagerconfig default -n scaling-manager-system
...
Status:
  Managed Namespaces:  team-a, team-b
  Conditions:
    Type                 Status  Reason             Message
    Ready                True    AllChecksPass
    PrometheusReachable  True    QueryOK            https://prometheus.monitoring.svc:9090
    KEDAConnected        True    MutualTLS          KEDA's copy is current
    MetricsScraped       True    OneScrapePerSource vllm: series for 2 namespaces; EPP series for 2 InferencePools
    NoOverlap            True    NoOtherController
    PolicySource         True    ControllerPolicy   no policy namespace resolves
Events:
  Normal   NamespaceManaged    2m   scaling-manager  team-b: labelled, 3 model servers found
  Warning  MetricsMissing      9m   scaling-manager  team-b: no vllm: series; is the monitoring component applied?
```

Every check of today's [preflight](../reference/preflight.md) carries over
as a condition. The ones that need a cluster read before the install
(`keda.crd`, `keda.version`) are checked at start. A failed check marks
`Ready` false and names the fix, as a preflight line does today.

### 5. Register the models

The controller publishes a suggested plan: one ScaledObject per model
server, and per role on a DisaggregatedSet. It writes the plan to the
ConfigMap `scaling-manager-scaledobjects-plan` in `scaling-manager-system`,
one key per managed namespace. The manifest creates that ConfigMap empty.
Applying the plan stays a person's or a GitOps tool's step:

```bash
NS=team-a
kubectl get configmap scaling-manager-scaledobjects-plan -n scaling-manager-system \
  -o jsonpath="{.data.$NS}" > scaledobjects-$NS.yaml      # review: the bounds, the modelID
kubectl apply -f scaledobjects-$NS.yaml -n $NS
```

The plan holds every managed namespace's plan, so it stays in the
controller's namespace, which only the admin reads. It is read from
workloads the namespace's owner controls, so it is untrusted input. The
controller writes only `ScaledObject` objects into it, each with the
external trigger at its own scaler and the
`app.kubernetes.io/managed-by: llm-d-scaling-manager` label. Read it before
applying. For a model added by hand, `scaledobject/external/` in the bases
is a checked-in overlay in the style of the llm-d guides.

**The command count**, one namespace or several: install (with the
download and verify), configure, label each namespace, verify, and register
each namespace. That is 5 commands beyond the download, against `smt`'s 3
and the advanced path's about 16.

### Passive, shadow, active

Each step is one field:

```bash
C="kubectl patch scalingmanagerconfig default -n scaling-manager-system --type merge -p"
$C '{"spec":{"utilizationShare":"shadow"}}'     # sharing computes and publishes, moves nothing
$C '{"spec":{"passive":false}}'                 # the controller scales
$C '{"spec":{"utilizationShare":"active"}}'     # sharing moves replicas
```

Active sharing scales a donor down by patching its pods, so each namespace
that shares needs the admin's grant first. The manifest ships the
ClusterRole unbound. The admin binds it per namespace from the release's
`scaling-manager-grants.yaml`, whose RoleBindings carry the install's
labels, so the uninstall finds them:

```bash
kubectl apply -n team-a -f scaling-manager-grants.yaml -l scaling-manager.llmd.ai/grant=pod-writer
```

Until then the `Ready` condition names the namespaces where active sharing
cannot act.

**Undo sharing.** Set the mode back to shadow and wait one cycle, so no
transfer marks are left on pods:

```bash
$C '{"spec":{"utilizationShare":"shadow"}}'
kubectl wait --for=jsonpath='{.status.sharing}'=shadow scalingmanagerconfig/default -n scaling-manager-system
# one cycle later (15 s):
$C '{"spec":{"utilizationShare":"off"}}'
kubectl delete -n team-a -f scaling-manager-grants.yaml -l scaling-manager.llmd.ai/grant=pod-writer
```

The budget stays.

When a policy namespace resolves (`scaling-manager-policy`, or the
label), the cluster policy there decides the limiters and the optimizer, as
today. The `PolicySource` condition says so, and that the object's sharing
field has no effect ([scaling policy](../reference/scaling-policy.md#configmap-structure)).
A model namespace's own policy map replaces the global one for its models;
the same condition names it.

### A DisaggregatedSet

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
     kubectl patch scalingmanagerconfig default -n scaling-manager-system --type merge \
       -p '{"spec":{"disaggregatedSetRoleWriter":true}}'
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

### Monitoring

Each scrape must exist once: a second doubles every rate, and a missing one
leaves the analyzers blind. The `MetricsScraped` condition reports both.

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
confirmed. The `MetricsScraped` condition names any role with no `vllm:`
series.

On GKE with Managed Prometheus, llm-d uses `PodMonitoring`. Our monitors are
Prometheus Operator objects, so scrape with GMP instead.

### Upgrade and rollback

```bash
kubectl apply --server-side -f scaling-manager-crds.yaml   # the new release's, verified as above
kubectl apply --server-side -f scaling-manager.yaml
```

The config object and the ScaledObjects are not in the manifest, so they
stay as they are. A field the new release adds takes its schema default.
Under GitOps, merge the version bump.

**Rollback** follows the [versioning rule](#2-configure):

- **Within v1alpha1,** apply the old release's manifest and CRD. The old
  schema then prunes, silently, any newer field you set, so take it out of
  the config object first, or know you lose it.
- **After a new storage version,** the old CRD cannot be applied. Roll the
  controller back with the old manifest alone, which must still read the new
  version, or not at all.

### Uninstall

Every selector is scoped to this install's `app.kubernetes.io/instance`, so
another install's objects are never touched:

```bash
I=app.kubernetes.io/instance=scaling-manager-system
C="kubectl patch scalingmanagerconfig default -n scaling-manager-system --type merge -p"
# 1. Sharing to shadow, wait a cycle (15 s), then the ScaledObjects.
$C '{"spec":{"utilizationShare":"shadow"}}'
kubectl delete scaledobject -A -l "$I,app.kubernetes.io/managed-by=llm-d-scaling-manager"
# 2. The per-namespace grants and monitors, and the labels.
kubectl delete rolebinding,podmonitor,servicemonitor,serviceaccount,secret -A -l "$I"
kubectl delete clusterrolebinding -l "$I"
kubectl label namespace -l scaling-manager.llmd.ai/managed=true scaling-manager.llmd.ai/managed-
# 3. Everything the manifest made, the config object with its namespace.
kubectl delete -f scaling-manager.yaml
# 4. The CRD, only when no other install still has a config object.
[ -z "$(kubectl get scalingmanagerconfig -A -o name)" ] && kubectl delete -f scaling-manager-crds.yaml
# 5. Nothing left:
kubectl get all,configmap,secret,role,rolebinding,servicemonitor,podmonitor,networkpolicy,scaledobject -A -l "$I"
kubectl get clusterrole,clusterrolebinding,clustertriggerauthentication -l "$I"
```

`kubectl delete -f` deletes the namespace `scaling-manager-system`, as
KEDA's and Kueue's do theirs. It holds only the scaling manager, the scaler
CA included.

Step 2 removes the label from every labelled namespace, which assumes one
main-path install per cluster. With more than one, remove it only from the
namespaces this install's selector admits, which `kubectl describe` lists
under `Managed Namespaces`.

### Customising with kustomize

The manifest is `kubectl kustomize config/install/cluster`, rendered by the
release with the image digest. To change it, write a kustomization over the
release's kustomize tarball (`scaling-manager-kustomize-$V.tar.gz`, signed
the same way):

- **KEDA elsewhere.** KEDA's namespace must be known at install: the
  NetworkPolicy, the client-certificate Secret and its Role name it. The
  manifest assumes `keda`. Elsewhere, add the `keda-namespace` component
  with your namespace, or edit the downloaded file after verifying it, since
  the signature covers the file as downloaded. For KEDA in `keda-system`:

  ```bash
  cosign verify-blob scaling-manager.yaml ...                # as above, on the file as downloaded
  sed -i 's/^\(  *namespace: \)keda$/\1keda-system/; s/kubernetes.io\/metadata.name: keda$/kubernetes.io\/metadata.name: keda-system/' scaling-manager.yaml
  ```

  The edit renames the namespace of the two KEDA-side objects and the
  NetworkPolicy's KEDA selector. Repeat it on every upgrade.
- **OpenShift.** The `openshift` component: the service CA, and
  `openshift-keda`.
- **Air-gapped.** Mirror the image by digest (`skopeo copy --all`), verify
  it with `cosign verify` first, and add one `images:` entry. The
  controller is the only image.

### The rights it holds

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

- `get`, `watch` and `update` on `scalingmanagerconfigs` and their status, in
  its own namespace;
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

## Advanced path: least privilege per namespace

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
- **The config.** The `ScalingManagerConfig` lives in the owner's namespace.
  The admin installs the CRD and a ClusterRole that aggregates to `admin`,
  so the owner may write it.

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
| `sm-owner/` | `$CTRL` | `owner`, and the `ScalingManagerConfig` |

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
ServiceAccount ([the renders](#what-the-renders-showed)).

The steps, for `CTRL=team-a`, with `kubectl diff -k` before each apply:

```bash
kubectl apply --server-side -f scaling-manager-crds.yaml                # admin, once: the CRD and its aggregate-to-admin ClusterRole
kubectl create rolebinding scaling-manager-owner -n $CTRL \
  --clusterrole=admin --user=<the owner>                                # admin, once
kubectl apply -k sm-steps/preflight/                                    # admin: preflight run 1
kubectl logs -f -n $CTRL job/scaling-manager-preflight-${V//./-}-$CTRL  # podmonitor.port, metrics.model FAIL: expected on run 1
kubectl apply -k sm-admin/                                              # admin
kubectl apply --server-side -k sm-owner/                                # owner: the config object and the controller
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

The config object lives in `sm-owner/` only, applied server-side, and the
switch fields stay out of it, as on the main path. The scaler TLS Secrets
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

## The llm-d path

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

## Designs compared

| | one manifest (recommended) | advanced path (kustomize, Jobs) | plugin over the advanced path | Helm | `smt` today |
| --- | --- | --- | --- | --- | --- |
| operator commands, one namespace | 5, after download and verify | about 16 | 3 | 2 (admin and owner charts), plus the preflight | 3, plus `docker` setup |
| files the operator keeps | 1 config object | 11 small kustomizations | 11, written for you | 2 values files | none |
| who installs | cluster admin | admin, then namespace owner | as the advanced path | admin and owner | admin and owner |
| controller rights | cluster-wide reads | per namespace | per namespace | either | either |
| config | `ScalingManagerConfig`, validated | the same object, in the owner's namespace | flags | values | flags, settings ConfigMap |
| preflight | continuous, as conditions | Jobs | Jobs | hook Jobs | in the commands |
| multi-namespace | a label each | a directory and two list entries each | `--model-namespace` | a list in values | `--model-namespace` |
| upgrade | apply the new manifest | new tarball, `diff`, apply, preflight | new binary | `helm upgrade` | new image, same commands |
| GitOps | yes; the switches stay out of git | yes; the steps stay out of the sync | yes, from the written files | yes | no |
| needs docker or a checkout | no | no | no | no | docker |

## For developers

### What has to be built

The main path moves work from the operator into the controller.

- **The `ScalingManagerConfig` CRD** (v1alpha1). It has:
  - a schema with every settings key, defaults and enums;
  - a status subresource with conditions;
  - printer columns for `-o wide`.

  The controller reads the settings ConfigMap too for one release, the
  object winning, and a field read at start restarts it.
- **The preflight, in the controller.** Each check of `internal/preflight`
  runs at start and then every cycle, and becomes a condition on the
  object, with an Event when it changes. New checks:
  - conflicts: another controller in a labelled namespace, by its
    RoleBindings and ClusterRoleBindings, never its Deployment;
  - `PolicySource`;
  - a double scrape, a missing EPP scrape, and the EPP metric names this
    release reads (llm-d's `env.sh` tracks `ROUTER_EPP_VERSION=main`).
- **The label watch.** The controller manages namespaces labelled
  `scaling-manager.llmd.ai/managed=true`. That replaces
  `LLMSM_WATCH_NAMESPACE` on this path; the variable stays for the advanced
  path. The opt-in is enforced, because the label alone is not a consent
  boundary:
  - `spec.namespaceSelector` is admin-owned;
  - the scaler refuses KEDA calls for a namespace it does not admit (today
    it registers any namespace KEDA sends), and the controller refuses
    workloads there;
  - the refused namespaces appear in a condition;
  - an optional ValidatingAdmissionPolicy limits who may set the label.
- **Its own serving TLS, without `create` on Secrets.** The manifest
  carries the scaler's CA and serving Secrets as `Opaque` with no `data`,
  under new names. The controller fills and rotates them with `get` and
  `update` by name. A server-side re-apply keeps what it wrote: checked on
  kind, where a Secret applied with no data, filled by another field
  manager, and applied again kept its data. On first start over an install
  made with `smt`, the controller copies the old CA across, so KEDA's open
  connection keeps working.
- **KEDA's copy, in the controller.**
  - The manifest pre-creates the one target Secret in KEDA's namespace
    (`Opaque`, no `data`) and the ClusterTriggerAuthentication that names
    it.
  - A Role in KEDA's namespace gives the controller `get` and `update` on
    that Secret by name.
  - There is no Job, no `create` on Secrets, no key in a model namespace,
    and no marker.

  KEDA's namespace must be known at install: `keda` by default, else the
  `keda-namespace` component or the documented edit.
- **The CA re-read.** The controller switches to mutual TLS once its copy is
  written, without a restart. It reports KEDA's connection (`none`,
  `current`, `stale`; `kedaCopyStale`) as the `KEDAConnected` condition.
  KEDA keeps one connection per scaler address until it restarts, so on a
  CA change the condition names the KEDA restart; the controller never
  restarts KEDA.
- **The plan writer**, a port of the discovery in
  `deploy/lib/scaledobject.sh`. It writes into the pre-created plan
  ConfigMap. An unwired-HPA report goes with it, for KEDA before 2.20.
- **The release.**
  - It attaches `scaling-manager.yaml` and `scaling-manager-monitoring.yaml`,
    both rendered from `config/install`.
  - It attaches the kustomize tarball, all in `SHA256SUMS` and signed.
  - From phase 3 it attaches the plugin binaries, each with a cosign bundle,
    and a krew manifest generated from `SHA256SUMS`.

### What smt does, and where it goes

| `smt` does | main path | advanced path |
| --- | --- | --- |
| the namespaces it manages | the label | `LLMSM_WATCH_NAMESPACE` patch and the Namespace reader's list |
| the preflight | conditions, continuous | the preflight Job |
| refuses an overlapping install | the conflict checks, as a condition; the controller manages nothing it overlaps on | the conflict checks in run 1 |
| KEDA's client certificate | the controller, one Secret by name | the `keda-auth` Job, in KEDA's namespace |
| `--prometheus-url`, `--prometheus-ca`, `--allow-http`, `--quota`, `--set` | fields of `ScalingManagerConfig`; the CA in a Secret it names | the same object |
| `--utilization-share`, `passive` | fields | fields |
| `--dset-role-writer` | a field, and the admin's grant per namespace | the same |
| the model-server PodMonitor, `--modelserver-port` | llm-d's component, or `scaling-manager-monitoring.yaml` | the `modelserver-monitoring` component, and a patch |
| the EPP ServiceMonitor where nothing scrapes it | the same file | `components/epp-monitoring` |
| `scaledobjects-plan` / `-apply`, the HPA re-wiring | the controller's plan, applied by a person; the unwired-HPA report | the same |
| the weights labeller's watch list | stays in `smt` and the plugin; a condition names the patch | the same |
| the stamp | not needed | not needed |

`smt` keeps every command, now applying the same rendered files; `prepull`,
`weights`, `engine-cache` and `workload-patch` do not change. One tree,
`config/install`, feeds the tools image, the release files, the manifest
and the tarball.

### The advanced path's internals

The sections below are for the advanced path only.

#### The Jobs

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

#### The preflight conflict checks

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

#### The KEDA certificate step, and why it runs in KEDA's namespace

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

#### The scaler TLS Secrets

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

#### What the renders showed

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

#### Words used here

- **the marker**: ConfigMap `scaling-manager-scaler-keda-auth` in the
  controller namespace; the controller serves mutual TLS only once it says
  KEDA's side is in place.
- **the stamp**: ConfigMap `scaling-manager-admin-setup`, which `smt
  admin-setup` writes so `smt install` can tell the admin step ran.
- **`scalerTLSAction`**: the key in `scaling-manager-controller-status` that
  prints what to run to give KEDA its certificate.

## Migration

**From a cluster-wide `smt` install** (`scaling-manager-system`), to the
main path:

1. Write `ScalingManagerConfig` from the live settings:
   `kubectl get configmap scaling-manager-settings -n scaling-manager-system -o yaml`
   has every key. Apply it.
2. Label each namespace the install named with `--model-namespace`.
3. `kubectl apply --server-side -f scaling-manager.yaml`.
   - The names, the namespace and the labels are those of today's
     cluster-wide files.
   - The new TLS Secrets take the old CA, so KEDA's connection stays up.
   - KEDA's copy keeps its name.
4. The settings ConfigMap is still read for one release; delete it once
   `kubectl describe` shows the object as the source.

**From a namespaced install**, either:

- move to the main path: uninstall it, keeping the ScaledObjects, then
  install as above and point them at the new scaler with the plan; or
- stay namespaced on the advanced path. The admin writes `sm-admin/` (or
  runs `kubectl scaling-manager init`) and runs `kubectl diff -k`, which
  should show only labels, the empty plan ConfigMap and the KEDA objects
  that exist already. The owner moves the settings into the config object.

An install made with `make deploy-scaling-manager` is not adopted, as for
`smt` today.

## Verification

- **A kind e2e for the main path**, `make test-e2e-manifest-install`, beside
  `test-e2e-release-manifests` and `test-e2e-tools-install`, on
  `test-e2e-release-manifests-setup` ([testing](../developer-guide/testing.md)).
  It applies the rendered `scaling-manager.yaml` and the config object,
  labels the model namespace, and waits for `Ready` on the conditions. It
  then registers a model from the plan, walks passive, shadow and active
  with the grant, runs the smoke suite, uninstalls, and runs the leftovers
  check. A second leg upgrades from the previous release's manifest. Images:
  the locally built controller loaded into kind, the prerequisites from
  `registry.k8s.io` and `quay.io` as today, never docker.io. Ginkgo runs
  with `-v`; a 0-spec run fails.
- **The advanced path** keeps its e2e, `make test-e2e-kustomize-install`,
  and the render-parity check, `hack/check-kustomize-install.sh` in `make
  lint-deploy-scripts`. The check compares `kubectl kustomize` against the
  release files, object for object, on non-empty, equal-count sides. It
  covers the [renders](#what-the-renders-showed), and its negative control
  is a base with one rule removed.
- **The manifest.** `hack/check-release-manifests.sh` gains
  `scaling-manager.yaml`:
  - in `SHA256SUMS` and signed;
  - the image by digest, and the controller image only;
  - no config object and no ScaledObject;
  - a re-apply over a live install changes nothing, checked server-side in
    the e2e.
- **GitOps.** The e2e applies the config object without the switch fields,
  flips them with `kubectl patch`, re-applies the file server-side, and
  checks the switches held.
- **The llm-d dry run.** An llm-d recipe falls under that repository's
  kustomize dry-run CI job, as a tier-2 guide.

## Phases

| phase | what | effort |
| --- | --- | --- |
| 1 | `config/install` (replacing `config/overlays`), `scaling-manager.yaml` and the tarball rendered from it, the parity check; the `ScalingManagerConfig` CRD and its reader, with the settings fallback; check whether the cluster-wide `configmaps` read can be narrowed | 3 weeks |
| 2 | the controller: the preflight as conditions with the new checks, the label watch with the namespace selector and the scaler refusing unadmitted namespaces, the optional label ValidatingAdmissionPolicy, its own TLS without `create`, KEDA's copy and the CA re-read, the plan writer; the kind e2e; the guide from a `guide.yaml` | 4 weeks |
| 3 | the advanced path: the Jobs, the steps, its e2e. `epp-monitoring` has fixed names, so two unscraped EPPs in one model namespace would collide: name its objects per EPP | 2 weeks |
| 4 | the plugin (`init`, `plan check`, `rewire`), krew manifest, release binaries; the README leads with the manifest | 2 weeks |

About eleven weeks of work; phases 1 and 2 overlap, so about eight elapsed.
More is controller work than before, and the operator's part is 5 commands.

## Open questions

1. **The CRD.** Accept a new API, `ScalingManagerConfig` v1alpha1, in place
   of the settings ConfigMap, with one release of fallback?
2. **Cluster-wide reads by default.** The main path needs the cluster-wide
   read rights of today's cluster-wide install. Is that the right default,
   with the advanced path for least privilege?
3. **The git ref for llm-d.** Install-from-release ruled out a remote
   kustomize base on 2026-11-10. An llm-d recipe needs one, pinned to a
   release commit with the image pinned by digest, as llm-d's autoscaler
   recipe does. Allow it for that use?
4. **The cluster-wide `configmaps` read.** Narrow it to the policy map's
   name, if every read the controller makes allows it?
5. **Kustomize floor** for the advanced path: which oldest `kubectl`?
6. **The weights labeller.** It stays an `smt` and plugin step. Is a
   condition naming the patch enough?

## Alternative: Helm (needs a maintainer decision)

AGENTS.md forbids a Helm chart, so this is not the recommendation. It is
here because the router installs with Helm:

- every llm-d operator already runs `helm` for the router;
- `helm upgrade` and `helm uninstall` give upgrade and removal;
- Flux's helm-controller runs hooks, and Argo CD maps Helm hooks to its
  sync hooks.

With one manifest, a chart becomes very thin. It would be the same
manifest, cut into `templates/` by CI, with three values: KEDA's namespace,
OpenShift, and the image repository. Config stays in the
`ScalingManagerConfig`, not in values. That thin chart, generated in CI
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
