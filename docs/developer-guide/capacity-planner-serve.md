# Capacity planner serve: internals

How `capacity-planner serve` is built: the page's scripts, the bucket edges,
the size search, shared calls, the plan budget, the caches and the tests.

What an operator runs and reads is in the guide:
[Capacity planner](../guides/capacity-planner/) for the page, and
[the HTTP API](../guides/capacity-planner/api.md) for every endpoint, field,
flag and error. This page does not repeat them.

This is phases 0 and 1 of [the capacity planner](../proposals/capacity-planner.md),
the API and the page at `/`, and phase 2's inverse mode (`POST /v1/size`) and
the page's saved scenarios and compare. The server is `cmd/capacity-planner`
(`serve.go`, `serve_inputs.go`, `serve_price.go`) over `internal/whatifapi`.

## Build and image

`make capacity-planner` builds `bin/capacity-planner` and stamps the commit into
the build, as the recordings it makes carry it. `capacity-planner` is not in
the manager image. Each release ships it as its own image,
`ghcr.io/llm-d-incubation/llm-d-scaling-manager/capacity-planner:<tag>`
(`Dockerfile.capacity-planner`: distroless, nonroot, entry point the binary),
and as binaries for linux, darwin and windows.
`make capacity-planner-image-check` builds the image and runs
`hack/check-capacity-planner-image.sh` against it, as the PR check does.

To look at the page against the goldens' recordings:

```
make capacity-planner
mkdir -p /tmp/recs
cp internal/engines/analyzers/learnreplay/testdata/goldens/ols-sweep/recording.jsonl.gz /tmp/recs/ols-sweep.jsonl.gz
bin/capacity-planner serve --recordings /tmp/recs
# open http://127.0.0.1:8080/#recording=ols-sweep.jsonl.gz&vp=30000&priceAs=1k&out=1000&ttft=0.5&tpot=0.004
```

The recording learned no V_P, so the link assumes one.

## The page

The page is four files in `internal/whatifapi/page/`, embedded in the binary
with `go:embed`: plain HTML, CSS and JavaScript, no library and no build step.
It is served on the same handler as the API, so the same check guards it:
loopback names only with `--auth=none`, and a bearer token allowed `get` on
`/whatif/` (the page) and `/whatif/page/*` (its files) with
`--auth=kubernetes`.

The page holds no model. It calls `GET /v1/sources`, `POST /v1/inputs`,
`POST /v1/plan` and, in size mode, `POST /v1/size`, and draws what they
return: a refused figure is drawn as a gap or its reason, never as a number,
and every string the API returns is escaped before it is drawn. It computes
only the axes' ranges and which knee is lower. Its Content-Security-Policy
allows its own script, style and API only, and no inline script or style. One
plan is in flight at a time; a knob moved meanwhile is priced when it returns,
and a response priced against inputs a later load replaced is dropped. The
rate prices on drag (100 ms after the last move), a target on release.

A toggle above the controls switches between the two questions: "How much does
this fleet serve?" (plan mode) and "What fleet does this traffic need?" (size
mode). In size mode the page posts the same scenario less the fleet to
`POST /v1/size`, with `maxReplicas` from the "Search up to" box and
`sizeOn: low` when "Size for the worst corner of V_P and f" is ticked; the
replica steppers become read-only outputs showing the answer, and the rest of
the page draws the plan the size returned. A rate prices on release in size
mode, not on drag, since each size searches several fleets. With no fleet
within the bound, the page shows the refusal and the figure at the bound, and
no plan. Where the answer's limit at its worst corner of V_P and f is under the
rate, the size box says so. The per-bucket view is unticked while sizing and
put back on switching to plan mode, which keeps the fleet the size found. A
response for a question since switched, or for inputs since reloaded, is
dropped. While a size runs the page dims the last answer and counts the
seconds; a newer question aborts the request (`AbortController`), and the
server stops pricing it. A size no fleet serves is explained in the page's
words (the "Search up to" box, the term to type, the target to raise), with
the server's detail shown only for refusals the page does not name
(`UNMET_SAID` in `planner.js`).

