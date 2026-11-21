package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The decode line's intercept B, stated where the recording measured none
// (issue llm-d-incubation/llm-d-scaling-manager#225). A pinned-B line on the
// default intercept (itl.DefaultBaselineSec) is refused as PinnedDefaultB:
// its slope was fitted against a B no engine was seen to have. On a real P/D
// window that is every cycle, because the tracker learns B only from a full
// fit. --assume-itl-b states B, and the slope is fitted again through it on
// the same readings (itl.Refit), so the line still runs through what the
// replicas read and only its extrapolation moves. Every figure on it is
// flagged AssumedITLB and outside the validated region (pricedVariant.stated).
// A line the tracker priced one-sided past the cap is not refitted: its price
// is set by the cap, not by B.

// assumedITLB is a decode figure priced on a line whose intercept B the
// caller stated (--assume-itl-b, the API's assume.itlB or
// assume.itlBFromFloor), its slope refitted through it.
const assumedITLB whatif.Flag = "AssumedITLB"

// itlBFloor is the --assume-itl-b value that takes B from the floor read
// below the band.
const itlBFloor = "floor"

// maxAssumedITLB is the largest B --assume-itl-b takes: a token every second
// with the cache empty is no engine's floor, and a typo (12s for 12ms).
const maxAssumedITLB = time.Second

// Where an assumed B came from (interceptAssumption.Source).
const (
	// itlBStated is a duration the caller typed.
	itlBStated = "stated"
	// itlBFromFloor is the intercept of the line through the floor below the
	// band and the readings' mean (itl.FloorIntercept).
	itlBFromFloor = "floor"
)

// interceptAssumption is what --assume-itl-b did to one decode variant's
// line.
type interceptAssumption struct {
	// Asked says the flag was set; Source is how (itlBStated, itlBFromFloor).
	Asked  bool
	Source string
	// Applied says the line was refitted through Seconds; Why, when it was
	// not, says why in words.
	Applied bool
	Seconds float64
	Why     string
	// FloorSeconds and FloorFill are the floor below the band and its fill,
	// for a B from the floor (0 with none).
	FloorSeconds, FloorFill float64
	// DefaultA is the slope the line had on the default intercept.
	DefaultA float64
}

// parseAssumeITLB reads --assume-itl-b: "floor", or a duration above 0 and at
// most maxAssumedITLB. Empty is not set.
func parseAssumeITLB(s string) (floor bool, b time.Duration, err error) {
	if s == itlBFloor {
		return true, 0, nil
	}
	d, perr := time.ParseDuration(s)
	if perr != nil || d < time.Microsecond || d > maxAssumedITLB {
		return false, 0, usagef("--assume-itl-b %q, want floor, or a duration from 1us to %s, such as 12ms", s, maxAssumedITLB)
	}
	return false, d, nil
}

// assumeIntercept states the decode line's intercept B as --assume-itl-b
// asks, on a line on the default intercept only (whatif.OnDefaultB): the
// slope is fitted again through it on the line's own readings, with the
// tracker's choice of fit (itl.Refit), and pv.state carries the new line,
// its readings and B as the baseline the bands refit through. A line whose B
// was measured is priced as it is; a held or borrowed line has no readings
// here to refit, and stays refused. pv.itlB says which, and why.
func (f *answerFlags) assumeIntercept(pv *pricedVariant) {
	if f.assumeITLB == "" {
		return
	}
	a := &pv.itlB
	a.Asked = true
	floor, stated, err := parseAssumeITLB(f.assumeITLB)
	if err != nil {
		a.Why = err.Error()
		return
	}
	vs := &pv.state
	a.Source, a.Seconds = itlBStated, stated.Seconds()
	if floor {
		a.Source, a.Seconds = itlBFromFloor, 0
	}
	switch {
	case vs.Line.IsZero():
		a.Why = "no ITL line to state B for"
		return
	case !whatif.OnDefaultB(vs.Tier, vs.Line):
		a.Why = fmt.Sprintf("the %s line is not refused on the default intercept (its B is %s); a stated B is used only there",
			vs.Tier, ms(vs.Line.B))
		return
	case vs.Tier != whatif.TierPinnedB:
		a.Why = fmt.Sprintf("a %s line has no readings of its own at this cycle to refit through a stated B; "+
			"ask at a cycle where the line is pinned-B", vs.Tier)
		return
	case !vs.Refitted || len(vs.LineReadings) == 0:
		a.Why = "the replay does not reproduce this pinned-B line from its readings, so it cannot be refitted through a stated B"
		return
	}
	if floor {
		a.FloorSeconds, a.FloorFill = vs.BelowBandITL, vs.BelowBandFill
		b, why := floorB(*vs)
		if why != "" {
			a.Why = why
			return
		}
		a.Seconds = b
	} else if vs.BelowBandITL > 0 && a.Seconds > vs.BelowBandITL {
		// Every reading below the band is B + A k, so at least B.
		a.Why = fmt.Sprintf("B %s is above the floor the decode Pods read below the band, %s at fill %.2f",
			ms(a.Seconds), ms(vs.BelowBandITL), vs.BelowBandFill)
		return
	}
	got, why := refitThrough(*vs, a.Seconds)
	if why != "" {
		a.Why = why
		return
	}
	a.Applied, a.DefaultA = true, vs.Line.A
	vs.Line, vs.LineReadings, vs.Baseline = got.Model, got.Read, a.Seconds
	pv.stated = append(pv.stated, "--assume-itl-b")
}

