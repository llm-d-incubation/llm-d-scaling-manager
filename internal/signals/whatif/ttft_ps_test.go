package whatif

import (
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// psFleet is the staircase's bucket B fleet (stairFleet) on the
// processor-sharing model, at prompt cv and arrivals' Ca^2, with no V_P
// margin: the tests here check the queueing, not the margin.
func psFleet(cv, ca2 float64) TTFTFleet {
	fl := stairFleet("B")
	fl.ProcessorSharing = true
	fl.Prefill.VPMargin = 0
	fl.Prefill.PromptCV = cv
	fl.Prefill.ArrivalCV2 = ca2
	return fl
}

// The expectation over the prompt is closed: E[u] = 1, E[u^2] = 1 + cv^2 and
// E[exp(-a u)] the gamma's Laplace transform, (1 + a cv^2)^(-1/cv^2); at cv 0
// the prompt is its mean.
func TestPS_ExpectationOverThePromptIsTheGammas(t *testing.T) {
	for _, cv := range []float64{0, 0.0577, 1, 3} {
		nearRel(t, "E[u]", ePoly{{c: 1, n: 1}}.expect(cv), 1, 1e-12)
		nearRel(t, "E[u^2]", ePoly{{c: 1, n: 2}}.expect(cv), 1+cv*cv, 1e-12)
		want := math.Exp(-0.7)
		if cv > 0 {
			want = math.Pow(1+0.7*cv*cv, -1/(cv*cv))
		}
		nearRel(t, "E[exp(-0.7 u)]", ePoly{{c: 1, a: 0.7}}.expect(cv), want, 1e-12)
	}
}

// One stage's extra at Ca^2 1: its mean is the M/G/1-PS sojourn's,
// x rho/(1 - rho) past the work; with exponential work its variance is the
// M/M/1-PS conditional variance (Coffman, Muntz and Trotter's),
// 2 rho x/(mu (1 - rho)^3) - 2 rho (1 - exp(-(1 - rho) mu x))/(mu^2 (1 - rho)^4); and for
// any work its small-x limit is rho x^2/(1 - rho)^2, x times the geometric
// number of others found on arrival.
func TestPS_OneStageIsTheKnownSojourn(t *testing.T) {
	const es = 0.0276
	for _, rho := range []float64{0.1, 0.5, 0.9} {
		st := newPSStage(rho/es, es, 1, 1)
		for _, u := range []float64{0.3, 1, 4} {
			x := u * es
			nearRel(t, "mean extra", st.extra().expect(0)*u, x*rho/(1-rho), 1e-12)
			mu := 1 / es
			d := 1 - rho
			mm1 := 2*rho*x/(mu*d*d*d) - 2*rho*(1-math.Exp(-d*mu*x))/(mu*mu*d*d*d*d)
			v := st.alpha*u - st.beta + st.beta*math.Exp(-st.decay*u)
			nearRel(t, "M/M/1-PS variance", v, mm1, 1e-9)
		}
		constant := newPSStage(rho/es, es, 1, 0)
		u := 1e-4
		v := constant.alpha*u - constant.beta + constant.beta*math.Exp(-constant.decay*u)
		nearRel(t, "small-x variance", v, rho*(u*es)*(u*es)/((1-rho)*(1-rho)), 1e-3)
	}
}

// At no load the PS side is the old work: the request's own time, a gamma at
// the prompt's spread, every percentile to the bit the first-come plan's at
// no load.
func TestPS_AtNoLoadIsTheOwnWork(t *testing.T) {
	for _, cv := range []float64{0, 0.0577, 1, 2} {
		for _, ca2 := range []float64{1, 4} {
			pl, u := psFleet(cv, ca2).plan()
			if u != nil {
				t.Fatal(u)
			}
			mix, parts, bad := pl.pf.psMix(0, pl.decodePods(), false)
			if bad != nil {
				t.Fatal(bad)
			}
			own := pl.pf.own()
			nearRel(t, "mean", mix.mean(), own.mean, 1e-12)
			nearRel(t, "stretch", parts.prefill+parts.pull, 0, 0)
			for _, alpha := range []float64{0.5, 0.1, 0.05, 0.01} {
				nearRel(t, "quantile", mix.quantile(alpha), own.quantile(1-alpha), 1e-9)
			}
			off := psFleet(cv, ca2)
			off.ProcessorSharing = false
			for _, q := range []float64{0, 0.9, 0.99} {
				on, old := psTTFT(t, psFleet(cv, ca2), 1e-9, q), psTTFT(t, off, 1e-9, q)
				nearRel(t, "TTFT at no load", on.Seconds, old.Seconds, 1e-6)
			}
		}
	}
}

// Toward the prefill knee the stretch runs away as 1/(1 - rho): the mean
// prefill stretch times (1 - rho_w) stays finite and positive, and the TTFT
// passes any bound; at the knee the price is refused.
func TestPS_DivergesAtTheKnee(t *testing.T) {
	fl := psFleet(0.0577, 1)
	fl.Decode.Replicas = 8 // the prefill knee the lower
	knee := psTTFT(t, fl, 1, 0.9).PrefillKnee
	var last float64
	for _, gap := range []float64{1e-2, 1e-3, 1e-4, 1e-5} {
		r := psTTFT(t, fl, knee*(1-gap), 0.9)
		if r.Seconds < 5*last {
			t.Errorf("at %g of the knee p90 %g s, under 5 times %g s at the step before", 1-gap, r.Seconds, last)
		}
		last = r.Seconds
		mean := psTTFT(t, fl, knee*(1-gap), 0)
		if s := mean.Parts.EngineWait * (1 - mean.PrefillUtilization); !(s > 0) || s > 1 {
			t.Errorf("at %g of the knee the stretch %g s times 1 - rho %g is %g", 1-gap, mean.Parts.EngineWait, mean.PrefillUtilization, s)
		}
	}
	if r := TTFTAt(fl, knee, 0.9); !isRefusal(r, Saturated) {
		t.Errorf("at the knee: %+v, want Saturated", r)
	}
}

// A pull that saturates its decode pods is refused, named, before either knee.
func TestPS_ASaturatedPullIsRefused(t *testing.T) {
	fl := psFleet(0.0577, 1)
	fl.Decode.Replicas = 1
	fl.Prefill.TransferSecondsPerToken = 2e-5 // 0.16 s a pull at 8000 tokens: 6.25 r/s a pod
	r := TTFTAt(fl, 7, 0.9)
	u, ok := r.(Unanswerable)
	if !ok || u.Reason != Saturated || !strings.Contains(u.Detail, "KV pull") {
		t.Fatalf("got %+v, want Saturated on the KV pull", r)
	}
	fl.ProcessorSharing = false
	if _, ok := TTFTAt(fl, 7, 0.9).(TTFT); !ok {
		t.Errorf("off, the pull is no stage and 7 r/s prices: %+v", TTFTAt(fl, 7, 0.9))
	}
}

// TTFT never falls as the rate rises, at the mean and every percentile, for
// constant to heavy prompts and Poisson to bursty arrivals.
func TestPS_MonotoneInTheRate(t *testing.T) {
	for _, cv := range []float64{0, 0.0577, 1, 3} {
		for _, ca2 := range []float64{1, 4} {
			fl := psFleet(cv, ca2)
			fl.Decode.Replicas = 8
			knee := psTTFT(t, fl, 1, 0).PrefillKnee
			for _, q := range []float64{0, 0.5, 0.9, 0.99} {
				prev := 0.0
				for i := 1; i <= 40; i++ {
					rate := knee * 0.98 * float64(i) / 40
					v := psTTFT(t, fl, rate, q).Seconds
					if v < prev*(1-1e-9) {
						t.Errorf("cv %g Ca^2 %g q %g: %g s at %g r/s, under %g s at the step before", cv, ca2, q, v, rate, prev)
					}
					prev = v
				}
			}
		}
	}
}

// The means add up: the PS stretches are the parts' EngineWait and
// KVPullWait, and the busiest pod's stretch is its x rho_w/(1 - rho_w),
// weighted by its share, with the other pod's.
func TestPS_TheMeanIsTheStretchesAndTheWork(t *testing.T) {
	fl := psFleet(0.0577, 1)
	rate := 28.3
	r := psTTFT(t, fl, rate, 0)
	p := fl.Prefill
	work := p.PromptTokens / p.VP
	w := p.Imbalance / 2
	rw, ro := rate*w*work, rate*(1-w)*work
	nearRel(t, "prefill stretch", r.Parts.EngineWait, w*work*rw/(1-rw)+(1-w)*work*ro/(1-ro), 1e-12)
	pull := p.PromptTokens * p.TransferSecondsPerToken
	rd := rate / 2 * pull
	nearRel(t, "pull stretch", r.Parts.KVPullWait, pull*rd/(1-rd), 1e-12)
	sum := r.Parts.EngineWait + r.Parts.KVPullWait + r.Parts.OwnPrefill + r.Parts.DecodeWait + r.Parts.DecodeEngine + r.Parts.RouterHop
	nearRel(t, "mean", r.Seconds, sum, 1e-12)
}

// D's per-token part comes net of the pull: on the staircase's term,
// 5.616e-6 s a token less the pull's 2.541e-6; a held D, with no per-token
// part, gives it up from its fixed part.
func TestPS_TheTermIsNetOfThePull(t *testing.T) {
	fixed, perToken := netOfPull(-0.00174, 5.6155e-6, 2.5412e-6, 8000)
	nearRel(t, "per token", perToken, 3.0743e-6, 1e-9)
	nearRel(t, "fixed", fixed, -0.00174, 0)
	fixed, perToken = netOfPull(0.05, 0, 2.5e-6, 8000)
	nearRel(t, "held per token", perToken, 0, 0)
	nearRel(t, "held fixed", fixed, 0.03, 1e-12)

	on, off := psFleet(0.0577, 1), psFleet(0.0577, 1)
	off.ProcessorSharing = false
	a, b := psTTFT(t, on, 1e-9, 0), psTTFT(t, off, 1e-9, 0)
	// At no load the pull's work moved from D into the own time: the sum is
	// the same, and the own time is the same, since both hold the pull.
	nearRel(t, "own", a.Parts.OwnPrefill, b.Parts.OwnPrefill, 1e-12)
	nearRel(t, "mean", a.Seconds, b.Seconds, 1e-9)
}

// The PS side against a tandem simulation of the same fleet: prefill pods at
// f, then the pull on the decode pods, each M/G/1-PS, at the staircase's
// buckets B, B-onset and C. The bands are what the method was measured to
// reach (ttft_ps.go): at Poisson below the onset the mean to 1 %, p90 to 2 %
// and p95 up to 5 % short; above it (four decode pods, rho_w to 0.84) every
// figure to 5 %; at Ca^2 4, where the stretch is an interpolation, 12 %.
func TestPS_AgainstATandemSimulation(t *testing.T) {
	type band struct{ lo, hi float64 }
	for _, c := range []struct {
		name           string
		ca2            float64
		decode         int
		rates          []float64
		mean, p90, p95 band
	}{
		{"B", 1, 2, []float64{13.317, 24.970, 28.299, 31.629}, band{-0.01, 0.01}, band{-0.02, 0.02}, band{-0.05, 0.01}},
		{"B-onset", 1, 4, []float64{39.110, 47.490, 53.636}, band{-0.05, 0.05}, band{-0.05, 0.05}, band{-0.05, 0.05}},
		{"C", 4, 2, []float64{13.317, 28.299, 31.629}, band{0, 0.12}, band{-0.12, 0.12}, band{-0.12, 0.12}},
	} {
		fl := psFleet(0.0577, c.ca2)
		fl.Decode.Replicas = c.decode
		pl, u := fl.plan()
		if u != nil {
			t.Fatal(u)
		}
		for _, rate := range c.rates {
			mix, _, bad := pl.pf.psMix(rate, c.decode, false)
			if bad != nil {
				t.Fatal(bad)
			}
			sim := simulateTandemPS(pl.pf, rate, c.decode, 200000, 1)
			for _, k := range []struct {
				what       string
				model, sim float64
				b          band
			}{
				{"mean", mix.mean(), sim.mean, c.mean},
				{"p90", mix.quantile(0.1), sim.quantile(0.9), c.p90},
				{"p95", mix.quantile(0.05), sim.quantile(0.95), c.p95},
			} {
				e := k.model/k.sim - 1
				t.Logf("%s %.3f r/s %s: model %.1f ms, simulation %.1f ms, %+.1f %%", c.name, rate, k.what, 1e3*k.model, 1e3*k.sim, 100*e)
				if e < k.b.lo || e > k.b.hi {
					t.Errorf("%s %.3f r/s %s: %+.1f %%, outside [%+.0f %%, %+.0f %%]", c.name, rate, k.what, 100*e, 100*k.b.lo, 100*k.b.hi)
				}
			}
		}
	}
}

// simSample is a simulated time's sample, sorted.
type simSample struct {
	mean float64
	v    []float64
}

func (s simSample) quantile(q float64) float64 { return s.v[int(q*float64(len(s.v)))] }

// simulateTandemPS runs n requests through the plan's PS stages at rate, the
// first tenth dropped as warm-up: arrivals Poisson, or a balanced-means
// two-phase hyperexponential at the plan's Ca^2; the prompt a gamma at the
// plan's cv; the busiest prefill pod picked at f/N_p and the others evenly;
// the decode pod uniformly, the pull arriving as the prefill ends. Each
// sample is the prompt's own work and both stages' stretch, as psMix prices.
func simulateTandemPS(pf prefillPlan, rate float64, decodePods, n int, seed uint64) simSample {
	rng := rand.New(rand.NewPCG(seed, 2))
	np := pf.p.Replicas
	es1, es2 := pf.work, pf.p.PromptTokens*pf.pull
	rest := pf.p.PromptTokens*pf.ownRate - es1 - es2
	arr := make([]float64, n)
	size := make([]float64, n)
	p := (1 + math.Sqrt((pf.ca2-1)/(pf.ca2+1))) / 2
	t := 0.0
	for i := range n {
		switch {
		case pf.ca2 == 1:
			t += rng.ExpFloat64() / rate
		case rng.Float64() < p:
			t += rng.ExpFloat64() / (2 * p * rate)
		default:
			t += rng.ExpFloat64() / (2 * (1 - p) * rate)
		}
		arr[i] = t
		size[i] = gammaSample(rng, pf.cv)
	}
	pod := func(worst float64, pods int) int {
		if pods == 1 || rng.Float64() < worst {
			return 0
		}
		return 1 + rng.IntN(pods-1)
	}
	t1 := tandemStage(arr, size, es1, np, func() int { return pod(pf.f/float64(np), np) })
	dep := make([]float64, n)
	for i := range dep {
		dep[i] = arr[i] + t1[i]
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return cmpFloat(dep[a], dep[b]) })
	arr2, size2 := make([]float64, n), make([]float64, n)
	for k, i := range order {
		arr2[k], size2[k] = dep[i], size[i]
	}
	t2s := tandemStage(arr2, size2, es2, decodePods, func() int { return rng.IntN(decodePods) })
	t2 := make([]float64, n)
	for k, i := range order {
		t2[i] = t2s[k]
	}
	var out simSample
	for i := n / 10; i < n; i++ {
		v := rest*size[i] + t1[i] + t2[i]
		out.v = append(out.v, v)
		out.mean += v
	}
	out.mean /= float64(len(out.v))
	slices.Sort(out.v)
	return out
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// tandemStage is each request's sojourn in a stage of pods M/G/1-PS servers,
// mean work es scaled by its size, the pod picked by pick; arrivals sorted.
func tandemStage(arr, size []float64, es float64, pods int, pick func() int) []float64 {
	res := make([]float64, len(arr))
	byPod := make([][]int, pods)
	for i := range arr {
		p := pick()
		byPod[p] = append(byPod[p], i)
	}
	for _, idx := range byPod {
		type job struct {
			i   int
			rem float64
		}
		var active []job
		now := 0.0
		next := 0
		for next < len(idx) || len(active) > 0 {
			na := math.Inf(1)
			if next < len(idx) {
				na = arr[idx[next]]
			}
			tf := math.Inf(1)
			least := -1
			if k := len(active); k > 0 {
				for j, a := range active {
					if least < 0 || a.rem < active[least].rem {
						least = j
					}
				}
				tf = now + active[least].rem*float64(k)
			}
			if na <= tf {
				if k := len(active); k > 0 {
					d := (na - now) / float64(k)
					for j := range active {
						active[j].rem -= d
					}
				}
				now = na
				active = append(active, job{i: idx[next], rem: es * size[idx[next]]})
				next++
				continue
			}
			d := active[least].rem
			for j := range active {
				active[j].rem -= d
			}
			now = tf
			kept := active[:0]
			for _, a := range active {
				if a.rem <= 1e-15 {
					res[a.i] = now - arr[a.i]
				} else {
					kept = append(kept, a)
				}
			}
			active = kept
		}
	}
	return res
}

// gammaSample is a gamma of mean 1 and coefficient of variation cv
// (Marsaglia and Tsang), 1 at cv 0.
func gammaSample(rng *rand.Rand, cv float64) float64 {
	if cv == 0 {
		return 1
	}
	k := 1 / (cv * cv)
	boost := 1.0
	if k < 1 {
		boost = math.Pow(rng.Float64(), 1/k)
		k++
	}
	d := k - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := rng.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := rng.Float64()
		if math.Log(u) < 0.5*x*x+d-d*v+d*math.Log(v) {
			return boost * d * v * cv * cv
		}
	}
}

// psTTFT is ttftAt's answer, failing the test on a refusal.
func psTTFT(t *testing.T, fl TTFTFleet, rate, q float64) TTFT {
	t.Helper()
	r, ok := ttftAt(t, fl, rate, q).(TTFT)
	if !ok {
		t.Fatalf("TTFTAt at %g r/s, q %g: %+v", rate, q, ttftAt(t, fl, rate, q))
	}
	return r
}

// nearRel fails when got is more than tol from want, relative to want
// (absolute at want 0).
func nearRel(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	scale := math.Abs(want)
	if scale == 0 {
		scale = 1
	}
	if !(math.Abs(got-want) <= tol*scale) {
		t.Errorf("%s: got %.12g, want %.12g (tol %g)", what, got, want, tol)
	}
}

// isRefusal reports whether r is an Unanswerable for reason.
func isRefusal(r Result, reason Reason) bool {
	u, ok := r.(Unanswerable)
	return ok && u.Reason == reason
}
