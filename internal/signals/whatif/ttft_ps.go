package whatif

import (
	"fmt"
	"math"
)

// This file prices the prefill pod and the KV pull as processor-sharing
// stages (TTFTFleet.ProcessorSharing; docs/proposals/slo-capacity-planner-ttft.md,
// "As built: processor sharing"). A prefill pod under chunked prefill does not
// queue a prompt behind the one it is computing: it admits every prompt and
// splits its step between them, so a prompt's prefill stretches while the pod
// is shared, and the vLLM queue stays empty. The decode pod's KV pull shares
// the pod's transfer the same way. The staircase showed both: prefill queue
// time 0 ms at every step, prefill time S/(1 - rho), and the decode pod's
// queue time (its KV pull) S/(1 - rho) as well.
//
// Each stage is an M/G/1-PS queue per pod. A request of work x there takes
// T(x) = x + E(x), the extra E being the time it shares the pod:
//
//   - P(E = 0) = (1 - rho) exp(-lambda x): it found the pod empty and nothing
//     arrived while it ran. Exact for Poisson arrivals.
//   - E[E] = x s rho/(1 - rho), with s = (1 + Ca^2)/2. At Ca^2 = 1 this is the
//     M/G/1-PS sojourn x/(1 - rho), exact and whatever the work's spread. The
//     stretch of a burstier stream is a stated interpolation, Kingman's ratio
//     with the PS mean's insensitivity to the work in place of Cs^2; checked
//     against a tandem simulation, not derived.
//   - Var[E] = 2 s rho/(1 - rho)^2 (x k/(1 - rho) - k^2 (1 - exp(-(1 - rho) x/k))/(1 - rho)^2),
//     k = E[S](Ca^2 + max(1, Cs^2))/2. At Ca^2 = 1 and exponential work (k = E[S])
//     it is the M/M/1-PS conditional variance, exact; for any work its limit
//     at small x, rho x^2/(1 - rho)^2, is exact (x times the geometric number
//     found on arrival); at large x it grows as rho E[S](Ca^2 + max(1, Cs^2)) x/(1 - rho)^3,
//     the central-limit rate of the work arriving meanwhile (exact with
//     Cs^2 >= 1).
//
// Given E > 0 the extra is a gamma of its conditional mean and variance. The
// prompt is a gamma (Prefill.PromptCV), so x is, and the prefill pod's
// work, the pull's and the per-token rest of the own time all move with the
// same prompt. The time is then four parts per pod class: neither stage
// shared, the prefill shared, the pull shared, both shared (independent
// given the prompt). Each part's first two moments over the prompt have a
// closed form (every term is c u^n exp(-a u), u the prompt over its mean),
// and each part is priced as a gamma of those moments. The busiest prefill
// pod takes f/N_p of the arrivals, the others share the rest evenly (as
// meanScore spreads them), so there are two pod classes and at most eight
// gammas, mixed by their share of requests.
//
// Against a tandem simulation of the staircase's bucket B fleet (two prefill
// pods at f 1.23, two decode pods, 8000-token prompts at cv 0.058;
// TestPS_AgainstATandemSimulation), the mean reads within 0.1 %, p90 under 1 %
// and p95 3-4.5 % short. On four decode pods, toward the prefill knee, every
// figure is within 2.5 %; at Ca^2 4 the mean reads 7-9 % high, p90 1-6 % high
// and p95 -7.5 to +10.2 %.

// psStage is one processor-sharing stage on one pod: mean work es, in
// seconds, at arrival rate lam, as functions of the prompt's relative size u.
type psStage struct {
	// rho is lam * es; g the mean stretch per unit of work, s rho/(1 - rho).
	rho, es, g float64
	// alpha, beta and decay make the extra's variance at u:
	// alpha u - beta + beta exp(-decay u).
	alpha, beta, decay float64
}

