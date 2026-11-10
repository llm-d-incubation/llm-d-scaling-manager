# ScaledObject trigger metadata

A model is registered with the scaling manager by a KEDA `ScaledObject` with a
`type: external-push` trigger that points at the scaling manager's external
scaler. KEDA passes the trigger's `metadata` map on every call. The keys on this
page are the whole per-workload configuration: there are no annotations, and
nothing else on the ScaledObject or the workload is read for these values.

The keys are parsed in `internal/registry/scalermeta.go`. KEDA metadata is a map
of strings, so quote every value (`"10.0"`, `"6000"`).

## A complete example

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: qwen-decode-llmsm
  namespace: my-models
spec:
  scaleTargetRef:
    apiVersion: apps/v1                      # leaderworkerset.x-k8s.io/v1 for an LWS
    kind: Deployment                         # or LeaderWorkerSet
    name: qwen-decode
  pollingInterval: 5
  cooldownPeriod: 30
  minReplicaCount: 1
  maxReplicaCount: 10
  advanced:
    restoreToOriginalReplicaCount: true
    horizontalPodAutoscalerConfig:
      behavior:                              # see "Scaling behavior" below
        scaleUp:
          stabilizationWindowSeconds: 0
          policies:
            - type: Percent
              value: 100
              periodSeconds: 5
        scaleDown:
          stabilizationWindowSeconds: 300
          policies:
            - type: Percent
              value: 100
              periodSeconds: 120
  triggers:
    - type: external-push
      name: scaling-manager-external-scaler
      metricType: AverageValue               # the default; never Value, see "Before the first decision"
      metadata:
        scalerAddress: scaling-manager-external-scaler.<controller-namespace>.svc:9090
        modelID: "Qwen/Qwen3-32B"            # = the engine's --served-model-name
        variantCost: "10.0"
        # scalingPolicy: "interactive"       # only once that tier exists in the ConfigMap
        defaultOutputTokens: "6000"          # long generations; seed only
      authenticationRef:      # TLS, the default. Plaintext install: ...svc.cluster.local:9090 and no authenticationRef
        name: scaling-manager-external-scaler-<controller-namespace>
        kind: ClusterTriggerAuthentication
