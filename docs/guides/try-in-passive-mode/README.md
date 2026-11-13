# Try the scaling manager in passive mode

## Overview

In passive mode the scaling manager runs next to whatever scales your models
today. It reads the same metrics, decides what it would order, and publishes
that, but it changes no replica count. You compare its numbers with yours, then
hand it one model at a time.

The release files install passive by default, so this guide starts from that
install. It shows where to read what the controller would have done, switches
one model to active and back, and does the same for the
[utilization-share optimizer](#utilization-share-in-passive-mode). Every
setting is described in [the scaling policy](../../reference/scaling-policy.md#passive).

## Prerequisites

- [ ] llm-d model servers in the namespace. No model yet? [Install a small
  llm-d model](../install-small-model/).
- [ ] KEDA and a Prometheus, as for [any install](../install-in-namespace/#prerequisites).
- [ ] `kubectl`, and the release files you installed from.

<!-- guide:prerequisites start -->
```bash
# The namespace your model servers run in, and where the controller runs (the same one unless your admin runs it elsewhere).
export NAMESPACE=<your-namespace>
export CONTROLLER_NAMESPACE=${NAMESPACE}
```
<!-- guide:prerequisites end -->

## Installation Instructions

### 1. Install the controller

Follow [Install the scaling manager in a namespace](../install-in-namespace/),
steps 1 to 4. Leave `passive: "true"` in the settings, as shipped. Install it
before you register any model, so that it never acts on one.

### 2. Check it is passive

<!-- guide:deploy.passivedefault start -->
```bash
# Prints true while the controller is passive.
kubectl get configmap scaling-manager-controller-status -n ${CONTROLLER_NAMESPACE} \
  -o jsonpath='{.data.passiveDefault}{"\n"}'
```
<!-- guide:deploy.passivedefault end -->

You should see `true`. An empty answer means the controller has not started
yet; wait a minute and run it again.

### 3. Register the models

The scaling manager sees a model only through a KEDA ScaledObject with its
trigger. What you do depends on what scales the model today.

**Scaled by hand, or not at all.** Create its ScaledObject as in
[Register the workloads](../install-in-namespace/#register-the-workloads). Set
`minReplicaCount` and `maxReplicaCount` so the model's current replica count
lies between them; a count outside them moves to the nearest bound. This is
the `hold` mode: the count stays where it is, and `kubectl scale` still
changes it.

**Scaled by KEDA.** Do not replace its triggers. Add the scaling manager's
beside yours:

<!-- guide:deploy.observevars start -->
```bash
# The model KEDA scales today, its ScaledObject, and the model it serves.
export SCALEDOBJECT=<its ScaledObject>
export MODEL_ID=<the model it serves>
```
<!-- guide:deploy.observevars end -->

<!-- guide:deploy.observe start -->
```bash
# Add the scaling manager's trigger beside the ones that scale the model. Passive, it answers 0 and leaves the scaling to them.
cat > observe-trigger.yaml <<EOF
- op: add
  path: /spec/triggers/-
  value:
    type: external-push
    name: llmsm
    metricType: AverageValue
    metadata:
      scalerAddress: scaling-manager-external-scaler.${CONTROLLER_NAMESPACE}.svc:9090
      modelID: ${MODEL_ID}
    authenticationRef:
      name: scaling-manager-external-scaler-${CONTROLLER_NAMESPACE}
      kind: ClusterTriggerAuthentication
EOF
kubectl patch scaledobject ${SCALEDOBJECT} -n ${NAMESPACE} --type json --patch-file observe-trigger.yaml
```
<!-- guide:deploy.observe end -->

KEDA keeps scaling on your triggers. This is the `observe` mode.

**Scaled by an HPA you wrote.** KEDA cannot share a workload with another
HPA: two HPAs on one workload fight over its replica count. Move that HPA's
metric into a KEDA ScaledObject first (KEDA has `cpu`, `memory` and
`prometheus` triggers), then follow the KEDA case.

## Verification

### 1. Check each model is passive

Wait a few minutes after step 3, then:

<!-- guide:verify.mode start -->
```bash
# Needs read access to the controller's logs.
kubectl logs -n ${CONTROLLER_NAMESPACE} deploy/scaling-manager-controller-manager --tail=-1 \
  | grep -E 'Passive mode (entered|left|changed)'
```
<!-- guide:verify.mode end -->

You should see two lines for each model, one from the controller's cycle and
one from the scaler KEDA calls, with the mode and the setting that decided it:

```text
2026-11-08T18:20:16Z	INFO	scaler/passive.go:58	Passive mode entered: the external scaler answers KEDA for this variant without moving it	{"namespace": "llm-d-sim", "scaledObject": "dev-model-decode-keda", "model": "test-model", "mode": "observe", "previousMode": "", "source": "LLMSM_PASSIVE"}
2026-11-08T18:20:25Z	INFO	steadystate/passive.go:101	Passive mode entered: the controller computes and publishes for this variant and changes none of its replica counts	{"namespace": "llm-d-sim", "model": "test-model", "variant": "dev-model-decode", "mode": "observe", "source": "LLMSM_PASSIVE", "foreignTriggers": 1}
```

`mode` is `hold` for a model with the scaling manager's trigger alone and
`observe` for one whose KEDA triggers you kept. `observe-unsafe` means a
`scalingModifiers` formula names the scaling manager's trigger; take it out of
the formula.

### 2. Compare what it would order with what runs

The operational dashboard's **Replica Overview** panel draws, for each
variant, `<variant> - current`, the replicas running, and `<variant> -
desired`, what the scaling manager would run. In passive mode the two lines
differ, and the gap is what it would have done. The same in Prometheus,
where `exported_namespace` is the model's namespace:

```promql
# what it would run, minus what runs, for the passive variants only
max by (exported_namespace, variant_name) (llmsm_desired_replicas)
  - max by (exported_namespace, variant_name) (llmsm_current_replicas)
  and on(exported_namespace, variant_name)
  max by (exported_namespace, variant_name) (llmsm_passive_mode{mode=~"hold|observe|observe-unsafe"} == 1)
```

Each time it would scale, it records an Event on the model's ScaledObject:

<!-- guide:verify.events start -->
```bash
# What it would have done, as Events on each ScaledObject.
kubectl get events -n ${NAMESPACE} --field-selector reason=PassiveRecommendation
```
<!-- guide:verify.events end -->

The message reads `passive: would scale from 3 to 5 replicas`, followed by the
reason. Leave it through each model's busy period, typically a day. See
[Reading the gap](#reading-the-gap) for what the gap cannot tell you.

## Make one model active

### 1. Make the model safe to scale

<!-- guide:active.readiness start -->
```bash
# From a checkout of the same release. Report what the model servers still need. Changes nothing.
make workload-patch NAMESPACE=${NAMESPACE}
```
<!-- guide:active.readiness end -->

Apply what it reports before the model scales. Without a drain hook, every
scale-down cuts the responses a replica is still writing. Without a checkout,
the release's tools image runs the same report (`smt` as defined in
[Prepare model servers to scale](../prepare-workloads/#prerequisites)):

```bash
smt workload-patch -n ${NAMESPACE}
```

### 2. Hand the model over

For a model in `hold`, there is nothing to do here. For a model in `observe`,
remove KEDA's other triggers from its ScaledObject first, so the scaling
manager's trigger is the only one. Done the other way round, both triggers
scale the model for a while, and the HPA follows whichever asks for more.

### 3. Switch it to active

<!-- guide:active.model start -->
```bash
# The model to make active.
export MODEL_ID=<the model it serves>
```
<!-- guide:active.model end -->

<!-- guide:active.switch start -->
```bash
# Make one model active. The rest stay passive. Creates the scaling-policy ConfigMap when there is none.
cat > passive-trial.yaml <<EOF
data:
  passive-trial: |
    model_id: ${MODEL_ID}
    namespace: ${NAMESPACE}
    passive: false
EOF
kubectl create configmap scaling-manager-scaling-policy-config -n ${CONTROLLER_NAMESPACE} 2>/dev/null || true
kubectl patch configmap scaling-manager-scaling-policy-config -n ${CONTROLLER_NAMESPACE} --patch-file passive-trial.yaml
```
<!-- guide:active.switch end -->

The entry is read live. Run the [log check](#1-check-each-model-is-passive)
again. You should see `Passive mode left` for this model, with
`"source": "perModel"`. From now on a scale-up applies at the next HPA sync,
and a scale-down waits KEDA's 300-second window.

### 4. Switch it back

<!-- guide:active.back start -->
```bash
# Make it passive again. It inherits the passive setting.
kubectl patch configmap scaling-manager-scaling-policy-config -n ${CONTROLLER_NAMESPACE} \
  --type json -p '[{"op":"remove","path":"/data/passive-trial"}]'
```
<!-- guide:active.back end -->

You should see `Passive mode entered` again. The model stays at the count it
has; change it with `kubectl scale`.

### 5. Every model active

When you are done comparing, hand over each model still in `observe`, as in
step 2. Then:

<!-- guide:active.all start -->
```bash
# Make every model active. Live, no restart.
kubectl patch configmap scaling-manager-settings -n ${CONTROLLER_NAMESPACE} --type merge \
  -p '{"data":{"passive":"false"}}'
```
<!-- guide:active.all end -->

The [`passiveDefault` check](#2-check-it-is-passive) then prints `false`.

## Utilization share in passive mode

The [utilization-share optimizer](../utilization-share/) moves GPUs between
models under one quota. A group of models that holds a passive model never
moves anything: it computes and publishes the shares, and starts no transfer.
So a passive install shows you what sharing would do before it does anything.

You need two or more models in the namespace, on one GPU product. Find that
product, and say how many of its GPUs the models may hold together:

<!-- guide:share.products start -->
```bash
# The GPU product of each node.
kubectl get nodes -L nvidia.com/gpu.product
```
<!-- guide:share.products end -->

<!-- guide:share.vars start -->
```bash
# The GPU product your models run on, and how many of its GPUs they may hold together.
export ACCELERATOR=<gpu product>
export GPUS=<gpus>
```
<!-- guide:share.vars end -->

Each step is a patch of the settings, read live:

<!-- guide:share.quota start -->
```bash
# A GPU quota for the models to share. Live.
kubectl patch configmap scaling-manager-settings -n ${CONTROLLER_NAMESPACE} --type merge \
  -p "{\"data\":{\"limiter\":\"quota\",\"quotas\":\"${ACCELERATOR}=${GPUS}\"}}"
```
<!-- guide:share.quota end -->

<!-- guide:share.shadow start -->
```bash
# Utilization share in shadow. Live.
kubectl patch configmap scaling-manager-settings -n ${CONTROLLER_NAMESPACE} --type merge \
  -p '{"data":{"utilizationShare":"shadow"}}'
```
<!-- guide:share.shadow end -->

<!-- guide:share.status start -->
```bash
# Prints the limiter and the utilization-share mode in force.
kubectl get configmap scaling-manager-controller-status -n ${CONTROLLER_NAMESPACE} \
  -o jsonpath='{.data.limiter} {.data.utilizationShare}{"\n"}'
```
<!-- guide:share.status end -->

You should see `quota/install-quota shadow`. Then let sharing act. That needs
the one grant of `patch` on pods, `scaling-manager-activate.yaml`, in the
namespace the models run in (download it with the other release files):

<!-- guide:share.activate start -->
```bash
# The pod-patch grant sharing needs, then the switch. While a model of a group is passive, the group only reports.
sed -e "s/__NAMESPACE__/${CONTROLLER_NAMESPACE}/g" -e "s/__MODEL_NAMESPACE__/${NAMESPACE}/g" \
  scaling-manager-activate.yaml | kubectl apply -f -
kubectl patch configmap scaling-manager-settings -n ${CONTROLLER_NAMESPACE} --type merge \
  -p '{"data":{"utilizationShare":"active"}}'
```
<!-- guide:share.activate end -->

The status now says `active`, because that is how the optimizer is configured.
What tells you it is held is `llmsm_utilization_share_passive_shadow`: it is
`1` for every group that holds a passive model. During a busy period the log
says what it would move:

<!-- guide:share.log start -->
```bash
# What it would move, from the controller's log.
kubectl logs -n ${CONTROLLER_NAMESPACE} deploy/scaling-manager-controller-manager --tail=-1 \
  | grep 'Utilization share: would rebalance'
```
<!-- guide:share.log end -->

```text
... Utilization share: would rebalance (shadow: the group holds a passive model) {"accelerator": "NVIDIA-A100-PCIE-80GB", "scope": "llm-d-sim", "budget": 4, ..., "replicasToMove": 1, "actionable": ["llm-d-sim/model-a/decode"], ...}
```

A quiet log means nothing would move. Read the rest in Prometheus:

```promql
# 1 while the group only reports because it holds a passive model
llmsm_utilization_share_passive_shadow == 1
# the GPUs the share would give each model, and which models a move would fix
llmsm_utilization_share_target_gpus{exported_namespace="<namespace>"}
llmsm_utilization_share_actionable{exported_namespace="<namespace>"} == 1
# how many replicas it would move
llmsm_utilization_share_replicas_to_move{scope="<namespace>"}
```

`llmsm_desired_replicas` of a passive model is what the scaling manager would
order without sharing; the share's answer is in `_target_gpus`, in GPUs. No
transfer starts, so `llmsm_utilization_share_transfers_total` does not grow.

### Go live

The group acts only once none of its models is passive. Hand over each model
still in `observe`, as in [step 2](#2-hand-the-model-over). Then make every
model active:

<!-- guide:share.live start -->
```bash
# Make every model active. Live, no restart.
kubectl patch configmap scaling-manager-settings -n ${CONTROLLER_NAMESPACE} --type merge \
  -p '{"data":{"passive":"false"}}'
```
<!-- guide:share.live end -->

For two to three minutes nothing in the group scales. Then
`llmsm_utilization_share_passive_shadow` reads `0`, and a transfer shows as
`UtilizationShareGiving` and `UtilizationShareReceiving` Events, then
`UtilizationShareReceived` once the receiver's replica is up.

## Cleanup

Remove any trigger you added in step 3 first (`kubectl edit scaledobject
${SCALEDOBJECT} -n ${NAMESPACE}`, delete the `llmsm` trigger): a trigger that
calls a removed scaler fails on every KEDA poll. If sharing was on, turn it off
first and wait until the status no longer says `draining`:

<!-- guide:cleanup.share start -->
```bash
# Sharing off first, so no transfer marks are left on pods. Wait until the status no longer says draining.
kubectl patch configmap scaling-manager-settings -n ${CONTROLLER_NAMESPACE} --type merge \
  -p '{"data":{"utilizationShare":"off"}}'
```
<!-- guide:cleanup.share end -->

Then uninstall as in [the install guide](../install-in-namespace/#cleanup),
and delete `scaling-manager-activate.yaml` from the model namespace with the
same `sed` and `kubectl delete -f -`. Delete the scaling-policy ConfigMap too:
it carries no label, so the uninstall leaves it.

<!-- guide:cleanup.policy start -->
```bash
# The scaling-policy ConfigMap step 3 of "Make one model active" created, if you created it. The label-based uninstall does not find it.
kubectl delete configmap scaling-manager-scaling-policy-config -n ${CONTROLLER_NAMESPACE} --ignore-not-found
```
<!-- guide:cleanup.policy end -->

## From a checkout instead

`make deploy-scaling-manager` installs passive with `LLMSM_PASSIVE=true`, and
the `make utilization-share-*` targets wrap the same settings with their own
checks ([Turn on the utilization-share optimizer,
safely](../utilization-share/)):

<!-- guide:checkout.install start -->
```bash
# The same passive install from a checkout of the release tag.
make check-prereqs
make setup-prereqs           # cluster admin
make deploy-scaling-manager LLMSM_PASSIVE=true
```
<!-- guide:checkout.install end -->

`make scaledobjects-plan` registers the models and offers `adopt` for one KEDA
scales; leave it at `no` while the model is in `observe` ([A new plan offers
adopt](#a-new-plan-offers-adopt)).

## Details

### A new plan offers adopt

Once the scaling manager's trigger sits on a ScaledObject, `make
scaledobjects-plan` writes `apply: adopt` for that workload. Adopt replaces
the ScaledObject's triggers with the scaling manager's alone, so applying it
drops yours. Set it to `no` until you hand the model over.

### A scaler without TLS

The trigger in step 3 is the TLS form. If the controller status's `scalerTLS`
starts with `plaintext`, step 4 of the install guide is not done yet: until it
is, use `scaling-manager-external-scaler.${CONTROLLER_NAMESPACE}.svc.cluster.local:9090`
and leave out `authenticationRef`.

### A scalingModifiers formula

If the ScaledObject combines its triggers with a `scalingModifiers` formula,
do not name the scaling manager's trigger in it. A passive trigger answers 0,
and the formula would take that in. The log then reports `observe-unsafe`.

### Reading the gap

In `observe` the scaling manager decides from the fleet KEDA sized, so the
gap is what it would order next from there, not where its own fleet would
be. In `hold` the fleet never moves, so it learns at one replica count only.

### A share group switched back to passive

Making one model of an acting group passive again puts the whole group back
to reporting. A transfer already under way is dropped after two minutes.

### What a passive model does not get

The warm pool plans no copy for it, scale-from-zero does not wake it, and no
node or borrowed Kueue capacity is held for its would-be growth. A passive
model at zero replicas stays at zero until you scale it. The full list is in
[the scaling policy](../../reference/scaling-policy.md#passive).

### Under a GPU limiter

The active models are allocated first, as if the passive ones were absent. A
passive model counts against the budget at what it holds, and its would-be
count is computed from what is left.

### Passive for some models only

The `passive` key works at every level of the scaling policy: the `default`
entry, a tier, or one model, the most specific winning. The settings key sets
the `default` entry's; a per-model entry still overrides it. See [the scaling
policy](../../reference/scaling-policy.md#passive).

### A count that must not move

`hold` answers the count you set, and the HPA makes the last call. In a few
rollout and Pending-pod cases it can move the count by one ([how exact the
hold is](../../reference/scaling-policy.md#how-exact-the-hold-is)). For a
count that must not move at all, use KEDA's
`autoscaling.keda.sh/paused-replicas` annotation.

## Next

- [A passive model does not scale](../../reference/troubleshooting.md#a-passive-model-does-not-scale):
  which mode it is in, and who set it.
- [Passive mode metrics](../../reference/prometheus.md#passive-mode)
- [Benchmark the scaling manager](../benchmarking/README.md#side-by-side-with-keda):
  KEDA scales under load, the scaling manager only reports.
- [Turn on the utilization-share optimizer, safely](../utilization-share/)
