package whatifapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The backtest (POST /v1/backtest, capacity-planner backtest): the planner
// fitted on one window predicts the TTFT and TPOT of another it did not see,
// step by step, from that window's inputs alone (its rate, its lengths, the
// replicas each role ran), and each prediction is set beside what the fleet
// measured. Nothing new is modelled: each step is the fleet a report prices
// (Pricer.Fleet) at the step's rate.
//
// The test window's latency (LatencySample) is read only by measuredOf: the
// prediction of a step (predict) is given the step's inputs and the fitted
// Pricer, never the latency, so a measured figure cannot reach a predicted
// one. TestBacktest_MeasuredLatencyDoesNotReachThePrediction holds that.

// LatencySample is one cycle's measured latency, server side: the router's
// histograms where the source carries them, and the engines' own
// (learnreplay.CycleLatency). Resolved.Latency[i] is the cycle of
// Resolved.Series[i].
type LatencySample struct {
	At     time.Time
	Router *domain.RouterLatency
	// EngineTTFT is the engines' TTFT histogram (the prefill pods' on a P/D
	// fleet), EngineTTFTMean their mean; EngineTPOT the decode pods' TPOT
	// histogram and EngineITLMean their mean inter-token latency.
	EngineTTFT     []domain.LatencyBucket
	EngineTTFTMean float64
	EngineTPOT     []domain.LatencyBucket
	EngineITLMean  float64
}

// ConfigField is one setting a run's figures depend on, by name ("model",
// "decode accelerator"), and its value; "" is not recorded.
type ConfigField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ConfigFlowControl is the field a RunConfig names the flow-control series
// by; a backtest reads it over each side's cycles, not at one.
const ConfigFlowControl = "flow-control series"

// RunConfig is what the source says of the configuration the fleet ran: the
// model, each role's accelerator, parallelism and the engine flags that move
// its latency, and the router's flow-control state.
type RunConfig struct {
	Fields []ConfigField
}

// The measured sources a backtest scores against.
const (
	MeasuredAuto   = "auto"
	MeasuredRouter = "router"
	MeasuredEngine = "engine"
)

// The backtest's defaults and bounds.
const (
	DefaultBacktestStep   = 5 * time.Minute
	minBacktestStep       = time.Minute
	maxBacktestStep       = time.Hour
	DefaultBacktestSettle = 2 * time.Minute
	maxBacktestSettle     = time.Hour
	// DefaultBacktestSplit is the share of a recording fitted on when one
	// recording is split.
	DefaultBacktestSplit = 0.6
	// BacktestTolerance is the "within" share: a prediction within 20% of
	// the measured figure.
	BacktestTolerance = 0.2
	// maxBacktestSteps bounds a backtest's steps.
	maxBacktestSteps = 2000
)

// The reasons a step is not judged at all.
const (
	ExcludedNoTraffic     = "noTraffic"
	ExcludedNoMeasurement = "noMeasurement"
	ExcludedScaling       = "scaling"
	ExcludedWarmUp        = "warmUp"
	ExcludedNotPriced     = "notPriced"
	// ExcludedOtherVariants is a step in which variants other than the
	// priced ones also served: the model's rate and the router's latency
	// are the whole model's, the replicas priced are not.
	ExcludedOtherVariants = "otherVariants"
	// ExcludedBurst is a step whose measurement window runs both near the
	// knee and far under it (burstIn): most often the start or the end of a
	// load near the knee, though an on-off load reads the same. Its requests
	// mostly arrived at that load, or queued behind it, and its mean rate is
	// no rate the fleet ran at.
	ExcludedBurst = "burst"
)

// BurstPeakKneeShare and BurstTroughShare are what makes a step's window a
// burst (burstIn): a cycle at BurstPeakKneeShare of the knee or more, and
// another at BurstTroughShare of that cycle's rate or less. A steady step
// near the knee passes, bursty arrivals included: on the Qwen3-32B
// staircase's C phases (Ca^2 4, 3.2 to 3.5 r/s) a window's one-minute
// rates fell to 0.38 of its busiest minute and no lower. Where a load
// starts or stops in the window, they fell to 0.17 or less: the warm-up's
// two steps on the burst read from 0.04 and 0 r/s to 4.3 r/s, against a
// knee of 4.08 r/s. Stated, not fitted
// (llm-d-incubation/llm-d-scaling-manager#278).
const (
	BurstPeakKneeShare = 0.8
	BurstTroughShare   = 0.25
)

// The reasons one figure of a judged step is not scored.
const (
	UnscoredFewRequests    = "fewRequests"
	UnscoredAboveHistogram = "aboveHistogram"
	UnscoredRefused        = "refused"
	UnscoredNotMeasured    = "notMeasured"
	// UnscoredNotComparable is a measured figure that is not the one
	// predicted: on a P/D fleet the engines' TTFT (the prefill pods'
	// alone), and the engines' mean inter-token latency for a mean TPOT.
	UnscoredNotComparable = "notComparable"
	// UnscoredUnsettled is a TTFT whose prefill queue does not settle inside
	// the step's measurement window: its relaxation time is more than
	// SettleShare of the window (BacktestStep.RelaxationSeconds). The wait
	// is correlated over about one relaxation time, so the window holds
	// fewer than 1/SettleShare independent readings of it: its TTFT is a
	// noisy sample of the steady state the planner prices.
	UnscoredUnsettled = "unsettled"
	// UnscoredBurst is a TTFT whose measurement window holds the start or
	// the end of a load near a prefill knee that rests on a lower bound of
	// V_P (BacktestStep.TTFTBurst): the step is judged on decode's own knee,
	// and only its TTFT, which a queue building near that knee may set, is
	// not scored.
	UnscoredBurst = "burst"
	// UnscoredUnresolvable is a measured percentile that fell in a histogram
	// bucket too wide to place it (MeasuredBucketWidth) while the prediction
	// fell inside the same bucket: interpolated inside the bucket, the
	// measured figure reads the bucket's grid, not the traffic, and the
	// bucket cannot say which side of the prediction the truth is.
	UnscoredUnresolvable = "unresolvable"
	// UnscoredVPLowerBound is a TTFT priced from a V_P that is only a lower
	// bound (TTFTFromLowerBoundVP) that read high, or at most
	// BacktestTolerance low: the direction its V_P terms push it. The pair
	// keeps its ratio but is scored apart from the point figures
	// (BacktestScore.VPLowerBound), so a coverage gain made of such figures
	// does not read as an accuracy loss. It is no bound (#515): its other
	// terms can read low by more, and a pair read more than
	// BacktestTolerance low is a miss, scored as a point like any other, and
	// counted in BacktestScore.VPLowerBound too.
	UnscoredVPLowerBound = "vpLowerBound"
)

// MeasuredBucketWidth is the widest bucket, as a share of its own lower
// edge, a measured percentile is placed in: U - L <= MeasuredBucketWidth L.
// A percentile is interpolated inside the bucket that holds it, and the
// interpolation reads L + q'(U - L) whatever the traffic did (#291), so the
// figure read and the truth can stand anywhere in (L, U], up to U/L apart.
// A bucket no wider than BacktestTolerance of its lower edge bounds that
// read error by the tolerance a verdict judges by; it does not make a
// verdict near the 20% line exact. A bucket from 0 never places one. Measured against the bucket's
// own edge, not the mean: a percentile below the mean sits in a bucket the
// mean says nothing about.
const MeasuredBucketWidth = BacktestTolerance

// bucketEps absorbs the rounding of bucket bounds written in decimal (1.2 x
// 0.025 is not 0.03 in binary).
const bucketEps = 1e-9

// The sides of a coarse measured bucket a prediction can fall on
// (BacktestPoint.OutsideBucket).
const (
	OutsideUnder = "under"
	OutsideOver  = "over"
)

// SettleShare is the most of a step's measurement window the prefill
// queue's relaxation time may take for the step's TTFT to be scored: a
// queue's wait is correlated over about one relaxation time (and relaxes to
// its steady state in a few), so one of a fifth of the window leaves five
// or more independent readings of the steady state in it
// (llm-d-incubation/llm-d-scaling-manager#305).
const SettleShare = 0.2

// relaxationSeconds is the relaxation time of the prefill fleet's queue,
// pooled as the planner prices it (the router sends each prompt to the pod
// with the least queued): t_r = 2 rho E[S] (Ca^2 + Cs^2) / (N (1 - rho)^2),
// with E[S] a pod's service time (the pods over the fleet's prefill knee),
// N the pods, rho the step's rate over that knee, Ca^2 the arrivals' spread
// priced (1 for random arrivals) and Cs the prompt spread priced (a pod's
// service time follows the prompt; the fixed latency a would narrow it).
// Pooling is the router's best case: pods held unevenly relax slower, up to
// N times (one pod's M/G/1 figure), so t_r may read short, and a step near
// the knee is then scored rather than left out. That is kept on purpose: on
// the Qwen3-32B staircase phases one pod's figure would leave 23 more steps
// (0.77 to 0.91 of the knee) unscored, 12 of whose 92 TTFT readings read
// more than 20% low, and a self-check that drops them no longer sees them
// (#305). It is +Inf at or past the knee, and 0 where nothing prices it.
func relaxationSeconds(rate, prefillKnee float64, pods int, ca2, cv float64) float64 {
	if !(rate > 0) || !(prefillKnee > 0) || pods <= 0 {
		return 0
	}
	rho := rate / prefillKnee
	if rho >= 1 {
		return math.Inf(1)
	}
	es := float64(pods) / prefillKnee
	return 2 * rho * es * (ca2 + cv*cv) / (float64(pods) * (1 - rho) * (1 - rho))
}

// settles says a queue of relaxation time tr settles inside a measurement
// window of window seconds: tr is at most SettleShare of it.
func settles(tr, window float64) bool { return tr <= SettleShare*window }

// backlogAt is a fluid backlog of q requests at the start of a step, t
// seconds into it, served at mu while requests arrive at lambda.
func backlogAt(q, lambda, mu, t float64) float64 { return max(0, q+(lambda-mu)*t) }

// backlogWait is the extra wait a fluid backlog of q requests carried into a
// step of dt seconds adds over the step's measurement window, its last w
// seconds: the mean of the backlog over mu across the window, and the most
// (at the window's start where it drains, its end where it grows), for an
// upper bound. Both are 0 with no backlog.
func backlogWait(q, lambda, mu, dt, w float64) (mean, most float64) {
	if !(q > 0) || !(mu > 0) || !(dt > 0) {
		return 0, 0
	}
	a := max(0, dt-max(w, 0))
	qa, qb := backlogAt(q, lambda, mu, a), backlogAt(q, lambda, mu, dt)
	most = max(qa, qb) / mu
	if !(dt > a) {
		return qa / mu, most
	}
	// The backlog is linear in t until it empties: the area under it.
	var area float64
	switch {
	case qb > 0 || lambda >= mu:
		area = (qa + qb) / 2 * (dt - a)
	default:
		empty := qa / (mu - lambda)
		area = qa / 2 * min(empty, dt-a)
	}
	return area / (dt - a) / mu, most
}

// statP90 is the percentile a verdict reports beside the mean.
const statP90 = "p90"

// primaryStats is the order a figure's statistic is taken in: the mean (the
// histogram's sum over its count, no bucket interpolated), else p90, else
// p50. The backtest's verdict takes the first scored in any step
// (primaryScore); the self-check the first scored in selfCheckMinSteps or
// more (selfCheckScore).
var primaryStats = []string{statisticMean, statP90, "p50"}

// primaryScore is the first of primaryStats scored in a step, nil with none.
func primaryScore(scores []BacktestScore) *BacktestScore {
	for _, stat := range primaryStats {
		i := slices.IndexFunc(scores, func(x BacktestScore) bool { return x.Statistic == stat })
		if i >= 0 && scores[i].All.Steps > 0 {
			return &scores[i]
		}
	}
	return nil
}

// backtestBanner is the backtest's standing in one line.
const backtestBanner = "The planner's figures are a model, not a bound: this check measures how far off they are on this fleet."

// The backtest's statistics, in order: the mean, p50, p90 and p95.
var backtestStats = []float64{0, 0.5, 0.9, 0.95}

// BacktestRequest is POST /v1/backtest's body. InputsID is the source fitted
// on. With TestInputsID the test is that source; without it InputsID's
// source is split at Split (a share of its span, "60%") or FitUntil (RFC
// 3339): the fit is the source read at the split, the test every cycle
// after it.
type BacktestRequest struct {
	InputsID     string `json:"inputsId"`
	TestInputsID string `json:"testInputsId,omitempty"`
	Split        string `json:"split,omitempty"`
	FitUntil     string `json:"fitUntil,omitempty"`
	// FitBelow and TestAbove split by load, in place of Split and FitUntil:
	// the fit is read from the steps whose arrival rate is below FitBelow
	// alone, and only the steps at or above TestAbove are judged. Each is a
	// rate in requests per second ("12.5") or a percentile of its side's
	// step rates ("p50"), over the steps that served traffic. With
	// TestInputsID, FitBelow reads InputsID's steps and TestAbove
	// TestInputsID's, and a test step the fit read a cycle of is not judged.
	FitBelow  string `json:"fitBelow,omitempty"`
	TestAbove string `json:"testAbove,omitempty"`
	// FitCycles is the cycles the fit was read from, for a split by load
	// (LoadFitCycles); set by the caller that resolved the fit, never sent.
	FitCycles *CycleSet `json:"-"`
	// Step is a step's length, 1m to 1h; absent, 5m. Settle is how long
	// after a change of replicas, and after the test's first cycle, a step
	// is not judged; absent, 2m.
	Step   string `json:"step,omitempty"`
	Settle string `json:"settle,omitempty"`
	// TTFT is a TTFT target to judge each step by, predicted and measured.
	TTFT *Objective `json:"ttft,omitempty"`
	// ArrivalCV2 is the test's Ca^2, when known; absent, Poisson.
	ArrivalCV2 *float64 `json:"arrivalCV2,omitempty"`
	// Measured is the latency scored against: router, engine or auto (the
	// router's where the source carries it).
	Measured string `json:"measured,omitempty"`
	// AllowConfigMismatch prices a test whose configuration differs from
	// the fit's, with the difference as a caveat; without it, refused.
	AllowConfigMismatch bool `json:"allowConfigMismatch,omitempty"`
	// Assume types a term the fit did not learn, as a plan's assume does;
	// every figure it moves is flagged, and the answer lists it.
	Assume *Assume `json:"assume,omitempty"`
	// TTFTModel prices TTFT with an experimental model in place of the
	// planner's: "" is the planner's, whatif.QueueRankMixture
	// (backtest_rank_mixture.go) and its variant
	// whatif.QueueRankMixtureNLLine (backtest_rank_mixture_nl.go) the
	// others. Every TTFT it prices reads OUTSIDE VALIDATED REGION.
	TTFTModel string `json:"ttftModel,omitempty"`
	// RankMixtureFitSteps turns on the rank mixture's fit-step fixes: its
	// KV-deferral lines and prompt slopes fitted on the steps under rho 0.9
	// only, and the ranks' shares read at the priced load
	// (whatif.RankMixtureOptions CleanFitSteps, SharesAtLoad). Off by
	// default; read only with TTFTModel rank-mixture.
	RankMixtureFitSteps bool `json:"rankMixtureFitSteps,omitempty"`
	// RankMixtureStratified prices the rank mixture with stratified service
	// moments (whatif.RankMixtureOptions.StratifiedMoments): each rank's
	// mean service and cs^2 integrated over the computed-token law, not
	// from its own draws. Absent, the server's default (on, unless the
	// server was started with --rank-mixture-stratified=false); false
	// reads them from each rank's own draws. Read only with TTFTModel
	// rank-mixture.
	RankMixtureStratified *bool `json:"rankMixtureStratified,omitempty"`
	// RankMixtureKVHold reads the KV-deferral lines no further than the
	// fit window's 95th percentile rank utilization, held past it, not
	// extrapolated (whatif.RankMixtureOptions.KVHold). Off by default;
	// only with TTFTModel rank-mixture-nl-line.
	RankMixtureKVHold bool `json:"rankMixtureKVHold,omitempty"`
	// RankMixtureRemainderSpread draws the router's remainder from the
	// router's measured request-processing time, where the fit window
	// recorded it (whatif.RankMixtureOptions.RemainderSpread); without it
	// the remainder is its line, as off, and the assumed line says so. Off
	// by default; only with TTFTModel rank-mixture-nl-line.
	RankMixtureRemainderSpread bool `json:"rankMixtureRemainderSpread,omitempty"`
	// RankMixtureRemainderServer draws the router's remainder from the
	// prefill pods' API servers and the router's own work, on the remainder
	// spread's shape, where the fit window recorded the pods' HTTP and
	// end-to-end series (whatif.RankMixtureOptions.RemainderServer);
	// without them the remainder is the spread, and the assumed line says
	// so. Off by default; only with TTFTModel rank-mixture-nl-line and
	// RankMixtureRemainderSpread.
	RankMixtureRemainderServer bool `json:"rankMixtureRemainderServer,omitempty"`
	// RankMixtureMinFitWindow is the shortest fit window the model, v2 or
	// its variant, prices from, a duration ("4h"): absent, the server's
	// default, else whatif.MinSpreadFitWindow (12h). A model fitted on a
	// window under 12h is flagged RankMixtureShortFitWindow. Only with a
	// TTFTModel.
	RankMixtureMinFitWindow string `json:"rankMixtureMinFitWindow,omitempty"`
	// PrefillSlots is the prefill engine's max-num-seqs, read by
	// TTFTModel's limited-PS check; 0 not known.
	PrefillSlots int `json:"prefillSlots,omitempty"`
	// TPOTModel prices the TPOT percentiles with an experimental
	// per-request distribution in place of the planner's ratio: "" is the
	// planner's, and whatif.TPOTRenewalLognormal the only other
	// (backtest_tpot_distribution.go). The mean TPOT stays the planner's.
	// Every percentile it prices reads OUTSIDE VALIDATED REGION.
	TPOTModel string `json:"tpotModel,omitempty"`
	// DecodeSlots is the decode engine's max-num-seqs and SpecTokens its
	// num_speculative_tokens, read by TPOTModel; 0 not known (or, for
	// SpecTokens, no speculative decoding).
	DecodeSlots int `json:"decodeSlots,omitempty"`
	SpecTokens  int `json:"specTokens,omitempty"`
	// TPOTDistMinFitSteps is the fewest fit steps the TPOT distribution
	// fits on: absent, the server's default, else MinTPOTDistFitSteps (24,
	// two hours of 5-minute steps). A distribution fitted on fewer than 24
	// is flagged TPOTDistributionShortFit. At least MinTPOTDistFitStepsFloor;
	// only with a TPOTModel.
	TPOTDistMinFitSteps int `json:"tpotDistMinFitSteps,omitempty"`
}

