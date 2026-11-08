# Try the scaling manager in passive mode

## Overview

In passive mode the scaling manager runs next to whatever scales your models
today. It reads the same metrics, decides what it would order, and publishes
that, but it changes no replica count. You compare its numbers with yours, then
hand it one model at a time.

This guide installs it passive in one namespace, shows where to read what it
would have done, and switches one model to active and back. The last part does
the same for the [utilization-share optimizer](#utilization-share-in-passive-mode).
Every setting is described in [the scaling policy](../../reference/scaling-policy.md#passive).

## Prerequisites

- [ ] llm-d model servers in the namespace. No model yet? [Install a small
  llm-d model](../install-small-model/).
- [ ] KEDA and a Prometheus, as for [any install](../install-in-namespace/#prerequisites).
- [ ] A cluster admin has run step 1, or you are a cluster admin.
- [ ] `kubectl`, `jq`, `yq` and `make`, in a checkout of this repository.

<!-- guide:prerequisites start -->
```bash
# The namespace your model servers run in, and where the controller runs (the same one unless your admin runs it elsewhere).
export NAMESPACE=<your-namespace>
export CONTROLLER_NAMESPACE=${NAMESPACE}

# Read-only. Reports the namespace, its model servers and the Prometheus it found.
make check-prereqs
```
<!-- guide:prerequisites end -->

If `check-prereqs` says the namespace is empty or missing, fix `NAMESPACE`
before going on.

## Installation Instructions

### 1. Cluster admin: prepare the namespace

<!-- guide:deploy.prereqs start -->
```bash
make setup-prereqs
```
<!-- guide:deploy.prereqs end -->

This is the same step as in [Install the scaling manager in a
namespace](../install-in-namespace/#1-cluster-admin-prepare-the-namespace).

### 2. Install the controller, passive

Install it passive before you register any model, so that it never acts on one.

<!-- guide:deploy.controller start -->
```bash
# The controller, passive for every model. It computes and publishes, and changes no replica count.
make deploy-scaling-manager LLMSM_PASSIVE=true
```
<!-- guide:deploy.controller end -->

You should see this warning near the end:

```text
[WARNING] Passive mode is ON controller-wide (LLMSM_PASSIVE=true): the controller computes and publishes, and changes no replica count. Pass LLMSM_PASSIVE=false to turn it off.
```

Then check the running controller read it. It reads `LLMSM_PASSIVE` only when
it starts:

<!-- guide:deploy.passivedefault start -->
```bash
# Prints true when the running controller started passive.
kubectl get configmap scaling-manager-controller-status -n ${CONTROLLER_NAMESPACE} \
  -o jsonpath='{.data.passiveDefault}{"\n"}'
```
<!-- guide:deploy.passivedefault end -->

You should see `true`. An empty answer means the controller has not started
yet; wait a minute and run it again.

### 3. Register the models

The scaling manager sees a model only through a KEDA ScaledObject with its
trigger. What you do depends on what scales the model today.

**Scaled by hand, or not at all.** Register it with the plan:

<!-- guide:deploy.register start -->
```bash
make scaledobjects-plan LLMSM_DEFAULT_SO_PLAN=passive-plan.yaml
# edit passive-plan.yaml: apply: yes for a model scaled by hand, no for one KEDA scales
make scaledobjects-apply LLMSM_DEFAULT_SO_PLAN=passive-plan.yaml
```
<!-- guide:deploy.register end -->

Set `minReplicas` and `maxReplicas` in the plan so the model's current replica
count lies between them; a count outside them moves to the nearest bound.
This is the `hold` mode: the count stays where it is, and `kubectl scale`
still changes it.

**Scaled by KEDA.** The plan writes `apply: "no"` for it, with a note that
offers `adopt`. Leave it at `no`: `adopt` replaces the ScaledObject's triggers
with the scaling manager's. Add the scaling manager's trigger beside yours
instead:

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
    name: scaling-manager
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

KEDA keeps scaling on your triggers. This is the `observe` mode. `MODEL_ID`
is the `modelID` the plan wrote for that workload. Before you run the plan
again, read [A new plan offers adopt](#a-new-plan-offers-adopt).

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

`mode` is `hold` for a model registered with the plan and `observe` for one
whose KEDA triggers you kept. `observe-unsafe` means a `scalingModifiers`
formula names the scaling manager's trigger; take it out of the formula.

### 2. Compare what it would order with what runs

Open the operational dashboard ([The dashboard](../install-in-namespace/#3-optional-see-it-in-a-dashboard))
and read the **Replica Overview** panel. For each variant it draws
`<variant> - current`, the replicas running, and `<variant> - desired`, what
the scaling manager would run. In passive mode the two lines differ, and the
gap is what it would have done.

The same in Prometheus. `exported_namespace` is the model's namespace:

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
reason.

Leave it through each model's busy period, typically a day. A positive gap
during a peak is a scale-up it would have made; a negative gap when load is
low is capacity it would have released. See [Reading the
gap](#reading-the-gap) for what the gap cannot tell you.

## Make one model active

### 1. Make the model safe to scale

<!-- guide:active.readiness start -->
```bash
# Report what the model servers still need. Changes nothing.
make workload-patch NAMESPACE=${NAMESPACE}
```
<!-- guide:active.readiness end -->

Apply what it reports before the model scales. Without a drain hook, every
scale-down cuts the responses a replica is still writing. See [Make the
workloads safe to scale](../install-in-namespace/#3-make-the-workloads-safe-to-scale).

### 2. Hand the model over

For a model in `hold`, there is nothing to do here.

For a model in `observe`, remove KEDA's other triggers first. Run the plan
again: it now offers `apply: adopt` for that workload, because its
ScaledObject carries the scaling manager's trigger. Keep `adopt` for that
entry and apply. The ScaledObject keeps its name and bounds, its triggers are
replaced by the scaling manager's alone, and the model moves to `hold` at its
current count. Done the other way round, both triggers scale the model for a
while, and the HPA follows whichever asks for more.

### 3. Switch it to active

<!-- guide:active.model start -->
```bash
# The model to make active.
export MODEL_ID=<the model it serves>
```
<!-- guide:active.model end -->

<!-- guide:active.switch start -->
```bash
# Make one model active. The rest stay passive.
cat > passive-trial.yaml <<EOF
data:
  passive-trial: |
    model_id: ${MODEL_ID}
    namespace: ${NAMESPACE}
    passive: false
EOF
kubectl patch configmap scaling-manager-scaling-policy-config -n ${CONTROLLER_NAMESPACE} --patch-file passive-trial.yaml
```
<!-- guide:active.switch end -->

The key is read live. Run the [log check](#1-check-each-model-is-passive)
again. You should see `Passive mode left` for this model, with
`"source": "perModel"`:

```text
2026-11-08T18:21:46Z	INFO	scaler/passive.go:60	Passive mode left: the external scaler answers KEDA with the controller's decision again	{"namespace": "llm-d-sim", "scaledObject": "dev-model-decode-keda", "model": "test-model", "mode": "active", "previousMode": "observe", "source": "perModel"}
2026-11-08T18:21:56Z	INFO	steadystate/passive.go:103	Passive mode left: the controller scales this variant again, under the HPA's own behavior	{"namespace": "llm-d-sim", "model": "test-model", "variant": "dev-model-decode", "mode": "active", "source": "perModel", "foreignTriggers": 1}
```

From now on a scale-up applies at the next HPA sync,
and a scale-down waits KEDA's 300-second window.

### 4. Switch it back

<!-- guide:active.back start -->
```bash
# Make it passive again. It inherits LLMSM_PASSIVE=true.
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
# Make every model active. The controller restarts to read it.
make deploy-scaling-manager LLMSM_PASSIVE=false
```
<!-- guide:active.all end -->

You should see `Restarting the controller` in its output, and then `false`
from the [`passiveDefault` check](#2-install-the-controller-passive).

## Utilization share in passive mode

The [utilization-share optimizer](../utilization-share/) moves GPUs between
models under one quota. A group of models that holds a passive model never
moves anything: it computes and publishes the shares, and starts no transfer.
So a passive install shows you what sharing would do before it does anything.

Install as in step 2, with a quota for the models to share:

<!-- guide:share.install start -->
```bash
# The controller, passive, with a GPU quota for the models to share.
make deploy-scaling-manager LLMSM_PASSIVE=true \
  LLMSM_LIMITER=quota LLMSM_QUOTAS='<accelerator>=<gpus>'
```
<!-- guide:share.install end -->

Register the models as in step 3, then check there is a budget and turn the
optimizer on:

<!-- guide:share.check start -->
```bash
# Read-only. Is there a budget to share, and does the GPU product resolve.
make utilization-share-check
```
<!-- guide:share.check end -->

<!-- guide:share.activate start -->
```bash
# Configure the optimizer to act. While a model of a group is passive, the group only reports.
make utilization-share-activate
```
<!-- guide:share.activate end -->

`make utilization-share-status` and `llmsm_utilization_share_mode` say
`active`, because that is how the optimizer is configured. What tells you it
is held is `llmsm_utilization_share_passive_shadow`: it is `1` for every group
that holds a passive model. During a busy period the log says what it would
move:

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

The line is written only when the roles a move would fix change, so a quiet
log means nothing would move. Read the rest in Prometheus:

```promql
# 1 while the group only reports because it holds a passive model
llmsm_utilization_share_passive_shadow == 1
# the GPUs the share would give each model, and which models a move would fix
llmsm_utilization_share_target_gpus{exported_namespace="<namespace>"}
llmsm_utilization_share_actionable{exported_namespace="<namespace>"} == 1
# how many replicas it would move
llmsm_utilization_share_replicas_to_move{scope="<namespace>"}
```

Two things differ from the per-model comparison above. `llmsm_desired_replicas`
of a passive model is what the scaling manager would order without sharing,
not the share's target; the share's answer is in `_target_gpus`, in GPUs. And no
transfer starts, so `llmsm_utilization_share_transfers_total` does not grow
and there are no `UtilizationShareGiving` or `UtilizationShareReceiving`
Events.

To go live, make every model of the group active, as in [Make one model
active](#make-one-model-active). The group acts once none of its models is
passive, after two to three minutes in which nothing in it scales.

If your models are already active and you only want sharing to report first,
you do not need passive mode: use the optimizer's own shadow mode in [Turn on
the utilization-share optimizer, safely](../utilization-share/).

## Cleanup

Remove any trigger you added in step 3 first (`kubectl edit scaledobject
${SCALEDOBJECT} -n ${NAMESPACE}`, delete the `scaling-manager` trigger). The
uninstall does not touch ScaledObjects it did not create, and a trigger that
calls a removed scaler fails on every KEDA poll.

<!-- guide:cleanup.uninstall start -->
```bash
make undeploy-scaling-manager
```
<!-- guide:cleanup.uninstall end -->

It removes the ScaledObjects it created, and KEDA returns each workload to its
earlier replica count. ScaledObjects you adopted are left and listed by name.

## Details

### A new plan offers adopt

Once the scaling manager's trigger sits on a ScaledObject, `make
scaledobjects-plan` writes `apply: adopt` for that workload. Adopt replaces
the ScaledObject's triggers with the scaling manager's alone, so applying it
drops yours. Set it to `no` until you hand the model over, as in [step
2](#2-hand-the-model-over).

### A scaler without TLS

The trigger in step 3 is the TLS form, the install's default. If
`kubectl get clustertriggerauthentication scaling-manager-external-scaler-${CONTROLLER_NAMESPACE}`
answers NotFound, the scaler is plaintext: use
`scaling-manager-external-scaler.${CONTROLLER_NAMESPACE}.svc.cluster.local:9090`
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
count is computed from what is left. Read its `desired` as "what it would get
after the models that scale".

### Already installed and active

`make deploy-scaling-manager LLMSM_PASSIVE=true` on a running install restarts
the controller passive. A re-install without `LLMSM_PASSIVE` keeps the value
the controller has. Editing `LLMSM_PASSIVE` in `scaling-manager-manager-config`
by hand does nothing until a restart: the controller logs `restart required`.

### Passive for some models only

The `passive` key works at every level of the scaling policy: the `default`
entry, a tier, or one model, the most specific winning. An active controller
with one `passive: true` entry observes that model alone. See [the scaling
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
