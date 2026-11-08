package itl

import (
	"cmp"
	"slices"
)

// WriteTimeBase is the time base of a reading on a learner-write line: the
// replica's ITL is a rate over the collector's ITL window
// (registration.ITLWindow), and its k the KV fill over the same minute when
// the collector read that (domain.ReplicaMetrics.KvUsageForITL).
const WriteTimeBase = "rate[1m]"

// WriteMaxPods is how many pods one learner-write line lists, lowest reading
// first; the rest are counted in podsOmitted, so a line stays under the CRI
// line limit however large the fleet. Every analyzer's learner-write lines,
// k2's and mu's as well as the ITL line's, take this one bound.
const WriteMaxPods = 16

// fitPods is the replicas obs came from, by pod name, each with how many of
// obs were its and their mean k and ITL. Nil when obs is empty.
func fitPods(obs []Observation) []FitPod {
	if len(obs) == 0 {
		return nil
	}
	by := map[string]*FitPod{}
	for _, o := range obs {
		p, ok := by[o.pod]
		if !ok {
			p = &FitPod{Pod: o.pod}
			by[o.pod] = p
		}
		p.Rows++
		p.K += o.K
		p.ITLSec += o.ITLSec
	}
	out := make([]FitPod, 0, len(by))
	for _, p := range by {
		p.K /= float64(p.Rows)
		p.ITLSec /= float64(p.Rows)
		out = append(out, *p)
	}
	slices.SortFunc(out, func(x, y FitPod) int { return cmp.Compare(x.Pod, y.Pod) })
	return out
}

// WritePods is a learner-write line's pods for the ITL line: the replicas
// whose readings the line was fitted from this cycle (FitPods), at most
// maxPods of them, lowest mean ITL first, each with its mean ITL as its
// reading, its mean k, how many of the fit's readings were its, and the time
// base; and how many were left out. A held slope, which fitted nothing this
// cycle, lists none.
func (r Reading) WritePods(maxPods int) ([]map[string]any, int) {
	ps := slices.Clone(r.FitPods)
	slices.SortStableFunc(ps, func(x, y FitPod) int {
		if c := cmp.Compare(x.ITLSec, y.ITLSec); c != 0 {
			return c
		}
		return cmp.Compare(x.Pod, y.Pod)
	})
	omitted := max(len(ps)-max(maxPods, 0), 0)
	ps = ps[:len(ps)-omitted]
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		out = append(out, map[string]any{
			"pod": p.Pod, "reading": p.ITLSec, "k": p.K, "rows": p.Rows, "timeBase": WriteTimeBase,
		})
	}
	return out, omitted
}

// AdmissionMargins is a learner-write line's admission tests for the ITL line
// with their margins, as logger key-value pairs: the full fit's gate, the
// steady readings' spread in k against DefaultMinKSpread and their count
// against DefaultMinSamples, each with how far over its threshold it is
// (negative: the gate was not passed, and the line is the pinned fit or a
// held slope), and how many readings the line was fitted from this cycle.
func (r Reading) AdmissionMargins() []any {
	return []any{
		"kSpread", r.SteadySpread, "kSpreadThreshold", DefaultMinKSpread,
		"kSpreadOverThreshold", r.SteadySpread - DefaultMinKSpread,
		"steadyHeldThreshold", DefaultMinSamples, "steadyHeldOverThreshold", r.SteadyHeld - DefaultMinSamples,
		"fitRows", r.FitRows,
	}
}
