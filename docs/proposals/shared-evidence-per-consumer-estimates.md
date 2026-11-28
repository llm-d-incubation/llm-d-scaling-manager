# Learned state: share the evidence, keep the estimates per consumer

**Status:** proposed, 2026-11-28. The direction is agreed; nothing is built. It refines
[Learned state across a restart: a snapshot](learned-state-snapshot.md) and
[Learned-state sharing by similarity](learned-state-sharing-by-similarity.md). It does not replace either.

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
- **Cross-variant reuse becomes partial pooling.** A sibling's evidence is a prior that weakens with configuration
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

The low bias is right for the controller, and the code says why (`k2_borrow.go:35-38`):

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

The controller-parity audit ends with "each of the 14 PlannerSwitches entries needs one setting for both sides,
with the two floor-B switches decided first". That treats two consumers with different losses as one. Some entries
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
restart; this repository's issue, not the older fork's #520 on queued work). Standard practice is bumpless transfer:
the controller's output must not step because the controller re-initialised.

### Reuse across variants uses equality keys

A k2 borrow (`k2_borrow.go:404-576`) needs every one of these:
- the same namespace;
- the same `model|accel|gpus|role|bucket|qN`;
- KV totals within 3% (`BorrowKvTotalTolerance`, `:127`);
- `IsCapacityCompatible`;
- a current donor window.

What it borrows is an estimate: the donor's `Average()`. That estimate is held for 30 minutes (`BorrowedK2Hold`),
then relaxed over 6 hours (`BorrowedK2Relax`) in serving time.

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
| ITL readings (k, ITL, time, settle flag, rank) | 20 | in memory only |
| below-band ITL readings and running sequences | up to 240 | in memory only, under `BaselineFromFloor` |
| k2 readings (the 10 admitted values, their clocks, args) | 10 | in the snapshot (`K2`, `K2Args`, `K2Stash`) |
| k2 first reading held for confirmation | 1 | in memory only |
| **unsaturated exposure:** the highest fill served without saturating, per bucket and settle period | 1 per period | not recorded |
| saturated mu readings and their clocks | window | in the snapshot (`Throughput`) |
| prefill minutes (f samples, V_P sources, the low-load minutes for `a`) | window | not in the snapshot (audit 1.2) |
| start samples (Ready time per counted Pod) | per Pod | in the snapshot (`StartCounted`, `StartSamples`) |
| tier, engine-args fingerprint, `Config`, `Args`, `VariantSeenAt` | 1 | in the snapshot |

- **Unsaturated exposure is new.** It is the evidence that answers "a too-low k2 never corrects itself". A variant
  that served fill f for a settle period without saturating has shown that its k2 is likely above f. Today that
  observation is thrown away. A wall-clock expiry stands in for it.
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

- Estimates are not shared. The controller's estimates stay in its snapshot, so its decisions after a restore are
  byte-identical (migration step 2). The planner never restores them; it refits from evidence.
- "On for the planner, off in the controller" stops being a flag on shared code that changes what the controller
  learns. It becomes the name of the planner's estimator over the same evidence.

### 3. Reuse across variants: partial pooling

