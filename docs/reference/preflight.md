# Preflight check

The release's preflight file runs a Job that checks the prerequisites of an
install and prints, for each check, PASS, WARN, FAIL or UNKNOWN, and the
command that fixes it. It reads; it changes nothing but its own result
ConfigMap. It reads no Secret.

| install | file |
| --- | --- |
| namespaced (one controller per namespace) | `scaling-manager-preflight.yaml` |
| cluster-wide, in `scaling-manager-system` | `scaling-manager-preflight-cluster.yaml` (applies as is) |

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
# Wait until the Job completes or fails, whichever comes first.
until kubectl get job scaling-manager-preflight -n $NS \
    -o jsonpath='{.status.conditions[?(@.status=="True")].type}' | grep -qE 'Complete|Failed'; do
  sleep 3
done
kubectl logs -n $NS job/scaling-manager-preflight
```

The Job's template cannot change, so delete the Job before each new run. It
deletes itself an hour after it ends (`ttlSecondsAfterFinished`), and is
stopped after ten minutes whatever it is doing (`activeDeadlineSeconds`).

If the Prometheus CA Secret `scaling-manager-prometheus-ca` is created or
changed, run the Job again: it mounts the Secret when it starts.

### Without a cluster admin

The ServiceAccount, Role, RoleBinding, ClusterRole and ClusterRoleBinding are
labelled `scaling-manager.llmd.ai/part=admin`, the Job `part=controller`. A
namespace owner may create all of them except the ClusterRole and its
binding. `kubectl apply -f scaling-manager-preflight.yaml` then creates the
rest and reports the two it was refused; the Job runs, every cluster read is
UNKNOWN, and the namespace checks still run.

### Other watched namespaces

The Role reads pods and PodMonitors in the controller's namespace only. For a
namespace `watchNamespaces` adds, its owner grants the same two reads there,
or those checks are UNKNOWN for it:

```bash
kubectl create role scaling-manager-preflight -n <model namespace> \
  --verb=list --resource=pods,podmonitors.monitoring.coreos.com
kubectl create rolebinding scaling-manager-preflight -n <model namespace> \
  --role=scaling-manager-preflight --serviceaccount=$NS:scaling-manager-preflight
```

### After the run

Delete the preflight's cluster grant and its account when the run is done.
They let the account read cluster state, which nothing needs between runs:

```bash
kubectl delete clusterrolebinding scaling-manager-preflight-$NS --ignore-not-found
kubectl delete -f scaling-manager-preflight.yaml --ignore-not-found
kubectl delete configmap scaling-manager-preflight-result -n $NS --ignore-not-found
```

For the cluster-wide file the binding is `scaling-manager-preflight`.

## Read it

```text
PASS    keda.crd              scaledobjects.keda.sh v1alpha1
FAIL    keda.networkpolicy    the release file's NetworkPolicy scaling-manager-external-scaler (not applied yet) admits port 9090 from keda, but KEDA runs in keda-system: ...
        fix: sed -i "s#kubernetes.io/metadata.name: keda\$#kubernetes.io/metadata.name: keda-system#" scaling-manager-namespaced*.yaml   # before applying it
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

Some checks need the controller to exist (its ServiceAccount, its status
ConfigMap). Before the install they are UNKNOWN; run the preflight again
after it.

## The checks

