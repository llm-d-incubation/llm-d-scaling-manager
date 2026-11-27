# Predicted latency and capacity in the controller

**Status:** built for passive and shadow models (issue #440, refs #169),
with the models in their current, pre-freeze forms. Built: the models'
inputs as collector signals; the fit cores shared by the capacity planner
and the controller in the signals layer; the two v2 learners in the slo
analyzer (observe, admit, fit, validate, hold, forget, report); the
predicted TTFT and TPOT, mean, p90 and p95, default and v2 side by side;
the v2 would-be prefill ask of passive and shadow models; the parity test
against the planner; the cost measured. Not built: the learned-state
snapshot of the v2 fits, the #169 knee modes, and v2 figures for active
models (the switch exists, nothing turns it on). The frozen forms (planned
for 2026-12-06) change the fit cores in `internal/signals/whatif`, not
their callers.

## What and why

The capacity planner (`cmd/capacity-planner`) predicts a fleet's TTFT and TPOT
and its knees from a recording. Operators want the same figures live, next to
what was measured. Issue #440 asks the controller for them from the planner's
own model code; #169 sets the rule that a planner fix is carried into the
controller's learners and analyzers, not copied.

So the planner's models are not added beside the engine. Each quantity is
**owned by the analyzer that owns it today**, learned by **the learner that
learns it today** (extended with a mode where the planner's model differs),
from **ordinary collector signals**, and the planner's replay
(`learnreplay`) runs the same learners and signal builders, so the two agree
by construction. Three rules from the issue hold throughout:

1. **One model, two callers.** Controller and planner call the same packages.
2. **Missing is reported, never estimated.** Per model, under the owning
   analyzer's metric family.
3. **Shadow until validated, and run wherever it is free.** Where the
   controller moves nothing -- a passive model, or every model while the
   utilization share plans in shadow -- the v2 models run by default: no risk
   to the fleet, and their figures are the live evidence the turn-on is
   scored on. For an active model they run only with the slo analyzer's
   switch `slo-v2-inputs` on (the existing switch registry,
   `SetSwitchesForReplay`, off from the constructor), and active sizing stays
   on the default models until that evidence passes.

## The engine's layers

`collector -> signals -> learners -> analyzers -> engine`
(`docs/developer-guide/signal-dataflow.md`; import order in
`hack/check-import-direction.sh`: collector 20, signals 30-32, analyzers 40).
Learners follow the shared rules of [analyzer-learner](analyzer-learner.md):
observe, admit or refuse, fit, predict with a source and an age, validate,
keep, refit, hold or forget; each fills a `learn.Report` and is reached by the
forget annotation through `learn.Learners`.

## Mapping, per quantity

| Planner quantity | Signals (collector -> signals layer) | Learner | Owning analyzer | Series (existing / new) | Switch | Built, or left |
| --- | --- | --- | --- | --- | --- | --- |
| TTFT, default model (mean, objective percentile) | scaling + TTFT-target queries; prefill observation (`signals/prefill`) | existing: `V_P`, `a`, `f` (`signals/prefill`), decode term, router hop | slo | existing `llmsm_slo_predicted_ttft_seconds{statistic}`, `llmsm_slo_ttft_reason`, `_refused`, `_flag`, `_vp_source` | existing TTFT switches | publish `mean`, `p90`, `p95` for every fleet shape the model prices (today: the objective's percentile, prefill variants of P/D) |
| TTFT, v2 rank mixture | five per-pod queries + router TTFT mean (below); `whatif.MixtureCycleOf` | **v2 mode of the TTFT learner**: `learn.TTFTMixture` (built) | slo | existing `llmsm_slo_predicted_ttft_seconds`, beside the default under the existing `learner` label; new `llmsm_slo_model_coverage`, `llmsm_slo_model_input_missing` | on for passive and share-shadow models; `slo-v2-inputs` for active (built, off) | fit (admit steady steps, robust fit, held-out validate, hold/forget), price, persist the fit in the learned-state snapshot |
| TPOT, default model (mean, `R_q` percentile) | scaling queries | existing: the slo analyzer's ITL line (`learn.SLO`), the TPOT percentile ratio | slo | existing `llmsm_slo_predicted_itl_seconds`, `llmsm_slo_fill`; new `llmsm_slo_predicted_tpot_seconds{statistic,learner="slo"}` (built) | existing | publish mean, p90, p95 |
| TPOT, v2 renewal lognormal | output-length histogram, ITL, running, accepted drafts, router TPOT histogram; `whatif.DecodeCycleOf` | **v2 mode of the TPOT ratio**: `learn.TPOTDistribution` (built) | slo | `llmsm_slo_predicted_tpot_seconds{learner="tpot-distribution"}`, same coverage series | as the TTFT v2 row | fit `C` on the router's buckets, price p90/p95 around the default mean, persist `C` |
| Would-be sizing under v2 | the v2 rows | the v2 learners | slo (ask), then the optimizer as today | existing `llmsm_replicas_suggested` / `_provided` with #441's `mode` (`passive`, `shadow`), plus the existing `learner` label | passive and share-shadow models only | what the v2 models would ask for, where nothing is applied; active sizing reads only the default |
| Decode knee (KV, batch, fill; ITL line) | scaling queries | existing: k2, ITL line, mu (`learn.K2`, `ITL`, `Mu`) | saturation | existing `llmsm_learned_state_k2_*`, `llmsm_itl_line_*`, `llmsm_itl_seconds_at_pricing_k`, `llmsm_pricing_k`, `llmsm_service_rate_per_replica`, `llmsm_kv_cache_tokens_capacity` | existing | where the planner's knee differs (#169 items 1, 3), a mode of the existing learner, behind a saturation switch |
| Prefill knee (`V_P` and its sources) | prefill observation | existing: `V_P` (`signals/prefill`) | slo (TTFT) and saturation (prefill floor) | existing `llmsm_slo_ttft_vp_source`, `llmsm_slo_ttft_replica_requests_per_second` | existing | `f` per pod count (#169 item 5) as a mode of the existing learner |
| Fleet knee and operating point | the above | the above | saturation (floor), throughput (service rate) | existing `llmsm_required_capacity`, `llmsm_spare_capacity`, `llmsm_saturation_utilization`, `llmsm_analyzer_demand` | existing | the planner's fleet knee read from the same learners; any new figure is a label of an existing family |

Nothing new is planned for knees and capacity: the planner's figures there
come from the learners the saturation, throughput and slo analyzers already
run (the planner replays them), so the work after the freeze is the #169
modes, each in its owning learner.

## Signals

What the v2 latency models read, whether the controller collected it before
this change, and over which window. "Scaling" is a query issued every cycle;
"TTFT target" one issued in a namespace whose policy prices TTFT; "record-only"
one issued only while a learner recording is on.

### TTFT v2 (rank mixture), built per cycle by `whatif.MixtureCycleOf`

| Signal (`signal` label) | Field | Role | Collected before | Window |
| --- | --- | --- | --- | --- |
| `prefill-pods` | a serving row of a prefill variant | prefill | scaling | cycle |
| `prefill-request-rate` | `RequestRate` | prefill | scaling | 1m |
| `prefill-ttft-mean` | `AvgTTFT` | prefill | scaling | 1m |
| `prefill-time-mean` | `AvgPrefillTime` (vLLM only) | prefill | **record-only** | 1m |
| `queue-time-mean` | `AvgQueueTime` | prefill | **record-only** | 1m |
| `computed-token-rate` | `PrefillComputedTokenRate` | prefill | scaling | 1m |
| `computed-tokens-histogram` | `ComputedTokensBuckets` (vLLM only) | prefill | TTFT target | 1m |
| `prompt-tokens-histogram` | `PromptTokensBuckets` | prefill | **record-only** | 5m |
| `queue-time-histogram` | `QueueTimeBuckets` | prefill | **record-only** | 1m |
| `decode-ttft-mean` | `AvgTTFT` | decode | scaling | 1m |
| `router-ttft-histogram` | `RouterLatency.TTFTBuckets` | router | slo analyzer | 5m |
| `router-ttft-mean` | `RouterLatency.TTFTSumRate`/`TTFTCountRate` | router | TTFT target | 5m |
| (engine flags) | prefill `--max-num-seqs`, chunked prefill | variant | discovered (`llmsm_engine_config`) | start |

### TPOT v2 (renewal lognormal), built per cycle by `whatif.DecodeCycleOf`

| Signal | Field | Role | Collected before | Window |
| --- | --- | --- | --- | --- |
| `decode-pods` | a serving decode row | decode | scaling | cycle |
| `decode-request-rate` | `RequestRate` | decode | scaling | 1m |
| `itl-mean` | `AvgITL` | decode | scaling | 1m |
| `output-tokens-histogram` | `OutputTokensBuckets` | decode | scaling | 5m |
| `running-requests` | `RunningRequests` | decode | scaling | instant |
| `spec-accepted-per-step` (optional) | `SpecAcceptedPerStep` | decode | scaling | 1m |
| `router-tpot-histogram` | `RouterLatency.TPOTBuckets` | router | **record-only** | 5m |
| `router-tpot-mean` | `RouterLatency.TPOTSumRate`/`TPOTCountRate` | router | slo analyzer | 5m |
| (mean TPOT) | the default TPOT model's mean | learner | the slo analyzer's ITL line | - |

The default models need no new collection: their inputs are scaling or
TTFT-target queries, and their learned state is the analyzers' own.

### When the v2 models run

The engine decides per model, each cycle, after `observePassive`:

| Model's mode | v2 inputs collected and coverage published | Sizing |
| --- | --- | --- |
| passive (`llmsm_passive_mode` not `active`) | yes, by default | would-be only, as today; v2's would-be prefill ask on `llmsm_replicas_suggested{learner="ttft-mixture"}` |
| any, while the utilization share plans in shadow | yes, by default | would-be only |
| active | only with `slo-v2-inputs` on | the default models, until the turn-on evidence passes |

Running them where nothing moves costs only the queries and the fits below,
and yields the live, held-out evidence the turn-on rule scores.

### Collection, and the proof that no decision moves

For a model the v2 models run for, the engine names its namespace
(`collector.ReadPredictionInputsIn`), and the collector issues the five
per-pod queries of `predictionReadReplicaQueries` as ordinary replica
queries: same registration, same per-cycle memo (once per namespace), same
failure counting (`query_type="slo_model_inputs"`, which
`LLMSMMetricsCollectionFailing` leaves out). Each is an existing record-only
or TTFT-target query; the v2 models add a third condition under which it is
issued. The router's TTFT mean and TPOT histogram come from the same router
queries through `CollectPredictionLatency`.

The values these queries fill are read today by other analyzers when present
(`AvgQueueTime` and `AvgPrefillTime` by the saturation analyzer's busy-share
guard and the prefill signals), so the collector keeps the rows carrying
them for the slo analyzer's v2 models (`PredictionRows`) and hands the
analyzers the rows a collection without the switch builds: each field only
a prediction query filled is cleared (`clearPredictionOnly`).
`TestPredictionRows_AnalyzersReadTheRowsOfACollectionWithoutIt` compares the
two through the real Prometheus parse path, with and without a TTFT target and
with a recording; with the clearing disabled it fails. A recording is
unaffected: it issues every query already, and nothing is cleared.

### Missing signals

`whatif.ModelInputs` lists every input with its objective and role;
`whatif.MissingModelInputs` reports, per objective, the ones no serving row of
the role (or the router) read this cycle. It lives beside the models, so the
planner can report the same names for a recording. The slo analyzer publishes
`llmsm_slo_model_input_missing{objective,signal}` and the one-hot
`llmsm_slo_model_coverage{objective,reason}` (`priced`, `abstained`,
`signal-missing`, `window-short`, `model-limit`). A failed query reads the same
as a series not exported: missing, never zero.

### Cost per cycle on a large fleet

Measured on the largest window a learner keeps (13 hours at a 15 s cycle:
3,120 cycles) of a P/D model with eight prefill ranks, four decode replicas
and 20-bucket histograms, under a load that rises and falls, so that every
fit runs to its end (`internal/engines/analyzers/slo/model_cost_test.go`;
one core of a 22-core laptop, other work running):

| what | measured | how often | 100 such models |
| --- | --- | --- | --- |
| TTFT refit (rank mixture, folds, held-out score) | 2.7 s | every 30 min per model, in the background, one model at a time | 270 s per 30 min: 15 % of one core, never on the decision path |
| TPOT refit and the default R_q | 0.05 s | with the TTFT refit | 5 s per 30 min |
| pricing: the mean, p90, p95 and the objective's percentile from one set of draws, and the prefill ask | 25 ms | at most once a minute per model | 2.5 s per minute, about 0.6 s per 15 s cycle, on the engine's loop |
| the window | 13 MiB per model | held | 1.3 GiB; held for 16 models at most (`ModelMaxWindows`): 208 MiB |
| queries | `6N` a cycle, `N` the namespaces | every cycle | 120 a cycle for 20 namespaces |

The budgets the first draft set (200 ms per refit, 5 ms per pricing, 10 MB
per model) do not hold for the rank mixture on a large P/D model: a refit
calibrates the spread on two folds by bisection, each probe drawing 16,000
requests per step, and the window keeps every cycle's histograms as maps.
So the refits run every 30 minutes instead of every 5, in the background,
one model at a time (a refit over 5 s spaces the next one twice as far),
and a model is priced at most once a minute. The memory is the open cost:
holding the histograms as slices, or the window as pooled steps, would cut
it, at the price of a change to the fit cores' inputs that the parity test
would then hold. Until then the analyzer keeps the windows of at most 16
models at once (`ModelMaxWindows`): the v2 models run for every passive
model, and for every model with the utilization share in shadow, and 100
windows would not fit in the controller's 512 MiB limit
(`config/base/manager`). A model past the bound reads coverage
`model-limit` until a window is freed. A model whose policy does not list
the `slo` analyzer is not observed at all: its `Analyze`, which publishes the
figures, does not run, so a window would cost a slot for nothing.

## Packages and import direction

| Code | Layer | Status |
| --- | --- | --- |
| model math (rank mixture, TPOT distribution, queues, inversions) | `internal/signals/whatif` (32) | shared already |
| per-cycle builders `MixtureCycleOf`, `DecodeCycleOf`, `FitCycleOf`; `ArrivalOf` | `internal/signals/whatif` | moved from `learnreplay`; the planner's series call them |
| fit cores `FitRankMixtureCycles`, `FitTPOTDistCycles`, `FitMixtureWindow`, `FitTPOTWindow`, their reports, `LatencyHist`, `RouterTTFTOf`, `MixtureBacklog` | `internal/signals/whatif` | **moved** from `whatifapi`; the planner's backtest and main view call them through thin adapters (`fitCycles`) |
| engine slots `EngineParams.PrefillSlots`, `DecodeSlots` | `internal/signals/capacity` (30) | moved from `cmd/capacity-planner` |
| input registry `ModelInputs`, `MissingModelInputs` | `internal/signals/whatif` | new, beside the builders |
| v2 learners (`model_learners.go`), their publication (`model_published.go`), `ObserveModelInputs`, `ModelPrefillAsk`, `RefitModel` | `internal/engines/analyzers/slo` (40) | new |
| `ModelV2At` (a recording through the v2 learners) | `internal/engines/analyzers/learnreplay` | new, for the parity test |
| default models' fleet (`servedState`, `servedFleet`, `ttft.go`) | `cmd/capacity-planner` (package `main`) | unchanged: read from the analyzers' learners through read-only views ([what-if capacity, Packaging](slo-capacity-planner.md#packaging)) |

`whatifapi` imports no analyzer; the slo analyzer (40) imports
`signals/whatif` (32), never `whatifapi`.

## Per-cycle flow

1. `beginCollectorCycle`, after `observePassive`, names every namespace
   holding a model the v2 models run for: passive, the share in shadow, or
   `slo-v2-inputs` on (`ReadsModelInputs`).
2. Each model's collection keeps its full rows; the analyzers get the cleared
   ones.
3. After `prepareModelData`, the engine hands the slo analyzer the kept rows
   (cut to the pods the analyzers read), the variant states, the arrival
   rate, the model's policy and the router reading (`ObserveModelInputs`).
4. The analyzer builds the cycle (`whatif.FitCycleOf`) for the model's one
   prefill and one decode variant, admits it unless nothing served, trims
   the window to `ModelWindow` (13 h), and, every `ModelRefitEvery`
   (30 min), starts a refit in the background, one model at a time.
5. A refit fits both models on the window cut into `ModelStep` (5 min)
   steps, with the engine flags the capacity store holds (prefill
   `--max-num-seqs` and chunked prefill; decode `--max-num-seqs` and
   speculative tokens). A fit is used where it passes the planner's checks
   and at least half its held-out steps (TTFT: within 20 % of the router's
   mean; TPOT: the p90 in the measured bucket); a refit that does not pass
   holds the fit in use for an hour (`ModelHoldFor`).
6. At most once a minute (`ModelPriceEvery`) the fit in use prices the
   current rate at the serving prefill pods (mean, p90, p95, the objective's
   percentile) and the prefill ask: the least pods, up to four times those
   serving, at which the objective's percentile meets the target.
7. Coverage is published: `priced`, `abstained`, `window-short` (under 12 h
   for the TTFT model, 2 h for the TPOT model), `signal-missing`, or
   `model-limit` (the analyzer keeps the windows of `ModelMaxWindows`, 16,
   models at once).
8. In `Analyze`, for a model observed on this cycle only (so a replay
   publishes what it did before), the slo analyzer publishes the default's
   TTFT mean, p90 and p95 beside its objective's percentile, the v2 TTFT,
   the default TPOT (its ITL line at the current fill, times R_q for the
   percentiles), the v2 TPOT p90 and p95 around the same mean, and the
   learners' reports.
9. When the decisions are applied, the engine adds the v2 prefill ask of a
   passive or shadow model to `llmsm_replicas_suggested` under
   `learner="ttft-mixture"`: the higher of the analyzers' own and the ask.
10. A model no longer reconciled has its series deleted (the engine's
    prune, and `metrics.DeleteSLOModel`); its window goes on the forget
    annotation, a change of its variant pair or engine arguments, and the
    state timeout.

## Parity with the planner

By construction, and tested. The planner's at-replicas and the slo
analyzer's learners build their cycles with the same builder
(`whatif.FitCycleOf`) and fit them with the same cores
(`whatif.FitMixtureWindow`, `whatif.FitTPOTWindow`).
`cmd/capacity-planner/v2_parity_test.go`
(`TestV2Parity_ControllerAndPlannerAgreeOnTheRecording`):

1. Replay the public q32 warm-up recording through `learnreplay.ModelV2At`,
   which feeds a slo analyzer each of the model's cycles as the engine feeds
   a passive model, and refits at the cycle asked.
2. Fit the planner's experimental models on the same recording at the same
   cycle, as `at-replicas --experimental-ttft=v2 --experimental-tpot=v2`
   does.
3. Compare, at the last cycle and at cycle 400: the TPOT fit report, its
   p90 and p95 ratios and the highest load served, and the TTFT fit report
   and its abstention, equal to the last bit.
4. Negative control: drop the router's TPOT histogram on the controller
   side only; the TPOT fit moves.

The q32 warm-up is 2.4 hours, so the TTFT model fits and then abstains
(its spread is calibrated on 12 hours) on both sides alike: the fitted
parameters are compared, the priced TTFT is not. No public recording holds
12 hours of a P/D fleet with its per-rank series yet.

## Turn-on rule

The evidence comes from the passive and share-shadow deployments that run
the v2 models by default: their v2 figures beside the default's
(`learner` label) and the measured latency, recorded and scored by
`capacity-planner backtest` under the planner's own validation bar
([calibration envelope](slo-capacity-planner.md#calibration-envelope-as-built)).
`slo-v2-inputs` (v2 figures for active models, export only) turns on when
that bar passes, the parity test passes on every golden, and the cost budget
holds on the largest. Sizing an active model with the v2 models is a later,
separate decision with its own replay and GPU validation, as #169 requires;
until then every active decision is the default models', and a passive or
shadow model's v2 sizing is would-be only.

## What waits for the Dec 6 freeze

- The frozen forms of the two models: they change the fit cores in
  `internal/signals/whatif` and, where a model adds or drops a signal,
  `whatif.ModelInputs` and `predictionReadReplicaQueries`, in the same
  change. The parity test holds the controller to them.
- The learned-state snapshot of the v2 fits, so a restart prices at once
  rather than after the window refills (12 hours for the TTFT model).
- v2 figures for active models (`slo-v2-inputs`), and any active sizing
  with them: the turn-on rule below.
- The #169 modes of the k2, ITL, mu and `V_P` learners for the knees.
- A cheaper window (histograms as slices, or pooled steps) if fleets of
  many large P/D models run the v2 models.

## Entities added

| Name | Kind | Why an existing one could not carry it |
| --- | --- | --- |
| `slo.SwitchV2Inputs` (`slo-v2-inputs`) | switch, in the existing slo switch registry | active models need an explicit, off-by-default enable flippable on evidence, and no existing switch gates these inputs. Passive and shadow follow the existing passive and share modes, with no switch. A controller flag was tried first and dropped in favour of the registry |
| `learner` label on `llmsm_slo_predicted_tpot_seconds`, `llmsm_slo_predicted_ttft_seconds` and `llmsm_replicas_suggested` | existing label | no new label: `learner` already names the learner behind a series (`learn.Report`); a `model` label was considered and dropped, as it reads as `model_name`. On `llmsm_replicas_suggested` the analyzers' own ask carries it empty, which PromQL reads as absent, so no existing query changes |
| `whatif.MixtureCycleOf`, `whatif.DecodeCycleOf` | functions (moved, not new) | existed unexported in `learnreplay`, which the controller cannot import below the analyzers; moved verbatim to the signals layer |
| `whatif.ModelInputs`, `MissingModelInputs` | signal registry | presence bits say whether one field was read; nothing lists which fields a model needs, and the planner needs the same list |
| `learn.TTFTMixture`, `learn.TPOTDistribution` | learner names | the v2 fits differ from the TTFT (`V_P`) and TPOT-ratio learners in what they fit; they are modes of those, but each needs its own report and forget key |
| `predictionReadReplicaQueries`, `PredictionRows`, `CollectPredictionLatency`, `ReadPredictionInputsIn` | collector query class and accessors | the existing classes (scaling, TTFT target, record-only) are each issued under one condition; the v2 inputs are a union of two of them under a third. The kept rows exist because clearing is the only way to issue them without moving a decision |
| `query_type="slo_model_inputs"` | label value of an existing counter | a failure must not raise the collection alert, as `ttft_inputs` and `learner_recording` do not |
| `whatif.FitCycle`, `FitCycleOf`, `MixtureWindowFit`, `TPOTWindowFit` | types and functions of the shared fit cores | the cores were keyed to the planner's inputs entry, which the analyzers cannot import; one per-cycle record both callers build is what makes their figures the same |
| `slo.ModelFit`, `RefitModel`, `WithModelRefits`, `learnreplay.ModelV2At` | test and replay entry points | the parity test needs the learners' fit at a given cycle of a recording; nothing in the controller calls them |
| `ModelPrefillAsk` and the engine's `v2Gaps` | the v2 would-be ask, read by the replica-gap step | #441's series carry the would-be figures of passive and shadow models; the v2 ask is one more figure of the same family, told apart by `learner` |
| `statistic` values `mean`, `p95` on `llmsm_slo_predicted_ttft_seconds` | label values | the series had the objective's percentile only; #440 asks for the mean, p90 and p95 |
| `llmsm_slo_model_coverage`, `llmsm_slo_model_input_missing` | series in the slo family | `llmsm_slo_ttft_reason` is per prefill variant and the default model's; coverage is per model and objective, and covers TPOT |
| `llmsm_slo_predicted_tpot_seconds` | series in the slo family | asked for by #440 beside `llmsm_slo_predicted_ttft_seconds`; `llmsm_slo_predicted_itl_seconds` is ITL at the priced fill, not a TPOT statistic |
| `modelInputsObserver` | optional interface in the engine | the engine holds the slo analyzer as `sloStateKeeper`, whose test doubles would all have to grow the methods; an optional interface is how `TakeLearnerReports` is taken |