For a quantity θ (k2, the line's A and B, mu) and a variant v with no or little evidence of its own:

- **Prior from siblings.** θ_v ~ Normal(θ_s, σ_s²/n_s + τ²(d)).
  - θ_s and n_s are the sibling's estimate and evidence count.
  - d is the configuration distance from the similarity tiers: 0 for the same `Fingerprint`, larger for
    `IsCapacityCompatible` only, larger again across a namespace.
  - τ² grows with d. Unresolved flags add distance, as the similarity proposal already says ("a distance penalty,
    not a veto").
- **Own readings update it.** The posterior weight of the sibling is w = (1/V_prior) / (1/V_prior + n_v/σ_v²). It
  falls as the variant's own evidence n_v grows, with no expiry constant and no equality key.
- **The controller keeps its rules on top.** Its borrowed k2 is a low quantile of that posterior, used only below
  the variant's own figure. So "LOWER ONLY" holds unchanged, and so do the existing hold, relax and spent-stays-spent
  rules.
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
- A down-side slew limit, as the control review recommends. For one settle period after a restore, a role's demand
  may not fall faster than a stated rate below its last published demand. Increases are free. The published demand
  and its time are added to the snapshot.

## Switches, sorted

The 18 `PlannerSwitches` entries (`learnreplay/state.go:587-607`), sorted by layer.
- **Evidence (admission):** decides which readings count. Shared, so it needs one setting, or a tag that lets each
  consumer apply its own.
- **Estimator:** decides what is made of the readings. Per consumer, so it may differ by design.

| # | switch | planner | controller | layer | what changes under this proposal |
|---|---|---|---|---|---|
| 1 | `PrefillALowLoadPerRank` | on | off | evidence | tag each minute with its rank; each side filters |
| 2 | `PrefillAEngineTiming` | on | off | evidence (a second source of readings) | store the engine timing readings; the choice of source becomes an estimator |
| 3 | `PrefillQueueByCapacity` | on | off | estimator | none |
| 4 | `PrefillFBusyMinutes` | on | off | estimator (median of the busiest) | none |
| 5 | `PrefillFPooledMinutes` | on | off | evidence (how minutes form a reading) | one setting, decided on a recording |
| 6 | `PrefillALowLoadByRate` | on | off | evidence | tag each minute with its rate test |
| 7 | `PrefillAMedianOverhead` | on | off | estimator | none |
| 8 | `ITLAdaptiveBandFloor` | on | off | evidence (which ITL readings enter the band) | tag readings below the fixed floor; each side filters |
| 9 | `PrefillVPFloorServed` | on | off | estimator | none |
| 10 | `PrefillVPServedUnqueued` | on | off | estimator | none |
| 11 | `PlannerCapAtStepBudget` | on | off | estimator | none (the controller twin `mu-at-step-budget` is gated on the #499 detector) |
| 12 | `BacktestStepOutputDispersion` | on | n/a | backtest only | none |
| 13 | `ITLBaselineFromFloor` | **off** | **on** | estimator | the disagreement is intended: the controller's B is a lower bound, the planner's is an interval |
| 14 | `ITLFloorBaselineHeld` | **off** | **on** | estimator (a hold) | same as 13 |
| slo 1 | `TTFTHopUnqueued` | on | off | evidence (which windows read the hop) | tag windows with "prefill queue empty" |
| slo 2 | `TTFTDecodeSpeculatingHeld` | on | off | estimator (a hold) | none |
| slo 3 | `TTFTRouterWaitInPrefill` | on (default) | off | estimator | none |
| slo 4 | `TTFTImbalanceAcrossCounts` | on (default) | off | estimator | none |

- **The audit's floor-B question (13, 14) dissolves.** The two consumers keep different B on purpose, read from the
  same below-band readings. The planner reported 14 under-reads under the controller's B (#318). That is the
  measured reason not to give the planner a held lower bound.
- **The evidence rows (1, 2, 5, 6, 8, slo 1) are the parity work that remains.** Rows 1, 2, 6, 8 and slo 1 become
  tags. Row 5 needs one setting.
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
  - **Test:** every learner golden (`learnreplay/testdata/goldens/`) and every `cmd/capacity-planner` golden replays
    byte-identically with the switches off. The PR template's `goldens:` section reads `none`.
- **Snapshots.**
  - The store keeps one document per format version, and leaves other versions untouched, so a rolled-back
    controller finds its own (`learned_state_store.go:50-52`).
  - Evidence goes in a **new document `v2`**, written beside `v1`. `v1` stays byte-identical while both are written.
  - A controller that does not know `v2` ignores it.
  - **Size:** evidence is larger than the estimates. At about 300 keys, 240 below-band readings per key, stored as
    float64 plus a timestamp, it would exceed the 700 KiB document limit (`learned_state_store.go:66-67`).
    - Store the readings quantised: ITL to 0.01 ms and the time as a minute offset from the key's newest reading,
      packed and base64 encoded, about 4 bytes per reading.
    - Store below-band readings as a fixed-size quantile sketch, not as a list. The estimator uses only q10
      (`below_band.go:31`).
    - Put `v2` in its own ConfigMap `scaling-manager-learned-evidence`, with its own budget and the same
      least-recently-used drop.
  - **Test:**
    - a `v1` document written by main restores into the new code byte-identically, with the switches off;
    - a `v2`-aware controller writes a `v1` key that is byte-identical to main's;
    - a main controller restores from a store that also holds `v2`.

### Staged switches

Each step is behind a switch that is off in the controller's constructor. With the switch off, output is
byte-identical.

| step | what | switch (package `saturation` unless named) | planner | controller | flipped by |
|---|---|---|---|---|---|
| 0 | Refactor: an `Evidence` type inside the analyzers. The estimators read from it; nothing persists. | none | - | - | byte-identical goldens and controller output |
| 1 | Write and restore `v2` evidence: the ITL windows, below-band readings, held k2 readings, prefill minutes. The restart slew limit. | `learned-evidence-snapshot` | n/a | off | the restart sweep below; #520's acceptance |
| 2 | The planner refits from evidence and never restores the controller's estimates from a restore event | `learnreplay` `planner-estimates-from-evidence` | **on** | n/a (planner only) | class matrix under Amendment A1 scoring |
| 3 | Unsaturated exposure recorded; planner k2 from the censored likelihood, with an interval | `k2-unsaturated-exposure` | on | off | planner: class matrix. Controller: only as a replacement for the expiry, with its own #169 line |
| 4 | Admission tags for rows 1, 2, 6, 8 and slo 1; each side filters | `evidence-admission-tags` (+ `slo`) | on | off | parity check on evidence only |
| 5 | Partial pooling for cross-variant reuse | `learned-state-pooling` | on | off | the two-variant cluster run the similarity proposal already asks for; the controller keeps lower-only |
| 6 | Stop writing estimates the planner no longer needs into recordings; drop `v1` after one release with both | none | - | - | one release cycle with both documents |

### Tests that gate the steps

- **Restore-and-recompute** (control review F5). On every restart golden, the controller's decisions are
  byte-identical whether it:
  - restores the evidence and recomputes its estimates; or
  - restores its estimates directly.

  This is what makes step 1 safe for the controller.
- **Planner from restored evidence.** The planner run on evidence restored from a controller snapshot equals the
  planner on the full recording window, within its band.
- **Restart at every cycle** (#520). Insert a restart at every cycle of every restart golden (or 50 sampled cycles).
  The decision may deviate by at most one replica and must recover within one settle period. Decode demand may not
  move by more than 5%.
- **Negative controls.**
  - Each new test fails on the parent commit.
  - The restart sweep fails with step 1's switch off on at least one golden; #520's own cases show it does today.
- **The closed-loop gate** (control review F4). Any step that moves controller sizing (1, 3 and 5 in the controller)
  waits for the closed-loop harness. Open-loop replay cannot show the controller's response.

## What it costs

- **Storage:** a second ConfigMap, budgeted. Writes stay leader-only, at most once a minute.
- **Code:**
  - the `Evidence` type and its persistence (step 0-1, the largest part);
  - the planner estimators (steps 2-3);
  - the pooling arithmetic (step 5, small once the evidence is shared).
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
4. **Row 5 (`PrefillFPooledMinutes`)** needs one setting for both sides. Which recording decides it?
