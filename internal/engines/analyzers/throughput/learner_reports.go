package throughput

import (
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// The line's reports (analyzer-learner.md, 6.1 to 6.3): each cycle it files a
// learn.Report per variant, as the saturation analyzer's lines do, under
// learn.Throughput, so its last-admitted series is its own. The analyzer
// fills them; the engine takes them (TakeLearnerReports) and publishes.

// SwitchITLFilesReports is the replay switch for the line's reports. Off, as
// before, the line files none. Nothing the analyzer prices reads them.
const SwitchITLFilesReports = "throughput-itl-files-learner-reports"

// SwitchITLKeepsPresentVariants is the replay switch for the state of a
// variant the engine reports present. On, it is not evicted, so a parked
// variant keeps its line and its series. Off, as before, it goes
// 2 * itl.DefaultObservationMaxAge after its last Analyze.
const SwitchITLKeepsPresentVariants = "throughput-itl-keeps-present-variants"

// reportModel is the key of a model's reports.
func reportModel(namespace, modelID string) string {
	return namespace + "|" + modelID
}

// noteLineReport files the variant's report for the cycle. Called with a.mu
// held; the tracker's lock is taken under it.
func (a *ThroughputAnalyzer) noteLineReport(key string, state *variantState, r itl.Reading, reason,
	namespace, modelID, variant string) {
	if !a.itlFilesReports {
		return
	}
	rep := a.lines.Report(learn.Report{Learner: learn.Throughput, Namespace: namespace, ModelID: modelID,
		Variant: variant, Role: domain.CanonicalRole(state.role), Source: learn.Source(reason)}, key, r)
	model := reportModel(namespace, modelID)
	a.learnerReports[model] = append(a.learnerReports[model], rep)
}

// noteForgottenLocked files keys of a variant as forgotten: the next
// TakeLearnerReports says so, whether or not the variant reports again.
// Called with a.mu held.
func (a *ThroughputAnalyzer) noteForgottenLocked(namespace, modelID, variant string, forgotten []learn.Forgotten) {
	if !a.itlFilesReports || len(forgotten) == 0 {
		return
	}
	a.forgottenReports = append(a.forgottenReports, learn.Report{Learner: learn.Throughput, Namespace: namespace,
		ModelID: modelID, Variant: variant, Forgotten: forgotten})
}

// noteEvictedLocked files the variant behind key as evicted. Called with a.mu
// held.
func (a *ThroughputAnalyzer) noteEvictedLocked(key string) {
	f := strings.SplitN(key, "\x00", 3)
	if len(f) != 3 {
		return
	}
	a.noteForgottenLocked(f[0], f[1], f[2], []learn.Forgotten{{Key: key, By: learn.ByEvicted}})
}

// TakeLearnerReports hands over each model's reports, by namespace|model, and
// keeps none, as the saturation analyzer's does: what was forgotten first, so
// that a key refilled on the same cycle stays published. Until the engine takes
// them each model's are overwritten every cycle. Nothing the analyzer prices
// reads them, so taking them changes no decision.
func (a *ThroughputAnalyzer) TakeLearnerReports() map[string][]learn.Report {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string][]learn.Report, len(a.learnerReports))
	for _, rep := range a.forgottenReports {
		model := reportModel(rep.Namespace, rep.ModelID)
		out[model] = append(out[model], rep)
	}
	for model, reports := range a.learnerReports {
		out[model] = append(out[model], reports...)
	}
	a.forgottenReports = nil
	a.learnerReports = make(map[string][]learn.Report)
	return out
}
