# Installing on OpenShift

What `make deploy-scaling-manager` does on OpenShift, and how it differs from Kubernetes.
For the full walk-through start with a guide —
[install in a namespace](../../docs/guides/install-in-namespace/) is the common
case — and for every variable see the
[installer options](../../docs/reference/install-options.md).

```bash
oc login --server=<api-url> --token=<token>
export NAMESPACE=<project where llm-d already serves>
make check-prereqs      # read-only; OpenShift is detected
make deploy-scaling-manager
```

`make deploy-scaling-manager-on-openshift` is the same install with `ENVIRONMENT=openshift`
fixed.

## Before you start

- **llm-d is already running in `NAMESPACE`.** The scaling manager does not deploy
  models or llm-d; it scales what is there. The preflight refuses to install
  when it cannot find model-server and EPP metrics (`SKIP_CHECKS=true` overrides).
- **KEDA comes from the platform.** Install the Custom Metrics Autoscaler
  operator (OpenShift's KEDA) first. The installer never installs KEDA on
  OpenShift; it only checks that `scaledobjects.keda.sh` is served.
- **User Workload Monitoring is enabled.** The installer deploys no Prometheus
  on OpenShift; the controller reads the platform's Thanos querier
  (`thanos-querier.openshift-monitoring`, port 9091). A controller namespace the
  installer creates is labelled `openshift.io/user-monitoring=true`.
- **Tools:** `oc`, `kubectl`, `helm`, `git`, `jq`, and go-yq v4.
- **Rights:** a namespace install splits in two. A cluster admin runs
  `make setup-prereqs` once per namespace; the project owner then runs
  `make deploy-scaling-manager`, which installs only the controller when the admin half is
  already there. See [cluster-admin setup](../../docs/guides/admin-cluster-setup/).

To list the GPU products the cluster's nodes advertise:

```bash
kubectl get nodes -o jsonpath='{range .items[?(@.status.allocatable.nvidia\.com/gpu)]}{.metadata.name}{"\t"}{.metadata.labels.nvidia\.com/gpu\.product}{"\n"}{end}'
```

## What the install creates

| Component | Where | Notes |
| --- | --- | --- |
| Controller `scaling-manager-controller-manager` | `NAMESPACE` (namespace scope, the default); `scaling-manager-system` with `SCOPE=cluster` | 1 replica (`LLMSM_REPLICAS`) |
| External scaler Service `scaling-manager-external-scaler` | same namespace, port 9090 (gRPC) | what every ScaledObject trigger names |
| ConfigMaps `scaling-manager-manager-config`, `scaling-manager-scaling-policy-config` | same namespace | |
| ServiceMonitor for the controller | same namespace | scraped by User Workload Monitoring |
| LeaderWorkerSet CRD | `lws-system` | only with `DEPLOY_LWS=true` |

It creates **no ScaledObject** unless asked. Without one, nothing is scaled: the
scaling manager learns a workload exists only when KEDA calls it about one.

```bash
make scaledobjects-plan      # lists your model servers; applies nothing
make scaledobjects-apply
```

See [Default ScaledObjects](../../docs/reference/install-options.md#default-scaledobjects).

## Check it

```bash
make verify-deployment
kubectl get scaledobject,hpa -n "$NAMESPACE"
kubectl logs -n "$NAMESPACE" deploy/scaling-manager-controller-manager -f
```

KEDA creates and owns the HPA behind each ScaledObject (`keda-hpa-<name>`). The
scaling manager's decision reaches KEDA over gRPC; read it from the OpenShift
console's metrics view as `llmsm_desired_replicas`. For a workload that does not
scale, see [troubleshooting](../../docs/reference/troubleshooting.md).

## Remove it

```bash
make undeploy-scaling-manager
```

Pass the same `NAMESPACE` and `SCOPE` you installed with. The Custom Metrics
Autoscaler and the platform monitoring are not touched, and on OpenShift the
installer never deletes namespaces.

To run the e2e suite against an OpenShift cluster, see
[the e2e README](../../test/e2e/).
