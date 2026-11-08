package whatif

import (
	"math"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The TTFT model's known-truth cases. None of them is a measurement: P2, the
// prefill-stress recording, is what will check the model against a real pod.
// What they pin is the arithmetic, against values computed without it:
// closed-form queues (M/M/1, Erlang's exact M/D/1, Pollaczek-Khinchine, Erlang C, the
// incomplete gamma's closed forms) and an independent numerical reference
// (scipy quad over a different form of the convolution) for the worked example
// and the cases with no closed form.

// workedPrefill is the worked example's prefill role
// (docs/proposals/slo-capacity-planner.md, "Worked example: ols-sweep"): one
// pod, V_P = 30,000 tok/s assumed, a = 10.1 + 2.5 ms as the page computed it,
// prompts of 1000 tokens with an assumed cv of 1.
func workedPrefill() Prefill {
	return Prefill{
		VP: 30000, VPAssumed: true, FixedLatency: 0.0126, Replicas: 1,
		PromptTokens: 1000, PromptCV: 1, PromptCVAssumed: true,
	}
}

// workedFleet is the worked example's fleet, flow control inferred off.
func workedFleet() TTFTFleet {
	d := olsSweep()
	return TTFTFleet{Prefill: workedPrefill(), Decode: &d, FlowControl: FlowControl{State: FlowControlNoSeries}}
}

func mustTTFT(t *testing.T, r Result) TTFT {
	t.Helper()
	v, ok := r.(TTFT)
	if !ok {
		t.Fatalf("want a TTFT, got %#v", r)
	}
	return v
}

func relNear(t *testing.T, what string, got, want, rel float64) {
	t.Helper()
	if !(math.Abs(got-want) <= rel*math.Abs(want)) {
		t.Errorf("%s = %.10g, want %.10g within %g relative", what, got, want, rel)
	}
}

// The page's worked example, against the independent reference's figures
// (scipy: 0.15996 / 0.15440 at p90, 0.23061 / 0.20999 at p95), which round to
// the page's 160 / 154 and 231 / 210 ms.
func TestTTFTWorkedExample(t *testing.T) {
	fl := workedFleet()
	op := 0.85 * mustAnswer(t, KneeRate(*fl.Decode)).Value
	p90 := mustTTFT(t, ttftAt(t, fl, op, 0.9))
	relNear(t, "TTFT p90", p90.Seconds, 0.1599611515667, 1e-6)
	relNear(t, "independent p90", p90.Independent, 0.15440219990354434, 1e-5)
	relNear(t, "rho", p90.PrefillUtilization, 0.3771893403059625, 1e-12)
	relNear(t, "theta", p90.WaitScale, 0.053520813773015215, 1e-12)
	relNear(t, "decode share", p90.DecodeShare, 0.03177463658731497, 1e-4)
	relNear(t, "floor p90", p90.Floor, 0.09157553643313485, 1e-9)
	p95 := mustTTFT(t, ttftAt(t, fl, op, 0.95))
	relNear(t, "TTFT p95", p95.Seconds, 0.2306087421668428, 1e-6)
	relNear(t, "independent p95", p95.Independent, 0.20999230485834747, 1e-5)
	for _, f := range []Flag{UnvalidatedTTFT, AssumedVP, AssumedPromptSpread, TransferOmitted, FlowControlInferredOff} {
		if !p90.HasFlag(f) {
			t.Errorf("worked example not flagged %s: %v", f, p90.Flags)
		}
	}
	// One pod, no imbalance: the pooled queue is the pod's own.
	relNear(t, "pooled p90 on one pod", p90.Pooled, p90.Seconds, 1e-12)
}

// Question 1 on the worked example: 200 ms gives 11.64 r/s at p90 and 11.10
// at p95 (the reference: 11.63853, 11.10495).
func TestTTFTWorkedExample_RateForTTFT(t *testing.T) {
	fl := workedFleet()
	for _, c := range []struct{ q, want float64 }{{0.9, 11.638534256575051}, {0.95, 11.104948214254135}} {
		a := mustAnswer(t, rateForTTFT(t, fl, TTFTTarget{Seconds: 0.2, Percentile: c.q}))
		relNear(t, "rate for 200 ms", a.Value, c.want, 1e-5)
		// The decode wait takes the tail's first share, so the prefill side
		// alone would carry more.
		if !(a.Basis.TTFT.PrefillOnlyRate > a.Value) || !a.HasFlag(HeldByDecodeAdmission) ||
			!slices.Equal(a.Basis.BoundBy, []Bound{BoundTTFT, BoundDecodeAdmission}) {
			t.Errorf("q %g: prefill alone %g, bound %v, flags %v", c.q, a.Basis.TTFT.PrefillOnlyRate, a.Basis.BoundBy, a.Flags)
		}
		relNear(t, "knee", a.Basis.TTFT.KneeRate, 13.312564951975148, 1e-9)
		back := mustTTFT(t, ttftAt(t, fl, a.Value, c.q))
		relNear(t, "TTFT back at the rate", back.Seconds, 0.2, 1e-6)
	}
}

// Flow control's onsets on the worked example: decode 12.50 r/s, prefill
// 25.6 (the reference: the chain's mean waiting count reaches 5 at 12.50098;
// rho^2/(1 - rho) = 5 at rho = 0.854102).
func TestFlowControlOnsets(t *testing.T) {
	fl := workedFleet()
	fl.FlowControl = FlowControl{State: FlowControlOn, QueueThreshold: 5, KVThreshold: 0.8}
	on, ok := FlowControlOnsets(fl).(Onsets)
	if !ok {
		t.Fatalf("no onsets: %#v", FlowControlOnsets(fl))
	}
	relNear(t, "decode onset", on.Decode, 12.500975816826703, 1e-6)
	relNear(t, "prefill onset", on.Prefill, 25.623058987490534, 1e-6)

	// At or above the decode onset TTFT is not priced; just below it, it is.
	if u, ok := ttftAt(t, fl, on.Decode, 0.9).(Unanswerable); !ok || u.Reason != AboveDecodeOnset {
		t.Errorf("at the decode onset: %#v", ttftAt(t, fl, on.Decode, 0.9))
	}
	below := mustTTFT(t, ttftAt(t, fl, 0.99*on.Decode, 0.9))
	off := mustTTFT(t, ttftAt(t, workedFleet(), 0.99*on.Decode, 0.9))
	// Below both onsets flow control changes nothing.
	if below.Seconds != off.Seconds || below.HasFlag(AboveFlowControlOnset) {
		t.Errorf("below the onsets: on %g off %g, flags %v", below.Seconds, off.Seconds, below.Flags)
	}
	// Question 1 caps at the decode onset: a loose target reaches it.
	a := mustAnswer(t, rateForTTFT(t, fl, TTFTTarget{Seconds: 30, Percentile: 0.9}))
	relNear(t, "rate capped at the onset", a.Value, on.Decode, 1e-12)
	if !slices.Equal(a.Basis.BoundBy, []Bound{BoundDecodeOnset}) {
		t.Errorf("bound %v, want the decode onset", a.Basis.BoundBy)
	}
	// Without flow control the same target is met up to near the knee.
	if b := mustAnswer(t, rateForTTFT(t, workedFleet(), TTFTTarget{Seconds: 30, Percentile: 0.9})); !(b.Value > on.Decode) {
		t.Errorf("flow control off: %g r/s, not above the onset %g", b.Value, on.Decode)
	}

	// A slower prefill pod moves its onset below decode's: at or above it the
	// answer is flagged, and below it the answer is not.
	slow := fl
	slow.Prefill.VP = 12000 // knee 12 r/s, onset 0.854 * 12 = 10.25
	son := FlowControlOnsets(slow).(Onsets)
	relNear(t, "slow prefill onset", son.Prefill, 0.8541019662496845*12, 1e-6)
	if v := mustTTFT(t, ttftAt(t, slow, 11, 0.9)); !v.HasFlag(AboveFlowControlOnset) {
		t.Errorf("above the prefill onset, not flagged: %v", v.Flags)
	}
	if v := mustTTFT(t, ttftAt(t, slow, 9, 0.9)); v.HasFlag(AboveFlowControlOnset) {
		t.Errorf("below the prefill onset, flagged: %v", v.Flags)
	}
	// Flow control on with a threshold unknown is refused.
	unknown := fl
	unknown.FlowControl.KVThreshold = 0
	if u, ok := ttftAt(t, unknown, 5, 0.9).(Unanswerable); !ok || u.Reason != FlowControlUnknown {
		t.Errorf("unknown thresholds: %#v", ttftAt(t, unknown, 5, 0.9))
	}
	// Several prefill pods: the busiest at f, the rest sharing the remainder,
	// scored by each pod's mean waiting count rho^2 (Ca^2 + Cs^2)/(2(1 - rho))
	// (here Cs^2 = 1, Ca^2 = 1) and averaged. The onset is where that mean
	// reaches the threshold; found here by its own bisection.
	imb := fl
	imb.Prefill.VP, imb.Prefill.Replicas, imb.Prefill.Imbalance = 12000, 3, 1.8
	lq := func(rho float64) float64 { return rho * rho * 2 / (2 * (1 - rho)) }
	score := func(rate float64) float64 {
		rb := rate * 1000 / 12000 / 3 // the mean pod's utilization
		rw, ro := 1.8*rb, rb*(3-1.8)/2
		if rw >= 1 {
			return math.Inf(1)
		}
		return (lq(rw) + 2*lq(ro)) / 3 / 5
	}
	lo, hi := 0.0, 3*12.0/1.8
	for range 200 {
		mid := (lo + hi) / 2
		if score(mid) < 1 {
			lo = mid
		} else {
			hi = mid
		}
	}
	relNear(t, "imbalanced prefill onset", FlowControlOnsets(imb).(Onsets).Prefill, hi, 1e-9)

	// A KV threshold below the fill the chain reaches before its queue
	// holds 5: the KV term sets the decode onset, where the chain's mean
	// fill reaches the threshold.
	kv := fl
	kv.FlowControl.KVThreshold = 0.3
	kon := FlowControlOnsets(kv).(Onsets).Decode
	fillAt := func(rate float64) float64 { return DecodeAdmission(*kv.Decode, rate).(Admission).Basis.Fill }
	relNear(t, "KV-bound decode onset's fill", fillAt(kon), 0.3, 1e-9)
	if !(kon < on.Decode) {
		t.Errorf("KV-bound onset %g not below the queue-bound %g", kon, on.Decode)
	}

	// Off, the onsets are zero.
	if on := FlowControlOnsets(workedFleet()).(Onsets); on != (Onsets{}) {
		t.Errorf("flow control off: onsets %+v", on)
	}
}

// prefillOnly is one pod, no decode side, Poisson arrivals.
func prefillOnly(cv float64) TTFTFleet {
	p := workedPrefill()
	p.PromptCV = cv
	return TTFTFleet{Prefill: p}
}

// At cv 1, f 1, one pod and Poisson arrivals the pod is M/M/1, and the
// exponential tail is exact: wait plus own work is exponential at mu - lambda,
// so TTFT_q = a + ln(1/(1-q))/(mu - lambda), and the mean a + 1/(mu - lambda).
func TestTTFTIsMM1Sojourn(t *testing.T) {
	fl := prefillOnly(1)
	mu := fl.Prefill.VP / fl.Prefill.PromptTokens
	for _, lam := range []float64{0, 1, 5, 15, 25, 29, 29.9} {
		for _, q := range []float64{0.5, 0.9, 0.99, 0.999} {
			v := mustTTFT(t, ttftAt(t, fl, lam, q))
			relNear(t, "M/M/1 quantile", v.Seconds, fl.Prefill.FixedLatency+math.Log(1/(1-q))/(mu-lam), 1e-9)
		}
		m := mustTTFT(t, ttftAt(t, fl, lam, 0))
		relNear(t, "M/M/1 mean", m.Seconds, fl.Prefill.FixedLatency+1/(mu-lam), 1e-12)
		if !m.HasFlag(DecodeSideOmitted) {
			t.Errorf("no decode side, not flagged: %v", m.Flags)
		}
	}
}

// With f = 1 and one pod the mean engine wait is Pollaczek-Khinchine,
// lambda E[S^2] / (2(1 - rho)), with E[S^2] = E[S]^2 (1 + cv^2). The
// quantiles are checked against Erlang's exact M/D/1 wait (TestMD1AgainstErlang)
// and a Lindley simulation (TestTailNotBelowLindleySimulation), not against the
// model's own tail formula.
func TestEngineWaitIsPollaczekKhinchine(t *testing.T) {
	for _, cv := range []float64{0, 0.3, 1, 2.5} {
		fl := prefillOnly(cv)
		es := fl.Prefill.PromptTokens / fl.Prefill.VP
		for _, lam := range []float64{3, 15, 27} {
			v := mustTTFT(t, ttftAt(t, fl, lam, 0))
			rho := lam * es
			relNear(t, "PK mean wait", v.Parts.EngineWait, lam*es*es*(1+cv*cv)/(2*(1-rho)), 1e-12)
			relNear(t, "mean", v.Seconds, fl.Prefill.FixedLatency+es+v.Parts.EngineWait, 1e-12)
		}
	}
}

// erlangMD1 is P(W <= t) for M/D/1 with unit service at utilization rho,
// Erlang's formula (1 - rho) sum_{k <= t} (-x_k)^k e^(x_k) / k!, x_k =
// rho (t - k). Its terms alternate and reach about e^(2 rho t), 1e106 at
// t = 128 and rho 0.95, against a sum near 1e-3, so it is summed in 1024-bit
// floats (about 300 digits) with x_k exact in them: a float64 x_k carries a
// 1e-16 error that e^x x^k carries into digits the sum needs. e^(x_k) is
// e^(rho t) e^(-rho k), one product a term.
func erlangMD1(t, rho float64) float64 {
	const prec = 1024
	nf := func(v float64) *big.Float { return new(big.Float).SetPrec(prec).SetFloat64(v) }
	r := nf(rho)
	ex := bigExp(new(big.Float).SetPrec(prec).Mul(r, nf(t)), prec)
	down := new(big.Float).SetPrec(prec).Quo(nf(1), bigExp(r, prec)) // e^(-rho)
	sum := nf(0)
	for k := 0; float64(k) <= t; k++ {
		x := new(big.Float).SetPrec(prec).Sub(nf(t), nf(float64(k)))
		x.Mul(x, r)
		term := new(big.Float).SetPrec(prec).Set(ex)
		for i := 1; i <= k; i++ {
			term.Mul(term, x)
			term.Quo(term, nf(float64(i)))
		}
		if k%2 == 1 {
			term.Neg(term)
		}
		sum.Add(sum, term)
		ex.Mul(ex, down)
	}
	f, _ := sum.Float64()
	return (1 - rho) * f
}

// bigExp is e^x for x >= 0: the Taylor series at x/2^m < 1, squared m times.
func bigExp(x *big.Float, prec uint) *big.Float {
	m := 0
	y := new(big.Float).SetPrec(prec).Set(x)
	for y.Cmp(big.NewFloat(1)) > 0 {
		y.Quo(y, big.NewFloat(2))
		m++
	}
	sum := new(big.Float).SetPrec(prec).SetInt64(1)
	term := new(big.Float).SetPrec(prec).SetInt64(1)
	eps := new(big.Float).SetPrec(prec).SetMantExp(big.NewFloat(1), -int(prec))
	for n := int64(1); n < 10000; n++ {
		term.Mul(term, y)
		term.Quo(term, new(big.Float).SetPrec(prec).SetInt64(n))
		sum.Add(sum, term)
		if new(big.Float).Abs(term).Cmp(new(big.Float).Mul(eps, sum)) < 0 {
			break
		}
	}
	for range m {
		sum.Mul(sum, sum)
	}
	return sum
}

// At cv 0 the pod is M/D/1, whose exact wait is Erlang's. The model's TTFT,
// less a and the constant own time, is the wait's quantile, and must never be
// below the exact one: an under-stated wait is too few replicas. The case
// that set a bounded body (Taylor's) at cv < 1: an exponential body behind the atom
// read the p90 wait at rho 0.2 as 0.43 of a service time, exact 0.59.
func TestMD1AgainstErlang(t *testing.T) {
	// The formula's own closed points: P(W = 0) = 1 - rho, P(W <= 1) =
	// (1 - rho) e^rho, P(W <= 2) = (1 - rho)(e^(2 rho) - rho e^rho).
	relNear(t, "P(W <= 0)", erlangMD1(0, 0.5), 0.5, 1e-15)
	relNear(t, "P(W <= 1)", erlangMD1(1, 0.5), 0.5*math.Exp(0.5), 1e-12)
	relNear(t, "P(W <= 2)", erlangMD1(2, 0.5), 0.5*(math.Exp(1)-0.5*math.Exp(0.5)), 1e-12)
	fl := prefillOnly(0)
	es := fl.Prefill.PromptTokens / fl.Prefill.VP
	// rho 0.95 at p999 waits about 70 service times: the bracket runs to 128,
	// where the 1024-bit sum still holds the terms' e^(2 rho t).
	for _, rho := range []float64{0.1, 0.2, 0.3, 0.5, 0.7, 0.8, 0.9, 0.95} {
		for _, q := range []float64{0.9, 0.99, 0.999} {
			exact := 0.0
			if rho > 1-q {
				lo, hi := 0.0, 1.0
				for erlangMD1(hi, rho) < q {
					if lo, hi = hi, 2*hi; hi > 128 {
						t.Fatalf("rho %g q %g: no bracket below 128 service times", rho, q)
					}
				}
				for range 50 {
					mid := (lo + hi) / 2
					if erlangMD1(mid, rho) < q {
						lo = mid
					} else {
						hi = mid
					}
				}
				exact = hi
			}
			v := mustTTFT(t, ttftAt(t, fl, rho/es, q))
			model := (v.Seconds - fl.Prefill.FixedLatency - es) / es
			if !(model >= exact*(1-1e-9)) {
				t.Errorf("rho %g q %g: wait %.6f service times, Erlang's exact %.6f", rho, q, model, exact)
			}
			t.Logf("rho %g q %g: model wait %.4f, exact %.4f, sojourn ratio %.4f", rho, q, model, exact, (model+1)/(exact+1))
		}
	}
}

// lindleyRef are sojourn (wait plus own work) quantiles, p90 / p99 / p999, of
// M/G/1 with gamma work of mean 1 and Poisson arrivals, from a Lindley
// simulation outside the code (numpy; 3M arrivals after 100k of warm-up; seeds
// 20261104, 7, 99 and 4242). Each figure is the least of the four seeds; the
// comment is the spread across them, (max - min)/min. The widest is 6.5%, at
// cv 3, rho 0.6, p999; the model must reach the least seed less 1%.
var lindleyRef = []struct {
	cv, rho float64
	q       [3]float64
}{
	{2, 0.3, [3]float64{6.124896, 15.876903, 25.727312}},  // spread p90 0.71% p99 0.62% p999 0.40%
	{2, 0.6, [3]float64{12.865078, 28.673734, 43.635434}}, // spread p90 1.61% p99 0.81% p999 1.90%
	{3, 0.3, [3]float64{10.104518, 30.387253, 51.565322}}, // spread p90 0.82% p99 1.29% p999 0.69%
	{3, 0.6, [3]float64{24.624507, 57.303451, 90.211524}}, // spread p90 0.79% p99 1.78% p999 6.48%
	{1, 0.6, [3]float64{5.747450, 11.477355, 17.195630}},  // spread p90 0.24% p99 0.41% p999 0.89%
	{0.5, 0.2, [3]float64{2.006231, 3.332983, 4.653273}},  // spread p90 0.20% p99 0.45% p999 0.71%
	{0.5, 0.3, [3]float64{2.256146, 3.875959, 5.502411}},  // spread p90 0.33% p99 0.71% p999 0.60%
	{0.5, 0.6, [3]float64{3.788792, 7.028987, 10.200655}}, // spread p90 0.70% p99 0.67% p999 3.37%
	{0, 0.2, [3]float64{1.587076, 2.430570, 3.269501}},    // spread p90 0.12% p99 0.31% p999 0.63%
	{0, 0.3, [3]float64{1.836400, 2.884717, 4.005296}},    // spread p90 0.10% p99 0.40% p999 0.59%
	{0, 0.6, [3]float64{3.094491, 5.512832, 7.854190}},    // spread p90 0.17% p99 0.31% p999 1.70%
}

// lindleyQuantiles are the quantiles lindleyRef holds.
var lindleyQuantiles = [3]float64{0.9, 0.99, 0.999}

// The model's prefill sojourn is never below the simulation's at p90, p99 or
// p999. Before round 1 of ev-shindin/llm-scaling-manager#471, at cv above 1, Kingman's exponential decay
// alone read the p99 at 0.94/0.94 (cv 2, rho 0.3/0.6) and 0.89/0.91 (cv 3) of
// the simulation, too few replicas by up to 10%.
func TestTailNotBelowLindleySimulation(t *testing.T) {
	const tol = 0.01
	for _, c := range lindleyRef {
		fl := prefillOnly(c.cv)
		es := fl.Prefill.PromptTokens / fl.Prefill.VP
		for i, q := range lindleyQuantiles {
			v := mustTTFT(t, ttftAt(t, fl, c.rho/es, q))
			model := (v.Seconds - fl.Prefill.FixedLatency) / es
			if !(model >= c.q[i]*(1-tol)) {
				t.Errorf("cv %g rho %g q %g: sojourn %.4f mean works, simulation's least seed %.4f (ratio %.4f)", c.cv, c.rho, q, model, c.q[i], model/c.q[i])
			}
			t.Logf("cv %g rho %g q %g: model/sim %.4f", c.cv, c.rho, q, model/c.q[i])
		}
	}
}

// The Cramer-Lundberg root: mu - lambda at cv 1 (M/M/1), and otherwise the
// root of lambda (M(g) - 1) = g, checked by the equation itself; none with no
// arrivals.
func TestLundbergRoot(t *testing.T) {
	for _, lam := range []float64{0.1, 0.5, 0.9} {
		relNear(t, "M/M/1 root", lundbergRoot(lam, 1, 1), 1-lam, 1e-12)
		for _, cv := range []float64{0, 0.3, 0.5, 2, 3} {
			g := lundbergRoot(lam, 1, cv)
			m := math.Exp(g)
			if cv > 0 {
				m = math.Pow(1-g*cv*cv, -1/(cv*cv))
			}
			if !(g > 0) || math.Abs(lam*(m-1)-g) > 1e-9*g {
				t.Errorf("lam %g cv %g: root %g leaves %g", lam, cv, g, lam*(m-1)-g)
			}
		}
	}
	// Near the knee the root falls toward 0 like 2(1 - rho)/(E[S](1 + cv^2))
	// (heavy traffic); it must stay positive, solve the equation, and fall as
	// rho rises.
	for _, cv := range []float64{0, 0.5, 1, 2} {
		prev := math.Inf(1)
		for _, lam := range []float64{0.95, 0.99, 0.999, 0.9999} {
			g := lundbergRoot(lam, 1, cv)
			m := math.Exp(g)
			if cv > 0 {
				m = math.Pow(1-g*cv*cv, -1/(cv*cv))
			}
			ht := 2 * (1 - lam) / (1 + cv*cv)
			if !(g > 0) || !(g < prev) || math.Abs(lam*(m-1)-g) > 1e-6*g || math.Abs(g-ht) > 0.2*ht {
				t.Errorf("rho %g cv %g: root %g (heavy traffic %g), residual %g", lam, cv, g, ht, lam*(m-1)-g)
			}
			prev = g
		}
	}
	if !math.IsInf(lundbergRoot(0, 1, 0.5), 1) {
		t.Error("a root with no arrivals")
	}
	// No root to find: NaN, never a number.
	for name, g := range map[string]float64{
		"rho 1": lundbergRoot(1, 1, 0.5), "rho above 1": lundbergRoot(2, 1, 0),
		"no work": lundbergRoot(0.5, 0, 1), "NaN work": lundbergRoot(0.5, math.NaN(), 1),
	} {
		if !math.IsNaN(g) {
			t.Errorf("%s: root %g, want NaN", name, g)
		}
	}
}

// The special functions never hand back a partial sum: no bracket or no
// convergence is NaN.
func TestSpecialFunctionsRefuseWithNaN(t *testing.T) {
	for name, v := range map[string]float64{
		"gammaQuantile at NaN shape": gammaQuantile(math.NaN(), 0.9),
		"logSeries at +Inf":          logSeries(1, math.Inf(1)),
		"lnGammaPSeries at NaN":      lnGammaPSeries(1, math.NaN()),
		// P(k, x) never reaches 1.5: no bracket, NaN rather than the last
		// doubling.
		"gammaQuantile above 1": gammaQuantile(1, 1.5),
		// The terms still rise at n = 1e6 (they peak near n = y), so the
		// series is cut before it converges: NaN, not the partial sum.
		"logSeries cut before it converges": logSeries(1, 1e7),
	} {
		if !math.IsNaN(v) {
			t.Errorf("%s: %g, want NaN", name, v)
		}
	}
	// And a sum of shapes it does not take is refused, not a panic.
	two, _ := expWait(0.3, 2).sum(expWait(0.2, 3))
	if _, ok := two.sum(expWait(0.1, 4)); ok {
		t.Error("summed a three-piece wait")
	}
	if _, ok := expWait(0.3, 2).sum(boundWait(0.2, 0.5, 3)); ok {
		t.Error("summed a shifted second wait")
	}
}

// Away from Poisson arrivals the wait is the Poisson tail stretched by Kingman's
// ratio, but never decays faster than one request's work does, 1/(cv^2 E[S]);
// and the bounded (shifted) body keeps its form, so nothing jumps at Ca^2 1. At
// cv 1 the root is 1/theta, so the stretch is Kingman's own figure.
func TestBurstyTailIsPoissonStretched(t *testing.T) {
	for _, c := range []struct{ cv, ca2, rho float64 }{{3, 1.5, 0.1}, {3, 0.5, 0.3}, {2, 2, 0.2}, {0.5, 2, 0.5}, {0.7, 0.1, 0.1}, {1, 2, 0.4}} {
		fl := prefillOnly(c.cv)
		es := fl.Prefill.PromptTokens / fl.Prefill.VP
		poisson := mustTTFT(t, ttftAt(t, fl, c.rho/es, 0.99))
		fl.Prefill.ArrivalCV2 = c.ca2
		v := mustTTFT(t, ttftAt(t, fl, c.rho/es, 0.99))
		s := (c.ca2 + c.cv*c.cv) / (1 + c.cv*c.cv)
		scale := max(s*poisson.WaitScale, c.cv*c.cv*es)
		relNear(t, "wait scale", v.WaitScale, scale, 1e-12)
		relNear(t, "offset", v.WaitOffset, poisson.WaitOffset*scale/poisson.WaitScale, 1e-12)
		if c.cv == 1 {
			relNear(t, "Kingman at cv 1", v.WaitScale, es*(c.ca2+1)/(2*(1-c.rho)), 1e-9)
		}
	}
}

// A TTFT answer leaves the TPOT fields of Basis at 0 and carries its own.
func TestRateForTTFTLeavesTPOTFieldsZero(t *testing.T) {
	for _, fl := range []TTFTFleet{workedFleet(), prefillOnly(1)} {
		a := mustAnswer(t, rateForTTFT(t, fl, TTFTTarget{Seconds: 0.2, Percentile: 0.9}))
		if a.Basis.KneeRate != 0 || a.Basis.Floor != 0 || a.Basis.Uncapped != 0 {
			t.Errorf("TPOT fields set: %+v", a.Basis)
		}
		if a.Basis.TTFT == nil || !(a.Basis.TTFT.Floor > 0) || !(a.Basis.TTFT.KneeRate > a.Value) || !(a.Basis.TTFT.PrefillOnlyRate >= a.Value) {
			t.Errorf("TTFT basis %+v at %g r/s", a.Basis.TTFT, a.Value)
		}
	}
	k := mustAnswer(t, PrefillKnee(workedPrefill()))
	if k.Basis.KneeRate != 0 || k.Basis.TTFT == nil || k.Basis.TTFT.KneeRate != k.Value {
		t.Errorf("prefill knee basis %+v", k.Basis)
	}
	// A decode answer has no TTFT basis.
	if b := mustAnswer(t, KneeRate(olsSweep())); b.Basis.TTFT != nil {
		t.Errorf("decode knee carries a TTFT basis: %+v", b.Basis.TTFT)
	}
}

// The prefill side alone with flow control on cannot place the decode onset:
// it answers, flagged, and does not refuse.
func TestDecodeOnsetUnchecked(t *testing.T) {
	fl := prefillOnly(1)
	fl.FlowControl = FlowControl{State: FlowControlOn, QueueThreshold: 5, KVThreshold: 0.8}
	v := mustTTFT(t, ttftAt(t, fl, 10, 0.9))
	a := mustAnswer(t, rateForTTFT(t, fl, TTFTTarget{Seconds: 0.2, Percentile: 0.9}))
	if !v.HasFlag(DecodeOnsetUnchecked) || !a.HasFlag(DecodeOnsetUnchecked) {
		t.Errorf("not flagged: %v / %v", v.Flags, a.Flags)
	}
	// With the decode side, or with flow control off, it is not raised.
	for name, f := range map[string]TTFTFleet{"decode given": func() TTFTFleet {
		w := workedFleet()
		w.FlowControl = fl.FlowControl
		return w
	}(), "flow control off": prefillOnly(1)} {
		if v := mustTTFT(t, ttftAt(t, f, 5, 0.9)); v.HasFlag(DecodeOnsetUnchecked) {
			t.Errorf("%s: flagged %v", name, v.Flags)
		}
	}
}

// One pod with no f is priced exactly as one pod with f = 1.
func TestOnePodNeedsNoImbalance(t *testing.T) {
	none, one := prefillOnly(1), prefillOnly(1)
	one.Prefill.Imbalance = 1
	for _, q := range []float64{0, 0.9, 0.99} {
		a, b := mustTTFT(t, ttftAt(t, none, 20, q)), mustTTFT(t, ttftAt(t, one, 20, q))
		if a.Seconds != b.Seconds || a.PrefillKnee != b.PrefillKnee || a.PrefillUtilization != b.PrefillUtilization {
			t.Errorf("q %g: no f %+v, f 1 %+v", q, a, b)
		}
	}
	ra := mustAnswer(t, rateForTTFT(t, none, TTFTTarget{Seconds: 0.2, Percentile: 0.9}))
	rb := mustAnswer(t, rateForTTFT(t, one, TTFTTarget{Seconds: 0.2, Percentile: 0.9}))
	if ra.Value != rb.Value {
		t.Errorf("rate: no f %g, f 1 %g", ra.Value, rb.Value)
	}
}

// Just above minPromptCV the gamma is priced, and it agrees with the constant
// prompt to within the spread itself: no step at the threshold.
func TestPromptCVThresholdBoundary(t *testing.T) {
	for _, q := range []float64{0, 0.9, 0.99} {
		for _, rate := range []float64{0, 10, 25} {
			c := mustTTFT(t, ttftAt(t, prefillOnly(0), rate, q))
			for _, cv := range []float64{2e-4, 5e-3} {
				v := mustTTFT(t, ttftAt(t, prefillOnly(cv), rate, q))
				relNear(t, "TTFT just above the threshold", v.Seconds, c.Seconds, 1e-2)
				relNear(t, "floor just above the threshold", v.Floor, c.Floor, 1e-2)
			}
		}
	}
}

// Above minPromptCV the own time really is the gamma, and accurate: its
// p99 sits z_0.99 * cv above the mean (normal to first order, the gamma's
// skew 2 cv adding (z^2 - 1) cv^2 / 3), not at the mean as a constant would.
func TestSmallPromptCVIsPricedAsGamma(t *testing.T) {
	const z = 2.3263478740408408 // the standard normal's 0.99 quantile
	for _, cv := range []float64{2e-4, 5e-3} {
		fl := prefillOnly(cv)
		pl, u := fl.plan()
		if u != nil {
			t.Fatal(u)
		}
		own := pl.pf.own()
		if own.det {
			t.Fatalf("cv %g priced as constant", cv)
		}
		want := z*cv + (z*z-1)*cv*cv/3
		relNear(t, "own-time p99 above its mean", own.quantile(0.99)/own.mean-1, want, 1e-2)
	}
}

// The wait-tail guard in price: a plan whose work no longer matches its knee
// (only a tampered plan; the knee check stands in front of it on every real
// one) puts rho at 2, where no Cramer-Lundberg root exists. Kingman's mean
// there is negative and finite, so without the guard the mean would be
// priced; it is refused.
func TestPriceRefusesAnUnreadableWaitTail(t *testing.T) {
	pl, u := prefillOnly(1).plan()
	if u != nil {
		t.Fatal(u)
	}
	pl.pf.work = 2 // one pod, f 1: rho = rate * work = 2 at 1 r/s
	for _, q := range []float64{0, 0.9} {
		if p := pl.price(1, q, true, false); p.u == nil || p.u.Reason != Saturated {
			t.Errorf("q %g: priced %+v, want Saturated", q, p.out)
		}
	}
}

// A prompt spread below minPromptCV prices as constant: cv 1e-7 read 8% high
// as a gamma (its shape 1e14 cancels in the incomplete gamma).
func TestTinyPromptCVIsConstant(t *testing.T) {
	for _, q := range []float64{0, 0.9, 0.99} {
		for _, rate := range []float64{0, 10, 25} {
			c := mustTTFT(t, ttftAt(t, prefillOnly(0), rate, q))
			for _, cv := range []float64{1e-7, 5e-5} {
				v := mustTTFT(t, ttftAt(t, prefillOnly(cv), rate, q))
				relNear(t, "TTFT at a tiny cv", v.Seconds, c.Seconds, 1e-3)
				relNear(t, "floor at a tiny cv", v.Floor, c.Floor, 1e-3)
			}
		}
	}
}

// Cases with no closed form, against the independent reference (scipy quad,
// with the engineWait rule written again in Python: the Cramer-Lundberg root
// by brentq, Taylor's body at cv < 1 as a shifted exponential) over
// P(W+S>t) = (1-p) sf(t) + p [exp(-rt) + int r exp(-ru) sf(t-u) du], a
// different form from the code's incomplete-gamma closed form): prompt
// spreads either side of 1, bursty arrivals with an imbalance across four
// pods and two decode replicas, a KV transfer term, and the prefill side
// alone at cv 1 and cv 0.
func TestTTFTAgainstReference(t *testing.T) {
	cases := []struct {
		name                        string
		rate, q, a, vp, il, cv, ca2 float64
		f                           float64
		np, nd                      int
		xfer, wantSeconds, wantMean float64
	}{
		{"cv0.5", 9, 0.9, 0.011, 30000, 1000, 0.5, 1, 1, 1, 1, 0, 0.09770488035635884, 0.0561332049162314},
		{"cv2", 9, 0.95, 0.011, 30000, 1000, 2, 1, 1, 1, 1, 0, 0.35241177380978406, 0.08291891920194568},
		{"bursty-imbalanced", 20, 0.9, 0.011, 30000, 1000, 1, 2, 1.5, 4, 2, 0, 0.13239144522086338, 0.06437997302707862},
		{"transfer", 8, 0.99, 0.02, 20000, 2000, 0.8, 1, 1, 1, 1, 2e-6, 1.9241895307941992, 0.45462260548736116},
		{"prefill-only", 25, 0.9, 0.011, 30000, 1000, 1, 1, 1, 1, 0, 0, 0.47151701859880935, 0.21100000000000005},
		{"constant-prompt", 25, 0.9, 0.011, 30000, 1000, 0, 1, 1, 1, 0, 0, 0.2610272810764572, 0.12766666666666668},
	}
	for _, c := range cases {
		fl := TTFTFleet{Prefill: Prefill{VP: c.vp, FixedLatency: c.a, Imbalance: c.f, Replicas: c.np,
			PromptTokens: c.il, PromptCV: c.cv, ArrivalCV2: c.ca2, TransferSecondsPerToken: c.xfer}}
		if c.nd > 0 {
			d := olsSweep()
			d.Replicas = c.nd
			fl.Decode = &d
		}
		v := mustTTFT(t, ttftAt(t, fl, c.rate, c.q))
		relNear(t, c.name+" quantile", v.Seconds, c.wantSeconds, 2e-6)
		m := mustTTFT(t, ttftAt(t, fl, c.rate, 0))
		relNear(t, c.name+" mean", m.Seconds, c.wantMean, 1e-9)
		if got := v.HasFlag(TransferOmitted); got != (c.xfer == 0) {
			t.Errorf("%s: TransferOmitted %v with transfer %g", c.name, got, c.xfer)
		}
	}
}

// The pooled figure is Allen-Cunneen: Erlang C (the test's own recursion,
// closed_test.go) times theta_p = E[S](Ca^2 + Cs^2) / (2 N (1 - rho)), and is
// never above the worst pod's figure.
func TestPooledIsAllenCunneen(t *testing.T) {
	fl := prefillOnly(1)
	fl.Prefill.Replicas, fl.Prefill.Imbalance, fl.Prefill.ArrivalCV2 = 4, 1.3, 1.5
	es := fl.Prefill.PromptTokens / fl.Prefill.VP
	for _, lam := range []float64{20, 60, 85} {
		m := mustTTFT(t, ttftAt(t, fl, lam, 0))
		rho := lam * es / 4
		wq := erlangC(4, lam*es) * es * (1.5 + 1) / (2 * 4 * (1 - rho))
		relNear(t, "pooled mean", m.Pooled, fl.Prefill.FixedLatency+es+wq, 1e-12)
		for _, q := range []float64{0, 0.9, 0.99} {
			v := mustTTFT(t, ttftAt(t, fl, lam, q))
			if !(v.Pooled <= v.Seconds) {
				t.Errorf("rate %g q %g: pooled %g above the worst pod's %g", lam, q, v.Pooled, v.Seconds)
			}
		}
	}
}

// The incomplete gamma against its closed forms: P(1, x) = 1 - e^-x,
// P(2, x) = 1 - e^-x (1 + x), P(1/2, x) = erf(sqrt x); and its quantile back.
func TestIncompleteGamma(t *testing.T) {
	for _, x := range []float64{1e-6, 0.01, 0.5, 1, 2.9, 3.1, 10, 40} {
		p1, q1 := gammaPQ(1, x)
		relNear(t, "P(1,x)", p1, -math.Expm1(-x), 1e-13)
		relNear(t, "Q(1,x)", q1, math.Exp(-x), 1e-12)
		_, q2 := gammaPQ(2, x)
		relNear(t, "Q(2,x)", q2, math.Exp(-x)*(1+x), 1e-12)
		ph, qh := gammaPQ(0.5, x)
		relNear(t, "P(1/2,x)", ph, math.Erf(math.Sqrt(x)), 1e-12)
		relNear(t, "Q(1/2,x)", qh, math.Erfc(math.Sqrt(x)), 1e-11)
		if x < 20 { // above, P rounds to 1 and its log to 0 in both
			relNear(t, "ln P(2,x)", lnGammaP(2, x), math.Log(-math.Expm1(-x)-x*math.Exp(-x)), 1e-10)
		}
	}
	for _, q := range []float64{0.01, 0.5, 0.9, 0.999} {
		relNear(t, "gamma(1) quantile", gammaQuantile(1, q), -math.Log1p(-q), 1e-12)
		for _, k := range []float64{0.04, 0.7, 3, 400} {
			p, _ := gammaPQ(k, gammaQuantile(k, q))
			relNear(t, "P at the quantile", p, q, 1e-10)
		}
	}
}

// The floor is the TTFT at no load: a + I_q/V_P + ITL(band.Lo), with I_q the
// prompt's q-quantile (for cv 1, -ln(1-q) E[I]).
func TestTTFTFloor(t *testing.T) {
	fl := workedFleet()
	d := *fl.Decode
	for _, q := range []float64{0.5, 0.9, 0.99} {
		v := mustTTFT(t, ttftAt(t, fl, 0, q))
		want := 0.0126 + -math.Log1p(-q)*1000/30000 + d.Line.ITLAt(d.Band.Lo)
		relNear(t, "floor", v.Floor, want, 1e-11)
		relNear(t, "TTFT at no load", v.Seconds, want, 1e-11)
		if !v.HasFlag(BelowFittedBand) {
			t.Errorf("decode's first step below the band, not flagged: %v", v.Flags)
		}
		// A target at the floor is refused, just above it is answered.
		if u, ok := rateForTTFT(t, fl, TTFTTarget{Seconds: v.Floor, Percentile: q}).(Unanswerable); !ok || u.Reason != TargetBelowFloor {
			t.Errorf("q %g at the floor: %#v", q, rateForTTFT(t, fl, TTFTTarget{Seconds: v.Floor, Percentile: q}))
		}
		if a := mustAnswer(t, rateForTTFT(t, fl, TTFTTarget{Seconds: want * 1.01, Percentile: q})); !(a.Value > 0) {
			t.Errorf("q %g just above the floor: %g r/s", q, a.Value)
		}
	}
	// The mean's floor is the mean prompt's.
	m := mustTTFT(t, ttftAt(t, fl, 0, 0))
	relNear(t, "mean floor", m.Floor, 0.0126+1000.0/30000+d.Line.ITLAt(d.Band.Lo), 1e-12)
}

// within runs f under a deadline: a refusal that went missing lets the queue run
// past saturation, where its scale is infinite and the quantile search does not
// return, and that must fail here by name rather than as the whole package's
// timeout. The deadline is generous: a call that takes under a second alone took
// over 5 s on a loaded machine, and a slow machine is not a hang.
func within(t *testing.T, what string, f func() Result) Result {
	t.Helper()
	done := make(chan Result, 1)
	go func() { done <- f() }()
	select {
	case r := <-done:
		return r
	case <-time.After(60 * time.Second):
		t.Fatalf("%s: no result within 60 s", what)
		return nil
	}
}

// ttftAt and rateForTTFT are TTFTAt and RateForTTFT under within's deadline,
// so a mutant that lets the model run past saturation fails the test that
// asked, by name, instead of stalling every test after it.
func ttftAt(t *testing.T, fl TTFTFleet, rate, q float64) Result {
	t.Helper()
	return within(t, "TTFTAt", func() Result { return TTFTAt(fl, rate, q) })
}

func rateForTTFT(t *testing.T, fl TTFTFleet, tg TTFTTarget) Result {
	t.Helper()
	return within(t, "RateForTTFT", func() Result { return RateForTTFT(fl, tg) })
}

// The knees: prefill N_p V_P / (f E[I]); TTFT refuses at it and above it
// (Saturated), as at decode's, and question 1 stays below it.
func TestTTFTSaturates(t *testing.T) {
	fl := workedFleet()
	fl.Prefill.VP, fl.Prefill.Replicas, fl.Prefill.Imbalance = 6000, 2, 1.25
	pk := mustAnswer(t, PrefillKnee(fl.Prefill))
	relNear(t, "prefill knee", pk.Value, 2*6000/(1.25*1000), 1e-12)
	for _, r := range []float64{pk.Value, pk.Value * (1 + 1e-9), pk.Value * 1.5, pk.Value * 3} {
		for _, q := range []float64{0, 0.9} {
			res := within(t, "TTFTAt at the prefill knee", func() Result { return TTFTAt(fl, r, q) })
			if u, ok := res.(Unanswerable); !ok || u.Reason != Saturated {
				t.Errorf("at %g r/s (q %g), prefill knee %g: %#v, want Saturated", r, q, pk.Value, res)
			}
		}
	}
	mustTTFT(t, within(t, "TTFTAt below the prefill knee", func() Result { return TTFTAt(fl, pk.Value*(1-1e-9), 0.9) }))
	dk := mustAnswer(t, KneeRate(*workedFleet().Decode)).Value
	for _, r := range []float64{dk, dk * 1.5} {
		res := within(t, "TTFTAt at the decode knee", func() Result { return TTFTAt(workedFleet(), r, 0.9) })
		if u, ok := res.(Unanswerable); !ok || u.Reason != Saturated {
			t.Errorf("at %g r/s, decode knee %g: %#v, want Saturated", r, dk, res)
		}
	}
	// Question 1 never answers at or above the lower knee, here prefill's.
	for _, q := range []float64{0, 0.9} {
		a := mustAnswer(t, within(t, "RateForTTFT", func() Result {
			return RateForTTFT(fl, TTFTTarget{Seconds: 100, Percentile: q})
		}))
		if !(a.Value < pk.Value) || a.Basis.TTFT.KneeRate != pk.Value {
			t.Errorf("q %g: rate %g, knee %g; want below the prefill knee %g", q, a.Value, a.Basis.TTFT.KneeRate, pk.Value)
		}
	}
}

// The own time's convolution with an exponential wait, against its closed
// forms, on both branches of the code's (z = b - r above and at or below 0)
// and at z = 0. For a gamma of shape 1 (rate b),
//
//	int_0^t b e^(-bs) e^(-r(t-s)) ds = b (e^(-bt) - e^(-rt)) / (r - b),  b t e^(-bt) at r = b,
//
// and for shape 2, b^2 e^(-rt) (1 - e^(-zt)(1 + zt)) / z^2.
func TestOwnTimeConvolution(t *testing.T) {
	const mean = 0.04
	for _, c := range []struct {
		name string
		r, t float64
	}{
		{"r below b", 5, 0.03}, {"r below b, late", 5, 0.4},
		{"r above b", 80, 0.03}, {"r above b, late", 80, 0.4}, {"r far above b", 900, 0.1},
		{"r at b", 0, 0.05}, // 0: r is set to b exactly
	} {
		one := newOwnTime(mean, 1)
		b := one.b
		if c.r == 0 {
			c.r = b
		}
		want := b * c.t * math.Exp(-b*c.t)
		if c.r != b {
			want = b * (math.Exp(-b*c.t) - math.Exp(-c.r*c.t)) / (c.r - b)
		}
		relNear(t, c.name+", shape 1", one.conv(c.r, c.t), want, 1e-11)

		two := newOwnTime(mean, 1/math.Sqrt2)
		b2 := two.b
		if z := b2 - c.r; z != 0 {
			want2 := b2 * b2 * math.Exp(-c.r*c.t) * -math.Expm1(-z*c.t) / (z * z)
			want2 -= b2 * b2 * math.Exp(-c.r*c.t) * math.Exp(-z*c.t) * z * c.t / (z * z)
			relNear(t, c.name+", shape 2", two.conv(c.r, c.t), want2, 1e-9)
		}
	}
	// And in a block: P(W + own > t) at a wait faster than the work
	// (r > b, the series branch), exact for shape 1:
	// sf(t) + p b (e^(-bt) - e^(-rt)) / (r - b).
	bl := block{wait: expWait(0.3, 80), own: newOwnTime(mean, 1)}
	for _, tt := range []float64{0.01, 0.05, 0.2} {
		b := 1 / mean
		want := math.Exp(-b*tt) + 0.3*b*(math.Exp(-b*tt)-math.Exp(-80*tt))/(80-b)
		relNear(t, "block tail, r above b", bl.exceeds(tt), want, 1e-11)
	}
}

// The union bound is never below the independent figure, and the optimised
// split is never above the even split, beta = (1-q)/2.
func TestUnionBoundAgainstIndependentAndEvenSplit(t *testing.T) {
	fl := workedFleet()
	d := *fl.Decode
	knee := mustAnswer(t, KneeRate(d)).Value
	for _, frac := range []float64{0.3, 0.6, 0.85, 0.9, 0.95, 0.98} {
		for _, q := range []float64{0.5, 0.9, 0.95, 0.99} {
			rate := frac * knee
			v := mustTTFT(t, ttftAt(t, fl, rate, q))
			if !(v.Seconds >= v.Independent*(1-1e-9)) {
				t.Errorf("%.2f of the knee, q %g: union %g below independent %g", frac, q, v.Seconds, v.Independent)
			}
			pl, _ := fl.plan()
			rho := pl.pf.utilization(rate)
			wait, _ := pl.pf.engineWait(rho)
			b1 := block{wait: wait, own: pl.pf.own()}
			adm := DecodeAdmission(d, rate).(Admission)
			even := 0.0126 + b1.quantile((1-q)/2) + adm.WaitQuantile(1-(1-q)/2) + v.Parts.FirstStep
			if !(v.Seconds <= even*(1+1e-12)) {
				t.Errorf("%.2f of the knee, q %g: optimised %g above the even split %g", frac, q, v.Seconds, even)
			}
		}
	}
}

// PrefillFromEstimate carries P1's figures and its refusals.
func TestPrefillFromEstimate(t *testing.T) {
	est := prefill.Estimate{VP: 30000, VPSource: prefill.SourcePeakUnderQueue, VPLowerBound: true, A: 0.011, F: 1.4}
	p, u := PrefillFromEstimate(est)
	if u != nil {
		t.Fatal(u)
	}
	above := est
	above.VPAboveSlopeBound = true
	if pa, _ := PrefillFromEstimate(above); !pa.VPAboveSlopeBound || p.VPAboveSlopeBound {
		t.Errorf("VPAboveSlopeBound not carried: %v from %v, %v from %v", pa.VPAboveSlopeBound, true, p.VPAboveSlopeBound, false)
	}
	if p.VP != 30000 || !p.VPLowerBound || p.FixedLatency != 0.011 || p.Imbalance != 1.4 || p.VPAssumed {
		t.Errorf("mapped %+v", p)
	}
	p.Replicas, p.PromptTokens, p.PromptCV = 4, 1000, 1
	if v := mustTTFT(t, ttftAt(t, TTFTFleet{Prefill: p}, 10, 0.9)); !v.HasFlag(VPLowerBound) {
		t.Errorf("a lower-bound V_P, not flagged: %v", v.Flags)
	}

	refusedF := est
	refusedF.F, refusedF.FRefused = 0, prefill.TooFewImbalanceMinutes
	p, _ = PrefillFromEstimate(refusedF)
	if p.Imbalance != 0 {
		t.Errorf("refused f mapped to %g", p.Imbalance)
	}
	p.Replicas, p.PromptTokens = 3, 1000
	if u, ok := ttftAt(t, TTFTFleet{Prefill: p}, 1, 0.9).(Unanswerable); !ok || u.Reason != NoImbalance {
		t.Errorf("three pods, no f: %#v", ttftAt(t, TTFTFleet{Prefill: p}, 1, 0.9))
	}
	p.Replicas = 1
	mustTTFT(t, ttftAt(t, TTFTFleet{Prefill: p}, 1, 0.9))

	for name, c := range map[string]struct {
		est  prefill.Estimate
		want Reason
	}{
		"V_P":    {prefill.Estimate{VPRefused: prefill.NoPrefillCapacity, A: 0.01}, NoPrefillCapacity},
		"SGLang": {prefill.Estimate{VPRefused: prefill.SGLangRefused, A: 0.01}, NoPrefillCapacity},
		"a":      {prefill.Estimate{VP: 30000, ARefused: prefill.TooFewLowLoadMinutes}, NoFixedLatency},
	} {
		if _, u := PrefillFromEstimate(c.est); u == nil || u.Reason != c.want {
			t.Errorf("%s: %v, want %s", name, u, c.want)
		}
	}
}

// ttftReasonCases are the TTFT model's cases for
// TestProperty_EveryReasonHasACase.
func ttftReasonCases(t *testing.T) map[Reason]Result {
	t.Helper()
	with := func(f func(*TTFTFleet)) TTFTFleet {
		fl := workedFleet()
		d := *fl.Decode
		fl.Decode = &d
		f(&fl)
		return fl
	}
	return map[Reason]Result{
		NoPrefillCapacity: ttftAt(t, with(func(fl *TTFTFleet) { fl.Prefill.VP = 0 }), 1, 0.9),
		NoFixedLatency:    ttftAt(t, with(func(fl *TTFTFleet) { fl.Prefill.FixedLatency = 0 }), 1, 0.9),
		NoImbalance:       ttftAt(t, with(func(fl *TTFTFleet) { fl.Prefill.Replicas = 2 }), 1, 0.9),
		NoPrefillReplicas: ttftAt(t, with(func(fl *TTFTFleet) { fl.Prefill.Replicas = 0 }), 1, 0.9),
		FlowControlUnknown: ttftAt(t, with(func(fl *TTFTFleet) {
			fl.FlowControl = FlowControl{State: FlowControlOn, QueueThreshold: 5}
		}), 1, 0.9),
		AboveDecodeOnset: ttftAt(t, with(func(fl *TTFTFleet) {
			fl.FlowControl = FlowControl{State: FlowControlOn, QueueThreshold: 5, KVThreshold: 0.8}
		}), 12.6, 0.9),
	}
}

// Every TTFT refusal for a fleet input, in check order.
func TestTTFTRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		set  func(*TTFTFleet)
		want Reason
	}{
		"too many prefill pods":  {func(fl *TTFTFleet) { fl.Prefill.Replicas, fl.Prefill.Imbalance = maxPrefillReplicas+1, 1 }, InvalidInput},
		"f below 1":              {func(fl *TTFTFleet) { fl.Prefill.Imbalance = 0.5 }, InvalidInput},
		"f above the pods":       {func(fl *TTFTFleet) { fl.Prefill.Imbalance = 1.5 }, InvalidInput},
		"no prompt":              {func(fl *TTFTFleet) { fl.Prefill.PromptTokens = 0 }, NoTraffic},
		"negative cv":            {func(fl *TTFTFleet) { fl.Prefill.PromptCV = -1 }, InvalidInput},
		"cv above 10":            {func(fl *TTFTFleet) { fl.Prefill.PromptCV = 11 }, InvalidInput},
		"negative Ca^2":          {func(fl *TTFTFleet) { fl.Prefill.ArrivalCV2 = -1 }, InvalidInput},
		"negative transfer":      {func(fl *TTFTFleet) { fl.Prefill.TransferSecondsPerToken = -1 }, InvalidInput},
		"bad flow-control state": {func(fl *TTFTFleet) { fl.FlowControl.State = 7 }, InvalidInput},
		"decode refused":         {func(fl *TTFTFleet) { fl.Decode.Replicas = 0 }, NoDecodeReplicas},
	} {
		fl := workedFleet()
		d := *fl.Decode
		fl.Decode = &d
		c.set(&fl)
		for q, r := range map[string]Result{
			"TTFTAt": ttftAt(t, fl, 1, 0.9), "RateForTTFT": rateForTTFT(t, fl, TTFTTarget{Seconds: 0.2, Percentile: 0.9}),
			"FlowControlOnsets": FlowControlOnsets(fl),
		} {
			if u, ok := r.(Unanswerable); !ok || u.Reason != c.want {
				t.Errorf("%s, %s: %#v, want %s", name, q, r, c.want)
			}
		}
	}
	fl := workedFleet()
	for name, r := range map[string]Result{
		"percentile 1":        ttftAt(t, fl, 1, 1),
		"negative percentile": ttftAt(t, fl, 1, -0.5),
		"negative rate":       ttftAt(t, fl, -1, 0.9),
		"zero target":         rateForTTFT(t, fl, TTFTTarget{Seconds: 0, Percentile: 0.9}),
		"target percentile 1": rateForTTFT(t, fl, TTFTTarget{Seconds: 0.2, Percentile: 1}),
	} {
		if !isInvalid(r) {
			t.Errorf("%s: %#v, want InvalidInput", name, r)
		}
	}
	// A question asked wrongly is a usage error even on a fleet the data
	// refuses: the question is checked first (exit 2 outranks exit 3).
	bad := workedFleet()
	bad.Prefill.VP = 0
	for name, r := range map[string]Result{
		"TTFTAt, bad rate":            ttftAt(t, bad, math.NaN(), 0.9),
		"TTFTAt, bad percentile":      ttftAt(t, bad, 1, 2),
		"RateForTTFT, bad target":     rateForTTFT(t, bad, TTFTTarget{Seconds: -1, Percentile: 0.9}),
		"RateForTTFT, bad percentile": rateForTTFT(t, bad, TTFTTarget{Seconds: 0.2, Percentile: 2}),
	} {
		if !isInvalid(r) {
			t.Errorf("%s on a refused fleet: %#v, want InvalidInput", name, r)
		}
	}
}

// A target above the floor by less than the bisection resolves has no
// positive rate known to meet it: refused, never answered with 0 r/s.
func TestRateForTTFTNeverAnswersZero(t *testing.T) {
	fl := prefillOnly(1)
	floor := mustTTFT(t, ttftAt(t, fl, 0, 0.9)).Floor
	for _, above := range []float64{1e-15, 1e-14} {
		switch r := rateForTTFT(t, fl, TTFTTarget{Seconds: floor * (1 + above), Percentile: 0.9}).(type) {
		case Answer:
			if !(r.Value > 0) {
				t.Errorf("floor x (1 + %g): answered %g r/s", above, r.Value)
			}
		case Unanswerable:
			if r.Reason != TargetBelowFloor {
				t.Errorf("floor x (1 + %g): %v", above, r)
			}
		}
	}
}
