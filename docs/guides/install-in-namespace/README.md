# Install the scaling manager in a namespace

## Overview

This guide installs the scaling manager (llm-d-scaling-manager) into one namespace,
from the files attached to a release, with `kubectl`. It sizes the llm-d model
servers in that namespace and nothing else. It decides how many replicas each
variant needs and hands that number to KEDA, which does the scaling.

It installs **passive**: the controller decides and publishes, and changes no
replica count until you switch it on ([step 5](#let-it-scale)).

A cluster admin applies the `part=admin` objects once
([step 1](#1-cluster-admin-prepare-the-namespace)) and wires KEDA
([step 4](#4-cluster-admin-give-keda-its-client-certificate)). The namespace
owner does the rest, and upgrades the controller.

Tested: the namespaced and the cluster-wide files on Kubernetes (kind, `make
test-e2e-release-manifests`, in CI and before each release). The OpenShift
files are not yet tested end to end.

Prefer a checkout and `make`? See [From a checkout instead](#from-a-checkout-instead).

## Prerequisites

- [ ] **llm-d model servers in the namespace.** No model yet? [Install a small
  llm-d model](../install-small-model/).
- [ ] **KEDA**, 2.18 or later; 2.21 is recommended: older KEDA can leave a new
  ScaledObject [unwired](../../reference/keda.md#unwired-hpa-before-keda-220)
  after the controller restarts. On OpenShift, the Custom Metrics Autoscaler
  operator. See the [KEDA reference](../../reference/keda.md).
- [ ] **A Prometheus** with the Prometheus Operator, scraping the model servers'
  EPP. The model-server monitor is in the release files.
- [ ] **For scale-from-zero only: the EPP's `flowControl` feature gate.** Add
  `featureGates: [flowControl]` to the `EndpointPickerConfig` and restart the
  EPP. Without it, a model at zero never wakes.
- [ ] `kubectl` and `curl`. A cluster admin for steps 1 and 4. On OpenShift,
  use `scaling-manager-namespaced-openshift.yaml`; it is not yet tested end to
  end.

<!-- guide:prerequisites.download start -->
```bash
# The release files. Pick the release you install.
V=<release, for example v0.11.0>
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V
for f in namespaced settings preflight scaler-tls model-namespace keda-auth; do
  curl -fsSLO $URL/scaling-manager-$f.yaml
done
```
<!-- guide:prerequisites.download end -->

Each file is pinned to the release's image by digest, and signed. Check them
now, before the `sed` below changes them: [Verify the release
files](../../reference/install-methods.md#verify-the-release-files).

<!-- guide:prerequisites.namespace start -->
```bash
# The namespace your model servers run in. The controller runs there too.
export NAMESPACE=<your-namespace>
for f in scaling-manager-*.yaml; do
  sed -i -e "s/__NAMESPACE__/$NAMESPACE/g" -e "s/__MODEL_NAMESPACE__/$NAMESPACE/g" "$f"
done
```
<!-- guide:prerequisites.namespace end -->

## Installation Instructions

### 1. Cluster admin: prepare the namespace

**Needs cluster-admin rights.** Steps 2, 3 and 5 do not: a namespace admin runs them.

<!-- guide:deploy.admin start -->
```bash
# Cluster admin, once per namespace. RBAC, ServiceAccounts, the monitors, the scaler NetworkPolicy, and the preflight's read-only RBAC.
kubectl apply -f scaling-manager-namespaced.yaml -l scaling-manager.llmd.ai/part=admin
kubectl apply -f scaling-manager-preflight.yaml -l scaling-manager.llmd.ai/part=admin
kubectl apply -f scaling-manager-model-namespace.yaml
```
<!-- guide:deploy.admin end -->

The namespace must already exist; no release file creates or deletes one. The
`part=admin` label selects the ClusterRoles and their bindings, the Roles,
the ServiceAccount, the ServiceMonitor and the NetworkPolicy; in the
preflight file, its ServiceAccount, Role, ClusterRole and their bindings. The
model-namespace file is all `part=admin`: the model-server PodMonitor and the
controller's Role. Names carry the namespace, so two installs never share one. The full list and why each
needs an admin: [Cluster-admin setup](../admin-cluster-setup/).

### 2. Settings and the preflight check

Open `scaling-manager-settings.yaml` and set `prometheusURL`, the https URL of
the Prometheus (or Thanos Querier) that scrapes your model servers. Every
other key has a default; each is explained in the file. Then create it:

<!-- guide:deploy.settings start -->
```bash
# Set prometheusURL in scaling-manager-settings.yaml first. Created once, never applied again.
kubectl create -f scaling-manager-settings.yaml
```
<!-- guide:deploy.settings end -->

An upgrade never touches this ConfigMap: it is not in the main file, and a
second `create` fails with `AlreadyExists`. Change it later with `kubectl
patch` ([Configuration](#configuration)).

If a private CA signed Prometheus's certificate, give the controller that CA:

<!-- guide:deploy.promca start -->
```bash
# Only when a private CA signed Prometheus's certificate.
kubectl create secret generic scaling-manager-prometheus-ca -n $NAMESPACE --from-file=ca.crt=<ca file>
kubectl patch configmap scaling-manager-settings -n $NAMESPACE --type merge \
  -p '{"data":{"prometheusCACertPath":"/etc/scaling-manager-prometheus-ca/ca.crt"}}'
```
<!-- guide:deploy.promca end -->

Now check the prerequisites. The preflight runs as a Job, reads the cluster
and changes nothing. Its RBAC came with step 1; this applies the Job alone:

<!-- guide:deploy.preflight start -->
```bash
# Read-only. Prints PASS, WARN, FAIL or UNKNOWN per check, and the fix.
kubectl delete job scaling-manager-preflight -n $NAMESPACE --ignore-not-found
kubectl apply -f scaling-manager-preflight.yaml -l scaling-manager.llmd.ai/part=controller
until kubectl get job scaling-manager-preflight -n $NAMESPACE \
    -o jsonpath='{.status.conditions[?(@.status=="True")].type}' | grep -qE 'Complete|Failed'; do sleep 3; done
kubectl logs -n $NAMESPACE job/scaling-manager-preflight
```
<!-- guide:deploy.preflight end -->

Fix every `FAIL` and run it again. Each line prints its fix. The common one:
KEDA does not run in namespace `keda`, which the scaler NetworkPolicy admits.
The line then prints a `kubectl patch` for the policy step 1 applied. Make the
same change in the file, so applying it again (an upgrade) keeps it:

```bash
# KEDA runs in keda-system, for example
sed -i 's#kubernetes.io/metadata.name: keda$#kubernetes.io/metadata.name: keda-system#' scaling-manager-namespaced.yaml
```

`UNKNOWN` is not a pass: that check was not made. Before the install some are
UNKNOWN because the controller does not exist yet. Every check, and what it
needs: [Preflight check](../../reference/preflight.md).

When you are done with the preflight (after step 4's run), the cluster admin
removes its cluster-wide reads. To run it again later, they apply its
`part=admin` objects again, as in step 1:

<!-- guide:deploy.preflightdone start -->
```bash
# Cluster admin, after the last run. The preflight's cluster reads are needed only while it runs.
kubectl delete clusterrolebinding scaling-manager-preflight-$NAMESPACE --ignore-not-found
```
<!-- guide:deploy.preflightdone end -->

### 3. Install the controller

<!-- guide:deploy.controller start -->
```bash
# The scaler's certificate Secrets (created once, empty; the controller fills them), then the controller.
kubectl create -f scaling-manager-scaler-tls.yaml
kubectl apply -f scaling-manager-namespaced.yaml -l scaling-manager.llmd.ai/part=controller
kubectl rollout status deployment/scaling-manager-controller-manager -n $NAMESPACE
```
<!-- guide:deploy.controller end -->

`scaling-manager-scaler-tls.yaml` is created once, never applied: applying it
over Secrets the controller filled would empty them. The controller mints the
scaler's CA and certificates into them.

### 4. Cluster admin: give KEDA its client certificate

KEDA reaches the scaler over mutual TLS. Until KEDA holds a client
certificate, the scaler serves plaintext and the controller says what to run:

<!-- guide:deploy.kedaauth start -->
```bash
# Cluster admin, in this directory. Prints the steps that give KEDA its client certificate.
export KEDA_NS=<the namespace KEDA runs in>
kubectl get configmap scaling-manager-controller-status -n $NAMESPACE \
  -o jsonpath='{.data.scalerTLSAction}{"\n"}'
```
<!-- guide:deploy.kedaauth end -->

In the directory that holds the release files, run the text after `then
restart the controller:` in step 1; step 2 only on a reinstall. It copies the
client certificate into KEDA's namespace, applies
`scaling-manager-keda-auth.yaml` (KEDA's ClusterTriggerAuthentication and the
marker the controller reads), and restarts the controller. Then:

<!-- guide:deploy.mtls start -->
```bash
# Prints "mutual TLS" once KEDA's side is in place and the controller restarted.
kubectl get configmap scaling-manager-controller-status -n $NAMESPACE \
  -o jsonpath='{.data.scalerTLS}{"\n"}'
```
<!-- guide:deploy.mtls end -->

Run the preflight from step 2 again: `keda.mtls`, `keda.networkpolicy` and
`rbac.controller` should now read PASS. `scaler.tls` reads UNKNOWN: the
NetworkPolicy admits the scaler port from KEDA's namespace only, so the
preflight's own handshake is dropped.

### 5. Register the workloads, then let it scale

#### Make the workloads safe to scale

A scale-down without a drain hook cuts the responses a replica is still
writing, and a scale-up without a model cache downloads the weights again.
The release's tools image reports what each model server still needs:

<!-- guide:deploy.readiness start -->
```bash
# With the release's tools image; no checkout. Reports what the model servers still need. Changes nothing.
docker run --rm --user "$(id -u):$(id -g)" -v "$HOME/.kube/config:/kube/config:ro" -v "$PWD:/work" \
  ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools:$V workload-patch -n $NAMESPACE
```
<!-- guide:deploy.readiness end -->

It writes a patch rather than changing your workloads. To act on it, and to
make each new replica start faster, follow [Prepare model servers to
scale](../prepare-workloads/): one recommended configuration, a few commands.

#### Register the workloads

Nothing is sized until a ScaledObject names the scaler. One per model-server
Deployment:

<!-- guide:deploy.register start -->
```bash
# One ScaledObject per model-server Deployment. Edit the names, the modelID and the bounds.
cat > scaledobject.yaml <<EOF
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: <deployment>-llmsm
  namespace: ${NAMESPACE}
spec:
  scaleTargetRef:
    name: <deployment>
  minReplicaCount: 1
  maxReplicaCount: 10
  triggers:
  - type: external-push
    name: scaling-manager-external-scaler
    metricType: AverageValue
    metadata:
      scalerAddress: scaling-manager-external-scaler.${NAMESPACE}.svc:9090
      modelID: <the model it serves>
    authenticationRef:
      name: scaling-manager-external-scaler-${NAMESPACE}
      kind: ClusterTriggerAuthentication
EOF
kubectl apply -f scaledobject.yaml
```
<!-- guide:deploy.register end -->

Set `minReplicaCount` and `maxReplicaCount` so the current replica count lies
between them. A model scaled by KEDA already? Add the trigger beside yours
instead, as [Try it in passive mode](../try-in-passive-mode/#3-register-the-models)
shows. Every trigger field: [ScaledObject trigger](../../reference/scaledobject-trigger.md).
The release's tools image writes these for every model server it finds:
`scaledobjects-plan -n $NAMESPACE --out plan.yaml`, edit, then
`scaledobjects-apply -n $NAMESPACE --plan plan.yaml` (how to run it: [Prepare
model servers](../prepare-workloads/#prerequisites)).

#### Let it scale

The controller is passive until you say otherwise. Compare what it would do
with what runs first ([Try it in passive mode](../try-in-passive-mode/)), then:

```bash
# Let the controller scale. Live: no restart.
kubectl patch configmap scaling-manager-settings -n $NAMESPACE --type merge -p '{"data":{"passive":"false"}}'
kubectl get configmap scaling-manager-controller-status -n $NAMESPACE -o jsonpath='{.data.passiveDefault}{"\n"}'
```

The second command prints `false` once the controller read the change.

## Verification

### 1. Check KEDA received the decision

<!-- guide:verify.objects start -->
```bash
kubectl get scaledobject,hpa -n ${NAMESPACE}
```
<!-- guide:verify.objects end -->

```text
NAME                                        SCALETARGETKIND      SCALETARGETNAME    MIN  MAX  READY  ACTIVE  TRIGGERS
scaledobject.keda.sh/dev-model-decode-llmsm   apps/v1.Deployment   dev-model-decode   1    10   True   True    external-push

NAME                                                  REFERENCE                     TARGETS     MINPODS  MAXPODS  REPLICAS
horizontalpodautoscaler.autoscaling/keda-hpa-dev-...  Deployment/dev-model-decode   1/1 (avg)   1        10       1
```

- **`READY True`**: KEDA reached the scaling manager.
- **`TARGETS` shows a number**: decisions are flowing. `1/1 (avg)` means one
  replica is the right size.
- **`ACTIVE True`**: there is traffic. `Unknown` with no traffic is normal.

**`TARGETS` reads `cpu: <unknown>/80%`** (with `READY False`): KEDA cannot reach
the scaler. Check the `scalerAddress` names this namespace, and that step 4
printed `mutual TLS`. **`READY True` but `TARGETS` shows only `cpu`**: KEDA's
first call for the metric failed and it does not retry; delete and re-apply
the ScaledObject. Other causes: [First-line
troubleshooting](../../reference/operations.md#first-line-troubleshooting).

### 2. Read the decisions

**Wait a few minutes first.** The scaling manager decides on a timer.

<!-- guide:verify.decisions start -->
```bash
# Needs read access to the controller's logs.
kubectl logs -n ${NAMESPACE} deploy/scaling-manager-controller-manager --tail=-1 | grep scaling-decision
```
<!-- guide:verify.decisions end -->

```text
scaling-decision {"modelID":"meta/llama","decisions":[{"name":"llama-decode-llmsm","curr":1,"tgt":3,"action":"scale-up"}]}
```

While passive, the same decisions show as `PassiveRecommendation` Events on
each ScaledObject.

### 3. (Optional) See it in a dashboard

<!-- guide:visualize.dashboard start -->
```bash
make dashboard
```
<!-- guide:visualize.dashboard end -->

**OpenShift only**, from a checkout, and it needs grafana-operator's CRDs. On
Kubernetes, import `deploy/grafana/operational-dashboard.json` into your own
Grafana.

### 4. (Optional) Drive some load

<!-- guide:visualize.smoke start -->
```bash
make benchmark-smoke NAMESPACE=${NAMESPACE}
```
<!-- guide:visualize.smoke end -->

Five minutes of 1024-in/1024-out requests at 15 req/s, from a checkout. It
needs a GPU cluster and is not a benchmark; for that, see [Benchmark the
scaling manager](../benchmarking/).

## Cleanup

Delete or repoint your ScaledObjects first: a trigger that calls a removed
scaler fails on every KEDA poll, and a workload parked at zero can never wake.
If utilization share was active, set it to `shadow` and wait a cycle before
anything else, so no transfer marks are left on pods; then delete
`scaling-manager-activate.yaml` in each namespace you applied it in.

The namespace owner removes the controller and what it wrote:

<!-- guide:cleanup.uninstall start -->
```bash
# Your ScaledObjects stay; delete or repoint them first. Keeps the scaler CA, for a reinstall.
kubectl delete -f scaling-manager-namespaced.yaml -l scaling-manager.llmd.ai/part=controller --ignore-not-found
kubectl delete -f scaling-manager-preflight.yaml -l scaling-manager.llmd.ai/part=controller --ignore-not-found
kubectl delete secret scaling-manager-prometheus-ca -n $NAMESPACE --ignore-not-found
kubectl delete configmap,secret -n $NAMESPACE \
  -l 'app.kubernetes.io/part-of=scaling-manager,scaling-manager.llmd.ai/scaler-tls!=ca'
```
<!-- guide:cleanup.uninstall end -->

Then the cluster admin removes the rest:

<!-- guide:cleanup.kedaauth start -->
```bash
# Cluster admin. The part=admin objects, the preflight's RBAC, the monitors, and KEDA's side of mutual TLS.
kubectl delete -f scaling-manager-namespaced.yaml --ignore-not-found
kubectl delete -f scaling-manager-preflight.yaml --ignore-not-found
kubectl delete -f scaling-manager-model-namespace.yaml --ignore-not-found
kubectl delete -f scaling-manager-keda-auth.yaml --ignore-not-found
kubectl delete secret scaling-manager-external-scaler-$NAMESPACE -n $KEDA_NS --ignore-not-found
```
<!-- guide:cleanup.kedaauth end -->

The model servers, EPP and InferencePools keep running. If you deployed the
model with [Install a small llm-d model](../install-small-model/), remove it
after the uninstall:

<!-- guide:cleanup.model start -->
```bash
make benchmark-teardown BENCHMARK_NAMESPACE=${NAMESPACE}
```
<!-- guide:cleanup.model end -->

### What an uninstall run by you leaves behind

- **The scaler CA**, Secret `scaling-manager-external-scaler-ca`, on purpose:
  a reinstall signs with it, so KEDA's open connection keeps working. Delete it
  with `kubectl delete secret scaling-manager-external-scaler-ca -n $NAMESPACE`
  when you will not reinstall.
- **The `part=admin` objects**, until the cluster admin runs their block
  above: the RBAC, the monitors, the NetworkPolicy and the preflight's RBAC.

## Configuration

The settings ConfigMap holds one key per setting. A change is a patch:

```bash
kubectl patch configmap scaling-manager-settings -n $NAMESPACE --type merge -p '{"data":{"<key>":"<value>"}}'
```

| Key | Default | Applies |
| --- | --- | --- |
| `prometheusURL` | none, **required** | restart |
| `prometheusCACertPath` | none: the system roots | restart |
| `passive` | `"true"` | live |
| `utilizationShare` | `"off"`; `shadow` or `active` ([guide](../utilization-share/)) | live |
| `limiter`, `quotas` | none: scaling is bounded by each ScaledObject's `maxReplicaCount` only | live |
| `scaleToZero` | `"false"` | restart |
| `watchNamespaces` | the controller's own | restart |
| `logVerbosity` | `"2"` | restart |

A restart key needs `kubectl rollout restart
deployment/scaling-manager-controller-manager -n $NAMESPACE`. Every key, and
the old `config.yaml` names it replaces: [Configuration](../../reference/configuration.md).
The thresholds, tiers and per-model entries stay in the optional scaling-policy
ConfigMap ([Scaling policy](../../reference/scaling-policy.md)).

## From a checkout instead

`make deploy-scaling-manager` is a supported install too, and gives the same
objects. It finds Prometheus, its CA and KEDA's namespace itself, installs
KEDA and Prometheus on request, and registers the workloads with a plan:

<!-- guide:checkout.install start -->
```bash
# The same install from a checkout of the release tag.
git checkout <release>
export NAMESPACE=<your-namespace>
make check-prereqs
make setup-prereqs           # cluster admin
make deploy-scaling-manager
```
<!-- guide:checkout.install end -->

`make scaledobjects-plan` and `make scaledobjects-apply` then register the
workloads, and `make undeploy-scaling-manager` removes the install. Every
option: [installer options](../../reference/install-options.md).

## Details

### Is a cluster policy in force?

The controller publishes the answer in its `scaling-manager-controller-status` ConfigMap:

```bash
# Print the policy namespace and the limiter in force
kubectl get cm scaling-manager-controller-status -n ${NAMESPACE} \
  -o jsonpath='{.data.policyNamespace} {.data.limiter}{"\n"}'
```

- `policyNamespace` is the controller's own namespace: your settings and your
  scaling-policy ConfigMap's limiters are in force.
- Any other value (such as `scaling-manager-policy`): the limiters come from
  there and yours are ignored. `limiter` names those in force, such as
  `quota/install-quota`.

### Why the prerequisites matter

- **No model servers:** a controller pointed at the wrong namespace installs
  cleanly, reports healthy and scales nothing.
- **`flowControl`:** it publishes the router queue the scaling manager reads as
  demand. It is the only signal at zero replicas.

### What you own, and what your admin owns

| What | Owner |
| --- | --- |
| The controller Deployment and its Services (`part=controller`), the settings, the scaling-policy ConfigMap, the scaler TLS Secrets | you |
| ServiceAccount, Roles, RoleBindings, the ServiceMonitor, the NetworkPolicy (`part=admin`) | your admin |
| ClusterRoles and ClusterRoleBindings named `...-<namespace>` | your admin |
| KEDA's ClusterTriggerAuthentication and its copy of the client certificate | your admin |

Every permission and its reason is in the [security
model](../../reference/security-model.md).

## Next

- [Prepare model servers to scale](../prepare-workloads/): the engine image,
  weights and compile caches on each GPU node, so a new replica starts fast
- [Try it in passive mode](../try-in-passive-mode/): compare its decisions
  with what runs before it scales anything
- [After the install](../../reference/operations.md)
- [Bound every install by real GPUs](../admin-gpu-bounding/). Without it, only
  each workload's `maxReplicaCount` bounds scaling.
- [Bridge a scale-up with a warm pool](../warm-pool/)
- [Scaling policy](../../reference/scaling-policy.md)
