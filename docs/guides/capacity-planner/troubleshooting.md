# Capacity planner: troubleshooting

Every refusal and error the [capacity planner](README.md) gives, with its fix.
Find the message you see; the sections follow the order you meet them.

## Starting the server

| You see | Fix |
| --- | --- |
| Serve exits: "serve needs --prometheus, --recordings or both" | Give a source: a recordings directory, a Prometheus, or both. |
| Serve exits: "--from: serve reads the window each request names" (or `--to`) | Drop `--from` and `--to`. The page's **Source**, or the request's `source`, names the window. |
| Serve exits: `--recordings` is not a directory | Give a directory that holds the recordings (`run.jsonl.gz`), not a file. |
| Serve exits: an address that is not loopback needs `--auth=kubernetes` | Keep `--listen 127.0.0.1:8080` and [tunnel to it](README.md#run-it-for-others), or set up [Kubernetes authentication](api.md#kubernetes-authentication). In a container whose port is published to `127.0.0.1` only, the image's default command already sets `--listen-all-in-container`. |
| Serve exits: `--listen-all-in-container` refused | It is refused in a Kubernetes Pod, with `--auth=kubernetes`, and with any address but `0.0.0.0:PORT`, `[::]:PORT` or `:PORT`. In a Pod use `--auth=kubernetes` with TLS. |
| Serve exits: `--auth=kubernetes` needs TLS | Give `--tls-cert-file` and `--tls-key-file`. |
| Serve exits: `--plan-budget` out of range | Give 0 (the default, 5m) up to 9m. |
| Serve exits: `--prometheus` needs a namespace | Give `--namespace` or `--allowed-namespaces`. |
| Serve exits: "namespace "x": ..." | A namespace in `--namespace` or `--allowed-namespaces` is not a DNS-1123 label (lower case letters, digits and `-`, at most 63). Fix its spelling. |
| Serve exits: "--namespace x is not among --allowed-namespaces" | Add it to `--allowed-namespaces`, or drop `--namespace`. |

## The page

| You see | Fix |
| --- | --- |
| "This server reads no source" | The recordings directory is empty, or holds no recording (with the image, the directory you mounted at `/recordings`). Put a `run.jsonl.gz` from [Record a window](../record-for-capacity-planner/), or [the sample](README.md#get-the-planner), in it and reload the page. |
| "the model has 2 decode variants (a, b)" | The page prices one P/D fleet. Open **Source**, type one name in **Decode variant** (or **Prefill variant**), and press **Load**. If it adds that all were recorded as serving both roles, the recording has no roles: record again with `--role-of`, or with a role label such as `llm-d.ai/role` on the Pods. |
| Refused figures everywhere | A term was not learned. See [Refused figures](#refused-figures). |
| "Pricing..." for several seconds | A new replica count, statistic or view prices whole curves; a rate move is fast. A size takes about 20 to 45 s. |
| A scenario, link or import is refused | It names a recording or namespace this server does not read, or fails the page's checks; the status line gives the reason. Start the server on the same source. |
| The **Bucket edges** box is marked red | Give 1 to 8 edges, smallest first, each at most 1024k. The reason is under the box. |
| A bucket line is dimmed and dotted (`EdgeBeyondContext`) | Its edge plus the output length passes the engines' `--max-model-len`. Lower the edge, or read the line as a request the engine would refuse. |
| Nothing flagged past the context window, but you expect it | The source does not know the window (`maxModelLen` refused under **Inputs**). Pass the engine arguments (`--engine-args`, and `--prefill-engine-args` for a P/D fleet) when you record or serve, or check your edges by hand. |

## Refused figures

A refused figure has a reason instead of a number. The page shows it on hover
and under **Inputs**; the API puts it in `refused`.

| Reason | What it means | Fix |
| --- | --- | --- |
| `NoPrefillCapacity` | No prefill capacity V_P: the prefill Pods never queued for compute long enough to learn it (deferred requests, such as those waiting for a cached prefix to load, do not count). SGLang always lands here; it exports no computed-token series. | Record a busier window, or assume V_P (**Assume a term**, API `assume.vp`). |
| `NoFixedLatency` | No fixed latency a: it is learned from low-load minutes at more than one prompt length. | Record a window with quiet minutes and mixed prompts, or assume a (`assume.a`). |
| `NoFixedLatency` with detail `P1 refused a: NoPromptSpread` or `TooFewLowLoadMinutes` | The quiet minutes did not span enough prompt lengths, or were too few, to tell the fixed cost a from the per-token cost. | As for `NoFixedLatency`. |
| `NoImbalance` | More than one prefill Pod and no f, the busiest Pod's share of the work. f = 1 is never assumed silently. | Record a window in which every prefill Pod carried load. |
| `NoLine` | No inter-token latency line to price TPOT from. | Answer at a later cycle where the line is learned (`--cycle N`, the page's **Cycle** box), or record a window in which decode's KV-cache use spans 0.30 or more ([what the line needs](metrics.md#what-your-prometheus-must-keep)). |
| `PinnedDefaultB` | The decode line rests on a 6 ms default for its intercept B, the per-token latency with the cache empty. No engine measured it: no full fit, and at that cycle no usable low-load reading from the last 30 minutes to take B from. `--assume-itl-b floor` reads the latest low-load readings in the recording, of any age. | Add `--assume-itl-b floor` (API `assume.itlBFromFloor: true`) to take B from what the decode Pods read at low KV fill, or a value: `--assume-itl-b 12ms` (API `assume.itlB: 0.012`; page **Assume a term**, ITL B in ms, which shows the floor's value: the page has no floor switch). Figures are flagged `AssumedITLB`. On a `held` or `borrowed` line it does nothing: pick a cycle where the line is `pinned-B` (`--cycle N`, the page's **Cycle** box). |
| `NoCapacity`, `NoBatchCap` | No KV capacity, or no batch cap, for the replica. | Make sure Prometheus keeps `vllm:cache_config_info`; pass the engine arguments (`--engine-args`, and `--prefill-engine-args` for a P/D fleet) or `--batch-cap` when you record or serve. |
| `NoTPOTHistogram` | A TPOT percentile asked with no ratio R_q of percentile to mean. | Type an assumed ratio (**TPOT percentile**, API `assume.tpotRatio`), or ask for the mean. |
| `TargetBelowFloor` | The target is under what any number of replicas reaches. | Raise the target. More replicas do not help. |
| `DecodeTTFTUnderLoad` | Decode has no ITL line, so TTFT uses the decode engine's measured TTFT. That TTFT was measured while requests waited on decode capacity, so it is a queue, not a floor. | For a recording, answer at the cycle the message names: the **Cycle** box (API `source.cycle`). For a Prometheus window, end the window at the time the message names (API `source.to`). On the CLI these are `--cycle` and `--to`. |
| `Saturated` | The rate is at or past the lower knee. | Lower the rate, or add replicas of the role whose knee is lower. |
| `AboveDecodeOnset`, `FlowControlUnknown` | With router flow control on, the rate is at the decode gate's onset, or the onsets cannot be placed. | Lower the rate; record with the router's series ([metrics](metrics.md#router-epp)). |
| `ReplicaBoundReached` (size) | No fleet within **Search up to** serves the rate. | Raise **Search up to** (API `maxReplicas`, at most 64), lower the rate, or raise the target that binds. |
| A refused term in a size | The search hit a refusal no replica count changes. | Fix that term as above, or raise the target. With **Size for the worst corner** ticked, untick it. |
| `NoPerRequestLengths` on the observed rate and bucket shares | Expected: the source carries no per-request lengths. | Nothing; the mix line uses the mean prompt and output. |
| `NotRecorded`, `UnreadableArgument` on `maxModelLen` | No `--max-model-len` in the recorded arguments, or one the planner cannot read. | Pass the engine arguments as the engine ran them. |

A TTFT flagged `DecodeQueueUnread` uses a decode TTFT the planner cannot check
for a queue: the engine does not report why requests wait (SGLang, or vLLM
before v0.20.0). If the floor is seconds where your clients see far less, it
was measured while decode was saturated. Answer at an earlier cycle or window.

A figure flagged `AssumedShape`, `AssumedTPOTRatio`, `AssumedITLB` or with an assumed term
stands, but rests on a value you typed. The **Flags** drawer says what clears
each flag. A TPOT percentile flagged `GlobalTPOTRatio` is priced at a fill the
source saw fewer than 5 windows at; record a window with more time at that load.

## The planner and the controller disagree on TPOT

The planner reads the decode line below its fitted band, at the fill a replica
actually runs at; the controller keeps the band's bottom (0.15 by default) as
its floor. Two things follow:

- A TPOT target between the line's intercept and its value at the band's bottom
  gets a rate from the planner, but the controller marks it infeasible and
  scales the variant on saturation alone.
- With short requests whose batch cap binds below the band, the controller
  prices up to about 1.6x less capacity a replica than the planner, so it may
  run more replicas than the planner sized.

Both are expected. They apply only to a line fitted on the variant's own
readings, in a window whose replicas also ran below the band: a borrowed or
default line, or one with no readings below the band, is read at the band's
bottom by both. A figure read below the band carries `BelowFittedBand`.

## API errors

| You see | Fix |
| --- | --- |
| `400` | The message names the field. Fix it; the [fields](api.md#the-scenario) list what each takes. |
| `401` | Send a bearer token (`Authorization: Bearer ...`); with a token for another audience, start the server with `--token-audiences`. |
| `403` "this server answers loopback names only" | The browser or a proxy named another host. Open `http://127.0.0.1:8080/`, or make the proxy keep the `Host` header. |
| `403` on a namespace | Add it to `--allowed-namespaces`, and with `--auth=kubernetes` grant the user `get` on `pods` there ([RBAC](api.md#kubernetes-authentication)). |
| `403` "may not get /whatif/..." | Grant `get` on the non-resource URL `/whatif/*` ([RBAC](api.md#kubernetes-authentication)). |
| `404` on an `inputsId` | The server no longer holds it, or restarted. Call `POST /v1/inputs` again. |
| `415` | Send `Content-Type: application/json`. |
| `503` with `Retry-After` | Busy: every plan slot is taken. Ask again after the delay. |
| `503` with `PlanBudgetExceeded` | The plan took longer than `--plan-budget` (default 5m) and was stopped. Untick "Show if all traffic were bucket b", use fewer bucket edges, or restart the server with a larger `--plan-budget` (at most 9m). [Details](api.md#the-plan-budget). |
| `503` "the request ended before it was priced" | The client went away or timed out. Ask again. |
| `500` | A server error; the message, or with `--auth=kubernetes` the server's stderr, says what. |

## Recording a window

The [record guide](../record-for-capacity-planner/) commands exit `0` when
done, `2` for your input, `1` for a failed query, and `3` (answer commands
only) for a refused role.

| You see | Fix |
| --- | --- |
| Exit 2: no replica with a KV-cache series | The namespace or window is wrong, or Prometheus does not keep `vllm:kv_cache_usage_perc` (`sglang:token_usage`). Check both. |
| Exit 2: window too large | The window passes the load bounds. Shorten it, lengthen `--step`, or add `--force` knowing the load ([Load on Prometheus](../record-for-capacity-planner/README.md#load-on-prometheus)). |
| Exit 2: overlapping `--role-of` selectors | Two selectors pick the same Pod, or their name prefixes nest. Make them distinct. |
| Exit 1: `query.max-samples` | One query returned more samples than Prometheus allows. Shorten the window, lengthen the step, or capture off-peak from a copy. |
| Exit 1: the token is refused over http | A token is sent over https only. Forward to Prometheus' https port and pass `--ca-file` ([Prometheus with a token](../record-for-capacity-planner/README.md#prometheus-with-a-token)). |
| The capture stops: samples off the step grid | A query frontend (Thanos, Mimir) aligned the range query. Query Prometheus itself, or use the curl and jq path. |
| `flag:` or `caveat:` lines | Not errors: something the window could not show. Read them before the answers. |
| `vLLM release not recorded` on every engine line | The recording was made before v0.12.0, the first release that reads the vLLM release, so it cannot show which release ran, nor that a variant ran two. Record the window again with v0.12.0 or later: the capture names each variant's release and adds a `flag:` line when a variant ran more than one ([the engine's release](../record-for-capacity-planner/README.md#the-vllm-release)). |
| Curl path: `MISSING` or `FAILED` lines, `bad=1` | Fetch those chunks again before you pack. On "too many samples", lower `CHUNK`. |
| Curl path: `0` series for `vllm:kv_cache_usage_perc` | The namespace or window is wrong. |
| Curl path: `window.txt NOT written` | A Deployment or its engine container was not found. Fix `DECODE_DEPLOYMENT` or `ENGINE_CONTAINER` and rerun the step. |
| Curl path: "the line above is not expanded" | Edit `DECODE_ARGS` (and `PREFILL_ARGS`) in `planner-export/window.txt` to the flags the engine really ran with. |
| Loading: a `STOP:` line | `window.txt` is not what the describe step writes. Do not go on; ask for a fresh export. |
| Loading: the converter refuses the export | A failed query, a chunk one metric has and another lacks, native histograms, or no sample at all. Fetch the export again. |