// BacktestResponse is POST /v1/backtest's answer, and capacity-planner
// backtest -o json.
type BacktestResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	WhatifSchema  string `json:"whatifSchema"`
	BacktestID    string `json:"backtestId"`
	Banner        string `json:"banner"`
	// Verdict is the answer in one plain sentence, and Excluded what was
	// not judged and why, in one more.
	Verdict string `json:"verdict"`
	// VerdictLines is the verdict a sentence a line: the span, TTFT,
	// TPOT, and where the steps sit against the benchmarks.
	VerdictLines []string       `json:"verdictLines"`
	Excluded     string         `json:"excluded"`
	Fit          BacktestWindow `json:"fit"`
	Test         BacktestWindow `json:"test"`
	// Split is how one source was split ("60%", or the time), "" with two.
	Split string `json:"split,omitempty"`
	// LoadSplit is the split by load, where the request asked one
	// (FitBelow, TestAbove); Split then says it in words.
	LoadSplit *BacktestLoadSplit `json:"loadSplit,omitempty"`
	Config    BacktestConfig     `json:"config"`
	Step      float64            `json:"step"`
	Settle    float64            `json:"settle"`
	Measured  string             `json:"measured"`
	// MeasuredNote says what the measured figures are.
	MeasuredNote string `json:"measuredNote"`
	// Assumed is every input of the prediction the test did not give.
	Assumed []string `json:"assumed"`
	// CachedTokenCost is the fit's phi, the cost of a cached prompt token,
	// priced at every step; absent where slo.SwitchTTFTCachedTokenCost is
	// off.
	CachedTokenCost *CachedTokenCost `json:"cachedTokenCost,omitempty"`
	TTFT            *Objective       `json:"ttft,omitempty"`
	Steps           []BacktestStep   `json:"steps"`
	Summary         BacktestSummary  `json:"summary"`
	Notes           []string         `json:"notes"`
	// RankMixture is the experimental TTFT model's fit, where the request
	// asked for it (BacktestRequest.TTFTModel).
	RankMixture *RankMixtureReport `json:"rankMixture,omitempty"`
	// TPOTDistribution is the experimental TPOT distribution's fit and its
	// score at the histogram's resolution, where the request asked for it
	// (BacktestRequest.TPOTModel).
	TPOTDistribution *TPOTDistReport `json:"tpotDistribution,omitempty"`
}

// BacktestWindow is one side of a backtest: its inputs, and the time it
// spans.
type BacktestWindow struct {
	InputsID string    `json:"inputsId"`
	Source   string    `json:"source"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Cycles   int       `json:"cycles"`
}

// BacktestConfig is the fit's configuration against the test's: Match when
// no recorded field differs; Differences each one that does, NotCompared
// what neither source records. Allowed says a mismatch was priced anyway.
type BacktestConfig struct {
	Match       bool             `json:"match"`
	Differences []ConfigMismatch `json:"differences"`
	NotCompared []string         `json:"notCompared"`
	Allowed     bool             `json:"allowed,omitempty"`
}

// ConfigMismatch is one setting that differs.
type ConfigMismatch struct {
	Field string `json:"field"`
	Fit   string `json:"fit"`
	Test  string `json:"test"`
}

// BacktestStep is one step of the test: its inputs, whether it was judged
// (Excluded says why not), and each statistic predicted and measured.
type BacktestStep struct {
	Start    time.Time `json:"start"`
	Cycles   int       `json:"cycles"`
	Excluded string    `json:"excluded,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Rate     float64   `json:"rate"`
	Prefill  int       `json:"prefill"`
	Decode   int       `json:"decode"`
	// PromptTokens and OutputTokens are the step's mean lengths, absent
	// where no cycle read them (the fit's are priced then).
	PromptTokens *float64 `json:"promptTokens,omitempty"`
	OutputTokens *float64 `json:"outputTokens,omitempty"`
	// PromptCV is the step's prompt spread, read from its prefill pods'
	// computed-token histograms; absent where they read none, and the
	// test's whole-window spread, else the fit's, is priced.
	PromptCV *float64 `json:"promptCV,omitempty"`
	// Requests is the requests the measured histograms hold over the step.
	Requests float64 `json:"requests"`
	// KneeShare is the rate over the fit's lower knee at the step's fleet,
	// and Load its band (LoadBelow80, LoadNearKnee, LoadPastKnee), the band
	// its TTFT is scored in; DecodeKneeShare and DecodeLoad the same over
	// decode's knee, the band its TPOT is scored in. Each is absent where
	// its knee did not price.
	KneeShare       *float64        `json:"kneeShare,omitempty"`
	Load            string          `json:"load,omitempty"`
	DecodeKneeShare *float64        `json:"decodeKneeShare,omitempty"`
	DecodeLoad      string          `json:"decodeLoad,omitempty"`
	TTFT            []BacktestPoint `json:"ttft"`
	TPOT            []BacktestPoint `json:"tpot"`
	// RelaxationSeconds is the prefill queue's relaxation time at the
	// step's rate (relaxationSeconds), absent where the prefill knee did not
	// price or the rate is past it. Unsettled says it is more than
	// SettleShare of the step's measurement window: the step's TTFT is not
	// scored (UnscoredUnsettled).
	RelaxationSeconds *float64 `json:"relaxationSeconds,omitempty"`
	Unsettled         bool     `json:"unsettled,omitempty"`
	// RankMixture is the experimental TTFT model's checks at the step,
	// where the request asked for it.
	RankMixture *RankMixtureCheck `json:"rankMixture,omitempty"`
	// TPOTDist is the experimental TPOT distribution at the step, where
	// the request asked for it.
	TPOTDist *TPOTDistCheck `json:"tpotDistribution,omitempty"`
	// TTFTBurst says the step's window is a burst (burstIn) against a
	// prefill knee priced from a lower bound of V_P, and not against
	// decode's knee: the true prefill knee may be far higher, so the step is
	// not excluded for it, and only its TTFT is not scored (UnscoredBurst).
	TTFTBurst bool `json:"ttftBurst,omitempty"`
	// Backlog is the requests a fluid queue carries into the step from the
	// steps before it, where one ran past the prefill knee (Q_k+1 =
	// max(0, Q_k + (rate - knee) x step)), and BacklogWait the mean wait it
	// adds to each TTFT predicted: absent with none.
	Backlog     *float64 `json:"backlog,omitempty"`
	BacklogWait *float64 `json:"backlogWait,omitempty"`
	// Target is the TTFT target judged, predicted and measured: met,
	// missed or unknown.
	Target *BacktestJudged `json:"target,omitempty"`
	// BatchBeyondFit is the batch the step's decode runs past the most the
	// fit's ITL line rows held (flag BatchBeyondFit): its mean TPOT's, or,
	// on a step past the decode knee, the knee's, where the TPOT is refused.
	// Its TPOT may read low and the decode knee high. Absent where the batch
	// is inside the fit's.
	BatchBeyondFit *BatchBeyondFit `json:"batchBeyondFit,omitempty"`
}

// BacktestPoint is one statistic of a step: the prediction (with the
// envelope's verdict), its upper bound, the measured figure, and their
// ratio. Unscored says why the pair is not scored.
type BacktestPoint struct {
	Statistic string   `json:"statistic"`
	Predicted Figure   `json:"predicted"`
	Bound     *float64 `json:"bound,omitempty"`
	Measured  *float64 `json:"measured,omitempty"`
	Ratio     *float64 `json:"ratio,omitempty"`
	InRegion  bool     `json:"inRegion"`
	// AboveHistogram says the measured figure was above the histogram's
	// top bucket while the prediction was under it: Ratio is then the
	// prediction over the top bound, no lower than the truth's, and never
	// within.
	AboveHistogram bool `json:"aboveHistogram,omitempty"`
	// MeasuredBucket is the histogram bucket the measured percentile fell
	// in, where it is too wide to place the percentile (MeasuredBucketWidth):
	// the truth is somewhere in it. With the prediction inside it the pair
	// is not scored (unresolvable); with the prediction outside it,
	// OutsideBucket says which side. For the router's TPOT p90 and p95 it is
	// the joint bracket (whatif.JointBracket: the router's TPOT histogram and,
	// above their first edge, the decode engines', at their sampling noise),
	// and Measured its point.
	MeasuredBucket *Band `json:"measuredBucket,omitempty"`
	// OutsideBucket is "under" or "over" where the prediction fell so far
	// below or above MeasuredBucket that even its nearer edge is more than
	// BacktestTolerance away: a certain miss in that direction. Ratio is
	// then the prediction over the nearer edge, a bound on the truth's that
	// understates the miss (no lower than it under, no higher over); never
	// within, and read low under. A prediction nearer the bucket than that
	// could be within, and is unresolvable.
	OutsideBucket string `json:"outsideBucket,omitempty"`
	Unscored      string `json:"unscored,omitempty"`
	// SameBucket says an unresolvable pair (UnscoredUnresolvable) had the
	// prediction inside MeasuredBucket itself, (Lo, Hi]: the histogram
	// cannot tell the two apart, so the pair is consistent at the bucket's
	// resolution, Hi/Lo wide. It is not within BacktestTolerance and is not
	// scored; an unresolvable pair without it had the prediction outside
	// the bucket, though within BacktestTolerance of its nearer edge.
	SameBucket bool `json:"sameBucket,omitempty"`
}

// BacktestJudged is a step judged against a TTFT target.
type BacktestJudged struct {
	Predicted string `json:"predicted"`
	Measured  string `json:"measured"`
}

// BacktestSummary is the test scored: the steps by status, and each
// statistic's score over every judged step and over those inside the
// validated region.
type BacktestSummary struct {
	Steps    int              `json:"steps"`
	Judged   int              `json:"judged"`
	Excluded BacktestExcluded `json:"excluded"`
	TTFT     []BacktestScore  `json:"ttft"`
	TPOT     []BacktestScore  `json:"tpot"`
	// Target is the TTFT target's agreement, with a target.
	Target *BacktestAgreement `json:"target,omitempty"`
	// ByLoad is the same scores over the judged steps of each load band:
	// TTFT by the step's Load, TPOT by its DecodeLoad.
	ByLoad []BacktestLoad `json:"byLoad"`
}

// The load bands a step is scored in: its rate over the fit's lower knee
// at its fleet.
const (
	LoadBelow80  = "below 0.8 of the knee"
	LoadNearKnee = "0.8 to 1.0 of the knee"
	LoadPastKnee = "past the knee"
)

// BacktestLoad is the scores over one load band's judged steps.
type BacktestLoad struct {
	Load string `json:"load"`
	// Steps is the judged steps whose TTFT falls in the band, DecodeSteps
	// those whose TPOT does.
	Steps       int             `json:"steps"`
	DecodeSteps int             `json:"decodeSteps"`
	TTFT        []BacktestScore `json:"ttft"`
	TPOT        []BacktestScore `json:"tpot"`
}

// BacktestExcluded counts the steps not judged, by reason.
type BacktestExcluded struct {
	NoTraffic     int `json:"noTraffic"`
	NoMeasurement int `json:"noMeasurement"`
	Scaling       int `json:"scaling"`
	WarmUp        int `json:"warmUp"`
	NotPriced     int `json:"notPriced"`
	OtherVariants int `json:"otherVariants"`
	Burst         int `json:"burst"`
	// Load is the steps of the test's span outside a load split's test
	// (ExcludedLoad).
	Load int `json:"load,omitempty"`
}

// BacktestScore is one statistic's score.
type BacktestScore struct {
	Statistic string        `json:"statistic"`
	All       BacktestStats `json:"all"`
	InRegion  BacktestStats `json:"inRegion"`
	// Unscored counts the judged steps whose pair was not scored, by
	// reason (UnscoredRefused and the rest); Refused the refused ones by
	// the refusal's reason (Saturated, NoPrefillCapacity, ...).
	Unscored map[string]int `json:"unscored"`
	Refused  map[string]int `json:"refused"`
	// SameBucket counts the unresolvable steps whose prediction fell inside
	// the measured bucket itself (BacktestPoint.SameBucket), and
	// SameBucketBand spans those buckets: consistent at the histogram's
	// resolution, a third outcome beside within and out, never counted
	// within. SameBucketWidth is the narrowest of those buckets' Hi/Lo, 0
	// where one starts at 0.
	SameBucket      int     `json:"sameBucket,omitempty"`
	SameBucketBand  *Band   `json:"sameBucketBand,omitempty"`
	SameBucketWidth float64 `json:"sameBucketWidth,omitempty"`
	// PastKnee is the measured figure's range over the steps refused as
	// saturated, where the source measured one: what the fleet did where
	// the planner gave no figure.
	PastKnee *Band `json:"pastKnee,omitempty"`
	// Outside counts the scored steps outside the validated region, by the
	// envelope's reason, its words before the first figure.
	Outside map[string]int `json:"outside"`
	// VPLowerBound is the steps whose figure was priced from a lower bound
	// of V_P (UnscoredVPLowerBound), scored apart from the point figures
	// All and InRegion hold; nil with none.
	VPLowerBound *BacktestApart `json:"vpLowerBound,omitempty"`
}

// BacktestApart is the score of every pair priced from a lower bound of
// V_P with a ratio: Steps of them, AtOrOver measured at or under the
// figure, Under measured over it (readUnder, strictly: the figure read
// under the measured one), Low those of Under read more than
// BacktestTolerance low (readLow), which are scored as point misses too,
// in All, and Bias the median of predicted over measured. The rest,
// Steps - Low, are scored apart only (UnscoredVPLowerBound).
type BacktestApart struct {
	Steps    int      `json:"steps"`
	AtOrOver int      `json:"atOrOver"`
	Under    int      `json:"under"`
	Low      int      `json:"low"`
	Bias     *float64 `json:"bias,omitempty"`
}

// apartOf is the score of ps, each with a ratio; nil with none.
func apartOf(ps []BacktestPoint) *BacktestApart {
	if len(ps) == 0 {
		return nil
	}
	a := &BacktestApart{Steps: len(ps)}
	ratios := make([]float64, len(ps))
	for i, p := range ps {
		ratios[i] = *p.Ratio
		if readUnder(p) {
			a.Under++
		}
		if readLow(p) {
			a.Low++
		}
	}
	a.AtOrOver = a.Steps - a.Under
	a.Bias = ptr(median(ratios))
	return a
}

// BacktestStats is the score over a set of steps. Bias is the median of
// predicted over measured; MedianError and P90Error the median and 90th
// percentile of |ln(predicted/measured)| as a share, e^x - 1; Coverage the
// share of steps measured at or under the prediction's upper bound;
// Within the share within BacktestTolerance. Each is absent over no step.
type BacktestStats struct {
	Steps       int      `json:"steps"`
	Bias        *float64 `json:"bias,omitempty"`
	MedianError *float64 `json:"medianError,omitempty"`
	P90Error    *float64 `json:"p90Error,omitempty"`
	Coverage    *float64 `json:"coverage,omitempty"`
	Within      *float64 `json:"within,omitempty"`
	// CoverageSteps is the steps whose prediction carried an interval,
	// those Coverage is over; Low the steps read more than
	// BacktestTolerance low, those above the histogram and those under a
	// coarse bucket (OutsideBucket) included.
	CoverageSteps int `json:"coverageSteps"`
	Low           int `json:"low"`
}

