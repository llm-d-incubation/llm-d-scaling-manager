package whatif

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// The calibration envelope: where the planner's figures were measured
// against a client, and how far off they read there
// (docs/guides/capacity-planner/accuracy.md). Each row is one kind of
// figure on one shape class, from one measured run: the region the run
// covered and the signed error it read over it. A figure inside a row's
// region carries that row's error; any other figure reads OUTSIDE VALIDATED
// REGION, with what put it outside. The region holds every input that shapes
// the figure, the traffic's prompt spread and arrivals first: a figure
// priced on an input the run did not price at is outside, whatever else
// matches. The rows are data (envelope.json), so that a new run adds rows
// without code.

// FigureKind is a kind of figure the planner prints.
type FigureKind string

// The kinds of figure. A kind no row names was never measured, and every
// figure of it is outside.
const (
	// FigureTTFT is the fleet's end-to-end TTFT at a rate, at a statistic.
	// Its knee share is the rate over the fleet's lower knee.
	FigureTTFT FigureKind = "ttft"
	// FigureMeanTPOT is decode's mean TPOT at a rate. Its knee share is the
	// rate over decode's knee.
	FigureMeanTPOT FigureKind = "mean-tpot"
	// FigurePercentileTPOT is decode's TPOT at a per-request percentile.
	FigurePercentileTPOT FigureKind = "percentile-tpot"
	// FigurePrefillKnee is the prefill knee: the rate the prefill pods
	// saturate at.
	FigurePrefillKnee FigureKind = "prefill-knee"
	// FigureDecodeKnee is decode's knee.
	FigureDecodeKnee FigureKind = "decode-knee"
	// FigureFlowControlOnset is the prefill stage's flow-control onset: the
	// rate past which the router holds requests.
	FigureFlowControlOnset FigureKind = "flow-control-onset"
	// FigureRateAtTTFT is the rate at a TTFT target. Its knee share is the
	// rate over the fleet's lower knee.
	FigureRateAtTTFT FigureKind = "rate-at-ttft"
	// FigureRateAtTPOT is decode's rate at a TPOT target.
	FigureRateAtTPOT FigureKind = "rate-at-tpot"
	// FigureOperatingCap is a rate the operating-point cap set: headroom
	// times the lower knee.
	FigureOperatingCap FigureKind = "operating-point-cap"
	// FigurePrefillLatency is the prefill pods' own latency, without the
	// decode side (TTFTParts.OwnPrefill and EngineWait): a client measures
	// TTFT end to end only.
	FigurePrefillLatency FigureKind = "prefill-latency"
)

// The topologies a class names.
const (
	// TopologyPD is a fleet of prefill and decode pods.
	TopologyPD = "pd"
	// TopologyAggregated is one pod doing both.
	TopologyAggregated = "aggregated"
)

// The flow-control states a class names; "" is not known (no queue series).
const (
	FlowControlNameOff = "off"
	FlowControlNameOn  = "on"
)

// FigureClass is what a figure was priced at, as the envelope's regions are
// drawn: the fleet, its accelerator, its traffic and how each was known. A
// zero count or length is unknown, and an unknown value is outside any
// region that bounds it.
type FigureClass struct {
	Topology        string
	Accelerator     string
	PrefillReplicas int
	DecodeReplicas  int
	PromptTokens    float64
	OutputTokens    float64
	// PromptCV is the prompt's spread priced; PromptCVUnknown says nothing
	// measured or stated it, so it is the planner's default (AssumedPromptCV).
	PromptCV        float64
	PromptCVUnknown bool
	// ArrivalCV2 is the arrivals' Ca^2; 0 reads as Poisson (1).
	// ArrivalsUnknown says nothing measured or stated them: Poisson assumed.
	ArrivalCV2      float64
	ArrivalsUnknown bool
	FlowControl     string
	// Model is the model served, and TensorParallel and
	// PrefillTensorParallel decode's and prefill's tensor parallelism.
	Model                 string
	TensorParallel        int
	PrefillTensorParallel int
	// PrefixHitRate is the prompts' prefix-cache hit rate priced.
	PrefixHitRate float64
	// Stated are the pricing inputs typed in that the run did not price at
	// (a flag such as --batch-cap): each puts the figure outside.
	Stated []string
}

