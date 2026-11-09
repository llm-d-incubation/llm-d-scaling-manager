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
//   - E[E] = x s rho/(1 - rho). At Ca^2 = 1, s = 1 and this is the M/G/1-PS
//     sojourn x/(1 - rho), exact and whatever the work's spread. Burstier
//     than Poisson, s is Kingman's ratio (Ca^2 + Cs^2)/(1 + Cs^2) (psBurst):
//     a stated rule, checked against simulation, not derived. Below Poisson
//     s is (1 + Ca^2)/2.
//   - Var[E] = 2 v rho/(1 - rho)^2 (x k/(1 - rho) - k^2 (1 - exp(-(1 - rho) x/k))/(1 - rho)^2),
//     k = E[S](Ca^2 + max(1, Cs^2))/2, v = s^2 past Poisson and s below it.
//     At Ca^2 = 1 and exponential work (k = E[S]) it is the M/M/1-PS
//     conditional variance, exact; for any work its limit at small x,
//     v rho x^2/(1 - rho)^2, is exact at Poisson (x times the geometric number
//     found on arrival); at large x it grows as v rho E[S](Ca^2 + max(1, Cs^2)) x/(1 - rho)^3,
//     the central-limit rate of the work arriving meanwhile (exact at
//     Poisson with Cs^2 >= 1).
//
// Each stage is priced at its pods' Ca^2, not the fleet's: a pod taking
// share p of a stream of Ca^2 sees p Ca^2 + 1 - p (splitCA2), the prefill pods
// at their shares and the pull at 1/N_d.
//
// Given E > 0 the extra is a gamma of its conditional mean and variance. The
// prompt is a gamma (Prefill.PromptCV), so x is, and the prefill pod's
// work, the pull's and the per-token rest of the own time all move with the
// same prompt. The time is then four parts per prefill pod: neither stage
// shared, the prefill shared, the pull shared, both shared (independent
// given the prompt). Each part's first two moments over the prompt have a
// closed form (every term is c u^n exp(-a u), u the prompt over its mean),
// and each part is priced as a gamma of those moments. The busiest prefill
// pod takes f/N_p of the arrivals and the others fall linearly from it
// (podShares; at two pods that is the rest), and the parts are mixed by
// their pods' shares of the requests: the quantile is the population's, a
// request on any pod, not the busiest pod's.
//
// Against a tandem simulation of the staircase's bucket B fleet (two prefill
// pods at f 1.23, two decode pods, 8000-token prompts at cv 0.058;
// TestPS_AgainstATandemSimulation), the mean reads within 0.1 %, p90 under 1 %
// short and p95 3.3-4.4 % short. On four decode pods, toward the prefill knee,
// every figure is within 2.5 %. Burstier than Poisson p90 and p95 read high:
// against one-pod M/G/1-PS simulations at Ca^2 2 and 4, rho 0.3 to 0.93 and
// prompt cv 0.058 and 1, they read -1.1 % to +117 % (the most at light load,
// near-constant prompts and Ca^2 4), and the mean -3.8 % to +45 %; bucket C
// (Ca^2 4, two pods) reads its mean 8-12 % high and p90 and p95 25-32 % high.
// The over-read at light load is real, not noise: on one pod at Ca^2 4 and cv
// 0.058, p95 reads +118 % at rho 0.3, +48 % at 0.6 and +10 % at 0.85, and a
// 250 ms p95 target is met at 0.68 of the rate that truly meets it. s is the
// heavy-traffic burst factor at every load; blending it from 1 toward
// Kingman's as rho rises is a follow-up (the proposal's "Burstier than
// Poisson").
//
// Past a prompt cv of MaxPSPromptCV the price is the larger of this and the
// first-come queue's (ttftPlan.price): no run has validated processor
// sharing at a wide spread.

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
	s, vs := psBurst(ca2, cs2)
	k := es * (ca2 + max(1, cs2)) / 2
	a := 2 * vs * rho / ((1 - rho) * (1 - rho))
	return psStage{
		rho: rho, es: es, g: s * rho / (1 - rho),
		alpha: a * es * k / (1 - rho), beta: a * k * k / ((1 - rho) * (1 - rho)), decay: (1 - rho) * es / k,
	}
}

