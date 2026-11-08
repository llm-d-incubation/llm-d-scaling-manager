# Router configuration

What to set on the llm-d inference router (the Endpoint Picker, EPP) so the
signals the scaling manager reads mean what it assumes.

The scaling manager does not configure the router or route anything. It *reads*
the router, and two router settings decide what it sees:

- **flow control** bounds the gateway queue, which the scaling manager reads as
  demand;
- **scheduling profiles** pick the replica for each request, and so decide
  whether the replicas the scaling manager orders get used.

Neither default is wrong. Both suit short prompts and behave differently under
long ones, and neither tells you when it is what limits you.

## Flow control bounds the demand signal

`internal/collector/registration/saturation.go` and `throughput_analyzer.go`
both read

```
llm_d_epp_flow_control_queue_size or inference_extension_flow_control_queue_size
```

as the model's queued demand. Requests in that queue have reached no engine
yet. `estimateSchedulerQueueDemand` charges them to the roles, and on a
disaggregated fleet they are most of what sizes prefill. **The bound on that
queue is the ceiling on the demand the scaling manager can see.**

Ask the router what it resolved, not the ConfigMap: defaults are filled in at
load and the ConfigMap does not show them:

```bash
# Print the flow-control configuration the router resolved
kubectl -n <ns> logs deploy/<model>-router-epp -c epp \
  | grep -o 'FlowControlConfig:{.*}' | head -1
```

An unconfigured `llm-d-router-endpoint-picker:v0.9.0` answers, in substance:

```
Controller:{DefaultRequestTTL:0s ExpiryCleanupInterval:1s ...}
Registry:{MaxBytes:0 MaxRequests:0
          PriorityBands:[{Priority:0 Queue:ListQueue MaxBytes:1000000000 MaxRequests:0}]}
```

That is three facts:

| setting | value here | meaning |
|---|---|---|
| `DefaultRequestTTL` | `0s` | **no expiry.** A queued request waits until the client gives up. |
| `MaxRequests` (band) | `0` | no count bound applied on this version. |
| `MaxBytes` (band) | `1000000000` | 1 GB — the only bound that actually applies. |

**Check your own version; do not copy this table.** These are what *this*
router resolved with no `flowControl` block. Defaults differ between versions: a
newer one defaults `defaultRequestTTL` to 60s and gives each band a count bound.
The command above prints what yours resolved.

So the queue is bounded by *bytes*, and prompt length decides how many requests
that is. Measured on a 30,000-token prompt trace at 12 req/s, against a fleet
that could not keep up:

| | observed |
|---|---|
| queue depth, peak | **4,772 requests** |
| queue bytes, peak | **814 MB — 81% of the 1 GB band cap** |
| mean time a request spent queued | **181 s** |
| requests evicted, all as `client disconnected: request evicted from queue: request context cancelled` | 246 |

At ~170 KB per queued request the byte cap is reached at about 6,000
requests, so here *bytes* bind first and the count never does. With 500-token
prompts the same cap is tens of thousands of requests, and the queue is in
effect unbounded. The same configuration behaves very differently with prompt
length.

Two consequences for the scaling manager:

1. **An unbounded queue is an unbounded demand signal.** The scaling manager
   keeps ordering replicas for queued work, including work whose client has
   already timed out. The 246 evictions above were all `client disconnected`,
   not the router shedding by policy: with `DefaultRequestTTL: 0s` it never does.
2. **A request queued for 181 seconds is not demand you can serve.** With a
   30-second time-to-first-token objective, a request queued for three minutes
   has already failed. Scaling for it buys nothing and costs GPUs.

### A TTL has to cover two different kinds of waiting

A request cannot dispatch for one of two reasons, and they need opposite
budgets:

| regime | meaning | what waiting buys | right budget |
|---|---|---|---|
| **saturated** | endpoints exist, all busy | a slot frees in seconds | short — keep TTFT inside the objective |
| **empty pool** | no endpoints at all | a pod starts: image pull + weight load | long — minutes |

