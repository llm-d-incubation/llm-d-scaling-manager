# Check the planner against a period it did not see

Run this before you trust the planner with your fleet. It learns from one
part of your own recording. It then predicts the TTFT and TPOT of the rest
from that period's traffic and replica counts only, never from its measured
latency, and compares each prediction with what your fleet measured.

## What you need

- [ ] The planner, as in [Get the planner](README.md#get-the-planner).
- [ ] A recording of a few hours of your fleet, from
  [Record a window](../record-for-capacity-planner/). Longer is better: the
  part the planner learns from must hold busy minutes, or it has nothing to
  learn. The recording must hold traffic you care about, at the replica
  counts you run.

## Run it

One command, on one recording:

```bash
capacity-planner backtest --recording run.jsonl.gz --split 60%
```

It learns from the first 60% of the recording and tests on the last 40%. To
cut at a time instead, use `--fit-until 2026-11-20T12:00:00Z`. To learn from
one recording and test on a later one of the same configuration, use
`--fit day1.jsonl.gz --test day2.jsonl.gz`.

To check that the planner extrapolates to load it has not seen, split by
load instead of by time:

```bash
capacity-planner backtest --recording run.jsonl.gz --fit-below p50 --test-above p75
```

It learns only from the five-minute steps whose arrival rate is below the
recording's median step rate, and judges only the steps in its top quartile.
Each bound is a rate (`12.5`, requests per second) or a percentile of the
steps' rates (`p50`); idle steps are left out of the percentile. The fit is
read from its steps alone, as if the recording held nothing else, and every
other check still applies: warm-up, replicas changing, bursts near the knee.
Steps under the test's bound are reported as `outside the test's load`.

With `--fit` and `--test`, `--fit-below` reads the fit's steps and
`--test-above` the test's, and the two recordings may cover the same hours:
a test step that the fit read a cycle of is not judged.

On the page: load the recording in Source, open **Check the planner against
a period it did not see**, set the slider, and press **Run**. To test on
another recording, pick **another recording** and choose it. The page never
runs the check by itself: **Check accuracy on this recording**, in the
answer panel's Accuracy row or on this tab, runs it with the slider's split
(half, unless you move it).

## Read the answer

The first lines are the answer. For example:

```text
Over the 1 h 13 min the planner did not see:
mean TTFT: within 20% of measured in 0 of the 14 five-minute steps scored (a figure, and enough measured requests);
its errors are wide; it leans high (median ratio 1.73); it read 2 more than 20% low. For 1 more it gave no figure,
calling the fleet overloaded; the fleet measured mean 11.6 s there. p90 TTFT: within 20% in 0 of 9, median ratio
1.88, 2 read more than 20% low.
mean TPOT: within 20% of measured in 15 of the 15 five-minute steps scored (a figure, and enough measured requests);
no overall lean (median ratio 0.98). p90 TPOT not scored in 10: the measured p90 fell in a histogram bucket wider
than 20% of its lower edge, too wide to place it, with the prediction inside it. To measure it, give the
histogram finer edges: the router's metrics.requestTPOTBuckets in the EndpointPickerConfig (an llm-d-router build
after v1.0.0), or vLLM's --custom-histogram-buckets (v0.32.0 and later). Edges about 15% apart place p90 and p95
within 20%.
None of these figures falls within the conditions our own accuracy benchmarks cover
(docs/guides/capacity-planner/accuracy.md): this check is the evidence for this fleet.
Judged 15 of 16 five-minute steps; not judged: 1 warm-up.
```

How to read it:

- **Within 20% in 15 of the 15 steps scored**: the count that matters. A
  step is scored when the planner gave a figure and the fleet measured
  enough requests to read the statistic (50 for the mean, 100 for a p90).
  The other steps, and why they were not scored, follow in the same line.
- **The mean leads**: the line quotes the mean, the histogram's sum over
  its count, which no bucket edge sets. The p90 follows it. A measured p90
  is interpolated inside the bucket that holds it, so it can sit anywhere
  in that bucket whatever the traffic did; it is placed only where the
  bucket is at most 1.2 times its lower edge, the 20% the verdict judges
  by. A router's buckets are often wider (TPOT 5 to 10 ms around a mean of
  7.5 ms). There the step shows the p90 with a `~`, and the bucket decides:
  a prediction more than 20% under or over the bucket's nearer edge is a
  certain miss, scored against that edge (which understates the miss) and
  never within; a prediction nearer than that, or inside it, could be
  within and is counted `unresolvable`, not as a miss. Of those, the steps
  whose prediction falls inside the measured bucket itself are counted
  apart, "in the measured bucket" (JSON `sameBucket`): the histogram cannot
  tell the two figures apart, so they are consistent at the bucket's
  resolution, which the words give with the bucket's width (TPOT 10 to
  20 ms is 2x wide). That is not within 20%, and never counts as within.
- **To measure a TPOT p90 or p95 the buckets cannot place**, give the
  histogram finer edges, and the verdict says so after the unscored steps.
  Edges about 15% apart across the TPOT range you care about place p90 and
  p95 within 20%. Either source works:
  - the router: `metrics.requestTPOTBuckets` (and `requestITLBuckets`) in
    the EndpointPickerConfig. It needs an llm-d-router build after v1.0.0
    ([llm-d-router#3539](https://github.com/llm-d/llm-d-router/pull/3539)),
    which also makes the default edges 5 ms apart from 5 to 50 ms.
  - vLLM: `--custom-histogram-buckets`, in vLLM v0.32.0 and later. See
    [vLLM's TPOT buckets](#vllms-tpot-buckets) below for the value.
- **Median ratio**: the planner's figure over the measured one, at the
  middle step. Above 1 it leans high at the middle; below 1 it leans low. A
  median near 1 can still hide steps it read low, so check that count too.
- **Read more than 20% low**: the steps where it said the fleet was faster
  than it was. These are the costly ones.
- **Gave no figure, calling the fleet overloaded**: steps whose rate was at
  or past what the planner thinks the fleet can carry. If the fleet served
  those minutes well, the planner is too cautious for your fleet.
- **Within the conditions our benchmarks cover**: how many figures fall
  where our own accuracy runs measured the planner. For a real fleet this is
  often none, and then this check, not the benchmark, is your evidence.
- **Not judged**: steps whose measurement reaches into the first minutes of
  the test or over a change of replicas, and steps with no traffic. Also
  steps whose measurement window runs both near the knee (a minute at 0.8
  of it or more) and far under it (a minute at a quarter of that or less):
  "a burst near the knee in its window". Their requests arrived at, or
  queued behind, a load the step's mean rate hides, so no steady-state
  figure at that mean is comparable. Neither TTFT nor TPOT is scored there.
  A load past the knee shorter than the measurement window is therefore
  never scored; the summary counts such steps.
- **TTFT not settled**: a step so near the prefill knee that the prefill
  queue does not settle inside its measurement window. Its relaxation time,
  2 rho S (Ca^2 + cv^2) / (N (1 - rho)^2) for a pod's service time S, N
  pods, the share of the knee rho, the arrivals' spread Ca^2 and the prompt
  spread cv priced, is over a fifth of the window. A queue's wait is
  correlated over about one relaxation time, so such a window holds fewer
  than five independent readings of it: its TTFT is a noisy sample of the
  steady state the planner prices, even where the load held for many steps
  (and, right after a change of load, the queue may still be on its way
  there). Its TTFT is not scored ("unsettled"); its TPOT is. The
  self-check's near-knee rule reads only the steps scored, and says so
  when steps it would have read did not settle.
- **TTFT an upper bound** (`<=` before the figure): a step priced from a
  V_P known only as a lower bound (flag `VPLowerBound`). A faster prefill
  only lowers the TTFT, so the true TTFT is at most the figure: a ceiling,
  not a reading, and a figure 1.2x or 1000x over what was measured is the
  bound holding. Where the fleet measured at most 20% over it, the
  step is counted `upperBound`, consistent, and not scored, so it moves no
  median, within share or self-check rule. Where the fleet measured more,
  the bound broke, and the step is scored like any other, read low: a
  figure under what was measured is never hidden. `--assume-vp` states a
  V_P and scores the TTFT as a figure again.
- **A backlog carried over**: where a step ran past the prefill knee, what
  it could not serve is still queued at the next step (a fluid queue: it
  grows by the rate less the knee over the step, and drains at the knee
  less the rate). The next steps' TTFT predictions add the mean wait of that
  backlog over their measurement window, and their bound the most; each
  such step shows it (`backlog`, `backlogWait`). A step with no requests
  drains the backlog at the knee last priced; a step the planner could not
  price (no knee, or a refusal) clears it, since nothing says what was
  served there.

Below that are each step (the mean and the p90 of each) and then the
scores: the mean, p50, p90 and p95 of TTFT and TPOT, then the same split by
load. The page draws the same steps over
time: the predicted mean and p90 as lines, the measured mean and p90 as
dots, a measured p90 in a bucket too wide to place it as a bar over that
bucket, steps it called overloaded marked in red at the top, and steps not
judged in grey.

The prediction uses each step's request rate, prompt and output lengths, the
spread of prompt and of output lengths, and the replicas each role ran. Unless you give
`--arrival-cv2` (1 is random arrivals, higher is burstier), it assumes random
arrivals. It is compared with the router's TTFT and TPOT histograms in the
same recording, over the window its histogram covers (the router's rate
window, five minutes on a default router); the inputs are read over the
same window. Without
router histograms it uses the engines' own, and leaves out what they cannot
compare: on a P/D fleet the engines' TTFT covers prefill alone.

### vLLM's TPOT buckets

To let the check score TPOT p90 and p95 from vLLM's own histograms, start
each model server with this flag (vLLM v0.32.0 and later):

```bash
--custom-histogram-buckets '{"inter_token_latency":[0.005,0.00575,0.0066,0.0076,0.0087,0.01,0.0115,0.0132,0.0152,0.0175,0.02,0.023,0.0265,0.0305,0.035,0.04,0.046,0.053,0.075,0.1,0.25,1.0]}'
```

- The flag takes JSON keyed by bucket family. The family is
  `inter_token_latency`. It sets the edges of two histograms:
  `vllm:inter_token_latency_seconds` and the per-request TPOT,
  `vllm:request_time_per_output_token_seconds`. There is no TPOT family of
  its own.
- The edges are 15% apart from 5 ms to 53 ms. Each bucket in that range is
  narrow enough to place a percentile within 20%. Above 53 ms the buckets
  are wide again. If your TPOT runs higher, carry the same 15% steps further
  up.
- Families you do not list keep vLLM's defaults. A family name vLLM does
  not know is an error at startup, so check the spelling.
- Finer edges add series: this list has 22 edges where the default has 19,
  so each of the two histograms adds 3 series per model server.
- The edges must be positive and strictly increasing.
- It changes only the histograms. Restart the servers before the window you
  record.

## The self-check beside every answer

You do not have to run the check yourself. Every answer the planner reads
from a recording checks the planner on that recording too: it learns from
the first half of the recording and the first three quarters, predicts the
quarter after each from its traffic and replicas alone, and compares with
what the fleet measured. The result comes first, in one line:

```text
Self-check: tested on the last 1 h 13 min of this recording, each part predicted from a fit on what came before it,
mean TTFT within 20% in 8 of 11 steps at 5 load levels (median ratio 1.18); mean TPOT within 20% in 12 of 12 steps at 5 load levels (median ratio 0.99).
It tested 0.24 to 4.13 r/s at 2 prefill + 2 decode: an answer at another rate or fleet is not covered by it. [weak]
  why: few or loose agreements: mean TTFT: within 20% in 73% of 11 steps; the within share could be as low as 45% over the load levels
