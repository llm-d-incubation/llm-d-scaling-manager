# Capacity planner

## Overview

The capacity planner is a web page that answers five questions about one
model's prefill/decode fleet:

1. With this many prefill and decode replicas, what TTFT and TPOT do I get at
   a given request rate?
2. What rate can the fleet carry and still meet the SLO, and which role runs
   out first?
3. What changes if I add a replica, or tighten a target?
4. How many prefill and decode replicas does a given rate need?
5. How many replicas did each hour of a day need, and how many did the fleet have?

It reads what your fleet did, from a window of Prometheus or from a
recording, and prices answers with the same model the `capacity-planner`
command line uses. It is read only: it never changes a replica count, a policy
or a ConfigMap. It runs on your machine, from one binary that serves both the
page and its API. Nothing is installed in the cluster; to share one planner
with a team, [run it in a cluster](in-cluster.md).

This page gets you to a first plan in about five minutes. The rest of the
capacity planner's pages:

| Page | For |
| --- | --- |
| [Run it in a cluster](in-cluster.md) | one planner in a namespace that people open from their browser: port-forward on Kubernetes, a Route with login on OpenShift |
| [Record a window](../record-for-capacity-planner/) | saving a window of Prometheus as a recording: with `capacity-planner`, or with only curl and jq |
| [The HTTP API](api.md) | calling the API yourself: every endpoint, field, flag and error, the plan budget, and Kubernetes authentication |
| [The metrics it needs](metrics.md) | every series it reads, and what your Prometheus must keep |
| [Troubleshooting](troubleshooting.md) | every refusal and error, with its fix |
| [Check the planner on your own data](check.md) | fit it on part of a recording, predict the rest, and compare with what the fleet measured |
| [How accurate each figure is](accuracy.md) | what `validated: error ...` and `OUTSIDE VALIDATED REGION` mean, and what was measured |

## Prerequisites

- [ ] One of: Docker (or Podman); `curl` and `cosign` for a binary (linux or
  darwin, amd64 or arm64, or windows amd64 from v0.12.2 on; before v0.12.2, on
  Windows, the linux binary in WSL); or a clone of
  this repository and Go 1.26 or later.
  `cosign` 3.0 or later checks the image and the binary; 2.4 is enough for the
  binary only.
