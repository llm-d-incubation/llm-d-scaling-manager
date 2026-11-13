# llm-d-scaling-manager Cycle Log

Each reconcile cycle, the saturation engine writes these INFO lines per model:
one `analyzer-result` for each analyzer that ran, one for the composite the
optimizer acted on, and one `scaling-decision`; a `throughput-demand-floor`
line on a cycle where the throughput floor changes a role's demand; and a
`composite holds a role` line for each held role. They show what the analyzer and optimizer computed,
with no debug logging needed.

The log uses zap's development **console** format, not JSON: timestamp, level,
caller, message, then the fields as one JSON object. A line looks like this
(wrapped here; it is one line in the log):

```text
2026-10-14T09:12:03.512Z  INFO  steadystate/engine_v2.go:1246  analyzer-result
  {"modelID": "my-model", "namespace": "default", "analyzer": "saturation", ...}
```

Top-level fields are written `"key": value`, with a space after the colon.
Nested values (`variants`, `decisions`) are compact JSON with no spaces. The
greps below rely on both.

---

## Log lines

### `analyzer-result`

One line per analyzer that ran for a model, written right after the threshold
post-step. The values are what the optimizer receives.

```text
2026-10-14T09:12:03.512Z  INFO  analyzer-result  {"modelID": "my-model", "namespace": "default",
  "analyzer": "saturation", "live": true, "supply": 2362173, "demand": 2240000, "util": 0.95, "rc": 273121,
  "sc": 0, "scaleUpThreshold": 0.85, "scaleDownBoundary": 0.7,
  "variants": [{"name":"primary","prc":1152000,"role":"both","reason":"P3-k2","observed":1},
               {"name":"v2","prc":403391,"role":"both","reason":"P1-obs","observed":3}]}
```

A P/D model also carries `roleRC` and `roleSC`: the per-role required and spare
capacity the optimizer branches on.

| Field | Description |
|---|---|
| `modelID` | the scaling manager model ID (unique within a namespace) |
| `namespace` | Kubernetes namespace |
| `analyzer` | Analyzer name, e.g. `"saturation"`, `"throughput"` |
| `live` | Whether the analyzer produced a good reading within its staleness window. It counts toward the composite only when the policy lists it, and `slo` never counts. `false` does not mean the other fields on this line are missing or stale-looking — they are this analyzer's real numbers |
| `supply` | Total token supply across ready replicas (readyCount × perReplicaCapacity) |
| `demand` | Total token demand. For saturation V2 it is the sum of three terms: resident KV tokens; a role-aware projection of requests waiting in each replica's engine queue; and a model-level, prefix-cache-discounted projection of requests still queued in llm-d flow control (`SchedulerQueue`, not attributed to any variant). See [scaling-policy-config.md](scaling-policy.md) |
| `util` | `demand / supply`; > 1.0 means the model is over capacity |
| `rc` | Required capacity (post-threshold): > 0 triggers scale-up |
| `sc` | Spare capacity (post-threshold): > 0 permits scale-down |
| `scaleUpThreshold` | Scale-up threshold for this analyzer (from config) |
| `scaleDownBoundary` | Scale-down boundary for this analyzer (from config) |
| `variants[].name` | Variant name |
| `variants[].prc` | Per-replica capacity in analyzer units (tokens for saturation) |
| `variants[].role` | P/D role: `prefill`, `decode`, or `both`. It reads `both` when the scale target has no `llm-d.ai/role` label, and when the analyzer does not set a role. Saturation V2 charges waiting requests by this role, so you need it to read `demand` |
| `variants[].reason` | How the variant's capacity was computed (see below) |
| `variants[].observed` | Replicas that reported metrics this cycle, before any cap; compare with the target's ready replicas |
| `roleRC`, `roleSC` | Per-role `rc` and `sc`; P/D models only. A role over its own threshold keeps the model scaling up even when the model-level `rc` is 0 |

An analyzer that computes no per-variant capacity writes an empty `variants`
array. With several analyzers enabled there are several `analyzer-result` lines,
each with the same `modelID`/`namespace` and its own `analyzer`.

#### The composite row

One additional `analyzer-result` line appears every cycle with
`"analyzer": "CompositeSignal"` — the single reduced signal the optimizer
actually consumes, never saturation's raw result even when saturation is the
only analyzer enabled. It is emitted through the exact same log line as any
analyzer, not a separate line shape, so every field above means the same
thing here.

```text
2026-10-14T09:12:03.512Z  INFO  analyzer-result  {"modelID": "my-model", "namespace": "default",
  "analyzer": "CompositeSignal", "live": true, "supply": 2362173, "demand": 2688000, "util": 1.14,
  "rc": 800180, "sc": 0, "scaleUpThreshold": 0.85, "scaleDownBoundary": 0.7,
  "variants": [{"name":"primary","prc":1152000,"role":"both","reason":"C0-agree","observed":1},
               {"name":"v2","prc":403391,"role":"both","reason":"C1-single","observed":3}]}
```

`demand` is in **saturation's units** always, so the numbers here are directly
comparable to saturation's own `analyzer-result` line above. When another
analyzer asks for more replicas than saturation, the composite raises `demand`
to what that many replicas are worth at saturation's `prc`, so on this line
`demand` is a target, not a
measurement. Above, another analyzer needs 2.33 `primary` replicas, which at
1152000 each is 2688000 against saturation's measured 2240000. This is the
same cycle as the saturation line: `supply` is the same one `primary` at
1152000 plus three `v2` at 403391, and the higher `demand` turns saturation's
`rc` of 273121 into 800180. `prc` is saturation's here because each role has one
variant. Where a role has variants on two accelerators and another analyzer
decides them, each gets the `prc` at which `demand` needs exactly the replicas
that analyzer asked of it, so its `prc` can differ from saturation's.

`variants[].reason` on the composite row is **not** an analyzer's capacity
provenance (`P0-store`, `T1-ols`, ...) — it is the composite's own decision
path (see the table below), naming how that variant's aggregated replica
count was reached, not how any one analyzer measured its own capacity.

### `scaling-decision`

One line per model, after the optimizer chose each variant's replica target.

```text
2026-10-14T09:12:03.540Z  INFO  scaling-decision  {"modelID": "my-model", "namespace": "default",
  "decisions": [{"name":"primary","curr":1,"tgt":2,"action":"scale-up"},
                {"name":"v2","curr":3,"tgt":3,"action":"no-change","atMax":true},
                {"name":"v3","curr":1,"tgt":1,"action":"no-change","limitedBy":"team-quota"}]}
```

