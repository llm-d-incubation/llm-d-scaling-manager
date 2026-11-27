package constants

// The slo analyzer's v2 latency models (slo.SwitchV2Inputs,
// docs/proposals/predicted-latency-in-controller.md): the series that say
// whether each model can price a model's latency, and the predicted TPOT the
// analyzer will publish beside its predicted TTFT. Shadow; absent while the
// switch is off.
const (
	// LLMSMSLOPredictedTPOTSeconds is the TPOT the slo analyzer predicts
	// for a decode variant at its current load, by statistic (mean, p90,
	// p95) and learner (slo, the default model; tpot-distribution, the v2
	// model, p90 and p95 around the default's mean), side by side. Set for
	// the models the v2 models run for.
	LLMSMSLOPredictedTPOTSeconds = "llmsm_slo_predicted_tpot_seconds"
	// LLMSMSLOModelCoverage is 1 for why an objective's v2 model stands
	// where it does for a model (SLOModelReasons), 0 for the others.
	LLMSMSLOModelCoverage = "llmsm_slo_model_coverage"
	// LLMSMSLOModelInputMissing is 1 for each input of an objective's v2
	// model the cycle did not read (whatif.ModelInputs); absent for one it
	// read.
	LLMSMSLOModelInputMissing = "llmsm_slo_model_input_missing"

	// LabelSignal names a model input: the whatif.ModelInputs name.
	LabelSignal = "signal"
)

// The reasons of llmsm_slo_model_coverage, one of which is 1 per model and
// objective.
const (
	// SLOModelPriced: the model priced the objective.
	SLOModelPriced = "priced"
	// SLOModelAbstained: the model refused the window, as the capacity
	// planner would on the same window.
	SLOModelAbstained = "abstained"
	// SLOModelSignalMissing: an input the model reads was not read this
	// cycle; llmsm_slo_model_input_missing names it.
	SLOModelSignalMissing = "signal-missing"
	// SLOModelWindowShort: the learner's window does not hold enough steps
	// to fit yet (the TTFT model calibrates its spread on 12 hours, the
	// TPOT model fits on two).
	SLOModelWindowShort = "window-short"
	// SLOModelModelLimit: the analyzer already keeps the v2 models' windows
	// of as many models as it holds at once (slo.ModelMaxWindows), so this
	// model is not fitted until one of them leaves.
	SLOModelModelLimit = "model-limit"
)

// SLOModelReasons is every coverage reason, in the order the series are
// published.
var SLOModelReasons = []string{
	SLOModelPriced, SLOModelAbstained, SLOModelSignalMissing, SLOModelWindowShort, SLOModelModelLimit,
}

// The statistics of llmsm_slo_predicted_ttft_seconds and
// llmsm_slo_predicted_tpot_seconds beside the objective's own.
const (
	SLOStatisticMean = "mean"
	SLOStatisticP90  = "p90"
	SLOStatisticP95  = "p95"
)

// SLOPredictedStatistics is every statistic the predicted series publish
// for each learner, in the order published.
var SLOPredictedStatistics = []string{SLOStatisticMean, SLOStatisticP90, SLOStatisticP95}