**This matters more here than usual, because the scaling manager scales to
zero.** Every scale-from-zero is the empty-pool case, and the wait is a pod
start: **60-64 seconds** measured on this stack. A TTL set for a
time-to-first-token objective (30 seconds, say) sheds every scale-from-zero
request about half a minute before its pod is ready. The autoscaler then looks
like it failed to scale, when the router gave up.

v0.11.0 and newer split the two: `defaultRequestTTL` for the saturated case,
`noEndpointRequestTTL` for the empty pool. Each is charged from the later of
enqueue time and the last regime change, so a regime change restarts the
budget. **v0.9.0 does not**: it has `DefaultRequestTTL` and no empty-pool
budget, so one number serves both and the only safe choice is the longer one.
That is why this repo's scenarios pin v0.11.0.

### Which router version you need

The two-budget split decides whether a scale-to-zero deployment works or
sheds its own cold starts, so it decides the version. From the shipped binaries,
not release notes:

| flow-control feature | v0.9.0 | v0.11.0 |
|---|---|---|
| `noEndpointRequestTTL` — the scale-from-zero waiting room | **absent** | present |
| `enableEviction` — in-flight eviction | absent | present |
| `priority-holdback-policy`, `soft-reflective-ceiling-policy` | absent | present |
| `sheddable-eviction-filter` | absent | present |
| `slo-deadline`, `edf-ordering`, round-robin and program-aware fairness | present | present |

**On v0.9.0 no `defaultRequestTTL` serves a scale-to-zero deployment.** Short
enough for a time-to-first-token objective kills cold starts. Long enough for a
cold start gives up the objective whenever the pool is just busy. Upgrade, or
accept one of those.

#### Migrating to v0.11.0: three changes

A P/D config does not start on v0.11.0 unchanged. Each of these is a startup
failure, and none is in a release note:

**1. `disagg-headers-handler` is gone.**

```
configuration validation failed: plugin type 'disagg-headers-handler' is not registered
```

**Delete the line**; there is no replacement. Upstream `runner.go` registers
only `disagg-profile-handler` (now `disagg.HandlerFactory`, via
`RegisterWithPluginDependencies`) and the three PD deciders. The v0.9.0 types
`disagg.HeadersHandler` and `disagg.PdProfileHandler` merged into one
`disagg.Handler` taking a `StageOrder`. (`header-profile-handler` in v0.11.0 is
*not* it: it is an unrelated Alpha plugin that picks a profile from a header.)

**2. `deciderPluginName` was removed.**

```
failed to parse parameters of the disagg-profile-handler
  json: unknown field "deciderPluginName"
```

Use `deciders.prefill`. v0.9.0 warned at every startup: *"Deprecated parameter
'deciderPluginName', use 'deciders.prefill' instead"*. Before any upgrade, grep
your router's log for deprecation warnings: they are the migration notes.

```yaml
- type: disagg-profile-handler
  parameters:
    deciders:
      prefill: always-disagg-pd-decider
```

**3. Declare the metrics data source.** This one does not crash, which makes
it the dangerous one.

v0.9.0 injected a working `metrics-data-source`; v0.11.0's auto-created producer
does not reach the engines. Without engine metrics every endpoint reads stale,
and the utilization detector scores a stale endpoint as **fully saturated** (it
fails closed). The router starts and reports healthy, then one request against
an idle fleet takes 19-30 s or is shed at the TTL with a 429. Declare it:

```yaml
- type: metrics-data-source
  parameters:
    scheme: "http"
    path: "/metrics"
    insecureSkipVerify: true
- type: core-metrics-extractor
```

After these three, the `flowControl` block resolves as written:
`DefaultRequestTTL: 30s, NoEndpointRequestTTL: 3m0s, PriorityBands:
[{Priority: 0, MaxBytes: 1073741824, MaxRequests: 2000}]`.

**Then verify the P/D split, not just that it starts.** A mistake in the
disaggregation plugins does not crash: it quietly routes everything to one role,
and a benchmark still gives plausible numbers. Drive a dozen completions and
check that *both* roles' `vllm:request_success_total` moved:

```bash
# Read the success counter on one pod per role, before and after the traffic
kubectl -n <ns> exec <pod> -c vllm -- \
  sh -c 'curl -s localhost:$VLLM_INFERENCE_PORT/metrics | grep ^vllm:request_success_total'
```

