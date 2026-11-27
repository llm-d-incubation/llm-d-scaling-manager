package steadystate

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// The slo analyzer's v2 latency models' wiring (slo.SwitchV2Inputs,
// docs/proposals/predicted-latency-in-controller.md). The v2 models run for
// a model the controller moves nothing for -- a passive model, or every
// model while the utilization share plans in shadow -- and, with the switch
// on, for every model: there they cost queries and no risk, and their figures
// are the live evidence the switch is turned on by. The collector issues
// their inputs and keeps them apart from the analyzers' rows; the engine
// hands them to the slo analyzer, which owns the latency predictions.
// Nothing here reaches the optimizer or a scaling decision, in any mode.

// modelInputsObserver is the slo analyzer's export point for the v2 models,
// taken as an optional interface as TakeLearnerReports is: the engine holds
// the analyzer as a sloStateKeeper, whose test doubles need not implement it.
type modelInputsObserver interface {
	ReadsModelInputs() bool
	ObserveModelInputs(in domain.AnalyzerInput, router *domain.RouterLatency) map[string]slo.ModelCoverage
	ModelPrefillAsk(namespace, modelID string) (slo.ModelPrefillAsk, bool)
}

var _ modelInputsObserver = (*slo.Analyzer)(nil)

// modelInputs is the slo analyzer as the v2 models' observer when it runs
// them for the model: the switch is on, the model is passive this cycle, or
// the utilization share plans in shadow. Nil otherwise.
func (e *Engine) modelInputs(namespace, modelID string) modelInputsObserver {
	obs, ok := e.sloAnalyzer.(modelInputsObserver)
	if !ok {
		return nil
	}
	if obs.ReadsModelInputs() || e.modelPassive(namespace, modelID) || e.shareInShadow() {
		return obs
	}
	return nil
}

// shareInShadow says the utilization share plans in shadow this cycle: it
// moves nothing, and the controller's targets are its would-be figures.
func (e *Engine) shareInShadow() bool {
	if e.Config == nil {
		return false
	}
	mode, err := e.Config.UtilizationShareMode()
	return err == nil && mode == constants.UtilizationShareModeShadow
}

// predictionNamespaces is every namespace of modelGroups holding a model the
// v2 models run for (modelInputs): the collector issues their queries there
// (collector.ReadPredictionInputsIn). Run after observePassive, which fills
// the cycle's passive models.
func (e *Engine) predictionNamespaces(modelGroups map[string][]llmdvariant.Variant) map[string]bool {
	var namespaces map[string]bool
	for _, vas := range modelGroups {
		if len(vas) == 0 || e.modelInputs(vas[0].Namespace, vas[0].Spec.ModelID) == nil {
			continue
		}
		if namespaces == nil {
			namespaces = map[string]bool{}
		}
		namespaces[vas[0].Namespace] = true
	}
	return namespaces
}

// observeModelInputs hands the slo analyzer one model's cycle for its v2
// models: the rows the collector kept for them, cut to the replicas the
// analyzers read (routing drops a pod no InferencePool selects), the
// model's variant states, arrival rate and policy (the TTFT objective the
// v2 prefill ask is priced at), and the router's latency as the models fit
// on it. A model whose policy does not list the slo analyzer is skipped: the
// v2 figures are published by its Analyze, which does not run for that
// model, so its window would hold one of the analyzer's ModelMaxWindows for
// nothing anyone reads.
func (e *Engine) observeModelInputs(ctx context.Context, data *modelData, policy domain.AnalyzerConfig) {
	if data == nil {
		return
	}
	if p, ok := policy.(*config.ScalingPolicy); ok && p != nil && !p.AnalyzerEnabled(domain.SLOAnalyzerName) {
		return
	}
	obs := e.modelInputs(data.namespace, data.modelID)
	if obs == nil {
		return
	}
	rows, kept := e.ReplicaMetricsCollector.PredictionRows(data.namespace, data.modelID)
	if !kept {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("SLO v2 inputs: the cycle kept no rows for the model",
			"namespace", data.namespace, "modelID", data.modelID)
		rows = data.replicaMetrics
	}
	in := domain.AnalyzerInput{Namespace: data.namespace, ModelID: data.modelID,
		ReplicaMetrics: sameReplicas(rows, data.replicaMetrics), VariantStates: data.variantStates,
		ArrivalRate: data.arrivalRate, Config: policy}
	router := e.ReplicaMetricsCollector.CollectPredictionLatency(ctx, data.modelID, data.namespace)
	published := obs.ObserveModelInputs(in, router) != nil
	if e.sloModelInputs == nil {
		e.sloModelInputs = map[string][2]string{}
	}
	key := utils.GetNamespacedKey(data.namespace, data.modelID)
	e.sloModelInputs[key] = [2]string{data.namespace, data.modelID}
	if published {
		if e.sloModelInputsSeen == nil {
			e.sloModelInputsSeen = map[string]bool{}
		}
		e.sloModelInputsSeen[key] = true
	}
}

// modelPrefillAsk is the slo analyzer's v2 TTFT prefill ask for a model this
// cycle, false where the analyzer does not run the v2 models or asks for
// none.
func (e *Engine) modelPrefillAsk(namespace, modelID string) (slo.ModelPrefillAsk, bool) {
	obs, ok := e.sloAnalyzer.(modelInputsObserver)
	if !ok {
		return slo.ModelPrefillAsk{}, false
	}
	return obs.ModelPrefillAsk(namespace, modelID)
}

// sameReplicas is the rows of kept whose pod has a row in read, in kept's
// order.
func sameReplicas(kept, read []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	pods := make(map[string]bool, len(read))
	for i := range read {
		pods[read[i].PodName] = true
	}
	out := make([]domain.ReplicaMetrics, 0, len(read))
	for i := range kept {
		if pods[kept[i].PodName] {
			out = append(out, kept[i])
		}
	}
	return out
}

// retainModelInputs deletes the v2 model series of every model activeKeys
// does not hold, and of every model the last cycle did not publish them for
// (sloModelInputsSeen): one still reconciled that turned active, left the
// share's shadow or dropped slo from its policy would otherwise keep its last
// coverage for good. nil deletes every one. Published before the model
// reaches recordAnalyzerMetrics, so tracked apart from lastAnalyzerSeries.
// It runs at the start of a cycle, before the cycle observes any model.
func (e *Engine) retainModelInputs(activeKeys map[string]bool) {
	for key, m := range e.sloModelInputs {
		if !activeKeys[key] || !e.sloModelInputsSeen[key] {
			metrics.DeleteSLOModelInputs(m[0], m[1])
			delete(e.sloModelInputs, key)
		}
	}
	e.sloModelInputsSeen = nil
}
