package constants

// The SLO analyzer's metrics. See docs/proposals/slo-tiers.md, "Observability".
const (
	// LLMSMSLOTargetSeconds is the latency target a model's policy declares.
	// Labels: namespace, model_name, objective, statistic.
	LLMSMSLOTargetSeconds = "llmsm_slo_target_seconds"
	// LLMSMSLOObservedSeconds is the latency the router observed over its window:
	// the mean for TPOT, the configured percentile for TTFT. Absent when the
	// router exported too few requests to judge.
	// Labels: namespace, model_name, objective, statistic.
	LLMSMSLOObservedSeconds = "llmsm_slo_observed_seconds"
	// LLMSMSLOAttainmentRatio is the share of requests whose TTFT met the target.
	// Absent below the sample minimum, never 0 or 1 by default.
	// Labels: namespace, model_name, objective, statistic.
	LLMSMSLOAttainmentRatio = "llmsm_slo_attainment_ratio"
	// LLMSMSLOObjectiveRatio is the share of requests a TTFT objective promises
	// within its target: 0.90 for p90. The attainment alert compares the
	// request-weighted attainment with it, since the percentile is otherwise
	// only in the statistic label's text. Published for TTFT only.
	// Labels: namespace, model_name, objective, statistic.
	LLMSMSLOObjectiveRatio = "llmsm_slo_objective_ratio"
	// LLMSMSLOFill is the KV fill, as a fraction of physical KV, a variant's SLO
	// capacity is priced at: where its mean inter-token latency meets the TPOT
	// target, clamped to the policy's ITL band ([0.15, 0.80] by default) and lowered where max_num_seqs binds
	// first, so the line's ITL there is at or below the target. Before any
	// scale-up headroom. Absent when the variant could not be priced.
	// Labels: namespace, model_name, variant_name, objective.
	LLMSMSLOFill = "llmsm_slo_fill"
	// LLMSMSLOPredictedITLSeconds is the mean inter-token latency the variant's
	// line predicts at the fill the fleet would actually run at once the
	// scale-up headroom is applied: the margin an operator really has.
	// Labels: namespace, model_name, variant_name.
	LLMSMSLOPredictedITLSeconds = "llmsm_slo_predicted_itl_seconds"
	// LLMSMSLOSource is 1 for the source a variant's SLO capacity came from and 0
	// for every other, like llmsm_replica_capacity_source.
	// Labels: namespace, model_name, variant_name, source.
	LLMSMSLOSource = "llmsm_slo_source"

	// LLMSMSLOWindowRequests is how many requests the router recorded for the
	// objective over its window. Present whenever the router reported anything,
	// so an absent llmsm_slo_observed_seconds beside it means "too few requests to
	// judge", and both absent means the router reported nothing.
	// Labels: namespace, model_name, objective, statistic.
	LLMSMSLOWindowRequests = "llmsm_slo_window_requests"

	// LLMSMSLOExcluded is 1 for the reason a model's router window is not being
	// judged this cycle and 0 for the others; all 0 while it is judged. A
	// window is excluded while it covers a wake from zero or a replica
	// starting, and for a grace period after, because those requests waited out
	// capacity no steady-state sizing could have had ready; and after cycles the
	// analyzer did not see (a skipped model, a controller restart). Per
	// objective: a prefill replica starting excludes TTFT and not TPOT.
	// Labels: namespace, model_name, objective, reason.
	LLMSMSLOExcluded = "llmsm_slo_excluded"

	// LLMSMSLOPredictedTTFTSeconds is the TTFT the model predicts for a prefill
	// variant at its current rate, at the objective's percentile. A shadow
	// figure, not a bound: see SLOTTFTPredictionCaveat for which way it errs.
	// Published with the reasons priced, decode-bound and routing-bound; absent
	// otherwise, and absent when the model refuses the rate.
	// Labels: namespace, model_name, variant_name, statistic.
	LLMSMSLOPredictedTTFTSeconds = "llmsm_slo_predicted_ttft_seconds"
	// LLMSMSLOTTFTUnmet is 1 when a prefill variant's current rate misses the
	// TTFT target (the prediction is above it, or the model refused the rate
	// at or over the prefill knee or the decode onset, or the rate is above
	// the prefill flow-control onset, or the target is below the floor), 0 when
	// met, and absent when not priced. Absent too for a target below a floor
	// only an assumed decode term puts over it: that says nothing of the fleet.
	// Labels: namespace, model_name, variant_name, statistic.
	LLMSMSLOTTFTUnmet = "llmsm_slo_ttft_unmet"
	// LLMSMSLOTTFTReplicaRequestsPerSecond is the request rate per replica at
	// which the bounded TTFT meets the target, at the current replica count.
	// Absent unless llmsm_slo_ttft_reason reads priced.
	// Labels: namespace, model_name, variant_name, statistic.
	LLMSMSLOTTFTReplicaRequestsPerSecond = "llmsm_slo_ttft_replica_requests_per_second"
	// LLMSMSLOTTFTVPSource is 1 for where a prefill variant's capacity V_P came
	// from and 0 for the others (SLOTTFTVPSources).
	// Labels: namespace, model_name, variant_name, statistic, vp_source.
	LLMSMSLOTTFTVPSource = "llmsm_slo_ttft_vp_source"
	// LLMSMSLOTTFTReason is 1 for the reason of a prefill variant's TTFT pricing
	// and 0 for the others (SLOTTFTReasons).
	// Labels: namespace, model_name, variant_name, statistic, reason.
	LLMSMSLOTTFTReason = "llmsm_slo_ttft_reason"
	// LLMSMSLOTTFTRefused is 1 for why the model refused a prefill variant's
	// current rate and 0 for the other (SLOTTFTRefusedReasons): the rate is past
	// what the fleet serves. llmsm_slo_ttft_reason is the answer at the target,
	// and stays priced beside it.
	// Labels: namespace, model_name, variant_name, statistic, reason.
	LLMSMSLOTTFTRefused = "llmsm_slo_ttft_refused"
	// LLMSMSLOTTFTFlag is 1 for each flag on a prefill variant's TTFT answer and
	// 0 for the others (SLOTTFTFlags).
	// Labels: namespace, model_name, variant_name, statistic, flag.
	LLMSMSLOTTFTFlag = "llmsm_slo_ttft_flag"

	// LabelObjective names an SLO objective: "ttft" or "tpot".
	LabelObjective = "objective"
	// LabelStatistic names the statistic an objective is judged on: "mean", or
	// "p" followed by the percentile ("p90").
	LabelStatistic = "statistic"
	// LabelVPSource names where a prefill variant's V_P came from.
	LabelVPSource = "vp_source"
	// LabelFlag names a flag on a TTFT answer.
	LabelFlag = "flag"
)

