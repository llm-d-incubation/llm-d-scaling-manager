package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The human output's pieces, shared by both questions.

// refusedText is a refusal as the output prints it: the reason, then what
// caused it.
func refusedText(u whatif.Unanswerable) string {
	if u.Detail == "" {
		return "refused: " + string(u.Reason)
	}
	return fmt.Sprintf("refused: %s (%s)", u.Reason, u.Detail)
}

// rateText is a rate in requests per second.
func rateText(r float64) string { return fmt.Sprintf("%.2f r/s", r) }

// boundText names what set a rate or a knee, in order.
func boundText(by []whatif.Bound) string {
	parts := make([]string, len(by))
	for i, b := range by {
		parts[i] = string(b)
	}
	return strings.Join(parts, ", ")
}

// lineMismatch is a line the recording's rows do not support: an answer read
// from it is printed, flagged, and says how far off the line check read it
// (checkText). The CLI alone raises it.
const lineMismatch whatif.Flag = "LineMismatch"

// checkText is the line check as the output prints it, "" with no line. A
// check that compared nothing says so: an empty result is not a pass.
func (pv pricedVariant) checkText() string {
	if pv.state.Line.IsZero() {
		return ""
	}
	c := pv.check
	left := fmt.Sprintf("%d on a ramp, %d above the band, %d older than %s", c.OnRamp, c.AboveBand, c.TooOld, whatif.MaxLineRowAge)
	if !c.Ran() {
		return fmt.Sprintf("not run: %d rows to compare at fill %.2f to %.2f, want %d (%s)",
			c.Compared, itl.DefaultGPSMinKForVerification, pv.decode.Band.Hi, whatif.MinLineRows, left)
	}
	verdict := "ok"
	if c.Mismatch() {
		verdict = "MISMATCH"
	}
	if c.OtherShape {
		verdict += fmt.Sprintf(" at another shape (rows at KV %s a request, priced at %s)",
			num(round(c.RowKVPerRequest)), num(round(pv.decode.KVPerRequest)))
	}
	return fmt.Sprintf("%s: median %.1f%% off the rate read, line %s; %d of %d rows past %.0f%%, worst %.1f%%, fill %.2f-%.2f, cycles %d-%d; left out %s",
		verdict, c.MedianErrorPct, directionText(c.SignedErrorPct), c.Past, c.Compared, itl.DefaultGPSMismatchThresholdPct,
		c.WorstErrorPct, c.FillLo, c.FillHi, c.FromCycle, c.ToCycle, left)
}

// directionText says which way a line reads, from its signed median error.
func directionText(signed float64) string {
	if signed > 0 {
		return "high"
	}
	return "low"
}

// lineText is the line a variant was priced on, and the fills it was read
// at.
func lineText(pv pricedVariant) string {
	tier := pv.state.Tier
	if tier == "" {
		tier = "unknown tier"
	}
	s := "line " + tier
	if pv.state.BorrowedFrom != "" {
		s += " from " + pv.state.BorrowedFrom
	}
	if top, ok := pv.observedFillMax(); ok {
		s += fmt.Sprintf(", observed fill up to %.2f", top)
	}
	return s
}

// flagsLine prints the flags the variant's answers carry, once each, in the
// order they first appear, with what each rests on where there is more to
// say. Nothing is printed when there are none.
func flagsLine(b *strings.Builder, pv pricedVariant, results ...whatif.Result) {
	var flags []whatif.Flag
	fill := -1.0
	for _, r := range results {
		var from []whatif.Flag
		switch a := r.(type) {
		case whatif.Answer:
			from = a.Flags
			if a.HasFlag(whatif.ExtrapolatedFill) {
				fill = max(fill, a.Basis.Fill)
			}
		case whatif.TTFT:
			from = a.Flags
		case whatif.Unanswerable:
			from = a.Flags
		}
		for _, f := range from {
			if !slices.Contains(flags, f) {
				flags = append(flags, f)
			}
		}
	}
	if len(flags) == 0 {
		return
	}
	parts := make([]string, 0, len(flags))
	for _, f := range flags {
		switch f {
		case whatif.AssumedShape:
			parts = append(parts, fmt.Sprintf("%s (IL %s, OL %s)", f, num(pv.shape.AvgInputTokens), num(pv.shape.AvgOutputTokens)))
		case whatif.ExtrapolatedFill:
			top, _ := pv.observedFillMax()
			parts = append(parts, fmt.Sprintf("%s (fill %.3f, %.3f above the readings)", f, fill, fill-top))
		case whatif.BorrowedLine:
			parts = append(parts, fmt.Sprintf("%s (from %s)", f, pv.state.BorrowedFrom))
		case whatif.TierUnstable:
			parts = append(parts, fmt.Sprintf("%s (%d/%d resamples changed tier)", f, pv.lines.TierChanges(), pv.lines.Drawn))
		case whatif.OutputSpreadUnread:
			parts = append(parts, fmt.Sprintf("%s (%s)", f, outputSpreadUnreadWords))
		case whatif.LineNearGate:
			parts = append(parts, fmt.Sprintf("%s (%d/%d)", f, pv.lines.TierChanges(), pv.lines.Drawn))
		default:
			parts = append(parts, string(f))
		}
	}
	fmt.Fprintf(b, "flags: %s\n", strings.Join(parts, "; "))
}
