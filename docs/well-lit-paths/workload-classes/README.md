# Give classes of workloads different scaling behaviour

A **tier** is a named, reusable policy entry (`interactive`, `standard`,
`batch`) that a model selects by name. One tier serves many models, so
retuning the tier retunes all of them at once. For anyone whose models fall
into a few classes of urgency, such as an interactive assistant and an
overnight batch job.

**Maturity:** Stable.

## When not to take it

- One model really needs its own numbers. Use a per-model override, which wins
  over any tier. Reach for it deliberately, not as the way to configure
  everything.

## Start here

You need [the saturation path](../scale-on-saturation/#start-here) in place,
and edit rights on a `scaling-manager-scaling-policy-config` the controller reads. No
guide covers this path, so the steps are here.

1. Owner of that ConfigMap: add the tier entries and a `defaultPolicy` naming one of them ([example](#what-it-needs)). No tier exists until you add it.
2. Namespace owner: name a tier on each variant with `scalingPolicy: <tier>` in its trigger metadata ([example](#what-it-needs)).
3. Check it works: the controller log line `Effective scaling policy` names the tier; out of your reach, the log is the admin's ([Verifying it](#verifying-it)).

To undo, remove `scalingPolicy` from the trigger; the variant scales under
the `default` entry again, with no restart. Controller out of your reach? See
[Details](#where-the-policy-lives).

## What it costs

- Nothing to run. The tiers themselves trade money for latency: a lower
  `scaleUpThreshold` holds more idle KV cache, a higher one queues more
  requests while a replica starts.
- Under a binding GPU budget, a low-`priority` tier gets fewer replicas than
  its demand asks, paid in queueing and TTFT (time to first token). Priority
  is the weight in the fair-share split (`fairShareValue` in
  `internal/engines/allocation/`), so it is what a class is worth under
  contention ([bound a fleet by real GPUs](../bound-by-gpus/)).
- A naming decision. A tier promises that every model wearing it wants the
  same behaviour. Once that stops being true, somebody adds a per-model
  override and the class stops meaning anything.

## Evidence

No benchmark run. One end-to-end suite covers the behaviour, on the simulator
only ([How it is tested](#how-it-is-tested)).

## Details

### What it needs

Tier entries in the scaling-policy ConfigMap. An entry **is** a tier when its
key is not `default` and its body sets no `model_id`:

```yaml
data:
  default: |
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
    priority: 1.0
    defaultPolicy: standard      # what a variant naming no tier gets
  standard: |                    # must exist: defaultPolicy names it
    priority: 1.0
  interactive: |
    scaleUpThreshold: 0.60
    scaleDownBoundary: 0.45
    priority: 2.0
  batch: |
    scaleUpThreshold: 0.95
    priority: 0.5
```

**`defaultPolicy` must name a tier that exists.** A `defaultPolicy` naming no
entry is never reported: every variant that names no tier silently scales
under the bare `default` entry. Here `standard` repeats the default numbers,
so it is only a place to retune the unnamed fleet later. Delete it and
`defaultPolicy` together, never one without the other.

A `scalingPolicy` key in each variant's KEDA trigger metadata selects the
tier. The trigger is already the registration, so nothing is watched, listed
or matched. Set it on the trigger, or in the variant's entry in
[the ScaledObject plan](../../reference/configuration.md#default-scaledobjects)
before `make scaledobjects-apply`:

```yaml
triggers:
  - type: external-push
    metadata:
      modelID: ibm/granite-13b
      scalingPolicy: interactive
```

### Where the policy lives

In the default install, the ConfigMap is in the controller's namespace. The
policy is read live, with no restart. If the controller is out of your reach,
create a
[namespace-local one](../../reference/scaling-policy.md#namespace-local-scaling-policy-map)
in your namespace; it replaces the global map whole for your models and is
also read live. Or ask the admin, who must restart the controller after
editing its own ConfigMap: run out of the tenant's reach (`LLMSM_WATCH_NS`), it
reads that ConfigMap only at start
([why](../../reference/scaling-policy.md#limiters-cluster-default-only-live)).

Re-running `make deploy-scaling-manager` resets the `default` entry of the controller's
own ConfigMap, and `defaultPolicy` with it: re-apply `defaultPolicy`
afterwards. The tier entries are other keys, and the installer keeps them
([why](../../reference/scaling-policy.md#re-running-the-installer-resets-the-default-entry)).
The full field list is in [the scaling policy reference](../../reference/scaling-policy.md).

### Choosing a tier

**No tier exists until you add it.** The shipped policy ConfigMap
(`config/base/manager/scaling-policy-configmap.yaml`) carries `interactive` and
`batch` only as commented-out examples, and `deploy/configmap-scaling-policy.yaml`
carries none. Until the entry is in the ConfigMap, `scalingPolicy: interactive`
only logs `Scaling policy not found` and scales under the default entry. To
use one, copy it out of the comment into `data:` of the ConfigMap. The
shipped examples, against the default entry:

| | `default` (shipped) | `interactive` (example) | `batch` (example) |
| --- | --- | --- | --- |
| `scaleUpThreshold` | 0.85 | 0.60 | 0.95 |
| `scaleDownBoundary` | 0.70 | 0.45 | inherits 0.70 |
| `priority` | 1.0 | 2.0 | 0.5 |

What the scale-up threshold buys:

- **Lower** (`interactive`): the model scales out earlier, so a new replica
  starts while the others still have room. You pay in idle KV cache: more
  replicas, all less full. Lower `scaleDownBoundary` with it, or the band
  collapses and the model scales down as soon as it scaled up.
- **Higher** (`batch`): replicas are packed harder before another is ordered.
  Fewer GPUs for the same traffic, at the price of queueing and TTFT while a
  replica starts.

Utilization is measured against `kvCacheThreshold`-scaled capacity, so the
real scale-out point is `kvCacheThreshold × scaleUpThreshold` of physical KV:
0.80 × 0.85 = 0.68 with the defaults, 0.48 for the `interactive` example.

A tier can also carry `scaleToZero`, so a class decides whether its models may
park. A model parks only if every tier its variants name allows it. It then
waits for the longest `retentionPeriod` among those tiers. A per-model entry
still overrides the tier
([scale to zero](../scale-to-zero/#start-here)).

### How a variant's policy is resolved

Most specific wins: **per-model override → named tier →
`default.defaultPolicy` → `default`**. `defaultPolicy` is read from the
`default` entry only, because a fallback chosen by a tier would be that tier
choosing for everyone but itself.

- **A tier that does not exist** falls back to `default`, and is logged
  against the variants that asked for it, listing the tiers that do exist.
  This covers a tier named in `scalingPolicy` only. A `defaultPolicy` naming a
  missing tier falls back the same way and is **not** logged
  (`resolveModelPolicy` in `internal/engines/steadystate/engine.go`).
- **Variants of one model naming different tiers** resolve to the
  lexicographically smallest, so the allocation is the same every cycle
  rather than flipping with map order.

### Verifying it

```bash
# which tier each model resolved to, and the thresholds it got (logged once per change)
kubectl logs -n <controller-namespace> deploy/<scaling-manager> | grep "Effective scaling policy"
# a tier that was named but does not exist
kubectl logs -n <controller-namespace> deploy/<scaling-manager> | grep "Scaling policy not found"
```

The log is the admin's when the controller is out of your reach; a tenant
watches `llmsm_desired_replicas{exported_namespace="<ns>"}` move at the tier's
thresholds. `Effective scaling policy` prints the tier (or `(default entry)`),
both thresholds, `kvCacheThreshold` and `priority`. For a variant naming no
tier it reads `(default entry)` whether or not `defaultPolicy` resolved, so
check the printed numbers. `Scaling policy not found` lists the tiers that
exist in `knownPolicies`. The per-cycle `analyzer-result` line has the
thresholds in force but **no policy name**.

### How it is tested

End-to-end: `test/e2e/scaling_policy_tier_test.go`. It covers a named tier
taking a workload where the default entry would not, fallback when the tier
does not exist, a per-model override beating the tier, and the tier's
thresholds resuming once that override is removed.

**The suite needs `USE_SIMULATOR=true`** and skips without it: it drives
`llm-d-inference-sim --fake-metrics`, which real vLLM rejects. A run without
the simulator is green while covering none of this.
