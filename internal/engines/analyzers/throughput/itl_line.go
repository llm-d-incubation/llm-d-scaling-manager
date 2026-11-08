package throughput

import (
	"context"
	"maps"
	"slices"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// The ITL line on the shared tracker (docs/proposals/analyzer-learner.md,
// PR 4b-1). The line was fitted from a window of every healthy reading, with
// a pinned fit over the cycle's own rows; it now fits as the saturation and
// slo analyzers' lines do, with a tracker of its own (itl.Tracker), keyed by
// variant as the window was. Two named changes, each behind a replay switch.

// SwitchITLSteadyGate is the replay switch for the move onto itl.Tracker,
// and #273's fixes with it. On: the full fit is taken from steady readings
// only; a reading taken while its replica settles (it joined, or load came
// back after idle) is stamped and kept out of the pinned fit; the pinned fit
// is the k-weighted mean of (ITL - B) / k over the window's readings in the
// band, with at least itl.DefaultMinSamples readings and
// steadyGate.minSettledSamples settled; B is remembered only when positive;
// an ITL of +Inf is no reading; and when there are too few readings the
// line holds its last A (Reason T2-held), for at most the slope hold and
// under the same engine arguments. A key with neither a line in the band
// nor a slope it may hold -- one that never fitted, under load that holds
// every replica above the band -- fits from its steady, settled readings
// above the band instead (Reason T2-above-band,
// itl.Admission.AboveBandFallback), until the band fits. A shape change
// empties the window and drops the held slope: the old shape's A is not
// held into the new shape. Since PR 4b-2 the settle is read from the
// engine's settle tracker, under switches of its own (settle.go). Off, as
// before: a window of every healthy reading in the band, OLS when it is
// ready, else A by least squares through B over the cycle's healthy rows,
// at any k.
const SwitchITLSteadyGate = "throughput-itl-steady-gate"

// SwitchITLTierHysteresis is the replay switch for the latch on the line's
// tier (issue #495, itl.Admission.TierHysteresis). Off, each cycle chooses
// its tier alone, as before.
const SwitchITLTierHysteresis = "throughput-itl-tier-hysteresis"

// SwitchITLKeepsWindowOnGPSMismatch is the replay switch for the
// GPS-mismatch clear (analyzer-learner.md, "The GPS-mismatch clear: dropped
// in 4b-1"). On: three cycles in a row whose generated-token rate is more
// than itl.DefaultGPSMismatchThresholdPct off the line's prediction no longer
// clear the window; the comparison is a diagnostic, judged at the k paired
// with the ITL (KvUsageForITL), logged per pod at the debug level and per
// variant when it starts or ends. Off, as before: the window is cleared, and
// each mismatched pod logs at the default level every cycle.
const SwitchITLKeepsWindowOnGPSMismatch = "throughput-itl-keeps-the-window-on-gps-mismatch"

// itlReasonT2Held is the Reason of a line whose A is held from an earlier
// fit, its window too short to fit one (SwitchITLSteadyGate).
const itlReasonT2Held = "T2-held"

// itlReasonT2AboveBand is the Reason of a line fitted from the readings above
// the band, the band giving none and the key holding no slope it may hold
// (SwitchITLSteadyGate; itl.Admission.AboveBandFallback).
const itlReasonT2AboveBand = "T2-above-band"

// SwitchITLRolloutGuard is the replay switch for the line's rollout guard
// (issue #481, itl.Tracker.RolloutRows). On, while the variant's scale target
// rolls out (capacity.Record.InRollout) or its replicas report more than one
// revision (domain.RevisionsMixed), the line is offered the newest revision's
// rows alone: an old pod's reading would be judged against the new engine's
// sequence cap, and a fit would span two engines. The first rows of a new
// revision clear the window. With no new pod serving it is offered none, and
// holds what it has, under the arguments it was learned under
// (variantState.rolloutHeld): the old pods still serve. Off, as before, every
// fresh row is offered. Only on the tracker (SwitchITLSteadyGate).
const SwitchITLRolloutGuard = "throughput-itl-rollout-guard"

// SwitchITLForgetsOnNewArguments is the replay switch for the line under new
// engine arguments (issue #481). On, a change of the variant's arguments
// (capacity.EngineParams.ArgsTag) forgets what the tracker learned of the key,
// keeping the replicas' traces, as the slo line's linesUnderArgs does: the
// window and B of the old engine. While the guard holds the line, the forget
// waits for a pod of the new revision: until then the old engine is what
// serves. Off, as before, only a held slope ends on a change of arguments, and
// the window goes on being fitted for up to itl.DefaultObservationMaxAge. Only
// on the tracker (SwitchITLSteadyGate).
const SwitchITLForgetsOnNewArguments = "throughput-itl-forgets-on-new-arguments"

// ITLWriteBand is how far a variant's A or B must move from the value
// learner-write last logged, as a fraction of that value, before it logs
// the line again: the saturation analyzer's band (saturation.ITLWriteBand).
const ITLWriteBand = 0.05

// LearnerHeartbeat is how long a line may go without a learner-write line
// while it holds: the saturation analyzer's heartbeat.
const LearnerHeartbeat = 10 * time.Minute

// LearnerWriteMaxPods is how many pods one learner-write line lists, lowest
// reading first; the rest are counted in podsOmitted, so a line stays under
// the CRI line limit: the bound every analyzer's learner-write lines share
// (itl.WriteMaxPods).
const LearnerWriteMaxPods = itl.WriteMaxPods

// itlLogged is the line learner-write last logged for a variant, and when.
type itlLogged struct {
	a, b   float64
	reason string
	at     time.Time
}

// switchFields is each replay switch by name, and the field it sets.
func (a *ThroughputAnalyzer) switchFields() map[string]*bool {
	return map[string]*bool{
		SwitchITLSteadyGate:                   &a.itlSteadyGate,
		SwitchITLKeepsWindowOnGPSMismatch:     &a.itlKeepsOnGPSMismatch,
		SwitchITLReadsSettle:                  &a.itlReadsSettle,
		SwitchITLWaitsAfterRestart:            &a.itlWaitsAfterRestart,
		SwitchITLKeepsSettleThroughMissingRow: &a.itlKeepsSettle,
		SwitchITLRestartUnderName:             &a.itlRestartUnderName,
		SwitchITLIdleOnEvidence:               &a.itlIdleOnEvidence,
		SwitchITLReadsFinitePodMean:           &a.itlFiniteOnly,
		SwitchITLOwnSettleBackdates:           &a.itlOwnBackdate,
		SwitchITLRolloutGuard:                 &a.itlRolloutGuard,
		SwitchITLFilesReports:                 &a.itlFilesReports,
		SwitchITLKeepsPresentVariants:         &a.itlKeepsPresent,
		SwitchITLForgetsOnNewArguments:        &a.itlForgetsOnNewArgs,
		SwitchITLAdmitsAtSequenceCap:          &a.itlAdmitsAtCap,
		SwitchITLForgetReachesTheLine:         &a.itlForgetReachesLine,
		SwitchITLTierHysteresis:               &a.itlTierHysteresis,
		SwitchKVPerRequestOutputDispersion:    &a.kvOutputDispersion,
	}
}

// ReplaySwitchNames is every name SetSwitchesForReplay accepts, sorted.
func ReplaySwitchNames() []string {
	return slices.Sorted(maps.Keys((&ThroughputAnalyzer{}).switchFields()))
}

// lineAdmission is adm for one variant of the model, whose engine engineOf
// read: with the line on the tracker, the above-band fallback; the engine
// arguments its rows were produced under, which bound a held slope
// (itl.Admission.Args); the stated sequence cap, under
// SwitchITLAdmitsAtSequenceCap (itl.Admission.MaxNumSeqs); and each
// replica's settle, read as the six settle switches say (settleAdmission).
// It reads no store: called with a.mu held.
func (a *ThroughputAnalyzer) lineAdmission(adm itl.Admission, namespace, modelID string, e variantEngine) itl.Admission {
	adm.Settle = nil
	if !a.itlSteadyGate {
		return adm
	}
	a.settleAdmission(&adm, namespace, modelID)
	adm.AboveBandFallback = !a.noAboveBandFallback
	adm.AboveBandWaitsAfterRestart = a.aboveBandWaitsAfterRestart
	adm.Args = e.args
	adm.TierHysteresis = a.itlTierHysteresis
	if a.itlAdmitsAtCap && e.batchCap > 0 {
		adm.MaxNumSeqs, adm.MaxNumSeqsSource = e.batchCap, e.batchCapSource
		// Readings at the cap sit above DefaultKSat, where a pinned fit
		// through the default B prices under the truth: priced one-sided
		// until B is learned.
		adm.OneSidedAboveKSat = !a.capPinsDefaultB
	}
	return adm
}

// lineUnderArgsLocked forgets what the tracker learned of key when the
// variant runs under other engine arguments than the ones its line was
// learned under (SwitchITLForgetsOnNewArguments), keeping the replicas'
// traces, as a forget does (forget.go). There is no stash: a rollback relearns
// its line, as on the slo line. Unknown arguments change nothing. Called with
// a.mu held; the tracker's lock is taken under it, as observe's sweep takes it.
func (a *ThroughputAnalyzer) lineUnderArgsLocked(key string, state *variantState, args, namespace, modelID, variant string, now time.Time) {
	if !a.itlForgetsOnNewArgs || args == "" {
		return
	}
	prev := state.lineArgs
	state.lineArgs = args
	if prev == "" || prev == args {
		return
	}
	_, _, forgotten := a.lines.ForgetKeysKeepingTraces(now, learn.ByArguments, func(k string) bool { return k == key })
	a.noteForgottenLocked(namespace, modelID, variant, forgotten)
}

// freshRows is rows less those whose metrics are stale: the tracker judges
// every other row itself (Ready, warm pool, ITL, k, band), and a stale row
// is not a reading of the replica now.
func freshRows(rows []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, len(rows))
	for _, m := range rows {
		if m.Metadata != nil && m.Metadata.FreshnessStatus == "stale" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// lineModel is the variant's line from the tracker's reading this cycle, and
// its Reason; it logs learner-write when the line moved. Called with a.mu
// held.
func (a *ThroughputAnalyzer) lineModel(ctx context.Context, key string, state *variantState,
	namespace, modelID, variant string, now time.Time) (itl.Model, string, bool) {
	// A cycle with no healthy row read nothing (observe): no line, and
	// its zero reading says so.
	r := state.reading
	reason := lineReason(r)
	a.noteLearnerWrite(ctx, key, r, reason, namespace, modelID, variant, now)
	a.noteLineReport(key, state, r, reason, namespace, modelID, variant)
	noteLineSilence(ctx, state, r, reason, namespace, modelID, variant)
	if r.Tier == "" {
		return itl.Model{}, reason, false
	}
	return r.Model, reason, true
}

// The causes a silent line's summary names (silenceCause), in the order
// they are judged.
const (
	// silenceNoHealthyRows: no row of the variant was healthy this cycle
	// (stale, no KV capacity, no ITL, no shape), so nothing was read.
	silenceNoHealthyRows = "no-healthy-rows"
	// silenceFitDeclined: the window held enough readings, and the pinned
	// fit refused them (a flat or negative slope).
	silenceFitDeclined = "fit-declined"
	// silenceShapeChange: the request shape changed, which empties the
	// window and drops the held slope, and the new shape has not fitted.
	silenceShapeChange = "shape-change"
	// silenceRollout: the variant is rolling out (SwitchITLRolloutGuard), and
	// the line has no line: the new pods have not taught one, or, with no new
	// pod serving, the hold has run past the slope's.
	silenceRollout = "rollout"
	// silenceAboveBandUnderKSat: every reading this cycle was above the
	// band at a k under itl.DefaultKSat: the band gets none, and the
	// above-band fallback keeps none.
	silenceAboveBandUnderKSat = "all-above-band-below-ksat"
	// silenceRestart: the line has not been live since the controller
	// started, or since the variant first appeared.
	silenceRestart = "restart"
	// silenceTooFewReadings: fewer than itl.DefaultMinSamples readings, or
	// fewer than minSettledSamples settled, and no slope to hold.
	silenceTooFewReadings = "too-few-readings"
)

// silenceCause is why the variant's line is silent this cycle, from what the
// analyzer already holds: the first of the causes above that applies.
func silenceCause(state *variantState, r itl.Reading) string {
	switch {
	case !state.hasReading:
		return silenceNoHealthyRows
	case r.Declined:
		return silenceFitDeclined
	case state.shapeChanged:
		return silenceShapeChange
	case state.rollout:
		return silenceRollout
	case r.Offered == 0 && r.AboveBand > 0 && r.AboveBandUnderKSat == r.AboveBand:
		return silenceAboveBandUnderKSat
	case !state.everLive:
		return silenceRestart
	}
	return silenceTooFewReadings
}

// noteLineSilence logs one line at the default level when the variant's line
// goes silent -- no line, so no throughput capacity and no say in the
// decision -- or comes back, as the GPS summary logs, with the cause of a
// silence (silenceCause; empty when live) and what its rows were this cycle:
// in the band, above it, below it, and settling; what the band's and the
// above-band windows hold; and the readings above the band admitted at the
// sequence cap, with the cap and its source (SwitchITLAdmitsAtSequenceCap;
// 0 and "none" when nothing states one, or the switch is off); and whether
// the line was priced one-sided through the least ITL past kSat
// (itl.Reading.PinnedAtLeastITL); and whether it is held on the old pods
// (rolloutHeld), which also logs the line when it changes. It logs the
// same line once too when the cap's source changes, or when readings at the
// cap start (atCap above 0) or stop, so a line that stays live still says
// when it is taught from above the band; a source change or a stop is
// logged only after a first line, and a start always. With no healthy row
// the tracker was not read, and every count is 0. Called with a.mu held.
func noteLineSilence(ctx context.Context, state *variantState, r itl.Reading, reason,
	namespace, modelID, variant string) {
	silent := r.Tier == ""
	cause := ""
	if silent {
		cause = silenceCause(state, r)
	} else {
		state.everLive, state.shapeChanged = true, false
	}
	atCap := r.Capped > 0
	capChanged := atCap != state.loggedAtCap ||
		(state.summaryLogged && state.batchCapSource != state.loggedCapSource)
	if silent == state.lineSilent && !capChanged && state.rolloutHeld == state.loggedRolloutHeld {
		return
	}
	state.lineSilent, state.loggedRolloutHeld = silent, state.rolloutHeld
	state.summaryLogged, state.loggedAtCap, state.loggedCapSource = true, atCap, state.batchCapSource
	ctrl.LoggerFrom(ctx).Info("throughput analyzer: ITL line", "namespace", namespace, "modelID", modelID,
		"variant", variant, "silent", silent, "cause", cause, "source", reason,
		"inBand", r.Offered-r.Capped, "aboveBand", r.AboveBand, "belowBand", r.BelowBand, "settling", r.Settling,
		"tierHeld", r.TierHeld, "rolloutHeld", state.rolloutHeld,
		"held", r.Held, "aboveBandHeld", r.AboveBandHeld,
		"atCap", r.Capped, "batchCap", state.batchCap, "batchCapSource", state.batchCapSource,
		"pinnedAtLeastITL", r.PinnedAtLeastITL)
}

// lineReason is the Reason a reading's line carries.
func lineReason(r itl.Reading) string {
	switch {
	case r.Tier == itl.TierOLS:
		return itlReasonT1OLS
	case r.Tier == itl.TierPinnedB && r.AboveBandFit:
		return itlReasonT2AboveBand
	case r.Tier == itl.TierPinnedB && r.SlopeHeld:
		return itlReasonT2Held
	case r.Tier == itl.TierPinnedB && r.BaselineLearned:
		return itlReasonT2Pinned
	case r.Tier == itl.TierPinnedB:
		return itlReasonT2Default
	}
	return itlReasonT2Failed
}

// noteLearnerWrite logs a learner-write line for the variant's line when it
// is set for the first time, changed Reason (dropping to no line included),
// moved ITLWriteBand from what was last logged, or has not logged for
// LearnerHeartbeat. At V(3), at most one line per variant per cycle, as the
// saturation analyzer's ITL line logs. Called with a.mu held.
func (a *ThroughputAnalyzer) noteLearnerWrite(ctx context.Context, key string, r itl.Reading, reason string,
	namespace, modelID, variant string, now time.Time) {
	last, logged := a.itlLogged[key]
	if reason == itlReasonT2Failed && (!logged || last.reason == itlReasonT2Failed) {
		return
	}
	if logged && last.reason == reason && !itl.Moved(r.Model.A, last.a, ITLWriteBand) &&
		!itl.Moved(r.Model.B, last.b, ITLWriteBand) && now.Sub(last.at) < LearnerHeartbeat {
		return
	}
	a.itlLogged[key] = itlLogged{a: r.Model.A, b: r.Model.B, reason: reason, at: now}
	pods, omitted := r.WritePods(LearnerWriteMaxPods)
	kv := []any{"learner", learn.ITL, "analyzer", AnalyzerName, "modelID", modelID, "namespace", namespace,
		"variant", variant, "a", r.Model.A, "b", r.Model.B, "previousA", last.a, "previousB", last.b,
		"firstWrite", !logged, "source", reason, "previousSource", last.reason, "samples", r.Held,
		"steadyHeld", r.SteadyHeld, "settling", r.Settling, "slopeAgeSeconds", r.SlopeAge(now).Seconds(),
		"pods", pods, "podsOmitted", omitted}
	ctrl.LoggerFrom(ctx).V(logging.VERBOSE).Info("learner-write", append(kv, r.AdmissionMargins()...)...)
}

// lineVariantState is the variant's snapshot with the line on the tracker:
// the window figures are the reading's (the steady readings' spread, since
// the full fit is judged on those alone), and B the tracker's remembered
// one. Called with a.mu held.
func (a *ThroughputAnalyzer) lineVariantState(key string, state *variantState, s shape.Shape) ThroughputVariantState {
	b, stored := a.lines.Baseline(key)
	return ThroughputVariantState{
		Shape:            s,
		ObservationReady: state.reading.Ready,
		KSpread:          state.reading.SteadySpread,
		SampleCount:      a.lines.WindowLen(key),
		LastSanityReport: state.lastSanityReport,
		ITLModel:         state.lastITLModel,
		PerReplicaSupply: state.lastPerReplicaSupply,
		TotalSupply:      state.lastTotalSupply,
		Demand:           state.lastDemand,
		Role:             state.role,
		LastFittedB:      b,
		HasFittedB:       stored && b > 0,
	}
}

// noteGPSMismatch is checkVariantGPSMismatch as a diagnostic
// (SwitchITLKeepsWindowOnGPSMismatch): each replica at a k of at least
// itl.DefaultGPSMinKForVerification -- the k paired with its ITL, as the
// line reads it -- whose generated-token rate is more than
// itl.DefaultGPSMismatchThresholdPct off the rate the line predicts. Each such
// pod logs at the debug level; the variant logs one line at the default
// level when it starts or stops mismatching, with how many pods did and the
// largest error. It clears nothing. Called with a.mu held.
func (a *ThroughputAnalyzer) noteGPSMismatch(ctx context.Context, state *variantState,
	metrics []domain.ReplicaMetrics, s shape.Shape, model itl.Model, namespace, modelID, variant string) bool {
	if s.KVreq <= 0 {
		endGPSMismatch(ctx, state, namespace, modelID, variant)
		return false
	}
	logger := ctrl.LoggerFrom(ctx)
	pods, worst, worstPod := 0, 0.0, ""
	for _, m := range metrics {
		if m.TotalKvCapacityTokens <= 0 {
			continue
		}
		k := m.KvUsageForITL()
		errPct, ok := itl.GPSErrorPct(model, k, float64(m.TotalKvCapacityTokens), s.KVreq, m.GenerationTokenRate)
		// A NaN error (an infinite observed rate) is unknown, as !ok is: it
		// is no evidence of a mismatch.
		if !ok || !(errPct > DefaultGPSMismatchThresholdPct) {
			continue
		}
		pods++
		if errPct > worst {
			worst, worstPod = errPct, m.PodName
		}
		logger.V(logging.DEBUG).Info("throughput analyzer: GPS mismatch detected",
			"namespace", namespace, "modelID", modelID, "variant", variant, "pod", m.PodName,
			"k", k, "GPSObs", m.GenerationTokenRate,
			"muDecModel", itl.TokenRate(model, k, float64(m.TotalKvCapacityTokens), s.KVreq),
			"gpsErrPct", errPct)
	}
	mismatched := pods > 0
	if mismatched != state.gpsMismatched {
		logGPSMismatch(ctx, namespace, modelID, variant, mismatched, pods, worstPod, worst)
	}
	state.gpsMismatched = mismatched
	return mismatched
}

// endGPSMismatch ends the variant's GPS mismatch when there is nothing to
// compare -- no line, or no shape -- logging the end if one was logged as
// started, so a mismatch is never left open in the log. Called with a.mu
// held.
func endGPSMismatch(ctx context.Context, state *variantState, namespace, modelID, variant string) {
	if state.gpsMismatched {
		logGPSMismatch(ctx, namespace, modelID, variant, false, 0, "", 0)
	}
	state.gpsMismatched = false
}

// logGPSMismatch is the variant's GPS mismatch summary line.
func logGPSMismatch(ctx context.Context, namespace, modelID, variant string, mismatched bool, pods int,
	worstPod string, worst float64) {
	ctrl.LoggerFrom(ctx).Info("throughput analyzer: GPS mismatch", "namespace", namespace, "modelID", modelID,
		"variant", variant, "mismatched", mismatched, "pods", pods, "worstPod", worstPod, "worstErrPct", worst,
		"thresholdPct", DefaultGPSMismatchThresholdPct)
}
