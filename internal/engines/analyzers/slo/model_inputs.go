package slo

import (
	"fmt"
	"runtime/debug"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The slo analyzer's v2 latency models (SwitchV2Inputs, issue #440,
// docs/proposals/predicted-latency-in-controller.md). The analyzer owns the
// latency predictions; the v2 TTFT and TPOT models of the capacity planner
// join it as learners of the shared learner rules
// (docs/proposals/analyzer-learner.md): each observes its cycle (built by
// whatif.MixtureCycleOf or DecodeCycleOf, the planner's own builders),
// admits, fits on a window, predicts, validates and is kept or forgotten.
//
// Each cycle the analyzer reads the inputs the collector kept for them
// (collector.PredictionRows), reports the missing ones per objective, files
// the cycle into the model's window, refits and prices (model_learners.go),
// and publishes where each model stands; Analyze publishes the figures
// beside the default model's (model_published.go). The models run in their
// current, pre-freeze forms: a frozen form changes the fit cores in
// internal/signals/whatif, not this. Nothing here reaches a sizing decision.

// SwitchV2Inputs is the replay switch of the v2 models' inputs for an
// active model. The engine runs the v2 models for every model it moves
// nothing for (a passive model, or any while the utilization share plans in
// shadow) whatever the switch; on, it runs them for active models too, and
// has the collector issue their inputs in every namespace with a model.
// Off from the constructor, like every shadow addition until its evidence
// flips the default.
const SwitchV2Inputs = "slo-v2-inputs"

// ReadsModelInputs reports whether the analyzer reads its v2 models' inputs
// for an active model (SwitchV2Inputs).
func (a *Analyzer) ReadsModelInputs() bool { return a.v2Inputs }

// modelLearner is one v2 latency learner: its name in the learner
// vocabulary and the objective it predicts.
type modelLearner struct {
	name      string
	objective string
}

// modelLearners is the v2 latency learners, in the order they report.
var modelLearners = []modelLearner{
	{name: learn.TTFTMixture, objective: whatif.ObjectiveTTFT},
	{name: learn.TPOTDistribution, objective: whatif.ObjectiveTPOT},
}

// ModelCoverage is where one v2 model stands for a model after a cycle:
// Reason is one of constants.SLOModelReasons, Missing the inputs not read,
// in whatif.ModelInputs' order.
type ModelCoverage struct {
	Learner string
	Reason  string
	Missing []string
}

// ObserveModelInputs reads one model's cycle for the v2 learners: in is the
// model's analyzer input with the rows the collector kept for them, its
// arrival rate and its policy (the TTFT objective the prefill ask is priced
// at), router the router's latency as they fit on it
// (collector.CollectPredictionLatency), nil where the router reported none.
// It files the cycle into the model's window, refits the learners when a
// refit is due (ModelRefitEvery), prices the cycle with the fits in use, and
// publishes and returns each objective's coverage: signal-missing where an
// input was not read and no fit prices, else priced, abstained or
// window-short (model_learners.go). A refit runs in the background, and a
// cycle after it prices with what it fitted. The figures are published by Analyze on
// the same cycle, beside the default model's. The engine decides which
// models it is called for (the switch, and the controller's mode), and
// calls it outside any analyzer's recovery: a panic here is logged and
// costs the model's v2 figures for the cycle, never the controller, and
// nil is returned.
func (a *Analyzer) ObserveModelInputs(in domain.AnalyzerInput, router *domain.RouterLatency) (out map[string]ModelCoverage) {
	defer func() {
		if r := recover(); r != nil {
			ctrl.Log.WithName("slo").Error(fmt.Errorf("panic: %v\n%s", r, debug.Stack()),
				"v2 latency models: observing the cycle failed; the model has no v2 figures this cycle",
				"namespace", in.Namespace, "modelID", in.ModelID)
			out = nil
		}
	}()
	now := a.now()
	missing := whatif.MissingModelInputs(in, router, "", "")
	var (
		w   *modelWindow
		c   whatif.FitCycle
		due bool
	)
	a.locked(func() { w, c, due = a.observeModelLocked(in, router, now) })
	reasons := map[string]string{whatif.ObjectiveTTFT: constants.SLOModelModelLimit, whatif.ObjectiveTPOT: constants.SLOModelModelLimit}
	if w != nil {
		if due && a.startModelRefit(in.Namespace, in.ModelID, w, now) {
			a.locked(func() { a.priceModelLocked(w, in, &c, now) })
		}
		a.locked(func() {
			reasons = map[string]string{whatif.ObjectiveTTFT: w.now.ttftReason, whatif.ObjectiveTPOT: w.now.tpotReason}
		})
	}
	out = make(map[string]ModelCoverage, len(modelLearners))
	for _, l := range modelLearners {
		c := ModelCoverage{Learner: l.name, Reason: reasons[l.objective], Missing: missing[l.objective]}
		if len(c.Missing) > 0 && c.Reason != constants.SLOModelPriced && c.Reason != constants.SLOModelModelLimit {
			c.Reason = constants.SLOModelSignalMissing
		}
		out[l.objective] = c
		metrics.RecordSLOModelInputs(in.Namespace, in.ModelID, l.objective, c.Reason, c.Missing)
	}
	return out
}

// locked runs f under a.mu, released however f returns.
func (a *Analyzer) locked(f func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f()
}