// newPSStage is the stage at arrival rate lam, mean work es, arrivals' Ca^2
// ca2 and the work's Cs^2 cs2. No work (es 0) is no stage: never shared, no
// extra.
func newPSStage(lam, es, ca2, cs2 float64) psStage {
	if !(es > 0) {
		return psStage{}
	}
	rho := lam * es
	s := (1 + ca2) / 2
	k := es * (ca2 + max(1, cs2)) / 2
	a := 2 * s * rho / ((1 - rho) * (1 - rho))
	return psStage{
		rho: rho, es: es, g: s * rho / (1 - rho),
		alpha: a * es * k / (1 - rho), beta: a * k * k / ((1 - rho) * (1 - rho)), decay: (1 - rho) * es / k,
	}
}

// alone is P(E = 0) at u, (1 - rho) exp(-rho u).
func (st psStage) alone() ePoly { return ePoly{{c: 1 - st.rho, a: st.rho}} }

// shared is P(E > 0) at u.
func (st psStage) shared() ePoly { return ePoly{{c: 1}}.plus(st.alone().scale(-1)) }

// extra is E[E] at u.
func (st psStage) extra() ePoly { return ePoly{{c: st.g * st.es, n: 1}} }

// second is E[E^2] at u: the variance plus the mean squared.
func (st psStage) second() ePoly {
	return ePoly{{c: st.alpha, n: 1}, {c: -st.beta}, {c: st.beta, a: st.decay}}.plus(st.extra().times(st.extra()))
}

// eTerm is c u^n exp(-a u).
type eTerm struct {
	c float64
	n int
	a float64
}

// ePoly is a function of the prompt's relative size u, a sum of eTerms: what
// the PS stages' moments are made of, so that their expectation over a gamma
// prompt is closed.
type ePoly []eTerm

func (p ePoly) plus(q ePoly) ePoly { return append(append(ePoly(nil), p...), q...) }

func (p ePoly) scale(c float64) ePoly {
	out := make(ePoly, len(p))
	for i, t := range p {
		out[i] = eTerm{c: t.c * c, n: t.n, a: t.a}
	}
	return out
}

func (p ePoly) times(q ePoly) ePoly {
	out := make(ePoly, 0, len(p)*len(q))
	for _, s := range p {
		for _, t := range q {
			out = append(out, eTerm{c: s.c * t.c, n: s.n + t.n, a: s.a + t.a})
		}
	}
	return out
}

// expect is E[p(u)] for u a gamma of mean 1 and coefficient of variation cv
// (1 at cv 0): E[u^n exp(-a u)] = prod_{j<n} (k + j)/k * (1 + a/k)^-(k + n),
// k = 1/cv^2, and u^n exp(-a) at cv 0.
func (p ePoly) expect(cv float64) float64 {
	var sum float64
	for _, t := range p {
		if t.c == 0 {
			continue
		}
		if cv == 0 {
			sum += t.c * math.Exp(-t.a)
			continue
		}
		k := 1 / (cv * cv)
		m := math.Exp(-(k + float64(t.n)) * math.Log1p(t.a/k))
		for j := range t.n {
			m *= (k + float64(j)) / k
		}
		sum += t.c * m
	}
	return sum
}

// psMix is the request's time through the PS stages, its own work and its
// stretch, less a: P(T > t) = sum_i w_i P(T_i > t), each T_i a block whose
// own time is a gamma part and whose wait is empty, or the decode wait for
// the independent figure.
type psMix struct {
	w []float64
	b []block
}

func (m psMix) exceeds(t float64) float64 {
	var p float64
	for i, w := range m.w {
		p += w * m.b[i].exceeds(t)
	}
	return min(max(p, 0), 1)
}

func (m psMix) mean() float64 {
	var s float64
	for i, w := range m.w {
		s += w * m.b[i].mean()
	}
	return s
}

func (m psMix) quantile(alpha float64) float64 {
	return tailQuantile(m.exceeds, m.mean(), alpha)
}