// BacktestAgreement is how often the predicted and the measured TTFT agreed
// on the target: MissedMisses is the costly case, a step predicted to meet
// the target that measured a miss.
type BacktestAgreement struct {
	Steps        int `json:"steps"`
	BothMet      int `json:"bothMet"`
	BothMissed   int `json:"bothMissed"`
	MissedMisses int `json:"missedMisses"`
	FalseAlarms  int `json:"falseAlarms"`
	NotJudged    int `json:"notJudged"`
}

// The words a judged target reads.
const (
	judgedMet     = "met"
	judgedMissed  = "missed"
	judgedUnknown = "unknown"
)

// handleBacktest is POST /v1/backtest.
func (s *Server) handleBacktest(w http.ResponseWriter, r *http.Request) {
	var req BacktestRequest
	if !s.readLongJSON(w, r, &req, maxBody) {
		return
	}
	out, err := s.backtest(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	body, err := marshal(out)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// backtest answers req on one plan slot, within the plan budget. A split
// resolves the fit at the split, as POST /v1/inputs would, and keeps it.
func (s *Server) backtest(ctx context.Context, req BacktestRequest) (*BacktestResponse, error) {
	// The server's experimental models (Config.TTFTModel, TPOTModel) price
	// a request that names none, as if it had.
	if req.TTFTModel == "" {
		req.TTFTModel = s.ttftModel
	}
	if req.TPOTModel == "" {
		req.TPOTModel = s.tpotModel
	}
	if req.RankMixtureStratified == nil {
		on := s.stratified()
		req.RankMixtureStratified = &on
	}
	s.rankMixtureDefaults(&req)
	test, ok := s.inputs.get(req.InputsID)
	if !ok {
		return nil, errUnknownInputs
	}
	fit := test
	var cutoff time.Time
	split := ""
	if err := checkLoadSplit(req); err != nil {
		return nil, err
	}
	switch {
	case loadSplitAsked(req):
		// The fit is InputsID's source read again from its steps below
		// FitBelow alone; the test, TestInputsID's or InputsID's whole.
		if req.TestInputsID != "" {
			if test, ok = s.inputs.get(req.TestInputsID); !ok {
				return nil, errUnknownInputs
			}
		}
		var err error
		if fit, req.FitCycles, err = s.loadFit(ctx, fit, req); err != nil {
			return nil, err
		}
	case req.TestInputsID != "":
		if req.Split != "" || req.FitUntil != "" {
			return nil, InputErrorf("split and fitUntil split one source: give them without testInputsId")
		}
		if test, ok = s.inputs.get(req.TestInputsID); !ok {
			return nil, errUnknownInputs
		}
	default:
		var cycle int
		var err error
		if cycle, cutoff, split, err = splitOf(test, req.Split, req.FitUntil); err != nil {
			return nil, err
		}
		in, err := refitRequest(test, cycle, cutoff)
		if err != nil {
			return nil, err
		}
		if fit, err = s.refit(ctx, in); err != nil {
			return nil, err
		}
	}
	// The engines' flags the models read are the fit's, as the source gives
	// them, where the request does not state them.
	if req.TTFTModel != "" && req.PrefillSlots == 0 {
		req.PrefillSlots = fit.engine.PrefillSlots
	}
	if req.TPOTModel != "" {
		if req.DecodeSlots == 0 {
			req.DecodeSlots = fit.engine.DecodeSlots
		}
		if req.SpecTokens == 0 {
			req.SpecTokens = fit.engine.SpecTokens
		}
	}
	q, err := backtestQuestionOf(fit, test, req, cutoff, split)
	if err != nil {
		return nil, err
	}
	if !acquire(s.planSlots) {
		return nil, errBusy
	}
	defer func() { <-s.planSlots }()
	bctx, cancel := context.WithTimeoutCause(ctx, s.planBudget, errPlanBudget)
	defer cancel()
	out, err := q.run(bctx)
	if err != nil {
		if errors.Is(context.Cause(bctx), errPlanBudget) && ctx.Err() == nil {
			return nil, BudgetError{Budget: s.planBudget}
		}
		return nil, err
	}
	out.SchemaVersion, out.WhatifSchema = SchemaVersion, s.whatifSchema
	return out, nil
}

// refit resolves in, the fit of a split, on one input slot, and keeps it.
func (s *Server) refit(ctx context.Context, in InputsRequest) (*inputsEntry, error) {
	if !acquire(s.inputSlots) {
		return nil, errBusy
	}
	res, err := s.resolver.Resolve(ctx, in)
	<-s.inputSlots
	if err != nil {
		return nil, err
	}
	doc := res.Inputs
	doc.SchemaVersion, doc.WhatifSchema = SchemaVersion, s.whatifSchema
	doc.InputsID = contentID("inputs", res.Content)
	fit := &inputsEntry{id: doc.InputsID, doc: doc, pricer: res.Pricer, series: res.Series, latency: res.Latency,
		config: res.Config, bytes: res.Bytes, mixture: res.Mixture, decode: res.Decode, engine: res.Engine}
	return s.inputs.add(doc.InputsID, fit), nil
}

// loadFit is the fit of a split by load: e's recording read again from its
// steps below req.FitBelow alone (LoadFitCycles), and those cycles.
func (s *Server) loadFit(ctx context.Context, e *inputsEntry, req BacktestRequest) (*inputsEntry, *CycleSet, error) {
	if e.doc.Source.Kind != SourceRecording {
		return nil, nil, InputErrorf("a split by load reads the fit from a recording's steps: a %q source is split by time (split, fitUntil)", e.doc.Source.Kind)
	}
	cs, err := LoadFitCycles(&Resolved{Series: e.series}, req)
	if err != nil {
		return nil, nil, err
	}
	in, err := refitRequest(e, 0, time.Time{})
	if err != nil {
		return nil, nil, err
	}
	in.Source.Cycles = cs
	fit, err := s.refit(ctx, in)
	return fit, cs, err
}

// Backtest answers req against sources resolved outside a server: the CLI's
// backtest, on the same code path as POST /v1/backtest. For a split, fit is
// the source read at the split (cutoff) and wholeTest the whole source the
// test steps are read from; for two sources, cutoff is zero.
func Backtest(ctx context.Context, fit, wholeTest *Resolved, req BacktestRequest, cutoff time.Time, split, whatifSchema string) (*BacktestResponse, error) {
	entry := func(res *Resolved) *inputsEntry {
		doc := res.Inputs
		doc.InputsID = contentID("inputs", res.Content)
		return &inputsEntry{id: doc.InputsID, doc: doc, pricer: res.Pricer, series: res.Series, latency: res.Latency, config: res.Config,
			mixture: res.Mixture, decode: res.Decode}
	}
	q, err := backtestQuestionOf(entry(fit), entry(wholeTest), req, cutoff, split)
	if err != nil {
		return nil, err
	}
	out, err := q.run(ctx)
	if err != nil {
		return nil, err
	}
	out.SchemaVersion, out.WhatifSchema = SchemaVersion, whatifSchema
	return out, nil
}

// SplitOf is where res splits at split (a share, "60%") or fitUntil (RFC
// 3339): the cycle to read the fit at (1-based), the time it was read, and
// the split in words.
func SplitOf(res *Resolved, split, fitUntil string) (int, time.Time, string, error) {
	return splitOf(&inputsEntry{series: res.Series}, split, fitUntil)
}

// splitOf is SplitOf of an inputs entry.
func splitOf(e *inputsEntry, split, fitUntil string) (int, time.Time, string, error) {
	n := len(e.series)
	if n < 2 {
		return 0, time.Time{}, "", InputErrorf("the source carries %d cycles: a split needs two or more (or give testInputsId)", n)
	}
	if split != "" && fitUntil != "" {
		return 0, time.Time{}, "", InputErrorf("split and fitUntil both name the split; give one")
	}
	first, last := e.series[0].At, e.series[n-1].At
	var cutoff time.Time
	words := ""
	switch {
	case fitUntil != "":
		t, err := time.Parse(time.RFC3339, fitUntil)
		if err != nil {
			return 0, time.Time{}, "", InputErrorf("fitUntil %q, want RFC 3339", fitUntil)
		}
		cutoff, words = t, "fit until "+t.UTC().Format(time.RFC3339)
	default:
		share := DefaultBacktestSplit
		if split != "" {
			v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(split), "%"), 64)
			if err != nil || !(v > 0 && v < 100) {
				return 0, time.Time{}, "", InputErrorf("split %q, want a share of the source between 0%% and 100%%, as 60%%", split)
			}
			share = v / 100
		}
		cutoff = first.Add(time.Duration(share * float64(last.Sub(first))))
		words = strconv.FormatFloat(math.Round(share*1000)/10, 'f', -1, 64) + "%"
	}
	cycle := 0
	for i, x := range e.series {
		if !x.At.After(cutoff) {
			cycle = i + 1
		}
	}
	switch {
	case cycle == 0:
		return 0, time.Time{}, "", InputErrorf("the split %s is before the source's first cycle (%s): nothing to fit on",
			cutoff.UTC().Format(time.RFC3339), first.UTC().Format(time.RFC3339))
	case cycle >= n:
		return 0, time.Time{}, "", InputErrorf("the split %s is at or after the source's last cycle (%s): nothing to test on",
			cutoff.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339))
	}
	return cycle, e.series[cycle-1].At, words, nil
}

// refitRequest is the inputs request that reads e's source at cycle, the
// fit of a split: a recording at that cycle, a Prometheus window cut at the
// cycle's time.
func refitRequest(e *inputsEntry, cycle int, cutoff time.Time) (InputsRequest, error) {
	d := e.doc
	in := InputsRequest{Model: d.Model, DecodeVariant: d.Fleet.Decode.Variant, PrefillVPPlateau: d.PrefillVPPlateau, PrefillHitAtCount: d.PrefillHitAtCount,
		PrefillVPServiceLine: d.PrefillVPServiceLine, PrefillFLoadCurve: d.PrefillFLoadCurve, ExperimentalTTFT: d.ExperimentalTTFT}
	if d.Fleet.Prefill != nil {
		in.PrefillVariant = d.Fleet.Prefill.Variant
	}
	switch d.Source.Kind {
	case SourceRecording:
		in.Source = &SourceRequest{Kind: SourceRecording, Name: d.Source.Name, Cycle: cycle}
	case SourcePrometheus:
		if d.Source.From == nil {
			return in, InputErrorf("the source does not say its window: split it by giving two windows")
		}
		in.Source = &SourceRequest{Kind: SourcePrometheus, Namespace: d.Namespace, From: d.Source.From.UTC().Format(time.RFC3339),
			To: cutoff.UTC().Format(time.RFC3339)}
	default:
		return in, InputErrorf("a %q source cannot be split: give testInputsId", d.Source.Kind)
	}
	return in, nil
}

// backtestQuestion is a backtest's request checked against its sides.
type backtestQuestion struct {
	fit, test *inputsEntry
	cutoff    time.Time
	split     string
	step      time.Duration
	settle    time.Duration
	measured  string
	target    *ReportTarget
	cv2       *float64
	promptCV  *float64
	// promptMaxRatio is promptCV's bound (stepInputs.promptMaxRatio).
	promptMaxRatio float64
	// contextTokens is the test's engines' context window (contextWindowOf),
	// which holds each step's prompt spread; 0 where unknown.
	contextTokens float64
	config        BacktestConfig
	assumed       []string
	testIndices   []int
	assume        Assume
	// targetAsked is the target as the answer names it.
	targetAsked *Objective
	// source is the latency scored against; measureWindow, when above 0,
	// the seconds each step's window spans, for a prediction made with no
	// latency to read it from; predictionHash the prediction a comparison
	// scores, which has no fit.
	source         string
	measureWindow  float64
	predictionHash string
	// cachedCost is phi priced at every step in place of the pricer's own
	// (Knobs.CachedTokenCost), for FitCachedTokenCost; nil is the pricer's.
	cachedCost *float64
	// mixture is the experimental TTFT model fitted on the fit window, nil
	// unless the request asked for it.
	mixture *rankMixtureQ
	// tpotDist is the experimental TPOT distribution fitted on the fit
	// window, nil unless the request asked for it.
	tpotDist *tpotDistQ
	// load is the split by load, nil unless the request asked one.
	load *loadSplit
}

// backtestQuestionOf checks req against fit and test, and refuses a test
// whose configuration differs from the fit's unless the request allows it.
func backtestQuestionOf(fit, test *inputsEntry, req BacktestRequest, cutoff time.Time, split string) (*backtestQuestion, error) {
	return questionOf(fit, test, req, cutoff, time.Time{}, split, false)
}

