package saturation

import (
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// Start time as a learner on the shared rules (analyzer-learner.md, PR 5c):
// each cycle it files a learn.Report per variant whose start it noted, logs a
// learner-write line per key whose published estimate was set for the first
// time, changed source, moved StartWriteBand from what was last logged, or
// has not logged for LearnerHeartbeat, and Forget reaches it through learn's
// list. All of it with SwitchStartBlendsFirstSamples on; off, the start
// estimate files no report and logs no learner-write, as in 5b.

// StartWriteBand is how far a key's published start estimate must move from
// the value learner-write last logged, as a fraction of it, before it logs
// again. A start moves only when a Pod starts, so most cycles move nothing;
// the band keeps one start among many from logging a line for a 1 % move.
// Not measured on a cluster run.
const StartWriteBand = 0.05

// startReasonAdmitted is the start learner's admission outcome for a start
// folded into the estimate this cycle.
const startReasonAdmitted learn.Reason = "admitted"

// startLogged is what learner-write last logged of a start key, and when.
type startLogged struct {
	value  float64
	source string
	at     time.Time
}

// startWrite is a learner-write line decided under a.mu and logged after it
// is released; the zero value logs nothing.
type startWrite struct {
	kv []any
}

// log logs w at the per-replica verbosity, as k2's and the ITL line's are.
func (w startWrite) log(logger logr.Logger) {
	if w.kv == nil {
		return
	}
	logger.V(perReplicaLogLevel).Info("learner-write", w.kv...)
}

// beginStartCycle starts the start learner's cycle for a model, at the top of
// Analyze: what an Analyze that failed part way left is dropped.
func (a *SaturationAnalyzer) beginStartCycle(namespace, modelID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.startCycles, namespace+"|"+modelID)
}

// noteStartLearnerLocked files the start learner's report of one variant for
// the cycle, from its estimate f and the starts admitted this cycle, and
// returns the learner-write line to log, if any. A note with no model (a
// direct call from a test of the fold) files nothing. Callers hold a.mu.
func (a *SaturationAnalyzer) noteStartLearnerLocked(note startNote, f startFigures, admitted int, now time.Time) startWrite {
	if !a.startBlends || note.modelID == "" {
		return startWrite{}
	}
	model := note.namespace + "|" + note.modelID
	r := learn.Report{Learner: learn.Start, Namespace: note.namespace, ModelID: note.modelID,
		Variant: note.variant, Role: note.role, Source: learn.Source(f.source),
		Writes: int64(admitted)}
	if admitted > 0 {
		r.Counts = map[learn.Reason]int64{startReasonAdmitted: int64(admitted)}
	}
	if f.measured > 0 {
		r.PricingKey, r.Samples, r.LastAdmitted = note.key, f.pods, a.startAdmittedAt[note.key]
	}
	r.Forgotten = a.startForgotten[model][note.variant]
	delete(a.startForgotten[model], note.variant)
	a.startCycles[model] = append(a.startCycles[model], r)

	// Only a measured estimate is a learned figure: the prior is not written
	// by anything this learner admitted.
	if f.measured == 0 {
		return startWrite{}
	}
	last, logged := a.startLogged[note.key]
	moved := math.Abs(f.published-last.value) > StartWriteBand*math.Abs(last.value)
	if logged && last.source == f.source && !moved && now.Sub(last.at) < LearnerHeartbeat {
		return startWrite{}
	}
	a.startLogged[note.key] = startLogged{value: f.published, source: f.source, at: now}
	_, restored := a.startRestored[note.key]
	return startWrite{kv: []any{
		"learner", learn.Start, "modelID", note.modelID, "namespace", note.namespace,
		"variant", note.variant, "role", note.role, "key", note.key,
		"value", f.published, "previousValue", last.value, "firstWrite", !logged,
		"source", f.source, "previousSource", last.source,
		"samples", f.pods, "needed", startConfirmPods,
		"sizedSeconds", f.sized, "measuredSeconds", f.measured,
		"prior", f.prior, "priorSource", f.priorSource,
		"restored", restored,
	}}
}

// closeStartCycle ends the start learner's cycle for a model: its reports
// join the other learners' (LearnerReports), and a variant whose keys were
// forgotten and which noted no start this cycle still files one, to say so.
func (a *SaturationAnalyzer) closeStartCycle(namespace, modelID string, rolesByVariant map[string]string) {
	model := namespace + "|" + modelID
	a.mu.Lock()
	defer a.mu.Unlock()
	reports := a.startCycles[model]
	delete(a.startCycles, model)
	for _, variant := range slices.Sorted(maps.Keys(a.startForgotten[model])) {
		reports = append(reports, learn.Report{Learner: learn.Start, Namespace: namespace, ModelID: modelID,
			Variant: variant, Role: domain.CanonicalRole(rolesByVariant[variant]), Forgotten: a.startForgotten[model][variant]})
	}
	delete(a.startForgotten, model)
	if len(reports) > 0 {
		a.learnerReports[model] = append(a.learnerReports[model], reports...)
	}
}

