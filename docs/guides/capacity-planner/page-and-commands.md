# Capacity planner: the page and the commands

The [quick start](README.md) gets you to a first answer, and
[Read the planner's answer](read-the-answer.md) walks one through. This page
has the rest: every panel of the page, and the commands that answer the
same questions from a terminal.

The planner answers five questions about one model's prefill/decode fleet:

1. With this many prefill and decode replicas, what TTFT and TPOT do I get at
   a given request rate?
2. What rate can the fleet carry and still meet the SLO, and which role runs
   out first?
3. What changes if I add a replica, or tighten a target?
4. How many prefill and decode replicas does a given rate need?
5. How many replicas did each hour of a day need, and how many did the fleet have?

## Read the page

Above the tabs, under the source, one card holds what every view reads:
**Targets and assumptions**. It has the TTFT and TPOT statistic and target,
the safety margin (headroom), the prompt and output length to price at, and
**Assume a term the source did not learn**. The Answer view prices them, the
day plan sizes each bucket under them, and the Right-size view judges the
fleet under them. **Check the planner** reads none of them: it learns its own
terms from the source. Change one and every view that used it says so: the
Answer prices again, a day plan on screen plans again (another view's waits,
dimmed, with a note naming the targets it used, and plans again when you open
it), and a right-size answer is hidden with the targets it was judged at.

The page has four tabs:

- **Answer**: the answer first, then the figures behind it and the what-if
  controls.
- **Day plan**: the replicas each bucket of the window needed.
- **Check the planner**: the planner predicting held-out hours of your
  traffic, against what was measured.
- **Right-size**: the fleet judged at the load the source saw.

While a recording loads, a bar at the top of the page says so, with a
**Cancel**; **Load** is off until it ends. After a load the page judges the
fleet at your busiest load in the background, and the bar says what runs.
It does not check the planner's accuracy on the recording: that check takes
seconds to a minute, so it runs only when you press **Check accuracy on this
recording**.

The **Answer** tab, top down:

1. **The header.** The model and variants, the source, the window it covers,
   and a chip "Checked on your traffic" that opens the evidence ("Accuracy
   not checked yet" until you run the check). The
   self-check in full and the TTFT model's validation figures sit closed
   under the source.
2. **The answer panel.** Fleet today, the verdict, the right size, headroom,
   +25% and +50% traffic, accuracy and the SLO used, each with a badge:
   Measured, Checked on your data, or Estimate.
   [Read the answer](read-the-answer.md#on-the-page-the-answer-first) walks
   it through. **Copy as image** puts it on the clipboard as a picture.
3. **A card when prefill capacity is unknown**, with two buttons: use the
   speed the pods were seen at as the limit, or load a busier window.
4. **One notice** on how far to trust the figures. A dagger (†) marks a
   figure an experimental model priced.
5. **The figures**: the limits, then TTFT and TPOT against the rate, side by
   side. **Download CSV** and **Download JSON** export the curves and the
   plan.
6. **What if**: the fleet and the request rate, in one row (one column on a
   phone). The targets are above the tabs.
7. Closed below: saved scenarios, the flags, the inputs and a glossary of the
   planner's symbols.

The page uses plain words; the symbols stay in tooltips and the glossary:

| Symbol | The page says |
| --- | --- |
| V_P | prefill speed per pod (tokens/s) |
| knee | the limit: the rate where latency takes off |
| operating point, cap | safe load: the safety margin times the limit |
| headroom | safety margin |
| f | load imbalance across prefill pods |
| cv | prompt-length spread |
| Ca² | how bursty arrivals are |

The rest of this section describes the controls and figures, in the
planner's own terms:

- **Fleet, traffic, SLO.** The page starts from now: the replica counts the
  source saw, and the policy's SLO and headroom. With no policy objective the
  page sets one (TTFT p90 1 s, TPOT mean 50 ms); where that is under 1.5 times
  the floor the first plan priced (the least latency any load gives), it
  starts at a round value above 1.5 times the floor instead, and says so under
  the target. A policy's target, or yours, is never moved: under the floor it
  is marked "no load meets it". The source carries no request
  rate, so the rate starts at the operating point, headroom times the lower
  knee (or the one knee that priced: see "One knee refused" below). Move the rate slider, step the replicas, pick a statistic, set a target.
  A target box takes milliseconds and any value: typed outside the slider's
  range, the slider moves to take it in. The target sliders are on a log
  scale, in steps of about 2%, from a twentieth of the target or floor to ten
  times it, so every target above the floor is in reach; a slider let go where
  it stands keeps the value typed.
  The request rate is the total rate: traffic classes are not priced apart
  yet.
- **Concurrency.** Under the rate, the page says how many requests are in
  the system on average at that rate: "About 30 requests in the system on
  average (1.5 in prefill, 25 decoding (13 a decode pod))". This is the rate
  times a request's mean time in the system, TTFT mean + (OL - 1) x TPOT
  mean (Little's law). It is an average, not a peak. At or past the limit
  the queue grows without bound, and the page says so instead of giving a
  number. If your load generator keeps a fixed number of requests open,
  type that number in **Or concurrency**: the page finds the rate that
  holds that many on the fleet shown, and prices there. If the fleet holds
  fewer requests than that anywhere below its limit, the page prices at the
  limit and says the load saturates the fleet: more open requests add
  queueing there, not throughput. "In prefill" counts the requests in
  the prefill engines alone, waiting or computing their prompt; the rest of
  the time to the first token (the fixed latency, the hop, the KV pull) is
  in the total but in no role. Moving the rate clears the concurrency.
  The size mode sizes for the rate the concurrency gave: a concurrency is a
  rate only on a given fleet.
- **Targets the policy does not set.** A TTFT target is needed to read a
  prefill limit. When the policy sets none, the page uses TTFT p90 1 s and
  TPOT mean 50 ms, and says "targets set by the page, not the policy" above
  the limits until you set your own.
- **About the TTFT model.** Every TTFT figure rests on a model that is not
  validated. The notice says so once; **About the TTFT model**, under the
  source, says how far off it read on the validation recordings, which are
  not your fleet. Treat TTFT as an estimate, not a bound.
- **The most the fleet serves.** The fleet limit is the lower of the prefill
  limit (from TTFT) and the decode limit (from TPOT), capped at headroom times
  the lower knee. When the cap is what sets it, the figure without headroom is
  shown beside it, with the role that would set it then. The bar shows both
  roles and the fleet on one axis, with the rate you chose. "binds" marks the
  role whose limit sets the fleet figure.
- **One knee refused.** The cap needs both knees. When the source cannot price
  decode's knee (`NoLine`, `PinnedDefaultB`), the cap and the fleet limit are
  refused, not shown at prefill's rate; with no TPOT target, decode's limit is
  too, since nothing else bounds decode. The rate then starts at headroom
  times the prefill knee, and the page says so under the rate: it is a place
  to start, not an operating point. Type your own rate. The prefill curve,
  the prefill limit and the TTFT figures stay. A refused prefill knee works
  the same way, starting from the decode knee. A source with no prefill
  variant at all keeps a cap from decode's knee alone.
- **+1 prefill, +1 decode.** The fleet limit with one more replica of each role.
  Adding a replica of the role that does not bind changes nothing, and the page
  says so.
- **The graphs.** The TTFT and TPOT curves against the rate. The dashed lines
  are your target and your rate; the open circle is the figure at your rate,
  the filled circle the highest rate that meets the target, marked "above the
  cap" when the cap stops the fleet first. Past the lower knee the model
  answers nothing, and the region is shaded. A gap in a curve is a rate the
  model refused: hover it for the reason.
- **"If all traffic were bucket b".** A checkbox under the TTFT graph adds one
  line per prompt size, each priced as if every request had that prompt. It is
  a what-if about a traffic shift, not the TTFT each size sees inside today's
  mix, and no limit is read from it.
- **Price at.** Prices the whole page, limits included, at a prompt length
  you state instead of the current mix, and **Output length** at an output
  length you state. The page then says so above the limits, and every figure
  is flagged `AssumedShape`.
- **TPOT percentile.** A percentile needs the ratio of that percentile to the
  mean. If the source did not learn it, type an assumed ratio; every figure
  that rests on it is flagged `AssumedTPOTRatio`. A learned ratio is read per
  KV-fill band of 0.2 where the source saw at least 5 windows in the band, so
  the tail can widen as load rises. A band with fewer windows uses the ratio
  read over every fill, and figures priced there are flagged
  `GlobalTPOTRatio`. No window is read below its true percentile. Where
  the router's histogram buckets are wide, it is read at the bucket's top, or
  lower where the measured mean proves it, flagged
  `MeanBoundedTPOTPercentile`; p90 and p99 in one wide bucket read the same.
  The ratio is the median window's plus a margin; from 6 windows or fewer, in
  all or in one fill band, it is the worst window, and the CLI says so.
  Finer router TPOT buckets give a firmer figure. On a decode variant whose
  step is host-bound (its measured intercept and floor under 5 ms, as a
  sub-1B model's), a ratio read off coarse buckets is refused,
  `TPOTBucketTooWide`: the tail there is host jitter the line does not
  model. Ask for the mean, or type an assumed ratio.
- **Flags and inputs.** Every figure carries the flags it rests on. The
  **Flags** drawer lists each once, in words, with what clears it. The
  **Inputs** drawer shows each term with its source, the terms the source
  refused, and how many you typed.
- **Right-size: is this fleet the right size?** On the Right-size tab, judge
  the fleet the knobs set at the load the source saw.
  1. Add a row per request shape: IL and OL in tokens (`k` for 1024) and,
     optionally, the prefix hit rate. A blank row is the shape the traffic
     ran at when it was busiest (`load`), so the verdict prices the load at
     the traffic that made it.
  2. Type the loads, comma separated: `80%` of each shape's knee, `5r/s`, or
     `seen`. Set **Load seen times** to 1.5 to add a column and a line for 50%
     more traffic; the verdict stays at the load seen.
  3. Pick the load seen to judge at: its p95 over the window less the
     one-minute rate's counting noise (the default, the busy spells), its
     p95, its mean, or its peak.
  4. Press **Judge the fleet**.

  The answer reads top down:
  - **The load judged**, and the mean, p95 and peak the source saw.
  - **Verdicts**, one sentence per shape at that load: UNDER-PROVISIONED
    (and which role to add), RIGHT-SIZED, or OVER-PROVISIONED (and which
    role can lose a replica), with the fleet it needs in a box: "needs 3
    prefill + 4 decode (has 2 + 2)", other fleets that also serve as "or 4 +
    3", "at least" when prefill is not priced, or "no fleet up to 32
    replicas serves this". With a growth set, a second line under each
    says what the grown traffic does. A typed shape is a what-if, marked "if
    all traffic were this shape". "decode only" marks a verdict the prefill
    side could not price.
  - **The matrix**, one row per shape and one column per load. A cell is the
    TTFT and TPOT at your SLO's statistics, at that load's rate, and a word:
    `ok` every target met, `MISS` a target missed (`MISS at onset` at the
    flow-control onset), `SAT` saturated, `?` not
    judged, `no SLO` no target set. The colour repeats the word. A TPOT
    marked `≤` is the decode line's floor: the load sits below the fill
    the line was fitted over, so the TPOT is at most that. A shape
    outside the lengths the source saw is marked EXTRAPOLATED.
  - **Each shape**, closed until you open it: the knees, how busy decode and
    prefill are at the load, the numbers behind the verdict, and what the
    figures rest on.
  - **How accurate is this?** What no run has validated yet.

  The report uses the fleet under What if and the targets, headroom and typed
  terms above the tabs, and says the targets it was judged at. Change a knob
  and the page hides the answer and asks you to judge again, naming the
  targets it was judged at. A server from before
  the report says so under the button; the rest of the page works. The same
  report is `capacity-planner at-replicas` and `POST /v1/report`.

A refused figure, or an error, is explained in
[Troubleshooting](troubleshooting.md).

### Why decode capacity can drop on prefix-heavy traffic

A prefix-cache hit saves prefill work. It does not save decode memory, unless
another running request holds the same blocks. A request that reuses an old
prefix still keeps its whole context in the KV cache while it decodes. So the
planner prices decode at the KV a running request was measured to hold, not at
the prompt minus its cached part. On traffic with long, mostly cached prompts,
fewer requests fit in the cache than the hit rate suggests, and the decode knee
comes out lower than a planner that trusts the hit rate.

The `kvPerRequest` term says where its figure came from:

| Source | Meaning |
| --- | --- |
| `measured` | Tokens in use over running requests, on the decode rows. |
| `measured, scaled to the shape asked` | That reading, scaled to the prompt and output you typed. |
| `..., above the context` | Added to either one above when the reading is more than twice the whole context. The engine keeps extra KV per request, such as a draft model's. It is priced as read. |
| `measured below the prefix-hit discount, priced at it` | The reading was below the hit-discounted figure, so the discounted figure is used. |
| `measured above the context on SGLang, priced at the context` | SGLang reports its busiest rank against the whole pod, so the reading is held at the whole context. |
| `context, no resident reading` | No usable reading. The whole context is priced: the prompt plus half the output, more where output lengths vary. |

## When a term is refused

A source that did not see enough load cannot learn every term, and the page
then shows refusals instead of numbers. Open **Assume a term the source did
not learn**, above the tabs, and type a value: the prefill rate V_P, the fixed latency a, the
prompt cv, the router hop, or decode's ITL B. A refused term's box is empty,
with the reason under it. A typed value flags every figure that rests on it.
[Each reason and its fix](troubleshooting.md#refused-figures).

A box whose value the source learned shows that value, muted and tagged
**learned**: the boxes above, the output length (the observed one) and R_q
(the one the plan priced, at a percentile TPOT). A learned value is not an
assumption: the page does not send it, the server uses its own, and no figure
is flagged for it. Edit the box and the value is yours: tagged **assumed**,
sent, and flagged, as before; **Use learned** beside it puts the learned value
back. Emptying the box, or typing the learned value, does the same. A V_P
learned from the served peak is tagged **lower bound**: the note says the real
speed is likely higher, and prefill is not sized until you type a V_P (that
value too, typed, is yours). A link or a saved scenario that carries a value keeps
it as an assumption.

The box opens by itself when a term is refused, and when V_P is only a lower
bound: the prefill pods never queued, so the source saw their load, not their
speed. Prefill's limit is then unknown, a day plan sizes decode alone, and
the note beside **Prefill speed V_P per pod** says so. Type a V_P there to
size prefill; the day plan's refusal points to the same box.

The same box holds **My router balances prefill by load**. Tick it when the
router scores each prefill pod's queue (llm-d's EPP queue scorer): it is the
CLI's `--load-aware-router` and the API's `assume.loadAwareRouter` (**A
load-aware router** under [Check a fleet at N replicas](#check-a-fleet-at-n-replicas)
says what it does). It is off by default, the link
carries it, and with it ticked every figure reads outside the validated
region.

Decode refused with `PinnedDefaultB` is the common case on a real P/D window:
the latency line's intercept B, the per-token latency with the cache empty,
was never measured, because no window spread the decode Pods' KV fill far
enough for a full fit. The planner does not take B from the low-load readings
on its own, as the controller does: on the recordings checked, a B taken that
way read TPOT well under what was measured. The **ITL B** box shows the B the
floor below the band gives; type it, or your own. On the CLI, `--assume-itl-b floor` uses that B
and `--assume-itl-b 12ms` states one. Figures on it are flagged
`AssumedITLB` and read as outside the validated region.

A fleet whose load never goes quiet still gets two terms, each flagged:

- **Prefill's fixed latency a, from the engines' own timing**
  (`AFromEngineTiming`). A busy fleet has no quiet minutes to measure a from.
  The planner then fits vLLM's prefill time against the tokens each request
  computed, using the less busy minutes, and adds the time a request spends
  on the prefill pod outside prefill and queueing. The assumed line gives the
  figures it used. The value holds for the prompt length and cache hit rate
  it was measured at. If even the shorter prompts compute more than 2,500
  tokens, a is extrapolated from long prompts and may read low; the assumed
  line says so.
- **A prefill knee raised to the work the fleet served** (`KneeAtServedWork`).
  Where V_P is only a lower bound, the prefill knee is at least the rate the
  prefill Pods' sustained computed-token work serves at the prompt priced
  (see below, after the lower-bound knee).
- **V_P raised with that knee** (`VPAtServedWork`, only with the replay
  switch `prefill-vp-at-served-work` on; off by default). Where the knee was
  raised to the served work, V_P is raised by the same ratio for the TTFT, so
  the TTFT is priced from the V_P the knee rests on. Off, the TTFT is priced
  from the lower V_P, and a rate under the raised knee can read the prefill
  Pods past their capacity. The assumed line gives the ratio.
- **A lower KV-fill floor for the decode latency line** (`AdaptedBandFloor`).
  The line is fitted over a range of KV fill. A fleet whose decode pods never
  fill far enough above the default floor (0.15) never gets the 0.30 range a
  fit needs. When at least 3 engines read below the floor, the planner lowers
  it to the 10th percentile of the engines' fills over the last 30 minutes,
  never under 0.05. It raises it back only after the fill has spread 0.35 for
  2 minutes. The flag gives the floor used.

**A fleet whose prefill never queues (experimental).** V_P is learned only
from minutes in which a prefill pod queued, so such a fleet gets V_P refused
(`NoPrefillCapacity`) or only a lower bound. `--prefill-vp-plateau` reads V_P
instead from where the prompt tokens served stop rising with the requests in
prefill per engine (the occupancy plateau). Where that curve has flattened
the level is V_P; where it is still rising it is a lower bound, and the
answer keeps the "at least" reading of a lower-bound knee. It is off by
default and applies to the one command you pass it to: `slo-to-rate`,
`replicas-to-slo`, `at-replicas`, `backtest`, `predict` and `day-plan` take
it, and `POST /v1/inputs` takes `prefillVPPlateau` ([API](api.md)).
Figures priced from it are flagged `VPFromPlateau` and read OUTSIDE
VALIDATED REGION, and the assumed line says "prefill speed from the
occupancy plateau (experimental, --prefill-vp-plateau)". The source of V_P
is `occupancy-plateau` (`vpOrigin` in `-o json`). It is off by default
because it has not been validated: on the public recordings its level reads
0.96 to 1.08 of the V_P learned from a queue, but most fleets it gives a
V_P to still lack a (`NoFixedLatency`), and TTFT priced just under a knee
from its lower bound read far over what was measured. Try it to see what a
fleet that never queued would need, for example
`capacity-planner at-replicas --recording run.jsonl.gz --ttft 2s@p90 --prefill-vp-plateau`;
do not size from it alone.

**Pricing another prefill pod count when prompts share prefixes.** With a
cache-aware router, each shared prefix stays on one prefill pod. More pods
split the shared prefixes, so each pod's cache holds more of its own and the
prefix hit rises; fewer pods lower it. By default the planner keeps the hit
it measured at every count, which reads the prompt tokens a pod computes too
low when you price fewer pods. `--prefill-hit-at-count` prices the hit at
the count you ask for.

If the recording holds vLLM's KV residency histograms (start vLLM with
`--kv-cache-metrics`; see [the metrics](metrics.md)), the planner fits how
long a cached block waits to be reused and prices the hit from it at fewer
pods and at more (the reuse-gap rule), flagged `PrefixHitAtCount`. A tier
past the GPU (CPU offload, a KV connector) keeps the total hit it served,
and the planner prices the load of the tokens that move to it. The
recording does not say how large that tier is, so at fewer pods the total
does not fall: an optimistic reading if the tier is small. On a lab run
recorded on 2 prefill pods, with the histograms rebuilt from its load
generator (the run had no `--kv-cache-metrics`), it read 0.29 at 1 pod
(measured 0.345) and 0.82 at 4 (measured 0.79). Those histograms follow
the same law the planner fits, so the run checks the arithmetic, not the
law against a real engine. At 1 pod the hit reads low because one pod
count does not show how many pods hold a copy of a prefix; that prices
more computed tokens, the safe side. With no tier past the GPU the most the
hit can reach is not measured, so at more pods the planner lets the missed
share of the prompt fall at most as fast as the pods rise, a stated limit
rather than a measured one: it priced 0.76 at 4.

Without those histograms:

- fewer pods: the hit times the new count over the recorded one, a safe
  floor (each request computes more tokens, so the prefill knee falls),
  flagged `PrefixHitAtCount`;
- more pods: the measured hit, kept, flagged `PrefixHitHeld` (the real hit
  is likely higher, so capacity reads low);
- the recorded count: nothing changes.

The floor assumes the router keeps each shared prefix on one pod at both
counts. Where load breaks that at fewer pods, the hit can fall below the
floor. The recorded count is the most prefill pods in the 20 cycles (five
minutes at the default cycle) the hit was read over, so a recording made just after a scale-down is priced
from the larger count.

The assumed line names the rule and the range the real hit lies in, and
`-o json` carries it as `hitAtCount`. It is off by default; `slo-to-rate`,
`replicas-to-slo`, `at-replicas`, `backtest`, `predict` and `day-plan` take
it, and `POST /v1/inputs` takes `prefillHitAtCount` ([API](api.md)). For
example, from a recording on 2 prefill pods:
`capacity-planner slo-to-rate --recording run.jsonl.gz --ttft 2s@p90 --prefill 1 --prefill-hit-at-count`.

**V_P from the prefill service line (`--prefill-vp-service-line`, not
validated).** Where the prefill pods never queued, the planner has only a
lower bound on their speed. This flag reads it from each pod's prefill time
against the tokens it computed a request instead: a fixed time a request
plus a time a token. It is off by default because on the public recordings
it read some TTFT figures under what was measured. Set, every figure reads
OUTSIDE VALIDATED REGION. The same commands take it, and `POST /v1/inputs`
takes `prefillVPServiceLine` ([API](api.md)).

**f against the load (`--prefill-f-load-curve`, lab-validated only).** The
busiest prefill pod's extra share is largest at a light load. This flag fits
f, the imbalance, against the tokens a pod computes a second at each prefill
count, `f(u) = f_inf + delta/u`, and prices the higher of that and the usual
f at each rate, so a light load reads the higher f it showed. A count with
fewer than 8 minutes, or whose f does not fall with the load, keeps its f,
and the answer says so. Set, every figure reads OUTSIDE VALIDATED REGION. The
same commands take it, and `POST /v1/inputs` takes `prefillFLoadCurve`
([API](api.md)).

**An experimental TTFT model (`--experimental-ttft`, for demonstration).**
The TTFT model has seven switches that are off by default, because none of
them has passed the release's validation. `--experimental-ttft` turns all
seven on for the one command you pass it to:

- `prefill-vp-window-best`: V_P read over the whole recording, not only its
  last 6 hours;
- `prefill-vp-at-served-work`: V_P raised to the work the prefill Pods
  served, where the prefill knee was raised to it;
- `slo-ttft-computed-hit`: a stated prompt netted by the computed-token hit
  (the load's shape, a day plan's bucket and a backtest's step are netted by
  it with the switch off too);
- `slo-ttft-tier-load` and `slo-ttft-tier-kv-wait`: the load of a cached
  token from a host, disk or peer tier, at the engine's measured wait for it;
- `slo-ttft-ps-by-engine`: the prefill queue priced as processor sharing
  where the engine's flags say it shares its steps;
- `slo-ttft-hop-window-means`: the router hop read on the means of its
  windows, on a fleet whose traffic is too noisy for the per-cycle gates.

`slo-to-rate`, `replicas-to-slo`, `at-replicas`, `backtest`, `predict` and
`day-plan` take it, and `POST /v1/inputs` takes `experimentalTTFT`
([API](api.md)). The page has no switch for it: start `serve
--experimental-ttft` (in a cluster, add it to the StatefulSet's `args`) and
every answer on the page uses it. Every prefill and TTFT figure of such an answer is flagged
`ExperimentalTTFT` and reads OUTSIDE VALIDATED REGION (experimental TTFT
model: processor sharing by engine config, router hop from window means,
prefill speed raised to the served work; not validated). The assumed line
lists the switches it turned on, the self-check names the flag, and the
footer says so. Without the flag the output is the default's, byte for byte.

How it read: on a production P/D fleet, fitted on the first half of a day
and tested on the second, the TTFT it priced read about 1.3x what was
measured at the mean, 1.35x at p90 and 1.7x at p95, none under 0.8. Those
figures are upper bounds: they rest on a V_P that is only a lower bound. The
self-check's folds on the same recording read the p90 TTFT about 0.76x of
measured, and called it inconsistent. It is not validated: it is for
demonstration only, to show what the model can do on such a fleet, never to
size from.

**TTFT on a fleet that never goes quiet is not validated: prefill capacity
V_P is a lower bound.** Such a fleet's prefill pods queue at every load, so
the planner's prefill capacity is under what the fleet serves, and the decode
side's share of TTFT is not priced. TTFT and prefill sizing can be wrong in
either direction; decode TPOT is usable. Every TTFT answer on such a fleet
says so.

Limits of both:

- A recording must hold every rank's engine rows. Where only the leader's
  are scraped, the planner refuses it
  ([troubleshooting](troubleshooting.md#a-recording-is-refused-engine-rows-do-not-stand-for-the-fleet)).
- The quiet-minute test per engine assumes a group's data-parallel ranks
  share its load evenly, and context-parallel ranks (PCP, DCP) are not
  counted as engines.
- The lowered floor is the same on every replay of one recording, but a
  different window of the same fleet can lower it to a different floor.

### Serve with the experimental v2 models

`backtest` has two more experimental models, each with its own flag
([how they read](accuracy.md)): `--experimental-ttft=v2` prices TTFT with
limited processor sharing per prefill rank, and `--experimental-tpot=v2`
prices the TPOT percentiles with a per-request TPOT distribution. Give both
to `serve`, and every check the page runs uses them, with no switch on the
page:

```bash
bin/capacity-planner serve --recordings ./recordings --experimental-ttft=v2 --experimental-tpot=v2
```

In a cluster, add both to the StatefulSet's `args`
([in-cluster](in-cluster.md#or-show-the-experimental-v2-models-on-the-page)).

- **What they change.** Checks (`POST /v1/backtest`, the page's
  "Check the planner"): a request that names no model is priced as if it
  asked for `ttftModel` `rank-mixture` and `tpotModel` `renewal-lognormal`
  ([API](api.md#the-backtest)); one that names a model keeps it. With
  `--experimental-ttft=v2` the main view's TTFT too: the model is fitted on
  the loaded source, and where it is identified it draws the TTFT curve,
  point A and the report's TTFT tables, also where the planner refuses
  them (a prefill that never queued). See
  [The v2 TTFT curve](#the-v2-ttft-curve). Sizes follow, from the model's
  prefill limit. With `--experimental-tpot=v2` the main view's TPOT p90
  and p95 too: see [The v2 TPOT curve](#the-v2-tpot-curve). A day plan
  prices with them too when asked: the page asks whenever the loaded inputs
  carry a model, so the day plan matches the Answer view (see
  [Plan replicas over a day](#plan-replicas-over-a-day)). Predictions are
  the default's. `=v2` does not turn
  on the seven switches above; plain `--experimental-ttft` does.
- **The v2nl-line variant.** `--experimental-ttft=v2nl-line` in place of
  `=v2` does all of the above with the variant
  ([how it reads](accuracy.md#the-measured-non-queue-variant---experimental-ttftv2nl-line)):
  every check, the main view, sizes and `at-replicas` are fitted and priced
  as `backtest --experimental-ttft=v2nl-line` fits and prices them. The
  TTFT caption and the inputs name it "Experimental TTFT model v2nl-line"
  (`ttftModel.model` `rank-mixture-nl-line`), and its held-out score comes
  from folds fitted the variant's way. It abstains more often than v2,
  wherever its fit window does not price its own mean and p90, and says
  why, as v2 does.
- **The variant's modes.** `--rank-mixture-kv-hold` and
  `--rank-mixture-remainder-spread`, with `=v2nl-line`, turn the variant's
  two modes on for every check and source, as on `backtest`. Give them
  together ([accuracy](accuracy.md)). `--rank-mixture-remainder-server`,
  with both of them, adds the third: the remainder drawn from the prefill
  pods' API servers, where the recording holds their series.
- **Shorter recordings.** Both TTFT models abstain on a fit window under
  12 hours, which every lab recording is. `--rank-mixture-min-fit-window 2h`
  lowers that minimum for every check and source; each TTFT a model prices
  from a window under 12 hours is flagged `RankMixtureShortFitWindow`.
  `backtest`, `at-replicas` and `day-plan` take the same flag.
- **All of them at once.** On a production recording of 12 hours or more:

  ```bash
  bin/capacity-planner serve --recordings ./recordings --experimental-ttft=v2nl-line \
    --rank-mixture-kv-hold --rank-mixture-remainder-spread --experimental-tpot=v2
  ```

  On a lab recording, add `--rank-mixture-min-fit-window 2h` (no longer than
  the recording) and `--tpot-dist-min-fit-steps 8`: the TPOT distribution
  otherwise needs 24 five-minute fit steps. Each percentile it prices from
  fewer than 24 is flagged `TPOTDistributionShortFit`.
- **The planner's opt-in switches.** `serve --prefill-vp-plateau` and
  `serve --prefill-hit-at-count` read every source as if its request had
  asked for the switch ([flags](api.md#flags)); the figures they move
  carry their flags and read outside the validated region.
- **What the page shows.** The summary says, for each model, that it is
  experimental and reads outside the validated region, with its fit; or
  that it abstained and why, and every figure is then the planner's own.
  For TPOT it adds in how many steps the predicted p90 and p95 fell in the
  measured one's joint bracket, and under or over it, beside the planner's
  own count ([the joint bracket](accuracy.md#where-a-measured-tpot-p90-or-p95-lies-the-joint-bracket)). The step table
  shows mean, p90 and p95 of TTFT and TPOT, predicted against measured.
  Each figure a model priced is marked `[experimental TTFT v2 ...]` or
  `[experimental TPOT v2 ...]`, one it abstained on `[... v2 abstained ...]`,
  and each TPOT p90 and p95 says whether it is in the measured bracket.
- **What it costs.** Every source the server loads is read once more for
  each model, so loading is slower.
- **The draws.** The v2 TTFT model reads each prefill rank's service from
  the computed-token histogram itself, so its figures barely move with the
  random draws ([accuracy](accuracy.md)). To price from each rank's own
  draws instead, the older way, add `--rank-mixture-stratified=false` to
  `serve`, `backtest` or `at-replicas`.
- Without the flags, the server's answers are the default's, byte for byte.

#### The v2 TTFT curve

- **Where it is drawn.** Up to the planner's own lower knee, never to the
  knee the model would read from its fitted prefill service rate: that one
  read 1.06 to 1.56 times the measured capacity on lab recordings. Where
  the prefill knee is only a lower bound and the fleet served more than it,
  the curve runs on to the highest load served, since the fleet served it.
- **Solid, then dashed.** Solid up to the highest load the source served;
  dashed beyond it, labelled "extrapolated (beyond the highest load
  measured)". Where the prefill knee is only a lower bound, or refused, the
  caption adds that the curve's right end is an estimate from the fitted
  prefill service rate.
- **The evidence.** The caption names the "Experimental TTFT model" and,
  where the source holds enough, its score on the source's own held-out
  folds: "held-out steps: N of M within 20%", for the mean.
- **Where it is not used.** A stated request shape, a stated prefill term
  (V_P, a, prompt spread, router hop) or a stated arrival burstiness: the
  model prices the source's own traffic, and the planner's TTFT stands. The
  same where it abstains (under 12 h of source, prompts that did not
  spread, the ranks' own rates or max-num-seqs not recorded).
- **Other fleets.** At another prefill count each replica's ranks take the
  recorded share profile at the rate per replica.
- **Limits and sizes.** Where it prices a fleet's TTFT it also sets the
  prefill limit (the highest rate whose TTFT meets the target, read off
  its curve) and the fleet's (the lowest of the safety margin times the
  curve's end, that rate and decode's limit), so a size works where the
  planner refuses one. A limit set at the curve's end, where the prefill
  knee is only a lower bound, is "at least" that rate (flagged
  `VPLowerBound`). One above the highest load measured is `extrapolated`.
- **The CLI.** `at-replicas --experimental-ttft=v2` prices its tables' TTFT
  the same way, from a `--recording`. Its verdict and summary cells stay
  the planner's own.

#### The v2 TPOT curve

- **What it moves.** Only the TPOT p90 and p95. The mean TPOT is the
  planner's own. The model places a per-request distribution around that
  mean ([how it reads](accuracy.md#experimental-the-per-request-tpot-distribution)),
  fitted on the loaded source, so each percentile is the mean times one
  ratio: the source's decode spread and output lengths, the same at every
  rate. The plan's `tpot.ratio` names it, with the source
  "experimental TPOT model".
- **What it prices.** The TPOT curve, point A, and decode's limit at a p90
  or p95 target: the rate whose mean meets the target over that ratio. A
  size at such a target sizes decode on it. The report's measured-shape
  table prices its TPOT percentile the same way. The report's verdict,
  capacity and summary cells stay the planner's own. A day plan asked for
  the experimental models sizes decode on it.
- **Solid, then dashed.** As the TTFT curve: solid up to the highest load
  the source served, per decode replica, and dashed beyond it. Each figure
  past that load carries `extrapolated` true, a size's steps among them.
  Such a figure is marked, not refused: the model's ratio is the source's,
  and the planner's own ratio is read the same way.
- **The evidence.** The caption names the "Experimental TPOT model" and its
  score on the source's held-out folds, at the measured mean: in how many
  steps the predicted p90 and p95 fell in the measured one's joint bracket:
  the router's TPOT histogram and, above their first edge, the decode
  engines', read together at their sampling noise
  ([how](accuracy.md#where-a-measured-tpot-p90-or-p95-lies-the-joint-bracket)).
  Each fold fits the model on the steps before its
  cut alone, so a source whose traffic changes over the window can score
  low here while the curve still lands in the measured buckets (see
  [Known issue](#known-issue-a-low-held-out-score)).
- **Where it is not used.** A statistic other than p90 or p95, a stated
  output length or a stated R_q (`assume.tpotRatio`): the planner's own
  percentile stands. The same where it abstains (under 2 h of steps with the router's
  TPOT histogram and the output lengths, max-num-seqs not read, more than
  1 % of sequences waiting for a slot, or C at its bound).
- **Other fleets.** At another decode count the ratio is the recorded one,
  and the assumed lines say so.
- **The self-check.** With the model on, the source's self-check prices its
  TPOT p90 with it too, each fold fitted on the fold's own window.
- **The CLI.** `at-replicas --experimental-tpot=v2` prices its tables' TPOT
  percentile the same way, from a `--recording`, at `--shape measured`.
  The default shape, the load's own, stays the planner's.
- **The per-length mixture.** `--experimental-tpot=v2-mix` (`tpotModel`
  `renewal-lognormal-per-length`) prices the same curve with one lognormal
  per output-length group in place of one at their mean
  ([accuracy](accuracy.md#experimental-the-per-length-mixture---experimental-tpotv2-mix)).
  Every rate is priced at the source window's pooled lengths, so the ratio
  is again one per source. It is off unless given.

##### Known issue: a low held-out score

On the repo's Qwen3-32B staircase recording the held-out score reads "p90
in the measured p90's joint bracket on 0 of 22", every one over it, while the curve at the rates served
lands in it on 24 of 29 steps. The two folds fit on the window's early
steps, whose outputs are long, so the renewal term barely moves their
spread and C is poorly pinned there: it comes out 19.5 and 16.4, against
7.6 on the whole window. Placed on the later short-output steps, that C
puts the p90 at about 0.022 s, just past the router's 0.02 s bucket edge,
while the measured p90 sits under it. The router's buckets there are 0.01
to 0.02 s and 0.02 to 0.04 s, so a 10 % miss reads as a whole bucket. The
score is shown as computed.

## Find the fleet a rate needs

Above the controls, pick **What fleet does this traffic need?** Then set the
rate and the targets as before. The page finds the fleet of the fewest GPUs
that serves that rate under your targets, and the replica counts become its
answer: you read them, you do not set them.

- **The answer.** The fewest GPUs: each role's replicas times its GPUs per
  replica, as the source states them. Of two fleets with as few GPUs, the
  one with fewer replicas, then the one with fewer decode replicas. When
  both roles take the same GPUs a replica, that is the fewest replicas.
  When they differ it can trade one role for the other: with 8-GPU prefill
  and 1-GPU decode replicas, 8 prefill + 5 decode (69 GPUs) rather than
  9 + 4 (76 GPUs). When the source does not state both roles' GPUs, it
  counts replicas. Beside it: the fleet limit at that fleet, the headroom
  over your rate, and the role and target that bind.
- **Why not fewer.** One prefill replica fewer, and one decode replica fewer,
  each with the rate it would serve and how far short of yours that is.
- **The worst corner.** The answer meets your rate at the learned prefill
  speed (V_P) and imbalance (f). Those are estimates, and the page also prices
  them 15% either way. When the answer falls short at the worst of those, the
  page says so: "at one corner of V_P and f this fleet serves only X r/s".
  Tick **Size for the worst corner of V_P and f** to find the fleet that
  serves your rate there too. It may need more replicas and takes longer.
  Tick it when V_P is a value you typed rather than one the source learned,
  or when the fleet must hold the rate even if V_P reads high. If the worst
  corner puts the TTFT target under the floor, the page says so: no replica
  count helps, so raise the target or untick the box.
- **Decode admission.** When decode admission holds the prefill limit down,
  the page notes it: there, decode replicas lift TTFT more than prefill
  replicas do, and the answer has weighed the two.
- **The rest of the page** is the fleet it found, exactly as the first mode
  would show it if you set those counts yourself.
- **Search up to.** The most replicas of a role the page tries, 32 unless you
  change it (at most 64). When no fleet within it serves the rate, the page
  says so, with the reason, what the largest fleet would serve, and what to
  change.
- **How long it takes.** Each answer searches several fleets, then draws the
  graphs of the fleet found, which takes most of the time. Expect about 20 s
  on a TTFT fleet from cold, about 25 s at 200 r/s, and about 45 s for the
  worst corner. The page dims the last answer and counts the seconds while it
  works. It prices the rate when you let go of the slider, and a new rate or
  target cancels a size still running.

Switch back to **How much does this fleet serve?** to step the replicas from
the fleet it found.

Each plan has a time limit, 5 minutes unless the server was started with
another `--plan-budget`. A plan past it is refused with `PlanBudgetExceeded`
([the plan budget](api.md#the-plan-budget)).

## Check a fleet at N replicas

To ask "is this fleet the right size for the traffic it saw?", run
`capacity-planner at-replicas` on a recording or a Prometheus window
(`--prometheus` and the capture flags, as for `replicas-to-slo`). Set the fleet
with `--prefill` and `--decode`; without them it judges the recorded counts.

<!-- guide:fleet.check start -->
```bash
# On the staircase sample, downloaded as the first sample was: two prefill and two decode replicas, judged at the
# load it saw, with what that load grown by a fifth does beside it, for its own shape and two it never saw. The
# sample learned no prefill speed, so V_P and a are assumed. Leave out --shape and --load for the shape the load ran at,
# 50%, 80% and 100% of the knee and the load seen.
curl -fsSL -o recordings/ttft-staircase-b-on.jsonl.gz \
  https://raw.githubusercontent.com/llm-d-incubation/llm-d-scaling-manager/${V}/internal/engines/analyzers/learnreplay/testdata/goldens/ttft-staircase-b-on/recording.jsonl.gz
bin/capacity-planner at-replicas --recording recordings/ttft-staircase-b-on.jsonl.gz \
  --assume-vp 200000 --assume-a 20ms --ttft 2s@p90 --tpot 30ms \
  --shape measured --shape 1k/1k --shape 16k/200/0.3 \
  --load 50% --load 80% --load 5r/s --load seen --load-seen-x 1.2
```
<!-- guide:fleet.check end -->

It replays the recording once and answers, for each shape and load:

- **Load judged.** The p95 of the arrival rate the window read, one reading
  per cycle with traffic, less its counting noise (below). The report prints the mean, p95 and peak, and the
  busiest 2 minutes (a rolling mean). `--load-statistic mean|p95|peak|p95-modulation` picks
  the one judged; `--load-seen` states a load instead. The mean hides a busy
  part: on the staircase sample it is 31.6 r/s, the p95 41.7 and the peak
  43.2.
- **Counting noise.** Each reading is a 1-minute rate, so even a steady
  Poisson stream reads above its rate at the p95 (about 21% at 1 r/s, 6% at
  12 r/s), and the queueing model already prices that noise. The report
  prints the p95 less it, `p95 less counting noise`, and judges at it
  (`--load-statistic p95` judges at the p95 itself). A steady load is then
  judged near its mean, a load that swings near its p95. Readings 15 s apart
  overlap, so their spread holds only part of a minute's noise (about 0.86 of
  it over 20 readings, a 5-minute `day-plan` bucket; 0.99 over the
  staircase's 282); only that part is taken out.
- **The load's shape.** Under each shape the report prints the shape of the
  cycles the load was read from (for the p95, every cycle at or above it).
  When the mix moved in the window, the measured shape (the last cycles')
  is not that traffic: a measured shape more than 20% off it in prompt or
  output length is flagged `ShapeLoadMismatch`. So the report prices the
  load at its own cycles' shape by default (`load`), and the measured shape
  only where no cycle of the load read one (a stated `--load-seen`).
  `--shape measured` asks for the last cycles' shape. On the guide's sample
  the p95, 29.7 r/s, ran at 1000/500 tokens while the measured shape is
  4569/1855: asked at the measured shape the report gives no verdict (NOT
  JUDGED, exit 3); at the load's own shape it needs 4 + 2, what `day-plan`
  sizes the same rate at.
  On a P/D fleet the load's prompt length comes from the decode pods, and
  their prefix hit is not the prefill pods'. The prefill side therefore
  prices the load's prompt less the share the prefill pods did not compute
  (cache hits on every tier: GPU, host memory, peer), the same work the
  measured shape prices. The answer says so: `prompt netted by the prefill
  pods' computed-token hit`. A day plan's buckets and a backtest's steps
  read their prompt the same way, so their prefill is priced the same way.
- **The load's shape as the default.** A model change becomes the default
  only where it moves no figure on the public recordings to under 0.8 of what
  was measured. On the decode-only sample (`throughput-sweep`) the load's
  shape prices TPOT at the p95 load, 26.9 r/s, at 2.79 ms against 3.14 ms at
  the measured shape; the decode engines measured 3.34 to 3.65 ms in those
  very cycles, so it reads 0.84 of measured at worst. It reads closer on the
  Qwen3-32B warm-up (TTFT p90 941 ms at the load's 1k prompts, where the
  measured shape saturates; the run measured 287 ms at that rate).
- **The p95 less counting noise as the default.** `p95-modulation` is the
  default here and in `day-plan --size-for`: the plain p95 counts the noise
  the queueing model already prices. With a whole minute's noise taken out
  (before the overlap share above), it moved the judged load by at most 0.06 r/s
  (0.2%) on every recording we hold, and no verdict or need. In `day-plan`
  it sizes for up to 23% less at a bucket and moved one bucket's need on
  three of seven recordings, by one replica (two Qwen3-32B warm-ups, sized
  for 0.38 against 0.48 r/s and 4.21 against 4.70 r/s, and the guide's
  sample, 1.68 against 2.00 r/s). Every recording we hold is
  a staircase or a sweep, whose loads swing; the case it is for, a steady
  low rate on a slow model, has no recording of its own yet, with its
  latency, to show the fleet sized at the modulated load still holds the
  SLO. `--load-statistic p95` and `--size-for p95` keep the old figure.
- **Growth.** `--load "seen x1.5"` (also `seen×1.5`, `seen*1.5`) asks what
  50% more traffic does: a column of its own and a short `x1.5 (...)` line
  under the verdict. It never changes the verdict at the load seen.
  `--load-seen-x 1.5` is an alias that adds the same column.
- **Concurrency as a load.** `--load 64c` asks for the rate at which the
  fleet holds 64 requests in the system on average, as a load generator
  that keeps 64 requests open drives it. Each shape finds its own rate: the
  row is `64c concurrent` at that rate. If the fleet holds fewer than 64
  below its knee, the row is the knee's rate, labelled `saturates`, and a
  line under the table says so: more open requests add queueing there, not
  throughput. Where the mean TTFT or TPOT is refused, there is no time in
  the system to translate with: the row is left out, and a line under the
  table says why.
- **Summary.** One row per shape (`--shape`, repeatable: `measured`,
  `load`, or `IL/OL[/hit]` in tokens, `k` for 1024), one column per load (`--load`,
  repeatable: `80%` of that shape's fleet knee, `5r/s`, `64c`, `seen`, or
  `seen x1.5`). A cell
  such as `617/3.2m @18.49 ok` is the TTFT and TPOT at the statistics the
  SLO judges (here TTFT p90 617 ms and, its p90 refused as host-bound,
  TPOT mean 3.2 ms, marked `m`; with no target, TTFT p90 and the
  `--tpot-percentile`) at 18.49 r/s, and both targets are met there. The mark is `ok`
  met, `MISS` missed, `MISS*` missed because the rate is past the prefill
  flow-control onset (the router queues) though the figure is within, `SAT`
  saturated, `?` not judged. A TPOT ending in `m` is the mean, where the
  percentile is refused. A shape outside the lengths or prefix-hit rate the
  recording saw is marked EXTRAPOLATED.
- **Verdict per shape**, in one sentence:
  - UNDER-PROVISIONED: the load is past headroom times the fleet knee, or a
    target is missed there.
  - OVER-PROVISIONED: a role has a replica to spare. One fewer of it must
    hold every target at the load judged, and the role's own knee and
    target at the window's peak too, since removing capacity is the costly
    mistake. A decode replica must also be spare at the decode knee's error
    bound: one fewer still meets the count the load needs if the knee reads
    as high as its label allows (`no spare: 3 decode if the decode knee
    reads 99% high` otherwise).
  - RIGHT-SIZED: the fleet holds, and no role has a replica to spare.
  - NOT JUDGED: where it would be UNDER-PROVISIONED on a load of other
    traffic than the shape priced, or on a knee the window's own fleet
    served past for 3 minutes; or a figure the verdict needs
    is refused (the sentence says
    which), there is no load, or the prefill side is not priced and decode
    alone holds. With no prefill speed learned, decode alone is judged
    ("(decode only)"): the report never calls such a fleet right-sized or
    over-provisioned, and gives no fleet rate at the SLO.

  UNDER and OVER name the fleet needed: the cheapest that holds every
  target at the load with headroom (the fewest GPUs, as
  [`POST /v1/size`](api.md#the-size) finds them), for example "needs 3
  prefill + 3 decode (has 2 + 2)". A role it cuts must hold at the window's peak
  too, as a spare must, so it never cuts a role the sentence says has no
  spare unless it also adds replicas of the other role, and then it says
  so ("trading prefill for decode"). Other mixes of as many replicas that
  hold are listed too. With the prefill side not priced it says "needs at
  least 3 decode".
  When the roles take different GPUs a replica, a `needed` line under the
  verdict gives the GPUs of the fleet and of the other mixes. A right-sized
  fleet gets that line too when another mix holds on fewer GPUs.
  `--spare N` adds N replicas of each role to the fleet needed, on the same
  line: the fleet that still holds with N replicas of each role lost (see
  [Size for failures, minReplicaCount and quota](#size-for-failures-minreplicacount-and-quota)).
  On a right-sized fleet the spare is added to the fleet judged, or to the
  mix of fewer GPUs when one holds. When no
  fleet within the bound holds, there is no `needed` line.
  Each role's standing is in the sentence ("decode: one fewer holds even
  at the peak 43.2 r/s; prefill: no spare (2.0% under its operating
  point)"), with any prefill imbalance f that no run learned at that pod
  count. That f is read off the pod counts that were learned; with none
  learned, or with `--imbalance-across-counts=false`, it is assumed at
  least 1.5.
- **A load-aware router:** if your router scores each pod's queue (llm-d's
  EPP queue scorer), pass `--load-aware-router` to any TTFT question. f is
  then held up to the load it was measured at and falls to 1 as the pods
  fill, so the prefill knee is the pooled one less the margin; the inputs
  say so ("f 1.5 held to 0.35 utilization, then falling to 1 ..."). The
  fleets at f 1 and f 1.5 are priced the same way. A pod much busier than the
  rest still saturates first. With no load measured for f, f is held. The
  flag is off by default, and it still sets the price where the router's
  series say what it scores ([Router policy](#router-policy)); an answer
  that disagrees with them says so in its assumed line. The API takes it as `assume.loadAwareRouter: true`, `backtest`
  and `predict` as `--load-aware-router`, and the page as **My router
  balances prefill by load** under **Assume a term**.
  Since no run measured it, the sentence also gives the fleet needed at f 1
  and at f 1.5 on every count above one prefill pod ("at f 1 needs 3 + 3,
  at f 1.5 needs 4 + 3", or "the same at f 1 to 1.5"), and `-o json` gives
  them as `needed.fSensitivity`. The `checks` lines give each fleet tried
  and its numbers.
- **Per shape:** utilization at the load (decode KV fill, batch slots, wait
  probability, and on a P/D fleet with a KV transfer, TTFT priced as
  processor sharing (the default), a `KV pull` row: the pull's load on each
  decode pod, which the KV fill leaves out, and whether it is the larger mean
  wait in TTFT, which then sits on the decode pods, not the prefill pods; at
  1 or more the pull saturates the decode pods and TTFT is refused; its wait
  is unpriced where TTFT is refused, or priced first come because the prompts
  are too uneven for processor sharing;
  prefill utilization pooled and on the busiest pod), the
  prefill, decode and fleet knees in req/s and output tok/s with what binds
  each, the operating point, the highest rate that holds the targets, and
  the SLO against each load. `e2e mean (sum)` is TTFT mean + (OL - 1) x TPOT
  mean, not a percentile. `in system` is the rate times it: the requests in
  the system on average (Little's law), `unbounded` at or past a knee (or
  where a queue saturates short of it), `not known` at or past a knee that
  is only a lower bound, `-` where the TTFT or TPOT it needs is refused.

The targets are the policy's unless `--ttft` and `--tpot` set them. Exit 0
means the report answered, whatever the verdict, saturated fleets included;
exit 3 means a shape's verdict is NOT JUDGED; exit 4 means the report
answered and the self-check found the figures a verdict rests on
inconsistent with the recording (the verdict says `self-check: not
trusted`; [the self-check](check.md#the-self-check-beside-every-answer)).
Scripts read the verdict from
`-o json`
(`shapes[].verdict.verdict`), which prints the same report as
[`POST /v1/report`](api.md#the-report).

With v0.12.3, the sample above prints (the start of it):

```text
ns-01/model-02  P/D 2 prefill (variant-02) + 2 decode (variant-01)  window 2026-11-06 02:18-03:30Z, cycle 289 of 289
decode engine   vLLM release not recorded
prefill engine  vLLM release not recorded
load judged     41.70 r/s, the p95 of the 282 of 289 cycles with traffic
load seen       per cycle (1-minute rates): mean 31.59, p95 41.70, peak 43.24 r/s; p95 less counting noise 41.66; busiest 2m0s (8 cycles): 42.13 r/s
SLO             TTFT p90 2000.0 ms (asked), TPOT mean 30.0 ms (asked)
cell = TTFT p90 ms / TPOT mean ms @ the rate priced, r/s; - refused (the shape's table says why)
mark = the SLO (TTFT p90 2000.0 ms (asked), TPOT mean 30.0 ms (asked)) at that rate: ok met, MISS missed, MISS* missed past the prefill flow-control onset (the router queues) though the figure is within, SAT saturated, ? not judged
summary                     50%                80%                 5r/s              seen            seen x1.2
  measured                  617/3.2 @18.49 ok  1944/6.8 @29.59 ok  388/3.1 @5.00 ok  -/- @41.70 SAT  -/- @50.04 SAT
  1k/1k EXTRAPOLATED        133/3.1 @17.78 ok  152/5.6 @28.45 ok   125/3.1 @5.00 ok  -/- @41.70 SAT  -/- @50.04 SAT
  16k/200/0.3 EXTRAPOLATED  667/3.2 @9.08 ok   1015/6.8 @14.53 ok  567/3.1 @5.00 ok  -/- @41.70 SAT  -/- @50.04 SAT
verdicts
  measured                  UNDER-PROVISIONED: at the p95 load of 41.7 r/s, 1.1x the fleet knee of 37.0 r/s, the fleet saturates; needs 3 prefill + 3 decode (has 2 + 2), 6 decode if the decode knee reads 99% high; the same at f 1 to 1.5 (prefill f 1 assumed at 3 pods).
                            x1.2 (50.0 r/s): UNDER-PROVISIONED: 1.4x the fleet knee (37.0 r/s); needs 3 prefill + 4 decode (has 2 + 2), 7 decode if the decode knee reads 99% high (prefill f 1 assumed at 3 pods).
  1k/1k EXTRAPOLATED        UNDER-PROVISIONED: at the p95 load of 41.7 r/s, 1.2x the fleet knee of 35.6 r/s, the fleet saturates; needs 1 prefill + 3 decode (has 2 + 2), 6 decode if the decode knee reads 99% high.
                            x1.2 (50.0 r/s): UNDER-PROVISIONED: 1.4x the fleet knee (35.6 r/s); needs 1 prefill + 4 decode (has 2 + 2), 7 decode if the decode knee reads 99% high.
  16k/200/0.3 EXTRAPOLATED  UNDER-PROVISIONED: at the p95 load of 41.7 r/s, 2.3x the fleet knee of 18.2 r/s, the fleet saturates; needs 4 prefill + 6 decode (has 2 + 2), 11 decode if the decode knee reads 99% high; at f 1 needs 4 + 6, at f 1.5 needs 5 + 6 (prefill f 1 assumed at 4 pods).
                            x1.2 (50.0 r/s): UNDER-PROVISIONED: 2.8x the fleet knee (18.2 r/s); needs 4 prefill + 7 decode (has 2 + 2), 13 decode if the decode knee reads 99% high (prefill f 1 assumed at 4 pods).
```

Read the label under the decode knee in each shape's capacity table: inside
the region a run measured it reads `validated in-sample: one-sided, error at
most +3%`; outside it, the `not validated` lines at the end of the report add
that the knee read 43% to 99% high on the 0.6B goldens. The needed fleet gives
the decode count at the label's upper error beside the point answer: +3%
inside the region, 99% outside it
([accuracy](accuracy.md#what-was-measured)). TTFT is not validated.

A verdict that would be UNDER-PROVISIONED is NOT JUDGED instead in two
cases, and no fleet is sized:

- The load and the shape priced are of different traffic: the measured
  shape priced is more than 20% longer, in prompt or output, than the
  cycles the load was read from (the `SHAPE/LOAD MISMATCH` line), so its
  knee reads low for that load. The summary gives both shapes; `--shape
  load` prices the load at its own. A shape priced lighter than the load
  keeps its UNDER: its knee only reads high.
- The window contradicts the model, where the load is past the knee's
  operating point: at the shape priced, the fleet priced,
  or a smaller one, served more than 1.25x the fleet knee for 3 minutes
  without a break (over the band's top where that sets the decode knee;
  never on a prefill knee read off a lower bound of V_P). A gap in the
  recording, two cycles more than two cycle times apart, is a break, and
  the 3 minutes are clock time. A recording with no per-cycle clock is
  counted at its mean cycle time, and the summary says so. The summary
  says how far and for how long; the knee reads low for that window.
  An aggregated fleet, with no prefill pods, is one pool judged on its
  own knee: the decode knee, whose line was fitted on pods that prefill
  too. A cycle up to 20% lighter than the shape priced has a higher knee,
  and its rate counts scaled down to the shape priced. Where decode sets
  the knee, the scale is the cycle's output times the KV one request
  holds, over the shape priced's: the prompt's KV sits on the decode pods
  too, on a P/D fleet as on one pool. A longer prompt never scales a rate
  up. One pool takes the prompt ratio
  where that is lower, as it also prefills on the same pods.

Only an UNDER is withheld: neither case shows the fleet holding.

The reverse case withholds the other verdicts. Where the measured shape asked
(`--shape measured`) is more than 20% shorter, in prompt or output, than the cycles the load was read
from, its knee reads high for that load. A verdict that the fleet holds,
RIGHT-SIZED or OVER-PROVISIONED, could then cut replicas the load needs. Such a
verdict is NOT JUDGED instead, no fleet is sized, and the summary gives both
shapes; `--shape load` prices the load at its own. An UNDER-PROVISIONED verdict
on that knee stands: a heavier load only makes it more true.

A prefill knee priced from a V_P that is only a lower bound (flag
`VPLowerBound`: the prefill Pods never queued for compute under load) is
at least its rate, and the report says so: `>=` before it in the
capacity table, `lowerBound` in the JSON, "at least" on the page. A load
past its operating point, short of decode's, is NOT JUDGED, never
saturated: the summary reads "3.1x a prefill knee of at least 4.0 r/s:
the prefill knee rests on a lower bound of V_P, and the true knee may be
higher, so whether the fleet saturates is not priced", and the summary
cells at and past that knee are marked `LB`, not `SAT`. Past decode's own
knee the fleet saturates whatever V_P is, and the verdict says so on
decode's knee. `--assume-vp` states a V_P to price the prefill side.

Below that knee the TTFT is still priced from the same lower bound, and a
faster prefill only lowers it, so every such TTFT is an upper bound: the
true TTFT is at most the figure. The report says so: `<=` before it in
the summary and the SLO table (and before the e2e sum built on it),
`upperBound` in the JSON, "≤" on the page. A target it meets is met. A
target it misses is not a known miss: the cell is marked `UB`, not
`MISS`, and the verdict is NOT JUDGED, never UNDER-PROVISIONED on it:
"TTFT p95 is at most 14.5 s against a 200 ms target: it is priced from a
lower bound of V_P, and the true TTFT may be lower, so the miss is not
known; --assume-vp states a V_P to price it." For the same reason `max
at SLO` is at least its rate, and a TTFT floor over the target is at most
that floor. A TPOT miss is known whatever V_P is, and still gives
UNDER-PROVISIONED.

The planner reads V_P from the last 6 hours of the recording, as the
controller does. Only with `--experimental-ttft` (see below) does it read
V_P over the whole recording: a fleet that queued for prefill only at its
busiest hours then keeps the V_P read then (flag `VPFromEarlierInWindow`,
and the assumed line says when: "V_P learned at +6.2 h of the window, 17.8 h
before the cycle priced"). It is a lower bound, raised to what a Pod was
seen to compute where that is higher. It is off by default because, on a
production P/D fleet, the TTFT priced from that lower bound for the rest of
the day read many times what was measured.

A fleet whose prefill Pods never queued for compute at all has no V_P from a
queue. The planner then takes the highest rate a prefill Pod computed over 3
minutes as V_P, as a lower bound, and nothing more (the assumed line says
"is only the rate a prefill pod was seen to compute"):

- The prefill knee is at least the rate that V_P gives. It is also at least
  the rate the fleet's prefill Pods kept up: the computed-token work they
  sustained over 5 minutes with their prefill backlog not growing, at the Pod
  count priced, in the last 6 hours, over the prompt they computed (flag
  `KneeAtServedWork`). A Pod computes no faster than it can, so work it kept
  up is work it can do. Both figures come from the engines' computed-token
  counter. The figure is the mean over those 5 minutes, not the peak minute.
  A burst carried with a queue building is not a rate the Pods hold: where
  the Pods served at the count priced only with their backlog growing, the
  knee is not raised and carries `ServedWorkUnsteady`. A recording that does
  not read every prefill Pod's waiting queue cannot show the backlog steady,
  so its knee is not raised either. It carries no flag, as nothing says the
  backlog grew.
- No TTFT is priced from such a V_P: every TTFT, and every rate asked at a
  TTFT target, reads "no prefill speed learned" (`NoPrefillCapacity`), as
  before. The true V_P may be far higher, so a TTFT priced from it would be
  an upper bound at best.

A window whose cycles do not cover its span (a capture or recording with
holes) gets a `window gaps` line under the header line: the cycles the span
should hold, those present, and the largest gap between the cycles either
side of it, ending `WindowGaps` above 5% missing (JSON `source.gaps`). The
load and the figures are read from the cycles present, so a gap over the
busy spell reads the load low: capture the window again. When any gap of
2 minutes or more borders the busiest stretch (a cycle either side of it
lies in the busiest 2 minutes, or is the busiest cycle), the observed load
is refused, `WindowGapAtPeak`: the verdict is `NOT JUDGED` and the command
exits 3. State the load with `--load-seen` to judge anyway. A gap is any
stretch with no cycle for the model, so a model parked at zero, or a cycle
time changed mid-window, reads as one too. A scrape that misses only the
engine's series still leaves a cycle, and is not counted.

### Router policy

Where the recording holds the router's prefix series (`llm_d_epp_prefix_*`)
or its per-endpoint series, the planner says what the router scores. A TTFT
answer adds it to its `ttft inputs` line ("router cache-aware and load-aware
(from its series)"), `-o json` gives it as `routerPolicy`, and the inputs
document as `policy.router`. A recording without these series answers as
before, with none of the three.

- **Cache-aware:** at the window's lightest load, the router sends a request
  where nearly all its cached prefix is (at least 90% of the cached tokens
  the best pod held). At 60% or less it is not cache-aware. The share is read
  from one prefix producer; if the producer the router reads changes within
  the window, the cache side is not detected.
- **Load-aware:** either of two readings.
  - The kept share falls as the load rises, over the whole window and in
    each half of it. This is a correlation, not a test of the scorer: a
    traffic mix that changes with the time of day, a cold cache, or eviction
    under load can do the same.
  - The router places requests unevenly so the pods' in-flight requests come
    out even. The placements must be uneven by more than twice what random
    placement would give (about 1/sqrt(n) for n requests per pod per window).
    With few requests per pod, the spread is not read.
- **Not detected:** the series are missing, there is one prefill pod (every
  placement keeps the whole hit), the window is under 20 minutes, the load
  did not vary enough, the halves disagree, or the readings sit between the
  bounds.

A cache-blind router also keeps the whole hit when every pod holds the same
prefix, so "cache-aware" means only that the router lost no hit it could
have kept. In P/D the spread reads the decode pods, where the router counts
placements, and the hit reads the prefill pods.

The window holds up to 12 hours. After you change the router's scorers, the
policy reads the old and new configuration together until the old cycles
age out, up to 12 hours. If the series stop for an hour, the window is
cleared and the policy reads not detected.

It sets no price yet: `--load-aware-router` still does. When the two
disagree, the answer's assumed line says so. The controller reads the same
series the same way. While a learner recording is on, it logs `router
policy read from its series` when the policy changes and the new one has
held for 10 minutes.

## Plan replicas over a day

How many prefill and decode replicas did your traffic need, hour by hour,
and how many did the fleet have? The planner cuts the window into buckets (5
minutes by default), reads each bucket's request rate, sizes it as **What
fleet does this traffic need?** sizes one rate, and sets the replicas the fleet
had beside it. Use a window of a day: a recording of a day, or 24 hours of
Prometheus.

On the page, load the source, set the SLO and headroom as usual, and press
**Plan the day** under the graphs. Three charts share one time axis: the
request rate (the mean of the bucket's cycles, and a wash up to their p95),
then prefill replicas and decode replicas, each as needed against had. A
bucket the fleet had fewer replicas than it needed is washed red; a bucket no
fleet within the bound serves is hatched, in the panel of the role short.
Hover a bucket for every value at that time; the table views list every hour
and every bucket, with why a bucket was not sized.
The plan says the SLO and headroom it was sized for. Change a target, the
headroom, a length or an assumed term above the tabs and a plan on screen
plans again; on another tab it dims, with a note naming the targets it used,
and plans again when you open it.

**With the experimental models.** On inputs loaded with the experimental
TTFT or TPOT model (a server started with `--experimental-ttft=v2` or
`--experimental-tpot=v2`), the page plans the day with them, as the Answer
view prices with them; from the command line, `day-plan
--experimental-ttft=v2 --experimental-tpot=v2` fits them on the
`--recording`. Where the TTFT model prices a TTFT the planner refuses (a
prefill that never queued), prefill is sized on it. A bucket a model sized
carries its flag (`RankMixtureTTFT`, `TPOTDistribution`) on its limit, its
needed replicas are marked † on the page and in the command's hours table,
and the plan names each model, with how many buckets it priced. Where one
prices a target the plan asks, each bucket is priced at its own rate and its
own request shape, read over at least an hour and 16000 requests around it
(at most 2 hours): the TTFT model draws from that window's prompt and
computed-token histograms, so an hour whose prefix hit is lower than the
day's is priced with the longer prefills it served; the TPOT model keeps its
ratio at the window's output lengths. A bucket whose shape lies outside what
the TTFT model's fit served is priced at the window's shape instead, flagged `WindowShape` and
`ShapeOutsideFit`, as is one whose window holds too few requests
(`ShapeFewRequests`); a note counts each. A stated
shape or term stays the planner's own. The TPOT model prices a p90 or p95
TPOT target, not the mean. The plan's notes
say, for each model, why each bucket it did not price is the planner's. On
a window under 12 hours a model abstains, the plan says so, and every figure
is the planner's own. Not validated: every figure a model sets reads
OUTSIDE VALIDATED REGION.

Each bucket is sized on its own, so a day of 5-minute buckets (289) takes
minutes. While a plan runs, the Day plan tab says so: how many buckets, when
it started and the seconds since, with **Cancel**, which stops the request
and frees **Plan the day**. The server reports no share done, so the time is
all it shows. One plan runs at a time: opening the tab again shows the one
running, and a plan you cancelled is not asked again until a knob changes.
Loading new inputs cancels a plan still running.
The **Bucket** starts at 5 minutes, the server's default, or at 15 minutes or
an hour where the window would otherwise cut into more than 300 buckets; pick
another to plan finer. A bucket you pick stays when you load again, unless
the new window would cut it into more than the 3000 buckets the server
sizes.

From the command line, on the sample recording (about an hour, so two hours
in the table):

```bash
# --prometheus URL --namespace NS --kube-state-metrics in place of --recording reads the last 24 hours.
bin/capacity-planner day-plan --recording recordings/ols-sweep.jsonl.gz --model model-02 --assume-vp 30000 --ttft 500ms@p95 --tpot 4ms
```

```text
Day plan for ns-01/model-02, 2026-11-01 21:50 to 2026-11-01 22:55 UTC, buckets of 5m0s, each sized for the p95 of its cycle rates (default; the mean where a bucket holds fewer than 20 cycles)
SLO: TTFT p95 500ms, TPOT mean 4ms; headroom 0.85. TTFT model UNVALIDATED. ...

REFUSED prefill's TTFT (TargetBelowFloor), in 4 buckets: target 0.5 s at or below the TTFT floor 0.6788 s
  what to do: The TTFT target is under the TTFT at zero load: no replica count meets it. Raise the TTFT target above the floor in the detail.
  what the plan did: Prefill is not sized: decode is sized alone, on its TPOT target and its knee times headroom.

each hour: a fleet that holds every bucket in it (priced again at each), the busiest bucket's own fleet, the fewest pods it had, and the fleet it had judged whole at each bucket's rate
hour (UTC)        rate mean  rate p95 need P need D busiest  had P  had D
2026-11-01 21:00       0.73      1.00      1      1     1+1      1      1  ok
2026-11-01 22:00       6.88     29.68    >=5    >=4     4+2      1      1  UNDER (SLO risk)

buckets: 11 sized (9 under, 0 over, 2 match; 11 for the p95, 0 for the mean, 0 for the max), 0 beyond the bound, 0 refused, 2 too little data, 0 no data
needed: to hold every bucket at least 5 prefill + 4 decode (prefill not priced in some buckets); the busiest bucket alone 4 prefill + 2 decode, the quietest 1 prefill + 1 decode (each the fewest replicas in all; another mix may serve too)
replica-hours over the 11 sized buckets: needed 1.75 decode (prefill not priced in 4 of the 11), had 0.92 decode (prefill not priced in 4 of the 11)
had over every bucket with data: 1.08 prefill + 1.08 decode replica-hours, 2.17 GPU-hours (GPUs per replica: 1 prefill, 1 decode)
hours under (SLO risk): 22:00
hours over (waste): none
flags: RateFromDecodeRows, WindowShape, PrefillNotPriced, UnvalidatedTTFT, AssumedVP, ... (every flag the buckets carry)
```

It exits 3: four buckets of 4k-token prompts cannot meet a 500 ms TTFT at
any replica count, so their prefill is not sized and their decode is sized
alone. A higher `--ttft` sizes them.

- **Needed.** The fleet of the fewest GPUs that serves the bucket's rate under
  the SLO and headroom, ties going to fewer replicas, then fewer decode
  replicas, as **What fleet does this traffic need?** finds it. With both
  roles at the same GPUs a replica, that is the fewest replicas. Another mix
  may serve the rate too.
- **An hour's need.** A fleet that holds every bucket of the hour, priced
  again at each bucket's rate and shape: the cheapest of the buckets' own
  fleets that holds them all, in the order a bucket's need is chosen, else
  each role's most over them (`NOT CONFIRMED` where it falls short at one).
  It is not the busiest
  bucket's: above, 22:15 runs 29.8 r/s of 500-token outputs and needs 4
  prefill and 2 decode, but 22:05 runs 9.5 r/s of 2500-token outputs and
  needs 4 decode, and 22:40 runs 15.9 r/s of 1400-token prompts and needs 5
  prefill. Four buckets sized decode alone, so the need is a lower bound
  (`>=`). The `busiest` column is the busiest bucket's own fleet. The
  summary's need holds every bucket of the window the same way.
- **Sized for.** Each bucket is sized for the p95 of its cycles' rates, less
  its counting noise, when it holds 20 cycles or more (5 minutes at a 15 s cycle), else for their mean,
  which is less conservative, flagged `FewCyclesForP95`. `--size-for
  mean|p95|p95-modulation|max` sets it; the table and each bucket say which
  was used. `p95-modulation` is the p95 less the counting noise of a
  1-minute rate, as in `at-replicas`: a bucket of a steady stream is sized
  near its mean. It is the default; see "The p95 less counting noise as the default" under
  `at-replicas` above.
  `--bucket 15m` sets the bucket.
- **Scale-up lead.** A replica must be Ready when its bucket starts, so it
  starts one start time before. The plan takes each role's start time from
  the recording (the start learner's slowest measured variant of the role;
  `fleet.<role>.startSeconds` in the inputs), or `--lead 90s` (both roles),
  `--lead prefill=3m,decode=90s`. With a lead, the `held` column is the
  fleet held in the hour: each role's most need over a bucket and every
  bucket that starts within the role's lead after it ends. The summary
  adds the held replica-hours: each scale-up's replicas counted from one
  lead before their bucket. On the sample (start times 67 and 68 s) the
  21:00 hour needs 1 + 1 but holds 1 + 2, for the 22:00 rise; the held
  decode replica-hours are 1.86 against 1.75 needed. The verdicts still
  judge the need. With no start time learned and none asked, nothing is
  held ahead, and a note says so.
- **Shape.** Each bucket is priced at its own observed prompt and output
  length. A bucket whose cycles read none is priced at the source's shape and
  flagged `WindowShape`. With an experimental model that prices a target
  the plan asks, a bucket is priced at its own shape read over a window of
  at least an hour and 16000 requests around it, or at the window's shape,
  flagged `WindowShape` with `ShapeOutsideFit` or `ShapeFewRequests`. `--assume-input-tokens` and `--assume-output-tokens`
  price every bucket at one stated length, flagged `EXTRAPOLATED` when it lies
  outside what the buckets observed.
- **No TTFT target.** With no `--ttft` and none in the policy, prefill is
  not sized, and decode is sized alone on its TPOT target and its knee times
  headroom, as `at-replicas` judges decode alone; a note says so. The flags
  are checked before the recording is replayed.
- **What the source cannot price.** A prefill rate V_P or a fixed latency a
  it did not learn, a TTFT target under the TTFT at zero load, or a decode line
  it did not fit refuses every rate alike. The plan says so at the top, with
  what to do, and sizes what does price: decode alone when prefill is refused,
  decode without its TPOT when the TPOT is refused. When the decode knee is
  refused (a decode line it did not fit, or no KV capacity or batch cap),
  nothing bounds decode, so, as when nothing prices, no bucket is sized and
  no figure of a need is printed, as `at-replicas` refuses the same fleet. It
  then exits 3.
- **Beyond the bound.** A bucket whose rate no fleet within `--max-prefill`
  and `--max-decode` (32 each) serves is not sized; its hour is UNDER. It
  also exits 3.
- **Too little data.** A bucket with fewer than 30 requests (`--min-requests`)
  is not sized. A bucket no cycle fell in has no data. Neither counts as under
  or over. A plan that sizes no bucket at all exits 3.
- **Gaps in the source.** When the source's cycles do not cover its span (a
  capture or a recording with holes), a note says how many cycles the span
  should hold, how many it has, and the largest gap; above 5% missing it
  ends `WindowGaps`. JSON: `gaps`. A bucket inside a gap has no data, and
  one a gap cuts into is sized from the cycles present: a gap over the
  busiest hours sizes them low. Capture again over the busy hours. When any
  gap of 2 minutes or more borders the busiest sized bucket (it cuts into
  it, or covers a bucket beside it), that bucket is refused,
  `WindowGapAtPeak`, the busiest need is read from the buckets left, and
  the plan exits 3.
- **Self-check.** As with `at-replicas`, a bucket's verdict that rests on
  figures the self-check found inconsistent says `not trusted` beside its
  hour, with a line under the buckets saying how many, and the plan exits 4
  (3 takes precedence). A bucket that priced prefill rests on the TTFT and
  TPOT figures; decode sized alone on the TPOT figures. A bucket priced at a
  stated shape (`--assume-input-tokens`, `--assume-output-tokens`), or whose
  sized rate on the fleet it had the self-check did not test, says `not
  covered by the self-check`, and changes no exit code. A refused or unsized
  bucket says neither, nor does a verdict on a fleet with no replica of a
  role it prices (it rests on no figure); a bucket beyond the bound can be
  not trusted but is not tested for coverage.
- **Had.** The median serving pods of the priced prefill and decode variants
  over the bucket; the rate is the model's (`OtherVariantsServed` where
  another variant also served). The fleet it had is judged whole, at the
  bucket's rate: UNDER (SLO risk) when its limit falls short, over (waste)
  when one replica fewer of either role would still serve it. An hour is
  UNDER when any of its buckets is, and over when every sized bucket is.
  Where prefill was not sized, decode is judged alone: the verdict reads
  "decode only", a spare decode replica is "decode only: a decode replica to
  spare" (listed under "hours decode only had a replica to spare", never
  under waste), and the summary says it is not a fleet recommendation. A
  bucket the model calls over whose TTFT the router measured over its target
  gets no verdict and the flag `MeasuredTTFTOverTarget`: a fleet that missed
  its target has nothing to spare.
  Had counts what served. Replicas asked for and not serving, those that
  exist and are not ready (starting, unschedulable, or held by an admission
  gate such as Kueue's; a rolling update's surge pods count too), show
  beside it as `2(+1)`, each role read off the hour's bucket of the fewest
  of that role serving, with a note. A replica whose pod was never
  created (a ResourceQuota refusing it) is not in the recording and is not
  shown. For a LeaderWorkerSet a replica is a group.
  Replica-hours and GPU-hours needed and had are over the sized buckets, and
  left out for prefill when any of them did not price it; the last line is
  what the fleet ran over every bucket with data.
- **Prefill not priced.** Decode is then sized on its own limit alone, a
  lower bound: `at least`, and `>=` in the table.
- **Steady state.** Each bucket is sized as if its rate held. It ignores the
  time a replica takes to start: a fleet that follows the plan must start
  replicas that long before the rate arrives.
- **JSON.** `-o json` prints every bucket, as `POST /v1/dayplan` returns it
  ([the day plan](api.md#the-day-plan)).

## Size for failures, minReplicaCount and quota

Every fleet the planner names, "needed" included, is the smallest that holds
your targets with **no replica lost**. It does not plan for a node, a zone or a
Pod going away. A fleet of 2 prefill + 2 decode loses half of a role to one
lost replica, and with it most of the rate that role carried, until a
replacement starts (on a GPU fleet, often minutes).

Add the spare yourself, and decide how many from your failure domains:

- **N+1.** One spare replica of each role: `--spare 1` on `at-replicas`, or
  `"spare": 1` on [`POST /v1/size`](api.md#the-size) and
  [`POST /v1/report`](api.md#the-report). The answer adds the spare to each
  role, with its GPUs, and that fleet still holds with one replica of each
  role lost at once.
- **Larger failure domains.** When one node or one zone holds several
  replicas of a role, the spare is the most replicas of that role one domain
  holds: spread the role with a topology spread constraint, then set
  `--spare` to the most replicas of the role that any one domain holds. A replica that spans
  nodes (a LeaderWorkerSet group) is lost whole when one of its nodes is.

Then map the answer onto the ScaledObject and the quota:

- **`minReplicaCount`** per role: the quietest need plus the spare. Take the
  quietest need from [the day plan](#plan-replicas-over-a-day) (`needed: at
  the quietest ...`), or from `at-replicas --load-statistic mean` over a quiet
  window, and add the spare. Below it, a failure at the trough leaves the
  role under its need for a whole replica start.
- **`maxReplicaCount`** per role: that role's largest need over the day (the
  largest `needed.prefill` and `needed.decode` over the day plan's
  `buckets[]` in `-o json`) plus the spare, with room for the growth you plan for (`--load-seen-x`). Take
  each role's own largest need over the buckets, not the busiest bucket's
  mix: each role scales on its own signal, and the prefill need need not peak
  where the rate does. Between the two bounds the autoscaler sets the count.
- **Quota.** Per accelerator type, the GPUs of those largest needs plus the
  spare, summed over every model the quota covers, plus room for a rollout's
  surge Pods: the `withSpare.gpus` of a size gives one model's at one rate.
  Below that, the quota refuses the spare replicas at the peak
  ([the quota limiter](../../reference/quota-limiter.md)).

The planner sizes on each role's GPUs per replica as the source states them:
a recording made by the scaling manager, or a capture with
`--kube-state-metrics`, reads them from the Pods. A capture without it takes
1 for every role (`--gpus-per-replica` sets one count for every role), and
the answer is then the fewest replicas. A source that states no GPUs for a
role is sized on replicas too, and the size says `minimizes: "replicas"`.

## Set your own prompt buckets

The "if all traffic were bucket b" view draws five buckets by default: 1k,
4k, 8k, 32k and 128k tokens. To draw others, tick the view, then type your
edges in **Bucket edges**, for example `2k, 16k, 64k`:

- Each edge is a bucket's largest prompt, in tokens. `16k` is 16384 tokens.
- Give 1 to 8 edges, smallest first, each at most 1024k.
- Leave the box blank for the default five.

Each line is then priced with every prompt at its edge, and named by it. A
line named `2000` is an edge of 2000 tokens; `2k` is 2048. The box shows
your edges by these names, so `2048` becomes `2k`.

There are five line colours. With more than five edges the colours repeat,
and the sixth line on is dashed; the table under the graph names every line.

Each new edge prices a fleet of its own, so a plan with many new edges takes
longer. Edges you already used for this fleet are not priced again.

The edges change only this view: the limits and the +1 steps stay the same.
The flag count can change, since each line brings its own flags.

**Edges past the context window.** A request needs room for its prompt and
its output, so a line whose edge plus the output length passes the engines'
`--max-model-len` is flagged `EdgeBeyondContext`, dimmed and dotted on the
graph, and marked in the table. This needs the source to know the window
(shown as "context window" under **Inputs**); when it does not, nothing is
flagged, so check your edges against it yourself.

The address keeps your edges, so a shared link draws the same lines. If an
edge in the box is wrong, the box is marked and says why under it until you
fix it, and the graphs keep the edges in use. If a link's edges are wrong, the
status line says so, and the page draws the default five.

## Share a scenario

The page keeps the source and every knob in its address, after the `#`. Copy
the address to reopen the same scenario later, against a server that reads the
same source. In the second mode the address keeps the question (the rate and
the targets), not the answer, so it is found again when the page opens.

## Save and compare scenarios

To compare two scenarios:

1. Price the first one: set the knobs and wait for the figures.
2. Press **Pin as A**. The page keeps that plan as A.
3. Change the knobs, or open a saved scenario with **Open**.
4. Read the result. The TTFT and TPOT graphs draw A as a grey dashed line
   beside the current curve, with A's limit point labelled "A: ... r/s". The
   table under the tabs sets A's fleet, limits, binding role and targets
   beside the current ones, with the change: "now refused" or "now priced"
   where only one side has a figure.

A is not priced again as you move the knobs. **Unpin A** clears it. The pin
is not saved or exported: it is gone when the page reloads. Both questions
work: in the second mode, pin the fleet the page found, then switch back and
step the replicas, or change the rate and size again. A size that no fleet
within the bound serves has nothing to pin, and while the current scenario
is being priced, or failed to price, the table says so instead of comparing.

To keep a scenario for later:

- **Save** keeps the scenario on screen in this browser, under the name you
  type. With no name typed, the page makes one from the model, the fleet, the
  rate, the targets and the headroom, dropping the last of these to fit 80
  characters. Saving under a name already used replaces it, and the status
  line says so. **Open** loads a saved one, **Delete** removes it. The page
  keeps at most 50.
- **Export JSON** downloads the scenario as a small file; **Import JSON**
  reads one, saves it and opens it. If another saved scenario already has
  its name, the import is saved as "name (2)", "name (3)" and so on. A file
  that fails the page's checks is refused, with the reason.
- A scenario holds the same text as the address after the `#`, so it opens
  only against a server that reads the same recording or namespace. One this
  server does not read is refused (an import is saved, and not opened), and
  a scenario that fails to load leaves the page as it was.

Saved scenarios live in this browser only, not on the server. A Prometheus
scenario reopens on the last hour ending now, as a shared address does.
