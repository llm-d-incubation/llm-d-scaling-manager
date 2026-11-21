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
| `fleet` | The prefill and the decode variant, with their replica counts at the cycle. |
| `observed` | The mean prompt and output and the prompt's spread; the default buckets (1k, 4k, 8k, 32k, 128k, each named by its upper edge) and the one the mean prompt falls in. The request rate and the share per bucket are refused (`NoPerRequestLengths`): the source carries neither. |
| `policy` | The policy's TTFT and TPOT objectives, the headroom (`scaleUpThreshold`), and the flow-control state. |
| `terms` | V_P, a, f, the prompt and its cv, the KV transfer, the router hop, the ITL line, the KV capacity and per-request KV, the batch cap, the context window (`maxModelLen`), and R_q at p90: each with its source, or the refusal that stopped it. |
| `assumed` | Every input the source did not give, in the CLI's words. |

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
| `traffic.mix` | `"observed"` | Or `{"promptTokens": N, "outputTokens": N}`, priced as the CLI's `--assume-input-tokens` and `--assume-output-tokens` and flagged `AssumedShape`. |
| `traffic.arrivalCV2` | Poisson | As `--arrival-cv2`. |
| `slo.ttft` | the policy's | `statistic` `mean` or `pNN`, `target` in seconds. A TTFT target is required. `bucketTargets` sets a target per bucket of the per-bucket view, keyed by bucket name. |
| `slo.tpot` | the policy's (a mean) | `statistic` `mean`, `pNN` or `none`. |
| `headroom` | the policy's `scaleUpThreshold` | (0, 1]. |
| `assume` | none | `vp`, `a` (seconds), `promptCV`, `tpotRatio`, `routerHop` (seconds), `itlB` (seconds), as the CLI's `--assume-*` flags; or `itlBFromFloor: true`, the CLI's `--assume-itl-b floor`. Each is flagged. `itlB` and `itlBFromFloor` act only on a `pinned-B` line refused `PinnedDefaultB` (not a `held` or `borrowed` one); the inputs' `itlBFloor` term is the B the floor gives. `flowControl` (`on` or `off`) states the router's flow control, the CLI's `--flow-control`, and is flagged as stated. `routerWait` is how a TTFT past the prefill flow-control onset carries the router's wait: `inPrefill` (`--router-wait-in-prefill`, router v1.0.0), `gateStage` (`--price-router-wait`, router v0.11.0) or `none` (flagged `AboveFlowControlOnset`); absent, the CLI's default. `inPrefill` and `gateStage` put every figure outside the validated region, as their flags do. API only: the page does not set them. |
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
| `limits` | `prefill`, `decode`, and `fleet` (the operating-point cap applied, with `withoutHeadroom`, `capApplied` and `binding`, the role with the lower limit); and `chosen`, the chosen rate, its headroom under the fleet limit, and `rateSource`: `request`, `operating point`, or with one knee refused `prefill knee; decode not priced` or `decode knee; prefill not priced`. With no TPOT asked, `decode` is absent, unless the decode knee is refused: then `decode` and `fleet` are that refusal. |
| `marginal` | The fleet limit with one more prefill replica, and with one more decode replica: the limit, the change, and which role binds. |
| `flags` | Every flag in the plan once, with the places it qualifies. |
| `assumed` | Every assumed input, in the CLI's words. |

Every figure is an object holding `value` or `refused`, never both. A refused
figure has no number, and no refusal is written as 0. A figure's `band` is its
bootstrap band over the ITL line's readings; its `sensitivity` is the span over
V_P and f, ±15%, with `hiRefused` or `loRefused` where a corner is refused.
The knees, both points, the TPOT's points and the limits carry `validation`:
whether a measured run covered the figure, and how far off it read there
([how accurate each figure is](accuracy.md)). Curve points carry none.
The full key list is pinned in
[`internal/whatifapi/testdata/schema-v1.txt`](../../../internal/whatifapi/testdata/schema-v1.txt).

## The size

