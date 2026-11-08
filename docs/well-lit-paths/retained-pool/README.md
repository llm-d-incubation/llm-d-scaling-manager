# Hold several large models on one set of GPUs

Some models are too large to start on demand: by the time one loads, the
request that wanted it has timed out. A retained pool keeps several such
models loaded on one set of GPUs, one awake at a time, and switches to
whichever needs it most. For teams with more large models than GPUs to run them.

**Maturity:** **Experimental.** The rule that picks which model is awake is
covered by unit tests only, and no benchmark run is written up
([details](#how-it-is-tested)).

## When not to take it

- Two of the models are busy at the same time. The pool refuses to switch
  rather than move the shortage from one model to the other; the answer is more
  accelerators.
- Your models start fast enough for ordinary replicas to arrive. Take
  [the warm pool bridge](../warm-pool-bridge/) instead.

## Start here

You need [the saturation path](../scale-on-saturation/#start-here) in place,
GPUs for a pool Pod sized for the largest model, and the cache claim and
images [every pool needs](../../guides/warm-pool/install.md#prerequisites).
Every step is in [the warm pool guide](../../guides/warm-pool/); the namespace
owner runs all of them.

1. [Check the models can be warmed here](../../guides/warm-pool/README.md#should-this-model-be-warmed-here-at-all), and [how many fit in one Pod](../../guides/warm-pool/sizing.md#how-many-models-fit-in-one-pod).
2. [Create the pool as retained](../../guides/warm-pool/multi-pool.md#retained-pools-which-model-holds-the-gpus) and [point the models at it](../../guides/warm-pool/install.md#adding-and-removing-a-pool-later).
3. Optionally set the two switch settings on the pool's ScaledObject ([what they do](#the-settings)).
4. Check it works: the controller log shows `retained pool is switching` when demand moves ([details](#verifying-it)).

To undo, [remove the pool](../../guides/warm-pool/install.md#adding-and-removing-a-pool-later).

## What it costs

- The whole pool, all the time. Its accelerators serve one model at a time and
  nothing else.
- A drain and a wake whenever demand moves from one model to another. In
  return, N models run on the hardware for one.

## Evidence

No benchmark run is written up. What exists:

- Nine unit test files cover the switch rule.
- One end-to-end spec, on the simulator, covers a retained pool keeping the
  Pod it lent.
- It has been exercised on a cluster.
- Benchmark scenario: `hack/benchmark/scenarios/guides/variants/v2-retained-switch.yaml`.

## Details

### What it needs

- A pool sized for the largest model, matching one shape: GPUs per Pod, how
  they are split across Pods, and the accelerator type. A warm copy is only
  reusable on the accelerator it was loaded on.
- A memory limit that holds the whole warm set. A sleeping model's weights
  are charged to the Pod's memory, so one model too many OOM-kills the
  launcher and **every resident model** with it.
- The pool declared as retained at create, so it is never a bridge in between:

  ```bash
  deploy/warmpool.sh create -n <ns> --name <pool> --type retained \
    --models 2 --model-size 8B --accelerator <product> --scaling-manager-namespace <scaling-manager-ns>
  ```

  `--type bridge --max-hold 5m` is the other choice. The two together are
  refused, because retention is what switches the hold timeout off. Naming
  neither leaves the controller's default in force. A re-run of `create`
  without `--type` keeps the pool retained; `--type bridge` turns it back.

### The settings

They live in the trigger metadata of the pool's ScaledObject
(`kubectl edit scaledobject llmsm-warm-pool-<pool> -n <ns>`). A later
`deploy/warmpool.sh create` for the pool keeps them.

```yaml
warmPoolRetained: "true"              # what --type retained writes
warmPoolSwitchSpareThreshold: "20"    # percent; unset = switch only on scale-up need
warmPoolMinSwitchInterval: "10m"      # default 10m
```

### The rule that decides who is awake

In the order it is applied:

1. **A candidate must be under more pressure than the model awake**: below
   the spare threshold when the awake one is not, or needing to scale up when
   the awake one does not.
2. **Equal pressure is not enough.** If the awake model is *also* short,
   nothing moves.
3. **Ties go to the tightest**: least spare capacity wins.
4. **No switch within `warmPoolMinSwitchInterval`, unless the candidate needs
   to scale up now.** No replicas are coming to relieve a model short of
   capacity, so it cannot wait.

Spare is a fraction of each model's *own* supply, so one threshold compares
models of different sizes. An unmeasured model is neither a candidate nor a
reason to stay put.

### Verifying it

```bash
# which model the pool is holding awake, and why it moved
kubectl logs -n <controller-namespace> deploy/<scaling-manager> | grep "retained pool is switching"
```

The line names the model, the reason and the interval. A decision **not** to
switch is logged at debug with its reason, which helps more when nothing
happened. The pool's own state is in
[Checking it works](../../guides/warm-pool/operating.md#checking-it-works).

### How it is tested

- Unit: `internal/warmpool/policy/retained_test.go`,
  `policy/switch_test.go`, `policy/intent_test.go`, `retained_pass_test.go`,
  `retained_stay_log_test.go`, `switch_test.go`, `switch_storm_test.go`,
  `internal/decision/awake_test.go`, and
  `internal/engines/analyzers/saturation/warmpool_bridge_test.go`. They cover
  the pressure comparison, the tie-break, interval preemption, and the switch
  storm an earlier version of the rule produced.
- End-to-end: `test/e2e/warm_pool_retained_test.go` (`Warm pool - a retained
  pool does not churn its Pod`, labels `full` and `warmpool-retained`): the
  pool keeps the Pod it lent past the hold time. It needs the simulator
  (`--fake-metrics`) and skips without it.
- **Not covered end to end:** the switch itself (which model is awake, the
  tie-break, interval preemption).
- The benchmark harness's start-path settings are in
  [Replica start time in the harness](../../guides/benchmarking/README.md#replica-start-time-in-the-harness).

The design, including why a switch needs both a threshold and an interval, is
in [proposals/warm-pool-retained.md](../../proposals/warm-pool-retained.md).