// oneSidedLine is why no B refits a line priced one-sided past the cap.
const oneSidedLine = "the line is priced one-sided past the cap, at a prior, not through B"

// refitThrough is the line refitted through B on vs's readings, with the
// tracker's choice of fit (itl.Refit), or why no line runs through B.
func refitThrough(vs learnreplay.VariantState, b float64) (itl.Refitted, string) {
	got := itl.Refit(vs.Readings.All, vs.Readings.Steady, b, vs.MinSettled, vs.OneSided)
	switch {
	case got.PinnedAtBPrior || got.PinnedAtLeastITL:
		return got, oneSidedLine
	case got.Tier != whatif.TierPinnedB || !itl.ValidModel(got.Model.A, got.Model.B) || math.Abs(got.Model.B-b) > 1e-12:
		return got, fmt.Sprintf("no slope fits the line's readings through B %s: it is at or above what they read", ms(b))
	}
	return got, ""
}

// floorAssumption is the B --assume-itl-b floor would price the line at, or
// why it would not: the floor's intercept (floorB), and a line refitted
// through it (refitThrough). The hint offers the floor only where it works.
func floorAssumption(vs learnreplay.VariantState) (float64, string) {
	if !vs.Refitted || len(vs.LineReadings) == 0 {
		return 0, "the replay does not reproduce this pinned-B line from its readings"
	}
	// A line priced one-sided past the cap takes no B at all: probed with
	// one under the default, whatever the floor gives.
	if _, why := refitThrough(vs, itl.DefaultBaselineSec/2); why == oneSidedLine {
		return 0, why
	}
	b, why := floorB(vs)
	if why != "" {
		return 0, why
	}
	if _, why := refitThrough(vs, b); why != "" {
		return 0, why
	}
	return b, ""
}

// floorB is the intercept of the line through the floor read below the band
// and the mean of the line's readings (itl.FloorIntercept), or why there is
// none.
func floorB(vs learnreplay.VariantState) (float64, string) {
	if !(vs.BelowBandITL > 0) {
		return 0, fmt.Sprintf("no floor below the band: fewer than %d steady readings under the band's bottom", itl.BelowBandMinReadings)
	}
	b, ok := itl.FloorIntercept(vs.LineReadings, itl.Observation{K: vs.BelowBandFill, ITLSec: vs.BelowBandITL})
	if !ok {
		return 0, fmt.Sprintf("the floor %s at fill %.2f gives no intercept with the line's readings "+
			"(their mean fill under twice the floor's, or B outside (0, the floor])", ms(vs.BelowBandITL), vs.BelowBandFill)
	}
	return b, ""
}

// ms is seconds as milliseconds, in words.
func ms(s float64) string { return fmt.Sprintf("%.1f ms", s*1000) }

