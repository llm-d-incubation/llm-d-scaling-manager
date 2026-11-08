package throughput

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// The analyzer's recorder (docs/proposals/analyzer-learner.md, 2.4): each
// exported method the engine may call records the call and what it returned,
// and does the unexported method behind it. Analyze observes through the
// unexported observe, so an Analyze is one event. With recording off (a nil
// recorder) each wrapper is one nil test and the call.

// The kinds of event this analyzer records.
const (
	KindAnalyze       = "throughput.Analyze"
	KindObserve       = "throughput.Observe"
	KindForgetVariant = "throughput.ForgetVariant"
	// KindNoteVariantsPresent is NoteVariantsPresent.
	KindNoteVariantsPresent = "throughput.NoteVariantsPresent"
)

// PresentEvent is NoteVariantsPresent's arguments.
type PresentEvent struct {
	Namespace, ModelID string
	Variants           []string
}

// AnalyzeEvent is one Analyze: its input and what it returned. Written
// whether or not it erred. The analyzer reads no capacity store.
type AnalyzeEvent struct {
	// Input is a learnrec.Input, the rows raw.
	Input  learnrec.Raw
	Result *domain.AnalyzerResult
	Error  string
}

// ObserveEvent is Observe's arguments and the reports it returned.
type ObserveEvent struct {
	Now                time.Time
	ModelID, Namespace string
	// Metrics is a []domain.ReplicaMetrics, encoded before the call.
	Metrics learnrec.Raw
	Reports map[string]SanityReport
}

// ForgetVariantEvent is ForgetVariant's arguments and what it dropped.
type ForgetVariantEvent struct {
	Namespace, ModelID, Variant string
	Dropped                     int
}

// WithRecorder records the analyzer's calls to rec; nil turns recording off.
// Call it before the analyzer is used.
func (a *ThroughputAnalyzer) WithRecorder(rec *learnrec.Recorder) *ThroughputAnalyzer {
	a.rec = rec
	return a
}

// SetSwitchesForReplay sets the package's replay switches by name: the test
// seam a replay drives one named change through (analyzer-learner.md, 2.5).
// Never called on a production path; a test holds that.
func (a *ThroughputAnalyzer) SetSwitchesForReplay(switches map[string]bool) error {
	rest := maps.Clone(switches)
	fields := a.switchFields()
	for name := range fields {
		delete(rest, name)
	}
	if err := learnrec.UnknownSwitches("throughput", rest); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for name, on := range switches {
		*fields[name] = on
	}
	return nil
}

// Analyze implements domain.Analyzer (analyze), and records the call.
func (a *ThroughputAnalyzer) Analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	if !a.rec.Active() {
		return a.analyze(ctx, input)
	}
	now := a.clock.Now()
	in := a.rec.Fragment(learnrec.InputOf(input))
	res, err := a.analyze(ctx, input)
	a.rec.Record(KindAnalyze, now, AnalyzeEvent{Input: in, Result: byVariantName(learnrec.ResultOf(res)), Error: learnrec.ErrorOf(err)})
	return res, err
}

// Observe processes one reconcile cycle for a model (observeDefaultBand), and records
// the call. The engine does not call it today; Analyze observes on its own.
func (a *ThroughputAnalyzer) Observe(
	ctx context.Context,
	now time.Time,
	modelID, namespace string,
	metrics []domain.ReplicaMetrics,
) map[string]SanityReport {
	if !a.rec.Active() {
		return a.observeDefaultBand(ctx, now, modelID, namespace, metrics)
	}
	rows := a.rec.Fragment(metrics)
	reports := a.observeDefaultBand(ctx, now, modelID, namespace, metrics)
	a.rec.Record(KindObserve, a.clock.Now(), ObserveEvent{Now: now, ModelID: modelID, Namespace: namespace,
		Metrics: rows, Reports: sortedReports(reports)})
	return reports
}

// NoteVariantsPresent stamps a model's variants as present (noteVariantsPresent),
// and records the call. The engine calls it for every variant that exists,
// parked ones included.
func (a *ThroughputAnalyzer) NoteVariantsPresent(namespace, modelID string, variantNames []string) {
	if a.rec.Active() {
		a.rec.Record(KindNoteVariantsPresent, a.clock.Now(), PresentEvent{Namespace: namespace, ModelID: modelID, Variants: variantNames})
	}
	a.noteVariantsPresent(namespace, modelID, variantNames)
}

// noteVariantsPresent stamps the state of a model's variants, so a parked
// variant, which no Analyze reaches, keeps its line and its series
// (SwitchITLKeepsPresentVariants). What is never seen again is evicted as
// before.
func (a *ThroughputAnalyzer) noteVariantsPresent(namespace, modelID string, variantNames []string) {
	now := a.clock.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.itlKeepsPresent {
		return
	}
	for _, name := range variantNames {
		if state, ok := a.variantStates[variantKey(namespace, modelID, name)]; ok {
			state.lastObservedAt = now
		}
	}
}

// ForgetVariant forgets one variant's ITL line on an operator's request
// (forgetVariant), logs each key it forgot on ctx's logger, and records the
// call. The engine calls it when the saturation analyzer acts on a forget
// token, after that analyzer's own ForgetVariant. It returns how many
// entries it dropped. One path for recording on and off, which differ only
// in the event: a forget is rare, so the clock is read either way.
func (a *ThroughputAnalyzer) ForgetVariant(ctx context.Context, namespace, modelID, variant string) int {
	now := a.clock.Now()
	n, forgotten := a.forgetVariant(namespace, modelID, variant)
	logging.ForgottenKeys(ctrl.LoggerFrom(ctx), AnalyzerName, learn.ITL, namespace, modelID, variant, learn.Keys(forgotten))
	if a.rec.Active() {
		a.rec.Record(KindForgetVariant, now, ForgetVariantEvent{Namespace: namespace, ModelID: modelID,
			Variant: variant, Dropped: n})
	}
	return n
}

// byVariantName is r with its variant capacities sorted by variant name, on
// a copy: Analyze builds them in map order, which no output may depend on.
func byVariantName(r *domain.AnalyzerResult) *domain.AnalyzerResult {
	if r == nil {
		return nil
	}
	r.VariantCapacities = slices.Clone(r.VariantCapacities)
	slices.SortStableFunc(r.VariantCapacities, func(x, y domain.VariantCapacity) int {
		return strings.Compare(x.VariantName, y.VariantName)
	})
	return r
}

// sortedReports is reports with each one's issues sorted, on a copy:
// CheckModelMetrics lists them in map order.
func sortedReports(reports map[string]SanityReport) map[string]SanityReport {
	out := make(map[string]SanityReport, len(reports))
	for v, r := range reports {
		r.Issues = slices.Clone(r.Issues)
		slices.Sort(r.Issues)
		out[v] = r
	}
	return out
}