// withWait is the mixture with wait in front of every part, taken
// independent of it.
func (m psMix) withWait(wait waitTail) psMix {
	out := psMix{w: m.w, b: make([]block, len(m.b))}
	for i, b := range m.b {
		out.b[i] = block{wait: wait, own: b.own}
	}
	return out
}

// psParts are the PS stages' mean stretches, in seconds: the prefill pods',
// over the requests, and the KV pull's.
type psParts struct {
	prefill, pull float64
}

// psMix is the PS side of the plan at rate: the prefill pods, and with
// decode pods the KV pull on them, at work I * pl.pull. pooled prices every
// prefill pod at the mean share (no imbalance), the optimistic side. It
// refuses (Saturated) where the pull saturates the decode pods.
func (pl prefillPlan) psMix(rate float64, decodePods int, pooled bool) (psMix, psParts, *Unanswerable) {
	i := pl.p.PromptTokens
	n := float64(pl.p.Replicas)
	es1 := pl.work
	var es2, lam2 float64
	if decodePods > 0 && pl.pull > 0 {
		es2, lam2 = i*pl.pull, rate/float64(decodePods)
		if !(lam2*es2 < 1) {
			u := refuse(Saturated, fmt.Sprintf("the KV pull saturates the %d decode pods at %g r/s (utilization %g)",
				decodePods, rate, lam2*es2))
			return psMix{}, psParts{}, &u
		}
	}
	base := max(i*pl.ownRate, es1+es2)
	pull := newPSStage(lam2, es2, pl.ca2, pl.cs2)

	// The pod classes: the busiest at f/N_p of the arrivals, and the other
	// N_p - 1 sharing the rest evenly; pooled, every pod at 1/N_p.
	type class struct{ share, lam float64 }
	classes := []class{{share: 1, lam: rate / n}}
	if !pooled && n > 1 {
		w := pl.f / n
		classes = []class{{share: w, lam: rate * w}, {share: 1 - w, lam: rate * (1 - w) / (n - 1)}}
	}
	var mix psMix
	var parts psParts
	parts.pull = pull.g * pull.es
	u := ePoly{{c: base, n: 1}}
	for _, c := range classes {
		if !(c.share > 0) {
			continue
		}
		pre := newPSStage(c.lam, es1, pl.ca2, pl.cs2)
		parts.prefill += c.share * pre.g * pre.es
		// Each part: its weight, and the first two moments of base*u plus
		// its extra, all as functions of u.
		for _, part := range []struct {
			w, e1, e2 ePoly
		}{
			{w: pre.alone().times(pull.alone())},
			{w: pre.shared().times(pull.alone()), e1: pre.extra().times(pull.alone()), e2: pre.second().times(pull.alone())},
			{w: pre.alone().times(pull.shared()), e1: pull.extra().times(pre.alone()), e2: pull.second().times(pre.alone())},
			{
				w:  pre.shared().times(pull.shared()),
				e1: pre.extra().times(pull.shared()).plus(pull.extra().times(pre.shared())),
				e2: pre.second().times(pull.shared()).plus(pull.second().times(pre.shared())).
					plus(pre.extra().times(pull.extra()).scale(2)),
			},
		} {
			w := part.w.expect(pl.cv)
			if !(w > 1e-12) {
				continue
			}
			m1 := u.times(part.w).plus(part.e1).expect(pl.cv) / w
			m2 := u.times(u).times(part.w).plus(u.times(part.e1).scale(2)).plus(part.e2).expect(pl.cv) / w
			mix.w = append(mix.w, c.share*w)
			mix.b = append(mix.b, block{own: gammaPart(m1, m2-m1*m1)})
		}
	}
	return mix, parts, nil
}

// gammaPart is the own time of a gamma of mean m and variance v: constant
// below the spread the model prices as a gamma (minPromptCV).
func gammaPart(m, v float64) ownTime {
	cv := 0.0
	if v > 0 && m > 0 {
		cv = math.Sqrt(v) / m
	}
	if cv < minPromptCV {
		cv = 0
	}
	return newOwnTime(m, cv)
}
