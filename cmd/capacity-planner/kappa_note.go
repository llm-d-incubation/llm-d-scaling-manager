package main

import (
	"fmt"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The text note on a stated condition number (whatif.Kappa).
//
// Every forward decode figure carries kappa = g/(1-g), g the loop gain at
// its rate, in the JSON and the API. The text says so only past
// whatif.KappaNoteAbove (4, a gain of 0.8): there a 1% error in the ITL
// line's slope moves the fill by 4% or more, and the TPOT by as much or,
// with a KV pull, more; the figure rests mostly on the slope. The note
// moves no figure.

// kappaAt is one forward decode figure's condition number, by what the
// text calls the figure.
type kappaAt struct {
	label string
	kappa *float64
}

// kappaNote is the line naming every figure whose kappa is past
// whatif.KappaNoteAbove, prefixed by indent; "" when none is.
func kappaNote(indent string, figures ...kappaAt) string {
	var past []string
	for _, f := range figures {
		if f.kappa != nil && *f.kappa > whatif.KappaNoteAbove {
			past = append(past, fmt.Sprintf("%.1f at %s", *f.kappa, f.label))
		}
	}
	if len(past) == 0 {
		return ""
	}
	return fmt.Sprintf("%scondition: kappa %s, past %s: a 1%% error in the ITL line's slope moves the fill there about kappa%% "+
		"and the TPOT as much or more, so these figures rest mostly on the slope\n", indent, strings.Join(past, ", "), num(whatif.KappaNoteAbove))
}

// kappaOf is a Result's kappa, nil for a refusal or a figure with none.
func kappaOf(r whatif.Result) *float64 {
	if a, ok := r.(whatif.Answer); ok {
		return a.Basis.Kappa
	}
	return nil
}

// reportKappaNote is at-replicas' note for a shape: the decode knee and
// each load's mean TPOT.
func reportKappaNote(s whatifapi.ReportShape) string {
	figures := make([]kappaAt, 0, 1+len(s.Table))
	figures = append(figures, kappaAt{"the decode knee", s.Capacity.DecodeKnee.Kappa})
	for _, row := range s.Table {
		figures = append(figures, kappaAt{row.Label, row.TPOTMean.Kappa})
	}
	return kappaNote("  ", figures...)
}
