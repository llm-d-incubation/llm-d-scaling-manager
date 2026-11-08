# Scale a P/D-disaggregated model

llm-d can run prefill (reading the prompt) and decode (writing the answer) as
separate deployments. The scaling manager scales each role on its own
evidence, not on an average of both. For anyone already running P/D, or
evaluating it for long-context work on medium-to-large models.

**Maturity:** **Experimental.** A working recipe with open items
([what is short](#what-is-still-short)).

## When not to take it

- You have not measured that a single-role deployment is the problem. P/D adds
  a second deployment and a transport to operate.
- Your traffic is short-context. P/D buys little there.
- You have no high-bandwidth network (IB, RoCE, EFA) between the roles. NIXL
  runs over TCP, but the fast network is what makes the shape worth having.

## Start here

You need an llm-d P/D deployment with a KV transport between the roles, and
the prerequisites of [the saturation path](../scale-on-saturation/#start-here).
The namespace owner runs every step.

1. Deploy the P/D model with [llm-d's own P/D guide](https://github.com/llm-d/llm-d/tree/main/guides/pd-disaggregation), adding the EPP's `flowControl` feature gate, which that guide leaves off ([details](#what-it-needs)).
2. After the cluster admin's `make setup-prereqs`, [install the scaling manager](../../guides/install-in-namespace/) and [register](../../guides/install-in-namespace/README.md#4-register-the-workloads) both roles under one `modelID`.
3. For long generations, set `defaultOutputTokens` on the decode trigger ([how](#tuning-it)).
4. Check it works: `llmsm_desired_replicas` shows one series per role ([details](#verifying-it)).

Nothing to undo beyond [uninstalling](../../guides/install-in-namespace/README.md#cleanup),
which removes the ScaledObjects; the model servers keep serving.

## What it costs

- Two deployments to operate instead of one, plus the transport.
- No GPUs of the scaling manager's own. The measured run used 90.2 decode
  GPU-minutes ([measured.md](measured.md)).

## Evidence

One scenario on 8 x H200 nodes, Qwen3-0.6B, controller built at `f5f98761`
on 2026-09-23: a constant 6 req/s whose request shape changes halfway, so the
right answer is two decode replicas, then three. Run as a pair (a cold pass,
then a warm one), and the pair repeated a day later on the same build.

| | warm pass | cold pass |
|---|---|---|
| decode replicas, mean / max | 2.34 / 3 | 2.47 / 4 |
| time to first token p50 / p95 / p99 | 99 ms / **155 ms** / 229 ms | 99 ms / 204 ms / 3.9 s |
| requests / errors | 13,288 / 0 | 13,287 / 0 |

The repeat landed within a tenth of every figure, except the cold pass's
first five minutes, which ranged 0.25 s to 5.6 s. Every row, the graphs and
how to read them: [measured.md](measured.md).

## Details

### What is still short

The scenario stands up and runs, the role handling is covered end to end, and
the shape has the benchmark below. Still open:

- A shape the controller has never seen saturated is sized from occupancy
  until it has (the cold pass in [measured.md](measured.md)).
- There is no P/D-specific steady-state e2e spec.
- The benchmark never ordered a second prefill replica, so ordering both roles
  together (the joint commit) has run in unit tests only.

### What it needs

- Prefill and decode workloads with the role on each pod template as the label
  `llm-d.ai/role: prefill` or `llm-d.ai/role: decode`. On a LeaderWorkerSet
  the label is read from the **leader** template. A missing label, or any
  other value, means `both` (a workload that serves whole requests), so a typo
  silently turns a role into an ordinary variant. Engine flags
  (`--kv-transfer-config`, `--disaggregation-mode`) are not consulted. Check with
  `kubectl get deploy -n <ns> -o custom-columns=NAME:.metadata.name,ROLE:.spec.template.metadata.labels.llm-d\.ai/role`.
- A KV transport between the roles.
- `featureGates: [flowControl]` in the EPP's `EndpointPickerConfig`; the
  install refuses without it. No guide in this repository deploys P/D; the
  benchmark scenario below does, for a benchmark.
- A ScaledObject per role, each with the model's `modelID` in trigger
  metadata. `make scaledobjects-plan` lists the prefill and decode workloads.

### Verifying it

```promql
# each role carries its own target
llmsm_desired_replicas{exported_namespace="<ns>"}

# required capacity is reported per role for a P/D model
llmsm_required_capacity{exported_namespace="<ns>"}
```

The engine reports required and spare capacity per role, not one number for
the pair.

### Tuning it

**There are no per-role thresholds.** Both ScaledObjects carry one `modelID`,
so both roles resolve to one policy: the same tier, the same per-model
override (matched on `model_id` + `namespace`), and the same
`scaleUpThreshold` / `scaleDownBoundary`, applied to each role's capacity
separately. If the two triggers name different `scalingPolicy` tiers, the
lexicographically smallest wins and the conflict is logged: name the same tier
on both, or none. See [scaling policy](../../reference/scaling-policy.md).

What you set per role, on each ScaledObject
([trigger reference](../../reference/scaledobject-trigger.md)):

- **`defaultOutputTokens` on the decode trigger** when generations are long. A
  queued request is priced at that many tokens of decode work until the fleet
  has measured its own output length. The built-in 512 under-prices a
  6000-token workload twelvefold. If both triggers carry one, the largest wins.

  ```bash
  kubectl patch scaledobject <decode> -n <ns> --type=json -p '[{"op":"add","path":"/spec/triggers/0/metadata/defaultOutputTokens","value":"4000"}]'
  ```

  Re-running `make scaledobjects-plan` and `make scaledobjects-apply` keeps
  keys you set on the trigger.
- **`minReplicaCount` / `maxReplicaCount` per role.** Size each role's `max`
  from its own GPUs per replica and what the cluster can hold. A role pinned at
  its `max` stops growing while the other keeps going. Keep `min` at 1 or more
  on both roles unless you mean to park the model.
- **`scaleFromZero.requirePrefill`** (policy entry, default `false`). When a
  parked P/D model wakes and prefill cannot be placed, decode wakes alone and
  runs both stages itself. Set `requirePrefill: true` if serving without
  prefill is worse than not serving.

What the roles mean to the engine's capacity model:
[the steady-state engine](../../concepts/steady-state-engine.md).

### How it is tested

- End-to-end: `test/e2e/scale_from_zero_pd_test.go`, three specs, for
  scale-from-zero only: the role is read from the pod template, a queued
  request wakes **decode**, and prefill is **never woken alone**. Steady-state
  scaling rides on [the saturation path](../scale-on-saturation/)'s suites.
- Unit, in `internal/engines/analyzers/saturation/`: `throughput_floor_test.go`,
  `throughput_floor_total_test.go`, `downstream_saturation_test.go`,
  `start_role_test.go` and the P/D cases in `analyzer_test.go`
  ([what each holds](measured.md#what-the-unit-specs-hold)).
- The joint commit (growing both roles together) is covered only by
  `internal/engines/allocation/paired_exhausted_role_test.go`.

### Replica start time

Every ramp is sized by how long a role's next replica takes to become Ready,
so get the start path right first:
[Preparing a workload](../../reference/workload-preparation.md#the-rest-of-the-start-path)
([why it matters here](measured.md#replica-start-time)).
