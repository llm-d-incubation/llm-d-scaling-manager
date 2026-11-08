package whatif

// The sensitivity sweep (docs/proposals/slo-capacity-planner.md, "Uncertainty"):
// V_P from a peak and f are extreme statistics, so a bootstrap says nothing of
// them, and they get a sweep of SensitivityShare either way instead. The sweep
// is the figure at each corner: it says how far the answer moves on those two
// inputs, and is no confidence interval.

// SensitivityShare is how far either way V_P and f are swept.
const SensitivityShare = 0.15

// Sensitivity is a figure's span over the sweep's corners, the figure itself
// included. Refused is the first refusal a corner gave, "" with none: the span
// then says nothing on the side that corner moves to, which is always the
// worse one, and a caller prints the reason there.
type Sensitivity struct {
	Lo, Hi  float64
	Refused Reason
}

// SensitivityFleets is fl at each corner of the sweep: V_P either way, with f
// either way too. f is held to its bounds, at least 1 and at most the pods. One
// pod has one f, exactly 1, so it has only V_P's two corners; so has an f of 0,
// which is no reading to sweep.
func SensitivityFleets(fl TTFTFleet) []TTFTFleet {
	var out []TTFTFleet
	p := fl.Prefill
	for _, vp := range []float64{p.VP * (1 - SensitivityShare), p.VP * (1 + SensitivityShare)} {
		if p.Replicas < 2 || p.Imbalance < 1 {
			at := fl
			at.Prefill.VP = vp
			out = append(out, at)
			continue
		}
		for _, f := range []float64{p.Imbalance * (1 - SensitivityShare), p.Imbalance * (1 + SensitivityShare)} {
			at := fl
			at.Prefill.VP = vp
			at.Prefill.Imbalance = min(max(f, 1), float64(p.Replicas))
			out = append(out, at)
		}
	}
	return out
}

// SensitivitySpan is the span of value over the corners' results. A corner
// that is no number gives Refused.
func SensitivitySpan(value float64, corners []Result) *Sensitivity {
	s := &Sensitivity{Lo: value, Hi: value}
	for _, r := range corners {
		switch v := r.(type) {
		case Answer:
			s.Lo, s.Hi = min(s.Lo, v.Value), max(s.Hi, v.Value)
		case TTFT:
			s.Lo, s.Hi = min(s.Lo, v.Seconds), max(s.Hi, v.Seconds)
		case Unanswerable:
			if s.Refused == "" {
				s.Refused = v.Reason
			}
		}
	}
	return s
}
