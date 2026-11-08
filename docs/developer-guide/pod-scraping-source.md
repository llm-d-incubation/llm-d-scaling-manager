# PodScrapingSource Usage Guide

## Overview

`PodScrapingSource` (`internal/collector/source/pod`) scrapes Prometheus-format
metrics straight from Kubernetes pods over HTTP. It implements the
`MetricsSource` interface, so it can stand in for Prometheus queries.

- Finds pods through a Service's selector and scrapes only Ready pods.
- Scrapes pods concurrently, up to a limit.
- Caches results with a TTL.
- Sends an optional Bearer token.
- Parses the standard Prometheus text format.

## Prerequisites

1. **Cluster access**: a working kubeconfig.
2. **A Service with a pod selector** that selects the target pods.
3. **A metrics endpoint** on the pods, in Prometheus format (`/metrics` or a
   custom path). EPP pods serve it on port `9090`.
4. **RBAC** for the client:
   - `get`, `list`, `watch` on `services` and `pods`;
   - for EPP metrics, `get` on the non-resource URL `/metrics`.
5. **Bearer token** (optional): if the metrics endpoint requires authentication,
   pass `BearerToken` or `BearerTokenFile`. The source reads no Secret itself.
   For EPP, the controller mounts the `scaling-manager-epp-metrics-token` Secret at
   `/var/run/secrets/epp-metrics/token` (see
   [the upstream authentication guide](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/site-src/guides/metrics-and-observability.md#scrape-metrics--pprof-profiles)).

## Basic Usage

```go
import (
    "context"
    "time"

    "sigs.k8s.io/controller-runtime/pkg/client"

    "github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
    "github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/pod"
)

config := pod.PodScrapingSourceConfig{
    ServiceName:      "my-service",   // required
    ServiceNamespace: "my-namespace", // required
    MetricsPort:      9090,
}

podSource, err := pod.NewPodScrapingSource(ctx, k8sClient, config)
if err != nil {
    // ServiceName or ServiceNamespace is empty
}
```

`NewPodScrapingSource` fills the defaults below for any field left at its zero
value.

## Configuration Reference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `ServiceName` | `string` | Yes | - | Service that selects the target pods |
| `ServiceNamespace` | `string` | Yes | - | Namespace of the Service |
| `MetricsPort` | `int32` | Yes | - | Port the pods serve metrics on |
| `MetricsPath` | `string` | No | `"/metrics"` | HTTP path of the metrics endpoint |
| `MetricsScheme` | `string` | No | `"http"` | `"http"` or `"https"` |
| `BearerToken` | `string` | No | `""` | Bearer token sent on every scrape |
| `BearerTokenFile` | `string` | No | `""` | File re-read on every scrape; overrides `BearerToken`, which becomes the fallback while the file is unreadable. The datastore sets it to `/var/run/secrets/epp-metrics/token` |
| `ScrapeTimeout` | `time.Duration` | No | `5s` | HTTP timeout per pod |
| `MaxConcurrentScrapes` | `int` | No | `10` | Most pods scraped at once |
| `DefaultTTL` | `time.Duration` | No | `30s` | How long scraped metrics stay cached |

Examples:

```go
// With a token file (re-read on every scrape, so a rotated token is picked up)
config := pod.PodScrapingSourceConfig{
    ServiceName:      "epp-service",
    ServiceNamespace: "default",
    MetricsPort:      9090,
    BearerTokenFile:  "/var/run/secrets/epp-metrics/token",
}

// With custom timeouts and concurrency
config := pod.PodScrapingSourceConfig{
    ServiceName:          "epp-service",
    ServiceNamespace:     "default",
    MetricsPort:          9090,
    ScrapeTimeout:        10 * time.Second,
    MaxConcurrentScrapes: 5,
    DefaultTTL:           60 * time.Second,
}
```

## Using PodScrapingSource

### Refresh

`Refresh()` discovers pods, scrapes every Ready pod, and updates the cache. It
returns one result under `"all_metrics"` (every value from every pod) plus one
result per metric name (the `__name__` label). With no Ready pods it returns an
empty `"all_metrics"` result and no error.

```go
results, err := podSource.Refresh(ctx, source.RefreshSpec{})
if err != nil {
    // pod discovery failed
}

for _, v := range results["all_metrics"].Values {
    fmt.Printf("%s=%f pod=%s\n", v.Labels["__name__"], v.Value, v.Labels["pod"])
}

queue := results["inference_pool_average_queue_size"] // one metric only
```

### Get

`Get()` reads the cache without scraping. It takes `"all_metrics"` or a metric
name, and returns `nil` when nothing is cached or the entry has expired.

```go
cached := podSource.Get("all_metrics", nil)
if cached == nil {
    results, err := podSource.Refresh(ctx, source.RefreshSpec{})
    // ...
}
```

### QueryList

`QueryList()` returns the query registry. The source registers one query,
`"all_metrics"`.

## Results

```go
type MetricResult struct {
    QueryName   string        // "all_metrics" or a metric name
    Values      []MetricValue
    CollectedAt time.Time     // when the pods were scraped
    Error       error
}

type MetricValue struct {
    Value     float64
    Timestamp time.Time
    Labels    map[string]string
}
```

Each value's labels are the original metric's labels (e.g. `model_name`,
`target_model_name`, or `name` for an inference pool), plus:

- `__name__`: the metric name, e.g. `inference_pool_average_queue_size`;
- `pod`: the pod that served it.

### EPP metrics

EPP pods serve the
[Gateway API Inference Extension metrics](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/site-src/guides/metrics-and-observability.md).
Common ones:

- **Requests:** `inference_objective_request_total` (counter),
  `inference_objective_request_error_total` (counter),
  `inference_objective_running_requests` (gauge),
  `inference_objective_request_duration_seconds` (distribution).
- **Inference pool:** `inference_pool_average_queue_size`,
  `inference_pool_per_pod_queue_size`,
  `inference_pool_average_kv_cache_utilization`, `inference_pool_ready_pods`
  (all gauges).
- **Flow control (experimental):** `inference_extension_flow_control_queue_size`
  (gauge), `inference_extension_flow_control_request_queue_duration_seconds`
  (distribution).

## Integration Examples

### Register in a SourceRegistry

```go
registry := source.NewSourceRegistry()
if err := registry.Register("pod-scraping-epp", podSource); err != nil {
    return err
}

src := registry.Get("pod-scraping-epp")
results, err := src.Refresh(ctx, source.RefreshSpec{})
```

### Use in an engine

```go
type MyEngine struct {
    source source.MetricsSource
}

// HasPendingRequests reports whether any EPP queue is non-empty.
func (e *MyEngine) HasPendingRequests(ctx context.Context) (bool, error) {
    results, err := e.source.Refresh(ctx, source.RefreshSpec{})
    if err != nil {
        return false, err
    }
    for _, name := range []string{
        "inference_pool_average_queue_size",
        "inference_extension_flow_control_queue_size",
    } {
        if r := results[name]; r != nil {
            for _, v := range r.Values {
                if v.Value > 0 {
                    return true, nil
                }
            }
        }
    }
    return false, nil
}
```

## Behaviour Notes

- **Discovery.** Pods come from the Service's `spec.selector`. Only pods whose
  `Ready` condition is `True` are scraped. A Service without a selector yields
  no pods.
- **Concurrency.** Up to `MaxConcurrentScrapes` pods are scraped at once, each
  with `ScrapeTimeout`. The results are merged into one `MetricResult`.
- **Caching.** Results are cached for `DefaultTTL` (default 30s), under
  `"all_metrics"` and under each metric name. `Get()` reads them without
  scraping.
- **Authentication** is optional: with neither `BearerTokenFile` nor
  `BearerToken` set, no `Authorization` header is sent. `BearerTokenFile` wins
  and is re-read on every scrape; while the file is unreadable or blank,
  `BearerToken` is the fallback. The source never reads a Secret: mount the
  token as a file, or pass it in.
