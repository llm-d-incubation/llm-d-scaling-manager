# TTFT, TPOT and ITL: what each component measures

Three latencies are named the same way across the stack, and each component
measures them differently. A TPOT of 30 ms on a client's report, on the
router's histogram, in the engine's ITL and in the capacity planner are four
different numbers. Read this page before comparing them, and before writing a
target into a scaling policy.

- **TTFT**: time to first token. When it starts depends on who measures it.
- **TPOT**: time per output token, per request: the time after the first token
  divided by the tokens after it.
- **ITL**: inter-token latency, the time between two tokens of one request.
  The engine records one value for each interval, so a long request
  contributes many values and a short one few.

## The table

| Who | Series or field | Starts and ends | Streaming | Window | Statistic |
| --- | --- | --- | --- | --- | --- |
| **Client** (a benchmark such as guidellm, or your application) | its own per-request records | TTFT: from sending the request to the first token received, network, router queue and engine queue included. TPOT: the rest of the request over its output tokens after the first | needs streaming for TTFT and TPOT | the client's run | whatever the client reports: mean, median, p90, p99 over requests |
| **Router** (llm-d EPP) TTFT | `llm_d_epp_request_ttft_seconds` | from the router receiving the request to the first token it relays; includes the router's flow-control queue, excludes the client's network | `streaming="true"` only; recorded when the stream ends | the SLO analyzer reads 5 minutes | per request; the SLO analyzer reads a percentile from the histogram buckets, interpolated between bucket bounds |
| **Router** (llm-d EPP) TPOT | `llm_d_epp_request_streaming_tpot_seconds` | per request, the time after the first token over the tokens after it | streaming responses that carry `usage` only | the SLO analyzer reads 5 minutes | per request; the SLO analyzer reads the mean over requests (sum over count), each request counted once |
| **Engine** (vLLM) TTFT | `vllm:time_to_first_token_seconds` | from the engine receiving the request to its first token; includes the engine's own queue, excludes the router | all requests | the controller reads 1 minute per pod | per request; the controller reads the mean over requests |
| **Engine** (vLLM) ITL | `vllm:inter_token_latency_seconds` | one value per decode step per running request; with speculative decoding a step emits several tokens, so one value covers several | all requests | the saturation analyzer reads 1 minute per pod | the mean over intervals: a long request weighs more than a short one. Saturation divides it by the tokens a step emits, so its line is per token |
| **Engine** (vLLM) TPOT | `vllm:request_time_per_output_token_seconds` | per request, like the router's TPOT, without the router | all requests | the controller reads 1 minute per pod | per request; the capacity planner's fall-back for its TPOT ratio R_q where the router's histogram is missing |
| **Scaling policy** `slo.ttft` | target, `percentile` 50-99 | the router's TTFT | as the router | 5 minutes | a percentile over requests (default p90) |
| **Scaling policy** `slo.tpot` | target, no percentile | the router's TPOT | as the router | 5 minutes | **the mean over requests**. A percentile is a validation error |
| **Saturation** TPOT bound on decode packing | the variant's ITL line, `ITL = A x fill + B` | the engine's ITL, per token, against the pod's mean KV fill | as the engine | the line's readings | the line at the pod's **mean** fill is set equal to `slo.tpot`, the fill held to the ITL band (`analyzerTuning.itlBand`): the mean ITL is held at the target, except at a band edge, and a replica above the mean fill runs above it |
| **Capacity planner** TTFT | `--ttft 2s@p90`, `ttft` in the API | predicted from the engine's prefill and queue, plus the router's hop where it was captured | as captured | the recording or the Prometheus window | the mean or the percentile asked |
| **Capacity planner** TPOT | `--tpot 30ms@p99`, `tpot` in the API | predicted from the decode ITL line at the predicted fill | as captured | the recording or the Prometheus window | the mean, or a percentile: the mean times a ratio R_q read from the router's TPOT histogram, else the engine's (or typed with `--assume-tpot-ratio`) |

## What follows from it

- **A percentile TPOT target cannot be written into a scaling policy.**
  `slo.tpot` is the mean over requests. If your SLO is "p99 TPOT under 30 ms",
  size with the capacity planner at `--tpot 30ms@p99`, then write the mean TPOT
  the planner reads at the replica count you chose into `slo.tpot`. The
  planner warns when it is given a percentile TPOT target. A mean of 20 ms can
  hide a p99 well above 30 ms.
- **The saturation bound and the SLO judgement read different numbers.** The
  bound holds the engine's mean ITL, per token and weighted by intervals, at
  the target. The SLO analyzer judges the router's TPOT, per request and
  averaged over requests. The router's figure adds the time between the
  engine and the client, and counts a short request as much as a long one. A
  fleet the bound holds at a 30 ms ITL can be judged at a router TPOT above
  30 ms.
- **The router's TTFT is longer than the engine's.** It includes the router's
  flow-control queue and the hop to the engine. A TTFT the engine reports
  under target can be judged a miss.
- **A client's figures are longer again,** by its network, and are over the
  client's own run, not a 5-minute window. To score what users got, score the
  client's records.
- **Speculative decoding** emits several tokens per engine step. The engine's
  ITL is then the step time, and saturation corrects it to a time per token
  with the accepted draft tokens. The router's TPOT is per token already.

Related: [the `slo` policy block](scaling-policy.md#slo-observes-bounds-decode-packing),
[how a TPOT target bounds decode packing](scaling-policy.md#how-a-tpot-target-bounds-decode-packing),
[SLO analyzer metrics](prometheus.md#slo-analyzer-metrics),
[the series the capacity planner reads](../guides/capacity-planner/metrics.md).
