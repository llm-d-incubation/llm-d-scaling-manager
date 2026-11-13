// Package slo is the SLO analyzer: it prices each variant's capacity at the
// fill where the variant meets the latency targets its scaling policy declares,
// and judges whether the model meets them as the router measured it.
//
// Phase 0 of docs/proposals/slo-tiers.md. On this release the engine runs the
// analyzer for every model whose policy lists it, publishes its (D, P) as
// llmsm_analyzer_demand / llmsm_analyzer_target{analyzer_name="slo"} and its own
// llmsm_slo_* series, and sizes from the saturation analyzer alone: nothing this
// package returns changes a replica count.
package slo

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/fleet"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// backlogDrainSeconds is how quickly the analyzer asks the fleet to clear the
// requests waiting in the router's queue: each costs O output tokens, spread
// over this many seconds of demand. A queue under a TPOT SLO is already a
// latency miss for someone, so the drain is short.
const backlogDrainSeconds = 60.0

// defaultStateTimeout is how long a variant's learned state -- its ITL window
// and intercept, its accelerator memo -- and a model's transient memo outlive
// the last cycle that analyzed them or reported them present, unless the engine
// sets another (WithStateTimeout). The saturation analyzer's default variant
// timeout.
const defaultStateTimeout = 24 * time.Hour

// RouterLatencySource reads a model's request latency as the router measured
// it. The collector implements it.
type RouterLatencySource interface {
	CollectRouterLatency(ctx context.Context, modelID, namespace string) *domain.RouterLatency
}

// Analyzer is the SLO analyzer. Safe for concurrent use.
type Analyzer struct {
	router RouterLatencySource
	store  *capacity.Store
	now    func() time.Time
	// stateTimeout is how long unused learned state is kept (EvictStale).
	stateTimeout time.Duration
	// slopeHoldFor is how long after it was fitted a line's slope may be held
	// while the window has too few readings to fit (itl.Admission.SlopeHoldFor).
	slopeHoldFor time.Duration

	// lines fits each generating variant's ITL(k), with the same itl.Tracker
	// and so the same rules as the saturation analyzer. Its own instance:
	// saturation fits only the decode role, and an aggregated replica (role
	// "both"), the common deployment, would never get a line there.
	lines *itl.Tracker
	// paths counts the tier of each line fitted, for the replay
	// (path_counts.go).
	paths pathCounters

	mu sync.Mutex
	// cycle counts the engine's cycles (BeginCycle), so a model can tell it
	// was not analyzed on the one before.
	cycle uint64
	// seen is when each variant key was last analyzed or reported present:
	// its line is evicted on it (EvictStale).
	seen map[string]time.Time
	// args is the engine arguments (capacity.EngineParams.ArgsTag) each
	// variant key's line was last fitted under: a change forgets the line
	// (linesUnderArgs).
	args map[string]string
	// accelerators is the last resolved accelerator per namespace/variant,
	// so a cycle on which the pods disagree about it keys the line where it
	// was (stableAccelerator).
	accelerators map[string]acceleratorMemo
	// transients is each model's recent fleet history, keyed namespace/model,
	// for the transient exclusion (transient.go).
	transients map[string]*transientMemo

	// rec records every call the engine makes, with what it returned
	// (recording.go); nil when recording is off.
	rec *learnrec.Recorder

	// batchCapAdmission is SwitchBatchCapAdmission and batchCapPerRank
	// SwitchBatchCapPerRank (issue ev-shindin/llm-scaling-manager#420): on from the constructor; a replay
	// turns them off (SetSwitchesForReplay).
	batchCapAdmission bool
	batchCapPerRank   bool

	// settle is the engine's settle tracker, read-only (WithSettleView);
	// nil when the engine gave none, and ownSettle, observed by the
	// analyzer itself, stands in (settle.go). ownSettled is when each model
	// was last observed into it, keyed namespace/model.
	settle     settle.View
	ownSettle  *settle.Tracker
	ownSettled map[string]time.Time
	// itlReadsSettle, itlWaitsAfterRestart, itlKeepsSettle, itlFiniteOnly,
	// itlRestartUnderName and itlIdleOnEvidence are the line's six settle
	// switches (settle.go): on from the constructor; a replay turns them off
	// (SetSwitchesForReplay).
	itlReadsSettle       bool
	itlWaitsAfterRestart bool
	itlKeepsSettle       bool
	itlFiniteOnly        bool
	itlRestartUnderName  bool
	itlIdleOnEvidence    bool
	// itlLongGapFirstSeen is SwitchITLLongGapFirstSeen (issue ev-shindin/llm-scaling-manager#459,
	// settle.go): on from the constructor; a replay turns it off
	// (SetSwitchesForReplay).
	itlLongGapFirstSeen bool
	// itlOwnBackdate is SwitchITLOwnSettleBackdates (issue ev-shindin/llm-scaling-manager#511,
	// settle.go): on from the constructor; a replay turns it off
	// (SetSwitchesForReplay).
	itlOwnBackdate bool
	// itlForgetReachesLine is SwitchITLForgetReachesTheLine (forget.go): on
	// from the constructor; a replay turns it off (SetSwitchesForReplay).
	itlForgetReachesLine bool
	// itlOneSidedAtCap is SwitchITLOneSidedAtCap (issue ev-shindin/llm-scaling-manager#467, one_sided.go):
	// on from the constructor; a replay turns it off (SetSwitchesForReplay).
	itlOneSidedAtCap bool
	// itlTierHysteresis is SwitchITLTierHysteresis (issue ev-shindin/llm-scaling-manager#495,
	// tier_hysteresis.go): on, the line's tier is latched.
	itlTierHysteresis bool
	// settleInterval is the controller's optimize interval
	// (WithOptimizationInterval), zero until set: what the line's own trace
	// judges an absence by (settle.Tolerance), as the saturation analyzer's.
	settleInterval time.Duration

	// itlFilesReports is SwitchITLFilesReports (learner.go): on from the
	// constructor; a replay turns it off (SetSwitchesForReplay). lineLogged is
	// what learner-write last logged per key, learnerReports each model's
	// reports of its last cycle, by namespace|model, and forgottenReports what
	// was forgotten since the engine last took them. Under mu.
	itlFilesReports  bool
	lineLogged       map[string]lineLogged
	learnerReports   map[string][]learn.Report
	forgottenReports []learn.Report
	// itlRolloutGuard is SwitchITLRolloutGuard (rollout.go): on from the
	// constructor; a replay turns it off (SetSwitchesForReplay).
	itlRolloutGuard bool

	// prefillEstimates is where P1's prefill estimates are read
	// (WithPrefillEstimates, prefill_estimates.go); nil when none was given.
	prefillEstimates PrefillEstimates

	// ttftPrice is SwitchTTFTPrice (ttft.go): on from the constructor; a
	// replay turns it off (SetSwitchesForReplay).
	ttftPrice bool
	// ttftMeasuredSpread is SwitchTTFTMeasuredSpread and
	// ttftMeasuredTransfer SwitchTTFTMeasuredTransfer (ttft.go): on from the
	// constructor; a replay turns them off (SetSwitchesForReplay).
	ttftMeasuredSpread, ttftMeasuredTransfer bool
	// ttftQuietTransfer is SwitchTTFTQuietTransfer (ttft.go): on from the
	// constructor; a replay turns it off (SetSwitchesForReplay).
	ttftQuietTransfer bool
	// ttftDecodeTerm is SwitchTTFTDecodeTerm (ttft.go): on from the
	// constructor; a replay turns it off (SetSwitchesForReplay).
	ttftDecodeTerm bool
	// ttftProcessorSharing is SwitchTTFTProcessorSharing (ttft.go): on from
	// the constructor; a replay turns it off (SetSwitchesForReplay).
	ttftProcessorSharing bool
	// ttftRouterWait is SwitchTTFTRouterWait (ttft.go): off from the
	// constructor, at whatif.DefaultPriceRouterWait; a replay turns it on.
	ttftRouterWait bool
	// ttftJointInputs is SwitchTTFTJointInputs (ttft_joint.go): on from the
	// constructor; a replay turns it off (SetSwitchesForReplay).
	ttftJointInputs bool
	// kvOutputDispersion is SwitchKVPerRequestOutputDispersion (issue ev-shindin/llm-scaling-manager#276,
	// output_dispersion.go): on from the constructor; a replay turns it off
	// (SetSwitchesForReplay).
	kvOutputDispersion bool
	// imbalance is each prefill variant's raw shares and decodeTTFT each
	// decode variant's measured TTFT, keyed namespace|model|variant, for the
	// TTFT pricing (ttft_fleet.go); decodeTerms is each decode variant's
	// readings of D and R, keyed the same. Under mu.
	imbalance   map[string]*imbalanceWindow
	decodeTTFT  map[string]*decodeTTFTWindow
	decodeTerms map[string]*decodeTermWindow
	// joint is each prefill variant's joint TTFT readings
	// (ttft_joint.go), under the same key. Under mu.
	joint map[string]*jointWindow
	// prompts is each prefill variant's prompt window (ttft.go), under the
	// same key, from the first row to carry a computed-token histogram.
	prompts map[string]*promptSpreadWindow
}

