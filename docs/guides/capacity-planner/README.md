# Plan a P/D fleet with the capacity planner

## Overview

The capacity planner is a web page that answers three questions about one
model's prefill/decode fleet:

1. With this many prefill and decode replicas, what TTFT and TPOT do I get at
   a given request rate?
2. What rate can the fleet carry and still meet the SLO, and which role runs
   out first?
3. What changes if I add a replica, or tighten a target?

It reads what your fleet did, from a window of Prometheus or from a
recording, and prices answers with the same model the `capacity-planner`
command line uses. It is read only: it never changes a replica count, a policy
or a ConfigMap.

It runs on your machine, from one binary that serves both the page and its
API. Nothing is installed in the cluster.

## Prerequisites

- [ ] A clone of this repository at the release you run, and Go 1.26 or later.
- [ ] One of:
  - `kubectl` access to Prometheus (a port-forward), and a Prometheus that
    holds the last hour of your model servers' metrics and kube-state-metrics
    ([what it must keep](../../reference/capacity-planner-metrics.md#what-your-prometheus-must-keep));
  - or recordings saved with
    [Record a window for the capacity planner](../record-for-capacity-planner/).
- [ ] The model is served as P/D: one prefill and one decode variant. TTFT is
  priced for P/D only.

<!-- guide:build start -->
```bash
# From a clone of the release you run. Needs Go 1.26 or later. There is no released capacity-planner binary or image.
make capacity-planner
```
<!-- guide:build end -->

## Start the planner

Pick one source. The server runs until you stop it.

From Prometheus:

<!-- guide:start.prometheus start -->
```bash
# From Prometheus. Reach it from your machine first; use the Service your Prometheus has.
# The planner reads the namespace your model servers run in, with the server's own access to Prometheus.
# --kube-state-metrics reads each Pod's role and readiness from kube-state-metrics in the same Prometheus.
# ENGINE_ARGS are the decode Deployment's engine arguments, read as the record guide reads them: kube-state-metrics
# does not export them. Without them the planner does not know what they set, such as the batch cap; the Inputs
# drawer then shows the batch cap's source.
export PROM_NS=<prometheus namespace>
export PROM_SVC=<prometheus service>
export NAMESPACE=<model namespace>
export ENGINE_ARGS="<the decode Deployment's engine arguments, space-separated>"
kubectl -n ${PROM_NS} port-forward svc/${PROM_SVC} 9090:9090 &
bin/capacity-planner serve --prometheus http://localhost:9090 --namespace ${NAMESPACE} --kube-state-metrics \
  --engine vllm --engine-args "${ENGINE_ARGS}"
```
<!-- guide:start.prometheus end -->

Or from recordings:

<!-- guide:start.recordings start -->
```bash
# From recordings you saved with the record guide. Put each recording `generate` wrote (run.jsonl.gz) in one
# directory; the capture (run.capture.jsonl.gz) is not a recording. Every file in the directory is offered by name.
mkdir -p recordings
mv run.jsonl.gz recordings/
bin/capacity-planner serve --recordings ./recordings
```
<!-- guide:start.recordings end -->

Both sources can be served at once: give both flags. The server prints the
address it listens on, and refuses to start if `--recordings` is not a
directory.

<!-- guide:open start -->
```bash
# The server listens on your machine only. Leave it running and open the page.
xdg-open http://127.0.0.1:8080/ || open http://127.0.0.1:8080/
```
<!-- guide:open end -->

The page opens on the last hour of Prometheus, ending now; with recordings
alone, on the first recording. To read another window or recording, open
**Source** at the top, pick it and press **Load**. If the source holds more
than one model, type the model's name; if the model has more than one prefill
or decode variant, type the variant's name too.

When you are done:

<!-- guide:stop start -->
```bash
# Ctrl-C stops the server. Then stop the port-forward, if you started one.
kill %1
```
<!-- guide:stop end -->

## Read the page

- **Fleet, traffic, SLO.** The page starts from now: the replica counts the
  source saw, and the policy's SLO and headroom. The source carries no request
  rate, so the rate starts at the operating point, headroom times the lower
  knee. Move the rate slider, step the replicas, pick a statistic, set a target.
  The critical rate is the total rate: regular and batch traffic are not
  priced apart yet.
- **Targets the policy does not set.** A TTFT target is needed to read a
  prefill limit. When the policy sets none, the page uses TTFT p90 1 s and
  TPOT mean 50 ms, and says "targets set by the page, not the policy" above
  the limits until you set your own.
- **The banner.** Every TTFT figure rests on a model that is not validated. The
  banner says how far off it read on the validation recordings, which are not
  your fleet. Treat TTFT as an estimate, not a bound.
- **The most the fleet serves.** The fleet limit is the lower of the prefill
  limit (from TTFT) and the decode limit (from TPOT), capped at headroom times
  the lower knee. When the cap is what sets it, the figure without headroom is
  shown beside it, with the role that would set it then. The bar shows both
  roles and the fleet on one axis, with the rate you chose. "binds" marks the
  role whose limit sets the fleet figure.
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
  that rests on it is flagged `AssumedTPOTRatio`.
- **Flags and inputs.** Every figure carries the flags it rests on. The
  **Flags** drawer lists each once, in words, with what clears it. The
  **Inputs** drawer shows each term with its source, the terms the source
  refused, and how many you typed.

## When a term is refused

A source that did not see enough load cannot learn every term, and the page
then shows refusals instead of numbers. Open **Assume a term the source did
not learn** and type a value: the prefill rate V_P, the fixed latency a, the
prompt cv or the router hop. The learned value, or the reason it is missing,
is shown under each box. A typed value flags every figure that rests on it.

## Share a scenario

The page keeps the source and every knob in its address, after the `#`. Copy
the address to reopen the same scenario later, against a server that reads the
same source.

## Run it for others

To use a planner running on another host, tunnel to it. The browser then
names `127.0.0.1`, which a server with no authentication answers:

<!-- guide:tunnel start -->
```bash
# From your laptop, to a planner running on a host you reach by SSH. The browser then names 127.0.0.1, which the server answers.
ssh -N -L 8080:127.0.0.1:8080 <user>@<host>
# then open http://127.0.0.1:8080/ on your laptop
```
<!-- guide:tunnel end -->

With no authentication (`--auth=none`, the default) the server answers only
requests addressed to a loopback name. A proxy in front of it must keep the
`Host` header the browser sent, `127.0.0.1:8080`, or the server answers 403;
do not expose such a proxy beyond your machine.

Bound to any address other than loopback, the server needs Kubernetes
authentication and TLS (`--auth=kubernetes`). The page is then behind the same
check as the API: a browser must send a bearer token, so open it through a
proxy that adds one. The flags and the RBAC are in
[Capacity planner serve](../../developer-guide/capacity-planner-serve.md#kubernetes-authentication).

## Troubleshooting

| You see | What it means |
| --- | --- |
| "This server reads no source" | Start it with `--prometheus`, `--recordings` or both. |
| "the model has 2 decode variants (a, b)" | The page prices one P/D fleet. Open **Source**, type one of the names in **Decode variant** (or **Prefill variant**), and press **Load**. |
| Refused figures everywhere, `NoPrefillCapacity` or `NoFixedLatency` | The window had too little load to learn V_P or a. Pick a busier window, or assume the term. |
| "Pricing..." for several seconds | A new replica count, statistic or view prices whole curves. A rate move is fast. |
| `401` | The server checks a bearer token. See [Run it for others](#run-it-for-others). |
| `403` "this server answers loopback names only" | The browser or a proxy named another host. Open `http://127.0.0.1:8080/`, or keep the `Host` header. |