// itlBWords is the variant's assumed B as the answer's assumed list names
// it; empty when none was asked or it was not applied (itlBRowText says why).
func (pv pricedVariant) itlBWords() string {
	a := pv.itlB
	switch {
	case !a.Applied:
		return ""
	case a.Source == itlBFromFloor:
		return fmt.Sprintf("ITL B %s, from the floor read below the band (%s at fill %.2f), in place of the %s default; "+
			"the slope refitted through it on the same readings; the bands hold B fixed",
			ms(a.Seconds), ms(a.FloorSeconds), a.FloorFill, ms(itl.DefaultBaselineSec))
	}
	return fmt.Sprintf("ITL B %s, stated, in place of the %s default; the slope refitted through it on the same readings; "+
		"the bands hold B fixed", ms(a.Seconds), ms(itl.DefaultBaselineSec))
}

// itlBNotUsed is why a stated B was not used, in the words of the question's
// source; empty when it was used or not asked.
func (pv pricedVariant) itlBNotUsed() string {
	a := pv.itlB
	switch {
	case !a.Asked || a.Applied:
		return ""
	case pv.api && a.Source == itlBFromFloor:
		return whatifapi.StatedITLBNotUsed + "assume.itlBFromFloor): " + a.Why
	case pv.api:
		return whatifapi.StatedITLBNotUsed + "assume.itlB): " + a.Why
	}
	return whatifapi.StatedITLBNotUsed + "--assume-itl-b): " + a.Why
}

// itlBRowText is the ITL B row: what was assumed, or why nothing was; empty
// when the flag was not set.
func (pv pricedVariant) itlBRowText() string {
	if w := pv.itlBWords(); w != "" {
		return strings.TrimPrefix(w, "ITL B ")
	}
	return strings.TrimPrefix(pv.itlBNotUsed(), "ITL B ")
}

// noLineHint is what to do about a variant that learned no ITL line: the
// line needs decode's KV-cache use to span 0.30 or more (troubleshooting.md,
// NoLine). It names both sources' words, so that the CLI's refusal and the
// API's are the same.
func (pv pricedVariant) noLineHint() string {
	return "; the line needs a window in which decode's KV-cache use spans 0.30 or more: answer at a later cycle where one is " +
		"learned (--cycle; source.cycle on the API), or record a busier window"
}

// pinnedDefaultBHint is what a PinnedDefaultB refusal adds: how to state B,
// in the words of the question's source, or why the stated B was not used.
func (pv pricedVariant) pinnedDefaultBHint() string {
	if w := pv.itlBNotUsed(); w != "" {
		return "; " + w
	}
	if pv.state.Tier != whatif.TierPinnedB {
		return fmt.Sprintf("; a %s line cannot take a stated B: answer at a cycle where the line is pinned-B", pv.state.Tier)
	}
	if !pv.state.Refitted || len(pv.state.LineReadings) == 0 {
		return "; no stated B can price it: the replay does not reproduce this pinned-B line from its readings, so it cannot be refitted"
	}
	b, why := floorAssumption(pv.state)
	if why == oneSidedLine {
		// Its price is set by the cap: a stated B would be declined too.
		return "; no stated B can price it: " + why
	}
	if why != "" {
		// The floor gives no B here: offering it would only be declined
		// (issue llm-d-incubation/llm-d-scaling-manager#277).
		// A floor read caps a stated B: every reading below the band is
		// B + A k, so at least B (assumeIntercept).
		api, cli := "assume.itlB in seconds", "--assume-itl-b 12ms states one"
		if f := pv.state.BelowBandITL; f > 0 {
			api = fmt.Sprintf("assume.itlB in seconds, at most the floor's %.4g", f)
			cli = fmt.Sprintf("--assume-itl-b with a duration at most the floor's %s states one", ms(f))
		}
		if pv.api {
			return "; state B for a flagged answer: " + api + " (on the page, Assume a term, ITL B); the floor gives none here: " + why
		}
		return "; state B for a flagged answer: " + cli + "; the floor gives none here: " + why
	}
	// The floor caps a stated B here too.
	f := pv.state.BelowBandITL
	floor := fmt.Sprintf(" (here %s, through the floor of %s at fill %.2f)", ms(b), ms(f), pv.state.BelowBandFill)
	if pv.api {
		return "; state B for a flagged answer: assume.itlBFromFloor true takes it from the floor below the band" + floor +
			fmt.Sprintf(", or assume.itlB in seconds, at most the floor's %.4g (on the page, Assume a term, ITL B)", f)
	}
	return "; state B for a flagged answer: --assume-itl-b floor takes it from the floor below the band" + floor +
		fmt.Sprintf(", or --assume-itl-b with a duration at most the floor's %s states one", ms(f))
}
