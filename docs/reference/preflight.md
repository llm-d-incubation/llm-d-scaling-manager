# Preflight check

The release's `scaling-manager-preflight.yaml` runs a Job that checks the
prerequisites of an install and prints, for each check, PASS, WARN, FAIL or
UNKNOWN, and the command that fixes it. It reads; it changes nothing but its
own result ConfigMap.

> The release files are a preview until the controller reads the settings
> ConfigMap. Until then, install with `make deploy-scaling-manager`; the
> preflight runs against either install.

## Run it

Create the settings first, with `prometheusURL` set: the preflight reads the
same settings the controller will.

```bash
export NS=team-a
sed -i "s/__NAMESPACE__/$NS/g" scaling-manager-settings.yaml scaling-manager-preflight.yaml
kubectl create -f scaling-manager-settings.yaml        # once; never overwritten

kubectl delete job scaling-manager-preflight -n $NS --ignore-not-found
kubectl apply -f scaling-manager-preflight.yaml
kubectl wait -n $NS --for=condition=complete --timeout=180s job/scaling-manager-preflight \
  || kubectl wait -n $NS --for=condition=failed --timeout=5s job/scaling-manager-preflight
kubectl logs -n $NS job/scaling-manager-preflight
```

The Job's template cannot change, so delete the Job before each new run.

The ServiceAccount, Role and ClusterRole in the file are labelled
`scaling-manager.llmd.ai/part=admin`, the Job `part=controller`. A namespace
owner without a cluster admin's ClusterRole still gets a run: every cluster
read is UNKNOWN, and the namespace checks still run.

Run it in `scaling-manager-system` for a cluster-wide install: the preflight
then checks every namespace that runs model servers (or those
`watchNamespaces` names). Elsewhere it checks a namespaced install: the
controller's namespace, or `watchNamespaces`. `--scope=namespace|cluster` in
the Job's args overrides this.

## Read it

```text
PASS    keda.crd              scaledobjects.keda.sh v1alpha1
FAIL    keda.networkpolicy    the release file's NetworkPolicy scaling-manager-external-scaler (not applied yet) admits port 9090 from keda, but KEDA runs in keda-system: ...
        fix: kubectl patch networkpolicy scaling-manager-external-scaler -n team-a --type json -p '[...]'
UNKNOWN rbac.controller       ServiceAccount scaling-manager-controller-manager does not exist in team-a yet: ...
11 PASS, 2 WARN, 1 FAIL, 1 UNKNOWN
UNKNOWN is not a pass: those checks were not made. Each line says what was refused or missing.
not applicable: kueue (kueue is "false"); activation (utilizationShare is "off")
```

| status | meaning | the Job |
| --- | --- | --- |
| PASS | checked, nothing wrong | |
| WARN | works, degraded or at a risk the line names | |
| FAIL | will not work until the fix is applied | exits 1: `condition=failed` |
| UNKNOWN | not checked: a read was refused or timed out, or the thing it checks is not installed yet | exits 0, but it is not a pass |

A refused read is always UNKNOWN, never PASS and never "absent". The same
lines are written as JSON to ConfigMap `scaling-manager-preflight-result`, key
`result.json`, with a one-line `summary`:

```bash
kubectl get configmap scaling-manager-preflight-result -n $NS -o jsonpath='{.data.summary}'
```

Some checks need the controller to exist (its ServiceAccount, its scaler
certificates). Before the install they are UNKNOWN; run the preflight again
after it.

## The checks

