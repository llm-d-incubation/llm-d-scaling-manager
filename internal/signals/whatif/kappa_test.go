package whatif

import (
	"math"
	"testing"
)

// kappaOf is an answer's kappa, failing where it has none.
func kappaOf(t *testing.T, what string, a Answer) float64 {
	t.Helper()
	if a.Basis.Kappa == nil {
		t.Fatalf("%s: no kappa", what)
	}
	return *a.Basis.Kappa
}

// elasticity is d ln f / d ln A at d's slope, by a central difference.
func elasticity(t *testing.T, d Decode, f func(Decode) Result) float64 {
	t.Helper()
	const h = 1e-6
	up, down := d, d
	up.Line.A *= 1 + h
	down.Line.A *= 1 - h
	hi, lo := mustAnswer(t, f(up)).Value, mustAnswer(t, f(down)).Value
	return (math.Log(hi) - math.Log(lo)) / (math.Log(1+h) - math.Log(1-h))
}

// Kappa is g/(1-g) on [0, 1), and nothing past it.
func TestKappa(t *testing.T) {
	for _, c := range []struct{ g, want float64 }{{0, 0}, {0.5, 1}, {0.8, 4}, {0.86, 0.86 / 0.14}} {
		got, ok := Kappa(c.g)
		if !ok {
			t.Fatalf("g %g: no kappa", c.g)
		}
		near(t, "kappa", got, c.want, 1e-12)
	}
	for _, g := range []float64{1, 1.2, -0.1, math.NaN(), math.Inf(1)} {
		if k, ok := Kappa(g); ok {
			t.Errorf("g %g: kappa %g, want none", g, k)
		}
	}
	// The text's note starts where DecodeLoopGain's rule does: g = 0.8.
	if k, _ := Kappa(0.8); math.Abs(k-KappaNoteAbove) > 1e-12 {
		t.Errorf("kappa at g 0.8 = %g, want KappaNoteAbove %g", k, KappaNoteAbove)
	}
}

// kappa is what it claims to be: the fill's elasticity to the line's slope,
// d ln k / d ln A, and the mean TPOT's, where the TPOT is the line's.
func TestKappa_IsTheElasticityToTheSlope(t *testing.T) {
	d := smallIntercept()
	for _, g := range []float64{0.7, 0.81, 0.86} {
		rate := rateAtGain(d, g)
		fill := mustAnswer(t, FillAtRate(d, rate))
		kappa := kappaOf(t, "fill", fill)
		near(t, "kappa against the gain", kappa, g/(1-g), 1e-9)
		near(t, "fill elasticity", elasticity(t, d, func(d Decode) Result { return FillAtRate(d, rate) }), kappa, 1e-5*kappa)
		tpot := func(d Decode) Result { return SLOAtReplicas(d, ReplicasQuery{Rate: rate}).MeanTPOT }
		near(t, "TPOT elasticity", elasticity(t, d, tpot), kappa, 1e-5*kappa)
	}
}

// Every forward figure carries kappa: the knee at its own rate, the fill,
// the operating rate and both TPOTs at theirs. The inverse one does not.
func TestKappa_OnEveryForwardFigure(t *testing.T) {
	d := smallIntercept()
	slo := SLOAtReplicas(d, ReplicasQuery{Headroom: 0.85, PercentileTarget: &TPOTTarget{Percentile: 0.9, Ratio: TPOTRatio{Value: 1.3}}})
	knee := mustAnswer(t, slo.KneeRate)
	rate := mustAnswer(t, slo.Rate)
	at := kappaOf(t, "operating rate", rate)
	near(t, "operating rate against its gain", at, d.gainAt(rate.Value)/(1-d.gainAt(rate.Value)), 1e-12)
	near(t, "mean TPOT", kappaOf(t, "mean TPOT", mustAnswer(t, slo.MeanTPOT)), at, 0)
	near(t, "TPOT p90", kappaOf(t, "TPOT p90", mustAnswer(t, slo.PercentileTPOT)), at, 0)
	near(t, "fill", kappaOf(t, "fill", mustAnswer(t, FillAtRate(d, rate.Value))), at, 0)
	kneeKappa := kappaOf(t, "knee", knee)
	near(t, "knee against its gain", kneeKappa, d.gainAt(knee.Value)/(1-d.gainAt(knee.Value)), 1e-12)
	near(t, "KneeRate", kappaOf(t, "KneeRate", mustAnswer(t, KneeRate(d))), kneeKappa, 0)
	if !(kneeKappa > at) {
		t.Errorf("knee kappa %g, want more than the operating rate's %g: the knee is the most any rate under it reaches", kneeKappa, at)
	}
	// Each figure holds its own copy.
	if slo.MeanTPOT.(Answer).Basis.Kappa == rate.Basis.Kappa {
		t.Error("the mean TPOT shares the operating rate's kappa pointer")
	}
	inverse := mustAnswer(t, RateForTPOT(d, RateQuery{Target: TPOTTarget{Seconds: 0.02}, Headroom: 0.85}))
	if inverse.Basis.Kappa != nil {
		t.Errorf("RateForTPOT carries kappa %g: an inverse figure is not amplified by the loop", *inverse.Basis.Kappa)
	}
}