// questionOf is backtestQuestionOf; predictOnly asks no latency of the
// test, and a non-zero until ends the test at that time.
func questionOf(fit, test *inputsEntry, req BacktestRequest, cutoff, until time.Time, split string, predictOnly bool) (*backtestQuestion, error) {
	q := &backtestQuestion{fit: fit, test: test, cutoff: cutoff, split: split, step: DefaultBacktestStep,
		settle: DefaultBacktestSettle, measured: MeasuredAuto, cv2: req.ArrivalCV2}
	var err error
	if req.Step != "" {
		if q.step, err = time.ParseDuration(req.Step); err != nil || q.step < minBacktestStep || q.step > maxBacktestStep {
			return nil, InputErrorf("step %q, want a duration from %s to %s", req.Step, minBacktestStep, maxBacktestStep)
		}
	}
	if req.Settle != "" {
		if q.settle, err = time.ParseDuration(req.Settle); err != nil || q.settle < 0 || q.settle > maxBacktestSettle {
			return nil, InputErrorf("settle %q, want a duration from 0s to %s", req.Settle, maxBacktestSettle)
		}
	}
	switch req.Measured {
	case "", MeasuredAuto:
	case MeasuredRouter, MeasuredEngine:
		q.measured = req.Measured
	default:
		return nil, InputErrorf("measured %q, want %s, %s or %s", req.Measured, MeasuredAuto, MeasuredRouter, MeasuredEngine)
	}
	if c := req.ArrivalCV2; c != nil && !(*c > 0 && *c <= 100) {
		return nil, InputErrorf("arrivalCV2 %g, want above 0 and at most 100", *c)
	}
	if q.target, q.targetAsked, err = targetOf(req.TTFT); err != nil {
		return nil, err
	}
	if len(test.series) == 0 {
		return nil, InputErrorf("the test source carries no cycles")
	}
	if len(test.latency) != len(test.series) && !predictOnly {
		return nil, InputErrorf("the test source carries no measured latency to score against")
	}
	if fit.id == test.id && cutoff.IsZero() {
		return nil, InputErrorf("the test is the fit's own source: a backtest scores a period the planner did not see; split it, or test another recording")
	}
	// A P/D fleet is priced with both roles. An aggregated one (no prefill
	// variant, its decode variant serving both roles) is priced on its
	// decode line alone: its TPOT, with its TTFT refused (issue
	// llm-d-incubation/llm-d-scaling-manager#512). The fit and the test
	// must be the same kind of fleet.
	if (fit.doc.Fleet.Prefill == nil) != (test.doc.Fleet.Prefill == nil) {
		return nil, InputErrorf("the fit and the test are not the same kind of fleet: one has a prefill variant and the other none; " +
			"a backtest compares a P/D fleet with a P/D fleet, or an aggregated one with an aggregated one")
	}
	if fit.doc.Model != test.doc.Model {
		return nil, InputErrorf("the fit is model %q and the test model %q: a backtest compares one model", fit.doc.Model, test.doc.Model)
	}
	fitIdx := []int{}
	for i, x := range fit.series {
		if cutoff.IsZero() || !x.At.After(cutoff) {
			if c := fit.doc.Source.Cycle; c == 0 || i < c {
				fitIdx = append(fitIdx, i)
			}
		}
	}
	testIdx := []int{}
	for i, x := range test.series {
		if cutoff.IsZero() || x.At.After(cutoff) {
			testIdx = append(testIdx, i)
		}
	}
	if err := checkLoadSplit(req); err != nil {
		return nil, err
	}
	if loadSplitAsked(req) {
		if !cutoff.IsZero() {
			return nil, InputErrorf("fitBelow and testAbove split by load, split and fitUntil by time: give one")
		}
		oneSource := fit.doc.Source.Kind == test.doc.Source.Kind && fit.doc.Source.Name == test.doc.Source.Name
		if q.load, testIdx, err = loadSplitOf(fit, test, req, q.step, oneSource); err != nil {
			return nil, err
		}
		q.split = q.load.words()
	}
	q.config = compareConfig(withFlowControl(fit.config, fit.series, fitIdx), withFlowControl(test.config, test.series, testIdx))
	if !q.config.Match {
		if !req.AllowConfigMismatch {
			return nil, ConfigMismatchError{InputError: InputError{Err: fmt.Errorf("the test ran another configuration than the fit: %s. "+
				"A backtest across configurations scores the difference, not the planner; give allowConfigMismatch (--allow-config-mismatch) "+
				"to price it anyway, with the difference as a caveat", mismatchWords(q.config.Differences))}, Differences: q.config.Differences}
		}
		q.config.Allowed = true
	}
	if q.load != nil {
		q.testIndices = testIdx
	} else {
		for i, x := range test.series {
			if (q.cutoff.IsZero() || x.At.After(q.cutoff)) && (until.IsZero() || !x.At.After(until)) {
				q.testIndices = append(q.testIndices, i)
			}
		}
	}
	switch {
	case len(q.testIndices) == 0 && q.load != nil:
		return nil, InputErrorf("testAbove %s (%.4g r/s): no step of the test at or above it that the fit did not read, nothing to test on",
			req.TestAbove, q.load.testRate)
	case len(q.testIndices) == 0:
		return nil, InputErrorf("no test cycle after the split")
	}
	slices.SortStableFunc(q.testIndices, func(a, b int) int { return test.series[a].At.Compare(test.series[b].At) })
	span := test.series[q.testIndices[len(q.testIndices)-1]].At.Sub(test.series[q.testIndices[0]].At)
	if int(span/q.step)+1 > maxBacktestSteps {
		return nil, InputErrorf("the test spans %s: at a step of %s that is more than %d steps; ask a longer step", span, q.step, maxBacktestSteps)
	}
	if cutoff.IsZero() && q.load == nil {
		// Two sources: the fit must end before the test starts. A fit that
		// reaches into the test's time saw its latency; one recorded after
		// it may open with learned state (an ITL line) the controller
		// learned during the test.
		fw := windowOf(fit, nil, time.Time{})
		t0 := test.series[q.testIndices[0]].At
		if fw.Cycles > 0 && !fw.To.Before(t0) {
			return nil, InputErrorf("the fit (%s to %s) does not end before the test starts (%s): a backtest scores a later period "+
				"the planner did not see, and a fit recorded after it may carry what was learned from it",
				fw.From.UTC().Format(time.RFC3339), fw.To.UTC().Format(time.RFC3339), t0.UTC().Format(time.RFC3339))
		}
	}
	// The test's prompt spread is an input, its lengths, never its latency:
	// pooled over the test's cycles, for a step whose own reads none.
	q.contextTokens, _ = contextWindowOf(&test.doc)
	if whole := stepInputsOf(seriesAt(test.series, q.testIndices), q.contextTokens); whole.promptCV != nil {
		q.promptCV, q.promptMaxRatio = whole.promptCV, whole.promptMaxRatio
	}
	if a := req.Assume; a != nil {
		// Checked as a plan's, at a TPOT percentile (a backtest prices its
		// percentiles): a term the pricer would refuse on every step is the
		// caller's error, not a test with no step judged.
		sc := Scenario{Assume: *a, TPOT: &ResolvedObjective{Statistic: "p90"}}
		if err := sc.checkAssume(); err != nil {
			return nil, err
		}
		q.assume = *a
		q.assumed = append(q.assumed, "terms typed in for the fit (assume): every figure they move is flagged")
	}
	if q.cv2 == nil {
		q.assumed = append(q.assumed, "random (Poisson) arrivals: the recording does not measure how bursty arrivals are; "+
			"state it with --arrival-cv2 (arrivalCV2) when known")
	}
	if err := checkMinFitWindow(req.RankMixtureMinFitWindow, req.TTFTModel); err != nil {
		return nil, err
	}
	if err := checkTPOTMinFitSteps(req.TPOTDistMinFitSteps, req.TPOTModel); err != nil {
		return nil, err
	}
	if (req.RankMixtureKVHold || req.RankMixtureRemainderSpread) && req.TTFTModel != string(whatif.QueueRankMixtureNLLine) {
		return nil, InputErrorf("rankMixtureKVHold and rankMixtureRemainderSpread are modes of ttftModel %q: give them with that model",
			whatif.QueueRankMixtureNLLine)
	}
	if err := checkRemainderServer(req.RankMixtureRemainderServer, req.RankMixtureRemainderSpread, req.TTFTModel); err != nil {
		return nil, err
	}
	switch req.TTFTModel {
	case "":
		if req.RankMixtureFitSteps {
			return nil, InputErrorf("rankMixtureFitSteps changes the fit of ttftModel %q: give it with that model", whatif.QueueRankMixture)
		}
	case string(whatif.QueueRankMixture), string(whatif.QueueRankMixtureNLLine):
		// The model prices only where its identifiability checks pass;
		// otherwise it abstains, and every TTFT is the planner's own.
		sw := rankMixtureSwitchesOf(req)
		switch {
		case len(test.mixture) != len(test.series):
			q.mixture = abstainedMixture("the test source did not give the per-rank series")
		default:
			fitted, ferr := fitRankMixture(fit, fitIdx, q.step, req.PrefillSlots, sw)
			if ferr != nil {
				q.mixture = abstainedMixture(ferr.Error())
			} else {
				q.mixture = fitted.identified().withHitAtCount(fit, fitIdx)
			}
		}
		q.mixture.report.Model = string(sw.model())
		q.assumed = append(q.assumed, q.mixture.assumedLines()...)
	default:
		return nil, InputErrorf("ttftModel %q, want %q, %q or none", req.TTFTModel, whatif.QueueRankMixture, whatif.QueueRankMixtureNLLine)
	}
	switch req.TPOTModel {
	case "":
	case whatif.TPOTRenewalLognormal, whatif.TPOTPerLengthMixture:
		// The distribution prices only where its checks pass; otherwise it
		// abstains, and every TPOT percentile is the planner's own.
		if len(test.decode) != len(test.series) {
			q.tpotDist = &tpotDistQ{report: TPOTDistReport{Model: req.TPOTModel, Experimental: true, FoldC: []float64{},
				Fold: []TPOTDistScore{}, Abstained: "the test source did not give the decode series"}}
		} else {
			q.tpotDist = fitTPOTDist(fit, fitIdx, q.step, req.DecodeSlots, req.SpecTokens, req.TPOTModel, req.TPOTDistMinFitSteps)
		}
		q.assumed = append(q.assumed, q.tpotDist.assumedLines()...)
	default:
		return nil, InputErrorf("tpotModel %q, want %q, %q or none", req.TPOTModel, whatif.TPOTRenewalLognormal, whatif.TPOTPerLengthMixture)
	}
	return q, nil
}

// ReasonConfigMismatch is the reason of a backtest refused because its test
// ran another configuration than its fit.
const ReasonConfigMismatch = "ConfigMismatch"

// ConfigMismatchError is a backtest refused because its test ran another
// configuration than its fit: a 400 with ReasonConfigMismatch, so that a
// caller can offer allowConfigMismatch without reading the words.
type ConfigMismatchError struct {
	InputError
	Differences []ConfigMismatch
}

// Unwrap is the InputError, so that IsInputError holds.
func (e ConfigMismatchError) Unwrap() error { return e.InputError }

// targetOf reads a backtest's TTFT target: the target, and the target as
// the answer names it; nil with none.
func targetOf(o *Objective) (*ReportTarget, *Objective, error) {
	if o == nil {
		return nil, nil, nil
	}
	if o.Target == nil || !(*o.Target > 0) {
		return nil, nil, InputErrorf("ttft.target: want seconds above 0")
	}
	stat := o.Statistic
	if stat == "" {
		stat = statisticMean
	}
	pq, err := parseStatistic("ttft.statistic", stat)
	if err != nil {
		return nil, nil, err
	}
	if !slices.Contains(backtestStats, pq) {
		return nil, nil, InputErrorf("ttft.statistic %q: a backtest judges the mean, p50, p90 or p95", stat)
	}
	return &ReportTarget{Seconds: *o.Target, Percentile: pq}, &Objective{Statistic: statisticName(pq), Target: o.Target}, nil
}

// mismatchWords is the differences in words.
func mismatchWords(ds []ConfigMismatch) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = fmt.Sprintf("%s is %s in the fit and %s in the test", d.Field, d.Fit, d.Test)
	}
	return strings.Join(parts, "; ")
}

// notRecordedConfig is what no recording carries, compared never.
var notRecordedConfig = []string{"the router's version and settings (scorers, flow-control limits): no recording carries them; " +
	"only whether the flow-control series is present is read"}

// withFlowControl is c with its flow-control series read over the cycles
// idx of series: present when most cycles read it, so that one missed
// scrape at the cycle a state was read at is no change of configuration.
func withFlowControl(c RunConfig, series []SeriesSample, idx []int) RunConfig {
	if len(idx) == 0 {
		return c
	}
	on := 0
	for _, i := range idx {
		if series[i].FlowControlSeries {
			on++
		}
	}
	v := "absent (off)"
	if 2*on > len(idx) {
		v = "present (on)"
	}
	out := RunConfig{Fields: slices.Clone(c.Fields)}
	for i := range out.Fields {
		if out.Fields[i].Name == ConfigFlowControl {
			out.Fields[i].Value = v
		}
	}
	return out
}

// compareConfig is the fit's configuration against the test's: a field
// recorded on both sides that differs is a mismatch, and so is a structural
// one (the roles, a variant not found); a field one side does not carry, or
// whose value one side could not read, is not compared.
func compareConfig(fit, test RunConfig) BacktestConfig {
	c := BacktestConfig{Match: true, Differences: []ConfigMismatch{}, NotCompared: slices.Clone(notRecordedConfig)}
	tv := map[string]string{}
	for _, f := range test.Fields {
		tv[f.Name] = f.Value
	}
	seen := map[string]bool{}
	for _, f := range fit.Fields {
		seen[f.Name] = true
		t, ok := tv[f.Name]
		switch {
		case !ok || f.Value == "" || t == "":
			c.NotCompared = append(c.NotCompared, f.Name+" (not read on both sides)")
		case f.Value != t:
			c.Match = false
			c.Differences = append(c.Differences, ConfigMismatch{Field: f.Name, Fit: f.Value, Test: t})
		}
	}
	for _, f := range test.Fields {
		if !seen[f.Name] {
			c.NotCompared = append(c.NotCompared, f.Name+" (not read on both sides)")
		}
	}
	return c
}

// stepOf is one step: its start, its cycles (by index into the test), the
// last of them (anchor: -1 with none), whose measurement window ends at the
// step's end, and the cycles in that window (window), whose inputs the step
// is priced at, so that the prediction and the measurement cover the same
// requests.
type stepOf struct {
	start         time.Time
	idx           []int
	anchor        int
	window        []int
	windowStart   time.Time
	windowSeconds float64
}

// defaultMeasureWindow is the rate window a measurement is read over when
// the source does not say (whatif.DefaultMeasureWindow).
const defaultMeasureWindow = whatif.DefaultMeasureWindow

// measureWindowOf is the window, seconds, the latency at l was read over:
// the router's own for the router (whatif.RouterWindowOf, as the
// experimental models' fits read it), defaultMeasureWindow otherwise.
func measureWindowOf(l LatencySample, source string) float64 {
	if source == MeasuredRouter {
		return whatif.RouterWindowOf(l.Router)
	}
	return defaultMeasureWindow
}

// layout is the test's steps, each with its anchor and its measurement
// window, the changes of replicas, and the test's first cycle. A window is
// q.measureWindow seconds when set (a prediction made with no latency to
// read it from), else the one the anchor's latency was read over.
func (q *backtestQuestion) layout() ([]stepOf, []time.Time, time.Time) {
	test := q.test
	first := test.series[q.testIndices[0]].At
	last := test.series[q.testIndices[len(q.testIndices)-1]].At
	start := first.UTC().Truncate(q.step)
	steps := make([]stepOf, int(last.UTC().Truncate(q.step).Sub(start)/q.step)+1)
	for i := range steps {
		steps[i].start, steps[i].anchor = start.Add(time.Duration(i)*q.step), -1
	}
	for _, i := range q.testIndices {
		k := int(test.series[i].At.UTC().Truncate(q.step).Sub(start) / q.step)
		steps[k].idx = append(steps[k].idx, i)
		steps[k].anchor = i
	}
	// Every cycle of the test's source, in time order: a step's window may
	// reach before the split, into cycles whose inputs (never latency) it
	// reads.
	all := make([]int, len(test.series))
	for i := range all {
		all[i] = i
	}
	slices.SortStableFunc(all, func(a, b int) int { return test.series[a].At.Compare(test.series[b].At) })
	// A change of replicas: the cycle at which either role's serving pods
	// moved, across the split too.
	var changes []time.Time
	for j := 1; j < len(all); j++ {
		a, b := test.series[all[j-1]], test.series[all[j]]
		if a.Prefill != b.Prefill || a.Decode != b.Decode {
			changes = append(changes, b.At)
		}
	}
	for k := range steps {
		st := &steps[k]
		if st.anchor < 0 {
			continue
		}
		at := test.series[st.anchor].At
		st.windowSeconds = q.measureWindow
		if !(st.windowSeconds > 0) {
			st.windowSeconds = measureWindowOf(test.latency[st.anchor], q.source)
		}
		st.windowStart = at.Add(-time.Duration(st.windowSeconds * float64(time.Second)))
		for _, i := range all {
			if x := test.series[i].At; x.After(st.windowStart) && !x.After(at) {
				st.window = append(st.window, i)
			}
		}
	}
	return steps, changes, first
}

// predictSteps prices every step of the test from its inputs alone.
func (q *backtestQuestion) predictSteps(ctx context.Context) ([]stepOf, []BacktestStep, error) {
	steps, changes, first := q.layout()
	p := &predictor{pricer: q.fit.pricer, fleets: map[predictKey]Fleet{}, cv2: q.cv2, promptCV: q.promptCV, promptMaxRatio: q.promptMaxRatio,
		headroom: q.fit.doc.Policy.Headroom, assume: q.assume, cachedCost: q.cachedCost, aggregated: !q.pd()}
	out := make([]BacktestStep, 0, len(steps))
	var fl fluid
	for _, st := range steps {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if st.anchor < 0 {
			fl.reset()
		}
		s := q.stepOf(p, st, changes, first, &fl)
		if q.load != nil && st.anchor < 0 {
			q.load.excluded(&s, st.start)
		}
		out = append(out, s)
	}
	return steps, out, nil
}

// run prices and scores every step.
func (q *backtestQuestion) run(ctx context.Context) (*BacktestResponse, error) {
	test := q.test
	q.source = q.measuredSource()
	source := q.source
	steps, predicted, err := q.predictSteps(ctx)
	if err != nil {
		return nil, err
	}
	out := &BacktestResponse{Banner: backtestBanner, Split: q.split, Config: q.config, Step: q.step.Seconds(), Settle: q.settle.Seconds(),
		Measured: source, MeasuredNote: measuredNote(source, q.pd()), Assumed: append([]string{}, q.assumed...), TTFT: q.targetAsked,
		Steps: []BacktestStep{}, Notes: []string{}}
	out.Fit = windowOf(q.fit, nil, q.cutoff)
	out.Test = windowOf(test, q.testIndices, time.Time{})
	if q.load != nil {
		r := q.load.report
		out.LoadSplit = &r
	}
	if c := q.fit.doc.CachedTokenCost; c != nil {
		// The fit's phi prices every step (slo.SwitchTTFTCachedTokenCost).
		cc := *c
		out.CachedTokenCost = &cc
		out.Assumed = append(out.Assumed, "the fit's "+c.Words())
	}
	for k, s := range predicted {
		if st := steps[k]; st.anchor >= 0 {
			scoreStep(&s, measuredOf(test.latency[st.anchor], source, st.windowSeconds, q.pd()), q.target)
		}
		out.Steps = append(out.Steps, s)
	}
	if q.tpotDist != nil {
		q.scoreTPOTDist(steps, out)
	}
	q.finish(out)
	return out, nil
}

