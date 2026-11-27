package slo

import (
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The v2 latency learners (learn.TTFTMixture, learn.TPOTDistribution): the
// capacity planner's rank-mixture TTFT model and renewal-lognormal TPOT
// distribution, fitted on the controller's own cycles by the planner's own
// fit cores (whatif.FitMixtureWindow, whatif.FitTPOTWindow), so that the two
// give the same figures on the same cycles (the parity test,
// cmd/capacity-planner/v2_parity_test.go).
// They follow the shared learner rules (docs/proposals/analyzer-learner.md):
//
//   - observe and admit: each cycle the engine hands over (ObserveModelInputs)
//     is built into a whatif.FitCycle; a cycle on which no prefill rank and no
//     decode row served is refused (idle). The fits admit steps themselves:
//     the TTFT fit leaves out a step that is not stationary, and both read
//     only steps the router measured.
//   - fit: once per ModelRefitEvery, on the window of the last ModelWindow
//     cut into ModelStep steps, in the background, one model at a time, off
//     the engine's decision path (a TTFT refit of a large P/D model on a full
//     window costs seconds: model_cost_test.go).
//   - price: at most once per ModelPriceEvery, at the cycle's rate and
//     serving prefill pods, the figures held between; the prefill ask is
//     searched outward from the serving pods.
//   - validate: a fit is used only where it passes the planner's checks (the
//     TTFT model's identifiability, the TPOT model's folds) and its
//     held-out self-check folds put at least ModelMinHeldOutShare of their
//     steps inside the planner's tolerance (HeldOutPasses).
//   - hold or forget: a refit that does not pass keeps the last fit that
//     did for ModelHoldFor; the window is forgotten when the model's prefill
//     or decode variant, or its engine arguments, change, on the forget
//     annotation (forgetVariant), and when the model goes unseen past the
//     state timeout (evictStale).
//   - report: each cycle a learn.Report per learner, under its name.
//
// Nothing here reaches a sizing decision: the figures are published, and the
// prefill ask is read by the engine for the would-be series of a passive or
// shadow model only.

const (
	// ModelStep is the length of the steps the v2 models fit on: the
	// planner's backtest step (whatifapi.DefaultBacktestStep).
	ModelStep = 5 * time.Minute
	// ModelRefitEvery is how often the v2 models are refitted
	// (WithModelRefits). A window of twelve hours and more moves little in
	// half an hour, and a TTFT refit costs seconds.
	ModelRefitEvery = 30 * time.Minute
	// ModelPriceEvery is how often a model's v2 figures and prefill ask are
	// priced: each pricing draws the rank mixture's 16,000 requests per
	// fleet size.
	ModelPriceEvery = time.Minute
	// ModelWindow is the cycles the v2 models fit on: the TTFT model's
	// spread is calibrated only on a window of whatif.MinSpreadFitWindow,
	// and an hour more keeps one there through a step's trim.
	ModelWindow = whatif.MinSpreadFitWindow + time.Hour
	// ModelCycleSpacing is the least time between two cycles a window keeps:
	// a cycle closer to the last one kept is priced and not kept, so a
	// window holds as much at a 5 s cycle as at the default 15 s one. The
	// fits pool cycles over the router's window and cut them into
	// ModelStep steps, so a cycle in three changes no step's mix; 13 s, not
	// 15, keeps every cycle of a 15 s interval through its jitter.
	ModelCycleSpacing = 13 * time.Second
	// modelMaxCycles bounds a window's cycles: ModelWindow at
	// ModelCycleSpacing.
	modelMaxCycles = int(ModelWindow/ModelCycleSpacing) + 1
	// ModelMaxWindows is the most models whose v2 windows the analyzer keeps
	// at once. A window of a large P/D model at a cycle every
	// ModelCycleSpacing holds about 15 MiB (model_cost_test.go measures 13
	// MiB at 15 s), and the v2 models run for every
	// passive model and, with the utilization share in shadow, for every
	// model, so without a bound a large fleet would hold more than the
	// controller's memory limit (512 MiB in config/base/manager). A model
	// over the bound reads coverage model-limit until a window is freed:
	// its model goes unobserved for ModelReleaseAfter, leaves (evictStale)
	// or is forgotten.
	ModelMaxWindows = 16
	// ModelReleaseAfter is how long a window not observed keeps its place
	// while another model waits for one (ModelMaxWindows): a model that
	// turned active, left the share's shadow or dropped the slo analyzer
	// is no longer observed, and its window is released to the model
	// waiting, not held to the state timeout.
	ModelReleaseAfter = time.Hour
	// ModelHoldFor is how long a fit that passed is held while the refits
	// after it do not.
	ModelHoldFor = time.Hour
	// ModelFitBudget is the time a refit of one model may take, both fits
	// together. A refit over it is reported (the over-budget count of the
	// learners' reports) and the next one waits twice as long.
	ModelFitBudget = 5 * time.Second
	// ModelMinHeldOutShare is the least share of held-out steps a fit must
	// put inside modelHeldOutTolerance to be used.
	ModelMinHeldOutShare = 0.5
	// modelHeldOutTolerance is the planner's backtest tolerance
	// (whatifapi.BacktestTolerance), what a held-out step must be within.
	modelHeldOutTolerance = 0.2
)

// Admission and fit outcomes of the v2 learners' reports.
const (
	modelAdmitted   learn.Reason = "admitted"
	modelIdle       learn.Reason = "idle"
	modelRefitted   learn.Reason = "refitted"
	modelHeld       learn.Reason = "held"
	modelOverBudget learn.Reason = "over-budget"
)

// Sources of the v2 learners' reports.
const (
	modelSourceFitted learn.Source = "fitted"
	modelSourceHeld   learn.Source = "held"
	modelSourceNone   learn.Source = "none"
)

// modelWindow is one model's v2 learners: the window of cycles they fit on,
// and each one's last fit. Under the analyzer's mu.
type modelWindow struct {
	// prefill and decode are the variant pair the window reads, and
	// prefillArgs and decodeArgs their engine arguments: a change of either
	// starts the window again.
	prefill, decode         string
	prefillArgs, decodeArgs string
	cycles                  []whatif.FitCycle
	seen                    time.Time
	// observed is the analyzer cycle (beginCycle) the window was last
	// observed on: Analyze publishes the v2 figures of a model observed on
	// its own cycle only.
	observed uint64
	// refitAt is when the next refit is due, fitting says one is running.
	refitAt time.Time
	fitting bool
	ttft    ttftLearner
	tpot    tpotLearner
	// counts is this cycle's admission outcomes, for both learners' reports.
	counts map[learn.Reason]int64
	// now is this cycle's figures, and lastPrefillPods the serving pods of
	// the prefill variant on it: what a refit records its fit at. priced is
	// the last pricing, held for ModelPriceEvery.
	now             modelFigures
	lastPrefillPods int
	priced          modelPricing
	// unreadSince is the first cycle of a run whose variant pair or
	// argument tags did not read (unreadPair), zero while they read.
	unreadSince time.Time
}

// modelPricing is a model's last v2 TTFT pricing and what it was priced at:
// it stands until ModelPriceEvery has passed, or the fit, the serving
// prefill pods or the objective change.
type modelPricing struct {
	at, fitAt time.Time
	pods      int
	q, target float64
	ok        bool
	ttft      map[string]float64
	ask       int
	askOK     bool
}

// ttftLearner is the v2 TTFT learner's fit.
type ttftLearner struct {
	// fit is the fit in use, fittedAt when it was fitted (zero: none), and
	// prefill the serving prefill pods it was fitted at. last is the last
	// refit's outcome, held whether fit is that refit's or an earlier one
	// held.
	fit      whatif.MixtureWindowFit
	fittedAt time.Time
	prefill  int
	last     whatif.MixtureWindowFit
	held     bool
}

// tpotLearner is the v2 TPOT learner's fit, and the default model's R_q read
// from the same window's router histograms.
type tpotLearner struct {
	fit      whatif.TPOTWindowFit
	fittedAt time.Time
	last     whatif.TPOTWindowFit
	held     bool
	// defaultRatio is the default model's R_q at 0.9 and 0.95
	// (whatif.TPOTRatioFromWindows over the window), 0 where not read.
	defaultRatio [2]float64
}

// modelFigures is one cycle's v2 figures for a model.
type modelFigures struct {
	// ttft is the v2 TTFT at the cycle's rate and prefill pods, by
	// statistic; nil where not priced. ttftReason and tpotReason are each
	// objective's coverage (constants.SLOModelReasons).
	ttft       map[string]float64
	ttftReason string
	tpotReason string
	// ask is the prefill pods the v2 TTFT model asks for at the objective,
	// askOK false with none.
	ask   int
	askOK bool
}

// ModelPrefillAsk is what the v2 TTFT model asks for a model's prefill
// variant this cycle: the least serving pods at which it prices the TTFT
// objective's percentile at the cycle's rate under the target.
type ModelPrefillAsk struct {
	Variant string
	Pods    int
}

// HeldOutPasses says a TTFT fit's held-out folds put at least
// ModelMinHeldOutShare of their steps within the tolerance; a fit with no
// held-out step passes (the planner's identifiability checks stand alone).
func HeldOutPasses(h whatif.HeldOutScore) bool {
	return h.Steps == 0 || float64(h.Within) >= ModelMinHeldOutShare*float64(h.Steps)
}

// tpotFoldsPass says a TPOT fit's self-check folds put the p90 in the
// measured bucket on at least ModelMinHeldOutShare of their steps.
func tpotFoldsPass(r whatif.TPOTDistReport) bool {
	steps, inside := 0, 0
	for _, f := range r.Fold {
		steps += f.Steps
		inside += f.P90Inside
	}
	return steps == 0 || float64(inside) >= ModelMinHeldOutShare*float64(steps)
}

// modelPair is the prefill and the decode variant the v2 models read for in:
// the one variant of each role; the decode role is the unsplit ("both")
// variant on a fleet with no decode role. "" where the role has none or
// several.
func modelPair(in domain.AnalyzerInput) (prefillVariant, decodeVariant string) {
	var prefills, decodes, both []string
	for _, vs := range in.VariantStates {
		switch domain.CanonicalRole(vs.Role) {
		case domain.RolePrefill:
			prefills = append(prefills, vs.VariantName)
		case domain.RoleDecode:
			decodes = append(decodes, vs.VariantName)
		case domain.RoleBoth:
			both = append(both, vs.VariantName)
		}
	}
	if len(decodes) == 0 {
		decodes = both
	}
	if len(prefills) == 1 {
		prefillVariant = prefills[0]
	}
	if len(decodes) == 1 {
		decodeVariant = decodes[0]
	}
	return prefillVariant, decodeVariant
}

// servingPods is the serving rows of variant in rows.
func servingPods(rows []domain.ReplicaMetrics, variant string) int {
	n := 0
	for i := range rows {
		if rows[i].VariantName == variant && prefill.Serving(rows[i]) {
			n++
		}
	}
	return n
}

// cycleRate is the rate the v2 TTFT model prices a cycle at: the model's
// arrival rate, or, with none, its prefill ranks' requests a second.
func cycleRate(c *whatif.FitCycle) float64 {
	if c.RateKnown && c.Rate > 0 && whatif.Finite(c.Rate) {
		return c.Rate
	}
	r := 0.0
	for _, rk := range c.Mixture.Ranks {
		r += rk.Rate
	}
	return r
}

// observeModelLocked files one cycle into the model's window and prices it
// with the fits in use; it returns the window, the cycle, and whether a
// refit is due (refitModel, run off the lock). The window is nil for a new
// model while the analyzer keeps a.modelMaxWindows windows already
// (ModelMaxWindows). Callers hold a.mu.
func (a *Analyzer) observeModelLocked(in domain.AnalyzerInput, router *domain.RouterLatency, now time.Time) (*modelWindow, whatif.FitCycle, bool) {
	key := reportModel(in.Namespace, in.ModelID)
	pv, dv := modelPair(in)
	pArgs, dArgs := a.engineParams(in.Namespace, in.ModelID, pv).ArgsTag(), a.engineParams(in.Namespace, in.ModelID, dv).ArgsTag()
	w := a.models[key]
	if w == nil && len(a.models) >= a.modelMaxWindows {
		a.releaseUnobservedModelsLocked(now)
	}
	if w == nil && len(a.models) >= a.modelMaxWindows {
		return nil, whatif.FitCycle{}, false
	}
	if w != nil && unreadPair(w, pv, dv, pArgs, dArgs) {
		if w.unreadSince.IsZero() {
			w.unreadSince = now
		}
	} else if w != nil {
		w.unreadSince = time.Time{}
	}
	if w != nil && !w.unreadSince.IsZero() && now.Sub(w.unreadSince) <= ModelReleaseAfter {
		// The variant pair or an engine argument tag did not read this
		// cycle (variant states missing, a capacity record not there yet):
		// the window stands, and the cycle is neither kept nor priced.
		// Unread past ModelReleaseAfter (the model moved to several
		// variants of a role), the window starts again below.
		w.seen, w.observed, w.counts = now, a.cycle, map[learn.Reason]int64{modelIdle: 1}
		w.now = modelFigures{ttftReason: constants.SLOModelSignalMissing, tpotReason: constants.SLOModelSignalMissing}
		return w, whatif.FitCycle{}, false
	}
	if w == nil || w.prefill != pv || w.decode != dv || w.prefillArgs != pArgs || w.decodeArgs != dArgs {
		if w != nil && len(w.cycles) > 0 {
			a.noteModelForgottenLocked(in.Namespace, in.ModelID, w, learn.ByArguments)
		}
		w = &modelWindow{prefill: pv, decode: dv, prefillArgs: pArgs, decodeArgs: dArgs}
		a.models[key] = w
	}
	w.seen, w.observed, w.counts = now, a.cycle, map[learn.Reason]int64{}
	c := whatif.FitCycleOf(in, ownedRouter(router), now, pv, dv)
	if len(c.Mixture.Ranks) == 0 && len(c.Decode.Groups) == 0 && len(c.Decode.Output) == 0 {
		w.counts[modelIdle]++
	} else {
		// Admitted is a cycle kept: one closer than ModelCycleSpacing to
		// the last one kept is priced only.
		if n := len(w.cycles); n == 0 || c.At.Sub(w.cycles[n-1].At) >= ModelCycleSpacing {
			w.counts[modelAdmitted]++
			w.cycles = append(w.cycles, c)
		}
	}
	cut := 0
	for cut < len(w.cycles) && !w.cycles[cut].At.After(now.Add(-ModelWindow)) {
		cut++
	}
	cut = max(cut, len(w.cycles)-modelMaxCycles)
	if cut > 0 {
		w.cycles = slices.Delete(w.cycles, 0, cut)
	}
	a.priceModelLocked(w, in, &c, now)
	due := !w.fitting && !now.Before(w.refitAt) && len(w.cycles) > 0
	return w, c, due
}

// unreadPair says the cycle's variant pair or engine argument tags are
// unread where the window's were read: a role whose variant is "" this
// cycle (none, or several during a migration), or an argument tag "" (the
// capacity record not read). A change between two read values starts the
// window again; a value going unread for a cycle does not.
func unreadPair(w *modelWindow, pv, dv, pArgs, dArgs string) bool {
	return (pv == "" && w.prefill != "") || (dv == "" && w.decode != "") ||
		(pArgs == "" && w.prefillArgs != "") || (dArgs == "" && w.decodeArgs != "")
}

// releaseUnobservedModelsLocked deletes the window of every model not
// observed for ModelReleaseAfter, filed as evicted: another model waits for
// its place. Callers hold a.mu.
func (a *Analyzer) releaseUnobservedModelsLocked(now time.Time) {
	for key, w := range a.models {
		if now.Sub(w.seen) <= ModelReleaseAfter {
			continue
		}
		if ns, model, ok := strings.Cut(key, "|"); ok {
			a.noteModelForgottenLocked(ns, model, w, learn.ByEvicted)
		}
		delete(a.models, key)
	}
}

// ownedRouter is a copy of r the window can keep: the caller's reading, and
// its buckets, stay the caller's, while a background refit reads the copy.
// Nil for nil.
func ownedRouter(r *domain.RouterLatency) *domain.RouterLatency {
	if r == nil {
		return nil
	}
	c := *r
	c.TTFTBuckets = slices.Clone(r.TTFTBuckets)
	c.TPOTBuckets = slices.Clone(r.TPOTBuckets)
	c.FlowControlWaitBuckets = slices.Clone(r.FlowControlWaitBuckets)
	c.ProcessingBuckets = slices.Clone(r.ProcessingBuckets)
	return &c
}

// refitModel refits a model's two learners on a copy of its window, off the
// lock, and installs what passes; a refit that does not pass leaves the fit
// in use held for ModelHoldFor.
func (a *Analyzer) refitModel(namespace, modelID string, w *modelWindow, now time.Time) {
	a.mu.Lock()
	w.fitting = true
	cycles := slices.Clone(w.cycles)
	pv, dv := w.prefill, w.decode
	prefillPods := w.lastPrefillPods
	a.mu.Unlock()

	start := time.Now()
	tt, tp, defaultRatio := a.fitModel(namespace, modelID, pv, dv, cycles)
	cost := time.Since(start)

	a.mu.Lock()
	defer a.mu.Unlock()
	w.fitting = false
	w.refitAt = now.Add(a.modelRefitEvery)
	if cost > ModelFitBudget {
		w.refitAt = now.Add(2 * a.modelRefitEvery)
		if w.counts != nil {
			w.counts[modelOverBudget]++
		}
	}
	if w.counts != nil {
		w.counts[modelRefitted]++
	}
	w.ttft.last = tt
	switch {
	case tt.Abstained() == "" && HeldOutPasses(tt.HeldOut):
		w.ttft.fit, w.ttft.fittedAt, w.ttft.prefill, w.ttft.held = tt, now, prefillPods, false
	case w.ttft.fittedAt.IsZero() || now.Sub(w.ttft.fittedAt) > ModelHoldFor:
		w.ttft.fit, w.ttft.fittedAt, w.ttft.held = tt, time.Time{}, false
	default:
		w.ttft.held = true
	}
	w.tpot.last, w.tpot.defaultRatio = tp, defaultRatio
	switch {
	case tp.Abstained == "" && tpotFoldsPass(tp.Fitted.Report):
		w.tpot.fit, w.tpot.fittedAt, w.tpot.held = tp, now, false
	case w.tpot.fittedAt.IsZero() || now.Sub(w.tpot.fittedAt) > ModelHoldFor:
		w.tpot.fit, w.tpot.fittedAt, w.tpot.held = tp, time.Time{}, false
	default:
		w.tpot.held = true
	}
}

// fitMixtureWindow is whatif.FitMixtureWindow, the TTFT learner's fit; a
// test replaces it to fail a refit.
var fitMixtureWindow = whatif.FitMixtureWindow

// modelMixtureSwitches is the TTFT learner's fit switches: v2 with the
// stratified service moments, as the capacity planner fits it by default.
// The planner's other switches (the variant QueueRankMixtureNLLine, its
// modes, a shorter fit window) have no slo switch yet.
var modelMixtureSwitches = whatif.RankMixtureSwitches{Stratified: whatif.DefaultRankMixtureStratified}

// fitModel fits a model's two learners on cycles, the window of the prefill
// variant pv and the decode variant dv, and reads the default model's R_q
// from the same window. A panic in a fit core fails the refit, not the
// controller: it is logged and both fits abstain, so the fits in use are
// held as after any refit that does not pass.
func (a *Analyzer) fitModel(namespace, modelID, pv, dv string, cycles []whatif.FitCycle) (
	tt whatif.MixtureWindowFit, tp whatif.TPOTWindowFit, defaultRatio [2]float64) {
	defer func() {
		if r := recover(); r != nil {
			ctrl.Log.WithName("slo").Error(fmt.Errorf("panic: %v\n%s", r, debug.Stack()),
				"v2 latency models: the refit failed; the fits in use are held", "namespace", namespace, "modelID", modelID)
			why := fmt.Sprintf("the refit failed: %v", r)
			tt = whatif.MixtureWindowFit{Fitted: whatif.AbstainedRankMixture(why)}
			tp = whatif.TPOTWindowFit{Abstained: why}
			defaultRatio = [2]float64{}
		}
	}()
	if pv != "" {
		tt = fitMixtureWindow(cycles, ModelStep, a.engineParams(namespace, modelID, pv).PrefillSlots(), modelHeldOutTolerance,
			modelMixtureSwitches)
	} else {
		tt = whatif.MixtureWindowFit{Fitted: whatif.AbstainedRankMixture("the model has not one prefill variant")}
	}
	if dv != "" {
		slots, spec := a.engineParams(namespace, modelID, dv).DecodeSlots()
		tp = whatif.FitTPOTWindow(cycles, ModelStep, slots, spec, whatif.TPOTRenewalLognormal, 0)
	} else {
		tp = whatif.TPOTWindowFit{Abstained: "the model has not one decode variant"}
	}
	var windows []whatif.TPOTWindow
	for i := range cycles {
		if tw, ok := whatif.TPOTWindowOf(cycles[i].Router); ok {
			windows = append(windows, tw)
		}
	}
	for i, q := range []float64{0.9, 0.95} {
		if r, _, u := whatif.TPOTRatioFromWindows(windows, q); u == nil {
			defaultRatio[i] = r.Value
		}
	}
	return tt, tp, defaultRatio
}

// priceModelLocked prices the cycle c with the fits in use: the v2 TTFT at
// the cycle's rate and serving prefill pods (mean, p90, p95, and the
// objective's percentile), the prefill ask at the TTFT target, and each
// objective's coverage. Callers hold a.mu.
func (a *Analyzer) priceModelLocked(w *modelWindow, in domain.AnalyzerInput, c *whatif.FitCycle, now time.Time) {
	w.now = modelFigures{ttftReason: constants.SLOModelPriced, tpotReason: constants.SLOModelPriced}
	pods := servingPods(in.ReplicaMetrics, w.prefill)
	w.lastPrefillPods = pods
	tt := &w.ttft
	span := w.span()
	switch {
	case w.prefill == "":
		w.now.ttftReason = constants.SLOModelAbstained
	case tt.fittedAt.IsZero() && span < whatif.MinSpreadFitWindow:
		w.now.ttftReason = constants.SLOModelWindowShort
	case tt.fittedAt.IsZero():
		w.now.ttftReason = constants.SLOModelAbstained
	case pods < 1 || tt.prefill < 1:
		w.now.ttftReason = constants.SLOModelAbstained
	default:
		target, q := ttftObjective(in.Config)
		p := &w.priced
		if p.at.IsZero() || now.Sub(p.at) >= ModelPriceEvery || !p.fitAt.Equal(tt.fittedAt) || p.pods != pods ||
			p.q != q || p.target != target {
			*p = a.priceModel(tt, cycleRate(c), pods, q, target)
			p.at, p.fitAt = now, tt.fittedAt
		}
		if !p.ok {
			w.now.ttftReason = constants.SLOModelAbstained
			break
		}
		w.now.ttft, w.now.ask, w.now.askOK = p.ttft, p.ask, p.askOK
	}
	switch {
	case w.decode == "":
		w.now.tpotReason = constants.SLOModelAbstained
	case w.tpot.fittedAt.IsZero() && span < whatif.MinTPOTDistFitSteps*ModelStep:
		w.now.tpotReason = constants.SLOModelWindowShort
	case w.tpot.fittedAt.IsZero():
		w.now.tpotReason = constants.SLOModelAbstained
	}
}

// span is the time the window's cycles cover.
func (w *modelWindow) span() time.Duration {
	if len(w.cycles) < 2 {
		return 0
	}
	return w.cycles[len(w.cycles)-1].At.Sub(w.cycles[0].At)
}

// percentileStatistic is the statistic label of the percentile q, a share:
// "p" and its percent, as the objective's own reading names it.
func percentileStatistic(q float64) string {
	return "p" + strconv.Itoa(int(q*100+0.5))
}

// ttftObjective is the TTFT target, seconds, and its percentile as a share,
// of the model's policy; 0 and 0 with none.
func ttftObjective(c domain.AnalyzerConfig) (target, q float64) {
	cfg, _ := c.(*config.ScalingPolicy)
	if cfg == nil || cfg.SLO == nil || cfg.SLO.TTFT == nil {
		return 0, 0
	}
	return cfg.SLO.TTFT.Target.Seconds(), float64(cfg.SLO.TTFTPercentile()) / 100
}

// priceModel prices the v2 TTFT at rate on pods serving prefill pods: the
// mean, p90 and p95 and the q percentile from one set of draws, and, with a
// target, the prefill ask (modelPrefillAsk) from the q percentile priced.
func (a *Analyzer) priceModel(tt *ttftLearner, rate float64, pods int, q, target float64) modelPricing {
	p := modelPricing{pods: pods, q: q, target: target}
	qs := []float64{0.9, 0.95}
	if q > 0 {
		qs = append(qs, q)
	}
	mean, quant, ok := tt.fit.Price(rate, float64(tt.prefill)/float64(pods), qs...)
	if !ok {
		return p
	}
	p.ok = true
	p.ttft = map[string]float64{constants.SLOStatisticMean: mean, constants.SLOStatisticP90: quant[0],
		constants.SLOStatisticP95: quant[1]}
	if q > 0 {
		// The objective's percentile, named as the default model's figure
		// at it is.
		p.ttft[percentileStatistic(q)] = quant[2]
	}
	if q > 0 && target > 0 {
		p.ask, p.askOK = a.modelPrefillAsk(tt, rate, pods, q, target, quant[2])
	}
	return p
}

// modelPrefillAsk is the least serving prefill pods, from one to
// maxPrefillAskFactor times the serving ones, at which the v2 TTFT model
// prices the q percentile at rate under target; false where none does.
// atPods is the q percentile at the serving pods, priced already. The TTFT
// falls as pods are added (each takes a smaller share of the rate), so the
// count is searched from the serving pods: by bisection below them where
// they meet the target, one by one above them where they do not (an ask is
// mostly a pod or two over the pods serving), so few fleet sizes are priced.
func (a *Analyzer) modelPrefillAsk(tt *ttftLearner, rate float64, pods int, q, target, atPods float64) (int, bool) {
	meets := func(n int) bool {
		if n == pods {
			return atPods <= target
		}
		_, v, ok := tt.fit.Price(rate, float64(tt.prefill)/float64(n), q)
		return ok && v[0] <= target
	}
	if meets(pods) {
		return sort.Search(pods-1, func(i int) bool { return meets(i + 1) }) + 1, true
	}
	for n := pods + 1; n <= maxPrefillAskFactor*pods; n++ {
		if meets(n) {
			return n, true
		}
	}
	return 0, false
}

// noteModelForgottenLocked files the model's v2 learners as forgotten, by
// by. Callers hold a.mu.
func (a *Analyzer) noteModelForgottenLocked(namespace, modelID string, w *modelWindow, by learn.By) {
	if !a.itlFilesReports {
		return
	}
	key := reportModel(namespace, modelID)
	for _, name := range []string{learn.TTFTMixture, learn.TPOTDistribution} {
		variant := w.prefill
		if name == learn.TPOTDistribution {
			variant = w.decode
		}
		a.forgottenReports = append(a.forgottenReports, learn.Report{Learner: name, Namespace: namespace, ModelID: modelID,
			Variant: variant, Forgotten: []learn.Forgotten{{Key: key, By: by}}})
	}
}

// modelReportsLocked is the v2 learners' reports of one model's cycle.
// Callers hold a.mu.
func (a *Analyzer) modelReportsLocked(namespace, modelID string, w *modelWindow) []learn.Report {
	key := reportModel(namespace, modelID)
	var last time.Time
	if n := len(w.cycles); n > 0 {
		last = w.cycles[n-1].At
	}
	report := func(name, variant, role string, fitted time.Time, held bool) learn.Report {
		r := learn.Report{Learner: name, Namespace: namespace, ModelID: modelID, Variant: variant, Role: role,
			Counts: maps.Clone(w.counts), Samples: len(w.cycles), LastAdmitted: last, Writes: w.counts[modelAdmitted],
			Source: modelSourceNone}
		switch {
		case fitted.IsZero():
		case held:
			r.PricingKey, r.Source = key, modelSourceHeld
			r.Counts[modelHeld]++
		default:
			r.PricingKey, r.Source = key, modelSourceFitted
		}
		return r
	}
	return []learn.Report{
		report(learn.TTFTMixture, w.prefill, domain.RolePrefill, w.ttft.fittedAt, w.ttft.held),
		report(learn.TPOTDistribution, w.decode, domain.RoleDecode, w.tpot.fittedAt, w.tpot.held),
	}
}

// ModelPrefillAsk is the v2 TTFT model's prefill ask for a model this cycle,
// false where the model was not observed this cycle or the model asks for
// none. The engine publishes it as a would-be figure only.
func (a *Analyzer) ModelPrefillAsk(namespace, modelID string) (ModelPrefillAsk, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.models[reportModel(namespace, modelID)]
	if w == nil || w.observed != a.cycle || !w.now.askOK {
		return ModelPrefillAsk{}, false
	}
	return ModelPrefillAsk{Variant: w.prefill, Pods: w.now.ask}, true
}

// ModelFit is a model's v2 learners' last refit, as RefitModel returns it:
// the variant pair the window reads, the serving prefill pods the TTFT fit
// was taken at, and each fit, passed or not.
type ModelFit struct {
	Prefill, Decode string
	PrefillPods     int
	Cycles          int
	TTFT            whatif.MixtureWindowFit
	TPOT            whatif.TPOTWindowFit
}

// RefitModel refits a model's v2 learners now, on the window as it stands,
// and returns what the refit fitted; false for a model never observed. The
// engine never calls it (the learners refit every ModelRefitEvery): the
// planner parity test does, to compare the learners' fit at a cycle with the
// capacity planner's on the same cycles.
func (a *Analyzer) RefitModel(namespace, modelID string) (ModelFit, bool) {
	a.mu.Lock()
	w := a.models[reportModel(namespace, modelID)]
	a.mu.Unlock()
	if w == nil {
		return ModelFit{}, false
	}
	a.refitModel(namespace, modelID, w, a.now())
	a.mu.Lock()
	defer a.mu.Unlock()
	return ModelFit{Prefill: w.prefill, Decode: w.decode, PrefillPods: w.lastPrefillPods, Cycles: len(w.cycles),
		TTFT: w.ttft.last, TPOT: w.tpot.last}, true
}

// WithModelRefits sets how often the v2 learners refit, ModelRefitEvery
// from the constructor, and whether a refit runs in the background (the
// constructor's) or inside ObserveModelInputs. Their steps stay ModelStep
// long whatever it is. A replay that refits at the one cycle it reads
// (learnreplay.ModelV2At) sets it past the recording, in the foreground, so
// as not to fit every half hour of a day it never reads; tests run refits in
// the foreground to read their outcome on the same cycle.
func (a *Analyzer) WithModelRefits(every time.Duration, background bool) *Analyzer {
	a.modelRefitEvery, a.modelRefitBackground = every, background
	return a
}

// startModelRefit runs a model's due refit: in the background, one model at
// a time (a model due while another refits waits for a later cycle), or in
// the foreground. It reports whether the refit ran in the foreground, so
// that the caller prices the cycle again with what it fitted.
func (a *Analyzer) startModelRefit(namespace, modelID string, w *modelWindow, now time.Time) bool {
	if !a.modelRefitBackground {
		a.refitModel(namespace, modelID, w, now)
		return true
	}
	a.mu.Lock()
	if a.modelRefitting {
		a.mu.Unlock()
		return false
	}
	a.modelRefitting = true
	w.fitting = true
	a.mu.Unlock()
	go func() {
		defer func() {
			a.mu.Lock()
			a.modelRefitting = false
			a.mu.Unlock()
		}()
		a.refitModel(namespace, modelID, w, now)
	}()
	return false
}