```

## Model keys

These appear on a ScaledObject that scales a model server.

| Key | Required | Default | Validation | Read by | What it does |
| --- | --- | --- | --- | --- | --- |
| `modelID` | yes, unless `warmPoolName` is set | — | non-empty | every engine; it is the grouping key | Names the model this workload serves. Must equal the engine's `--served-model-name` (else `--model`), because engine metrics are matched on `model_name`. ScaledObjects with the same `modelID` in one namespace are variants of one model and are sized against each other. |
| `variantCost` | no | `"10.0"` | a number, `>= 0` | cost-aware optimizer; scale-from-zero variant choice | Price of **one replica** of this variant, relative to the model's other variants: GPUs per replica × relative GPU price. On scale-up the optimizer grows the variant with the lowest cost per unit of capacity; on scale-down it sheds the most expensive variant first. With one variant per model it changes nothing. See [accelerator variants](../well-lit-paths/accelerator-variants/). |
| `scalingPolicy` | no | the `default` entry's `defaultPolicy`, else the `default` entry | any string | steady-state engine policy resolution | Names a policy **tier** in the scaling-policy ConfigMap. A name with no matching entry falls back to the default entry and logs `Scaling policy not found; this variant is scaling under the default entry.` Variants of one model naming different tiers: the lexicographically smallest wins. See [tiers](../well-lit-paths/workload-classes/#choosing-a-tier). |
| `defaultOutputTokens` | no | built-in 512 | whole number `>= 0`; `"0"` means unset | steady-state engine (demand floor) | Seeds how many tokens one request generates, so a router queue is priced before any replica has completed a generation. Replaced by the fleet's own measurement as soon as one exists, and also outranked by the output length the fleet last recorded, which is carried across a gap in readings: the seed is used only when the fleet has neither. Across a model's variants the **largest** wins (set it on the decode trigger under P/D). See [expected output length](scaling-policy.md#expected-output-length). |
| `warmPool` | no | the namespace's only pool | any string | warm-pool controller | Which warm pool this model may borrow from. Needed only when the namespace has more than one pool; with several pools and none named, the model gets no warm copy and the controller logs why. |
| `warmPoolCopies` | no | absent = automatic | whole number `>= 0` | warm-pool controller | How many warm copies of this model the pool holds. Absent: the pool ranks it and holds at most one. `"0"`: never warm it. `"1"`: always keep one. `"N"`: keep N, so N scale-ups can bridge at once. |
| `replicaStartSeconds` | no | built-in 70 s | a plain decimal number of seconds from `1` to `3600` (`"120"`, `"15.5"`, `"1e2"`); any other value is ignored, never rejected | saturation analyzer (demand floor, utilization-share pacing) | The seed for how long a replica takes to become Ready. Until a replica of the variant has started, the throughput floor projects the backlog over it and utilization-share pacing uses it. For the first three pods the start estimate blends it with the measured starts, staying between half and twice the seed. Sizing never uses a start shorter than a measured one. From the third pod on, the measurement alone is used ([cycle log](cycle-log.md#replica-start-measured-replica-start-outlier-rejected-and-replica-start-change-confirmed)). A seed the measured starts put outside half to twice it logs `A variant's replicaStartSeconds is contradicted by its measured starts`. A seed under 5 minutes makes utilization-share pacing faster than its default. Any other value, including one with spaces, a unit (`"120s"`) or hex, is treated as unset and logs `A variant's replicaStartSeconds was ignored` once each time the value changes, with the value quoted and cut at 64 characters. The value is read only while the model has replicas reporting metrics, so a model scaled to zero logs nothing. The trigger is still accepted. The controller log is the only place a bad seed shows: there is no metric and no Kubernetes event. To find one, search the controller's log for the message, for example `kubectl logs -n <controller-namespace> deploy/scaling-manager-controller-manager \| grep "replicaStartSeconds was ignored"`. |

`scalerAddress` is KEDA's own key (where to dial); the scaling manager never
reads it. On a TLS install (the default) its value is
`scaling-manager-external-scaler.<controller-namespace>.svc:9090`, and the trigger carries
an `authenticationRef` to ClusterTriggerAuthentication
`scaling-manager-external-scaler-<controller-namespace>`. On a plaintext install it is
`scaling-manager-external-scaler.<controller-namespace>.svc.cluster.local:9090`, with no
`authenticationRef`. See [external scaler mutual TLS](install-options.md#external-scaler-mutual-tls).

**A bad value freezes the model at its current count.** If `modelID` is empty,
or `variantCost`, `warmPoolCopies` or `defaultOutputTokens` does not parse, the
workload is skipped (`internal/utils/variant.go`). The scaling manager never
plans it, so `GetMetrics` keeps answering the target's current replica count
([before the first decision](#before-the-first-decision-and-the-fallback)). The
HPA holds whatever count the workload had, under any load, for as long as the
typo stands.

KEDA shows nothing on the ScaledObject. Three things do:

- the gauge `llmsm_scaler_undecided_targets` stays above 0 for the namespace;
- the counter `llmsm_scaler_undecided_answers_total{reason="not-managed"}` keeps
  growing;
- the controller logs `external scaler has no decision for this target` once
  for that ScaledObject, with `reason` `not-managed`, at the default verbosity.

The cause is in a log line at verbosity 4, which the default (2) hides. Add
`--v=4` to the controller Deployment's `args` (see
[advanced settings](configuration-advanced.md#controller)) and look for
`Skipping a registered workload with unusable trigger metadata`: the
ScaledObject is in `scaledObject`, the key and value in `error`. If a registered
model never gets a decision, check these values first.

## Warm-pool keys

These appear only on the ScaledObject that scales a warm **pool** Deployment.
`warmPoolName` marks the trigger as a pool's; with it set, no `modelID` is
needed and the model keys above are ignored. `deploy/warmpool.sh create` writes
these for you; an unset key keeps the controller's flag default. Any value that
does not parse makes the whole pool trigger refused rather than defaulted.

| Key | Validation | Written by `warmpool.sh create` | What it does |
| --- | --- | --- | --- |
| `warmPoolName` | non-empty | always | Declares this ScaledObject as warm pool `<name>`. |
| `warmPoolSleepMinSize` | whole number `>= 0` | `--reserve` (default 1) | Free pods kept in reserve. `maxReplicaCount` must exceed it, or the pool can never warm anything. |
| `warmPoolMaxHold` | positive duration (`5m`) | `--max-hold` | Longest a bridge pod is lent before it is reclaimed. |
| `warmPoolMinHold` | duration `>= 0` (`90s`) | `--min-hold` | Shortest a bridge pod is lent, so it is not returned the instant the new replica is Ready with a cold cache. Capped by `warmPoolMaxHold`. |
| `warmPoolRetained` | `true`/`false` | `--type retained` / `bridge` | Makes the pool serving capacity rather than a bridge: no hold timeout. See [retained pool](../well-lit-paths/retained-pool/). |
| `warmPoolSwitchSpareThreshold` | percent, 0–100 | no | Retained pool only: spare capacity, as a percent of a model's supply, below which the pool switches to that model. |
| `warmPoolMinSwitchInterval` | duration `>= 0` | no | Retained pool only: minimum time between two switches. Default `10m`. |
| `warmPoolPreloadTop` | whole number `>= 0` | no | Warm this many of the busiest models without waiting for each to miss first. |
| `warmPoolGPUMemoryUtilization` | fraction, `0 < x <= 1` | no | Overrides `--gpu-memory-utilization` for warm copies: less KV cache, more models per pod. |

Sizing these is covered in [the warm-pool guide](../guides/warm-pool/sizing.md).

## How the keys get onto the ScaledObject

`make scaledobjects-plan` writes an editable plan, one entry per model server,
and `make scaledobjects-apply` turns each `apply: yes` entry into a ScaledObject
(`deploy/lib/scaledobject.sh`). After applying, it checks that KEDA put the
scaler's metric on the HPA of every ScaledObject naming this scaler, including
hand-written and GitOps-owned ones. Any that KEDA left on the CPU default it
re-wires the way `make scaledobjects-rewire` does, and it names any that stay
unwired. The re-wire sets the trigger key `scalingManagerRewire`, which a GitOps tool
shows as drift.

The plan carries `modelID`, `minReplicas`, `maxReplicas`, `variantCost` and
`scalingPolicy` (commented out unless set);
the generated trigger has `scalerAddress`, `modelID`, `variantCost` and, when
set, `scalingPolicy`. The other keys are not in the plan. Set them in one of
two ways:

- **A template for every model.** `LLMSM_DEFAULT_SO_TEMPLATE=<file>` replaces the
  generated shape. It substitutes `{{NAMESPACE}} {{NAME}} {{KIND}}
  {{APIVERSION}} {{MODEL_ID}} {{SCALER_ADDRESS}} {{MIN}} {{MAX}}
  {{VARIANT_COST}} {{SCALING_POLICY}}`; anything else in the file is copied
  as written. The sample is
  `config/samples/keda/external-scaler/scaledobject-template.yaml`.

  ```bash
  make scaledobjects-apply LLMSM_DEFAULT_SO_PLAN=scaling-manager-plan.yaml LLMSM_DEFAULT_SO_TEMPLATE=$PWD/my-template.yaml
  ```

- **One model, after the fact.** Patch the trigger:

  ```bash
  kubectl patch scaledobject <name> -n <ns> --type=json -p \
    '[{"op":"add","path":"/spec/triggers/0/metadata/defaultOutputTokens","value":"6000"}]'
  ```

  Re-running `scaledobjects-plan` and `scaledobjects-apply` keeps it.
  `apply: yes` skips a workload that already has a ScaledObject. `apply: adopt`
  writes the plan's `scalerAddress`, `modelID`, `variantCost` and
  `scalingPolicy`, and carries over every other key of a trigger that already
  points at the scaling manager, such as `defaultOutputTokens` or `warmPool`. For
  such a trigger the plan also proposes the `modelID` it already has. Adopting
  from a different trigger (`prometheus`, `cpu`) carries nothing over.

## Scaling behavior

The generator writes this HPA behavior, and it is the recommended shape for any
ScaledObject that points at the scaling manager, hand-written or adopted:

| | stabilization | policy |
| --- | --- | --- |
| scale-up | 0 s | `Percent 100` every 5 s |
| scale-down | 300 s | `Percent 100` every 120 s |

Scale-up acts on the current answer: a new replica already pays its start time.
Scale-down waits for 300 s of lower answers. After a restart or leader change
the window also guards the new leader's first decisions, which rest on a short
history. Removing a replica too early costs a cold start; keeping one too long
only costs money.

Override the defaults per install with
`LLMSM_SO_SCALE_{UP,DOWN}_{PERIOD,STABILIZATION}`, or replace the shape with
`LLMSM_DEFAULT_SO_TEMPLATE`. `apply: adopt` keeps the existing ScaledObject's
`advanced` block, so an adopted object keeps whatever behavior it had; check it.

### Before the first decision, and the fallback

After every controller restart and every leader change, the new leader has no
decision until its first cycle completes. **Until then it answers KEDA's
`GetMetrics` with the scale target's current replica count**, read from the
target's `scale` subresource, the same place KEDA and the HPA read it, so a
custom resource whose replica count lives somewhere other than `spec.replicas`
is read correctly. With the metric's target of 1 the HPA computes the
count it already has and holds: nothing scales down on a number nobody decided,
and KEDA sees no error. Each such answer increments
`llmsm_scaler_undecided_answers_total{reason="no-decision"}`, and the ScaledObject
counts in `llmsm_scaler_undecided_targets` until its first decided answer.
`IsActive` and the push stream behave the same
way: before the first decision they report the target active if it has
replicas. Only when the current count cannot be read does `GetMetrics` fail,
with the code of the failed read. A target kind with no `scale` subresource
cannot be held this way and fails.

A decision that stops being republished is treated the same way. A positive
decision older than 60 s, or three optimization intervals if that is longer, is
**stale**: `IsActive` stops honouring it and `GetMetrics` answers the current
count instead (`reason="stale"`), so the HPA holds rather than following a
number no engine stands behind.

This holds only with the trigger's `metricType` at its default,
`AverageValue`. The scaler answers a replica count, and with `AverageValue` the
HPA computes `ceil(value / 1)`, the count itself. With `metricType: Value` it
computes `ceil(current replicas × value)`, so a decision of 3 on 3 replicas
asks for 9 and a held count squares. The installer writes
`metricType: AverageValue` explicitly on the triggers it generates, including
when it adopts a ScaledObject (adoption replaces the triggers), and warns about
a `Value` trigger when it renders a ScaledObject template.

A standby replica, one that does not hold the leader lease, still refuses
`GetMetrics` and `IsActive` with `Unavailable`: the leader may have decided
something else. KEDA records a trigger error and takes no scaling action, and
its connection moves to the leader within seconds.

The generator writes a KEDA `fallback` into every ScaledObject with a
`minReplicaCount` of 1 or more:

```yaml
fallback:
  failureThreshold: 3              # LLMSM_SO_FALLBACK_FAILURES
  replicas: <minReplicaCount>
  behavior: currentReplicasIfHigher
