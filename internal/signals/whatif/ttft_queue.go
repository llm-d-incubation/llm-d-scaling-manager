package whatif

import "math"

// This file is the queueing arithmetic of the TTFT model
// (docs/proposals/slo-capacity-planner-ttft.md): the distribution of a
// request's own prefill time, the waits that sit in front of it, the
// convolution of the two, and the optimised union bound across the prefill
// and decode sides. It holds no fleet: ttft.go builds these from one.
//
// UNVALIDATED. Every figure here is checked against closed-form queueing
// values (M/M/1, Erlang's exact M/D/1, Pollaczek-Khinchine, Erlang C), an
// independent numerical integration and a Lindley simulation, not against a
// measured TTFT. The prefill-stress
// recording (P2) is what will check the model against a real pod.

// ownTime is a request's own prefill time, a + I*c, less the constant a:
// I*c with the prompt I gamma distributed at its mean and coefficient of
// variation (an exponential at cv 1, the page's assumed spread), and
// deterministic at cv 0. c is seconds per prompt token.
type ownTime struct {
	// mean is E[I]*c, in seconds.
	mean float64
	// k and b are the gamma's shape, 1/cv^2, and rate, 1/(mean*cv^2); both 0
	// when det.
	k, b float64
	// det is a constant own time (cv 0).
	det bool
}

func newOwnTime(mean, cv float64) ownTime {
	if cv == 0 {
		return ownTime{mean: mean, det: true}
	}
	cv2 := cv * cv
	return ownTime{mean: mean, k: 1 / cv2, b: 1 / (mean * cv2)}
}

// quantile is the own time's q-th percentile, q in (0, 1).
func (o ownTime) quantile(q float64) float64 {
	if o.det {
		return o.mean
	}
	return gammaQuantile(o.k, q) / o.b
}

// sf is P(own > t).
func (o ownTime) sf(t float64) float64 {
	if t <= 0 {
		return 1
	}
	if o.det {
		if t < o.mean {
			return 1
		}
		return 0
	}
	_, q := gammaPQ(o.k, o.b*t)
	return q
}

// conv is the integral over s in [0, t] of g(s) * exp(-r*(t - s)), g the
// own time's density: the part of P(W + own > t) in which the wait W, with
// tail exp(-r*u), has not yet run out. It has a closed form through the
// incomplete gamma function:
//
//	z = b - r > 0:  exp(-r t) (b/z)^k P(k, z t)
//	z <= 0:         exp(-r t) b^k/Gamma(k) * sum_n t^(n+k) y^n / (n! (n+k)),  y = -z t
//
// The second is the integral of s^(k-1) exp(y s / t) term by term; its terms
// are all positive, so no cancellation. Both are summed in logs: (b/z)^k
// overflows as r nears b while P(k, zt) underflows.
func (o ownTime) conv(r, t float64) float64 {
	if t <= 0 {
		return 0
	}
	if o.det {
		if t < o.mean {
			return 0
		}
		return math.Exp(-r * (t - o.mean))
	}
	k, b := o.k, o.b
	if z := b - r; z > 0 {
		return math.Exp(-r*t + k*(math.Log(b)-math.Log(z)) + lnGammaP(k, z*t))
	}
	y := (r - b) * t
	lgk, _ := math.Lgamma(k)
	return math.Exp(-r*t + k*math.Log(b*t) - lgk + logSeries(k, y))
}

// logSeries is ln sum_{n>=0} y^n / (n! (n+k)), for y >= 0 and k > 0.
func logSeries(k, y float64) float64 {
	if !(y >= 0) || math.IsInf(y, 1) {
		return math.NaN() // outside the series' domain: never sum a million NaN terms
	}
	if y == 0 {
		return -math.Log(k)
	}
	ly := math.Log(y)
	// The terms rise to about n = y, then fall faster than geometrically.
	// They are summed in logs against a running maximum, rescaling the sum
	// whenever a larger term arrives, so nothing is stored.
	top, sum := math.Inf(-1), 0.0
	for n := 0; n < maxSeriesTerms; n++ {
		lg, _ := math.Lgamma(float64(n + 1))
		lt := float64(n)*ly - lg - math.Log(float64(n)+k)
		if lt > top {
			sum = sum*math.Exp(top-lt) + 1
			top = lt
		} else {
			sum += math.Exp(lt - top)
		}
		if float64(n) > y && lt < top-40 {
			return top + math.Log(sum)
		}
	}
	return math.NaN() // did not converge: never a partial sum
}

// maxSeriesTerms bounds every series and continued fraction here. The
// arguments are bounded by the quantiles asked for (a few hundred mean
// waits at most), so it is never reached on a fleet the model prices; it
// stops a reading error from spinning.
const maxSeriesTerms = 1_000_000

