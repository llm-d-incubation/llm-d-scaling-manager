# Learned state: share the evidence, keep the estimates per consumer

**Status:** proposed, 2026-11-28. The direction is agreed; nothing is built. It refines
[Learned state across a restart: a snapshot](learned-state-snapshot.md) and
[Learned-state sharing by similarity](learned-state-sharing-by-similarity.md). It does not replace either.
Tracked in #540; the controller ports are #169 lines; restarts are #520.

**Audience:** contributors to `internal/engines/analyzers/saturation`, `internal/signals/itl`,
`internal/engines/analyzers/learnreplay` and `internal/engines/steadystate`.

Citations are at main `4ca90ae9`.

## The design in one paragraph

The learned-state snapshot mixes two kinds of thing:
- **evidence:** readings, their clocks, settle flags, the tier and the engine arguments they were taken under;
- **estimates** made from the evidence: k2, held slopes, held floor B, lower bounds.

The estimates carry the controller's loss function. The controller wants k2 low on purpose, because it can watch the
fleet and correct. The capacity planner has no feedback, so it needs unbiased figures with intervals.

Split the two:
- **Shared evidence**, stored once. Both consumers read it, and parity between them is checked on it alone.
- **Estimates per consumer.** The controller keeps its biased-low k2, its holds and its lower bounds. The planner
  refits from the same evidence with its own estimators and reports intervals.

Two things follow:
- **Cross-variant reuse becomes partial pooling.** Siblings' evidence forms a prior that weakens with configuration
  distance and fades as the variant's own evidence grows. It is no longer an equality key with an expiry constant.
