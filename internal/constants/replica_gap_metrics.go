package constants

// What the analyzers asked for against what the controller provides (issue
// #439). Per variant: X, the suggestion, and Y, what is provided, and why they
// differ. A variant with no decision this cycle has none of these series; every
// gauge is replaced each cycle, so a variant, reason or quota that goes -- or a
// variant whose mode changes -- takes its series with it.
const (
	// LLMSMReplicasSuggested is a gauge: X, the replicas the analyzers ask for
	// before any limiter, the variant's maxReplicas ceiling or the utilization
	// share. Equal to LLMSMReplicasProvided unless one of those changed the
	// target.
	// Labels: namespace, model_name, variant_name, role, mode (controller_instance when set)
	LLMSMReplicasSuggested = "llmsm_replicas_suggested"

	// LLMSMReplicasProvided is a gauge: Y, the replicas the variant gets after
	// the limiters and the utilization share. With mode active it is the value
	// of llmsm_desired_replicas, the target the controller sets. With mode
	// passive or shadow nothing is applied and it is a would-be figure (see
	// ReplicasMode*).
	// Labels: namespace, model_name, variant_name, role, mode (controller_instance when set)
	LLMSMReplicasProvided = "llmsm_replicas_provided"

	// LLMSMReplicasShort is a gauge: the part of X minus Y one reason accounts
	// for (ReplicasShortReason*). limiter_name names the quota entry, or the
	// Kueue borrowing rule, for reason quota, as llmsm_decisions_limited_total
	// does, and is empty for every other reason. Present only while positive.
	// Labels: namespace, model_name, variant_name, role, mode, reason, limiter_name (controller_instance when set)
	LLMSMReplicasShort = "llmsm_replicas_short"

	// LLMSMReplicasBorrowed is a gauge: Y minus X when the utilization share
	// provides more than the analyzers ask for, 0 otherwise. Not Kueue cohort
	// borrowing.
	// Labels: namespace, model_name, variant_name, role, mode (controller_instance when set)
	LLMSMReplicasBorrowed = "llmsm_replicas_borrowed"

	// LLMSMQuotaRemainingAfterSuggestedGPUs is a gauge: what a quota's pool
	// would have left if every variant it bounds were at X: its limit, minus
	// what is held now, minus the GPUs X adds (or plus those it frees).
	// Negative when the suggestions do not fit. scope is a namespace for a
	// namespace-scoped quota, "cluster" otherwise. An unlimited pool has no
	// series.
	// Labels: limiter_name, scope, accelerator (controller_instance when set),
	// as llmsm_quota_limit_gpus.
	LLMSMQuotaRemainingAfterSuggestedGPUs = "llmsm_quota_remaining_after_suggested_gpus"
)

// Values of LabelMode on the suggested/provided series: whether Y is applied.
const (
	// ReplicasModeActive: Y is applied, the value of llmsm_desired_replicas.
	ReplicasModeActive = "active"
	// ReplicasModePassive: the model is passive, so nothing is applied. Y is
	// what the controller would provide were it active: its would-be target
	// (llmsm_desired_replicas) fitted to the nodes, and moved as a share
	// planning its group would move it.
	ReplicasModePassive = "passive"
	// ReplicasModeShadow: a utilization share planning in shadow (shadow mode,
	// or a group holding a passive model) would set the variant's target, so
	// nothing it plans is applied. Y is the target the share would set:
	// llmsm_desired_replicas moved as the share would move it, to where its
	// transfers would end.
	ReplicasModeShadow = "shadow"
)

// Reasons of LLMSMReplicasShort.
const (
	// ReplicasShortReasonQuota: a quota entry, or a Kueue borrowing rule, held
	// the variant below X.
	ReplicasShortReasonQuota = "quota"
	// ReplicasShortReasonGPU: the physical GPU inventory held it.
	ReplicasShortReasonGPU = "gpu"
	// ReplicasShortReasonNodeFit: the pools held the GPUs but no node held a
	// whole replica.
	ReplicasShortReasonNodeFit = "node-fit"
	// ReplicasShortReasonMaxReplicas: the variant's own maxReplicas ceiling.
	ReplicasShortReasonMaxReplicas = "max-replicas"
	// ReplicasShortReasonShareLent: the utilization share lowered the variant
	// to give its GPUs to another model.
	ReplicasShortReasonShareLent = "share-lent"
)