// The batch cap's readers in this analyzer (issue ev-shindin/llm-scaling-manager#420). Each took the cap
// from the engine's flags alone, so where the flags are unread the
// llm-d.ai/max-num-seqs annotation reached the saturation analyzer's P3 and
// not this one. Each now reads capacity.EngineParams.StatedBatchCap, behind
// its own switch. The saturation analyzer's learned cap is not read here:
// it is that analyzer's memory, which this one cannot see.

// SwitchBatchCapAdmission is the replay switch for the line's sequence-cap
// admission (issue ev-shindin/llm-scaling-manager#420). On, a replica is at its cap (itl.Admission) at the
// cap the flags or the annotation state (StatedBatchCap). Off, as before, at
// the flags' cap alone.
const SwitchBatchCapAdmission = "slo-batch-cap-admission"

// SwitchBatchCapPerRank is the replay switch for the batch cap a replica is
// priced at (issue ev-shindin/llm-scaling-manager#420). On, at the cap the flags or the annotation state,
// and the parser's default where neither does. Off, as before, at the
// flags' cap or the default (capacity.EngineParams.BatchCap).
const SwitchBatchCapPerRank = "slo-batch-cap-per-rank"

type acceleratorMemo struct {
	name     string
	lastUsed time.Time
}

// New returns an SLO analyzer. router may be nil, in which case the SLO is
// priced but never judged; store may be nil, in which case no engine batch cap
// applies and capacity comes from live readings only.
func New(router RouterLatencySource, store *capacity.Store) *Analyzer {
	return &Analyzer{
		router:       router,
		store:        store,
		now:          clock.RealClock{}.Now,
		stateTimeout: defaultStateTimeout,
		slopeHoldFor: capacity.HistoryRetention,
		lines:        itl.NewTracker(),
		seen:         make(map[string]time.Time),
		args:         make(map[string]string),
		accelerators: make(map[string]acceleratorMemo),
		transients:   make(map[string]*transientMemo),

		batchCapAdmission: true,
		batchCapPerRank:   true,

		ownSettle:            settle.NewTracker(),
		ownSettled:           make(map[string]time.Time),
		itlReadsSettle:       true,
		itlWaitsAfterRestart: true,
		itlKeepsSettle:       true,
		itlFiniteOnly:        true,
		itlRolloutGuard:      true,
		itlFilesReports:      true,
		lineLogged:           make(map[string]lineLogged),
		learnerReports:       make(map[string][]learn.Report),
		itlRestartUnderName:  true,
		itlIdleOnEvidence:    true,
		itlLongGapFirstSeen:  true,
		itlOwnBackdate:       true,
		itlForgetReachesLine: true,
		itlOneSidedAtCap:     true,
		itlTierHysteresis:    true,

		ttftPrice: true,

		ttftMeasuredSpread:   true,
		ttftMeasuredTransfer: true,
		ttftQuietTransfer:    true,
		ttftDecodeTerm:       true,
		ttftProcessorSharing: true,
		ttftRouterWait:       whatif.DefaultPriceRouterWait,
		ttftJointInputs:      true,
		kvOutputDispersion:   true,
		imbalance:            make(map[string]*imbalanceWindow),
		prompts:              make(map[string]*promptSpreadWindow),
		decodeTTFT:           make(map[string]*decodeTTFTWindow),
		decodeTerms:          make(map[string]*decodeTermWindow),
		joint:                make(map[string]*jointWindow),
	}
}

// WithClock sets the clock the analyzer stamps and ages its learned state on:
// the engine passes its cycle clock, which holds one reading per cycle. Call it
// before the analyzer is used.
func (a *Analyzer) WithClock(c clock.PassiveClock) *Analyzer {
	a.now = c.Now
	return a
}

