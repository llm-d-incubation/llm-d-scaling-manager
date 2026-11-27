package metrics

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

var (
	sloPredictedTPOT     *prometheus.GaugeVec
	sloModelCoverage     *prometheus.GaugeVec
	sloModelInputMissing *prometheus.GaugeVec

	// sloModelMissing is the inputs last published missing per
	// namespace|model|objective, so an input read again has its series
	// deleted rather than left at a stale 1.
	sloModelMu      sync.Mutex
	sloModelMissing = map[string][]string{}
)

// registerSLOModelInputMetrics creates and registers the slo analyzer's v2
// model series. Called from InitMetrics, after controllerInstance is set.
func registerSLOModelInputMetrics(registry prometheus.Registerer) error {
	withInstance := func(labels ...string) []string {
		if controllerInstance != "" {
			labels = append(labels, constants.LabelControllerInstance)
		}
		return labels
	}
	gauge := func(name, help string, labels []string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	}
	sloPredictedTPOT = gauge(constants.LLMSMSLOPredictedTPOTSeconds,
		"SLO analyzer, shadow only: the TPOT predicted for a decode variant at its current load, in seconds, by "+
			"statistic (mean, p90, p95) and by the learner that priced it (slo, the analyzer's own; "+
			"tpot-distribution, the v2 model), side by side, for a model the v2 models run for (passive, or the utilization share in shadow).",
		withInstance(constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName, constants.LabelStatistic,
			constants.LabelLearner))
	sloModelCoverage = gauge(constants.LLMSMSLOModelCoverage,
		"SLO analyzer, shadow only (switch slo-v2-inputs): 1 for why an objective's v2 latency model stands where it "+
			"does for a model, 0 for the others: priced, abstained, signal-missing, window-short, model-limit.",
		withInstance(constants.LabelNamespace, constants.LabelModelName, constants.LabelObjective, constants.LabelReason))
	sloModelInputMissing = gauge(constants.LLMSMSLOModelInputMissing,
		"SLO analyzer, shadow only (switch slo-v2-inputs): 1 for each input of an objective's v2 latency model the "+
			"controller did not read this cycle; absent for an input it read.",
		withInstance(constants.LabelNamespace, constants.LabelModelName, constants.LabelObjective, constants.LabelSignal))
	for name, c := range map[string]prometheus.Collector{
		constants.LLMSMSLOPredictedTPOTSeconds: sloPredictedTPOT,
		constants.LLMSMSLOModelCoverage:        sloModelCoverage,
		constants.LLMSMSLOModelInputMissing:    sloModelInputMissing,
	} {
		if err := registry.Register(c); err != nil {
			return fmt.Errorf("failed to register %s: %w", name, err)
		}
	}
	sloModelMu.Lock()
	sloModelMissing = map[string][]string{}
	sloModelMu.Unlock()
	return nil
}

// sloModelLabels is one model's label set, with the instance where set.
func sloModelLabels(namespace, modelID string, more prometheus.Labels) prometheus.Labels {
	labels := prometheus.Labels{constants.LabelNamespace: namespace, constants.LabelModelName: modelID}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	for k, v := range more {
		labels[k] = v
	}
	return labels
}

// RecordSLOModelInputs publishes where an objective's v2 model stands for a
// model (constants.SLOObjectiveTTFT or SLOObjectiveTPOT): reason, one of
// constants.SLOModelReasons, as the one coverage series at 1, and missing,
// the inputs not read this cycle. An input published missing before and
// read now has its series deleted. Nil-guarded so tests that never call
// InitMetrics are a no-op.
func RecordSLOModelInputs(namespace, modelID, objective, reason string, missing []string) {
	if sloModelCoverage == nil || (objective != constants.SLOObjectiveTTFT && objective != constants.SLOObjectiveTPOT) {
		return
	}
	for _, r := range constants.SLOModelReasons {
		sloModelCoverage.With(sloModelLabels(namespace, modelID,
			prometheus.Labels{constants.LabelObjective: objective, constants.LabelReason: r})).Set(boolValue(r == reason))
	}
	for _, s := range missing {
		sloModelInputMissing.With(sloModelLabels(namespace, modelID,
			prometheus.Labels{constants.LabelObjective: objective, constants.LabelSignal: s})).Set(1)
	}
	key := namespace + "\x00" + modelID + "\x00" + objective
	sloModelMu.Lock()
	defer sloModelMu.Unlock()
	for _, s := range sloModelMissing[key] {
		if !slices.Contains(missing, s) {
			sloModelInputMissing.Delete(sloModelLabels(namespace, modelID,
				prometheus.Labels{constants.LabelObjective: objective, constants.LabelSignal: s}))
		}
	}
	if len(missing) == 0 {
		delete(sloModelMissing, key)
		return
	}
	sloModelMissing[key] = slices.Clone(missing)
}

// DeleteSLOModelInputs removes every v2 model series of a model, the
// predicted TPOT included.
func DeleteSLOModelInputs(namespace, modelID string) {
	if sloModelCoverage == nil {
		return
	}
	match := prometheus.Labels{constants.LabelNamespace: namespace, constants.LabelModelName: modelID}
	for _, g := range []*prometheus.GaugeVec{sloPredictedTPOT, sloModelCoverage, sloModelInputMissing} {
		g.DeletePartialMatch(match)
	}
	prefix := namespace + "\x00" + modelID + "\x00"
	sloModelMu.Lock()
	defer sloModelMu.Unlock()
	for key := range sloModelMissing {
		if strings.HasPrefix(key, prefix) {
			delete(sloModelMissing, key)
		}
	}
}
