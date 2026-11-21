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

On the page: load the recording in Source, open **Check the planner against
a period it did not see**, set the slider, and press **Run**. To test on
another recording, pick **another recording** and choose it.

## Read the answer

The first lines are the answer. For example:

```text
Over the 1 h 52 min the planner did not see:
p90 TTFT: within 20% of measured in 11 of the 15 five-minute steps scored (a figure, and enough measured requests);
it leans high (median ratio 1.09). For 7 more it gave no figure, calling the fleet overloaded; the fleet measured
p90 358 ms to 374 ms there.
p90 TPOT: within 20% of measured in 2 of the 15 five-minute steps scored (a figure, and enough measured requests);
its errors are wide; it leans high (median ratio 1.24); it read 5 more than 20% low. For 7 more it gave no figure, ...
None of these figures falls within the conditions our own accuracy benchmarks cover
(docs/guides/capacity-planner/accuracy.md): this check is the evidence for this fleet.
Judged 22 of 23 five-minute steps; not judged: 1 warm-up.
```

How to read it:

- **Within 20% in 11 of the 15 steps scored**: the count that matters. A
  step is scored when the planner gave a figure and the fleet measured
  enough requests to read the statistic (100 for a p90). The other steps,
  and why they were not scored, follow in the same line.
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

Below that are each step and then the scores: the mean, p50, p90 and p95 of
TTFT and TPOT, then the same split by load. The page draws the same steps over
time: predicted lines, measured dots, steps it called overloaded marked in
red at the top, and steps not judged in grey.

The prediction uses each step's request rate, prompt and output lengths, the
spread of prompt lengths, and the replicas each role ran. Unless you give
`--arrival-cv2` (1 is random arrivals, higher is burstier), it assumes random
arrivals. It is compared with the router's TTFT and TPOT histograms in the
same recording, over the window its histogram covers (the router's rate
window, five minutes on a default router); the inputs are read over the
same window. Without
router histograms it uses the engines' own, and leaves out what they cannot
compare: on a P/D fleet the engines' TTFT covers prefill alone.

## The self-check beside every answer

You do not have to run the check yourself. Every answer the planner reads
from a recording checks the planner on that recording too: it learns from
the first half of the recording and the first three quarters, predicts the
quarter after each from its traffic and replicas alone, and compares with
what the fleet measured. The result comes first, in one line:

```text
Self-check: tested on the last 1 h 13 min of this recording, each part predicted from a fit on what came before it,
p90 TTFT within 20% in 2 of 7 steps (median ratio 1.34); p90 TPOT within 20% in 8 of 8 steps (median ratio 0.94). [inconsistent]
  Do not trust prefill sizing and the TTFT figures from this recording; decode sizing and the TPOT figures (loosely) held.
```

On the page it is a badge beside the source, with the same lines under it.

| Badge | Means |
| --- | --- |
| ok | The planner predicted this recording well, over 12 steps or more with 70% of them within 20%: you can size from its figures. |
| weak | It held, but over fewer than 12 steps or with fewer than 70% of them within 20%. Size with care. |
| inconsistent | The planner and this recording disagree: the median ratio is outside 0.8 to 1.25, fewer than half the steps are within 20%, or the planner called the fleet overloaded in half the steps where it served. The line says which figures not to trust. |
| not run | The recording is shorter than an hour, carries no latency, or is not a P/D fleet; fewer than 6 steps could be scored for each figure (the refusals are named); or, on the server, it ran out of its time budget. The line says which. |

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

- the p90 TTFT is within 20% in most of the steps it gave a figure for;
- the median ratio is between about 0.95 and 1.2 (leaning a little high
  costs replicas, leaning low costs the SLO);
- it read few steps more than 20% low;
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
| `--fit`, `--test` | | Two recordings. The fit must end before the test starts. |
| `--step` | `5m` | A step, from `1m` to `1h`. A step shorter than the router's five-minute window shares requests with its neighbours. |
| `--settle` | `2m` | How long after a change of replicas, and after the test starts, a measurement is not judged. |
| `--arrival-cv2` | random | How bursty arrivals are, when you know it: 1 is random. |
| `--ttft-target` | | Judge each step against a target too: `500ms@p90`. The summary counts the steps predicted to meet it that measured a miss. |
| `--measured` | `auto` | `router`, `engine`, or `auto` (the router's where recorded). |
| `--assume-vp`, `--assume-a` | | A prefill pod's prompt tokens per second, and its fixed latency, where the part it learned from held none. Flagged. |
| `--allow-config-mismatch` | off | Score a test of another configuration, with the difference printed. |
| `-o json` | text | The same answer as `POST /v1/backtest` ([the API](api.md#the-backtest)). |

The command exits 0 when it judged at least one step, 3 when it judged none,
and 2 on a usage error or a refused configuration.