// WithStateTimeout sets how long unused learned state is kept: the engine
// passes the saturation analyzer's variant timeout, so
// LEARNED_STATE_VARIANT_TIMEOUT governs both. A non-positive d keeps the
// default. Call it before the analyzer is used.
func (a *Analyzer) WithStateTimeout(d time.Duration) *Analyzer {
	if d > 0 {
		a.stateTimeout = d
	}
	return a
}

// WithSlopeHoldFor sets how long after it was fitted a line's slope may be held
// while the window has too few readings to fit: the engine passes the
// saturation analyzer's bucket retention, so LEARNED_STATE_BUCKET_RETENTION
// bounds the hold in both. A non-positive d keeps the default. Call it before
// the analyzer is used.
func (a *Analyzer) WithSlopeHoldFor(d time.Duration) *Analyzer {
	if d > 0 {
		a.slopeHoldFor = d
	}
	return a
}

// StateTimeout reports how long unused learned state is kept.
func (a *Analyzer) StateTimeout() time.Duration { return a.stateTimeout }

// Name implements domain.Analyzer.
func (a *Analyzer) Name() string { return domain.SLOAnalyzerName }

// Judging reports whether the analyzer can judge an SLO at all: whether it
// was given a source for the router's latency. Without one it still prices
// variants, and every objective stays unjudged.
func (a *Analyzer) Judging() bool { return a.router != nil }

// analyze is Analyze, and what the call read from the router and published,
// for the recorder.
func (a *Analyzer) analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, judged, error) {
	logger := ctrl.LoggerFrom(ctx)
	now := a.now()
	cfg, _ := input.Config.(*config.ScalingPolicy)
	if cfg == nil {
		return nil, judged{}, fmt.Errorf("slo analyzer: config is %T, want *config.ScalingPolicy", input.Config)
	}
	result := &domain.AnalyzerResult{
		AnalyzerName: domain.SLOAnalyzerName,
		ModelID:      input.ModelID,
		Namespace:    input.Namespace,
		AnalyzedAt:   now,
	}
	snap := metrics.SLOModelSnapshot{Namespace: input.Namespace, ModelName: input.ModelID}
	a.mu.Lock()
	delete(a.learnerReports, reportModel(input.Namespace, input.ModelID))
	a.mu.Unlock()
	// Without the engine's settle tracker, the analyzer observes its own
	// first, as the engine does before any analyzer runs (settle.go).
	a.observeOwnSettle(input, now)

	fleet := readFleet(input)
	if a.kvOutputDispersion {
		fleet.shape = fleet.shape.WithOutputDispersion(fleet.outputDispersion)
	}
	// An output length nobody has reported yet is not a demand of zero: it is
	// no demand at all, and a combiner must read it as undefined. So the demand
	// is left out, and every generating variant below reports no data.
	shapeKnown := fleet.shape.AvgOutputTokens > 0 && whatif.Finite(fleet.shape.AvgOutputTokens)
	demand := fleet.demand(input)
	if shapeKnown {
		result.TotalDemand = demand
		if fleet.disaggregated {
			result.RoleDemand = map[string]float64{domain.RoleDecode: demand}
		}
	}

	var tpotTarget float64
	if cfg.SLO != nil && cfg.SLO.TPOT != nil {
		tpotTarget = cfg.SLO.TPOT.Target.Seconds()
	}
	// The threshold the engine publishes this analyzer's target with: the slo
	// entry's own when it sets one, the tier's otherwise.
	scaleUp, _ := cfg.AnalyzerThresholds(domain.SLOAnalyzerName)
	var decodeLines []ttftDecodeLine
	for _, vs := range fleet.variants {
		key := a.variantKey(input.Namespace, input.ModelID, vs)
		vc, reading, d := a.priceVariant(input, vs, key, fleet, tpotTarget, scaleUp, now)
		a.noteLineLearner(logger, input.Namespace, input.ModelID, vs, key, d, now)
		if domain.CanonicalRole(vs.Role) == domain.RoleDecode {
			decodeLines = append(decodeLines, ttftDecodeLine{state: vs, detail: d})
		}
		if !shapeKnown && reading != nil {
			vc.PerReplicaCapacity, vc.Reason = 0, domain.ReasonNoData
			reading.Source = constants.SLOSourceNoOutputLength
			reading.Fill, reading.PredictedITL = math.NaN(), math.NaN()
		}
		if shapeKnown && fleet.generatingReplicas > 0 && isGenerating(vs.Role) {
			vc.TotalDemand = demand * float64(fleet.ready[vs.VariantName]) / float64(fleet.generatingReplicas)
		}
		// Over the Ready replicas the demand was split across, not
		// CurrentReplicas: a replica still starting serves none of it.
		if vc.PerReplicaCapacity > 0 && vc.ObservedReplicas > 0 {
			vc.Utilization = vc.TotalDemand / (float64(vc.ObservedReplicas) * vc.PerReplicaCapacity)
		}
		result.VariantCapacities = append(result.VariantCapacities, vc)
		if reading != nil {
			snap.Variants = append(snap.Variants, *reading)
			logger.V(logging.VERBOSE).Info("slo-variant",
				"modelID", input.ModelID, "namespace", input.Namespace,
				"variant", vs.VariantName, "role", vs.Role, "source", reading.Source,
				"fill", reading.Fill, "predictedITL", reading.PredictedITL,
				"perReplicaTokensPerSec", vc.PerReplicaCapacity, "reason", vc.Reason,
				// Which line, and what it was fitted from: the window's key,
				// the coefficients, and what the window was offered and held.
				"lineKey", key, "lineA", d.line.Model.A, "lineB", d.line.Model.B, "lineTier", d.line.Tier,
				"tierHeld", d.line.TierHeld,
				// A held slope reads pinned or pinned-default on the metric
				// like a fit made this cycle; these say it is an old one.
				"lineSlopeHeld", d.line.SlopeHeld, "lineSlopeAge", d.line.SlopeAge(now),
				"lineRolloutHeld", d.rolloutHeld,
				// Priced one-sided at the cap (SwitchITLOneSidedAtCap): no
				// learned B, and the prior's line (its intercept the prior on
				// B, capped at the window's least ITL, through the mean of the
				// readings past kSat at or past the fill priced) met the target
				// at a lower fill than the fit through the default B; and
				// whether that fill was past every such reading, where the
				// bound no longer holds.
				"linePinnedAtBPrior", d.line.PinnedAtBPrior, "linePriorBeyondReadings", d.line.PriorBeyondReadings,
				"considered", d.line.Considered, "held", d.line.Held, "kept", d.line.Offered,
				"belowBand", d.line.BelowBand,
				"aboveBand", d.line.AboveBand, "noK", d.line.NoK, "noITL", d.line.NoITL,
				"notReady", d.line.NotReady, "declined", d.line.Declined,
				// Of the kept, those out of the full fit (their replica's
				// load had moved) and those of a replica inside its settle
				// (the settle tracker: it joined, restarted, or load came
				// back), out of the pinned fit too.
				"unsteady", d.line.Unsteady, "settling", d.line.Settling,
				// The fill before the clamp and the batch cap, and the cap's inputs.
				"targetFill", d.targetFill, "capacityTokens", d.capTokens, "kvTokensPerRequest", fleet.shape.KVreq,
				"maxNumSeqs", d.maxSeqs, "maxNumActiveSeqs", d.activeSeqs, "engineRanks", d.ranks,
				"batchCap", d.batchCap, "batchCapSource", d.batchCapSource,
				// The cap the line admits a replica at, above the band (itl.Admission).
				"lineBatchCap", d.lineBatchCap, "lineBatchCapSource", d.lineBatchCapSource, "scaleUpThreshold", scaleUp)
		}
	}

	var j judged
	snap.Objectives, j.Latency, j.RouterCalled = a.judge(ctx, input, cfg.SLO)

	// After the loop, whatever order the variants sort in: a prefill variant
	// reads its decode variant's line. After the judge: the decode term reads
	// the router's mean it collected.
	snap.TTFT = a.priceTTFTVariants(logger, input, cfg, fleet, decodeLines, j.Latency, now)

	// A window that covered a wake or a starting replica is not judged: the
	// requests in it waited out capacity no steady-state sizing could have had
	// ready. The request count is still published, and the reason is.
	window, grace := time.Duration(routerWindowSeconds(snap.Objectives))*time.Second, transientGrace(input.ReplicaMetrics)
	ttftExcluded, tpotExcluded := a.noteTransient(input.Namespace+"/"+input.ModelID, input.VariantStates, window, grace, now)
	if logger.V(logging.VERBOSE).Enabled() {
		t := a.transientDetail(input.Namespace + "/" + input.ModelID)
		// Why a window is not judged, and until when: the mark's reason and
		// time, per scope, and the cycles since the model was last analyzed.
		logger.V(logging.VERBOSE).Info("slo-transient",
			"modelID", input.ModelID, "namespace", input.Namespace,
			"ttftExcluded", ttftExcluded, "tpotExcluded", tpotExcluded,
			"allMark", t.all.reason, "allMarkAt", t.all.at, "generatingMark", t.generating.reason,
			"generatingMarkAt", t.generating.at, "holdFor", window+grace,
			"cyclesSinceAnalyzed", t.cyclesSinceAnalyzed, "pendingSince", t.pendingSince)
	}
	for i := range snap.Objectives {
		o := &snap.Objectives[i]
		o.Excluded = tpotExcluded
		if o.Objective == constants.SLOObjectiveTTFT {
			o.Excluded = ttftExcluded
		}
		if o.Excluded != "" {
			o.Observed, o.Attainment = math.NaN(), math.NaN()
		}
	}
	for _, o := range snap.Objectives {
		logger.V(logging.VERBOSE).Info("slo-model",
			"modelID", input.ModelID, "namespace", input.Namespace,
			"objective", o.Objective, "statistic", o.Statistic,
			"target", o.TargetSeconds, "observed", o.Observed, "attainment", o.Attainment,
			"requests", o.Requests, "excluded", o.Excluded,
			"demandTokensPerSec", demand, "outputTokens", fleet.shape.AvgOutputTokens)
	}
	metrics.PublishSLO(snap, now)
	j.Published = snap
	return result, j, nil
}

