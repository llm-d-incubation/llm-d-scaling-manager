package whatif

import (
	"math"
	"slices"
	"testing"
)

// The 8000-token prompts of the 32B and 0.6B staircases sit in a (5000,
// 10000] bucket: the cv read is the widest the bucket allows, 0.31, and the
// longest prompt is the bucket's bound, 1.25 times the mean (issue
// llm-d-incubation/llm-d-scaling-manager#304).
func TestReadPromptSpread_TheLongestPromptIsTheHighestBoundHeld(t *testing.T) {
	inf := math.Inf(1)
	s := ReadPromptSpread(map[float64]float64{2000: 0, 5000: 0, 10000: 4, 20000: 4, inf: 4}, 8000)
	nearRel(t, "cv", s.CV, 0.306, 0.01)
	nearRel(t, "longest over the mean", s.MaxRatio, 1.25, 1e-12)
	if s.Assumed || s.Clamped {
		t.Errorf("%+v", s)
	}
	// Mass past the last bound bounds nothing; nor does no histogram.
	if c := ReadPromptSpread(map[float64]float64{10000: 3, inf: 4}, 8000); c.MaxRatio != 0 || !c.Clamped {
		t.Errorf("past the last bound: %+v", c)
	}
	if a := ReadPromptSpread(nil, 8000); a.MaxRatio != 0 {
		t.Errorf("no histogram: %+v", a)
	}
}

// gammaPDF is the density of the own time's gamma at s.
func gammaPDF(o ownTime, s float64) float64 {
	lg, _ := math.Lgamma(o.k)
	return math.Exp(o.k*math.Log(o.b) + (o.k-1)*math.Log(s) - o.b*s - lg)
}

// The held gamma keeps the measured mean: censoring alone at the bound reads
// the mean under it, by 4.3% at the 8k staircases' cv 0.31 and bound 1.25,
// 3.4% at cv 0.61 and 2, and 12% at cv 0.75 and 1.6 (the reads the
// backtest meets); stretched by holdScale first, E[min(sX, ratio)] is 1.
func TestHoldScale_KeepsTheMeasuredMean(t *testing.T) {
	for _, c := range []struct{ cv, ratio, deficit, s float64 }{
		{0.31, 1.25, 0.043, 1.065}, {0.61, 2, 0.034, 1.041}, {0.75, 1.6, 0.12, 1.227},
	} {
		k := 1 / (c.cv * c.cv)
		censored := func(s float64) float64 {
			p, _ := gammaPQ(k+1, k*c.ratio/s)
			_, q := gammaPQ(k, k*c.ratio/s)
			return s*p + c.ratio*q
		}
		nearRel(t, "censoring's deficit", 1-censored(1), c.deficit, 0.05)
		s := holdScale(k, c.ratio)
		nearRel(t, "stretch", s, c.s, 0.002)
		nearRel(t, "held mean", censored(s), 1, 1e-12)
		nearRel(t, "E[u] held", ePoly{{c: 1, n: 1}}.expect(c.cv, c.ratio), 1, 1e-12)
	}
	// A bound just over the mean needs a stretch far past the first bracket.
	k := 1.0
	s := holdScale(k, 1.0001)
	p, _ := gammaPQ(k+1, k*1.0001/s)
	_, q := gammaPQ(k, k*1.0001/s)
	if !(s > 1000) {
		t.Errorf("stretch at a bound 1.0001 times the mean: %g, want past 1000", s)
	}
	nearRel(t, "held mean at a bound just over the mean", s*p+1.0001*q, 1, 1e-9)
	for _, r := range []float64{0, 1, 0.9, math.Inf(1), math.NaN()} {
		if s := holdScale(10, r); s != 1 {
			t.Errorf("ratio %g: stretch %g, want 1", r, s)
		}
	}
}

