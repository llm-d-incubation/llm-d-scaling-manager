package whatifapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// Resolver resolves a source for one model: the replay of a recording, or of
// a Prometheus window captured and generated in memory, to one cycle. It is
// slow (seconds), and the server calls it once per POST /v1/inputs.
type Resolver interface {
	Resolve(ctx context.Context, req InputsRequest) (*Resolved, error)
}

// SourceLister is implemented by a Resolver that can list the sources it
// reads; the server answers GET /v1/sources from it, and with empty lists
// when the Resolver does not implement it. The server sets SchemaVersion and
// replaces nil lists with empty ones. An error is answered as writeErr
// classes it, so it must name no server path.
type SourceLister interface {
	Sources(ctx context.Context) (SourcesResponse, error)
}

// Resolved is a source resolved: the inputs as the API reports them, less
// their id, and the Pricer that prices fleets from them.
type Resolved struct {
	// Inputs is the response body, but for SchemaVersion and InputsID, which
	// the server sets.
	Inputs InputsResponse
	// Content names the inputs by content (the recording's bytes, the cycle,
	// the model and the variants): the server hashes it into the inputsId.
	Content string
	Pricer  Pricer
	// Series is every cycle of the source, oldest first, for a day plan
	// (POST /v1/dayplan); nil where the source carries none.
	Series []SeriesSample
	// Latency is each cycle's measured latency, Latency[i] the cycle of
	// Series[i], for a backtest (POST /v1/backtest); nil where the source
	// carries none. Nothing that prices a fleet reads it.
	Latency []LatencySample
	// Config is the configuration the source says the fleet ran, which a
	// backtest compares between its fit and its test.
	Config RunConfig
	// Folds is the source read at earlier cycles, for the self-check
	// (SelfCheckOf); nil where none was asked. PlannerVersion is what the
	// self-check stamps an issue with.
	Folds          []FoldFit
	PlannerVersion string
	// Mixture is each cycle as the experimental rank-mixture TTFT model
	// reads it, Mixture[i] the cycle of Series[i]
	// (learnreplay.MixtureSeriesFrom); nil unless that model was asked for
	// (BacktestRequest.TTFTModel).
	Mixture []whatif.MixtureCycle
	// Decode is each cycle as the experimental TPOT distribution reads it,
	// Decode[i] the cycle of Series[i] (learnreplay.DecodeSeriesFrom); nil
	// unless that model was asked for (BacktestRequest.TPOTModel).
	Decode []whatif.DecodeCycle
	// TPOTModel is the experimental TPOT distribution Decode was read for
	// (whatif.TPOTRenewalLognormal or whatif.TPOTPerLengthMixture; "" the
	// former), what a day plan and at-replicas fit on it.
	TPOTModel string
	// Engine is the engines' flags those models read, as the source gives
	// them; zero where neither model was asked for. A backtest request that
	// does not state them takes the fit's.
	Engine EngineSlots
	// TTFTModel is how a caller outside a server fits the TTFT model on
	// Mixture (DayPlan): the zero value is v2 at its defaults.
	TTFTModel TTFTModelOptions
	// TPOTMinFitSteps is the fewest fit steps the TPOT distribution fits on
	// for that caller (DayPlan), 0 MinTPOTDistFitSteps.
	TPOTMinFitSteps int
	// Bytes is an estimate of the memory the entry keeps alive (the Pricer,
	// the Series and the Inputs), its weight in the inputs cache
	// (Config.MaxInputsBytes); 0 weighs nothing.
	Bytes int
}

// EngineSlots is what the experimental backtest models read of the engines'
// flags: the prefill and decode engines' max-num-seqs (PrefillSlots as the
// prompts a prefill rank runs at once: 1 without chunked prefill) and the
// decode engine's num_speculative_tokens; 0 not read (or no speculative
// decoding).
type EngineSlots struct {
	PrefillSlots, DecodeSlots, SpecTokens int
}