// pricingDetail is what a variant was priced from, for its log line.
type pricingDetail struct {
	line itl.Reading
	// source is where the line came from (fitLine), and band the fill range
	// it is fitted over; the TTFT pricing reads both, with or without a TPOT
	// target.
	source     string
	band       whatif.FillBand
	targetFill float64
	capTokens  float64
	// maxSeqs is max_num_seqs times ranks, and activeSeqs is
	// max_num_active_seqs times ranks (0 when unset). The replica's batch cap
	// is the lower of the two.
	maxSeqs    int64
	activeSeqs int64
	ranks      int64
	// batchCap is the cap per rank the replica was priced at, and
	// batchCapSource where it came from: batchCapFlags, batchCapDeclared
	// (the llm-d.ai/max-num-seqs annotation) or batchCapDefault (the
	// parser's, the flags unread), or batchCapNone with no params at all.
	// maxSeqs is max_num_seqs alone, the default where the flags are
	// unread, so it is not what priced a replica whose cap is declared
	// (issue ev-shindin/llm-scaling-manager#420).
	batchCap       int64
	batchCapSource string
	// lineBatchCap is the cap per rank the line admits a replica at above
	// its band (itl.Admission.MaxNumSeqs), and lineBatchCapSource where it
	// came from: batchCapFlags, batchCapDeclared or batchCapNone. Never a
	// default, so it differs from batchCap where the flags are unread and
	// nothing is declared.
	lineBatchCap       int64
	lineBatchCapSource string
	// rolloutHeld is the line held on the old pods: the variant rolls out and
	// no new pod serves (itl.RolloutSight.Held).
	rolloutHeld bool
}

// Where the batch cap a replica is priced at came from (pricingDetail), as
// the saturation analyzer names it on its k2-decision line.
const (
	batchCapFlags    = "flags"
	batchCapDeclared = "declared"
	batchCapDefault  = "default"
	// batchCapNone is no cap to read: for the line's admission, none the
	// flags or the annotation state; for the price, none at all (the scale
	// target unread). Nothing is at it, and nothing is capped by it. The
	// throughput analyzer logs the same value (itl.MaxNumSeqsSourceNone).
	batchCapNone = itl.MaxNumSeqsSourceNone
)

