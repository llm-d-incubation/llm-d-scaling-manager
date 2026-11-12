# What-if serve: the capacity planner's API

`whatif serve` runs the capacity planner's HTTP API on your machine. You
resolve a source once (a recording, or a window of Prometheus), then ask for
plans against it: curves, the two points on each graph, the fleet's limits and
what one more replica of each role would change. Every figure is one the
`whatif` CLI also prints, from the same code.

This is phase 0 of [the capacity planner](../proposals/capacity-planner.md):
the API, no page. Like the rest of `whatif`, it is a contributor and operator
tool and is not in the manager image.

## Start it

From a recordings directory:

```
go build -o bin/whatif ./cmd/whatif
bin/whatif serve --recordings ./recordings
```

It listens on `127.0.0.1:8080` and prints the address on stderr. Stop it with
Ctrl-C.

From Prometheus (through a port-forward, as `whatif capture` reads it):

```
bin/whatif serve --prometheus http://localhost:9090 --namespace llm --kube-state-metrics
```

Both sources can be served at once.

## Ask for a plan

Resolve the inputs. A recording is named by its file name in `--recordings`,
never by a path:

```
curl -s localhost:8080/v1/inputs -d '{"model": "model-02",
  "source": {"kind": "recording", "name": "ols-sweep.jsonl.gz"}}'
```

With no `source`, the inputs are a Prometheus window of the last hour, ending
now. A window of your own:

```
curl -s localhost:8080/v1/inputs -d '{"model": "qwen3-32b",
  "source": {"kind": "prometheus", "namespace": "llm",
             "from": "2026-11-09T10:00:00Z", "to": "2026-11-09T11:00:00Z"}}'
```

The answer carries an `inputsId`. Price a scenario against it:

```
curl -s localhost:8080/v1/plan -d '{"inputsId": "sha256:...",
  "fleet": {"prefill": 1, "decode": 1},
  "traffic": {"classes": {"critical": {"rate": 6}}, "mix": {"promptTokens": 1000, "outputTokens": 1000}},
  "slo": {"ttft": {"statistic": "p95", "target": 0.2}, "tpot": {"statistic": "mean", "target": 0.004}},
  "assume": {"vp": 30000}}'
```

Everything but `inputsId` has a default: the recorded replica counts, the
observed mix, the policy's SLO and headroom, and as the rate the operating
point (headroom times the lower knee).

## Flags

| Flag | What it does |
| --- | --- |
| `--recordings DIR` | Recordings a `recording` source names by file name. |
| `--prometheus URL` | The Prometheus a `prometheus` source reads. With it, every [capture flag](whatif-capture.md) but `--from`, `--to` and `-o` applies: the role map, `--step`, `--policy`, the engine, TLS and the bearer token. The window comes with each request. |
| `--namespace NS` | The namespace a `prometheus` source reads when the request names none. |
| `--batch-cap N` | The engine's batch cap a window is captured with, as `capture --batch-cap`. |
| `--listen ADDR` | Where to serve. Default `127.0.0.1:8080`. Any address that is not loopback needs `--auth=kubernetes`; serve refuses to start without it. |
| `--auth none\|kubernetes` | `none` (the default) is loopback only. `kubernetes` checks a bearer token with TokenReview, then asks a SubjectAccessReview for `get` on the non-resource URL `/whatif` plus the path (`/whatif/v1/plan`). A missing or unauthenticated token is 401, a user not allowed is 403. The client comes from `KUBECONFIG`, `~/.kube/config` or the in-cluster config. |
| `--tls-cert-file`, `--tls-key-file` | The server's certificate and key. Required with `--auth=kubernetes`: a token is not sent in the clear. |
| `--shutdown-timeout` | How long requests in flight may run after Ctrl-C. Default 10s. |

An RBAC rule that grants the API:

```yaml
rules:
- nonResourceURLs: ["/whatif/*"]
  verbs: ["get"]
```

No request can name a host or a file path: the Prometheus URL is a flag, and a
recording is a file name inside `--recordings`. An error names no server path.

## Endpoints

| Method and path | Does |
| --- | --- |
| `POST /v1/inputs` | Resolves a source for one model: the replay of a recording to a cycle, or a Prometheus window captured and generated in memory (seconds). Returns `inputsId` and every term with its source. |
| `POST /v1/plan` | Prices a scenario against an `inputsId`. |

`GET /v1/models`, `POST /v1/size` (inverse mode) and the page are later phases.
Any other path is a 404.

### The inputs

| Key | What it is |
| --- | --- |
| `inputsId` | `sha256:` and a hash of the recording's bytes, the cycle, the model and the variants. The same source gives the same id. |
| `source` | The kind, the recording's name or the window, the cycle read and how many the recording holds. |
| `fleet` | The prefill and the decode variant, with their replica counts at the cycle. |
| `observed` | The mean prompt and output and the prompt's spread; the fixed buckets (1k, 4k, 8k, 32k, 128k, each named by its upper edge) and the one the mean prompt falls in. The request rate and the share per bucket are refused: the replayed state carries neither. |
| `policy` | The policy's TTFT and TPOT objectives, the headroom (`scaleUpThreshold`), and the flow-control state. |
| `terms` | V_P, a, f, the prompt and its cv, the KV transfer, the router hop, the ITL line, the KV capacity and per-request KV, the batch cap, and R_q at p90: each with its source, or the refusal that stopped the fleet there. |
| `assumed` | Every input the source did not give, in the CLI's words. |

