# Capacity planner: extend what a recording can answer with simulation

**Status:** proposed (2026-11-23). Nothing here is built.

## Why

The capacity planner learns only from what a fleet did. Where the fleet never went, it refuses or gives a bound. That is the right default, but it leaves the questions people ask most often unanswered:

- **Where does a fleet that never queued saturate?** Its prefill speed is learned from busy minutes. A fleet that always had spare prefill gives only a lower bound on the knee ("at least N r/s"), and no TTFT past it.
- **What happens at load the fleet never saw?** Fitting on the light hours of a day and predicting the busiest quarter shows the problem: TPOT carries over (the mean within 20% on every held-out step), but TTFT does not, because the light hours hold no queue. The planner refuses rather than guess.
- **What would another configuration do?** Other tensor or data parallelism, another GPU, another prefill-to-decode ratio, other replica counts. A recording at one configuration says little about another.
- **What does routing change at another pod count?** Prefix-cache-aware routing gives a different hit rate with fewer or more pods (#384). A recording keeps the routing it ran with.

Two open simulators predict LLM serving without GPUs, and both cover the P/D stacks we size. Used carefully, either one could answer these questions where the planner has no data. Used carelessly, either one could turn a planner that says "I don't know" into one that is confidently wrong.

## The simulators

| | AISim (ai-dynamo/aisim) | BLIS (inference-sim/inference-sim) |
|---|---|---|
| What it is | The successor to AIConfigurator: a performance model (forward-pass latency and memory), a request replay in virtual time, and a configuration search | A CPU-only, deterministic discrete-event simulator of an LLM serving stack; "does not load model weights or execute tensor kernels" |
| Latency model | Op-level analytical (GEMM, attention, communication, memory) over a calibrated kernel database; whole-forward measurements; online regression from observed iteration timings; tree-ensemble models | Analytical basis functions with learned scaling coefficients and overhead terms, "fit once globally"; a coefficient registry ranked from measured to assumed |
| Routing and admission | A frontend AISim router (with Dynamo) | The llm-d router's own code, hosted in simulated time, "with no change to any routing decision"; admission and flow control |
| P/D, KV tiers, speculation | P/D (vLLM, TRT-LLM); GPU, host and storage KV tiers; n-gram speculation; MTP points in its accuracy data | P/D; KV transfer and offload to CPU, file system, object store and peer-to-peer; speculative acceptance profiles (partial) |
| Accuracy it reports | Mean error against silicon on measured benchmark cells, with no fixed pass threshold; its own release notes list biases up to about 1.9x and 3.6x on some models and GPUs | 6.7% median end-to-end error over 36 experiments (six models, three GPU types); its dashboard's TTFT mean error is in the 20s of percent; "not calibrated for deeply saturated conditions" |
| Percentiles | TTFT and TPOT reported; p90/p95 not confirmed | Per-request rows, so any percentile; p90 used in its case studies |
| Inputs | Model id, hardware, engine, parallelism, synthetic load or traces; checked-in profiles, and GPU profiling runs for anything new | A scenario file (engine kind and version, model, GPU, parallelism), a rate or a trace |
| Calibration from a running fleet | Online regression from observed iteration workloads and wall times | Not documented; its `calibrate` command was removed in v0.16.0 |
| Maturity | v0.13.0 (2026-10-21), very active; license not confirmed | v0.18.2 (2026-11-19), Apache-2.0, pre-1.0 |

Sources: the AISim README and docs/perf-model, its v0.12.0 and v0.13.0 release notes; the BLIS README, its releases, the llm-d blog "BLIS: evolving llm-d at simulation speed", and arXiv 2610.14255. Their accuracy dashboards render in JavaScript and were not read.

Neither one is validated on the fleets we size, and neither reports production backtests. That is the gap this proposal closes before anything uses them.

## The rule: a simulator is a source, never the truth

1. **Measured wins.** Where the recording measured a term, the planner prices it from the measurement, as today. A simulated term fills only what the recording cannot give.
2. **Calibrated on this fleet first.** A simulator is used for a fleet only after it reproduces that fleet's own recording, scored by the planner's backtest on the window the fleet ran. A simulator that does not reproduce what was measured is not asked about what was not.
3. **Flagged everywhere.** Every figure a simulated term moves is flagged `Simulated`, reads OUTSIDE VALIDATED REGION, and names the simulator, its version and its calibration score. The self-check and the footer say so.
4. **Never below what was measured.** Where a simulated knee or speed falls under a measured lower bound, the bound stands. A simulation may lift "at least N" toward an estimate; it may not cut a measured figure.
5. **Off by default.** An opt-in flag per command (`--simulate <adapter>`), as the experimental models are today.

## Design

### Phase 0: score the simulators on our recordings, no planner change

Before any code goes into the planner, measure whether either simulator is good enough to be worth it.

- **Data:** the lab recordings the cross-validation already uses, the TTFT validation runs, and the staircases with a measured knee.
- **Method:** run each simulator at each recording's configuration (model, GPU, parallelism, engine flags, replicas) and its measured load (rate, prompt and output lengths, prefix-hit rate), step by step. Score the simulated TTFT and TPOT mean, p90 and p95, and the knee, with the same scorer the planner's backtest uses: ratio, within 20%, under 0.8, and in the measured bucket.
- **Compare against:** the planner itself on the same steps, including where the planner refuses.
- **Outcome:** a table per simulator and recording. It decides whether phase 1 is worth building, and with which simulator.

### Phase 1: a simulated source beside the recording

The planner reads a second source, produced by a simulator adapter:

- **Calibration run.** The adapter runs the simulator over the recording's own window, at its measured load and configuration. The planner scores it against what was measured. Below a pass bar (for example the mean within 20% on 80% of steps, and no p90 under 0.8), the simulated source is refused for this fleet, with the score shown.
- **Extension run.** Past the bar, the adapter runs the simulator where the question asks: a higher rate, other replica counts, another configuration. Its output is converted into the same series a recording holds (rates, latency histograms, queue and KV figures), so the existing pricing reads it unchanged.
- **What it may supply:** the terms the recording cannot learn. That means the prefill knee and the queue curve past the highest measured load, a configuration not recorded, and the hit-rate change at another pod count where the simulator models the router.
- **Interface:** an adapter per simulator under `internal/whatifsource/sim/`. It is a scenario writer, a runner (the simulator's CLI, in a container or on the PATH) and a converter to the planner's series. The adapter pins the simulator's version.

### Phase 2: blend instead of switch

Phase 1 switches from measured to simulated at the edge of the data. A later step could use the simulator as a prior that the recording updates, for example as the prior for the prefill speed, with the measured busy minutes as evidence. This is for later, once phase 1 shows where the simulators are right.

## Acceptance

- **Phase 0:** a written comparison on at least the lab staircases with a measured knee and the TTFT validation runs, for each simulator against the planner.
- **Phase 1:**
  - On the staircases with a measured knee, a planner fitted on the light part of the run and extended by simulation predicts the knee within 15% where the planner alone gives only a lower bound.
  - Fitting on the light hours and predicting the busy ones, TTFT is priced where today it is refused, with the mean within 20% on most held-out steps.
  - No figure newly reads under 0.8 on the public recordings, as for every planner switch.
  - Where the calibration run fails its bar, the planner refuses the simulated source and says why. A test shows this.
  - Without the flag, the output is byte-identical.

## Risks

- **The saturated regime is where both are weakest,** and it is the regime the knee lives in. BLIS says it is not calibrated there; AISim's own biases reach several times on some models. The calibration run limits the damage, but only within the load the fleet reached.
- **Coverage:** a model or GPU without a profile or coefficients may need GPU runs first (AISim), or fall back to roofline-quality estimates (BLIS).
- **Moving targets:** both are pre-1.0 and change their schemas. AISim says replay and sweeper "may change without a standard deprecation period". Pinning a version per adapter is required.
- **Licences:** BLIS is Apache-2.0. AISim's licence text must be read before an adapter ships.
- **Engine drift:** a simulator models an engine version. A fleet on a fork or a newer build may differ, which the calibration run is there to catch.

## Open questions

- Which simulator first? Phase 0 decides. BLIS hosts the llm-d router itself, which matters for prefix-aware routing. AISim covers more hardware and the configuration search.
- Should the calibration bar be per role (prefill, decode) or per figure?
- Can a simulator's coefficients be fitted to our recording, rather than only scored against it? AISim's online regression suggests so. For BLIS it is not documented.
- Where the simulator and a measured lower bound disagree, how is the gap shown on the page?

## Related

- [Capacity planner](capacity-planner.md), and [what-if capacity: end-to-end TTFT](slo-capacity-planner-ttft.md) for the prefill speed and knee.
- #384 (the prefix hit rate at another pod count), #387 (choosing the queue model by fit), #368 (prefill speed without a queue).
