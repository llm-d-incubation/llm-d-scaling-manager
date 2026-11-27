# Capacity planner: quick start

The capacity planner reads what your prefill/decode fleet did, from
Prometheus or from a recording, and answers:

- **How much traffic can this fleet carry** before TTFT or TPOT misses its
  target, and which role runs out first?
- **Is the fleet the right size** for the load it saw, and if not, how many
  prefill and decode replicas does it need?
- **How many replicas does a given rate need**, or each hour of a day?

It only reads. It never changes a replica count, a policy or a ConfigMap.
One file serves both a web page and a command line. This page takes about
10 minutes, on a sample recording, with nothing installed in a cluster.

## Before you start

- [ ] A Windows (amd64), macOS or Linux computer with `curl`, and internet
  access to GitHub.
- [ ] For your own fleet, later: the model served as P/D (one prefill and
  one decode variant; TTFT is priced for P/D only), and either `kubectl`
  access to a Prometheus that holds the last hour of its metrics and
  kube-state-metrics ([what it must keep](metrics.md#what-your-prometheus-must-keep)),
  or a recording made with [Record a window](../record-for-capacity-planner/).

Use the planner of the release your controller runs: it replays recordings
with the controller's own learners, so both ship under one tag, and
`capacity-planner version` prints it.

## Get the planner

Pick one. Each puts the planner at `bin/capacity-planner` in the directory
you run it from. The steps below run from that same directory.

### Windows

<!-- guide:quick.windows start -->
```powershell
# Windows (amd64), in PowerShell. curl.exe ships with Windows 10 and later. The check prints True when the
# download matches the release's SHA256SUMS.
$V = "<release, v0.12.2 or later>"
$B = "capacity-planner-$V-windows-amd64.exe"
$URL = "https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V"
curl.exe -fsSL -O "$URL/$B" -O "$URL/SHA256SUMS"
((Select-String -Path SHA256SUMS -Pattern " $B$").Line -split " ")[0] -eq (Get-FileHash $B -Algorithm SHA256).Hash.ToLower()
New-Item -ItemType Directory -Force bin | Out-Null; Move-Item -Force $B bin/capacity-planner.exe
bin/capacity-planner version
```
<!-- guide:quick.windows end -->

Download with `curl.exe` as shown. A copy downloaded with a browser carries
the browser's mark, and Windows may warn before it runs;
`Unblock-File bin/capacity-planner.exe` removes the mark.

### macOS or Linux

<!-- guide:quick.unix start -->
```bash
# macOS or Linux. OS is darwin or linux; ARCH is arm64 (Apple silicon) or amd64 (Intel). The check prints OK
# when the download matches the release's SHA256SUMS.
export V=<release, v0.12.0 or later> OS=darwin ARCH=arm64
B=capacity-planner-${V}-${OS}-${ARCH}
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/${V}
curl -fsSL -O ${URL}/${B} -O ${URL}/SHA256SUMS
grep " ${B}$" SHA256SUMS | shasum -a 256 -c -
mkdir -p bin && install -m 0755 ${B} bin/capacity-planner
bin/capacity-planner version
```
<!-- guide:quick.unix end -->

On Linux, `sha256sum -c -` works in place of `shasum -a 256 -c -`. On macOS,
a binary downloaded with `curl` runs as is. One downloaded with a browser is
quarantined, and macOS refuses to open it;
`xattr -d com.apple.quarantine bin/capacity-planner` lets it run.

### In a cluster, for a team

One planner in a namespace, that people open from their own browser. This
is [Run it in a cluster](in-cluster.md); its first section is the same
three steps as here: install, load a recording, open. On OpenShift the
planner sits behind a Route and the cluster's login instead of a
port-forward ([OpenShift](in-cluster.md#openshift)).

Other ways (the container image, cosign signatures, Git Bash on Windows, a
build from source) are in [every way to get and run it](install-options.md).

## Ask it about a sample

Download two sample recordings into `recordings/`.

<!-- guide:sample.get start -->
```bash
# Two sample recordings, from the source tree of the release you run. ols-sweep: one prefill and one decode
# replica, an hour of a test run. ttft-staircase-b-on: two and two, 72 minutes of rising load.
export V=<release, v0.12.0 or later>
mkdir -p recordings
curl -fsSL -o recordings/ols-sweep.jsonl.gz \
  https://raw.githubusercontent.com/llm-d-incubation/llm-d-scaling-manager/${V}/internal/engines/analyzers/learnreplay/testdata/goldens/ols-sweep/recording.jsonl.gz
curl -fsSL -o recordings/ttft-staircase-b-on.jsonl.gz \
  https://raw.githubusercontent.com/llm-d-incubation/llm-d-scaling-manager/${V}/internal/engines/analyzers/learnreplay/testdata/goldens/ttft-staircase-b-on/recording.jsonl.gz
```
<!-- guide:sample.get end -->

<!-- guide:sample.getWindows start -->
```powershell
# The same two samples, in PowerShell, with $V set as above.
New-Item -ItemType Directory -Force recordings | Out-Null
$S = "https://raw.githubusercontent.com/llm-d-incubation/llm-d-scaling-manager/$V/internal/engines/analyzers/learnreplay/testdata/goldens"
curl.exe -fsSL -o recordings/ols-sweep.jsonl.gz "$S/ols-sweep/recording.jsonl.gz"
curl.exe -fsSL -o recordings/ttft-staircase-b-on.jsonl.gz "$S/ttft-staircase-b-on/recording.jsonl.gz"
```
<!-- guide:sample.getWindows end -->

Ask whether the staircase sample's fleet, two prefill and two decode
replicas, was the right size for the traffic it saw:

<!-- guide:sample.ask start -->
```bash
# Judge the staircase sample's fleet at the load it saw. The same command in PowerShell. Takes a few seconds.
bin/capacity-planner at-replicas --recording recordings/ttft-staircase-b-on.jsonl.gz
```
<!-- guide:sample.ask end -->

### Look at three things first

With v0.12.3, it prints (lines cut short with `...`; the full answer, line
by line, is in [Read the planner's answer](read-the-answer.md)):

```text
Self-check not run: too few steps were scored: a longer recording, with busy minutes in its first half, gives the planner more to fit and to check (TTFT refused NoPrefillCapacity in 7 steps; TPOT refused PinnedDefaultB in 7 steps). [notRun]
...
verdicts
  measured  UNDER-PROVISIONED (decode only): at the p95 load of 41.7 r/s, 1.1x the fleet knee of 37.0 r/s, the fleet saturates; needs at least 3 decode (has 2), 6 decode if the decode knee reads 99% high.
...
capacity            req/s     out tok/s   bound by
  prefill knee      no prefill speed learned            P1 refused V_P of variant-02: no prefill speed learned; pass --assume-vp for a flagged answer
  decode knee       36.98     7396        decode band top at fill 0.798
                    OUTSIDE VALIDATED REGION (ExtrapolatedFill: an input the run did not price)
  fleet knee        36.98     7396        decode (prefill not priced)
```

1. **The knee: how much the fleet carries.** The `fleet knee` is the rate
   at which the fleet saturates: 37 requests per second, set by decode.
   The policy's headroom puts the fleet's operating point at 0.85 of it.
2. **Predicted against measured: can you trust it on this fleet?** The first
   line is the self-check. The planner learns from the start of a
   recording, predicts the rest from its traffic alone, and compares with
   what the fleet measured. On this sample it scores no step, so it says
   `[notRun]`: no evidence either way. The sample learned no prefill
   speed, so TTFT is not predicted, and no full fit of decode's latency
   line (`PinnedDefaultB`), so TPOT is not either. A recording with busy
   minutes in its first half gets a grade
   ([trust levels](read-the-answer.md#trust-levels-ok-weak-inconsistent)).
3. **The verdict: what to do.** `UNDER-PROVISIONED`: the busy minutes ran
   past the knee. It needs at least 3 decode replicas. "At least" because
   prefill was not priced; with a prefill speed it names a full fleet.

v0.12.2 gives the same knee and verdict, but prints no decode count at the
knee's worst error.

## Open the page

The page answers the same questions with sliders and graphs. Start it on
the samples, and leave it running:

<!-- guide:sample.serve start -->
```bash
# Serve the page on the samples. It listens on 127.0.0.1:8080, on your machine only, until Ctrl-C. If 8080 is
# busy, add --listen 127.0.0.1:8090 and use that port below. The same command in PowerShell.
bin/capacity-planner serve --recordings ./recordings
```
<!-- guide:sample.serve end -->

Then open the page on the other sample. The address assumes a prefill
speed, because this sample has none to learn, and sets targets it can meet:

<!-- guide:sample.open start -->
```bash
# vp=30000 assumes a prefill rate V_P of 30000 tokens/s per prefill replica: the sample has no prefill
# measurement to learn it from. The address also sets targets the sample can meet. If neither command opens
# a browser, paste the address into one.
xdg-open 'http://127.0.0.1:8080/#recording=ols-sweep.jsonl.gz&vp=30000&priceAs=1k&out=1000&ttft=0.5&tpot=0.004' \
  || open 'http://127.0.0.1:8080/#recording=ols-sweep.jsonl.gz&vp=30000&priceAs=1k&out=1000&ttft=0.5&tpot=0.004'
```
<!-- guide:sample.open end -->

<!-- guide:sample.openWindows start -->
```powershell
# In a second PowerShell window. The address is the one above.
Start-Process 'http://127.0.0.1:8080/#recording=ols-sweep.jsonl.gz&vp=30000&priceAs=1k&out=1000&ttft=0.5&tpot=0.004'
```
<!-- guide:sample.openWindows end -->

On the page, the same three things sit here:

- **The knee:** "The most the fleet serves", the bar under the controls,
  and the shaded region of the graphs past it.
- **Predicted against measured:** the self-check badge beside **Source**,
  with its lines under it. It reads `not run` on both samples: this one
  is under an hour, and the staircase learned no prefill speed and no full
  fit of decode's latency line. A recording of your own fleet gets a grade.
- **The verdict:** press **Judge the fleet** under the graphs.

Ctrl-C in the server's window stops it.

## Point it at your own fleet

**From Prometheus, live.** Port-forward to your Prometheus and serve the
namespace your model servers run in:

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

**From a recording.** Save a window of your fleet with
[Record a window](../record-for-capacity-planner/), put the `run.jsonl.gz`
it writes into `recordings/`, and ask about it as you asked about the
sample:

```bash
bin/capacity-planner at-replicas --recording recordings/run.jsonl.gz
```

A recording also travels: send it to whoever runs the planner. A source that
did not see enough load cannot learn every term; the answer then says which
and what to pass ([refused figures](troubleshooting.md#refused-figures)).

## Next

| Page | For |
| --- | --- |
| [Read the planner's answer](read-the-answer.md) | the sample's answer, line by line: knees, verdicts, "at least", the self-check, OUTSIDE VALIDATED REGION |
| [The page and the commands](page-and-commands.md) | every panel of the page; sizing a rate, a day plan, spares and quota, scenarios |
| [Every way to get and run it](install-options.md) | the container image, signatures, Git Bash, a build from source, Prometheus from a container, a planner on another host |
| [Run it in a cluster](in-cluster.md) | one planner in a namespace for a team: port-forward on Kubernetes, a Route with login on OpenShift |
| [Record a window](../record-for-capacity-planner/) | saving a window of Prometheus as a recording: with `capacity-planner`, or with only curl and jq |
| [Check the planner on your own data](check.md) | fit it on part of a recording, predict the rest, and compare with what the fleet measured |
| [How accurate each figure is](accuracy.md) | what `validated in-sample: error ...` and `OUTSIDE VALIDATED REGION` mean, and what was measured |
| [Troubleshooting](troubleshooting.md) | every refusal and error, with its fix |
| [The HTTP API](api.md) | calling the API yourself: every endpoint, field, flag and error |
| [The metrics it needs](metrics.md) | every series it reads, and what your Prometheus must keep |