```

Each figure is judged at the first of its mean, p90 and p50 scored in 6
steps or more; with none scored in 6, at the one scored in the most steps
(on a tie, in that order). The backtest's own verdict quotes the mean
wherever it scored one.

An answer given stated inputs is checked with them. With
`--assume-vp`, `--assume-a`, `--assume-prompt-cv`, `--assume-router-hop`,
`--assume-itl-b`, `--arrival-cv2`, `--flow-control`, `--router-wait-in-prefill`
or `--price-router-wait`, the recording is replayed with the same values, and
the line names them:

```text
Self-check, replayed with the stated --assume-prompt-cv 0.06 (flagged as stated): tested on ...
```

A stated V_P, a, router hop, ITL B or router wait also puts the figures outside the
validated region, and the line says so. A stated TPOT ratio
(`--assume-tpot-ratio`), stated lengths (`--assume-input-tokens`,
`--assume-output-tokens`), a stated KV transfer base
(`--assume-transfer-base`, on `replicas-to-slo` and `slo-to-rate` only:
`at-replicas` refuses it) and the other opt-in switches (`--batch-cap`,
`--ps-onsets`, `--prefill-running-cap`, `--charge-decode-gate`) are not
replayed: the check scores p90 TPOT on the recording's own ratio, the
recording's own traffic, and the switches' defaults. The line ends
`It ran without the stated ...` to say so. Where a stated ITL B is not used
at the fits (the line is held there), the line says in how many steps. On the
server, a report or day plan that states inputs carries the source's
self-check, which ran before anything was stated, with the same ending.

On the page it is a badge beside the source, with the same lines under it.

| Badge | Means |
| --- | --- |
| held on its own steps (`ok` in JSON) | The planner predicted this recording's own steps well: 12 steps or more at 4 load levels or more, 70% of them within 20%, a median ratio from 0.95 to 1.25, and the within share still at least half when the levels are resampled. It tested only the rates and replica counts the recording ran: it says nothing of other fleets. |
| weak | It held, but over fewer than 12 steps or 4 load levels, with fewer than 70% of the steps within 20%, leaning low (a median ratio from 0.80 to 0.95), or with a within share that could be under half once the levels are resampled. Size with care. |
| inconsistent | The planner and this recording disagree. The line says which figures not to trust, and why: the median ratio is outside 0.80 to 1.25; fewer than half the steps are within 20%; more than 15% of the steps read more than 20% low, or any one read under 0.67 of measured; on a figure judged at its mean, the p90 read more than 20% low in half or more of its scored steps (6 or more); at 0.8 to 1.0 of the knee the worst tenth of the steps (3 or more) read outside 0.67 to 2 times measured; the planner called the fleet overloaded in half the steps where it served; or it called the fleet overloaded in steps where the fleet's measured p90 TTFT stayed within 1.5x of the median of its steps below 0.8 of the knee, so its queue did not grow (2 or more, and a fifth of those steps): the knee reads low. |
| not run | The recording is shorter than an hour, carries no latency, or is not a P/D fleet; fewer than 6 steps could be scored for each figure (the refusals are named); or, on the server, it ran out of its time budget. The line says which. |

The steps are not independent: a load level holds for several steps, and
both cuts come from one recording. The line counts the load levels (a run of
steps at one fleet and a rate within 15% of the level's first; one step off a
level the run returns to stays in it), and the
within share's bound resamples whole levels.

The line ends with what the check tested: `It tested 2.10 to 9.84 r/s at 2
prefill + 2 decode`. An `at-replicas` verdict at another rate, fleet or
shape says `not covered by the self-check`. A verdict that rests on the
figures an inconsistent self-check names says `not trusted`, and the command
exits 4 (`verdict.trust` and `exitCode` in the JSON); the verdict is still
printed. `day-plan` judges each bucket's verdict the same way, at the
bucket's sized rate on the fleet it had, or its stated shape: `not trusted`
or `not covered by the self-check` beside the hour (`buckets[].trust`, `hours[].trust`, `summary.counts.untrusted` and
`notCovered` in the JSON), and it exits 4 when a bucket is not trusted.

A recording whose engine rows do not stand for the fleet gets no badge: the
planner refuses it outright, as an input error. That happens on multi-Pod
groups where only one rank's engine is scraped. See
[troubleshooting](troubleshooting.md#a-recording-is-refused-engine-rows-do-not-stand-for-the-fleet).

On **inconsistent**, the answer offers an issue to file: a link to a new
GitHub issue, prefilled with diagnostics. It holds the planner's version,
the checks that failed and their numbers, the refusals, the token-size
class of the traffic, the fleet's roles and replica counts, flow control on
or off, the engine and its release, and the number of folds and the span
they tested. Its numbers are ratios, step counts and the latencies the fleet
measured. It holds no model, namespace, variant or pod name, no request rate,
and no recording. Nothing is sent until you read it and submit it.

What to do on inconsistent: do not size the figures it names from this
recording; record a longer window with busy minutes early in it, then check
again; if it stays inconsistent, file the issue.

The fits at the cuts are read in the same replay that reads the answer's
state; on the CLI the self-check adds one replay of the recording's traffic
and latency, which it scores against. Measured with `at-replicas`, on and
off: 38.6 s against 35.0 s on the 2.4-hour q32 warm-up, and 245 s against
256 s on a 1.7-hour 0.6B staircase recording (run-to-run noise). `--no-self-check` (and
`selfCheck: false` on `POST /v1/inputs`, or `serve --no-self-check`) turns it
off.

## Check it blind: predict now, compare later

To show that the planner never saw the latency it is scored against, split
the check in two. You share only the load of a period, the planner predicts
it, and you keep the prediction. Later you compare it with what the fleet
measured.

1. Take the latency out of a recording of the period to predict. What is
   left is each cycle's load (request rates, prompt and output lengths,
   replica counts, the engines' settings) and the router's rate window;
   every analyzer output, which holds figures learned from latency, is
   dropped.

   ```bash
   capacity-planner strip-latency -in day2.jsonl.gz -o day2-load.jsonl.gz
   ```

2. Predict it from a fit recording that ends before the period starts:

   ```bash
   capacity-planner predict --fit day1.jsonl.gz --window day2-load.jsonl.gz -o prediction.json --csv prediction.csv
   ```

   The file holds every step's predicted TTFT and TPOT (mean, p50, p90,
   p95), the planner's version, the time it was made, a hash of the inputs
   it was priced from, and a hash of the file itself. Given a window that
   still carries its latency, `predict` says so and does not read it: the
   steps and the inputs hash are the same.

3. Later, compare it with the period measured:

   ```bash
   capacity-planner compare --prediction prediction.json --measured day2.jsonl.gz
   ```

   It prints the same answer as `backtest`. The measured recording may be
   longer than the period: only the period is compared. A prediction file
   edited after it was made and not rehashed, or a recording of another
   period or another load, is refused. The hash shows an edit; it is not a
   signature.

On the page: **Blind: predict now, compare later**, under the check.

## Is it good enough?

Size from the planner when, below the load it thinks the fleet can carry:

- the mean TTFT and TPOT are within 20% in most of the steps it gave a
  figure for, and the p90s too where the histogram could place them;
- the median ratio is between 0.95 and 1.25 (leaning a little high
  costs replicas, leaning low costs the SLO);
- it read few steps more than 20% low (15% or fewer), and none under 0.67;
- it gave no figure only where the fleet really was overloaded.

## If the result is bad

- **No figure in any step** (`refused: NoPrefillCapacity`, `PinnedDefaultB`,
  `NoFixedLatency`): the part it learned from did not hold the busy minutes
  it learns from. Give it more: a larger `--split`, or a longer recording.
  [Troubleshooting](troubleshooting.md) has each refusal.
- **Refused: the test ran another configuration**: the two parts differ in
  something the recording carries, such as the accelerator, the
  parallelism, an engine flag, the image or the flow-control series. A
  score across configurations measures the change, not the planner. Test on
  a period of the same configuration. `--allow-config-mismatch` scores it
  anyway and prints the difference.
- **Reads high near the knee**: expected. Close to the load it thinks the
  fleet can carry, the planner's tail rose faster than the fleets' on every
  run we measured, so it leans high there. Judge it by the steps below 0.8 of that load (the
  by-load lines).
- **Reads high everywhere**: check the `assumed:` lines. An assumed prompt
  spread or arrival pattern can move every figure.
- **Within 20% is low but the median ratio is near 1**: its errors are wide
  both ways. Do not size from single steps.
- **Reads low**: open an issue with the recording (or the output of
  `-o json`). That is the side that costs an SLO.

## Reference

| Flag | Default | Notes |
| --- | --- | --- |
| `--recording` with `--split` or `--fit-until` | `--split 60%` | One recording, cut in two. |
| `--fit`, `--test` | | Two recordings. The fit must end before the test starts, unless they are split by load. |
| `--fit-below`, `--test-above` | | Split by load: fit on the steps whose rate is below the first, judge the steps at or above the second. A rate (`12.5`) or a percentile of the steps' rates (`p50`). Give both. Not with `--split` or `--fit-until`. |
| `--step` | `5m` | A step, from `1m` to `1h`. A step shorter than the router's five-minute window shares requests with its neighbours. |
| `--settle` | `2m` | How long after a change of replicas, and after the test starts, a measurement is not judged. |
| `--arrival-cv2` | random | How bursty arrivals are, when you know it: 1 is random. |
| `--ttft-target` | | Judge each step against a target too: `500ms@p90`. The summary counts the steps predicted to meet it that measured a miss. Where the measured percentile fell in a bucket too wide to place it, the bucket judges: met at or over its upper edge, missed at or under its lower, and not judged on the measured side for a target inside it. |
| `--measured` | `auto` | `router`, `engine`, or `auto` (the router's where recorded). |
| `--assume-vp`, `--assume-a` | | A prefill pod's prompt tokens per second, and its fixed latency, where the part it learned from held none. Flagged. |
| `--assume-prompt-cv` | each step's | The prompts' spread, 0 to 3, in place of the one each step reads from the histogram, which is coarse on long prompts. Flagged. |
| `--assume-router-hop`, `--assume-itl-b` | | The router hop, and decode's ITL B (`12ms`, or `floor`), as an answer's flags of the same names: check an answer with the inputs it stated. Flagged. |
| `--allow-config-mismatch` | off | Score a test of another configuration, with the difference printed. |
| `--flow-control` | inferred | `on` or `off`: the router's flow control, in place of what the fit's queue series says. To score a test that ran with flow control on against a fit that ran with it off, add `--allow-config-mismatch` too. |
| `--router-wait-in-prefill`, `--price-router-wait` | on, off | How a step past the prefill flow-control onset carries the router's wait: `--router-wait-in-prefill` (on by default) for router v1.0.0, `--price-router-wait` for v0.11.0, which turns the default off. With `--router-wait-in-prefill=false` and no `--price-router-wait`, the step is flagged `AboveFlowControlOnset`; use that on router v0.11.0. Set both and they are refused. |
| `--load-aware-router` | off | State that the router balances prefill by load (llm-d's EPP queue scorer), as an answer's flag of the same name: f is held up to the load it was learned at, then falls to 1 at the pooled prefill knee. The API's `assume.loadAwareRouter`; every figure reads outside the validated region with it. `predict` takes it too. |
| `-o json` | text | The same answer as `POST /v1/backtest` ([the API](api.md#the-backtest)). |

The command exits 0 when it judged at least one step, 3 when it judged none,
and 2 on a usage error or a refused configuration.
