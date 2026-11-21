package main

import (
	"fmt"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The load's shape (issue #294): the load is judged at a statistic of the
// window's per-cycle rates, read at some of its cycles, while a measured
// shape is the decode variant's latest cycles with traffic. When the mix
// moved in the window the two are of different traffic, and the load is
// priced at a shape it never ran at. The report prints the shape of the
// load's own cycles beside the shape priced, flags a measured shape more
// than ShapeLoadTolerance off it, and prices it on asking (--shape load).

// loadCycles is the cycles the load judged at was read from, numbered from
// 1: for the p95 (and the p95 less its noise) every cycle at or above the
// p95, for the peak every cycle at it, for the mean every cycle with
// traffic. Nil with a stated load or none.
func loadCycles(rates []float64, stat string, stats map[string]float64) []int {
	var floor float64
	switch stat {
	case whatifapi.LoadMean:
		floor = 0
	case whatifapi.LoadP95, whatifapi.LoadP95Modulation:
		floor = stats[whatifapi.LoadP95]
	case whatifapi.LoadPeak:
		floor = stats[whatifapi.LoadPeak]
	default:
		return nil
	}
	var out []int
	for i, x := range rates {
		if whatif.Finite(x) && x > 0 && x >= floor {
			out = append(out, i+1)
		}
	}
	return out
}

// loadShapeOf is the shape of the cycles of series that cycles names, each
// weighted by its request rate (alike where none read one); nil when none of
// them read a shape.
func loadShapeOf(series []learnreplay.CycleShape, cycles []int, stat string) *whatifapi.ReportLoadShape {
	want := make(map[int]bool, len(cycles))
	for _, c := range cycles {
		want[c] = true
	}
	var at, read []learnreplay.CycleShape
	var total float64
	for _, x := range series {
		if want[x.Cycle] {
			at = append(at, x)
			if x.RequestRate > 0 {
				read = append(read, x)
				total += x.RequestRate
			}
		}
	}
	// Weighted by request rate over the cycles that read one; alike where
	// none did.
	if len(read) > 0 {
		at = read
	}
	if len(at) == 0 {
		return nil
	}
	out := &whatifapi.ReportLoadShape{Statistic: stat, Cycles: len(at), FromCycle: at[0].Cycle, ToCycle: at[len(at)-1].Cycle}
	for _, x := range at {
		w := 1 / float64(len(at))
		if total > 0 {
			w = x.RequestRate / total
		}
		out.InputTokens += w * x.InputTokens
		out.OutputTokens += w * x.OutputTokens
		out.PrefixHitRate += w * x.PrefixHitRate
	}
	return out
}

// decodeShapeSeries is the shape series of the decode variant priced: the
// one named, or with none named (POST /v1/report on a model of one decode
// variant), the model's only variant that is not prefill.
func (r *reporter) decodeShapeSeries() []learnreplay.CycleShape {
	var only []learnreplay.CycleShape
	n := 0
	for _, v := range r.st.Variants {
		if r.s.decodeVariant != "" && v.Variant == r.s.decodeVariant {
			return v.ShapeSeries
		}
		if domain.CanonicalRole(v.Role) != domain.RolePrefill {
			only, n = v.ShapeSeries, n+1
		}
	}
	if r.s.decodeVariant == "" && n == 1 {
		return only
	}
	return nil
}

// resolveLoadShapes fills each shape asked as the load's (--shape load) with
// the load's cycles' lengths; an input error when there are none to take.
func (r *reporter) resolveLoadShapes() error {
	for i, sh := range r.q.Shapes {
		if !sh.Load {
			continue
		}
		ls := r.loadShape
		if ls == nil {
			var why string
			switch r.loadStat {
			case loadStated:
				why = "the load was stated (--load-seen), so it was read at no cycle"
			case "":
				why = "the source saw no traffic, so there is no load to read a shape at"
			default:
				why = "no cycle the load was read from read a shape"
			}
			return whatifapi.InputErrorf("shape %s: %s; state the lengths (IL/OL)", whatifapi.ShapeLoad, why)
		}
		hit := ls.PrefixHitRate
		r.q.Shapes[i].IL, r.q.Shapes[i].OL, r.q.Shapes[i].Hit = ls.InputTokens, ls.OutputTokens, &hit
	}
	return nil
}

// shapeLoad sets rs's load shape, and flags a measured shape that differs
// from it by more than the tolerance (FlagShapeLoadMismatch). A stated shape
// is shown beside it but never flagged: the question chose it.
func (r *reporter) shapeLoad(rs *whatifapi.ReportShape) {
	if r.loadShape == nil {
		return
	}
	ls := *r.loadShape
	ls.Mismatch = r.sh.Measured && whatifapi.ShapeLoadMismatch(rs.InputTokens, rs.OutputTokens, ls)
	rs.LoadShape = &ls
	if ls.Mismatch {
		rs.Flags = append(rs.Flags, whatifapi.FlagShapeLoadMismatch)
	}
}

// statWords is a load statistic in words: p95-modulation is "p95 less
// counting noise", the others their names.
func statWords(stat string) string {
	if stat == whatifapi.LoadP95Modulation {
		return "p95 less counting noise"
	}
	return stat
}

// loadShapeText is a shape's load shape line, "" with none.
func loadShapeText(s whatifapi.ReportShape) string {
	ls := s.LoadShape
	if ls == nil {
		return ""
	}
	which := map[string]string{whatifapi.LoadMean: "every cycle with traffic", whatifapi.LoadPeak: "the peak",
		whatifapi.LoadP95: "the p95 and above", whatifapi.LoadP95Modulation: "the p95's cycles, the p95 and above"}[ls.Statistic]
	line := fmt.Sprintf("  the load's cycles (%s: %d, cycles %d-%d): IL %s OL %s, prefix hit %.2f", which, ls.Cycles, ls.FromCycle, ls.ToCycle,
		num(round(ls.InputTokens)), num(round(ls.OutputTokens)), ls.PrefixHitRate)
	if ls.Mismatch {
		line += fmt.Sprintf("\n  SHAPE/LOAD MISMATCH: the load is priced at a shape more than %.0f%% off the traffic that made it; --shape load prices it at its own",
			100*whatifapi.ShapeLoadTolerance)
	}
	return line + "\n"
}
