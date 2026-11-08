# Several pools, and what each holds

[← Warm pool guide](README.md)

## More than one pool

A warm copy is reusable only on the accelerator it was loaded on. A cluster with
two GPU models needs **two pools**, and a variant that needs several devices
needs a pool whose Pods hold that many. `plan` tells you which pools the
namespace wants and which models can share each.

Each extra pool is another `create`, with its own name and accelerator:

```bash
# Create a second pool for H100 models
deploy/warmpool.sh create -n <namespace> --name h100   --accelerator NVIDIA-H100-80GB-HBM3 --gpus 1   --models 4 --model-size 8B   --scaling-manager-namespace <where the scaling manager runs>
```

Copying the manifests by hand instead? Change the pool name in **all three**
places, plus the object's own name:

```yaml
metadata:
  name: llmsm-warm-pool-h100        # must differ from the first pool's
  labels:
    llm-d.ai/warm-pool: h100      # the pool's name
spec:
  selector:
    matchLabels:
      llm-d.ai/warm-pool: h100    # or both Deployments select the same Pods
  template:
    metadata:
      labels:
        llm-d.ai/warm-pool: h100  # must match
```

Missing the **selector** bites quietly: two Deployments with identical selectors
fight over one set of Pods, and neither replica count means anything afterwards.

Then give it its own ScaledObject. **This is what makes it a pool**: its
`warmPoolName` must match the label above and its `scaleTargetRef` must name this
Deployment. A copied Deployment with no trigger holds accelerators nothing will
use, and the scaling manager reports it as undeclared.

Then each model names the pool it may borrow from, in its trigger metadata:

```yaml
triggers:
  - type: external-push
    metadata:
      modelID: <model>
      warmPool: h100
```

**With one pool you write none of this**: a ScaledObject that says nothing still
gets a warm copy. With more than one pool the key is required. The scaling
manager will not guess, because a wrong guess spends a full model load on a copy
that can never serve; it names the variant and the pools that exist instead.

The scaling manager also declines a model whose accelerator does not match the
pool's, or that needs more devices than a Pod holds, once per reason:

```
warm pool will never warm this model; it is pointed at a pool that cannot hold it {"variant":"...","reason":"needs 4 GPUs, this Pod holds 1"}
```

A pool created without `--accelerator` has no `nodeSelector`, so its accelerator
is whatever node its Pods land on: the same mismatch, by accident.

## Choosing what stays warm

By default the pool decides: parked models first, then the busiest, then
anything that has missed twice. That is right for almost everything.

Two things it cannot work out, both set per model in the trigger metadata:

```yaml
triggers:
  - type: external-push
    metadata:
      modelID: <model>
      warmPoolCopies: "2"
```

| value | means |
| --- | --- |
| *absent* | automatic — the pool ranks it, and holds at most one copy |
| `"0"` | never warm this model — and release it if it already is |
| `"1"` | always keep one warm, whatever the ranking thinks |
| `"N"` | keep N warm, so N scale-ups of this model can bridge **at once** |

- **`"1"` is not the same as absent.** Absent lets a quiet model lose its slot
  to a busier one; `"1"` pins it. A low-traffic but latency-critical model needs
  that, and popularity ranking never gives it.
- **`"N"` covers simultaneous scale-ups.** One warm copy bridges one scale-up;
  the second goes cold beside free Pods. It is also the only way to weight a
  shared pool toward one model, since automatic mode holds one copy of each.
- Copies always land in different Pods. A second copy in the same Pod would
  share the first's accelerators and could never serve a second bridge.
- Lowering the number releases the excess, oldest first, so it means exactly that
  many, not "at least". A copy that is BRIDGING is never released: it serves live
  traffic, and `max-hold` already returns it. Automatic mode never releases
  anything.

## Retained pools: which model holds the GPUs

A retained pool *is* the serving capacity: its models are too large to start on
demand, so no ordinary replicas come and exactly one model is awake at a time.
Something must decide which, or the model that woke first keeps the accelerators
however the load moves.

Choose retention at create, so the pool is never a bridge in between:

```bash
# Create a retained pool
deploy/warmpool.sh create -n <ns> --name <pool> --type retained ...
```

The two switch knobs are trigger metadata on the pool's ScaledObject, beside the
`warmPoolRetained: "true"` that `--type retained` writes:

```yaml
warmPoolSwitchSpareThreshold: "20"    # percent
warmPoolMinSwitchInterval: "10m"      # default 10m
```

The rule, in order:

1. **A candidate must be under more pressure than the awake model.** Either it
   is below the spare threshold and the awake one is not, or the optimizer wants
   to scale it up and not the awake one.
2. **Equal pressure is not enough.** If the awake model is *also* below the
   threshold, or *also* needs to scale up, nothing moves: switching would just
   move the shortage and pay a drain and a wake. The pool cannot make two models
   comfortable at once.
3. **Ties go to the tightest.** At equal urgency, the candidate with least spare
   capacity wins.
4. **No switch within `warmPoolMinSwitchInterval` of the last one, unless the
   candidate needs to scale up.** One short of capacity *now* cannot wait, and no
   replicas are coming. This is safe because of rule 2: if both models are
   growing, the candidate never gets this far, so the pool cannot trade its GPUs
   back and forth.

Spare is a **fraction of each model's own supply**, so one threshold compares
models of different sizes. A model the optimizer has not measured is neither a
candidate nor a reason to stay put. Leave `warmPoolSwitchSpareThreshold` unset
and only the scale-up rule applies.

The controller logs `retained pool is switching the model it holds awake`, with
the model, the reason and the interval. A decision *not* to switch is logged at
debug, with the reason.
