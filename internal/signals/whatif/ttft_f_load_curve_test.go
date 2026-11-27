package whatif

import (
	"testing"
)

// curvePrefill is loadAwarePrefill's fleet, f held at 1.05, with f against
// the load f(u) = 0.97 + 520/u (the lab's offline fit; the planner's own
// read 0.96 + 591/u, issue llm-d-incubation/llm-d-scaling-manager#482)
// when curve is set: three pods
// at V_P 30,000 tok/s and 1000-token prompts, so u is rate * 1000 / 3.
func curvePrefill(curve bool) Prefill {
	p := loadAwarePrefill(false, DefaultVPMargin)
	p.Imbalance = 1.05
	if curve {
		p.ImbalanceCurveFloor, p.ImbalanceCurveExcess = 0.97, 520
	}
	return p
}

// f at a rate is the higher of the held f and the curve's at the rate's
// load, at most the pods: 1.75 at 2 r/s (u 667), the held 1.05 at 30 r/s
// (u 10,000, where the curve reads 1.022), the pods at no load. Without the
// curve it is the held f at every rate.
func TestFLoadCurve_FIsTheHigherOfTheTwo(t *testing.T) {
	on, _ := curvePrefill(true).check()
	off, _ := curvePrefill(false).check()
	relNear(t, "f at 2 r/s", on.fAt(2), 0.97+520/(2*1000/3.0), 1e-12)
	relNear(t, "f at 30 r/s", on.fAt(30), 1.05, 1e-12)
	relNear(t, "f at no load", on.fAt(0), 3, 1e-12)
	relNear(t, "f at 0.1 r/s, held to the pods", on.fAt(0.1), 3, 1e-12)
	for _, rate := range []float64{0, 2, 30} {
		if got := off.fAt(rate); got != 1.05 {
			t.Errorf("off: f at %g r/s = %g, want 1.05", rate, got)
		}
	}
}

// The knee is the lower of the held one and the curve's root of
// f(u) rho = 1 - margin: rho = (1 - margin - delta/V_P)/f_inf. With f_inf
// under the held f the held knee is lower and stands, to the bit; with
// f_inf above it the curve's root is the knee, and f(u) rho there is
// 1 - margin.
func TestFLoadCurve_TheKneeIsTheLowerRoot(t *testing.T) {
	held := kneeOf(t, curvePrefill(false))
	if got := kneeOf(t, curvePrefill(true)); got != held {
		t.Errorf("knee with f_inf 0.97 under f 1.05: %.9g, want the held %.9g", got, held)
	}

	p := curvePrefill(true)
	p.ImbalanceCurveFloor = 1.04
	p.ImbalanceCurveExcess = 3000
	k := kneeOf(t, p)
	rho := (1 - DefaultVPMargin - 3000/30000.0) / 1.04
	relNear(t, "curve knee", k, 90*rho, 1e-12)
	if !(k < held) {
		t.Errorf("curve knee %.6g, want under the held %.6g", k, held)
	}
	pl, _ := p.check()
	relNear(t, "f(u) rho at the knee", pl.fAt(k)*k/90, 1-DefaultVPMargin, 1e-9)
}

// The curve never prices a TTFT under the held f's, and the TTFT still
// rises with the rate up to the knee, with each queue riseProblem sweeps:
// f(u) rho = (f_inf u + delta)/V_P rises with the load.
func TestFLoadCurve_TTFTRisesWithTheRate(t *testing.T) {
	for name, c := range sweepFleets() {
		on, off := c[1], c[1]
		on.Prefill.ImbalanceCurveFloor, on.Prefill.ImbalanceCurveExcess = 0.97, 520
		k := kneeOf(t, on.Prefill)
		prev := 0.0
		for i := 1; i < 200; i++ {
			rate := k * float64(i) / 200
			a := mustTTFT(t, TTFTAt(on, rate, 0.9)).Seconds
			if !(a >= prev) {
				t.Errorf("%s: TTFT p90 falls at %.4g r/s: %.6g after %.6g", name, rate, a, prev)
				break
			}
			prev = a
			if b, ok := TTFTAt(off, rate, 0.9).(TTFT); ok && a < b.Seconds*(1-1e-12) {
				t.Errorf("%s: at %.4g r/s the curve's %.6g under the held %.6g", name, rate, a, b.Seconds)
				break
			}
		}
	}
}

// One pod has no f to price, so the curve is not read there; a curve with
// either term at or under 0 is the caller's mistake.
func TestFLoadCurve_OnePodAndInvalidTerms(t *testing.T) {
	p := curvePrefill(true)
	p.Replicas, p.Imbalance = 1, 1
	pl, u := p.check()
	if u != nil {
		t.Fatal(u)
	}
	if got := pl.fAt(1); got != 1 {
		t.Errorf("one pod: f %g, want 1", got)
	}
	for _, c := range [][2]float64{{0, 520}, {0.97, 0}, {-1, 520}, {0.97, -5}} {
		q := curvePrefill(false)
		q.ImbalanceCurveFloor, q.ImbalanceCurveExcess = c[0], c[1]
		if _, u := q.check(); u == nil || u.Reason != InvalidInput {
			t.Errorf("curve %v: %v, want InvalidInput", c, u)
		}
	}
}