// Pricer builds the fleet a scenario prices. It must be safe to call from
// several goroutines.
type Pricer interface {
	// Fleet is the fleet at k. An error is a knob the CLI refuses as a usage
	// error (InputError); a refusal of the data is a figure's, not an error.
	Fleet(k Knobs) (Fleet, error)
}

// Knobs is what changes the fleet a scenario prices: the replica counts, the
// traffic's lengths, the statistics asked, and every assumed term. The rate
// and the targets are not knobs: one fleet prices every rate and target.
type Knobs struct {
	Prefill, Decode int
	// PromptTokens and OutputTokens are stated mean lengths, priced in place
	// of the observed ones (the CLI's --assume-input-tokens and
	// --assume-output-tokens, flagged AssumedShape); 0 is the observed one.
	PromptTokens, OutputTokens float64
	// PromptTokensRead says PromptTokens is a whole prompt read from the
	// source's own cycles (a day plan's bucket, a backtest's step), not
	// stated: its hit is then the source's too, and on a P/D fleet the
	// prefill pods' computed-token hit nets it, as it nets a PrefixHit read
	// from the load's cycles (PrefixHitRead). The decode rows' prefix-cache
	// hit is not a hit of the prefill pods: netted by it, the prompt their
	// caches served was priced as computed.
	PromptTokensRead bool
	// PrefixHit is a stated prefix-cache hit rate in [0, 1), nil the
	// observed one (flagged AssumedShape).
	PrefixHit *float64
	// PrefixHitRead says PrefixHit was read from the source (the load's
	// cycles, ReportShapeAsk.Load), not stated: on a P/D fleet the prefill
	// pods' computed-token hit replaces it, with slo.SwitchTTFTComputedHit on
	// or off, and a stated one governs.
	PrefixHitRead bool
	// PrefillComputedTokens is the prompt the prefill pods computed a
	// request, read over the same cycles as PromptTokens (a backtest's
	// step), off the prefill pods' rows where PromptTokens is read off the
	// decode rows; 0 is none. It moves a figure only where the replay turned
	// slo.SwitchTTFTPrefixAttention on: the prefill work is then this
	// prompt, and PromptTokens less it is the cached context its attention
	// spans (whatif.Prefill.CachedContextRead). PrefixHit is not used for
	// it: that hit nets the decode variant's prompt too, with the switch off
	// as on.
	PrefillComputedTokens float64
	// CachedTokenCost is phi, the cost of a cached prompt token, priced in
	// place of the one the pricer fitted (CachedTokenCost); nil is the
	// pricer's own. It moves a figure only where the replay turned
	// slo.SwitchTTFTCachedTokenCost on and the prompt has a cached share.
	CachedTokenCost *float64
	// TTFTPercentile and TPOTPercentile are q in (0, 1); 0 is the mean.
	TTFTPercentile, TPOTPercentile float64
	Assume                         Assume
	// PromptMaxRatio is the longest prompt over the mean that the histogram
	// Assume.PromptCV was read from bounds (whatif.PromptSpread.MaxRatio),
	// priced beside it: a backtest states each step's own spread, and the
	// bound is the same histogram's. 0 is none; it is ignored without
	// Assume.PromptCV, and where the replay did not turn on
	// slo.SwitchTTFTPromptSupport.
	PromptMaxRatio float64
	// OutputDispersion is the spread of the output lengths, D =
	// E[O^2]/E[O]^2 (shape.Dispersion), that a backtest step's decode rows
	// read, priced in place of the fit's where the replay turned
	// saturation.SwitchBacktestStepOutputDispersion on (the planner's
	// default); 0 is the fit's. No flag sets it.
	OutputDispersion float64
	ArrivalCV2       *float64
	// Headroom is the operating point's fraction of the lower knee.
	Headroom float64
}

