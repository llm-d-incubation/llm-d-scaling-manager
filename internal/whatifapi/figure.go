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
		f := Figure{Value: ptr(v.Value), Unit: unit, Sensitivity: spanOf(v.Sensitivity, worseIsHigh), Flags: flagNames(v.Flags)}
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
		return f
	case whatif.TTFT:
		f := Figure{Value: ptr(v.Seconds), Unit: unitSeconds, Sensitivity: spanOf(v.Sensitivity, true), Flags: flagNames(v.Flags)}
		if v.Floor > 0 {
			f.Floor = ptr(v.Floor)
		}
		return f
	case whatif.Unanswerable:
		return Figure{Refused: refusalOf(v)}
	}
	return Figure{}
}

// refusalOf is a refusal on the wire.
func refusalOf(u whatif.Unanswerable) *Refusal {
	return &Refusal{Reason: string(u.Reason), Detail: u.Detail, Flags: flagNames(u.Flags), AssumedTermDecides: u.AssumedTermDecides}
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

// flagNames is flags as strings, nil with none.
func flagNames(flags []whatif.Flag) []string {
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
