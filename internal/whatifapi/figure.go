package whatifapi

import (
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The units a figure is in.
const (
	unitRate    = "r/s"
	unitSeconds = "s"
)

// figureOf is r on the wire, in unit: an answer's value and what it rests on,
// or a refusal with no number. worseIsHigh says which side of a span a refused
// corner takes: the higher TTFT, or the lower rate. A nil r (a question not
// asked) is the zero Figure; callers leave it out.
func figureOf(r whatif.Result, unit string, worseIsHigh bool) Figure {
	switch v := r.(type) {
	case whatif.Answer:
		f := Figure{Value: ptr(v.Value), Unit: unit, Sensitivity: spanOf(v.Sensitivity, worseIsHigh), Flags: FlagNames(v.Flags)}
		if v.Band != nil {
			f.Band = &Band{Lo: v.Band.Lo, Hi: v.Band.Hi}
		}
		for _, b := range v.Basis.BoundBy {
			f.BoundBy = append(f.BoundBy, string(b))
		}
		if v.Basis.Uncapped > 0 {
			f.Uncapped = ptr(v.Basis.Uncapped)
		}
		if v.Basis.Floor > 0 {
			f.Floor = ptr(v.Basis.Floor)
		}
		if t := v.Basis.TTFT; t != nil {
			if t.Floor > 0 {
				f.Floor = ptr(t.Floor)
			}
			if t.PrefillOnlyRate > 0 {
				f.PrefillOnlyRate = ptr(t.PrefillOnlyRate)
			}
		}
		if b := v.Basis.Batch; b != nil {
			f.BatchBeyondFit = &BatchBeyondFit{Priced: b.Priced, Fitted: b.Fitted, Ranks: b.Ranks}
		}
		return f
	case whatif.TTFT:
		f := Figure{Value: ptr(v.Seconds), Unit: unitSeconds, Sensitivity: spanOf(v.Sensitivity, true), Flags: FlagNames(v.Flags)}
		if v.Floor > 0 {
			f.Floor = ptr(v.Floor)
		}
		f.UpperBound = TTFTUpperBound(v.Flags)
		return f
	case whatif.Unanswerable:
		return Figure{Refused: refusalOf(v)}
	}
	return Figure{}
}

// TTFTUpperBound says a TTFT carrying flags is an upper bound: priced from a
// V_P that is only a lower bound (whatif.VPLowerBound), not a stated one
// (whatif.AssumedVP). A pod's prompt time and its queue's wait both fall as
// V_P rises, at every rate under the knee, so the true TTFT is at most the
// one priced (#369).
func TTFTUpperBound(flags []whatif.Flag) bool {
	return slices.Contains(flags, whatif.VPLowerBound) && !slices.Contains(flags, whatif.AssumedVP)
}

// upperBoundFigure says f is a latency priced as an upper bound
// (TTFTUpperBound), read from its flags, so that a saved prediction that
// predates Figure.UpperBound reads the same.
func upperBoundFigure(f Figure) bool {
	return f.Value != nil && f.Unit == unitSeconds && slices.Contains(f.Flags, string(whatif.VPLowerBound)) &&
		!slices.Contains(f.Flags, string(whatif.AssumedVP))
}

// refusalOf is a refusal on the wire.
func refusalOf(u whatif.Unanswerable) *Refusal {
	return &Refusal{Reason: string(u.Reason), Detail: u.Detail, Flags: FlagNames(u.Flags), AssumedTermDecides: u.AssumedTermDecides}
}

// spanOf is a span on the wire: a refused corner leaves its worse side with no
// number, as the CLI prints the reason in that side's place.
func spanOf(s *whatif.Sensitivity, worseIsHigh bool) *Span {
	if s == nil {
		return nil
	}
	out := &Span{Lo: ptr(s.Lo), Hi: ptr(s.Hi)}
	switch {
	case s.Refused == "":
	case worseIsHigh:
		out.Hi, out.HiRefused = nil, string(s.Refused)
	default:
		out.Lo, out.LoRefused = nil, string(s.Refused)
	}
	return out
}

// FlagNames is flags as strings, nil with none: how every flag reaches the
// wire.
func FlagNames(flags []whatif.Flag) []string {
	if len(flags) == 0 {
		return nil
	}
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = string(f)
	}
	return out
}

// ptr is a pointer to a copy of x.
func ptr(x float64) *float64 { return &x }