// FleetClass is the class of a P/D fleet's figures, less the accelerator,
// which the fleet does not carry, and less what the caller knows of how the
// spread and the arrivals were read: PromptCVUnknown is the fleet's
// PromptCVAssumed, which a stated spread also sets.
func FleetClass(fl TTFTFleet) FigureClass {
	c := FigureClass{Topology: TopologyPD, PrefillReplicas: fl.Prefill.Replicas, PromptTokens: fl.Prefill.PromptTokens,
		PromptCV: fl.Prefill.PromptCV, PromptCVUnknown: fl.Prefill.PromptCVAssumed, ArrivalCV2: fl.Prefill.ArrivalCV2}
	switch fl.FlowControl.State {
	case FlowControlOff:
		c.FlowControl = FlowControlNameOff
	case FlowControlOn:
		c.FlowControl = FlowControlNameOn
	}
	if fl.Decode != nil {
		c.DecodeReplicas, c.OutputTokens = fl.Decode.Replicas, fl.Decode.OutputTokens
	}
	return c
}

// Figure is one figure to judge.
type Figure struct {
	Kind FigureKind
	// Percentile is the statistic, q in (0, 1); 0 is the mean.
	Percentile float64
	Class      FigureClass
	// KneeShare is the figure's rate over its knee (FigureKind says which);
	// 0 is unknown, and outside any row that bounds it.
	KneeShare float64
	// Flags are the figure's own: some put it outside every region
	// (Envelope.OutsideFlags).
	Flags []Flag
}

// RateFigure is a rate at a target as the envelope judges it, on the CLI and
// the API alike: a rate-at-ttft where a TTFT bound set it, a rate-at-tpot
// where only decode's did, and the operating-point cap where only the cap
// did. boundBy is the rate's bounds as written (Basis.BoundBy); its knee
// share is value over the lower knee.
func RateFigure(value float64, boundBy []string, q float64, class FigureClass, lowerKnee float64, flags []Flag) Figure {
	ttftSide := []Bound{BoundTTFT, BoundDecodeAdmission, BoundPrefillOnset, BoundDecodeOnset, BoundDecodeGateCharge, BoundPrefillKnee}
	kind := FigureOperatingCap
	for _, b := range boundBy {
		switch {
		case strings.Contains(b, string(BoundTTFT)) || slices.Contains(ttftSide, Bound(b)):
			kind = FigureRateAtTTFT
		case kind != FigureRateAtTTFT && (b == string(BoundTPOT) || b == "decode "+string(BoundTPOT)):
			kind = FigureRateAtTPOT
		}
	}
	f := Figure{Kind: kind, Class: class, Flags: flags}
	if kind == FigureRateAtTTFT {
		f.Percentile = q
	}
	if value > 0 && lowerKnee > 0 {
		f.KneeShare = value / lowerKnee
	}
	return f
}

// Span is a closed range, Lo to Hi.
type Span struct {
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
}

// spanSlack is how far past a stored bound a value still holds: the bounds
// are stored rounded, and a figure equal to one as printed is inside.
const spanSlack = 1e-9

// holds reports whether the span holds v, ends included.
func (s Span) holds(v float64) bool {
	return v >= s.Lo-spanSlack*math.Max(1, math.Abs(s.Lo)) && v <= s.Hi+spanSlack*math.Max(1, math.Abs(s.Hi))
}

// shareSlack is the knee share's: stored to 3 decimals.
const shareSlack = 5e-4

// Region is where a row was measured. An empty list or a nil span bounds
// nothing.
type Region struct {
	Topology        string    `json:"topology"`
	Accelerators    []string  `json:"accelerators,omitempty"`
	PrefillReplicas []int     `json:"prefillReplicas,omitempty"`
	DecodeReplicas  []int     `json:"decodeReplicas,omitempty"`
	PromptTokens    *Span     `json:"promptTokens,omitempty"`
	PromptCV        *Span     `json:"promptCV,omitempty"`
	OutputTokens    *Span     `json:"outputTokens,omitempty"`
	ArrivalCV2      *Span     `json:"arrivalCV2,omitempty"`
	FlowControl     []string  `json:"flowControl,omitempty"`
	Percentiles     []float64 `json:"percentiles,omitempty"`
	// Models are the model's names, TensorParallel and PrefillTensorParallel
	// the decode and prefill tensor parallelism, and PrefixHitRate the
	// prefix-cache hit rate, as the run priced them.
	Models                []string `json:"models,omitempty"`
	TensorParallel        []int    `json:"tensorParallel,omitempty"`
	PrefillTensorParallel []int    `json:"prefillTensorParallel,omitempty"`
	PrefixHitRate         *Span    `json:"prefixHitRate,omitempty"`
	// MinKneeShare and MaxKneeShare are the lowest and highest knee share
	// measured, ends included; 0 bounds nothing.
	MinKneeShare float64 `json:"minKneeShare,omitempty"`
	MaxKneeShare float64 `json:"maxKneeShare,omitempty"`
}

