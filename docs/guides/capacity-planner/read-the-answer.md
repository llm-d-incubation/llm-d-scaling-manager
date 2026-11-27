# Read the planner's answer

This page walks one real answer, the one the [quick start](README.md) asks
for, part by part:

```bash
bin/capacity-planner at-replicas --recording recordings/ttft-staircase-b-on.jsonl.gz
```

The sample is 72 minutes of a test fleet of two prefill and two decode
replicas, under load that rises in steps. Every output below was printed by
the planner at v0.12.3 on that sample; only the annotations are added. The
page's **Judge the fleet** answers with the same report, in panels.

## On the page: the answer first

The page opens on the **Answer** tab. Its top panel is the answer in a few
lines, small enough to paste on a slide (**Copy as image**). Each line has a
badge that says how far to trust it:

| Badge | Means |
| --- | --- |
| Measured | Read off the recording: the fleet, its busiest load, the latency it measured. |
| Checked on your data | The planner predicted held-out hours of your own traffic, and this is how close it came. |
| Estimate | The model's figure: sizing, headroom, the verdict. |

The lines, top down:

- **Fleet today**: the replicas and GPUs the source ran, its busiest two
  minutes and its peak.
- **Verdict**: whether the fleet saturates at its busiest load, and the TTFT
  and TPOT it measured at the busiest held-out step against your targets.
- **Right size**: the fleet judged at the traffic's own shape. Where prefill
  was not sized it says so: "Decode only: 4-6 decode would hold TPOT.
  Prefill was not sized, so this is not a fleet recommendation."
- **Headroom**: how far the load can grow before each role's limit. "at
  least" marks a limit known only as a lower bound.
- **+25% / +50% traffic**: the fleet the busiest load grown by a quarter
  and by a half needs, at the same SLO and shape, as "N prefill + M decode
  (G GPUs)". The page sizes each with `POST /v1/size` once the busiest load
  is judged. A size past the highest load measured says "extrapolated past
  the tested range"; one resting on a prefill limit known only as a lower
  bound says fewer prefill may do. Where no fleet serves the rate the row
  says why. Under the default model, where prefill's limit is not measured,
  the row says it cannot size more traffic and what to do: load a busier
  window, or use the speed the pods were seen at. A source with no prefill
  variant is not sized: the page sizes prefill and decode together.
  Change the SLO targets, their percentiles or the shape and the page asks
  the verdict and these sizes again; until the answers arrive those rows
  read "Judging..." or "Sizing...", never the answer for the old targets.
- **Accuracy**: how many held-out five-minute steps the planner predicted
  within 20%, which way it leans, and the rates and fleet that check
  covers. Outside that range an answer is an extrapolation. The page runs
  no check when it loads: until you run one, the row shows the server's
  quick check or self-check where there is one, else "Not checked yet",
  with a **Check accuracy on this recording** button that runs it.
- **SLO used**: the targets, and whether they are the page's defaults.

The chip at the top, "Checked on your traffic: TTFT 130/144 within 20%",
opens **Check the planner**, which holds the evidence. Before a check it
quotes the server's quick check where there is one ("Quick check on your
traffic: ..."), reads "Self-check only: full accuracy check not run yet"
where the server's self-check filled the Accuracy row, and "Accuracy not
checked yet" otherwise.

### When prefill capacity is unknown

If the prefill pods never queued, the recording shows only that they handle
at least the speed they ran at, not their limit. The page then shows one
card instead of a refused figure:

> **Prefill capacity unknown.** The prefill pods never queued, so they handle
> at least 18,000 tokens/s per pod, but their limit is not observed. Decode
> alone serves up to 40 r/s.

It has two buttons:

- **Use 18,000 tokens/s as the limit (conservative)** prices prefill at the
  speed seen. Every figure on it is flagged as assumed, and the real limit
  can only be higher.
- **Load a busier window** opens the source. Pick a window where prefill ran
  busy enough to queue.

The code (`NoPrefillCapacity`) sits under the card in small type, and in the
Flags drawer.

### One notice a view

How far to trust the figures is said once a view, never in every cell. A
figure priced by an experimental model carries a dagger (†), and the view's
notice says what it means: "Experimental model (checked on your data)". Which
figures fall outside the validated region is in the **Flags** drawer. The
TTFT model's validation figures are under **About the TTFT model**, below the
source.

### The other tabs

- **Day plan**: a chart of the replicas each bucket needed against the
  replicas the fleet ran, hours that missed the SLO in red, and "Peak needed
  N at HH:MM" above it. The hourly table is closed below. Where prefill was
  not sized, a spare decode replica is "decode only", never waste.
