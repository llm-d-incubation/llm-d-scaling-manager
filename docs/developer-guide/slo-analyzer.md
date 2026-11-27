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
  (`ttftPlan.price`, flagged `SpreadBeyondProcessorSharing`), unless the
  longest prompt is at most `MaxPSPromptSupport` (1.6) times the mean, at
  `Ca²` up to 1, with `slo-ttft-prompt-support` on
  (`Prefill.BeyondProcessorSharing`). Off, a first-come wait on the busiest
  pod (`prefillPlan.engineWait`).
- `PromptSupport` (`slo-ttft-prompt-support`, planner-only, off from the
  constructor): the spread carries the longest prompt its histogram allows
  over the mean (`PromptSpread.MaxRatio`, the highest bucket bound holding
  requests), priced as `Prefill.PromptMaxRatio`: the prompt's gamma is
  stretched so its held mean stays the measured mean (`holdScale`) and
  censored there (`ownTime.capped` on first come, `ePoly.expect`'s hold on
  processor sharing), and the bound, not the cv, decides processor sharing
  at arrivals no burstier than Poisson.
  Off, `MaxRatio` is zeroed in `promptSpread` and the gamma runs on. The
  planner's replay does not turn it on either (`learnreplay.PlannerSwitches`):
  on, its backtests read TTFT under measured where they read at or above it.
  A backtest step carries its own histogram's bound beside its cv
  (`whatifapi.Knobs.PromptMaxRatio`).