// finish adds the summary, the verdict and the notes to a scored answer.
func (q *backtestQuestion) finish(out *BacktestResponse) {
	out.Summary = summarize(out.Steps, q.target != nil)
	unread := 0
	for _, st := range out.Steps {
		if st.Excluded == "" && st.PromptCV == nil {
			unread++
		}
	}
	if unread > 0 {
		from := "the fit's"
		if q.promptCV != nil {
			from = "the test window's, pooled"
		}
		out.Assumed = append(out.Assumed, fmt.Sprintf("the prompt-length spread of %d judged steps was not read (no computed-token histogram): %s is priced", unread, from))
	}
	out.VerdictLines, out.Excluded = verdictOf(out, q.step, !q.pd())
	out.Verdict = strings.Join(out.VerdictLines, " ")
	out.Notes = append(out.Notes, backtestNotes(q)...)
	if n := batchBeyondFitNote(out.Steps); n != "" {
		out.Notes = append(out.Notes, n)
	}
	if q.mixture != nil {
		r := q.mixture.report
		out.RankMixture = &r
	}
	if q.tpotDist != nil {
		r := q.tpotDist.report
		out.TPOTDistribution = &r
		out.Notes = append(out.Notes, q.tpotDist.note())
	}
	out.BacktestID = contentID("backtest", struct {
		Fit, Test    string
		Cutoff       time.Time
		Step, Settle time.Duration
		Measured     string
		Target       *ReportTarget
		CV2          *float64
		Assume       Assume
		Allowed      bool
	}{q.fitID(), q.test.id, q.cutoff, q.step, q.settle, q.source, q.target, q.cv2, q.assume, q.config.Allowed})
	if q.mixture != nil {
		out.BacktestID = contentID("backtest", struct {
			ID, Model, Assumed, Abstained string
			Stratified                    bool
		}{out.BacktestID, q.mixture.report.Model, q.mixture.report.Assumed, q.mixture.report.Abstained, q.mixture.report.Stratified})
	}
	if q.tpotDist != nil {
		out.BacktestID = contentID("backtest", struct{ ID, TPOTModel, Assumed, Abstained string }{out.BacktestID, q.tpotDist.report.Model,
			q.tpotDist.report.Assumed, q.tpotDist.report.Abstained})
	}
	if q.load != nil {
		out.BacktestID = contentID("backtest", struct {
			ID   string
			Load BacktestLoadSplit
		}{out.BacktestID, q.load.report})
	}
}

// fitID is the fit's inputs id, or the prediction's for a comparison.
func (q *backtestQuestion) fitID() string {
	if q.fit == nil {
		return q.predictionHash
	}
	return q.fit.id
}

// pd says the test is a P/D fleet: its inputs name a prefill variant.
func (q *backtestQuestion) pd() bool { return q.test.doc.Fleet.Prefill != nil }

// seriesAt is the samples at idx.
func seriesAt(series []SeriesSample, idx []int) []SeriesSample {
	out := make([]SeriesSample, len(idx))
	for i, j := range idx {
		out[i] = series[j]
	}
	return out
}

// windowOf is one side's window: its cycles at idx (all with nil), or up to
// cutoff when not zero.
func windowOf(e *inputsEntry, idx []int, cutoff time.Time) BacktestWindow {
	w := BacktestWindow{InputsID: e.id, Source: e.doc.Source.Name}
	if c := e.doc.Source.Cycle; idx == nil && cutoff.IsZero() && c > 0 && c <= len(e.series) {
		// A source read at an earlier cycle: its fit ends there.
		cutoff = e.series[c-1].At
	}
	if w.Source == "" && e.doc.Source.From != nil {
		w.Source = fmt.Sprintf("prometheus %s %s to %s", e.doc.Namespace, e.doc.Source.From.UTC().Format(time.RFC3339),
			e.doc.Source.To.UTC().Format(time.RFC3339))
	}
	var ats []time.Time
	if idx != nil {
		for _, i := range idx {
			ats = append(ats, e.series[i].At)
		}
	} else {
		for _, x := range e.series {
			if cutoff.IsZero() || !x.At.After(cutoff) {
				ats = append(ats, x.At)
			}
		}
	}
	if len(ats) > 0 {
		w.From, w.To, w.Cycles = ats[0], ats[len(ats)-1], len(ats)
	}
	return w
}

// measuredSource is the latency the test is scored against: the asked one,
// or the router's where any cycle carries its TTFT histogram.
func (q *backtestQuestion) measuredSource() string {
	if q.measured != MeasuredAuto {
		return q.measured
	}
	for _, i := range q.testIndices {
		if r := q.test.latency[i].Router; r != nil && len(r.TTFTBuckets) > 0 {
			return MeasuredRouter
		}
	}
	return MeasuredEngine
}

// measuredNote says what a measured source's figures are.
func measuredNote(source string, pd bool) string {
	if source == MeasuredRouter {
		return "the router's histograms (server side, the same scrape as the inputs): TTFT from the request's arrival at the router, " +
			"its queue included; TPOT per streamed request. Each step is priced at the inputs of the same window its histogram covers"
	}
	w := "the engines' vLLM histograms (the same scrape as the inputs): the source carries no router series. The engines' mean " +
		"inter-token latency is per token, not the per-request TPOT predicted: the mean TPOT is not scored"
	if pd {
		w += "; on a P/D fleet the engines' TTFT is the prefill pods' alone (no router, KV transfer or decode step): TTFT is not scored"
	}
	return w
}

// burstIn says a step's window holds a change of load near the knee: a cycle
// whose rate is BurstPeakKneeShare of knee or more, and one at
// BurstTroughShare of that rate or less. lo and hi are the least and the
// largest rate read; a cycle with no rate is not read.
func burstIn(samples []SeriesSample, knee float64) (lo, hi float64, ok bool) {
	n := 0
	for _, x := range samples {
		if !x.RateKnown {
			continue
		}
		if n == 0 {
			lo, hi = x.Rate, x.Rate
		}
		lo, hi = min(lo, x.Rate), max(hi, x.Rate)
		n++
	}
	return lo, hi, n > 1 && knee > 0 && hi >= BurstPeakKneeShare*knee && lo <= BurstTroughShare*hi
}

// stepOf predicts one step: its inputs, read over the cycles of its
// measurement window, why it is not judged, and each statistic predicted.
// It reads no latency: scoreStep sets the measurement beside it.
func (q *backtestQuestion) stepOf(p *predictor, st stepOf, changes []time.Time, first time.Time, fl *fluid) BacktestStep {
	out := BacktestStep{Start: st.start, Cycles: len(st.idx), TTFT: []BacktestPoint{}, TPOT: []BacktestPoint{}}
	if st.anchor < 0 || len(st.window) == 0 {
		out.Excluded, out.Detail = ExcludedNoMeasurement, "no cycle in the step"
		return out
	}
	in := stepInputsOf(seriesAt(q.test.series, st.window), q.contextTokens)
	out.Rate, out.Prefill, out.Decode = in.rate, in.prefill, in.decode
	if in.hasShape {
		out.PromptTokens, out.OutputTokens = ptr(in.prompt), ptr(in.output)
	}
	if in.promptCV != nil {
		out.PromptCV = ptr(*in.promptCV)
	}
	end := q.test.series[st.anchor].At
	switch {
	case !(in.rate > 0):
		if math.IsNaN(in.rate) {
			fl.reset() // a rate that is no number says nothing of what drained
		} else {
			fl.idle(q.step.Seconds())
		}
		out.Excluded, out.Detail = ExcludedNoTraffic, "no requests arrived"
		return out
	case st.windowStart.Before(first.Add(q.settle)) || (q.settle == 0 && st.windowStart.Before(first)):
		out.Excluded, out.Detail = ExcludedWarmUp, fmt.Sprintf("its measurement window (%s) starts within %s of the test's first cycle",
			time.Duration(st.windowSeconds*float64(time.Second)), q.settle)
	case slices.ContainsFunc(changes, func(c time.Time) bool { return !c.After(end) && c.After(st.windowStart.Add(-q.settle)) }):
		out.Excluded, out.Detail = ExcludedScaling, fmt.Sprintf("the replicas changed in its measurement window or within %s before it", q.settle)
	case in.otherPods:
		out.Excluded, out.Detail = ExcludedOtherVariants, "variants other than the priced ones also served: the rate and the latency are the whole model's"
	case q.pd() && (in.prefill == 0 || in.decode == 0):
		out.Excluded, out.Detail = ExcludedNotPriced, fmt.Sprintf("%d prefill and %d decode pods served: a P/D fleet is priced with both", in.prefill, in.decode)
	case !q.pd() && in.decode == 0:
		out.Excluded, out.Detail = ExcludedNotPriced, "no aggregated pod served"
	}
	pred, err := p.predict(in)
	if err != nil {
		fl.reset()
		if out.Excluded == "" {
			out.Excluded, out.Detail = ExcludedNotPriced, err.Error()
		}
		return out
	}
	if q.mixture != nil {
		q.priceMixture(&pred, &out, st)
	}
	if q.tpotDist != nil {
		q.priceTPOTDist(&pred, &out, st)
	}
	if pred.prefillKnee != nil && !q.mixturePriced(out) {
		if tr := relaxationSeconds(in.rate, *pred.prefillKnee, in.prefill, pred.ca2, pred.promptCV); tr > 0 && !math.IsInf(tr, 1) {
			out.RelaxationSeconds = ptr(tr)
			out.Unsettled = !settles(tr, st.windowSeconds)
		}
		fl.mu = *pred.prefillKnee
		if w, most := fl.step(in.rate, *pred.prefillKnee, q.step.Seconds(), st.windowSeconds); w > 0 {
			out.Backlog, out.BacklogWait = ptr(fl.carried), ptr(w)
			for i := range pred.ttft {
				if v := pred.ttft[i].Value; v != nil {
					pred.ttft[i].Value = ptr(*v + w)
				}
				if b := pred.ttftBound[i]; b != nil {
					pred.ttftBound[i] = ptr(*b + most)
				}
			}
		}
	} else {
		fl.reset()
	}
	if pred.knee != nil {
		// A burst is judged against a knee the model prices. A prefill knee
		// that rests on a lower bound of V_P is not one: the true knee may be
		// far higher, so whether the window ran near it is unknown. There the
		// step is judged against decode's knee, and a burst against the
		// lower-bound knee alone withholds only the step's TTFT, which a
		// prefill queue building in the burst may set (#280).
		judgeKnee := pred.knee
		if pred.kneeLowerBound {
			judgeKnee = pred.decodeKnee
		}
		samples := seriesAt(q.test.series, st.window)
		if judgeKnee != nil {
			if lo, hi, ok := burstIn(samples, *judgeKnee); ok && out.Excluded == "" && in.rate < *judgeKnee {
				out.Excluded, out.Detail = ExcludedBurst, fmt.Sprintf("its measurement window holds the start or the end of a load near the knee: cycles from %.2f to %.2f r/s against a knee of %.2f r/s, so its mean rate, %.2f r/s, is no rate the fleet ran at",
					lo, hi, *judgeKnee, in.rate)
			}
		}
		if _, _, ok := burstIn(samples, *pred.knee); ok && pred.kneeLowerBound && out.Excluded == "" && in.rate < *pred.knee &&
			!q.mixturePriced(out) {
			out.TTFTBurst = true
		}
		out.KneeShare = ptr(in.rate / *pred.knee)
		out.Load = loadBand(*out.KneeShare)
	}
	if pred.decodeKnee != nil {
		out.DecodeKneeShare = ptr(in.rate / *pred.decodeKnee)
		out.DecodeLoad = loadBand(*out.DecodeKneeShare)
	}
	for i, q := range backtestStats {
		out.TTFT = append(out.TTFT, predictedPoint(q, pred.ttft[i], pred.ttftBound[i]))
		out.TPOT = append(out.TPOT, predictedPoint(q, pred.tpot[i], pred.tpotBound[i]))
	}
	out.BatchBeyondFit = pred.beyondFit
	return out
}

// fluid is the backlog a fluid queue carries from step to step: what runs
// past the prefill knee in one step is still queued at the next.
type fluid struct {
	// q is the backlog at the end of the last step, and carried the one
	// carried into the step being priced; mu is the knee it was last
	// served at.
	q, carried, mu float64
}

// reset drops the backlog: a step that did not price, or had no cycle,
// breaks the chain.
func (f *fluid) reset() { f.q, f.carried, f.mu = 0, 0, 0 }

// step carries the backlog through one step of dt seconds at rate lambda
// and prefill knee mu, and is the mean and the most wait it adds over the
// step's measurement window, its last w seconds (backlogWait). A rate or a
// knee that is no number breaks the chain.
func (f *fluid) step(lambda, mu, dt, w float64) (mean, most float64) {
	if math.IsNaN(lambda) || math.IsNaN(mu) || math.IsNaN(dt) {
		f.reset()
		return 0, 0
	}
	f.carried = f.q
	mean, most = backlogWait(f.q, lambda, mu, dt, w)
	f.q = backlogAt(f.q, lambda, mu, dt)
	return mean, most
}

// idle drains the backlog through a step with no requests, at the knee mu
// last priced.
func (f *fluid) idle(dt float64) {
	if f.mu > 0 {
		f.q = backlogAt(f.q, 0, f.mu, dt)
	} else {
		f.q = 0
	}
	f.carried = 0
}

// scoreStep sets beside a step's predictions what the fleet measured: the
// step is not judged when it measured no request, and each statistic is
// scored where both are figures. It reads the predictions from the step,
// so a step predicted elsewhere scores as one predicted here.
func scoreStep(out *BacktestStep, m measured, target *ReportTarget) {
	if out.Excluded == ExcludedNoTraffic {
		return
	}
	out.Requests = m.requests
	if out.Excluded == "" && !(m.requests > 0) {
		out.Excluded, out.Detail = ExcludedNoMeasurement, "the source measured no request in the step"
	}
	for i := range out.TTFT {
		why := m.ttftWhy[i]
		// A refused figure counts by its refusal, not twice: a step
		// that does not settle or is a burst unscores only a figure
		// priced.
		priced := out.TTFT[i].Predicted.Value != nil
		if why == "" && out.Unsettled && priced {
			why = UnscoredUnsettled
		}
		if why == "" && out.TTFTBurst && priced {
			why = UnscoredBurst
		}
		out.TTFT[i] = scoredPoint(out.TTFT[i], m.ttft[i], m.requests, m.ttftTop, why, m.ttftBucket[i])
	}
	for i := range out.TPOT {
		out.TPOT[i] = scoredPoint(out.TPOT[i], m.tpot[i], m.tpotRequests, m.tpotTop, m.tpotWhy[i], m.tpotBucket[i])
	}
	if target != nil {
		out.Target = judgeTarget(target, out.TTFT, m)
	}
}

// stepInputs is what a step gives the prediction: never its latency.
type stepInputs struct {
	rate            float64
	prefill, decode int
	prompt, output  float64
	hasShape        bool
	// computed is the prompt the step's prefill pods computed a request,
	// their computed tokens over their requests; 0 where they read none.
	computed float64
	// promptCV is the step's own prompt spread, read from its prefill
	// pods' computed-token histograms; nil where they read none.
	promptCV *float64
	// promptMaxRatio is the longest prompt over the mean the same
	// histograms bound (whatif.PromptSpread.MaxRatio), raised to
	// promptCVGrid; 0 with none.
	promptMaxRatio float64
	// outputDispersion is the step's own output spread, D = E[O^2]/E[O]^2
	// over its decode rows (outputDispersionOf), on outputDispersionGrid;
	// 0 where a cycle of the step read none.
	outputDispersion float64
	// otherPods says variants other than the priced ones served in it.
	otherPods bool
}

// stepInputsOf is the step's inputs: its mean rate, the median serving pods
// of each role, and its lengths weighted by rate. contextTokens is the
// engines' context window, which holds the prompt spread's mass past the
// histogram's last bound (whatif.ReadPromptSpreadWithin); 0 where unknown.
func stepInputsOf(samples []SeriesSample, contextTokens float64) stepInputs {
	in := stepInputs{rate: rateOf(samples).Mean}
	a := actualOf(samples)
	in.prefill, in.decode = a.Prefill, a.Decode
	in.prompt, in.output, _, in.hasShape = bucketShape(samples)
	in.otherPods = slices.ContainsFunc(samples, func(x SeriesSample) bool { return x.OtherPods > 0 })
	in.outputDispersion = outputDispersionOf(samples)
	pooled := map[float64]float64{}
	var computed, requests, computedAll, requestsAll float64
	for _, x := range samples {
		if !(x.PromptRequestRate > 0) {
			continue
		}
		computedAll += x.PromptComputedRate
		requestsAll += x.PromptRequestRate
		if x.PromptBuckets == nil {
			continue
		}
		computed += x.PromptComputedRate
		requests += x.PromptRequestRate
		for b, r := range x.PromptBuckets {
			pooled[b] += r
		}
	}
	if requestsAll > 0 {
		in.computed = computedAll / requestsAll
	}
	if requests > 0 {
		if sp := whatif.ReadPromptSpreadWithin(pooled, computed/requests, contextTokens); !sp.Assumed {
			in.promptCV = ptr(math.Round(sp.CV/promptCVGrid) / (1 / promptCVGrid))
			in.promptMaxRatio = maxRatioOnGrid(sp.MaxRatio)
		}
	}
	return in
}

// promptCVGrid is the grid a step's prompt spread is priced on, so that
// steps of nearly one spread share a fleet.
const promptCVGrid = 0.05