| Field | Description |
|---|---|
| `modelID` | the scaling manager model ID |
| `namespace` | Kubernetes namespace |
| `decisions[].name` | Variant name |
| `decisions[].curr` | Replica count at this cycle |
| `decisions[].tgt` | Target replica count chosen by the optimizer |
| `decisions[].action` | `scale-up`, `scale-down`, or `no-change` |
| `decisions[].atMax` | `true` when the target is the variant's `maxReplicaCount`; absent otherwise. Same as `llmsm_variant_at_max_replicas` |
| `decisions[].limitedBy` | what held the variant's scale-up back this cycle: a limiter entry's name, `gpu-limiter` for the physical limiter, `node-fit`, `pd-pair-borrows-whole` or `lender-cannot-reclaim`. Absent when nothing did. The same name as the `ResourceConstrained` Event and the `limiter_name` label of `llmsm_decisions_limited_total` |
| `decisions[].limitedByAlso` | the other limiters that held it back exactly as tightly, in the `limiters:` order; absent when one did alone. The Event names them too; the metric label carries `limitedBy` alone |

### `throughput-demand-floor`

Emitted per role during the analysis, so before that model's
`analyzer-result`, and only on a cycle where the
throughput floor changed that role's demand. The floor takes the larger of
the resident KV demand (occupancy with the queues' residency charge removed,
since the queue is priced as a backlog instead) and what the arrival rate and
the queue ask for at the throughput one replica sustains
(`saturatedThroughput`). So the line also appears when that LOWERS demand: a
queue priced as a backlog can be worth less than its residency charge. It
lowers demand only when `mayOrder` is `true`, `heldAtFleet` is `false`, and
`arrivalRate` accounts for at least half of the resident KV
(`arrivalRate / saturatedThroughput x perReplicaCapacity >= 0.5 x
residentDemand`). Otherwise the arrival rate is still reading the minute
before a load step, and `flooredTo` is never below `demandBeforeFloor`. Most
cycles of a fleet that is coping have none.