Measured here after the migration: completions in 27-120 ms (v0.9.0 was
44-113 ms), and 12 requests incremented both roles by exactly 12.

**Pin the router version**; do not inherit it from a benchmark harness. An
unpinned router can change how requests spread and how the queue is bounded
between runs, with nothing in the output saying so.

### What to set

```yaml
# EndpointPickerConfig  (v0.11.0 or newer)
flowControl:
  defaultRequestTTL: 30s        # saturated: your TTFT objective
  noEndpointRequestTTL: 180s    # empty pool: pod start + margin.
                                # Size this from YOUR measured startup --
                                # 60-64 s on this stack, so 180 s is ~3x.
  priorityBands:
    - priority: 0
      maxRequests: 2000         # a COUNT bound, so behaviour stops depending
                                # on prompt length
      maxBytes: 1Gi
```

On v0.9.0, with no split, the only safe single value clears pod start
(`defaultRequestTTL: 180s`), and the TTFT objective goes unserved while the pool
is busy.

Two easy mistakes:

**A TTL acts only if the caller waits at least as long.** Set a gateway request
timeout no shorter than the TTL. Otherwise the client disconnects first, and the
request is evicted as a cancelled context, not shed by policy. That is what all
246 evictions above were (`request context cancelled`): the router had no
budget, so the caller set the only bound.

**Set the count bound explicitly.** With only a byte bound, the depth at which
the router starts rejecting moves tenfold with prompt length, and nothing says
so. On some versions a per-band `0` means "unset, take the default", not "no
limit", so `0` is not reliably unbounded. Read the resolved configuration from
the log.

### The prerequisite that fails closed

Dispatch needs **fresh** model-server metrics. The default saturation
detector scores an endpoint whose metrics are older than its staleness threshold
as *fully saturated*: it fails closed, not open. If the router loses its scrape
path to every endpoint (a NetworkPolicy, a port change, TLS, a starved refresh
loop), saturation pins high, dispatch stalls, and queued requests leave at their
TTL, or with no TTL, never.

Keep the metrics refresh interval well inside the staleness threshold. Watch
`inference_extension_flow_control_pool_saturation` next to queue size. On the run
above it peaked at 28.8 with the queue 4,772 deep: saturation was reported, but
the queue had no bound to act on it.

## Scheduling profiles decide whether ordered replicas get used

A P/D router carries one profile per role. The stock prefill profile is:

```yaml
- name: prefill
  plugins:
    - pluginRef: prefill-filter
    - pluginRef: prefix-cache-scorer
      weight: 3
    - pluginRef: queue-scorer
      weight: 2
    - pluginRef: kv-cache-utilization-scorer
      weight: 2
```

Scores are normalised to `[0,1]` and weighted-summed, so **the largest weight
wins ties it should not**. With `prefix-cache-scorer: 3` against
`queue-scorer: 2`, a prefix hit adds up to 3, while the whole gap between an
idle and a saturated replica adds at most 2. A replica that has seen the prefix
gets the request however busy it is.

Measured on the same trace, prefill side, at 8-10 ready replicas:

- the busiest replica held **65-100% of the waiting queue** at the median,
  where even sharing would be ~15%
- fewer than 40% of ready replicas had any queued request at all
- work per replica spread from 34% of the total down to 3%

The scaling manager ordered ten prefill replicas, about four times what the
load needed, and the queue still passed 1,300. **When a scaled-out fleet's
latency does not improve, measure the spread before scaling further.** Routing
can cap latency where no replica count helps.

### What to check first

Check that the affinity you pay for exists at all. Without prefix caching on
the engines, the prefix scorer steers toward KV that was never kept:

```bash
# on an engine pod: is prefix caching even on?
kubectl -n <ns> get deploy <model>-prefill -o json \
  | jq -r '.spec.template.spec.containers[]|select(.name=="vllm")|.args|join(" ")' \
  | tr ' ' '\n' | grep -i prefix

# and is it being used?  0.0 everywhere means it is not
kubectl -n <ns> exec <engine-pod> -- \
  curl -s localhost:8000/metrics | grep '^vllm:prefix_cache_queries_total'
```