// waitTail is a wait with an atom at 0 and, past it, a sum of shifted
// exponential pieces: piece i adds w_i to P(W > u) for u < s_i and
// w_i exp(-r_i (u - s_i)) from s_i on. The weights of a hypoexponential part
// are signed (its pieces share a shift); their sum is P(W > 0).
type waitTail struct {
	w, r, s []float64
}

// expWait is a wait that is 0 with probability 1 - p and otherwise
// exponential at rate r: P(W > u) = p exp(-r u). The decode admission wait
// takes this form, and so does the worst pod's at cv >= 1.
func expWait(p, r float64) waitTail {
	if p == 0 {
		return waitTail{}
	}
	return waitTail{w: []float64{p}, r: []float64{r}, s: []float64{0}}
}

// boundWait is P(W > u) = min(p, c exp(-g u)), c >= p: an upper bound of the
// form Taylor's bound on an M/G/1 wait takes, g the Cramer-Lundberg root,
// capped at P(W > 0) = p. It is the wait p exp(-g (u - s)) shifted by
// s = ln(c/p)/g, flat at p before; at c = p it is the unshifted exponential.
func boundWait(p, c, g float64) waitTail {
	if p == 0 {
		return waitTail{}
	}
	return waitTail{w: []float64{p}, r: []float64{g}, s: []float64{math.Log(max(c, p)/p) / g}}
}

// sum is the wait of the two waits in sequence, taken independent: the
// atom times the other's tail, plus, where both wait, a hypoexponential.
// Rates within a relative 1e-6 of each other are parted by that much, which
// moves the figure by about as much: the hypoexponential's coefficients are
// r2/(r2-r1) and -r1/(r2-r1).
//
// ok is false for shapes it does not sum: anything but a single-piece wait
// and an unshifted single-piece one (or an empty one).
func (a waitTail) sum(b waitTail) (_ waitTail, ok bool) {
	if len(a.w) == 0 {
		return b, true
	}
	if len(b.w) == 0 {
		return a, true
	}
	if len(a.w) != 1 || len(b.w) != 1 || b.s[0] != 0 {
		return waitTail{}, false
	}
	p1, r1, s1, p2, r2 := a.w[0], a.r[0], a.s[0], b.w[0], b.r[0]
	if math.Abs(r2-r1) <= 1e-6*max(r1, r2) {
		r2 = r1 * (1 + 1e-6)
	}
	c1, c2 := r2/(r2-r1), -r1/(r2-r1)
	// Only the first waits: its own piece. Only the second: its piece,
	// unshifted. Both: s1 + Exp(r1) + Exp(r2), a hypoexponential at s1.
	return waitTail{
		w: []float64{p1*(1-p2) + p1*p2*c1, p1 * p2 * c2, p2 * (1 - p1)},
		r: []float64{r1, r2, r2},
		s: []float64{s1, s1, 0},
	}, true
}

// mean is E[W].
func (a waitTail) mean() float64 {
	var m float64
	for i, w := range a.w {
		m += w * (a.s[i] + 1/a.r[i])
	}
	return m
}

// block is the wait W followed by the request's own time: P(W + own > t),
// exact for independent parts (first-come service with Poisson arrivals
// makes a request's wait independent of its own work):
//
//	P(W + own > t) = P(own > t) + sum_i w_i [P(t - s_i < own <= t) + conv_{r_i}(t - s_i)]
//
// with P(own > x) = 1 and conv = 0 for x <= 0; at s_i = 0 the first term in
// the brackets is 0.
type block struct {
	wait waitTail
	own  ownTime
}

func (bl block) exceeds(t float64) float64 {
	if t <= 0 {
		return 1
	}
	sf := bl.own.sf(t)
	p := sf
	for i, w := range bl.wait.w {
		x := t - bl.wait.s[i]
		p += w * (bl.own.sf(x) - sf + bl.own.conv(bl.wait.r[i], x))
	}
	if math.IsNaN(p) {
		// Never a quantile of 0 from a tail that could not be read: the
		// quantile runs out to +Inf instead, which the caller refuses.
		return 1
	}
	return min(max(p, 0), 1)
}

// mean is E[W + own].
func (bl block) mean() float64 {
	return bl.own.mean + bl.wait.mean()
}

// quantile is the least t with P(W + own > t) <= alpha, alpha in (0, 1), by
// bisection: the tail is non-increasing in t.
func (bl block) quantile(alpha float64) float64 {
	return tailQuantile(bl.exceeds, bl.mean(), alpha)
}