// The kinds of range a row's error is.
const (
	// SpreadPercentiles is the 10th to the 90th percentile of point
	// readings at MinPercentileLevels independent levels or more, with
	// ErrorMin and ErrorMax beside it.
	SpreadPercentiles = "p10-p90"
	// SpreadMinMax is the lowest to the highest point reading, at fewer than
	// MinPercentileLevels independent levels.
	SpreadMinMax = "min-max"
	// SpreadBracket is the ends of brackets: the measured figure lay between
	// two ladder steps, so each reading is a range, not a point.
	SpreadBracket = "bracket"
	// SpreadOneSided is a bound: the run only showed the measured figure was
	// past a step, so the error is known on one side.
	SpreadOneSided = "one-sided"
)

// EnvelopeRow is one kind of figure on one shape class, from one run.
type EnvelopeRow struct {
	Figure FigureKind `json:"figure"`
	// Shape names the shape class in words; Source the run.
	Shape  string `json:"shape"`
	Source string `json:"source"`
	Region Region `json:"region"`
	// ErrorLo and ErrorHi are the signed error, the model's figure over the
	// measured one less 1, as Spread says. Nil is unbounded on that side.
	ErrorLo *float64 `json:"errorLo,omitempty"`
	ErrorHi *float64 `json:"errorHi,omitempty"`
	// ErrorMin and ErrorMax are the lowest and the highest point reading,
	// beside a p10-p90.
	ErrorMin *float64 `json:"errorMin,omitempty"`
	ErrorMax *float64 `json:"errorMax,omitempty"`
	// Spread is the kind of range ErrorLo and ErrorHi are.
	Spread string `json:"spread"`
	// Readings is how many readings the error rests on, and Levels how many
	// load levels they were taken at (0 where each reading is its own).
	Readings int `json:"readings"`
	Levels   int `json:"levels,omitempty"`
	// Holdout says the run was never used to tune the planner: its rows are
	// out-of-sample. False is in-sample: the run tuned the planner's
	// constants or is one of its goldens, so its error is how well the
	// planner fits the run it was fitted on, a floor on the error elsewhere.
	Holdout bool `json:"holdout,omitempty"`
}

// MinPercentileLevels is the fewest independent readings a p10-p90 is read
// over: below it, the 10th and 90th percentiles are the 2nd and the
// next-to-last readings, and the range is a min-max (#308).
const MinPercentileLevels = 10

// Independent is how many independent readings the row's error rests on:
// its load levels where readings repeat a level (a level read twice, once
// per flow-control phase, is one reading of the planner, priced the same),
// else its readings.
func (r *EnvelopeRow) Independent() int {
	if r.Levels > 0 && r.Levels < r.Readings {
		return r.Levels
	}
	return r.Readings
}

// CountText is the row's count in words: "n=6 load levels, 12 readings"
// where readings repeat a level, else "n=12".
func (r *EnvelopeRow) CountText() string {
	if r.Independent() != r.Readings {
		return fmt.Sprintf("n=%d load levels, %d readings", r.Independent(), r.Readings)
	}
	return fmt.Sprintf("n=%d", r.Readings)
}

// Envelope is the planner's measured accuracy.
type Envelope struct {
	Rows []EnvelopeRow `json:"rows"`
	// AllowedFlags are, by source, the flags a figure a row of that source
	// validates may carry: those the run's own figures carried. Any other
	// flag puts a figure outside, so a new flag is outside until a run
	// prices it.
	AllowedFlags map[string][]Flag `json:"allowedFlags"`
	// OutsideFlags say, for some flags outside AllowedFlags, why in words.
	OutsideFlags map[Flag]string `json:"outsideFlags"`
	// Never says, by kind, why a kind no row names has none.
	Never map[FigureKind]string `json:"never"`
}