// A held own time is its gamma stretched by holdScale and censored at the
// hold: no mass past it, its mass past it on it, its mean E[min(sX, hold)]
// the measured mean, and its convolution with a wait's exponential tail the
// censored density's, against a numerical integration.
func TestOwnTime_HeldAtTheLongestPrompt(t *testing.T) {
	free := newOwnTime(1.0, 0.31)
	held := free.capped(1.25)
	stretched := free
	stretched.b /= holdScale(free.k, 1.25)
	_, past := gammaPQ(stretched.k, stretched.b*1.25)
	nearRel(t, "mass past the hold", past, 0.263, 0.01)
	if held.sf(1.25) != 0 || held.sf(1.3) != 0 {
		t.Errorf("held sf past the hold: %g, %g", held.sf(1.25), held.sf(1.3))
	}
	nearRel(t, "sf under the hold", held.sf(1.1), stretched.sf(1.1), 1e-12)
	if q := held.quantile(0.9); q != 1.25 {
		t.Errorf("p90 %g, want the hold: the gamma's p90 %g is past it", q, stretched.quantile(0.9))
	}
	nearRel(t, "p50 under the hold", held.quantile(0.5), stretched.quantile(0.5), 1e-12)

	// E[min(X, hold)] and the convolution by the trapezoid rule.
	const n = 200000
	h := 1.25 / n
	var mean float64
	r, at := 2.0, 1.8
	var conv float64
	for i := 1; i <= n; i++ {
		s0, s1 := float64(i-1)*h, float64(i)*h
		f0, f1 := 0.0, gammaPDF(stretched, s1)
		if s0 > 0 {
			f0 = gammaPDF(stretched, s0)
		}
		mean += h * (s0*f0 + s1*f1) / 2
		conv += h * (f0*math.Exp(-r*(at-s0)) + f1*math.Exp(-r*(at-s1))) / 2
	}
	mean += 1.25 * past
	conv += past * math.Exp(-r*(at-1.25))
	nearRel(t, "held mean", held.mean, mean, 1e-6)
	nearRel(t, "held mean is the measured mean", mean, free.mean, 1e-6)
	nearRel(t, "held conv past the hold", held.conv(r, at), conv, 1e-6)
	nearRel(t, "held conv under the hold", held.conv(r, 1.1), stretched.conv(r, 1.1), 1e-12)

	// No bound, a bound at or under the mean, and a constant own time are
	// left as they were.
	for _, h := range []float64{0, 1, 0.9, math.Inf(1), math.NaN()} {
		if got := free.capped(h); got != free {
			t.Errorf("hold %g: %+v", h, got)
		}
	}
	det := newOwnTime(1, 0)
	if det.capped(1.25) != det {
		t.Error("a constant own time moved")
	}
}

// The processor-sharing moments over a held prompt: E[u] and E[u^2] of the
// stretched, censored gamma, and E[exp(-a u)], against a numerical
// integration of min(sX, hold) over the gamma's density, the same held
// prompt the own time prices (ownTime.capped).
func TestEPoly_HeldAtTheLongestPrompt(t *testing.T) {
	cv, hold := 0.31, 1.25
	o := newOwnTime(1, cv).capped(hold)
	_, past := gammaPQ(o.k, o.b*hold)
	const n = 200000
	h := hold / n
	var m1, m2, ma float64
	for i := 1; i <= n; i++ {
		s0, s1 := float64(i-1)*h, float64(i)*h
		f0, f1 := 0.0, gammaPDF(o, s1)
		if s0 > 0 {
			f0 = gammaPDF(o, s0)
		}
		m1 += h * (s0*f0 + s1*f1) / 2
		m2 += h * (s0*s0*f0 + s1*s1*f1) / 2
		ma += h * (math.Exp(-0.7*s0)*f0 + math.Exp(-0.7*s1)*f1) / 2
	}
	m1 += hold * past
	m2 += hold * hold * past
	ma += math.Exp(-0.7*hold) * past
	nearRel(t, "E[u]", ePoly{{c: 1, n: 1}}.expect(cv, hold), m1, 1e-6)
	nearRel(t, "E[u^2]", ePoly{{c: 1, n: 2}}.expect(cv, hold), m2, 1e-6)
	nearRel(t, "E[exp(-0.7 u)]", ePoly{{c: 1, a: 0.7}}.expect(cv, hold), ma, 1e-6)
	nearRel(t, "E[u] is the measured mean", m1, 1, 1e-6)
	if !(ePoly{{c: 1, n: 2}}.expect(cv, hold) < ePoly{{c: 1, n: 2}}.expect(cv, 0)) {
		t.Error("held E[u^2] must be under the free one")
	}
}

