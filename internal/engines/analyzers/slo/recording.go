package slo

import (
	"context"
	"maps"
	"slices"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// The analyzer's recorder (docs/proposals/analyzer-learner.md, 2.4): each
// exported method the engine calls records the call and what it returned, and
// does the unexported method of the same name. With recording off (a nil
// recorder) each wrapper is one nil test and the call.

// The kinds of event this analyzer records.
const (
	KindAnalyze             = "slo.Analyze"
	KindEvictStale          = "slo.EvictStale"
	KindBeginCycle          = "slo.BeginCycle"
	KindNoteVariantsPresent = "slo.NoteVariantsPresent"
	KindForgetVariant       = "slo.ForgetVariant"
)

// judged is what one Analyze read from the router and published, beside its
// result.
type judged struct {
	// Latency is what the router returned; RouterCalled is whether it was
	// asked at all (a policy with no objective does not ask).
	Latency      *domain.RouterLatency
	RouterCalled bool
	// Published is the snapshot handed to metrics.PublishSLO: the judged
	// objectives and each variant's pricing, an output like the result.
	Published metrics.SLOModelSnapshot
}

// AnalyzeEvent is one Analyze: its input, the capacity records it read, the
// router latency it was served, and what it returned and published. Written
// whether or not it erred.
type AnalyzeEvent struct {
	// Input is a learnrec.Input, the rows raw.
	Input learnrec.Raw
	// Records is each variant's capacity.Record before Analyze read it, by
	// variant name (a map[string]*capacity.Record). The analyzer reads only
	// their EngineParams.
	Records learnrec.Raw
	// RouterCalled is whether Analyze asked the router; RouterLatency is
	// what it answered, which a replay serves back instead of a query.
	RouterCalled  bool
	RouterLatency *domain.RouterLatency
	// Result is the result without AnalyzedAt; Published the SLO snapshot it
	// published; Error the error, if any.
	Result    *domain.AnalyzerResult
	Published *metrics.SLOModelSnapshot
	Error     string
}

// EvictStaleEvent is EvictStale's argument and what it evicted.
type EvictStaleEvent struct {
	Now     time.Time
	Evicted Evicted
}

// PresentEvent is NoteVariantsPresent's arguments.
type PresentEvent struct {
	Namespace, ModelID string
	Variants           []string
}

// ForgetVariantEvent is ForgetVariant's arguments and how many entries it
// dropped.
type ForgetVariantEvent struct {
	Namespace, ModelID, Variant string
	Dropped                     int
}

// WithRecorder records the analyzer's calls to rec; nil turns recording off.
// Call it before the analyzer is used.
func (a *Analyzer) WithRecorder(rec *learnrec.Recorder) *Analyzer {
	a.rec = rec
	return a
}

// SlopeHoldFor reports how long a line's slope may be held (WithSlopeHoldFor).
func (a *Analyzer) SlopeHoldFor() time.Duration { return a.slopeHoldFor }

// SetSwitchesForReplay sets the package's replay switches by name: the test
// seam a replay drives one named change through (analyzer-learner.md, 2.5).
// The switches are SwitchBatchCapAdmission, SwitchBatchCapPerRank,
// SwitchITLReadsSettle, SwitchITLWaitsAfterRestart,
// SwitchITLKeepsSettleThroughMissingRow, SwitchITLReadsFinitePodMean,
// SwitchITLRestartUnderName, SwitchITLIdleOnEvidence and
// SwitchITLForgetReachesTheLine, SwitchITLOneSidedAtCap, SwitchTTFTPrice,
// SwitchTTFTMeasuredSpread, SwitchTTFTMeasuredTransfer,
// SwitchTTFTProcessorSharing and SwitchKVPerRequestOutputDispersion; a name it does not know is an error, and sets nothing. Never called on a production
// path; a test holds that.
func (a *Analyzer) SetSwitchesForReplay(switches map[string]bool) error {
	rest := maps.Clone(switches)
	fields := a.switchFields()
	for name := range fields {
		delete(rest, name)
	}
	if err := learnrec.UnknownSwitches("slo", rest); err != nil {
		return err
	}
	for name, on := range switches {
		*fields[name] = on
	}
	return nil
}

// switchFields is each replay switch by name, and the field it sets.
func (a *Analyzer) switchFields() map[string]*bool {
	return map[string]*bool{
		SwitchBatchCapAdmission: &a.batchCapAdmission,
		SwitchBatchCapPerRank:   &a.batchCapPerRank,

		// Learner PR 4a: the line on the settle tracker (settle.go).
		SwitchITLReadsSettle:                  &a.itlReadsSettle,
		SwitchITLWaitsAfterRestart:            &a.itlWaitsAfterRestart,
		SwitchITLKeepsSettleThroughMissingRow: &a.itlKeepsSettle,
		SwitchITLReadsFinitePodMean:           &a.itlFiniteOnly,
		SwitchITLRestartUnderName:             &a.itlRestartUnderName,
		SwitchITLIdleOnEvidence:               &a.itlIdleOnEvidence,

		// Issue ev-shindin/llm-scaling-manager#459: a long gap on the analyzer's own tracker is
		// first-seen, whenever EvictStale ran (settle.go).
		SwitchITLLongGapFirstSeen: &a.itlLongGapFirstSeen,

		// Issue ev-shindin/llm-scaling-manager#511: the own tracker's first sighting is one observation,
		// backdated (settle.go).
		SwitchITLOwnSettleBackdates: &a.itlOwnBackdate,

		// Issue ev-shindin/llm-scaling-manager#495: the line's reports and learner-write (learner.go).
		SwitchITLFilesReports: &a.itlFilesReports,

		// Issue ev-shindin/llm-scaling-manager#481: no rows for the line in a rollout (rollout.go).
		SwitchITLRolloutGuard: &a.itlRolloutGuard,

		// Issue ev-shindin/llm-scaling-manager#431: the forget-learned-state annotation reaches the line
		// (forget.go).
		SwitchITLForgetReachesTheLine: &a.itlForgetReachesLine,

		// Issue ev-shindin/llm-scaling-manager#467: the line priced one-sided at the sequence cap while B
		// is unlearned (one_sided.go).
		SwitchITLOneSidedAtCap: &a.itlOneSidedAtCap,

		// The ITL line's tier latch (issue ev-shindin/llm-scaling-manager#495).
		SwitchITLTierHysteresis: &a.itlTierHysteresis,

		// S1: each prefill variant priced against the TTFT target, in shadow
		// (ttft.go).
		SwitchTTFTPrice:            &a.ttftPrice,
		SwitchTTFTMeasuredSpread:   &a.ttftMeasuredSpread,
		SwitchTTFTMeasuredTransfer: &a.ttftMeasuredTransfer,
		SwitchTTFTQuietTransfer:    &a.ttftQuietTransfer,
		// The decode term of a TTFT on a decode line (ttft.go).
		SwitchTTFTDecodeTerm: &a.ttftDecodeTerm,
		// The prefill pod and the KV pull as processor-sharing stages
		// (ttft.go).
		SwitchTTFTProcessorSharing: &a.ttftProcessorSharing,

		// Issue ev-shindin/llm-scaling-manager#276: the spread of output lengths in the KV per request
		// (output_dispersion.go).
		SwitchKVPerRequestOutputDispersion: &a.kvOutputDispersion,
	}
}

// ReplaySwitchNames is every name SetSwitchesForReplay accepts, sorted: the
// switch runs hold each to a row (analyzer-learner.md, 2.5). A test seam,
// like SetSwitchesForReplay; nothing on a production path calls it, and a
// test holds that.
func ReplaySwitchNames() []string {
	return slices.Sorted(maps.Keys((&Analyzer{}).switchFields()))
}

// Analyze implements domain.Analyzer (analyze), and records the call.
func (a *Analyzer) Analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	if !a.rec.Active() {
		res, _, err := a.analyze(ctx, input)
		return res, err
	}
	now := a.now()
	in := a.rec.Fragment(learnrec.InputOf(input))
	records := a.rec.Fragment(capacity.RecordsOf(a.store, input.Namespace, input.ModelID, learnrec.VariantNames(input)))
	res, j, err := a.analyze(ctx, input)
	ev := AnalyzeEvent{Input: in, Records: records, RouterCalled: j.RouterCalled, RouterLatency: j.Latency,
		Result: learnrec.ResultOf(res), Error: learnrec.ErrorOf(err)}
	if err == nil {
		ev.Published = &j.Published
	}
	a.rec.Record(KindAnalyze, now, ev)
	return res, err
}