//go:embed envelope.json
var envelopeJSON []byte

var defaultEnvelope = sync.OnceValues(func() (Envelope, error) { return ParseEnvelope(envelopeJSON) })

// DefaultEnvelope is the envelope the repository's measured runs draw
// (envelope.json). It panics on a file that does not parse, which a test
// holds.
func DefaultEnvelope() Envelope {
	e, err := defaultEnvelope()
	if err != nil {
		panic(err)
	}
	return e
}

// ParseEnvelope reads an envelope, refusing a row that judges nothing or
// says its error wrongly.
func ParseEnvelope(raw []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return Envelope{}, fmt.Errorf("envelope: %w", err)
	}
	for i, r := range e.Rows {
		switch {
		case r.Figure == "" || r.Source == "" || r.Shape == "":
			return Envelope{}, fmt.Errorf("envelope: row %d: figure, shape and source are required", i)
		case r.Region.Topology == "":
			return Envelope{}, fmt.Errorf("envelope: row %d: no topology", i)
		case r.Readings < 1:
			return Envelope{}, fmt.Errorf("envelope: row %d: no readings", i)
		case r.ErrorLo == nil && r.ErrorHi == nil:
			return Envelope{}, fmt.Errorf("envelope: row %d: no error on either side", i)
		case !slices.Contains([]string{SpreadPercentiles, SpreadMinMax, SpreadBracket, SpreadOneSided}, r.Spread):
			return Envelope{}, fmt.Errorf("envelope: row %d: spread %q", i, r.Spread)
		case r.Spread == SpreadPercentiles && r.Independent() < MinPercentileLevels:
			return Envelope{}, fmt.Errorf("envelope: row %d: a p10-p90 over %d independent readings: under %d is a min-max",
				i, r.Independent(), MinPercentileLevels)
		}
	}
	return e, nil
}

// Validation is what the envelope says of one figure: the row whose region
// holds it, or why none does.
type Validation struct {
	Row *EnvelopeRow
	// Outside is why no row holds the figure; "" when one does.
	Outside string
}

// Validated reports whether a row holds the figure.
func (v Validation) Validated() bool { return v.Row != nil }

// String is the figure's label: "validated in-sample: error -5..+18%
// (min-max, n=6 load levels, 12 readings; q32 staircase)" or "OUTSIDE
// VALIDATED REGION (why)". The count, independent first, the kind of range
// and whether the run is a holdout are always printed.
func (v Validation) String() string {
	r := v.Row
	if r == nil {
		return "OUTSIDE VALIDATED REGION (" + v.Outside + ")"
	}
	n := r.CountText()
	head := "validated in-sample"
	if r.Holdout {
		head = "validated on a holdout"
	}
	switch r.Spread {
	case SpreadPercentiles:
		return fmt.Sprintf("%s: error %s (p10-p90, min-max %s, %s; %s)", head, ErrorText(r.ErrorLo, r.ErrorHi),
			ErrorText(r.ErrorMin, r.ErrorMax), n, r.Source)
	case SpreadMinMax:
		return fmt.Sprintf("%s: error %s (min-max, %s; %s)", head, ErrorText(r.ErrorLo, r.ErrorHi), n, r.Source)
	case SpreadBracket:
		what := "between ladder steps"
		if r.Readings == 1 {
			what = "single reading, between two ladder steps"
		}
		return fmt.Sprintf("%s: error %s (%s, %s; %s)", head, ErrorText(r.ErrorLo, r.ErrorHi), n, what, r.Source)
	}
	return fmt.Sprintf("%s: one-sided, error %s (%s, not crossed within the steps run; %s)", head, ErrorText(r.ErrorLo, r.ErrorHi), n, r.Source)
}

// ErrorText prints a signed error range in percent: "-2..+16%", or one side
// of it, "at most -6%", where the other is unbounded.
func ErrorText(lo, hi *float64) string {
	pct := func(x float64) string {
		r := math.Round(100 * x)
		if r == 0 {
			r = 0 // no "-0"
		}
		return fmt.Sprintf("%+.0f", r)
	}
	switch {
	case lo != nil && hi != nil:
		return pct(*lo) + ".." + pct(*hi) + "%"
	case hi != nil:
		return "at most " + pct(*hi) + "%"
	case lo != nil:
		return "at least " + pct(*lo) + "%"
	}
	return "unbounded"
}