| check | FAIL when | WARN when | UNKNOWN when |
| --- | --- | --- | --- |
| `settings` | a key does not parse (a bad or repeated quota, an unknown mode, a negative `logVerbosity`, `utilizationShare` without a limiter, `quota` with no budget), or there are no settings at all | the settings are absent and the pre-v0.11.0 `scaling-manager-manager-config` is read instead | the settings cannot be read |
| `keda.crd` | `scaledobjects.keda.sh` is absent | | the CRD cannot be read |
| `keda.version` | KEDA is older than 2.18 | an untested minor; before 2.21.0 (CVE-2026-77524); before 2.20.0 (a ScaledObject made just after a controller restart can stay unwired) | KEDA's operator cannot be read, or its image names no version |
| `keda.networkpolicy` | the scaler NetworkPolicy, applied or in the file (`keda`; `openshift-keda` on OpenShift), does not admit the namespace KEDA runs in. The fix is a `kubectl patch`, or a `sed` on the file before it is applied | the external-metrics APIService is not KEDA's | where KEDA runs cannot be read |
| `keda.mtls` | the marker says mutual TLS is in place and the ClusterTriggerAuthentication is missing, or the controller reports KEDA's handshakes refused (`scalerUnreachable`) | no marker: the scaler serves plaintext. The fix lists the setup commands | before the install: whether KEDA's copy of the certificates matches is not readable without its Secret. After it, `scalerUnreachable` and the alert `LLMSMExternalScalerUnreachable` say so |
| `scaler.tls` (with mutual TLS) | the chain the scaler serves, read by a TLS handshake, does not verify against the scaler CA the controller publishes (`scalerCACert` in `scaling-manager-controller-status`): another CA, another host, or expired | | the controller has not run, publishes no CA, or the handshake gets no answer (the NetworkPolicy admits port 9090 from KEDA's namespace only) |
| `prometheus.reachable` | `prometheusURL` is unset, plain `http://`, refused, times out, or its certificate fails against the mounted Secret `scaling-manager-prometheus-ca` | | the settings cannot be read |
| `metrics.model` | a namespace runs model servers and Prometheus holds no `vllm:` series for it | some of `vllm:kv_cache_usage_perc`, `vllm:num_requests_waiting`, `vllm:num_requests_running` are missing, or no model server runs yet | Prometheus is not reachable or refuses the preflight's account, or no series and the pods cannot be listed |
| `podmonitor.port` | no PodMonitor selects the model servers, the one that does names a port none of them declares, or its targets are down | no Prometheus Operator, or no model server yet | PodMonitors or pods cannot be listed |
| `prometheus.selects` | no Prometheus resource selects the PodMonitor, by its PodMonitor or namespace selector; the line says which label it needs | no Prometheus Operator | Prometheus resources cannot be listed |
| `openshift.user-monitoring` | a checked namespace is labelled `openshift.io/user-monitoring=false` | the controller's namespace lacks `openshift.io/user-monitoring=true`, which the `make` install set | namespaces or pods cannot be read |
| `rbac.controller` | the controller's ServiceAccount may not do what it needs, by SubjectAccessReview per verb and resource; or may not list nodes with the `gpu-inventory` limiter | it may not list nodes (no `gpu-inventory`) | the ServiceAccount does not exist yet, or a review is refused or cannot be evaluated |
| `rbac.namespaces` | a namespace `watchNamespaces` names is missing from the namespace readers, which name only the controller's namespace; the fix is the `kubectl patch` per namespace | the same, before the install file is applied | the ClusterRole cannot be read |
| `kueue` (with `kueue: "true"`) | the Kueue CRDs are absent, or the controller may not list ClusterQueues (`scaling-manager-kueue.yaml`) | | the CRDs or the controller's account cannot be read |
| `overlap` | another controller already manages a namespace this one would: a cluster-wide one, or one whose `--watch-namespace` names it | | a namespaced preflight may not list Deployments: it reads the cluster-wide controller in `scaling-manager-system` by name, and says other namespaces were not looked at |
| `accelerators` (with `quotas`) | | a quota key matches no node's accelerator label | nodes cannot be listed |
| `activation` (with `utilizationShare: "active"`) | the controller may not patch pods in a sharing namespace; the fix is the `scaling-manager-activate.yaml` line for it | | a review is refused |

`scaler.tls`, `openshift.user-monitoring`, `kueue`, `accelerators`,
`activation` and, for a cluster-wide install, `rbac.namespaces` are listed
under "not applicable" when they do not apply. When the settings cannot be
read, the checks they gate are UNKNOWN instead.

Each check has 20 seconds (`--check-timeout`); one that runs out is UNKNOWN,
and the run goes on.

## What it may do

Its own ServiceAccount, `scaling-manager-preflight`, never the controller's.
It reads no Secret: the Prometheus CA is mounted into the Job by the kubelet,
and the scaler's chain is read by a handshake. It sends its token to
Prometheus only over `https` with the certificate verified: not over `http`,
and not with `prometheusInsecureSkipVerify`.

- A Role in the controller's namespace: `get` on the settings, the legacy
  configuration, the KEDA marker, the controller status and its result
  ConfigMap, on the controller's ServiceAccount and on the scaler
  NetworkPolicy, each by name; `list` pods and PodMonitors there; `create`
  ConfigMaps and `update` its result. `create` cannot be narrowed by name;
  the preflight creates only `scaling-manager-preflight-result`.
- A ClusterRole, namespaced file: `get` on KEDA's and Kueue's CRDs, the
  external-metrics APIService, this install's ClusterTriggerAuthentication,
  the `keda-operator` and `scaling-manager-controller-manager` Deployments,
  and the two namespace-reader ClusterRoles, each by name; `get` namespaces;
  `list` nodes and Prometheus resources; `create` SubjectAccessReviews, which
  grants nothing.
- A ClusterRole, cluster-wide file: the same, but `list` Deployments, pods and
  PodMonitors in every namespace instead of by name.

## Not yet

`manager preflight verify`, the after-install mode (the controller Ready, the
scaler protocol, each ScaledObject's HPA wired), is not built. Until it is,
check the rollout with `kubectl rollout status
deployment/scaling-manager-controller-manager -n $NS` and each HPA as
[after the install](operations.md) describes.

Design: [install-from-release.md](../proposals/install-from-release.md),
"The preflight check".
