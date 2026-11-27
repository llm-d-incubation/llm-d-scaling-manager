# Capacity planner capture and Generate: internals

`capacity-planner capture` turns a past window in Prometheus into the inputs the
analyzers would have read, and `capacity-planner generate` plays them through the
analyzers as a controller's cycles would have. The output is an ordinary
learner recording. The design is R2 in
[the learner proposal, 2.9](../proposals/analyzer-learner.md#29-recorder-fields-for-ttft-and-capture).

This page is how it is built. How to run it, its flags, its load on
Prometheus and how to read the answers are in the guide:
[Record a window](../guides/record-for-capacity-planner/), part of
[the capacity planner guide](../guides/capacity-planner/).
[The metrics it needs](../guides/capacity-planner/metrics.md) lists every raw
series the queries below read. Capturing from a snapshot is in
[Testing: Capture a past window](testing.md#capture-a-past-window).

`replicas-to-slo` and `slo-to-rate`, given the capture flags in place of
`--recording`, capture and generate in memory along this same path, and answer
as `--recording` would on the files
([Prometheus mode](../proposals/slo-capacity-planner.md#prometheus-mode-as-built-d4)).
So does `serve` for a `prometheus` source.

Exit codes: 2 for the caller's input (`whatifsource.IsInputError`: a bad
flag, invalid options, overlapping `--role-of` selectors, no Pod the role map
attributes with a KV-cache series, a window too large without `--force`, a
capture `generate` cannot read); 1 for anything else
(`whatifsource.ErrQueryFailed`, or a `generate` that fails inside the
analyzers).

## Load and bounds

A window of more than 10,000 steps is asked in range queries of at most 10,000
steps, one part after another (`Options.RangeQueryPoints` in tests). The
collector asks a cycle's queries together, so different queries run at the
same time, as many as a controller's cycle asks at once. A capture registers
about 80 queries with kube-state-metrics (67 without), and then checks the
[optional series](#optional-series), one range query each (61 for vLLM).

`whatifsource.MaxStepQueries` (1,000,000) steps times registered queries is
checked before any query: about 12,500 steps with the 80 queries.
`whatifsource.MaxSamples` (20 million, at about 60 bytes each) bounds the
samples held, counted as the answers arrive. Past either, exit 2 unless
`--force`.

Every query is required. One that fails cancels the capture's context at
once and names the query; the collector's retries do not sleep through their
backoff on an answer that cannot change. A failed query would leave its field
absent at every step, which a capture cannot tell from a field the window did
not have, so the generated run would read inputs no controller read. Every
sample must be on the step grid, `--from` plus a whole number of steps; a
query frontend that aligns start to step answers off it, and the capture
fails naming the cause.

## The pieces

| Piece | Where | What it does |
| --- | --- | --- |
| `windowAPI` | `internal/whatifsource/window.go` | A `promv1.API` that answers each instant query at the current step. It asks each PromQL expression once, as one range query over the window (split into parts of at most 10,000 steps, `Options.RangeQueryPoints` in tests). Range queries are the only requests it sends. The first that fails cancels the capture's context with its error. |
| The collector | `internal/collector` | The controller's own `ReplicaMetricsCollector`, over a `PrometheusSource` on `windowAPI`, with every query registered as the engine registers it. Each step opens a recording cycle (`BeginRecordingCycle(true)`), so the TTFT inputs are read too. `SetClock` stamps the rows at the step, not at the wall clock. |
| `fleet` | `internal/whatifsource/fleet.go` | The cluster at one step, from kube-state-metrics. It is the collector's `PodLocator` (which variant a Pod is) and its Pod reader (readiness, Ready time, start time, revision, deletion). |
| `RoleOf` | `internal/whatifsource/roles.go` | `--role-of ROLE=SELECTOR`: a label selector, matched against the `kube_pod_labels` labels when they carry every key it names. When they do not, a one-equality selector `app=x` matches Pods named `x-...`, and the capture is flagged as inferred. Two selectors whose name prefixes nest (`app=qwen` and `app=qwen-prefill`), or a Pod two selectors match, are refused (`ErrRoleOverlap`, exit 2). |
| `Capture` | `internal/whatifsource/capture.go` | Two passes over the window. The first finds every variant and its model from the KV-cache series. The second collects every model at every step. It writes a gzipped learnrec stream: the start line, a `capture.Window` line and one `capture.Cycle` line per model per step. |
| `Generate` | `internal/engines/analyzers/learnreplay/generate.go` | The third replay mode. It plays the cycles in the engine's order and writes a recording stamped with its own build's commit. |
| `promtest` | `internal/whatifsource/promtest` | A fake Prometheus HTTP API for tests. It serves Prometheus's own JSON and records every request. |

## The capture file

Each line is a learnrec event (`learnrec.Read` reads the file):

- `start`: `learnrec.Start`, built from the analyzers a controller with the
  policy would build, with the step as its optimize interval. The slo
  analyzer is always built and judging. The throughput analyzer is built
  when the policy lists it.
- `capture.Window`: `learnreplay.CaptureWindow`. It holds the namespace, the
  window, the step, where the roles came from, the flags (caveats), and each
  variant's model, role, accelerator, GPU count and parsed engine
  parameters (`capacity.EngineParams`). `Optional` (`learnreplay.CaptureSeries`,
  omitted while empty) holds what the capture found of each optional series,
  below. Generate does not read it.
- `capture.Cycle`: `learnreplay.CaptureCycle`. It holds one model's
  `learnrec.Input` at one step (rows in variant and Pod order, the variants'
  states, the merged policy, the scheduler queue, the arrival rate, the
  router latency and the flow-control pools), and the model's parked
  variants.

## Optional series

`whatifsource.OptionalSeriesList` is the cache-tier, offloading and
KV-transfer series an operator collects for TTFT on cached fleets (vLLM
connector and offload counters, NIXL bytes, LMCache, the router's cached
tokens, SGLang's hit tiers), and the router, P/D sidecar and engine front-end
series that explain the TTFT spent outside the engines. No query the collector registers reads them, so
a capture does not replay them. `checkOptional` (`internal/whatifsource/optional.go`)
asks one range query per series of the capture's engine (and every router
and LMCache series), over the window at the step, on a `windowAPI` of its
own with its own sample bound per query. A counter (`_total`, `_sum`,
`_count`) is asked as `sum by (<By>)` and its increase is summed step to
step, a fall read as a restart; anything else as `count`. The result is
`Summary.Optional` and the window line's `Optional`; `capture` prints one
line per series (`whatifsource.OptionalText`).

Unlike the collector's queries, none is required: an empty answer is
`absent`, and a failed one is recorded with its error (`unchecked`) while
the capture goes on. Only a cancelled context stops the checks. Their range
queries and samples count in the summary's totals, but not toward
`MaxStepQueries`.

The list, the operator table in
[metrics.md](../guides/capacity-planner/metrics.md#cache-tiers-offloading-and-kv-transfer)
(with each series' tier) and the record guide's `CACHE_METRICS` are kept in
step by `internal/whatifsource/metrics_doc_test.go`.
`hack/check-record-export.sh` (in `make lint-deploy-scripts`) runs the guide's
fetch and check steps against a fake Prometheus: absent series pass, a failed
one fails, `LMCACHE_NAMESPACE` reaches the LMCache server's series, and each
empty kube-state-metrics series gets its `EMPTY:` hint. With a fake `kubectl`
over fixtures, it runs the describe step and Path 1's engine-arguments step on
a DisaggregatedSet (each role picked by its `llm-d.ai/role` label), a
LeaderWorkerSet and a Deployment, and refuses a role with no label.

## Generate's order

One step, at the step's instant, as `steadystate` runs a cycle:

1. `slo.BeginCycle`;
2. the settle tracker keeps only the models with a variant that has
   replicas, as the engine's prune does (`retainSettle`): a model parked
   whole is forgotten. A step with no such model ends here, as the engine's
   cycle with no active variant does;
3. `NoteVariantsPresent` on saturation and then slo, model by model in key
   order: first the variants with replicas, then the parked ones;
4. saturation's `EvictStaleHistory`, the store's `EvictStale`, slo's
   `EvictStale`.

Then each model, in key order:

5. the settle tracker. It observes the model's rows. A model with variants
   and no rows is held and gets nothing more, as `onNothingObserved` does;
6. for each variant, in name order, `StableAcceleratorFor` and then
   `FileScaleTarget`. A capture never forgets a variant on request;
7. `Analyze`: saturation, then slo and throughput when the policy lists
   them. The slo analyzer's router serves the captured router latency.

The generated recording's start line carries the capture's flags as
`learnrec.Start.Caveats`, so a golden made from it says what the capture
could not see. A Fidelity replay keeps them, and the export allows their
words (`internal/whatifsource` is one of `MessageWordSources`).

## Where each input comes from

The rows are the collector's, so every field comes from the query the
controller reads it from, by the same rules (presence bits, NaN, readiness,
the uptime bound). `TestCapture_EachQueryFillsItsField` checks each field
present, absent and NaN.

| Input | Query (`registration.Query...`) or series |
| --- | --- |
| `KvCacheUsage`, `QueueLength` (and their presence bits); the busiest rank's | `KvCacheUsage`, `QueueLength`, `MaxRankKvCacheUsage`, `MaxRankQueueLength` |
| `CapacityQueueLength`, `MaxRankCapacityQueueLength`, `CapacityQueueKnown` | `CapacityQueueLength`, `MaxRankCapacityQueueLength` |
| `LandedQueueLength`, `MaxRankLandedQueueLength`, `LandedQueueKnown` | `LandedQueueLength`, `MaxRankLandedQueueLength` |
| `NumGpuBlocks`, `BlockSize`, `TotalKvCapacityTokens`, `EngineRanks`, `TokensInUse` | `CacheConfigInfo` |
| Length means; `OutputTokensBuckets`, `AvgOutputTokensSquared` | `AvgInputTokens(Recent)`, `AvgOutputTokens(Recent)`, `OutputTokensBuckets` |
| `PrefixCacheHitRate` | `PrefixCacheHitRate` |
| `AvgITL` with `AvgITLKnown` and `AvgITLEmpty` (NaN), `AvgTTFT`, `AvgServiceTime` | `AvgITL`, `AvgTTFT`, `AvgServiceTime` |
| `PrefillComputedTokenRate`, `GenerationTokenRate`, `RequestRate`, `KvUsageInstant`, `KvUsageMean` | the queries of the same names |
| `RunningRequests`, `PreemptionRate`, `SpecAcceptedPerStep` | the queries of the same names |
| R1's TTFT inputs: `PromptTokensBuckets`, `TTFTBuckets`, `AvgQueueTime`, `QueueTimeBuckets`, `QueueMin`, `AvgPrefillTime`, `AvgKVTransferTime`, `TPOTBuckets` | `PromptTokensBuckets`, `TTFTBuckets`, `AvgQueueTime`, `QueueTimeBuckets`, `QueueMin`, `AvgPrefillTime`, `AvgKVTransferTime`, `TPOTBuckets` |
| `AvgFrontendTime`, `AvgRenderTime`, `RenderRate` (and each engine's of an LWS group) | `AvgHTTPGenerationTime` less `AvgE2ELatency`, `AvgHTTPRenderTime`, `HTTPRenderRate` (the HTTP series carry no model labels: attached by instance to the Pods the model's own series name) |
| `Input.SchedulerQueue`, `Input.ArrivalRate` | `SchedulerQueueSize`, `SchedulerQueueBytes`, `ModelArrivalRate` |
| `Input.RouterLatency` | `SLORouterTTFTBuckets`, `SLORouterTPOTSum`, `SLORouterTPOTCount`, `SLORouterTPOTBuckets`, `FlowControlWaitBuckets`, `SLORouterProcessingBuckets`, `SLORouterProcessingSum`, `SLORouterProcessingCount`, `SLORouterRenderSum`, `SLORouterRenderCount`, `SLORouterPrefixPredicted`, `SLORouterPrefixBestAvailable` (no model labels: joined to the one model the router's TTFT names in the namespace); `Endpoints` and `PrefixProducers` from `SLORouterEndpointPlacements`, `SLORouterEndpointQueue`, `SLORouterEndpointInFlightRequests`, `SLORouterEndpointInFlightTokens` and the five `SLORouterPrefix...` queries (read from the router Pods whose placements name the model alone) |
| `Input.FlowControlPools` | `FlowControlPoolModels`, `FlowControlPoolSaturation`, `FlowControlOutcomes` |
| `Ready`, `ReadySince`, `StartSeconds`, `Revision`; a deleted Pod's series dropped | `kube_pod_status_phase`, `kube_pod_status_ready`, `kube_pod_status_ready_time`, `kube_pod_created`, `kube_pod_start_time`, `kube_pod_deletion_timestamp`, and the ReplicaSet in `kube_pod_owner` |
| Variant, role | `kube_pod_owner` and `kube_replicaset_owner` (the Deployment), `label_llm_d_ai_role` in `kube_pod_labels`; or `--role-of` |
| `CurrentReplicas`, `PendingReplicas`, `DesiredReplicas`; parked variants | the variant's engine Pods in kube-state-metrics (those with a KV series at some step of the window, so a router Pod a name prefix also picks is not counted); `kube_deployment_spec_replicas` |
| `GPUsPerReplica`, `AcceleratorName` | `kube_pod_container_resource_requests{resource="nvidia_com_gpu"}`; `kube_pod_info` joined to `kube_node_labels`; else `--gpus-per-replica`, `--accelerator` |
| Engine parameters | `--engine`, `--engine-args`, `--batch-cap`, parsed by `capacity.ParseEngineArgs`; the prefill role's from `--prefill-engine-args` when given (empty is the engine's defaults, and `--batch-cap` is then the decode role's only), else from `--engine-args` and `--batch-cap` like every role |

## What a capture cannot know

Each of these is either flagged in the capture or fixed by what it writes:

- **Engine parameters.** kube-state-metrics does not export them. They come
  from flags, so the capture always carries `FlagParamsFromFlags`. The
  engine image is a placeholder.
- **Rollouts.** No variant is ever filed as rolling out, and every capture
  says so (`FlagNoRollouts`).
- **Variant states.** A capture fills the replica counts, GPUs per replica,
  role and accelerator. Discovery's other fields (held and filled replicas,
  placements, Kueue) stay zero.
- **Readiness without kube-state-metrics.** Every Pod reads as Ready, with
  no Ready time (`FlagNoReadiness`). A step at which kube-state-metrics
  lists no Pod is read the same way, its Pods attributed by `--role-of`
  alone, and the capture says at how many steps (`FlagKSMGaps`).
- **LeaderWorkerSets.** kube-state-metrics has no owner for them: a Pod
  is read from its labels instead (`workloadOf`). A DisaggregatedSet's Pod
  is variant `<set>-<role>`, as the controller names the role's scaler; a
  LeaderWorkerSet's is the LWS name; `--role-of` overrides both. One
  replica is one group, keyed by the LWS name and group-index labels: its
  GPUs are summed over the group's Pods (`fleet.replicaGPUs`), it counts
  once in the variant's replicas and is pending while any of its Pods is
  not Ready (`pod.replicaKey`), and the collector merges the rows of a
  group whose every Pod runs an engine (`collector.collapseToGroups`).
  Without those labels in kube-state-metrics a variant whose Pods a
  StatefulSet owns is flagged (`FlagMultiPodGroups`). A given
  `--gpus-per-replica` replaces what kube-state-metrics reads
  (`FlagGPUsOverride`).

## Tests

- `internal/whatifsource`: each query into its field (present, absent,
  NaN), the model-level inputs, kube-state-metrics (roles, readiness, GPUs,
  parking), chunked range queries, and deterministic bytes; a failed query
  failing the capture at once, overlapping `--role-of`, replicas counted
  from engine Pods only, the rollout and multi-Pod flags, the step grid,
  the window-size and sample bounds, and whose fault each error is
  (`IsInputError`); the optional series present, absent and unchecked,
  a counter's increase across a restart, and only the captured engine's
  series checked.
- `cmd/capacity-planner`: the flags, usage errors (exit 2), a Prometheus error
  (exit 1, and the capture that was at `-o` kept), the step and the
  window, and `generate`; the bearer token kept off a redirect's host and
  off http on the same host, refused for http or when empty, TLS flags
  refused for http, the `--insecure-skip-verify` and `--batch-cap`
  warnings, the request timeout, `generate`'s exit codes and single error
  prefix, `startOf` reading the caveats, and the flags surviving capture,
  generate, a Fidelity replay and the export.
- `learnreplay` (`TestGenerate_*`): the call order on a hand-built capture,
  refusals, a Fidelity replay of the generated recording, the settle
  tracker forgetting a model parked whole, and the caveats.
- `steadystate` (`TestWhatifCapture_*`): the round trip. An in-process
  controller collects from a fake Prometheus and records. A capture of the
  same series plays through Generate. Every event is compared byte for
  byte: each Analyze's input, result and store records, and the order of
  calls in every cycle. The engine is given variant states with only the
  fields a capture fills. That is the bound the comparison holds to. The
  fleet has a model parked whole partway through the window (the settle
  tracker's verdicts must match the engine's too) and a router Pod in
  kube-state-metrics. The round trip runs again with the window split
  into range queries of five steps, and with a role map by Pod name that
  also picks the router.
