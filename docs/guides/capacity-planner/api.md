# Capacity planner: the HTTP API

The page the [capacity planner](README.md) serves draws what its HTTP API
returns. You can call the same API yourself: resolve a source once, then ask
for plans against it. Every figure is one the `capacity-planner` command line
also prints, from the same code.

This page lists every endpoint, flag, field and error. How the server is built
(its caches, the search, the page's scripts, the tests) is in the developer
page, [Capacity planner serve](../../developer-guide/capacity-planner-serve.md).

## Prerequisites

- [ ] `capacity-planner` from [Get the planner](README.md#get-the-planner).
- [ ] `curl`.
- [ ] A source: a directory of recordings ([record one](../record-for-capacity-planner/)),
  or a Prometheus you can reach ([what it must keep](metrics.md#what-your-prometheus-must-keep)).

## Ask for a plan

Start the server on a recordings directory. It listens on `127.0.0.1:8080`,
prints the address on stderr, and stops on Ctrl-C:

```bash
bin/capacity-planner serve --recordings ./recordings
```

Or on Prometheus, through a port-forward (both sources can be served at once):

```bash
bin/capacity-planner serve --prometheus http://localhost:9090 --namespace llm --kube-state-metrics
```

Resolve the inputs. A recording is named by its file name in `--recordings`,
never by a path:

```bash
curl -s localhost:8080/v1/inputs -H 'Content-Type: application/json' -d '{"model": "model-02",
  "source": {"kind": "recording", "name": "ols-sweep.jsonl.gz"}}'
```

With no `source`, the inputs are a Prometheus window of the last hour, ending
now. A window of your own:

```bash
curl -s localhost:8080/v1/inputs -H 'Content-Type: application/json' -d '{"model": "qwen3-32b",
  "source": {"kind": "prometheus", "namespace": "llm",
             "from": "2026-11-09T10:00:00Z", "to": "2026-11-09T11:00:00Z"}}'
```

`model` may be left out when the source holds one model. When a model has
more than one variant of a role, name the one to price with `prefillVariant`
and `decodeVariant` beside `model` (the CLI's `--prefill-variant` and
`--variant`). The error that asks for one lists the names.

`prefillVPPlateau: true` reads V_P from the occupancy plateau where the
prefill pods never queued (the CLI's `--prefill-vp-plateau`). It is
experimental and off by default: every figure priced from that V_P is
flagged `VPFromPlateau` and its `validation` reads OUTSIDE VALIDATED REGION,
the inputs' `assumed` says "prefill speed from the occupancy plateau
(experimental, --prefill-vp-plateau)", and the answer carries
`prefillVPPlateau: true`. A plateau that has not flattened is a lower bound,
flagged `VPLowerBound` as well. It gets its own `inputsId`, so plans,
reports, backtests and day plans on it never mix with the default's
([the page](page-and-commands.md#when-a-term-is-refused)).

`prefillHitAtCount: true` prices the prefix hit at each prefill count asked
for, not the one measured (the CLI's `--prefill-hit-at-count`; off by
default). At fewer prefill replicas than the source's the hit is the floor
hit x N'/N and each request computes more prompt tokens, flagged
`PrefixHitAtCount`; at more it is kept, flagged `PrefixHitHeld`. Where the
source holds vLLM's KV residency histograms, the hit at both fewer and more
replicas is the reuse-gap rule's, and the answer carries the fitted law as
`prefillHitModel`. The answer carries `prefillHitAtCount: true` and its own
`inputsId`.

`prefillVPServiceLine: true` reads V_P from the prefill service line
where the prefill pods never queued, or V_P is only a lower bound under the
line (the CLI's `--prefill-vp-service-line`). It is off by default and not
validated: every figure's `validation` reads OUTSIDE VALIDATED REGION, and
the answer carries `prefillVPServiceLine: true` and its own `inputsId`.

`prefillFLoadCurve: true` prices f, the prefill imbalance, against the load
per pod at each prefill count whose fit is identified (the CLI's
`--prefill-f-load-curve`). It is off by default and validated on lab
recordings only: every figure's `validation` reads OUTSIDE VALIDATED REGION,
and the answer carries `prefillFLoadCurve: true` and its own `inputsId`.

`experimentalTTFT: true` prices TTFT with the experimental TTFT model (the
CLI's `--experimental-ttft`): the seven TTFT switches that are off by
default, on for this source. It is for demonstration and not validated:
every prefill and TTFT figure is flagged `ExperimentalTTFT` and its
`validation` reads OUTSIDE VALIDATED REGION, the inputs' `assumed` lists
the switches, and the answer carries `experimentalTTFT: true`. It gets its
own `inputsId`, so plans, reports, backtests and day plans on it never mix
with the default's ([the page](page-and-commands.md#when-a-term-is-refused)).

The answer carries an `inputsId`. Price a scenario against it:

```bash
curl -s localhost:8080/v1/plan -H 'Content-Type: application/json' -d '{"inputsId": "sha256:...",
  "fleet": {"prefill": 1, "decode": 1},
  "traffic": {"classes": {"critical": {"rate": 6}}, "mix": {"promptTokens": 1000, "outputTokens": 1000}},
  "slo": {"ttft": {"statistic": "p95", "target": 0.5}, "tpot": {"statistic": "mean", "target": 0.004}},
  "assume": {"vp": 30000}}'
```

Everything but `inputsId` has a default: the recorded replica counts, the
observed mix, the policy's SLO and headroom, and as the rate the operating
point (headroom times the lower knee). A TTFT target has no default when
the policy sets none, as in the samples: send `slo.ttft`, or the plan is a
400. The answer's `limits.fleet` is the most the fleet serves under the
targets, here about 11 r/s, so 6 r/s holds.

## Endpoints

| Method and path | Does |
| --- | --- |
| `POST /v1/inputs` | Resolves a source for one model: a recording replayed to a cycle, or a Prometheus window captured in memory (seconds). Returns `inputsId` and every term with its source. |
| `POST /v1/plan` | Prices a scenario against an `inputsId`. |
| `POST /v1/size` | The fewest prefill and decode replicas whose fleet limit reaches a rate under the SLO, with the plan at that fleet; or why no fleet within the bound does. |
| `POST /v1/report` | One fleet judged at the load the source saw, for each request shape and load asked: utilization, knees, the SLO against load, a shape-by-load summary and a verdict per shape. The same report as `capacity-planner at-replicas -o json`. |
| `POST /v1/dayplan` | The prefill and decode replicas each bucket of the source's window needed, against the replicas it had: [the day plan](#the-day-plan). |
| `POST /v1/backtest` | The planner fitted on one window, predicting another step by step beside what the fleet measured: [the backtest](#the-backtest). |
| `POST /v1/predict` | A window predicted from its load alone, as a prediction file to keep: [predict and compare](#predict-and-compare). |
| `POST /v1/compare` | A prediction file scored against the same window measured: [predict and compare](#predict-and-compare). |
| `GET /v1/sources` | What the server reads: whether it reads a Prometheus, the namespaces it may read and the default one, and the file names in `--recordings` (sorted, at most 1000, `recordingsTruncated` true past that). A listed file that is not a learner recording is refused when loaded. Every user the API answers sees all of these. With `--allow-upload`, also the uploads (first, newest first), `uploads[]` with each one's `fileName`, `bytes` and `uploadedAt`, and `upload.maxBytes`. |
| `POST /v1/recordings` | Uploads a recording, with `--allow-upload` only: [Upload a recording](#upload-a-recording). |
| `GET /` | The page, with its files under `/page/`. |

Any other path is a 404.

## Flags

| Flag | What it does |
| --- | --- |
| `--recordings DIR` | Recordings a `recording` source names by file name. Serve refuses to start if it is not a directory. |
| `--prometheus URL` | The Prometheus a `prometheus` source reads. With it, every [capture flag](../record-for-capacity-planner/README.md#capture-flags) but `--from`, `--to` and `-o` applies: the role map, `--step`, `--policy`, the engine, TLS and the bearer token. The window comes with each request. |
| `--namespace NS` | The namespace a `prometheus` source reads when the request names none. |
| `--allowed-namespaces NS,...` | The namespaces a `prometheus` source may read. Default `--namespace` alone; with `--prometheus` one of the two is required. Another namespace is a 403; one that is not a DNS-1123 label a 400. |
| `--batch-cap N` | The engine's batch cap a window is captured with, as `capture --batch-cap`. |
| `--listen ADDR` | Where to serve. Default `127.0.0.1:8080`. Any address that is not loopback needs `--auth=kubernetes`, or for the unspecified address `--listen-all-in-container`; serve refuses to start without one. |
| `--listen-all-in-container` | Only for a container whose port is published to the host's loopback (`-p 127.0.0.1:8080:8080`). With `--auth=none` it allows `--listen` on `0.0.0.0:PORT`, `[::]:PORT` or `:PORT`, and nothing else. Refused in a Kubernetes Pod (`KUBERNETES_SERVICE_HOST` set). Serve prints a warning. The image's default command sets it. Never set it where the port is reachable from a network. |
| `--auth none\|kubernetes` | `none`, the default, is loopback only. `kubernetes` checks a bearer token: see [Kubernetes authentication](#kubernetes-authentication). |
| `--tls-cert-file`, `--tls-key-file` | The server's certificate and key. Required with `--auth=kubernetes`. |
| `--token-audiences A,...` | The audiences a TokenReview checks a token for. Default: the API server's own. |
| `--shutdown-timeout` | How long requests in flight may run after Ctrl-C. Default 10s. |
| `--plan-budget` | How long one plan may be priced. Default 5m; 0 is the default too. At most 9m. See [The plan budget](#the-plan-budget). |
| `--max-inputs N` | How many sources are loaded at once. Default 2. Each holds its replay's memory while it loads. Past it, `POST /v1/inputs` is a 503 with `Retry-After`. |
| `--inputs-cache-size SIZE` | The most memory the loaded sources kept may take in all, such as `256Mi` (the default). Each source's memory is an estimate, not a measure. At most 16 are kept. The least used is dropped first; the newest is always kept. |
| `--allow-upload` | Lets people upload a recording from the page, or with `POST /v1/recordings`. Off by default. See [Upload a recording](#upload-a-recording). |
| `--upload-max-size SIZE` | With `--allow-upload`, the largest upload, such as `256Mi` (the default). |
| `--experimental-ttft[=v2\|=v2nl-line]` | Alone, every source is read as if the request asked for `experimentalTTFT`. `=v2` instead prices every backtest that names no `ttftModel` with `rank-mixture`, and every source is loaded as if it asked for `ttftModel` `rank-mixture` (the main view's TTFT). `=v2nl-line` does the same with `rank-mixture-nl-line`. Experimental, not validated. See [Serve with the experimental v2 models](page-and-commands.md#serve-with-the-experimental-v2-models). |
| `--rank-mixture-stratified=false` | With `--experimental-ttft=v2`, or a request naming `ttftModel` `rank-mixture`: price the model from each prefill rank's own random draws, the older way, where the request does not send `rankMixtureStratified`. On by default ([accuracy](accuracy.md)). |
| `--prefill-vp-plateau`, `--prefill-hit-at-count` | The planner's opt-in switches, by the names the other commands give them: every source is read as if its request asked for `prefillVPPlateau` or `prefillHitAtCount`. The inputs echo the switch and their `inputsId` names it, and every figure it moves carries its own flag and reads outside the validated region. A request may turn a switch on, never off. Off by default. |
| `--rank-mixture-kv-hold`, `--rank-mixture-remainder-spread` | With `--experimental-ttft=v2nl-line` only (a usage error without it): the variant's two modes, on for every backtest and source priced with the variant whose request does not set `rankMixtureKVHold` or `rankMixtureRemainderSpread`. Give them together ([accuracy](accuracy.md)). Each changes the `inputsId`. |
| `--rank-mixture-remainder-server` | With `--experimental-ttft=v2nl-line` and `--rank-mixture-remainder-spread` only (a usage error without them): the variant's remainder server, on for every backtest and source priced with the variant whose request sends `rankMixtureRemainderSpread` (or takes it from the server) and does not set `rankMixtureRemainderServer` ([accuracy](accuracy.md)). Changes the `inputsId`. |
| `--rank-mixture-min-fit-window D` | With `--experimental-ttft=v2` or `=v2nl-line`: the shortest fit window the model prices from, where a request names none (`rankMixtureMinFitWindow`). Default `12h`, under which the model abstains. Lower it (`2h`) to fit the model on a lab recording; every TTFT it prices from a window under 12 h is flagged `RankMixtureShortFitWindow`. Changes the `inputsId`. |
| `--tpot-dist-min-fit-steps N` | With `--experimental-tpot=v2`: the fewest five-minute fit steps the TPOT distribution fits on, where a request names none (`tpotDistMinFitSteps`). Default 24 (two hours), at least 4. Lower it (8) to fit the distribution on a lab recording; every percentile it prices from fewer than 24 steps is flagged `TPOTDistributionShortFit`. Changes the `inputsId`. |
| `--experimental-tpot=v2` | Every backtest that names no `tpotModel` is priced with `renewal-lognormal`, and every source is loaded as if it asked for `tpotModel` `renewal-lognormal` (the main view's TPOT p90 and p95). `=v2-mix` does the same with `renewal-lognormal-per-length`, the per-length mixture. Experimental, not validated. See [The v2 TPOT curve](page-and-commands.md#the-v2-tpot-curve). |

No request can name a host or a file path: the Prometheus URL is a flag, and a
recording is a file name inside `--recordings`, or the name the server gave an
upload. An error names no server path.

## Requests

Every request body is one JSON value of at most 1 MiB, sent as
`Content-Type: application/json`. An unknown field, or a second JSON value, is
a 400. An upload is the one exception: see below.

With `--auth=none` the server answers only requests addressed to a loopback
name (`localhost`, `127.0.0.0/8`, `[::1]`), so a web page that rebinds its own
name to 127.0.0.1 cannot read it. This holds with `--listen-all-in-container`
too. It is not access control: any client that reaches the port can send
`Host: localhost`. Only where you publish the port bounds who can ask.

## Upload a recording

Start the server with `--allow-upload`. On the page, pick the file under
**Upload a recording** and press **Upload**; the page then loads it. With curl:

```bash
curl -sS -X POST --data-binary @qwen3-32b.jsonl.gz \
  -H 'Content-Type: application/octet-stream' 'http://localhost:8080/v1/recordings?filename=qwen3-32b.jsonl.gz'
```

```json
{"schemaVersion": 1, "name": "upload:3f9a2b1c4d5e6f703f9a2b1c4d5e6f703f9a2b1c4d5e6f703f9a2b1c4d5e6f70.jsonl.gz", "bytes": 95114025,
 "fileName": "qwen3-32b.jsonl.gz", "uploadedAt": "2026-11-20T14:05:00Z"}
```

Load it as any recording, with `"source": {"kind": "recording", "name": "upload:3f9a2b1c4d5e6f703f9a2b1c4d5e6f703f9a2b1c4d5e6f703f9a2b1c4d5e6f70.jsonl.gz"}`.

- **Who can upload is who can open the planner.** Every user who can open it
  also sees every upload, as they see `--recordings`. With
  `--auth=kubernetes`, an upload needs `post` on `/whatif/v1/recordings`
  (below); `get` on `/whatif/*` does not allow it.
- **Kept as sent.** The planner does not rename anything in the file. To hide
  real names, upload an exported copy ([Real names in
  recordings](in-cluster.md#real-names-in-recordings)).
- **Where it goes.** A directory of the server's own, mode 0700, under
  `TMPDIR` (else `/tmp`). The name is `upload:` and the SHA-256 of the
  file's bytes. The optional `filename` is a label only: the page and
  `GET /v1/sources` show it beside the upload, its size and time, so
  people can tell uploads apart. Of its last path element, letters,
  digits, up to two accents after a letter, and ` ._-+()[],=@` are kept,
  anything else becomes `_`; trimmed, at most 80 characters. It names no
  file, and everyone who can open the planner sees it. It is not tied to
  who uploaded: the same bytes sent again, by anyone, under another name
  take that name; sent with none, they keep theirs. Either way they count
  as uploaded now, and newest. The last 4 uploads are kept; an
  older one is deleted, once no load is reading it. All are deleted
  when the server stops. A server that starts deletes what a stopped one left
  in the same `TMPDIR`, as after a container restart.
- **Send** `.jsonl` or `.jsonl.gz` bytes as `application/octet-stream` or
  `application/gzip`. Another type is a 415: a page on another site cannot
  send these types without asking the server first, and the server never
  answers that.
- **The same file twice** is a 200 with the name it already has, and counts
  as the newest upload again; a new one is a 201.
- **An upload may take 15 minutes** to arrive and be checked; other requests
  still have 30 s. An upload that sends nothing for 60 s is stopped with a
  408, so a stalled client does not hold the one upload slot.

The server reads every line before it keeps the file, and refuses:

| Status | When |
| --- | --- |
| 403 | The server was started without `--allow-upload`. |
| 413 | Larger than `--upload-max-size`; gunzips to more than 16 times it (4 GiB by default); or has a line longer than 64 MiB. |
| 400 | Not a learner recording: not JSON lines, a gzip cut short, no start line, or empty. |
| 408 | Nothing arrived for 60 s, or the upload took more than 15 minutes. |
| 503 | Another upload is being read. Ask again. |
| 500 | The server could not write the file to its disk. The response names no server path; the server's log has the detail. |

Loading an upload is refused with a 413 when the memory the loaded recording
would take, as estimated, is more than `--inputs-cache-size`, so one upload cannot push every
other loaded source out. A recording in `--recordings` is not refused this
way.

## The inputs

`POST /v1/inputs` returns:

| Key | What it is |
| --- | --- |
| `inputsId` | `sha256:` and a hash of the recording's bytes, the cycle, the model and the variants. The same source gives the same id. |
| `source` | The kind, the recording's name or the window, the cycle read and how many the recording holds. |
| `fleet` | The prefill and the decode variant, with their replica counts at the cycle, `gpusPerReplica` where the source states it, and `startSeconds`, how long a new replica of the role took to become Ready as the source's start learner measured it (its slowest measured variant of the role; `startSeeded` true where it is a ScaledObject's seed, nothing measured), absent where none was learned. |
| `observed` | The mean prompt and output and the prompt's spread; the default buckets (1k, 4k, 8k, 32k, 128k, each named by its upper edge) and the one the mean prompt falls in. The request rate and the share per bucket are refused (`NoPerRequestLengths`): the source carries neither. |
| `policy` | The policy's TTFT and TPOT objectives, the headroom (`scaleUpThreshold`), the flow-control state, and `router`: what the router scores, read from its own series (see [Router policy](page-and-commands.md#router-policy)), absent where the source holds none of them. `router.policy` is `cache-aware and load-aware`, `cache-aware`, `load-aware`, `neither`, one side with the other `not detected`, or `not detected`; `cacheAware`, `loadAware`, `loadAwareByHit` and `loadAwareBySpread` are each `yes`, `no` or `not detected`, with the reason and the evidence (kept share at light and heavy load, spread counts). It sets no price. |
| `terms` | V_P, a, f, the prompt and its cv, the KV transfer, the router hop, the ITL line, the KV capacity and per-request KV, the batch cap, the context window (`maxModelLen`), and R_q at p90: each with its source, or the refusal that stopped it. |
| `assumed` | Every input the source did not give, in the CLI's words. |
| `measured` | Where the source has router histograms: its busiest UTC hour (`from`, `to`, its mean `rate` and its `requests`) and the TTFT and TPOT the router measured over it, `ttft` and `tpot` at the `statistic` `mean`, `p90` and `p95`. Each gives its `value` in seconds, or the `bucket` (`lo`, `hi`) that holds it where the bucket is too wide to place it, or `why` it is not given (above the top bucket, too few requests, not measured). It prices nothing: the answer panel shows it as what users saw. Absent with no histograms or under 10 cycles in the hour. |
| `ttftModel` | With the request's `ttftModel` `rank-mixture` or `rank-mixture-nl-line` (or `serve --experimental-ttft=v2` or `=v2nl-line`): the experimental TTFT model fitted on the source, as a backtest fits it, which the plan, the size and the report then price TTFT with. `model` names which one, and `label` says so too (`Experimental TTFT model` for v2, `Experimental TTFT model v2nl-line` for the variant). The inputs request also takes `rankMixtureKVHold`, `rankMixtureRemainderSpread`, `rankMixtureRemainderServer` and `rankMixtureMinFitWindow`, as [the backtest](#the-backtest) does, each the server's default where absent; each changes the `inputsId`, and the fit reports `shortFitWindow` where the window was under 12 h. It also carries `abstained` (why it prices nothing; the planner's TTFT then stands), `heldOut` (on the source's held-out self-check folds, at the `statistic` `mean`: `within` 20% and read `low` of `steps`, the `medianRatio`, the rates `rateFrom` to `rateTo` and the recorded `prefill` and `decode` fleet), `evidence` (that in words), `measuredUpTo` (the highest step rate served, r/s) and `fit`. The inputs request's `ttftModel` names the model; it changes the `inputsId`. Its `rankMixtureStratified` false prices the model from each rank's own draws, as in [the backtest](#the-backtest), and changes the `inputsId` too. The plan's `ttft.model` says whether it priced that fleet (`priced`, else `notPriced`), and each figure past `measuredUpTo` carries `extrapolated` true. The report carries the same `ttftModel`. |
| `tpotModel` | With the request's `tpotModel` `renewal-lognormal` (or `serve --experimental-tpot=v2`): the experimental TPOT distribution fitted on the source, which the plan, the size and the report then price TPOT p90 and p95 with, around the planner's own mean. `label`, `abstained` (why it prices nothing; the planner's percentile then stands), `heldOut` (on the source's self-check folds, at the measured mean: of `steps`, `p90Inside` and `p95Inside` put the predicted percentile in the measured one's joint bracket ([how it reads](accuracy.md#where-a-measured-tpot-p90-or-p95-lies-the-joint-bracket)); the recorded `decode` count), `evidence` (that in words), `measuredUpTo` (the highest step rate served, r/s), `p90Ratio` and `p95Ratio` (the percentile over the mean at every rate) and `fit` (as a backtest's `tpotDistribution`). The inputs request's `tpotModel` names the model; it changes the `inputsId`, and the source's `selfCheck` prices its TPOT p90 with it (the percentile it scores). The plan's `tpot.model` says whether it priced that fleet (`priced`, else `notPriced`: a statistic other than p90 or p95, a stated output length or `assume.tpotRatio`), `tpot.ratio` names the `source` "experimental TPOT model", and each TPOT figure past `measuredUpTo`, per decode replica, carries `extrapolated` true, a size's steps among them. The report carries the same `tpotModel`. |
| `cachedTokenCost` | With the replay switch `slo-ttft-cached-token-cost` on (off by default): `phi`, what a cached prompt token costs a request over a computed one, fitted on the source's own steps; `fitted`, the `steps` it was fitted on, the `cachedShare` (the share of the whole prompt the engines did not compute), phi on the `firstHalf` and the `secondHalf` of the steps, and `why` where it was not fitted (phi is then 0 and nothing moves). Absent with the switch off. |

**The context window**, `maxModelLen`, is the smallest `--max-model-len`
(SGLang's `--context-length`) that the prefill and decode variants' recorded
arguments set. It is refused `NotRecorded` when none is set, and
`UnreadableArgument` when either variant sets one the planner cannot read.
`--max-model-len` is read as vLLM reads it: a lower-case suffix is decimal
(`16k` is 16000), an upper-case one binary (`16K` is 16384), and `auto` or `-1`
is the model's own, which the source does not carry.

## The scenario

`POST /v1/plan` takes:

| Key | Default | Notes |
| --- | --- | --- |
| `inputsId` | required | From `POST /v1/inputs`. |
| `fleet.prefill`, `fleet.decode` | the recorded counts | 1 to 64. |
| `traffic.classes.critical.rate` | the operating point | Requests per second over the model: in v1 the total rate. `regular` and `batch` must be 0. With one knee refused, the default is headroom times the other knee, a rate to start from (see `limits.chosen.rateSource`). Required (a 400 without it) only when neither knee is answered. |
| `traffic.classes.critical.concurrency` | none | The load as requests in the system on average, in place of `rate` (giving both is a 400). The plan prices at the rate at which the scenario's fleet holds that many: rate x (TTFT mean + (OL - 1) x TPOT mean) equals it (Little's law), found below the lower knee. `limits.chosen.rateSource` is then `concurrency`, and `limits.chosen.fromConcurrency` gives the concurrency asked and the rate, with `atKnee: true` where the fleet holds fewer requests than that below its knee: the rate is then the knee's (or where the mean time in system is first refused, short of it), and more concurrency adds queueing, not throughput; and `lowerBound: true` where the time in system is an upper bound (a TTFT priced from a lower bound of V_P): the true rate is at least `rate`. A 400 where no knee is answered, where the fleet holds fewer than that below a prefill knee that is only a lower bound (whether it saturates there is not known), where the mean TTFT or TPOT is refused at every rate, or with no output length known. Above 0 and at most 1048576. |
| `traffic.mix` | `"observed"` | Or `{"promptTokens": N, "outputTokens": N}`, priced as the CLI's `--assume-input-tokens` and `--assume-output-tokens` and flagged `AssumedShape`. |
| `traffic.arrivalCV2` | Poisson | As `--arrival-cv2`. |
| `slo.ttft` | the policy's | `statistic` `mean` or `pNN`, `target` in seconds. A TTFT target is required. `bucketTargets` sets a target per bucket of the per-bucket view, keyed by bucket name. |
| `slo.tpot` | the policy's (a mean) | `statistic` `mean`, `pNN` or `none`. |
| `headroom` | the policy's `scaleUpThreshold` | (0, 1]. |
| `assume` | none | `vp`, `a` (seconds), `promptCV`, `tpotRatio`, `routerHop` (seconds), `itlB` (seconds), as the CLI's `--assume-*` flags; or `itlBFromFloor: true`, the CLI's `--assume-itl-b floor`. Each is flagged. `itlB` and `itlBFromFloor` act only on a `pinned-B` line refused `PinnedDefaultB` (not a `held` or `borrowed` one); the inputs' `itlBFloor` term is the B the floor gives. `flowControl` (`on` or `off`) states the router's flow control, the CLI's `--flow-control`, and is flagged as stated. `routerWait` is how a TTFT past the prefill flow-control onset carries the router's wait: `inPrefill` (`--router-wait-in-prefill`, router v1.0.0), `gateStage` (`--price-router-wait`, router v0.11.0) or `none` (flagged `AboveFlowControlOnset`; use it on router v0.11.0); absent, the CLI's default, `inPrefill`. `gateStage` puts every figure outside the validated region, as its flag does; with `inPrefill` a figure the switch prices is outside through its `FlowControlWaitPriced` flag. API only: the page does not set `flowControl` or `routerWait`. `loadAwareRouter: true` states that the router balances prefill by load (llm-d's EPP queue scorer), the CLI's `--load-aware-router`: f is held up to the utilization it was learned at, then falls to 1 at the pooled prefill knee, which lifts that knee. It is stated, never read from the router's series (`policy.router` reports those, and sets no price), and every figure reads outside the validated region with it; absent or `false` (the default), f is held at every load. The page sets it with **My router balances prefill by load** under **Assume a term**. |
| `views.allTrafficPerBucket` | off | The per-bucket view: one line per bucket, "if all traffic were bucket b". |
| `bucketEdges` | 1024, 4096, 8192, 32768, 131072 | The per-bucket view's upper edges, in tokens. Needs `views.allTrafficPerBucket`. |

### Bucket edges

- 1 to 8 edges, ascending, each a whole number from 1 to 1048576. Anything
  else is a 400 naming the edge.
- Each bucket is named by its edge: in k when it is a whole number of 1024
  tokens (`16384` is `16k`), else in tokens (`2000` is `2000`).
  `slo.ttft.bucketTargets` keys are these names; a key that names no bucket of
  the plan is a 400 listing the plan's buckets.
- Edges move only the per-bucket lines, each priced with every request at its
  edge. The limits read the mix line and do not move.
- Asking for the default edges is the same plan as asking for none.
- Each edge not priced before costs a fleet's curves: eight fresh edges took
  about 13 s on a cold server.
- When the context window is known, a line whose edge plus the output tokens
  passes it is flagged `EdgeBeyondContext`: vLLM refuses such a request. The
  line is still priced. With the window unknown nothing is flagged.

## The plan

`POST /v1/plan` returns:

| Key | What it is |
| --- | --- |
| `scenarioId` | A hash of the scenario with its defaults applied. The same scenario gives the same id and the same bytes. |
| `banner` | What every TTFT figure says of itself: it is not validated. |
| `scenario` | The request with its defaults applied. |
| `ttft.line` | The mix line: its curve (65 rates, crowded toward the lower knee), its floor, point A (`atRate`, the TTFT at the chosen rate) and point B (`atTarget`, the rate at the target). |
| `ttft.perBucket` | With the per-bucket view, one line per bucket, each with its own knees, target and points. No limit reads it. |
| `ttft.markers` | The prefill and decode knees, the operating-point cap (headroom times the lower knee; with one knee refused, that knee's refusal, decode's first; a source with no prefill variant at all keeps a cap from the decode knee alone), and the flow-control onsets when flow control is on. |
| `tpot` | The TPOT line at the statistic asked (`pNN` is the mean times R_q, with R_q's source in `ratio`), its curve, floor and points. Absent with `"statistic": "none"`, or when neither the request nor the policy sets a TPOT target. |
| `limits` | `prefill`, `decode`, and `fleet` (the operating-point cap applied, with `withoutHeadroom`, `capApplied` and `binding`, the role with the lower limit); and `chosen`, the chosen rate, its headroom under the fleet limit, and `rateSource`: `request`, `concurrency`, `operating point`, or with one knee refused `prefill knee; decode not priced` or `decode knee; prefill not priced`. `chosen.concurrency` is the load at the chosen rate as requests in the system on average (Little's law): `fleet`, the rate x (TTFT mean + (OL - 1) x TPOT mean); `prefill`, the rate x a request's mean time in the prefill engine (its wait on the prefill pod, a cached prefix's load from a tier, and the prompt's own compute; not the fixed latency, the KV transfer or the router's gate, which no engine holds); `routerQueue`, the rate x the wait at the router's flow-control gate, where that gate is priced as a stage (`assume.routerWait` `gateStage`); `decode`, the rate x OL x TPOT mean, the sequences decoding at once, and `decodePerPod`. `fleet` is more than the roles' sum: it holds the fixed latency, the hop, the KV pull and decode's first token too. The roles are what the engines' `vllm:num_requests_running` + `num_requests_waiting` count on average, but vLLM sets those gauges after each step has dropped the requests it finished, so a prompt prefilled in one step never shows in them: on prefill pods they read under `prefill`. The pods' own histograms read it: the request rate x the mean queue plus prefill time. Each is refused where the mean it rests on is. At or past the lower knee, or where the mean TTFT or TPOT is refused `Saturated` short of it (a queue such as the KV pull's saturating first), `unbounded` is true and `fleet` is refused `Saturated`: the queue grows there, and no average holds. With no TPOT asked, `decode` is absent, unless the decode knee is refused: then `decode` and `fleet` are that refusal. |
| `marginal` | The fleet limit with one more prefill replica, and with one more decode replica: the limit, the change, and which role binds. |
| `flags` | Every flag in the plan once, with the places it qualifies. |
| `assumed` | Every assumed input, in the CLI's words. |

Every figure is an object holding `value` or `refused`, never both. A refused
figure has no number, and no refusal is written as 0. A figure's `band` is its
bootstrap band over the ITL line's readings; its `sensitivity` is the span over
V_P and f, ±15%, with `hiRefused` or `loRefused` where a corner is refused.
The knees, both points, the TPOT's points and the limits carry `validation`:
whether a measured run covered the figure, and how far off it read there
([how accurate each figure is](accuracy.md)): inside a measured row it also
gives the kind of range (`spread`), the independent load levels (`levels`)
and `holdout`, false for an in-sample row. Curve points carry none.
The full key list is pinned in
[`internal/whatifapi/testdata/schema-v1.txt`](../../../internal/whatifapi/testdata/schema-v1.txt).

## The size

`POST /v1/size` takes the plan's keys except `fleet`, `views` and
`bucketEdges` (asking for edges is a 400), and two more:

| Key | Default | Notes |
| --- | --- | --- |
| `traffic.classes.critical.rate` | required | The rate to size for. A `concurrency` is a 400: it is a rate only on a given fleet, and here the fleet is the answer. Translate it with `POST /v1/plan` on a fleet, and size for that rate. The answer's `plan` carries `limits.chosen.concurrency` at the rate, on the fleet found. |
| `maxReplicas` | `{"prefill": 32, "decode": 32}` | The most replicas of a role it tries, 1 to 64 each. |
| `sizeOn` | `point` | What the fleet must reach the rate with: `point`, its limit, or `low`, the low end of the limit's span over V_P and f (about five times the cost). |
| `spare` | `0` | Replicas of each role to add to the answer, 0 to 64: `answer.withSpare` is the fleet that still serves the rate with that many replicas of each role lost. |

The inputs must have a prefill variant; a source with none is a 400. The search
finds the fleet of the fewest GPUs whose fleet limit reaches the rate: each
role's replicas times its GPUs per replica, as the inputs state them
(`fleet.prefill.gpusPerReplica`, `fleet.decode.gpusPerReplica`). Of two with
as few GPUs, the one with fewer replicas in all, then the one with fewer
decode replicas. With both roles at the same GPUs a replica, that is the
fewest replicas, as before v0.12.3; where the inputs do not state both, it
counts replicas (`minimizes: "replicas"`). The report's `needed` fleet is
found the same way, and a fleet with fewer replicas of a role than the one
judged must also hold that role at the peak.

| Key | What it is |
| --- | --- |
| `sizeId` | A hash of the scenario, the bound, `sizeOn` and a `spare` above 0. The same size gives the same id and bytes. |
| `rate`, `maxReplicas`, `sizeOn`, `spare` | What was asked; `spare` absent at 0. |
| `minimizes` | What the search minimised: `gpus`, or `replicas` where the inputs do not state both roles' GPUs per replica. |
| `searched` | How many fleets the search priced, besides the answer's plan. |
| `answer.fleet` | The fleet of the fewest GPUs (or replicas). |
| `answer.gpusPerReplica`, `answer.gpus` | Each role's GPUs per replica and the fleet's GPUs; absent where the inputs do not state both roles'. `answer.withSpare.gpus` is absent likewise. |
| `answer.withSpare` | With `spare` above 0: `spare`, `fleet` (the answer plus `spare` replicas of each role) and its `gpus`. It serves the rate with `spare` replicas of each role lost at once, since the answer does. Map it onto `minReplicaCount`, `maxReplicaCount` and quota as [the guide](page-and-commands.md#size-for-failures-minreplicacount-and-quota) says. |
| `answer.limit`, `answer.binding` | The plan's `limits.fleet` at that fleet, with its span in `sensitivity`, and the role that binds; `limit.boundBy` says which SLO or cap sets it. Sized on `point`, the span's low end may be under the rate. |
| `answer.headroom` | The limit less the rate; sized `low`, the span's low end less the rate. |
| `answer.fewerPrefill`, `answer.fewerDecode` | The fleet with one replica fewer of that role: its limit, its shortfall (`headroom`, negative), and the role that binds. Absent when the role has one replica. |
| `answer.plan` | The plan at that fleet, exactly as `POST /v1/plan` returns it. |
| `unmet` | Instead of `answer` when no fleet within the bound serves the rate: `refused` (the reason, such as `NoPrefillCapacity` or `TargetBelowFloor`, or `ReplicaBoundReached` with the figure the bound reached), `role` (`decode`, `prefill`, or `fleet` when each role alone reaches the rate and the fleet does not), and `atBound`, the fleet at the bound. |

A size with no fleet within the bound is a 200 with `unmet`, not an error.
A decode knee the source refuses (such as `NoLine`, `PinnedDefaultB` or
`NoCapacity`) is `unmet` for
`decode` even with no TPOT asked: nothing else bounds decode, so the fleet
limit is that refusal, as in the plan's `limits`.
[What to do about each reason](troubleshooting.md#refused-figures).

## The report

`POST /v1/report` judges one fleet: is it under-provisioned, right-sized or
over-provisioned at the load the source saw (its p95 less counting noise by default)? Resolve
the source first, then:

```bash
curl -s localhost:8080/v1/report -H 'Content-Type: application/json' -d '{"inputsId": "sha256:...",
  "fleet": {"prefill": 2, "decode": 3},
  "shapes": ["measured", "1k/1k", "16k/200/0.3"],
  "loads": ["50%", "80%", "5r/s", "seen"], "loadSeenX": 1.2,
  "slo": {"ttft": {"statistic": "p90", "target": 2}, "tpot": {"statistic": "mean", "target": 0.03}},
  "assume": {"vp": 200000, "a": 0.02}}'
```

Every field but `inputsId` has a default.

| Field | Default | Meaning |
| --- | --- | --- |
| `fleet.prefill`, `fleet.decode` | the recorded counts | The fleet to judge. |
| `shapes` | `["load"]`, or `["measured"]` where no cycle of the load read a shape (a stated `loadSeen`) | Request shapes, at most 6: `measured` (the decode variant's last cycles), `load` (the shape of the cycles the load judged at was read from), or `IL/OL[/hit]` in tokens, `k` for 1024 (`8k/200`, `1k/4k/0.3`). |
| `loads` | `["50%", "80%", "100%", "seen"]` | The tables' rates, at most 8: a share of each shape's fleet knee (`80%`), a rate (`5r/s`), a concurrency (`64c`: the rate at which each shape's fleet holds 64 requests in the system on average, found below its knee; a shape that cannot translate it leaves the row out and lists it in `shapes[].untranslated` with the reason), `seen`, or the load seen grown (`seen x1.5`, also `seen×1.5` or `seen*1.5`): a what-if with its own column and its own verdict in `shapes[].growth`. |
| `loadStatistic` | `p95-modulation` | Which statistic of the source's per-cycle arrival rate to judge at: `mean`, `p95`, `peak`, or `p95-modulation`, the p95 with the 1-minute rate's counting noise taken out (the share of it that readings 15 s apart hold). Not with `loadSeen`. |
| `loadSeen` | the source's rate at `loadStatistic` | The load to judge at, requests per second. |
| `loadSeenX` | none | An alias that adds `seen xN` to `loads`. It never changes the load judged at. |
| `arrivalCV2` | 1 (Poisson) | The arrivals' Ca^2, as a plan's `traffic.arrivalCV2`. |
| `headroom` | the policy's `scaleUpThreshold` | The operating point, a fraction of the fleet knee in (0, 1). Unlike a plan's (0, 1], 1 is refused: the verdict judges at the operating point, and at the knee itself a fleet saturates. |
| `slo.ttft`, `slo.tpot` | the policy's targets | `statistic` (`mean` or `pNN`) and `target` in seconds. |
| `ttftStatistics` | `["p50", "p90"]` | The TTFT statistics the tables print, at most 4. |
| `tpotStatistic` | `p90` | The TPOT percentile printed beside the mean; `mean` for the mean alone. |
| `assume` | none | As for a plan: `vp`, `a`, `promptCV`, `routerHop`, `tpotRatio`, `itlB`, `itlBFromFloor`, `flowControl`, `routerWait`, `loadAwareRouter`. |
| `spare` | `0` | Replicas of each role added to a verdict's `needed` fleet, 0 to 64 (the CLI's `--spare`): `needed.withSpare`. |

The answer:

| Field | Holds |
| --- | --- |
| `source` | What was read: the namespace, the model, the cycle and `totalCycles`, `firstAt` and `lastAt`, and `gaps`: the window's `cycleSeconds` (the lower median of the time between distinct cycles), `expectedCycles` its span holds at that cycle time, `presentCycles`, `missingShare`, the `largestGapSeconds` with `largestGapFrom` and `largestGapTo`, and `flagged` (`WindowGaps`) above 5% missing. `gaps` is absent with fewer than three distinct cycles. |
| `fleet` | The fleet judged: `prefill`, `decode`, their variants, and `prefillFrom`/`decodeFrom` (`recording` or `asked`). |
| `load` | The load judged at (`rate`, the same as `seen`, as a figure that carries the refusal when there is none: `NoTraffic`, or `WindowGapAtPeak`: see below), the load seen and its `source` (`observed`, `asked` or `none`), the `statistic` judged (`mean`, `p95`, `peak`, `p95-modulation`, or `stated`), `cycles` read and `cyclesWithTraffic`, the per-cycle `mean`, `p95` and `peak` (each a 1-minute rate), `p95Modulation` (the p95 less the counting noise a 1-minute rate carries, mean/60 in variance: mean + (p95 - mean) x the modulation's share of the standard deviation), `p95IsPeak` below 20 cycles, and `rolling`, the busiest 2 minutes' mean, idle and failed reads counted as 0, counted in the window's own cycle time (`source.gaps.cycleSeconds` when it misses cycles). |
| `loads` | The tables' rates, in order: the summary's columns. |
| `shapes[]` | One per shape asked, in order. `extrapolated` is true when IL, OL or the prefix-hit rate lies outside the per-cycle means the source saw, with `extrapolatedWhy`. `loadShape` is the shape of the cycles the load was read from (`statistic`, `inputTokens`, `outputTokens`, `prefixHitRate`, each cycle weighted by its request rate; `cycles`, `fromCycle`, `toCycle`), absent with a stated load or when none of those cycles read a shape; for `p95-modulation` it is read at the p95's cycles; `mismatch` is true, and the shape flagged `ShapeLoadMismatch`, when a measured shape's prompt or output length lies more than 20% off it. |
| `shapes[].utilization` | At the load judged: decode's KV fill, batch slots occupied of the fleet's slots, and wait probability; the KV pull on the decode pods (`decode.kvPull`, below); prefill's utilization pooled and on the busiest pod. |
| `shapes[].utilization.decode.kvPull` | On a P/D fleet with a decode line and a KV transfer, with TTFT priced as processor sharing (the default): `utilization`, the pull's load per decode pod, 1 or more where it saturates them and TTFT is refused; `wait`, its mean wait in seconds; `unpriced`, why `wait` is absent: `ttftRefused` (the mean TTFT is refused at the load) or `firstCome` (the prompts are too uneven for processor sharing, first come's larger price is taken, and it prices no pull stage; the TTFT answer's `parts.kvPullUtilization` then reads 0 while this row carries the pull's load); and `setsTTFT`, true where the pull on its own is the larger mean wait in TTFT, which then sits on the decode pods. Absent where no pull is priced, and where the fleet itself is refused. |
| `shapes[].utilization.decode.kvSpread` | Measured, not priced: how unevenly the window's decode pods were filled, on the fleet that ran. Each pod is read at its busiest engine rank, so on a data-parallel pod the mean is the mean of the pods' busiest ranks. Each cycle's ratio is the busiest pod's fill over that mean, read on the cycles with two decode pods or more and a mean fill of at least `MinKVSpreadFill` (0.05 of the cache: under it the ratio is request-count noise). `cycles`, how many were read; `median`, `p95` and `peak`, their ratios; `pods`, the decode pods a cycle read (`median`, `min`, `max`): the ratio grows with the pod count and is of that fleet, not of the fleet a shape is priced at; `atLoad` and `atLoadCycles`, the median over the cycles the load judged was read from, selected by rate as the measured TTFT is (absent with a stated load); `busiestFillPeak` and `meanFillPeak`, the highest busiest-pod fill and the highest mean. `kvFill` is unchanged: the fill the model predicts at the load, which this does not adjust. A busiest peak at the band top beside a mean peak under it is a KV limit the hottest pod met first. Absent where no cycle qualified. |
| `shapes[].capacity` | The prefill, decode and fleet knees, the operating point and the highest rate that holds every target (`maxAtSLO`), each with output tokens a second, what binds it, and `concurrency`, the requests in the system on average at that rate (`unbounded` at a knee; at a knee that is only a lower bound, refused `PastLowerBoundKnee`: at least the knee, requests queue without bound only past the true knee, which is not known). `lowerBound` is true on a rate that rests on a V_P known only as a lower bound (flag `VPLowerBound`): the prefill knee, and the fleet knee and operating point where it sets them, and `maxAtSLO` where its TTFT is an upper bound; the true rate is at least `value`. A `maxAtSLO` refused `TargetBelowFloor` on such a floor says the floor is at most its figure. With `prefillVPPlateau`, each of these rows priced from the plateau's V_P carries a `validation` that reads OUTSIDE VALIDATED REGION (flag `VPFromPlateau`). |
| `shapes[].table[]` | One row per load: TTFT at each statistic, TPOT mean and percentile, and `e2eMeanSum`, TTFT mean + (OL - 1) x TPOT mean, a sum of means and not a percentile. `cell` is the row in the summary: the TTFT and the TPOT at the statistics the SLO judges them at (with no target, TTFT p90 and the table's TPOT percentile; the TPOT mean where the percentile is refused; each `statistic` says which), and `mark`, the SLO judged at its own statistics (`met`, `missed`, `missed at onset` for a TTFT past the prefill flow-control onset, `saturated`, `past a lower-bound knee` for a rate at or past a prefill knee priced from a lower bound of V_P and short of decode's knee, never judged saturated, `at most, over target` for a TTFT over its target that is only an upper bound, `unknown`, `no target`). A TTFT priced from a V_P known only as a lower bound (flag `VPLowerBound`, no `AssumedVP`) carries `upperBound: true`: the true TTFT is at most `value`, a target it meets is met and one it misses is not known to be missed; so does the `e2eMeanSum` built on it. `concurrency` is the load at the row's rate as requests in the system on average, by Little's law: `fleet` is `rate` x `e2eMeanSum`, `prefill`, `routerQueue`, `decode` and `decodePerPod` as in `limits.chosen.concurrency` of a plan; each is refused where the mean it rests on is, and `unbounded` is true at or past the knee or where a mean is refused `Saturated`. A row asked as a concurrency carries `fromConcurrency`: the concurrency asked, the `rate` it translates into, `atKnee` where the fleet holds fewer requests than that below its knee (the label then ends `saturates`), and `lowerBound` where that rate is only a lower bound. One the fleet does not hold below a lower-bound prefill knee is not translated (`untranslated`). A decode figure (a TPOT, the decode knee) whose load runs more sequences a pod than the most the rows its ITL line was fitted on held carries the flag `BatchBeyondFit` and `batchBeyondFit` (`priced`, `fitted`, in sequences a pod, or an engine rank on a pod of several data-parallel ranks, which `ranks` then counts): the figure is priced all the same, and may read low (a TPOT) or high (the knee). |
| `shapes[].verdict` | `UNDER-PROVISIONED` when the load judged is past headroom times the fleet knee or a target is missed there; `OVER-PROVISIONED` when a role has a replica to spare: one fewer of it holds every target at the load judged, and the role's own knee and target at the peak (`shrink` names the roles); `RIGHT-SIZED` otherwise; `roles[]` is each role's `spare` and `detail` (decode is spare only where one fewer still meets `decodeAtErrorBound`, and `needed` cuts decode no lower); `needed` (on UNDER and OVER, and on RIGHT-SIZED as below) is the cheapest fleet that holds, `{prefill, decode, decodeOnly, alternatives, unmet, maxReplicas, minimizes, gpusPerReplica, gpus, withSpare, decodeAtErrorBound, decodeKneeError}`, by the search `/v1/size` runs (`decodeAtErrorBound`: the decode count when the decode knee reads as high as its label allows, `decodeKneeError` that share, the 0.6B goldens' 0.991 outside every validated region, absent where it is 0; never above `maxReplicas`, which it reads where more would be needed; `alternatives` are other mixes of as many replicas, which may take more GPUs; `minimizes` is absent on a decode-only need and on the fleet judged; `gpus` and `gpusPerReplica` are absent where a counted role's GPUs are not stated, and on a decode-only need `gpus` counts decode alone; `withSpare` is absent with `unmet`; on RIGHT-SIZED it is a mix of fewer GPUs that holds when the roles' GPUs differ, else with a `spare` the fleet judged); each role it cuts also holds its own knee and target at the peak, as a `spare` must, so it cuts a role with no spare only in trade for more of the other, which the summary says ("trading prefill for decode"); `grow` names the roles it adds to, and on OVER can name a role `shrink` names too; `NOT JUDGED` where it would be `UNDER-PROVISIONED` on a load whose cycles ran more than 20% lighter, in prompt or output, than the measured shape priced (the load and the shape priced are of different traffic), or where it would be `RIGHT-SIZED` or `OVER-PROVISIONED` and the measured shape priced is more than 20% lighter, in prompt or output, than the load's cycles (the knee reads high for that load), or, with the load past the operating point, where at the shape priced the fleet priced or a smaller one served more than 1.25x the fleet knee for 3 minutes without a break (the window contradicts the model; over the band's top where that sets the decode knee, never on a lower-bound prefill knee); with no knee, no load, a target its own check cannot price, or a prefill side that does not price (`scope` `decode only`: never right-sized or over-provisioned, and no `maxAtSLO`; `needed` is the decode it takes, `decodeOnly`, and the summary says how many decode hold it and that it is no fleet recommendation); or `OVER-PROVISIONED` where the router measured the TTFT over its target at the cycles of the load judged (`POST /v1/report` only; flag `MeasuredTTFTOverTarget`: a fleet that missed its target has nothing to spare); or a load past the operating point of a prefill knee priced from a lower bound of V_P, short of decode's (the summary gives the knee as "at least" its rate and never says the fleet saturates; past decode's knee too, `UNDER-PROVISIONED` on decode's knee); or a TTFT over its target that is only an upper bound (`upperBound`), where no other target is missed (the summary says the TTFT is "at most" its figure and that `assume.vp` prices it). `summary` is one plain sentence, `reason` gives the numbers, and `checks` each fleet tried. `trust` is what the source's self-check says of the verdict: `not trusted: ...` when it rests on figures the self-check found inconsistent (`selfCheck.untrusted`; the report's `exitCode` is then 4, with the verdict still given), `not covered by the self-check: ...` at a rate, fleet or shape it did not test; absent otherwise. |
| `shapes[].growth[]` | One per growth load asked, in order: `load` (`seen x1.5`), `x`, the `rate` judged at, and a `verdict` of the same form. A what-if beside `verdict`, never in its place. |
| `notValidated` | What no run has validated: where the decode knee is outside its validated region (its own `capacity.decodeKnee.validation` says), that inside it read at most +3% and on the 0.6B goldens 43% to 99% high; and TTFT is unvalidated. |

Each shape and load is priced from the one replay the inputs resolved: a
report replays nothing.

The load observed is refused `WindowGapAtPeak` when a gap in the source could
hide a busier stretch than any it holds: the gap is at least 2 minutes long
(any gap, not only the largest), and the cycle on either side of it lies in
the busiest 2 minutes (`rolling`) or is the cycle of the highest rate. A
gap's length is the time between the cycles either side of it less one
cycle. The verdict is then `NOT JUDGED`, and the CLI exits 3. A stated
`loadSeen` is never refused.

## The day plan

`POST /v1/dayplan` sizes every bucket of the source's window for its own
rate, as `POST /v1/size` sizes one rate, and sets the replicas the fleet had
beside it. It reads the cycles of the source the `inputsId` was resolved
from: the whole recording, or the whole Prometheus window. For a day, resolve
a 24-hour window. It takes the size's `slo`, `assume`, `headroom` and
`maxReplicas`, and `traffic` with `mix` and `arrivalCV2` only (a rate is a
400: each bucket's rate is its own), and three more. Unlike a size, it needs no
TTFT target: with none in `slo.ttft` or the policy, prefill is not sized,
decode is sized alone, and the missing target adds nothing to `refused`.

| Key | Default | Notes |
| --- | --- | --- |
| `step` | `5m` | A bucket's length, a Go duration from `15s` to `24h`. Buckets start on the step in UTC. |
| `sizeFor` | `p95-modulation` | The rate of a bucket's cycles to size for: `mean`, `p95`, `p95-modulation` (the p95 less the counting noise of a 1-minute rate; the bucket's `rate.p95Modulation`) or `max`. A bucket of fewer than 20 cycles asked for its `p95` or `p95-modulation` is sized for its `mean` and flagged `FewCyclesForP95`; each bucket's `sizedFor` says which. |
| `lead` | the source's | Each role's scale-up lead time, seconds: `{"prefill": 180, "decode": 90}`. A role left out takes the start time the source learned (`fleet.<role>.startSeconds` in the inputs), else 0. |
| `minRequests` | 30 | A bucket with fewer requests is `tooLittleData`, not sized. |
| `experimentalModels` | false | Price the plan with the experimental TTFT and TPOT models the inputs were resolved with (`ttftModel`, `tpotModel` in `POST /v1/inputs`, or a server started with `--experimental-ttft=v2` or `--experimental-tpot=v2`), as a plan and a size on those inputs are. Inputs resolved with no model are refused (400). Where the TTFT model prices a TTFT the planner refuses, prefill is sized on it, each such bucket's `limit` flagged `RankMixtureTTFT`. Where a model prices a target the plan asks, each bucket is sized at its own rate and at its own request shape, read over a window around it of at least 16000 requests and an hour (at most 2 hours; `shape.model` gives the window and its mean prompt and computed tokens and computed-token p90, and `shape.promptTokens` and `outputTokens` are then the window's): the TTFT model draws from that window's prompt and computed-token histograms, so a bucket's own prefix hit is priced; the TPOT model keeps its ratio at the window's output lengths, around the planner's mean at the bucket's own. A bucket whose shape lies outside what the TTFT model's fit steps served (mean prompt or computed tokens outside their rate-weighted 5th to 95th percentile, or a computed-token p90 above the largest step's) is sized at the window's shape, flagged `WindowShape` and `ShapeOutsideFit` (`shape.model.outsideFit` says which and the range); one whose window cannot reach 16000 requests is too, flagged `WindowShape` and `ShapeFewRequests`; a note counts each. A bucket at a stated shape or term is the planner's own, and the TPOT model prices a p90 or p95 target, not the mean; a note says, per model, why each bucket it did not price is the planner's. |

A term the source cannot price at any rate is not a bucket's refusal. The plan
probes each shape once at one replica a role: a TPOT it cannot price
(`PinnedDefaultB`, `NoLine`) is dropped and decode is sized on the fleet
limit; a TTFT it cannot price (`NoPrefillCapacity`, `NoFixedLatency`, a
target under the zero-load floor) leaves prefill unsized and decode sized
alone, on its knee times headroom (and its TPOT, where that prices); when
decode's knee is refused too, nothing is sized. Each lands in `refused`. Only a
rate no fleet within `maxReplicas` serves is a bucket's own `unmet`.
When any gap of 2 minutes or more has a cycle on either side of it in the
busiest sized bucket (the one of the highest sized rate: the gap cuts into
it, or covers a bucket beside it), that bucket is refused `WindowGapAtPeak`
(term `window`, flag `WindowGapAtPeak`): the gap could hide busier minutes
than it read. The next busiest bucket takes its place, and is refused too
while a gap borders it. The CLI
exits 3 when `refused` is not empty, a bucket is unmet, or no bucket was
sized; else 4 when a bucket is not trusted (`summary.counts.untrusted`). A
bucket not covered changes no exit code. It prints the plan either way.

Each bucket is priced at its own observed prompt and output length (rounded
to a 5% grid, so buckets of nearly one shape share their priced fleets), or
at the inputs' shape with `WindowShape` where its cycles read none or the
experimental models (`experimentalModels`) price it at the window's shape
(`ShapeOutsideFit`, `ShapeFewRequests`), or at
`traffic.mix` where the request states it: then `Extrapolated` on every
bucket when the stated length lies outside what the buckets observed.

| Key | What it is |
| --- | --- |
| `from`, `to`, `step`, `sizeFor`, `sizeForSource`, `lead`, `minRequests`, `ttft`, `tpot`, `headroom`, `maxReplicas` | What was planned. `ttft` is absent when neither the request nor the policy sets a TTFT target: prefill is then not sized, and decode is sized alone (`PrefillNotPriced`, and a note). |
| `buckets[]` | `start`, `samples` (cycles), `requests`, `rate` (`mean`, `p95`, `max` of its cycle rates, and `p95Modulation` when sized for it), `sizedFor` and `sizedRate`, `shape`, `status`, `needed` and `limit` (sized: the fewest GPUs, or the fewest replicas where the roles' GPUs per replica are equal or not stated, ties to fewer replicas, then fewer decode, and another mix may serve too; `needed.prefill` absent and `needed.atLeast` true when prefill is not priced, decode then a lower bound), `unmet` and `unmetRole` (beyond the bound), `actual` (the median serving pods of the priced prefill and decode variants), `notServing` (the median of each role's replicas asked for and not serving: replicas that exist and are not ready, starting, unschedulable or held by an admission gate; a pod never created is not in the recording; for a LeaderWorkerSet a replica is a group; absent when none), `hadLimit` (the limit of the fleet it had, priced whole), `verdict` (`under` when `hadLimit` falls short of the rate, `over` when one replica fewer of either role still serves it, `match`, `refused`; none, with the flag `MeasuredTTFTOverTarget`, where it would be `over` and the router measured the bucket's TTFT over its target), `scope` (`decode only` where prefill was not priced and decode was judged alone), `measuredTTFT` (the router's TTFT in the bucket at the target's statistic, seconds, read off its last cycle; absent with too few requests), `trust` (what the self-check says of the verdict, as a report's `verdict.trust`: `not trusted: ...` when it rests on figures the self-check found inconsistent, the TTFT figures where the bucket priced prefill and the TPOT figures always; `not covered by the self-check: ...` when the bucket was priced at a stated shape, or its sized rate on the fleet it had is not one it tested (an unmet bucket is not tested for coverage); absent otherwise, on a verdict judged on no figure (a fleet with no replica of a role it prices), and on a bucket refused or not sized) and `flags`. |
| `hours[]` | Per UTC hour: the rate over its cycles, `needed` (a fleet that holds every sized bucket of the hour, priced again at each bucket's rate and shape: the cheapest of the buckets' own fleets that holds them all, in the order a bucket's need is chosen, else each role's most over them; `notConfirmed` true where it falls short at one; with `atLeast` where any bucket sized decode alone), `busiest` (the fleet of its one bucket of the highest sized rate, which need not hold a bucket of a lower rate but longer requests), the fewest pods it had, `notServing` (for each role, the replicas asked for and not serving in its bucket of the fewest serving, so beside the fewest it had; absent when none), `unmet` (buckets beyond the bound), and the verdict: `under` when any bucket was under or beyond the bound, `refused` when every bucket with data was refused, `over` when every sized bucket was over; `trust` is its worst bucket's opening words, `not trusted` or `not covered by the self-check`. |
| `summary` | `peakNeeded` (a fleet that holds every sized bucket, as an hour's `needed`; `peakNotConfirmed` as `notConfirmed`), `busiestNeeded` and `minNeeded` (the needs of the busiest and the quietest sized buckets), `sizedOnGpus` (the GPUs per replica the search weighed each role by, present only where they differ between the roles), replica-hours and GPU-hours needed and had over the sized buckets (absent when none was sized; prefill's hours and the GPU-hours also when any sized bucket did not price prefill, `counts.prefillNotPriced`), `hadReplicaHoursAll` and `hadGpuHoursAll` over every bucket with data, `underHours`, `overHours` (whole fleets judged over), `decodeOverHours` (hours over with decode judged alone: no fleet's waste), `refusedHours`, and the counts, including how many buckets were sized for each statistic, and how many the self-check does not trust (`untrusted`: the CLI's `day-plan` then exits 4) or does not cover (`notCovered`); these two are absent at 0, and absent too when no self-check ran. |
| `searched` | How many fleets the plan priced. |
| `ttftModel`, `tpotModel` | With `experimentalModels`, each model as the inputs fitted it (as in `POST /v1/inputs`), `abstained` saying why one priced nothing; a note counts the buckets each priced. Absent otherwise. |
| `refused[]` | Every term the source cannot price at any rate, and the busiest bucket refused beside a gap (term `window`), with `role`, `term` (`ttft`, `tpot`, `decodeKnee`, `fleet`, or `window` for the busiest bucket refused `WindowGapAtPeak`), `reason`, `detail`, `hint` (what to do), `effect` (what the plan did instead) and `buckets`. Empty when every term priced. |
| `lead`, `buckets[].held`, `hours[].held`, `summary.heldReplicaHours`, `summary.heldGpuHours` | With a scale-up lead for either role: `lead.prefill` and `lead.decode` (`seconds`, and `source`: `request`, `learned`, `seeded` or `none`); a bucket's `held`, each role's most need over the bucket and every bucket that starts within the role's lead after it ends (a bucket not sized can hold the replicas of one after it); an hour's, each role's most held over its buckets; and the held replica-hours and GPU-hours, each scale-up's replicas counted from one lead before their bucket. All absent with no lead, but `lead` is present when one is asked, even at 0. |
| `notes` | What it does not price: the steady state each bucket is sized at, the scale-up lead it holds ahead, and the prefix hit rate. A note names the source's gaps when its cycles do not cover its span. |
| `gaps` | The source's gaps, as the report's `source.gaps`. |

```bash
curl -s http://127.0.0.1:8080/v1/dayplan -H 'Content-Type: application/json' -d '{
  "inputsId": "'"$ID"'", "step": "5m",
  "slo": {"ttft": {"statistic": "p95", "target": 0.5}, "tpot": {"statistic": "mean", "target": 0.004}},
  "assume": {"vp": 30000}}' | jq '.summary'
```

## The backtest

`POST /v1/backtest` fits the planner on one source and predicts another from
its inputs alone: each step's rate, prompt and output lengths, prompt spread
and serving replicas. Each prediction is set beside the latency the source
measured, the router's histograms where recorded, else the engines'. The task
is in [Check the planner](check.md).

| Key | Default | Notes |
| --- | --- | --- |
| `inputsId` | required | The source fitted on, from `POST /v1/inputs`. |
| `testInputsId` | | The source tested on. Without it, `inputsId`'s source is split. |
| `split`, `fitUntil` | `60%` | Where to split one source: a share of its span, or an RFC 3339 time. The fit is the source read at the split's cycle, resolved as `POST /v1/inputs` resolves it; the test is every cycle after it. |
| `fitBelow`, `testAbove` | | Split by load, in place of `split` and `fitUntil`: the fit is `inputsId`'s recording read again from the steps whose arrival rate is below `fitBelow` alone, and only the steps at or above `testAbove` are judged. Each is a rate in requests per second (`"12.5"`) or a percentile of its side's step rates (`"p50"`, idle steps left out). With `testInputsId`, `fitBelow` reads `inputsId`'s steps and `testAbove` the test's, the two may cover the same hours, and a test step the fit read a cycle of is not judged. A recording source only; one bound alone, or beside `split`, is a 400. |
| `step`, `settle` | `5m`, `2m` | A step's length (`1m` to `1h`), and how long after a change of replicas, and after the test's start, a step is not judged. |
| `ttft` | | A target (`statistic`, `target` seconds) to judge each step by, predicted and measured. |
| `arrivalCV2` | Poisson | The test's Ca^2: 1 is random arrivals, higher is burstier. |
| `measured` | `auto` | `router`, `engine`, or `auto`. |
| `assume` | | Terms the fit did not learn, the router's flow control (`flowControl`, `routerWait`) and its balancing (`loadAwareRouter`), as a plan's `assume` (`vp`, `a`, `promptCV` up to 3, `routerHop`, `tpotRatio`, applied at every percentile, `itlB` or `itlBFromFloor`). Every figure they move is flagged. A value a plan would refuse is a 400. |
| `allowConfigMismatch` | false | A test of another model is a 400. A test whose recorded configuration differs from the fit's is a 400 with `reason` `ConfigMismatch`, naming each field: the roles, the flow-control series (read over each side's cycles), and per role the accelerator, GPUs, vLLM release and every engine setting the capacity fingerprint holds (parallelism, dtypes, batch and context limits, speculative decoding, KV connector, prefix caching, checkpoint, image). A setting a side could not read is not compared. True prices it, with the difference in `config.differences` and the notes. A test that is the fit's own source, or starts before the fit ends, is a 400. |
| `ttftModel` | the server's | `rank-mixture` prices TTFT with the experimental per-rank limited processor sharing model, fitted on the fit window ([accuracy](accuracy.md)). `rank-mixture-nl-line` prices with its variant that measures each part of the non-queue time on its own and always fits as `rankMixtureFitSteps` and `rankMixtureStratified` do; its `rankMixture` block adds `zParts` and `fitMean`. Empty is the planner's own, unless the server runs with `--experimental-ttft=v2`. Any other value is a 400. The server reads the per-rank series it needs only with that flag; without it the model abstains. |
| `rankMixtureFitSteps` | false | With `rank-mixture`: fit the KV-deferral lines and the prompt slopes only on steps whose busiest prefill rank is under utilization 0.9, and read the ranks' shares at the priced load ([accuracy](accuracy.md)). The model's `assumed` line says which fit steps it read. Without `rank-mixture` it is a 400. |
| `rankMixtureStratified` | the server's (true) | With `rank-mixture`: read each prefill rank's mean service and its spread from the computed-token histogram itself, not from the rank's own random draws ([accuracy](accuracy.md)). On unless the request sends false, or the server runs with `--rank-mixture-stratified=false`. The answer's `rankMixture.stratified` says which way it priced. |
| `rankMixtureKVHold` | false | With `rank-mixture-nl-line` only (any other model is a 400): read the KV-deferral lines no further than the 0.95 quantile of the fit window's rank utilizations, held past it; `rankMixture.kv.rhoHold` reports it ([accuracy](accuracy.md)). |
| `rankMixtureRemainderSpread` | false | With `rank-mixture-nl-line` only (any other model is a 400): draw the router's remainder from the router's measured request-processing time where the fit window recorded it; `rankMixture.zParts` adds `remainderShapeMean` and `remainderShapeP95`. Without the series the remainder is its line and the assumed line says so ([accuracy](accuracy.md)). |
| `rankMixtureRemainderServer` | false | With `rank-mixture-nl-line` and `rankMixtureRemainderSpread` only (anything else is a 400): draw the router's remainder from the prefill pods' API servers (their render and HTTP handler times against vLLM's end-to-end time) and the router's own work, on the spread's shape, where the fit window recorded them; `rankMixture.zParts.remainderServer` reports the fit, the unmeasured rest's share (`restShare`) among it; with that share over 0.5 a TTFT priced past the fit window's top rate (`topRate`) is flagged `RemainderRestPastFit`. Without the series the remainder is the plain spread, `zParts.remainderServerOff` says why and the assumed line says so ([accuracy](accuracy.md)). |
| `rankMixtureMinFitWindow` | the server's (`12h`) | With a `ttftModel`: the shortest fit window the model prices from, a duration (`2h`); under it the model abstains. A model fitted on a window under 12 h reports `rankMixture.shortFitWindow` and `minFitWindow`, and every TTFT it prices is flagged `RankMixtureShortFitWindow`. Not a duration, or without a `ttftModel`: a 400. |
| `prefillSlots` | the fit's | The prefill engine's `--max-num-seqs`, read by `rank-mixture`. Without it, the server takes it from the fit's recorded engine flags. |
| `tpotModel` | the server's | `renewal-lognormal` prices the TPOT percentiles with the experimental per-request TPOT distribution, and `renewal-lognormal-per-length` with it as a mixture over the output lengths; the mean stays the planner's. Empty is the planner's own, unless the server runs with `--experimental-tpot=v2`. Any other value is a 400. The server reads the decode series it needs only with that flag; without it the model abstains. |
| `decodeSlots`, `specTokens` | the fit's | The decode engine's `--max-num-seqs` and `num_speculative_tokens`, read by both TPOT models. Without them, the server takes them from the fit's recorded engine flags. |
| `tpotDistMinFitSteps` | the server's (24) | With a `tpotModel`: the fewest five-minute fit steps the distribution fits on, at least 4; under it the distribution abstains. One fitted on fewer than 24 reports `tpotDistribution.shortFit` and `minFitSteps`, and flags every percentile it prices `TPOTDistributionShortFit`. Under 4, or without a `tpotModel`: a 400. The inputs request takes it too, for the main view. |

| Key | What it is |
| --- | --- |
| `verdict`, `verdictLines`, `excluded` | The answer, whole and a sentence a line (the span, TTFT, TPOT, the benchmark conditions), and what was not judged. |
| `backtestId`, `banner`, `notes`, `step`, `settle`, `ttft` | The answer's id, its standing in one line, what it rests on, the step and settle in seconds, and the target asked. |
| `fit`, `test`, `split`, `loadSplit` | Each side's source, span and cycles, and how one source was split; for a split by load, `loadSplit`: the bounds as asked (`fitBelow`, `testAbove`), as rates (`fitBelowRate`, `testAboveRate`), the steps each side kept (`fitSteps`, `testSteps`), and `fitRead`, the test's steps over its floor left out because the fit read them. |
| `config` | `match`, `differences[]` (`field`, `fit`, `test`), `notCompared` (what a side does not record, the router's version always), `allowed`. |
| `measured`, `measuredNote` | The latency scored against, and what its figures are. |
| `assumed` | The inputs the test did not give: random arrivals, a prompt-length spread not read. |
| `rankMixture` | With `ttftModel`: the model's fit (`model`, `experimental` true, `assumed`), or `abstained`, why it priced nothing: every TTFT is then the planner's own, flagged `RankMixtureAbstained`. Each TTFT it prices is flagged `RankMixtureTTFT` and reads OUTSIDE VALIDATED REGION. Each step carries its checks in `steps[].rankMixture`. |
| `tpotDistribution` | With `tpotModel`: the fit (`model`, `experimental` true, `assumed`, `c`), or `abstained`, and `test`, its score at the histogram's resolution: `p90Inside` and `p95Inside`, the steps whose predicted percentile fell in the measured one's joint bracket ([how it reads](accuracy.md#where-a-measured-tpot-p90-or-p95-lies-the-joint-bracket)), beside the planner's own (`defaultP90Inside`, `defaultP95Inside`); `p90Below`, `p90Above`, `p95Below` and `p95Above` the steps under and over it; `engineJoined` the steps whose bracket the decode engines' TPOT histogram joined, and `conflicts` the brackets read as the router's own bucket. Each percentile it prices is flagged `TPOTDistribution`, one it abstained on `TPOTDistributionAbstained`. Each step carries `steps[].tpotDistribution` with `p90Inside` and `p95Inside`. |
| `cachedTokenCost` | The fit's phi, priced at every step, as the inputs' `cachedTokenCost` gives it; absent with `slo-ttft-cached-token-cost` off. |
| `steps[]` | `start`, `cycles`, `excluded` (`warmUp`, `scaling`, `noTraffic`, `noMeasurement`, `notPriced`, `otherVariants`, `burst`, `load`: outside a load split's test) with `detail`, the inputs (`rate`, `prefill`, `decode`, `promptTokens`, `outputTokens`, `promptCV`), `requests`, `kneeShare` and `load` (TTFT's band), `decodeKneeShare` and `decodeLoad` (TPOT's), `ttft[]` and `tpot[]` per statistic (`mean`, `p50`, `p90`, `p95`): `predicted` (a figure, with its validation), `bound` (the planner's own upper estimate: the top of its span over V_P and f, or of its band; absent with none), `measured`, `ratio` (predicted over measured), `inRegion`, `aboveHistogram` (measured above the top bucket while predicted under it: `ratio` is over the top bound, and counts as read low), `measuredBucket` (`lo`, `hi`: the bucket a measured percentile fell in where it is too wide to place it, more than 1.2 times its lower edge, or any bucket from 0; the truth is somewhere in it), `outsideBucket` (`under` or `over`: the prediction is more than 20% beyond that bucket's nearer edge, a certain miss; `ratio` is over the nearer edge, a bound that understates the miss, never within, and `under` counts as read low; the bucket is kept beside a refusal, and `pastKnee` reads its edges), `unscored` (`fewRequests`, `aboveHistogram`, `refused`, `notMeasured`, `notComparable`, `unresolvable`: the prediction inside that bucket or within 20% of its edges, so the histogram cannot say whether it is within; shown and not scored; `unsettled`: TTFT only, the step's `unsettled`; `upperBound`: TTFT only, a prediction priced from a V_P known only as a lower bound, `predicted.upperBound`, that the fleet measured at most 20% over: the true TTFT is at most the figure, so a figure above what was measured is the bound holding, consistent and not scored; where the fleet measured more than 20% over it the bound broke, and the step is scored as any other, read low; a predicted target miss from it counts not judged in `target`); `relaxationSeconds` (the prefill fleet's queue relaxation time at the step's rate, pooled over its pods; absent past the knee or where no prefill knee priced) and `unsettled` (over a fifth of the measurement window, so the window's TTFT is a noisy sample: TTFT not scored); `backlog` and `backlogWait` (requests a fluid queue carries in from a step past the prefill knee, and the mean wait it adds over the step's measurement window to each TTFT predicted; the bound adds the most); `batchBeyondFit` (`priced`, `fitted`: the sequences a pod the step's decode runs, an engine rank's on a pod of several data-parallel ranks, which `ranks` then counts, and the most the rows the fit's ITL line was fitted on held, where it is past them: the mean TPOT's, or on a step past the decode knee the knee's; its TPOT figures carry the flag `BatchBeyondFit`, and its TPOT may read low and the knee high; absent inside the fit's batch); and `target` with a target. Each step's inputs are read over the window its measurement covers: the cycles of the measurement's rate window (the router's, recorded with it; else one minute) ending at the step's last cycle. A source with no prefill variant is a 400: a backtest prices a P/D fleet. |
| `summary` | `steps`, `judged`, `excluded` counts, and per statistic over `all` judged steps and `inRegion`: `steps`, `bias` (the median ratio), `medianError` and `p90Error` (of \|ln ratio\| as a share: 0.5 is a factor of 1.5), `coverage` over `coverageSteps` (measured at or under the bound, where there is one), `within` (within 20%), `low` (read more than 20% low); `unscored` by reason, `refused` by the refusal's reason, `pastKnee` (the measured range where it refused as saturated), and `outside` by the envelope's reason; `byLoad[]` the same per load band (`steps` by TTFT's band, `decodeSteps` by TPOT's); `target` the agreement on the target. |

A measured percentile needs 50 requests in the step at p50, 100 at p90 and
200 at p95 (50 for the mean): fewer is `fewRequests`, not scored.

```bash
curl -s http://127.0.0.1:8080/v1/backtest -H 'Content-Type: application/json' -d '{
  "inputsId": "'"$ID"'", "split": "60%", "step": "5m"}' | jq -r '.verdict, .excluded'
```

## The self-check

`POST /v1/inputs` self-checks the source by default (`selfCheck`, true
unless the server was started with `--no-self-check`): the answer's
`selfCheck`, and the report's and the day plan's, carry it. `selfCheck` false
turns it off for that source.

| Key | What it is |
| --- | --- |
| `status` | `ok` (the page and the CLI say "held on its own steps"), `weak`, `inconsistent` or `notRun` ([the rules](check.md#the-self-check-beside-every-answer)). |
| `line`, `why`, `trust` | The result in one sentence, what made the status, and which figures not to trust. |
| `ttft`, `tpot` | Each figure scored over the folds at its `statistic`: the mean (the histogram's sum over its count) when scored in 6 steps or more, else the p90, else the p50 (the first scored in 6 or more; with none, the one scored in the most); `steps`, `within`, `bias`, `low`, `judged`, `overloaded`, `pastKnee` (what the fleet measured where the planner gave no figure), `refused`; `levels` (the independent load levels the steps fall in), `withinBound` (the within share's lower bound over a bootstrap of the levels), `minRatio` (the lowest step's ratio), `nearKnee` (`steps` at 0.8 to 1.0 of the knee and the `worst` tenth's ratio), `nearKneeUnsettled` (TTFT only: steps there left unscored as `unsettled`, which `nearKnee` does not hold), `servedPastKnee` (TTFT only: steps called overloaded that the fleet served unqueued), `upperBound` (TTFT only: steps priced from a lower bound of V_P that the fleet measured under, consistent and not scored, so no rule reads them; absent with none). |
| `untrusted` | With `inconsistent`: the figures not to trust, `ttft` and or `tpot`. A report's verdict that rests on one says so in `verdict.trust`, and the report's `exitCode` is 4 (3, a verdict not judged, takes precedence). A day plan's bucket says so in `buckets[].trust`, and `day-plan` exits 4 (3 takes precedence). |
| `covered` | What the check tested: `rateLo` and `rateHi` of its judged steps, and the `fleets` they ran. A report's verdict at another rate, fleet or shape says `not covered by the self-check` in `verdict.trust`. |
| `folds[]` | Each cut: `fitTo`, `testFrom`, `testTo`, `judged`. |
| `checks[]` | Each rule: `name`, `passed`, `detail`. |
| `issueUrl` | With `inconsistent`: a new-issue page prefilled with diagnostics that hold no name, no rate and no recording, for the user to review and file. |
| `seconds` | What the self-check took, its fits' replay apart (shared with the answer's). |
| `stated` | The answer's stated inputs the recording was replayed with (`--assume-itl-b floor`, `assume.promptCV 0.06`), named as the question's source names them. Absent with none. |
| `withoutStated` | The answer's stated inputs the self-check ran without: a TPOT ratio, stated lengths, the CLI's opt-in switches set off their defaults, and on a report or day plan from the server every stated term and Ca^2 (the source's self-check ran before them; a report's shapes are not listed). The line says so. Absent with none. |

## Predict and compare

The backtest in two steps, for a check where the planner never holds the
latency it is scored against. The task is in
[Check it blind](check.md#check-it-blind-predict-now-compare-later).

`POST /v1/predict` takes `inputsId` (the fit), `windowInputsId` (the window
to predict, its latency taken out or not: none is read), and the backtest's
`step`, `settle`, `arrivalCV2`, `assume` and `allowConfigMismatch`, plus
`measureWindow` (each step's inputs are read over it: by default the
router's rate window, which a stripped window keeps where the router
measured TTFT, else one minute, the engines'). It answers the prediction file: `kind`, `format`,
`plannerVersion`, `whatifSchema`, `madeAt`, `model`, `fit`, `window`,
`step`, `settle`, `measureWindow`, `arrivalCV2`, `config`, `assumed`,
`notes`, `steps[]` (a backtest's steps, predicted only; a step's `unsettled` is set from its rate and inputs alone, so a compare leaves that step's TTFT unscored whatever was measured), `inputsHash` (the
SHA-256 of every step's inputs) and `predictionHash` (the SHA-256 of the
file with this field empty).

`POST /v1/compare` takes `prediction` (the file, as JSON), `inputsId` (the
same window measured, latency included), and optionally `ttft` and
`measured`. It answers a backtest. A file that does not match its own hash,
an unknown field, a measured window whose cycles or load differ from the
predicted one, a window measured over another rate window, or one with no
latency is a 400.

## Errors

Every error, whatever its status, has the same shape. An error a client can
act on also has a `reason`:

```json
{"schemaVersion": 1, "error": "slo.ttft.statistic \"p100\", want mean or a percentile from p1 to p99.9 (p90)"}
```

| Status | When |
| --- | --- |
| 400 | A field asked wrongly; the message names it. With `reason` `ConfigMismatch`: a backtest whose test ran another configuration than its fit; the message names each field, and `allowConfigMismatch` scores it anyway. |
| 401 | `--auth=kubernetes` and no token, or a token TokenReview does not accept. |
| 403 | A Host that is not a loopback name with `--auth=none`; a namespace not in `--allowed-namespaces`; with `--auth=kubernetes`, a user not allowed the path or the namespace; an upload without `--allow-upload`. |
| 404 | An unknown path, or an `inputsId` the server no longer holds: resolve the inputs again. |
| 413 | An upload past a bound, or an upload that would take too much memory loaded: [Upload a recording](#upload-a-recording). |
| 415 | A `Content-Type` other than `application/json` (for an upload, other than `application/octet-stream` or `application/gzip`). |
| 500 | Anything else. A failed authentication review is a 500 that names no client error; the error goes to stderr. |
| 503 with `Retry-After: 1` | The server is busy: it prices as many plans at once as it has CPUs, and two inputs at once. Ask again. |
| 503 with `reason: PlanBudgetExceeded` | The plan ran past `--plan-budget`. See below. |
| 503, no reason | The request ended (its client went away) before it was priced. |

Reading a request may take 30 s, and an idle connection is closed after 2
minutes. Once its body has arrived, a request that computes (`POST /v1/inputs`,
`/v1/plan`, `/v1/size`, `/v1/report`, `/v1/dayplan`, `/v1/backtest`,
`/v1/predict`, `/v1/compare`) may run for 30 minutes: loading a day-long
recording takes minutes. Any other response may take 10 minutes, and an upload
has its own 15. A proxy in front of the planner must wait as long
([OpenShift](in-cluster.md#long-recordings-load-slowly)).
[Fixes for each](troubleshooting.md#api-errors).

### The plan budget

A plan still being priced when `--plan-budget` (default 5 minutes) runs out
stops at its next figure and is a 503, with no `Retry-After`:

```json
{"schemaVersion": 1, "error": "the plan was not priced within the server's plan budget of 5m0s, and was stopped: ask without the per-bucket view (on the page, untick \"Show if all traffic were bucket b\") or with fewer bucket edges, or start the server with a larger --plan-budget", "reason": "PlanBudgetExceeded"}
```

- Nothing of the stopped plan is kept: asking again prices it again from the
  start. The page does not ask again on its own.
- The budget is checked between figures, so a plan can run past it by one
  figure, normally milliseconds.
- A size's search is not under the budget; only the plan it answers with is.
- A request for a plan already being priced waits for it and shares its clock,
  so it can be refused sooner than its own budget.
- The most is 9 minutes, well inside the 30 minutes a plan's request may run,
  so the refusal is written before the connection closes, even for a backtest
  that first loads a recording. A negative value or one above 9m stops the
  server from starting.

A cold plan of the worked recording takes under 20 s.

## In a container

The release image's entry point is `capacity-planner`, and its default command
is `serve --listen 0.0.0.0:8080 --listen-all-in-container --recordings /recordings`.
`/recordings` is empty; mount a directory over it, run as yourself (recordings
are written readable by their owner only), and publish the port to the host's
loopback only. [Get the planner](README.md#get-the-planner) has the command.

Never run the default command with `--network host`: the server would listen
on every interface of the host. Give the command yourself, with
`--listen 127.0.0.1:8080` and without `--listen-all-in-container`
([In a container](install-options.md#in-a-container)). For a browser in a Kubernetes
Pod, see [Run it in a cluster](in-cluster.md); for API clients, use
`--auth=kubernetes` with TLS.

## Kubernetes authentication

With `--auth=kubernetes` the server:

1. checks the bearer token with a TokenReview (missing or not accepted: 401);
2. asks a SubjectAccessReview for `get` on the non-resource URL `/whatif` plus
   the path, for every method: `POST /v1/plan` is `get` on `/whatif/v1/plan`,
   and the page is `/whatif/` and `/whatif/page/*` (not allowed: 403). An
   upload, `POST /v1/recordings`, is `post` on `/whatif/v1/recordings`: add a
   rule with `verbs: ["post"]` for the users who may upload;
3. for a `prometheus` source, also asks for `get` on `pods` in its namespace.

Reviews are reused for 30 s. The client comes from `KUBECONFIG`,
`~/.kube/config` or the in-cluster config; serve reads a kubeconfig only with
`--auth=kubernetes`. The path keeps the command's old name, so RBAC written for
an earlier release still applies.

The server reads Prometheus with its own credentials, so a namespace must be
both in `--allowed-namespaces` and allowed to the user. The RBAC for a group
`planners` and a ServiceAccount `planner`, reading the namespace `llm`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: capacity-planner
rules:
- nonResourceURLs: ["/whatif/*"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: capacity-planner
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: capacity-planner
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: planners
- kind: ServiceAccount
  name: planner
  namespace: llm
---
# One Role and RoleBinding per namespace in --allowed-namespaces.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: capacity-planner-reader
  namespace: llm
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: capacity-planner-reader
  namespace: llm
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: capacity-planner-reader
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: planners
- kind: ServiceAccount
  name: planner
  namespace: llm
```

The server's own identity needs `create` on
`tokenreviews.authentication.k8s.io` and
`subjectaccessreviews.authorization.k8s.io`, as `system:auth-delegator`
grants.

Start the server; it runs in the foreground:

```bash
bin/capacity-planner serve --auth=kubernetes --listen 0.0.0.0:8443 \
  --tls-cert-file tls.crt --tls-key-file tls.key \
  --prometheus https://prometheus.example:9090 --kube-state-metrics --namespace llm
```

Then, in another terminal:

```bash
TOKEN=$(kubectl -n llm create token planner)
curl -s --cacert ca.crt https://planner.example:8443/v1/inputs \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model": "qwen3-32b", "source": {"kind": "prometheus", "namespace": "llm"}}'
```

A token minted for another audience (`kubectl create token --audience capacity-planner`)
is accepted only when the server is started with `--token-audiences capacity-planner`.

A browser does not send a bearer token by itself: open the page through a
proxy that adds one. The page reaches its files and the API by relative paths,
so a proxy may serve it under a prefix; open it with the trailing slash
(`/planner/`, not `/planner`).