- **Check the planner**: a three-line verdict card (TTFT and TPOT within 20%,
  and the range tested), then the charts. A measured dot is coloured by how
  it stands against the prediction: within 20%, more than 20% over, or more
  than 20% under. A figure the histogram cannot place, at many steps, is one
  grey band. The scores, every step and the method are closed below. In the
  step table a cell reads "2.10 vs 2.20 s ✓ 5%": predicted against measured,
  within 20%, 3% apart. **Failing steps only** keeps the steps more than 20%
  off.
- **Right-size**: the fleet judged at the traffic's own shape, with your
  shapes and loads beside it, as described below for the CLI.

Every result card has **Copy link**, **Download CSV** and **Download JSON**.

### A figure the histogram cannot place

A percentile is read from the measured histogram. When one bucket is too
wide (10 ms to 20 ms is 2x wide), the measured p90 is somewhere in it, and
the check cannot score a prediction against it. The Check tab draws that
bucket as one band and says so under the charts. Finer edges measure it:
the router's `metrics.requestTPOTBuckets` in the EndpointPickerConfig, or
vLLM's `--custom-histogram-buckets` ([the value](check.md#vllms-tpot-buckets)). Edges about 15% apart place p90 and p95
within 20%.

## The answer, from the CLI

### 1. The self-check: predicted against measured

```text
Self-check not run: too few steps were scored: a longer recording, with busy minutes in its first half, gives the planner more to fit and to check (TTFT refused NoPrefillCapacity in 7 steps; TPOT refused PinnedDefaultB in 7 steps). [notRun]
```

Before it answers, the planner tests itself on your recording. It learns
from the first part, predicts the TTFT and TPOT of the rest from its
traffic and replica counts alone, and compares each five-minute step with
what the fleet measured.

On this sample it scores no step:

- `TTFT refused NoPrefillCapacity in 7 steps`: the sample learned no
  prefill speed, so the planner predicts no TTFT for it.