// SLO objective label values.
const (
	SLOObjectiveTTFT = "ttft"
	SLOObjectiveTPOT = "tpot"
)

// SLO source label values: where a variant's SLO capacity came from, or why
// there is none.
const (
	// SLOSourceOwn: the variant's own ordinary least-squares ITL fit.
	SLOSourceOwn = "own"
	// SLOSourcePinned: a fit with the intercept pinned to this variant's last
	// full fit, because the fleet's readings did not span enough of k to fit
	// both coefficients, or the full fit was not a valid line. A balanced fleet
	// often sits here indefinitely.
	SLOSourcePinned = "pinned"
	// SLOSourcePinnedDefault: a pinned fit whose intercept is the built-in
	// floor, not one this variant was ever measured at. Not a measurement of
	// the hardware: on a large accelerator the real intercept is several times
	// the floor, and the slope fitted against it is too steep.
	SLOSourcePinnedDefault = "pinned-default"
	// SLOSourceNoOutputLength: no replica has reported an output length, so
	// neither the demand nor the per-request KV footprint is known.
	SLOSourceNoOutputLength = "no-output-length"
	// SLOSourceNoLine: too few readings in the window to fit any line, and no
	// earlier slope to hold: a new variant warming up, replicas that report no
	// ITL, or replicas outside the band -- below a fill of 0.15 (an idle fleet)
	// or above 0.80 (an overloaded one), which can last indefinitely. A variant
	// that has fitted holds its last slope there instead and reads pinned or
	// pinned-default, within the slope hold and under unchanged engine
	// arguments (itl.Admission). The slo-variant log line counts which.
	SLOSourceNoLine = "no-line"
	// SLOSourceDeclined: the window held enough readings and the pinned fit
	// refused them, most often because the variant's ITL sits below the pinned
	// intercept (a fast card under the built-in floor) and the slope came out
	// negative. A steady state, not a warm-up.
	SLOSourceDeclined = "declined"
	// SLOSourceInfeasible: no fill in the line's range meets the target.
	SLOSourceInfeasible = "infeasible"
	// SLOSourceInputInvalid: an input was missing, non-finite or non-positive.
	SLOSourceInputInvalid = "input-invalid"
)

