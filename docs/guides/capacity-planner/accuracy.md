# Capacity planner: how accurate each figure is

Every figure the [capacity planner](README.md) prints says whether a real run
checked it, and how far off it read there. **Validated means measured, not
accurate: read the range.** A validated figure can be twice the real value.
And every region so far is **in-sample**: the one run that measured it also
tuned the planner (see [What was measured](#what-was-measured)).

## Read the label

Each figure carries one of two labels.

- **`validated in-sample: error +3..+152% (min-max, n=6 load levels, 12 readings; q32 staircase)`.**
  Your fleet and traffic sit inside what a measured run covered. On that
  run the planner's figure read 3% to 152% over what the client measured,
  lowest to highest, over 6 independent load levels (12 readings: each level
  read twice). `in-sample` says the run also tuned the planner; a run kept
  out of tuning would print `validated on a holdout`.
- **`OUTSIDE VALIDATED REGION (prompt spread not measured (cv 2 assumed), ...)`.**
  No run checked a figure like this one. The bracket names the first thing
  that differs. The figure may still be right; nobody has measured it.

The label always gives the count (`n=`, the independent load levels first)
and what kind of range it is:

| Range | Means |
| --- | --- |
| `p10-p90` with `min-max` | 10 or more independent load levels: the middle 8 in 10, then the extremes |
| `min-max` | fewer than 10 independent load levels: the lowest and the highest |
| `single reading, between two ladder steps` | one measurement, known only to lie between two steps of the run |
| `one-sided ... not crossed within the steps run` | the run never reached the point. For a rate, `at most -6%`: the planner read at least 6% low, and how much lower is unknown. For a knee, `at most +3%`: the fleet held the planner's knee to within the run's 3% hold rule |

The error is the planner's figure over the measured one, less 1.

| Figure | `+` means | Safe side |
| --- | --- | --- |
| TTFT, TPOT | the planner reads slower than measured | `+` |
| Knee, onset, rate at a target | the planner promises more than the fleet held | `-` |

Where to find it:

- **CLI text:** after each figure, one label a line.
- **CLI JSON (`-o json`):** `validation` on each answer. It holds
  `validated`, `label`, `errorLo` and `errorHi` (shares, not percent),
  `readings` and `outside`; inside a measured row also `source`, `shape`,
  `spread` (the kind of range: `min-max`, `p10-p90`, `bracket` or
  `one-sided`), `levels` (the independent load levels among the readings)
  and `holdout` (true only for a row whose run never tuned the planner,
  false for an in-sample one). A refused figure, and the operating rate,
  which is an input, have none.
- **API (`POST /v1/plan`):** `validation` on the knees, the TTFT at the rate,
  the rate at the target, the TPOT's two points and the limits. Curve points
  and the marginal steps carry none.

## What was measured

One run so far: Qwen3-32B at tensor parallelism 2 on H200, 2 prefill and 2
decode Pods. It climbed a ladder of 6 load levels, from 0.40 to 0.90 of the
knee, each judged over 7 to 22 minutes. Each level ran twice, once with the
router's flow control off and once on. The client timed every request.

So "n=6 load levels, 12 readings" is 6 load levels, each read twice. The
planner prices both phases the same below the onset, so the two readings of
a level are one reading of the planner, and the count is 6. Over 6 values a
10th and a 90th percentile are just the second-lowest and the second-highest,
so under 10 independent levels the label gives the lowest and the highest.
The error grows with load: on long prompts the mean TTFT reads about +7% at
0.40 of the knee and +58% to +141% at 0.90. The range on a label covers the
whole band of load it names.

**In-sample.** This run is also one of the planner's goldens
(one of its test fixtures), and constants the planner ships were read from it. Its
errors are how well the planner fits the run it was tuned on, so treat them
as a floor on the error at another fleet, not a bound. No run kept out of
tuning has been measured yet; when one is, its rows will print `validated on
a holdout`.

**The decode knee.** Inside its region (the row below) the decode knee held
to within the run's 3% hold rule. Outside it, on the Qwen3-0.6B goldens, three
knees read 43% to 99% high past the table's bound and one 42% low; the
planner says the same 43% to 99%. `at-replicas` prints the label under the decode knee
in its capacity table, says the 0.6B range under `not validated` only when
the knee is outside, and sizes decode at the label's upper error inside the
region, or at 99.1% outside it, beside the point answer
(`needed.decodeAtErrorBound`, at most the search's `maxReplicas`). The
fleet needed never cuts decode below that count, whatever the verdict:
decode has a replica to spare only where one fewer still meets it. The
label's `n=` gives the independent load levels first, then the raw
readings. The region does not bound the arrivals' Ca^2: a knee is a
throughput, and how bursty the arrivals are does not move it, so a decode
knee at the run's fleet and shape reads validated at any Ca^2, though the
run sent Ca^2 1 (the row's shape). The label is all that moves; the knee
does not.

This table is generated from the rows the planner ships:

<!-- envelope-table: begin (go test ./cmd/capacity-planner -run TestStaircaseQ32 -update) -->
| Figure | Shape | Error | Range | Readings | Knee share | Flow control |
| --- | --- | --- | --- | --- | --- | --- |
| decode-knee | 1000-token prompts, 1000 out, Ca^2 1 | at most +3% | one-sided | 1 |  | off |
| flow-control-onset | 8000-token prompts, 200 out, Ca^2 1 | -4..+2% | bracket | 1 |  | on |
| flow-control-onset | 8000-token prompts, 200 out, Ca^2 4 | -7..-4% | bracket | 1 |  | on |
| mean TPOT | 1000-token prompts, 1000 out, Ca^2 1 | +1..+18% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | any |
| mean TPOT | 8000-token prompts, 200 out, Ca^2 1 | -3..+1% | min-max | 6 load levels (12 readings) | 0.096 to 0.215 | any |
| mean TPOT | 8000-token prompts, 200 out, Ca^2 4 | -5..-1% | min-max | 6 load levels (10 readings) | 0.096 to 0.203 | any |
| prefill-knee | 8000-token prompts, 200 out, Ca^2 1 | at most +3% | one-sided | 1 |  | off |
| prefill-knee | 8000-token prompts, 200 out, Ca^2 4 | at most +3% | one-sided | 1 |  | off |
| rate-at-ttft p90 | 1000-token prompts, 1000 out, Ca^2 1 | -6..+0% | bracket | 1 | 0.825 to 0.875 | off, on |
| rate-at-ttft p95 | 1000-token prompts, 1000 out, Ca^2 1 | -6..+0% | bracket | 1 | 0.825 to 0.875 | off, on |
| rate-at-ttft p90 | 8000-token prompts, 200 out, Ca^2 1 | at most -6% | one-sided | 1 | 0.825 to 0.875 | off, on |
| rate-at-ttft p95 | 8000-token prompts, 200 out, Ca^2 1 | at most -6% | one-sided | 1 | 0.825 to 0.875 | off, on |
| rate-at-ttft p90 | 8000-token prompts, 200 out, Ca^2 4 | at most +0% | one-sided | 2 | 0.740 to 0.875 | off, on |
| rate-at-ttft p95 | 8000-token prompts, 200 out, Ca^2 4 | at most +2% | one-sided | 2 | 0.740 to 0.875 | off, on |
| ttft mean | 1000-token prompts, 1000 out, Ca^2 1 | -15..+11% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft p90 | 1000-token prompts, 1000 out, Ca^2 1 | -11..+11% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft p95 | 1000-token prompts, 1000 out, Ca^2 1 | -11..+10% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft mean | 8000-token prompts, 200 out, Ca^2 1 | +7..+141% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft p90 | 8000-token prompts, 200 out, Ca^2 1 | +4..+152% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft p95 | 8000-token prompts, 200 out, Ca^2 1 | -4..+171% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft mean | 8000-token prompts, 200 out, Ca^2 4 | +9..+122% | min-max | 6 load levels (9 readings) | 0.400 to 0.850 | off, on |
| ttft p90 | 8000-token prompts, 200 out, Ca^2 4 | +25..+164% | min-max | 6 load levels (9 readings) | 0.400 to 0.850 | off, on |
| ttft p95 | 8000-token prompts, 200 out, Ca^2 4 | +31..+184% | min-max | 6 load levels (9 readings) | 0.400 to 0.850 | off, on |
<!-- envelope-table: end -->

What this says:

- **Short prompts:** the planner is close. TTFT reads from about 18% fast
  (the mean, at the lightest step) to 9% slow, and mean TPOT up to 18% slow.
  A figure that reads fast promises a TTFT the fleet will not deliver. At
  light load (0.4 of the knee) the p90 read 13% fast and the mean 18% fast:
  add that much before you compare a short-prompt TTFT with your SLO.
- **Mean TPOT on long prompts:** within a few percent, from 5% fast to 1% slow.
- **Long prompts:** TTFT reads from 4% fast to 184% slow. It is mostly slow,
  and more so near the knee.
  Sizing for TTFT therefore errs on the side of more Pods than needed.
- **Knees, flow control off:** every phase held at the knee the planner
  names. The run stopped there, and its hold rule cannot see a fleet short
  by less than 3%, so the knee is "not exceeded within the steps run": at
  most about 3% high.
- **Flow control on:** the fleet stops at the router's onset, not at the
  knee: 5% to 25% under it in this run. The knee is outside with flow control
  on; read the onset's label instead. For bursty traffic the router started
  holding requests 4% to 7% before the onset the planner names.
- **Rates:** the fleet held every rate the planner named at its target. For
  short prompts the true rate lies between the planner's and 6% above it. For
  long prompts the run's steps did not go far enough to say how much more.

A figure counts as inside only if every input that priced it is one the run
priced:

- The prompts' length spread was measured or stated, and is close to the
  run's (cv 0.046 to 0.072). The default, cv 2 with no histogram, is outside.
- The arrivals were measured or stated (`--arrival-cv2`), with Ca² within
  1.25x of the run's. Poisson assumed by default is outside.
- The router's flow-control state is known. Without its queue series, state
  it with `--flow-control on|off`. A knee needs it off.
- Every term was learned from the recording. A flag that types a term in
  (`--assume-vp`, `--assume-a`, `--assume-router-hop`,
  `--assume-transfer-base`, `--batch-cap`, `--assume-tpot-ratio`, the
  experimental pricing flags, the capture's engine overrides) puts every
  figure outside. So does any flag on a figure that the run's own figures did
  not carry.
- The model is Qwen3-32B, at tensor parallelism 2 for prefill and decode, on
  H200, with 2 prefill and 2 decode Pods.
- Prompt and output lengths are within 1.25x of a measured shape, and the
  prompts share little prefix (hit rate up to 0.05).
- Statistic: the mean, p90 or p95.
- The load is within the levels the run measured; the table gives each
  row's band of knee shares.
- Flow control below its onset. A figure flagged `AboveFlowControlOnset` is
  outside: the router's wait there is not priced.

Past the onset on router v1.0.0, `--router-wait-in-prefill` takes the
router's wait as part of the prefill time, so the figure stands and a rate
answer can pass the onset (up to 0.9 of the prefill knee). It is on by
default. On a router v1.0.0 staircase (Qwen3-32B, 2 prefill and 2 decode,
three seeds), the figure read 1.3x to 3.4x the router's measured TTFT at 15
steps past the onset, never under, and no step up to 0.9 of the knee
collapsed. One more step, disturbed when both prefill pods were replaced, read
0.78x at the p95; the backtest excludes it as warm-up, a choice made after the
run. The figure errs high: it overstates the wait rather than claiming an SLO
the fleet misses.

**On router v0.11.0, set `--router-wait-in-prefill=false`** (the API's
`assume.routerWait: none`). There the router's gate collapsed on its TTL at 15
of 41 steps past the onset, and this figure reads short at every one of them.
Off, a figure past the onset is flagged `AboveFlowControlOnset`, a miss, and
the rate answer stops at the onset. The planner cannot read the router's
version, so this is the operator's call.

To ask the CLI the way the run was priced, for example:

```bash
capacity-planner replicas-to-slo --recording run.jsonl.gz --prefill 2 --decode 2 \
  --assume-input-tokens 8000 --assume-output-tokens 200 --ttft-percentile 90 \
  --assume-prompt-cv 0.0577 --arrival-cv2 1 --flow-control off
```

**A backtest prices each step at its own output spread.** The spread of
output lengths sets the KV a running request holds. By default a backtest
step is priced at the spread its own decode pods' output-length histograms
read, as it already is at its own lengths; a step that read none keeps the
fit's. Before, every step took the spread the fit read in its last minutes.
On the 0.6B staircase, whose warm-ups end on a few minutes of short outputs,
the predicted decode KV fill on the 1000/1000 phases fell from 1.87 to 1.59
times what the pods held, and on the Qwen3-32B A-off phase from 1.19 to
1.05. No backtest figure newly read more than 20% under what was measured.

## Never validated

These always read `OUTSIDE VALIDATED REGION`:

- Aggregated variants (one Pod does prefill and decode).
- Prefill's own latency. The client times TTFT end to end only.
- TPOT percentiles, and decode's rate at a TPOT target.
- A rate the operating-point cap set. Read the knee's label instead.
- A knee with flow control on or not known. Read the onset's label instead.
- Any figure priced with a flag that types a term in.
- Any prefill or TTFT figure priced with `--experimental-ttft`
  (`ExperimentalTTFT`) or from the occupancy plateau (`VPFromPlateau`).
- p99, and any statistic but the mean, p90 and p95.
- Any model but Qwen3-32B, any tensor parallelism but 2, and any Pod count
  but 2 prefill and 2 decode.
- Any accelerator but H200.
- Any TTFT from `backtest --experimental-ttft=v2` (below).
- Any TPOT percentile from `--experimental-tpot=v2`, in a backtest or the main view (below).

## Experimental: the rank-mixture TTFT model

`capacity-planner backtest --experimental-ttft=v2` prices TTFT with a second
model, for a P/D fleet whose prefill engines run chunked prefill on several
data-parallel ranks. It is off by default, and every TTFT it prices is flagged
`RankMixtureTTFT` and reads `OUTSIDE VALIDATED REGION`. TPOT, the exclusions and
the scoring are the planner's own.

What it prices, per request: `TTFT = Z + q + P`.

| part | model | where it comes from | assumption, and the check on each step |
|---|---|---|---|
| `P`, prefill | limited processor sharing on the rank the request lands on, `K` = `--max-num-seqs` prompts at once (1 without chunked prefill). With probability `1 - rho^K` the request starts at once and takes the PS sojourn `S(x)/(1 - rho)`, spread by the planner's PS rule; otherwise it first waits an exponential admission wait. The mean is Zhang and Zwart's, `E[S]/(1 - rho) * ((1 - rho^K) + rho^K (1 + cs^2)/2)`. `S(x) = s0 + x/V`, `x` drawn from the computed-token histogram, which also gives `cs^2` | `s0` and `V` by inverting each rank's mean prefill time, `E[S] = E[T]/(1 + lambda E[T])`, then a line in its mean computed prompt. Where the prompts do not spread (a staircase at one length), the line is not identified: the service is taken proportional to the prompt (`serviceProportional`), right at the prompt it was read at | the approximation is a heavy-traffic one: a rank above `rho` 0.9 is flagged `LimitedPSHeavyTraffic`. Against a simulated rank at `cs^2` 4.9 its mean read within 8 % at `rho` 0.3 to 0.85, and plain PS 17 % to 46 % low from 0.6. The answer reports the share of requests that wait for admission and the busiest rank's tail index (Nair, Wierman and Zwart) |
| the ranks | each rank its own queue at its share of the traffic; the population is the mixture | the ranks' measured request shares, busiest first | the shares hold over the fit (`shareCv` in the answer) |
| `q`, KV wait | none with probability `1 - p_d`, else exponential with mean `m_d`; both lines in the ranks' utilization. Fitted, not derived | the prefill engines' queue-time histogram at 0.3 s and 1 s | priced inside the utilizations it was fitted over, else flagged `KVDeferralOutside` |
| `Z`, the rest | `a0 + s * beta * L`, `L` drawn from the prompt-length histogram | `beta`: the slopes of the engine's pre-queue overhead, the decode engine's first token and the router's remainder against the prompt. A slope is fitted only where its prompts spread (cv 0.03 or more); at one prompt length it is 0 and named in `betaUnidentified`. `s` is a calibration, fitted on the fit window's self-check folds so that their median p90 ratio is 1; p95 is not fitted. On a fit window under 12 h, `s` is 1, not fitted (`spreadDefaulted`); a fitted `s` under 1 is priced at 1 (`spreadFloored`) | `s` near 1 means `beta` holds the prompt's whole effect; its distance from 1 is what `beta` misses. Fitted `s` follows the fit window's own p90-over-mean, so it is not yet known to carry from one day to the next |
| each step | priced as if its rate held (pointwise stationary) | | the busiest rank's relaxation, `E[S]/(1 - rho)^2`, well under the step; else the step is not scored (`unsettled`). A step whose arrivals (the router's rate `A`) ran ahead of its completions (the engine rows' rate `D`) by `(A - D)/D` over 0.1 left a backlog adding over a tenth of the step to the wait: it is not stationary. The fit leaves it out (`fitStepsBacklogged`), and the model refuses it as a test step |

Two fixes to the fit are off by default, and `--rank-mixture-fit-steps`
(`rankMixtureFitSteps`) turns both on:
- **Clean fit steps.** The KV-deferral lines and the prompt slopes are
  fitted only on steps whose busiest prefill rank sits under utilization 0.9.
  Past it, a step's queue time is the overload's, not a wait for KV, and its
  front end slows with the overload, not with the prompt.
- **Shares at the load.** A router that balances by load spreads a busy
  fleet's requests more evenly than a quiet one's. The ranks' shares at a
  priced rate are the mean of the five fit steps nearest that rate, not the
  fit window's mean.

With fewer than four steps under 0.9, the two fits read every step; with
fewer than five steps whose ranks carried a request a second, the shares are
the fit window's mean. The model's `assumed` line says which it priced.

**Each rank's service is read from the histogram, by default.** The mean
service and its spread are read once from the computed-token histogram
itself, and each rank draws one request per equal slice of it. The older
way read them from each rank's own few hundred random draws. On a
computed-token histogram with a long tail (a few requests compute a whole
uncached prompt), whether those draws catch the tail is chance, and a figure
moved by several percent with the seed. On a production P/D recording,
priced with three seeds, a step's figure moved by a median 2.4 to 2.9 % and
at most 12 to 17 % the older way, and by 0.5 to 1.0 % and at most 1.2 to
2.6 % this way. The median ratios to measured did not move.

To price the older way, set `--rank-mixture-stratified=false` on
`backtest`, `at-replicas` or `serve`, or send `rankMixtureStratified` false
in the request. The backtest's `rankMixture.stratified` says which way it
priced. The two switches are independent: `--rank-mixture-fit-steps` can
be on or off either way.

The model prices only what its inputs identify. It abstains on the whole backtest where:
- a prompt slope is not identified;
- the spread is not calibrated (fit window under 12 h);
- the spread is not identified: it, or one self-check fold's, ends at its upper bound 8, so no spread
  brings the fold's p90 ratio to 1;
- `a0` is negative. `a0` is refitted at each spread to hold the mean, so a spread that outgrows `beta`
  pushes it down; a negative one is that offset, not a time;
- the service line is not identified (one prompt length);
- the ranks' own rates or `--max-num-seqs` are not read;
- or the fit fails.

Every TTFT is then the planner's own, flagged `RankMixtureAbstained`, and `rankMixture.abstained` says why. On
the public staircases (about an hour each, at one prompt length) it abstains.

The answer's `rankMixture` block names every fitted figure, and each step's
`rankMixture` block its checks. The model needs a recording made by this version
or later. It reads each prefill rank's own request rate, prefill time, queue
time, TTFT and prompt from a LeaderWorkerSet group's engines
(`GroupEngines`). In an older recording, a row's ranks share its rate evenly,
and the answer says so.

### The measured non-queue variant (`--experimental-ttft=v2nl-line`)

`backtest --experimental-ttft=v2nl-line` prices with v2's queue and service
line, and fits them another way. It is off by default, and its figures carry
the same flags. The answer's `rankMixture.model` reads `rank-mixture-nl-line`.

- **Non-queue part.** `Z` is measured part by part, not calibrated. Each part
  is its own line in the prompt, with an intercept and slope held at 0 or
  above: the engine's pre-queue overhead, the decode engine's first token and
  the router's remainder. `a0` is the sum of the intercepts, so it is never
  negative, and `s` is 1. `rankMixture.zParts` names each part.
- **Spreads of the parts.** Each part varies request to request. It is drawn
  lognormal about its line, so its mean does not move. The decode part's sigma
  comes from the decode engines' TTFT histogram. The overhead's is the one at
  which the priced prefill-engine TTFT reads the engines' measured p90. The
  router's remainder has no histogram of its own and stays a line.
- **v2's switches, always on.** The variant fits as
  `--rank-mixture-fit-steps` and `--rank-mixture-stratified` do: the
  KV-deferral lines and the parts on the steps under utilization 0.9, the
  shares at the priced load, and each rank's service read from the histogram.

No spread or location is calibrated to the router's TTFT, so the fit window
must price itself. This check is on the same window, and the router's
remainder is the router's mean less both engines': a fit window priced at its
mean mostly says the prefill engine's queue and service are. The variant
abstains where:
- the fit window's median ratio of priced to measured mean (`fitMean`) is
  outside 0.8 to 1.25;
- its median p90 ratio (`fitP90`) is outside 0.9 to 1.25;
- the fit window is under 12 hours, too short for those checks;
- the computed prompts do not spread (one prompt length);
- or, as for v2, the ranks' own rates or `--max-num-seqs` are not read.

On the public recordings it abstains, mostly on the fit window's length.

Two modes of the variant, each off by default and refused without
`--experimental-ttft=v2nl-line`. Neither moves `fitMean` or `fitP90`, so
neither changes whether the variant abstains.

- **KV-deferral hold** (`--rank-mixture-kv-hold`, `rankMixtureKVHold`). The
  KV-deferral lines are fitted over the fit window's loads. Priced past them,
  the lines are extrapolated, and on a production P/D recording that read
  the busier half's queue well over what was measured. With the mode, the
  lines are read no further than the 0.95 quantile of the rank utilizations
  the fit window prices (`rankMixture.kv.rhoHold`); past it the wait is held.
  A figure past the fitted range is still flagged `KVDeferralOutside`.
- **Router remainder spread** (`--rank-mixture-remainder-spread`,
  `rankMixtureRemainderSpread`). The router's remainder is drawn from the
  router's measured request-processing time
  (`llm_d_epp_request_processing_duration_seconds`): its line less that
  time's mean, plus a draw of that time's measured law, so a mostly flat
  line gets the router's own tail. `rankMixture.zParts` gives the mean and
  the 0.95 quantile over the mean. It needs a recording that holds the
  series, on at least 4 fit steps. Recordings made before it was recorded,
  and every public recording here, hold none: the remainder is then its
  line, as with the mode off, and the assumed line says so.

On the public recordings neither mode moves a figure: the variant abstains
there, and none holds the router's request-processing series. On a
production P/D recording, re-captured with the series (development window
only):

- The hold alone took the load split's p90 and p95 figures under 1.25 of
  measured, and read one more mean under 0.8. That mean is one test step,
  the split's lowest, which already read just over 0.8 with the mode off; the
  hold lowers it a little, as it lowers every busier step. The offline
  reference lowers the same step by the same amount, and keeps it just over
  0.8 only because it reads the step a little higher with the mode off. It is
  how the hold acts on one outlier step, not a difference in the hold.
- The remainder spread alone brought the time split's p90 and p95 toward
  measured, and most of its figures under 0.8 back within, but it raised
  the load split's figures over 1.25 several times over.
- Both together kept the time split's gain, left fewer figures over 1.25
  on the load split than with both off, and read no figure under 0.8 that
  read within with both off. Use the two together.

The hold given without the remainder spread is not refused, so each can
still be compared alone, but `backtest`, `serve`, `at-replicas` and
`day-plan` print a warning and the backtest's assumed line says so.

A third mode builds on the remainder spread: the **remainder server**
(`--rank-mixture-remainder-server`, `rankMixtureRemainderServer`), refused
without `--experimental-ttft=v2nl-line` and `--rank-mixture-remainder-spread`
and off by default. It does not move `fitMean` or `fitP90` either. Most of
the router's remainder is the router calling a serving pod's `/render`
endpoint, and that pod's API server handling the generation request before
and after the engine's own time. The mode draws the remainder from those
parts, each measured, in place of one line:

- the router's own work outside its render call: its request-processing mean
  less its render mean (`llm_d_epp_token_producer_render_duration_seconds`),
  a constant;
- the render call on the pod: its `/render` handler time
  (`http_request_duration_seconds`), a line in the prompt;
- the generation request's time in the pod's API server outside the engine:
  its HTTP handler time less vLLM's end-to-end time
  (`vllm:e2e_request_latency_seconds`), a line in the prompt. Optional: where
  the pods do not record it, it is part of the rest;
- the rest, which nothing measures (the sidecar, the network, the decode
  pod's API server, the API server's output path): a line in the prompt,
  fitted to what the parts above leave of each fit step's remainder.

The pods read are the ones the variant prices (prefill pods); a decode pod's
API server is never read, and is part of the rest. The router's work and the
render are drawn on the remainder spread's measured law, scaled to their
predicted mean, as the spread draws the router's processing. No part moves
with the rate: a load-dependent term on the pods' API servers added nothing
on two production recordings. `rankMixture.zParts.remainderServer` gives
every fitted figure. The assumed line names the rest and its share of the fit
window's remainder (`restShare`); where that share is over 0.5, a TTFT priced
at a rate past the fit window's top (`topRate`) is flagged
`RemainderRestPastFit`, since nothing says how the unmeasured part moves past
the loads it was read over. The mode needs at least 12 fit steps that hold
the router's processing and a pod's render or frontend: recordings made
before these series were recorded, and every public recording here, hold
none, and the remainder is then the plain spread, `zParts.remainderServerOff`
says why and the assumed line says so.

On a production P/D recording re-captured with these series (development
window only), against the hold and the spread together, the remainder
server:

- read every p90 and p95 figure on the load split within 1.25 of measured,
  where the hold and the spread together left several over it;
- brought the busiest steps' p90 and p95 closer to measured, with fewer
  figures over 1.25;
- cost the time split a little: its p95 read slightly lower, and a few more
  figures read under 0.8 than with the spread alone, though far fewer than
  with every mode off;
- read one more mean under 0.8 on the load split, on the same outlier step
  the hold lowers (just under 0.8 now, just over it before).

Its render, frontend and router-work figures matched the offline reference's
within a few percent (the rest has the same mean, held as a constant), and
the deltas above matched the reference's in direction. On every
public recording, and on that recording as first captured (without the
pods' series), the mode prices every figure exactly as the spread does.

The 12-hour minimum, for v2 and the variant alike, is a default:
`--rank-mixture-min-fit-window` (`rankMixtureMinFitWindow`) lowers it, on
`backtest`, `serve`, `at-replicas` and `day-plan`. A model fitted on a shorter
window prices as it would on a long one, but its own checks (v2's spread
calibration, the variant's fit-window ratios) read fewer hours and loads, so
every TTFT it prices is flagged `RankMixtureShortFitWindow`.

`serve` and `at-replicas` take `--experimental-ttft=v2nl-line` too, and fit
and price the variant exactly as `backtest` does, abstentions included
([serve with the experimental models](page-and-commands.md#serve-with-the-experimental-v2-models)).

## Experimental: the per-request TPOT distribution

`capacity-planner backtest --experimental-tpot=v2` prices the TPOT percentiles
from a model of each request's TPOT, in place of the ratio `R_q` read from the
histogram's bucket edges. It is off by default. Every percentile it prices is
flagged `TPOTDistribution` and reads `OUTSIDE VALIDATED REGION`. The mean TPOT
is the planner's own, and the distribution is placed around it.

Why a ratio read at a bucket edge runs wide: the router's TPOT buckets are
coarse (10 ms to 20 ms, then 20 ms to 40 ms). Where the measured p90 falls in
the 10 ms to 20 ms bucket, the edge reading prices it at 20 ms whatever the
spread inside is.

What it prices, per request: `log TPOT ~ Normal(log m - sigma^2/2, sigma^2)`,
with `m` the planner's mean and `sigma^2 = g^2 + C E[1/(L-1)]`.

| part | model | where it comes from |
|---|---|---|
| the server | a decode replica's data-parallel ranks step in lockstep. Below `--max-num-seqs` no sequence waits for a slot: the step time, not a queue, sets a token's time | the share that would wait is Erlang C at the fit window's highest mean running sequences per rank (`slotWait`). Over 1 % the model abstains |
| `g^2`, the replicas | the spread of the replicas' time per token, weighted by their request rates | each replica's step time over `1 +` the drafts it accepts a step |
| `C E[1/(L-1)]`, the request | with speculative decoding a step emits `1 + A` tokens. The steps a request of `L` tokens needs is a first passage of that renewal process. By the renewal central limit theorem its squared CV is about `Var(1+A)/(1+E A)` over `L - 1`: a long request sits near the mean, a short one spreads | `E[1/(L-1)]` from the decode pods' output-length histogram over the step. `C` is fitted by interval-censored likelihood over every bucket of the fit window's router TPOT histogram, floored at the renewal value for the engine's `num_speculative_tokens` (`renewal`) |

`C` is the only fitted figure, and no percentile is fitted, so p90 and p95 are
both held out. The fit window's two self-check folds (its first half, its first
three quarters) each fit `C` alone (`foldC`) and are scored on the steps after
their cut (`fold`).

Each step is also scored at the histogram's resolution: the largest gap between
the predicted and the measured share of requests under a bucket edge (`ks`), and
whether the predicted p90 and p95 lie in the measured ones' joint bracket (see
below), or under it or over it. `shapeKs` is the same gap with the distribution
placed at the measured mean, so it shows the shape's miss without the mean's. A
note under the answer gives these figures beside the planner's own ratio.

The model abstains on the whole backtest where `--max-num-seqs` is not read,
where a slot queue would form, where under two hours of fit steps read the
router's TPOT histogram and the output lengths, or where `C` ends at its bound.
Every percentile is then the planner's own, flagged `TPOTDistributionAbstained`.

The two hours of fit steps (24 five-minute steps) are a default:
`--tpot-dist-min-fit-steps` (`tpotDistMinFitSteps`, at least 4) lowers it on
`backtest`, `serve`, `at-replicas` and `day-plan`, so a lab recording gets the
distribution too. A distribution fitted on fewer than 24 steps reports
`shortFit` and `minFitSteps`, and flags every percentile it prices
`TPOTDistributionShortFit`: its renewal coefficient and its self-check folds
read fewer steps.

### Where a measured TPOT p90 or p95 lies: the joint bracket

A step carries two histograms of each request's TPOT: the router's (edges 5,
10, 20, 40, 60 and 80 ms) and the decode engines' own (vLLM's 10, 25, 50, 75
and 100 ms). Each one pins the share of requests under each of its edges. The
backtest reads the measured p90 and p95 from both at once:

- **Where it lies.** The bracket is the tightest interval the percentile must
  lie in under both histograms: from the largest edge the share is surely
  under it at, to the smallest edge the share is surely at or over it at.
  Each share is allowed two binomial standard errors either way, plus 0.005
  for the two counting slightly different requests. A thin step therefore
  gets a wider bracket than its bucket.
- **The engines join above their first edge only.** Their first bucket is a
  different population: it holds requests the router never times, and on a
  P/D fleet an extra observation near zero for about half the requests. The
  router and the engines disagree at their shared 10 ms edge on most steps.
  So above 10 ms the engines' histogram gives the shape, anchored at the
  router's share at 10 ms, and only where at least 50 requests lie above it.
  The engines' histogram is a one-minute rate, so its requests, and with them
  its noise and those 50, are counted over that minute, not over the
  router's five.
- **The figure inside.** The measured figure is read inside the merged
  sub-bucket with the log of the share above it linear in the log of the
  TPOT. Held out at the engines' 25 ms edge, predicted from the router alone,
  this reading's tail ratio was 1.16 at the median. Linear reading inside the
  bucket, which the backtest used before, gave 1.74.
- **When the two disagree.** Where they cannot both hold at that noise, the
  bracket is the router's own bucket, and the step counts as a conflict
  (`conflicts`).

This changes what "inside" means for the TPOT p90 and p95, in the checks and
in the main backtest alike. A predicted figure is inside where it lies in the
bracket; `p90Below`, `p90Above`, `p95Below` and `p95Above` count the rest. In
the main backtest the bracket is the measured bucket: one wider than 20% of its
lower edge leaves the pair `unresolvable` with the prediction near it, and a
certain miss with the prediction more than 20% past it. With the router alone
the bracket is its bucket, widened by the noise. The mean and the p50 are the
router's own.

What it removes: the linear reading inside a wide router bucket read the
measured p90 and p95 high, which made the planner's and the model's figures
look low. Against the joint bracket, the `=v2` model is almost never under the
bracket, and is slightly over it at p95.

**This changes the accuracy figures a backtest reports for TPOT p90 and p95,
with no change to the planner.** The measured figure, and with it the ratio,
`within`, `low`, the certain misses and the `unresolvable` count, now come
from the bracket. Backtests from before this change are not comparable on
those two rows. The change does not lean one way:

- Where an engine edge falls inside the router's bucket, the bracket is
  narrower, so a prediction that was `unresolvable` can now be a certain miss.
- Where a step is thin, or its share sits near an edge, the noise widens the
  bracket past the router's bucket. A prediction that was a certain miss can
  then be `unresolvable`: the histograms cannot say it missed. It is not
  counted within.
- The figure inside a wide bucket is now read lower (the log-linear reading
  above), so a planner that reads TPOT slow scores worse, and one that reads
  it fast scores better.

The mean and the p50 rows do not change.

### Experimental: the per-length mixture (`--experimental-tpot=v2-mix`)

`--experimental-tpot=v2-mix` (`tpotModel` `renewal-lognormal-per-length`)
prices the same distribution as one lognormal per output-length group, in
place of one lognormal at the lengths' mean `E[1/(L-1)]`:

`P(TPOT <= x) = sum over groups of w_i Phi((log(x/m) + s_i^2/2) / s_i)`, with
`s_i^2 = g^2 + C / (L_i - 1)` and `w_i` the group's share of the requests.

Each output-length bucket is cut into up to 24 groups, log-even, with the
lengths spread as `E[1/(L-1)]` spreads them. With one group it is the
lognormal. `C` is fitted the same way, each fit step at its own lengths. The
main view prices every rate at the source window's pooled lengths, as
`=v2` prices at their pooled `E[1/(L-1)]`.

It is off by default. It stays off until it passes a sealed test window.

On the public lab recordings, each fitted and scored on its own steps (in
sample), at the measured mean, the steps whose p90 or p95 lies in the measured
one's bracket:

| recordings | steps | `=v2` p90 | `=v2` p95 | `=v2-mix` p90 | `=v2-mix` p95 |
|---|---|---|---|---|---|
| Qwen3-32B staircase, 6 runs (q32j) | 136 | 135 | 133 | 135 | 133 |
| Qwen3-32B warm-up (cpval-s2) | 29 | 24 | 21 | 27 | 25 |
| Qwen3-32B warm-ups (q32h, q32f, repo-q32) | 71 | 53 | 38 | 65 | 60 |
| Qwen3-0.6B staircases and warm-ups (4 runs) | 82 | 40 | 37 | 63 | 41 |

An earlier count of this table read the engines' one-minute rates as if they
held over the router's five minutes, which took five times their requests and
too little noise. Counted over the minute, the engines join on fewer steps
(cpval-s2 19 of 29 where it was 29, q32j 132 of 136 where it was 133), and
none of the counts that table held moved. The q32h, q32f and repo-q32 row is
new.

Where `=v2` misses, it reads over the bracket: on Qwen3-0.6B, 41 p90s and 45
p95s over it, against 18 and 40 for the mixture. The q32j runs barely tell the
two apart: nearly every step is already inside. On Qwen3-0.6B most requests'
TPOT is under 10 ms, where neither histogram has an edge; only finer vLLM
buckets (`--custom-histogram-buckets`) would place those percentiles better.

## Adding a run

The regions are data, in `internal/signals/whatif/envelope.json`. A new run
adds rows with its own `source`. `TestStaircaseQ32_TheErrorsAreFrozen` writes
the q32 rows (`go test ./cmd/capacity-planner -run TestStaircaseQ32 -update`).
It prices every step of the run and writes the per-step table to
`cmd/capacity-planner/testdata/staircase-q32-validation.golden`.
[The proposal's "Calibration envelope, as built"](../../proposals/slo-capacity-planner.md#calibration-envelope-as-built)
explains how the rows are drawn.