On a stack started with `--no-enable-prefix-caching`, those counters read `0.0`
on every replica for the whole run: the highest-weighted prefill signal is
optimising for a cache that does not exist.

### What is NOT established

Swapping the weights (`queue-scorer: 3`, `prefix-cache-scorer: 2`) on this
workload **did not improve prefill latency**: phase-2 time-to-first-token went
from 54 s to 80 s, and the busiest replica's queue share from 65% to 100%. One
run does not show the swap is harmful either: two runs with the weights
*unchanged* scored 100% and 65%, so run-to-run spread is wider than the effect.

Measure the weights on your own workload, over repeated runs; there is no
known-good value. What holds today: *if* your prefill replicas are unevenly
loaded, look at the scorer weights, and measure the spread before changing them.

## Which plugins to add — usually none

The router ships ordering, fairness and usage-limit policies and a saturation
detector. Most are driven by something the *client* sends. A plugin your
traffic cannot drive changes nothing and makes the config harder to read. Check
the input first:

| plugin | needs | inert without it |
|---|---|---|
| `round-robin-fairness-policy`, `program-aware-fairness` | `x-llm-d-inference-fairness-id` per request | every request lands in one `default-flow` and there is nothing to rotate between |
| `slo-deadline-ordering-policy` | a TTFT SLO header, in milliseconds | all requests get a far-future deadline and sort identically |
| sheddable-band tuning (`defaultNegativePriorityBand`) | an `InferenceObjective` with `priority < 0` | unclassified traffic defaults to priority 0, which is **non-sheddable**, so the negative band is never used |
| `priority-holdback-policy` | several priorities *and* `--allow-experimental-plugins` | nothing to hold back |

A benchmark harness sending plain OpenAI completions drives none of these, nor
does most single-tenant production traffic. The flow-control block above is what
matters. The plugins are for genuinely distinct classes of traffic, and then the
first step is the `InferenceObjective`s that classify them.

Worth knowing even unconfigured: sheddability comes only from
`InferenceObjective.spec.priority < 0`. There is no `sheddable: true` field, and
traffic with no matching objective is non-sheddable. A cluster with no
`InferenceObjective`s cannot shed anything by priority, whatever the bands say.

## Changing router configuration

**The router reads its configuration once, at startup** (`--config-file
/config/<name>.yaml`, no reload or watch). The `/config` volume has no
`subPath`, so an edited ConfigMap *does* reach the pod within about a minute,
but nothing re-reads it until the process restarts.

**Restarting the router is a brief outage, not a rolling update.** On a stock
deployment:

- `replicas: 1`
- `strategy: Recreate` — the old pod is terminated *before* the new one starts
- the pod carries both the Envoy proxy and the EPP, and the model Service
  points at it, so it is the data plane and not a sidecar

There is no overlap and no PodDisruptionBudget. In-flight requests are dropped
and new ones refused until the readiness probe passes, typically 10-30 seconds.
**Change router configuration while the fleet is idle**; during a benchmark or
production window it corrupts the run or drops traffic.

```bash
# Patch the config, restart the router, and wait for it
kubectl -n <ns> patch cm <model>-router-epp --type merge --patch-file patch.json
kubectl -n <ns> rollout restart deploy/<model>-router-epp
kubectl -n <ns> rollout status  deploy/<model>-router-epp --timeout=180s
```

Then confirm what the router loaded, not what the ConfigMap says. Defaults and
plugins are resolved at load, and only the log shows the result:

```bash
# Print the prefill profile the router loaded
kubectl -n <ns> logs deploy/<model>-router-epp -c epp \
  | grep -o 'prefill:{Filters.*Scorers: \[[^]]*\]'
```

Expect the weights you set, as floats:

```
prefill:{Filters: [prefill-filter/by-label], Scorers: [prefix-cache-scorer: 3.000000, queue-scorer: 2.000000, ...]
```

## See also

- [`metrics.md`](metrics.md) — the full metric surface the scaling manager reads
- [`scaling-policy.md`](scaling-policy.md) — thresholds the queue feeds into
- [`troubleshooting.md`](troubleshooting.md) — symptoms and where to look
