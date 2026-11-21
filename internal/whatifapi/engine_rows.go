package whatifapi

import (
	"fmt"
	"slices"
	"strings"
)

// EngineRateTolerance is how far under the router's rate the request rate
// the engine rows report may be over a window before the rows are taken not
// to stand for the fleet (EngineRowsShort): 20%, wider than a window's edges
// and a replica starting or draining move it. Rows reading over the router
// are not refused: rows that leave ranks out can only read under it, and
// traffic sent to the Pods past the router (a warm-up, an adapter named
// apart from its base model) reads over it.
const EngineRateTolerance = 0.2

// MinRowsPerReplica is the least ratio of a role's engine rows to its
// serving replicas, in the median cycle of those that state replicas,
// before the rows are taken not to stand for the fleet (EngineRowsShort): a replica is one engine row, and
// fewer is a replica whose engine the recording did not read.
const MinRowsPerReplica = 0.75

// minEngineRateCycles is the fewest cycles with both rates read that
// EngineRowsShort compares: fewer say nothing of a window.
const minEngineRateCycles = 4

// EngineRowsShort returns why the engine rows of series do not stand for the
// fleet, or "" where they do (or nothing says otherwise). The planner reads a
// replica as its engine rows: their request rate, their count. On a fleet of
// multi-Pod groups whose rows report one rank (one Pod) of several, the rows
// add up to a fraction of the router's rate and count a fraction of the
// replicas, so every per-replica rate and every knee in request units reads
// low by the ranks left out (issue
// llm-d-incubation/llm-d-scaling-manager#331). Two checks, either refuses:
//
//   - the rows' summed request rate (SeriesSample.EngineRate) against the
//     router's, over the cycles that read both and whose router queue held
//     nothing (a queue's arrivals run ahead of what the engines complete),
//     more than EngineRateTolerance under it;
//   - a role's serving rows against the serving replicas its variants
//     state, the median over the cycles that state them, under
//     MinRowsPerReplica.
func EngineRowsShort(series []SeriesSample) string {
	var why []string
	var engine, router float64
	n := 0
	for _, s := range series {
		if s.RateKnown && s.Rate > 0 && s.EngineRate > 0 && !s.RouterQueued {
			engine += s.EngineRate
			router += s.Rate
			n++
		}
	}
	if n >= minEngineRateCycles {
		if r := engine / router; r < 1-EngineRateTolerance {
			why = append(why, fmt.Sprintf("the engine rows report %.3g r/s against the router's %.3g r/s (%.0f%%)",
				engine/float64(n), router/float64(n), 100*r))
		}
	}
	type count struct {
		ratio          float64
		rows, replicas int
	}
	for _, role := range []struct {
		name string
		of   func(SeriesSample) (rows, replicas int)
	}{
		{"prefill", func(s SeriesSample) (int, int) { return s.Prefill, s.PrefillReplicas }},
		{"decode", func(s SeriesSample) (int, int) { return s.Decode, s.DecodeReplicas }},
	} {
		var counts []count
		for _, s := range series {
			if r, m := role.of(s); m > 0 {
				counts = append(counts, count{float64(r) / float64(m), r, m})
			}
		}
		if len(counts) == 0 {
			continue
		}
		slices.SortStableFunc(counts, func(a, b count) int {
			switch {
			case a.ratio < b.ratio:
				return -1
			case a.ratio > b.ratio:
				return 1
			}
			return 0
		})
		if med := counts[len(counts)/2]; med.ratio < MinRowsPerReplica {
			why = append(why, fmt.Sprintf("%d %s engine rows stand for %d serving replicas (the median cycle)", med.rows, role.name, med.replicas))
		}
	}
	if len(why) == 0 {
		return ""
	}
	return "the engine rows do not stand for the fleet: " + strings.Join(why, "; ") +
		", as a multi-Pod group's rows do when they report one rank of several"
}