`POST /v1/size` takes the plan's keys except `fleet`, `views` and
`bucketEdges` (asking for edges is a 400), and two more:

| Key | Default | Notes |
| --- | --- | --- |
| `traffic.classes.critical.rate` | required | The rate to size for. |
| `maxReplicas` | `{"prefill": 32, "decode": 32}` | The most replicas of a role it tries, 1 to 64 each. |
| `sizeOn` | `point` | What the fleet must reach the rate with: `point`, its limit, or `low`, the low end of the limit's span over V_P and f (about five times the cost). |

The inputs must have a prefill variant; a source with none is a 400. The search
finds the fewest replicas in all whose fleet limit reaches the rate; of two as
few, the one with fewer decode replicas. A role's GPUs per replica are not
weighed. The report's `needed` fleet is found the same way, and a fleet with
fewer replicas of a role than the one judged must also hold that role at the
peak.

| Key | What it is |
| --- | --- |
| `sizeId` | A hash of the scenario, the bound and `sizeOn`. The same size gives the same id and bytes. |
| `rate`, `maxReplicas`, `sizeOn` | What was asked. |
| `searched` | How many fleets the search priced, besides the answer's plan. |
| `answer.fleet` | The fewest replicas. |
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
over-provisioned at the load the source saw (its p95 by default)? Resolve
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
| `shapes` | `["measured"]` | Request shapes, at most 6: `measured`, or `IL/OL[/hit]` in tokens, `k` for 1024 (`8k/200`, `1k/4k/0.3`). |
| `loads` | `["50%", "80%", "100%", "seen"]` | The tables' rates, at most 8: a share of each shape's fleet knee (`80%`), a rate (`5r/s`), `seen`, or the load seen grown (`seen x1.5`, also `seen×1.5` or `seen*1.5`): a what-if with its own column and its own verdict in `shapes[].growth`. |
| `loadStatistic` | `p95` | Which statistic of the source's per-cycle arrival rate to judge at: `mean`, `p95` or `peak`. Not with `loadSeen`. |
| `loadSeen` | the source's rate at `loadStatistic` | The load to judge at, requests per second. |
| `loadSeenX` | none | An alias that adds `seen xN` to `loads`. It never changes the load judged at. |
| `arrivalCV2` | 1 (Poisson) | The arrivals' Ca^2, as a plan's `traffic.arrivalCV2`. |
| `headroom` | the policy's `scaleUpThreshold` | The operating point, a fraction of the fleet knee in (0, 1). Unlike a plan's (0, 1], 1 is refused: the verdict judges at the operating point, and at the knee itself a fleet saturates. |
| `slo.ttft`, `slo.tpot` | the policy's targets | `statistic` (`mean` or `pNN`) and `target` in seconds. |
| `ttftStatistics` | `["p50", "p90"]` | The TTFT statistics the tables print, at most 4. |
| `tpotStatistic` | `p90` | The TPOT percentile printed beside the mean; `mean` for the mean alone. |
| `assume` | none | As for a plan: `vp`, `a`, `promptCV`, `routerHop`, `tpotRatio`, `itlB`, `itlBFromFloor`, `flowControl`, `routerWait`. |

The answer:

