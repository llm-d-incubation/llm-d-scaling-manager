package whatif

import (
	"fmt"
	"math"
	"testing"
)

// burstyCVs are the prompt spreads the arrivals' properties are held at: either
// side of 1, where the tail changes form, and the top a caller passes.
var burstyCVs = []float64{0, 0.5, 1, 2, 3}

// burstyCa2s is Ca^2 from Poisson up, with the step just past 1 that the old
// tail jumped at.
var burstyCa2s = []float64{1, 1.0001, 1.5, 2, 2.5, 3, 4, 6, 8}

// flowControlled is a fleet with the gate on, at the upstream thresholds.
func flowControlled(fl TTFTFleet) TTFTFleet {
	fl.FlowControl = FlowControl{State: FlowControlOn, QueueThreshold: FlowControlQueueThreshold, KVThreshold: FlowControlKVThreshold}
	return fl
}

// burstyFleets are the fleets the properties run on, each at a prompt spread.
func burstyFleets(cv float64) map[string]TTFTFleet {
	worked := workedFleet()
	worked.Prefill.PromptCV = cv
	return map[string]TTFTFleet{"prefill alone": prefillOnly(cv), "worked": worked, "gated": flowControlled(worked)}
}

// lowerKnee is the rate a fleet saturates at: its lower role's.
func lowerKnee(t *testing.T, fl TTFTFleet) float64 {
	t.Helper()
	knee := mustAnswer(t, PrefillKnee(fl.Prefill)).Value
	if fl.Decode != nil {
		knee = min(knee, mustAnswer(t, KneeRate(*fl.Decode)).Value)
	}
	return knee
}

// A burstier arrival never prices better. At every prompt spread, TTFT never
// falls as Ca^2 rises from 1 to 8, at the mean and in the tail, from a light
// load to near the knee, and a refusal past an onset promises nothing.
func TestProperty_TTFTNeverFallsWithCa2(t *testing.T) {
	for _, cv := range burstyCVs {
		for name, base := range burstyFleets(cv) {
			knee := lowerKnee(t, base)
			for _, q := range []float64{0, 0.5, 0.9, 0.95, 0.99} {
				for _, frac := range []float64{0.1, 0.3, 0.5, 0.7, 0.85, 0.95} {
					prev, compared := 0.0, 0
					for _, ca2 := range burstyCa2s {
						fl := base
						fl.Prefill.ArrivalCV2 = ca2
						v, ok := ttftAt(t, fl, frac*knee, q).(TTFT)
						if !ok {
							prev = math.Inf(1)
							continue
						}
						compared++
						if !(v.Seconds >= prev*(1-1e-9)) {
							t.Errorf("%s cv %g q %g at %.2f of the knee: TTFT %g at Ca^2 %g, after %g", name, cv, q, frac, v.Seconds, ca2, prev)
						}
						prev = v.Seconds
					}
					if compared == 0 && name != "gated" {
						t.Errorf("%s cv %g q %g at %.2f of the knee: nothing priced", name, cv, q, frac)
					}
				}
			}
		}
	}
}

// The rate at a target never rises with Ca^2, and a refusal is a rate of 0.
func TestProperty_RateNeverRisesWithCa2(t *testing.T) {
	for _, cv := range burstyCVs {
		for name, base := range burstyFleets(cv) {
			for _, q := range []float64{0, 0.9, 0.95, 0.99} {
				zero := mustTTFT(t, ttftAt(t, base, 1e-6, q))
				for _, over := range []float64{1.2, 1.5, 2, 3, 6} {
					target := TTFTTarget{Seconds: over * zero.Seconds, Percentile: q}
					prev, answered := math.Inf(1), 0
					for _, ca2 := range burstyCa2s {
						fl := base
						fl.Prefill.ArrivalCV2 = ca2
						rate := 0.0
						if a, ok := rateForTTFT(t, fl, target).(Answer); ok {
							rate = a.Value
							answered++
						}
						if !(rate <= prev*(1+1e-6)) {
							t.Errorf("%s cv %g q %g target %.4g: %g r/s at Ca^2 %g, after %g", name, cv, q, target.Seconds, rate, ca2, prev)
						}
						prev = rate
					}
					if answered == 0 {
						t.Errorf("%s cv %g q %g target %.4g: no rate answered", name, cv, q, target.Seconds)
					}
				}
			}
		}
	}
}

// Wait and rate are continuous across Ca^2 1, from both sides: the step that
// left the Poisson branch is gone.
func TestProperty_ContinuousAcrossPoisson(t *testing.T) {
	for _, cv := range burstyCVs {
		for name, base := range burstyFleets(cv) {
			knee := lowerKnee(t, base)
			at := func(ca2 float64) (TTFT, float64) {
				fl := base
				fl.Prefill.ArrivalCV2 = ca2
				v := mustTTFT(t, ttftAt(t, fl, 0.4*knee, 0.95))
				a := mustAnswer(t, rateForTTFT(t, fl, TTFTTarget{Seconds: 2 * v.Floor, Percentile: 0.95}))
				return v, a.Value
			}
			poisson, poissonRate := at(1)
			for _, ca2 := range []float64{1 - 1e-7, 1 + 1e-7} {
				v, rate := at(ca2)
				relNear(t, fmt.Sprintf("%s cv %g Ca^2 %g TTFT", name, cv, ca2), v.Seconds, poisson.Seconds, 1e-5)
				relNear(t, fmt.Sprintf("%s cv %g Ca^2 %g rate", name, cv, ca2), rate, poissonRate, 1e-4)
			}
		}
	}
}

// Above 1 no decay beats Poisson's: that was the bug at cv above 1, where
// Kingman's 1/theta is faster than the Cramer-Lundberg root.
func TestBurstyTailNeverOutrunsPoisson(t *testing.T) {
	for _, cv := range burstyCVs {
		for _, rho := range []float64{0.2, 0.5, 0.8} {
			fl := prefillOnly(cv)
			es := fl.Prefill.PromptTokens / fl.Prefill.VP
			poisson := mustTTFT(t, ttftAt(t, fl, rho/es, 0.99))
			for _, ca2 := range burstyCa2s {
				fl.Prefill.ArrivalCV2 = ca2
				v := mustTTFT(t, ttftAt(t, fl, rho/es, 0.99))
				if !(v.WaitScale >= poisson.WaitScale*(1-1e-12)) {
					t.Errorf("cv %g rho %g Ca^2 %g: wait scale %g, under Poisson's %g", cv, rho, ca2, v.WaitScale, poisson.WaitScale)
				}
			}
		}
	}
}
