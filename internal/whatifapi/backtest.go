package whatifapi

import (
	"cmp"
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
	// UnscoredUnresolvable is a measured percentile that fell in a histogram
	// bucket too wide to place it (MeasuredBucketWidth) while the prediction
	// fell inside the same bucket: interpolated inside the bucket, the
	// measured figure reads the bucket's grid, not the traffic, and the
	// bucket cannot say which side of the prediction the truth is.
	UnscoredUnresolvable = "unresolvable"
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

// statP90 is the percentile a verdict reports beside the mean.
const statP90 = "p90"

// primaryStats is the order a verdict and the self-check take a figure's
// statistic in: the mean (the histogram's sum over its count, no bucket
// interpolated), else p90, else p50, the first scored in any step.
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
	Split    string         `json:"split,omitempty"`
	Config   BacktestConfig `json:"config"`
	Step     float64        `json:"step"`
	Settle   float64        `json:"settle"`
	Measured string         `json:"measured"`
	// MeasuredNote says what the measured figures are.
	MeasuredNote string `json:"measuredNote"`
	// Assumed is every input of the prediction the test did not give.
	Assumed []string        `json:"assumed"`
	TTFT    *Objective      `json:"ttft,omitempty"`
	Steps   []BacktestStep  `json:"steps"`
	Summary BacktestSummary `json:"summary"`
	Notes   []string        `json:"notes"`
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
	// Target is the TTFT target judged, predicted and measured: met,
	// missed or unknown.
	Target *BacktestJudged `json:"target,omitempty"`
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
	// OutsideBucket says which side.
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
	// PastKnee is the measured figure's range over the steps refused as
	// saturated, where the source measured one: what the fleet did where
	// the planner gave no figure.
	PastKnee *Band `json:"pastKnee,omitempty"`
	// Outside counts the scored steps outside the validated region, by the
	// envelope's reason, its words before the first figure.
	Outside map[string]int `json:"outside"`
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
	if !readJSON(w, r, &req) {
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
	test, ok := s.inputs.get(req.InputsID)
	if !ok {
		return nil, errUnknownInputs
	}
	fit := test
	var cutoff time.Time
	split := ""
	if req.TestInputsID != "" {
		if req.Split != "" || req.FitUntil != "" {
			return nil, InputErrorf("split and fitUntil split one source: give them without testInputsId")
		}
		if test, ok = s.inputs.get(req.TestInputsID); !ok {
			return nil, errUnknownInputs
		}
	} else {
		var cycle int
		var err error
		if cycle, cutoff, split, err = splitOf(test, req.Split, req.FitUntil); err != nil {
			return nil, err
		}
		in, err := refitRequest(test, cycle, cutoff)
		if err != nil {
			return nil, err
		}
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
		fit = &inputsEntry{id: doc.InputsID, doc: doc, pricer: res.Pricer, series: res.Series, latency: res.Latency,
			config: res.Config, bytes: res.Bytes}
		fit = s.inputs.add(doc.InputsID, fit)
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

// Backtest answers req against sources resolved outside a server: the CLI's
// backtest, on the same code path as POST /v1/backtest. For a split, fit is
// the source read at the split (cutoff) and wholeTest the whole source the
// test steps are read from; for two sources, cutoff is zero.
func Backtest(ctx context.Context, fit, wholeTest *Resolved, req BacktestRequest, cutoff time.Time, split, whatifSchema string) (*BacktestResponse, error) {
	entry := func(res *Resolved) *inputsEntry {
		doc := res.Inputs
		doc.InputsID = contentID("inputs", res.Content)
		return &inputsEntry{id: doc.InputsID, doc: doc, pricer: res.Pricer, series: res.Series, latency: res.Latency, config: res.Config}
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
	in := InputsRequest{Model: d.Model, DecodeVariant: d.Fleet.Decode.Variant}
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
	fit, test   *inputsEntry
	cutoff      time.Time
	split       string
	step        time.Duration
	settle      time.Duration
	measured    string
	target      *ReportTarget
	cv2         *float64
	promptCV    *float64
	config      BacktestConfig
	assumed     []string
	testIndices []int
	assume      Assume
	// targetAsked is the target as the answer names it.
	targetAsked *Objective
	// source is the latency scored against; measureWindow, when above 0,
	// the seconds each step's window spans, for a prediction made with no
	// latency to read it from; predictionHash the prediction a comparison
	// scores, which has no fit.
	source         string
	measureWindow  float64
	predictionHash string
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
	if fit.doc.Fleet.Prefill == nil || test.doc.Fleet.Prefill == nil {
		return nil, InputErrorf("the inputs have no prefill variant: a backtest prices a P/D fleet")
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
	q.config = compareConfig(withFlowControl(fit.config, fit.series, fitIdx), withFlowControl(test.config, test.series, testIdx))
	if !q.config.Match {
		if !req.AllowConfigMismatch {
			return nil, ConfigMismatchError{InputError: InputError{Err: fmt.Errorf("the test ran another configuration than the fit: %s. "+
				"A backtest across configurations scores the difference, not the planner; give allowConfigMismatch (--allow-config-mismatch) "+
				"to price it anyway, with the difference as a caveat", mismatchWords(q.config.Differences))}, Differences: q.config.Differences}
		}
		q.config.Allowed = true
	}
	for i, x := range test.series {
		if (q.cutoff.IsZero() || x.At.After(q.cutoff)) && (until.IsZero() || !x.At.After(until)) {
			q.testIndices = append(q.testIndices, i)
		}
	}
	if len(q.testIndices) == 0 {
		return nil, InputErrorf("no test cycle after the split")
	}
	slices.SortStableFunc(q.testIndices, func(a, b int) int { return test.series[a].At.Compare(test.series[b].At) })
	span := test.series[q.testIndices[len(q.testIndices)-1]].At.Sub(test.series[q.testIndices[0]].At)
	if int(span/q.step)+1 > maxBacktestSteps {
		return nil, InputErrorf("the test spans %s: at a step of %s that is more than %d steps; ask a longer step", span, q.step, maxBacktestSteps)
	}
	if cutoff.IsZero() {
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
	if whole := stepInputsOf(seriesAt(test.series, q.testIndices)); whole.promptCV != nil {
		q.promptCV = whole.promptCV
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
// the source does not say: the engines' histograms are over a minute.
const defaultMeasureWindow = 60.0

// measureWindowOf is the window, seconds, the latency at l was read over.
func measureWindowOf(l LatencySample, source string) float64 {
	if source == MeasuredRouter && l.Router != nil && l.Router.WindowSeconds > 0 {
		return l.Router.WindowSeconds
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
	p := &predictor{pricer: q.fit.pricer, fleets: map[predictKey]Fleet{}, cv2: q.cv2, promptCV: q.promptCV,
		headroom: q.fit.doc.Policy.Headroom, assume: q.assume}
	out := make([]BacktestStep, 0, len(steps))
	for _, st := range steps {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		out = append(out, q.stepOf(p, st, changes, first))
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
	for k, s := range predicted {
		if st := steps[k]; st.anchor >= 0 {
			scoreStep(&s, measuredOf(test.latency[st.anchor], source, st.windowSeconds, q.pd()), q.target)
		}
		out.Steps = append(out.Steps, s)
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
	out.VerdictLines, out.Excluded = verdictOf(out, q.step)
	out.Verdict = strings.Join(out.VerdictLines, " ")
	out.Notes = append(out.Notes, backtestNotes(q)...)
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

// stepOf predicts one step: its inputs, read over the cycles of its
// measurement window, why it is not judged, and each statistic predicted.
// It reads no latency: scoreStep sets the measurement beside it.
func (q *backtestQuestion) stepOf(p *predictor, st stepOf, changes []time.Time, first time.Time) BacktestStep {
	out := BacktestStep{Start: st.start, Cycles: len(st.idx), TTFT: []BacktestPoint{}, TPOT: []BacktestPoint{}}
	if st.anchor < 0 || len(st.window) == 0 {
		out.Excluded, out.Detail = ExcludedNoMeasurement, "no cycle in the step"
		return out
	}
	in := stepInputsOf(seriesAt(q.test.series, st.window))
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
		out.Excluded, out.Detail = ExcludedNoTraffic, "no requests arrived"
		return out
	case st.windowStart.Before(first.Add(q.settle)) || (q.settle == 0 && st.windowStart.Before(first)):
		out.Excluded, out.Detail = ExcludedWarmUp, fmt.Sprintf("its measurement window (%s) starts within %s of the test's first cycle",
			time.Duration(st.windowSeconds*float64(time.Second)), q.settle)
	case slices.ContainsFunc(changes, func(c time.Time) bool { return !c.After(end) && c.After(st.windowStart.Add(-q.settle)) }):
		out.Excluded, out.Detail = ExcludedScaling, fmt.Sprintf("the replicas changed in its measurement window or within %s before it", q.settle)
	case in.otherPods:
		out.Excluded, out.Detail = ExcludedOtherVariants, "variants other than the priced ones also served: the rate and the latency are the whole model's"
	case in.prefill == 0 || in.decode == 0:
		out.Excluded, out.Detail = ExcludedNotPriced, fmt.Sprintf("%d prefill and %d decode pods served: a P/D fleet is priced with both", in.prefill, in.decode)
	}
	pred, err := p.predict(in)
	if err != nil {
		if out.Excluded == "" {
			out.Excluded, out.Detail = ExcludedNotPriced, err.Error()
		}
		return out
	}
	if pred.knee != nil {
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
	return out
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
		out.TTFT[i] = scoredPoint(out.TTFT[i], m.ttft[i], m.requests, m.ttftTop, m.ttftWhy[i], m.ttftBucket[i])
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
	// promptCV is the step's own prompt spread, read from its prefill
	// pods' computed-token histograms; nil where they read none.
	promptCV *float64
	// otherPods says variants other than the priced ones served in it.
	otherPods bool
}

// stepInputsOf is the step's inputs: its mean rate, the median serving pods
// of each role, and its lengths weighted by rate.
func stepInputsOf(samples []SeriesSample) stepInputs {
	in := stepInputs{rate: rateOf(samples).Mean}
	a := actualOf(samples)
	in.prefill, in.decode = a.Prefill, a.Decode
	in.prompt, in.output, _, in.hasShape = bucketShape(samples)
	in.otherPods = slices.ContainsFunc(samples, func(x SeriesSample) bool { return x.OtherPods > 0 })
	pooled := map[float64]float64{}
	var computed, requests float64
	for _, x := range samples {
		if x.PromptBuckets == nil || !(x.PromptRequestRate > 0) {
			continue
		}
		computed += x.PromptComputedRate
		requests += x.PromptRequestRate
		for b, r := range x.PromptBuckets {
			pooled[b] += r
		}
	}
	if requests > 0 {
		if sp := whatif.ReadPromptSpread(pooled, computed/requests); !sp.Assumed {
			in.promptCV = ptr(math.Round(sp.CV/promptCVGrid) / (1 / promptCVGrid))
		}
	}
	return in
}

// promptCVGrid is the grid a step's prompt spread is priced on, so that
// steps of nearly one spread share a fleet.
const promptCVGrid = 0.05

// predictKey is one fleet a backtest prices.
type predictKey struct {
	np, nd         int
	prompt, output float64
	promptCV       float64
	q              float64
}

// predictor prices steps from the fit alone: the Pricer, and the inputs of
// each step. It never sees a latency.
type predictor struct {
	pricer   Pricer
	fleets   map[predictKey]Fleet
	cv2      *float64
	promptCV *float64
	// headroom is the fit's: it moves no TTFT or TPOT priced at a rate.
	headroom float64
	// assume is the request's typed terms.
	assume Assume
}

// prediction is one step's figures at each statistic of backtestStats.
type prediction struct {
	ttft, tpot           []Figure
	ttftBound, tpotBound []*float64
	// knee is the lower knee of the fleet that priced, decodeKnee decode's;
	// nil with none.
	knee, decodeKnee *float64
}

// predict prices one step at each statistic: the TTFT as a report's table
// prices it (TTFTPoint), bounded by the top of its span over V_P and f; the
// TPOT, bounded by the top of its band. Each carries the envelope's verdict.
func (p *predictor) predict(in stepInputs) (prediction, error) {
	var out prediction
	if !(in.rate > 0) || in.prefill == 0 || in.decode == 0 {
		return out, errors.New("no fleet to price")
	}
	for _, q := range backtestStats {
		k := predictKey{np: in.prefill, nd: in.decode, q: q, promptCV: -1}
		if in.hasShape {
			k.prompt, k.output = shapeKnob(in.prompt), shapeKnob(in.output)
		}
		cv := p.assume.PromptCV
		if cv == nil {
			switch {
			case in.promptCV != nil:
				cv = ptr(max(*in.promptCV, promptCVGrid))
			case p.promptCV != nil:
				cv = p.promptCV
			}
		}
		// The key is the spread priced: none (-1, the fit's) or its value.
		if cv != nil {
			k.promptCV = *cv
		}
		fl, ok := p.fleets[k]
		if !ok {
			kn := Knobs{Prefill: k.np, Decode: k.nd, PromptTokens: k.prompt, OutputTokens: k.output, TTFTPercentile: q, TPOTPercentile: q,
				ArrivalCV2: p.cv2, Headroom: p.headroom}
			kn.Assume = p.assume
			kn.Assume.PromptCV = cv
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
		}
		out.ttft, out.ttftBound = append(out.ttft, tf), append(out.ttftBound, tb)
		out.tpot, out.tpotBound = append(out.tpot, pf), append(out.tpotBound, pb)
	}
	return out, nil
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
		m.requests, m.ttftTop = th.total*windowSeconds, th.top()
	}
	if pok {
		m.tpotRequests, m.tpotTop = ph.total*windowSeconds, ph.top()
	}
	for i, q := range backtestStats {
		switch q {
		case 0:
			m.ttft[i], m.tpot[i] = ttftMean, tpotMean
		default:
			if tok {
				m.ttft[i], m.ttftBucket[i] = ptr(th.quantile(q)), th.coarseBucket(q)
			}
			if pok {
				m.tpot[i], m.tpotBucket[i] = ptr(ph.quantile(q)), ph.coarseBucket(q)
			}
		}
	}
	return m
}

// latencyHist is a cumulative latency histogram, repaired to be monotone,
// ending in +Inf.
type latencyHist struct {
	buckets []domain.LatencyBucket
	total   float64
}

// histOf is the histogram bs, repaired as Prometheus's histogram_quantile
// repairs one; ok false with no +Inf bucket or no request.
func histOf(in []domain.LatencyBucket) (latencyHist, bool) {
	bs := make([]domain.LatencyBucket, 0, len(in))
	for _, b := range in {
		if math.IsNaN(b.UpperBound) || !whatif.Finite(b.Rate) || b.Rate < 0 {
			continue
		}
		bs = append(bs, b)
	}
	slices.SortFunc(bs, func(x, y domain.LatencyBucket) int { return cmp.Compare(x.UpperBound, y.UpperBound) })
	if len(bs) == 0 || !math.IsInf(bs[len(bs)-1].UpperBound, 1) {
		return latencyHist{}, false
	}
	for i := 1; i < len(bs); i++ {
		bs[i].Rate = max(bs[i].Rate, bs[i-1].Rate)
	}
	total := bs[len(bs)-1].Rate
	if !(total > 0) {
		return latencyHist{}, false
	}
	return latencyHist{buckets: bs, total: total}, true
}

// top is the highest finite bound, 0 with none.
func (h latencyHist) top() float64 {
	if len(h.buckets) < 2 {
		return 0
	}
	return h.buckets[len(h.buckets)-2].UpperBound
}

// quantile is the q-quantile by linear interpolation inside its bucket, as
// histogram_quantile, but +Inf in the +Inf bucket: the top bound would read
// the tail low.
func (h latencyHist) quantile(q float64) float64 {
	rank := q * h.total
	for i, b := range h.buckets {
		if b.Rate < rank {
			continue
		}
		if math.IsInf(b.UpperBound, 1) {
			return math.Inf(1)
		}
		lower, below := 0.0, 0.0
		if i > 0 {
			lower, below = h.buckets[i-1].UpperBound, h.buckets[i-1].Rate
		}
		if b.Rate == below {
			return b.UpperBound
		}
		return lower + (b.UpperBound-lower)*(rank-below)/(b.Rate-below)
	}
	return math.Inf(1)
}

// coarseBucket is the finite bucket holding the q-quantile where it is too
// wide to place the quantile: U - L over MeasuredBucketWidth L, any bucket
// from 0 included; nil where it is narrow enough, or the quantile is above
// the top bucket (scored as a miss, not placed).
func (h latencyHist) coarseBucket(q float64) *Band {
	rank := q * h.total
	for i, b := range h.buckets {
		if b.Rate < rank {
			continue
		}
		if math.IsInf(b.UpperBound, 1) {
			return nil
		}
		lower := 0.0
		if i > 0 {
			lower = h.buckets[i-1].UpperBound
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
	case *meas > 0 && *pred.Value > 0:
		p.Ratio = ptr(*pred.Value / *meas)
	default:
		p.Unscored = UnscoredNotMeasured
	}
	return p
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
		j.Predicted = judgedMissed
		if *v <= target.Seconds {
			j.Predicted = judgedMet
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
	var all, in []BacktestPoint
	for _, st := range steps {
		ps := points(st)
		if st.Excluded != "" || i >= len(ps) {
			continue
		}
		p := ps[i]
		if p.Ratio == nil {
			if p.Unscored != "" {
				sc.Unscored[p.Unscored]++
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
	sc.All, sc.InRegion = statsOf(all), statsOf(in)
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
// refusal's reason.
func backtestHint(reason string) string {
	switch reason {
	case string(whatif.Saturated):
		return "every step was at or past the load the planner thinks the fleet can carry"
	case string(whatif.NoPrefillCapacity), string(whatif.NoFixedLatency), string(whatif.PinnedDefaultB), string(whatif.NoLine):
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
	quoted = primaryScore(scores)
	if quoted == nil {
		var why []string
		hint := ""
		if i := slices.IndexFunc(scores, func(x BacktestScore) bool { return x.Statistic == statP90 }); i >= 0 {
			for _, r := range slices.Sorted(maps.Keys(scores[i].Refused)) {
				n := scores[i].Refused[r]
				why = append(why, fmt.Sprintf("%s in %d", r, n))
				if hint == "" || r != string(whatif.Saturated) {
					hint = backtestHint(r)
				}
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
	if sc.Statistic != statP90 {
		w += percentileWords(name, scores)
	}
	return w, quoted
}

// percentileWords is the p90 beside the figure a verdict quotes: within 20%
// in how many of the steps its measured p90 could be placed, and in how
// many a histogram bucket too wide to place it left it unscored.
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
			"too wide to place it, with the prediction inside it.", name, n, MeasuredBucketWidth*100)
	}
	return w
}

// verdictOf is the answer, one sentence a figure and one on the region,
// and what was not judged in one more. It says only what the judged steps
// say: each share is over the steps the planner gave a figure for, with
// the steps it did not, and why, beside it.
func verdictOf(out *BacktestResponse, step time.Duration) ([]string, string) {
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
		ttft, tq := verdictPart("TTFT", s.TTFT, unit)
		tpot, pq := verdictPart("TPOT", s.TPOT, unit)
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
		{e.NotPriced, "not priced"}, {e.OtherVariants, "other variants serving"}} {
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
			"shown and not scored (unresolvable). The verdict quotes the mean, the histogram's sum over its count.", minRequestsFor(0.5), minRequestsFor(0.9), minRequestsFor(0.95), MeasuredBucketWidth*100),
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
