# Learned-state sharing by similarity, not by identity

**Status:** proposed; step 3 of [Order of work](#order-of-work) is built
(per-variant k2 and throughput windows, and a compatible sibling's k2
borrowed lower-only with an expiry), the rest is not. Supersedes part of the sharing key on
`feat/engine-config-identity` (PR ev-shindin/llm-scaling-manager#126), and proposes a storage model for the
k2 and saturated-throughput windows that main does not have; see
[What this changes](#what-this-changes).

A variant with no learned state of its own cannot price its first decisions from
a measurement. It can price them from a *formula*, or it can start from what
another variant already measured. The second is better when the two variants
really are running the same engine, and worse when they are not.

This proposal answers two questions:

1. **What "the same engine" has to mean** before one variant may start from
   another's measurement.
2. **Where learned state is stored**, so that the first answer can be applied at
   all.

## The design in one paragraph

Every variant writes only its **own** measurements, filed under its own
identity and the engine configuration they were taken under. A variant that has
no usable measurement of its own **reads** other variants' records and picks the
most compatible one. Similarity is applied on the read, never on the write. A
borrowed figure may make a variant more conservative and may never let it claim
more capacity than its own estimate.

## Three quantities, two storage models today

The analyzer learns three things per replica shape. On main they are not stored
the same way.

| quantity | key on main | scope |
| --- | --- | --- |
| ITL line (baseline, replica start) | `itlWindowKey`: `namespace\|model\|variant\|accelerator\|gpus` | **per variant** |
| k2 (saturated occupancy) | `historyKey`: `model\|accelerator\|gpus\|role\|outBucket\|qN` | **pooled** across namespaces, variants and engine configurations |
| saturated throughput | `throughputKey`: `model\|accelerator\|gpus\|role\|inBucket\|outBucket\|qN` | **pooled**, the same way |

(`internal/engines/analyzers/saturation/analyzer.go` and `keys.go`, before the
change that ships with this proposal; that change makes the two pooled rows
per variant.)

The pooled windows are not a cap. `P2-hist` returns the pooled window's average
as **this variant's k2** (`replica_capacity.go`, the `K2SrcHistorical` branch),
and every variant's own `P1-obs` readings are averaged into that same window.
So a variant running `--max-num-seqs 512` and one running `128` on the same
model, accelerator and GPU count share one k2. The higher one makes the other
claim more capacity per replica and order fewer replicas. That is the direction
that breaks TTFT and does not recover: a higher k2 holds the fleet small, and a
small fleet does not saturate, so it never produces the reading that would
correct it.

`nearestSaturatedThroughput` extends the pool once more: an empty output bucket
borrows the nearest neighbour bucket's median, again with no variant in the key.

PR ev-shindin/llm-scaling-manager#126 added a fingerprint-keyed ITL line and an `enableLearnedStateReuse`
switch that gates it, and the cross-variant capacity figure a zero-replica
variant takes (`lookupCompatibleCapacity`). The switch does not reach the k2 or
throughput windows, because their keys never had a variant to scope by.

## The problem with proving identity

PR ev-shindin/llm-scaling-manager#126 answers "the same engine" with a digest. `EngineParams.Fingerprint()`
hashes the launch flags that drive latency; `engineFingerprint` refuses to
produce a key at all unless `Complete()` is true, and `itlPhysicsKey` falls back
to the unshared per-variant key when there is no fingerprint. A borrowed line is
therefore an assertion: *these two variants are running the same engine.*

That assertion has to be exactly right, because a false equality licenses a
variant to price itself from a measurement taken on a different engine. And it
cannot be made exactly right. Five adversarial review rounds found seven ways
to produce a false equality or to destroy a true one, every one of them
measured as a digest collision rather than argued:

| break | effect |
| --- | --- |
| a boolean flag swallows the next token, and an unresolvable `$VAR` there is dropped unrecorded | `--enforce-eager $EXTRA_ARGS` hashes as no flag at all |
| single-dash aliases are not read | `-q fp8` hashes as unquantized: an FP8 and a BF16 serving on one line |
| a valueless flag swallows a following alias | `--enforce-eager -q fp8` hashes as `--enforce-eager` alone |
| a glued single-character alias | `-qfp8`, `-O3`, `-O.level=3` hash as no flag at all |
| long flags neither parser maps | `--no-enable-prefix-caching`, which two of this repo's own benchmark scenarios set, hashes as prefix caching ON |
| the engine image is not hashed | `vllm-openai:v0.8.5` and `:v0.11.0` with identical flags collide, and a canary rollout is the likeliest way to get two same-flag variants, where the new cold one is the borrower |
| `EngineParams` is frozen on the first live record | the digest describes a configuration that is no longer running |

Two attempts to close the first four made things worse rather than better. The
second attempt denied sharing to ordinary deployments (`grep -q`,
`pip install -q`, `curl -O` and `python3 -O` in a shell preamble each cost a
fleet its key, because `splitShellString` flattens the whole `/bin/sh -c`
payload and a preamble's flags are indistinguishable from the engine's) while
`-qfp8` kept colliding. Both attempts were reverted.

The pattern is the finding. **A parser cannot soundly answer "did I read
everything" from a flattened shell command line**, and an identity claim needs
exactly that answer. Hashing the image plus the verbatim argument list would be
sound, but it fails the other way: two variants differing only in
`--served-model-name` would never share, which is the denial problem again,
arrived at on purpose.

## The reframe

A borrowed figure does not have to be an identity claim. It can be a **prior**.

Under that reading a slightly-wrong match is not a correctness failure, it is a
worse starting point, and the failure mode changes from *false equality*, which
is unbounded, to *imperfect ranking*, which degrades gracefully.

It is also what the codebase already does for the neighbouring quantity.
`EngineParams.IsCapacityCompatible` compares twelve latency-relevant fields and
**ignores every other field**: it is a similarity predicate, not a byte
identity. `Store.FindCompatible` deliberately does not gate on `Complete()`;
`internal/signals/capacity/store.go` argues that at length, because gating it
once cost a 30x capacity over-estimate. So graded matching is already in the
tree, already reviewed, and already defended for capacity records. This extends
it to the three learned quantities.

## Storage: per variant, read by similarity

A prior can only be ranked if the reading still says where it came from. A
pooled window cannot: once two variants' k2 readings are averaged into one
`historyKey`, nothing can separate them again, so no tier, no distance and no
switch can be applied to them afterwards. Ranking needs the provenance kept, so
storage has to be per variant.

**Write.** Each variant files its own readings, and only its own, under

```
namespace | model | variant | configDigest | accelerator | gpus | role | buckets | qN
```

The ITL window already has the first part of this shape. The change is to give
k2 and saturated throughput the same per-variant scope, and to put the
configuration digest into all three, so a configuration change starts a fresh
window rather than appending to one recorded under the old flags.

**Read.** A variant asks for its own window first. If that has no usable reading
it searches the other variants' records and takes the best tier available
(next section). The search runs over the controller's internal store, never
over Prometheus series: a series carries no writer identity and no honest age,
so it cannot be ranked by distance or freshness (the finding recorded for
learned state across a restart, PR ev-shindin/llm-scaling-manager#116).

**What it fixes on its own.** Keying by configuration digest closes two of the
three breaks that are about the digest describing the wrong thing: the ITL
window not being re-keyed on a configuration change, and `EngineParams` frozen
on the first live record. The unhashed engine image still needs its own change.
It also gives `enableLearnedStateReuse` one meaning for all three quantities:
off means no sibling's record becomes a variant's answer. A sibling's figure
that can only lower a variant's capacity -- the k2 borrow below -- is used
either way.

## Tiers

For each quantity, pick the best available source, preferring in order:

| tier | source | predicate |
| --- | --- | --- |
| 0 | the variant's own window | it has a usable reading |
| 1 | another variant with the **same resolved flags** | identical `Fingerprint()`, both `Complete()` |
| 2 | another variant **compatible on what matters** | same model, accelerator, GPU count and role, and `IsCapacityCompatible` |
| 3 | no record | fall back to the derived figure, as today |

The shape part of the key (role, input and output bucket, queue threshold)
must match at every tier. The neighbour-bucket borrow in
`nearestSaturatedThroughput` becomes a distance like any other: it searches the
variant's own other buckets before another variant's same bucket.

Tier 2 is where the unreadable-configuration cases land. An unresolved flag
stops being a **veto** and becomes a **distance penalty**: a variant whose
`--block-size` could not be read is not refused a prior, it is matched on the
eleven fields that were read and marked as a weaker match. This is why the
seven breaks stop being breaks: every one of them was "the gate was asked a
question it could not answer". Remove the gate and the question disappears.

Tier 1 keeps the digest, so `Fingerprint()` and `llmsm_engine_config` stay as ev-shindin/llm-scaling-manager#126
builds them. What changes is that an absent or incomplete fingerprint demotes a
variant to tier 2 instead of dropping it to tier 3.

## Rules a borrowed reading obeys

1. **It may only make a variant more conservative, at every tier.** For k2 and
   saturated throughput that means a borrowed figure is used only when it is
   *lower* than what the variant would otherwise get from its own derivation;
   a donor can never raise a variant's capacity. Too high costs TTFT: the
   fleet is held small, saturates, and P1-obs measures the true k2 within a
   few cycles, but the tail latency of that episode does not recover. Too
   low costs replicas and does **not** correct itself -- see rule 2. For the ITL line, ev-shindin/llm-scaling-manager#126
   already enforces the equivalent ("cap, never order"): `borrowedReading`
   routes the reading so `mayOrder` is never set, `thinOwnWindow` excludes it
   from the queue release, and `fleetHasMeasuredItself` refuses it. This rule
   does **not** relax for a tier-1 match. It is what makes a wrong prior
   survivable.
2. **A prior gives way: to own data at once, and to the variant's own figure
   over time even without it.** Own data displaces a prior, and sooner at a
   greater distance: a tier-2 prior by fewer own observations than a tier-1
   prior. But own data may never arrive, and for k2 and throughput rule 1
   guarantees that is the case it is borrowed in. Both are measured only at
   saturation (P1-obs needs the queue over its threshold), and a figure that is
   too low orders extra replicas, so each runs light, nothing saturates, and
   nothing is measured; the low capacity also blocks the scale-down that would
   let a replica saturate. A donor's figure can be right for the donor and too
   low for the borrower, and without an expiry it would stand for as long as
   the donor exists. So a borrowed figure holds for a bounded time and then
   **relaxes slowly to the variant's own figure** (P3 derived, or P4 k1). As
   it rises the fleet thins a replica at a time until one saturates and the
   variant measures itself. The relaxation never goes above the variant's own
   figure, so rule 1 still holds throughout: its worst case is the variant's
   no-borrow behaviour, reached hours later instead of at once. This is the
   "relaxed upward only slowly" policy of
   `docs/plans/analyzers/k2-capacity-model.md`, applied to a borrowed figure
   rather than an own one.
3. **Reading a donor does not extend the donor's life.** Today
   `computeCapacityHistory` and `nearestSaturatedThroughput` `Touch()` the
   window they read, so retention is "last used". Under per-variant storage a
   borrower touching a donor would keep a dead variant's record alive for as
   long as anything borrows it, and the sweep would never reclaim it. A
   variant's own read touches its own window; a borrow reads without touching.
   This is the same rule `StableAcceleratorFor` follows on ev-shindin/llm-scaling-manager#126.
4. **A deleted variant's records stay readable until they expire.** A canary
   that replaces a variant is the best case for borrowing, and its donor is the
   variant being replaced. The existing `VariantTimeout` horizon (24h) covers
   it if records are not dropped when the variant disappears.
5. **Several tier-1 donors are pooled on the read.** Splitting storage per
   variant means fewer samples per window, and a window that cannot reach
   `DefaultMinKSpread` never produces a fit. Combining the samples of every
   tier-1 donor at read time recovers the pooling main has today, but only
   among variants that actually match. Tier-2 donors are not pooled; the best
   one is taken.

## The risk this proposal does not remove

**"Learn on the way" may never complete.** A fit needs k-spread of at least
`DefaultMinKSpread` (0.30), and this project has measured the autoscaler
defeating its own learning: unpinned, per-replica KV occupancy fell to a median
of 0.049 against the 0.15 admissibility floor, so 13% of readings were
admissible and the window stalled at 4 of 10 samples. A cold variant can borrow
indefinitely and never graduate.

Two consequences, both deliberate:

- The tier is **permanent state, not a transient**. It must be visible for as
  long as it applies: in the `k2-decision` and `derived-mu` lines, and in
  `itl-line-borrowed` from ev-shindin/llm-scaling-manager#126, with the donor named, and as a `reason` value so it is
  reachable from Prometheus rather than only from logs.
- Rule 1 cannot be relaxed on the grounds that a prior has "settled". It has
  not settled; it has persisted.

**And the knowledge problem does not vanish**, it softens. Ranking similarity
still needs to know which fields drive latency. The difference is that getting
the ranking slightly wrong now costs accuracy, where getting an identity claim
slightly wrong cost correctness. `IsCapacityCompatible`'s twelve fields are an
already-reviewed answer to the same question.

## What this changes

**On main**, this changes how k2 and saturated throughput are stored and read:

- `historyKey` and `throughputKey` gain namespace, variant and configuration
  digest.
- `P2-hist` reads the variant's own window. With none, a compatible sibling's
  lower figure is borrowed as `P2-borrow` under rules 1 and 2 (built; see
  step 3 below).
- `nearestSaturatedThroughput` searches by distance as above, and stops
  touching what it borrows.
- No migration. These windows live in the controller's memory and an upgrade
  restarts it, so they start empty whatever the key; the new key costs nothing
  a restart does not already cost. What changes after that is how fast a
  window fills: one variant's replicas feed it, not every variant's.

**On PR ev-shindin/llm-scaling-manager#126**, this supersedes the key and keeps everything else.

Superseded:

- `engineFingerprint`'s `if !rec.EngineParams.Complete() { return "" }`, which
  becomes a tier-2 demotion rather than a refusal.
- `itlPhysicsKey`'s fall-through to the per-variant key on an empty
  fingerprint, which becomes a tier-2 lookup before giving up.

Kept, unchanged, and sound as reviewed:

- `Fingerprint()`, `FingerprintVersion`, the field-classification drift test,
  and `llmsm_engine_config`; tier 1 needs all of it.
- The sharing plumbing: `itlLines`, `publishLine`, `borrowLine`, the sweep on
  `bucketRetention`, and the narrowing.
- The floor's cap-not-order routing, `mayOrder`/`borrowedOnly`, and the
  `heldWhy` composition.
- `enableLearnedStateReuse`, which ships **off**. Under this proposal it gates
  tiers 1 and 2 for all three quantities wherever a sibling's figure becomes
  the answer. The k2 borrow (tier 2, lower-only) is the exception, below.

The engine image is the one break this proposal does not address. It wants its
own change: hash the image into the digest, or make an image change a tier-2
distance.

## One lower bound per key, and windows keyed by their arguments

Built on ev-shindin/llm-scaling-manager#151, ev-shindin/llm-scaling-manager#163 and ev-shindin/llm-scaling-manager#152 together with the borrow controls first proposed
in ev-shindin/llm-scaling-manager#165, after a review found that their mechanisms disagreed once combined. Three of
them each meant "a figure from somewhere else may only make this variant more
conservative, for a while": the sibling k2 borrow (ev-shindin/llm-scaling-manager#132, ev-shindin/llm-scaling-manager#165), the restored
prior (ev-shindin/llm-scaling-manager#151), and the ceiling a parked variant keeps after a rollout (ev-shindin/llm-scaling-manager#151).
Each had its own clock and its own expiry. Forgetting a variant, turning the
borrow off, trimming the snapshot and the image check each knew some of them
and not the others, and each gap was a bug: an image-only rollout re-armed a
spent borrow, a prior was swept during a park, a forget left an unused prior
behind.

**One record per bucket.** Every k2 history key (variant, bucket, role,
hardware) has at most one lower bound. It names where its figure comes from:
a sibling variant, or the variant itself under engine arguments it no longer
runs. It holds the figure, a clock that counts serving time only, and what it
last ran on. There is one rule for all of them:

- it is used only below what the variant would get without it;
- it holds for 30 minutes of serving time, then relaxes to the variant's own
  figure over 6 hours of serving time;
- once spent, it stays spent until there is new evidence: the variant's
  engine arguments changed, or a figure is now more than 20% lower than the
  lowest one on offer while the bound ran. A donor that measured higher, a
  donor that went and came back, a figure that was on offer all along, an
  image-only rollout, and a fingerprint from another `FingerprintVersion` are
  not new evidence. Neither is a configuration that was not read completely:
  a bound armed under one records no arguments;
- a change of arguments restarts it, even mid-run: the new arguments get a
  full hold, not what was left of a bound armed on another engine hours ago;
- a replica that may not arm it (a warm-pool bridge, a Pod that is not Ready)
  reads it as it stood at the last serving cycle, or as the fresh bound it is
  about to be when there is new evidence;
- a sibling's figure is a candidate only from a window written under the
  arguments that sibling runs now, and only when the sibling's measured KV
  total is within 3% of the replica's own live one;
- it goes when its variant goes.

`disableK2Borrow` turns off sibling figures only. The variant's own figure
from its previous arguments is not a borrow, and turning the borrow off does
not drop a variant to k1. Forgetting a variant drops its own previous figure
and ends any running borrow as spent.

**Windows carry their arguments.** Each k2 and throughput window records the
engine arguments (`ArgsFingerprint`, the configuration without the image) it
was written under. The key does not change, so this costs no cold start. When
a variant's arguments change, the next read or write of a window written
under the old ones puts the window aside under them, for a bucket retention,
and starts a fresh one. Its k2 becomes the bucket's lower bound as the
variant's own previous figure, in place of any older one (below).
Its throughput is not read: a service rate is never borrowed, from a sibling
or from the past.

A rollback is not a new engine. A variant returned to arguments it ran before
gets the windows put aside under them back as its own, k2 and service rate,
with full authority; relaxing its own measurement away, or pricing it from
the figure of the arguments it was rolled back from, was what a rollback
cost before.

A configuration that was not read completely is still a configuration: its
windows are tagged with its fingerprint marked incomplete, so moving to or
from it is a change. Only two equal incomplete fingerprints are taken as the
same engine. A window with no recorded arguments, from before this change, is
taken as written under the arguments the variant runs now.

While the variant is mid-rollout, nothing is written to a window: a
saturated pod may be an old one, and the record already names the new
arguments. Mid-rollout is what the scale target reports this cycle -- a
Deployment's updated replicas or unobserved generation, a LeaderWorkerSet's
`UpdateInProgress` condition -- or the variant's own replicas reporting more
than one pod template revision. The rule has no memory, so an undo, a
restart, a failover or a forget in mid-rollout reads the same as any other
cycle. An earlier version remembered where a rollout began and gated only
argument changes; each review found another way that memory was lost or
wrong (an undo, a restart, a LeaderWorkerSet status lag), and every gap
recorded old pods as the new engine's own.

A LeaderWorkerSet's status counts are not the test. Its controller leaves a
group whose worker StatefulSet does not exist yet out of `UpdatedReplicas`,
while `Replicas` counts the group's Pending leader, and the worker
StatefulSet is created only once the leader is Ready or scheduled -- so
fewer updated groups than groups is every scale-up of a set with the
`LeaderReady` startup policy or exclusive placement, and gating on it held
back every reading for as long as the new group took to start, which is
when the variant saturates. Nor do its conditions carry a generation, so a
template change its controller has not acted on cannot be told from the
status; the pods' revisions tell it once any group has moved. The window
before that -- the controller not yet reconciled, or down -- is not covered.

A rollout's readings price nothing. The fleet is priced from what the
variant already holds: its own window under the arguments it runs, or --
when the rollout changed the arguments -- its previous figure, on a bound
the rollout freezes; a sibling's figure only below either, as ever. An
earlier version folded the readings into the previous figure, first as the
lowest single reading, then as the median of a few. The lowest gave one
outlier all the weight and held the fleet at it for 6.5 hours after the
rollout. The median needed readings that one cycle rarely gives, and the
per-cycle sweep dropped what a cycle had collected before the next added
to it; where it did fold, the figure was dropped the cycle the rollout
ended, so the hold it promised after the rollout never came. The cost of
the simple rule is that a rollout which lowers capacity -- a new image
that holds less -- is priced on the figure from before it until the new
pods saturate after it ends, and an image-only rollout records nothing
until it ends either. Known limits: a LeaderWorkerSet held part way by a
partition reports two revisions for as long as it is held, so nothing is
learned for the variant until it is let through; and a template change
its controller has not acted on for any group is not seen as a rollout,
so the first readings of that window are recorded under the new
arguments.

The lower bound waits for the rollout. Its hold and relaxation are what the
variant spends measuring itself, and with every reading held back nothing
can be measured: a clock that ran relaxed the variant toward derived or k1
above what its pods demonstrably held for as long as a rollout stuck on a
pending surge pod. Frozen, a bound the change of arguments restarted keeps
its whole hold for the variant's first serving cycle after the rollout.

The previous figure is the variant's LATEST measurement, under the arguments
it just left: a later change replaces it, lower or not, and the variant's
first measurement under the arguments it runs retires it. It ages as the
window it stands in for: a bucket retention after it was last offered to a
decision. An earlier version aged it from when it became one, so a bound
frozen by a rollout, or used by a variant serving a few minutes a day, lost
it at full strength in one cycle. A rollback is the stash's case, which puts
the window measured under the arguments rolled back to in front of any
previous figure.

Across a change of arguments the old saturated throughput still holds the
floor, with the authority of a neighbouring bucket's: the floor may hold the
fleet on it, never grow it, until the new arguments measure their own -- for
at most the hold and relaxation of a lower bound, 6.5 hours of serving, and
read without touching it, so it ages from the change. Dropped, the change
switched off the one floor that can only add replicas; unbounded and
touched, a change that doubled the service rate held the old engine's fleet,
at half load, for as long as the load was steady.

This is the same whether the arguments changed while the controller ran or
while it was down. A restore installs each window with the arguments it was
written under, and the first read applies the rule above -- under a
configuration not read completely too, and for a document that recorded no
arguments at all (an upgrade from a release without them, or past a
FingerprintVersion bump), whose windows are tagged with no configuration's
arguments and so each becomes a previous figure. The separate prior of ev-shindin/llm-scaling-manager#151
is gone; a document a ev-shindin/llm-scaling-manager#151 build wrote has its priors read as previous
figures. What a restart keeps is decided per window, not per variant,
so a window learned before a live rollout and exported after it is no longer
restored as if it described the new arguments.

The snapshot exports a window under its variant's arguments with no tag of
its own, and a window still tagged with other arguments put aside, with the
previous figure a read would have made of it -- from the document alone and
on the window's own clocks: it became a figure when the window was last
written and was last used when the window was last read, so two exports a
minute apart are the same document and the figure survives a restart as
long as the window would have. Stamped from the last write alone, a restore
dropped the figure of every window a coping fleet kept alive by reading it.
A running bound is exported unarmed so that a restore arms it afresh, as the
live path restarts it. A controller from before the tags ignores the
put-aside windows, so a rollback never installs one as the variant's own. A
variant whose configuration was not read completely is exported under it,
tagged as unread: it restores only into the same unread configuration.

**What stays per variant.** The ceiling a parked variant keeps after a rollout
(`PriorLiveCapacity`) stays on the capacity record. It is the last measured
`min(k1, k2)` for the whole variant, and the windows hold k2 alone, per bucket,
and are moved only when they are read, which a parked variant never does.
Reading the windows instead would drop the k1 part of the ceiling for a
k1-bound variant. The restore keeps its per-variant check only for what is
not per window: the ITL baseline needs the whole configuration, image
included, and the start-time estimate needs the same arguments.

**What it reports.** `llmsm_learned_state_k2_source` names the variant's own
previous figure `P2-prior`, distinct from a sibling's `P2-borrow`, with its
age. `llmsm_learned_state_restores_total` counts each restore by its outcome:
`whole`, `args`, `prior`, `incomplete` or `differs`.

## Order of work

1. **Merge ev-shindin/llm-scaling-manager#126 as it is.** It ships the digest and `llmsm_engine_config` with
   reuse off, which is what question 3 below needs measured.
2. **Answer question 3 from `llmsm_engine_config` on a real fleet.**
3. **Per-variant storage for k2 and throughput**, with tiers 0 and 3 only and
   rule 3. This is a behaviour change on its own: it removes today's
   cross-variant k2 pooling whatever the switch says. It needs a cluster run
   with two variants of one model under different `--max-num-seqs`, showing
   that neither inherits the other's k2.
   **Built** for namespace and variant (`historyKey`, `throughputKey`), with
   specs in `per_variant_history_test.go`; the configuration digest, now on
   main with ev-shindin/llm-scaling-manager#126, is the next change to the key. The cluster run is not done.
   **Storage alone had a cost**, found in review: a variant with no
   saturated reading of its own used to read a sibling's pooled k2, and
   per-variant windows sent it to the derived figure or to k1 instead. In a
   spec, a sibling saturated at 4000 tokens gave a new variant 4000 before
   and 12800 (its k1) after: a canary starting at 3.2x the capacity, the
   TTFT direction. **Closed by the k2 borrow, built with this step:**
   `P2-borrow` (`k2_borrow.go`) reads a sibling in the same namespace, on the
   same model, accelerator, GPU count, role, bucket and queue threshold,
   whose engine parameters pass `IsCapacityCompatible` -- tier 2 on main,
   without waiting for ev-shindin/llm-scaling-manager#126's digest. It is used only below the variant's
   own capacity, `min(k1, own k2)` (rule 1), held for `BorrowedK2Hold`
   (30 min) and relaxed to the own figure over `BorrowedK2Relax` (6 h)
   (rule 2), read without touching the donor (rule 3), and logged with the
   donor named. The relaxation clock is per bucket of the variant's own key:
   a flickering donor does not restart it, and a bucket the variant has never
   measured gets its own hold. Keyed per variant, a variant past its first
   6.5 hours went straight to its own k1 in a new bucket (3.2x in review),
   which is this gap again; per bucket, the cost is at most one hold per
   bucket, in replicas. The clock counts serving time only -- cycles where a
   Ready replica of the variant's own serves the bucket -- so a parked
   canary does not wake with its hold spent, and a warm-pool bridge reads a
   borrow without starting it. A spent borrow stays spent: the clock keeps
   the figure and the borrower's engine arguments it last ran on, and
   re-arms only on a figure more than 20% lower, or other arguments (see
   "One lower bound per key" above). An earlier version dropped the clock a
   retention after it ended, which re-armed the borrow at full strength once
   a week with no new evidence.
   One side effect, accepted: the priced capacity is written to the capacity
   store as a live record, and `Store.FindCompatible` reads records across
   namespaces for zero-replica variants. A borrowed figure can reach another
   namespace that way, without relaxation. It can only lower what it reaches,
   and P3/P4 figures already travel the same path; the k2 windows themselves
   are written only by P1-obs, so a borrowed figure never becomes a donor. It is NOT
   behind `enableLearnedStateReuse`: that switch gates where a sibling's
   figure can become an answer that orders fewer replicas, and this one
   cannot. Specs in `k2_borrow_test.go`, each with a mutation that fails it.
   **Throughput is not borrowed across variants.** A canary with no mu of
   its own simply has no floor until it measures one (the floor skips an
   unpriced replica), so it carries no too-low risk to expire; a borrowed mu
   would, and would need the same expiry before it could be added.
4. **Tiers 1 and 2 for the ITL line** behind `enableLearnedStateReuse`, and
   tier 1 (identical flags, by `Fingerprint()`) for k2 once the digest is on
   main. The k2 half of rule 1 is already pinned: a spec in which a donor's
   higher k2 fails to raise the borrower's ("never raises a variant above its
   own figure").

## Open questions

1. **How many own observations should displace a tier-2 prior?** "Fewer than
   tier 1" is the rule; the numbers want a measurement, not a guess.
2. **Should tier 2 require a *minimum* similarity**, or is "same model,
   accelerator, GPU count" enough? A variant differing in `MaxModelLen` by 100x
   is compatible by the twelve-field predicate but probably should not lend.
3. **Is tier 2 worth it at all**, or is tier 1 plus a correct image hash enough
   in practice? Worth measuring on a real fleet before building the tiering: if
   nearly every real pair that should share is already tier 1, the tiering is
   complexity for no gain.
4. **Should a different namespace be a distance or a wall?** Borrowing across
   namespaces is what lets a new tenant's first deployment start warm. It also
   lets one tenant's configuration shape another's sizing. A distance keeps the
   first; a wall, or a policy setting, keeps the second.
5. **Where does the tier surface in Prometheus?** This project's convention is
   a `reason` on an existing series rather than a new gauge.

Question 3 is the one to answer first, and it is answerable from
`llmsm_engine_config` on a real fleet without writing any of this.
