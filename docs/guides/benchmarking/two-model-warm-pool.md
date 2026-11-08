# Two models, anti-phase bursts, warm pool on and off

[← Benchmarking guide](README.md)

A warm pool is insurance: every pool Pod holds its accelerators all the time, so
a pool of *N* lowers your maximum fleet by *N*. The case for paying that is that
**one pool covers many models**. This scenario measures that claim. Every other
warm-pool measurement here has been one model bridging its own scale-up, where
the pool is pure overhead at steady state.

Two models, each with its own EPP and ScaledObject behind one gateway, take
bursts that do not coincide. Their total demand is flat; each model's own demand
swings. Each model scales on its own signal, so without a pool both pay a full
cold model load on every rise, just as the other gives capacity back. With a
pool, one Pod bridges whichever model is rising and is handed back in time for
the other.

The same traffic runs once per **arm**, and the arms are compared on
time-to-first-token **and** accelerator-seconds:

| arm | what it is | the insurance it pays |
| --- | --- | --- |
| `nopool` | autoscaling alone, each model between 1 and `MAX_REPLICAS` | none — every rise pays a cold model load |
| `pool` | the same, plus a 2-Pod warm pool with both models resident | 2 accelerators held for the whole run |
| `floor` | no pool, each model **held at 2 or more** for the whole run | 2 extra replicas held for the whole run |

`nopool` is the baseline, `pool` is the claim under test, and `floor` is what a
pool competes with in practice (over-provisioning): which insurance is cheaper
for what it buys?