// supportFleet is the worked fleet under processor sharing at the 8k
// staircase's spread read, cv 0.31, with the longest prompt at ratio (0 is
// none).
func supportFleet(ratio float64) TTFTFleet {
	fl := workedFleet()
	fl.ProcessorSharing = true
	fl.Prefill.PromptCV, fl.Prefill.PromptMaxRatio = 0.31, ratio
	return fl
}

// A bound on the longest prompt at MaxPSPromptSupport or under prices
// processor sharing alone at a cv past MaxPSPromptCV, so the 8k answers are
// no longer flagged SpreadBeyondProcessorSharing; a wider bound, or none,
// keeps the cv's rule. Held at the bound, the tail reads under the free
// gamma's; the mean does not move, the held gamma keeping the measured mean.
func TestTTFT_ThePromptsSupportDecidesProcessorSharing(t *testing.T) {
	if !(MaxPSPromptSupport < gammaQuantile(1/(MaxPSPromptCV*MaxPSPromptCV), 0.99)*MaxPSPromptCV*MaxPSPromptCV) {
		t.Fatalf("MaxPSPromptSupport %g must sit under the 99th percentile of the gamma at MaxPSPromptCV", MaxPSPromptSupport)
	}
	free, held, wide := supportFleet(0), supportFleet(1.25), supportFleet(2)
	for _, c := range []struct {
		name string
		fl   TTFTFleet
		flag bool
	}{{"no bound", free, true}, {"1.25", held, false}, {"2", wide, true}, {"at the limit", supportFleet(MaxPSPromptSupport), false}} {
		if got := c.fl.Prefill.BeyondProcessorSharing(); got != c.flag {
			t.Errorf("%s: beyond processor sharing %v, want %v", c.name, got, c.flag)
		}
		res := mustTTFT(t, ttftAt(t, c.fl, 2, 0.9))
		if got := slices.Contains(res.Flags, SpreadBeyondProcessorSharing); got != c.flag {
			t.Errorf("%s: flagged %v, want %v (%v)", c.name, got, c.flag, res.Flags)
		}
	}
	if h, w := mustTTFT(t, ttftAt(t, held, 2, 0.9)), mustTTFT(t, ttftAt(t, wide, 2, 0.9)); !(h.Seconds < w.Seconds) {
		t.Errorf("p90: held %g s, want under the wide bound's %g s", h.Seconds, w.Seconds)
	}
	nearRel(t, "mean, held against the wide bound",
		mustTTFT(t, ttftAt(t, held, 2, 0)).Seconds, mustTTFT(t, ttftAt(t, wide, 2, 0)).Seconds, 1e-9)
	// First come, the bound alone moves the own time: the tail held under
	// free, the mean where it was.
	fc := func(ratio, q float64) TTFT {
		fl := supportFleet(ratio)
		fl.ProcessorSharing = false
		return mustTTFT(t, ttftAt(t, fl, 2, q))
	}
	if h, f := fc(1.25, 0.9), fc(0, 0.9); !(h.Seconds < f.Seconds) {
		t.Errorf("first come p90: held %g s, want under the free %g s", h.Seconds, f.Seconds)
	}
	nearRel(t, "first come mean, held against free", fc(1.25, 0).Seconds, fc(0, 0).Seconds, 1e-6)
}

// A longest prompt under the mean, or not a number, is the caller's mistake.
func TestTTFT_ALongestPromptUnderTheMeanIsRefused(t *testing.T) {
	for _, r := range []float64{0.5, -1, math.NaN(), math.Inf(1)} {
		res := TTFTAt(supportFleet(r), 2, 0.9)
		if u, ok := res.(Unanswerable); !ok || u.Reason != InvalidInput {
			t.Errorf("ratio %g: %#v", r, res)
		}
	}
}
