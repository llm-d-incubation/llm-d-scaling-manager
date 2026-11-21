# The slo analyzer and the shared ITL tracker

How the `slo` analyzer works, for whoever changes it. Operators read
[`slo`](../reference/scaling-policy.md#slo-observes-bounds-decode-packing); the
design and its open questions are in the
[proposal](../proposals/slo-tiers.md).

On this release the analyzer **observes**. It runs, publishes `llmsm_slo_*` and
its own `llmsm_analyzer_demand` / `llmsm_analyzer_target`, and no replica count
reads its result: `collectV2ModelRequest` (`internal/engines/steadystate/engine_v2.go`)
hands the optimizer `namedResults[0]`, saturation's.
`TestListingSLOLeavesTheDecisionUnchanged` pins that with the real analyzer.

## Where the code is

| package | what |
| --- | --- |
| `internal/config` (`slo.go`) | `SLOPolicy`, `SLOObjective`, `SLODuration`, `SLOPercentile`; `Validate`, `SLOSummary`, `SLOMisconfiguration` |
| `internal/collector` (`router_latency.go`), `registration/slo.go` | `CollectRouterLatency`: the router's TTFT histogram and TPOT `_sum`/`_count`, per model, over 5 minutes, for each model whose policy lists the analyzer. While a learner recording is on, the engine also calls it for every model every cycle, and it adds the router's TPOT histogram and the flow-control wait histogram (`registration/flow_control_pools.go`), which only the recording reads |
| `internal/engines/analyzers/slo` | the analyzer: `analyzer.go` (cycle, fleet, line, judging, state), `histogram.go` (quantile, attainment, sample minimum), `transient.go` (exclusion) |
| `internal/signals/whatif` (`decode.go`) | the TPOT pricing: `PriceTPOT`, `ReachableFill`, `ReplicaTokenRate`, `RealisedFill`, `FillBand`; shared with the saturation analyzer's derived mu (`mu.go`) |
| `internal/signals/whatif` (`ttft.go`, `ttft_ps.go`, `ttft_term.go`, `ttft_queue.go`) | the TTFT pricing S1 runs in shadow (`ttft.go` here builds the fleet): `TTFTAt`, `RateForTTFT`, and the switches listed under the table. The model is [the TTFT page](../proposals/slo-capacity-planner-ttft.md#as-built-processor-sharing) |
| `internal/signals/itl` (`tracker.go`) | `Tracker`: the ITL windows and intercepts, shared with the saturation analyzer |
| `internal/metrics` (`slo_metrics.go`) | `PublishSLO`, `SweepSLO`, `DeleteSLOModel` |
| `internal/engines/steadystate` | the engine's calls into it (below) |

The TTFT pricing's switches, each a `TTFTFleet` field and a replay switch:

- `ProcessorSharing` (`slo-ttft-processor-sharing`, on): the prefill pods and
  the KV pull are processor-sharing stages (`prefillPlan.psStages`), and
  `D`'s per-token part is net of the pull (`netOfPull`). Past a prompt cv of
  `MaxPSPromptCV` (0.25) a price is the larger of that and first come
  (`ttftPlan.price`, flagged `SpreadBeyondProcessorSharing`). Off, a
  first-come wait on the busiest pod (`prefillPlan.engineWait`).
- `PriceRouterWait` (`slo-ttft-router-wait`, opt-in, off, the CLI's
  `--price-router-wait` default): above the prefill onset, up to
  `MaxRouterWaitKneeShare` of the knee, prices the flow-control gate as a
  stage (`whatif.newGateStage`; `TTFTParts.RouterWait` and `BurstWait`) and
  trades `AboveFlowControlOnset` for `FlowControlWaitPriced`, so the verdict
  there is read from the figure. Validated at one held staircase step; it
  stays off until a cluster staircase with several held steps past the onset
  passes
  ([the router stage](../proposals/slo-capacity-planner-ttft.md#as-built-the-router-stage)).
- `PSOnsets` (`slo-ttft-ps-onsets`, off, the CLI's `--ps-onsets` default):
  places the prefill onset on the processor-sharing stage
  (`prefillPlan.psMeanScore`, `ttftPlan.placement`) at `Prefill.RunningCap`.
  The analyzer leaves the cap at 0 (unknown), which takes the lower of that
  onset at a cap of 1 and the first-come one.
- `ChargeDecodeGate` (`slo-ttft-decode-gate-charge`, off, the CLI's
  `--charge-decode-gate` default): charges the decode gate's closed share
  (`ttftPlan.gateClosed`) to `RateForTTFT` alone, bound
  `BoundDecodeGateCharge`, which `decodeBound` does not read as decode-bound.
- The last two are replay-only in the analyzer: `SetSwitchesForReplay` is
  the only way on, and no policy key sets them.

One more switch sets an input, not a `TTFTFleet` field:
`slo-ttft-imbalance-across-counts` (off, `whatif.DefaultImbalanceAcrossCounts`;
the capacity planner's `--imbalance-across-counts` is on by default,
`whatif.DefaultPlannerImbalanceAcrossCounts`, issue #300). It sets `f` at a prefill pod
count P1 has not learned (`ttftInputs.imbalanceAcross`,
`whatif.ImbalanceReadings.At`). Off, `min(N, max(1.5, f))`. On:

- A count P1 learned keeps its own `f` (`prefill.Estimate.FByPods`), as P1
  gave it.
- Any other count reads `f(n) = 1 + c·g(n)`. `g(n)` is the expected max of
  `n` standard normals (`prefill.NormalMaxMean`). `c` is the population
  spread of the pods' shares, with no `sqrt(n/(n-1))` correction. It is
  fitted through `f = 1` by least squares, each count weighted by its minutes
  (`FSamplesByPods`).
- The current count's raw share, when P1 has not learned that count, is one
  more point of 5 minutes. It only raises `c`, so a hot pod now reaches
  `N+1`. At the current count, `f` is at least the raw share.
- With no count learned, `f` is at least `whatif.FreshImbalance` (1.15).
- `routing()` reads the raw share as before.
- Every `f` not learned is flagged `AssumedImbalance`. The log's `fSource`
  says `learned`, `fitted from counts {2}` (with `and the raw share at 3`),
  `fitted from the raw share at 3`, `default`, `raw share` or `one pod`.

Known gaps (the first blocks flipping the default):

- P1's minutes live in memory for 6 h and are not in the learned state. Right
  after a restart, nothing is learned, and the switch prices 1.15 (or the raw
  share's curve) where off prices at least 1.5.
- P1 reads one share per row. A data-parallel pod, or an LWS group whose
  every Pod runs an engine, is one row of `k` engines, and the spread inside
  it is not seen: about `c/sqrt(k)` per row, so `f` reads low (about 1.05
  for a true 1.20 at `k = 4`). Such an `f` stays flagged `AssumedImbalance`
  (`prefill.Estimate.FMultiRank`). A per-engine fit, `g(N·k)`, is a
  follow-up.

It is replay-only too.

`slo-ttft-low-load-decode-ttft` (off) sets the decode engine's measured TTFT,
which prices the decode side when no decode line does
(`whatif.TTFTFleet.DecodeTTFT`). Off, it is the largest of the last five
cycles (`noteDecodeTTFT`). After an overload that is the queue's wait: 17-21 s
on `ttft-staircase-b-off` and 6-12 s on `ttft-staircase-c-on`, against 0.15
and 0.23 s calm. On:

- Each cycle carries the decode rows' capacity backlog
  (`prefill.DecodeBacklogOf`), held for `prefill.DecodeTTFTLag` after the
  queue drains (`prefill.DecodeBacklogCarry`). The capacity planner reads the
  same rule.
- The figure is the largest of the window's cycles with no backlog
  (`decodeTTFTWindow.lowLoad`).
- With none, the latest such figure is held for `prefill.HeldDecodeAge`
  after the last window that held it, then the window's largest stands, as
  off.
- A decode row with no capacity series (an older vLLM, SGLang) cannot say
  it is calm, and the mean holds its TTFT. While the window holds a cycle
  with such a row, the figure is the window's largest, as off, and any held
  calm figure is dropped. The cycle also carries a backlog of 1, for
  `prefill.DecodeTTFTLag` like any other. The planner only flags such rows
  (`BacklogUnread`).

The planner refuses a decode TTFT read under a backlog
(`whatif.DecodeTTFTUnderLoad`) and names a calmer cycle to answer at. The
analyzer cannot answer at another cycle, so it holds a calm figure instead. Replay-only;
`TestGoldens_TTFTLowLoadDecodeTTFTOnTheStaircases` replays the staircases
both ways.

`slo-ttft-sizes-prefill` (off, replay-only, `ttft_prefill_ask.go`) sizes a
prefill variant on TTFT
([#131](https://github.com/llm-d-incubation/llm-d-scaling-manager/issues/131)).
Off, a prefill variant is `no-data`, and a short-prompt prefill that never
queues is not priced at all: P1's `a` needs low-load minutes over a spread of
prompt lengths, which one prompt length run above low load never gives. On:

- Where P1 refused `a`, the joint window's `a₀` stands
  (`withJointFixedLatency`; `aSource` on the `slo-ttft-variant` line).
- For a prefill variant whose reading is `priced`, the ask is the least pod
  count the model prices under the target, each count at its own `f`
  (`prefillAsk`), searched up to 4 times the Ready pods; past that nothing is
  asked. `PerReplicaCapacity` is the arrival rate over the ask (requests per
  second), reason `slo-ttft`, and the role's demand is the arrival rate
  (`sizePrefill`). A model with several serving prefill variants is not
  sized: their asks are never priced together. Decode-bound, routing-bound
  and refused readings ask nothing.
- `a₀` comes from the joint window, kept only for one prefill and one decode
  variant, with `slo-ttft-joint-inputs` on. The engine's capacity check reads
  the role's demand against its scale-up threshold, as it does for decode.
- On the staircase goldens only `a-off` is sized, at one replica. `b-off`,
  the issue's case, is below the floor at the assumed prompt cv of 2: no
  count meets 190 ms, so nothing is asked.
  `TestGoldens_TTFTSizesPrefillOnTheStaircases` holds both. In one
  unrecorded replay at a measured cv of 0.1 (no test holds it), the steps that missed (30-35 r/s) asked a
  median of 2-4 replicas, at most 5, where saturation asks 2; past 37 r/s the
  ask was 5-8, or none past the bound, as the decode engine's TTFT nears the
  target.

It stays off until a cluster run settles whether a third prefill replica
holds the target on that shape.

The analyzer's met/unmet is `whatif.TTFTMeets`. `RateForTTFT`, which the
CLI's and the planner API's rates and the analyzer's published rate come
from, is capped under the prefill onset (`BoundPrefillOnset`), so it answers
only rates that verdict calls met. The CLI and the API publish no verdict of
their own; they carry the flags it reads.

**Behaviour change with the TTFT recorder (R1), recording on or off.** A
router histogram bucket that Prometheus returns as NaN or Inf used to read
as a bucket of rate 0: the Prometheus source stores a non-finite sample as 0
and flags it, and `modelValues` checked the value alone. It now reads the
flag (`source.MetricValue.Finite`) and drops the bucket. So a window with
non-finite buckets can be judged differently after an upgrade:

- a TTFT histogram whose buckets are all non-finite used to read as a
  model that served nobody, or as a reading; it now has no buckets, so the
  objective is unjudged, and an SLO judged "met" can read "unjudged";
- one non-finite bucket among finite ones used to enter the histogram as a
  zero at its bound; it is now left out, and the quantile is taken from the
  others.
The TPOT `_sum`/`_count` pair reads the same rule; a non-finite one already
read as no reading.

The slo analyzer judges only a `RouterLatency` that `Judgeable` says is a
reading (a TTFT histogram or a TPOT request rate). While a recording is on,
`CollectRouterLatency` can return one holding only the recorded histograms;
the analyzer treats it as the nil it was before they existed.

## One cycle

1. `optimize` (`engine.go`) calls `beginSLOCycle` once it is past its error
   returns: on the no-model return, and after the inventory check. A failed
   cycle and its retry are one cycle.
2. Per model, `resolveModelPolicy`, then `runAnalyzers`. The analyzer
   runs only where the policy lists it (`AnalyzerEnabled`).
3. `Analyze`:
   - `readFleet`: Ready, not warm-pool rows; the request-weighted shape
     (`shape.Shape`, so `KVreq = ILeff + O/2`, or `ILeff + D·O/2` when every
     row it counts read the output-length bound, switch
     `slo-kv-per-request-output-dispersion`, `output_dispersion.go`); the demand, `λ × O` plus the
     router's backlog over `backlogDrainSeconds`.
   - Per decode or aggregated variant, `priceVariant`:
     - `variantKey` keys the line on `windowKey` with `stableAccelerator`;
     - `fitLine` calls `Tracker.Observe` and names the source: `own`,
       `pinned`, `pinned-default`, or with no line `declined` or `no-line`;
     - the pricing is `internal/signals/whatif`'s (`decode.go`), shared with
       the saturation analyzer's derived mu and with the capacity planner;
     - `whatif.PriceTPOT` inverts `ITL = A·k + B` at the target, clamps to
       the policy's band (`whatif.FillBand`, `[0.15, 0.80]` by default), or
       says `infeasible` when the ITL at its bottom already reaches it;
     - `whatif.ReachableFill` caps the fill where the replica's batch cap
       binds: `max_num_seqs` times the data-parallel ranks the rows report
       (`fleetReading.engineRanks`),
       `whatif.ReplicaTokenRate` is the per-replica capacity there, and
       `whatif.RealisedFill` the fill the fleet would run at after `scaleUpThreshold`
       headroom, where `PredictedITL` is read.
   - `judge` reads the router: the TPOT mean as `_sum ÷ _count` (both or
     nothing); the TTFT percentile and attainment from `newHistogram`, only
     at or above `minSamples` (`max(50, 1000/(100-p))`).
   - `noteTransient` decides, per objective, whether the window is judged:
     `unobserved` when the model was not analyzed on the previous cycle
     (`a.cycle > m.analyzedCycle+1`, or first sight); `zero-replicas` or
     `starting` from `transientOf`; held for the window plus `transientGrace`.
     `boundPending` stops counting a variant with no Pod ages (a multi-Pod
     LeaderWorkerSet group) as starting after 30 minutes pending; an
     unobserved cycle clears its pending time.
   - `PublishSLO` sets this cycle's series and deletes the ones it no longer
     holds.

## The engine's hooks

The engine holds the analyzer as `sloAnalyzer`, an interface
(`sloStateKeeper`: `BeginCycle`, `NoteVariantsPresent`, `EvictStale`,
`ForgetVariant`), and calls:

- `beginSLOCycle` (above);
- `forgetOnRequest`: when the saturation analyzer acts on a
  `llm-d.ai/forget-learned-state` token, `ForgetVariant` drops the variant's
  line under every key (window, steady window, traces, intercept, held slope),
  the engine arguments it was learned under, and the variant's accelerator
  memo, and logs each key it forgot on the caller's logger
  (`logging.ForgottenKeyMessage`). The replay applies the recorded
  `slo.ForgetVariant` (`TestReplay_TheSLOForgetIsApplied`). The token is the saturation analyzer's;
  this analyzer keeps none. It walks `learn.Learners` and forgets `learn.ITL`,
  its own instance of the line (`forget.go`, issue ev-shindin/llm-scaling-manager#431, which also names
  the maps a forget keeps). Replay switch `slo-itl-forget-reaches-the-line`;
  off, nothing is forgotten here;
- `notePresentVariants`: `NoteVariantsPresent` stamps a parked variant's line,
  accelerator memo and transient memo, so parking does not age them out;
- `evictStaleLearnedState`: `EvictStale` forgets what was not analyzed or
  present for `StateTimeout`, which `NewEngine` sets from the saturation
  analyzer's variant timeout (`LEARNED_STATE_VARIANT_TIMEOUT`). A cycle with no
  model does not reach it. With no engine settle view, the analyzer's own
  tracker does not wait for it: `observeOwnSettle` reads a model last analyzed
  more than `StateTimeout` ago as first-seen, backdated, with its line keys'
  replica traces dropped, whether `EvictStale` ran first, between two models,
  or not at all (issue ev-shindin/llm-scaling-manager#459). Backdated means one observation, at the cycle,
  of replicas `settle.Joined` `itl.DefaultSteadyFor` earlier, but never
  before a Pod's own Ready time, or a bridge's lend time
  (`settlerow.ObserveOwnSince`, issue ev-shindin/llm-scaling-manager#511); two observations that far apart
  would read the gap as an absence (`settle.Rejoined` at the cycle). The
  sighting holds only the row read at the cycle: its trace is one sample and
  an idle run starts at one row. The throughput analyzer's own tracker does
  the same, and at an optimize interval of 48 s or more, where its tolerance
  covered the gap, its trace and `IdleRows` now start at 1, not 2 (replay
  switch `throughput-itl-own-settle-backdates-the-first-sighting`). That is
  the settle reading only: the line's
  windows, intercepts and held slope still go when `EvictStale` sweeps them.
  Replay switch
  `slo-itl-own-settle-reads-a-long-gap-as-first-seen`; off, a model
  `EvictStale` did not evict reads the gap as an absence and settles;
- `metrics.SweepSLO` every cycle, and `metrics.DeleteSLOModel` in
  `pruneAnalyzerSeries` and `evictAllAnalyzerSeries`.

The name `slo` is reserved: `reconcileExternalAnalyzers` does not run an
external analyzer by that name.

## `itl.Tracker`

One copy of the ITL bookkeeping, used by both analyzers, each with its own
instance and its own keys.

- **A reading** is a row of the variant that is Ready, not lent by the warm
  pool, with a finite positive ITL and `0 < k ≤ 0.80`. `Reading` counts each
  reason a row was turned away; `Offered` the readings the window kept (logged
  as `kept`), `BelowBand` those it dropped below `k = 0.15`. A variant whose
  replicas all run above 0.80 (or below 0.15) stops filling its window, and
  what it holds ages out within 30 minutes. A key that has fitted then holds
  its last `A` (`Reading.SlopeHeld`) and still reads `pinned` or
  `pinned-default`; `slo-variant` logs `lineSlopeHeld` and `lineSlopeAge` to
  tell it from a fresh fit. The hold ends when the slope is older than
  `WithSlopeHoldFor` (the saturation analyzer's `BucketRetention`, 7 days by
  default) or the engine's arguments (`EngineParams.ArgsTag`, the tag the
  saturation analyzer compares too) differ from the ones it was fitted under.
  An arguments change also forgets the key's window, intercept and slope
  (`linesUnderArgs`, like the saturation analyzer's `itlUnderArgs` but with
  no stash), so the old engine's readings are not refitted and held as the
  new one's. A key that never fitted reads `no-line`, with
  `aboveBand` or `belowBand` counting why.
  The exception is a replica at its sequence cap
  (`Admission.atSequenceCap`): `RunningRequests` exactly
  `max_num_seqs` times its ranks, with `max_num_seqs` read from the
  engine's flag, `QueueLength` above zero, and a known `PreemptionRate`
  at most `analyzerTuning.itlBand.capMaxPreemptionRate`. Its reading above the band is kept and
  counted in `Capped` (issue ev-shindin/llm-scaling-manager#287). `Observe` takes an `itl.Admission`:
  the policy's band (`analyzerTuning.itlBand.minObservableK`, `analyzerTuning.itlBand.maxObservableK`), the cap and
  the allowance. The zero value is the default band with no cap.
- **The window** (`Window`) holds at most 20 readings for at most 30 minutes.
  Nothing clears it on a change of shape.
- **The fit.** With 10 **steady** readings spanning 0.30 of `k`, a
  least-squares `Fit`, and its `B` remembered when positive. A reading is
  steady when its replica's `k` stayed within 0.10 over the last two minutes
  (`DefaultSteadyFor`, `DefaultMaxKDrift`) and, if the replica joined after the
  key was first observed, it has served that long: across a step in load the
  instantaneous `k` and the `rate[1m]` ITL describe different moments. The
  steady readings outlive the window's oldest by up to five minutes
  (`DefaultSteadyCarry`), so a step's old level is still there when the new
  one settles. Otherwise, with 10 readings,
  `FitPinnedB` with `B` held at the remembered intercept or, before there is
  one, `DefaultBaselineSec` (6 ms), over the steady readings when there are 10
  of them and over every reading only when there are not. It fits by least
  absolute deviations, so a replica's first readings after it starts serving,
  far above the line, do not move `A`. A balanced fleet spans little of `k`, so the
  pinned fit is the normal case. A pinned fit that refuses the readings is
  `Declined`.
- **Sweeping.** `Sweep(now, stale)` drops windows and intercepts whose key the
  caller calls stale; the callback runs under the tracker's lock. The caller
  stamps keys on its own clock.
- **Restart.** Saturation restores intercepts from its learned-state snapshot
  (`RestoreBaseline`). The slo analyzer restores nothing, so after a restart
  its lines are `pinned-default` until the fleet spans `k` again.

## Answering the two common questions

- *Why was a model's window not judged at 10:05?* `llmsm_slo_excluded` says the
  reason. At `--v=3` (the default is 2) the `slo-transient` line says when each mark was set, for
  how long it holds, the cycles since the model was last analyzed, and when
  each variant began pending.
- *What fill did a variant price at, from which line?* The `slo-variant` line:
  `lineKey`, `lineA`, `lineB`, `lineTier`, the rows `considered`, the readings the
  window `kept` and `held`, `scaleUpThreshold`,
  `targetFill` before the clamp, and the batch cap's inputs. `batchCap` and
  `batchCapSource` (`flags`, `declared`, `default`, `none`) are the cap per
  rank the replica was priced at; `maxNumSeqs` is `--max-num-seqs` alone,
  so it stays the parser's 256 where the flags are unread even when a
  declared cap priced the replica. `lineBatchCap` and `lineBatchCapSource`
  (`flags`, `declared`, `none`) are the cap the line's admission read a
  replica as at; `none` takes no reading above the band as capped.

## Tests

- Unit specs in `internal/engines/analyzers/slo` (Ginkgo, `TestSLO`) and
  `internal/signals/itl` (`TestITL`); both run under `-race` in `make test`.
- Engine wiring: `engine_slo_wiring_test.go`, plain Go tests.
- Alerts: `test/alerts/testdata/slo-alerts.test.yaml`, run by
  `hack/check-alert-rules.sh` under promtool.
- A change should come with a negative control: the fix reverted alone fails
  a named test. A mutation "caught" only by the steadystate Ginkgo suite
  (`TestSaturationEngine`) with no spec named is not caught: that suite flakes
  under load.