// priceVariant fits the variant's line and prices one replica of it at the
// TPOT target. The reading is nil for a variant the TPOT objective does not
// describe: a prefill role, or a policy with no TPOT target.
func (a *Analyzer) priceVariant(input domain.AnalyzerInput, vs domain.VariantReplicaState, key string,
	fleet fleetReading, tpotTarget, scaleUp float64, now time.Time) (domain.VariantCapacity, *metrics.SLOVariantReading, pricingDetail) {
	d := pricingDetail{targetFill: math.NaN(), capTokens: math.NaN()}
	vc := domain.VariantCapacity{
		VariantName:      vs.VariantName,
		Role:             vs.Role,
		ReplicaCount:     vs.CurrentReplicas,
		PendingReplicas:  vs.PendingReplicas,
		StuckReplicas:    vs.StuckReplicas,
		PendingAges:      vs.PendingAges,
		ObservedReplicas: fleet.ready[vs.VariantName],
		Reason:           domain.ReasonNoData,
	}
	// Prefill has no TPOT (the P/D sidecar asks it for one token), and its
	// TTFT is priced in shadow only (ttft.go), so the result carries nothing
	// of it. Left as no-data, so a combiner treats it as undefined rather
	// than as zero.
	if !isGenerating(vs.Role) {
		return vc, nil, d
	}
	params := a.engineParams(input.Namespace, input.ModelID, vs.VariantName)
	// Analyze has checked the config's type; a direct caller may pass none,
	// which reads as the defaults.
	cfg, _ := input.Config.(*config.ScalingPolicy)
	adm := admission(cfg, params, a.batchCapAdmission)
	d.lineBatchCap, d.lineBatchCapSource = adm.MaxNumSeqs, adm.MaxNumSeqsSource
	// A held slope ends on the retention and on a change of engine arguments
	// (itl.Admission), and a change of arguments forgets the window first, so
	// the old engine's readings are neither refitted nor held as the new one's.
	args := params.ArgsTag()
	rows, held := a.lineRows(input, fleet, key, vs.VariantName)
	d.rolloutHeld = held
	if held {
		args = a.heldArgs(key, args)
	} else {
		a.linesUnderArgs(key, args, input.Namespace, input.ModelID, vs.VariantName, now)
	}
	adm.SlopeHoldFor, adm.Args = a.slopeHoldFor, args
	// When each replica's settle began: the engine's settle tracker
	// (settle.go).
	a.settleAdmission(&adm, input.Namespace, input.ModelID)
	// Readings at the cap sit above kSat, where a pinned fit through the
	// default B prices under the truth: priced one-sided until B is learned
	// (one_sided.go).
	a.oneSided(&adm, tpotTarget, a.priorScope(key, args, input.VariantStates))
	a.tierPrice(&adm, tpotTarget, capacityInputs{tokens: fleet.capacityTokens(vs.VariantName, params),
		kvPerRequest: fleet.shape.KVreq, ranks: fleet.engineRanks(vs.VariantName)}, params)
	// The fill is clamped to the band the line is fitted over.
	band := whatif.FillBand{Lo: adm.MinK, Hi: adm.MaxK}
	line, source, r := a.fitLine(key, rows, vs.VariantName, adm, now)
	d.line, d.source, d.band = r, source, band
	if !(tpotTarget > 0) {
		return vc, nil, d
	}
	reading := newVariantReading(vs.VariantName, source)
	if source == constants.SLOSourceNoLine || source == constants.SLOSourceDeclined {
		return vc, reading, d
	}
	p := whatif.PriceTPOT(band, line, tpotTarget)
	d.targetFill = p.Target
	switch {
	case !p.OK:
		reading.Source = constants.SLOSourceInputInvalid
		vc.Reason = domain.ReasonError
		return vc, reading, d
	case !p.Feasible:
		// Abstain, never cap: holding an infeasible variant at its count would
		// freeze a single-variant fleet as load rises.
		reading.Source = constants.SLOSourceInfeasible
		return vc, reading, d
	}
	// The batch cap bounds one engine: max_num_seqs, lowered by
	// max_num_active_seqs when set (issue ev-shindin/llm-scaling-manager#335). A replica of several
	// data-parallel ranks admits that many times it, against capacity tokens
	// that are already the whole replica's (issue ev-shindin/llm-scaling-manager#150).
	d.ranks = fleet.engineRanks(vs.VariantName)
	if params != nil {
		d.maxSeqs = params.MaxNumSeqs * d.ranks
		d.activeSeqs = params.MaxNumActiveSeqs * d.ranks
	}
	d.batchCap, d.batchCapSource = a.pricedBatchCap(params)
	maxSeqs := d.batchCap * d.ranks
	capTokens := fleet.capacityTokens(vs.VariantName, params)
	d.capTokens = capTokens
	fill := whatif.ReachableFill(band, p.Fill, capTokens, fleet.shape.KVreq, maxSeqs)
	rate := whatif.ReplicaTokenRate(line, fill, capTokens, fleet.shape.KVreq, maxSeqs)
	if !(rate > 0) {
		reading.Source = constants.SLOSourceInputInvalid
		vc.Reason = domain.ReasonError
		return vc, reading, d
	}
	vc.PerReplicaCapacity = rate
	vc.Reason = "slo-" + source
	reading.Fill = fill
	if k := whatif.RealisedFill(band, line, fill, scaleUp, capTokens, fleet.shape.KVreq, maxSeqs); whatif.Finite(k) {
		reading.PredictedITL = line.ITLAt(k)
	}
	return vc, reading, d
}

// pricedBatchCap is the cap per rank a replica is priced at, and where it came
// from: the flags, the annotation, the parser's default, or none.
func (a *Analyzer) pricedBatchCap(params *capacity.EngineParams) (int64, string) {
	batchCap, source := params.BatchCap(), batchCapDefault
	if params.ExplicitMaxNumSeqs() > 0 {
		source = batchCapFlags
	} else if stated := params.StatedBatchCap(); a.batchCapPerRank && stated > 0 {
		batchCap, source = stated, batchCapDeclared
	}
	if batchCap <= 0 {
		source = batchCapNone
	}
	return batchCap, source
}

// admission is which rows teach the variant's line: the policy's ITL band, and
// the sequence cap only when something states it (itl.Admission): with
// readsDeclared (SwitchBatchCapAdmission) the flags or the
// llm-d.ai/max-num-seqs annotation (StatedBatchCap), without it the flags
// alone. The saturation analyzer builds the same from the same two sources
// (atCapBatchCap) and never from its learned cap, so both analyzers read a
// variant's rows alike.
func admission(cfg *config.ScalingPolicy, params *capacity.EngineParams, readsDeclared bool) itl.Admission {
	if cfg == nil {
		cfg = &config.ScalingPolicy{}
	}
	lo, hi := cfg.ITLBand()
	batchCap, source := params.ExplicitMaxNumSeqs(), batchCapFlags
	if batchCap == 0 && readsDeclared {
		batchCap, source = params.StatedBatchCap(), batchCapDeclared
	}
	if batchCap == 0 {
		source = batchCapNone
	}
	return itl.Admission{MinK: lo, MaxK: hi, MaxNumSeqs: batchCap, MaxNumSeqsSource: source,
		CapMaxPreemptionRate: cfg.ITLCapPreemptionRate(), Steady: cfg.ITLSteadiness()}
}

