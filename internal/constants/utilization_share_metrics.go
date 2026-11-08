package constants

// Utilization-share optimizer metrics (docs/proposals/utilization-share-optimizer.md,
// section 9). Role-keyed series carry namespace, model_name and role; group-keyed
// series carry accelerator_type and scope (a namespace for a namespace-quota
// group, "cluster" for the cluster group). Every gauge series is replaced each
// cycle, so a model or group that leaves takes its gauges with it; counters and
// histograms accumulate for the life of the process.
const (
	// LLMSMUtilizationShareHeadroom is a gauge: x_r, the traffic spike a role
	// absorbs before it must scale, as a fraction of its need (negative when the
	// role is short). Absent for a role with no demand.
	LLMSMUtilizationShareHeadroom = "llmsm_utilization_share_headroom"

	// LLMSMUtilizationShareTargetGPUs is a gauge: Ĝ_r, the continuous target the
	// tolerance band is judged against, in GPUs.
	LLMSMUtilizationShareTargetGPUs = "llmsm_utilization_share_target_gpus"

	// LLMSMUtilizationShareActionable is a gauge: 1 when a role is out of band and
	// off its integer target -- a move could fix it -- else 0.
	LLMSMUtilizationShareActionable = "llmsm_utilization_share_actionable"

	// LLMSMUtilizationShareSpareGPUs is a gauge: S, the group's budget minus every
	// role's claim. Negative when the quota is short.
	LLMSMUtilizationShareSpareGPUs = "llmsm_utilization_share_spare_gpus"

	// LLMSMUtilizationShareReplicasToMove is a gauge: the replicas the integer
	// target would move. In shadow mode this is what would be planned.
	LLMSMUtilizationShareReplicasToMove = "llmsm_utilization_share_replicas_to_move"

	// LLMSMUtilizationShareTransfersTotal is a counter: transfers that left the
	// ledger, by outcome (done, fill-timeout, cancelled, aborted, redirected,
	// wrong-pod).
	LLMSMUtilizationShareTransfersTotal = "llmsm_utilization_share_transfers_total"

	// LLMSMUtilizationSharePromisedGPUs is a gauge: P, GPUs released for a
	// receiver and not yet held by it.
	LLMSMUtilizationSharePromisedGPUs = "llmsm_utilization_share_promised_gpus"

	// LLMSMUtilizationShareEffectiveSeconds is a gauge: a derived timing in force,
	// by param and the source of its inputs (section 8.4).
	LLMSMUtilizationShareEffectiveSeconds = "llmsm_utilization_share_effective_seconds"

	// LLMSMUtilizationShareSwinging is a gauge: 1 while a role is planned on its
	// mean need (section 6.7 rule 5).
	LLMSMUtilizationShareSwinging = "llmsm_utilization_share_swinging"

	// LLMSMUtilizationShareReleaseSeconds is a histogram: transfer start to the
	// donor's GPUs released.
	LLMSMUtilizationShareReleaseSeconds = "llmsm_utilization_share_release_seconds"

	// LLMSMUtilizationShareReserveDebtGPUs is a gauge: reserve GPUs spent and
	// not yet refilled (section 6.2).
	LLMSMUtilizationShareReserveDebtGPUs = "llmsm_utilization_share_reserve_debt_gpus"

	// LLMSMUtilizationShareWithheldTotal is a counter: transfers not planned, by
	// reason (reversal-hold, not-actionable, return-not-calm, return-pod-busy,
	// return-role-short, return-receiver-unplaceable, donor-peak,
	// awaiting-return, scale-down-band).
	LLMSMUtilizationShareWithheldTotal = "llmsm_utilization_share_withheld_total"

	// LLMSMUtilizationShareActual is a gauge: u_r, a role's utilization at the
	// GPUs it holds, on the same scale as its scale-up threshold.
	LLMSMUtilizationShareActual = "llmsm_utilization_share_actual"

	// LLMSMUtilizationShareFloorExcessGPUs is a gauge: the GPUs a role's floor
	// holds above its need (section 5.5).
	LLMSMUtilizationShareFloorExcessGPUs = "llmsm_utilization_share_floor_excess_gpus"

	// LLMSMUtilizationShareClaimsTotal is a counter: wake claims on releasing
	// transfers, by outcome (redirected, refused-score, refused-fit,
	// refused-held, none-releasing).
	LLMSMUtilizationShareClaimsTotal = "llmsm_utilization_share_claims_total"

	// LLMSMUtilizationShareDonorsPerTransfer is a histogram: the donor replicas
	// that fund one receiver replica (section 6.5).
	LLMSMUtilizationShareDonorsPerTransfer = "llmsm_utilization_share_donors_per_transfer"

	// LLMSMUtilizationShareStartsTotal is a counter: what the optimizer started,
	// by kind (transfer, fill, refill, return) and skipped_waits.
	LLMSMUtilizationShareStartsTotal = "llmsm_utilization_share_starts_total"

	// LLMSMUtilizationShareMode is a gauge: 1 for the mode the optimizer is in
	// -- off, invalid (the optimizer block did not validate), shadow or active
	// -- and 0 for the others.
	LLMSMUtilizationShareMode = "llmsm_utilization_share_mode"

	// LLMSMUtilizationShareInFlight is a gauge: a group's transfers in flight, by
	// state (releasing, filling). Published only while the optimizer acts.
	LLMSMUtilizationShareInFlight = "llmsm_utilization_share_in_flight"

	// LLMSMUtilizationShareHeadroomCapped is a gauge: 1 while a group's quota is
	// contended and each role's headroom is capped (maxHeadroomReplicas), 0
	// while the spare is spread. Published only while the optimizer acts.
	LLMSMUtilizationShareHeadroomCapped = "llmsm_utilization_share_headroom_capped"

	// LLMSMUtilizationSharePassiveShadow is a gauge: 1 while a group is planned
	// in shadow because it holds a passive model, under an optimizer that
	// otherwise acts; 0 for every other group the optimizer plans.
	LLMSMUtilizationSharePassiveShadow = "llmsm_utilization_share_passive_shadow"

	// LabelParam names a derived timing; LabelSource is where its inputs came
	// from.
	LabelParam  = "param"
	LabelSource = "source"

	// LabelMode is the optimizer mode of LLMSMUtilizationShareMode; LabelState
	// is a transfer state of LLMSMUtilizationShareInFlight.
	LabelMode  = "mode"
	LabelState = "state"

	// LabelUrgent is "true" on a transfer whose receiver was below its need.
	LabelUrgent = "urgent"

	// LabelKind is what LLMSMUtilizationShareStartsTotal counts: one of
	// UtilizationShareKinds. LabelSkippedWaits is "true" on a move that
	// skipped the stabilization waits (a hard imbalance).
	LabelKind         = "kind"
	LabelSkippedWaits = "skipped_waits"

	// LabelScope is the budget scope of a utilization-share group.
	LabelScope = "scope"

	// UtilizationShareClusterScope is the scope label of the cluster group.
	UtilizationShareClusterScope = "cluster"
)

