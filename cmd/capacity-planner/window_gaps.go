package main

import (
	"fmt"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// rollingCycle is the cycle time the busiest stretch is counted in: the
// mean time between the state's cycles (cycleSeconds), or the window's
// median one (WindowGaps.CycleSeconds) where it misses cycles, since across
// a gap the mean reads long and the stretch would hold too few cycles.
func rollingCycle(st *learnreplay.State, g *whatifapi.WindowGaps) float64 {
	if g != nil && g.ExpectedCycles > g.PresentCycles {
		return g.CycleSeconds
	}
	return cycleSeconds(st)
}

// busiestStretch is the last index of the k consecutive rates of the highest
// mean, idle and failed reads counting as 0, and that mean; -1 with fewer
// than k rates. The first of equal stretches is kept.
func busiestStretch(rates []float64, k int) (int, float64) {
	if k < 1 || len(rates) < k {
		return -1, 0
	}
	end, peak := -1, 0.0
	var sum float64
	for i, x := range rates {
		if whatif.Finite(x) && x > 0 {
			sum += x
		}
		if i >= k {
			if y := rates[i-k]; whatif.Finite(y) && y > 0 {
				sum -= y
			}
		}
		if i >= k-1 && (end < 0 || sum/float64(k) > peak) {
			end, peak = i, sum/float64(k)
		}
	}
	return end, peak
}

// gapAtPeak refuses the observed load when a gap in the window could hide
// its peak (WindowGaps.GapAt): a gap at least RollingWindow long whose cycle
// on either side lies in the busiest stretch (rolling, the busiest
// RollingWindow of consecutive cycles) or is the cycle of the highest rate.
// The load is read from the cycles present, and minutes busier than any of
// them may lie in the gap. Nil when no gap does.
func gapAtPeak(rates []float64, times []time.Time, g *whatifapi.WindowGaps, rolling *whatifapi.ReportRolling) *whatif.Unanswerable {
	if g == nil || len(times) != len(rates) || len(rates) == 0 {
		return nil
	}
	var gap, at string
	if rolling != nil {
		if end, _ := busiestStretch(rates, rolling.Cycles); end >= 0 {
			first := end - rolling.Cycles + 1
			if gap = g.GapAt(times[first], times[end]); gap != "" {
				at = fmt.Sprintf("the busiest %s (%s to %sZ)", time.Duration(rolling.WindowSeconds*float64(time.Second)).Round(time.Second),
					times[first].UTC().Format("15:04"), times[end].UTC().Format("15:04"))
			}
		}
	}
	if gap == "" {
		top := -1
		for i, x := range rates {
			if whatif.Finite(x) && x > 0 && (top < 0 || x > rates[top]) {
				top = i
			}
		}
		if top >= 0 {
			if gap = g.GapAt(times[top], times[top]); gap != "" {
				at = fmt.Sprintf("the busiest cycle (%sZ)", times[top].UTC().Format("15:04"))
			}
		}
	}
	if gap == "" {
		return nil
	}
	return &whatif.Unanswerable{Reason: whatif.Reason(whatifapi.ReasonWindowGapAtPeak),
		Detail: fmt.Sprintf("a gap in the window, %s, borders %s: the load in the gap was not read and may have been higher; "+
			"capture the window again, or state the load with --load-seen (API loadSeen)", gap, at)}
}
