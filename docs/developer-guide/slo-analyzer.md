# The slo analyzer and the shared ITL tracker

How the `slo` analyzer works, for whoever changes it. Operators read
[`slo` (observe only)](../reference/scaling-policy.md#slo-observe-only); the
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
| `internal/signals/whatif` (`ttft.go`, `ttft_ps.go`, `ttft_term.go`, `ttft_queue.go`) | the TTFT pricing S1 runs in shadow (`ttft.go` here builds the fleet): `TTFTAt`, `RateForTTFT`. With `TTFTFleet.ProcessorSharing` (switch `slo-ttft-processor-sharing`, on) the prefill pods and the KV pull are processor-sharing stages, `prefillPlan.psStages`, and `D`'s per-token part is net of the pull (`netOfPull`); past a prompt cv of `MaxPSPromptCV` (0.25) a price is the larger of that and first come (`ttftPlan.price`, flagged `SpreadBeyondProcessorSharing`); off, a first-come wait on the busiest pod (`prefillPlan.engineWait`). The model is [the TTFT page](../proposals/slo-capacity-planner-ttft.md#as-built-processor-sharing) |
| `internal/signals/itl` (`tracker.go`) | `Tracker`: the ITL windows and intercepts, shared with the saturation analyzer |
| `internal/metrics` (`slo_metrics.go`) | `PublishSLO`, `SweepSLO`, `DeleteSLOModel` |
| `internal/engines/steadystate` | the engine's calls into it (below) |

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
       the saturation analyzer's derived mu and with the what-if planner;
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