### The plan

| Key | What it is |
| --- | --- |
| `scenarioId` | A hash of the scenario with its defaults applied. The same scenario is the same id and the same bytes. |
| `banner` | What every TTFT figure says of itself, from `constants.SLOTTFTPredictionCaveat`. |
| `scenario` | The request with its defaults applied. |
| `ttft.line` | The mix line: its curve (65 rates, crowded toward the lower knee, the last two at and past it), its floor, point A (`atRate`, the TTFT at the chosen rate) and point B (`atTarget`, the rate at the target). |
| `ttft.perBucket` | With `views.allTrafficPerBucket`, one line per bucket, "if all traffic were bucket b": each priced with every request at the bucket's upper edge, with its own knees, target (`slo.ttft.bucketTargets`, default the mix target) and points. No limit reads it. |
| `ttft.markers` | The prefill and decode knees, the operating-point cap (headroom times the lower knee), and the flow-control onsets when flow control is on. |
| `tpot` | The TPOT line at the statistic asked (`mean`, or `pNN` priced as the mean times R_q, with R_q's source in `ratio`), its curve, floor and points. Absent with `"tpot": {"statistic": "none"}`, or when neither the request nor the policy sets a TPOT target. |
| `limits` | `prefill` (`slo-to-rate`'s prefill rate), `decode` (its decode rate), `fleet` (its fleet rate, the operating-point cap applied, with `withoutHeadroom` at headroom 1, `capApplied`, and `binding`, the role with the lower limit), and `chosen`, the chosen rate and its headroom under the fleet's limit. |
| `marginal` | The fleet's limit with one more prefill replica, and with one more decode replica: the limit, the change, and which role binds then. These limits carry no span over V_P and f. |
| `flags` | Every flag in the plan once, with the places it qualifies. |
| `assumed` | Every assumed input of the scenario's fleet, in the CLI's words. |

Every figure is an object holding `value` or `refused`, never both. A refused
figure has no number in it, and no refusal is written as 0. A figure's `band`
is its bootstrap band over the ITL line's readings; its `sensitivity` the span
over V_P and f, ±15%, with the worse side refused (`hiRefused`, `loRefused`)
where a corner is.

The full key list is pinned in
[`internal/whatifapi/testdata/schema-v1.txt`](../../internal/whatifapi/testdata/schema-v1.txt).
`whatifSchema` names the CLI schema the answer, refusal and flag shapes follow.

### What the scenario takes

| Key | Default | Notes |
| --- | --- | --- |
| `fleet.prefill`, `fleet.decode` | the recorded counts | 1 to 64. |
| `traffic.classes.critical.rate` | the operating point | r/s over the model: in v1 the total rate. `regular` and `batch` must be 0. |
| `traffic.mix` | `"observed"` | Or `{"promptTokens": N, "outputTokens": N}`: priced as the CLI's `--assume-input-tokens` and `--assume-output-tokens`, flagged `AssumedShape`. |
| `traffic.arrivalCV2` | Poisson | As `--arrival-cv2`. |
| `slo.ttft` | the policy's | `statistic` `mean` or `pNN`, `target` in seconds. A TTFT target is required. |
| `slo.tpot` | the policy's (a mean) | `statistic` `mean`, `pNN` or `none`. |
| `headroom` | the policy's `scaleUpThreshold` | (0, 1]. |
| `assume` | none | `vp`, `a` (seconds), `promptCV`, `tpotRatio`, `routerHop` (seconds): as the CLI's `--assume-*` flags, each flagged. |
| `views.allTrafficPerBucket` | off | The per-bucket view. |

A field asked wrongly is a 400 naming it, and an `inputsId` the server no
longer holds a 404: resolve the inputs again.

## Caching and latency

The server keeps caches only: the inputs by `inputsId`, each plan by
`scenarioId`, each fleet's curves by its knobs (replicas, mix, statistics,
assumptions), and each fleet's limits and marginal steps by its knobs, the
targets and the headroom. So moving the rate prices the two points alone;
moving a target prices the limits again, not the curves; a new replica count
or assumption prices everything.

Measured on the goldens' recordings (`go test ./cmd/whatif -run '^$' -bench
Plan`), see [As built](../proposals/capacity-planner.md#as-built-phase-0).

## Tests

| Test | Holds |
| --- | --- |
| `TestServeParity` (`cmd/whatif`) | Every JSON golden in `cmd/whatif/testdata`, asked of the API, gives the same limits, points, bounds, flags and refusals. |
| `TestServeCurvesAgreeWithPoints` | Each curve point is `TTFTAt` and `SLOAtReplicas` at its rate; point B lies on the curve; past the lower knee every point is refused `Saturated`. |
| `TestSchemaIsPinned` (`internal/whatifapi`) | The key list. `-update` rewrites it. |
| `TestPlanRefusalShape`, `apitest.CheckFigures` | No number beside a refusal, and no refusal written as 0, on every response. |
| `TestCaching` | The same scenario is the same id and bytes; a rate move prices no curve and no limit; a target move prices limits only. |
| `TestCheckListen`, `TestAuthenticate`, `TestServeRefusesUnsafeBinds` | Loopback serves with no token; any other bind refuses to start without `--auth=kubernetes`; with it, a token the SubjectAccessReview does not allow is 403. |