- `PriceRouterWait` (`slo-ttft-router-wait`, opt-in, off, the CLI's
  `--price-router-wait` default): above the prefill onset, up to
  `MaxRouterWaitKneeShare` of the knee, prices the flow-control gate as a
  stage (`whatif.newGateStage`; `TTFTParts.RouterWait` and `BurstWait`) and
  trades `AboveFlowControlOnset` for `FlowControlWaitPriced`, so the verdict
  there is read from the figure. Validated at one held staircase step; it
  stays off until a cluster staircase with several held steps past the onset
  passes
  ([the router stage](../proposals/slo-capacity-planner-ttft.md#as-built-the-router-stage)).
- `RouterWaitInPrefill` (`slo-ttft-router-wait-in-prefill`, off from the
  constructor; the CLI's `--router-wait-in-prefill`, on by default there,
  `whatif.DefaultRouterWaitInPrefill`, #287): over the same range takes the
  router's wait as already inside the processor-sharing prefill price
  (`ttftInputs.routerWaitInPrefill`, `TTFTFleet.RouterWaitInPrefill`). No
  figure moves; the flag is `FlowControlWaitPriced` in place of
  `AboveFlowControlOnset`, the verdict is read from the figure, and the
  published rate may pass the onset. Off, past the prefill onset the planner's
  flag is `FlowControlWaitPriced` where the analyzer's is
  `AboveFlowControlOnset`; on, the analyzer prices the planner's fleet as the
  planner does at its defaults
  (`TestTTFT_TheRouterWaitInPrefillSwitchIsThePlannersPrice`, issue #367).
  Turning it on by default waits for a live check with the controller.
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
One switch is a `whatif.Prefill` field, not a `TTFTFleet` field, and the
planner's alone: `PrefixAttention` (`slo-ttft-prefix-attention`, off from the
constructor and not in `learnreplay.PlannerSwitches`, so off in the planner
too), a prefix hit's cost (issue #311). The planner reads it through
`learnreplay.PrefillState.PrefixAttention`; the analyzer prices nothing with
it. On:

- V_P's rescale along the curve reads `I_eff + 2h`, `h` the cached tokens,
  not `I_eff` (`Prefill.shapedVP`, `attentionSpan`). Each computed token
  attends over the cached ones too: at 32k with a 75% hit a pod computes the
  8k a fresh 8k prompt does, but V_P reads about 1.7x high without it.
  Where the prefill rows read the computed prompt and the whole one, `h` is
  each side's cached context, every tier's hits counted
  (`pricedFleet.cachedContext`, `Prefill.CachedContextRead`, issue #383):
  the question's from the computed-token hit of the same rows and minutes,
  V_P's from `VPWholePromptTokens − VPPromptTokens`. A token served from a
  host, disk or peer tier is a miss to the prefix-cache hit rate, yet the
  prompt attends over it. A prompt netted from a whole one
  (`--assume-input-tokens`, a backtest's step) reads `h` as the whole less
  the netted one, so no token is both computed and cached, and only where
  `slo-ttft-computed-hit` netted it by every tier's hit: netted by the
  prefix-cache hit rate, a tier's tokens are priced as computed. Only where
  they are not read is `h`
  `I_eff·hit/(1 − hit)`, from each side's prefix-cache hit rate. It needs
  both whole prompts known and a cached share above
  `MaterialPrefixHitMove` on either side; otherwise the rescale is as
  before.
- The planner nets an assumed prompt by a stated hit rate (`--prefix-hit`),
  else the prefill variant's, not the decode variant's
  (`pricedFleet.effectivePrompt`).
- A backtest's step is priced at the prompt its own prefill pods computed,
  their computed tokens over their requests
  (`whatifapi.Knobs.PrefillComputedTokens`, `pricedFleet.stepComputedPrompt`),
  and `h` is its whole prompt less that. Off, the step's whole prompt is
  netted by the fit's hit, so a step that hit, tested against a fit that did
  not, is priced as if it computed the whole prompt. A step's hit at or under
  `MaterialPrefixHitMove` moves nothing.
- The KV transfer scales with the tokens it moves, the whole prompt less the
  decode variant's hits (`pricedFleet.transferPerToken`).

On whatever the switch says, `prefill.Estimate` records the whole prompt and
the hit rate V_P was read at (`VPWholePromptTokens`, `VPPromptHitRate`), and
`VPShapeMoved` is raised where the hits moved (`Prefill.prefixMoved`): the
hit rate by more than `MaterialPrefixHitMove` (0.1, stated, not measured),
or the whole prompt by more than `MaxVPShapeRatio` with a hit rate above 0.1
on either side. With the switch on and the cached context read, the hit on
each side is its cached share of the whole prompt, as the rescale prices it,
not the prefix-cache hit rate: like with like (issue #383). A state that
recorded no whole prompt raises nothing new. No committed recording has a
prefix hit, so no golden moves.

`ComputedHit` (`slo-ttft-computed-hit`, planner-only, off from the
constructor) nets a whole prompt the planner prices for the prefill pods
(`--assume-input-tokens`, `--shape load`, a stated shape with no hit, a
backtest or day-plan step) by the prefill pods' computed-token hit,
`1 − computed tokens / whole prompt tokens`
(`whatif.PromptWindow.ComputedHit`). It reads `PrefillComputedTokenRate`
and the one-minute prompt `AvgInputTokensRecent` from the same rows and
minute. `vllm:prefix_cache_hits` counts only the GPU prefix cache, so a hit
from an offload tier or a KV connector reads there as computed. The planner
then priced more prefill work per request than the engines' own counter
showed, and V_P is read from that counter. Details:

- The planner reads the switch through `learnreplay.PrefillState.ComputedHit`
  (`pricedFleet.netByComputedHit`).
- A hit the question states (`--prefix-hit`, an `IL/OL/hit` shape) still
  governs. One the load's cycles filled in (`whatifapi.Knobs.PrefixHitRead`)
  does not.
- The load's shape (`--shape load`, the report's default) is netted this way
  with the switch off too (`pricedFleet.loadShapeRead`): its hit is the
  decode variant's, which on a P/D fleet is not the prefill pods'. Its prompt
  is then in computed tokens, so it keeps the served-work floor
  (`pricedFleet.servedComputed`).
- So is a whole prompt read from the source's own cycles with no hit stated
  (`whatifapi.Knobs.PromptTokensRead`): a day plan's bucket at its own shape
  (`readShape`) and a backtest's step. `loadShapeRead` is the one rule for
  both. A stated prompt (`--assume-input-tokens`, a stated mix, a plan's
  bucket edges) is netted as before.
- It applies only where the computed-token hit or the hit it replaces is
  above `MaterialPrefixHitMove`. No committed recording has a hit, so it
  moves no golden.
- The hit rate printed beside the prompt stays the prefix-cache one, which is
  what V_P's is compared with.
- The measured shape is unchanged: its prompt is already the computed one.

`TierKVWait` (`slo-ttft-tier-kv-wait`, planner-only, off from the constructor)
prices the tier load, with `slo-ttft-tier-load` on, at the engine's measured
wait for the KV instead of the transfer's seconds, where the wait is longer
(`whatif.TierLoad.WaitPerRequest`: the mean `AvgQueueTime`, request-weighted,
over the prefill rows with no request waiting for capacity). A loaded request
is scheduled at a step boundary after its load, so it waits past the
transfer. The wait is the one at the traffic read: it rises with load, and
is held as read at the rate asked. The knee does not move.

`TierLoad` (`slo-ttft-tier-load`, planner-only, off from the constructor)
prices a cached prompt token that came from outside the GPU. A hit in the GPU
prefix cache costs only the attention over it. A hit from a host, disk or
peer tier is loaded into the GPU first, and with vLLM's `OffloadingConnector`
the request waits for that load before its prefill is scheduled. Off, the
price took that load as free.

- **What it reads.** The prefill rows' cached tokens by tier
  (`domain.ReplicaMetrics.CachedTokenRateBySource`, from
  `vllm:prompt_tokens_cached_by_source`) and their KV transfers
  (`KVTransferRates`: bytes and seconds a second of the offload load into the
  GPU, of a tiering spec's reads from its secondary tiers, and of NIXL), each
  recorded only (`registration/cache_tiers.go`, `kv_transfers.go`).
- **The term** (`whatif.PromptWindow.TierLoad`, `pricedFleet.tierLoad`). Each
  transfer kind's seconds per token is the bytes it moved per cached token
  over the bandwidth it ran at. The load per request is a request's tokens of
  each tier times the seconds per token of the kinds that moved them: the
  offload load for host, disk and peer tokens, the tier read beside it for
  disk and peer tokens, NIXL for peer tokens on a pod with no offload load. At
  the question it scales with the whole prompt asked, the tiers' shares held
  as read, and a stated hit caps their share. It is a request's own latency,
  added to `a` (`whatif.Prefill.TierLoadSeconds`, `TTFTParts.TierLoad`), not
  the queue's work: the prefill knee does not move.
- **Overlap.** Priced with none. The `OffloadingConnector` loads before the
  request is scheduled, so none is expected there. A connector that loads
  layer by layer overlaps part of it, and the term is then an upper bound;
  the switch-off figure is the lower one.
- **Where `a` was read.** `a` from engine prefill timing subtracts the
  engine's queue time, which holds a request's wait for its KV load: all of
  the load is added. `a` from low-load minutes holds the load at the traffic
  it was read at: only the question's excess over the traffic read is added.
  A stated `a` (`--assume-a`) adds nothing.
- **No bandwidth is guessed.** A tier serving `whatif.MinTierShare` (1%) of
  the prompt or more with no transfer that prices it leaves the term out,
  flagged `TierLoadUnmeasured`. A recording with no split by tier, where the
  computed-token hit is above `MaterialPrefixHitMove`, is flagged
  `TierSplitUnread`.
- **The attention span** is `slo-ttft-prefix-attention`'s, whether this
  switch is on or not: its cached context counts every tier's hits (issue
  #383).
- No committed recording reads the cached tokens by tier, so the switch moves
  no golden.
- **Not on by default (issue #386).** On a private production P/D recording
  with a host and a peer tier, neither this switch nor `slo-ttft-tier-kv-wait`
  reads a figure newly under 0.8 of measured, and the KV wait brings the 75%
  time split's median mean TTFT closer to measured. At the 50%
  split, whose figures rest on a lower-bound V_P and read high, both raise
  the median error and take figures from within 20% to more than 20% over,
  which the issue's rule forbids. `learnreplay.PlannerSwitches` says when they
  go on.

`CachedTokenCost` (`slo-ttft-cached-token-cost`, planner-only, off from the
constructor) prices the load of a cached prompt token. A token served from a
host tier or a peer is not computed, but its KV is loaded before the
request's prefill can use it. With the switch on, a request's own latency
carries `phi × cached / V_P` beside its computed prompt:

- `cached` is the whole prompt less the computed one over every tier: the
  whole prompt asked times the computed-token hit, or at the measured
  prompt, computed × hit / (1 − hit) (`pricedFleet.priceCachedLoad`).
- The load is added to `a` (`whatif.Prefill.FixedLatency`): it is in the
  request's own latency only, never in the pod's work, so no knee moves.
  V_P is read from busy pods whose loads overlapped their compute.
- `phi` is one figure per fleet, in [0, 1], fitted on the source's own
  steps up to the cycle read (`whatifapi.FitCachedTokenCost`): each step a
  backtest judges, its mean TTFT scored and below the knee, is priced at
  phi 0 and at phi 1; its implied phi is the measured mean less the first
  price over the second less the first (the price is linear in phi), and
  phi is the median, held to [0, 1]. A step read high for another reason
  pulls phi to 0, never under it. phi on each half of the steps is reported
  beside it.
- phi is 0, with the reason, where V_P was not learned under a queue (busy
  minutes or the peak under a queue; not the back-pressure bound, a served
  peak, or V_P raised to the served peak), the computed-token hit is under
  `MinCachedTokenCostShare` (0.1), or fewer than `MinCachedTokenCostSteps`
  (12) steps qualify. A self-check's folds fit their own phi on the steps
  before their cut. An answer that reads no steps (the one-shot commands,
  at-replicas with the self-check off) names phi as not fitted.
- phi is net of what the learned terms already hold. `a` is read at the
  shape its minutes ran at: its low-load fit, or its overhead outside the
  prefill and the queue, takes in the load at that cached share. phi is the
  load the learned terms do not hold; on a fleet whose cached share does not
  move it can fit near 0. In the synthetic recording of
  `TestCachedTokenCost_SyntheticRecording` (cost 0.8 injected as queue
  time) it fits 0.40, and the mean TTFT reads as measured with it, 0.55 of
  it without.

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

`slo-ttft-hop-unqueued` (off) sets which router windows teach the decode
term's hop R (`prefill.DecodeTermCycle.HopUnqueued`). On, a window counts
only when none of its cycles had a serving prefill row with requests waiting for
compute: the capacity part of the waiting count where the pod publishes it,
else the whole count. Off, as before, a queued window counts too. Its R is
then the queue's timing: two means of many seconds, read over different
windows while both climb. The capacity planner's replay turns it on
(`learnreplay.PlannerSwitches`) and reads it for its own decode term windows.
That holds on a recording with no slo analyzer too
(llm-d-incubation/llm-d-scaling-manager#281).

`slo-ttft-hop-window-means` (off, planner-only) reads the hop R on each
router window's means (`prefill.DecodeTermCycle.HopWindowMeans`). The
per-cycle gates hold every cycle of a window to the rate ratio and the prompt
drift. On a production fleet, one-minute noise alone breaks them in every
window, so R is never learned and the assumed 20 ms stands. With the switch
on:

- A window is admitted when its earlier and later halves agree: their mean
  rates within the rate ratio and their mean prompts within the drift. A ramp
  still fails; noise around a level does not.
- The held gate and the prefill-queue gate stay, whatever
  `slo-ttft-hop-unqueued` says.
- R is the median of at least `prefill.MinHopMeanWindows` admitted windows,
  held at their median prompt, with no prompt line. The planner names its
  source `measured, window means`.

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