The address's fragment carries the source and the knobs
(`#recording=run.jsonl.gz&prefill=2&rate=6&ttft=0.8&vp=30000`), and the page
rewrites it after each plan. Seconds are seconds, as the API takes them. In
size mode it carries `mode=size`, `max` when it is not 32 and `low=1` when
sizing for the worst corner, and no replica counts: they are the answer.

### Saved scenarios and compare

Saved scenarios and compare (phase 2) are the page's alone; the API has no
endpoint for them and the server keeps nothing. A saved scenario is the
fragment's text (`linkParams()` in `planner.js`) under a name, in the
browser's `localStorage` key `capacity-planner.scenarios`, at most 50. An
exported one is a JSON file:

```json
{"kind": "capacity-planner-scenario", "version": 1, "name": "two prefill",
 "link": "recording=run.jsonl.gz&prefill=2&decode=1&rate=6&ttftStat=p90&ttft=0.8&tpotStat=mean&tpot=0.05&headroom=0.85",
 "saved": "2026-11-14T21:08:00.000Z"}
```

The logic that needs no page is in `internal/whatifapi/page/scenario.js`,
loaded before `planner.js`: `esc`, the checks, the store's edits, the names,
which sources a scenario can open, the link's handshake with the loads, the
import's branches and the compare table. `hack/check-planner-scenarios.sh`
(in `make lint-deploy-scripts`, which sets up node in CI) runs those
functions under node on fixtures, and fails without node.

A file (at most 64 KiB) and every store entry read back are checked by
`checkScenario` before use: the kind and version, a name of 1 to 80
characters with no control character, and a link of at most 4096 characters
whose every key is one `linkParams` writes, each once, numbers finite and not
negative, statistics `mean` or `pNN`, and a recording or namespace named. A
file that fails is refused with the reason. A store entry that fails is not
listed but is kept, and written back as it was on every save and delete
(`putScenario`, `dropScenario`): it may be another version's. The list is
built from `Option` nodes, so a name is never markup; elsewhere names reach
HTML only through `esc`. An import is planned by `importPlan`: one the same
as a saved scenario is opened without saving, one whose name another has is
saved as "name (2)" and so on (`freeName`), within the 80 characters, and the
store is written once or not at all. A generated name is `scenarioTitle`:
whole clauses dropped from the end, headroom first, to fit.

Opening a scenario, or the link the page opens on, needs a source this
server reads (`sourceProblem`): otherwise it is refused before any request,
and no other source is loaded in its place; an import is then saved and not
opened. The link's knobs are applied by one load (`state.link`,
`linkAtLoadStart`, `linkAtLoadEnd`): the newest load takes the link over, so
a Load pressed before the first load returns still applies it. The page's
own link waits for the first load that succeeds. A scenario's load that
fails puts back what `beginOpen` recorded (the link, the address, the source
form and the name box), and no later Load applies it.

"Pin as A" copies the plan response on screen (in size mode, the answer's
plan) into `state.pin`. It is not priced again: the graphs draw its TTFT and
TPOT curves dashed (class `pin`, a longer dash and a thicker line than the
guides), with its point B labelled "A: x r/s" ("(cap)" where its cap holds the
limit under it) and its target where that differs. A curve that is the
current one (`sameCurve`) is named "A: same as current" in the legend. The
compare box is `compareRows`: A's fleet, rate, limits, headroom, binding and
targets beside the plan on screen, with the change as printed (10.1 to 10.2
is +0.10), "now refused" or "now priced" where one side is refused, and
headroom coloured only at the same rate. The x axis runs far enough to show
both. With no plan on screen it says why instead (`state.priceFailed`: the
last price or size failed; no fleet within the bound; being priced, set as
soon as size mode starts), and Pin is disabled meanwhile.

## Bucket edges

Each bucket is named by its edge by `bucketName` in
`internal/whatifapi/scenario.go`, so the default edges keep their names and no
two edges share one.

Nothing in the inputs is per bucket (the observed mix is refused,
`NoPerRequestLengths`), so there is nothing to re-bucket and nothing to match:
no learned figure is priced from a bucket. When a recorder carries
per-request lengths and the mix gains shares, edges that do not match the
shares' buckets will need a rule; until then there is none to need.