// Reasons a model's router window is excluded from judgement.
const (
	SLOExcludedZeroReplicas = "zero-replicas"
	SLOExcludedStarting     = "starting"
	// SLOExcludedUnobserved: the analyzer did not see the model's previous
	// cycles -- it was skipped, as a waking model whose Pods have no metrics
	// yet is, or the controller restarted -- so the window may hold a
	// transient nothing here saw.
	SLOExcludedUnobserved = "unobserved"
)

// SLOExcludedReasons is every exclusion reason, in the order they are published.
var SLOExcludedReasons = []string{SLOExcludedZeroReplicas, SLOExcludedStarting, SLOExcludedUnobserved}

// Where a prefill variant's V_P came from: P1's sources, or none.
const (
	SLOTTFTVPBusyMinutes       = "busy-minutes"
	SLOTTFTVPPeakUnderQueue    = "peak-under-queue"
	SLOTTFTVPBackPressureBound = "back-pressure-bound"
	SLOTTFTVPNone              = "none"
)

// SLOTTFTVPSources is every V_P source value, in the order they are published.
var SLOTTFTVPSources = []string{SLOTTFTVPBusyMinutes, SLOTTFTVPPeakUnderQueue, SLOTTFTVPBackPressureBound, SLOTTFTVPNone}

// The reasons of a prefill variant's TTFT pricing.
const (
	// SLOTTFTReasonPriced: the per-replica rate is published.
	SLOTTFTReasonPriced = "priced"
	// SLOTTFTReasonNoEstimate: no P1 estimate from this cycle.
	SLOTTFTReasonNoEstimate = "no-estimate"
	// SLOTTFTReasonNoDecodeSide: flow control is on and no decode line is
	// priced, so the decode onset cannot be placed.
	SLOTTFTReasonNoDecodeSide = "no-decode-side"
	// SLOTTFTReasonDecodeBound: decode sets the rate, so more prefill replicas
	// cannot raise it.
	SLOTTFTReasonDecodeBound = "decode-bound"
	// SLOTTFTReasonRoutingBound: one pod takes at least 80% of the work, so
	// the fix is the router's scoring, not replicas.
	SLOTTFTReasonRoutingBound = "routing-bound"
	// The model's refusals, in kebab case.
	SLOTTFTReasonNoPrefillCapacity = "no-prefill-capacity"
	SLOTTFTReasonNoFixedLatency    = "no-fixed-latency"
	SLOTTFTReasonNoImbalance       = "no-imbalance"
	SLOTTFTReasonNoPrefillReplicas = "no-prefill-replicas"
	SLOTTFTReasonNoDecodeReplicas  = "no-decode-replicas"
	SLOTTFTReasonNoCapacity        = "no-capacity"
	SLOTTFTReasonNoBatchCap        = "no-batch-cap"
	SLOTTFTReasonAboveDecodeOnset  = "above-decode-onset"
	SLOTTFTReasonSaturated         = "saturated"
	SLOTTFTReasonTargetBelowFloor  = "target-below-floor"
	SLOTTFTReasonNoTraffic         = "no-traffic"
	SLOTTFTReasonTooManySlots      = "too-many-slots"
	SLOTTFTReasonInputInvalid      = "input-invalid"
)

