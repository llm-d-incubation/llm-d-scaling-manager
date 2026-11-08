# Serve one model on two accelerator variants

Deploy one model twice, on different hardware or different tensor-parallel
widths, and let the autoscaler choose which deployment grows. For anyone with
more than one accelerator type who would rather not split traffic by hand.

**Maturity:** Stable. No end-to-end spec asserts which variant grows
([How it is tested](#how-it-is-tested)).

## When not to take it

- The deployments serve different models. Grouping is by `modelID`; each
  model takes [the saturation path](../scale-on-saturation/) on its own.
- You cannot price the variants against each other. A wrong relative cost
  gives a confident decision in the wrong direction.

## Start here

You need [the saturation path](../scale-on-saturation/#start-here) in place,
and two deployments of the same model behind one InferencePool (the set of pods one router serves) and one EPP
(the llm-d request router). The namespace owner runs the steps.

1. [Plan the registration](../../guides/install-in-namespace/README.md#4-register-the-workloads) with `make scaledobjects-plan`, and give both entries the same `modelID`.
2. Set each entry's `variantCost` to the price of one replica ([what that means](#what-it-needs)), then run `make scaledobjects-apply`.
3. Check it works: `llmsm_desired_replicas{exported_namespace="<ns>"}` has one series per variant, and the cheaper one per unit of capacity grows first.

No guide stands up the second deployment;
[the two-variant benchmark](../../developer-guide/two-variant-scaling-manager-benchmark.md)
does, for a benchmark. Nothing to undo beyond
[uninstalling](../../guides/install-in-namespace/README.md#cleanup), which
removes the ScaledObjects.

## What it costs

- No accelerators of its own; the point is to use fewer.
- An accurate `variantCost` for each variant, kept up to date.

## Evidence

One benchmark run in July 2026, on an earlier build, with one GPU type and two
tensor-parallel sizes. Scenario:
`hack/benchmark/scenarios/guides/two-variant-scaling-manager.yaml`, with the variants
under `scenarios/guides/variants/`; the topology and run are in
[the two-variant benchmark](../../developer-guide/two-variant-scaling-manager-benchmark.md).

## Details

### How it chooses

Each deployment has its own ScaledObject, and both carry the same `modelID`,
so the scaling manager scales them as one group.

- **Scale-up** grows the variant with the **lowest cost per unit of
  capacity**: `variantCost` divided by what one replica serves. That is not
  the same as the cheapest replica.
- **Scale-down** removes replicas from the variant with the highest
  `variantCost` per replica first (ties broken by the analyzer's score, then by
  name). It keeps at least one replica on the cheapest variant while no more
  expensive variant holds any. So the variant grown first for efficiency can
  also be shed first, if its replicas are dearer.

With the shipped example pricing, a TP=2 primary at cost 10 and a TP=1
secondary at cost 5 cost the same per GPU. A TP=2 replica serves more than
twice a TP=1 one, so the primary is more efficient and grows first.

### What it needs

- One InferencePool and one EPP in front of both deployments.
- The same `modelID` in each ScaledObject's trigger metadata. This is the
  join; get it wrong and you have two independent variants that happen to
  serve the same weights.
- A relative cost per variant, as `variantCost` in the trigger metadata
  (default `"10.0"`; see [the trigger reference](../../reference/scaledobject-trigger.md)).
  Set it to GPUs per replica × the relative price of that GPU. It is a
  relative number, not a currency; no real prices are read. The plan writes it
  into the trigger. The old `llm-d.ai/variant-cost` annotation is ignored.

### Verifying it

```bash
# which variant the optimizer grew, and why it was the efficient one
kubectl logs -n <controller-namespace> deploy/<scaling-manager> | grep analyzer-result
```

The log is the admin's when the controller is out of your reach. Its
`variants` array names each variant, its per-replica capacity and the reason
code ([the cycle log](../../reference/cycle-log.md)).

### How it is tested

**No end-to-end spec asserts which variant grows.** `test/e2e/limiter_test.go`
runs two variants of one model, one per accelerator type. It asserts that both
are discovered, both get `llmsm_desired_replicas`, and neither exceeds its
`maxReplicaCount`: the discovery and the GPU bound, not the cost order. Unit
specs assert the cost order
(`internal/engines/allocation/cost_aware_optimizer_test.go`); beyond them, the
evidence is the benchmark scenario and its written-up run. Add an end-to-end
spec if you depend on it. The harness's start-path settings are in
[Replica start time in the harness](../../guides/benchmarking/README.md#replica-start-time-in-the-harness).

### Tuning it

Per-variant scoring and analyzer choice:
[scaling policy](../../reference/scaling-policy.md). How capacity per variant
is derived, including from deployment arguments:
[the steady-state engine](../../concepts/steady-state-engine.md).