Edges equal to the default are dropped from the scenario
(`Scenario.BucketEdges` is absent), so a plan that asks them has the same
`scenarioId` and bytes as one that does not. Other edges are part of the
`scenarioId` and of the limits' cache key. `POST /v1/size` refuses
`bucketEdges` before it looks its inputs up: a size prices the mix line alone.

The cap is 8 for cost. Each edge not priced before is a fleet's curve set at
five corners of V_P and f, priced while the plan holds its slot: on the worked
recording (model-02, V_P assumed 30000 tokens/s, TTFT p95 200 ms), eight fresh
edges took 13.4 s and a 548 KB response, against 9.0 s and 371 KB for the
default five on a cold server; in review, twelve fresh edges took 16 to 19 s.
An edge priced before is served from the curve cache (keyed by the fleet's
knobs, the prompt among them), whatever the plan's other edges.

**The context window.** `maxModelLen` is parsed in
`internal/signals/capacity/human_int.go` as vLLM's `human_readable_int_or_auto`
does (`vllm/utils/argparse_utils.py`); SGLang's `--context-length` is read as a
plain integer. Where either variant sets a value the parser cannot read, the
term is refused `UnreadableArgument` rather than taking the other's: that
variant may refuse shorter prompts than the other allows. The mean output
stands in for `max_tokens` in the `EdgeBeyondContext` check, since the source
does not carry it. The goldens' recordings run `--max-model-len 16384`, so a
default per-bucket plan on them flags its 32k and 128k lines.

On the page, `parseEdges` in `scenario.js` reads the **Bucket edges** box by
the server's rules. The page sends them as `bucketEdges` with the view only. A
box it cannot read prices nothing, and is marked (`aria-invalid`, a red
border, the reason under it, `edgesBoxProblem`) for as long as its text is not
the edges the lines show. The box is rewritten in its canonical form, each
edge by its bucket's name (`bucketName` there, as the server's), so the box,
the link and the lines' labels agree and one plan has one link. A link may
carry edges in any form `parseEdges` reads; the page uses them and rewrites
the link in canonical form (`linkedEdges`). Edges that do not parse are
refused by `linkProblem`, so a scenario file with edges out of order or too
many is refused on import. Compare reads the limits, which the edges do not
move; where A's and the current plan's edges differ, its table has a Buckets
row.

## The size search

Every fleet the search tries is priced as the plan's +1 steps are (the limits
at the targets and headroom, with no span over V_P and f), or with
`sizeOn: low` as the plan's own limits are (with the span, five times the
cost). Each count is found by galloping up from one replica (1, 2, 4, 8, ...
up to the bound) and then bisecting back, so the cost grows with the answer,
not with the bound: a rate one replica of each serves prices one fleet.