// installStartLearnerLocked installs, beside the restored start estimate of
// key, its sample count and last admitted stamp from s, and marks it
// restored until it admits a start. A count that is not one a live key could
// hold (no Pod, startConfirmPods or more, a mean that is not a positive
// number) is not installed, and the estimate keeps full authority, as one
// from a document without the count does: the blend can only lengthen what
// sizing reads, so a lost count orders less, never more, than a kept one. A
// stamp in the future past the skew is not installed. With
// SwitchStartBlendsFirstSamples off nothing is, as in 5b. Callers hold a.mu.
func (a *SaturationAnalyzer) installStartLearnerLocked(key string, s LearnedState, now time.Time) {
	if !a.startBlends {
		return
	}
	a.startRestored[key] = struct{}{}
	if v, ok := s.StartSamples[key]; ok && v.Pods > 0 && v.Pods < startConfirmPods && finitePositive(v.MeanSeconds) {
		// Clamped at the longest start the collector reports: a mean over
		// it is no live key's, and would size every start at it.
		a.startSamples[key] = startSample{pods: v.Pods, mean: min(v.MeanSeconds, maxRestoredStartSeconds)}
	}
	if at, ok := s.StartAdmittedAt[key]; ok && !at.IsZero() {
		if at, inTime := notFuture(at, now); inTime {
			a.startAdmittedAt[key] = at
		}
	}
}

// installStartCountedLocked installs, beside the restored start estimate of
// key, the Ready times the document stored for its counted Pods
// (SwitchStartCountedPodsRestored). Beside an estimate only: a Pod counted
// into an estimate that was not installed has to be measured again. A time
// in the future past the skew is not installed: that Pod is folded as a new
// one. Callers hold a.mu.
func (a *SaturationAnalyzer) installStartCountedLocked(key string, s LearnedState, now time.Time) {
	if !a.startCountsRestored {
		return
	}
	for pod, ready := range s.StartCounted {
		if base, ok := startCountedKeyOf(pod); !ok || base != key || ready.IsZero() {
			continue
		}
		if ready, inTime := notFuture(ready, now); inTime {
			a.startCountedRestored[pod] = ready
		}
	}
}

// exportStartCountedLocked adds to s the Ready times of the Pods counted into
// its estimates, and the restored ones no row has claimed yet, beside an
// estimate only, so a count never outlives it. Callers hold a.mu.
func (a *SaturationAnalyzer) exportStartCountedLocked(s *LearnedState) {
	add := func(pod string, ready time.Time) {
		base, ok := startCountedKeyOf(pod)
		if !ok {
			return
		}
		if _, measured := s.StartSeconds[base]; !measured {
			return
		}
		if s.StartCounted == nil {
			s.StartCounted = map[string]time.Time{}
		}
		s.StartCounted[pod] = ready
	}
	for pod, ready := range a.startCountedRestored {
		add(pod, ready)
	}
	for pod, ready := range a.startCountedReady {
		add(pod, ready)
	}
}

// copyStartCounted copies into dst the Ready times src stores for the Pods
// of start key.
func copyStartCounted(dst *LearnedState, src LearnedState, key string) {
	for pod, ready := range src.StartCounted {
		if base, ok := startCountedKeyOf(pod); !ok || base != key {
			continue
		}
		if dst.StartCounted == nil {
			dst.StartCounted = map[string]time.Time{}
		}
		dst.StartCounted[pod] = ready
	}
}

// forgetStartLocked forgets the start learner's state for one variant: its
// estimate, sample count, last admitted stamp, restored mark and what
// learner-write last logged, under every key. Each estimate's key is
// reported forgotten, by by, on the variant's next report. It returns how
// many estimates, counts and stamps it dropped; the Pods counted, refused
// and the counter beside them are working state, dropped with the variant's
// other maps (dropVariantLocked). Callers hold a.mu.
func (a *SaturationAnalyzer) forgetStartLocked(namespace, modelID, variant string, by learn.By) int {
	prefix := namespace + "|" + modelID + "|" + variant + "|"
	of := func(k string) bool { return strings.HasPrefix(k, prefix) }
	var keys []string
	for k := range a.startSeconds {
		if of(k) {
			keys = append(keys, k)
		}
	}
	n := len(keys)
	maps.DeleteFunc(a.startSeconds, func(k string, _ float64) bool { return of(k) })
	before := len(a.startSamples) + len(a.startAdmittedAt)
	maps.DeleteFunc(a.startSamples, func(k string, _ startSample) bool { return of(k) })
	maps.DeleteFunc(a.startAdmittedAt, func(k string, _ time.Time) bool { return of(k) })
	n += before - len(a.startSamples) - len(a.startAdmittedAt)
	maps.DeleteFunc(a.startRestored, func(k string, _ struct{}) bool { return of(k) })
	maps.DeleteFunc(a.startLogged, func(k string, _ startLogged) bool { return of(k) })
	// A forgotten key's seed is judged afresh, so its warning may fire again.
	maps.DeleteFunc(a.startSeedWarned, func(k string, _ float64) bool { return of(k) })
	if len(keys) > 0 && a.startBlends {
		model := namespace + "|" + modelID
		if a.startForgotten[model] == nil {
			a.startForgotten[model] = map[string][]learn.Forgotten{}
		}
		for _, k := range slices.Sorted(slices.Values(keys)) {
			a.startForgotten[model][variant] = append(a.startForgotten[model][variant], learn.Forgotten{Key: k, By: by})
		}
	}
	return n
}