// Judge says whether a row holds f, and why not when none does: the first
// miss of the row of its kind it misses least.
func (e Envelope) Judge(f Figure) Validation {
	if f.Class.Topology == TopologyAggregated {
		return Validation{Outside: "aggregated variants: never measured"}
	}
	// A statistic no row of the kind measured is named against all of them.
	var stats []float64
	for _, r := range e.Rows {
		if r.Figure == f.Kind {
			for _, q := range r.Region.Percentiles {
				if !slices.Contains(stats, q) {
					stats = append(stats, q)
				}
			}
		}
	}
	if len(stats) > 0 && !slices.Contains(stats, f.Percentile) {
		slices.Sort(stats)
		return Validation{Outside: fmt.Sprintf("%s, measured %s", statisticText(f.Percentile), statisticsText(stats))}
	}
	best, bestMisses := "", math.MaxInt
	for i := range e.Rows {
		r := &e.Rows[i]
		if r.Figure != f.Kind {
			continue
		}
		misses := r.Region.misses(f, e, r.Source)
		if len(misses) == 0 {
			return Validation{Row: r}
		}
		if len(misses) < bestMisses {
			best, bestMisses = misses[0], len(misses)
		}
	}
	if best == "" {
		if why, ok := e.Never[f.Kind]; ok {
			return Validation{Outside: why}
		}
		return Validation{Outside: string(f.Kind) + ": never measured"}
	}
	return Validation{Outside: best}
}