// quantiler is block 1 of the union bound: a time with a quantile, which is
// a block (a wait, then the request's own time) or, under processor sharing,
// a psMix.
type quantiler interface {
	quantile(alpha float64) float64
}

// tailQuantile is the least t with exceeds(t) <= alpha, alpha in (0, 1), by
// bisection from mean: exceeds is non-increasing in t.
func tailQuantile(exceeds func(float64) float64, mean, alpha float64) float64 {
	lo, hi := 0.0, mean
	if !(hi > 0) {
		hi = 1
	}
	for i := 0; exceeds(hi) > alpha; i++ {
		if i > 200 {
			return math.Inf(1)
		}
		lo, hi = hi, 2*hi
	}
	for range 200 {
		mid := lo + (hi-lo)/2
		if exceeds(mid) > alpha {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo <= 1e-13*hi {
			break
		}
	}
	return hi
}

// split is the optimised union bound (docs/proposals/slo-capacity-planner-ttft.md,
// "Combining the parts into a percentile"): for any alpha + beta = 1 - q,
//
//	TTFT_q <= Q_{1-alpha}(block 1) + Q_{1-beta}(W_decode)
//
// whatever the dependence between the two, since they are driven by the same
// arrivals. It returns the least sum over beta, and that beta. The decode
// term is 0 once beta reaches the decode wait probability pd, and block 1's
// quantile only grows with beta, so the search runs over (0, min(pd, 1-q)):
// a log-spaced grid, then a golden-section search beside its best point.
// The even split, beta = (1-q)/2, is a candidate too, so the answer is never
// above it whatever the search finds.
func split(b1 quantiler, dec waitTail, q float64) (value, beta float64) {
	eps := 1 - q
	if len(dec.w) == 0 {
		return b1.quantile(eps), 0
	}
	pd, rd := dec.w[0], dec.r[0]
	f := func(beta float64) float64 {
		d := 0.0
		if beta < pd {
			d = math.Log(pd/beta) / rd
		}
		return b1.quantile(eps-beta) + d
	}
	hi := min(pd, eps*(1-1e-9))
	lo := eps * 1e-12
	if hi <= lo {
		// The decode wait is too rare to price: give it no share.
		return min(b1.quantile(eps), f(hi)), hi
	}
	const grid = 48
	best, bestX := math.Inf(1), 0.0
	for i := 0; i <= grid; i++ {
		x := math.Log(lo) + (math.Log(hi)-math.Log(lo))*float64(i)/grid
		if v := f(math.Exp(x)); v < best {
			best, bestX = v, x
		}
	}
	// Golden section in log(beta) over the best grid point's neighbours.
	step := (math.Log(hi) - math.Log(lo)) / grid
	a, c := max(bestX-step, math.Log(lo)), min(bestX+step, math.Log(hi))
	const g = 0.6180339887498949
	x1, x2 := c-g*(c-a), a+g*(c-a)
	f1, f2 := f(math.Exp(x1)), f(math.Exp(x2))
	for range 60 {
		if f1 <= f2 {
			c, x2, f2 = x2, x1, f1
			x1 = c - g*(c-a)
			f1 = f(math.Exp(x1))
		} else {
			a, x1, f1 = x1, x2, f2
			x2 = a + g*(c-a)
			f2 = f(math.Exp(x2))
		}
	}
	for _, x := range []float64{x1, x2} {
		if v := f(math.Exp(x)); v < best {
			best, bestX = v, x
		}
	}
	beta = min(math.Exp(bestX), hi) // exp(log(hi)) can round above hi
	if v := f(eps / 2); v < best {
		best, beta = v, eps/2
	}
	return best, beta
}

// lundbergRoot is the Cramer-Lundberg root of an M/G/1 queue with Poisson
// arrivals at lam and gamma work of mean es and coefficient of variation cv
// (constant at cv 0): the gamma > 0 with lam (M(gamma) - 1) = gamma, where
// M(g) = (1 - g cv^2 es)^(-1/cv^2), or exp(g es) for constant work. The left
// side less the right is convex, 0 at 0 with slope rho - 1 < 0, and grows
// without bound toward the pole of M (or forever), so the root is the one
// sign change, found by bisection. At cv 1 it is 1/es - lam.
//
// NaN when there is no root to find: a work mean that is not positive and
// finite, or rho = lam*es at or above 1 (the queue is unstable).
func lundbergRoot(lam, es, cv float64) float64 {
	if !(lam > 0) {
		return math.Inf(1) // no arrivals, no wait: nothing to bound
	}
	if !Finite(es) || !(es > 0) || !Finite(lam) || !(lam*es < 1) {
		return math.NaN()
	}
	k, pole := 0.0, math.Inf(1)
	if cv > 0 {
		k = 1 / (cv * cv)
		pole = k / es
	}
	phi := func(g float64) float64 {
		var m float64
		if cv > 0 {
			m = math.Exp(-k * math.Log1p(-g/pole))
		} else {
			m = math.Exp(g * es)
		}
		return lam*(m-1) - g
	}
	hi := pole
	if math.IsInf(pole, 1) {
		// Constant work: phi grows like exp(g es), so a root lies within
		// a few hundred doublings of 1/es; past that there is none.
		hi = 1 / es
		for i := 0; !(phi(hi) > 0); i++ {
			if i >= 2000 || math.IsInf(hi, 1) {
				return math.NaN()
			}
			hi *= 2
		}
	}
	lo := 0.0
	for range 200 {
		mid := lo + (hi-lo)/2
		// lo is kept below the root, so the decay returned is never the
		// faster side of it.
		if phi(mid) < 0 {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo <= 1e-14*hi {
			break
		}
	}
	return lo
}

// erlangCProb is Erlang C, the probability an arrival waits in an M/M/c
// queue at offered load a (in servers), by the Erlang B recursion, which is
// stable for any c. 1 at or above saturation.
func erlangCProb(c int, a float64) float64 {
	if a >= float64(c) {
		return 1
	}
	b := 1.0
	for n := 1; n <= c; n++ {
		b = a * b / (float64(n) + a*b)
	}
	return float64(c) * b / (float64(c) - a*(1-b))
}

// gammaPQ is the regularized incomplete gamma pair P(k, x) and
// Q(k, x) = 1 - P(k, x), each computed directly where it is the small one:
// the series for P below x = k + 1, the continued fraction for Q above
// (Numerical Recipes' gser and gcf).
func gammaPQ(k, x float64) (p, q float64) {
	if x <= 0 {
		return 0, 1
	}
	if x < k+1 {
		p = math.Exp(lnGammaPSeries(k, x))
		return p, 1 - p
	}
	q = math.Exp(lnGammaQFraction(k, x))
	return 1 - q, q
}

// lnGammaP is ln P(k, x), accurate where P is tiny.
func lnGammaP(k, x float64) float64 {
	if x <= 0 {
		return math.Inf(-1)
	}
	if x < k+1 {
		return lnGammaPSeries(k, x)
	}
	return math.Log1p(-math.Exp(lnGammaQFraction(k, x)))
}

// lnGammaPSeries is ln P(k, x) by its series; NaN when the series does not
// converge within maxSeriesTerms, never a partial sum.
func lnGammaPSeries(k, x float64) float64 {
	term := 1 / k
	sum := term
	for n := 1; n < maxSeriesTerms; n++ {
		term *= x / (k + float64(n))
		sum += term
		if term < sum*1e-17 {
			lgk, _ := math.Lgamma(k)
			return -x + k*math.Log(x) - lgk + math.Log(sum)
		}
	}
	return math.NaN()
}

// lnGammaQFraction is ln Q(k, x) by its continued fraction (modified Lentz);
// NaN when it does not converge within maxSeriesTerms, never a partial value.
func lnGammaQFraction(k, x float64) float64 {
	const tiny = 1e-300
	b := x + 1 - k
	c := 1 / tiny
	d := 1 / b
	h := d
	for i := 1; i < maxSeriesTerms; i++ {
		an := -float64(i) * (float64(i) - k)
		b += 2
		d = an*d + b
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = b + an/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del
		// Within a few units in the last place of 1: 1e-16 alone is below
		// float64's spacing above 1 and would be met only by del == 1.
		if math.Abs(del-1) < 4e-16 {
			lgk, _ := math.Lgamma(k)
			return -x + k*math.Log(x) - lgk + math.Log(h)
		}
	}
	return math.NaN()
}

// gammaQuantile is the x with P(k, x) = q, by bisection; NaN when no
// bracket is found (P never reaches q, or reads NaN).
func gammaQuantile(k, q float64) float64 {
	lo, hi := 0.0, max(k, 1)
	for i := 0; ; i++ {
		p, _ := gammaPQ(k, hi)
		if p >= q {
			break
		}
		if i >= 2000 || math.IsNaN(p) {
			return math.NaN()
		}
		lo, hi = hi, 2*hi
	}
	for range 200 {
		mid := lo + (hi-lo)/2
		p, _ := gammaPQ(k, mid)
		if math.IsNaN(p) {
			return math.NaN()
		}
		if p < q {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo <= 1e-15*hi {
			break
		}
	}
	return hi
}