// SLOTTFTReasons is every TTFT reason value, in the order they are published.
var SLOTTFTReasons = []string{
	SLOTTFTReasonPriced, SLOTTFTReasonNoEstimate, SLOTTFTReasonNoDecodeSide,
	SLOTTFTReasonDecodeBound, SLOTTFTReasonRoutingBound,
	SLOTTFTReasonNoPrefillCapacity, SLOTTFTReasonNoFixedLatency, SLOTTFTReasonNoImbalance,
	SLOTTFTReasonNoPrefillReplicas, SLOTTFTReasonNoDecodeReplicas, SLOTTFTReasonNoCapacity,
	SLOTTFTReasonNoBatchCap, SLOTTFTReasonAboveDecodeOnset, SLOTTFTReasonSaturated,
	SLOTTFTReasonTargetBelowFloor, SLOTTFTReasonNoTraffic, SLOTTFTReasonTooManySlots,
	SLOTTFTReasonInputInvalid,
}

// SLOTTFTRefusedReasons is why the model refuses a current rate as past what the
// fleet serves, in the order they are published.
var SLOTTFTRefusedReasons = []string{SLOTTFTReasonSaturated, SLOTTFTReasonAboveDecodeOnset}

// SLOTTFTPredictionCaveat is which way the TTFT prediction errs, on the
// prefill-stress recordings' 14 light-load points, 7 each at p90 and p95. A
// test holds its figures to the frozen validation table.
const SLOTTFTPredictionCaveat = "Not a bound, and which way it errs depends on the prompt spread. At the " +
	"assumed cv 2 (flag AssumedPromptSpread) it reads high: over the router at 13 of 14 points, worst " +
	"3.15x at p90 and 4.13x at p95. At the spread actually sent it reads low: under at 9 of 14, worst 34% under."

// SLOTTFTFlags is every flag the flag series zero-fills, in the order they are
// published: the TTFT model's, the decode flags a TTFT answer can carry, and
// the SLO analyzer's own. The names are whatif.Flag values, spelled here
// because this package imports nothing; a test in the slo package holds them
// to whatif's. AssumedVP and DecodeOnsetUnchecked always read 0: the analyzer
// assumes no V_P and publishes no unchecked answer.
var SLOTTFTFlags = []string{
	"UnvalidatedTTFT", "AssumedVP", "VPLowerBound", "VPAboveSlopeBound", "AssumedImbalance",
	"AssumedPromptSpread", "TransferOmitted", "DecodeSideOmitted", "DecodeTTFTUnmeasured",
	"DecodeEngineMeasured", "DecodeTermOmitted", "DecodeStepSlopeAssumed", "RouterHopAssumed",
	"DecodeTermFloored", "DecodeTermPrefixHits",
	"VPScaledToPrompt", "VPShapeMoved", "FlowControlInferredOff",
	"AboveFlowControlOnset", "HeldByDecodeAdmission", "DecodeOnsetUnchecked",
	"BelowFittedBand", "KVBoundSlots",
	"AssumedPoissonArrivals", "PromptSpreadClamped", "FlowControlMismatch",
	"PromptSpreadAbovePeak", "ImbalanceNotSustained", "OutputSpreadUnread",
}

// SLOSources is every SLO source value, in the order they are published.
var SLOSources = []string{
	SLOSourceOwn, SLOSourcePinned, SLOSourcePinnedDefault, SLOSourceNoLine, SLOSourceDeclined,
	SLOSourceNoOutputLength, SLOSourceInfeasible, SLOSourceInputInvalid,
}