```

It covers a controller that is down or unreachable, which the scaler cannot
answer for. Each failed read of the metric by the HPA, about one every 15 s,
counts as a failure. Once more than `failureThreshold` reads have failed in a
row, KEDA stops passing the error on and feeds the HPA
`max(current replicas, replicas)` instead. With the default of 3 that is the
fourth failed read, about a minute in. Since `replicas` is the object's own
`minReplicaCount`, that is the current count: the fallback never scales anything
up or down. What it changes is that the HPA keeps recording a recommendation
while the scaler is away, so the 300 s scale-down window still covers the
moment the scaler returns, and the ScaledObject's `Fallback` condition says it
is in effect. A fleet does not shed replicas on the first answer after an
outage longer than the window.

**A `minReplicaCount` of 0 gets no fallback.** KEDA treats an erroring trigger
as inactive, and with `fallback.replicas: 0` its scale loop takes the
scale-to-zero path: a model that is serving is parked once `cooldownPeriod`
passes, with no `failureThreshold` in the way. With no `fallback` the same
error only marks the trigger failing and scales nothing, so a scale-to-zero
model is safer without one. The plan table and the apply lines show
`(no fallback: floor 0)` for those entries.

`behavior` needs KEDA 2.17 or later (the installer pins 2.21). Once per plan or
apply the installer asks whether the ScaledObject schema has the field, first
with `kubectl explain scaledobjects.spec.fallback.behavior`, which reads the
published OpenAPI and works for any authenticated user, and then, if that
cannot answer, by reading the ScaledObject CRD. It writes no fallback at all,
with a warning, when the field is missing, shown as
`(no fallback: KEDA < 2.17)`, or when neither read answers, shown as
`(no fallback: KEDA version unknown)`. The warning says when the CRD read was
`Forbidden`. An older KEDA drops `behavior`, and the static fallback left behind
would force `minReplicaCount` whatever the fleet is running. A tenant install
normally gets its answer from the OpenAPI, and so a fallback; if its identity
cannot read that either, its ScaledObjects get none until it is run as one that
can, or a template supplies one. A plan whose every entry renders from
`LLMSM_DEFAULT_SO_TEMPLATE` writes no fallback and is not probed.

`apply: adopt` adds the same fallback to an adopted ScaledObject that has none,
and leaves one that has a fallback exactly as it is, with a warning if that
fallback is the zero-replica shape on a floor of 0, or if its `replicas` is
above the `minReplicaCount` the plan sets (KEDA holds at least that many during
an outage). The fallback is not a trigger key, so keeping the trigger's
hand-set keys does not touch it. An object the installer cannot read is not
patched at all. A
`LLMSM_DEFAULT_SO_TEMPLATE` owns its own fallback; the shipped sample has none,
and the installer refuses to render a template whose ScaledObject document has
a fallback that comes out as `replicas: 0` on a floor of 0, or that `yq` cannot
read.