The search is `whatifapi.SmallestFleet`, which `at-replicas`' needed fleet
and the day plan's buckets run too. It ranks fleets by cost, each role's
replicas times its weight (`whatifapi.ReplicaCost`: the inputs'
`RoleInfo.GPUsPerReplica`, or 1 each where either role's is not stated),
then by the replicas in all, then by decode replicas. With equal weights that
is the fewest replicas in all, ties to fewer decode, the order before
v0.12.3.

1. Decode's floor: the fewest decode replicas whose own limit (TPOT, and the
   operating-point cap on decode's knee) reaches the rate, priced at one
   prefill replica, since the prefill count does not move it. With no TPOT
   asked, one. No fleet with fewer decode replicas reaches the rate.
2. From the floor up, the fewest prefill replicas whose fleet limit reaches
   the rate at each decode count, each search bounded by the count before
   (more decode never needs more prefill). At one decode count the fewest
   prefill replicas rank first on every key. It stops once one prefill
   replica beside the decode count ranks no better than the best found
   (every fleet with more decode is at least that on each key), or one
   prefill replica serves. `TestSmallestFleet_MatchesExhaustive` checks the
   stop against trying every fleet in the bound. This is the
   coupling: decode admission holds the prefill limit down, and where it sets
   most of a TTFT, the floor's prefill count can be several times what one
   more decode replica needs (on the staircase at TTFT p95, 19 prefill + 5
   decode against 5 + 6). Taking the cheapest in all also makes the answer's
   cost rise with the rate.

A limit reaches the rate when it is no more than a billionth under it: limits
are bisections' answers, and on the worked recording the fleet limit wanders
by up to 8e-13 of itself as decode replicas are added on a plateau.

The search holds one plan slot from its start through its answer's plan, which
it prices as `POST /v1/plan` would (same request, the fleet set) and reads the
answer's limit, headroom and binding from, so the answer is what a plan of
that fleet says. The search is kept by `sizeId` before that plan is priced:
a size whose plan did not finish (its client gone) is not searched again.
Decode's own limit has no span, so its probes are priced without one even
when sized `low`.

The search takes each limit to rise with its replica count. Where one does
not, galloping and bisecting can step over a smaller count that serves the
rate; the count just below the answer is always priced and checked, so a fleet
with one replica fewer of either role that serves the rate is a 500, not an
answer, and so is a plan at the answer that does not.

## Shared calls

`flight` (`internal/whatifapi/flight.go`) runs one call per key at a time:
callers that ask for a key while its call runs wait for it and share its
result, so identical cold requests price once and take one slot. The server
has one per kind (`planFlight` by `scenarioId`, `sizeFlight` by `sizeId`,
`curveFlight`, `limitFlight`), and the authenticator one for its reviews. Where a size's answer plan is
already being priced for another request, the size waits for it; if that
request was turned away busy, the size asks once more, on its own slot. A
caller never reruns its own call on a deadline error.

Plan slots are a channel of `MaxPlans` tokens (the CPU count) and input slots
one of `MaxInputs` (`--max-inputs`, two by default); a request that finds none
free is `errBusy`, a 503 with `Retry-After: 1`. The inputs cache holds at most
16 entries and `MaxInputsBytes` (`--inputs-cache-size`) in all, each weighed
by `Resolved.Bytes`, an estimate: `heapEstimate` of `sizeOf`
(`cmd/capacity-planner/sizeof.go`) of the replayed state, the series and the
inputs document, a quarter over what `sizeOf` reaches for the allocator's
rounding. A file rewritten during a resolve is refused after the last pass
(`recordingSource.unchanged`): its size, modification time or file identity
moved, so nothing is cached under the first pass's content id.

A recording is never held whole. `recordingSource` opens the file once and
reads it in passes, gunzipped as it goes: `check` (every line, and the
content hash), then `learnreplay.StateFrom` (resolve the model, replay to the
cycle, with no recording of its own) and `learnreplay.SeriesFrom`. Each pass
holds one line (`learnrec.Scanner`). `TestServeResolvesALargeRecordingWithoutHoldingIt`
holds it to that. A plan whose client goes away stops between figures.
The HTTP server's timeouts are in `serve.go`: read 30 s, idle 2 minutes,
write 10 minutes (`writeTimeout`).

## The plan budget

`planWithin` prices a plan under
`context.WithTimeoutCause(ctx, planBudget, errPlanBudget)`. Only the budget's
own end is a `BudgetError` (503, reason `PlanBudgetExceeded`, written by
`writeErrorReason`): a plan whose client ended first is the client's end, with
no reason, and an error the plan returns for its own reason as the budget ends
is that error. Nothing of a stopped plan is cached, and its slot is freed.

The budget is checked between figures, so a plan can run past it by one
figure. A size's search is not under the budget, only the plan it answers
with. A request that joins a plan already in flight shares that plan's clock.
`maxPlanBudget` in `serve.go` is 9 minutes, a minute under `writeTimeout`, so
the refusal is written before the connection closes; `--plan-budget` 0 is
`DefaultPlanBudget` (5 minutes).

The page retries only a 503 with `Retry-After` (`retryDelay` in
`scenario.js`), so it never retries `PlanBudgetExceeded`.

## Caching and latency

The server keeps caches only: the inputs by `inputsId`, each plan by
`scenarioId`, each fleet's curves by its knobs (replicas, mix, statistics,
assumptions), and each fleet's limits and marginal steps by its knobs, the
targets and the headroom, and each size by `sizeId`. So moving the rate prices
the two points alone; moving a target prices the limits again, not the curves;
a new replica count or assumption prices everything.

