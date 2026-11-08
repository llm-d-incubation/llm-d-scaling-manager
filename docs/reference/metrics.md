# Metrics Health Monitoring

## Overview

Use this page when scaling looks stuck and you need to know whether the
problem is the decision or the data. llm-d-scaling-manager checks that the vLLM
metrics it needs are arriving before it decides anything, and holds steady when
they are not.

There is no CRD or status condition to `kubectl get`. You see the metrics
health in three places: the gauges below, a `MetricsUnavailable` Warning Event
on the workload's ScaledObject, and the controller log.

## The gauges

These are queryable, and they are what to alert on:

```promql
# Are the pods behind each model being found at all?
llmsm_metrics_pods_discovered

# How many of them were decided on with fresh data this cycle?
llmsm_metrics_freshness_status{status="fresh"}

# Pods decided on with stale data, or none at all.
llmsm_metrics_freshness_status{status!="fresh"} > 0

# Did the variant get a fresh decision this cycle? 0 = its saturation gauges are stale.
llmsm_saturation_metrics_up == 0
```

`llmsm_metrics_freshness_status` counts pods per `(variant_name, status)`, with
`status` one of `fresh`, `stale`, `missing`, `unavailable`. It is not a 0/1
flag.

`llmsm_metrics_collection_errors_total` rising alongside them points at
Prometheus itself (a failing query) rather than at a ServiceMonitor gap. The
full catalogue, with what each one should read on a healthy fleet, is in
[Monitoring](monitoring.md#the-metrics-that-answer-specific-questions).

## The `MetricsUnavailable` Event

When a variant's metrics go from available to unavailable, the controller
records a **Warning** Event, reason `MetricsUnavailable`, on the variant's
**ScaledObject**. It fires on that transition only, not every cycle, and not
for a variant whose metrics were never seen (one that started at zero).

| message | means | remedy |
| --- | --- | --- |
| `Failed to collect metrics for model` | the Prometheus query failed | check Prometheus reachability and `llmsm_metrics_collection_errors_total`; the controller log carries the error |
| `No saturation metrics available for model` | the query worked and returned nothing for this model | check that a ServiceMonitor or PodMonitor selects the model pods, that the pods are Ready and serve `/metrics`, and that Prometheus `/targets` shows them up |

```bash
kubectl get events -n <model-namespace> \
  --field-selector involvedObject.kind=ScaledObject,reason=MetricsUnavailable
```

The matching log line, every cycle while it lasts:

```bash
kubectl logs -n "$NS" -l app.kubernetes.io/name=scaling-manager --tail=-1 \
  | grep 'No saturation metrics available for model'
```

## Graceful degradation

When a variant's metrics are unavailable, the scaling manager:

1. **Skips optimization** for that variant (no new scaling decision).
2. **Keeps the current replica count.** It does not fall back to a default, and
   it does not park.
3. **Retries** on the next cycle.
4. **Keeps optimizing other variants** whose metrics are available.

## Key components

- **`internal/collector`**: queries Prometheus, reports per-pod freshness, and
  records the `MetricsUnavailable` Event.
- **`internal/metrics`**: publishes the gauges above.

## Best practices

1. **Alert on `llmsm_metrics_freshness_status{status!="fresh"} > 0`** for a few
   cycles.
2. **Watch `MetricsUnavailable` Events** on the ScaledObjects you own.
3. **Validate the ServiceMonitor** during initial deployment: `make verify-deployment`.
4. **Test the metrics flow** before relying on the scaling manager in production.

## Related documentation

- [Monitoring](monitoring.md): the full metric catalogue and what each should read
- [Prometheus integration](prometheus.md)
- [ServiceMonitor configuration](../../config/base/monitoring/servicemonitor.yaml)
