# After the install

Verifying the scaling manager works, and the first things to check when it does not.

> Part of the [deployment guide](../../deploy/).

This page is the operational entry point. Longer subjects have their own pages:

- **[Watching what the scaling manager decides](monitoring.md)** -- the dashboard,
  the metrics that answer specific questions, and reading the logs.
- **[Prepare model servers to scale](../guides/prepare-workloads/)** -- the
  recommended configuration, from the release's tools image; every alternative
  and its costs are in [Preparing a workload: every option](workload-preparation.md).
- **[Troubleshooting](troubleshooting.md)** -- from a symptom to its runbook.
- **[Upgrade and uninstall](upgrade-and-uninstall.md)** -- pinning a version,
  what a restart costs, and what an uninstall leaves behind.

## Verifying the install

Every command on this page uses `$NS` for the namespace the **controller** runs
in. Start with:

```bash
make verify-deployment
```

It reports the controller's namespace ("the scaling manager controller is running and ready
in `<ns>`"). With several installs, or none, it lists every candidate and stops;
pick yours from the list.

"Ready" means the rollout finished and at least one controller pod is Ready. A
controller Deployment scaled to 0 fails the check with "scaled to 0 replicas".
Re-running the install sets it back to `LLMSM_REPLICAS` (default `1`). An
install with `LLMSM_REPLICAS=0` asked for that, so there it is a warning.

Set `NS` to the controller's namespace. To find it by the Deployment label:

```bash
kubectl get deploy -A -l app.kubernetes.io/name=scaling-manager \
  -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name
NS=$(kubectl get deploy -A -l app.kubernetes.io/name=scaling-manager \
  -o jsonpath='{.items[0].metadata.namespace}')
```

- With several installs, the second command takes the first one listed. Set
  `NS` by hand from the first command's list.
- Without cluster-wide read, set it directly: `NS=<your-namespace>` for a
  namespace-scoped install, or the `LLMSM_NS` it was installed with.

`MODEL_NS` is the namespace your workloads run in; for a namespace-scoped
install it equals `NS`:

```bash
MODEL_NS="$NS"   # or the namespace holding your ScaledObjects
```

Confirm registration matches what is actually serving:

```bash
make verify-scaledobjects
```

For more detail, ask the controller and KEDA directly:

```bash
kubectl get pods -n "$NS"                     # the controller is Running
kubectl get scaledobject -n "$MODEL_NS"       # your managed workloads
kubectl get hpa -n "$MODEL_NS"                # KEDA created one per ScaledObject
```

The chain works when the ScaledObject reads `READY True` and its HPA's
`TARGETS` shows a number (`1/1 (avg)`). `<unknown>` in `TARGETS` means KEDA got
no answer: check the trigger's `scalerAddress` and that `modelID` is set. Do not
use the HPA's `CurrentMetrics` as the check. It holds an empty entry even when
nothing is being scaled.

If metrics are the problem, follow them forward:

```bash
# 1. the model server exposes them
kubectl port-forward -n <llm-namespace> <vllm-pod> 8000:8000
curl -s http://localhost:8000/metrics | grep vllm:

# 2. Prometheus scrapes them  (query vllm:num_requests_running)
kubectl port-forward -n <monitoring-namespace> svc/kube-prometheus-stack-prometheus 9090:9090

# 3. the scaling manager reads them and decides
kubectl logs -n "$NS" -l app.kubernetes.io/name=scaling-manager --tail=-1 | grep scaling-decision
```

Two gauges also show step 3: `llmsm_metrics_pods_discovered` (pods found per
model, should be > 0) and `llmsm_metrics_freshness_status{status="fresh"}` (should
equal the pod count). See [Metrics health](metrics.md).

## Testing autoscaling

One command, against what you already installed:

```bash
make benchmark-smoke NAMESPACE=<namespace>
```

It sends 1024-in/1024-out requests at 15 req/s for five minutes, then
snapshots the dashboard over that window (when there is a dashboard).

- It first checks KEDA, a controller managing *this* namespace, model servers,
  an EPP and a ScaledObject, and reports every gap at once.
- It stands nothing up, so it is safe on a live namespace. It needs no
  benchmark CLI.