// misses is what of f the region does not hold, in words: the inputs that
// shape a figure first (the prompt's spread, the arrivals, an assumed
// term), then the fleet, the shape and the load.
func (g Region) misses(f Figure, e Envelope, source string) []string {
	var out []string
	c := f.Class
	if c.Topology != g.Topology {
		out = append(out, fmt.Sprintf("topology %s, measured %s", orUnknown(c.Topology), g.Topology))
	}
	if len(g.Percentiles) > 0 && !slices.Contains(g.Percentiles, f.Percentile) {
		out = append(out, fmt.Sprintf("%s, measured %s", statisticText(f.Percentile), statisticsText(g.Percentiles)))
	}
	if s := g.PromptCV; s != nil {
		switch {
		case c.PromptCVUnknown:
			out = append(out, fmt.Sprintf("prompt spread not measured (cv %s assumed), measured at cv %s", ratioText(c.PromptCV), spanText(*s, ratioText)))
		case !s.holds(c.PromptCV):
			out = append(out, fmt.Sprintf("prompt cv %s, measured %s", ratioText(c.PromptCV), spanText(*s, ratioText)))
		}
	}
	ca2 := c.ArrivalCV2
	if ca2 == 0 {
		ca2 = 1
	}
	if s := g.ArrivalCV2; s != nil {
		switch {
		case c.ArrivalsUnknown:
			out = append(out, "arrivals not measured (Poisson assumed), measured at Ca^2 "+spanText(*s, ratioText))
		case !s.holds(ca2):
			out = append(out, fmt.Sprintf("arrivals Ca^2 %s, measured %s", ratioText(ca2), spanText(*s, ratioText)))
		}
	}
	for _, s := range c.Stated {
		out = append(out, s+" stated, which the run did not price")
	}
	for _, fl := range f.Flags {
		if slices.Contains(e.AllowedFlags[source], fl) {
			continue
		}
		why, ok := e.OutsideFlags[fl]
		if !ok {
			why = string(fl) + ": an input the run did not price"
		}
		out = append(out, why)
	}
	if len(g.FlowControl) > 0 && !slices.Contains(g.FlowControl, c.FlowControl) {
		out = append(out, fmt.Sprintf("flow control %s, measured %s", orUnknown(c.FlowControl), strings.Join(g.FlowControl, ", ")))
	}
	if len(g.Models) > 0 && !slices.Contains(g.Models, c.Model) {
		out = append(out, fmt.Sprintf("model %s, measured %s", orUnknown(c.Model), strings.Join(g.Models, ", ")))
	}
	if len(g.TensorParallel) > 0 && !slices.Contains(g.TensorParallel, c.TensorParallel) {
		out = append(out, fmt.Sprintf("decode tensor parallelism %s, measured %s", countText(c.TensorParallel), intsText(g.TensorParallel)))
	}
	if len(g.PrefillTensorParallel) > 0 && !slices.Contains(g.PrefillTensorParallel, c.PrefillTensorParallel) {
		out = append(out, fmt.Sprintf("prefill tensor parallelism %s, measured %s", countText(c.PrefillTensorParallel), intsText(g.PrefillTensorParallel)))
	}
	if s := g.PrefixHitRate; s != nil && !s.holds(c.PrefixHitRate) {
		out = append(out, fmt.Sprintf("prefix hit rate %s, measured %s", ratioText(c.PrefixHitRate), spanText(*s, ratioText)))
	}
	if len(g.Accelerators) > 0 && !slices.Contains(g.Accelerators, c.Accelerator) {
		out = append(out, fmt.Sprintf("accelerator %s, measured %s", orUnknown(c.Accelerator), strings.Join(g.Accelerators, ", ")))
	}
	if len(g.PrefillReplicas) > 0 && !slices.Contains(g.PrefillReplicas, c.PrefillReplicas) {
		out = append(out, fmt.Sprintf("%s prefill pods, measured %s", countText(c.PrefillReplicas), intsText(g.PrefillReplicas)))
	}
	if len(g.DecodeReplicas) > 0 && !slices.Contains(g.DecodeReplicas, c.DecodeReplicas) {
		out = append(out, fmt.Sprintf("%s decode pods, measured %s", countText(c.DecodeReplicas), intsText(g.DecodeReplicas)))
	}
	if s := g.PromptTokens; s != nil && !(c.PromptTokens > 0 && s.holds(c.PromptTokens)) {
		out = append(out, fmt.Sprintf("prompt %s tokens, measured %s", amountText(c.PromptTokens), spanText(*s, amountText)))
	}
	if s := g.OutputTokens; s != nil && !(c.OutputTokens > 0 && s.holds(c.OutputTokens)) {
		out = append(out, fmt.Sprintf("output %s tokens, measured %s", amountText(c.OutputTokens), spanText(*s, amountText)))
	}
	if g.MinKneeShare > 0 || g.MaxKneeShare > 0 {
		switch {
		case f.KneeShare <= 0:
			out = append(out, "rate against the knee unknown")
		case g.MaxKneeShare > 0 && f.KneeShare > g.MaxKneeShare+shareSlack:
			out = append(out, fmt.Sprintf("at %.3f of the knee, measured %s", f.KneeShare, shareText(g)))
		case f.KneeShare < g.MinKneeShare-shareSlack:
			out = append(out, fmt.Sprintf("at %.3f of the knee, measured %s", f.KneeShare, shareText(g)))
		}
	}
	return out
}

// shareText is a region's knee shares in words.
func shareText(g Region) string {
	if g.MinKneeShare > 0 {
		return fmt.Sprintf("%.3f to %.3f", g.MinKneeShare, g.MaxKneeShare)
	}
	return fmt.Sprintf("up to %.3f", g.MaxKneeShare)
}

// unknownText stands for a class value no input gave.
const unknownText = "unknown"

func orUnknown(s string) string {
	if s == "" {
		return unknownText
	}
	return s
}

func countText(n int) string {
	if n <= 0 {
		return unknownText
	}
	return strconv.Itoa(n)
}

func intsText(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ", ")
}

func amountText(x float64) string {
	if x <= 0 {
		return unknownText
	}
	return strconv.FormatFloat(math.Round(x), 'f', -1, 64)
}

func ratioText(x float64) string { return strconv.FormatFloat(x, 'g', 3, 64) }

func spanText(s Span, text func(float64) string) string {
	return text(s.Lo) + " to " + text(s.Hi)
}

// statisticText names a statistic: "mean", or "p90".
func statisticText(q float64) string {
	if q == 0 {
		return "mean"
	}
	return "p" + strconv.FormatFloat(math.Round(1e6*q)/1e4, 'f', -1, 64)
}

func statisticsText(qs []float64) string {
	parts := make([]string, len(qs))
	for i, q := range qs {
		parts[i] = statisticText(q)
	}
	return strings.Join(parts, ", ")
}
