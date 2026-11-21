package whatifapi

import (
	"fmt"
	"slices"
	"time"
)

// FlagWindowGaps marks an answer read from a window that misses more than
// WindowGapShare of the cycles its span should hold: a capture or recording
// with holes, whose load and figures are read from the cycles present only.
const FlagWindowGaps = "WindowGaps"

// WindowGapShare is the share of a window's expected cycles that may be
// missing before an answer is flagged WindowGaps.
const WindowGapShare = 0.05

// WindowGaps is how much of a window's span its cycles cover: the cycles the
// span should hold at the window's own cycle time, those present, and the
// longest stretch with none. A recording or Prometheus window with holes is
// read from the cycles present alone, and a hole over the busiest minutes
// reads the load low.
type WindowGaps struct {
	// CycleSeconds is the window's cycle time: the median time between two
	// consecutive cycles.
	CycleSeconds float64 `json:"cycleSeconds"`
	// ExpectedCycles is the cycles the span from the first cycle to the
	// last holds at CycleSeconds, and PresentCycles those read; MissingShare
	// is the share of ExpectedCycles not read.
	ExpectedCycles int     `json:"expectedCycles"`
	PresentCycles  int     `json:"presentCycles"`
	MissingShare   float64 `json:"missingShare"`
	// LargestGapSeconds is the longest stretch between two consecutive
	// cycles beyond one cycle time, 0 with none; LargestGapFrom and
	// LargestGapTo are the cycles either side of it.
	LargestGapSeconds float64    `json:"largestGapSeconds"`
	LargestGapFrom    *time.Time `json:"largestGapFrom,omitempty"`
	LargestGapTo      *time.Time `json:"largestGapTo,omitempty"`
	// Flagged is MissingShare above WindowGapShare (FlagWindowGaps).
	Flagged bool `json:"flagged"`
	// long is every gap at least RollingWindow long, oldest first: those
	// that could hide a busiest stretch of their own (GapAt).
	long []gapSpan
}

// gapSpan is one gap: the cycles either side of it, and its length beyond
// one cycle time.
type gapSpan struct {
	from, to time.Time
	seconds  float64
}

// words is the gap in words: "15m0s from 02:42 to 02:58Z", the times being
// the cycles either side of it.
func (s gapSpan) words() string {
	return fmt.Sprintf("%s from %s to %sZ", time.Duration(s.seconds*float64(time.Second)).Round(time.Second),
		s.from.Format("15:04"), s.to.Format("15:04"))
}

// WindowGapsOf is the gaps of a window whose cycles were read at times,
// oldest first; a time read twice counts once. Nil with fewer than three
// distinct cycles, where no cycle time can be read. Two cycles further apart
// than one and a half cycle times count the cycles that would fit between
// them as missing.
//
// It counts cycles the source does not hold, whatever the cause: a capture
// or recording that lost them, a model parked at zero (no cycle is recorded
// for it), or a cycle time that changed within the window (the median is
// one of the two, and the longer reads as gaps).
func WindowGapsOf(times []time.Time) *WindowGaps {
	diffs := make([]float64, 0, len(times))
	kept := make([]time.Time, 0, len(times))
	for _, t := range times {
		if n := len(kept); n > 0 {
			d := t.Sub(kept[n-1]).Seconds()
			if d <= 0 {
				continue
			}
			diffs = append(diffs, d)
		}
		kept = append(kept, t)
	}
	if len(kept) < 3 {
		return nil
	}
	times = kept
	sorted := slices.Clone(diffs)
	slices.Sort(sorted)
	// The lower median: with two intervals, a gap is never its own cycle.
	cycle := sorted[(len(sorted)-1)/2]
	g := &WindowGaps{CycleSeconds: cycle, PresentCycles: len(times)}
	missing := 0
	largest := -1
	for i, d := range diffs {
		if d <= 1.5*cycle {
			continue
		}
		missing += int(d/cycle+0.5) - 1
		gap := d - cycle
		if gap > g.LargestGapSeconds {
			g.LargestGapSeconds, largest = gap, i
		}
		if gap >= RollingWindow.Seconds() {
			g.long = append(g.long, gapSpan{from: times[i].UTC(), to: times[i+1].UTC(), seconds: gap})
		}
	}
	if largest >= 0 {
		from, to := times[largest].UTC(), times[largest+1].UTC()
		g.LargestGapFrom, g.LargestGapTo = &from, &to
	}
	g.ExpectedCycles = g.PresentCycles + missing
	g.MissingShare = float64(missing) / float64(g.ExpectedCycles)
	g.Flagged = g.MissingShare > WindowGapShare
	return g
}

// ReasonWindowGapAtPeak refuses a load, or a day plan's bucket, read beside a
// gap that could hide busier minutes than any the window holds: a gap at
// least RollingWindow long borders the window's busiest stretch (GapAt).
const ReasonWindowGapAtPeak = "WindowGapAtPeak"

// GapAt is, in words, a gap that could hide the window's peak: one at least
// RollingWindow long, so that it could hold a busiest stretch of its own,
// with a cycle either side of it in [from, to], the busiest stretch the
// window shows ("15m0s from 02:42 to 02:58Z"; the longest such gap). Every
// gap that long counts, not only the largest. "" when none borders it, and
// on a nil window. It reads gaps WindowGapsOf kept, which JSON does not
// carry: on a WindowGaps decoded from JSON it is always "".
func (g *WindowGaps) GapAt(from, to time.Time) string {
	if g == nil {
		return ""
	}
	in := func(t time.Time) bool { return !t.Before(from) && !t.After(to) }
	var at *gapSpan
	for i, s := range g.long {
		if (in(s.from) || in(s.to)) && (at == nil || s.seconds > at.seconds) {
			at = &g.long[i]
		}
	}
	if at == nil {
		return ""
	}
	return at.words()
}

// Text is the gaps in words for a window that misses cycles, "" for one that
// misses none: "289 cycles expected, 229 present (21% missing), largest gap
// 15m0s from 02:42 to 02:58Z", the times being the cycles either side of it.
func (g *WindowGaps) Text() string {
	if g == nil || g.ExpectedCycles == g.PresentCycles {
		return ""
	}
	s := fmt.Sprintf("%d cycles expected, %d present (%.0f%% missing)", g.ExpectedCycles, g.PresentCycles, 100*g.MissingShare)
	if g.LargestGapFrom != nil {
		s += ", largest gap " + gapSpan{from: *g.LargestGapFrom, to: *g.LargestGapTo, seconds: g.LargestGapSeconds}.words()
	}
	if g.Flagged {
		s += " " + FlagWindowGaps
	}
	return s
}