// outputDispersionGrid is the grid a step's output spread is priced on, so
// that steps of nearly one spread share a fleet.
const outputDispersionGrid = 0.05

// outputDispersionOf is the step's output spread, D = E[O^2]/E[O]^2 over
// its cycles' decode rows, each cycle weighted by its rate as bucketShape
// weights the lengths, rounded to outputDispersionGrid: 0 where a cycle
// that read an output length read no E[O^2] (the two must be one reading of
// the same rows), or none read a length. Pooled over the step, not the
// mean of each cycle's D: lengths that vary from cycle to cycle are a
// spread the step's requests hold too.
func outputDispersionOf(samples []SeriesSample) float64 {
	var w, ol, sq float64
	for _, x := range samples {
		if !x.HasShape {
			continue
		}
		if !(x.OutputTokensSquared > 0) || !(x.OutputTokens > 0) {
			return 0
		}
		wx := x.Rate
		if !(wx > 0) {
			wx = 1e-9
		}
		w += wx
		ol += wx * x.OutputTokens
		sq += wx * x.OutputTokensSquared
	}
	if !(w > 0) {
		return 0
	}
	d := shape.Dispersion(ol/w, sq/w)
	return math.Round(d/outputDispersionGrid) * outputDispersionGrid
}

// maxRatioOnGrid is a prompt bound raised to promptCVGrid, so that steps of
// nearly one bound share a fleet: raised, as a higher bound prices more of
// the gamma's tail, never less than the histogram allows. 0 stays 0.
func maxRatioOnGrid(r float64) float64 {
	if !(r > 0) {
		return 0
	}
	// The tolerance keeps a bound already on the grid (1.25) on it.
	return math.Ceil(r/promptCVGrid-1e-9) / (1 / promptCVGrid)
}

// predictKey is one fleet a backtest prices.
type predictKey struct {
	np, nd           int
	prompt, output   float64
	computed         float64
	promptCV         float64
	promptMaxRatio   float64
	outputDispersion float64
	q                float64
}

// predictor prices steps from the fit alone: the Pricer, and the inputs of
// each step. It never sees a latency.
type predictor struct {
	pricer   Pricer
	fleets   map[predictKey]Fleet
	cv2      *float64
	promptCV *float64
	// promptMaxRatio is promptCV's bound, for a step whose own reads none.
	promptMaxRatio float64
	// headroom is the fit's: it moves no TTFT or TPOT priced at a rate.
	headroom float64
	// assume is the request's typed terms.
	assume Assume
	// cachedCost is Knobs.CachedTokenCost at every step.
	cachedCost *float64
	// aggregated says the fleet has no prefill variant: each step is priced
	// at its decode pods alone, which serve both roles.
	aggregated bool
}

// prediction is one step's figures at each statistic of backtestStats.
type prediction struct {
	ttft, tpot           []Figure
	ttftBound, tpotBound []*float64
	// knee is the lower knee of the fleet that priced, decodeKnee decode's
	// and prefillKnee prefill's; nil with none.
	knee, decodeKnee, prefillKnee *float64
	// kneeLowerBound says knee is the prefill knee and rests on a lower
	// bound of V_P (whatif.VPLowerBound).
	kneeLowerBound bool
	// promptCV is the prompt spread priced, and ca2 the arrivals' (1 for
	// random arrivals).
	promptCV, ca2 float64
	// beyondFit is the batch the step's decode runs past its line's fit
	// (BacktestStep.BatchBeyondFit), nil inside it.
	beyondFit *BatchBeyondFit
}

// predict prices one step at each statistic: the TTFT as a report's table
// prices it (TTFTPoint), bounded by the top of its span over V_P and f; the
// TPOT, bounded by the top of its band. Each carries the envelope's verdict.
func (p *predictor) predict(in stepInputs) (prediction, error) {
	var out prediction
	if !(in.rate > 0) || (in.prefill == 0 && !p.aggregated) || in.decode == 0 {
		return out, errors.New("no fleet to price")
	}
	for _, q := range backtestStats {
		k := predictKey{np: in.prefill, nd: in.decode, q: q, promptCV: -1, outputDispersion: in.outputDispersion}
		if in.hasShape {
			k.prompt, k.output = shapeKnob(in.prompt), shapeKnob(in.output)
			// The computed prompt on the same grid, never above the whole
			// one: it is read only with slo.SwitchTTFTPrefixAttention on.
			if in.computed > 0 {
				k.computed = min(shapeKnob(in.computed), k.prompt)
			}
		}
		// A spread read from a histogram carries the bound that histogram
		// sets on the prompt; a typed one (assume.promptCV) carries none.
		cv := p.assume.PromptCV
		if cv == nil {
			switch {
			case in.promptCV != nil:
				cv, k.promptMaxRatio = ptr(max(*in.promptCV, promptCVGrid)), in.promptMaxRatio
			case p.promptCV != nil:
				cv, k.promptMaxRatio = p.promptCV, p.promptMaxRatio
			}
		}
		// The key is the spread priced: none (-1, the fit's) or its value.
		if cv != nil {
			k.promptCV = *cv
		}
		fl, ok := p.fleets[k]
		if !ok {
			// The step's prompt is read from its cycles, not stated
			// (Knobs.PromptTokensRead).
			kn := Knobs{Prefill: k.np, Decode: k.nd, PromptTokens: k.prompt, PromptTokensRead: k.prompt > 0, OutputTokens: k.output, TTFTPercentile: q, TPOTPercentile: q,
				ArrivalCV2: p.cv2, Headroom: p.headroom}
			kn.Assume = p.assume
			kn.Assume.PromptCV = cv
			kn.PromptMaxRatio = k.promptMaxRatio
			kn.OutputDispersion = k.outputDispersion
			kn.CachedTokenCost = p.cachedCost
			kn.PrefillComputedTokens = k.computed
			var err error
			if fl, err = p.pricer.Fleet(kn); err != nil {
				return out, err
			}
			p.fleets[k] = fl
		}
		class := fl.Class()
		t, _ := fl.TTFTPoint(in.rate)
		tf := figureOf(t, unitSeconds, true)
		share := 0.0
		if v, ok := t.(whatif.TTFT); ok {
			k := v.PrefillKnee
			if v.DecodeKnee > 0 && (k <= 0 || v.DecodeKnee < k) {
				k = v.DecodeKnee
			}
			share = shareOf(in.rate, k)
		}
		tf = validate(tf, whatif.FigureTTFT, q, class, share)
		var tb *float64
		if tf.Value != nil {
			if span := figureOf(fl.TTFTAt(in.rate), unitSeconds, true); span.Sensitivity != nil && span.Sensitivity.Hi != nil {
				tb = ptr(max(*tf.Value, *span.Sensitivity.Hi))
			}
		}
		mean, pct := fl.TPOTAt(in.rate)
		r, kind := mean, whatif.FigureMeanTPOT
		if q != 0 {
			r, kind = pct, whatif.FigurePercentileTPOT
		}
		pf := figureOf(r, unitSeconds, true)
		kneeShare := 0.0
		if v, ok := valueOf(fl.DecodeKnee()); ok {
			kneeShare = shareOf(in.rate, v)
		}
		pf = validate(pf, kind, q, class, kneeShare)
		if q == 0 {
			out.beyondFit = stepBeyondFit(pf, fl.DecodeKnee(), in.rate)
		}
		var pb *float64
		if pf.Value != nil && pf.Band != nil {
			pb = ptr(max(*pf.Value, pf.Band.Hi))
		}
		if out.knee == nil {
			var ks []float64
			for _, r := range []whatif.Result{fl.PrefillKnee(), fl.DecodeKnee()} {
				if v, ok := valueOf(r); ok && v > 0 {
					ks = append(ks, v)
				}
			}
			if len(ks) > 0 {
				out.knee = ptr(slices.Min(ks))
			}
			if v, ok := valueOf(fl.DecodeKnee()); ok && v > 0 {
				out.decodeKnee = ptr(v)
			}
			if v, ok := valueOf(fl.PrefillKnee()); ok && v > 0 {
				out.prefillKnee = ptr(v)
			}
			if a, ok := fl.PrefillKnee().(whatif.Answer); ok && out.knee != nil && a.Value == *out.knee && a.HasFlag(whatif.VPLowerBound) {
				out.kneeLowerBound = true
			}
			out.promptCV, out.ca2 = whatif.AssumedPromptCV, 1
			if cv != nil {
				out.promptCV = *cv
			}
			if p.cv2 != nil {
				out.ca2 = *p.cv2
			}
		}
		out.ttft, out.ttftBound = append(out.ttft, tf), append(out.ttftBound, tb)
		out.tpot, out.tpotBound = append(out.tpot, pf), append(out.tpotBound, pb)
	}
	return out, nil
}

// stepBeyondFit is a step's batch past its decode line's fit: the mean TPOT's
// (mean), or, where the step's rate is at or past the decode knee and the
// TPOT is refused, the knee's, priced at the batch the knee runs. Nil inside
// the fit's batch.
func stepBeyondFit(mean Figure, knee whatif.Result, rate float64) *BatchBeyondFit {
	if b := mean.BatchBeyondFit; b != nil {
		c := *b
		return &c
	}
	k, ok := knee.(whatif.Answer)
	if !ok || mean.Refused == nil || k.Basis.Batch == nil || rate < k.Value {
		return nil
	}
	b := k.Basis.Batch
	return &BatchBeyondFit{Priced: b.Priced, Fitted: b.Fitted, Ranks: b.Ranks}
}

// batchBeyondFitNote is the note a backtest adds where steps ran past the
// batch the fit's decode line was fitted on (BacktestStep.BatchBeyondFit):
// which, and the batches; "" with none.
func batchBeyondFitNote(steps []BacktestStep) string {
	var at []string
	var fitted, lo, hi int64
	ranks := 0
	for _, s := range steps {
		b := s.BatchBeyondFit
		if b == nil || s.Excluded != "" {
			continue
		}
		at = append(at, s.Start.UTC().Format("15:04"))
		fitted, ranks = b.Fitted, b.Ranks
		if lo == 0 || b.Priced < lo {
			lo = b.Priced
		}
		hi = max(hi, b.Priced)
	}
	if len(at) == 0 {
		return ""
	}
	runs := fmt.Sprintf("about %d", lo)
	if hi != lo {
		runs = fmt.Sprintf("about %d to %d", lo, hi)
	}
	return fmt.Sprintf("BatchBeyondFit on %d judged steps (%s): the decode line was fitted on up to %d sequences %s; these steps run %s, "+
		"so TPOT may read low and the decode knee high. The figures are priced all the same.", len(at), strings.Join(at, ", "), fitted,
		whatif.BatchUnit(ranks), runs)
}

// measured is one step's measured figures at each statistic of
// backtestStats: +Inf where the histogram cannot say (above its top
// bucket), nil with none, and why a figure is not comparable ("" where it
// is). requests and tpotRequests are the requests each histogram holds
// over its window; ttftTop and tpotTop each histogram's top finite bound.
type measured struct {
	ttft, tpot             []*float64
	ttftWhy, tpotWhy       []string
	ttftBucket, tpotBucket []*Band
	requests, tpotRequests float64
	ttftTop, tpotTop       float64
}

// measuredOf is the latency the cycle l read, over its window of
// windowSeconds: the router's histograms and means, or the engines'. On a
// P/D fleet the engines' TTFT is the prefill pods' alone, and the engines'
// TPOT mean is a mean inter-token latency: neither is the figure the
// planner predicts, so each is marked not comparable rather than scored.
func measuredOf(l LatencySample, source string, windowSeconds float64, pd bool) measured {
	n := len(backtestStats)
	m := measured{ttft: make([]*float64, n), tpot: make([]*float64, n), ttftWhy: make([]string, n), tpotWhy: make([]string, n),
		ttftBucket: make([]*Band, n), tpotBucket: make([]*Band, n)}
	var tb, pb []domain.LatencyBucket
	var ttftMean, tpotMean *float64
	switch {
	case source == MeasuredRouter && l.Router != nil:
		r := l.Router
		tb, pb = r.TTFTBuckets, r.TPOTBuckets
		if r.TTFTCountRate > 0 {
			ttftMean = ptr(r.TTFTSumRate / r.TTFTCountRate)
		}
		if r.TPOTCountRate > 0 {
			tpotMean = ptr(r.TPOTSumRate / r.TPOTCountRate)
		}
	case source == MeasuredEngine:
		tb, pb = l.EngineTTFT, l.EngineTPOT
		if l.EngineTTFTMean > 0 {
			ttftMean = ptr(l.EngineTTFTMean)
		}
		for i := range n {
			if pd {
				m.ttftWhy[i] = UnscoredNotComparable
			}
		}
		m.tpotWhy[0] = UnscoredNotComparable
	}
	th, tok := histOf(tb)
	ph, pok := histOf(pb)
	if tok {
		m.requests, m.ttftTop = th.Total*windowSeconds, th.Top()
	}
	if pok {
		m.tpotRequests, m.tpotTop = ph.Total*windowSeconds, ph.Top()
	}
	for i, q := range backtestStats {
		switch q {
		case 0:
			m.ttft[i], m.tpot[i] = ttftMean, tpotMean
		default:
			if tok {
				m.ttft[i], m.ttftBucket[i] = ptr(th.Quantile(q)), th.coarseBucket(q)
			}
			if pok {
				m.tpot[i], m.tpotBucket[i] = ptr(ph.Quantile(q)), ph.coarseBucket(q)
				if source == MeasuredRouter && (q == 0.9 || q == 0.95) {
					m.tpot[i], m.tpotBucket[i] = jointTPOT(l, windowSeconds, q, m.tpot[i], m.tpotBucket[i])
				}
			}
		}
	}
	return m
}

// jointTPOT is the router's measured TPOT q-quantile read from every
// per-request TPOT histogram the cycle l holds at once (whatif.JointBracket:
// the router's and, above their first edge, the decode engines', at their
// sampling noise): the bracket's point, and the bracket where it is too wide
// to place the quantile (MeasuredBucketWidth), the pair then unresolvable
// with the prediction inside it. Where the bracket is open above (the
// quantile within noise of the top edge) the router's own reading, meas and
// bucket, stands.
func jointTPOT(l LatencySample, windowSeconds, q float64, meas *float64, bucket *Band) (*float64, *Band) {
	r := l.Router
	if r == nil || len(r.TPOTBuckets) == 0 {
		return meas, bucket
	}
	edges, cum := make([]float64, len(r.TPOTBuckets)), make([]float64, len(r.TPOTBuckets))
	for i, b := range r.TPOTBuckets {
		edges[i], cum[i] = b.UpperBound, b.Rate
	}
	counts := make([]float64, len(cum))
	prev := 0.0
	for i, c := range cum {
		c = max(prev, c)
		counts[i] = (c - prev) * windowSeconds
		prev = c
	}
	st := withEngineTPOT(whatif.TPOTDistStep{Edges: edges, Counts: counts}, l.EngineTPOT)
	b, ok := whatif.JointBracket(st, q)
	if !ok || math.IsInf(b.Hi, 1) || !whatif.Finite(b.Point) {
		return meas, bucket
	}
	p := ptr(b.Point)
	if meas != nil && math.Abs(b.Point-*meas) <= 1e-9*math.Abs(*meas) {
		// The router's own linear reading, recomputed from shares in place
		// of rates: keep the router's figure, not its rounding.
		p = meas
	}
	if b.Lo > 0 && b.Hi <= (1+MeasuredBucketWidth)*b.Lo*(1+bucketEps) {
		return p, nil
	}
	return p, &Band{Lo: b.Lo, Hi: b.Hi}
}

// latencyHist is a cumulative latency histogram (whatif.LatencyHist, the
// one the slo analyzer's v2 models read), with the backtest's bucket
// placement (coarseBucket).
type latencyHist struct {
	whatif.LatencyHist
}

// histOf is the histogram in, repaired as Prometheus's histogram_quantile
// repairs one (whatif.LatencyHistOf); ok false with no +Inf bucket or no
// request.
func histOf(in []domain.LatencyBucket) (latencyHist, bool) {
	h, ok := whatif.LatencyHistOf(in)
	return latencyHist{h}, ok
}

// coarseBucket is the finite bucket holding the q-quantile where it is too
// wide to place the quantile: U - L over MeasuredBucketWidth L, any bucket
// from 0 included; nil where it is narrow enough, or the quantile is above
// the top bucket (scored as a miss, not placed).
func (h latencyHist) coarseBucket(q float64) *Band {
	rank := q * h.Total
	for i, b := range h.Buckets {
		if b.Rate < rank {
			continue
		}
		if math.IsInf(b.UpperBound, 1) {
			return nil
		}
		lower := 0.0
		if i > 0 {
			lower = h.Buckets[i-1].UpperBound
		}
		if lower > 0 && b.UpperBound <= (1+MeasuredBucketWidth)*lower*(1+bucketEps) {
			return nil
		}
		return &Band{Lo: lower, Hi: b.UpperBound}
	}
	return nil
}

