package domain

import (
	"math"
	"math/rand/v2"
	"testing"
)

// vllmBounds are vLLM's request_generation_tokens buckets (1-2-5) up to a
// 50k context.
var vllmBounds = []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 20000, 50000}

// histogram returns the cumulative buckets and the mean of xs, as the
// collector reads them.
func histogram(xs []float64) ([]LengthBucket, float64) {
	out := make([]LengthBucket, 0, len(vllmBounds)+1)
	var sum float64
	for _, x := range xs {
		sum += x
	}
	for _, b := range append(append([]float64{}, vllmBounds...), math.Inf(1)) {
		var n float64
		for _, x := range xs {
			if x <= b {
				n++
			}
		}
		out = append(out, LengthBucket{UpperBound: b, Rate: n})
	}
	return out, sum / float64(len(xs))
}

func squaredMean(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x * x
	}
	return s / float64(len(xs))
}

// The bound is never above the true E[O^2], is exact for one length, and on
// vLLM's buckets recovers part of the spread of the issue's three
// distributions.
func TestSquaredMeanLowerBound(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	const n, ol = 20000, 500.0
	sigma := math.Sqrt(math.Log(1 + 2.25))
	cases := []struct {
		name    string
		draw    func() float64
		minGain float64 // the least fraction of D-1 the bound must recover
	}{
		{"fixed", func() float64 { return ol }, 0},
		{"uniform", func() float64 { return math.Max(1, math.Round(r.Float64()*2*ol)) }, 0.3},
		{"exponential", func() float64 { return math.Max(1, math.Round(r.ExpFloat64()*ol)) }, 0.3},
		{"lognormal cv 1.5", func() float64 {
			return math.Max(1, math.Round(math.Exp(math.Log(ol)-sigma*sigma/2+sigma*r.NormFloat64())))
		}, 0.3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			xs := make([]float64, n)
			for i := range xs {
				xs[i] = c.draw()
			}
			bs, mean := histogram(xs)
			lb, ok := SquaredMeanLowerBound(bs, mean)
			if !ok {
				t.Fatal("no reading")
			}
			truth := squaredMean(xs)
			if lb > truth*(1+1e-9) {
				t.Fatalf("bound %.0f above the true E[O^2] %.0f", lb, truth)
			}
			dTrue, dLB := truth/(mean*mean), lb/(mean*mean)
			if c.minGain == 0 {
				if math.Abs(dLB-1) > 1e-9 {
					t.Fatalf("one length read a spread: D %.6f", dLB)
				}
				return
			}
			if gain := (dLB - 1) / (dTrue - 1); gain < c.minGain {
				t.Fatalf("D %.3f against a true %.3f: recovered %.0f%% of the spread", dLB, dTrue, 100*gain)
			}
		})
	}
}

// A histogram that is not one reading gives none.
func TestSquaredMeanLowerBound_NotAReading(t *testing.T) {
	inf := math.Inf(1)
	full := []LengthBucket{{100, 4}, {500, 10}, {inf, 10}}
	for name, c := range map[string]struct {
		bs   []LengthBucket
		mean float64
	}{
		"no +Inf bucket":         {[]LengthBucket{{100, 4}, {500, 10}}, 200},
		"no requests":            {[]LengthBucket{{100, 0}, {inf, 0}}, 200},
		"no buckets":             {nil, 200},
		"a zero mean":            {full, 0},
		"a NaN mean":             {full, math.NaN()},
		"an infinite mean":       {full, inf},
		"a mean under the lows":  {[]LengthBucket{{100, 0}, {500, 10}, {inf, 10}}, 50},
		"a mean over the highs":  {full, 600},
		"every rate NaN":         {[]LengthBucket{{100, math.NaN()}, {inf, math.NaN()}}, 200},
		"the +Inf rate negative": {[]LengthBucket{{100, 4}, {inf, -1}}, 200},
	} {
		if v, ok := SquaredMeanLowerBound(c.bs, c.mean); ok {
			t.Errorf("%s: read %v", name, v)
		}
	}
}

// Order does not matter, a falling cumulative rate counts no requests, and a
// mean just outside what the buckets allow is clamped onto them.
func TestSquaredMeanLowerBound_Edges(t *testing.T) {
	inf := math.Inf(1)
	sorted := []LengthBucket{{100, 4}, {500, 10}, {inf, 10}}
	shuffled := []LengthBucket{{inf, 10}, {100, 4}, {500, 10}}
	a, okA := SquaredMeanLowerBound(sorted, 250)
	b, okB := SquaredMeanLowerBound(shuffled, 250)
	if !okA || !okB || math.Abs(a-b) > 1e-6 {
		t.Fatalf("order changed the bound: %v %v", a, b)
	}
	// Four at 100 at most, six above 100: the least E[O^2] for a mean of
	// 250 puts the four at 100 and the six at 350.
	if want := (4*100.0*100 + 6*350.0*350) / 10; math.Abs(a-want) > 1e-3*want {
		t.Fatalf("bound %.1f, want %.1f", a, want)
	}
	// A negative or -Inf bound is no length: it is dropped, not read as a
	// span whose top is under its bottom.
	for _, bad := range []float64{-5, math.Inf(-1)} {
		v, ok := SquaredMeanLowerBound(append([]LengthBucket{{bad, 2}}, sorted...), 250)
		if !ok || math.Abs(v-a) > 1e-6 {
			t.Errorf("a bound of %v: %v %v, want %v as without it", bad, v, ok, a)
		}
	}
	if _, ok := SquaredMeanLowerBound([]LengthBucket{{100, 6}, {500, 4}, {inf, 10}}, 200); !ok {
		t.Fatal("a falling rate must read as no requests in that bucket, not refuse the histogram")
	}
	// A mean just outside the buckets is clamped onto them, and the bound is
	// returned against the caller's own mean, so the spread it gives is the
	// one the buckets imply, not inflated by the clamp. The ceiling is four
	// at 100 and six at 500 (a mean of 340, E[O^2] 154,000); the floor is
	// four at 0 and six at 100 (a mean of 60, E[O^2] 6,000).
	for _, c := range []struct {
		name         string
		mean, spread float64
	}{
		{"0.5% over the ceiling", 340 * 1.005, 154_000.0 / (340 * 340)},
		{"0.5% under the floor", 60 * 0.995, 6_000.0 / (60 * 60)},
	} {
		v, ok := SquaredMeanLowerBound(sorted, c.mean)
		if !ok {
			t.Fatalf("%s: no reading", c.name)
		}
		if got := v / (c.mean * c.mean); math.Abs(got-c.spread) > 1e-9 {
			t.Errorf("%s: spread %.6f, want %.6f, the buckets' own", c.name, got, c.spread)
		}
	}
}