// newVariantReading is a variant's reading with every figure unknown: NaN,
// which PublishSLO does not publish. The one place readings are made, because a
// field left at Go's zero value is published as a real 0: a fill of zero, or a
// predicted ITL of zero, which reads as a met target.
func newVariantReading(variant, source string) *metrics.SLOVariantReading {
	nan := math.NaN()
	return &metrics.SLOVariantReading{Variant: variant, Source: source, Fill: nan, PredictedITL: nan}
}

// fitLine fits one variant's ITL(k) = A*k + B with the shared itl.Tracker --
// the saturation analyzer's rules, by construction -- and names where the line
// came from: a full fit, B pinned to this key's own last full fit, or B pinned
// to the default floor; or why there is none: the window held enough readings
// and the pinned fit refused them, or it holds too few.
func (a *Analyzer) fitLine(key string, rows []domain.ReplicaMetrics, variant string, adm itl.Admission, now time.Time) (itl.Model, string, itl.Reading) {
	a.mu.Lock()
	a.seen[key] = now
	a.mu.Unlock()
	adm.TierHysteresis = a.tierHysteresisOn()
	r := a.lines.Observe(key, variant, rows, adm, now)
	a.paths.countFit(r.Tier)
	switch {
	case r.Tier == itl.TierOLS:
		return r.Model, constants.SLOSourceOwn, r
	case r.Tier == itl.TierPinnedB && r.BaselineLearned:
		return r.Model, constants.SLOSourcePinned, r
	case r.Tier == itl.TierPinnedB:
		// Not a measurement of this hardware: a constant for another card.
		// Labelled apart, so the line it pins is not read as one.
		return r.Model, constants.SLOSourcePinnedDefault, r
	case r.Declined:
		// Not "too few yet": a steady state. Most often a fast card whose ITL
		// sits below the pinned intercept, so the slope comes out negative.
		return itl.Model{}, constants.SLOSourceDeclined, r
	}
	return itl.Model{}, constants.SLOSourceNoLine, r
}

// routerWindowSeconds is the rate window the router figures were taken over:
// the collector's, carried on the reading, and the collector's default when no
// figure came back.
func routerWindowSeconds(readings []metrics.SLOObjectiveReading) float64 {
	for _, r := range readings {
		if r.WindowSeconds > 0 {
			return r.WindowSeconds
		}
	}
	return defaultRouterWindowSeconds
}

// defaultRouterWindowSeconds matches registration.SLORouterWindowSeconds, the
// window the router queries use. Spelled here so the analyzer does not import
// the collector's registration package for one number.
const defaultRouterWindowSeconds = 300.0

// judge reads the router's latency for the model and reports each objective:
// the TPOT mean, and the TTFT percentile with its attainment. A figure from too
// few requests is NaN, which is not published. It returns, for the recorder, the
// latency it read and whether it asked the router at all.
func (a *Analyzer) judge(ctx context.Context, input domain.AnalyzerInput, slo *config.SLOPolicy) ([]metrics.SLOObjectiveReading, *domain.RouterLatency, bool) {
	if slo.Empty() {
		return nil, nil, false
	}
	var rl *domain.RouterLatency
	if a.router != nil {
		rl = a.router.CollectRouterLatency(ctx, input.ModelID, input.Namespace)
	}
	// While a learner recording is on, the router latency may hold only the
	// recorded histograms. That is no reading: judged, and recorded, as the
	// nil it was before they were collected.
	if !rl.Judgeable() {
		rl = nil
	}
	var out []metrics.SLOObjectiveReading
	if slo.TPOT != nil {
		r := metrics.SLOObjectiveReading{
			Objective: constants.SLOObjectiveTPOT, Statistic: "mean",
			TargetSeconds: slo.TPOT.Target.Seconds(), Observed: math.NaN(), Attainment: math.NaN(),
			Requests: math.NaN(),
		}
		if rl != nil {
			r.WindowSeconds = rl.WindowSeconds
			r.Requests = rl.TPOTCountRate * rl.WindowSeconds
			// A mean of 0 is no reading: it would count as met.
			if r.Requests >= minSamples(0) && rl.TPOTSumRate > 0 {
				r.Observed = rl.TPOTSumRate / rl.TPOTCountRate
			}
		}
		out = append(out, r)
	}
	if slo.TTFT != nil {
		p := slo.TTFTPercentile()
		r := metrics.SLOObjectiveReading{
			Objective: constants.SLOObjectiveTTFT, Statistic: "p" + strconv.Itoa(p),
			TargetSeconds: slo.TTFT.Target.Seconds(), Observed: math.NaN(), Attainment: math.NaN(),
			ObjectiveRatio: float64(p) / 100, Requests: math.NaN(),
		}
		if rl != nil {
			r.WindowSeconds = rl.WindowSeconds
			if h, ok := newHistogram(rl.TTFTBuckets); ok {
				r.Requests = h.total * rl.WindowSeconds
				if r.Requests >= minSamples(p) {
					r.Observed = h.quantile(float64(p) / 100)
					r.Attainment, _ = h.fractionWithin(r.TargetSeconds)
				}
			} else if slices.ContainsFunc(rl.TTFTBuckets, func(b domain.LatencyBucket) bool { return math.IsInf(b.UpperBound, 1) }) &&
				!slices.ContainsFunc(rl.TTFTBuckets, func(b domain.LatencyBucket) bool { return b.Rate != 0 }) {
				// The router reported the histogram and nobody was served: a
				// count of 0, not an absent one. Absent reads as "the router's
				// series are missing", which an idle model's are not.
				r.Requests = 0
			}
		}
		out = append(out, r)
	}
	return out, rl, a.router != nil
}

