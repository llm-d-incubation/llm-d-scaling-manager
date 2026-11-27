package whatif

import (
	"math"
	"time"
)

// HeldOutScore is a model's mean TTFT scored on held-out steps: Within the
// steps whose predicted mean is within the tolerance of the measured one
// (|predicted/measured - 1| <= tolerance), Low those read more than the
// tolerance low, of Steps scored; MedianRatio the median of predicted over
// measured; RateLo and RateHi the lowest and highest rate scored.
type HeldOutScore struct {
	Within, Low, Steps int
	MedianRatio        float64
	RateLo, RateHi     float64
}

// RankMixtureHeldOut scores the model on the self-check folds FitRankMixture
// cuts its fit window into (half to three quarters, three quarters to the
// end): each fold's core is fitted on the steps before its cut alone, priced
// at spread, its A0 fitted on those same steps, and its mean TTFT compared
// with the router's measured mean on each step after the cut. The spread is
// the window's (RankMixture.Spread), which the folds' p90 calibrated, so the
// p90 is not scored here; the mean scored is never fitted on the steps it is
// scored on. A fold that does not fit scores nothing.
func RankMixtureHeldOut(steps []MixtureStep, o RankMixtureOptions, spread, tolerance float64) HeldOutScore {
	return heldOutFolds(steps, tolerance, func(train []MixtureStep) (RankMixture, float64, bool) {
		fm, _, err := FitCore(train, o)
		if err != nil {
			return fm, 0, false
		}
		fm.Spread = spread
		a0, ok := a0Of(drawSteps(fm, train), spread)
		return fm, a0, ok
	})
}

// RankMixtureNLHeldOut is RankMixtureHeldOut for QueueRankMixtureNLLine: on
// the same folds, each fold's model is fitted with FitRankMixtureNL on the
// steps before its cut alone and priced as fitted, at its own A0 and
// Spread 1, since the variant calibrates nothing; its mean TTFT is compared
// with the router's measured mean on each step after the cut.
func RankMixtureNLHeldOut(steps []MixtureStep, o RankMixtureOptions, tolerance float64) HeldOutScore {
	return heldOutFolds(steps, tolerance, func(train []MixtureStep) (RankMixture, float64, bool) {
		fm, _, err := FitRankMixtureNL(train, o)
		return fm, fm.A0, err == nil
	})
}

// heldOutFolds scores, on each self-check fold, the model fit returns from
// the fold's steps before its cut, priced at its A0 and at Spread times
// its Beta, on the fold's steps after the cut. A fold fit refuses (ok
// false) scores nothing.
func heldOutFolds(steps []MixtureStep, tolerance float64, fit func(train []MixtureStep) (m RankMixture, a0 float64, ok bool)) HeldOutScore {
	var out HeldOutScore
	if len(steps) < 8 {
		return out
	}
	start := steps[0].At.Add(-1)
	span := steps[len(steps)-1].At.Sub(start)
	cut := func(f float64) time.Time { return start.Add(time.Duration(f * float64(span))) }
	var ratios []float64
	for _, c := range [][3]float64{{0, 0.5, 0.75}, {0, 0.75, 1}} {
		train := stepsBetween(steps, cut(c[0]), cut(c[1]))
		test := stepsBetween(steps, cut(c[1]), cut(c[2]))
		fm, a0, ok := fit(train)
		if !ok {
			continue
		}
		spread := fm.Spread
		for i, p := range drawSteps(fm, test) {
			if p.priceError || !(p.mean > 0) {
				continue
			}
			m, _ := p.d.Stats(a0, spread*p.beta)
			if !Finite(m) {
				continue
			}
			r := m / p.mean
			ratios = append(ratios, r)
			switch {
			case math.Abs(r-1) <= tolerance:
				out.Within++
			case r < 1:
				out.Low++
			}
			rate := test[i].Rate
			if out.Steps == 0 || rate < out.RateLo {
				out.RateLo = rate
			}
			out.RateHi = max(out.RateHi, rate)
			out.Steps++
		}
	}
	if len(ratios) > 0 {
		out.MedianRatio = medianOf(ratios)
	}
	return out
}
