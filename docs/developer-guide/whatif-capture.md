# What-if capture and Generate

`whatif capture` turns a past window in Prometheus into the inputs the
analyzers would have read, and `whatif generate` plays them through the
analyzers as a controller's cycles would have. The output is an ordinary
learner recording. The design is R2 in
[the learner proposal, 2.9](../proposals/analyzer-learner.md#29-recorder-fields-for-ttft-and-capture).
How to run it on a real window is in
[Testing: Capture a past window](testing.md#capture-a-past-window).

```
whatif capture  --prometheus URL --namespace NS (--kube-state-metrics | --role-of ROLE=SELECTOR ...)
                --from T0 --to T1 -o run.capture.jsonl.gz
                [--step 15s] [--policy FILE]
                [--engine vllm|sglang] [--engine-args "ARGS"] [--batch-cap N]
                [--gpus-per-replica 1] [--accelerator NAME]
                [--bearer-token-file FILE] [--ca-file FILE] [--insecure-skip-verify]
                [--timeout 2m30s] [--force]
whatif generate -in run.capture.jsonl.gz -o run.jsonl
```

| Flag | What it does |
| --- | --- |
| `--prometheus` | The Prometheus base URL, http or https. Required. An error message never quotes its userinfo, path, query or fragment: they are replaced by `REDACTED` ([the rules](../proposals/slo-capacity-planner.md#prometheus-mode-as-built-d4)). |
| `--namespace` | The namespace whose models to capture. Required. |
| `--kube-state-metrics` | Read Pods, owners, readiness, GPUs and nodes from kube-state-metrics in the same Prometheus. |
| `--role-of ROLE=SELECTOR` | The Pods a label selector picks serve ROLE (`prefill`, `decode`, `both`), as one variant. Repeat per variant. |
| `--from`, `--to` | The window, RFC 3339 or Unix seconds, truncated to the millisecond. Required. |
| `--step` | The interval between cycles, taken as the run's optimize interval. Whole milliseconds. Default 15s. |
| `--policy` | A scaling-policy entry (YAML), applied as the default entry. Default: the defaults. |
| `--engine` | The inference engine the variants run, `vllm` or `sglang`. Default `vllm`. |
| `--engine-args` | The engine's arguments, space-separated, parsed for its parameters (kube-state-metrics has none). |
| `--batch-cap` | The engine's batch cap (`--max-num-seqs`, or `--max-running-requests`). Warned about when `--engine-args` sets it too. |
| `--gpus-per-replica`, `--accelerator` | Stand-ins where kube-state-metrics gives no GPU request or node label. |
| `--bearer-token-file` | A file holding a bearer token. Sent over https only, to Prometheus' own host only (compared without case), never on a redirect elsewhere. Refused with an http URL, or when the file is empty. |
| `--ca-file` | A PEM file of the CAs that sign an https Prometheus' certificate, trusted in place of the system's: for a Prometheus behind a private CA (an OpenShift service CA, say). Refused with an http URL. |
| `--insecure-skip-verify` | Do not verify the certificate. Warned about on stderr. Refused with an http URL. |
| `--timeout` | How long one HTTP request may take. Default 2m30s, a little over Prometheus' own 2-minute query timeout. |
| `--force` | Capture past the load and memory bounds below. |
| `-o` | Where to write the capture. Written beside it and renamed over it when whole, so a failed run leaves the file that was there. |

`whatif generate` takes `-in` (the capture, gzipped or not) and `-o` (the
recording; a name ending in `.gz` is gzipped). Both commands write their
file with mode 0600, through a temporary file renamed over it.

`make whatif` builds `bin/whatif`, with the commit in the build. The command
is not in the manager image.

To answer the planner's questions from a window without writing either
file, give the same flags to `whatif replicas-to-slo` or `whatif
slo-to-rate` in place of `--recording`: they capture and generate in memory
along this same path, and answer as `--recording` would on the files.
[Prometheus mode](../proposals/slo-capacity-planner.md#prometheus-mode-as-built-d4)
in the planner's CLI reference has the details.

Exit codes: 0 done; 2 the caller's input (a bad flag, invalid options,
overlapping `--role-of` selectors, no Pod the role map attributes with a
KV-cache series, a window too large without `--force`, a capture `generate`
cannot read; `whatifsource.IsInputError`); 1 anything else (a failed
Prometheus query, `whatifsource.ErrQueryFailed`, or a `generate` that fails
inside the analyzers).

## Asking for TTFT

The answer commands price a P/D fleet's TTFT when a TTFT flag is given:

```
whatif replicas-to-slo --recording run.jsonl --ttft-percentile 90 [--assume-vp TOKENS] [--assume-a D] [--prefill N]
whatif slo-to-rate     --recording run.jsonl --tpot 4ms --ttft 200ms@p95 [--assume-vp TOKENS] [--assume-a D] [--prefill N]
```

`--prometheus` takes the same flags, and gives the same answer.

- **Treat every TTFT figure as unvalidated.** Each prints `UNVALIDATED`, and it
  is not a bound. At the assumed prompt `cv` 2, which is what the CLI prints
  for rows with no computed-token histogram, it read 1.1x to 4.1x the router's
  percentile on the P2 recordings; at the spread the load generator sent
  (`cv` 0.06) up to 33% under it. Do not size from it yet.
- **Read the `assumed` row first.** It names each input the recording did not
  give. `V_P ASSUMED` means `--assume-vp` supplied `V_P`, and `a ASSUMED` that
  `--assume-a` supplied `a`.
- **An assumed term can decide the answer.** The `assumed` row says so when one
  alone refuses a target, or moves the answer by 25%. `--assume-router-hop 8ms`
  supplies the hop between the router and the engines.
- **`--assume-vp` overrides,** as every `--assume-*` flag does. Give it when
  `NoPrefillCapacity` is refused, because the pods never queued long enough for
  `V_P` to be learned, or to ask what another `V_P` would do. A learned figure
  is printed beside it: `V_P 130000 ASSUMED, learned 115621`.
- **A decode row can rest on `V_P` too.** Where the prefill knee is the lower,
  it sets the operating rate, and the rate and TPOT rows say so and carry the
  verdict.
- **`NoFixedLatency` is a fleet of one prompt length.** `a` is learned from
  low-load minutes at more than one length. Give `--assume-a 11ms` for a flagged
  answer: the learned figure, if any, prints beside it.
- **Read the band as a sweep.** The knee, each TTFT and each TTFT rate print the
  figure at `V_P` and `f` 15% either way. It is not a confidence interval. A
  `Saturated` in it means the rate has no 15% to spare.
- **`Saturated` means the rate is at or above the knee.** It is a refusal, exit
  `3`, not a figure.
- **Aggregated fleets and per-bucket answers are refused:** the planner's M1.

Exit codes are `0`, `3` for a refusal, `2` for a usage error and `1` for an
internal one. The inputs, the rows and the JSON are in
[TTFT, as built](../proposals/slo-capacity-planner.md#ttft-as-built).

## The joint TTFT model

```bash
whatif joint-ttft --recording run.jsonl [--cycle N|last] [--model M] [--prefill N] [--decode N] [--rate R] [--prompt I] [--hop D] [-o json]
```

Prices the joint prefill and decode model's mean TTFT at a fleet, from the
inputs the recording's rows support at the cycle: the fleet and rate that ran
by default, or the ones the flags give. The hop is assumed (20 ms, flagged
`RouterHopAssumed`) unless `--hop` states it. A refusal (`no-prefill-capacity`
until a prefill pod has read an empty queue, `saturated`, `batch-unheld`, and
the rest) is printed and exits `3`. The JSON is `schemaVersion` 1, its keys
lowerCamel.

The inputs are fed as the slo analyzer feeds them (only one prefill and one
decode variant, swept on a change of accelerator or engine arguments, the
prompt read SGLang's way on SGLang) with one difference: no settle tracker, so
a replica's first two minutes after a start or an idle spell are read too. See
[As built (J1)](../proposals/slo-capacity-planner-ttft.md#as-built-j1).

## Reading the line check

Every answer on an ITL line prints a `line check` row: the token rate the line
predicts against the rate the recording's decode rows reported, off a ramp.

- **`MISMATCH`** is a median error above 15%, flagged `LineMismatch` on every
  answer on that line. Do not size from it: the line or the shape is wrong, and
  the check cannot say which.
- **`at another shape`** is a verdict on rows whose KV a request is more than 20%
  from the one priced. Both figures print. It says nothing of the traffic you asked
  about.
- **`not run`** is under 5 rows to compare. The row counts where the others went:
  on a ramp, above the band, or older than 30 minutes. Rows below fill 0.30 are not
  counted. It is not a pass.

[The rules and the figures on the goldens](../proposals/slo-capacity-planner.md#the-line-check-as-built).

## Load on Prometheus

A capture turns every query the controller asks into one range query over
the whole window, at the step. A window of more than 10,000 steps is asked
in parts of at most 10,000 steps, one part after another. The collector
asks a cycle's queries together, so different queries run at the same time,
as many as a controller's cycle asks at once; each is asked once for the
whole capture, and every later step reads its answer from memory.

A capture registers about 80 queries with kube-state-metrics (67 without);
one namespace with a single vLLM model asks about 56 of them. For a fleet
of 10 engine Pods, each per-Pod query returns 10 series, and each histogram
about 10 to 20 per Pod (one per bucket), roughly 1,000 series in all. Rough
figures, to be read as orders of magnitude:

| Window at 15 s | Steps | Range requests | Samples returned and held |
| --- | --- | --- | --- |
| 1 hour | 241 | about 56 | about 240,000 |
| 1 day | 5,761 | about 56 | about 5.8 million |
| 3 days | 17,281 | about 112 (two parts each) | about 17 million; past `MaxStepQueries`, so it needs `--force` |

Prometheus reads far more raw samples than it returns: each `rate(...[1m])`
reads every scrape in its minute at every step.

- **Capture off-peak**, or better, **from a snapshot** served by a local
  Prometheus ([Testing](testing.md#capture-a-past-window)), never by loading
  a production server with a long window.
- **The usual failure is `query.max-samples`**: a histogram over many steps
  loads more samples than the server allows in one query. It stops the
  capture with exit 1 and names the query; shorten the window or lengthen
  the step.
- **Bounds.** `whatifsource.MaxStepQueries` (1,000,000) steps times
  registered queries is checked before any query: about 12,500 steps with
  the 80 queries, a little over two days at 15 s. `whatifsource.MaxSamples`
  (20 million, a little over a gigabyte at about 60 bytes each) bounds the
  samples held, counted as the answers arrive. Past either, the capture
  stops with exit 2 unless `--force`.

## Talking to Prometheus

- Every HTTP request has a timeout, `--timeout`.
- **Port-forwarding to an authenticated Prometheus:** `kubectl port-forward`
  gives a plain-http local URL, and the token is never sent over http, so
  the capture is refused. Forward to Prometheus' https port and capture
  from `https://localhost:PORT` with `--ca-file` (the certificate must name
  `localhost`, or use `--insecure-skip-verify` knowing what it gives up), or
  capture from a local snapshot, which needs no token.
- Every sample must be on the step grid, `--from` plus a whole number of
  steps. A query frontend that aligns start to step (Thanos, Mimir) answers
  off it, and the capture fails, naming the cause.

**Every query is required.** One that fails stops the capture at once and
names the query; the collector's retries do not sleep through their
backoff on an answer that cannot change. A failed query would leave its
field absent at every step, which a capture cannot tell from a field the
window did not have, so the generated run would read inputs no controller
read.

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
  parameters (`capacity.EngineParams`).
- `capture.Cycle`: `learnreplay.CaptureCycle`. It holds one model's
  `learnrec.Input` at one step (rows in variant and Pod order, the variants'
  states, the merged policy, the scheduler queue, the arrival rate, the
  router latency and the flow-control pools), and the model's parked
  variants.

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
| `NumGpuBlocks`, `BlockSize`, `TotalKvCapacityTokens`, `EngineRanks`, `TokensInUse` | `CacheConfigInfo` |
| Length means; `OutputTokensBuckets`, `AvgOutputTokensSquared` | `AvgInputTokens(Recent)`, `AvgOutputTokens(Recent)`, `OutputTokensBuckets` |
| `PrefixCacheHitRate` | `PrefixCacheHitRate` |
| `AvgITL` with `AvgITLKnown` and `AvgITLEmpty` (NaN), `AvgTTFT`, `AvgServiceTime` | `AvgITL`, `AvgTTFT`, `AvgServiceTime` |
| `PrefillComputedTokenRate`, `GenerationTokenRate`, `RequestRate`, `KvUsageInstant`, `KvUsageMean` | the queries of the same names |
| `RunningRequests`, `PreemptionRate`, `SpecAcceptedPerStep` | the queries of the same names |
| R1's TTFT inputs: `PromptTokensBuckets`, `TTFTBuckets`, `AvgQueueTime`, `QueueTimeBuckets`, `QueueMin`, `AvgPrefillTime`, `AvgKVTransferTime`, `TPOTBuckets` | `PromptTokensBuckets`, `TTFTBuckets`, `AvgQueueTime`, `QueueTimeBuckets`, `QueueMin`, `AvgPrefillTime`, `AvgKVTransferTime`, `TPOTBuckets` |
| `Input.SchedulerQueue`, `Input.ArrivalRate` | `SchedulerQueueSize`, `SchedulerQueueBytes`, `ModelArrivalRate` |
| `Input.RouterLatency` | `SLORouterTTFTBuckets`, `SLORouterTPOTSum`, `SLORouterTPOTCount`, `SLORouterTPOTBuckets`, `FlowControlWaitBuckets` |
| `Input.FlowControlPools` | `FlowControlPoolModels`, `FlowControlPoolSaturation`, `FlowControlOutcomes` |
| `Ready`, `ReadySince`, `StartSeconds`, `Revision`; a deleted Pod's series dropped | `kube_pod_status_phase`, `kube_pod_status_ready`, `kube_pod_status_ready_time`, `kube_pod_created`, `kube_pod_start_time`, `kube_pod_deletion_timestamp`, and the ReplicaSet in `kube_pod_owner` |
| Variant, role | `kube_pod_owner` and `kube_replicaset_owner` (the Deployment), `label_llm_d_ai_role` in `kube_pod_labels`; or `--role-of` |
| `CurrentReplicas`, `PendingReplicas`, `DesiredReplicas`; parked variants | the variant's engine Pods in kube-state-metrics (those with a KV series at some step of the window, so a router Pod a name prefix also picks is not counted); `kube_deployment_spec_replicas` |
| `GPUsPerReplica`, `AcceleratorName` | `kube_pod_container_resource_requests{resource="nvidia_com_gpu"}`; `kube_pod_info` joined to `kube_node_labels`; else `--gpus-per-replica`, `--accelerator` |
| Engine parameters | `--engine`, `--engine-args`, `--batch-cap`, parsed by `capacity.ParseEngineArgs` |

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
- **LeaderWorkerSets.** kube-state-metrics has no owner for them. Use
  `--role-of`. A replica spans several Pods, but GPUs per replica are read
  from one Pod's request; a variant whose Pods a StatefulSet owns, or that
  carry the LeaderWorkerSet labels, is flagged (`FlagMultiPodGroups`).

## Tests

- `internal/whatifsource`: each query into its field (present, absent,
  NaN), the model-level inputs, kube-state-metrics (roles, readiness, GPUs,
  parking), chunked range queries, and deterministic bytes; a failed query
  failing the capture at once, overlapping `--role-of`, replicas counted
  from engine Pods only, the rollout and multi-Pod flags, the step grid,
  the window-size and sample bounds, and whose fault each error is
  (`IsInputError`).
- `cmd/whatif`: the flags, usage errors (exit 2), a Prometheus error
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