// valueOf is an answered figure's value; ok false on a refusal or nil.
func valueOf(r whatif.Result) (float64, bool) {
	switch v := r.(type) {
	case whatif.Answer:
		return v.Value, true
	case whatif.TTFT:
		return v.Seconds, true
	}
	return 0, false
}

// refusedFigure is a figure that is the refusal u.
func refusedFigure(u *Refusal) Figure {
	c := *u
	c.Flags = slices.Clone(u.Flags)
	return Figure{Refused: &c}
}

// Validation is the calibration envelope's verdict on a figure
// (whatif.Envelope; docs/guides/capacity-planner/accuracy.md): the measured
// row whose region holds it, or why none does.
type Validation struct {
	Validated bool `json:"validated"`
	// Label is the verdict in words: "validated in-sample: error -5..+18%
	// (min-max, n=6 load levels, 12 readings; q32 staircase)", or "OUTSIDE
	// VALIDATED REGION (why)".
	Label string `json:"label"`
	// Source, Shape, ErrorLo, ErrorHi, Spread, Readings, Levels and Holdout
	// are the row that holds the figure, absent outside it. The errors are
	// signed shares, the model over the measured figure less 1; one absent
	// is unbounded on its side. Spread is the kind of range they are
	// ("min-max", "p10-p90", "bracket" or "one-sided"); Readings the
	// readings the range rests on, Levels the independent load levels among
	// them; Holdout is set with every row, true only for a row whose run
	// never tuned the planner and false for an in-sample one, and absent
	// outside every row.
	Source   string   `json:"source,omitempty"`
	Shape    string   `json:"shape,omitempty"`
	ErrorLo  *float64 `json:"errorLo,omitempty"`
	ErrorHi  *float64 `json:"errorHi,omitempty"`
	Spread   string   `json:"spread,omitempty"`
	Readings int      `json:"readings,omitempty"`
	Levels   int      `json:"levels,omitempty"`
	Holdout  *bool    `json:"holdout,omitempty"`
	// Outside is why no row holds the figure.
	Outside string `json:"outside,omitempty"`
}

// ValidationOf is v on the wire.
func ValidationOf(v whatif.Validation) *Validation {
	w := &Validation{Validated: v.Validated(), Label: v.String(), Outside: v.Outside}
	if r := v.Row; r != nil {
		w.Source, w.Shape, w.ErrorLo, w.ErrorHi, w.Readings = r.Source, r.Shape, r.ErrorLo, r.ErrorHi, r.Readings
		holdout := r.Holdout
		w.Spread, w.Levels, w.Holdout = r.Spread, r.Levels, &holdout
	}
	return w
}

// validate is f with the shipped envelope's verdict on it, as a figure of
// kind at statistic q, priced at share of its knee (0: none); a refused
// figure has no number to judge, and is f as it is.
func validate(f Figure, kind whatif.FigureKind, q float64, class whatif.FigureClass, share float64) Figure {
	return judged(f, whatif.Figure{Kind: kind, Percentile: q, Class: class, KneeShare: share})
}

// validateRate is validate for a rate at a target, judged as the CLI judges
// it (whatif.RateFigure): by what bound it, at its share of the lower knee.
func validateRate(f Figure, q float64, class whatif.FigureClass, lowerKnee float64) Figure {
	if f.Value == nil {
		return f
	}
	return judged(f, whatif.RateFigure(*f.Value, f.BoundBy, q, class, lowerKnee, nil))
}

// judged is f with the verdict on wf, which takes f's flags.
func judged(f Figure, wf whatif.Figure) Figure {
	if f.Value == nil {
		return f
	}
	wf.Flags = make([]whatif.Flag, len(f.Flags))
	for i, name := range f.Flags {
		wf.Flags[i] = whatif.Flag(name)
	}
	v := whatif.DefaultEnvelope().Judge(wf)
	f.Validation = ValidationOf(v)
	if v.Validated() {
		// A figure the measured run covered is not flagged unvalidated beside
		// its own label, as the CLI's (settled).
		f.Flags = slices.DeleteFunc(slices.Clone(f.Flags), func(name string) bool { return name == string(whatif.UnvalidatedTTFT) })
		if len(f.Flags) == 0 {
			f.Flags = nil
		}
	}
	return f
}

// shareOf is rate over knee; 0 where either is not a figure.
func shareOf(rate, knee float64) float64 {
	if !(rate > 0 && knee > 0) {
		return 0
	}
	return rate / knee
}