| Field | Description |
|---|---|
| `role` | `prefill`, `decode` or `both` |
| `demandBeforeFloor`, `flooredTo` | the role's demand before and after the floor, in the same unit as `analyzer-result`'s `demand` |
| `residentDemand` | `demandBeforeFloor` with the queues' residency charge removed: what the replicas hold now |
| `perReplicaCapacity` | the per-replica capacity the floor converts replicas into demand with. For a `derived` rate it is the variant's capacity times `scaleUpThreshold`: that rate is already the rate at the target fill, so the engine's scale-up headroom is not applied to it a second time |
| `arrivalRate`, `saturatedThroughput`, `replicasImplied` | the requests per second arriving, what one replica completes at saturation, and the replicas their ratio asks for |
| `backlogRequests`, `projectedBacklog`, `drainSeconds` | the queue measured, the queue the floor priced over the next replica's start time, and how fast it is to be drained. In `backlogRequests` each replica's own engine queue counts for at most what that replica drains in the drain time (its `saturatedThroughput` x `drainSeconds`, or x the start time where longer): added replicas cannot take a request waiting inside another engine. The router's queue counts in full. While `backlogGrew` is `true` no queue is capped, and `backlogRequests` equals `queuedRequests` |
| `queuedRequests` | the queues `backlogRequests` is built from, with no engine queue capped |
| `heldAtFleet` | `true` when the floor was capped at what the current fleet supplies at the scale-up threshold: it may hold the fleet up, but not order more replicas |
| `heldWhy` | why it was capped; see below. Empty when `heldAtFleet` is `false` |
| `mayOrder`, `borrowedOnly` | whether the role's rate is one the floor may order on, and whether every rate it has is borrowed rather than measured by the role's own replicas |
| `lowerBound`, `growthCapped` | `lowerBound` is `true` when prefill's rate is a lower bound: learned while prefill queued behind a full decode (only with [`enablePrefillThroughputBound`](scaling-policy.md#enableprefillthroughputbound) on), or, below the knee, read from how long each request occupies a prefill replica and how many it completes (on by default). `growthCapped` is `true` when the floor on that bound was cut to twice the ready prefill fleet |
| `orderedBehindQueue`, `queueJustifiedReplicas` | whether a held floor still ordered behind a standing queue, and how many replicas the queue was worth when it did |
| `muKey`, `muSource` | the learned key the role's `saturatedThroughput` was priced under, and where it came from: `measured`, `derived`, `served-siblings` (a measurement yielded to what its full siblings serve), `neighbour`, `borrowed-line` or `lower-bound`. On a role of several variants, the variant whose figure is nearest the role's. Empty on a cycle with none. The key's output-length bucket is the one the k2 window uses (see `P1-obs` below), so both are read from the same bucket. While the output length is in the middle of a large change (20% past a bucket boundary, for up to 2 minutes) the key keeps its old bucket, that window takes no new readings, and its figure counts as `neighbour`: the derived rate prices where there is one |
| `startEstimateSeconds`, `startSource` | the replica start time `projectedBacklog` was projected over, and where it came from: `measured`, or `seed` (the ScaledObject's `replicaStartSeconds`, for a role with no start measured); `none` (and 0) when neither is known, and the floor projects nothing |

On a disaggregated fleet the per-role lines do not add up to the model's
total, so a second line, `throughput-demand-floor-total`, gives it:
`modelTotalBefore` and `modelTotalAfter`, and the `roles` the floor moved.

**`heldWhy` values.** Each says which reading the floor would not order on:

| Value | Meaning | What changes it |
| --- | --- | --- |
| `single-sample` | the role's own throughput window holds one reading | more saturated readings |
| `borrowed` | the only rate is borrowed from a neighbouring output-length bucket, or the output length is in the middle of a large change and the old bucket's window is held for up to 2 minutes | a reading in the bucket the traffic is in now |
| `borrowed-line` | the rate is derived from an inter-token-latency line another variant fitted (shared because `enableLearnedStateReuse` is on) | the variant fitting its own line |
| `shape-change` | the traffic's shape changed and the rate was measured on the old one | the new shape settling |
| `lower-bound` | prefill's only rate is a lower bound with one reading | a second reading a minute later, or prefill's own measurement |
| `borrowed+shape-change`, `borrowed-line+shape-change`, `lower-bound+shape-change` | both at once; the borrow is named first because it is the one that does not go away on its own | as above, both |

A borrowed figure never orders replicas here: it can hold demand up while the
role measures itself, never raise it above what the fleet already supplies.
A prefill lower bound with two readings may order, up to twice the ready
prefill fleet in one cycle.

`prefill-throughput-bound-reading` is logged each time a queued prefill replica
adds a reading to its lower bound: `computedTokenRate`, `effectiveInputTokens`,
and `reading`, their ratio in requests per second.

Below the knee a prefill replica's own queue reads empty, so neither its
measured rate nor the bound above is learned. Each prefill replica then
reads `max(requests completed per second, 1 / mean service time)` (per engine
rank) every cycle: at least what it served, and at least one prompt at a time,
so a lower bound on what it sustains. The floor prices prefill on it with
`lowerBound=true` and `muSource` `lower-bound`, and `replica-capacity-decision`
(at `--v=3`) names its bucket `busy-share`. It keeps the highest figure a
quieter period gave, because prompts that share a step under load each take
longer. A prefill priced on either bound also keeps its share of the router
queue only up to twice its ready fleet.

### `replica-start-measured`, `replica-start-outlier-rejected` and `replica-start-change-confirmed`

How long a replica takes to become Ready, per variant. The throughput floor
prices the backlog over this time, so a long start raises `projectedBacklog`.

`replica-start-measured` is logged on a cycle where new replicas of the variant
became Ready: `namespace`, `variant`, `observations` (replicas counted this
cycle), `estimateSeconds` (the published estimate), `source` (`measured`, or
`seed` while nothing is measured), and:

- `pods`: how many different pods the estimate is from, at most 3.
- `prior` and `priorSource`: what the estimate starts from. `priorSource` is
  `seed` when the ScaledObject sets
  [`replicaStartSeconds`](scaledobject-trigger.md), else `default` (70 s).
- `sizedSeconds`: the start the floor and share pacing use.

Until 3 pods are measured, `estimateSeconds` is a blend of the prior and the
measured starts: `((3 - pods) * prior + pods * mean) / 3`. With a seed S it
stays between S/2 and 2S. `sizedSeconds` is never shorter than a measured
start, so the blend can only make the controller order more. From the third
pod on, both are the measured estimate.

`A variant's replicaStartSeconds is contradicted by its measured starts` is
logged once per variant and seed, and again after a controller restart, while
the mean of the measured starts is outside half to twice the seed:
`replicaStartSeconds`, `measuredSeconds`, `pods`, `lowSeconds`, `highSeconds`,
`publishedSeconds`, `sizedSeconds` and `podsNeeded` (3). Until three pods have
started, the seed holds the estimate within those bounds, and sizing at no less
than half the seed. For a variant with one replica that can take days. Correct
or remove the seed on the ScaledObject.

`replica-start-outlier-rejected` is logged when one replica's start is more
than `factor` (3) times the estimate and is left out: `namespace`, `variant`,
`pod`, `sampleSeconds`, `estimateSeconds`, `factor`, `distinctPods` (how many
different pods have been left out so far; 0 when a new pod's start inside the
bound cleared the count on the same cycle), `needed` (how many it takes, 3)
and `reason` (`start-above-bound`). Two causes are common: a readiness probe
that fails and recovers, which makes a pod report its age as its start time,
and a real change, such as a slower image or node.

- A pod is logged when it is first left out, and again every 10 minutes
  while it still is.
- One pod reporting again and again counts once, so a single blip never
  moves the estimate.
- The pods count across rollouts, not only while they run together. A
  variant with one replica learns a slower image on its third slow start.
- A start inside the bound from a new pod clears the count.

`replica-start-change-confirmed` is logged when `needed` different pods have
been left out: the estimate becomes the median of their starts.
`distinctPods`, `medianSeconds` and `previousEstimateSeconds` say what
changed.

If the variant really got slower and you do not want to wait for three slow
starts, forget its learned state with the `llm-d.ai/forget-learned-state`
annotation ([troubleshooting](troubleshooting.md#reset-one-variants-learned-state)).
On the next cycle the starts of the pods running then are measured afresh.
To give a new variant a better starting point than 70 s, set
[`replicaStartSeconds`](scaledobject-trigger.md) on its ScaledObject.

### `learner-start`

One line per model, on the analyzer's first cycle for it: after the
controller starts, or when a model first appears (or comes back after being
unseen long enough to be forgotten). It says what the learners begin with:

- `restoredKeys`: learned keys restored from the snapshot, per learner (`k2`,
  `mu`, `itl`).
- `coldVariants`: variants with none, per learner.
- `startKeys`: replica start estimates restored from the snapshot.
- `sightings`: replicas whose first sight came back from the snapshot, so
  they do not wait again. Always 0 with the snapshot off.
- `oldestAdmittedAgeSeconds`: the oldest restored key's last admitted
  reading, `-1` when none.
- `settleUntil`: when k2's wait ends for the replicas that wait, at the latest.
  With the snapshot off that is every replica, a rate window from this cycle.
  With it on, a replica with a restored sighting has no new wait, so when no
  replica is cold `settleUntil` is this cycle, or the end of a wait already
  under way before the restart. Until then a saturated replica that waits
  reads `P1-obs-unsettled`, and with the snapshot off a variant with no k2 is
  priced at k1. The ITL line's own wait, `steadyFor`, runs on past it.
- `reason`: one of three. "...each replica waits its settle, k2 and the mu
  written beside it a rate window from the controller's first sight of it,
  so by settleUntil at the latest" is the default. "...a replica whose first
  sight the snapshot restored keeps its settle, and each other replica waits
  its settle; settleUntil is when the last k2 wait ends, and the ITL line's
  wait, steadyFor, runs on past it" appears when `sightings` is above 0. "...replicas already Ready keep
  their settle" appears only when a replay turns the controller-restart wait
  off; then `settleUntil` is the cycle itself.

```text
2026-11-01T09:00:00.020Z  INFO  learner-start  {"modelID": "my-model",
  "namespace": "default", "restoredKeys": {"itl": 0, "k2": 0, "mu": 0},
  "coldVariants": {"k2": 1, "mu": 1}, "startKeys": 0, "sightings": 0,
  "oldestAdmittedAgeSeconds": -1, "settleUntil": "2026-11-01T09:01:00Z",
  "reason": "the analyzer's first cycle for this model, after a controller start or as the model first appears: each replica waits its settle, k2 and the mu written beside it a rate window from the controller's first sight of it, so by settleUntil at the latest"}
```

### `learner-write` (`--v=3`)

At most one line per learned key per cycle, when its figure is set for the
first time, moves more than its band from the value last logged (k2: 2 %), or
has not logged for 10 minutes: `learner` (`k2`), `variant`, `role`, `key`,
`value`, `previousValue`, `firstWrite`, `source`, `samples`, `restored`, and
`pods`: the pods whose readings were admitted this cycle, lowest reading first, at most 16,
each with its `reading`, `timeBase`, `passedBy` (the admission test it passed),
`settledForSeconds` and `queueOverThreshold` (its busiest rank's queue over
the saturation threshold); `podsOmitted` counts the rest.

`restored` is `true`, for `k2`, `mu`, `itl` and `start` alike, while the figure
came from the snapshot and the learner has admitted no reading under the key
since the controller restarted. `source` stays what it was: a restore is a
field, not a source. A `k2-decision` line at `P2-hist` carries `restored` too.
For `mu` an exact repeat of the last reading is not admitted, so a fleet whose
rate does not move stays `restored` while it serves. `itl` is `true` while its
window has kept no reading, on a line priced from the restored intercept or
held slope; an intercept put back after a rollback of engine arguments reads
`false`, restored or not.

For `learner` `mu` the key is the throughput key, `value` the service rate
the variant was priced at (requests per second), and `source` where it came
from, as `muSource` above. A change of `source` logs whatever the value, with
`previousSource`, so each flip of the measured/derived choice is one line;
otherwise the band is 5 %. `pods` lists the readings admitted this cycle.
A mu reading is admitted beside a k2 reading, so each entry normally carries
that reading's `settledForSeconds` and `passedBy`; an entry with no k2
margin on record leaves both out.

For `learner` `itl` the key is the ITL window of the decode or aggregated (unlabeled) variant, and the line
carries `a` and `b` (seconds per token, and seconds per token per unit of KV
fill) with `previousA` and `previousB`, `source` (`ols` for a full fit,
`pinned-B` for a fit of A with B pinned, `pinned-B-held` for the last A held
while too few settled readings remain), `samples` (readings in the window),
`steadyHeld` and `slopeAgeSeconds` (how old a held A is, 0 for a fit made this
cycle). It logs when the line is set for the first time, when `source`
changes, when A or B moves more than 5 % from the value last logged, or after
10 minutes. The throughput and slo analyzers' ITL lines log the same fields,
with `analyzer` and `settling`, and `source` in their own words.

`pods` lists the pods whose readings the line was fitted from this cycle,
lowest `reading` first, at most 16; `podsOmitted` counts the rest. Each entry
carries `reading` (the pod's mean ITL over those readings, seconds per token),
`k` (their mean KV fill), `rows` (how many of the fit's readings were the
pod's) and `timeBase`. A held slope fitted nothing this cycle and lists no
pods. The line also carries the full fit's admission test with its margins:
`kSpread` (the steady readings' spread in KV fill) against `kSpreadThreshold`
(0.30), `steadyHeld` against `steadyHeldThreshold` (10), each with how far
over its threshold it is (`kSpreadOverThreshold`, `steadyHeldOverThreshold`;
negative when the test was missed and the line is the pinned fit or a held
slope), and `fitRows`, the readings the line was fitted from.

For `learner` `start` the key is the variant's start key, and `value` the
published start estimate in seconds, with `previousValue`. `source` is
`measured`. The line also carries:

- `samples`: pods the estimate is from, and `needed` (3).
- `sizedSeconds`: the start the floor uses.
- `measuredSeconds`: the measured estimate alone.
- `prior` and `priorSource`: as in `replica-start-measured`.
- `restored`: `true` for an estimate restored from the snapshot that has
  measured no start since.

It logs when the estimate is set for the first time, when it moves more than
5 % from the value last logged, or after 10 minutes. There is no `pods` list.

### `replica-capacity-summary`

One line per variant per cycle: which replicas the saturation analyzer priced,
and what each was priced at. `replicas` lists them as
`<pod>=<capacity>/<bound>/<k2 source>`. `bound` is `k2` when the compute bound
is the smaller and `k1` otherwise (a tie is `k1`, as `replica-capacity-decision`
says). The k2 source is one of the values below. Then, when they apply:

- `/P1-obs-…`: the replica read saturated but the reading was not recorded as
  k2, and the label says why (`unsettled`, `downstream`, `unadmitted`,
  `rollout` or `invalid`; see the table below).
- `/mu:<why>`: a decode or aggregated (unlabeled) replica with no derived service rate. `why` is
  `no-line` (no ITL line fitted yet), `no-shape`, `k-price`, `no-seqs`,
  `no-itl` or `no-rate`; for an aggregated replica also `shape-unpriced` (the
  request shape is more than 20% heavier than its line's and the prefill cost
  is not learned), `line-shape-unknown` (a line from before the shape was
  recorded, or a held slope that is not its line), `shape-unknown` (no
  capacity or length to compute the shape) or `occupancy-unknown` (a prefill
  reading that is not a number).
- `/bridge`: a warm-pool Pod lent to the variant.

A variant with very many replicas is written over several lines, each with
`part` and `parts`, so no line passes the container runtime's 16 KiB line
limit; `priced` is the variant's whole count on every part.

```text
2026-10-14T09:12:03.510Z  INFO  replica-capacity-summary  {"modelID": "my-model",
  "namespace": "default", "variant": "primary", "priced": 2,
  "replicas": ["decode-0=1152000/k2/P1-obs", "decode-1=1152000/k2/P2-hist"]}
```

The full per-replica detail is at `--v=3`: `replica-capacity-decision` (k1, k2,
demand and queue terms), `k2-decision` (the k2 priority chain, with its donor
when borrowed) and `derived-mu` (every term of a decode or aggregated replica's
service rate, including the batch cap it was capped at, `batchCap`, and where
that came from, `batchCapSource`; see [the batch cap](scaling-policy.md#the-batch-cap-llm-daimax-num-seqs-and-enablebatchcaplearning)).
For an aggregated replica it also logs `reportOnly`, `shapeG` and
`lineShapeG` (the shape now and the line's), `priceCase`, `pricedA`,
`shapeTermC` and `shapeTermIdentified`, `prefillStepsPerToken`,
`prefillKnown` and `prefillRefused`, `occupancy`, `lifted`, `liftReason`,
`agreeCycles`, `gpsMedianErrPct` and `gpsMedianErrOK`; the values are listed
in [troubleshooting](troubleshooting.md).
They are written once per replica per cycle, about 1.9 KB a replica, so the
default leaves them out. See [Enabling the log](#enabling-the-log).

---

## Reason values (`reason` field)

Each analyzer sets `reason` to say how it computed a variant's per-replica
capacity. It is free text. The saturation V2 analyzer uses:

| Reason | Meaning |
|---|---|
| `P0-store` | from the **capacity store** (no live replicas) |
| `P1-obs` | k2 from **observed** tokens-in-use (queue saturated), blended into the same rolling average `P2-hist` reads, so one noisy cycle gets 1/N weight. The window (`historyKey`) is per output-length bucket of the **fleet's** output length, not the replica's own average, and the bucket is held: it changes only once the fleet's length, averaged over 10 minutes, is 3% past the boundary, or once the length has stayed 20% past it for 2 minutes. So a workload whose output length sits on a bucket boundary (500 tokens, say) keeps one window. When the bucket does move to one with no window, k2 is priced from the bucket left as `P2-prior` until the new bucket is measured, and `--v=3` logs `k2-key-bucket-moved` |
| `P2-hist` | k2 from the **historical** rolling average. The window is kept for 7 days from when it was last *read*, not written, so a bucket priced from it every cycle keeps its value however long the fleet goes without saturating. While kept it is used: with `min(k1, k2)`, it is the most conservative number available. After 7 days unread the bucket is forgotten and the next decision falls to `P3-k2` or `P4-k1` |
| `P2-borrow` | No k2 history of its own for this traffic shape, so k2 from a **sibling variant**: same namespace, model, accelerator, GPU count, role, output-length bucket and queue threshold, and capacity-compatible engine parameters (same `--max-num-seqs` and `--max-num-active-seqs`, batch-token budget, KV settings and quantization), and a measured KV cache total within 3% of this variant's when both have one. Used only when **lower** than the variant's own capacity without it (`P3-k2` or `P4-k1`, capped at k1), so it never orders fewer replicas. A k2 that is too low never corrects itself (light load never saturates, and only saturation measures k2), so the borrow holds 30 minutes, then relaxes linearly to the variant's own figure over 6 hours. The clock is per output-length bucket, and counts only cycles where a Ready replica of the variant's own serves that bucket with requests on it: it stops while the variant is parked, and a warm-pool bridge or a Pod that is not Ready reads the borrow without starting or moving the clock. A donor that leaves and returns does not restart it. Once relaxed, the borrow stays spent: it starts again only when a figure on offer is more than 20% below the one it last ran on, or the variant's engine arguments have changed (a new image alone is not a change). A donor that measures higher does not restart it. `disableK2Borrow` turns it off for the model and pauses its clock. The clock lasts as long as the variant; a controller restart keeps it only through the learned-state snapshot (`LEARNED_STATE_SNAPSHOT`). The `k2-decision` line names the `donor`, its `donorK2`, this variant's `ownCapacity` (its own k2 or k1, whichever is lower), and how far it has `relaxed`. The variant's own saturated readings replace it from its second admitted cycle (`P1-obs-unconfirmed` holds the first) |
| `P2-prior` | k2 from the variant's **own** earlier measurement, made under engine arguments it no longer runs (its flags changed in a rollout, while the controller ran or across a restart), or in the output-length bucket the k2 key just left (the bucket it moved to has no window yet). Not a borrow, and `disableK2Borrow` does not turn it off. The same two rules as `P2-borrow`: used only when **lower** than the variant's own capacity under the new arguments, and it relaxes to that capacity (30 minutes, then 6 hours, of serving). A new image alone does not make one. A later change of arguments restarts it only if the variant measured a newer figure first; one that measured nothing leaves its clock running. The `k2-decision` line names the variant itself as `donor`. The variant's saturated readings under the new arguments replace it from their second admitted cycle |
| `P3-k2` | k2 **derived** from deployment parameters (vLLM model args). The `k2-decision` line names the batch cap it used (`batchCap`, per engine rank) and where it came from (`batchCapSource`): `flags`, `declared` (the `llm-d.ai/max-num-seqs` annotation), `learned` (`enableBatchCapLearning`; a `batch-cap-learned` line at `--v=3` says when a pod raised it) or `default` (256, the flags unread). The `derived-mu` line names the cap mu was capped at the same way, and `none` when the scale target was never read; see [the batch cap](scaling-policy.md#the-batch-cap-llm-daimax-num-seqs-and-enablebatchcaplearning). Never for a **prefill** variant: the formula needs a real output length, and prefill's avgOutput (~0-1, it hands off before generating) reduces it to the batch-token budget |
| `P4-k1` | k2 unavailable; **fell back** to k1 (memory-bound capacity). Normal for prefill, not degraded; see `P3-k2` |

These appear in the `k2-decision` line's `priority` field but never as a
variant's `reason`, because no capacity comes from them:

| value | meaning |
| --- | --- |
| `P1-obs-invalid` | an observation above the KV cache's **physical** ceiling was discarded as a scrape artifact. The bound is the ceiling, not k1: k1 is the ceiling times `kvCacheThreshold` (0.80 by default), so occupancy between the two is kept. The analyzer falls through to the next priority |
| `P1-obs-downstream` | a **prefill** replica's saturated queue was not recorded because a **decode** replica was full and queued (resident KV at its k1, queue at the threshold) in the same cycle or the last minute (the collector's row window). Prefill finishes a request only when decode pulls its KV, so with decode saturated, prefill's KV, queue and completion rate measure decode, not prefill. Neither k2 nor the saturated throughput (`throughput-demand-floor`) is recorded, but with [`enablePrefillThroughputBound`](scaling-policy.md#enableprefillthroughputbound) on the throughput is kept as prefill's lower bound (`prefill-throughput-bound-reading`); the analyzer falls through, and prefill's demand is held for the cycle, neither ordering nor releasing (`prefill-demand-held`). A decode queue of only KV transfers in flight (KV under k1) does not count. Both roles saturate at once only when decode is short at prefill's completion rate; prefill's reading then waits for decode to recover |
| `P1-obs-rollout` | a saturated queue was not recorded because the variant is mid-rollout: its scale target reports a rollout in progress -- any rollout, image-only included; for a LeaderWorkerSet its `UpdateInProgress` condition, so a scale-up is not one -- or its replicas report more than one pod template revision. Old pods still report while the record already names the new configuration, so the reading may be an old pod's. Nothing is written to a window until the rollout completes. The variant is priced from what it already holds, its own window, or after a change of arguments its previous figure (`P2-prior`); where a saturated reading is lower than that, a second `P1-obs-unadmitted` line (`chainK2`, `chainPriority`, reason ending `mid-rollout`) says the row is priced at the reading for this cycle only, so a bucket with no figure is not priced at k1 while it shows full. Nothing of that reading is kept. The lower bound waits for the rollout: its hold and relaxation run only on cycles that may record, so a rollout that stalls -- a surge pod pending, a LeaderWorkerSet held part way by a rolling-update partition -- does not relax the variant toward k1 while nothing can be measured, and a previous figure keeps its whole hold for when it ends. A LeaderWorkerSet held at a partition reads as mid-rollout for as long as it is held, so nothing is learned for the variant until it is let through. An undo, or a controller restart, in mid-rollout is a rollout like any other |
| `P1-obs-unadmitted` | a saturated queue on a **warm-pool bridge** or a pod that is **not Ready** was not recorded as the variant's k2. A bridge runs the pool's engine configuration (a lower `--gpu-memory-utilization`, a different batch ceiling); a pod that is not Ready gets no traffic, so its queue and resident KV are left over or frozen. These are the rows the saturated throughput already skips. A k2 recorded from them would be too low, and a k2 that is too low never corrects itself, because it orders replicas and nothing saturates again. The analyzer falls through to the next priority. For a **bridge** (and for a saturated Ready replica mid-rollout, see `P1-obs-rollout`) whose reading is lower than what that gives, a second `P1-obs-unadmitted` line (`chainK2`, `chainPriority`) says the row is priced at the reading for this cycle only, and the variant's reason can read `P1-obs-unadmitted`: a variant only a saturated bridge is carrying is priced at what the bridge holds, not at k1. A pod that is not Ready is priced at what the chain gives, mid-rollout too: its reading may be a pod draining or a hang with frozen gauges, and would be re-read every cycle. Nothing is recorded either way, and the borrow clock is not started. A pod that fails its readiness probe under load does not record k2 either; if `P1-obs-unadmitted` names a pod of the variant's own during a load peak, its readiness probe is timing out. When the controller cannot list a namespace's Pods it treats every Pod as Ready, so these readings are recorded; `llmsm_metrics_collection_errors_total{query_type="pod_list"}` counts those cycles |
| `P1-obs-unconfirmed` | a saturated reading passed every test above, but it would be the first in its window: a traffic shape with no k2 on record (a cold start, a new output-length bucket, a controller restart without the learned-state snapshot, or a window whose last reading is older than a day). One reading alone would be the whole window, and a second moves it by half their difference, so the reading is held, not written. The next cycle's admitted reading under the same key writes both: averaged, not checked against each other. A sibling's reading in the same cycle does not count, because a load step lands on every replica at once, and a reading held under engine arguments the variant no longer runs is dropped. The cycle that holds it is priced at the lower of the reading and the next priority (a second `P1-obs-unadmitted` line, reason ending `held as its window's first reading`), so a bucket with no figure, or a window from before the gap, does not price the replica higher while it shows full. The reading that follows is judged against the held readings as the k2 on record. The saturated throughput (`throughput-demand-floor`) is judged as on any `P1-obs` cycle |
| `P1-obs-unsettled` | a saturated queue was not recorded because it looks like a **load step**, not a limit: the busiest rank's waiting requests, at the request's footprint, add up to more than everything it holds, it is not both completing at 70% of the rate on record for the traffic shape (its measured saturated throughput, or the derived one) and holding 70% of the k2 on record, and it has not been queued for a whole minute with resident KV within 25% of a minute ago, up or down (a fall under a deep queue is traffic moved off the replica, not a limit). A replica handed a step reads a deep queue on a nearly empty cache for a minute (the queue is a one-minute peak), and one whose batch is capped by `max_num_seqs` keeps filling for minutes on long outputs; recorded, either reading would stay as k2 for as long as the window is used. Nothing is written, the saturated throughput included; the analyzer falls through to the next priority. A genuine compute bound at low occupancy keeps its queue with flat residency and is recorded a minute after it first shows; with a noisy KV gauge, later |
| `no-data` | no ready replicas, no stored record, no compatible variant: capacity is 0 this cycle (normal for a new variant) |
| `error` | K2 priority not in the known set: an unlabelled code path. Should not occur |

`P1-obs` to `P4-k1` describe the variant's representative replica: the one whose
effective capacity is the lower median of all ready replicas (the one that set
`prc`). `P1-obs` means live data and a high-confidence estimate; `P4-k1` means
no replica had a compute-bound signal, and the estimate is conservative.

The throughput analyzer uses:

| Reason | Meaning |
|---|---|
| `T1-ols` | capacity from Tier-1 OLS fit (observation window ready) |
| `T2-pinned` | capacity from Tier-2 constrained OLS with a prior fitted B |
| `T2-default` | capacity from Tier-2 constrained OLS with the default baseline B (cold start) |
| `T2-held` | the last fitted slope, held because the window is too short to fit a new one: the fleet sits below the band or above it, or its replicas are still settling. B is the variant's measured intercept, or the default. The hold ends when the slope is older than `LEARNED_STATE_BUCKET_RETENTION` (7 days by default), or when the variant's engine arguments change. A shape change drops it |
| `T2-above-band` | no line from the band and no slope to hold, so the line is fitted from settled readings above the band at k of at least 0.85 (10 readings, 3 settled, as in the band), with B at the least ITL among them. A replica at its sequence cap is not among them: its readings are the band's (see `atCap` below). This is a variant that never fitted, under load that keeps every replica above the band: a controller restart into saturation, or a variant that starts there. It errs one way only: it prices a higher ITL than the true line, so a lower per-replica capacity and more replicas. On readings of the true line without noise it never under-provisions; with 2 % gauge noise it was at most 0.5 % under in simulation, within the 5 % known-truth bound. Its slope is never held and its B never stored; the first fit in the band replaces it. It does not wait out a controller restart or leader change, since readings taken during warm-up can only raise its price: measured with 4 replicas all above the band, 15 s cycles and `steadyFor` 2 min, it came back about 45 seconds after a simulated restart. A pod that became Ready less than `steadyFor` before the restart still waits |
| `T2-failed` | no line this cycle; the variant is skipped. The causes: fewer than 10 readings in the window, or fewer than 3 settled; every reading above the band at k under 0.85 (nothing the fallback may use); a shape change, which empties the window and drops the held slope; a controller restart or leader change, which starts with no line and waits the settle (see `restart` below); or no healthy row this cycle (stale metrics, no ITL, no KV capacity, no request shape). The `throughput analyzer: ITL line` line below names which |

When a variant's line goes silent (`T2-failed`) or comes back, the throughput
analyzer logs one line at the default level; also when readings at a
replica's sequence cap start or stop, or the cap's source changes (`atCap`,
`batchCapSource` below). It logs only on the change, not every cycle:

```text
INFO  throughput analyzer: ITL line  {"namespace": "default", "modelID": "my-model", "variant": "decode",
  "silent": true, "cause": "too-few-readings", "source": "T2-failed", "inBand": 3, "aboveBand": 0,
  "belowBand": 0, "settling": 0, "held": 6, "aboveBandHeld": 0, "atCap": 0, "batchCap": 0,
  "batchCapSource": "none", "pinnedAtLeastITL": false}
```

| Field | Meaning |
|---|---|
| `silent` | `true` when the line went silent, `false` when it came back |
| `cause` | why the line is silent; empty when it came back. See the next table |
| `source` | the `reason` of this cycle's line: `T2-failed` when silent, else the tier it came back on |
| `inBand` | this cycle's readings inside the band |
| `aboveBand` | this cycle's rows above the band and not at their sequence cap (not counted in the band) |
| `belowBand` | this cycle's rows below the band: replicas too lightly loaded to teach the line |
| `settling` | this cycle's readings from replicas still settling (just joined, or load just came back) |
| `held` | readings the band's window holds. The line needs 10, with 3 settled |
| `aboveBandHeld` | readings the above-band window holds, at k of at least 0.85. The fallback needs 10, with 3 settled |
| `atCap` | this cycle's readings above the band taken into the band's window because the replica is at its sequence cap (below). The line is logged once too, even while it stays live, when `atCap` goes above 0 or back to 0, or `batchCapSource` changes. A replica whose batch moves on and off its cap from cycle to cycle logs once per flip: expect a line each such cycle while it does |
| `batchCap`, `batchCapSource` | the sequence cap per engine rank the line reads for the variant, and where it comes from: `flags` (`--max-num-seqs`, lowered by `--max-num-active-seqs`), `declared` (the `llm-d.ai/max-num-seqs` annotation), or `none` (`batchCap` 0: no replica above the band teaches the line) |
| `pinnedAtLeastITL` | `true` when this cycle's line was priced one-sided at the cap: the variant has no learned intercept B yet, and the line through the least ITL of its readings at k 0.85 or more priced higher than the line through the default B (below) |

**A replica at its sequence cap teaches the line from above the band.** Its
batch is exactly the stated cap on every engine rank, requests are waiting,
and its preemption rate was read and is at most
`analyzerTuning.itlBand.capMaxPreemptionRate` (0 by default). Such a replica
cannot take another sequence however much cache is free, so its fill above
the band is its sequences' contexts, not memory pressure, and its reading is
a point on the line. The throughput line takes it into the band's window, as
the saturation and slo lines do, and counts it in `atCap`. It needs the cap
stated (`--max-num-seqs` on the engine, or the `llm-d.ai/max-num-seqs`
annotation); a cap the engine picks by default is not read. A variant whose
only loaded replica is at its cap is then priced from its own line
(`T1-ols`, `T2-pinned` or `T2-default`) once the window holds 10 readings,
not from the above-band fallback (`T2-above-band`). Until the variant has
learned its line's intercept B from a full fit in the band, that price is
**one-sided**: without noise, the line is never priced under the true line
at k 0.85. The
readings at the cap lie above 0.85, and a line through the default B (6 ms)
would price under the truth whenever the true B is higher, so the line is
also drawn through the least ITL those readings show, and the higher of the
two at 0.85 is the price. Without noise it errs towards more replicas,
never fewer: in simulation, by 2 to 11 % on a true B of 10 to 30 ms. With
2 % gauge noise it was at most 0.5 % under in simulation, within the 5 %
known-truth bound: on a shallow line with the cap near 0.85 its margin is
under a millisecond, the size of the noise. The `pinnedAtLeastITL` field
says when the line was priced this way. Once B is learned, the
line through it prices as usual. Which way the fleet moves: where the
fallback priced such a variant, the price is about the same (both are drawn
through the least ITL), so the fleet orders as many replicas, and the line
now holds its slope through a dip below the band; where the line was silent
(a capped replica between k 0.80 and 0.85, or cycles before the fallback had
its 10 readings), the analyzer is **live again**: it orders a scale-up when
it needs one, and votes on scale-down. A capped replica that
preempts more than `capMaxPreemptionRate` is not read at its cap: it stays in
`aboveBand`, and only the fallback may use it.

The saturation and slo lines price a variant at its cap one-sided too, until
B is learned, against an upper prior on B rather than the least ITL. With no
learned B, the line through the default 6 ms prices under the truth: at the
saturation line's pricing point (k 0.68 with the default thresholds) by 3 to
20 % on a true B of 10 to 30 ms, which made the derived mu too high and
ordered too few replicas. Each line now also draws a line with its
intercept the prior, through the mean of its readings at the cap that sit at
or past the fill it is priced at (the three of highest fill when fewer
do), and prices on the higher of the two where the line is priced. A line
priced this way is not lent to other variants of the same engine: they
borrow nothing from it while it is.

The prior is 30 ms, raised to the largest B another variant has learned on
the same physics: the same model, accelerator and GPU count, under the same
engine arguments, in the same role. A learned B only ever raises the prior.
A variant on other hardware, or another engine, never lowers it, so a fleet
of small intercepts cannot pull a whole-node decode under its B. The prior
is then capped at the least ITL the variant's own window shows, which no
reading can put under the true B, so an outlier fit cannot inflate it.

The rule: on readings without noise, at a fill k at or under the mean fill
`kbar` of the readings the line is drawn through, the price is over the true
line by `(b - B)(1 - k/kbar)`, `b` the capped prior. So it is not under the
truth while the true B is at most the prior, and over it by at most the
prior's error: on the grid, 0 to 14 % at the 30 ms prior. A true B above
the prior is under-priced by `(B - b)(1 - k/kbar)`: 6 % at a true B of 40 ms,
against 22 % before. Past `kbar` the error turns: the rule draws through
readings at or past the priced fill for that reason, and where every
reading is under it, `itl-fit` logs `priorBeyondReadings` true (`slo-variant`:
`linePriorBeyondReadings`) and the bound does not hold there. With 2 %
gauge noise, where the true B equals the prior, the price was at most 1.1 %
under in simulation, 0.07 % on average.

The slo line prices the fill where its ITL meets the TPOT target, and a
line under the truth met it at too high a fill. It now meets it at or under
the truth's fill, without noise, for a true B at most the prior. It abstains
(`infeasible`) only where the target is under every ITL the variant has
read, so nothing it measured shows B is under the target; one reading under
the target is enough to price it. `itl-fit` logs `pinnedAtBPrior`, and
`slo-variant` logs `linePinnedAtBPrior`, when the price came from the
prior's line.

| `cause` | Meaning |
|---|---|
| `no-healthy-rows` | no row of the variant was healthy this cycle, so nothing was read. Every count is 0 |
| `fit-declined` | the window had enough readings, but they gave no usable slope (flat or falling) |
| `shape-change` | the request shape changed. The window was emptied and the held slope dropped; the line waits for 10 readings at the new shape |
| `rollout` | the variant is rolling out: its status says so, or its replicas report two revisions. The line takes only the newest revision's rows, and none while no new pod serves. It is silent when a new pod has brought new engine arguments and not yet taught 10 readings, or when no new pod serves and the hold has ended (`BucketRetention`, 7 days by default). `rolloutHeld` says the line is held on the old pods. While it is silent the analyzer gives no figure: another listed analyzer decides, and listed alone the role is held |
| `all-above-band-below-ksat` | every row was above the band at k under 0.85. Neither the band nor the fallback may use them |
| `restart` | the line has not been live since the controller started (a restart or a leader change), or since the variant appeared. Every replica waits its settle (`steadyFor`, 2 minutes by default) before the band may fit, then the window needs 10 readings, 3 of them settled. Measured in simulation at 15 s cycles with `steadyFor` 2 min: 2 to 2.5 minutes with 3 or 4 replicas in the band, 2.5 minutes with one. It grows with `steadyFor`, and with fewer replicas once 10 readings take longer than `steadyFor` to arrive. With 4 replicas all above the band, the above-band line does not wait, and this lasted about 45 seconds. If the first cycle after the restart has no healthy row, the line logs `no-healthy-rows` instead: the line is logged only when it goes silent or comes back, so `restart` does not appear for that silence. A rollout is not a restart: if it goes silent while it runs the line shows `rollout`, and after it, its new pods being joiners, `too-few-readings` with a high `settling` count |
| `too-few-readings` | fewer than 10 readings, or fewer than 3 settled, and no slope to hold. Read `held`, `belowBand` and `settling` to see why |

Other analyzers may set their own values or leave `reason` empty.

The **composite row's** `reason` values are a different vocabulary — the
decision path that produced that variant's aggregated replica count, not any
analyzer's own capacity provenance:

| Reason | Meaning |
|---|---|
| `C0-agree` | more than one analyzer produced a usable signal for this variant; the aggregate is the max across them |
| `C1-single` | exactly one analyzer produced a usable signal for this variant |
| `C4-no-signal` | no listed analyzer had a usable signal for this variant. With saturation listed, the variant is sized on saturation's measured capacity as before; with saturation unlisted, its role holds where it is |

`C4-no-signal` on every variant reports the model as `no-composite-signal`.
Its replicas are still charged to a GPU quota, since they hold their GPUs
either way.

A role the composite holds logs one line a cycle:

```text
INFO  composite holds a role  {"modelID": "my-model", "namespace": "default", "role": "decode",
  "noScaleUp": false, "noScaleDown": true, "reason": ["analyzer-in-grace:throughput"]}
```

`reason` lists every rule that holds the role:

| `reason` | Holds | Meaning |
|---|---|---|
| `no-listed-analyzer-sizes-role` | up and down | saturation is not listed, and no listed analyzer can size the role |
| `other-role-cannot-grow` | up | P/D only: the other role is held for `no-listed-analyzer-sizes-role`, so this one does not grow either |
| `analyzer-in-grace:<name>` | down | the listed analyzer `<name>` is inside its staleness grace (up to three cycles) with no reading, and the role waits for it before shrinking |
| `saturation-holds-role` | down | saturation holds the role, and another analyzer's demand replaced saturation's held figure; scale-down needs every analyzer to agree, so the role does not shrink |

---

## Grep patterns

```bash
LOGS="kubectl logs -n $NS -l app.kubernetes.io/name=scaling-manager --tail=-1"

# All analyzer results for a specific model
$LOGS | grep analyzer-result | grep '"modelID": "my-model"'

# Saturation analyzer only
$LOGS | grep analyzer-result | grep '"analyzer": "saturation"'

# Scale-up decisions only
$LOGS | grep scaling-decision | grep '"action":"scale-up"'

# The composite signal only (what the optimizer actually consumed)
$LOGS | grep analyzer-result | grep '"analyzer": "CompositeSignal"'

# Full cycle for one model (both lines)
$LOGS | grep -E 'analyzer-result|scaling-decision' | grep '"modelID": "my-model"'
```

`modelID` is unique only within a namespace. Add `| grep '"namespace": "<ns>"'`
when two namespaces serve the same model.

---

## Ordering and timing

In one reconcile cycle, for one model:

1. One `analyzer-result` line per enabled analyzer (saturation first, then the
   others in registration order).
2. One more `analyzer-result` line for the composite
   (`"analyzer": "CompositeSignal"`). It is built from all of them, so it is
   always last.
3. One `scaling-decision` line, after the optimizer has processed every model in
   the cycle.

The two are not adjacent in the log: other models' lines can fall between them.
Filter by `modelID` and `namespace` to match them up.

---

## Enabling the log

Both lines are INFO and appear in the default controller log. Nothing to enable.

The per-replica lines (`replica-capacity-decision`, `k2-decision`,
`derived-mu`) need `--v=3`. So does `hack/benchmark/dump_k2_decisions.py`,
which reads them; `make benchmark-run` installs at that level. At `--v=3` a
cycle writes about 1.9 KB more per replica. Install with it:

```bash
make deploy-scaling-manager LLMSM_VERBOSITY=3
```

or add it to a running controller (this restarts it):

```bash
kubectl -n $NS patch deploy scaling-manager-controller-manager --type json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--v=3"}]'
```

A restart starts the analyzer's in-memory learning over unless the learned-state
snapshot is on (`LEARNED_STATE_SNAPSHOT`). The default level's
`replica-capacity-summary` already says when a replica's k2 reading was not
recorded and why a decode replica has no derived rate, so check it first.