- `TPOT refused PinnedDefaultB in 7 steps`: no window spread the decode
  Pods' KV fill far enough to fit decode's latency line, so the planner
  predicts no TPOT from a fit either
  ([when a term is refused](page-and-commands.md#when-a-term-is-refused)).
- `[notRun]`: the grade, here no evidence either way. A recording with
  busy minutes in its first half, where prefill's speed and decode's line
  are both learned, gets
  `ok`, `weak` or `inconsistent`, and the rates and fleet it tested. See
  [Trust levels](#trust-levels-ok-weak-inconsistent).

### 2. The fleet and the load

```text
ns-01/model-02  P/D 2 prefill (variant-02) + 2 decode (variant-01)  window 2026-11-06 02:18-03:30Z, cycle 289 of 289
decode engine   vLLM release not recorded
prefill engine  vLLM release not recorded
load judged     41.70 r/s, the p95 of the 282 of 289 cycles with traffic
load seen       per cycle (1-minute rates): mean 31.59, p95 41.70, peak 43.24 r/s; p95 less counting noise 41.66; busiest 2m0s (8 cycles): 42.13 r/s
SLO             TTFT p95 190.0 ms (policy)
```

- The model, its fleet, and the window the recording covers.
- `load judged`: the fleet is judged at the p95 of the request rate, its
  busy spells, not at the mean. The mean is 31.6 r/s and the peak 43.2.
- `SLO`: the targets, here the policy's. `--ttft 2s@p90 --tpot 30ms` sets
  your own.

### 3. The summary and the verdict

```text
summary     50%              80%              100%            seen
  measured  -/3.2m @18.49 ?  -/6.8m @29.59 ?  -/- @36.98 SAT  -/- @41.70 SAT
verdicts
  measured  UNDER-PROVISIONED (decode only): at the p95 load of 41.7 r/s, 1.1x the fleet knee of 37.0 r/s, the fleet saturates; needs at least 3 decode (has 2), 6 decode if the decode knee reads 99% high.
```

- **Summary.** One column per load: 50%, 80% and 100% of the knee, and the
  load seen. A cell is TTFT / TPOT in ms at that rate, then a mark: `ok`
  every target met, `MISS` missed, `SAT` saturated, `?` not judged. `-`
  is a figure the planner refused (no prefill speed, so no TTFT); `m` marks
  a TPOT mean where the percentile is refused.
- **Verdict.** One sentence: what the fleet is, why, and the fleet it
  needs. Here: the busy load, 41.7 r/s, is 1.1 times the knee, so the fleet
  saturates. [Verdicts](#verdicts-under-over-right-sized-not-judged) lists
  the four words.
- `needs at least 3 decode (has 2)`: see ["At least"](#at-least-and-lower-bounds).
- `6 decode if the decode knee reads 99% high`: the same need if the knee
  is as far off as the worst error measured outside the validated region
  ([below](#outside-validated-region)).

### 4. The shape and how busy each role is

```text
shape measured: IL 8003 OL 200, prefix hit 0.00 (measured, cycles 269-288)
  the load's cycles (the p95 and above: 15, cycles 247-283): IL 8001 OL 200, prefix hit 0.00
utilization at the load judged
  decode        KV fill saturated (band top 0.8), slots saturated of 222, wait p saturated
  prefill       utilization pooled no prefill speed learned, worst pod no prefill speed learned
```

- `IL 8003 OL 200`: the request shape priced, 8003 prompt tokens in and 200
  out, read from the last cycles. The busy cycles ran the same shape. When
  they differ by more than 20%, the answer says `SHAPE/LOAD MISMATCH`.
- Decode at the load: its KV cache, its batch slots and the chance a
  request waits are all past saturation. Prefill is not priced.

### 5. The knee: how much the fleet carries

```text
capacity            req/s     out tok/s   in system   bound by
  prefill knee      no prefill speed learned                        P1 refused V_P of variant-02: no prefill speed learned; pass --assume-vp for a flagged answer
  decode knee       36.98     7396        unbounded   decode band top at fill 0.798
                    OUTSIDE VALIDATED REGION (ExtrapolatedFill: an input the run did not price)
  fleet knee        36.98     7396        unbounded   decode (prefill not priced)
  operating (0.85)  31.44     6287        -           0.85 x fleet knee (scaleUpThreshold)
  max at SLO        no prefill speed learned                        no fleet rate: the prefill side is not priced, and decode alone does not bound the fleet (slo-to-rate gives none either): P1 refused V_P of variant-02: no prefill speed learned; pass --assume-vp for a flagged answer
```

The **knee** is the rate at which a role saturates: past it, queues grow
without bound and latency with them. Each role has one; the fleet's is the
lower of the two, and `bound by` says what sets it. Here decode saturates
at 36.98 r/s, when its KV cache fills to 0.8.

- `operating (0.85)`: the operating point, the knee times the policy's
  headroom (its `scaleUpThreshold`). A fleet run above it has no room for a
  burst.
- `max at SLO`: the highest rate that still meets every target. It needs
  TTFT, so it is refused here.
- `in system`: how many requests are in the system on average at that
  rate, see [the SLO against load](#6-the-slo-against-load-and-the-checks).
  At a knee the queue grows without bound, so there is no average:
  `unbounded`. At a knee that is only a lower bound (`>=`) it reads
  `not known`: the queue grows without bound only past the true knee,
  which may be higher.
- `prefill knee ... refused`: this sample has no measurement of prefill
  speed (V_P), so prefill is not priced. `--assume-vp` states one, and
  every figure on it is flagged as assumed.

### 6. The SLO against load, and the checks

```text
SLO vs load         r/s    TTFT p50/90 ms            TPOT mean/p90 ms           e2e mean (sum)            in system (mean)
  50% of knee       18.49  no prefill speed learned  3.15/TPOT bucket too wide  no prefill speed learned  -
  80% of knee       29.59  no prefill speed learned  6.81/TPOT bucket too wide  no prefill speed learned  -
  100% of knee      36.98  no prefill speed learned  saturated                  no prefill speed learned  unbounded
  load seen (113%)  41.70  no prefill speed learned  saturated                  no prefill speed learned  unbounded
checks
  2 prefill + 2 decode  does not hold: 41.70 r/s is 1.1x the fleet knee of 36.98 r/s; prefill not priced (no prefill speed learned); TTFT p95 not judged (no prefill speed learned)
```

The latency at each load, and each fleet the planner tried. `TPOT bucket
too wide`: the router's histogram buckets are too coarse to read a p90,
so only the mean is given.

`in system (mean)` is the load as a concurrency: how many requests are in
the system on average at that rate, from arrival to the last token. It is
the rate times `e2e mean (sum)` (Little's law), so it is an average, not a
peak. It needs the mean TTFT and the mean TPOT: here TTFT is not priced, so
it reads `-`. At or past the knee it reads `unbounded`: the queue grows
there, and no average holds. In `-o json` each row's `concurrency` also
splits it by where the requests are: `prefill` (in the prefill engine:
waiting there or computing their prompt), `routerQueue` (held at the
router's flow-control gate, when that gate is priced), `decode` (the rate
times OL times the mean TPOT, the sequences decoding at once) and
`decodePerPod`. The fleet figure is more than their sum: it also holds
the fixed latency, the hop and the KV pull.

To check `prefill` against a running fleet, do not sum vLLM's
`num_requests_running` and `num_requests_waiting` gauges on the prefill
pods: vLLM sets them after each step has dropped the requests it
finished, so a prompt prefilled in one step never shows in them, and they
read low. Use the pods' own histograms instead: the request rate times
the mean queue time plus the mean prefill time.

If you drive load at a fixed concurrency, ask for it as a load:
`--load 64c` is the rate at which the fleet holds 64 requests on average.

### 7. Flags and what is not validated

```text
flags: DecodeOnly; ExtrapolatedFill; BelowFittedBand; CoarseTPOTBucket; HostBoundTPOTTail

not validated:
  - decode knee: outside its validated region (see its label); inside the region the q32 staircase measured (1000-token prompts, 1000 out, Ca^2 1) it read at most +3%, and outside it, on the 0.6B goldens, 43% to 99% high (docs/proposals/slo-capacity-planner.md, Measured on the goldens)
  - e2e mean is a sum, TTFT mean + (OL - 1) x TPOT mean, not a percentile; in system is the rate times it (Little's law), the requests in the system on average
```

Every flag names something an answer rests on that was assumed, stretched
or read loosely. [Troubleshooting](troubleshooting.md#refused-figures) and
the page's **Flags** drawer say what each means and what clears it. `not
validated` lists the figures no measured run has checked.

The command exits 0: it answered. It exits 3 when a verdict is NOT JUDGED,
and 4 when the self-check found a verdict's figures inconsistent.

## Verdicts: UNDER, OVER, RIGHT-SIZED, NOT JUDGED

The same sample, with a prefill speed and targets stated, at three fleets.
`--assume-vp 200000 --assume-a 20ms` states prefill's speed and fixed
latency; `--ttft 2s@p90 --tpot 30ms` sets targets the sample can meet:

```bash
bin/capacity-planner at-replicas --recording recordings/ttft-staircase-b-on.jsonl.gz \
  --assume-vp 200000 --assume-a 20ms --ttft 2s@p90 --tpot 30ms --prefill 2 --decode 2
```

```text
  measured  UNDER-PROVISIONED: at the p95 load of 41.7 r/s, 1.1x the fleet knee of 37.0 r/s, the fleet saturates; needs 3 prefill + 3 decode (has 2 + 2), 6 decode if the decode knee reads 99% high; the same at f 1 to 1.5 (prefill f 1 assumed at 3 pods).
```

With `--prefill 3 --decode 3`:

```text
  measured  RIGHT-SIZED: at the p95 load of 41.7 r/s, the fleet holds every target with headroom; decode: no spare (11.6% under its operating point); prefill: no spare (34.5% under its operating point) (prefill f 1 assumed at 3 pods).
```

With `--prefill 4 --decode 4`:

```text
  measured  OVER-PROVISIONED: at the p95 load of 41.7 r/s, decode: one fewer holds even at the peak 43.2 r/s; prefill: one fewer holds even at the peak 43.2 r/s; needs 3 prefill + 3 decode (has 4 + 4), 6 decode if the decode knee reads 99% high; at f 1 needs 3 + 3, at f 1.5 needs 4 + 3 (decode knee outside its validated region; prefill f 1 assumed at 4 pods; prefill f 1 assumed at 3 pods).
```

From the release after v0.12.3, decode reads `no spare: 6 decode if the
decode knee reads 99% high` here and the fleet needed is 3 prefill + 4
decode: a decode cut must also hold if the decode knee reads as high as its
label allows ([outside validated region](#outside-validated-region)).

And with no prefill speed: the first command, with `--decode 3`:

```text
  measured  NOT JUDGED: at the p95 load of 41.7 r/s, TTFT p95 cannot be priced (the recording learned no prefill speed; --assume-vp states one).
```

| Verdict | Means | What to do |
| --- | --- | --- |
| UNDER-PROVISIONED | The busy load runs past headroom times the knee, or misses a target. | Scale to the fleet it `needs`. Size each role's `maxReplicaCount` at least that high. |
| RIGHT-SIZED | Every target holds with headroom, and no role has a replica to spare. | Nothing. `no spare (11.6% under its operating point)` says how close each role is. |
| OVER-PROVISIONED | A role has a replica to spare: one fewer still holds every target at the busy load, and at the peak. | Remove the spare, or lower `minReplicaCount`. It never names a cut that fails at the peak, nor a decode cut below the count the decode knee's error bound needs. |
| NOT JUDGED | A figure the verdict needs is refused, the load and the shape priced are different traffic, or the window contradicts the model. The sentence says which. | Do what it names: `--assume-vp` for a missing prefill speed, `--shape load` for a shape mismatch, or a longer recording. |

The first three verdicts above rest on the stated prefill speed, which this
sample's self-check finds wrong for TTFT: each also prints `self-check: not
trusted` under it, and the command exits 4
([trust levels](#trust-levels-ok-weak-inconsistent)).

`f` is how unevenly the router spreads prompts over the prefill pods; with
no run to learn it at a pod count, the planner gives the need at both ends
of the range. [Check a fleet at N replicas](page-and-commands.md#check-a-fleet-at-n-replicas)
gives every rule.

## "At least" and lower bounds

`needs at least 3 decode (has 2)` and `(decode only)` mean prefill was not
priced, so the verdict rests on decode alone. The fleet needs at least
that; prefill could need more. Such a verdict is never RIGHT-SIZED or
OVER-PROVISIONED, which is why the same sample at `--decode 3` is NOT
JUDGED. To get a full answer, state a prefill speed with `--assume-vp`, or
record a window where prefill ran busy enough to learn it.

The day plan writes the same as `>=` in its tables. A prefill speed learned
on a fleet that never went quiet is itself a lower bound, and every TTFT
answer on it says so ([when a term is refused](page-and-commands.md#when-a-term-is-refused)).

## Trust levels: ok, weak, inconsistent

The self-check's grade, in brackets at the end of its line and as a badge
on the page:

| Grade | Means | What to do |
| --- | --- | --- |
| `ok` (held on its own steps) | 12 steps or more at 4 load levels or more, 70% within 20% of measured, a median ratio from 0.95 to 1.25. | Size from it, at the rates and fleet it tested. |
| `weak` | It held, but on fewer steps or load levels, fewer within 20%, or leaning low (median ratio 0.80 to 0.95). | Size with care. A longer recording with more load levels firms it up. |
| `inconsistent` | The planner and the recording disagree. The line says which figures not to trust. | Do not size from those figures. Record a longer window with busy minutes early, and check again. |
| `notRun` | The recording is under an hour, or holds too few steps to score. This sample, as the quick start runs it. | No evidence either way. |

On the sample with a stated prefill speed and fixed latency, the check
scores TTFT, and the stated speed does not match this fleet. TPOT is still
not scored: decode's latency line has no full fit.

```text
Self-check, replayed with the stated --assume-vp 200000, --assume-a 20ms (flagged as stated, outside the validated region): tested on the last 36 min of this recording, each part predicted from a fit on what came before it, p90 TTFT within 20% in 0 of 6 steps at 2 load levels (median ratio 2.40). It tested 35.76 to 41.55 r/s at 2 prefill + 2 decode: an answer at another rate or fleet is not covered by it. [inconsistent]
  why: the planner and this recording disagree: p90 TTFT: median ratio 2.40, want 0.80 to 1.25; within 20% in 0% of the steps, want 50% or more
  Do not trust prefill sizing and the TTFT figures from this recording.
```

Every verdict that rests on the TTFT figures then carries:

```text
            self-check: not trusted: the self-check found this recording's TTFT figures inconsistent with it, and this verdict rests on them.
```

and the command exits 4. The verdict is still printed. An inconsistent
answer also prints a link to file an issue with its diagnostics, which
hold no names and no recording. [The self-check](check.md#the-self-check-beside-every-answer)
has every rule; [Check the planner](check.md) runs a longer check yourself.

## OUTSIDE VALIDATED REGION

```text
  decode knee       36.98     7396        decode band top at fill 0.798
                    OUTSIDE VALIDATED REGION (ExtrapolatedFill: an input the run did not price)
```

Each figure says whether a measured run covered conditions like yours.
Inside one, it carries `validated in-sample: error ...` with the error
measured there. Outside, it says `OUTSIDE VALIDATED REGION` and, in
brackets, the first condition that differs: here, the recording did not
measure how bursty arrivals were, so Poisson arrivals are assumed.

It does not mean the figure is wrong. It means no run has checked it. The
`not validated` lines give the error seen elsewhere (the decode knee read
43% to 99% high outside the region), and the verdict gives the need at that
error beside its point answer (`6 decode if the decode knee reads 99%
high`). Size between the two by how much a miss costs you.
[How accurate each figure is](accuracy.md) has what was measured.
