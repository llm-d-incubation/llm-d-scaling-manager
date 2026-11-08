package saturation

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// The ITL line as a learner on the shared rules (docs/proposals/
// analyzer-learner.md, 6.1 to 6.3, PR 4a): each cycle it files a
// learn.Report per decode variant it fitted, logs one learner-write line per
// key whose line moved, and Forget reaches it through learn's list of
// learners. The analyzer logs; the Report carries counts, never a log line.

// ITLWriteBand is how far a key's A or B must move from the value
// learner-write last logged, as a fraction of that value, before it logs the
// line again. A change of tier (a full fit, B pinned, a held slope) logs
// whatever the values did. A pinned fit refits A on every cycle from a
// window of up to 20 readings that a replica's cycle moves by about a
// twentieth, so a band under 5 % logged most cycles of a steady fleet; one
// over it missed the slope a load step moves. Not measured on a cluster run.
const ITLWriteBand = 0.05

// The ITL line's sources, as its Report and its learner-write line name
// them: the tier of the line it holds.
const (
	itlSourceOLS    learn.Source = itl.TierOLS
	itlSourcePinned learn.Source = itl.TierPinnedB
	itlSourceHeld   learn.Source = "pinned-B-held"
	itlSourceNoLine learn.Source = ""
)

// itlLogged is the line learner-write last logged for a key, and when.
type itlLogged struct {
	a, b   float64
	source learn.Source
	at     time.Time
}

// itlSource is the source a reading's line names.
func itlSource(r itl.Reading) learn.Source {
	switch {
	case r.Tier == itl.TierOLS:
		return itlSourceOLS
	case r.Tier == itl.TierPinnedB && (r.SlopeHeld || r.TierHeld):
		return itlSourceHeld
	case r.Tier == itl.TierPinnedB:
		return itlSourcePinned
	}
	return itlSourceNoLine
}

// beginITLCycle starts the ITL line's cycle for a model, at the top of
// Analyze: what an Analyze that failed part way left is dropped.
func (a *SaturationAnalyzer) beginITLCycle(namespace, modelID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.itlCycles, namespace+"|"+modelID)
}

// noteITLLearner files the ITL line's report of one variant for the cycle,
// and logs a learner-write line when the key's line is set for the first
// time, changed source (dropping to no line included), moved ITLWriteBand
// from what was last logged, or has not logged for LearnerHeartbeat. At
// V(3), at most one line per key per cycle.
func (a *SaturationAnalyzer) noteITLLearner(namespace, modelID, variant, role, key string, r itl.Reading,
	now time.Time, logger logr.Logger) {
	model := namespace + "|" + modelID
	source := itlSource(r)
	role = a.itlReportRole(role)
	// The key priced the variant when it holds a line of its own: a variant
	// with none borrows (after this), or has no line at all.
	report := a.itlTracker.Report(learn.Report{Learner: learn.ITL, Namespace: namespace, ModelID: modelID,
		Variant: variant, Role: role, Source: source}, key, r)
	restored := a.itlTracker.Restored(key)
	a.mu.Lock()
	report.Forgotten = a.itlForgotten[model][variant]
	delete(a.itlForgotten[model], variant)
	a.itlCycles[model] = append(a.itlCycles[model], report)
	last, logged := a.itlLogged[key]
	// A key whose line drops to none logs that once, with an empty source:
	// after a controller restart with the snapshot off, or once a held slope
	// ages out, the line being gone is the event worth a line. A key that
	// never had one logs nothing.
	if source == itlSourceNoLine && (!logged || last.source == itlSourceNoLine) {
		a.mu.Unlock()
		return
	}
	if logged && last.source == source && !itl.Moved(r.Model.A, last.a, ITLWriteBand) &&
		!itl.Moved(r.Model.B, last.b, ITLWriteBand) && now.Sub(last.at) < LearnerHeartbeat {
		a.mu.Unlock()
		return
	}
	a.itlLogged[key] = itlLogged{a: r.Model.A, b: r.Model.B, source: source, at: now}
	a.mu.Unlock()
	pods, omitted := r.WritePods(LearnerWriteMaxPods)
	kv := []any{"learner", learn.ITL, "modelID", modelID, "namespace", namespace, "variant", variant,
		"role", role, "key", key, "a", r.Model.A, "b", r.Model.B,
		"previousA", last.a, "previousB", last.b, "firstWrite", !logged,
		"source", source, "previousSource", last.source, "samples", r.Held,
		"restored", restored, "steadyHeld", r.SteadyHeld, "slopeAgeSeconds", r.SlopeAge(now).Seconds(),
		"pods", pods, "podsOmitted", omitted}
	logger.V(perReplicaLogLevel).Info("learner-write", append(kv, r.AdmissionMargins()...)...)
}

// closeITLCycle ends the ITL line's cycle for a model: its reports join k2's
// and mu's (LearnerReports), and a variant whose keys were forgotten and
// which fitted nothing this cycle still files one, to say so.
//
// rolesByVariant names each variant's role; a forgotten variant it does not
// name is reported as decode, as before the aggregated line (ev-shindin/llm-scaling-manager#432).
func (a *SaturationAnalyzer) closeITLCycle(namespace, modelID string, rolesByVariant map[string]string) {
	model := namespace + "|" + modelID
	a.mu.Lock()
	defer a.mu.Unlock()
	reports := a.itlCycles[model]
	delete(a.itlCycles, model)
	for _, variant := range slices.Sorted(maps.Keys(a.itlForgotten[model])) {
		role := domain.RoleDecode
		if r, ok := rolesByVariant[variant]; ok {
			role = a.itlReportRole(r)
		}
		reports = append(reports, learn.Report{Learner: learn.ITL, Namespace: namespace, ModelID: modelID,
			Variant: variant, Role: role, Forgotten: a.itlForgotten[model][variant]})
	}
	delete(a.itlForgotten, model)
	a.learnerReports[model] = append(a.learnerReports[model], reports...)
}

// forgetITLLocked forgets the ITL line's state for one variant: its window,
// steady window, traces, intercept and held slope under every key, and what
// learner-write last logged of them. Each key is reported forgotten, by by,
// on the variant's next report. It returns how many windows and intercepts
// it dropped, as the variant's forget counted them before the line was on
// learn's list. Callers hold a.mu.
func (a *SaturationAnalyzer) forgetITLLocked(namespace, modelID, variant string, by learn.By) int {
	prefix := namespace + "|" + modelID + "|" + variant + "|"
	windows, baselines, forgotten := a.itlTracker.ForgetKeys(a.now(), by, func(k string) bool {
		return strings.HasPrefix(k, prefix)
	})
	for key := range a.itlLogged {
		if strings.HasPrefix(key, prefix) {
			delete(a.itlLogged, key)
		}
	}
	// The shape of an aggregated variant's window and line goes with them
	// (itl_aggregated_shape.go).
	for key := range a.itlAggregated {
		if strings.HasPrefix(key, prefix) {
			delete(a.itlAggregated, key)
		}
	}
	if len(forgotten) > 0 {
		model := namespace + "|" + modelID
		if a.itlForgotten[model] == nil {
			a.itlForgotten[model] = map[string][]learn.Forgotten{}
		}
		a.itlForgotten[model][variant] = append(a.itlForgotten[model][variant], forgotten...)
	}
	return windows + baselines
}
