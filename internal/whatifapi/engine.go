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
	// TTFTPercentile and TPOTPercentile are q in (0, 1); 0 is the mean.
	TTFTPercentile, TPOTPercentile float64
	Assume                         Assume
	ArrivalCV2                     *float64
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
	// target (nil with no TPOT target), the prefill role's at the TTFT target,
	// and the fleet's at headroom and at headroom 1, each spanned.
	Limits(ttftSeconds, tpotSeconds, headroom float64) LimitSet
	// PlainLimits is Limits' decode, prefill and fleet rates at headroom with
	// no span over V_P and f, a fifth of the cost: the marginal steps' and a
	// bucket line's.
	PlainLimits(ttftSeconds, tpotSeconds, headroom float64) (decode, prefill, fleet whatif.Result)
	// TPOTRatio is R_q as resolved for the percentile asked: its value and
	// source, or its refusal; the zero Ratio with the mean asked.
	TPOTRatio() Ratio
	// Assumed is every assumed input of the fleet as read, in words, as the
	// CLI's assumed row names them.
	Assumed() []string
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