// linesUnderArgs forgets key's line -- its window, intercept and held slope --
// when the variant now runs under other engine arguments than the ones the line
// was learned under, as the saturation analyzer's itlUnderArgs does. Without
// it the window kept the old engine's readings for up to
// itl.DefaultObservationMaxAge, the pinned fit refitted A from them each cycle
// and stamped it with the new arguments, and the old engine's slope was then
// held as the new one's for the whole slope hold (issue ev-shindin/llm-scaling-manager#284). Unknown
// arguments change nothing. There is no stash: a rollback relearns its line.
func (a *Analyzer) linesUnderArgs(key, args, namespace, modelID, variant string, now time.Time) {
	if args == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	prev, ok := a.args[key]
	a.args[key] = args
	if !ok || prev == args {
		return
	}
	// The tracker's lock is taken under a.mu, as EvictStale takes it.
	_, _, forgotten := a.lines.ForgetKeys(now, learn.ByArguments, func(k string) bool { return k == key })
	a.noteForgottenLocked(namespace, modelID, variant, forgotten)
}

func (a *Analyzer) engineParams(namespace, modelID, variant string) *capacity.EngineParams {
	if a.store == nil {
		return nil
	}
	if rec := a.store.Get(namespace, modelID, variant); rec != nil {
		return rec.EngineParams
	}
	return nil
}

// evictStale forgets the learned state of variants and models not analyzed or
// reported present for the state timeout. The engine calls it with the
// saturation analyzer's sweep on every cycle that has a model to plan, whether
// or not any model lists this analyzer, so each key is visited once per cycle
// rather than once per analyzed model. A cycle with no active model does not
// sweep (the engine returns before it), so the timeout counts reconciled time,
// as the saturation analyzer's does.
func (a *Analyzer) evictStale(now time.Time) Evicted {
	a.mu.Lock()
	defer a.mu.Unlock()
	stale := func(key string) bool {
		at, ok := a.seen[key]
		return !ok || now.Sub(at) > a.stateTimeout
	}
	var ev Evicted
	var evicted []learn.Forgotten
	ev.Lines, ev.Baselines, evicted = a.lines.ForgetKeys(now, learn.ByEvicted, stale)
	a.noteEvictedLocked(evicted)
	for key := range a.seen {
		if stale(key) {
			delete(a.seen, key)
		}
	}
	for key := range a.args {
		if stale(key) {
			delete(a.args, key)
		}
	}
	for key, m := range a.transients {
		if now.Sub(m.seenAt) > a.stateTimeout {
			delete(a.transients, key)
			ev.Transients++
		}
	}
	for key, m := range a.accelerators {
		if now.Sub(m.lastUsed) > a.stateTimeout {
			delete(a.accelerators, key)
			ev.Accelerators++
		}
	}
	for key, w := range a.imbalance {
		if now.Sub(w.seen) > a.stateTimeout {
			delete(a.imbalance, key)
			ev.TTFTWindows++
		}
	}
	for key, w := range a.decodeTTFT {
		if now.Sub(w.seen) > a.stateTimeout {
			delete(a.decodeTTFT, key)
			ev.TTFTWindows++
		}
	}
	for key, w := range a.decodeTerms {
		if now.Sub(w.seen) > a.stateTimeout {
			delete(a.decodeTerms, key)
			ev.TTFTWindows++
		}
	}
	for key, w := range a.prompts {
		if now.Sub(w.seen) > a.stateTimeout {
			delete(a.prompts, key)
			ev.TTFTWindows++
		}
	}
	for key, w := range a.joint {
		if now.Sub(w.seen) > a.stateTimeout {
			delete(a.joint, key)
			ev.TTFTWindows++
		}
	}
	a.evictOwnSettleLocked(now)
	return ev
}

// Evicted counts what one EvictStale forgot, per map.
type Evicted struct {
	Lines, Baselines, Transients, Accelerators int
	// TTFTWindows counts the imbalance, decode-TTFT and prompt windows.
	TTFTWindows int `learnrec:"omitzero"`
}

// Any reports whether anything was evicted.
func (e Evicted) Any() bool {
	return e.Lines+e.Baselines+e.Transients+e.Accelerators+e.TTFTWindows > 0
}

// beginCycle marks the start of an engine cycle. The engine calls it once per
// cycle, before any model is analyzed and whether or not any is: a model not
// analyzed on the cycle before the one it is analyzed in was skipped, and its
// window is not judged (unobserved).
func (a *Analyzer) beginCycle() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cycle++
}

// noteVariantsPresent stamps the learned state of a model's variants that
// were discovered this cycle, analyzed or not: a variant parked at zero is
// not analyzed, and must not lose its line for it.
func (a *Analyzer) noteVariantsPresent(namespace, modelID string, variantNames []string) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, name := range variantNames {
		// The accelerator memo too, as saturation's presence does: a variant
		// parked past the state timeout would otherwise wake with no memo, and a
		// first cycle of unresolved readings would key a new, empty line.
		if m, ok := a.accelerators[namespace+"/"+name]; ok {
			m.lastUsed = now
			a.accelerators[namespace+"/"+name] = m
		}
		prefix := namespace + "|" + modelID + "|" + name + "|"
		for key := range a.seen {
			if strings.HasPrefix(key, prefix) {
				a.seen[key] = now
			}
		}
		if w := a.imbalance[ttftWindowKey(namespace, modelID, name)]; w != nil {
			w.seen = now
		}
		if w := a.decodeTTFT[ttftWindowKey(namespace, modelID, name)]; w != nil {
			w.seen = now
		}
		if w := a.decodeTerms[ttftWindowKey(namespace, modelID, name)]; w != nil {
			w.seen = now
		}
		if w := a.prompts[ttftWindowKey(namespace, modelID, name)]; w != nil {
			w.seen = now
		}
		if w := a.joint[ttftWindowKey(namespace, modelID, name)]; w != nil {
			w.seen = now
		}
	}
	if m, ok := a.transients[namespace+"/"+modelID]; ok {
		m.seenAt = now
	}
}

// variantKey is windowKey with the stable accelerator in it.
func (a *Analyzer) variantKey(namespace, modelID string, vs domain.VariantReplicaState) string {
	vs.AcceleratorName = a.stableAccelerator(namespace, vs.VariantName, vs.AcceleratorName)
	return windowKey(namespace, modelID, vs)
}

// stableAccelerator is the accelerator to key a variant's line under: the
// reading when it resolves, else the last one that did. The pods of one
// variant disagreeing about it for a cycle is not new hardware, and keying on
// the raw reading opened a fresh, empty window every time they did. The same
// rule as the saturation analyzer's, kept per analyzer like the lines.
func (a *Analyzer) stableAccelerator(namespace, variant, accelerator string) string {
	key := namespace + "/" + variant
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if constants.IsAcceleratorResolved(accelerator) {
		a.accelerators[key] = acceleratorMemo{name: accelerator, lastUsed: now}
		return accelerator
	}
	if last, ok := a.accelerators[key]; ok {
		last.lastUsed = now
		a.accelerators[key] = last
		return last.name
	}
	return accelerator
}

