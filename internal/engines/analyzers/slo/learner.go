package slo

import (
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// The line as a learner on the shared rules (analyzer-learner.md, 6.1 to 6.3):
// each cycle it files a learn.Report per generating variant, as the saturation
// analyzer's lines do, under learn.SLO, so its last-admitted series is its
// own, and logs learner-write when the key's line moved. The analyzer fills
// and logs; the engine takes the reports (TakeLearnerReports) and publishes.

// SwitchITLFilesReports is the replay switch for the line's report and its
// learner-write. Off, as before, it files and logs neither. Nothing the
// analyzer prices reads them.
const SwitchITLFilesReports = "slo-itl-files-learner-reports"

// ITLWriteBand is how far a key's A or B must move from the value learner-write
// last logged, as a fraction of that value, before it logs the line again: the
// saturation analyzer's band (saturation.ITLWriteBand).
const ITLWriteBand = 0.05

// LearnerHeartbeat is how long a line may go without a learner-write line
// while it holds: the saturation analyzer's heartbeat.
const LearnerHeartbeat = 10 * time.Minute

// LearnerWriteMaxPods is how many pods one learner-write line lists, lowest
// reading first; the rest are counted in podsOmitted, so a line stays under
// the CRI line limit: the bound every analyzer's learner-write lines share
// (itl.WriteMaxPods).
const LearnerWriteMaxPods = itl.WriteMaxPods

// lineLogged is the line learner-write last logged for a key, and when.
type lineLogged struct {
	a, b   float64
	source string
	at     time.Time
}

// reportModel is the key of a model's reports.
func reportModel(namespace, modelID string) string {
	return namespace + "|" + modelID
}

// noteLineLearner files the report of one variant's line for the cycle, and
// logs a learner-write line when the key's line is set for the first time,
// changed source (dropping to no line included), moved ITLWriteBand from what
// was last logged, or has not logged for LearnerHeartbeat. At V(3), at most
// one line per key per cycle.
func (a *Analyzer) noteLineLearner(logger logr.Logger, namespace, modelID string, vs domain.VariantReplicaState,
	key string, d pricingDetail, now time.Time) {
	if !a.itlFilesReports || !isGenerating(vs.Role) {
		return
	}
	r, source := d.line, d.source
	role := domain.CanonicalRole(vs.Role)
	rep := a.lines.Report(learn.Report{Learner: learn.SLO, Namespace: namespace, ModelID: modelID,
		Variant: vs.VariantName, Role: role, Source: learn.Source(source)}, key, r)
	silent := r.Tier == ""
	a.mu.Lock()
	model := reportModel(namespace, modelID)
	a.learnerReports[model] = append(a.learnerReports[model], rep)
	last, logged := a.lineLogged[key]
	wasSilent := last.source == constants.SLOSourceNoLine || last.source == constants.SLOSourceDeclined
	// A key whose line drops to none logs that once; one that never had a
	// line logs nothing.
	if silent && (!logged || wasSilent) {
		a.mu.Unlock()
		return
	}
	if logged && last.source == source && !itl.Moved(r.Model.A, last.a, ITLWriteBand) &&
		!itl.Moved(r.Model.B, last.b, ITLWriteBand) && now.Sub(last.at) < LearnerHeartbeat {
		a.mu.Unlock()
		return
	}
	a.lineLogged[key] = lineLogged{a: r.Model.A, b: r.Model.B, source: source, at: now}
	a.mu.Unlock()
	pods, omitted := r.WritePods(LearnerWriteMaxPods)
	kv := []any{"learner", learn.ITL, "analyzer", domain.SLOAnalyzerName, "modelID", modelID, "namespace", namespace,
		"variant", vs.VariantName, "role", role, "key", key, "a", r.Model.A, "b", r.Model.B,
		"previousA", last.a, "previousB", last.b, "firstWrite", !logged, "source", source,
		"previousSource", last.source, "samples", r.Held, "steadyHeld", r.SteadyHeld, "settling", r.Settling,
		"slopeAgeSeconds", r.SlopeAge(now).Seconds(), "pods", pods, "podsOmitted", omitted}
	logger.V(logging.VERBOSE).Info("learner-write", append(kv, r.AdmissionMargins()...)...)
}

// noteForgottenLocked files keys of a variant as forgotten: the next
// TakeLearnerReports says so, whether or not the variant reports again.
// Callers hold a.mu.
func (a *Analyzer) noteForgottenLocked(namespace, modelID, variant string, forgotten []learn.Forgotten) {
	if !a.itlFilesReports || len(forgotten) == 0 {
		return
	}
	a.forgottenReports = append(a.forgottenReports, learn.Report{Learner: learn.SLO, Namespace: namespace,
		ModelID: modelID, Variant: variant, Forgotten: forgotten})
}

// noteEvictedLocked files each key the sweep evicted against its variant, read
// from the key's shape (windowKey). Callers hold a.mu.
func (a *Analyzer) noteEvictedLocked(evicted []learn.Forgotten) {
	for _, f := range evicted {
		delete(a.lineLogged, f.Key)
		parts := strings.Split(f.Key, "|")
		n := len(parts)
		if n < 5 {
			continue
		}
		a.noteForgottenLocked(parts[0], strings.Join(parts[1:n-3], "|"), parts[n-3], []learn.Forgotten{f})
	}
}

// TakeLearnerReports hands over each model's reports, by namespace|model, and
// keeps none, as the saturation analyzer's does: what was forgotten first, so
// that a key refilled on the same cycle stays published. Until the engine takes
// them each model's are overwritten every cycle. Nothing the analyzer prices
// reads them, so taking them changes no decision.
func (a *Analyzer) TakeLearnerReports() map[string][]learn.Report {
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