- **A restart restores the observation windows** as well as the parameters fitted from them, so it is bumpless
  (#520).

## The problem

### One snapshot, two loss functions

The snapshot is `saturation.LearnedState` (`saturation/learned_snapshot.go:95-193`). It is stored in the ConfigMap
`scaling-manager-learned-state` under the key `v1` (`steadystate/learned_state_store.go:49-53, 81`).

Most of what it holds is an estimate, and several estimates are biased on purpose:

| field | what it is | how it is biased |
|---|---|---|
| `K2` | rolling window of admitted saturated-KV readings (10 values) | evidence, but admitted only at saturation, so it is censored (below) |
| `LowerBounds` | the k2 lower bound per key, with donor, prior and clocks | **low on purpose**: "LOWER ONLY" (`k2_borrow.go:28-33`) |
| `ITLSlopes` | held slope A and floor B per ITL key | **held**: kept after the readings behind it age out |
| `ITLBaseline`, `ITLLines` | fitted B; a line shared across variants | point estimates with no interval |
| `Throughput.Held` | the busy-share bound's held figure | **held lower bound** |
| `StartSeconds` | start-time estimate | point estimate; its sufficient statistics (`StartSamples`) sit beside it |
| `K2Args`, `Config`, `Args`, the clocks, `VariantSeenAt` | fingerprints and times | evidence |

The low bias is right for the controller, and the code says why (`k2_borrow.go:35-39`):

> IT EXPIRES. A too-LOW k2 does not correct itself. k2 is measured only at saturation (P1-obs needs the queue over
> its threshold), and a low k2 orders extra replicas, so each runs light, nothing saturates, nothing is measured,
> and the low figure also blocks the scale-down that would let a replica saturate.

The same rule appears in `k2_settle.go:15-16` and `k2_confirm.go:15-17`. A controller can afford a low k2: it orders
a replica too many and watches. A planner answering "how many replicas at N" cannot watch. A deliberately low k2 is a
deliberately high replica count, and that bias goes straight into the answer.

### The planner inherits the controller's estimates

The planner does not read the ConfigMap. `learnreplay.StateAt` replays a recording through the same analyzers, with
`PlannerSwitches` applied (`learnreplay/state.go:586-628, 630-650`). That sharing of code is the right design. But
two things carry the controller's estimates into the planner:

1. **The restore event.** If the recording holds a `RestoreLearnedState` event, the replay feeds the same snapshot
   document to `RestoreLearnedState` (`learnreplay/apply.go:99-108`). The planner then starts from the controller's
   lower bounds, held slopes and floor B.
2. **The analyzers themselves.** The replayed analyzers build the controller's estimates, and the planner reads them.
   A planner-side choice is expressed as a switch on the shared code.

### Parity is asked on the wrong layer

`PlannerSwitches` has 18 entries: 14 saturation and 4 slo (`state.go:587-607`). The controller sets none of the
planner-only ones, so they stay false. It sets `itlBaselineFromFloor` and `itlFloorBaselineHeld` to true
(`saturation/analyzer.go:935-936`).

The parity work so far (#169) has treated each of the 14 saturation entries as needing one setting for both sides,
with the two floor-B switches decided first. That treats two consumers with different losses as one. Some entries
decide **what counts as a reading**, and those must match. Others decide **what to estimate from the readings**, and
those may differ by design. The classification is in [Switches, sorted](#switches-sorted).

### A restart keeps the parameters and loses the state

- The ITL window holds 20 readings per key, over 30 minutes, with a minimum spread of 0.30 and a band of 0.15-0.80
  (`signals/itl/window.go:13-36, 66`). It is not in the snapshot. That is deliberate in the snapshot proposal
  (`learned-state-snapshot.md:83`), and the operator doc says so (`docs/reference/install-options.md:956`).
- The below-band readings (`itl/below_band.go:25-31`, 240 per key) and k2's held first reading (`k2_confirm.go:45-47`)
  are not in it either.
- What is restored instead is the slope and floor B (`ITLSlopes`).

After a restart, the window refills with readings that the settle tracker marks as post-restart. A gate that drops
them can leave a key with no line, and demand moves on the restart goldens (#520: the ITL gate losing the line at a
restart). Standard practice is bumpless transfer: the controller's output must not step because the controller
re-initialised.

### Reuse across variants uses equality keys

A k2 borrow (`k2_borrow.go:404-576`) takes, as a candidate, either of two figures below the variant's own:
- the variant's own previous figure, measured under engine arguments it no longer runs (`lb.prior`, taken as a
  candidate with `own: true`, `:435-436`);
- a sibling's measured k2, where the sibling has every one of these:
  - the same namespace;
  - the same `model|accel|gpus|role|bucket|qN`;
  - KV totals within 3% (`BorrowKvTotalTolerance`, `:127`);
  - `IsCapacityCompatible`;
  - a current donor window.

Among several candidates the highest is taken, the least far below the variant's own figure; the variant's own
previous figure wins a tie (`:370-378`). What a sibling lends is an estimate: the donor's `Average()`. That estimate
is held for 30 minutes (`BorrowedK2Hold`), then relaxed over 6 hours (`BorrowedK2Relax`) in serving time.

`enableLearnedStateReuse` (`config/saturation_scaling.go:135`, off by default) gates the fingerprint-keyed ITL line
and `lookupCompatibleCapacity`. It does not gate the k2 borrow. Its own comment says equality over a digest of
parsed engine flags "is not sound" (`:88-98`).

The similarity proposal already reframed a borrowed reading as a prior, not an identity. This proposal gives that
prior a weight.

## The design

### 1. Evidence: what is shared

Evidence is what was observed, with what is needed to judge it later. It is never a fitted value.

| evidence | per key | today |
|---|---|---|
| ITL readings: k, ITL, time, settle flag, replica, running sequences | 20 | in memory only |
| below-band ITL readings, same fields, grouped by running sequences | up to 240 | in memory only, under `BaselineFromFloor` |
| k2 readings (the 10 admitted values, their clocks, args) | 10 | in the snapshot (`K2`, `K2Args`, `K2Stash`) |
| k2 first reading held for confirmation | 1 | in memory only |
| **unsaturated exposure:** the highest fill served without saturating, per bucket and settle period | 1 per period | not recorded |
| saturated mu readings and their clocks | window | in the snapshot (`Throughput`) |
| prefill minutes (f samples, V_P sources, the low-load minutes for `a`) | window | not in the snapshot |
| start samples (Ready time per counted Pod) | per Pod | in the snapshot (`StartCounted`, `StartSamples`) |
| tier, engine-args fingerprint, `Config`, `Args`, `VariantSeenAt` | 1 | in the snapshot |

- **Unsaturated exposure is new.** It is the evidence that answers "a too-low k2 never corrects itself". A variant
  that served fill f for a settle period without saturating has shown that its k2 is likely above f. Today that
  observation is thrown away. A wall-clock expiry stands in for it.
- **Below-band readings keep their sequence counts.** The floor below the band is read per octave of running
  sequences, and the line through those floors gives the floor by count (`itl.BelowBandByCount`,
  `below_band.go:114-132`; `whatif.BelowBand.LeastSequences` and `ByCount`; #286). A single quantile over all the
  readings would drop that, so the evidence keeps the readings grouped by octave, with the least sequence count.
- **Admission is a tag, not a drop, where the tag is cheap.** For example: was this minute low-load by rate? Was
  it per rank? Then each consumer applies its own admission rule to the same readings. Where a tag would be too
  large, the admission rule stays shared and needs one setting for both sides (see [Switches, sorted](#switches-sorted)).

### 2. Estimates: what each consumer makes of it

| quantity | controller (keeps what it does today) | planner |
|---|---|---|
| k2 | the lower bound and the borrow, lower only; held first reading | the posterior of k2 from the k2 readings *and* the unsaturated exposure (a censored likelihood), as a median and an interval |
| ITL line (A, B) | held slope, floor B held (`itlFloorBaselineHeld`) | a refit on the window with its standard error; B as an interval [floor, fit], not a point |
| mu | the busy-share held bound | an unbiased estimate from saturated readings, with an interval |
| prefill `a`, f, V_P | as the controller's switches set them | as `PlannerSwitches` set them |
| start | the mean | the mean and p90 (the day plan's lead time needs p90) |

- Estimates are not shared. The controller's estimates stay in its snapshot and are restored directly, so its
  decisions after a restore do not depend on the evidence (migration step 1). The planner never restores them; it
  refits from evidence (step 2).
- "On for the planner, off in the controller" stops being a flag on shared code that changes what the controller
  learns. It becomes the name of the planner's estimator over the same evidence.
- **The evidence is collected under the controller's policy.** The fills the ITL line sees, and the loads prefill
  sees, are the ones the controller's sizing produced. The planner's refit is unbiased inside that range. Asked
  about a rate or a replica count outside it, the planner extrapolates, and its intervals cover the sampling noise
  in the readings, not that selection. The concept page's bands section says the same of today's bands
  ([modeling and optimization](../concepts/modeling-and-optimization.md)). The censoring itself is not a bias: the
  controller's past estimate, which decides what saturates, is observed, so the censored likelihood stays valid.

### 3. Reuse across variants: partial pooling

For a quantity θ and a variant v with no or little evidence of its own.

- **Scale.** k2 and mu are positive, so they are pooled as log θ. A and B are correlated through the fit, so they
  are pooled jointly, as one vector with the fit's covariance; a sibling's B is never moved without its A.
- **A hierarchical prior over the siblings.** A variant usually has several siblings s = 1..m, not one.
  - Each sibling's estimate θ̂_s has a sampling variance V_s = σ_s²/n_eff,s from its own evidence.
  - The siblings are draws around a group value: θ_s ~ Normal(μ, τ²(d_s)), where d_s is the sibling's
    configuration distance from v.
  - μ is estimated from the siblings, each weighted by 1/(V_s + τ²(d_s)). v's prior is
    Normal(μ̂, Var(μ̂) + τ²(d_v)). With one sibling this reduces to the sibling's figure, widened by its distance.
  - d comes from the similarity tiers: 0 for the same `Fingerprint`, larger for `IsCapacityCompatible` only,
    larger again across a namespace. τ² grows with d. Unresolved flags add distance, as the similarity proposal
    already says ("a distance penalty, not a veto").
- **Own readings update it.** The posterior weight of the prior is w = (1/V_prior) / (1/V_prior + n_eff,v/σ_v²).
  It falls as the variant's own evidence grows, with no expiry constant and no equality key.
- **Counts are effective counts.** Readings every 15 s, of overlapping one-minute rates, are autocorrelated. A raw
  count makes the sibling's weight fall too fast and the interval too narrow. n_eff is the count of independent
  blocks, as the planner's bands already resample in blocks of 4 cycles (`whatif/band.go`), or n(1 - ρ)/(1 + ρ)
  from the lag-1 autocorrelation ρ.
- **The controller keeps its rules on top.** Its borrowed k2 is a low quantile of that posterior, used only below
  the variant's own figure. So "LOWER ONLY" holds unchanged, and so do the existing hold, relax and spent-stays-spent
  rules.
  - This still changes the controller's sizing: a low quantile of a posterior is not the donor's `Average()`, and
    it moves the borrowed figure even under LOWER ONLY. It is its own #169 line, behind a switch that is off in the
    controller's constructor, and it waits for the closed-loop gate (see [Tests that gate the steps](#tests-that-gate-the-steps)).
- **What expires a low prior is evidence.** It is the unsaturated exposure, through the censored likelihood, not
  wall time. Until that likelihood is built and validated, the wall-clock relax stays.
- **The planner** uses the posterior median and interval.
- **Siblings share evidence, never estimates.** A donor's biased-low k2 is not a prior. Its readings are.
- `enableLearnedStateReuse` stays the opt-in for the ITL line and compatible capacity, and `disableK2Borrow` stays
  the switch for the borrow. Pooling changes what they compute, not who turns them on.

### 4. Restarts persist the observation windows (#520)

- The controller writes the evidence of section 1 and restores it, so the ITL window, the below-band readings and
  the held first k2 reading survive a restart.
- Restored readings keep their own clocks and settle flags. A restore never makes a reading younger, the same rule
  the snapshot already applies to figures (`learned_snapshot.go:20-22`).
- **A down-side slew limit, on its own switch.** For one settle period after a restore, a role's demand may not fall
  faster than a stated rate below its last published demand. Increases are free. The published demand and its time
  are stored in the evidence document, not in `v1`. It is a controller behaviour change, so it has its own switch
  (step 1b), separate from persistence, and the closed-loop gate judges it alone. It is not in the first version of
  the evidence document: step 1 persists evidence only.

## Switches, sorted

The 18 `PlannerSwitches` entries (`learnreplay/state.go:587-607`), sorted by layer. The criterion is where the
switch acts:
- **Evidence (admission):** applied *before* a reading is stored. It decides which readings count, and what it
  drops cannot be recovered later. Shared, so it needs one setting, or a tag that lets each consumer apply its own.
- **Estimator:** applied *over* stored readings. It decides what is made of them. Per consumer, so it may differ by
  design.

The rows are classified against the storage after migration: section 1 stores the prefill minutes raw, so a rule
that today forms readings from minutes at ingestion becomes an estimator over the stored minutes.

| # | switch | planner | controller | layer | what changes under this proposal |
|---|---|---|---|---|---|
| 1 | `PrefillALowLoadPerRank` | on | off | evidence | tag each minute with its rank; each side filters |
| 2 | `PrefillAEngineTiming` | on | off | evidence (a second source of readings) | store the engine timing readings; the choice of source becomes an estimator |
| 3 | `PrefillQueueByCapacity` | on | off | evidence: sets each stored sample's busy flag at ingestion (`prefill/prefill.go:673-675`), which V_P's busy minutes and peak-under-queue read | tag each minute with both busy tests (both waiting counts are in the row); each side filters |
| 4 | `PrefillFBusyMinutes` | on | off | estimator: applied at estimate time (`est.imbalance`, `prefill.go:734`) | none |
| 5 | `PrefillFPooledMinutes` | on | off | estimator after migration: applied at ingestion today (`ks.noteImbalance`, `prefill.go:714`), over the stored minutes once they are stored | pooling runs over the stored minutes; no shared setting needed |
| 6 | `PrefillALowLoadByRate` | on | off | evidence | tag each minute with its rate test |
| 7 | `PrefillAMedianOverhead` | on | off | estimator | none |
| 8 | `ITLAdaptiveBandFloor` | on | off | evidence (which ITL readings enter the band) | tag readings below the fixed floor; each side filters |
| 9 | `PrefillVPFloorServed` | on | off | estimator | none |
| 10 | `PrefillVPServedUnqueued` | on | off | estimator | none |
| 11 | `PlannerCapAtStepBudget` | on | off | estimator | none. The controller twin `mu-at-step-budget` (`step_budget_cap.go:27`) is off from the constructor; its flip waits on the step-budget-binding detector (#499, `capacity/step_budget_binding.go`) |
| 12 | `BacktestStepOutputDispersion` | on | n/a | backtest only | none |
| 13 | `ITLBaselineFromFloor` | **off** | **on** | estimator | the disagreement is intended: the controller's B is a lower bound, the planner's is an interval |
| 14 | `ITLFloorBaselineHeld` | **off** | **on** | estimator (a hold) | same as 13 |
| slo 1 | `TTFTHopUnqueued` | on | off | evidence (which windows read the hop) | tag windows with "prefill queue empty" |
| slo 2 | `TTFTDecodeSpeculatingHeld` | on | off | estimator (a hold) | none |
| slo 3 | `TTFTRouterWaitInPrefill` | on (default) | off | estimator | none |
| slo 4 | `TTFTImbalanceAcrossCounts` | on (default) | off | estimator | none |

- **The floor-B question (13, 14) dissolves.** The two consumers keep different B on purpose, read from the same
  below-band readings. The planner reported 14 under-reads under the controller's B (#318). That is the measured
  reason not to give the planner a held lower bound.
- **The evidence rows (1, 2, 3, 6, 8, slo 1) are the parity work that remains.** All six become tags or a stored
  second source; none needs one shared setting once step 4 lands.
- **The estimator rows need no parity.** A controller port of a planner estimator is still a #169 line with its
  own evidence, behind a switch that is off in the controller's constructor.

## Migration

### Byte compatibility

- **Recordings.**
  - A learnrec event is `{Seq, Now, Kind, Data}` (`signals/learnrec/recorder.go:28-41`). Recordings carry no schema
    version; compatibility rests on `learnrec:"omitzero"`.
  - Every new field is `omitzero`, and every new event kind is written only when its switch is on. A controller with
    the switches off writes byte-identical recordings.
  - Old recordings decode and replay unchanged.
- **Goldens.**
  - **Controller side.** With every switch off, every learner golden (`learnreplay/testdata/goldens/`) replayed
    with the controller's switches is byte-identical. For the controller, `goldens:` reads `none` at every step.
  - **Planner side.** Steps 2-5 are on in `PlannerSwitches`, so they move planner goldens. Each step's PR lists the
    goldens it moves in its `goldens:` section, regenerated, with the reason per golden:
    - step 2 moves every planner golden whose recording holds a `saturation.RestoreLearnedState` event (the
      candidates are the `*-restart`, `pd-restart` and `start-restored` sets) and every `cmd/capacity-planner`
      golden over those recordings;
    - step 3 moves the k2 figures and their intervals wherever the replay records unsaturated exposure, which is
      most recordings with a saturating stretch;
    - step 4 is expected to move none: each side's filter over the tags reproduces today's switch, and the step's
      test is that the planner goldens do not move;
    - step 5 moves the goldens whose recordings hold two or more variants sharing a sibling key.
- **The registry tests.** Every new switch passes the three tests a new planner flag or switch must pass:
  - slo `TestTTFT_EveryModelFlagIsListedOrOffThePath`;
  - whatifapi `TestPageNamesEveryFlag`;
  - learnreplay `TestMessageWords_AreCurrent`.

  Step 2 is a switch in the `learnreplay` package itself. `PlannerSwitches` has package keys for `saturation` and
  `slo` only today, so step 2 adds a `learnreplay` key, and `withPlannerSwitches` and the recording's switch check
  learn it.
- **Snapshots: a separate store.**
  - The evidence is a separate document in its own ConfigMap, `scaling-manager-learned-evidence`. It is not the
    next format version of the learned-state store: it has its own store, its own key and its own budget.
  - `scaling-manager-learned-state` and its `v1` document are not touched, so `v1` stays byte-identical, and its
    900 KiB data budget (`learnedStateMaxData`, `learned_state_store.go:67`) is not shared with the evidence.
  - The evidence store has its own budget, the same limits as the learned-state store (700 KiB a document, 900 KiB
    of data), and the same least-recently-used drop.
  - A controller that does not know the evidence ConfigMap ignores it.
  - **Size.** A reading's fields are k, ITL, time, settle flag, replica and running sequences. Packed:
    - k to 1/1000 (2 bytes); ITL to 0.01 ms (3 bytes); time as whole seconds before the key's newest reading
      (2 bytes); the settle flag and an index into the key's replica table (1 byte); running sequences (2 bytes);
    - about 10 bytes a reading, and about 13 after base64, which adds a third.

    At about 300 keys, 20 ITL readings and 240 below-band readings each, that is about 1 MB, over budget. So the
    below-band readings are kept per octave of running sequences, and each group keeps only the readings its floor
    can be read from: its lowest floor(0.1·n_g) + 1 readings by ITL, its count n_g, and the counts of the readings
    not kept by minute of age (30 bytes a group), so that pruning by age keeps n_g right. With the least sequence
    count beside them, that is what `BelowBandByCount` reads. A key then holds about 20 + 30 readings and its age
    counts, under 1 KB, and 300 keys about 260 KB.
    - The floor is exact at the restore. Once a kept reading ages out, the reading at the floor's index may be one
      that was not kept, and the floor read from the kept ones is at or below the true one. That lasts until live
      readings refill the group, at most the 30-minute age limit, and is inside the tolerance the
      restore-and-recompute test states.
  - **RBAC and the installer.**
    - Today's Role grants get and update on `scaling-manager-learned-state` by name only
      (`config/base/rbac/learned-state-role.yaml`, `resourceNames`). The evidence ConfigMap is added to that Role's
      `resourceNames`; create is already granted without a name.
    - The installer lists the learned-state Role as optional (`deploy/lib/install_core.sh:67, 80-81`), and cleanup
      removes the learned-state ConfigMap (`deploy/lib/cleanup.sh:262-267`). Both learn the evidence ConfigMap, and
      the rendered overlays are checked for the new name.
    - On an install whose Role was not upgraded, the evidence write is Forbidden. That is reported as Forbidden,
      once, as the learned-state store already reports its own, and never read as "no evidence stored". The
      controller then runs on `v1` alone, as today.
  - **Tests:**
    - a `v1` document written by main restores into the new code byte-identically, with the switches off;
    - an evidence-aware controller writes a `v1` key that is byte-identical to main's;
    - a main controller restores from a namespace that also holds the evidence ConfigMap;
    - a controller whose Role lacks the evidence ConfigMap reports Forbidden and restores `v1` unchanged.

### Staged switches

Each step is behind a switch that is off in the controller's constructor. With the switch off, controller output is
byte-identical.

| step | what | switch (package `saturation` unless named) | planner | controller | flipped by |
|---|---|---|---|---|---|
| 0 | Refactor: an `Evidence` type inside the analyzers. The estimators read from it; nothing persists. | none | - | - | byte-identical goldens and controller output |
| 1 | Write and restore the evidence document: the ITL windows, below-band readings, held k2 readings, prefill minutes. | `learned-evidence-snapshot` | n/a | off | the restart sweep below; #520's acceptance |
| 1b | The restart slew limit, with the published demand stored in the evidence document | `restart-demand-slew-limit` | n/a | off | the closed-loop gate, judged alone |
| 2 | The planner refits from evidence and never restores the controller's estimates from a restore event | `learnreplay` `planner-estimates-from-evidence` | **on** | n/a (planner only) | the class matrix, scored as the [accuracy page](../guides/capacity-planner/accuracy.md) scores it |
| 3 | Unsaturated exposure recorded; planner k2 from the censored likelihood, with an interval | `k2-unsaturated-exposure` | on | off | planner: the class matrix. Controller: only as a replacement for the expiry, with its own #169 line |
| 4 | Admission tags for rows 1, 2, 3, 6, 8 and slo 1; each side filters | `evidence-admission-tags` (+ `slo`) | on | off | parity check on evidence only; planner goldens unmoved |
| 5 | Partial pooling for cross-variant reuse | `learned-state-pooling` | on | off | the two-variant cluster run the similarity proposal already asks for; the controller's low quantile is its own #169 line |
| 6 | Stop writing estimates the planner no longer needs into recordings; drop `v1` after one release with both | none | - | - | one release cycle with both documents |

**Order, and older recordings.** Step 2 needs step 1. The planner can refit from evidence only on recordings where
the restore event carries it, which step 1 writes. A recording made before step 1 has a restore event that holds the
controller's estimates and no evidence. With step 2 on, the planner restores nothing from it and refits from the
readings that follow, so figures asked for before the windows refill are refused: up to 30 minutes of recording
after each restore for the ITL line, and longer for k2. That is a coverage cost on every older restart recording.
Step 2's PR reports how many figures it refuses on the restart goldens. If the cost is too high, the fallback is to
restore the controller's estimates on older recordings only, with a note that names them as the controller's.

### Tests that gate the steps

- **Restore-and-recompute.** On every restart golden, compare a controller that restores the evidence and recomputes
  with one that restores its estimates directly. They cannot be byte-identical, and the test does not ask it:
  - Several estimates are not functions of the persisted evidence: the held slopes (`ITLSlopes`), kept after their
    readings age out; `LowerBounds`, with donor and prior; the K2 window, built over hours; and `Throughput.Held`.
    These are restored directly from `v1`, in both arms, and never recomputed.
  - The estimates that are functions of the persisted evidence are compared within a stated tolerance. The fitted
    A and B on the restored ITL window agree within the quantisation's bound (0.005 ms on each ITL, so 0.005 ms on
    B and the matching bound on A for the window's spread), and the below-band floor is equal at the restore.
  - The replica decision is equal on every cycle, except where the demand sits within that tolerance of a replica
    boundary; such cycles may differ by one replica, and the test lists them.

  This is what makes step 1 safe for the controller.
- **Planner from restored evidence.** The planner run on evidence restored from a controller snapshot is compared
  with the planner run on the same recording cut to the window the evidence holds. They agree within the tolerance
  above. Against the full recording window, which is longer by design, the figures are reported, and are expected
  inside the band; that comparison does not gate.
- **Restart at every cycle** (#520). Insert a restart at every cycle of every restart golden (or 50 sampled cycles).
  The decision may deviate by at most one replica and must recover within one settle period. Decode demand may not
  move by more than 5%.
- **Negative controls.**
  - Each new test fails on the parent commit.
  - The restart sweep fails with step 1's switch off on at least one golden; #520's own cases show it does today.
- **The closed-loop gate.** Any step that moves controller sizing (1b, 3 and 5 in the controller) waits for a
  closed-loop harness, which is not built. Open-loop replay cannot show the controller's response to its own
  output.

## What it costs

- **Storage:** a second ConfigMap with its own budget. Writes stay leader-only, at most once a minute.
- **RBAC and install:** the evidence ConfigMap added to the learned-state Role, the installer's optional list and
  cleanup, and the Forbidden path on an install whose Role was not upgraded.
- **Code:**
  - the `Evidence` type and its persistence (steps 0-1, the largest part);
  - the planner estimators (steps 2-3), and the `learnreplay` switch key;
  - the pooling arithmetic (step 5, small once the evidence is shared).
- **Goldens:** planner goldens move at steps 2, 3 and 5, each regenerated and listed in its PR.
- **Review load:** every estimator port to the controller remains a separate decision. This proposal makes those
  decisions smaller, not fewer.

## Related, not in scope

- **The ITL estimator itself**, a robust token-weighted fit gated on its standard error. It is a separate proposal.
  It may remove the post-restart gate entirely. Step 1 is still needed, because a robust fit also needs its readings
  after a restart.
- **Raising the ITL window from 20 toward 240 readings.** That is out of scope in the snapshot proposal, and still
  is. Step 1 persists the window at its current size.
- **TPOT percentiles from the decode chain's dispersion.** That is an estimator over the same evidence, and fits this
  design once step 2 is built.

## Open questions

1. **The distance scale τ²(d).** Fit it from the variants that have their own evidence: empirical Bayes over pairs
   at each tier. Or fix it per tier, conservatively, until enough pairs exist?
2. **Is a namespace a distance or a wall?** The similarity proposal leaves this open. The default here is a large
   distance, not a wall, but the controller keeps today's same-namespace rule until step 5 is validated.
3. **Where the settle period for the slew limit comes from:** the settle tracker's own period per role, or a fixed
   bound?