// windowKey names one variant's line. The accelerator and GPU count are part
// of it, because ITL(k) is a property of the hardware a replica runs on.
func windowKey(namespace, modelID string, vs domain.VariantReplicaState) string {
	return namespace + "|" + modelID + "|" + vs.VariantName + "|" + vs.AcceleratorName + "|" +
		strconv.Itoa(vs.GPUsPerReplica)
}

func isGenerating(role string) bool {
	return role != domain.RolePrefill
}

// fleetReading is what the analyzer reads off one model's replicas once per
// cycle.
type fleetReading struct {
	variants      []domain.VariantReplicaState
	disaggregated bool
	// ready counts each variant's Ready, non-pool replica rows, every role
	// included: it is the variant's observed replicas, and a prefill variant
	// reported as 0 read as a scrape gap.
	ready map[string]int
	// generatingReplicas is the total of ready over decode and aggregated
	// variants.
	generatingReplicas int
	// shape is the request shape the generating replicas are serving.
	shape shape.Shape
	// outputDispersion is the spread of their output lengths, D =
	// E[O^2]/E[O]^2, with shape's weights; 0 unless every row shape counted
	// read the bound on E[O^2] (output_dispersion.go).
	outputDispersion float64
	// requestRate is the generating replicas' summed completion rate, the
	// fallback when the router reports no arrival rate.
	requestRate float64
	rows        []domain.ReplicaMetrics
	// mixed names the variants whose replicas report more than one revision
	// (domain.RevisionsMixed).
	mixed map[string]bool
}

func readFleet(input domain.AnalyzerInput) fleetReading {
	f := fleetReading{ready: make(map[string]int), rows: input.ReplicaMetrics,
		mixed: domain.RevisionsMixed(input.ReplicaMetrics)}
	f.variants = append(f.variants, input.VariantStates...)
	sort.Slice(f.variants, func(i, j int) bool { return f.variants[i].VariantName < f.variants[j].VariantName })
	roles := make(map[string]string, len(f.variants))
	for _, vs := range f.variants {
		roles[vs.VariantName] = vs.Role
		if vs.Role == domain.RolePrefill || vs.Role == domain.RoleDecode {
			f.disaggregated = true
		}
	}
	// Request-rate weighted, so a replica serving most of the traffic decides
	// the shape; equal weights when no replica reports a rate.
	var in, out, hit, weight float64
	for _, rm := range input.ReplicaMetrics {
		if !rm.Ready || rm.FromWarmPool {
			continue
		}
		f.ready[rm.VariantName]++
		if !isGenerating(roles[rm.VariantName]) {
			continue
		}
		f.generatingReplicas++
		if whatif.Finite(rm.RequestRate) && rm.RequestRate > 0 {
			f.requestRate += rm.RequestRate
		}
	}
	weightOf := func(rm domain.ReplicaMetrics) float64 {
		if f.requestRate > 0 {
			return rm.RequestRate
		}
		return 1
	}
	// counted is a row the shape counts. A row with any non-finite shape
	// figure is no reading of the shape: one NaN summed in would turn the
	// whole fleet's shape into NaN.
	counted := func(rm domain.ReplicaMetrics) bool {
		w := weightOf(rm)
		return rm.Ready && !rm.FromWarmPool && isGenerating(roles[rm.VariantName]) &&
			w > 0 && whatif.Finite(w) && whatif.Finite(rm.AvgOutputTokens) && rm.AvgOutputTokens > 0 &&
			whatif.Finite(rm.AvgInputTokens) && whatif.Finite(rm.PrefixCacheHitRate)
	}
	for _, rm := range input.ReplicaMetrics {
		if !counted(rm) {
			continue
		}
		w := weightOf(rm)
		in += w * rm.AvgInputTokens
		out += w * rm.AvgOutputTokens
		hit += w * rm.PrefixCacheHitRate
		weight += w
	}
	if weight > 0 {
		f.shape = shape.New(in/weight, out/weight, hit/weight)
		// Over the same rows with the same weights (fleet.OutputDispersion):
		// 0, no reading, unless every one of them read the bound.
		if d, ok := fleet.OutputDispersion(input.ReplicaMetrics, counted, fleet.WeightedBy(weightOf)); ok {
			f.outputDispersion = d
		}
	}
	return f
}

// demand is the decode work arriving, in output tokens per second: the arrival
// rate times the output length, plus the router's backlog drained over
// backlogDrainSeconds. Zero when the output length is unknown.
func (f fleetReading) demand(input domain.AnalyzerInput) float64 {
	o := f.shape.AvgOutputTokens
	if !(o > 0) || !whatif.Finite(o) {
		return 0
	}
	lambda := input.ArrivalRate
	if !(lambda > 0) || !whatif.Finite(lambda) {
		lambda = f.requestRate
	}
	d := lambda * o
	if q := input.SchedulerQueue; q != nil && q.QueueSize > 0 {
		d += float64(q.QueueSize) * o / backlogDrainSeconds
	}
	return d
}

// capacityTokens is the mean KV capacity of the variant's ready replicas, in
// tokens: the live reading where there is one, the engine's configured
// override otherwise (whatif.CapacityTokens, which the saturation analyzer
// reads them with too).
func (f fleetReading) capacityTokens(variant string, params *capacity.EngineParams) float64 {
	var sum float64
	var n int
	for _, rm := range f.rows {
		if rm.VariantName != variant || !rm.Ready || rm.FromWarmPool {
			continue
		}
		if c := whatif.CapacityTokens(params, rm.Ranks(), rm.TotalKvCapacityTokens); c > 0 {
			sum += c
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// engineRanks is the data-parallel engine ranks one of the variant's ready
// replicas runs, over the rows capacityTokens reads (ready, not a warm-pool
// bridge): the rounded mean of their counts, and one when there is no such
// row. A row with no count is one rank. Every replica of a variant
// runs one layout, so the mean is that layout's count; the rounding only keeps
// a row caught mid-rollout from making the cap fractional.
func (f fleetReading) engineRanks(variant string) int64 {
	var sum, n int
	for _, rm := range f.rows {
		if rm.VariantName != variant || !rm.Ready || rm.FromWarmPool {
			continue
		}
		sum += rm.Ranks()
		n++
	}
	if n == 0 {
		return 1
	}
	return max(int64(math.Round(float64(sum)/float64(n))), 1)
}
