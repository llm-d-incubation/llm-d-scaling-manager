# Capacity planner: how accurate each figure is

Every figure the [capacity planner](README.md) prints says whether a real run
checked it, and how far off it read there. **Validated means measured, not
accurate: read the range.** A validated figure can be twice the real value.
And every region so far is **in-sample**: the one run that measured it also
tuned the planner (see [What was measured](#what-was-measured)).

## Read the label

Each figure carries one of two labels.

- **`validated in-sample: error +15..+156% (min-max, n=6 load levels, 12 readings; q32 staircase)`.**
  Your fleet and traffic sit inside what a measured run covered. On that
  run the planner's figure read 15% to 156% over what the client measured,
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
  `readings` and `outside`. A refused figure, and the operating rate, which
  is an input, have none.
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
The error grows with load: on long prompts the mean TTFT reads about +27% at
0.40 of the knee and +63% to +149% at 0.90. The range on a label covers the
whole band of load it names.

**In-sample.** This run is also one of the planner's goldens
(one of its test fixtures), and constants the planner ships were read from it. Its
errors are how well the planner fits the run it was tuned on, so treat them
as a floor on the error at another fleet, not a bound. No run kept out of
tuning has been measured yet; when one is, its rows will print `validated on
a holdout`.

**The decode knee.** Inside its region (the row below) the decode knee held
to within the run's 3% hold rule. Outside it, on the Qwen3-0.6B goldens, it
read 44% to 103% high. `at-replicas` prints the label under the decode knee
in its capacity table, says the 0.6B range under `not validated` only when
the knee is outside, and sizes decode at the label's upper error inside the
region, or at 103% outside it, beside the point answer
(`needed.decodeAtErrorBound`). The JSON's `readings` is the raw count; the
label's `n=` gives the independent load levels first.

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
| rate-at-ttft p90 | 1000-token prompts, 1000 out, Ca^2 1 | at most -6% | one-sided | 1 | 0.825 to 0.875 | off, on |
| rate-at-ttft p95 | 1000-token prompts, 1000 out, Ca^2 1 | at most -6% | one-sided | 1 | 0.825 to 0.875 | off, on |
| rate-at-ttft p90 | 8000-token prompts, 200 out, Ca^2 1 | at most -6% | one-sided | 1 | 0.825 to 0.875 | off, on |
| rate-at-ttft p95 | 8000-token prompts, 200 out, Ca^2 1 | at most -6% | one-sided | 1 | 0.825 to 0.875 | off, on |
| rate-at-ttft p90 | 8000-token prompts, 200 out, Ca^2 4 | at most +0% | one-sided | 2 | 0.740 to 0.875 | off, on |
| rate-at-ttft p95 | 8000-token prompts, 200 out, Ca^2 4 | at most +2% | one-sided | 2 | 0.740 to 0.875 | off, on |
| ttft mean | 1000-token prompts, 1000 out, Ca^2 1 | -5..+18% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft p90 | 1000-token prompts, 1000 out, Ca^2 1 | -2..+16% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft p95 | 1000-token prompts, 1000 out, Ca^2 1 | -4..+14% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft mean | 8000-token prompts, 200 out, Ca^2 1 | +27..+149% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft p90 | 8000-token prompts, 200 out, Ca^2 1 | +15..+156% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft p95 | 8000-token prompts, 200 out, Ca^2 1 | +5..+174% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft mean | 8000-token prompts, 200 out, Ca^2 4 | +22..+127% | min-max | 6 load levels (9 readings) | 0.400 to 0.850 | off, on |
| ttft p90 | 8000-token prompts, 200 out, Ca^2 4 | +33..+167% | min-max | 6 load levels (9 readings) | 0.400 to 0.850 | off, on |
| ttft p95 | 8000-token prompts, 200 out, Ca^2 4 | +37..+186% | min-max | 6 load levels (9 readings) | 0.400 to 0.850 | off, on |
<!-- envelope-table: end -->

What this says:

- **Short prompts:** the planner is close. TTFT reads up to about 18% slow,
  mean TPOT up to 18% slow.
- **Mean TPOT on long prompts:** within a few percent, from 5% fast to 1% slow.
- **Long prompts:** TTFT reads 15% to 190% slow, more so near the knee.
  Sizing for TTFT therefore errs on the side of more Pods than needed.
- **Knees, flow control off:** every phase held at the knee the planner
  names. The run stopped there, and its hold rule cannot see a fleet short
  by less than 3%, so the knee is "not exceeded within the steps run": at
  most about 3% high.
- **Flow control on:** the fleet stops at the router's onset, not at the
  knee: 5% to 25% under it in this run. The knee is outside with flow control
  on; read the onset's label instead. For bursty traffic the router started
  holding requests 4% to 7% before the onset the planner names.
- **Rates:** the fleet held every rate the planner named at its target. The
  run's steps did not go far enough to say how much more.

A figure counts as inside only if every input that priced it is one the run
priced:

- The prompts' length spread was measured or stated, and is close to the
  run's (cv 0.046 to 0.072). The default, cv 2 with no histogram, is outside.
- The arrivals were measured or stated (`--arrival-cv2`), with Ca² within
  1.25x of the run's. Poisson assumed by default is outside.
- The router's flow-control state is known. Without its queue series, state
  it with `--flow-control on|off`. A knee needs it off.
- Every term was learned from the recording. A flag that types a term in
  (`--assume-vp`, `--assume-a`, `--assume-router-hop`, `--batch-cap`,
  `--assume-tpot-ratio`, the experimental pricing flags, the capture's engine
  overrides) puts every figure outside. So does any flag on a figure that the
  run's own figures did not carry.
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
answer can pass the onset (up to 0.9 of the prefill knee). On six bursty
steps (Ca² 4, up to 0.88 of the knee) it read 1.5x to 3.7x the client's
TTFT, never under; that margin comes from the learned V_P being a lower
bound, not from the switch. It is off by default, because no Poisson step past the onset has run on
v1.0.0, and setting it puts every figure outside the validated region, even
where it does not apply. Do not use it on router
v0.11.0, where the wait adds to TTFT and this reads short.

To ask the CLI the way the run was priced, for example:

```bash
capacity-planner replicas-to-slo --recording run.jsonl.gz --prefill 2 --decode 2 \
  --assume-input-tokens 8000 --assume-output-tokens 200 --ttft-percentile 90 \
  --assume-prompt-cv 0.0577 --arrival-cv2 1 --flow-control off
```

## Never validated

These always read `OUTSIDE VALIDATED REGION`:

- Aggregated variants (one Pod does prefill and decode).
- Prefill's own latency. The client times TTFT end to end only.
- TPOT percentiles, and decode's rate at a TPOT target.
- A rate the operating-point cap set. Read the knee's label instead.
- A knee with flow control on or not known. Read the onset's label instead.
- Any figure priced with a flag that types a term in.
- p99, and any statistic but the mean, p90 and p95.
- Any model but Qwen3-32B, any tensor parallelism but 2, and any Pod count
  but 2 prefill and 2 decode.
- Any accelerator but H200.

## Adding a run

The regions are data, in `internal/signals/whatif/envelope.json`. A new run
adds rows with its own `source`. `TestStaircaseQ32_TheErrorsAreFrozen` writes
the q32 rows (`go test ./cmd/capacity-planner -run TestStaircaseQ32 -update`).
It prices every step of the run and writes the per-step table to
`cmd/capacity-planner/testdata/staircase-q32-validation.golden`.
[The proposal's "Calibration envelope, as built"](../../proposals/slo-capacity-planner.md#calibration-envelope-as-built)
explains how the rows are drawn.