// minRequestsFor is the fewest requests a step's measured statistic needs:
// 50 for the mean, else ten beyond the percentile, max(50, 10/(1-q)), as the
// slo analyzer judges a window.
func minRequestsFor(q float64) float64 {
	if q == 0 {
		return 50
	}
	return max(50, math.Round(10/(1-q)))
}

// predictedPoint is one statistic as predicted, before any measurement.
func predictedPoint(q float64, pred Figure, bound *float64) BacktestPoint {
	return BacktestPoint{Statistic: statisticName(q), Predicted: pred, Bound: bound,
		InRegion: pred.Validation != nil && pred.Validation.Validated}
}

// scoredPoint is p with the measured figure meas beside it, from a
// histogram holding the given count of requests (its +Inf bucket over the
// window), whose top finite bound is top, scored
// where both are figures. A measured figure above the top bucket is a miss
// the planner read low wherever its own figure is under the top bound: it
// is scored at the top bound, a ratio no lower than the truth's, and never
// within. why is a measurement not comparable. bucket is the measured
// percentile's bucket where it is too wide to place it: with the prediction
// more than BacktestTolerance beyond its nearer edge, a certain miss, scored
// against that edge (OutsideBucket), never within; otherwise the figure is
// shown and not scored, unresolvable.
func scoredPoint(p BacktestPoint, meas *float64, requests, top float64, why string, bucket *Band) BacktestPoint {
	q, _ := parseStatistic("", p.Statistic)
	pred := p.Predicted
	switch {
	case why != "":
		p.Unscored = why
		return p
	case meas == nil:
		p.Unscored = UnscoredNotMeasured
		return p
	}
	if requests >= minRequestsFor(q) && bucket != nil {
		// Beside a refusal too: the truth is somewhere in it.
		p.MeasuredBucket = bucket
	}
	switch {
	case requests < minRequestsFor(q):
		// Too few to read the statistic: no measured figure, not even
		// beside a refusal (what the fleet measured where it was refused).
		p.Unscored = UnscoredFewRequests
		return p
	case math.IsInf(*meas, 1):
		// JSON holds no +Inf: a figure above the top bucket is AboveHistogram,
		// or unscored aboveHistogram, with no measured value.
	case !whatif.Finite(*meas) || *meas < 0:
		p.Unscored = UnscoredNotMeasured
		return p
	default:
		p.Measured = meas
	}
	switch {
	case pred.Value == nil:
		p.Unscored = UnscoredRefused
	case math.IsInf(*meas, 1) && top > 0 && *pred.Value > 0 && *pred.Value < top:
		p.Ratio, p.AboveHistogram = ptr(*pred.Value/top), true
	case math.IsInf(*meas, 1):
		p.Unscored = UnscoredAboveHistogram
	case bucket != nil && *pred.Value > 0 && *pred.Value < (1-BacktestTolerance)*bucket.Lo:
		p.Ratio, p.OutsideBucket = ptr(*pred.Value/bucket.Lo), OutsideUnder
	case bucket != nil && *pred.Value > (1+BacktestTolerance)*bucket.Hi:
		p.Ratio, p.OutsideBucket = ptr(*pred.Value/bucket.Hi), OutsideOver
	case bucket != nil:
		p.Unscored = UnscoredUnresolvable
		p.SameBucket = inBucket(*pred.Value, *bucket)
	case *meas > 0 && *pred.Value > 0:
		p.Ratio = ptr(*pred.Value / *meas)
	default:
		p.Unscored = UnscoredNotMeasured
	}
	if p.Ratio != nil && lowerBoundVPFigure(pred) && !readLow(p) {
		// Scored apart, its ratio kept (UnscoredVPLowerBound). One read
		// more than BacktestTolerance low stays a point: a miss.
		p.Unscored = UnscoredVPLowerBound
	}
	return p
}

// inBucket reports whether v falls in the histogram bucket b, (Lo, Hi]: a
// figure on a bound belongs to the bucket that bound closes. The edges carry
// bucketEps, as the bounds are written in decimal.
func inBucket(v float64, b Band) bool {
	return v > b.Lo*(1+bucketEps) && v <= b.Hi*(1+bucketEps)
}

// readUnder says a pair with a ratio read under what was measured: the
// measured figure above the histogram's top bucket, or in a coarse bucket
// wholly above the prediction, or the ratio under 1. Strict: no tolerance.
func readUnder(p BacktestPoint) bool {
	return p.AboveHistogram || p.OutsideBucket == OutsideUnder || *p.Ratio < 1
}

// readLow says a pair with a ratio read more than BacktestTolerance under
// what was measured: above the histogram's top bucket, under a coarse
// bucket wholly above it, or the ratio under 1 - BacktestTolerance.
func readLow(p BacktestPoint) bool {
	return p.AboveHistogram || p.OutsideBucket == OutsideUnder || *p.Ratio < 1-BacktestTolerance
}

// judgeTarget is the step against the TTFT target, predicted (from its
// points) and measured; a measured figure above the top bucket is a miss
// only where the target is under that bucket. A measured percentile in a
// bucket too wide to place it is judged by the bucket's edges: a histogram
// bucket counts requests over L up to U, so the target is met at or above
// U, missed at or under L, and unknown between.
func judgeTarget(target *ReportTarget, points []BacktestPoint, m measured) *BacktestJudged {
	i := slices.Index(backtestStats, target.Percentile)
	j := &BacktestJudged{Predicted: judgedUnknown, Measured: judgedUnknown}
	if i < 0 || i >= len(points) {
		return j
	}
	if v := points[i].Predicted.Value; v != nil {
		switch {
		case *v <= target.Seconds:
			j.Predicted = judgedMet
		case lowerBoundVPFigure(points[i].Predicted):
			// Over the target from a lower bound of V_P: the miss is not
			// known (CellVPLowerBound).
		default:
			j.Predicted = judgedMissed
		}
	}
	v := m.ttft[i]
	switch {
	case v == nil || m.ttftWhy[i] != "" || m.requests < minRequestsFor(target.Percentile):
	case math.IsInf(*v, 1):
		if target.Seconds < m.ttftTop {
			j.Measured = judgedMissed
		}
	case m.ttftBucket[i] != nil:
		switch b := m.ttftBucket[i]; {
		case target.Seconds >= b.Hi:
			j.Measured = judgedMet
		case target.Seconds <= b.Lo:
			j.Measured = judgedMissed
		}
	case *v <= target.Seconds:
		j.Measured = judgedMet
	default:
		j.Measured = judgedMissed
	}
	return j
}

// summarize scores the steps.
func summarize(steps []BacktestStep, target bool) BacktestSummary {
	s := BacktestSummary{Steps: len(steps), TTFT: []BacktestScore{}, TPOT: []BacktestScore{}}
	for _, st := range steps {
		switch st.Excluded {
		case "":
			s.Judged++
		case ExcludedNoTraffic:
			s.Excluded.NoTraffic++
		case ExcludedNoMeasurement:
			s.Excluded.NoMeasurement++
		case ExcludedScaling:
			s.Excluded.Scaling++
		case ExcludedWarmUp:
			s.Excluded.WarmUp++
		case ExcludedNotPriced:
			s.Excluded.NotPriced++
		case ExcludedOtherVariants:
			s.Excluded.OtherVariants++
		case ExcludedBurst:
			s.Excluded.Burst++
		case ExcludedLoad:
			s.Excluded.Load++
		}
	}
	s.TTFT, s.TPOT = scoresOf(steps)
	s.ByLoad = []BacktestLoad{}
	for _, band := range []string{LoadBelow80, LoadNearKnee, LoadPastKnee} {
		var in, dec []BacktestStep
		for _, st := range steps {
			if st.Excluded != "" {
				continue
			}
			if st.Load == band {
				in = append(in, st)
			}
			if st.DecodeLoad == band {
				dec = append(dec, st)
			}
		}
		l := BacktestLoad{Load: band, Steps: len(in), DecodeSteps: len(dec)}
		l.TTFT, _ = scoresOf(in)
		_, l.TPOT = scoresOf(dec)
		s.ByLoad = append(s.ByLoad, l)
	}
	if target {
		a := &BacktestAgreement{}
		for _, st := range steps {
			if st.Excluded != "" || st.Target == nil {
				continue
			}
			a.Steps++
			switch {
			case st.Target.Predicted == judgedUnknown || st.Target.Measured == judgedUnknown:
				a.NotJudged++
			case st.Target.Predicted == judgedMet && st.Target.Measured == judgedMet:
				a.BothMet++
			case st.Target.Predicted == judgedMissed && st.Target.Measured == judgedMissed:
				a.BothMissed++
			case st.Target.Predicted == judgedMet:
				a.MissedMisses++
			default:
				a.FalseAlarms++
			}
		}
		s.Target = a
	}
	return s
}

// noteSameBucket counts one step consistent at bucket b's resolution.
func (sc *BacktestScore) noteSameBucket(b Band) {
	sc.SameBucket++
	w := 0.0
	if b.Lo > 0 {
		w = b.Hi / b.Lo
	}
	if sc.SameBucketBand == nil {
		sc.SameBucketBand, sc.SameBucketWidth = &Band{Lo: b.Lo, Hi: b.Hi}, w
		return
	}
	sc.SameBucketBand.Lo, sc.SameBucketBand.Hi = min(sc.SameBucketBand.Lo, b.Lo), max(sc.SameBucketBand.Hi, b.Hi)
	if w == 0 || sc.SameBucketWidth == 0 {
		sc.SameBucketWidth = 0
	} else {
		sc.SameBucketWidth = min(sc.SameBucketWidth, w)
	}
}

// SameBucketWords is the steps consistent at the histogram's resolution, in
// words with the bucket and its width, "" with none: "in the measured
// bucket, 10 ms to 20 ms (2.0x wide), in 97": never a share within.
func (sc BacktestScore) SameBucketWords() string {
	if sc.SameBucket == 0 || sc.SameBucketBand == nil {
		return ""
	}
	b := sc.SameBucketBand
	width := "from 0"
	if sc.SameBucketWidth > 0 {
		width = fmt.Sprintf("%.1fx wide", sc.SameBucketWidth)
		if b.Hi > b.Lo*sc.SameBucketWidth*(1+bucketEps) {
			width = fmt.Sprintf("buckets %.1fx wide or wider", sc.SameBucketWidth)
		}
	}
	return fmt.Sprintf("in the measured histogram bucket itself in %d, %s to %s (%s): consistent at the bucket's resolution, not within %.0f%%",
		sc.SameBucket, secondsWords(b.Lo), secondsWords(b.Hi), width, BacktestTolerance*100)
}

// loadBand is the band of a rate at share of the knee.
func loadBand(share float64) string {
	switch {
	case share < 0.8:
		return LoadBelow80
	case share < 1:
		return LoadNearKnee
	}
	return LoadPastKnee
}

// scoresOf is each statistic's TTFT and TPOT score over the judged steps.
func scoresOf(steps []BacktestStep) (ttft, tpot []BacktestScore) {
	ttft, tpot = make([]BacktestScore, 0, len(backtestStats)), make([]BacktestScore, 0, len(backtestStats))
	for i, q := range backtestStats {
		ttft = append(ttft, scoreOf(q, steps, func(st BacktestStep) []BacktestPoint { return st.TTFT }, i))
		tpot = append(tpot, scoreOf(q, steps, func(st BacktestStep) []BacktestPoint { return st.TPOT }, i))
	}
	return ttft, tpot
}

// scoreOf is statistic i's score over the judged steps.
func scoreOf(q float64, steps []BacktestStep, points func(BacktestStep) []BacktestPoint, i int) BacktestScore {
	sc := BacktestScore{Statistic: statisticName(q), Unscored: map[string]int{}, Refused: map[string]int{}, Outside: map[string]int{}}
	var all, in, apart []BacktestPoint
	for _, st := range steps {
		ps := points(st)
		if st.Excluded != "" || i >= len(ps) {
			continue
		}
		p := ps[i]
		if p.Ratio != nil && lowerBoundVPFigure(p.Predicted) {
			apart = append(apart, p)
			if p.Unscored == UnscoredVPLowerBound {
				// Not a point: counted unscored, and scored apart only.
				sc.Unscored[p.Unscored]++
				continue
			}
			// Read more than BacktestTolerance low: a point miss as well.
		}
		if p.Ratio == nil {
			if p.Unscored != "" {
				sc.Unscored[p.Unscored]++
			}
			if p.SameBucket && p.MeasuredBucket != nil {
				sc.noteSameBucket(*p.MeasuredBucket)
			}
			if r := p.Predicted.Refused; r != nil {
				sc.Refused[r.Reason]++
				if lo, hi, ok := measuredRange(p); r.Reason == string(whatif.Saturated) && ok {
					if sc.PastKnee == nil {
						sc.PastKnee = &Band{Lo: lo, Hi: hi}
					}
					sc.PastKnee.Lo, sc.PastKnee.Hi = min(sc.PastKnee.Lo, lo), max(sc.PastKnee.Hi, hi)
				}
			}
			continue
		}
		all = append(all, p)
		switch {
		case p.InRegion:
			in = append(in, p)
		case p.Predicted.Validation != nil && p.Predicted.Validation.Outside != "":
			sc.Outside[outsideKey(p.Predicted.Validation.Outside)]++
		}
	}
	sc.All, sc.InRegion, sc.VPLowerBound = statsOf(all), statsOf(in), apartOf(apart)
	return sc
}

// measuredRange is what p's measured figure says: the value, or the bucket
// it fell in where that is too wide to place it (the interpolated value
// would read the grid); ok false with no finite measurement.
func measuredRange(p BacktestPoint) (lo, hi float64, ok bool) {
	switch m := p.Measured; {
	case p.MeasuredBucket != nil:
		return p.MeasuredBucket.Lo, p.MeasuredBucket.Hi, true
	case m != nil && whatif.Finite(*m):
		return *m, *m, true
	}
	return 0, 0, false
}

// outsideKey is an envelope's reason for a figure outside it, cut to its
// words before the first figure and the first clause.
func outsideKey(why string) string {
	why, _, _ = strings.Cut(why, ",")
	if i := strings.IndexAny(why, "0123456789"); i > 0 {
		why = why[:i]
	}
	return strings.TrimSpace(why)
}

// statsOf is the score over ps, each with a ratio.
func statsOf(ps []BacktestPoint) BacktestStats {
	s := BacktestStats{Steps: len(ps)}
	if len(ps) == 0 {
		return s
	}
	ratios := make([]float64, len(ps))
	errs := make([]float64, len(ps))
	covered, within := 0, 0
	for i, p := range ps {
		ratios[i] = *p.Ratio
		errs[i] = math.Abs(math.Log(*p.Ratio))
		if c, known := coveredBy(p); known {
			s.CoverageSteps++
			if c {
				covered++
			}
		}
		if !p.AboveHistogram && p.OutsideBucket == "" && math.Abs(*p.Ratio-1) <= BacktestTolerance {
			within++
		}
		if p.AboveHistogram || *p.Ratio < 1-BacktestTolerance {
			s.Low++
		}
	}
	n := float64(len(ps))
	s.Bias = ptr(median(ratios))
	s.MedianError = ptr(math.Expm1(median(errs)))
	s.P90Error = ptr(math.Expm1(nearestRank(errs, 0.9)))
	s.Within = ptr(float64(within) / n)
	if s.CoverageSteps > 0 {
		s.Coverage = ptr(float64(covered) / float64(s.CoverageSteps))
	}
	return s
}

// coveredBy says whether p's measured figure was at or under its upper
// bound; known false with no bound, or with the measured figure in a coarse
// bucket the bound falls inside (it could be either side).
func coveredBy(p BacktestPoint) (covered, known bool) {
	switch {
	case p.Bound == nil:
		return false, false
	case p.AboveHistogram:
		return false, true
	case p.MeasuredBucket != nil && *p.Bound >= p.MeasuredBucket.Hi:
		return true, true
	case p.MeasuredBucket != nil && *p.Bound <= p.MeasuredBucket.Lo:
		return false, true
	case p.MeasuredBucket != nil:
		return false, false
	}
	return *p.Measured <= *p.Bound, true
}

// median is the median of xs, the mean of the middle two of an even count.
func median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// nearestRank is the nearest-rank q-quantile of xs.
func nearestRank(xs []float64, q float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[max(int(math.Ceil(q*float64(len(s))))-1, 0)]
}

// leanWords is a statistic's lean in words: its median ratio, and how wide
// its errors are. It never calls a lean safe: a median above 1 can hide
// steps it read low, which the verdict counts beside it.
func leanWords(a BacktestStats) string {
	lean := "no overall lean"
	switch r := *a.Bias; {
	case r > 1.05:
		lean = "it leans high"
	case r < 0.95:
		lean = "it leans low"
	}
	if *a.Within < 0.5 {
		lean = "its errors are wide; " + lean
	}
	return fmt.Sprintf("%s (median ratio %.2f)", lean, *a.Bias)
}

