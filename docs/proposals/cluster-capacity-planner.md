# Cluster capacity planner: a graphical front end for what-if

**Status:** proposal, 2026-11-09. Nothing on this page is built. It builds on
`cmd/whatif` and `internal/signals/whatif`, which are
([what-if capacity](slo-capacity-planner.md), [end-to-end TTFT](slo-capacity-planner-ttft.md)),
and it assumes the processor-sharing TTFT pricing on `fix/ttft-processor-sharing`
(PR #16) lands as it stands.

**Audience:** contributors. There is no operator page yet; one goes in
`docs/guides/` when phase 1 ships.

## What this is

An operator has a P/D model with an SLO and wants three answers without
learning the CLI's flags:

1. "With 2 prefill and 3 decode replicas, what TTFT and TPOT do I get at 6 r/s?"
2. "What rate can those replicas carry and still meet my SLO? Which role runs
   out first?"
3. "What changes if I add a replica, or tighten the target?"

The what-if engine answers all three today, one CLI call at a time
(`whatif replicas-to-slo`, `whatif slo-to-rate`). This proposal puts a
graphical planner on top of it:

- an HTTP API that returns curves, points and limits as JSON
  ([The API](#the-api));
- a small web page served by that API, with sliders and two graphs per
  traffic class ([The screen](#the-screen));
- a Grafana link and, where a plugin is allowed, an optional panel; Grafana is
  not the planner's main home ([Grafana](#can-it-be-built-on-grafana)).

The user-facing name is **cluster capacity planner**. `whatif` stays the name of
the engine and the CLI. See open question 1 on the word "cluster".

### Scope of v1

- One model, one prefill variant and one decode variant (the TTFT model is P/D
  only; aggregated TTFT is the planner's M1 and not built).
- The critical class only. Regular and batch are on screen and disabled
  ([Traffic classes](#traffic-classes)).
- TTFT and TPOT. TTFT at any percentile from p50 to p99, or the mean. TPOT as
  the mean, or a per-request percentile when a ratio `R_q` is learned or
  assumed.
- Read only. The planner never changes a replica count, a policy or a
  ConfigMap.

## The user's design, and what this proposal changes

The design asked for, kept as the core:

- Controls: prefill replicas, decode replicas, requests/s per class (critical,
  regular, batch; regular and batch set to 0 and disabled in v1).
- One tab per class; the last two disabled in v1.
- In the critical tab, a TTFT graph and a TPOT graph. The SLO type (p90, p95,
  ...) is picked and the graphs follow it.
- On each graph two points: the SLO value at the chosen rate, and the rate at
  the chosen SLO value. Both set by sliders.
- TTFT has one line per prompt-length bucket and one SLO target per bucket.
- A final answer: the most requests/s the fleet serves under the current SLOs,
  as the prefill limit, the decode limit and the smaller of the two.

Each change below is a proposal, with its reason. None removes anything above.

| # | Change | Why |
| --- | --- | --- |
| C1 | Draw the target as a horizontal line and the rate as a vertical line; the two points are where they cross the curve | The two points then read as one picture: the gap between them is the headroom, and moving either slider moves a line, not an unexplained dot |
| C2 | Mark the knee, the operating-point cap and the flow-control onsets on the rate axis, and shade past the knee as "saturated" | The model refuses at or above the knee (`Saturated`). A curve that stops with no reason reads like a bug |
| C3 | A binding-constraint bar under the controls: prefill limit, decode limit, fleet limit, the chosen rate on the same axis | Says which role binds and by how much without reading two graphs |
| C4 | "+1 prefill" and "+1 decode" deltas next to the fleet limit | The usual next question. Adding a replica of the role that does not bind changes nothing; the screen should say so |
| C5 | An inverse mode: type a target rate, get the fewest replicas per role that carry it under the SLO | The planning question is often "how many do I need", not "what does this fleet do" |
| C6 | Every number carries its caveats: a badge for assumed, unvalidated, held by a flag or refused | The engine never returns a number without its flags; a screen that drops them over-states the model ([Provenance and caveats](#provenance-and-caveats)) |
| C7 | A traffic mix control (per-bucket shares), defaulting to the observed mix | Per-bucket lines depend on the mix. Without a mix control the user cannot see why a line moved |
| C8 | Per-bucket lines are priced **in the mix** by default; "if all traffic were this bucket" is a separate, labelled view | These are different questions with different answers; the second is what the engine computes today ([TTFT per bucket](#ttft-per-bucket)) |
| C9 | In v1 the critical rate is the **total** rate, labelled so | Batch traffic in the engine slows critical traffic whatever the router does; setting it to 0 is not neutral ([Traffic classes](#traffic-classes)) |
| C10 | Save a scenario, and later compare two side by side | Planning is comparing options. Phase 3 |
| C11 | Call the critical/regular/batch split a "traffic class", not a "tier" | "Tier" already means a named scaling-policy tier and an ITL line's tier in this repo |
| C12 | Start every session from "now": the current replicas, the observed rate and mix, the policy's SLO | The first screen then answers "where am I", and every slider move is a change from there |

## The scenario

A scenario is everything one screen prices. It has two halves: the **inputs**,
learned or typed, which the user does not drag, and the **knobs**, which the
sliders move.

| Part | Field | Values | Default |
| --- | --- | --- | --- |
| Inputs | `inputsId` | a content hash of the resolved inputs ([Sources](#where-the-inputs-come-from)) | the current source |
| Fleet | `prefill`, `decode` | replicas, integers 0..64 | the Ready count |
| Traffic | `classes.critical.rate` | r/s over the model, total in v1 | the observed rate |
| Traffic | `classes.regular.rate`, `classes.batch.rate` | r/s; 0 and disabled in v1 | 0 |
| Traffic | `mix` | per bucket: share, mean prompt, mean output, prompt cv | the observed mix |
| Traffic | `arrivalCV2` | `Ca²`, 1 is Poisson | learned, else 1 (flagged) |
| SLO | `ttft.statistic` | `mean`, or `p50`..`p99` | the policy's `slo.ttft` |
| SLO | `ttft.targets` | seconds per bucket, or one for all | the policy's target for every bucket |
| SLO | `tpot.statistic` | `mean`, or `pNN` (needs `R_q`) | the policy's `slo.tpot` |
| SLO | `tpot.target` | seconds | the policy's target |
| Policy | `headroom` | (0, 1]; 1 lifts the operating-point cap | the policy's `scaleUpThreshold`, 0.85 |
| Assumptions | `assume` | typed values for learned terms: `vp`, `a`, `imbalance`, `promptCV`, `tpotRatio`, `routerHop` | none |

The statistic names are the ones the SLO metrics already use (`statistic`
label: `mean` or `p90`), so a planner link from Grafana can pass them
unchanged.

A typed assumption replaces a learned term and raises the engine's own flag
(`AssumedVP`, `AssumedImbalance`, `AssumedPromptSpread`, `AssumedTPOTRatio`,
`RouterHopAssumed`), exactly as the CLI's `--assume-*` flags do. The screen shows
the learned value beside the typed one.

## Where the inputs come from

The engine reads one learned state: the analyzers' state replayed to a cycle
(`learnreplay.StateAt`). The planner offers three sources of it, and shows
which one is in use at the top of the screen.

| Source | What it reads | Freshness | Built today |
| --- | --- | --- | --- |
| Recording | a `.jsonl.gz` recording, replayed to a cycle (`whatif ... --recording`) | fixed | yes, in the CLI |
| Prometheus window | a past window captured and generated in memory (`whatif ... --prometheus --from --to`) | as of the window's end; seconds to replay | yes, in the CLI |
| Controller | the controller's in-memory learned state for the model, at its last cycle | one cycle, 15 s | no: the read-only handler in [Data per mode](slo-capacity-planner.md#data-per-mode) |

The learned-state ConfigMap ([snapshot](learned-state-snapshot.md)) is not a
source on its own. It holds k2, throughput windows, ITL baselines and start
estimates, but not `V_P`, `a`, `f` or the prompt spread, so it cannot price
TTFT. It can lend an ITL line, as the planner page already says.

**Resolving inputs is separate from pricing.** `POST /v1/inputs` resolves a
source once and returns every term with its value, its source and its flags,
under a content hash (`inputsId`). Every later call prices against that hash,
so dragging a slider never re-reads Prometheus, and two people with the same
hash see the same numbers. This is the `freeze` command's idea (a prediction
named by the content of its inputs, no clock and no path in it), applied to an
interactive session.

### Provenance and caveats

The rule: **no number on screen without what it rests on.**

- **Per input**, a source chip: `learned` (with the cycle range and source,
  e.g. `V_P busy-minutes, cycles 1210-1840`), `borrowed` (with the donor),
  `assumed` (typed, or the engine's default such as prompt cv 2), or
  `refused` (with the reason).
- **Per answer**, badges from the engine's flags. Three are shown as words, not
  icons, because they change how far to trust the figure:
  - `UnvalidatedTTFT`, on every TTFT figure: a banner, not a badge, with the
    measured bias in words ("reads up to 33% low at light load, from the P2
    recordings; not a bound"). The banner's text comes from the API, which
    takes it from the same constant the controller publishes
    (`constants.SLOTTFTPredictionCaveat`), so it cannot drift from the model.
  - `AboveFlowControlOnset`: the rate is past where the router starts holding
    requests; the TTFT is priced, and the region is drawn shaded.
  - `SpreadBeyondProcessorSharing`: the prompt cv is above 0.25, so the TTFT
    is the larger of the processor-sharing and first-come prices.
- **Refusals are gaps, never zeros.** A curve point the engine refuses is
  absent, with the reason on hover (`Saturated`, `AboveDecodeOnset`,
  `TargetBelowFloor`). A refused limit prints the reason in place of a number.
  This is the API's job as much as the page's: a refusal is a different JSON
  shape from an answer, as `whatif.Result` is sealed in Go.
- **Bands.** TPOT's bootstrap band and TTFT's `V_P`/`f` sensitivity
  (`Sensitivity`, ±15%) are drawn as a shaded band around the curve, and
  the limits print the span ("7.9 r/s, 7.1 to 8.6 over V_P and f").
- **The flags drawer** lists every flag on the screen once, in plain words,
  each with the inputs it came from and the action that clears it ("learn
  `V_P`: run at a load that keeps a prefill pod busy for a minute").

## The two graphs

Both graphs share one x axis, the rate over the model in r/s, from 0 to just
past the lower knee.

### TPOT

One line: the TPOT statistic picked (mean, or `pNN` as mean times `R_q`)
against the rate, from `whatif.SLOAtReplicas` with `ReplicasQuery.Rate` at each
grid rate. The target is a horizontal line; the chosen rate a vertical one.

- **Point A**, the TPOT at the chosen rate: `SLOAtReplicas` at that rate.
- **Point B**, the rate at the target: `RateForTPOT`, with `Basis.Uncapped`
  shown when the operating-point cap set the rate ("TPOT alone would allow
  12.0").
- Markers: the decode knee (`KneeRate`) and the operating-point cap
  (`headroom` times the lower knee).
- A target below the floor (`TargetBelowFloor`) draws the floor as a line and
  the target under it in red, with no point B.

TPOT depends on the mix too (through `KVreq` and the output length). v1 draws
one line for the mix; per-output-bucket TPOT is later.

### TTFT

One line per prompt-length bucket, each with its own target, at the statistic
picked. The prefill limit is the smallest of the per-bucket point Bs.

- **Point A per bucket**: the TTFT of that bucket's requests at the chosen
  rate.
- **Point B per bucket**: the total rate at which that bucket's TTFT reaches
  its target, the mix held.
- Markers: the prefill knee (`PrefillKnee`), the decode knee, the
  flow-control onsets (`FlowControlOnsets`; the decode onset caps the rate,
  the prefill onset shades the region past it), the operating-point cap.
- The TTFT floor per bucket, drawn as the line's left end; a target under it
  refuses (`TargetBelowFloor`).

The y axis is clipped at three times the largest target: every TTFT curve goes
to infinity at the knee, and an unclipped axis flattens the region that
matters.

### TTFT per bucket

This is where the engine does not yet do what the design asks.

`TTFTAt` and `RateForTTFT` price **one prompt distribution**, a gamma of a
mean and a cv. A bucket today means "if all traffic looked like bucket b"
([token buckets](slo-capacity-planner.md#token-buckets-and-the-current-mix)),
and `freeze` prices its staircase buckets that way. That is a real question
("what if my traffic shifts to long prompts?"), but it is not "what TTFT do my
8k-prompt requests see when they share the pods with everyone else". The two
differ a lot: a short prompt sharing a pod with long ones waits behind them,
and an even 8k/52k mix priced at its mean reads 32% under
([where it is most wrong](slo-capacity-planner-ttft.md#where-it-is-most-wrong)).

Processor sharing makes the in-mix question tractable. Under PS a request of
work `x` takes `x + E(x)`, its extra time a function of its own size and the
pod's load from the whole mix (`ttft_ps.go` already prices the stretch as a
function of the prompt's relative size `u`). So a bucket's TTFT in a mix is:
the load `rho` from the whole mix, and `T(x)` with `x` drawn from the
bucket's own prompt distribution. That is new engine work, not a UI change:

- **E1** (engine): `TTFTAtInMix(fleet, mix, bucket, rate, q)` and
  `RateForTTFTInMix(fleet, mix, targets)`, the latter returning the binding
  bucket. Priced under PS only; first-come in a mix is out of scope. Not
  validated until a staircase runs a mixed bucket.

Until E1 lands, the planner draws:

- one TTFT line for the **current mix** with one target (what `RateForTTFT`
  prices today);
- the per-bucket lines in a separate view, **"if all traffic were this
  bucket"**, faint and labelled so, never used for the fleet limit.

Bucket edges: the repo's buckets are `classifyInputLength`'s edges (100, 500,
1500, 3000, 6000 tokens), which put every long-context prompt in one bucket.
The planner shows only buckets with at least 5% of the observed requests, and
the edges are an open question (2).

## The answer: the most the fleet serves

```
prefill limit  = RateForTTFT(fleet, target)        min over buckets once E1 lands
decode limit   = RateForTPOT(decode, target, headroom)
fleet limit    = min(prefill limit, decode limit), then the operating-point cap
binding        = the role whose limit is lower; on a tie, both
headroom       = fleet limit - chosen rate         (negative: the SLO is missed)
```

The `BoundBy` the engine returns is shown verbatim beside each limit: `TTFT`,
`TTFT` then `decode admission` (flagged `HeldByDecodeAdmission`: the decode
side's admission wait is what holds the prefill limit down, so the fix is a
decode replica, not a prefill one), `decode flow-control onset`, `prefill
knee`, `TPOT`, `batch cap`, `band top`, `operating-point cap`. The
`HeldByDecodeAdmission` case is why the binding role is not always the role
named on the limit's row, and the screen says which replica to add.

The prefill row also shows `Basis.TTFT.PrefillOnlyRate`, the rate the prefill
side alone allows, when it differs.

## Traffic classes

The design names three classes: critical, regular, batch. Nothing in this repo
models classes yet. llm-d classifies requests with an `InferenceObjective`'s
integer `priority`; traffic with no objective is priority 0; a negative
priority is sheddable ([router configuration](../reference/router-configuration.md)).
The proposed mapping, to be confirmed (open question 3):

| Class | `InferenceObjective` priority |
| --- | --- |
| critical | the highest band configured above 0 |
| regular | 0, which unclassified traffic also lands in |
| batch | below 0 (sheddable) |

**Why v1 is critical only.**

1. **No data per class.** The collector sums the router's flow-control wait and
   queue over priority (`QueryFlowControlWaitBuckets`, `flow_control_pool_models`),
   so no recording can learn anything per class. The planner cut the same
   thing ([TTFT as built](slo-capacity-planner.md#ttft-as-built)).
2. **The model prices one band, first come.** Several bands were cut from the
   CLI for reason 1.
3. **Classes share the engine.** Priority acts at the router's gate. Once a
   batch request is on a prefill pod it shares the pod's step with critical
   ones (processor sharing), and it holds KV and batch slots on decode. With
   flow control off, the gate admits everything. So batch traffic slows
   critical traffic whatever the priorities say, and pricing critical at its
   own rate with batch at 0 over-promises whenever batch runs.

So in v1 the critical slider is the **total** rate, labelled "critical (all
traffic priced as one class)", and the observed rate it starts at is the total.
That is conservative for critical traffic and honest about what is priced.

**Later** (phase 4), a rate per class needs: the collector split by
`priority`, the recorder carrying it, and a router-gate model with priority
(a priority queue at the gate above the onset, the engine's PS stretch from the
total in-engine load below and above it). Each class then gets its own tab and
targets; the fleet limit becomes "the largest total at the current class
shares that meets every class's targets", and the bar shows which class binds.

## The screen

Main screen, critical tab, before E1 (one mix line plus the "all traffic"
view collapsed):

```
+---------------------------------------------------------------------------------------+
| Cluster capacity planner     model: qwen3-32b  ns: llm      [ source: controller v ]  |
| inputs: cycle 1840, 12 s ago, 0 refused, 3 assumed [details]                  [save]  |
| ! TTFT model UNVALIDATED: reads up to 33% low at light load. Not a bound.             |
+---------------------------------------------------------------------------------------+
| FLEET                        | TRAFFIC                          | SLO                   |
| prefill  [-]  2 [+]  now 2   | critical (all traffic as one)    | TTFT [p90 v] 800 ms   |
| decode   [-]  3 [+]  now 3   |   6.0 r/s  |----o--------|  now 5.1 |   |--o----------| |
|                              | regular  0  (later)  disabled    | TPOT [mean v] 12 ms   |
| [ how many replicas for ...] | batch    0  (later)  disabled    |   |-----o-------| |
|                              | mix: observed [edit]             | headroom 0.85         |
+---------------------------------------------------------------------------------------+
| [ Critical ]   [ Regular  (later) ]   [ Batch  (later) ]                               |
+---------------------------------------------------------------------------------------+
| MOST THE FLEET SERVES UNDER THESE SLOs                                                 |
|                 0         2         4         6    |    8         10        12  r/s   |
| prefill  TTFT   [###########################################]  7.9   BINDS           |
| decode   TPOT   [#################################################]  9.4  op. cap      |
| fleet           7.9 r/s  (7.1 .. 8.6 over V_P, f)   you chose 6.0: headroom 1.9 (24%) |
| +1 prefill -> 9.4 (+1.5, now decode binds)        +1 decode -> 7.9 (+0)               |
+---------------------------------------------------------------------------------------+
| TTFT p90                                  | TPOT mean                                  |
|  s                       .  |  ////////   |  ms                        |  ////////     |
| 2.4 +                   .   |  saturated  | 16 +                       |  saturated    |
|     |                  .    |  //////     |    |                  .    |  //////       |
| 0.8 +- - - - - - - - -*- - -|- target     | 12 +- - - - - - - - - - -*-|- target       |
|     |            .  ' |     |             |    |            .'    |    |               |
|     |      o .'       |     |             |    |      o.'         |    |               |
| 0.3 + . '  |          |     |             |  6 +.'    |           |    |               |
|     +------+----------+-----+---> r/s     |    +------+-----------+----+---> r/s       |
|           6.0        7.9   9.4            |          6.0        10.2  11.1             |
|        your rate  at target  knee         |       your rate  at target  knee            |
|  o TTFT at your rate: 0.52 s              |  o TPOT at your rate: 8.1 ms                |
|  * rate at 800 ms: 7.9 r/s                |  * rate at 12 ms: 10.2 r/s (cap 9.4)        |
| [show "if all traffic were bucket b"]     |                                             |
+---------------------------------------------------------------------------------------+
| FLAGS (5)  UnvalidatedTTFT | AssumedPromptSpread cv 2 | VPLowerBound | ...  [explain] |
+---------------------------------------------------------------------------------------+
```

After E1 the TTFT graph carries one line per bucket, each with its dashed
target in the same color, and the prefill row names the binding bucket:

```
| TTFT p90, in the mix                                   | prefill knee 9.4
|  s                                       C       .     |
| 2.0 +- - - - - - - - - - - - - - - - - -*- C target    |
|     |                                 .'               |  ///////
| 0.8 +- - - - - - - - - - - - - -*- - - - B target      |  saturated
|     |               B      . '                         |  ///////
| 0.3 +- - - -*- - - - - - - - - - - - - - A target      |
|     | A . ' o           o                              |
|     +-------+-----------+--------+---------+-----------+---> r/s
|            3.1         6.0      7.0       8.4
|       A at target    your rate  B at     C at
| prefill limit 7.0 r/s, bound by bucket B (8k-32k) p90
```

Controls:

- **Sliders with a number box.** A slider for coarse moves, a box for an exact
  value. Replicas are steppers (integers). The rate slider runs to 1.2 times
  the lower knee, so the saturated region is reachable and shows as such.
- **Dragging a point** on a graph moves the matching slider: point A moves the
  rate, point B the target. Optional; the sliders are the primary control.
- **Per-bucket targets** live in a small table under the TTFT graph (bucket,
  share, target slider), collapsed to one "all buckets" row by default.
- **Disabled tabs** say why on hover: "needs traffic split by priority; see
  [Traffic classes](#traffic-classes)".
- **Inverse mode** ("how many replicas for ..."): type a rate; the answer is
  the fewest prefill and decode replicas whose fleet limit reaches it, with the
  binding role, from `POST /v1/size`.
- **Compare** (phase 3): pin the current scenario as A, change knobs, and the
  screen shows both: curves overlaid, A dashed, and a table of the limits with
  deltas.

## The API

One package, `internal/whatifapi`, holds the handlers. It calls
`internal/signals/whatif` and `learnreplay`, and builds no model of its own:
every number it returns is one the CLI can also print. It is mounted by:

- **`whatif serve`** (phase 1): a new subcommand of the CLI, serving the API
  and the page from one binary. It reads recordings from a directory and
  Prometheus as `whatif capture` does. This is the planner's standalone mode,
  outside the manager image, like the CLI.
- **the controller** (phase 3): the same handlers on the metrics server, off by
  default, read only, giving the "controller" source. This is the handler
  [Data per mode](slo-capacity-planner.md#data-per-mode) already proposes.

### Endpoints

All JSON. `schemaVersion` is the API's own (1), with a `whatifSchema` field
naming the CLI schema its answer, refusal and flag shapes follow (`schema-v3`
after PR #16).

| Method and path | Does | Cost |
| --- | --- | --- |
| `GET /v1/models?source=...` | the models a source can price, with their roles and variants | cheap |
| `POST /v1/inputs` | resolve a source for one model into an `inputsId` and every term with its provenance | a recording replay or a Prometheus capture; seconds |
| `POST /v1/plan` | price a scenario: curves, points, limits, marginal deltas, flags | tens of ms per call (to measure) |
| `POST /v1/size` | the fewest replicas per role whose fleet limit reaches a rate | a few `plan`s |
| `GET /` | the page | static |

`POST /v1/inputs`:

```json
{
  "model": "qwen3-32b",
  "source": {"kind": "prometheus", "namespace": "llm", "from": "2026-11-09T10:00:00Z", "to": "2026-11-09T11:00:00Z"}
}
```

`kind` is `recording` (`name`, `cycle`), `prometheus` (as above; the URL and
role map are server flags, not request fields), or `controller` (no further
fields). The response:

```json
{
  "schemaVersion": 1,
  "inputsId": "sha256:4f1c...",
  "model": "qwen3-32b",
  "source": {"kind": "prometheus", "fromCycle": 1, "toCycle": 240, "lastAt": "2026-11-09T11:00:00Z"},
  "fleet": {"prefill": {"variant": "qwen3-32b-prefill", "replicas": 2},
            "decode":  {"variant": "qwen3-32b-decode",  "replicas": 3}},
  "observed": {"rate": 5.1,
               "mix": [{"bucket": "xxlong", "share": 0.62, "promptTokens": 8100, "outputTokens": 480, "promptCV": 0.4},
                       {"bucket": "huge",   "share": 0.31, "promptTokens": 31000, "outputTokens": 350, "promptCV": 0.2}]},
  "policy": {"ttft": {"statistic": "p90", "seconds": 0.8}, "tpot": {"statistic": "mean", "seconds": 0.012},
             "headroom": 0.85, "flowControl": "disabled"},
  "terms": [
    {"name": "vp", "value": 31200, "unit": "tokens/s", "source": "busy-minutes", "cycles": [1210, 1840], "flags": []},
    {"name": "a", "value": 0.041, "unit": "s", "source": "low-load fit", "flags": []},
    {"name": "imbalance", "value": 1.18, "source": "computed-token share", "flags": []},
    {"name": "promptCV", "value": 2, "source": "assumed", "flags": ["AssumedPromptSpread"]},
    {"name": "itlLine", "value": {"a": 0.0044, "b": 0.0061}, "source": "ols", "flags": []},
    {"name": "tpotRatio", "refused": {"reason": "NoTPOTHistogram", "detail": "no TPOT histogram"}}
  ]
}
```

`POST /v1/plan` takes a scenario ([The scenario](#the-scenario)) and returns
everything the screen draws:

```json
{
  "schemaVersion": 1,
  "scenarioId": "sha256:9a07...",
  "banner": "TTFT model UNVALIDATED: reads up to 33% low at light load. Not a bound.",
  "ttft": {
    "statistic": "p90",
    "semantics": "mix",
    "lines": [{
      "bucket": "mix", "target": 0.8, "floor": 0.21,
      "curve": [{"rate": 0.5, "seconds": 0.23, "lo": 0.22, "hi": 0.25},
                {"rate": 9.4, "refused": {"reason": "Saturated", "detail": "9.4 r/s at or above the prefill knee 9.4 r/s"}}],
      "atRate":   {"rate": 6.0, "seconds": 0.52, "flags": ["UnvalidatedTTFT"]},
      "atTarget": {"seconds": 0.8, "rate": 7.9, "boundBy": ["TTFT"], "prefillOnlyRate": 7.9,
                   "sensitivity": {"lo": 7.1, "hi": 8.6}, "flags": ["UnvalidatedTTFT"]}
    }],
    "markers": {"prefillKnee": 9.4, "decodeKnee": 11.1, "operatingCap": 7.99,
                "onsets": {"prefill": 0, "decode": 0}}
  },
  "tpot": {"statistic": "mean", "target": 0.012, "floor": 0.0061,
           "curve": [{"rate": 0.5, "seconds": 0.0064}],
           "atRate": {"rate": 6.0, "seconds": 0.0081},
           "atTarget": {"seconds": 0.012, "rate": 9.4, "uncapped": 10.2, "boundBy": ["operating-point cap"]},
           "markers": {"decodeKnee": 11.1}},
  "limits": {"prefill": {"rate": 7.9, "boundBy": ["TTFT"], "bucket": "mix"},
             "decode":  {"rate": 9.4, "boundBy": ["operating-point cap"]},
             "fleet":   {"rate": 7.9, "binding": ["prefill"]},
             "chosen":  {"rate": 6.0, "headroom": 1.9}},
  "marginal": {"prefillPlusOne": {"fleetRate": 9.4, "binding": ["decode"]},
               "decodePlusOne":  {"fleetRate": 7.9, "binding": ["prefill"]}},
  "flags": [{"flag": "AssumedPromptSpread", "on": ["ttft"], "text": "prompt spread assumed: cv 2", "clears": "a prompt-length histogram (recorder R1)"}]
}
```

Rules the payload keeps:

- **A refused value has `refused` and no number.** Never `0`, never `null` in
  a number's place. A schema test walks every response and checks it.
- **Flags travel with the value they qualify**, and once more in the top-level
  list with their text.
- **The curve grid** is about 64 rates, denser toward the knee (even steps in
  `1 - rate/knee`), so the region where TTFT turns up is drawn finely.
- **Points come from the model, not from the curve.** Point A and point B are
  priced at the exact slider value, never interpolated.

`POST /v1/size` takes `inputsId`, the traffic, the SLO, a target rate and a
replica ceiling per role, and returns the fewest `(prefill, decode)` whose
fleet limit reaches the rate, or the refusal. The roles are coupled: decode
admission enters TTFT, so it searches decode first on TPOT, then prefill on
TTFT at that decode count, then rechecks decode once.

### Caching and cost

- `inputsId` entries are immutable and kept in an LRU. A `controller` or a
  rolling Prometheus source resolves to a new id when it is refreshed, on a
  "refresh" button or every N seconds when the user asks for live mode; the
  screen shows the age.
- `plan` splits its work by what changes it: the curves depend on the inputs,
  the fleet, the mix and the statistic, not on the rate or the targets, so
  dragging the rate or a target slider recomputes only the points and the
  limits. Results are cached by `scenarioId`.
- The page debounces slider moves (about 100 ms) and drops stale responses.
- **Budget:** a slider move answered in under 200 ms on the server. Not yet
  measured: `RateForTTFT` bisects up to 200 prices, each an optimised union
  bound, and PS pricing adds work per price. Phase 0 measures it on the
  goldens and the processor-sharing fleets before the page is built.

### Authentication

- `whatif serve` binds `127.0.0.1` by default and serves no auth there: a
  person on their own machine, reading Prometheus through a port-forward, as
  the CLI does today.
- Bound to any other address it requires `--auth=kubernetes`: a bearer token
  checked with TokenReview and SubjectAccessReview on a non-resource URL
  (`/whatif/*`, verb `get`), the same filter the controller's metrics server
  uses (`filters.WithAuthenticationAndAuthorization`). TLS as the metrics
  server does it.
- On the controller it sits behind the metrics server's existing filter, off by
  default.
- The API is read only. It accepts no recording upload (recordings come from a
  server-side directory) and no Prometheus URL in a request (a server flag), so
  it cannot be pointed at an arbitrary host.
- A saved scenario can be exported pseudonymised with the CLI's existing
  redaction (`cmd/whatif/redact.go`): no namespace, pod or model names.

### Several models

Inputs and plans are per model. `GET /v1/models` lists them; the page has a
model picker; a URL carries the model and the knobs, so a link reopens the same
scenario. Models that share a quota are not priced together: that is the
[utilization-share optimizer](utilization-share-optimizer.md)'s question, not
this one.

## Can it be built on Grafana?

Partly. The operational dashboard (`deploy/grafana/operational-dashboard.json`)
uses only built-in panels and the Prometheus data source, and ships as a
labelled ConfigMap that a Grafana sidecar picks up
([monitoring](../reference/monitoring.md)). Often that Grafana is not ours.

| Need | Grafana | Notes |
| --- | --- | --- |
| Call the API | Only with a data-source plugin | No built-in data source calls an arbitrary HTTP JSON API. The usual choice is the Infinity data source, maintained by Grafana Labs but not bundled; it must be installed |
| Recompute on a control change | Yes | A variable change re-runs every query; Infinity can put `${prefill}` etc. in the request body |
| Sliders | No | Dashboard variables are dropdowns (custom, query), a text box, or constants. A slider that writes a variable needs a third-party panel plugin; none is maintained by Grafana Labs, so their status must be checked before relying on one |
| Discrete knobs instead | Yes | Replicas 1..32 and percentiles as dropdowns work well; a rate or a target as a dropdown of 20 values is usable, not pleasant |
| Curve with two points and lines | Yes, with effort | The XY chart panel draws several series; the API can return the target line, the rate line and the points as extra two-point series, so no Grafana feature is needed for them |
| Per-bucket series and targets | Yes for lines; clumsy for targets | One target variable per bucket, and the bucket list changes with the mix |
| Tabs per class | Rows, or one dashboard per class with links | Tabs exist in Grafana's newer dashboard layouts, which are not in every Grafana our users run; a disabled tab with a reason has no equivalent |
| Refusals as gaps, a badge per number | Weak | A stat panel can show a refusal text via value mappings; a flag beside every figure is not a pattern Grafana supports |
| Drag a point, compare scenarios, inverse mode | No / panels only | Inverse mode is one more panel; dragging and side-by-side compare are not available |
| Latency | Fine | One API call per panel per change; the API caches by scenario |
| Air-gapped | Plugin must be preinstalled | The bundled kube-prometheus-stack Grafana installs plugins at start from grafana.com; an air-gapped cluster needs the plugin in a custom image or volume |
| Ours to change | Rarely | The dashboard is published into the user's Grafana; we cannot install plugins there. A namespace admin may not even publish the dashboard ([monitoring](../reference/monitoring.md#if-you-cannot-write-to-the-monitoring-namespace)) |
| Auth | Workable | Infinity sends a bearer token from its data-source config, server side, to the API's Kubernetes auth |

**Verdict.** Grafana is the right place for **"where am I now"**, and the wrong
place for **planning**.

- For monitoring, no plugin is needed. The SLO analyzer already publishes the
  current picture: `llmsm_slo_predicted_ttft_seconds`, `llmsm_slo_ttft_unmet`,
  `llmsm_slo_ttft_replica_requests_per_second` (the per-replica rate at which
  TTFT meets the target), `llmsm_slo_fill`, the reasons and the flags. A small
  row on the operational dashboard can show "rate now vs rate the fleet carries
  under the SLO" per role, from those series, plus a data link "open in the
  capacity planner" carrying the model, namespace and statistic in the URL.
- For planning, a Grafana-only build is feasible where the Infinity plugin is
  allowed: dropdowns for every knob, XY charts for the curves, stat panels for
  the limits. It loses the sliders, linked points, per-bucket targets, the
  caveat badges and compare, and it depends on a plugin we cannot install in
  most users' Grafana or in an air-gapped one. Offered as an optional
  dashboard in phase 3, not as v1.

## Other front ends considered

| Option | For | Against |
| --- | --- | --- |
| **A small web page served by the API** (recommended) | Real sliders, linked points, badges, compare; no plugin; one binary; works air-gapped if the page vendors its chart library | A front end to maintain; one more port to expose |
| Grafana only | Where operators already look | See the verdict above |
| CLI only (today) | Built, scriptable, golden-tested | One point at a time; the shape of the curve and the headroom are invisible |
| TUI (terminal sliders and plots) | No browser, works over SSH | Plots are coarse; hard to show bands and per-bucket lines; not shareable as a link |
| Notebook | Flexible for a modeller | Not an operator tool; no auth or caching story |

**Recommendation:** the web page served by `whatif serve`, with the API as the
contract, and a Grafana row plus a link for monitoring. The page:

- plain HTML and a small amount of JavaScript, with a chart library vendored
  into the binary (`go:embed`), no CDN, no build step that pulls from the
  network at run time;
- no state on the server beyond caches; the scenario lives in the URL and in an
  exported JSON file;
- the container image, when there is one, on a fully qualified non-Docker-Hub
  base, as the manager's is.

## Phases

| Phase | Ships | Depends on |
| --- | --- | --- |
| 0 | `internal/whatifapi` with `inputs` and `plan` (mix line, TPOT, limits, marginal), `whatif serve` without a page; timing measured on the goldens and PS fleets | PR #16 |
| 1 | The page: critical tab, TTFT (mix line, and the "all traffic were bucket b" view) and TPOT, prefill/decode steppers, rate and target sliders, SLO statistic pickers, the binding bar, +1 deltas, flags drawer, provenance chips; recording and Prometheus sources | 0 |
| 2 | E1, per-bucket TTFT in the mix, with per-bucket targets and the binding bucket; inverse mode (`/v1/size`) | E1 in `internal/signals/whatif`; a mixed-bucket staircase to validate it |
| 3 | Controller source (handler on the metrics server, off by default); scenario save and compare; Grafana monitoring row and link; optional Infinity dashboard | 1 |
| 4 | Regular and batch classes | collector split by `priority`, recorder fields, a priority gate model |

## Risks

- **A graph makes an unvalidated number look settled.** The TTFT model reads
  up to 33% low at light load and carries `UnvalidatedTTFT` on every figure.
  Mitigation: the banner, the bands, the badges, and the API's refusal shape;
  the page never rounds a refused or flagged figure into a clean one.
- **"All traffic were bucket b" read as per-bucket in the mix.** Mitigation:
  separate view, separate label, never used for a limit (C8).
- **Classes priced as if they did not share the engine.** Mitigation: v1 prices
  the total (C9); classes wait for data.
- **Latency on slider drag.** Unmeasured; phase 0 measures it before the page.
  Fallback: price points on release rather than on drag.
- **A second front end to maintain.** Kept small by having no model in it: all
  arithmetic is in `internal/signals/whatif`, and the API is golden-tested
  against the CLI.
- **Exposure.** A read-only API that can read a Prometheus is still a data
  path. Loopback by default, Kubernetes auth otherwise, no host or file in a
  request.

## Tests

- **Parity with the CLI.** For each CLI golden in `cmd/whatif/testdata` that
  has a JSON form (`replicas-to-slo-worked-json`, `slo-to-rate-worked-json`,
  `ttft-rate-worked-json`, `ttft-replicas-worked-json`, the refusals), the
  equivalent `plan` call returns the same limits, points, `BoundBy`, flags and
  refusals. A drift between the API and the CLI is a test failure.
- **Schema file.** `internal/whatifapi/testdata/schema-v1.txt`, the field list,
  checked like the CLI's `schema-v*.txt`.
- **No number where a refusal is.** A test walks every response from the
  goldens and the synthetic fleets, and fails on a numeric field beside a
  `refused`, and on any refusal serialised as 0.
- **Curves agree with points.** At each grid rate the curve's value equals
  `TTFTAt` and `SLOAtReplicas` there; point B lies on the curve within the
  bisection's tolerance; past the knee every point is refused `Saturated`.
- **Caching.** The same scenario gives the same `scenarioId` and bytes; a
  changed knob changes the id; a rate or target change does not recompute the
  curves (counted with a test hook, as `onOldFloor` does).
- **Auth.** Loopback without a token answers; any other bind without
  `--auth=kubernetes` refuses to start; with it, a token without the SAR is
  403.
- **The page.** A headless browser test against a stub API that serves fixed
  responses: every figure with flags shows its badge, a refused limit shows its
  reason and no number, the disabled tabs cannot be selected, and the page
  loads no external URL. Run with the unit tests, not the kind e2e: the API
  needs no cluster.
- **Model accuracy is not tested here.** It is the staircase's job
  ([TTFT ground truth](slo-capacity-planner-ttft.md#ground-truth-and-the-staircase)).

## Open questions

1. **The name.** "Cluster capacity planner" plans one model's P/D fleet, not a
   cluster. Keep the name (it is what operators will search for), or "capacity
   planner"?
2. **Prompt buckets.** The shared edges (100 to 6000 tokens) put long-context
   traffic in one bucket. Use the staircase's buckets, a fixed set for long
   context (e.g. 1k, 4k, 8k, 32k, 128k), or let the user set edges?
3. **Classes.** Is the critical/regular/batch to `InferenceObjective` priority
   mapping above right, and should the class names be configurable?
4. **Default source.** Should the page open on a Prometheus window (works
   without the controller) or the controller (one cycle fresh, phase 3)?
5. **Headroom.** Should the fleet limit apply the operating-point cap by
   default (the CLI does), or show the uncapped limit with the cap as a marker
   only?
6. **TPOT percentile.** The SLO analyzer's config accepts a mean TPOT only;
   the CLI accepts a percentile with `R_q`. Offer the percentile in the picker
   from v1, flagged when `R_q` is assumed?
7. **E1 priority.** Is per-bucket TTFT in the mix worth an engine change and a
   mixed-bucket staircase before phase 1 ships, or is the single mix line
   enough for v1?
8. **Grafana.** Is an optional Infinity dashboard wanted at all, given the
   plugin cannot be installed in most users' Grafana?

## See also

- [What-if capacity](slo-capacity-planner.md): the two questions, the CLI, the
  JSON output.
- [End-to-end TTFT](slo-capacity-planner-ttft.md): the TTFT model and where it
  is most wrong.
- [SLO tiers](slo-tiers.md): the SLO analyzer, its configuration and the
  metrics the Grafana row reads.
- [Capturing a window for what-if](../developer-guide/whatif-capture.md).