- [ ] One of:
  - `kubectl` access to Prometheus (a port-forward), and a Prometheus that
    holds the last hour of your model servers' metrics and kube-state-metrics
    ([what it must keep](metrics.md#what-your-prometheus-must-keep));
  - or recordings saved with [Record a window](../record-for-capacity-planner/).
- [ ] The model is served as P/D: one prefill and one decode variant. TTFT is
  priced for P/D only.

## Get the planner

Use the planner of the release your controller runs. The planner replays
recordings with the controller's learners, so each release ships both under
one tag, and `capacity-planner version` prints it. The image and the
binaries ship from v0.11.2 on.

The image serves recordings only, so put one in `./recordings` first: your
own, from [Record a window](../record-for-capacity-planner/), or the sample.
With an empty directory the page says "This server reads no source".

<!-- guide:sample.get start -->
```bash
# A sample recording, from the source tree of the release you run: one P/D model, one prefill and one decode
# replica, an hour of a test run. Or put your own run.jsonl.gz from the record guide in recordings/ instead.
export V=<release, v0.12.0 or later>
mkdir -p recordings
curl -fsSL -o recordings/ols-sweep.jsonl.gz \
  https://raw.githubusercontent.com/llm-d-incubation/llm-d-scaling-manager/${V}/internal/engines/analyzers/learnreplay/testdata/goldens/ols-sweep/recording.jsonl.gz
```
<!-- guide:sample.get end -->

Run the image:

<!-- guide:get.image start -->
```bash
# The image of the release you run. It serves the recordings in the directory you mount, with no authentication.
# --user runs it as you: the recordings the record guide writes are 0600, so the container must run as you to
# read them. -p 127.0.0.1:... keeps the port
# off your network; never publish it beyond 127.0.0.1. Rootless Podman: --userns=keep-id in place of --user,
# and with SELinux mount with :ro,Z.
export V=<release, v0.12.0 or later>
mkdir -p recordings
docker run --rm --user "$(id -u):$(id -g)" -p 127.0.0.1:8080:8080 -v "$PWD/recordings:/recordings:ro" \
  ghcr.io/llm-d-incubation/llm-d-scaling-manager/capacity-planner:${V}
```
<!-- guide:get.image end -->

Keep the port on `127.0.0.1`: the server has no access control of its own
([why](api.md#requests)).

To check the image's signature:

<!-- guide:get.verify start -->
```bash
# Optional. The image is signed by the release workflow, as the release files are. Needs cosign 3.0 or later; cosign 2.x reports no signatures for these images.
cosign verify ghcr.io/llm-d-incubation/llm-d-scaling-manager/capacity-planner:${V} \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/${V}"
```
<!-- guide:get.verify end -->

Then [open the page](#open-the-page). Any other command runs in place of
`serve`, for example `capacity-planner:${V} version`. To read Prometheus
from the container, see [In a container](#in-a-container).

Or download the binary for your platform. The checks are the ones
[the release files](../../reference/install-methods.md#verify-the-release-files)
use:

<!-- guide:get.binary start -->
```bash
# Or the binary for your platform: OS linux or darwin, ARCH amd64 or arm64; on Windows, the .exe below (v0.12.2 or later).
# Check it against SHA256SUMS and its signature (cosign 2.4 or later), as for the other release files. On macOS,
# use shasum -a 256 -c - for sha256sum -c -.
export V=<release, v0.12.0 or later> OS=linux ARCH=amd64
B=capacity-planner-${V}-${OS}-${ARCH}
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/${V}
curl -fsSL -O ${URL}/${B} -O ${URL}/${B}.sigstore.json -O ${URL}/SHA256SUMS
grep " ${B}$" SHA256SUMS | sha256sum -c -
cosign verify-blob ${B} --bundle ${B}.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/${V}"
mkdir -p bin && install -m 0755 ${B} bin/capacity-planner
bin/capacity-planner version
```
<!-- guide:get.binary end -->

On Windows:

<!-- guide:get.windows start -->
```bash
# On Windows (amd64), from Git Bash, which has curl and sha256sum. cosign as above, when you have it.
# The planner commands below run as written in Git Bash.
export V=<release, v0.12.2 or later>
B=capacity-planner-${V}-windows-amd64.exe
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/${V}
curl -fsSL -O ${URL}/${B} -O ${URL}/${B}.sigstore.json -O ${URL}/SHA256SUMS
grep " ${B}$" SHA256SUMS | sha256sum -c -
mkdir -p bin && cp ${B} bin/capacity-planner.exe
bin/capacity-planner.exe version
```
<!-- guide:get.windows end -->

In PowerShell, `Get-FileHash -Algorithm SHA256 <file>` prints the hash to
compare with the file's line in `SHA256SUMS`. On Windows, open the page
with `start http://127.0.0.1:8080/`; `serve --allow-upload` keeps uploads
in `%TMP%` (usually `%TEMP%`), and Ctrl-C stops it and removes them.

Or build it from a checkout:

<!-- guide:get.checkout start -->
```bash
# Or from a clone of the release you run, with Go 1.26 or later.
make capacity-planner
```
<!-- guide:get.checkout end -->

## Start the planner

With the binary or a checkout, pick one source. The server runs until you
stop it.

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
directory. Every flag is in [the API page](api.md#flags).

### In a container

The image's own command serves the recordings mounted at `/recordings`, as
in [Get the planner](#get-the-planner). Name a recording by its file name in
**Source**.

To read Prometheus from the container, use the host's network, so the
container reaches your port-forward, and give the command below, which
listens on `127.0.0.1` ([why not the default command](api.md#in-a-container)).
With the variables set as above:

<!-- guide:container.prometheus start -->
```bash
# The image, reading Prometheus (Linux; on macOS use the binary). --network host lets the container reach your
# port-forward. Never use the image's default command with --network host: it would listen on every interface
# of your machine. Give the command below, which listens on 127.0.0.1 without --listen-all-in-container.
# For an https Prometheus, mount the token and CA files and pass --bearer-token-file and --ca-file.
# serve reads no kubeconfig unless --auth=kubernetes, so none is mounted. The port-forward uses 9091, so it does
# not clash with one already on 9090.
kubectl -n ${PROM_NS} port-forward svc/${PROM_SVC} 9091:9090 &
docker run --rm --network host ghcr.io/llm-d-incubation/llm-d-scaling-manager/capacity-planner:${V} \
  serve --listen 127.0.0.1:8080 --prometheus http://127.0.0.1:9091 --namespace ${NAMESPACE} --kube-state-metrics \
  --engine vllm --engine-args "${ENGINE_ARGS}"
```
<!-- guide:container.prometheus end -->

## Open the page

<!-- guide:open start -->
```bash
# The server listens on your machine only. Leave it running and open the page.
xdg-open http://127.0.0.1:8080/ || open http://127.0.0.1:8080/
```
<!-- guide:open end -->

With the sample recording, open this address instead. It assumes a prefill
rate (V_P, prefill tokens per second per prefill replica), because the
sample has no prefill measurement. With your own recording, leave `vp` out;
only if the page refuses with `NoPrefillCapacity`, type one under **Assume a
term the source did not learn**
([the refusal](troubleshooting.md#refused-figures)).

<!-- guide:sample.open start -->
```bash
# vp=30000 assumes a prefill rate V_P of 30000 tokens/s per prefill replica: the sample has no prefill
# measurement to learn it from. The address also sets targets the sample can meet. If neither command opens
# a browser, paste the address into one.
xdg-open 'http://127.0.0.1:8080/#recording=ols-sweep.jsonl.gz&vp=30000&priceAs=1k&out=1000&ttft=0.5&tpot=0.004' \
  || open 'http://127.0.0.1:8080/#recording=ols-sweep.jsonl.gz&vp=30000&priceAs=1k&out=1000&ttft=0.5&tpot=0.004'
```
<!-- guide:sample.open end -->

The page opens on the last hour of Prometheus, ending now; with recordings
alone, on the first recording. To read another window or recording, open
**Source** at the top, pick it and press **Load**. If the source holds more
than one model, type the model's name; if the model has more than one prefill
or decode variant, type the variant's name too.

When you are done:

<!-- guide:stop start -->
```bash
# Ctrl-C stops the server. Only if you started a port-forward, stop it too, from the shell that started it (%1 is that shell's first background job).
kill %1
```
<!-- guide:stop end -->

## Read the page

- **Fleet, traffic, SLO.** The page starts from now: the replica counts the
  source saw, and the policy's SLO and headroom. The source carries no request
  rate, so the rate starts at the operating point, headroom times the lower
  knee (or the one knee that priced: see "One knee refused" below). Move the rate slider, step the replicas, pick a statistic, set a target.
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
- **One knee refused.** The cap needs both knees. When the source cannot price
  decode's knee (`NoLine`, `PinnedDefaultB`), the cap and the fleet limit are
  refused, not shown at prefill's rate; with no TPOT target, decode's limit is
  too, since nothing else bounds decode. The rate then starts at headroom
  times the prefill knee, and the page says so under the rate: it is a place
  to start, not an operating point. Type your own rate. The prefill curve,
  the prefill limit and the TTFT figures stay. A refused prefill knee works
  the same way, starting from the decode knee. A source with no prefill
  variant at all keeps a cap from decode's knee alone.
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
  that rests on it is flagged `AssumedTPOTRatio`. A learned ratio is read per
  KV-fill band of 0.2 where the source saw at least 5 windows in the band, so
  the tail can widen as load rises. A band with fewer windows uses the ratio
  read over every fill, and figures priced there are flagged
  `GlobalTPOTRatio`. No window is read below its true percentile. Where
  the router's histogram buckets are wide, it is read at the bucket's top, or
  lower where the measured mean proves it, flagged
  `MeanBoundedTPOTPercentile`; p90 and p99 in one wide bucket read the same.
  The ratio is the median window's plus a margin; from 6 windows or fewer, in
  all or in one fill band, it is the worst window, and the CLI says so.
  Finer router TPOT buckets give a firmer figure.
- **Flags and inputs.** Every figure carries the flags it rests on. The
  **Flags** drawer lists each once, in words, with what clears it. The
  **Inputs** drawer shows each term with its source, the terms the source
  refused, and how many you typed.
- **Is this fleet the right size?** Below the graphs, judge the fleet the
  knobs set at the load the source saw.
  1. Add a row per request shape: IL and OL in tokens (`k` for 1024) and,
     optionally, the prefix hit rate. A blank row is the measured shape.
  2. Type the loads, comma separated: `80%` of each shape's knee, `5r/s`, or
     `seen`. Set **Load seen times** to 1.5 to add a column and a line for 50%
     more traffic; the verdict stays at the load seen.
  3. Pick the load seen to judge at: its p95 over the window (the default,
     the busy spells), its mean, or its peak.
  4. Press **Judge the fleet**.

  The answer reads top down:
  - **The load judged**, and the mean, p95 and peak the source saw.
  - **Verdicts**, one sentence per shape at that load: UNDER-PROVISIONED
    (and which role to add), RIGHT-SIZED, or OVER-PROVISIONED (and which
    role can lose a replica), with the fleet it needs in a box: "needs 3
    prefill + 4 decode (has 2 + 2)", other fleets that also serve as "or 4 +
    3", "at least" when prefill is not priced, or "no fleet up to 32
    replicas serves this". With a growth set, a second line under each
    says what the grown traffic does. A typed shape is a what-if, marked "if
    all traffic were this shape". "decode only" marks a verdict the prefill
    side could not price.
  - **The matrix**, one row per shape and one column per load. A cell is the
    TTFT and TPOT at your SLO's statistics, at that load's rate, and a word:
    `ok` every target met, `MISS` a target missed (`MISS at onset` at the
    flow-control onset), `SAT` saturated, `?` not
    judged, `no SLO` no target set. The colour repeats the word. A TPOT
    marked `≤` is the decode line's floor: the load sits below the fill
    the line was fitted over, so the TPOT is at most that. A shape
    outside the lengths the source saw is marked EXTRAPOLATED.
  - **Each shape**, closed until you open it: the knees, how busy decode and
    prefill are at the load, the numbers behind the verdict, and what the
    figures rest on.
  - **How accurate is this?** What no run has validated yet.

  The report uses the knobs above: fleet, targets, headroom and typed terms.
  Change a knob and the page asks you to judge again. A server from before
  the report says so under the button; the rest of the page works. The same
  report is `capacity-planner at-replicas` and `POST /v1/report`.

A refused figure, or an error, is explained in
[Troubleshooting](troubleshooting.md).

### Why decode capacity can drop on prefix-heavy traffic

A prefix-cache hit saves prefill work. It does not save decode memory, unless
another running request holds the same blocks. A request that reuses an old
prefix still keeps its whole context in the KV cache while it decodes. So the
planner prices decode at the KV a running request was measured to hold, not at
the prompt minus its cached part. On traffic with long, mostly cached prompts,
fewer requests fit in the cache than the hit rate suggests, and the decode knee
comes out lower than a planner that trusts the hit rate.

The `kvPerRequest` term says where its figure came from:

| Source | Meaning |
| --- | --- |
| `measured` | Tokens in use over running requests, on the decode rows. |
| `measured, scaled to the shape asked` | That reading, scaled to the prompt and output you typed. |
| `..., above the context` | Added to either one above when the reading is more than twice the whole context. The engine keeps extra KV per request, such as a draft model's. It is priced as read. |
| `measured below the prefix-hit discount, priced at it` | The reading was below the hit-discounted figure, so the discounted figure is used. |
| `measured above the context on SGLang, priced at the context` | SGLang reports its busiest rank against the whole pod, so the reading is held at the whole context. |
| `context, no resident reading` | No usable reading. The whole context is priced: the prompt plus half the output, more where output lengths vary. |

## When a term is refused

A source that did not see enough load cannot learn every term, and the page
then shows refusals instead of numbers. Open **Assume a term the source did
not learn** and type a value: the prefill rate V_P, the fixed latency a, the
prompt cv, the router hop, or decode's ITL B. The learned value, or the reason
it is missing, is shown under each box. A typed value flags every figure that
rests on it. [Each reason and its fix](troubleshooting.md#refused-figures).

Decode refused with `PinnedDefaultB` is the common case on a real P/D window:
the latency line's intercept B, the per-token latency with the cache empty,
was never measured. The **ITL B** box shows the B the floor below the band
gives; type it, or your own. On the CLI, `--assume-itl-b floor` uses that B
and `--assume-itl-b 12ms` states one. Figures on it are flagged
`AssumedITLB` and read as outside the validated region.

A fleet whose load never goes quiet still gets two terms, each flagged:

- **Prefill's fixed latency a, from the engines' own timing**
  (`AFromEngineTiming`). A busy fleet has no quiet minutes to measure a from.
  The planner then fits vLLM's prefill time against the tokens each request
  computed, using the less busy minutes, and adds the time a request spends
  on the prefill pod outside prefill and queueing. The assumed line gives the
  figures it used. The value holds for the prompt length and cache hit rate
  it was measured at. If even the shorter prompts compute more than 2,500
  tokens, a is extrapolated from long prompts and may read low; the assumed
  line says so.
- **A lower KV-fill floor for the decode latency line** (`AdaptedBandFloor`).
  The line is fitted over a range of KV fill. A fleet whose decode pods never
  fill far enough above the default floor (0.15) never gets the 0.30 range a
  fit needs. When at least 3 engines read below the floor, the planner lowers
  it to the 10th percentile of the engines' fills over the last 30 minutes,
  never under 0.05. It raises it back only after the fill has spread 0.35 for
  2 minutes. The flag gives the floor used.

**TTFT on a fleet that never goes quiet is not validated: prefill capacity
V_P is a lower bound.** Such a fleet's prefill pods queue at every load, so
the planner's prefill capacity is under what the fleet serves, and the decode
side's share of TTFT is not priced. TTFT and prefill sizing can be wrong in
either direction; decode TPOT is usable. Every TTFT answer on such a fleet
says so.

Limits of both:

- The quiet-minute test per engine assumes a group's data-parallel ranks
  share its load evenly, and context-parallel ranks (PCP, DCP) are not
  counted as engines.
- The lowered floor is the same on every replay of one recording, but a
  different window of the same fleet can lower it to a different floor.

## Find the fleet a rate needs

Above the controls, pick **What fleet does this traffic need?** Then set the
rate and the targets as before. The page finds the fewest replicas that serve
that rate under your targets, and the replica counts become its answer: you
read them, you do not set them.

- **The answer.** The fewest replicas in all; of two fleets as few, the one
  with fewer decode replicas. Beside it: the fleet limit at that fleet, the headroom
  over your rate, and the role and target that bind.
- **Why not fewer.** One prefill replica fewer, and one decode replica fewer,
  each with the rate it would serve and how far short of yours that is.
- **The worst corner.** The answer meets your rate at the learned prefill
  speed (V_P) and imbalance (f). Those are estimates, and the page also prices
  them 15% either way. When the answer falls short at the worst of those, the
  page says so: "at one corner of V_P and f this fleet serves only X r/s".
  Tick **Size for the worst corner of V_P and f** to find the fleet that
  serves your rate there too. It may need more replicas and takes longer.
  Tick it when V_P is a value you typed rather than one the source learned,
  or when the fleet must hold the rate even if V_P reads high. If the worst
  corner puts the TTFT target under the floor, the page says so: no replica
  count helps, so raise the target or untick the box.
- **Decode admission.** When decode admission holds the prefill limit down,
  the page notes it: there, decode replicas lift TTFT more than prefill
  replicas do, and the answer has weighed the two.
- **The rest of the page** is the fleet it found, exactly as the first mode
  would show it if you set those counts yourself.
- **Search up to.** The most replicas of a role the page tries, 32 unless you
  change it (at most 64). When no fleet within it serves the rate, the page
  says so, with the reason, what the largest fleet would serve, and what to
  change.
- **How long it takes.** Each answer searches several fleets, then draws the
  graphs of the fleet found, which takes most of the time. Expect about 20 s
  on a TTFT fleet from cold, about 25 s at 200 r/s, and about 45 s for the
  worst corner. The page dims the last answer and counts the seconds while it
  works. It prices the rate when you let go of the slider, and a new rate or
  target cancels a size still running.

Switch back to **How much does this fleet serve?** to step the replicas from
the fleet it found.

Each plan has a time limit, 5 minutes unless the server was started with
another `--plan-budget`. A plan past it is refused with `PlanBudgetExceeded`
([the plan budget](api.md#the-plan-budget)).

## Check a fleet at N replicas

To ask "is this fleet the right size for the traffic it saw?", run
`capacity-planner at-replicas` on a recording or a Prometheus window
(`--prometheus` and the capture flags, as for `replicas-to-slo`). Set the fleet
with `--prefill` and `--decode`; without them it judges the recorded counts.

<!-- guide:fleet.check start -->
```bash
# On the staircase sample, downloaded as the first sample was: two prefill and two decode replicas, judged at the
# load it saw, with what that load grown by a fifth does beside it, for its own shape and two it never saw. The
# sample learned no prefill speed, so V_P and a are assumed. Leave out --shape and --load for the measured shape at
# 50%, 80% and 100% of the knee and the load seen.
curl -fsSL -o recordings/ttft-staircase-b-on.jsonl.gz \
  https://raw.githubusercontent.com/llm-d-incubation/llm-d-scaling-manager/${V}/internal/engines/analyzers/learnreplay/testdata/goldens/ttft-staircase-b-on/recording.jsonl.gz
bin/capacity-planner at-replicas --recording recordings/ttft-staircase-b-on.jsonl.gz \
  --assume-vp 200000 --assume-a 20ms --ttft 2s@p90 --tpot 30ms \
  --shape measured --shape 1k/1k --shape 16k/200/0.3 \
  --load 50% --load 80% --load 5r/s --load seen --load-seen-x 1.2
```
<!-- guide:fleet.check end -->

It replays the recording once and answers, for each shape and load:

- **Load judged.** The p95 of the arrival rate the window read, one reading
  per cycle with traffic. The report prints the mean, p95 and peak, and the
  busiest 2 minutes (a rolling mean). `--load-statistic mean|p95|peak` picks
  the one judged; `--load-seen` states a load instead. The mean hides a busy
  part: on the staircase sample it is 31.6 r/s, the p95 41.7 and the peak
  43.2.
- **Growth.** `--load "seen x1.5"` (also `seen×1.5`, `seen*1.5`) asks what
  50% more traffic does: a column of its own and a short `x1.5 (...)` line
  under the verdict. It never changes the verdict at the load seen.
  `--load-seen-x 1.5` is an alias that adds the same column.
- **Summary.** One row per shape (`--shape`, repeatable: `measured`, or
  `IL/OL[/hit]` in tokens, `k` for 1024), one column per load (`--load`,
  repeatable: `80%` of that shape's fleet knee, `5r/s`, `seen`, or
  `seen x1.5`). A cell
  such as `617/5.0 @18.49 ok` is TTFT p90 617 ms and TPOT p90 5.0 ms at
  18.49 r/s, and the SLO's own statistics are met there. The mark is `ok`
  met, `MISS` missed, `MISS*` missed because the rate is past the prefill
  flow-control onset (the router queues) though the figure is within, `SAT`
  saturated, `?` not judged. A TPOT ending in `m` is the mean, where the
  percentile is refused. A shape outside the lengths or prefix-hit rate the
  recording saw is marked EXTRAPOLATED.
- **Verdict per shape**, in one sentence:
  - UNDER-PROVISIONED: the load is past headroom times the fleet knee, or a
    target is missed there.
  - OVER-PROVISIONED: a role has a replica to spare. One fewer of it must
    hold every target at the load judged, and the role's own knee and
    target at the window's peak too, since removing capacity is the costly
    mistake.
  - RIGHT-SIZED: the fleet holds, and no role has a replica to spare.
  - NOT JUDGED: a figure the verdict needs is refused (the sentence says
    which), there is no load, or the prefill side is not priced and decode
    alone holds. With no prefill speed learned, decode alone is judged
    ("(decode only)"): the report never calls such a fleet right-sized or
    over-provisioned, and gives no fleet rate at the SLO.

  UNDER and OVER name the fleet needed: the smallest that holds every
  target at the load with headroom, found by the same search as
  [`POST /v1/size`](api.md#the-size), for example "needs 3 prefill +
  3 decode (has 2 + 2)". A role it cuts must hold at the window's peak
  too, as a spare must, so it never cuts a role the sentence says has no
  spare unless it also adds replicas of the other role, and then it says
  so ("trading prefill for decode"). Other mixes of the same size that
  hold are listed too. With the prefill side not priced it says "needs at
  least 3 decode".
  Each role's standing is in the sentence ("decode: one fewer holds even
  at the peak 43.2 r/s; prefill: no spare (1.9% under its operating
  point)"), with any prefill imbalance f that no run learned at that pod
  count. That f is assumed at 1.5. With `--imbalance-across-counts`
  (opt-in), it is read off the pod counts that were learned, or 1.15 with
  none. The `checks` lines give each fleet tried and its numbers.
- **Per shape:** utilization at the load (decode KV fill, batch slots, wait
  probability; prefill utilization pooled and on the busiest pod), the
  prefill, decode and fleet knees in req/s and output tok/s with what binds
  each, the operating point, the highest rate that holds the targets, and
  the SLO against each load. `e2e mean (sum)` is TTFT mean + (OL - 1) x TPOT
  mean, not a percentile.

The targets are the policy's unless `--ttft` and `--tpot` set them. Exit 0
means the report answered, whatever the verdict, saturated fleets included;
exit 3 means a shape's verdict is NOT JUDGED. Scripts read the verdict from
`-o json`
(`shapes[].verdict.verdict`), which prints the same report as
[`POST /v1/report`](api.md#the-report).

With v0.12.1, the sample above prints (the start of it):

```text
ns-01/model-02  P/D 2 prefill (variant-02) + 2 decode (variant-01)  window 2026-11-06 02:18-03:30Z, cycle 289 of 289
decode engine   vLLM release not recorded
prefill engine  vLLM release not recorded
load judged     41.70 r/s, the p95 of the 282 of 289 cycles with traffic
load seen       per cycle (1-minute rates): mean 31.59, p95 41.70, peak 43.24 r/s; busiest 2m0s (8 cycles): 42.13 r/s
SLO             TTFT p90 2000.0 ms (asked), TPOT mean 30.0 ms (asked)
cell = TTFT p90 ms / TPOT p90 ms @ the rate priced, r/s; - refused (the shape's table says why); m the TPOT mean, where the p90 is refused
mark = the SLO (TTFT p90 2000.0 ms (asked), TPOT mean 30.0 ms (asked)) at that rate: ok met, MISS missed, MISS* missed past the prefill flow-control onset (the router queues) though the figure is within, SAT saturated, ? not judged
summary                     50%                80%                  5r/s              seen            seen x1.2
  measured                  617/5.0 @18.49 ok  1944/10.8 @29.59 ok  388/4.9 @5.00 ok  -/- @41.70 SAT  -/- @50.04 SAT
  1k/1k EXTRAPOLATED        133/4.9 @17.78 ok  152/9.0 @28.45 ok    125/4.9 @5.00 ok  -/- @41.70 SAT  -/- @50.04 SAT
  16k/200/0.3 EXTRAPOLATED  667/5.0 @9.08 ok   1015/10.8 @14.53 ok  567/4.9 @5.00 ok  -/- @41.70 SAT  -/- @50.04 SAT
verdicts
  measured                  UNDER-PROVISIONED: at the p95 load of 41.7 r/s, 1.1x the fleet knee of 37.0 r/s, the fleet saturates; needs 3 prefill + 3 decode (has 2 + 2) (prefill f 1.5 assumed at 3 pods).
                            x1.2 (50.0 r/s): UNDER-PROVISIONED: 1.4x the fleet knee (37.0 r/s); needs 4 prefill + 4 decode (has 2 + 2) (prefill f 1.5 assumed at 4 pods).
  1k/1k EXTRAPOLATED        UNDER-PROVISIONED: at the p95 load of 41.7 r/s, 1.2x the fleet knee of 35.6 r/s, the fleet saturates; needs 1 prefill + 3 decode (has 2 + 2).
                            x1.2 (50.0 r/s): UNDER-PROVISIONED: 1.4x the fleet knee (35.6 r/s); needs 1 prefill + 4 decode (has 2 + 2).
  16k/200/0.3 EXTRAPOLATED  UNDER-PROVISIONED: at the p95 load of 41.7 r/s, 2.3x the fleet knee of 18.2 r/s, the fleet saturates; needs 5 prefill + 6 decode (has 2 + 2) (prefill f 1.5 assumed at 5 pods).
                            x1.2 (50.0 r/s): UNDER-PROVISIONED: 2.8x the fleet knee (18.2 r/s); needs 6 prefill + 7 decode (has 2 + 2) (prefill f 1.5 assumed at 6 pods).
```

Read the `not validated` lines after each shape (not shown above): the decode knee read 44% to 103%
high on the goldens, and TTFT is not validated.

A window whose cycles do not cover its span (a capture or recording with
holes) gets a `window gaps` line under the header line: the cycles the span
should hold, those present, and the largest gap between the cycles either
side of it, ending `WindowGaps` above 5% missing (JSON `source.gaps`). The
load and the figures are read from the cycles present, so a gap over the
busy spell reads the load low: capture the window again. When any gap of
2 minutes or more borders the busiest stretch (a cycle either side of it
lies in the busiest 2 minutes, or is the busiest cycle), the observed load
is refused, `WindowGapAtPeak`: the verdict is `NOT JUDGED` and the command
exits 3. State the load with `--load-seen` to judge anyway. A gap is any
stretch with no cycle for the model, so a model parked at zero, or a cycle
time changed mid-window, reads as one too. A scrape that misses only the
engine's series still leaves a cycle, and is not counted.

## Plan replicas over a day

How many prefill and decode replicas did your traffic need, hour by hour,
and how many did the fleet have? The planner cuts the window into buckets (5
minutes by default), reads each bucket's request rate, sizes it as **What
fleet does this traffic need?** sizes one rate, and sets the replicas the fleet
had beside it. Use a window of a day: a recording of a day, or 24 hours of
Prometheus.

On the page, load the source, set the SLO and headroom as usual, and press
**Plan the day** under the graphs. Three charts share one time axis: the
request rate (the mean of the bucket's cycles, and a wash up to their p95),
then prefill replicas and decode replicas, each as needed against had. A
bucket the fleet had fewer replicas than it needed is washed red; a bucket no
fleet within the bound serves is hatched, in the panel of the role short.
Hover a bucket for every value at that time; the table views list every hour
and every bucket, with why a bucket was not sized.

From the command line, on the sample recording (about an hour, so two hours
in the table):

```bash
# --prometheus URL --namespace NS --kube-state-metrics in place of --recording reads the last 24 hours.
bin/capacity-planner day-plan --recording recordings/ols-sweep.jsonl.gz --model model-02 --assume-vp 30000 --ttft 500ms@p95 --tpot 4ms
```

```text
Day plan for ns-01/model-02, 2026-11-01 21:50 to 2026-11-01 22:55 UTC, buckets of 5m0s, each sized for the p95 of its cycle rates (default; the mean where a bucket holds fewer than 20 cycles)
SLO: TTFT p95 500ms, TPOT mean 4ms; headroom 0.85. TTFT model UNVALIDATED. ...

REFUSED prefill's TTFT (TargetBelowFloor), in 4 buckets: target 0.5 s at or below the TTFT floor 0.6788 s
  what to do: The TTFT target is under the TTFT at zero load: no replica count meets it. Raise the TTFT target above the floor in the detail.
  what the plan did: Prefill is not sized: decode is sized alone, on its TPOT target and its knee times headroom.

each hour: the need of its busiest bucket, the fewest pods it had, and the fleet it had judged whole at each bucket's rate
hour (UTC)        rate mean  rate p95 need P need D  had P  had D
2026-11-01 21:00       0.73      1.00      1      1      1      1  ok
2026-11-01 22:00       6.88     29.68      4      2      1      1  UNDER (SLO risk)

buckets: 11 sized (9 under, 0 over, 2 match; 11 for the p95, 0 for the mean, 0 for the max), 0 beyond the bound, 0 refused, 2 too little data, 0 no data
needed: at the busiest bucket 4 prefill + 2 decode, at the quietest 1 prefill + 1 decode (the fewest replicas in all; another mix may serve too)
replica-hours over the 11 sized buckets: needed 1.75 decode (prefill not priced in 4 of the 11), had 0.92 decode (prefill not priced in 4 of the 11)
had over every bucket with data: 1.08 prefill + 1.08 decode replica-hours, 2.17 GPU-hours (GPUs per replica: 1 prefill, 1 decode)
hours under (SLO risk): 22:00
hours over (waste): none
flags: RateFromDecodeRows, WindowShape, PrefillNotPriced, UnvalidatedTTFT, AssumedVP, ... (every flag the buckets carry)
```

It exits 3: four buckets of 4k-token prompts cannot meet a 500 ms TTFT at
any replica count, so their prefill is not sized and their decode is sized
alone. A higher `--ttft` sizes them.

- **Needed.** The fewest replicas that serve the bucket's rate under the SLO
  and headroom: the fewest in all, ties going to fewer decode replicas, as
  **What fleet does this traffic need?** finds them. Another mix may serve the
  rate too. An hour's need is its busiest bucket's.
- **Sized for.** Each bucket is sized for the p95 of its cycles' rates when it
  holds 20 cycles or more (5 minutes at a 15 s cycle), else for their mean,
  which is less conservative, flagged `FewCyclesForP95`. `--size-for
  mean|p95|max` sets it; the table and each bucket say which was used.
  `--bucket 15m` sets the bucket.
- **Shape.** Each bucket is priced at its own observed prompt and output
  length. A bucket whose cycles read none is priced at the source's shape and
  flagged `WindowShape`. `--assume-input-tokens` and `--assume-output-tokens`
  price every bucket at one stated length, flagged `EXTRAPOLATED` when it lies
  outside what the buckets observed.
- **What the source cannot price.** A prefill rate V_P or a fixed latency a
  it did not learn, a TTFT target under the TTFT at zero load, or a decode line
  it did not fit refuses every rate alike. The plan says so at the top, with
  what to do, and sizes what does price: decode alone when prefill is refused,
  decode without its TPOT when the TPOT is refused. When the decode knee is
  refused (a decode line it did not fit, or no KV capacity or batch cap),
  nothing bounds decode, so, as when nothing prices, no bucket is sized and
  no figure of a need is printed, as `at-replicas` refuses the same fleet. It
  then exits 3.
- **Beyond the bound.** A bucket whose rate no fleet within `--max-prefill`
  and `--max-decode` (32 each) serves is not sized; its hour is UNDER. It
  also exits 3.
- **Too little data.** A bucket with fewer than 30 requests (`--min-requests`)
  is not sized. A bucket no cycle fell in has no data. Neither counts as under
  or over. A plan that sizes no bucket at all exits 3.
- **Gaps in the source.** When the source's cycles do not cover its span (a
  capture or a recording with holes), a note says how many cycles the span
  should hold, how many it has, and the largest gap; above 5% missing it
  ends `WindowGaps`. JSON: `gaps`. A bucket inside a gap has no data, and
  one a gap cuts into is sized from the cycles present: a gap over the
  busiest hours sizes them low. Capture again over the busy hours. When any
  gap of 2 minutes or more borders the busiest sized bucket (it cuts into
  it, or covers a bucket beside it), that bucket is refused,
  `WindowGapAtPeak`, the busiest need is read from the buckets left, and
  the plan exits 3.
- **Had.** The median serving pods of the priced prefill and decode variants
  over the bucket; the rate is the model's (`OtherVariantsServed` where
  another variant also served). The fleet it had is judged whole, at the
  bucket's rate: UNDER (SLO risk) when its limit falls short, over (waste)
  when one replica fewer of either role would still serve it. An hour is
  UNDER when any of its buckets is, and over when every sized bucket is.
  Replica-hours and GPU-hours needed and had are over the sized buckets, and
  left out for prefill when any of them did not price it; the last line is
  what the fleet ran over every bucket with data.
- **Prefill not priced.** Decode is then sized on its own limit alone, a
  lower bound: `at least`, and `>=` in the table.
- **Steady state.** Each bucket is sized as if its rate held. It ignores the
  time a replica takes to start: a fleet that follows the plan must start
  replicas that long before the rate arrives.
- **JSON.** `-o json` prints every bucket, as `POST /v1/dayplan` returns it
  ([the day plan](api.md#the-day-plan)).

## Set your own prompt buckets

The "if all traffic were bucket b" view draws five buckets by default: 1k,
4k, 8k, 32k and 128k tokens. To draw others, tick the view, then type your
edges in **Bucket edges**, for example `2k, 16k, 64k`:

- Each edge is a bucket's largest prompt, in tokens. `16k` is 16384 tokens.
- Give 1 to 8 edges, smallest first, each at most 1024k.
- Leave the box blank for the default five.

Each line is then priced with every prompt at its edge, and named by it. A
line named `2000` is an edge of 2000 tokens; `2k` is 2048. The box shows
your edges by these names, so `2048` becomes `2k`.

There are five line colours. With more than five edges the colours repeat,
and the sixth line on is dashed; the table under the graph names every line.

Each new edge prices a fleet of its own, so a plan with many new edges takes
longer. Edges you already used for this fleet are not priced again.

The edges change only this view: the limits and the +1 steps stay the same.
The flag count can change, since each line brings its own flags.

**Edges past the context window.** A request needs room for its prompt and
its output, so a line whose edge plus the output length passes the engines'
`--max-model-len` is flagged `EdgeBeyondContext`, dimmed and dotted on the
graph, and marked in the table. This needs the source to know the window
(shown as "context window" under **Inputs**); when it does not, nothing is
flagged, so check your edges against it yourself.

The address keeps your edges, so a shared link draws the same lines. If an
edge in the box is wrong, the box is marked and says why under it until you
fix it, and the graphs keep the edges in use. If a link's edges are wrong, the
status line says so, and the page draws the default five.

## Share a scenario

The page keeps the source and every knob in its address, after the `#`. Copy
the address to reopen the same scenario later, against a server that reads the
same source. In the second mode the address keeps the question (the rate and
the targets), not the answer, so it is found again when the page opens.

## Save and compare scenarios

To compare two scenarios:

1. Price the first one: set the knobs and wait for the figures.
2. Press **Pin as A**. The page keeps that plan as A.
3. Change the knobs, or open a saved scenario with **Open**.
4. Read the result. The TTFT and TPOT graphs draw A as a grey dashed line
   beside the current curve, with A's limit point labelled "A: ... r/s". The
   table under the tabs sets A's fleet, limits, binding role and targets
   beside the current ones, with the change: "now refused" or "now priced"
   where only one side has a figure.

A is not priced again as you move the knobs. **Unpin A** clears it. The pin
is not saved or exported: it is gone when the page reloads. Both questions
work: in the second mode, pin the fleet the page found, then switch back and
step the replicas, or change the rate and size again. A size that no fleet
within the bound serves has nothing to pin, and while the current scenario
is being priced, or failed to price, the table says so instead of comparing.

To keep a scenario for later:

- **Save** keeps the scenario on screen in this browser, under the name you
  type. With no name typed, the page makes one from the model, the fleet, the
  rate, the targets and the headroom, dropping the last of these to fit 80
  characters. Saving under a name already used replaces it, and the status
  line says so. **Open** loads a saved one, **Delete** removes it. The page
  keeps at most 50.
- **Export JSON** downloads the scenario as a small file; **Import JSON**
  reads one, saves it and opens it. If another saved scenario already has
  its name, the import is saved as "name (2)", "name (3)" and so on. A file
  that fails the page's checks is refused, with the reason.
- A scenario holds the same text as the address after the `#`, so it opens
  only against a server that reads the same recording or namespace. One this
  server does not read is refused (an import is saved, and not opened), and
  a scenario that fails to load leaves the page as it was.

Saved scenarios live in this browser only, not on the server. A Prometheus
scenario reopens on the last hour ending now, as a shared address does.

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

A proxy in front of a server with no authentication must keep the `Host`
header the browser sent, `127.0.0.1:8080`, or the server answers 403
([why](api.md#requests)); do not expose such a proxy beyond your machine.

Bound to any address other than loopback, the server needs Kubernetes
authentication and TLS (`--auth=kubernetes`). The page is then behind the same
check as the API: a browser must send a bearer token, so open it through a
proxy that adds one. The flags and the RBAC are in
[Kubernetes authentication](api.md#kubernetes-authentication).