The plans (512) and the sizes (128) are kept as response bytes, each cache
also bounded at 64 MiB (`defaultResponseBytes`): a per-bucket plan with 8
edges is about 550 KB, so 512 of them would hold some 280 MB. The newest
response is kept whatever it weighs. The curve sets (256) and limit entries
are bounded by count alone; their size in bytes was not measured.

Measured on the goldens' recordings (`go test ./cmd/capacity-planner -run '^$' -bench
Plan`), see [As built](../proposals/capacity-planner.md#as-built-phase-0).

## Tests

| Test | Holds |
| --- | --- |
| `TestServeParity` (`cmd/capacity-planner`) | Every JSON golden in `cmd/capacity-planner/testdata`, asked of the API, gives the same limits, points, bounds, flags and refusals. |
| `TestServeCurvesAgreeWithPoints` | Each curve point is `TTFTAt` and `SLOAtReplicas` at its rate; point B lies on the curve; past the lower knee every point is refused `Saturated`. |
| `TestSchemaIsPinned` (`internal/whatifapi`) | The key list. `-update` rewrites it. |
| `TestPlanRefusalShape`, `apitest.CheckFigures` | No number beside a refusal, and no refusal written as 0, on every response. |
| `TestCaching` | The same scenario is the same id and bytes; a rate move prices no curve and no limit; a target move prices limits only. |
| `TestBucketEdgesPriceTheirOwnLines`, `TestBucketTargetsFollowTheEdges`, `TestServeBucketEdges` | A plan's edges give one line per edge, named by it and priced at it, the CLI's fleet at every curve point on the goldens' worked fleet; the limits do not move; bucket targets name the plan's buckets. |
| `TestDefaultBucketEdgesAreNoEdges`, `TestBucketEdgesKeyTheScenario` | The default edges asked are the plan with none, byte for byte; other edges are another `scenarioId`, priced on their own. |
| `TestSharedEdgeIsCached`, `TestLRUBoundedInBytes` | An edge two plans share is priced once; the plan and size caches drop the least used responses past their bytes. |
| `TestEdgeBeyondContext`, `TestEdgeBeyondContextOnARefusal`, `TestServeBucketEdges` | A line whose edge plus the output passes a known context window is flagged `EdgeBeyondContext` on its points, on the refusal where they are refused, and in the flag list; the mix line and limits are not; with the window unknown, refused or 0, nothing is. |
| `TestContextOf`, `TestParseMaxModelLen`, `TestMaxModelLenSuffixesInArgs` | The window is the smallest of the fleet's own variants', unknown where one is unreadable; `--max-model-len` is read with vLLM's suffixes. |
| `TestBucketPointBFollowsItsEdge`, `TestWithFlagCopies`, `TestLRUBytesEdges`, `TestSizeRefusesEdgesFirst` | Each line's point B is its own edge's limit; adding a flag never writes into a shared slice; the byte bound on growth and at the bound; a size refuses edges before it looks its inputs up. |
| `TestBucketEdgesRefused`, `TestPageSetsBucketEdges` | Edges asked wrongly, or of a size, are a 400 naming the edge and why; the page keeps the edges in the link and sends them with the view only. |
| `TestPageServed`, `TestServeServesThePage` | The page and its files are served from the binary with their content types and security headers; anything else under `/page/` is a 404. |
| `TestPageBehindAuth` | The page sits behind the API's check: 401 with no token, 403 for a user not allowed `/whatif/`, 403 for a foreign Host on loopback. |
| `TestPageLoadsNothingExternal`, `TestPageIDsExist` | The page names no other origin, runs no inline script or style, and uses no absolute path; every element its script reads is on it. |
| `TestPageNamesEveryFlag` | Every flag the engine, the SLO analyzer and the CLI declare has plain words on the page. |
| `TestSourcesListingFails` | A listing that fails is answered by its error's class, never as a listing. |
| `TestServeSources`, `TestSourcesWithoutLister` | `GET /v1/sources` lists every recording a `recording` source accepts, links included, and nothing it refuses; it says when it cut the list, and whether a Prometheus is read; an error names no server path. |
| `TestSizeAnswerIsThePlan`, `TestServeSizeIsThePlan` | A size's answer is `POST /v1/plan` at its fleet, byte for byte on the stub and key for key on the goldens' worked fleet. |
| `TestSizeOneFewerFails` | One replica fewer of either role misses the rate, in the step and in a plan of that fleet. |
| `TestSizeRefusals`, `TestServeSizeRefusesATermRefused` | No fleet within the bound is `unmet` with its reason and the figure at the bound: the bound reached by prefill, by decode, or by the fleet with neither role alone short; a refused term; a TPOT percentile with no ratio; no advice to raise a bound already at 64. |
| `TestSizeRequestsRefused`, `TestSizeCachingAndCost` | A size asked wrongly is a 400 naming the field; the same size is the same bytes, searched once, in a handful of fleets, and one fleet for an answer of one of each. |
| `TestSizeSearchSurvivesItsPlan` | One plan slot sizes; a size whose answer's plan ended with its client is not searched again. |
| `TestSizeOnTheLowEnd` | `sizeOn: low` sizes on the low end of the span, where `point` leaves it under the rate; the answer's headroom is the low end's. |
| `TestSizeOnLowRefusedAtTheWorstCorner` | Sized `low` where the worst corner refuses the limit, the size is that refusal, said of the corner, never `ReplicaBoundReached`. |
| `TestPlanPastItsBudgetIsRefused`, `TestPlanEndedByItsClientIsNotTheBudget`, `TestPlanErrorAsTheBudgetEndsIsItsOwn`, `TestPlanWithinNeedsAllThreeConditions`, `TestPlanSharedPastItsBudget`, `TestFlightCallerKeepsItsOwnDeadlineError`, `TestServePlanBudgetBounds`, `TestPageRetriesOnlyABusyServer` | A plan past its budget is a 503 with reason `PlanBudgetExceeded` within a figure of the budget, caches nothing and frees its slot; a plan whose client ends first is the client's end, with no reason; an error the plan returns for its own reason as the budget ends is that error; each of the three conditions alone; a request sharing the plan gets the same refusal and a later one prices afresh; a caller never reruns its own call on a deadline error; `--plan-budget` 0 is the default and above 9m is refused; the page retries only a busy server. |
| `TestServeKneeRoundingPricesAtOnce` | At 12 prefill and 19 decode replicas, the curve point at the prefill knee is refused `Saturated` in milliseconds. Two imbalance corners sit one rounding step under it there, and the point used to take over 30 minutes. |
| `TestSizeConcurrentSearchOnce`, `TestSizeBusyAndDecodeOnly` | Identical sizes at once search once; with every slot taken a size is a 503 before any search; a source with no prefill variant is a 400. |
| `TestSizeOneFewerThatServesIsAnError` | On a limit that dips, a fleet one replica fewer that serves the rate is a 500, not an answer. |
| `TestSizeBehindAuth`, `TestPageAsksSize` | `/v1/size` sits behind the same check as the other endpoints, authorized as `get` on `/whatif/v1/size`; the page has the mode toggle and posts sizes by a relative path. |
| `TestPageSavesAndCompares` | The save, open, delete, export, import, pin and unpin controls are on the page and wired to `scenario.js`'s functions; the store and the file hold the link's text; every key the link writes, the assumed terms' included, is one an import admits; no line of either script that builds markup puts a name in raw; a pinned plan is a copy drawn dashed. |
| `hack/check-planner-scenarios.sh` | Runs `scenario.js` under node (217 cases; it fails without node): `esc`, `retryDelay` (only a 503 with `Retry-After` is retried, never `PlanBudgetExceeded`), the bucket edges by the server's rules, the box's mark, the link's edges and the lines' styles, the link and name checks on good and hostile fixtures, the store keeping entries it cannot read, the cap, distinct suffixed names for repeated 80-character imports, the import's branches, which sources open, the link's handshake with loads that fail or are overtaken, generated names, and the compare table: every text escaped, refusals said, changes as printed, headroom coloured only at the same rate. |
| `TestCheckListen`, `TestAuthenticate`, `TestServeRefusesUnsafeBinds` | Loopback serves with no token; any other bind refuses to start without `--auth=kubernetes`; with it, a token the SubjectAccessReview does not allow is 403. |