| check | FAIL when | WARN when | UNKNOWN when |
| --- | --- | --- | --- |
| `settings` | a key does not parse (a bad quota, an unknown mode, `utilizationShare` without a limiter, `quota` with no budget), or there are no settings at all | the settings are absent and the pre-v0.11.0 `scaling-manager-manager-config` is read instead | the settings cannot be read |
| `keda.crd` | `scaledobjects.keda.sh` is absent | | the CRD cannot be read |
| `keda.version` | KEDA is older than 2.18 | an untested minor, or before 2.21.0 (CVE-2026-77524) | KEDA's Deployment cannot be listed, or its image names no version |
| `keda.networkpolicy` | the scaler NetworkPolicy (applied, or the one the file will apply: `keda`, `openshift-keda` on OpenShift) admits another namespace than the one KEDA runs in; the fix is the `kubectl patch` | the external-metrics APIService is not KEDA's | where KEDA runs cannot be read |
| `keda.mtls` | the marker says mutual TLS is in place, and the ClusterTriggerAuthentication or KEDA's copy of the client certificate is missing, or the certificates do not chain (the CA KEDA trusts does not verify the scaler, or the client CA does not verify KEDA) | no marker: the scaler serves plaintext. The fix lists the four commands that set it up | a Secret cannot be read, or the controller has not created its certificates yet |
| `prometheus.reachable` | `prometheusURL` is unset, plain `http://`, refused, times out, or its certificate fails against Secret `scaling-manager-prometheus-ca` | | that Secret cannot be read |
| `metrics.model` | a managed namespace runs model servers and Prometheus holds no `vllm:` series for it | some of `vllm:kv_cache_usage_perc`, `vllm:num_requests_waiting`, `vllm:num_requests_running` are missing, or no model server runs yet | Prometheus is not reachable, or refuses the preflight's account |
| `podmonitor.port` | no PodMonitor selects the model servers, the one that does names a port none of them declares, or its targets are down | no Prometheus Operator, or no model server yet | PodMonitors or pods cannot be listed |
| `prometheus.selects` | no Prometheus resource selects the PodMonitor, by its PodMonitor or namespace selector; the line says which label it needs | no Prometheus Operator | Prometheus resources cannot be listed |
| `openshift.user-monitoring` | a checked namespace is labelled `openshift.io/user-monitoring=false` | the controller's namespace lacks `openshift.io/user-monitoring=true`, which the `make` install set | namespaces cannot be read |
| `rbac.controller` | the controller's ServiceAccount may not do what it needs, by SubjectAccessReview per verb and resource; or may not list nodes with the `gpu-inventory` limiter | it may not list nodes (no `gpu-inventory`) | the ServiceAccount does not exist yet, or a review is refused |
| `rbac.namespaces` | a namespace `watchNamespaces` names is missing from the namespace readers, which name only the controller's namespace; the fix is the `kubectl patch` per namespace | the same, before the install file is applied | the ClusterRole cannot be read |
| `kueue` (with `kueue: "true"`) | the Kueue CRDs are absent, or the controller may not list ClusterQueues (`scaling-manager-kueue.yaml`) | | the CRDs cannot be read |
| `overlap` | another controller already manages a namespace this one would: a cluster-wide one, or one whose `--watch-namespace` names it | | Deployments cannot be listed cluster-wide |
| `accelerators` (with `quotas`) | | a quota key matches no node's accelerator label | nodes cannot be listed |
| `activation` (with `utilizationShare: "active"`) | the controller may not patch pods in a sharing namespace; the fix is the `scaling-manager-activate.yaml` line for it | | a review is refused |

`openshift.user-monitoring`, `kueue`, `accelerators`, `activation` and, for a
cluster-wide install, `rbac.namespaces` are listed under "not applicable" when
they do not apply.

## What it may do

Its own ServiceAccount, `scaling-manager-preflight`, never the controller's:

- a Role in the controller's namespace: `get` on the settings, the legacy
  configuration, the KEDA marker and its result ConfigMap, on the Prometheus CA
  and scaler certificate Secrets, on the controller's ServiceAccount and on the
  scaler NetworkPolicy, each by name; `create` ConfigMaps, `update` its result;
- a ClusterRole: `get` on KEDA's and Kueue's CRDs, the external-metrics
  APIService, this install's ClusterTriggerAuthentication and KEDA's copy of
  its client certificate, and the two namespace-reader ClusterRoles, each by
  name; `get` namespaces; `list` Deployments, pods, nodes, PodMonitors and
  Prometheus resources; `create` SubjectAccessReviews, which grants nothing.

Delete the preflight objects after the install if you want them gone; they
hold no secret:

```bash
kubectl delete -f scaling-manager-preflight.yaml --ignore-not-found
kubectl delete configmap scaling-manager-preflight-result -n $NS --ignore-not-found
```

## Not yet

`manager preflight verify`, the after-install mode (the controller Ready, the
scaler protocol, each ScaledObject's HPA wired), is not built. Until it is,
check the rollout with `kubectl rollout status
deployment/scaling-manager-controller-manager -n $NS` and each HPA as
[after the install](operations.md) describes.

Design: [install-from-release.md](../proposals/install-from-release.md),
"The preflight check".