For runs whose numbers you intend to compare, use
[Benchmark the scaling manager](../guides/benchmarking/). Full procedures, including the
simulator and the e2e suites, are in [Testing](../developer-guide/testing.md).

## First-line troubleshooting

| symptom | most likely cause | check |
| --- | --- | --- |
| The scaling manager pod is not `Running` | image pull, resources, or Prometheus unreachable | `make verify-deployment`, then `kubectl describe pod -n "$NS" -l app.kubernetes.io/name=scaling-manager` |
| a `MetricsUnavailable` Warning Event on the ScaledObject | the model's metrics stopped arriving: usually the ServiceMonitor or PodMonitor does not select the model pods, so the series never reach Prometheus | `kubectl get events -n <ns> --field-selector involvedObject.kind=ScaledObject,reason=MetricsUnavailable`, then `make verify-deployment`, `kubectl get servicemonitor,podmonitor -n <ns>` and Prometheus `/targets`. See [Metrics health](metrics.md#the-metricsunavailable-event) |
| HPA exists but its `TARGETS` reads `<unknown>` | KEDA never got an answer — usually the trigger's `scalerAddress` or a missing `modelID` | `kubectl get scaledobject,hpa -n <ns>`, then `kubectl describe hpa -n <ns> keda-hpa-<so-name>` |
| The ScaledObject reads Ready, but its HPA carries only `cpu`; KEDA logs `got empty metric spec` | KEDA asks the scaler for the metric spec only when the ScaledObject changes. That call failed (the scaler was restarting) and KEDA does not retry | `make scaledobjects-rewire` re-asks for every unwired ScaledObject of this install and reports what is still unwired. `DRY_RUN=true` only lists them. If it names a KEDA operator restart, do that instead. On KEDA 2.19 and older, `KEDA_HPA_REPAIR=true` makes the controller do this itself ([how](keda.md#the-controller-repair-keda_hpa_repair)). See [Troubleshooting](troubleshooting.md#an-hpa-stays-on-the-cpu-default) |
| nothing scales, no errors | a limiter is declared and the workload's accelerator does not resolve, so it gets no GPU budget | `kubectl logs -n "$NS" -l app.kubernetes.io/name=scaling-manager --tail=-1 \| grep -i accelerator` |
| a model never wakes from zero | the EPP flow-control queue is not reaching the scaling manager | see [Troubleshooting](troubleshooting.md) |
| `READY False` on the ScaledObject, and the HPA's `TARGETS` reads `cpu: <unknown>/80%` | KEDA could not fetch the metric spec from the scaling manager, so it fell back to a CPU metric. The trigger names a scaler it cannot reach — most often a `scalerAddress` naming a **different install's namespace** than the controller actually runs in. `make scaledobjects-repoint` fixes exactly this: it rewrites `scalerAddress` on objects that ask for the scaling manager but name a namespace where no scaler runs, and leaves one pointing at a second live install alone | `kubectl get scaledobject -n <ns> -o custom-columns=NAME:.metadata.name,ADDR:.spec.triggers[0].metadata.scalerAddress`, then `kubectl get svc -n "$NS" scaling-manager-external-scaler` (the address must be `scaling-manager-external-scaler.$NS...`). Admin, across the cluster: the same with `-A` |
| demand looks far too low for the load you are driving, and an `UnattributedReadyPods` Warning Event (`has N ready pod(s) but none attributed`) keeps recurring | FMA is in the namespace and nothing is scraping its launcher pods, so the traffic they serve is invisible | `kubectl get events -n <ns> --field-selector reason=UnattributedReadyPods`, then `make verify-fma`, see also [FMA launcher pods](#fma-launcher-pods) |
| The scaling manager applies no decisions for a workload, silently, though the HPA reads a healthy ratio the whole time | the ScaledObject's `modelID` no longer matches what the container actually serves — a hand-changed model that nothing re-syncs | `make verify-scaledobjects` |
| `check-prereqs`/`deploy-scaling-manager` refuses: "No llm-d found in `<ns>`" | the namespace holds no llm-d model servers yet | finish the llm-d install in `<ns>`, or `SKIP_CHECKS=true` |
| refuses: "Monitoring not enabled for your llm-d servers; please enable Model-server metrics" | nothing scrapes the model servers — the scaling manager would hold every workload at `minReplicas` and say so only in its log | run `make setup-prereqs` (same `SCOPE`): it adds the PodMonitor in each llm-d namespace where nothing scrapes the model servers. By hand: `kubectl apply -n <ns> -k config/modelserver-metrics`, or llm-d's own `guides/recipes/modelserver/components/monitoring` |
| warns: "the PodMonitor scrapes the container port named 'modelserver', and no model-server pod in `<ns>` declares it" (not a refusal) | the scaling manager's PodMonitor scrapes the port llm-d names `modelserver`. A model server whose port has another name (`http`, say) gives Prometheus no target, so no engine metrics arrive. The warning lists each pod with its port names. The same check warns when model-server pods run but the PodMonitor's selector matches none of them | name the model server's metrics port `modelserver` (or give the pods the labels it selects), or add a PodMonitor of your own for the port it uses |
| refuses: "Router monitoring not enabled; please enable EPP metrics" | nothing scrapes the EPP — the throughput analyzer has no arrival-rate signal (both the flow-control enqueue counter and `scheduler_attempts_total` read 0) | run `make setup-prereqs` (same `SCOPE`): it adds a ServiceMonitor, and the token it scrapes with, for each EPP nothing scrapes. Or llm-d's own: re-run the router install with `guides/recipes/router/features/monitoring.values.yaml` |
| warns: "EPP flow control: off" (not a refusal) | the `flowControl` feature gate is off, as llm-d ships it. Scaling works; a model at zero replicas never wakes, and the arrival rate reads low while a model is saturated | add `featureGates: [flowControl]` to the EPP's `EndpointPickerConfig`, re-run the router install, restart the EPP; llm-d ships a ready values file at `guides/workload-autoscaling/keda-epp/<guide>/router.values.yaml` |

### `no children to pick from` after a reinstall

A ScaledObject that outlived an uninstall can stay `READY False` after the
reinstall, against a healthy scaler. KEDA's gRPC client backs off on a name
that did not resolve while the scaling manager was gone, for longer than the
reinstall takes.

Restart the KEDA operator (cluster admin: KEDA is shared by every tenant):

```bash
kubectl rollout restart deploy/keda-operator -n "${KEDA_NAMESPACE:-keda-system}"
```

`keda-system` is the installer's default (`KEDA_NAMESPACE`). A KEDA the cluster
already had may live elsewhere; find it with
`kubectl get pods -A -l app.kubernetes.io/name=keda-operator` (admin).

The ScaledObjects go `READY True` within a poll interval of the restart.
Deleting ScaledObjects before uninstalling avoids the problem; `make
undeploy-scaling-manager` does that for the ones it created. See [Upgrade and
uninstall](upgrade-and-uninstall.md).

### FMA launcher pods

A namespace running Fast Model Actuation needs two things, and both fail
silently when missing:

- **The launcher pods must be scraped.** They declare no container ports, so a
  PodMonitor that selects by port name makes no target for them at all. Fix with
  `kubectl apply -k config/fma-launcher-metrics -n <ns>`.
- **The plan must target the requester**, not a decode Deployment, when the
  requester is the only serving workload there.

Symptoms: demand far below the load you drive, a recurring
`UnattributedReadyPods` Warning Event (`has N ready pod(s) but none
attributed`), or a variant flat at `minReplicaCount` while the queue grows. The
text is in the Event, not the log:

```bash
kubectl get events -n <ns> --field-selector reason=UnattributedReadyPods
```

How attribution works, how to size `maxReplicas` from the launcher pool, and
why GPU accounting is a lower bound: [the FMA
post-mortem](../proposals/fma-post-mortem.md).

First stop for any of these:

```bash
kubectl logs -n "$NS" -l app.kubernetes.io/name=scaling-manager --tail=200
```

Deeper diagnosis — EPP metrics, scale-from-zero, slow scale-up — is in
[Troubleshooting](troubleshooting.md).

## Changing policy safely

Thresholds, tiers, scale-to-zero and limiters live in
`scaling-manager-scaling-policy-config` in the controller's namespace. Cluster limits an
admin publishes live in the same-named ConfigMap in the policy namespace
(`scaling-manager-policy` by default); editing that one is a cluster-admin action.

When an edit takes effect:

- **The controller's own ConfigMap:** live, no restart. Exception: a controller
  installed out of reach (`LLMSM_WATCH_NS` naming another namespace) reads it only
  at start.
- **The policy namespace's ConfigMap:** live for a cluster-scoped controller. A
  namespace-scoped controller reads it once at start, so restart each one
  (`kubectl rollout restart deploy/scaling-manager-controller-manager -n <its ns>`).

1. **Back up** (strip the fields that would block a later replace):

   ```bash
   kubectl get configmap scaling-manager-scaling-policy-config -n "$NS" -o yaml \
     | yq 'del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.managedFields)' \
     > policy-backup.yaml
   ```

2. **Edit**:

   ```bash
   kubectl edit configmap scaling-manager-scaling-policy-config -n "$NS"
   ```

3. **Confirm it took**, within a cycle:

   ```bash
   kubectl logs -n "$NS" -l app.kubernetes.io/name=scaling-manager --tail=-1 --since=5m \
     | grep -E 'Updated global scaling policy from ConfigMap|Loaded cluster policy from the policy namespace|GPU limiter \(re\)built from config|Effective scaling policy'
   ```

   | line | means |
   | --- | --- |
   | `Updated global scaling policy from ConfigMap` | the controller's own ConfigMap was read; `entries` is how many tiers parsed |
   | `Loaded cluster policy from the policy namespace` | the admin's policy ConfigMap was read; carries `limiters` and `limiterMode` |
   | `GPU limiter (re)built from config` | a `limiters:` change took effect |
   | `Effective scaling policy` | which tier a model resolved to |

   No line means the edit did not apply. Look for an error in the same log
   window; usually a malformed block.

4. **Roll back** if it misbehaves:

   ```bash
   kubectl replace -f policy-backup.yaml
   ```

These settings are read **only at startup**; changing them needs
`kubectl rollout restart -n "$NS" deploy/scaling-manager-controller-manager`:

| setting | where |
| --- | --- |
| Prometheus URL, TLS and token | the `config.yaml` key of the `scaling-manager-manager-config` ConfigMap, or the Deployment's env |
| `GLOBAL_OPT_INTERVAL` (the cycle length), feature flags, `LEARNED_STATE_*`, the Prometheus cache | same |
| enabling or disabling the throughput analyzer | scaling policy; the controller records a `ThroughputAnalyzerRestartRequired` Warning Event on the ConfigMap |

Every setting's answer is in the Applies column of [the configuration reference](configuration.md#where-settings-live).
A restart has its own cost; see [Upgrade and uninstall](upgrade-and-uninstall.md#after-every-upgrade-or-restart).

## What the controller is running with

The controller publishes what it has read and decided in the
`scaling-manager-controller-status` ConfigMap in its own namespace. Read it, rather than the
log, to confirm a policy edit took or to see which policy governs a controller:

```bash
kubectl get cm scaling-manager-controller-status -n "$NS" -o jsonpath='{.data}{"\n"}'
```

| key | value |
| --- | --- |
| `policyNamespace` | the namespace whose `scaling-manager-scaling-policy-config` is the cluster policy, the one limiters and the optimizer selection come from: its own, or a policy namespace such as `scaling-manager-policy` |
| `policyConfigMap` | that ConfigMap's name |
| `policyResourceVersion` | `metadata.resourceVersion` of the policy ConfigMap last **loaded**, accepted or rejected. Written only once that content is in force, so after an edit, a match with the ConfigMap's own `resourceVersion` means the controller runs with it. Empty when no policy ConfigMap is in force. Compare for equality only |
| `policyAccepted` | `true`, or `false` when the `default` entry was rejected; the controller then runs without it |
| `policyError` | why the `default` entry was rejected; empty when accepted |
| `policyIgnored` | values in the `default` entry that could not be used, as `<namespace>/<name>: ` (the ConfigMap) then `<field>="<value>": <reason>`, separated by `; `. Values are cut to 64 bytes; at most 10 fields, then `+<n> more`. The rest of the entry applies. Empty when none. Always the controller's own ConfigMap, even with a separate policy namespace. It describes the entry parsed in the latest read: when that `default` entry is rejected and the previous one stays, it is empty and `policyAccepted`/`policyError` say why |
| `limiter` | the limiter in force, `<mode>/<entry names>`, e.g. `quota/install-quota`, or `none` |
| `limiterWarnings` | what is wrong with the limiters in force though they still apply, joined by `; `: for example a namespace a quota excludes that no limit bounds ([validation](quota-limiter.md#validation)). Empty when none |
| `utilizationShare` | the optimizer's mode: `off`, `invalid`, `shadow` or `active` |
| `utilizationShareError` | the optimizer block's error when the mode is `invalid` |
| `passiveDefault` | `true` or `false`: the controller-wide passive switch in force ([passive](scaling-policy.md#passive)). The `passive` key of the [settings ConfigMap](configuration.md#the-settings-configmap) when set, which is live; else the `LLMSM_PASSIVE` read at start |
| `passiveAtStart` | `true` or `false`: the `LLMSM_PASSIVE` the controller read at start. It is read only then: a different value in `scaling-manager-manager-config` means a restart is due, and `make deploy-scaling-manager` restarts it. Not when `settingsSources` says `passive=settings`: `LLMSM_PASSIVE` is then not read |
| `settingsResourceVersion`, `settingsSource`, `settingsSources`, `settingsFromManagerConfig`, `settingsRejected`, `settingsUnknownKeys` | what the controller read from the settings ConfigMap, and where each setting comes from ([confirm a change](configuration.md#confirm-a-change)) |
| `utilizationShareNote` | present while the settings in force differ from the block as written. Today: a namespace-scoped controller with no quota on its namespace ignores `physicalGroups`. The mode is not `invalid` |
| `resolvedAccelerators` | the GPU products the controller resolved for its models in its last cycle, joined by `,`: the names a quota key must match. Empty when that cycle had no active model (every model at zero) or resolved none of them (see the `AcceleratorNotResolved` events); absent until the first cycle. It follows the cycle's decisions: on a cycle with no decision for a model, a product known only from the labels of the nodes its pods ran on can drop out until the next one. `make utilization-share-check` reads it |
| `leader` | the pod that wrote it |
| `controllerInstance` | the `controller_instance` metrics label, when set |
| `clusterQuotaPerController` | present while a namespace-scoped controller enforces a `scope: cluster` quota: it is charged only its own variants' GPUs, so the quota caps this controller, not the cluster. With one controller on the cluster that is the same cap |
| `podListDenied` | present while a namespace-scoped controller under `gpu-inventory` cannot list Pods in every namespace (`llmsm_pod_access_denied`). If other namespaces run GPU pods on the same nodes, their GPUs read as free; if none do, nothing is miscounted. LeaderWorkerSet group detection reads the same Pod list, so while this is present it sees only this namespace's Pods too |
| `scalerUnreachable` | present while the KEDA external scaler, on mutual TLS, refused at least three TLS handshakes over a minute or more within the last five minutes and served no call (`llmsm_scaler_keda_unreachable`): KEDA reaches nothing, so the controller discovers no model and nothing scales. It says how many were refused, the last reason and the likely causes: `remote error: tls: bad certificate` or `certificate signed by unknown authority` from KEDA's Pod means KEDA's operator holds the CA or client certificate of before the scaler's CA changed (restart it); `first record does not look like a TLS handshake` means a ScaledObject dials in plaintext ([troubleshooting](troubleshooting.md#scaledobject-ready-false-or-targets-unknown)) |
| `scalerTLS` | an install from the release files only (`--external-scaler-tls-bootstrap`): how the KEDA external scaler serves. `mutual TLS: ...` names the three Secrets, the client identity, the CA's sha256 and when the serving certificate expires; `plaintext: <why>` says why not, most often that KEDA's side (ConfigMap `scaling-manager-scaler-keda-auth`) was not in place when the controller started. Absent on a `make` install, whose installer provisions the certificates |
| `scalerTLSAction` | with the same flag: what a person must run about the scaler's certificates, as numbered steps, one per line, each with its command; set `KEDA_NS` to KEDA's namespace before running them. Wire KEDA to the certificates the controller minted (copy the client Secret into KEDA's namespace, apply `scaling-manager-keda-auth.yaml`, restart the controller); restart the controller once KEDA's side is in place; restart KEDA's operator after a new CA; rotate an expiring CA. The controller never replaces an existing CA by itself: KEDA keeps the CA of its first connection until its operator restarts. Absent when there is nothing to do |
| `scalerCACert` | with the same flag: the public PEM certificate the scaler's serving certificate verifies against, which is what KEDA is told to trust: the scaler CA, or the OpenShift service CA bundle when that CA issued the serving certificate. Never a private key. A check of the scaler's TLS verifies against it instead of reading Secrets |
| `scalerCASha256` | the sha256 of the first certificate in `scalerCACert` |
| `inFlightLedger` | state, not status: the node holes held for replicas whose pods hold no node yet, as JSON, each with its end time ([GPU limiter](gpu-limiter.md#a-replica-must-fit-one-node)). The controller reads it back after a restart or a leader change. Do not edit it; a value it cannot parse is ignored and rewritten |
| `formatVersion` | `1`; bumped only for an incompatible change to these keys |

Only the leader writes it, and only when a value changes. A failed write is
logged and retried and never stops scaling. A missing ConfigMap means the
controller predates it or lacks the `scaling-manager-controller-status-role` grant (a
cluster admin re-runs `make setup-prereqs`). It does not mean scaling stopped.

## Stopping and leader handover

**Stopping takes up to 40s.** On SIGTERM the controller finishes the running
cycle, stops (up to 25s) and releases its lease, so the next leader starts at
once. The Deployment allows 40s (`terminationGracePeriodSeconds`).

On a slow API server the lease release itself can take up to `RenewDeadline`
(50s by default). The kubelet kills the pod at 40s, so the release may not
finish. The next leader then waits for the lease to expire (`LeaseDuration`,
60s by default) before it takes over. Scaling decisions pause for that minute;
nothing else is lost.

A kustomize patch that keeps `terminationGracePeriodSeconds` at 10 is no worse
than before the upgrade. It only loses the faster failover: the pod is killed
before it releases the lease, and the next leader waits out `LeaseDuration`.

<details>
<summary>Details</summary>

The release is one Get and one Update of the Lease, bounded together by
`RenewDeadline` (client-go `LeaderElector.release`); controller-runtime gives
each request a timeout of `RenewDeadline/2`.

</details>

## Command cheatsheet

```bash
# === llm-d-scaling-manager Controller ===
kubectl get pods -n "$NS"
kubectl logs -n "$NS" -l app.kubernetes.io/name=scaling-manager -f
kubectl describe deployment scaling-manager-controller-manager -n "$NS"

# === Managed workloads (a ScaledObject IS the registration) ===
kubectl get scaledobject,hpa -n <namespace>
kubectl describe scaledobject <name> -n <namespace>
kubectl describe hpa keda-hpa-<name> -n <namespace>
kubectl get events -n <namespace> --field-selector involvedObject.kind=ScaledObject
kubectl get scaledobject,hpa -A                       # (admin) every namespace
NAMESPACE=<namespace> make scaledobjects-rewire       # HPA on cpu only: KEDA asks for the metric again

# === Metrics and Monitoring ===
kubectl get servicemonitor,podmonitor -n <namespace>
kubectl get servicemonitor,podmonitor -A              # (admin)
kubectl port-forward -n <monitoring-namespace> svc/kube-prometheus-stack-prometheus 9090:9090

# === KEDA (admin; keda-system unless KEDA_NAMESPACE said otherwise) ===
kubectl get pods -n "${KEDA_NAMESPACE:-keda-system}"
kubectl logs -n "${KEDA_NAMESPACE:-keda-system}" -l app=keda-operator --tail=-1

# === vLLM / Application ===
kubectl get pods -n <app-namespace>
kubectl logs -n <app-namespace> <vllm-pod>
kubectl port-forward -n <app-namespace> <vllm-pod> 8000:8000

# === Configuration ===
kubectl get configmap -n "$NS"
kubectl get configmap scaling-manager-manager-config -n "$NS" -o yaml          # Prometheus URL, intervals
kubectl get configmap scaling-manager-scaling-policy-config -n "$NS" -o yaml   # thresholds, tiers, limiters
```
