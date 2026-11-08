# Scale a model to zero, and get it back

## Overview

An idle model releases all its accelerators, and comes back when a request
arrives. There are two parts, and they fail separately:

- **Parking** (the scaling manager): when a model serves nothing for
  `retentionPeriod`, every variant goes to zero.
- **Waking** (the EPP queue and KEDA): a request for a model with no endpoints
  waits in the EPP's flow-control queue. The scaling manager sees it and KEDA
  scales the workload up from zero.

Parking can work perfectly while waking is broken, and nothing looks wrong
until a request arrives. **Check the wake path first.**

You turn it on with two settings that must agree: scale-to-zero enabled for the
model, and `minReplicaCount: 0` on **every** variant.

## Prerequisites

- [ ] A model already serving under the scaling manager
  ([Install the scaling manager in a namespace](../install-in-namespace/)).
- [ ] The **running** EPP has the `flowControl` feature gate. Without it the
  model parks and never wakes.
- [ ] Each model runs **one** engine, vLLM or SGLang, not both.
- [ ] The model has served at least one request. Until then its request counter
  does not exist and it never parks ([why](#a-new-model-will-not-park)).

<!-- guide:prerequisites.flowcontrol start -->
```bash
# Confirm the running EPP parsed the flowControl gate. Wanted output contains featureGates:["flowControl"].
kubectl logs -n <llmd-namespace> deploy/<epp-deployment> --all-containers | grep -m1 -i featuregates
# Missing? Restart the EPP so it re-reads its config:
#   kubectl rollout restart -n <llmd-namespace> deploy/<epp-deployment>
```
<!-- guide:prerequisites.flowcontrol end -->

<!-- guide:prerequisites.engine start -->
```bash
# List each model server and its image. Each model must run one engine, vLLM or SGLang.
kubectl get deploy -n <llmd-namespace> -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.template.spec.containers[0].image}{"\n"}{end}'
```
<!-- guide:prerequisites.engine end -->

## Installation Instructions

**Half one: allow the model to park.**

<!-- guide:deploy.policy start -->
```bash
# Half one: allow the model to park.
kubectl edit configmap scaling-manager-scaling-policy-config -n <scaling-manager-namespace>
# Under `default`, or under the model's own entry, add:
#   scaleToZero:
#     enabled: true
#     retentionPeriod: 10m   # idle time before parking
```
<!-- guide:deploy.policy end -->

- The model's own entry is a per-model entry whose body names its `model_id` and
  `namespace`.
- A named tier can carry `scaleToZero` too. If a model's variants name
  different tiers, the model parks only if every one of them allows it, and
  waits for the longest `retentionPeriod` among them. A variant that names no
  tier follows the ones that do.
- `retentionPeriod` must be at least 30s. A shorter or misspelled value is
  skipped, and the model uses the value from the layer below: the tier's,
  then `default`'s, then 10m. The
  `ConfigFieldIgnored` event on the ConfigMap says so.
- Re-running `make deploy-scaling-manager` resets the `default` entry, which drops
  `scaleToZero`. Put it on a per-model entry or a named tier to keep it, or
  repeat the edit.
- Out-of-reach controller: it reads its own ConfigMap only at start, so restart
  it after the edit:
  `kubectl rollout restart deploy/scaling-manager-controller-manager -n <scaling-manager-namespace>`.

**Half two: let every variant reach zero.**

<!-- guide:deploy.bounds start -->
```bash
# Half two: let every variant reach zero. Every MIN must be 0.
kubectl get scaledobject -n <llmd-namespace> -o custom-columns=NAME:.metadata.name,MIN:.spec.minReplicaCount,MAX:.spec.maxReplicaCount
# Any MIN above 0: re-run the plan, set minReplicas: 0 on the model's entries, apply it
make scaledobjects-plan
make scaledobjects-apply
# or patch one ScaledObject directly:
#   kubectl patch scaledobject <name> -n <llmd-namespace> --type=merge -p '{"spec":{"minReplicaCount":0}}'
```
<!-- guide:deploy.bounds end -->

One variant left at `1` keeps the whole model up. Existing ScaledObjects appear
in the plan as `apply: adopt`, which updates their bounds in place.

**Check the ScaledObject, not the HPA.**

<!-- guide:deploy.hpa start -->
```bash
# Read the ScaledObject's MIN, not the HPA's. The HPA always reads 1.
kubectl get scaledobject -n <llmd-namespace> -o custom-columns=NAME:.metadata.name,MIN:.spec.minReplicaCount
# minReplicas: 1 on the derived HPA is expected:
kubectl get hpa -n <llmd-namespace> -o custom-columns=NAME:.metadata.name,MIN:.spec.minReplicas
```
<!-- guide:deploy.hpa end -->

KEDA always sets the HPA's `minReplicas` to `1`, because the HPA cannot express
0, and takes the last step to zero itself. Only `spec.minReplicaCount` on the
ScaledObject decides. If you changed it and nothing parks, re-apply the
ScaledObject through the plan. Never edit the HPA or delete the ScaledObject.

## Verification

**1. Ask what is in the way.** No output is the healthy answer.

<!-- guide:verify.blocked start -->
```bash
# Ask the controller what stops this model parking. No output is healthy.
TOKEN=$(kubectl get secret scaling-manager-epp-metrics-token -n <scaling-manager-namespace> -o jsonpath='{.data.token}' | base64 -d)
kubectl port-forward -n <scaling-manager-namespace> svc/scaling-manager-controller-manager-metrics-service 8443:8443 &
curl -sSfk -H "Authorization: Bearer $TOKEN" https://localhost:8443/metrics | grep llmsm_model_scaling_blocked
```
<!-- guide:verify.blocked end -->

This is the cluster admin's route: it needs the metrics-reader binding from
setup. Without it, `curl -f` fails with 401 rather than printing nothing. As a
namespace owner, ask Prometheus instead:
`llmsm_model_scaling_blocked{exported_namespace="<llmd-namespace>"} == 1`.

| Reason | What to change |
| --- | --- |
| `variant-floor` | a variant still has `minReplicaCount > 0`: finish half two |
| `policy-forbids-zero` | every variant allows zero but the policy does not: finish half one |
| `engine-unsupported` | the model runs **both** vLLM and SGLang |
| `activation-retention` | just woken and held for `retentionPeriod`. Clears itself |
| `no-wake-signal` | the EPP exports no flow-control queue. **Fix before letting it park** |

**2. Watch it park.**

<!-- guide:verify.park start -->
```bash
# Stop sending traffic and wait out retentionPeriod. llmsm_model_replicas 0 means parked.
kubectl get deploy -n <llmd-namespace> -w
curl -sSfk -H "Authorization: Bearer $TOKEN" https://localhost:8443/metrics | grep llmsm_model_replicas
```
<!-- guide:verify.park end -->

With the defaults this takes about 15 minutes ([why](#how-long-parking-takes)).

**3. Prove it wakes.**

<!-- guide:verify.wake start -->
```bash
# Park the model, send a request, and check every link of the wake. Restores what it changed.
./hack/verify_scale_from_zero.sh <llmd-namespace> <deployment> <modelID>
```
<!-- guide:verify.wake end -->

It names the link that broke. A wake caused by something else (a floor, a manual
scale, another controller) does not count as a pass: it also checks the queue
depth and the scaling manager's activation.

## Cleanup

<!-- guide:cleanup.disable start -->
```bash
# Set scaleToZero.enabled to false. Raise minReplicaCount back too, or it reports policy-forbids-zero.
kubectl edit configmap scaling-manager-scaling-policy-config -n <scaling-manager-namespace>
```
<!-- guide:cleanup.disable end -->

## When a model will not park

These settings are valid, cost money, and show no symptom: the model just keeps
serving.

| What you set | What happens |
| --- | --- |
| scale-to-zero on, one variant with `minReplicaCount > 0` | the model never reaches zero; idle accelerators stay billed |
| every variant at `minReplicaCount: 0`, scale-to-zero off | the scaling manager keeps the model up |
| every variant at `minReplicaCount: 0`, scale-to-zero on in `default` but off in a tier one variant names | the model never reaches zero: every tier its variants name must allow it. The log line `Scale-to-zero is disabled by the scaling policy of a tier` lists the tiers in `disabledBy` and says what to change in `fix`. Enable `scaleToZero` on each of them, or on the model's per-model entry. If that entry is what disables it, the line says `disabledByPerModelEntry=true`; enable it there |

The second is most misleading with a **single variant**, where
`minReplicaCount: 0` looks like a request to park. None is rejected. All
show on `llmsm_model_scaling_blocked`, and the controller logs once when the
answer changes.

### A new model will not park

A model that has never served a request has never emitted its request counter
(`vllm:request_success_total` or `sglang:num_requests_total`). A missing counter
is not zero, so the scaling manager keeps the fleet as it is and logs, every
cycle:

```text
ERROR Failed to get request count, keeping current decisions
  {"error": "no values in request count result for model <id> (metrics may not be scraped yet)"}
```

Send one request; the counter then exists and the model parks.

## When a model will not wake

`no-wake-signal` means the EPP exports **no** flow-control queue, not an empty
one. An idle queue reads `0` and is healthy. A missing queue reads nothing, and
a model parked behind it is stranded.

The scaling manager reports it for **serving** models too. Parking uses the
engine's request counter, not flow control, so a model can park behind an EPP
that can never wake it. The warning comes while the fix is still cheap.

The usual cause is an EPP pod that started before the gate was added. The EPP
reads `--config-file` once at startup and its Deployment has no
`checksum/config` annotation, so every ConfigMap edit needs a restart. That is
why the prerequisite check asks the running process, not the ConfigMap. The EPP
often runs as a sidecar next to envoy, so the check uses `--all-containers`.

## Monitoring

| Signal | Means |
| --- | --- |
| `llmsm_model_scaling_blocked{reason}` | present only while that reason holds; absent is healthy |
| `llmsm_model_replicas` | replicas serving a model, across variants. `0` is normal for a parked model |
| `LLMSMModelWillNotScaleToZero` | info, 15m: settings disagree, costing accelerators |
| `LLMSMModelHasNoWakeSignal` | warning, 10m: parked models here will not come back |
| `LLMSMModelParkedWhileRequestsRefused` | critical, 5m: at zero while requests are refused |

Page on the last one. It alerts on the symptom, so it catches causes nobody
listed. Start from `llmsm_model_scaling_blocked` for that model. The
[operational dashboard](../../reference/monitoring.md) has the panels *Models
that will not scale, by reason* and *Models at zero replicas*.

## Configuration

| Parameter | Default | Example |
| --- | --- | --- |
| `scaleToZero.enabled` | inherits `LLMSM_SCALE_TO_ZERO` | `true` |
| `scaleToZero.retentionPeriod` | `10m` | `5m` |
| `scaleFromZero.requirePrefill` | `false` (P/D: a decode variant may wake alone) | `true` |
| ScaledObject `minReplicaCount` | `1` | `0`, on **every** variant |
| ScaledObject `cooldownPeriod` (KEDA) | `300s` | `30s` |

## How long parking takes

Two timers run one after the other:

```text
last request ──┬─ retentionPeriod ─────┬─ ≤1 optimize interval ─┬─ cooldownPeriod ─┬─ 0 replicas
               │ (scaling manager waits)│ (trigger goes inactive)│ (KEDA waits)     │
```

KEDA's cooldown starts only after the scaling manager decides the model needs
zero. With the defaults that is **10m + 300s, about 15 minutes**, after the last
request. Shortening one timer shortens only its own part.

Both timers apply only to the final step out of service. A scale-down while the
model still serves (10 to 3) goes through the HPA and waits for neither.

`retentionPeriod` also holds a just-woken model up before it may park again.
Without that hold, the wake would be undone before the new pod could serve the
request still waiting in the EPP queue.

## Details

- **Where the setting is read.** The `default` entry, then the tier each
  variant names in `scalingPolicy` (or the tier `default.defaultPolicy` names),
  then a per-model entry, which wins. Parking is a model property: a model is at
  zero only when nothing serves it. So when its variants name different tiers,
  it parks only if every tier allows it, and waits for the longest
  `retentionPeriod` among them.
- **Why an out-of-reach controller needs a restart.** It reads its own
  ConfigMap only at start. A namespace-local map in the model's namespace is read
  live ([why](../../reference/scaling-policy.md#limiters-cluster-default-only-live)).
- **Why one engine.** Idleness comes from the engine's request counter. A model
  running both engines would need both counters summed, so it is refused rather
  than measured with half its traffic.

## Next

- [After the install](../../reference/operations.md): what to watch, first-line
  troubleshooting
- [Scaling policy configuration](../../reference/scaling-policy.md): every
  field, and how tiers resolve
- [Monitoring](../../reference/monitoring.md): the dashboard and every metric