// Values of LabelMode on LLMSMUtilizationShareMode.
const (
	// UtilizationShareModeOff is a scaling policy that selects no optimizer.
	UtilizationShareModeOff = "off"
	// UtilizationShareModeInvalid is an optimizer block that did not validate:
	// today's optimizer runs instead.
	UtilizationShareModeInvalid = "invalid"
	// UtilizationShareModeShadow is the optimizer computing and reporting,
	// actuating nothing.
	UtilizationShareModeShadow = "shadow"
	// UtilizationShareModeActive is the optimizer moving GPUs.
	UtilizationShareModeActive = "active"
)

// UtilizationShareModes are every LabelMode value, in the order published.
var UtilizationShareModes = []string{UtilizationShareModeOff, UtilizationShareModeInvalid,
	UtilizationShareModeShadow, UtilizationShareModeActive}

// Values of LabelState on LLMSMUtilizationShareInFlight.
const (
	// UtilizationShareStateReleasing is a transfer whose donor has not
	// released its GPUs yet.
	UtilizationShareStateReleasing = "releasing"
	// UtilizationShareStateFilling is a transfer whose receiver has not taken
	// the released GPUs yet.
	UtilizationShareStateFilling = "filling"
)

// Values of LabelOutcome on LLMSMUtilizationShareTransfersTotal: how a transfer
// left the ledger. The allocation package's ShareOutcome values are these.
const (
	// UtilizationShareOutcomeDone is a transfer whose receiver holds the GPUs,
	// or an idle fill that landed.
	UtilizationShareOutcomeDone = "done"
	// UtilizationShareOutcomeFillTimeout is a transfer whose receiver did not
	// take the released GPUs within the fill timeout.
	UtilizationShareOutcomeFillTimeout = "fill-timeout"
	// UtilizationShareOutcomeCancelled is a release called off while it was
	// still free.
	UtilizationShareOutcomeCancelled = "cancelled"
	// UtilizationShareOutcomeAborted is a release that did not land within the
	// release timeout, or a donor set that could not complete.
	UtilizationShareOutcomeAborted = "aborted"
	// UtilizationShareOutcomeRedirected is a transfer a wake claimed.
	UtilizationShareOutcomeRedirected = "redirected"
	// UtilizationShareOutcomeWrongPod is a node-planned transfer whose donor
	// shrank by a different pod than the one marked.
	UtilizationShareOutcomeWrongPod = "wrong-pod"
)