// EvictStale forgets the learned state not analyzed or reported present for
// the state timeout (evictStale), and records the call.
func (a *Analyzer) EvictStale(now time.Time) Evicted {
	if !a.rec.Active() {
		return a.evictStale(now)
	}
	at := a.now()
	ev := a.evictStale(now)
	a.rec.Record(KindEvictStale, at, EvictStaleEvent{Now: now, Evicted: ev})
	return ev
}

// BeginCycle marks the start of an engine cycle (beginCycle), and records
// the call.
func (a *Analyzer) BeginCycle() {
	if a.rec.Active() {
		a.rec.Record(KindBeginCycle, a.now(), struct{}{})
	}
	a.beginCycle()
}

// NoteVariantsPresent stamps the learned state of a model's variants that
// were discovered this cycle (noteVariantsPresent), and records the call.
func (a *Analyzer) NoteVariantsPresent(namespace, modelID string, variantNames []string) {
	if a.rec.Active() {
		a.rec.Record(KindNoteVariantsPresent, a.now(), PresentEvent{Namespace: namespace, ModelID: modelID, Variants: variantNames})
	}
	a.noteVariantsPresent(namespace, modelID, variantNames)
}

// ForgetVariant forgets one variant's ITL line on an operator's request
// (forgetVariant), logs each key it forgot on ctx's logger, and records the
// call. The engine calls it when the saturation analyzer acts on a forget
// token, after that analyzer's own ForgetVariant. It returns how many
// entries it dropped. One path for recording on and off, which differ only
// in the event: a forget is rare, so the clock is read either way.
func (a *Analyzer) ForgetVariant(ctx context.Context, namespace, modelID, variant string) int {
	now := a.now()
	n, forgotten := a.forgetVariant(namespace, modelID, variant)
	logging.ForgottenKeys(ctrl.LoggerFrom(ctx), domain.SLOAnalyzerName, learn.ITL, namespace, modelID, variant, learn.Keys(forgotten))
	if a.rec.Active() {
		a.rec.Record(KindForgetVariant, now, ForgetVariantEvent{Namespace: namespace, ModelID: modelID,
			Variant: variant, Dropped: n})
	}
	return n
}
