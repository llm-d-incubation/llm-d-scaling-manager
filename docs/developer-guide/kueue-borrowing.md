# Kueue borrowing internals

How Kueue cohort borrowing is built, for contributors. Paths are under
`internal/` unless noted. What an operator does and sees is in
[the guide](../guides/kueue-borrowing/) and
[the quota limiter reference](../reference/quota-limiter.md#cohort-borrowing);
the design, with the formulas, is the
[Kueue borrowing and limiter composition proposal](../proposals/kueue-borrowing-and-limiter-composition.md)
(Part B and its observability).

This page covers borrowing only. How the declared limiters are built and
composed, the node view, and the scale-from-zero snapshot (Part A) are on
[How the limiter set is built](limiter-composition.md). Counting and handing
back the capacity of lenders that do not reclaim, behind
`kueue.releaseToNonReclaimingLenders` (PR-C), is under
[Release to lenders Kueue does not reclaim for](#release-to-lenders-kueue-does-not-reclaim-for).

## What is read

All read-only. A cohort name with no Cohort object is a flat cohort.

| Object and field | Used for |
|---|---|
| ClusterQueue `spec.cohortName` (`spec.cohort` in v1beta1) | cohort membership |
| ClusterQueue `…resources[].nominalQuota`, `borrowingLimit`, `lendingLimit` | the reach formula |
| ClusterQueue `spec.resourceGroups[].coveredResources` | co-resources: every covered resource is computed like the GPU |
| ClusterQueue `status.flavorsReservation[].resources[].total` | reserved GPUs. Kueue charges quota at reservation; `flavorsUsage` lags it under admission checks |
| ClusterQueue `status.flavorsReservation[].resources[].borrowed` | `llmsm_kueue_borrow_drift_gpus` |
| ClusterQueue `spec.preemption.reclaimWithinCohort` | the guaranteed figure, which lenders count |
| ClusterQueue `spec.preemption.borrowWithinCohort` | `borrow-within-cohort` |
| ClusterQueue flavors per GPU resource | `flavor-unpinned`, only where more than one flavor offers the GPU |
| ClusterQueue `spec.admissionChecks`, `status.fairSharing`, `status.effectiveQuotas` | withheld reasons |
| Cohort `spec.parentName`, and any Cohort naming this one as parent | nested cohorts |
| ResourceFlavor `nodeLabels`, `tolerations`, `topologyName` | node fit per flavor, flavor pinning, `topology-aware` |
| Workloads, cluster-wide (informer, field index on owner UID) | managed check, priority, lender requests per flavor, gated demand of every cohort namespace, preemption |
| Namespace `ResourceQuota` `hard`, `used` (cached) | GPU bound and ceiling; with several, the tightest `hard` |
| Deployment `kueue-controller-manager` image | `kueue-version-untested`, `kueue-version-unknown` |
| Its leader-election Lease `renewTime`, `leaseDuration` | `kueue-controller-down` |

The reader resolves the API version through the RESTMapper, which prefers the
served version with the highest discovery priority (v1beta2 today). The `pkg/`
constants copied from Kueue (condition types, reasons, label keys, the
controller's Deployment name and namespace) live in `kueue/kueue_constants.go`
with their source path and the Kueue version they were copied from.

## Code touch points

- **Kueue figures:** `kueue/reader.go`; `kueue/borrow.go` (the reach formula,
  `QueueState.CanReclaim`, the `Reason` constants); `kueue/eligibility.go`
  (the per-namespace conditions, the managed check); `kueue/pending.go`
  (lender requests); `kueue/workloads.go` (the informer and
  `WorkloadOwnerUIDField`); `kueue/version.go` (`ControllerVersion`: version
  and liveness); `kueue/kueue_constants.go` (`TestedKueueVersions`).
- **Config:** `config/quota_limiter.go` (`ExternalCaps`, `BoundBy`, the
  `kueue:` block, `KueueBorrowSelected`).
- **The quota inventory:** `engines/allocation/quota_inventory.go` (pools,
  the borrowing log line). `ResourceConstraints` gains the borrow caps, the
  shared pools and a namespace-to-cohort map; the within-nominal copy drops all
  three. `mergeSharedPools` (`borrow_constraints.go`) ORs `Stale` and keeps the
  older `ReadAt`.
- **Counting and spending:** `engines/allocation/borrow_constraints.go`,
  `borrow_placement.go` (`placeUnit`, `placeReplica`), `borrow_node_fit.go`
  (`eligibleFit`, `templateAdmits`), `borrow_inflight.go`, `borrow_rescale.go`;
  the in-flight node ledger's borrowed shape in `engines/allocation/inflight.go`
  and `engines/steadystate/inflight.go` (`borrowedFlavors`).
- **Giving back:** `engines/allocation/borrow_governor.go` (`BorrowGovernor`,
  `namespaceBorrowed`, the ledger); `engines/allocation/borrow_lender_release.go`
  (the opt-in release to lenders Kueue does not reclaim for);
  `engines/steadystate/borrowing.go`, `borrowing_release.go` (the
  placement-timeout release, the persisted ledger, cluster-autoscaler events),
  `borrowing_lender_release.go` (the release latency), `preemption_watch.go`.
- **Reasons and signals:** `engines/allocation/borrow_report.go` (the
  counted-but-0 reasons, `LimiterBudgets`, `MarkPreemptionsUnreadable`);
  `engines/steadystate/borrowing_signals.go`.
- **Wakes:** `engines/scalefromzero/candidates.go` (the within-nominal check,
  `nominal-lent-out`) and `wake_tick.go`. A refused wake is reported as
  `wake-no-capacity` or `wake-nominal-lent-out` (`engine.go`,
  `reportWakeBlocked`), reasons the scale-from-zero loop alone owns
  (`constants.ScalingBlockedReasonOwners`).
- **Status:** `controllerstatus/limiter_budgets.go` (`limiterBudgets`, withheld
  and changed entries kept first under the cap) and `controllerstatus/publisher.go`
  (`borrowReleaseLedger`).
- **Installer:** `deploy/lib/cli.sh` (`LLMSM_QUOTA_KUEUE_BORROW`),
  `deploy/lib/limiter_policy.sh` (the entry it writes), `deploy/lib/prereqs.sh`;
  `config/components/kueue-reader`; `deploy/lib/physical_limiter.sh`
  (`pl_grant_kueue_borrow_read`, the cluster-wide Workload read, granted only
  when `policy_counts_borrowing` says a policy counts borrowing;
  `pl_grant_kueue_controller_read`, the Role in the Kueue namespace).

## Mechanics

**Reason precedence.** A namespace carries one reason: the first that applies
in the reference table's order, withheld before frozen before counted but 0.
A missing permission is the reason of the read it blocks, never "absent":
Workloads not readable cluster-wide is `lenders-unreadable`, an owner kind is
`not-kueue-managed`, the Deployment or Lease (never found or Forbidden) is
`kueue-controller-down`.

**Managed check.** A GPU pod is managed when a Workload with quota reserved
backs it, requests its GPUs, and is owned by Pod, Job, JobSet,
LeaderWorkerSet or StatefulSet. Workloads are looked up by owner UID through
the informer's field index. A namespace with no GPU pods keeps its previous
answer; on the first read it counts as not managed. Eligibility is evaluated
only for namespaces with managed variants, spread over the interval, and only
for the namespaces `borrowNamespaces` selects.

**Which lenders count.** `Any` reclaims for every borrower. `LowerPriority`
counts per borrowing model: the lender's capacity is counted for a model only
when the model's own Workload priority is strictly below the lender's, as
Kueue reclaims only for a Workload that outranks the borrower. `Never` does
not reclaim. Only reclaiming lenders' lent nominal enters the pool. When the pool
is 0 while non-reclaiming lenders hold unused nominal, the reason is
`lender-cannot-reclaim`. `reclaimablePools` takes a `release` flag
(`QuotaLimiterConfig.KueueReleasesToNonReclaiming`): set, every lender's
unused nominal enters the pool unrestricted, so `drawable` is `Free` and the
reason never shows.

**Co-resources.** For each resource the GPU flavor's resource group covers,
free capacity is computed with the same formula and divided by the variant's
per-replica request from its pod template; the reach in replicas is the
minimum. A request that cannot be determined withholds with
`co-resource-bound`.

**Reserved but not yet visible.** The engine, not the reader, takes from the
shared pool what was asked for that Kueue has not reserved: pods behind the
`kueue.x-k8s.io/admission` gate in every namespace of the cohort's
ClusterQueues (so a second controller's ask is seen), and borrowed growth not
yet visible as pods (`DeductUnreserved`). It stays deducted until the pods
exist or the desired count goes back down. That is the pool's half. The node's
half is Part A's in-flight ledger (`allocation.DefaultInFlight`): the
steady-state engine admits every target after the release step, and a
variant with a borrowed draw in flight is admitted with its flavor and its
templates' placement (`ReplicaShape.Flavor`, `Placements`), so its hole is held
on the flavor's nodes, and stays there when the draw resolves into a gated pod.
Neither half counts the other's: the ledger never debits a pool, the draw and
the gated deduction never debit a node, and the draw ends when the pods exist,
where the gated deduction starts (`TestAGatedBorrowedReplicaIsDebitedOnce`).

**Node fit.** Borrowing uses Part A's `NodeView`, obtained with `NodeViewFor`.
It exists only while the physical limiter bounds the cycle: declared, not
skipped, and with a node snapshot younger than `decision.NodeGPUsMaxAge` (2
minutes). It counts GPUs only, not CPU or memory. Part A already judges a
pod's tolerations against the node's taints, places best-fit and keeps an
exclusive-topology group in one domain. Borrowing adds two filters
(`eligibleFit`, cached per cycle like Part A's eligible list): the pod
templates' `nodeSelector` and required node affinity, and, for a borrowed
unit, the flavor's `nodeLabels`, with the flavor's tolerations added to the
template's. When a pass counts borrowing the placer (`modelPlacer`) owns node
fit for the whole model, guaranteed and borrowed: the pick sizes against the
pools only and the placer debits the view once per replica, charged to the
model like any other placement. A tentative unit that does not place is rolled
back with its owners (`saveDebits`, `restoreDebits`). A replica the pools held
and no node did is marked as node fit's cut (`markCut`), so the decision says
`node-fit`, not the pool with the least free. `placeUnit` places a P/D pair
only whole, both roles on borrowed capacity, never one role guaranteed and
its partner borrowed.

A reclaiming queue growing back into its own nominal is placed without a node
hole only for the GPUs peers actually borrow on its flavor and Kueue would
preempt for it: what a lender lends but nobody borrows is not reclaimable
capacity, and with a node down an exemption sized by unused nominal ordered a
pod that stayed Pending forever. The wake applies the same cap.

The rules on that exemption (`Reclaim`, `borrow_nominal.go`):

- **Whole replicas only, with nothing of `PreemptAfter` left**
  (`Reclaim.exempt`). Growth Kueue admits before it preempts needs a hole, so
  a 16-GPU replica that takes 8 of `PreemptAfter` first is fitted to a node
  whole.
- **The kept part never leaves the pool.** `PreemptKept`, the part of
  `PreemptAfter` a queue keeps unused (`max(0, G - U)`), is taken first and
  debits no cohort pool; only the rest of `PreemptAfter` does.
- **Recorded whole in `DefaultBorrowInFlight`.** A `Reclaim` entry carries
  `GPUs` (preempted for), `After` and `Kept`. `DeductReclaimed` lowers
  `LentOut`, `PreemptAfter` and `PreemptKept`; `DeductUnreservedAt` takes the
  pool part of `After` off the entry's chunk, for the wake and the next cycle.
- **A variant re-places its own entry.** `BorrowInFlight.Reclaims` goes to
  the optimizer as `ReclaimCredit`, so a replica whose pods are not created
  yet keeps its target. Without the credit it flapped 3/2/3.
- **The governor watches what skipped the hole.** An entry ordered without a
  hole is marked `NoHole`. The governor keeps it under `placementTimeout` like
  a borrowed replica (`BorrowCycle.NoHole`, `noHoleRelease`): if it never
  places it is released as `borrow-unplaced`, and the model backs off. While
  backed off, `ReclaimBlocked` makes its growth need a real hole. Kueue
  preempts by priority, not by node, so the preempted borrower may free no
  node the replica fits.

The restricted (LowerPriority) part of a chunk stays closed across cycles,
not just for one pass, while the chunk holds borrowing at a priority its
lender does not outrank. Two things count: a draw in flight marked
`ClosesRestricted`, and the read's `SharedPool.Borrowed` at
`BorrowerPriority`, the highest reserved-Workload priority of the borrowing
queues. A borrower whose priority was not read closes it.

The share planner plans after the pass on what it left
(`PassLeft.NodeDebits`): the borrowed placements are charged to their models
on the same view, so the idle fill does not place into a hole a borrowed
replica took, and a model the share plans itself has its placements taken
back. Rescale over the within-nominal copy places through Part A's
`fitRescaleToNodes` on the same view.
Scale-from-zero checks wakes in one tick against one snapshot and a per-tick
ledger (`decideWake`, the engine's `tickWakes` and `tickNodes`); across
ticks, a woken replica holds nothing until its pods are scheduled or
nominated. The warm pool reads whole units per node from
`decision.HeadroomStore.Units`.

**Placement clocks.** `VariantMetadata.UnplacedAt` is keyed by pod name, or
for a LeaderWorkerSet by group index, a group counting from its newest
unplaced pod. Unplaced means `PodScheduled=False` with reason `Unschedulable`,
or the Kueue gate held longer than a fixed 30 s. The timeout and the share
optimizer's `HoldFill` share one setting when `placementTimeout` is set;
unset, `HoldFill` keeps its derived wait. `VariantMetadata.GatedAt` is the
part of `UnplacedAt` held at the gate waiting for quota
(`variantmeta.countGated`): a gated pod whose Workload is `QuotaReserved`
(`kueue.ReservedPods`) waits on an admission check and is left out.

**Gated replicas outside borrowing.** A namespace-scope Kueue entry sets
`ResourceConstraints.KueueBacked` (and `KueueCountsBorrowing` when it is
configured to count). The engine lists the gated pods of every namespace such
an entry carries (`allocation.KueueNamespaces`), borrowing counted or not,
leaves out the reserved ones (`reservedGated`), and records the rest with
`BorrowInFlight.SetCycle`, so the KU1 charge (`admittedUsage`) sees them
everywhere. `allocation.UncountedKueueNamespaces` is the namespaces released
at the gate: an entry's that counts no borrowing, or a counting entry's whose
queue is in no cohort (`BorrowWithheld` `no-cohort`). A borrowing cycle
releases them in `timeoutReleases` (`gatedRelease` after `borrowedRelease`). A
cycle that counts no borrowing keeps `HoldReleases` and then calls
`BorrowGovernor.GatedReleases` (`governGated`, its own `gatedCycle`), which
touches no borrowing state and reads no Events. `gatedRelease` runs
`borrowedRelease` over `GatedAt` alone, clocked by `uncountedSince`, never
stretched, with cause `kueue-gate` and no backoff; its pins (`gatedPins`)
outlast `HoldReleases`' idle window while unlanded. Counted namespaces keep
`pastSince` and `UnplacedAt` as before
(`TestGatedReplicaOutsideBorrowingIsChargedAndReleased`).

**A wake's reclaim yields.** `BorrowInFlight.RecordWake` queues each reclaim
entry. A steady-state cycle that counts borrowing reads the queue
(`PendingWakeReclaims`: at most `InFlightTTL` old, and only while the record
still holds the wake's entry) and hands it to the pass as
`GreedyByScoreOptimizer.WakeReclaims`. `yieldReclaimed` counts those beside the
pass's own reclaims, so the rules are one
(`TestWakeReclaimLowersOwnBorrowerOnce`). After the pass, `recordBorrowDraws`
reports what the pass yielded for (`WakeReclaimsYielded`,
`YieldedWakeReclaims`), so a reclaim is yielded for once and a cycle without
a greedy pass loses none. The queue is in memory and does not survive a
restart.

**Cluster-autoscaler wait.** A `TriggeredScaleUp` Event pauses the clock only
when its `involvedObject.uid` matches the pod's UID; equal timestamps with a
later `NotTriggerScaleUp` read as not pending. Capped at three times the
timeout.

**Preemption.** Kueue sets the pod condition `TerminationTarget` (reason
`WorkloadEvictedDueToPreempted`) before it deletes the pod. On kind, the pod
and its Workload were gone 1.4 s later, inside one engine cycle, so polling
misses it. `PreemptionWatch` records the condition from the pod informer,
including the delete tombstone; the Workload informer covers the rest. The pod
is matched to a model by owner name prefix (`<deployment>-` or `<lws>-`),
longest wins. A buffer overflow withholds new borrowing for a cycle
(`preemptions-unreadable`).

**Lender signal.** A lender waits when a pending Workload in its ClusterQueue
requests a borrowed resource (GPU or covered co-resource) on a flavor its
`nodeSelector` and affinity allow (all flavors when that cannot be
determined), and fits within its unused nominal. `BorrowGovernor` closes the
cohort to new borrowing on the raw signal, and keeps it closed for
`borrowCooldown` after the wait ends. The debounce (two fresh reads and
`lenderDebounce`) gates only what is published:
`llmsm_kueue_lender_waiting_seconds` and its alert.

**Backoff.** `BorrowGovernor` keeps a cooldown per (model, cohort), started by
a preemption (`MarkPreempted`) or a placement-timeout release. A repeat while
backing off doubles it from `borrowCooldown`, up to four times
(`maxBackoffLevel`) and never past `maxBackoff` (40m) unless `borrowCooldown`
itself is longer (`backoffFor`); one clean cooldown resets it. The configured value is published on
`llmsm_kueue_borrow_cooldown_seconds`. The cooldowns and the releases in flight
persist under `borrowReleaseLedger`.

**Version and liveness.** `ControllerVersion` reads the
`kueue-controller-manager` Deployment's image and its leader Lease. Down: the
Lease's `renewTime` older than twice its `leaseDuration`, else
`readyReplicas` 0. Not found or Forbidden: down, so a missing Role in the
Kueue namespace reads as `kueue-controller-down`; so does a Kueue whose
leader-election ID is not the default (the Lease is read by name). A
Deployment read whose image carries no version withholds with
`kueue-version-unknown`, unless `kueue.assumeTestedVersion`, which lifts that
reason only. `llmsm_kueue_controller_version_known` is `Known ||
assumeTestedVersion`, published only while some entry counts borrowing, so an
opted-out cluster never pages `LLMSMKueueVersionUnknown`. The alert's `unless`
on `llmsm_quota_borrow_withheld{reason="kueue-controller-down"}`, per
`controller_instance`, keeps a Deployment that cannot be read (0 and down) to
one page, `LLMSMKueueControllerDown`.

**Never-read.** An entry with a static `-1` and no successful Kueue read
stays in the decision with its finite caps, and reports
`llmsm_kueue_never_read` (`ResourceConstraints.KueueNeverRead`). It is not a
skipped provider, so `llmsm_constraint_provider_skipped` stays clear and one
cause pages once. It is not fail-closed: the `-1` types are bounded by the
other limiters, and Kueue gates the pods.

**Two entries over one pool.** When two Kueue entries govern one cohort and
only one sets `borrowNamespaces`, the controller logs the warning
`borrow-namespaces-split` and rejects nothing.

**Scale.** Workloads come from an informer and ResourceQuotas from a cache; no
lock is held across I/O; Namespace labels come from one LIST per refresh; the
bounded log line is emitted only on change.

## Release to lenders Kueue does not reclaim for

Behind `kueue.releaseToNonReclaimingLenders`, off by default. The chunk
carries `SharedPoolChunk.ReleaseToNonReclaiming` (ANDed when two entries read
one cohort) and `RawFree`, Kueue's own free pool (the smaller of two reads).
`BorrowGovernor.Releases` runs `lenderReleases` after the placement-timeout
releases and their pins:

- **What the read gives.** `kueue.LenderState` carries, beside the raw
  `Waiting`: `PendingGPUs`, the smallest pending Workload Kueue could admit
  next (`lenderNext`: queue order by priority then `PendingWorkload.Since`,
  the `QueueResource.admits` fit rule shared with `lenderPendingOn`, none in
  `requeueState.requeueAt` backoff, none `preemptsWithin` would admit by
  preempting the queue's own `LowestReserved`, the head alone under
  `StrictFIFO`); `PendingPriority`, the highest priority among them;
  `PendingSince`; and `Kept`, `QueueResource.keptFree()`.
- **Trigger.** A lender in a fresh, opted-in chunk with `Waiting` set, whose
  policy Kueue does not reclaim under, with no `LenderRelease` in flight, and
  `releaseDue`: two fresh reads in its debounce watch, or `PendingSince` past
  `kueueSchedulingInterval`. `kueueReclaimsAll` leaves a `LowerPriority`
  lender above the chunk's `BorrowerPriority` to Kueue;
  `kueueFreeing` holds every release, reported `LenderPreempting`, while
  the chunk's `EvictingGPUs` is above 0: reserved GPUs, on the chunk's
  flavor, of Workloads `workloadVictim` reads as preempted (`Evicted=True`,
  reason `Preempted`, no `QuotaReserved=True` transition after it), evicted
  within `kueue.EvictionBound` (30m; older ones go to
  `StuckEvictingGPUs`); `kueueMayPreemptOutside` waits one
  `kueueSchedulingInterval` (`lenderPreemptWindow` when
  `EvictionsRead` is false) while the chunk's `BorrowerLowest` (unknown, or
  below the head) and a `Borrowed` above this controller's own say Kueue may
  preempt an outside borrower for it. Nothing while
  `BorrowCycle.LedgerUnread`.
- **Need.** `PendingGPUs - Kept - outranked`, then the pool's `poolBudget.free`
  (`budgetOf`: `RawFree`, plus `current - target` (not for a `yielded`
  decision; counted before KEDA acts, see the comment) and `held - current`
  of the cohort's decisions on the flavor, capped per namespace at what it
  borrows, plus landed, not taken, releases) is taken
  off, in lender order. `outranked` is what `lenderCandidates` finds Kueue
  will preempt: under `LowerPriority`, the models `PendingPriority` outranks,
  capped per namespace by `borrowedLeft`. `poolBudget.borrowed`, Kueue's
  `Borrowed`, bounds and is decremented by each release.
- **Candidates and order.** Models in a namespace of the cohort with GPUs past
  guaranteed on a type the chunk serves, no release of any cause in flight
  (`modelPinned`). Under `LowerPriority`, an outranked model and one whose
  priority is not known (`modelKueuePriority`) are not candidates. Sorted by
  Kueue priority (unread last), scaling priority, cost descending.
- **Units.** `yieldUnitWhere`, the reclaim yield's rule over the decisions'
  targets, a single-role replica only on a variant the chunk serves.
  `unitTakes` counts only `heldBelow` (replicas below `min(target, held)`)
  and refuses a unit past `borrowedLeft`. A unit passing the remaining need by
  more than its largest replica moves on to the next model (the reclaim
  yield's guard). `releaseTo` is all or nothing: short of the need it
  restores the decisions and `borrowedLeft` and returns nil; the lender is
  reported `LenderUnservable` in `LenderReport`. At most `maxLedgerUnits`
  variants per release.
- **In flight.** `LenderRelease` per `LenderKey`: units with their model and
  target, GPUs, `BorrowedBefore`, `ReleasedAt`, `LandedAt`, `Taken`,
  `TimedOut`. `observeLenderReleases` stamps `LandedAt` once every variant
  holds no more than `max(target, minReplicas)`, and starts the models'
  backoff in the cohort then. It clears the entry on a fresh read that shows
  the lender gone or not waiting, or `Borrowed <= BorrowedBefore - GPUs`. A
  landed entry with a read past `LandedAt + kueueSchedulingInterval` and
  neither is `Taken`: its units drop and it stays while the lender waits
  (`LenderTaken` in the report), at most twice `lenderTimeout`. Past `lenderTimeout(cohort)` (the larger of
  `ReleaseActuation` and twice the cohort's latency) it goes, except a P/D
  model half landed (`halfLanded`), kept until it lands or twice the
  timeout (then `LenderReport.HalfLowered`, a Warning
  `LenderReleaseHalfLanded` Event). A restored entry's unit whose variant is gone while its namespace
  is read drops. `applyLenderPins` keeps the targets, clamped to
  `minReplicas`, also in cycles that count no borrowing (`HoldReleases`,
  which drops a landed entry).
- **Ledger.** `BorrowLedger.LenderReleases`; `Version` is written only beside
  them, so the off path persists what it did before. `UnmarshalBorrowLedger`
  refuses a newer version (`ErrBorrowLedgerVersion`); the engine then holds
  the ledger unread (`ledgerNewerReason`) and does not persist over it.
  `restoreLenderReleases` drops entries out of bounds (`maxLedgerUnits`,
  `maxLedgerGPUs`) and duplicates. The publisher carries the key over to a
  new term.
- **Latency.** `steadystate.releaseLatencies`: per cohort of
  `allocation.LenderReleaseScope`, the slowest `allocation.ReleaseBound` over
  the variants a release there may lower, capped at `MaxReleaseLatency`. It
  sets `BorrowSettings.ReleaseLatencies` and
  `llmsm_kueue_borrow_release_latency_seconds{cohort}`.
- **Signals.** `VariantBorrowStatus.LenderRelease` (`released-to-lender`),
  cause `lender-waiting` on the counter (created for the scope's models) and
  the Event. `reportLenders` turns `LenderReport` into
  `llmsm_kueue_lender_release_blocked` and, once per change, a log line and a
  Warning `LenderReleaseBlocked` Event; `LenderReport.Shared` into a warning
  that another borrower, possibly a second controller, holds part of Kueue's
  borrowed figure.

Tests: `kueue/lender_pending_test.go`, `reader_lender_test.go`;
`engines/allocation/borrow_release_count_test.go`,
`borrow_lender_release_test.go`; `engines/steadystate/borrowing_lender_release_test.go`,
`borrowing_lender_signals_test.go`; promtool cases 6, 6b and 7 in
`test/alerts/testdata/kueue-alerts.test.yaml`.

## Testing

- **Unit**: `make test`. The envtest suites run under WSL; see
  [Testing](testing.md).
  - `internal/config/quota_limiter_kueue_test.go`: every merge rule, untyped
    grants, the per-flavor split, the reserved key and exclusions.
  - `internal/kueue/`: `reader_test.go` (attribution, inactive queues, both
    served API versions, the refresh cache, a caller's cancelled context),
    and the `borrow_`, `eligibility_`, `lender_pending_`, `reader_*` and
    `workloads_` tests.
  - `internal/engines/allocation/`: `quota_inventory_source_test.go` (the
    effective entry reaching the optimizer's constraints, stale snapshots,
    one shared reader), `quota_inventory_borrow*_test.go`, and the
    `borrow_*_test.go` files (conditions, node fit, giving back).
  - `internal/engines/steadystate/`: `borrowing_*_test.go`,
    `preemption_*_test.go`.
  - `internal/engines/scalefromzero/borrow_wake_test.go`: the within-nominal
    wake.
  - `internal/accelerator/identity_test.go`: how a full product label meets
    a short static key.
  - `hack/check-limiter-declaration.sh` pins what `LLMSM_QUOTA_KUEUE` and
    `LLMSM_QUOTA_KUEUE_BORROW` emit.
- **End to end, Kueue's CRDs only**: `test/e2e/kueue_quota_test.go` (labels
  `full`, `kueue-quota`) on kind. Kueue grants one GPU against a static cap of
  three; the effective entry reads one, then three again after the LocalQueue
  is deleted. It runs with Kueue's CRDs only
  (`deploy/ci-pr-checks/install-kueue-crds.sh`, wired into
  `test-e2e-full-with-setup` on kind). It fails, not skips, when they are
  absent there, and skips with a reason on a cluster that does not serve them.
- **End to end, live Kueue**: `test/e2e/kueue_borrow_test.go` (label
  `kueue-borrow`). The live e2e runs in CI job `e2e-tests-kueue-borrow`.
  The model holds at its guaranteed figure, counted at 0 as
  `lender-cannot-reclaim`, while its only lender is on `Never`; with the
  lender on `Any`, borrowing is withheld while an unmanaged GPU pod runs, and
  the model is not ordered past its nominal while the Kueue controller is
  down -- each hold the only one in force; once Kueue is back it grows into
  the cohort with every pod placed; after Kueue preempts the borrowed pods the
  model drops back, borrows
  nothing during the cooldown and grows again after it. Last, with the lender
  back on `Never` and waiting, nothing is handed back until
  `releaseToNonReclaimingLenders` is set; then the model drops to its
  guaranteed figure and Kueue admits the lender.
  `deploy/ci-pr-checks/install-kueue.sh` installs Kueue from `registry.k8s.io`
  and refuses a non-kind context unless `KUEUE_ALLOW_NON_KIND=true`.
  `REQUIRE_KUEUE_E2E=true` turns its runtime skips into failures; keep
  `E2E_TIMEOUT` below the job timeout. How to run it:
  [Kueue specs](testing.md#kueue-specs).
- **Not covered end to end**: inactive queues, finalizers and `stopPolicy`
  transitions (imitated by fixtures), and enforcement past the effective entry.
- **Kueue versions.** `TestedKueueVersions` in `kueue/kueue_constants.go`
  accepts v0.19 and v0.20; `e2e-tests-kueue-borrow` runs v0.20.0 only. Adding one means
  re-copying the constants in `kueue/kueue_constants.go` and running the e2e
  against it.