// durationWords is d in hours and minutes, or minutes.
func durationWords(d time.Duration) string {
	m := int(math.Round(d.Minutes()))
	switch {
	case m < 60:
		return strconv.Itoa(m) + " min"
	case m%60 == 0:
		return strconv.Itoa(m/60) + " h"
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}

// numberWords are the step lengths a verdict names in words.
var numberWords = map[int]string{1: "one", 2: "two", 5: "five", 10: "ten", 15: "fifteen", 30: "thirty"}

// stepWords is the step in words: "one-minute steps", "five-minute steps".
func stepWords(step time.Duration) string {
	m := int(step.Minutes())
	n, ok := numberWords[m]
	if !ok {
		n = strconv.Itoa(m)
	}
	return n + "-minute steps"
}

// secondsWords is x seconds in ms under one second, else in seconds.
func secondsWords(x float64) string {
	if x < 1 {
		return fmt.Sprintf("%.0f ms", x*1000)
	}
	return fmt.Sprintf("%.1f s", x)
}

// backtestHint is what to do about a statistic no step priced, by the
// refusal's reason, on an aggregated fleet (no prefill variant) where aggregated.
func backtestHint(reason string, only, aggregated bool) string {
	every := "every step was"
	if !only {
		every = "the steps so refused were"
	}
	if aggregated && reason == string(whatif.NoLine) {
		// An aggregated pod with a large KV cache may never fill to the band:
		// a longer fit at the same load learns no more (issue #512).
		return "the aggregated pods learned no ITL line by KV fill: a line needs their fill to span the ITL band, " +
			"and each step's refusal names the highest fill they read"
	}
	switch reason {
	case string(whatif.Saturated):
		return every + " at or past the load the planner thinks the fleet can carry"
	case string(whatif.PastLowerBoundKnee):
		return every + " at or past a prefill knee priced from a lower bound of V_P, " +
			"and the true knee may be higher, so the planner gave no price there and does not call it overloaded"
	case string(whatif.NoPrefillCapacity):
		return "no prefill speed was learned to price a TTFT from: none under a queue, or, where the prefill pods never queued, " +
			"only the rate a pod computed, which prices the knee as a lower bound and no TTFT, or one read only at prompts other than the step's " +
			"(VPPromptNotCovered); fit on more (a larger split, or a longer recording)"
	case string(whatif.NoFixedLatency), string(whatif.PinnedDefaultB), string(whatif.NoLine):
		return "the fitted part held too few busy minutes to learn from; fit on more (a larger split, or a longer recording)"
	}
	return "see the refusal in the steps"
}

// verdictPart is one figure's sentence: its mean (else p90, else p50,
// primaryStats) within 20% in how many of the steps it priced, its lean,
// the steps it read more than 20% low, and the steps it gave no figure for
// and why; then the p90 beside it, over the steps whose measured p90 a
// bucket could place.
func verdictPart(name string, scores []BacktestScore, unit string) (line string, quoted *BacktestScore) {
	return verdictPartFor(name, scores, unit, false)
}

// verdictPartFor is verdictPart; aggregated says the fleet has no prefill
// variant, whose refusals a hint names in its own terms (backtestHint).
func verdictPartFor(name string, scores []BacktestScore, unit string, aggregated bool) (line string, quoted *BacktestScore) {
	quoted = primaryScore(scores)
	if quoted == nil && apartScore(scores) != nil {
		w := fmt.Sprintf("The planner's %s was not scored as a point in any step.%s", name, apartWords(name, scores, ""))
		if other := refusedSteps(scores); other > 0 {
			w += fmt.Sprintf(" %d more were refused (see the steps).", other)
		}
		return w, nil
	}
	if quoted == nil {
		var why []string
		hint := ""
		if i := slices.IndexFunc(scores, func(x BacktestScore) bool { return x.Statistic == statP90 }); i >= 0 {
			only := len(scores[i].Refused) == 1
			var hints []string
			for _, r := range slices.Sorted(maps.Keys(scores[i].Refused)) {
				n := scores[i].Refused[r]
				why = append(why, fmt.Sprintf("%s in %d", r, n))
				if hint == "" || r != string(whatif.Saturated) {
					hint = backtestHint(r, only, aggregated)
				}
				if r == string(whatif.PastLowerBoundKnee) || r == string(whatif.NoPrefillCapacity) {
					hints = append(hints, backtestHint(r, only, aggregated))
				}
			}
			if !only && len(hints) > 1 {
				// A lower-bound knee and no V_P beside it: each says why for its
				// own steps, not the last one for all.
				hint = strings.Join(hints, "; ")
			}
			if n := scores[i].Unscored[UnscoredNotComparable]; n > 0 && len(why) == 0 {
				return fmt.Sprintf("%s was not scored: the source measured no %s comparable with the prediction (see measured).", name, name), nil
			}
		}
		slices.Sort(why)
		if len(why) == 0 {
			return fmt.Sprintf("The planner's %s was not scored in any step.", name), nil
		}
		w := fmt.Sprintf("The planner gave no %s figure for any step: %s (refused: %s).", name, hint, strings.Join(why, ", "))
		if i := slices.IndexFunc(scores, func(x BacktestScore) bool { return x.Statistic == statP90 }); i >= 0 && scores[i].PastKnee != nil {
			b := scores[i].PastKnee
			w += fmt.Sprintf(" Where it called the fleet overloaded, the fleet measured p90 %s %s to %s.", name, secondsWords(b.Lo), secondsWords(b.Hi))
		}
		return w, nil
	}
	sc := quoted
	a := sc.All
	w := fmt.Sprintf("%s %s: within %.0f%% of measured in %d of the %d %s scored (a figure, and enough measured requests); %s",
		sc.Statistic, name, BacktestTolerance*100, int(math.Round(*a.Within*float64(a.Steps))), a.Steps, unit, leanWords(a))
	if a.Low > 0 {
		w += fmt.Sprintf("; it read %d more than 20%% low", a.Low)
	}
	w += "."
	if n := sc.Refused[string(whatif.Saturated)]; n > 0 {
		w += fmt.Sprintf(" For %d more it gave no figure, calling the fleet overloaded", n)
		switch b := sc.PastKnee; {
		case b == nil:
		case secondsWords(b.Lo) == secondsWords(b.Hi):
			w += fmt.Sprintf("; the fleet measured %s %s there", sc.Statistic, secondsWords(b.Lo))
		default:
			w += fmt.Sprintf("; the fleet measured %s %s to %s there", sc.Statistic, secondsWords(b.Lo), secondsWords(b.Hi))
		}
		w += "."
	}
	other := 0
	for r, n := range sc.Refused {
		if r != string(whatif.Saturated) {
			other += n
		}
	}
	if other > 0 {
		w += fmt.Sprintf(" %d more were refused for another reason (see the steps).", other)
	}
	if n := sc.Unscored[UnscoredFewRequests]; n > 0 {
		w += fmt.Sprintf(" %d had too few requests to measure a %s.", n, sc.Statistic)
	}
	if n := sc.Unscored[UnscoredAboveHistogram]; n > 0 {
		w += fmt.Sprintf(" %d measured above the histogram's top bucket, beyond what it can tell.", n)
	}
	if n := sc.Unscored[UnscoredUnsettled]; n > 0 {
		w += fmt.Sprintf(" %d ran so near the prefill knee that its queue does not settle inside the window (relaxation time over %.0f%% of it): "+
			"the window's TTFT is a noisy sample of the steady state priced.", n, 100*SettleShare)
	}
	if n := sc.Unscored[UnscoredBurst]; n > 0 {
		w += fmt.Sprintf(" %d held the start or the end of a load near a prefill knee priced from a lower bound of V_P: "+
			"its TTFT, which a queue building there may set, is not scored.", n)
	}
	w += apartWords(name, []BacktestScore{*sc}, " more")
	if sc.Statistic != statP90 {
		w += percentileWords(name, scores)
	}
	return w, quoted
}

// apartScore is the score apart (BacktestScore.VPLowerBound) of the first
// of primaryStats that has one, with its statistic; nil with none.
func apartScore(scores []BacktestScore) *BacktestScore {
	for _, stat := range primaryStats {
		if i := slices.IndexFunc(scores, func(x BacktestScore) bool { return x.Statistic == stat }); i >= 0 && scores[i].VPLowerBound != nil {
			return &scores[i]
		}
	}
	return nil
}

// refusedSteps is the refused steps of the first of primaryStats that has
// any, 0 with none.
func refusedSteps(scores []BacktestScore) int {
	for _, stat := range primaryStats {
		if i := slices.IndexFunc(scores, func(x BacktestScore) bool { return x.Statistic == stat }); i >= 0 {
			n := 0
			for _, c := range scores[i].Refused {
				n += c
			}
			if n > 0 {
				return n
			}
		}
	}
	return 0
}

// apartWords is what a verdict says of the steps scored apart
// (UnscoredVPLowerBound), "" with none; more is " more" beside steps
// already counted.
func apartWords(name string, scores []BacktestScore, more string) string {
	sc := apartScore(scores)
	if sc == nil {
		return ""
	}
	a := sc.VPLowerBound
	w := ""
	if n := a.Steps - a.Low; n > 0 {
		w = fmt.Sprintf(" In %d%s steps its %s %s was priced from a lower bound of V_P, which is neither a point figure nor a bound, "+
			"and read high or at most %.0f%% low: scored apart (vpLowerBound).", n, more, sc.Statistic, name, BacktestTolerance*100)
	}
	if a.Low > 0 {
		w += fmt.Sprintf(" %d of the steps scored read more than %.0f%% low from a lower bound of V_P: scored as misses.", a.Low, BacktestTolerance*100)
	}
	w += fmt.Sprintf(" Of all %d priced so, it read under the measured %s in %d, median ratio %.2f.", a.Steps, name, a.Under, *a.Bias)
	return w
}

// TPOTBucketHint is how to measure a TPOT percentile the histogram is too
// coarse to place: the verdict says it after the steps it left unscored,
// and the page shows the verdict as it is, so the CLI and the page say it
// in the same words.
const TPOTBucketHint = "To measure it, give the histogram finer edges: the router's metrics.requestTPOTBuckets in the " +
	"EndpointPickerConfig (an llm-d-router build after v1.0.0), or vLLM's --custom-histogram-buckets (v0.32.0 and later). " +
	"Edges about 15% apart place p90 and p95 within 20%."

// percentileWords is the p90 beside the figure a verdict quotes: within 20%
// in how many of the steps its measured p90 could be placed, and in how
// many a histogram bucket too wide to place it left it unscored, with
// TPOTBucketHint after TPOT's.
func percentileWords(name string, scores []BacktestScore) string {
	i := slices.IndexFunc(scores, func(x BacktestScore) bool { return x.Statistic == statP90 })
	if i < 0 {
		return ""
	}
	sc, w := scores[i], ""
	if a := sc.All; a.Steps > 0 {
		w = fmt.Sprintf(" p90 %s: within %.0f%% in %d of %d, median ratio %.2f", name, BacktestTolerance*100,
			int(math.Round(*a.Within*float64(a.Steps))), a.Steps, *a.Bias)
		if a.Low > 0 {
			w += fmt.Sprintf(", %d read more than 20%% low", a.Low)
		}
		w += "."
	}
	if n := sc.Unscored[UnscoredUnresolvable]; n > 0 {
		w += fmt.Sprintf(" p90 %s not scored in %d: the measured p90 fell in a histogram bucket wider than %.0f%% of its lower edge, "+
			"too wide to place it, with the prediction inside it or within %.0f%% of its edge", name, n, MeasuredBucketWidth*100, BacktestTolerance*100)
		if words := sc.SameBucketWords(); words != "" {
			w += "; " + words
			if rest := n - sc.SameBucket; rest > 0 {
				w += fmt.Sprintf(", and outside it, within %.0f%% of its edge, in %d", BacktestTolerance*100, rest)
			}
		}
		w += "."
		if name == "TPOT" {
			w += " " + TPOTBucketHint
		}
	}
	return w
}

// verdictOf is the answer, one sentence a figure and one on the region,
// and what was not judged in one more. It says only what the judged steps
// say: each share is over the steps the planner gave a figure for, with
// the steps it did not, and why, beside it.
func verdictOf(out *BacktestResponse, step time.Duration, aggregated bool) ([]string, string) {
	s := out.Summary
	span := out.Test.To.Sub(out.Test.From)
	if out.Test.Cycles > 1 {
		span += span / time.Duration(out.Test.Cycles-1)
	}
	unit := stepWords(step)
	lines := []string{fmt.Sprintf("Over the %s the planner did not see:", durationWords(span))}
	if s.Judged == 0 {
		lines = append(lines, "no step could be judged; see what was not judged.")
	} else {
		ttft, tq := verdictPartFor("TTFT", s.TTFT, unit, aggregated)
		tpot, pq := verdictPartFor("TPOT", s.TPOT, unit, aggregated)
		lines = append(lines, ttft, tpot)
		region, scored := 0, 0
		for _, sc := range []*BacktestScore{tq, pq} {
			if sc != nil {
				region, scored = region+sc.InRegion.Steps, scored+sc.All.Steps
			}
		}
		switch {
		case scored == 0:
		case region == 0:
			lines = append(lines, "None of these figures falls within the conditions our own accuracy benchmarks cover "+
				"(docs/guides/capacity-planner/accuracy.md): this check is the evidence for this fleet.")
		default:
			lines = append(lines, fmt.Sprintf("%d of these %d figures fall within the conditions our own accuracy benchmarks cover.", region, scored))
		}
	}
	e := s.Excluded
	var parts []string
	for _, x := range []struct {
		n    int
		what string
	}{{e.WarmUp, "warm-up"}, {e.Scaling, "replicas changing"}, {e.NoTraffic, "no traffic"}, {e.NoMeasurement, "no measured requests"},
		{e.NotPriced, "not priced"}, {e.OtherVariants, "other variants serving"}, {e.Burst, "a burst near the knee in its window"},
		{e.Load, "outside the test's load"}} {
		if x.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", x.n, x.what))
		}
	}
	excluded := fmt.Sprintf("Judged %d of %d %s", s.Judged, s.Steps, unit)
	if len(parts) > 0 {
		excluded += "; not judged: " + strings.Join(parts, ", ")
	}
	excluded += "."
	return lines, excluded
}

// backtestNotes is what the answer rests on, in words.
func backtestNotes(q *backtestQuestion) []string {
	notes := []string{
		"Fitted on the fit window alone. Each step of the test is priced from its own inputs (request rate, prompt and output lengths, " +
			"prompt-length spread, the replicas each role served), read over the same window its measurement covers, and never from its measured latency.",
		fmt.Sprintf("Each step is measured at its last cycle, over the rate window its histogram was read over (the router's, recorded "+
			"with it; the engines' one minute): a step shorter than the window is measured over the window, so neighbouring steps share "+
			"requests and are not independent. A step whose window reaches before the test, or over a change of "+
			"replicas, is not judged. A percentile is scored with %0.f requests or more at p50, %0.f at p90 and %0.f at p95 (50 for the mean); "+
			"measured percentiles are interpolated inside the histogram's buckets, and one whose bucket is wider than %.0f%% of its lower "+
			"edge is too wide to place: the bucket, not the traffic, would set it. A prediction more than 20%% beyond that bucket's "+
			"nearer edge is a certain miss, scored against that edge, which understates the miss, and never within; one nearer is "+
			"shown and not scored (unresolvable). The router's TPOT p90 and p95 are read from its TPOT histogram and, above their "+
			"first edge, the decode engines' together, each at its sampling noise: the bracket both allow is the bucket, and a reading "+
			"inside it (log survival linear in log TPOT) the measured figure. The verdict quotes the mean, the histogram's sum over its count.", minRequestsFor(0.5), minRequestsFor(0.9), minRequestsFor(0.95), MeasuredBucketWidth*100),
		fmt.Sprintf("Median ratio is the middle step's predicted over measured; typical and worst-10%% miss the median and p90 of the factor "+
			"between them; under the upper bound the share measured at or under the planner's own upper estimate, where it gives one; "+
			"within, the share inside %.0f%%. A measured figure above the histogram's top bucket, or a prediction more than 20%% "+
			"under the coarse bucket the measured one fell in, counts as read low.", BacktestTolerance*100),
		"In region: the figure falls within the conditions our own accuracy benchmarks cover (docs/guides/capacity-planner/accuracy.md).",
	}
	if q.config.Allowed {
		notes = append(notes, "CONFIGURATION DIFFERS, priced anyway: "+mismatchWords(q.config.Differences)+". The score includes that difference.")
	}
	return notes
}