// psBurst is the stretch's factor s on the M/G/1-PS mean, and vs on the
// variance's scale, at arrivals' Ca^2 ca2 and the work's Cs^2 cs2.
//
// At Ca^2 <= 1 both are (1 + Ca^2)/2: 1 at Poisson, where the mean is exact.
// Burstier than Poisson, s is Kingman's ratio (Ca^2 + Cs^2)/(1 + Cs^2), the
// first-come wait's at the same Ca^2 over its Poisson one, and vs is s^2: a
// burst stretches every request in it by about the burst's size, so the
// extra scales as s and its variance as s^2. (1 + Ca^2)/2, with the variance
// at s, read 15-49 % short at Ca^2 2 to 4 on near-constant prompts
// (docs/proposals/slo-capacity-planner-ttft.md, "As built: processor
// sharing"). Both rules meet at Ca^2 1.
func psBurst(ca2, cs2 float64) (s, vs float64) {
	if ca2 <= 1 {
		s = (1 + ca2) / 2
		return s, s
	}
	s = (ca2 + cs2) / (1 + cs2)
	return s, s * s
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

// stageMix is the request's time through the PS stages, its own work and its
// stretch, less a: P(T > t) = sum_i w_i P(T_i > t), each T_i a block whose
// own time is a gamma part and whose wait is empty, or the decode wait for
// the independent figure.
type stageMix struct {
	w []float64
	b []block
}

func (m stageMix) exceeds(t float64) float64 {
	var p float64
	for i, w := range m.w {
		p += w * m.b[i].exceeds(t)
	}
	return min(max(p, 0), 1)
}

func (m stageMix) mean() float64 {
	var s float64
	for i, w := range m.w {
		s += w * m.b[i].mean()
	}
	return s
}

func (m stageMix) quantile(alpha float64) float64 {
	return tailQuantile(m.exceeds, m.mean(), alpha)
}

// withWait is the mixture with wait in front of every part, taken
// independent of it.
func (m stageMix) withWait(wait waitTail) stageMix {
	out := stageMix{w: m.w, b: make([]block, len(m.b))}
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

// psStages is the PS side of the plan at rate: the prefill pods, and with
// decode pods the KV pull on them, at work I * pl.pull. pooled prices every
// prefill pod at the mean share (no imbalance), the optimistic side. It
// refuses (Saturated) where the pull saturates the decode pods.
func (pl prefillPlan) psStages(rate float64, decodePods int, pooled bool) (stageMix, psParts, *Unanswerable) {
	i := pl.p.PromptTokens
	n := float64(pl.p.Replicas)
	es1 := pl.work
	var es2, lam2 float64
	if decodePods > 0 && pl.pull > 0 {
		es2, lam2 = i*pl.pull, rate/float64(decodePods)
		if !(lam2*es2 < 1) {
			u := refuse(Saturated, fmt.Sprintf("the KV pull saturates the %d decode pods at %g r/s (utilization %g)",
				decodePods, rate, lam2*es2))
			return stageMix{}, psParts{}, &u
		}
	}
	base := max(i*pl.ownRate, es1+es2)
	var pull psStage
	if decodePods > 0 {
		pull = newPSStage(lam2, es2, splitCA2(pl.ca2, 1/float64(decodePods)), pl.cs2)
	}

	// The pod classes: each pod at its share of the arrivals (podShares),
	// the busiest at f/N_p; pooled, one class of every pod at 1/N_p. share
	// is the class's weight among the requests, pod one pod's share of the
	// arrivals.
	type class struct{ share, pod float64 }
	classes := []class{{share: 1, pod: 1 / n}}
	if !pooled && n > 1 {
		classes = classes[:0]
		for _, w := range podShares(pl.f, pl.p.Replicas) {
			classes = append(classes, class{share: w, pod: w})
		}
	}
	var mix stageMix
	var parts psParts
	parts.pull = pull.g * pull.es
	u := ePoly{{c: base, n: 1}}
	for _, c := range classes {
		if !(c.share > 0) {
			continue
		}
		pre := newPSStage(rate*c.pod, es1, splitCA2(pl.ca2, c.pod), pl.cs2)
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

// splitCA2 is the Ca^2 of the arrivals one pod takes at share p of a stream
// of Ca^2 ca2, p ca2 + 1 - p. A pod that takes a part of a bursty stream sees
// it less bursty, so each stage is priced at its pods' Ca^2, not the fleet's.
//
// It is exact for a renewal stream split at random (Bernoulli routing). A
// load-aware router, which sends the next request to the least loaded pod,
// spreads a burst wider than that, so there it errs safe. It is optimistic
// where a burst stays on one pod: prefix-affinity routing that keeps a
// session's or a shared prefix's requests together, and bursts that are not
// renewal (a modulated stream whose rate swings for minutes, which a split
// does not thin); there a pod sees more of the fleet's Ca^2 than p of it.
func splitCA2(ca2, p float64) float64 { return p*ca2 + 1 - p }

// podShares is each of n prefill pods' share of the arrivals at imbalance f
// (the busiest pod's share over the mean): the busiest at f/n, and the rest
// falling linearly by pod down to the least share that keeps the mean at
// 1/n, (2 - f)/n, or to 0 where that is negative (f > 2), the step then set so
// the shares still sum to 1. At n 2 it is f/2 and 1 - f/2, which f fixes.
//
// f fixes only the busiest pod's share; at two pods that fixes both, and the
// shares are exact. From three pods on the others can spread many ways.
// Spreading them evenly is the least spread they can have, and the stretch is
// convex in a pod's share, so even spreading is the population's most
// optimistic reading (about 10 % low at three pods, in review). The worst
// spread f allows, every other pod's share piled on as few pods as it can,
// reads far higher. The linear fall is a middle estimate, less optimistic
// than even spreading and not a bound, until the router's per-pod shares are
// read: at f 1.5 and 0.8 of the knee it prices the mean stretch 4.6 % above
// even at three pods and 9.6 % at four (TestPS_TheFallIsNoLowerThanEven).
func podShares(f float64, n int) []float64 {
	if n == 1 {
		return []float64{1}
	}
	top := f / float64(n)
	at := func(d float64) []float64 {
		out := make([]float64, n)
		for j := range out {
			out[j] = max(top-float64(j)*d, 0)
		}
		return out
	}
	d := 2 * (f - 1) / (float64(n) * float64(n-1))
	if top-float64(n-1)*d < 0 {
		// The fall reaches 0 before the last pod: the step whose clamped
		// shares sum to 1, by bisection (the sum falls as the step rises,
		// from f at 0 to top at top).
		lo, hi := 0.0, top
		for range 200 {
			mid := lo + (hi-lo)/2
			var sum float64
			for _, w := range at(mid) {
				sum += w
			}
			if sum > 1 {
				lo = mid
			} else {
				hi = mid
			}
		}
		d = hi
	}
	out := at(d)
	// The rest take what the busiest leaves, exactly.
	var rest float64
	for _, w := range out[1:] {
		rest += w
	}
	if rest > 0 {
		for j := 1; j < n; j++ {
			out[j] *= (1 - top) / rest
		}
	}
	return out
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
