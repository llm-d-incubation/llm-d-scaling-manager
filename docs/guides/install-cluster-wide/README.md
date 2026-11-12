# Install the scaling manager for the whole cluster

## Overview

This guide installs one scaling manager (llm-d-scaling-manager) that sizes the
llm-d model servers in **every** namespace, from the files attached to a
release, with `kubectl`. It decides how many replicas each variant needs and
hands that number to KEDA, which does the scaling. It installs **passive**:
nothing scales until you switch it on.

Use it when one team runs the cluster. Where namespaces belong to different
teams, install [one controller per namespace](../install-in-namespace/) instead:
failure domains, policy and upgrades stay separate.

**This scope is work in progress.** The steps are the namespaced guide's, with
the cluster-wide files; read that guide for what each step does.

Tested: the namespaced and the cluster-wide files on Kubernetes (kind, `make
test-e2e-release-manifests`, in CI and before each release; this guide's steps
with a model in a namespace other than the controller's). The OpenShift files
are not yet tested end to end; each says so in its header.

Prefer a checkout and `make`? See [From a checkout instead](#from-a-checkout-instead).

## Prerequisites

- [ ] Cluster-admin rights.
- [ ] llm-d model servers somewhere on the cluster.
- [ ] **KEDA**, 2.18 or later; 2.21 is recommended ([KEDA reference](../../reference/keda.md)).
  On OpenShift, the Custom Metrics Autoscaler operator.
- [ ] A Prometheus with the Prometheus Operator.
- [ ] **For scale-from-zero only: the `flowControl` feature gate on every
  EPP** (`featureGates: [flowControl]` in the `EndpointPickerConfig`).

<!-- guide:prerequisites.download start -->
```bash
# The release files. Pick the release you install.
V=<release, for example v0.11.0>
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V
for f in cluster settings preflight-cluster scaler-tls model-namespace keda-auth; do
  curl -fsSLO $URL/scaling-manager-$f.yaml
done
```
<!-- guide:prerequisites.download end -->

Each file is pinned to the release's image by digest, and signed. Check them
now, before the `sed` below changes them: [Verify the release
files](../../reference/install-methods.md#verify-the-release-files).

<!-- guide:prerequisites.namespace start -->
```bash
# The controller runs in scaling-manager-system. The cluster-wide files already name it.
kubectl create namespace scaling-manager-system
for f in scaling-manager-settings.yaml scaling-manager-scaler-tls.yaml scaling-manager-keda-auth.yaml; do
  sed -i "s/__NAMESPACE__/scaling-manager-system/g" "$f"
done
```
<!-- guide:prerequisites.namespace end -->

On OpenShift (not yet tested end to end), use
`scaling-manager-cluster-openshift.yaml` in place of
`scaling-manager-cluster.yaml` in every command, set `prometheusURL` to Thanos Querier (the
settings file names it), and label the namespace
`openshift.io/user-monitoring=true`.

## Installation Instructions

### 1. KEDA's namespace, settings and the preflight check

The scaler NetworkPolicy in `scaling-manager-cluster.yaml` admits KEDA from
namespace `keda` (`openshift-keda` in the OpenShift file). If KEDA runs
elsewhere, name its namespace before you apply the file, and keep the edit for
every upgrade:

<!-- guide:deploy.kedans start -->
```bash
# Before applying, name the namespace KEDA runs in in the scaler NetworkPolicy (the file says keda). Keep this edit for every upgrade.
export KEDA_NS=<the namespace KEDA runs in>
sed -i "s#kubernetes.io/metadata.name: keda\$#kubernetes.io/metadata.name: $KEDA_NS#" scaling-manager-cluster.yaml
```
<!-- guide:deploy.kedans end -->

Set `prometheusURL` in `scaling-manager-settings.yaml`, then:

<!-- guide:deploy.settings start -->
```bash
# Set prometheusURL in scaling-manager-settings.yaml first. Created once, never applied again.
kubectl create -f scaling-manager-settings.yaml
```
<!-- guide:deploy.settings end -->

<!-- guide:deploy.preflight start -->
```bash
# Read-only. Prints PASS, WARN, FAIL or UNKNOWN per check, and the fix.
kubectl delete job scaling-manager-preflight -n scaling-manager-system --ignore-not-found
kubectl apply -f scaling-manager-preflight-cluster.yaml
until kubectl get job scaling-manager-preflight -n scaling-manager-system \
    -o jsonpath='{.status.conditions[?(@.status=="True")].type}' | grep -qE 'Complete|Failed'; do sleep 3; done
kubectl logs -n scaling-manager-system job/scaling-manager-preflight
```
<!-- guide:deploy.preflight end -->

Fix every `FAIL` and run it again. A `keda.networkpolicy` WARN is expected
until step 2 when KEDA is outside `keda`, even after the `sed` above: the
preflight cannot read the file, so it judges the policy as shipped. Run it
again after step 2, where it checks the applied policy and must PASS. Every
check: [Preflight check](../../reference/preflight.md). If a private CA signed Prometheus's
certificate, give the controller that CA:

<!-- guide:deploy.promca start -->
```bash
# Only when a private CA signed Prometheus's certificate.
kubectl create secret generic scaling-manager-prometheus-ca -n scaling-manager-system --from-file=ca.crt=<ca file>
kubectl patch configmap scaling-manager-settings -n scaling-manager-system --type merge \
  -p '{"data":{"prometheusCACertPath":"/etc/scaling-manager-prometheus-ca/ca.crt"}}'
```
<!-- guide:deploy.promca end -->

When you are done with the preflight (after step 3), remove its cluster-wide
reads; applying the preflight file again restores them:

<!-- guide:deploy.preflightdone start -->
```bash
# After the last run. The preflight's cluster reads are needed only while it runs.
kubectl delete clusterrolebinding scaling-manager-preflight --ignore-not-found
```
<!-- guide:deploy.preflightdone end -->

### 2. Install the controller

<!-- guide:deploy.controller start -->
```bash
kubectl create -f scaling-manager-scaler-tls.yaml
kubectl apply -f scaling-manager-cluster.yaml
kubectl rollout status deployment/scaling-manager-controller-manager -n scaling-manager-system
```
<!-- guide:deploy.controller end -->

Then, in each namespace with model servers, the model-server monitor:

<!-- guide:deploy.model start -->
```bash
# In each namespace with model servers. The model-server PodMonitor (and a Role the cluster-wide controller does not need).
MODEL_NS=<model namespace>
sed -e "s/__NAMESPACE__/scaling-manager-system/g" -e "s/__MODEL_NAMESPACE__/$MODEL_NS/g" \
  scaling-manager-model-namespace.yaml | kubectl apply -f -
```
<!-- guide:deploy.model end -->

### 3. Give KEDA its client certificate

<!-- guide:deploy.kedaauth start -->
```bash
# In this directory, with KEDA_NS set as above. Prints the steps that give KEDA its client certificate.
kubectl get configmap scaling-manager-controller-status -n scaling-manager-system \
  -o jsonpath='{.data.scalerTLSAction}{"\n"}'
```
<!-- guide:deploy.kedaauth end -->

<!-- guide:deploy.mtls start -->
```bash
# Prints "mutual TLS" once KEDA's side is in place and the controller restarted.
kubectl get configmap scaling-manager-controller-status -n scaling-manager-system \
  -o jsonpath='{.data.scalerTLS}{"\n"}'
```
<!-- guide:deploy.mtls end -->

In this directory, run the text after `then restart the controller:` in
step 1 of what it prints; step 2 only on a reinstall. Then check:

### 4. Register the workloads, then let it scale

One ScaledObject per model-server Deployment, as in [the namespaced
guide](../install-in-namespace/#register-the-workloads), with
`scalerAddress: scaling-manager-external-scaler.scaling-manager-system.svc:9090`
and `authenticationRef` naming `scaling-manager-external-scaler-scaling-manager-system`.
Then [let it scale](../install-in-namespace/#let-it-scale), with `-n
scaling-manager-system`.

## Verification

<!-- guide:verify.objects start -->
```bash
kubectl get scaledobject,hpa -A
```
<!-- guide:verify.objects end -->

Look for **`READY True`** on the ScaledObject and a number in the HPA's
`TARGETS`. `1/1 (avg)` means one replica is the right size. Do not use
`CurrentMetrics` as the check ([why](#why-not-currentmetrics)).

`TARGETS` showing `<unknown>` means KEDA cannot reach the scaler: check the
`scalerAddress`, and that step 3 printed `mutual TLS`. `READY True` but
`TARGETS` showing only `cpu` means KEDA's first call for the metric failed and
it does not retry: delete and re-apply the ScaledObject.

## Cleanup

Delete or repoint the ScaledObjects first: one parked at zero cannot wake once
the scaler is gone. Then:

<!-- guide:cleanup.uninstall start -->
```bash
# ScaledObjects stay; delete or repoint them first. Keeps the scaler CA, for a reinstall.
kubectl delete -f scaling-manager-cluster.yaml --ignore-not-found
kubectl delete -f scaling-manager-preflight-cluster.yaml --ignore-not-found
kubectl delete -f scaling-manager-keda-auth.yaml --ignore-not-found
kubectl delete secret scaling-manager-external-scaler-scaling-manager-system -n $KEDA_NS --ignore-not-found
kubectl delete configmap,secret -n scaling-manager-system \
  -l 'app.kubernetes.io/part-of=scaling-manager,scaling-manager.llmd.ai/scaler-tls!=ca'
```
<!-- guide:cleanup.uninstall end -->

Delete the model-server monitor in each model namespace with the same `sed`
and `kubectl delete -f -`. The namespace `scaling-manager-system` and the scaler
CA in it stay; delete them when you will not reinstall. If a namespace's model
came from [Install a small llm-d model](../install-small-model/), remove it per
namespace:

<!-- guide:cleanup.model start -->
```bash
make benchmark-teardown BENCHMARK_NAMESPACE=<the model namespace>
```
<!-- guide:cleanup.model end -->

Full detail: [Upgrade and uninstall](../../reference/upgrade-and-uninstall.md).

## Configuration

The settings ConfigMap in `scaling-manager-system`, as for [a namespaced
install](../install-in-namespace/#configuration). `watchNamespaces` empty means
every namespace.

A cluster-scoped install can read every ConfigMap and patch every pod in the
cluster; every permission is in the [security
model](../../reference/security-model.md#cluster-scoped-both-platforms).

## From a checkout instead

`make deploy-scaling-manager SCOPE=cluster` installs the same objects. It finds
Prometheus and KEDA itself, and installs KEDA with `KEDA_HELM_INSTALL=true`:

<!-- guide:checkout.install start -->
```bash
# The same install from a checkout of the release tag.
git checkout <release>
make check-prereqs SCOPE=cluster
make deploy-scaling-manager SCOPE=cluster
```
<!-- guide:checkout.install end -->

<!-- guide:checkout.register start -->
```bash
# From a checkout. Plan and apply ScaledObjects in every namespace with model servers. Keep SCOPE=cluster.
make scaledobjects-plan SCOPE=cluster LLMSM_DEFAULT_SO_PLAN=scaling-manager-plan.yaml
# edit scaling-manager-plan.yaml: apply: yes|no|adopt, the modelID, the replica bounds
make scaledobjects-apply SCOPE=cluster LLMSM_DEFAULT_SO_PLAN=scaling-manager-plan.yaml
```
<!-- guide:checkout.register end -->

Keep `SCOPE=cluster` on every command: it defaults to `namespace`, and the plan
then scans only the controller's own namespace. `make undeploy-scaling-manager
SCOPE=cluster` removes the install and the ScaledObjects it created. Every
option: [installer options](../../reference/install-options.md).

## Details

- **Why `flowControl`.** At zero replicas there are no model-server metrics, so
  the router queue the gate publishes is the only sign anyone wants the model.
- **Why the NetworkPolicy matters.** Without it any Pod can call the scaler on
  `:9090`. With mutual TLS, only KEDA can, whatever the policy says.

### Why not `CurrentMetrics`

On a ScaledObject whose trigger the scaling manager never answers, it still
shows an empty entry, `[{"type":""}]`, while `READY` is `False`, `TARGETS`
reads `<unknown>/1 (avg)` and nothing scales.

## Next

- [Bound every install by real GPUs](../admin-gpu-bounding/)
- [Share GPUs between models in different
  namespaces](../utilization-share-cluster-wide/)
- [After the install](../../reference/operations.md)
- [Install methods](../../reference/install-methods.md): GitOps, direct
  Kustomize, and verifying the release files