// Fleet prices one fleet. Each method is one of the CLI's own figures, with
// its flags, band and span as the CLI prints them.
type Fleet interface {
	// PrefillKnee is the prefill knee, with its span over V_P and f.
	PrefillKnee() whatif.Result
	// DecodeKnee is decode's knee, banded.
	DecodeKnee() whatif.Result
	// Onsets is the flow-control onsets, nil with flow control off.
	Onsets() whatif.Result
	// TTFTAt is the TTFT at rate, with its span: a curve's point.
	TTFTAt(rate float64) whatif.Result
	// TTFTPoint is the TTFT at rate as replicas-to-slo --rate prints it, and
	// what the CLI's assumed row adds of it (an assumed decode term that
	// decides it).
	TTFTPoint(rate float64) (whatif.Result, []string)
	// TPOTAt is decode's mean TPOT at rate, and its percentile (nil with the
	// mean asked), each banded, as replicas-to-slo --rate prints them.
	TPOTAt(rate float64) (mean, percentile whatif.Result)
	// Limits is slo-to-rate's rates at the targets: decode's at the TPOT
	// target, the prefill role's at the TTFT target, and the fleet's at
	// headroom and at headroom 1, each spanned. With no TPOT target, decode's
	// is nil, unless DecodeKnee is refused: then nothing bounds decode, and
	// decode's limit and the fleet's are that refusal.
	Limits(ttftSeconds, tpotSeconds, headroom float64) LimitSet
	// PlainLimits is Limits' decode, prefill and fleet rates at headroom with
	// no span over V_P and f, a fifth of the cost: the marginal steps' and a
	// bucket line's. A refused decode knee with no TPOT target is decode's
	// limit and the fleet's, as in Limits.
	PlainLimits(ttftSeconds, tpotSeconds, headroom float64) (decode, prefill, fleet whatif.Result)
	// TPOTRatio is R_q as resolved for the percentile asked: its value and
	// source, or its refusal; the zero Ratio with the mean asked.
	TPOTRatio() Ratio
	// Assumed is every assumed input of the fleet as read, in words, as the
	// CLI's assumed row names them.
	Assumed() []string
	// Class is the class the envelope judges the fleet's figures by
	// (whatif.FigureClass): its pods, accelerator, shape and flow control.
	Class() whatif.FigureClass
}

// TPOTRatioFleet is a Fleet whose TPOT percentile is its mean TPOT times a
// ratio R_q, and which can price it at a ratio an experimental model gives
// in place of its own (tpot_model_main.go): its percentile curve, its point
// and decode's limit at a percentile target alike.
type TPOTRatioFleet interface {
	Fleet
	// WithTPOTRatio is the fleet with its TPOT percentile priced at r.
	WithTPOTRatio(r ModelRatio) Fleet
}

// ModelRatio is R_q as an experimental model gives it: Value, the Flag on
// every figure priced with it, the Source the plan's ratio names, and the
// Assumed line the fleet adds.
type ModelRatio struct {
	Value float64
	Flag  whatif.Flag
	// More is the flags the ratio's figures carry beside Flag.
	More    []whatif.Flag
	Source  string
	Assumed string
}

// LimitSet is slo-to-rate's rates at one set of targets. Notes is what the
// CLI's assumed row adds of them.
type LimitSet struct {
	Decode, Prefill, Fleet, WithoutHeadroom whatif.Result
	Notes                                   []string
}

// InputError is a request the caller asked wrongly: the server answers it
// with 400 and its message.
type InputError struct{ Err error }

// Error is what the caller asked wrongly.
func (e InputError) Error() string {
	if e.Err == nil {
		return "invalid request"
	}
	return e.Err.Error()
}

// Unwrap is the error, for errors.Is and errors.As.
func (e InputError) Unwrap() error { return e.Err }

// InputErrorf is an InputError of a formatted message.
func InputErrorf(format string, args ...any) error {
	return InputError{Err: fmt.Errorf(format, args...)}
}

// IsInputError reports whether err is an InputError.
func IsInputError(err error) bool {
	var e InputError
	return errors.As(err, &e)
}