// UtilizationShareOutcomes are every LabelOutcome value. Each is published at
// 0 for an acting group, so the first transfer of an outcome is an increase()
// and not a series appearing at 1.
var UtilizationShareOutcomes = []string{
	UtilizationShareOutcomeDone, UtilizationShareOutcomeFillTimeout, UtilizationShareOutcomeCancelled,
	UtilizationShareOutcomeAborted, UtilizationShareOutcomeRedirected, UtilizationShareOutcomeWrongPod,
}

// Values of LabelKind on LLMSMUtilizationShareStartsTotal.
const (
	// UtilizationShareKindTransfer is a donor replica moved to a receiver (a
	// donor set counts once).
	UtilizationShareKindTransfer = "transfer"
	// UtilizationShareKindFill is a receiver raised into unassigned GPUs.
	UtilizationShareKindFill = "fill"
	// UtilizationShareKindRefill is a donor lowered to pay back the reserve.
	UtilizationShareKindRefill = "refill"
	// UtilizationShareKindReturn is a donor's headroom above its target given
	// back to the quota.
	UtilizationShareKindReturn = "return"
)

// UtilizationShareKinds are every LabelKind value, published at 0 for an
// acting group.
var UtilizationShareKinds = []string{
	UtilizationShareKindTransfer, UtilizationShareKindFill, UtilizationShareKindRefill, UtilizationShareKindReturn,
}

// Values of LabelParam on LLMSMUtilizationShareEffectiveSeconds: the derived
// timings in force.
const (
	// UtilizationShareParamWindow is the donors' scale-down window.
	UtilizationShareParamWindow = "window"
	// UtilizationShareParamReleaseTimeout bounds a release.
	UtilizationShareParamReleaseTimeout = "release-timeout"
	// UtilizationShareParamFillTimeout bounds a fill.
	UtilizationShareParamFillTimeout = "fill-timeout"
	// UtilizationShareParamReversalHold is how long a role that gave may not
	// receive, and one that received may not give.
	UtilizationShareParamReversalHold = "reversal-hold"
	// UtilizationShareParamSwingWindow is the window two reversals must fall
	// in for a role to be planned on its mean need.
	UtilizationShareParamSwingWindow = "swing-window"
)
