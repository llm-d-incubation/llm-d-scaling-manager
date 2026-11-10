# The next TTFT staircase

**Status: proposed, not run.** Nothing below has touched a cluster. Every rate and
hour is sized from the first run's warm-up, so it is an estimate. The real ones come
from the fresh warm-up and are frozen before the first step.

Why a new run: the first one (M2) failed buckets A and B, and that failure chose the
decode term, so M2 cannot score the term
([as built, M2](../../proposals/slo-capacity-planner-ttft.md#as-built-m2)). Until a
fresh frozen run passes, every TTFT figure prints UNVALIDATED.

The tooling is in the tree: [how to run it](../../developer-guide/testing.md#the-ttft-staircase).
Everything this plan freezes is in
[`staircase_spec.yaml`](../../../hack/benchmark/staircase_spec.yaml).

## The run in one page

0. `make benchmark-staircase-smoke` in a namespace of its own: done, see below. Then
   `make benchmark-staircase-lapse`, about 1 GPU-hour, which has not been run. Repeat
   either if `staircase.sh` changes.
1. Stand up a fresh namespace with `slo` listed from the first cycle.
2. Warm up with a prompt sweep.
3. `make benchmark-staircase-freeze`: the prediction, the ladder and their hashes.
4. Repeat 1 of the gate buckets. The run scores it in the loop: a fail, or a phase that
   did not finish, stops the run and frees the GPUs, since a later repeat cannot undo
   it. C is scored here at the frozen `Ca²`.
5. Repeat 1 of the held-out buckets, then repeats 2 and 3.
6. `make benchmark-staircase-redo`, then `make benchmark-staircase-analyze`: the report,
   with each falsifier's verdict.

Nothing in the spec, the ladder or the targets changes after step 2 starts. A later
note goes in a new dated section and edits nothing above it.

## What the smoke proved

The smoke passed 20 of 20 checks on a real API server, at commit 88a631e96, in 26
minutes, on 3 GPUs at most.

**Proved there:** the lease and the namespace label hold the run's id; the deadline Job
is active and has its rights by `auth can-i`; the lease moves on renewal; the load runs
and its requests are saved; flow control turns on; the fleet moves from 1+1 to 1+2; an
abort inside a load lands in 1 s; the aborted load is saved; the teardown confirms; no
pod is left.

**Not proved:**

- **The deadline Job's actual lapse and delete.** A run's own teardown removes the Job,
  so it never fires. `make benchmark-staircase-lapse` tests this: a 1+1 fleet on a
  300 s lease, the runner killed with -9, and nobody but the Job. About 40 minutes and 1
  GPU-hour, on 2 GPUs. It has not been run, and has run only against stubs.
- **A stalled rollout**, which is exercised only by a stub.
- **The `rest` stage's quiet transfer.** Only a warm-up can show it. If a freeze prints a
  transfer near the every-row figure, the floor is the saturated one: stop and look.

One message from the smoke was not a fault: `keep_larger` printed `No such file or
directory` on the first save of each file. That was a redirect on a file that did not
exist yet, not a missing directory; every save went through.

## Decided by the user

- **Bucket A's target sits above the corrected floor, set from the fresh warm-up.**
- **The client is the mark's witness; the router is a cross-check.** The report reads
  the mark from the client. The router's column never decides one.
- **The 1.10 and 0.75 mark applies at the operating point only.** The knee is reported
  apart, with no mark.

## What is frozen

| Item | Value | Status |
| --- | --- | --- |
| Operating rate | 0.85 of the bucket's lower predicted knee | From M2 |
| TTFT target | `TTFTAt` at the operating rate, flow control off, at p90 and p95 | From M2 |
| Target floor | At least 1.2 times the model's floor; the frozen file says when this raised a target | PROPOSED. M2's A sat 1.03x over its floor, where a target says nothing of a rate |
| TPOT target | Mean TPOT priced at the operating rate, and that times the warm-up's `R_q` | From M2 |
| Predicted rate | `RateForTTFT` at the target, in the flow-control state run. Bucket C and `H-ca*` are re-priced at their measured `Ca²`, targets held (`whatif freeze --targets --ca2`) | Page |
| Steps | 0.40, 0.60, 0.75, 0.80, 0.85, 0.90, 0.95, 1.00 of the knee | PROPOSED. 0.85 is a step, so the mark interpolates over a short span |
| Hold | 10 minutes, first 3 discarded, lengthened to fit 4 relaxation times, at most 25 minutes | Page, with a cap |
| Stop | Client p90 above twice the p90 target over the whole window | Page |
| Safety stop | 4000 in flight. It is not a judgement: a step it cuts is a bound. The report names which rule ended each phase, which M2's log did not | Fixes an M2 deviation |
| Validity | Every request the seeded schedule put in the window recorded (one row and 0.1% of slack), the reading credited to the rate that schedule sent, failures at most 1%, send lag p99 at most 100 ms, `replicas.log` at the bucket's fleet, `Ca²` sent within 0.75 to 1.25 of asked, at the noise floor. A step past any of these is void: a crossing on it is a bound, and the report lists it. No engine error, or the run stops | M2's, enforced |
| Seeds | From 3001, a new one per phase. None of M2's 201 to 206 | |
| Repeats | 3, and the statistic is the median. A verdict stands only if every repeat agrees | Page |

M2's warm-up cannot check the margin: it ends saturated, so past the transfer's age its
floors are the saturated ones, 68.1 ms on A and 414 ms on B. The rates and hours below do not read the
floor. A floor there also prices the assumed 20 ms hop, since M2's recording holds no
router mean.

## The warm-up

The warm-up teaches everything the prediction reads, and M2's left S2's gate unreachable:
no recording held `V_P`, `a`, `slo` and a decode line on one cycle.

| Stage | Load | Teaches |
| --- | --- | --- |
| `line` | Closed loop, seven levels of 7 minutes, 1000 and 8000 prompts | The ITL line, `D`, and `R` (a router window needs a steady 5 minutes, and M2's 5-minute levels gave one) |
| `sweep` | Poisson light load at 1000, 2000, 4000, 6000 and 8000 | `a` and its curve. M2 swept to 16000; here that length is held out, so 6000 takes its place |
| `busy` | Closed loop, 8000 prompts, 8 tokens, 11 minutes | `V_P`, which P1 reads over its last 30 minutes |
| `rest` | Poisson light load at 8000, 6 minutes | The transfer: the figure a freeze reads ages out in 5 minutes, and M2's warm-up ended saturated |

- **Order.** `line`, `sweep`, `busy`, where M2 ran `sweep`, `line`, `busy`. This puts
  `a` next to `V_P`; M2 held both on only 44 of 317 cycles. That the new order holds
  them longer is an assumption the run checks.
- **`slo` is listed before the recorder starts**, and the freeze refuses a warm-up whose
  last cycle fails the gate.
- **Read what the freeze prints.** Its last lines list each bucket's flags and
  assumptions, and `report.md` repeats them. `router hop 20 ms` there means `R` was not
  learned, which no build has yet done on a cluster. The run can go on, but then it
  scores the assumed hop: `make benchmark-staircase-abort` if that is not wanted.

## What is held out

A, B and C are the gate buckets the page names, and the warm-up has seen their shapes.
Each `H-` bucket holds out one thing the warm-up never ran: a prompt or an output in
`H-3k` and `H-16k`, the fleet in `H-pods`, `Ca²` in `H-ca2` and `H-ca8`, and in `H-low`
only the rates, on B's shape. `H-16k` prices `a`'s curve past the longest prompt it was
fitted on, which is the test.

| Bucket | Prompt / output | `N_p` + `N_d` | `Ca²` | Flow control | Tests |
| --- | --- | --- | --- | --- | --- |
| A | 1000 / 1000 | 2 + 2 | 1 | off, on | `UnvalidatedTTFT` |
| B | 8000 / 200 | 2 + 2 | 1 | off, on | `UnvalidatedTTFT` |
| C | 8000 / 200 | 2 + 2 | 4 | off, on | `AssumedPoissonArrivals` |
| `B-onset` | 8000 / 200 | 2 + 4 | 1 | on | `AboveFlowControlOnset`, and a step near the prefill knee |
| `C-onset` | 8000 / 200 | 2 + 4 | 4 | on | `AboveFlowControlOnset` |
| `H-3k` | 3000 / 500 | 2 + 2 | 1 | off | Another prompt, another output |
| `H-16k` | 16000 / 500 | 2 + 2 | 1 | off | The long end of the prompt; the hop's growth |
| `H-pods` | 8000 / 200 | 3 + 3 | 1 | off | `N_p` of 3, more decode |
| `H-ca2` | 8000 / 200 | 2 + 2 | 2 | off | M2's errors do not cancel here if the burst reading is right |
| `H-ca8` | 8000 / 200 | 2 + 2 | 8 | off | The far side of the same |
| `H-low` | 8000 / 200 | 2 + 2 | 1 | off | 2, 4, 6, 8 and 10 r/s. M2 started at 17.6 |

**Why the onset buckets double decode.** Decode binds at `N_d` 2: the prefill knee is
57.4 r/s and decode's 44.0, and M2's fleet collapsed at 38.7 to 41.3 r/s, so no steady
step can pass the prefill onset of 54.6. At `N_d` 4 decode's knee is 87.9 and prefill
binds. The page lifts this flag on "B and C", and this reads that as their shape, not
their fleet: **a decision below.**

**The ladder must reach past the onset with a window that means something.** The onset
is 0.95 of the knee, so the room is thin. On B's shape 4 relaxation times fit the
7-minute window at 0.96 of the knee, 0.97 needs an 11-minute hold, and 1.00 fits no
window and stays flagged. The report says per step whether the window held them, and
marks an onset untestable if the first step above it did not. C's onset is 48.0 r/s,
so `C-onset` has the wider room, at the cost of two flagged steps at the top.

**Another engine step time is not in the spec.** It needs a slower model or GPU, so a
second standup and warm-up. It is the first thing to cut.

## Falsifiers, set now

Each is a claim from M2's analysis, fitted on M2. A new run can break any of them, and
the report says whether it did.

| Claim | Read as | Band | M2 |
| --- | --- | --- | --- |
| Decode's TTFT rises with step time at about 7 steps | Slope of the decode engine's TTFT on the client's TPOT, per bucket | 5.5 to 9 | 6.36 to 7.79, read by the report's code on all six phases |
| The hop grows with the prompt | Fit of the hop, ms per 1000 prompt tokens, over the bucket lengths | 0.5 to 1.5 | 1.02 |
| Bursts cost less than `Ca²` prices | Client p90, C over B, median over the steps | 1.0 to 1.8 | 1.33 off and on, each step 1.14 to 1.80. The model said 2.40 then, and says 2.09 at the operating rate since the `Ca²` tail changed (ev-shindin/llm-scaling-manager#501) |

The gate buckets' `Ca²` and a void step are read strictly at the gate: a crossing on a
void step is no reading, so it stops the run as `no valid crossing`, like a fail. At
the end, the report gives it as a bound.

**The tail moved after this band was set.** On M2's fleet, with the warm-up's term and
quiet transfer, C over B now prices 2.09 at p90 and 2.28 at p95 at the operating rate
(B 229.2 and 254.5 ms, C 479.6 and 580.4 ms), and 1.68 at p90 at 17.6 r/s. So the old top,
2.1, no longer separated the claim from the model at the operating rate. The top is now
1.8, M2's highest step, set before the freeze. Knees, onsets, ladders and hours do not move: `C`'s prefill onset stays
48.0 r/s.

A slope out of band means the term's form is wrong, not its coefficient. A hop flat in
the prompt, on `H-16k` above all, means the hop is the wrong name for it. A burst ratio
at or above the model's says M2's fall was the fleet, and `H-ca2` and `H-ca8` then say
whether it is any better.

**The ITL slope is judged at the operating point only (2026-11-10).** A bucket whose
ladder is in shares of its prefill knee, `B-onset` and `C-onset`, which double decode and
drive the prefill side to its onset and past it, prints its slope as `reported, onset
phase` and counts toward nothing. This was changed after repeat 1 showed the onset
phases at 12.8 and 19.3 while the operating-point phases held 6.2 to 8.2: repeat 1's
result stays recorded as failing, and repeats 2 and on use the scoped check. The onset
buckets are read from the frozen file (a prefill knee at or under the lower knee), so
a run frozen before the change gets the scope without its spec moving. No falsifier
stops the run: the gate scores TTFT alone.

## Per-pod `Ca²`

The question is whether routing thins bursts: `Ca²` at a pod, against the `Ca²` sent.
That needs each request's pod, and **no pod identity is confirmed yet.**

- The driver writes a `pod` column from a response header (`--pod-header`), and the
  report reads a `pod-map.csv` of request index and pod, from any log join. With
  neither, it prints no reading, and never a figure.
- The router's trace catalog on `main` names no response header with the pod. Its
  `main` log docs give the sidecar a `request finished` line with `decoder` and
  `prefiller` targets, which would give both roles, but the same page at v0.11.0, the
  version M2 ran, and at v1.0.0 does not. This is from the docs alone, not from a pod.
- The proxy can likely add a response header with its upstream host, which names the
  decode pod and not prefill. Not tried.

Spend one short probe on a small fleet before the run to find out which holds. Without
one, `H-ca2` and `H-ca8` still score the fleet's rate, and this one question stays open.

## Finer buckets: chart, config or code

There is no chart in this repository to change, and the router's buckets are not set
by one.

| Histogram | Today | Change | Where |
| --- | --- | --- | --- |
| `llm_d_epp_request_ttft_seconds`, 200 to 400 ms | Edges 0.2, 0.4, 0.6: M2's recording reads them, and upstream `gateway-api-inference-extension` v1.2.1 has the same list as a Go literal | **Code.** The router's `main` docs give no key for it. It needs an upstream change | `llm-d-router`, an issue to file |
| `llm_d_epp_request_streaming_tpot_seconds`, for `R_q` | Edges 0.005, 0.01, 0.02, 0.04: M2's `R_q` is flagged `CoarseTPOTBucket` | **Config, on a newer router.** `main`'s docs give 5 ms spacing from 5 to 50 ms by default and a `metrics.requestTPOTBuckets` key. The docs of v1.0.0, the latest tag, do not, and M2 ran v0.11.0 | Our scenario's router pin, once a tag has it |

Neither blocks the run: the client is the witness. A router bump inside this run is
not wanted, since it would change what the warm-up learns from M2's fleet. The client
gives a per-request TPOT percentile, so the TPOT mark needs no finer bucket; only the
learned `R_q` does.

## What it costs

From `staircase_freeze.py plan` on r1's frozen warm-up, each ladder laid from the
spec's steps times the bucket's knee as a fresh freeze lays it, at every step of every
ladder. A phase that stops early costs less: M2's phases ran 34 to 74 minutes of a
100-minute ladder.

| Bucket | Pods | Hours a phase | Phases | GPU-hours |
| --- | --- | --- | --- | --- |
| A | 4 | 1.75 | 6 | 42 |
| B | 4 | 1.74 | 6 | 42 |
| C | 4 | 2.59 | 6 | 62 |
| `B-onset` | 6 | 2.17 | 3 | 39 |
| `C-onset` | 6 | 1.82 | 3 | 33 |
| **Gate** | | **48.5** | **24** | **218** |
| `H-3k` | 4 | 1.75 | 3 | 21 |
| `H-16k` | 4 | 2.04 | 3 | 24 |
| `H-pods` | 6 | 1.74 | 3 | 31 |
| `H-ca2` | 4 | 1.90 | 3 | 23 |
| `H-ca8` | 4 | 2.07 | 3 | 25 |
| `H-low` | 4 | 1.74 | 3 | 21 |
| **Held out** | | **33.7** | **18** | **145** |

Against main's code and spec on the same file: C's three extra steps add 41 minutes a
phase, B-onset's two measure-only steps 50, and sizing a bursty step's window for the
top of its Ca² band adds 4 to C-onset, 3 to `H-ca2` and 4 to `H-ca8`.

Add about 2 hours and 8 GPU-hours for the standup and warm-up. **The whole run is at
most about 370 GPU-hours and 84 hours end to end**, against the harness's hard cap of
96 hours, at 4 to 6 GPUs at a time (6 pods at TP 1 is the peak). Phases that stop at
about 0.95 of the knee, as M2's did, would bring it lower; that is not computed.

A failure in the gate buckets' repeat 1 stops the rest.

## What to cut first

In order. Nothing here touches a bucket a flag is lifted by.

1. **Another engine step time.** Not in the totals. About 50 GPU-hours for its own
   warm-up and A and B off. Leave it out unless a falsifier breaks.
2. **Repeats 2 and 3 of the held-out buckets**, if repeat 1 lands in the mark: saves 87
   GPU-hours. It weakens only the spread on a bucket no flag depends on.
3. **`H-pods`**: saves up to 30. It is priced at an assumed `f` (below), so it scores
   that assumption as much as the model.
4. **`H-ca8`**: saves 24, keeping `H-ca2`, where the errors should stop cancelling.

Do not cut `C-onset` or `B-onset` to save money: without them `AboveFlowControlOnset`
stays, as it did after M2.

## What only this run can settle

- Whether the term lifts A and B into the mark. The consistency check on M2 is not
  evidence: it read 8 of 31 levels within 10% at p90, on the data that chose the form.
- Whether a fleet can be changed between phases as `staircase.sh` does it. M2 never did.
- Whether `R` is learned on a cluster, and whether `a` and `V_P` overlap for long under
  the new warm-up order.

## To decide before the run

1. **May the onset buckets double decode?** *Recommend:* yes. On `N_d` 2 the flag cannot
   be tested at all, on M2's evidence. The cost is that the page's "B and C" becomes
   their shape on another fleet.
2. **`N_p` 3 is priced at an assumed `f` of 1.5**, because a warm-up at `N_p` 2 learns
   none at 3. *Recommend:* keep it and read the flag, or cut `H-pods`. A second `busy`
   stage at `N_p` 3 might teach it, for about 3 GPU-hours. That is not checked, and not
   in the spec.
3. **Does the burst band stay 1.0 to 2.1?** *Decided:* no, the top is 1.8, M2's highest
   step. It was set when the model said 2.40, and a ratio just under 2.1 now agrees with
   the model rather than showing it overprices.
4. **Is 1.2 the right floor margin?** *Recommend:* yes, as proposed. It did not bind on
   M2's warm-up, and it only needs to keep a target off the flat part of the curve.