| Field | Holds |
| --- | --- |
| `fleet` | The fleet judged: `prefill`, `decode`, their variants, and `prefillFrom`/`decodeFrom` (`recording` or `asked`). |
| `load` | The load judged at (`rate`, the same as `seen`, as a figure that carries the refusal when there is none), the load seen and its `source` (`observed`, `asked` or `none`), the `statistic` judged (`mean`, `p95`, `peak`, or `stated`), `cycles` read and `cyclesWithTraffic`, the per-cycle `mean`, `p95` and `peak` (each a 1-minute rate), `p95IsPeak` below 20 cycles, and `rolling`, the busiest 2 minutes' mean, idle and failed reads counted as 0. |
| `loads` | The tables' rates, in order: the summary's columns. |
| `shapes[]` | One per shape asked, in order. `extrapolated` is true when IL, OL or the prefix-hit rate lies outside the per-cycle means the source saw, with `extrapolatedWhy`. |
| `shapes[].utilization` | At the load judged: decode's KV fill, batch slots occupied of the fleet's slots, and wait probability; prefill's utilization pooled and on the busiest pod. |
| `shapes[].capacity` | The prefill, decode and fleet knees, the operating point and the highest rate that holds every target (`maxAtSLO`), each with output tokens a second and what binds it. |
| `shapes[].table[]` | One row per load: TTFT at each statistic, TPOT mean and percentile, and `e2eMeanSum`, TTFT mean + (OL - 1) x TPOT mean, a sum of means and not a percentile. `cell` is the row in the summary: the TTFT and the TPOT at the statistics the SLO judges them at (with no target, TTFT p90 and the table's TPOT percentile; the TPOT mean where the percentile is refused; each `statistic` says which), and `mark`, the SLO judged at its own statistics (`met`, `missed`, `missed at onset` for a TTFT past the prefill flow-control onset, `saturated`, `unknown`, `no target`). |
| `shapes[].verdict` | `UNDER-PROVISIONED` when the load judged is past headroom times the fleet knee or a target is missed there; `OVER-PROVISIONED` when a role has a replica to spare: one fewer of it holds every target at the load judged, and the role's own knee and target at the peak (`shrink` names the roles); `RIGHT-SIZED` otherwise; `roles[]` is each role's `spare` and `detail`; `needed` (on UNDER and OVER) is the smallest fleet that holds, `{prefill, decode, decodeOnly, alternatives, unmet, maxReplicas}`, by the search `/v1/size` runs; each role it cuts also holds its own knee and target at the peak, as a `spare` must, so it cuts a role with no spare only in trade for more of the other, which the summary says ("trading prefill for decode"); `grow` names the roles it adds to, and on OVER can name a role `shrink` names too; `NOT JUDGED` with no knee, no load, a target its own check cannot price, or a prefill side that does not price (`scope` `decode only`: never right-sized or over-provisioned, and no `maxAtSLO`). `summary` is one plain sentence, `reason` gives the numbers, and `checks` each fleet tried. `trust` is what the source's self-check says of the verdict: `not trusted: ...` when it rests on figures the self-check found inconsistent (`selfCheck.untrusted`; the report's `exitCode` is then 4, with the verdict still given), `not covered by the self-check: ...` at a rate, fleet or shape it did not test; absent otherwise. |
| `shapes[].growth[]` | One per growth load asked, in order: `load` (`seen x1.5`), `x`, the `rate` judged at, and a `verdict` of the same form. A what-if beside `verdict`, never in its place. |
| `notValidated` | What no run has validated: the decode knee read 44% to 103% high on the goldens, and TTFT is unvalidated. |

Each shape and load is priced from the one replay the inputs resolved: a
report replays nothing.

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
| `sizeFor` | `p95` | The rate of a bucket's cycles to size for: `mean`, `p95` or `max`. A bucket of fewer than 20 cycles asked for its `p95` is sized for its `mean` and flagged `FewCyclesForP95`; each bucket's `sizedFor` says which. |
| `minRequests` | 30 | A bucket with fewer requests is `tooLittleData`, not sized. |

A term the source cannot price at any rate is not a bucket's refusal. The plan
probes each shape once at one replica a role: a TPOT it cannot price
(`PinnedDefaultB`, `NoLine`) is dropped and decode is sized on the fleet
limit; a TTFT it cannot price (`NoPrefillCapacity`, `NoFixedLatency`, a
target under the zero-load floor) leaves prefill unsized and decode sized
alone, on its knee times headroom (and its TPOT, where that prices); when
decode's knee is refused too, nothing is sized. Each lands in `refused`. Only a
rate no fleet within `maxReplicas` serves is a bucket's own `unmet`. The CLI
exits 3 when `refused` is not empty, a bucket is unmet, or no bucket was
sized, and prints the plan either way.