**Every model gets the same ceiling in every arm**: as many replicas as it asks
for, up to what the cluster can place, with the pool on top. The cost of each
insurance is **measured** in accelerator-seconds, not imposed through the cap
([why](#why-the-ceiling-is-not-lowered)).

## The phases

Default shape: a 2-minute lead-in, then four 8-minute phases separated by 90 s
bands where neither model bursts (2310 s, 38.5 minutes). Model A (Llama) bursts
to `HIGH_RPS` = 9, model B (Qwen) to `HIGH_RPS_B` = 8, because their per-replica
capacity differs. Each phase is split at `RISE_WINDOW` = 240 s into the rise
stage the report reads and the remainder:

| phase | window | model A | model B | what it is for |
| --- | --- | --- | --- | --- |
| lead-in | 0–120s | 3 rps | 3 rps | both stacks serve before anything is measured, so the first burst is a scale-up and not a first-request penalty |
| 1 | 120–600s | 3 rps | **8 rps** | B rises (rise stage 120–360s). A is idle, and A's replicas are capacity B does not have |
| band | 600–690s | 3 rps | 3 rps | neither model bursting, so the two cannot burst at once when they drain at different speeds |
| 2 | 690–1170s | **9 rps** | 3 rps | A rises (690–930s) while B falls — the swap the pool is supposed to absorb |
| band | 1170–1260s | 3 rps | 3 rps | |
| 3 | 1260–1740s | 3 rps | **8 rps** | B rises again (1260–1500s), with whatever state the first cycle left |
| band | 1740–1830s | 3 rps | 3 rps | |
| 4 | 1830–2310s | **9 rps** | 3 rps | A rises again (1830–2070s) |

The table comes from the same generator that renders the load, so the windows
cannot drift: `python3 hack/benchmark/two_model_profile.py --emit schedule`
prints the live one. Change the shape with `PHASE_SECONDS`, `CYCLES`, `LOW_RPS`,
`HIGH_RPS`. `preflight` refuses phases below 420 s
([why 8 minutes](#why-8-minutes-and-not-4)).

Arrivals are a **Poisson process at the phase's rate, open-loop**: issued
whether or not earlier requests returned. A closed-loop driver sends less when
the server slows, so the queue never grows and a cold scale-up looks free.

How the load is generated and calibrated is in [Details](#details): the
[load generator](#the-load-generator-is-the-llm-d-harness), [non-overlapping
bursts](#the-two-models-bursts-must-never-overlap), [the
prompts](#the-prompts-share-no-prefix), [per-stage
latency](#every-latency-is-the-generators-own-per-stage) and [the data
path](#the-data-path-resolves-nothing).

## What makes the arms comparable

Six ways the arms can silently stop being comparable, each still producing a
complete, plausible table. The tooling enforces every one:

| | enforced by |
| --- | --- |
| Every model must have the **same ceiling** in every arm. A model capped at 2 in one arm and 3 in another differs in TTFT for a reason that is not the insurance under test. | `run` writes `budget.json` (ceiling, floor, pool size); the report **refuses** an arm whose per-model ceiling differs from nopool's, a `pool` arm with no pool, or a `floor` arm whose floor is no higher than nopool's |
| The fleet must start at the floor. With 300s scale-down stabilization and one arm always running first, the second would start on an already-scaled fleet and pay no cold load at all. | `reset` pins both ScaledObjects at `MIN_REPLICAS`, waits for it, then releases them |
| The pool must be **warm**. A cold pool pays a model load *into* the pool on the first burst, on top of the replica's own — a true measurement of a pool nobody would operate that way. | `warm` pins a copy of each model and waits; `run ARM=pool` re-checks |
| The traffic must be identical. | both arms render the same inference-perf profiles from `SEED`; the report refuses if the seeds or the schedule differ |
| The load must reach **more than one replica**. Capacity that is never routed to cannot affect TTFT, so a run where the router pinned everything to one engine measures a one-replica fleet in every arm. | `run` reads each engine's own prompt-token counter every 60 s for the whole arm and keeps the highest reading, so a replica scaled away mid-run stays in evidence; the report **refuses** if, per model, one engine did nearly all the work |
| The **driver** must not be the queue being measured. | inference-perf reports its own `schedule_delay`; the report refuses above 250 ms at p95, and refuses an arm that reports none |

## What it needs

- [ ] **Free accelerators for the peak**: `2 × MAX_REPLICAS × GPUS_PER_REPLICA`,
  plus `POOL_REPLICAS × GPUS_PER_REPLICA` for the pool arm. `preflight` counts
  what is *placeable* (free accelerators on schedulable, Ready nodes with the CPU
  and memory a replica asks for) and refuses below the peak: a run whose bursts
  sit Pending measures the scheduler. Set `MAX_REPLICAS` to what the models could
  ask for, not a cap you want to impose.
- [ ] **One accelerator kind.** A warm copy is reusable only on the accelerator
  it was loaded on. `preflight` refuses if the cluster advertises more than one
  and `ACCELERATOR` is not set. A pool on the wrong product never lends, and
  nothing fails: the run just reports the pool did nothing.
- [ ] **Two models of similar size**, both loadable from the model cache the pool
  mounts (`CACHE_CLAIM`, default `model-pvc`).
- [ ] Everything [the warm pool guide](../warm-pool/) needs: the two images, the
  RBAC to patch Pods, the NetworkPolicy.
- [ ] **Know who owns Istio.** The standup installs `istio-base` and `istiod` by
  helmfile. On a cluster that runs Istio by other means, that install fails on
  ownership metadata part-way, and the half-built namespace then trips the
  EPP-reuse guard on the next attempt. `GATEWAY_REUSE=auto` (the default) reuses
  the cluster's Istio: it marks its own copy of the scenario `skipInstall`, with
  the control plane in `GATEWAY_PROVIDER_NAMESPACE` (`istio-system` unless set),
  and installs Istio only where there is none. `true` and `false` force it.
  `preflight` checks this both ways, and no tracked scenario is edited by hand.
- [ ] **A node with 64 GiB free for the load Pod.** Each loader container is
  limited to 32 GiB. inference-perf keeps every synthetic prompt it makes, so
  memory grows all run. Measured: the 9 rps loader was OOMKilled at 8 GiB
  thirty-three minutes in, at the start of its fourth rise, with the arm's
  accelerators already spent. The driver now stops the arm the moment a loader
  container dies, but the Job still has to fit.

## Running it

Every step is separate and says what it found: the expensive failures are the
ones found at minute 40 of a 45-minute run.

```bash
export BENCHMARK_NAMESPACE=<your namespace>
```

The models, their stack names and their accelerator budget live in
`hack/benchmark/scenarios/guides/two-model-warm-pool.yaml`. `MODEL_A`/`MODEL_B`
and `STACK_A`/`STACK_B` in the driver must match it; `verify` fails loudly if
they drift.

**1. Preflight, before anything exists.**

```bash
make benchmark-two-model-preflight
```

It checks free accelerators against the peak, one accelerator kind, the phase
length against scale-down stabilization, and the model cache claim.

**2. Stand up both stacks.**

```bash
make benchmark-two-model-standup
```

**One** standup: two stacks, one shared gateway. Two standups would not work:
the single-model guide uses `gateway.className: epponly`, which deploys no
Gateway and which the renderer refuses for a multi-stack scenario, so there
would be no shared address.

**3. Verify, before any load.**

```bash
make benchmark-two-model-verify
```

It checks exactly one gateway Service, **two** EPP deployments, a ready decode
replica per stack, and a real request answered per model on its own path. A
stack that 404s for one model produces a full run of failures that reads like a
pool result.

**4. First arm: no pool.**

```bash
make benchmark-two-model-reset
make benchmark-two-model-run ARM=nopool
```

Refuses if a pool exists. About 45 minutes at the defaults (7 minutes of
preload, 38.5 of load).

**5. Create the pool.**

```bash
make benchmark-two-model-pool-create
```

It is a **bridge**, with `--max` equal to `--replicas` so the pool's own
ScaledObject cannot move accelerators under the measurement. An idle pool Pod
reports **NotReady on purpose**, which keeps it out of the InferencePool. Do not
wait on `readyReplicas`.

**6. Warm it: both models resident, before any load.**

```bash
make benchmark-two-model-warm
make benchmark-two-model-residency   # what each Pod is actually holding
```

This decides whether the run means anything. `warm` pins one copy of each model
with `warmPoolCopies: "1"` and **waits** until both are resident, asking each
pool Pod's supervisor over loopback (the pool's NetworkPolicy admits that port
only from the scaling manager controller, so a probe Pod could not reach it).
Pinning matters beyond the first burst: in automatic mode a quiet variant loses
its slot, and here each model is the quiet one for half of every cycle, just
before its own burst.

**7. Second arm: with the pool.**

```bash
make benchmark-two-model-reset
make benchmark-two-model-run ARM=pool
```

Refuses if no pool exists, and re-checks residency (the pool can lose a copy
between `warm` and `run`). The models keep the same ceiling; the pool's two
accelerators are on top, and show up in the GPU-seconds. `SKIP_WARM_GATE=1` says
a cold pool is what you meant to measure.

**8. Third arm: over-provisioned, no pool.** Optional; it prices the pool against
the alternative.

```bash
make benchmark-two-model-pool-delete
make benchmark-two-model-reset
make benchmark-two-model-run ARM=floor
```

Refuses if a pool exists. It sets `minReplicaCount` to `FLOOR_REPLICAS`
(default 2) on both ScaledObjects (a Deployment scaled by hand would not hold:
at the low rate the controller recommends 1 and KEDA puts it back), waits for
both models to reach the floor, then drives the same load. The next `reset` puts
the floor back to 1.

**9. Report.**

```bash
make benchmark-two-model-report
```

It compares every arm against `nopool` and, when both `pool` and `floor` ran,
prices the two insurances against each other. It writes `report.md`,
`report.json` (every number in the tables) and three SVG plots (p95/p50 per rise
window, GPU-seconds per arm, the fleet timeline) using the standard library
alone; regenerate them anywhere with
`hack/benchmark/two_model_plots.py --json report.json --out .`.

**10. Tear down. It is a shared cluster.**

```bash
make benchmark-two-model-teardown
make benchmark-two-model-status      # confirm nothing still holds an accelerator
```

## Reading the result

A complete three-arm run, with tables and plots, is on the well-lit path:
[What a warm pool buys, measured](../../well-lit-paths/warm-pool-bridge/measured.md).

- **TTFT over the whole run** matters least: most of a run is steady state,
  where the pool only holds accelerators.
- **TTFT per rise** (the first `RISE_WINDOW`, 240 s, after a model's rate goes
  up) is where a bridge can matter. It is printed **one row per rise**, not
  pooled: each rise is a *single scale-up event*, so pooling its hundreds of
  requests gives a number that looks like n=700 and weighs like n=1.
- **Accelerator-seconds include the pool's own.** A pool arm that wins on TTFT
  while holding extra accelerators has spent, not won. It excludes the pool's
  warm-up before the run, so it *understates* the cost.
- **Failures are counted beside every percentile.** A request that did not get
  all the tokens it asked for is a failure. Counting torn streams as successes
  lets a saturating arm shed its worst requests while its p95 improves.

The report **refuses** arms that are not comparable: different schedule or seed,
unissued arrivals (the driver, not the cluster, was the limit), driver queueing
above 250 ms at p95, or a pool arm allowed more accelerators.

**How far this goes.** Four scale-up events per arm, one run each, no
repetition, no confidence interval. A difference the size of the spread between
two rises of the same model is not a result. To claim a direction, repeat the
pair and check the sign holds.

## What it does not measure

- **Throughput and ITL.** Output length is pinned (`ignore_eos`), so decode work
  per request is constant by construction; this run is about the arrival of
  capacity, not its quality.
- **A-versus-B.** The two models have different tokenizers, so one prompt string
  is a different token count for each. Comparing arms is valid; comparing model A
  with model B is not.
- **In-phase bursts.** Both models spiking together is a capacity question, not
  a sharing one: a pool of two lends twice.
- **More than one pool.** One accelerator kind, one pool. See
  [several pools](../warm-pool/multi-pool.md).

## Details

### Why the ceiling is not lowered

An earlier version lowered the pool arm's cap by the pool's share so the arms
would "peak at the same number of accelerators". That also stopped the pool arm
from ever holding three real replicas *and* a bridge, a fleet nobody would run.
So the cap is the same everywhere, and each insurance is priced in
accelerator-seconds.

### Why 8 minutes and not 4

Scale-down stabilization here is 300 s: fast up, slow down, because
under-provisioning costs TTFT irrecoverably while over-provisioning only costs
money. With a shorter phase, the falling model still holds every replica it grew
through the rising model's whole window, the two never compete, and the
anti-phase premise is never exercised.

The anti-phase property is **structural**: the generator builds B's rate from
the opposite level of A's, so no edit can make the two burst together while the
file claims they do not.

### Calibrating the load

**It must be heavy enough to force a scale-up.** Measured on an H200: one replica
of an 8B model absorbed **40 rps** of 1000-token requests without queueing,
because `maxNumSeq` defaults to 256, so neither arm added a replica. The scenario
caps concurrency at `maxNumSeq: 32`, a normal production value, and the defaults
(3 → 9 rps, 1000 input and 500 output tokens) then take each model from one
replica to three and back. This does not bias the comparison: a pool bridges a
**model load**, whose cost is set by the weights and storage. It changes when a
scale-up triggers, not how long the new replica takes, identically in both arms.

**The burst must also be SERVABLE at full scale.** At 1000-token outputs, 12 rps
was more than three replicas could deliver: the queue grew without bound, the
driver pegged at its worker cap, and both arms would have measured "which is
less broken". Measured here, one replica serves ~3.8 rps of 500-token requests
and three serve ~11, so 3 → 9 rps needs a scale-up and is served once it lands.
If you change models, accelerators or token counts, **re-measure both ends**:
the rate that forces a scale-up, and the rate the fleet serves at its ceiling.

### The load generator is the llm-d harness

Load comes from **inference-perf**, the generator llm-d-benchmark uses, run from
the `ghcr.io/llm-d/llm-d-benchmark` image: one instance per model, as two
containers of one Pod. Each of these fixed a defect in the bespoke client it
replaced:

- **Token counts are real.** inference-perf tokenizes with each model's own
  tokenizer, so `INPUT_TOKENS` means input tokens for *both* models. A generator
  that counts words is off by a different amount per model.
- **The schedule is data, not control flow.** Each phase becomes inference-perf
  `load.stages` entries, and anti-phase is the two profiles carrying **mirrored
  ladders** over identical boundaries. `two_model_profile.py` renders both from
  one schedule and writes it next to the results; the report refuses two arms
  whose schedules differ.
- **The driver reports its own queueing.** `load_summary.schedule_delay` is how
  late inference-perf issued its own arrivals. The report refuses an arm whose
  p95 exceeds `--max-queue-delay` (250 ms), and refuses an arm that reports none,
  since a missing value would percentile to a passing 0.0.

The two containers share a **start barrier**: an absolute epoch `PRELOAD_GRACE`
seconds (default 420) after the Job is created. Each fetches its tokenizer
*before* the barrier, because the two downloads take different times and the
difference would offset the ladders all run. A container that reaches the
barrier late prints `LATE-START`, and `run` refuses the arm; raise
`PRELOAD_GRACE` and rerun.

**Stages run back to back.** `load.interval` is inference-perf's *sleep between
stages* (`if self.stageInterval: await sleep(self.stageInterval)`), not a metrics
interval, though llm-d's shipped guide profiles set it to `30.0`. At 30 that is
thirty seconds of **zero load** right before every rise: queues drain and the
autoscaler sees an idle fleet just before the burst, understating every
scale-up, and 240 s of silence inside a 2040 s run. It is `0` here.

### The two models' bursts must never overlap

That is the whole premise, and it does not hold for free. **A stage ends when
its in-flight requests drain, and the bursting model drains slower**, so the two
models cross a boundary at different instants. Measured on a multi-node H200 cluster: per-stage
drains of 4–16 s, and a cumulative divergence that reached **6.1 s and changed
sign** as the burst moved between models. Each second of it is both models
bursting, which a pool of one lendable Pod would record as the pool failing:
an artefact of the driver.

So `OVERLAP_SECONDS` (default 90) inserts a band between bursts where **both**
models sit at the low rate:

- No stage ever has both models high. This is checked on what the generator
  emits with no arguments, because a band that needs a flag is no guarantee.
- The report **refuses** a run whose measured drift exceeded the band, computed
  from the generator's own per-stage elapsed time.

The band dips total demand to `2 × LOW` instead of `LOW + HIGH` for 90 s; with
300 s scale-down stabilization no replica is given back because of it.

**Tokenizers are cached on the shared model claim.** Otherwise each arm fetches
both from the internet, and a blip costs the whole arm. Measured: a
`CAS Client Error` from the Hugging Face CDN took model A down while B fetched
fine, and the run was refused after five minutes of preload grace with both
models' accelerators held. The `hf` volume is `CACHE_CLAIM` under
`subPath: benchmark-tokenizer-cache`, so nothing is written into the weights
tree; with no such claim it falls back to an `emptyDir` and refetches. The fetch
retries five times with backoff.

`RISE_WINDOW` (default 240) is how much of each phase becomes its own stage. It
must be shorter than `PHASE_SECONDS`; the profile generator refuses otherwise,
because an uncut phase has no rise stage to read.

### The prompts share no prefix

`LOAD_DATA` (default `synthetic`) selects inference-perf's dataset, and it
decides whether a scale-up can be measured at all. `synthetic` slices every
prompt from a random offset into a 5 MB corpus, per request, so no two requests
start alike.

The reason is llm-d's shipped scheduling profile. It weights
`prefix-cache-scorer` highest (3, against 2 for queue depth and 2 for KV
utilisation), and the scorer's hashes chain from the first token. A replica that
cached a prompt's opening wins every later request sharing it, **regardless of
its queue**, and a replica that cached nothing never gets a request, so it never
starts winning. Measured on a multi-node H200 cluster with the `shared_prefix` dataset (32 groups
× 64 prompts, cycled, the suite's own shape): per model, the busiest engine did
**99.0 % and 98.7 %** of the prompt tokens over a 36-minute arm, and every added
replica did 0.3–2.0 %. Both arms were one-replica fleets, and the aggregate hid
it: pooled across both models the busiest engine showed 48.9 %, which looks like
spread. With `synthetic`, an **empty** second replica took 49.4 % of 9 rps from
its first minute (p50 TTFT 58 ms).

The report refuses an arm where, per model, one engine did nearly all the work.
`LOAD_DATA=shared_prefix` with `PREFIX_GROUPS` (default 32) is kept to reproduce
the pin, not to measure with.

With no prefix hits every request is a full `INPUT_TOKENS` prefill, so a
replica's capacity is lower than a shared-prefix run suggests. Two Llama
replicas served 9 rps at p50 58 ms under `synthetic`; if you change the dataset,
re-measure both ends of the burst ([calibrating the load](#calibrating-the-load)).

### Every latency is the generator's own, per stage

inference-perf v0.6.1 reports `time_to_first_token` as a **per-stage
distribution**. Its per-request records carry no token timestamps (measured on
a multi-node H200 cluster: 210 records, raw response chunks, no time on any), so nothing can be
re-cut into a window after the run.

So **the rise window is a stage**. Every phase after the lead-in is split at
`RISE_WINDOW` (default 240 s) into an opening sub-stage and the remainder, at the
same rate, and the headline number is read from
`stage_N_lifecycle_metrics.json` for the opening stage of each rise. Both models
split at the same boundaries, so one stage index is one window for both, and the
opening of a *falling* model's phase comes free as a control.

- The report computes no percentiles. It prints the generator's, and refuses an
  arm whose stage files do not cover the whole schedule.
- `start_time` in the per-request file is a **monotonic** clock (measured:
  `10655087.75`, an uptime), so GPU samples are aligned against the driver's
  start barrier instead.

**Per-request reporting is off.** That file stores the raw SSE text of every
chunk and reached **1.2 GB for an eleven-minute run**, which `kubectl cp`
(exec+tar) truncated, losing an arm after all its accelerators were spent.
Nothing needs it: request counts are in `successes.count` and failures in
`failures.by_label`, in stage files of 7 KB each. A stored run that has one is
still read.

Results arrive as `stage_N_lifecycle_metrics.json` and
`summary_lifecycle_metrics.json`, one directory per model; `harness_results.py`
converts them to the `requests.jsonl` + `meta.json` pair the report compares,
computing nothing the harness already measured.

### The data path resolves nothing

The load addresses the gateway by **ClusterIP**, not by name. Measured twice on
a multi-node H200 cluster at only ~10 rps total: **5.7% of requests** died of
`ClientConnectorDNSError: Temporary failure in name resolution`, spread across
the run. aiohttp resolves once per connection, and the cluster default
`ndots:5` makes a four-dot service name try every search domain first: four
lookups per connection. That is the driver's own loss, three times the report's
client-side threshold, and it voids the arm.

So the driver reads the gateway's ClusterIP before the run and puts it in both
profiles, the load Pod runs with `ndots:1`, and if the ClusterIP cannot be read
the fallback is an **absolute** name (trailing dot). This is safe because the
shared HTTPRoute matches on path and declares no hostnames; if that changes, the
load must carry a `Host` header.

Each container also sends one real request and waits for a 200 before the start
barrier, because the load Pod's istio sidecar is not ready the instant the app
container starts.
