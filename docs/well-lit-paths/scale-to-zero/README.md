# Scale to zero, and get back

A model that sits idle gives its accelerators back entirely, and comes back
when a request arrives. For models with real idle windows: nights, weekends,
a dev namespace.

**Maturity:** Stable.

## When not to take it

- The first request of a burst must be answered faster than a replica loads.
  Take the [warm pool path](../warm-pool-bridge/) instead, or both.
- You cannot confirm the running EPP (the llm-d request router) has flow
  control on. Without it, a parked model never wakes.

## Start here

You need [the saturation path](../scale-on-saturation/#start-here) in place and
a model that has served at least one request. Every step is in
[Scale a model to zero, and get it back](../../guides/scale-to-zero/).

1. Namespace owner: [check the wake signal](../../guides/scale-to-zero/README.md#prerequisites) before anything parks.
2. Whoever owns the policy ConfigMap (the namespace owner by default, the admin in `llmsm-<ns>`): [allow parking in the policy](../../guides/scale-to-zero/README.md#installation-instructions).
3. Namespace owner: let every variant of the model reach zero (`minReplicaCount: 0` on each ScaledObject), as the same guide section shows.
4. [Verify the round trip](../../guides/scale-to-zero/README.md#verification): the model parks, then one request wakes it.

To undo, see [Cleanup](../../guides/scale-to-zero/README.md#cleanup). If the
controller is out of your reach, you can still allow parking yourself
([details](#details-the-policy-when-the-controller-is-out-of-reach)).

## What it costs

- Nothing while running, and the accelerators are really released.
- The first request after an idle window waits for a replica to load.
- A model can park and then fail to wake ([the one risk](#the-one-risk-worth-stating-first)).

## Evidence

No benchmark run. The end-to-end specs under
[How it is tested](#how-it-is-tested) cover parking and waking, but only when
switched on.

## Details

### The one risk worth stating first

**A cluster can park a model perfectly and be unable to wake it**, and nothing
about the park looks wrong. Verify the round trip, never just the park.

Two separate parts do the work. **Parking** is the scaling manager's: a model
that serves nothing for `retentionPeriod` goes to zero. **Waking** is the EPP's
flow-control queue plus KEDA: a request queued for a model with no endpoints
brings it back. The EPP reads its config only at startup, so editing its
ConfigMap does not reach a running pod. The guide's first check therefore asks
the EPP process what it *parsed*.

### What it needs

- The `flowControl` feature gate parsed by the running EPP pod.
- Parking allowed for the model **and** `minReplicaCount: 0` on every variant.
  One without the other is valid and quietly does nothing; the scaling manager
  reports which half is missing. `llmsm_model_scaling_blocked` carries a `reason`
  whenever something prevents a scale action: look there first.
- A `retentionPeriod` longer than one optimization cycle. A parking pass faster
  than `retentionPeriod` is stale state, not success.
- Judge it from the ScaledObject's `minReplicaCount`, never from the HPA KEDA
  creates, which always reads `minReplicas: 1`.

### How it is tested

- End-to-end: `test/e2e/scale_to_zero_test.go`,
  `scale_to_zero_roundtrip_test.go`, `scale_to_zero_sglang_test.go`,
  `scale_from_zero_test.go`, `scale_from_zero_capacity_test.go`.
- **These specs skip unless `SCALE_TO_ZERO_ENABLED=true`**, which defaults to
  false in `test/e2e/config.go`. A skipped spec reports as a pass, so check the
  spec count.

`retentionPeriod` and the other settings are in the
[scaling policy reference](../../reference/scaling-policy.md).

### Details: the policy when the controller is out of reach

When the controller runs in `llmsm-<ns>`, the tenant can allow parking without
the admin. A
[namespace-local `scaling-manager-scaling-policy-config`](../../reference/scaling-policy.md#namespace-local-scaling-policy-map)
in the model's namespace is read with no label. It replaces the controller's
map whole for that namespace, so its `default` must carry every threshold it
wants. The admin's own edit in `llmsm-<ns>` takes effect only after
`kubectl rollout restart deploy/scaling-manager-controller-manager -n llmsm-<ns>`, because
that controller reads its ConfigMap at start
([why](../../reference/scaling-policy.md#limiters-cluster-default-only-live)).