Each bucket is priced at its own observed prompt and output length (rounded
to a 5% grid, so buckets of nearly one shape share their priced fleets), or
at the inputs' shape with `WindowShape` where its cycles read none, or at
`traffic.mix` where the request states it: then `Extrapolated` on every
bucket when the stated length lies outside what the buckets observed.

| Key | What it is |
| --- | --- |
| `from`, `to`, `step`, `sizeFor`, `sizeForSource`, `minRequests`, `ttft`, `tpot`, `headroom`, `maxReplicas` | What was planned. `ttft` is absent when neither the request nor the policy sets a TTFT target: prefill is then not sized, and decode is sized alone (`PrefillNotPriced`, and a note). |
| `buckets[]` | `start`, `samples` (cycles), `requests`, `rate` (`mean`, `p95`, `max` of its cycle rates), `sizedFor` and `sizedRate`, `shape`, `status`, `needed` and `limit` (sized: the fewest replicas in all, ties to fewer decode, and another mix may serve too; `needed.prefill` absent and `needed.atLeast` true when prefill is not priced, decode then a lower bound), `unmet` and `unmetRole` (beyond the bound), `actual` (the median serving pods of the priced prefill and decode variants), `notServing` (the median of each role's replicas asked for and not serving: replicas that exist and are not ready, starting, unschedulable or held by an admission gate; a pod never created is not in the recording; for a LeaderWorkerSet a replica is a group; absent when none), `hadLimit` (the limit of the fleet it had, priced whole), `verdict` (`under` when `hadLimit` falls short of the rate, `over` when one replica fewer of either role still serves it, `match`, `refused`) and `flags`. |
| `hours[]` | Per UTC hour: the rate over its cycles, `needed` (a fleet that holds every sized bucket of the hour, priced again at each bucket's rate and shape: the fewest of the buckets' own fleets that holds them all, else each role's most over them; `notConfirmed` true where it falls short at one; with `atLeast` where any bucket sized decode alone), `busiest` (the fleet of its one bucket of the highest sized rate, which need not hold a bucket of a lower rate but longer requests), the fewest pods it had, `notServing` (for each role, the replicas asked for and not serving in its bucket of the fewest serving, so beside the fewest it had; absent when none), `unmet` (buckets beyond the bound), and the verdict: `under` when any bucket was under or beyond the bound, `refused` when every bucket with data was refused, `over` when every sized bucket was over. |
| `summary` | `peakNeeded` (a fleet that holds every sized bucket, as an hour's `needed`; `peakNotConfirmed` as `notConfirmed`), `busiestNeeded` and `minNeeded` (the needs of the busiest and the quietest sized buckets), replica-hours and GPU-hours needed and had over the sized buckets (absent when none was sized; prefill's hours and the GPU-hours also when any sized bucket did not price prefill, `counts.prefillNotPriced`), `hadReplicaHoursAll` and `hadGpuHoursAll` over every bucket with data, `underHours`, `overHours`, `refusedHours`, and the counts, including how many buckets were sized for each statistic. |
| `searched` | How many fleets the plan priced. |
| `refused[]` | Every term the source cannot price at any rate, with `role`, `term` (`ttft`, `tpot`, `decodeKnee`), `reason`, `detail`, `hint` (what to do), `effect` (what the plan did instead) and `buckets`. Empty when every term priced. |
| `notes` | What it does not price: scale-up lag (it sizes steady state), and the prefix hit rate. |

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
| `step`, `settle` | `5m`, `2m` | A step's length (`1m` to `1h`), and how long after a change of replicas, and after the test's start, a step is not judged. |
| `ttft` | | A target (`statistic`, `target` seconds) to judge each step by, predicted and measured. |
| `arrivalCV2` | Poisson | The test's Ca^2: 1 is random arrivals, higher is burstier. |
| `measured` | `auto` | `router`, `engine`, or `auto`. |
| `assume` | | Terms the fit did not learn, and the router's flow control (`flowControl`, `routerWait`), as a plan's `assume` (`vp`, `a`, `promptCV` up to 3, `routerHop`, `tpotRatio`, applied at every percentile, `itlB` or `itlBFromFloor`). Every figure they move is flagged. A value a plan would refuse is a 400. |
| `allowConfigMismatch` | false | A test of another model is a 400. A test whose recorded configuration differs from the fit's is a 400 with `reason` `ConfigMismatch`, naming each field: the roles, the flow-control series (read over each side's cycles), and per role the accelerator, GPUs, vLLM release and every engine setting the capacity fingerprint holds (parallelism, dtypes, batch and context limits, speculative decoding, KV connector, prefix caching, checkpoint, image). A setting a side could not read is not compared. True prices it, with the difference in `config.differences` and the notes. A test that is the fit's own source, or starts before the fit ends, is a 400. |

| Key | What it is |
| --- | --- |
| `verdict`, `verdictLines`, `excluded` | The answer, whole and a sentence a line (the span, TTFT, TPOT, the benchmark conditions), and what was not judged. |
| `backtestId`, `banner`, `notes`, `step`, `settle`, `ttft` | The answer's id, its standing in one line, what it rests on, the step and settle in seconds, and the target asked. |
| `fit`, `test`, `split` | Each side's source, span and cycles, and how one source was split. |
| `config` | `match`, `differences[]` (`field`, `fit`, `test`), `notCompared` (what a side does not record, the router's version always), `allowed`. |
| `measured`, `measuredNote` | The latency scored against, and what its figures are. |
| `assumed` | The inputs the test did not give: random arrivals, a prompt-length spread not read. |
| `steps[]` | `start`, `cycles`, `excluded` (`warmUp`, `scaling`, `noTraffic`, `noMeasurement`, `notPriced`, `otherVariants`, `burst`) with `detail`, the inputs (`rate`, `prefill`, `decode`, `promptTokens`, `outputTokens`, `promptCV`), `requests`, `kneeShare` and `load` (TTFT's band), `decodeKneeShare` and `decodeLoad` (TPOT's), `ttft[]` and `tpot[]` per statistic (`mean`, `p50`, `p90`, `p95`): `predicted` (a figure, with its validation), `bound` (the planner's own upper estimate: the top of its span over V_P and f, or of its band; absent with none), `measured`, `ratio` (predicted over measured), `inRegion`, `aboveHistogram` (measured above the top bucket while predicted under it: `ratio` is over the top bound, and counts as read low), `unscored` (`fewRequests`, `aboveHistogram`, `refused`, `notMeasured`, `notComparable`); and `target` with a target. Each step's inputs are read over the window its measurement covers: the cycles of the measurement's rate window (the router's, recorded with it; else one minute) ending at the step's last cycle. A source with no prefill variant is a 400: a backtest prices a P/D fleet. |
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
| `ttft`, `tpot` | The p90 scored over the folds: `steps`, `within`, `bias`, `low`, `judged`, `overloaded`, `pastKnee` (what the fleet measured where the planner gave no figure), `refused`; `levels` (the independent load levels the steps fall in), `withinBound` (the within share's lower bound over a bootstrap of the levels), `minRatio` (the lowest step's ratio), `nearKnee` (`steps` at 0.8 to 1.0 of the knee and the `worst` tenth's ratio), `servedPastKnee` (TTFT only: steps called overloaded that the fleet served unqueued). |
| `untrusted` | With `inconsistent`: the figures not to trust, `ttft` and or `tpot`. A report's verdict that rests on one says so in `verdict.trust`, and the report's `exitCode` is 4 (3, a verdict not judged, takes precedence). |
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
`notes`, `steps[]` (a backtest's steps, predicted only), `inputsHash` (the
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

Reading a request may take 30 s, an idle connection is closed after 2 minutes,
and a response may take 10 minutes (a cold plan, or a capture).
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
- The most is 9 minutes, a minute under the 10-minute response limit, so the
  refusal is written before the connection closes. A negative value or one
  above 9m stops the server from starting.

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
([In a container](README.md#in-a-container)). For a browser in a Kubernetes
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
