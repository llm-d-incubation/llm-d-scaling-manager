package whatif

import (
	"math"
	"slices"
	"testing"
)

// The burst rule: Poisson is 1 and 1; burstier, Kingman's ratio and its
// square; below Poisson (1 + Ca^2)/2 for both. The two rules meet at Ca^2 1.
func TestPS_TheBurstRule(t *testing.T) {
	for _, c := range []struct{ ca2, cs2, s, vs float64 }{
		{1, 0, 1, 1}, {1, 3, 1, 1},
		{4, 0, 4, 16}, {4, 1, 2.5, 6.25}, {2, 0.0033, 2.0033 / 1.0033, (2.0033 / 1.0033) * (2.0033 / 1.0033)},
		{0.5, 0, 0.75, 0.75},
	} {
		s, vs := psBurst(c.ca2, c.cs2)
		nearRel(t, "s", s, c.s, 1e-12)
		nearRel(t, "vs", vs, c.vs, 1e-12)
	}
	below, _ := psBurst(1-1e-9, 0)
	above, _ := psBurst(1+1e-9, 0)
	nearRel(t, "continuous at 1", above, below, 1e-8)
	// A pod's share of a stream: all of it keeps the stream's Ca^2, none
	// of it is Poisson, and a split of Poisson stays Poisson.
	nearRel(t, "whole", splitCA2(4, 1), 4, 0)
	nearRel(t, "half", splitCA2(4, 0.5), 2.5, 1e-12)
	nearRel(t, "Poisson", splitCA2(1, 0.3), 1, 1e-12)
}

// The pods' shares: the busiest at f/N_p, the rest falling linearly to the
// mean-preserving least, clamped at 0, always summing to 1. At two pods they
// are f/2 and the rest, as before.
func TestPS_PodShares(t *testing.T) {
	for _, c := range []struct {
		f float64
		n int
	}{{1, 1}, {1, 3}, {1.23, 2}, {1.5, 2}, {1.5, 3}, {1.8, 4}, {2, 3}, {3, 4}, {2.5, 3}, {4, 4}, {1.2, 8}} {
		w := podShares(c.f, c.n)
		if len(w) != c.n {
			t.Fatalf("f %g n %d: %d shares", c.f, c.n, len(w))
		}
		var sum float64
		for j, x := range w {
			sum += x
			if x < 0 || (j > 0 && x > w[j-1]+1e-12) {
				t.Errorf("f %g n %d: shares %v not non-negative and falling", c.f, c.n, w)
			}
		}
		nearRel(t, "sum", sum, 1, 1e-12)
		nearRel(t, "busiest", w[0], c.f/float64(c.n), 1e-12)
		if c.f <= 2 && c.n > 1 {
			// Linear all the way: the least is (2 - f)/N_p.
			nearRel(t, "least", w[c.n-1], (2-c.f)/float64(c.n), 1e-9)
		}
	}
	w := podShares(1.23, 2)
	nearRel(t, "two pods, the other", w[1], 1-1.23/2, 1e-12)
}

// Spreading the other pods by the linear fall reads the population no lower
// than spreading them evenly, which f alone cannot tell apart: the stretch is
// convex in a pod's share. At two pods the two are the same.
func TestPS_TheFallIsNoLowerThanEven(t *testing.T) {
	for _, n := range []int{2, 3, 4} {
		fl := psFleet(0.0577, 1)
		fl.Prefill.Replicas = n
		fl.Prefill.Imbalance = 1.5
		fl.Decode.Replicas = 4
		pl, u := fl.plan()
		if u != nil {
			t.Fatal(u)
		}
		rate := 0.8 * pl.pf.knee()
		mix, parts, bad := pl.pf.psStages(rate, pl.decodePods(), false)
		if bad != nil {
			t.Fatal(bad)
		}
		// The even spread, by hand: the busiest at f/n, the rest at
		// (1 - f/n)/(n - 1) each.
		w := 1.5 / float64(n)
		even := w * stretchAt(pl.pf, rate*w)
		if n > 1 {
			o := (1 - w) / float64(n-1)
			even += (1 - w) * stretchAt(pl.pf, rate*o)
		}
		t.Logf("n %d: prefill stretch %.3f ms on the fall, %.3f ms even; p90 %.1f ms", n, 1e3*parts.prefill, 1e3*even, 1e3*mix.quantile(0.1))
		if n == 2 {
			nearRel(t, "two pods", parts.prefill, even, 1e-12)
		} else if parts.prefill < even*(1+1e-9) {
			t.Errorf("n %d: the fall's stretch %g s is not above the even spread's %g s", n, parts.prefill, even)
		}
	}
}

// stretchAt is one prefill pod's mean stretch at arrival rate lam, Poisson.
func stretchAt(pf prefillPlan, lam float64) float64 {
	rho := lam * pf.work
	return pf.work * rho / (1 - rho)
}

// Past MaxPSPromptCV a TTFT under processor sharing is the larger of the two
// queues' figures, at every percentile and the mean, and carries
// SpreadBeyondProcessorSharing; at or under it, processor sharing's alone and
// no flag.
func TestPS_AWideSpreadIsTheLargerQueue(t *testing.T) {
	for _, cv := range []float64{0.0577, MaxPSPromptCV, 0.5, 1, 2} {
		fl := psFleet(cv, 1)
		wide := cv > MaxPSPromptCV
		off := fl
		off.ProcessorSharing = false
		for _, rate := range []float64{5, 20, 28.3} {
			for _, q := range []float64{0, 0.9, 0.95} {
				got := psTTFT(t, fl, rate, q)
				if got.HasFlag(SpreadBeyondProcessorSharing) != wide {
					t.Errorf("cv %g: flag %v, want %v", cv, got.HasFlag(SpreadBeyondProcessorSharing), wide)
				}
				pl, u := fl.plan()
				if u != nil {
					t.Fatal(u)
				}
				pl.psCapped = false
				alone := pl.price(rate, q, true, true)
				if alone.u != nil {
					t.Fatal(alone.u)
				}
				first := psTTFT(t, off, rate, q)
				want := alone.out.Seconds
				if wide {
					want = math.Max(want, first.Seconds)
				}
				nearRel(t, "the larger", got.Seconds, want, 1e-12)
			}
		}
	}
	// The flag reaches question 1's answer too.
	r := RateForTTFT(psFleet(1, 1), TTFTTarget{Seconds: 0.6, Percentile: 0.9})
	a, ok := r.(Answer)
	if !ok || !slices.Contains(a.Flags, SpreadBeyondProcessorSharing) {
		t.Errorf("RateForTTFT at cv 1: %+v, want SpreadBeyondProcessorSharing", r)
	}
}
