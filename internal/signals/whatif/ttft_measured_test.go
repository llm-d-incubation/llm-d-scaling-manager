package whatif

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The changes P2 asked of P3 (docs/proposals/slo-capacity-planner-ttft.md,
// "As built (P3 after P2)"): the decode engine's measured TTFT where there is
// no decode line, V_P rescaled along P1's curve to the question's prompt, and
// a margin on V_P near the knee. Each case is known truth: a closed form the
// test computes without the model.

// measuredFleet is one prefill pod of learned V_P (not assumed), a constant
// prompt, no decode line, and the decode engine's TTFT measured at the same
// prompt.
func measuredFleet(decode float64) TTFTFleet {
	return TTFTFleet{
		Prefill: Prefill{VP: 30000, FixedLatency: 0.0126, Replicas: 1, PromptTokens: 1000,
			VPPromptTokens: 1000},
		DecodeTTFT: DecodeTTFT{Seconds: decode, PromptTokens: 1000},
	}
}

// With no decode line the decode engine's measured TTFT is added to every
// figure: at cv 0 and no load TTFT is a + I/V_P + D exactly, and at cv 1 it
// is the M/M/1 sojourn plus D.
func TestDecodeEngineTTFTIsAdded(t *testing.T) {
	const d = 0.04
	fl := measuredFleet(d)
	for _, q := range []float64{0, 0.9, 0.99} {
		v := mustTTFT(t, ttftAt(t, fl, 0, q))
		relNear(t, "TTFT at no load", v.Seconds, 0.0126+1000.0/30000+d, 1e-12)
		relNear(t, "floor", v.Floor, 0.0126+1000.0/30000+d, 1e-12)
		relNear(t, "decode engine part", v.Parts.DecodeEngine, d, 0)
		if v.Parts.FirstStep != 0 || v.HasFlag(DecodeTTFTUnmeasured) || v.HasFlag(DecodeSideOmitted) || !v.HasFlag(DecodeEngineMeasured) {
			t.Errorf("q %g: first step %g, flags %v", q, v.Parts.FirstStep, v.Flags)
		}
	}
	mm1 := fl
	mm1.Prefill.PromptCV = 1
	mu := 30000.0 / 1000
	for _, lam := range []float64{5, 20, 29} {
		for _, q := range []float64{0.9, 0.99} {
			v := mustTTFT(t, ttftAt(t, mm1, lam, q))
			relNear(t, "M/M/1 sojourn plus D", v.Seconds, 0.0126+math.Log(1/(1-q))/(mu-lam)+d, 1e-9)
		}
		m := mustTTFT(t, ttftAt(t, mm1, lam, 0))
		relNear(t, "mean", m.Seconds, m.Parts.EngineWait+m.Parts.OwnPrefill+m.Parts.DecodeEngine, 1e-12)
	}
	// Question 1 round-trips with the term in it.
	a := mustAnswer(t, rateForTTFT(t, mm1, TTFTTarget{Seconds: 0.2, Percentile: 0.9}))
	relNear(t, "rate for 200 ms", a.Value, mu-math.Log(10)/(0.2-0.0126-d), 1e-6)
}

// The decode engine's TTFT holds the KV pull, so a transfer term beside it is
// not priced (it would count the pull twice) and TransferOmitted is not
// raised; with a decode line DecodeTTFT is not read.
func TestDecodeEngineTTFTHoldsTheTransfer(t *testing.T) {
	fl := measuredFleet(0.04)
	with := fl
	with.Prefill.TransferSecondsPerToken = 3e-6
	a, b := mustTTFT(t, ttftAt(t, fl, 10, 0.9)), mustTTFT(t, ttftAt(t, with, 10, 0.9))
	if a.Seconds != b.Seconds || a.HasFlag(TransferOmitted) || b.HasFlag(TransferOmitted) {
		t.Errorf("transfer priced beside the decode engine: %g vs %g, flags %v / %v", a.Seconds, b.Seconds, a.Flags, b.Flags)
	}
	line := workedFleet()
	withD := line
	withD.DecodeTTFT = DecodeTTFT{Seconds: 5, PromptTokens: 1000}
	if x, y := mustTTFT(t, ttftAt(t, line, 5, 0.9)), mustTTFT(t, ttftAt(t, withD, 5, 0.9)); x.Seconds != y.Seconds || y.Parts.DecodeEngine != 0 {
		t.Errorf("DecodeTTFT read beside a decode line: %g vs %g", x.Seconds, y.Seconds)
	}
}

// The term scales up with a longer prompt than it was measured at and is held
// for a shorter one: D * max(1, I/I_d).
func TestDecodeEngineTTFTScalesUpOnly(t *testing.T) {
	for _, c := range []struct{ prompt, want float64 }{{2000, 0.08}, {1000, 0.04}, {500, 0.04}} {
		fl := measuredFleet(0.04)
		fl.Prefill.PromptTokens, fl.Prefill.VPPromptTokens = c.prompt, c.prompt
		v := mustTTFT(t, ttftAt(t, fl, 0, 0.9))
		relNear(t, "decode engine part", v.Parts.DecodeEngine, c.want, 1e-12)
	}
}

// Unmeasured is flagged and leaves the term out, never a silent zero; a
// reading that is not a finite non-negative figure, or a figure with no
// prompt, is refused.
func TestDecodeEngineTTFTUnmeasured(t *testing.T) {
	v := mustTTFT(t, ttftAt(t, measuredFleet(0), 5, 0.9))
	if !v.HasFlag(DecodeTTFTUnmeasured) || !v.HasFlag(DecodeSideOmitted) || v.HasFlag(DecodeEngineMeasured) || v.Parts.DecodeEngine != 0 {
		t.Errorf("unmeasured: %v, part %g", v.Flags, v.Parts.DecodeEngine)
	}
	for name, d := range map[string]DecodeTTFT{
		"negative": {Seconds: -0.01, PromptTokens: 1000}, "NaN": {Seconds: math.NaN(), PromptTokens: 1000},
		"infinite": {Seconds: math.Inf(1), PromptTokens: 1000}, "no prompt": {Seconds: 0.04},
		"NaN prompt": {Seconds: 0.04, PromptTokens: math.NaN()}, "under a token": {Seconds: 0.04, PromptTokens: 0.5},
	} {
		fl := measuredFleet(0)
		fl.DecodeTTFT = d
		if !isInvalid(ttftAt(t, fl, 5, 0.9)) || !isInvalid(rateForTTFT(t, fl, TTFTTarget{Seconds: 1, Percentile: 0.9})) {
			t.Errorf("%s: %#v", name, ttftAt(t, fl, 5, 0.9))
		}
	}
}

// A decode term that overflows is refused, never answered as an infinite
// TTFT or floor: a prompt of 1e-320 tokens (below MinDecodeTTFTPrompt) and a
// figure of 1e308 s, which is finite alone and infinite once scaled to a
// prompt twice as long. Both at the mean and at p90, for both questions.
func TestDecodeEngineTTFTNeverInfinite(t *testing.T) {
	for name, d := range map[string]DecodeTTFT{
		"denormal prompt": {Seconds: 0.04, PromptTokens: 1e-320},
		"huge figure":     {Seconds: 1e308, PromptTokens: 500},
	} {
		fl := measuredFleet(0)
		fl.DecodeTTFT = d
		for _, q := range []float64{0, 0.9} {
			r := ttftAt(t, fl, 5, q)
			if v, ok := r.(TTFT); ok {
				t.Errorf("%s q %g: answered %g s, floor %g, decode %g", name, q, v.Seconds, v.Floor, v.Parts.DecodeEngine)
			} else if !isInvalid(r) {
				t.Errorf("%s q %g: %#v, want InvalidInput", name, q, r)
			}
			if a := rateForTTFT(t, fl, TTFTTarget{Seconds: 1, Percentile: q}); !isInvalid(a) {
				t.Errorf("%s q %g: RateForTTFT %#v, want InvalidInput", name, q, a)
			}
		}
	}
	// A large figure that stays finite is priced.
	fl := measuredFleet(0)
	fl.DecodeTTFT = DecodeTTFT{Seconds: 1e300, PromptTokens: 1000}
	if v := mustTTFT(t, ttftAt(t, fl, 5, 0.9)); !(v.Seconds >= 1e300) || !Finite(v.Seconds) {
		t.Errorf("finite 1e300 s: %g", v.Seconds)
	}
}

// curveFleet is a prefill pod whose V_P was read at 30k tokens, with P1's

// pooled curve (runs 3 and 4: b 7.1815e-7, c 2.0280e-10).
func curveFleet(prompt float64) TTFTFleet {
	return TTFTFleet{Prefill: Prefill{VP: 134775.41, FixedLatency: 0.01086, Replicas: 1, PromptTokens: prompt,
		VPPromptTokens: 30000, CurveSlope: 7.1815e-7, CurveCurvature: 2.0280e-10}}
}

// V_P is rescaled along the curve to a longer prompt, V_P * s(I_vp)/s(I)
// with s(I) = b + c*I, and held for a shorter one; the knee says which V_P
// was priced. A wiring check: it re-types the formula, so it shows the
// rescale is applied where it should be and with the flag, not that the
// formula is right (TestVPRescaleNeverAboveTheStepModel and
// TestVPRescaleOnRun2At37k test that).
func TestVPRescaledAlongTheCurve(t *testing.T) {
	s := func(i float64) float64 { return 7.1815e-7 + 2.0280e-10*i }
	for _, c := range []struct {
		prompt, vp float64
		flag       Flag
	}{
		{37000, 134775.41 * s(30000) / s(37000), VPScaledToPrompt},
		{60000, 134775.41 * s(30000) / s(60000), VPScaledToPrompt},
		{30000, 134775.41, ""},
		{8000, 134775.41, ""},
	} {
		fl := curveFleet(c.prompt)
		k := mustAnswer(t, PrefillKnee(fl.Prefill))
		relNear(t, "knee", k.Value, c.vp/c.prompt, 1e-12)
		for _, f := range []Flag{VPScaledToPrompt, VPShapeMoved} {
			if k.HasFlag(f) != (f == c.flag) {
				t.Errorf("prompt %g: flags %v, want %q", c.prompt, k.Flags, c.flag)
			}
		}
	}
}

// The rescale never reads V_P above the truth. A busy pod's step of B tokens
// costs s0 + (b + c*I)*B, so its true V_P is 1/(h + s(I)) with h = s0/B >= 0.
// For every h and every prompt the priced V_P (rescaled above I_vp, held
// below) is at or under it; at h = 0 and above I_vp it is exact.
func TestVPRescaleNeverAboveTheStepModel(t *testing.T) {
	const b, c, ivp = 7.1815e-7, 2.0280e-10, 30000.0
	s := func(i float64) float64 { return b + c*i }
	for _, h := range []float64{0, 1e-8, 2.5e-7, 1e-6, 1e-5} {
		truth := func(i float64) float64 { return 1 / (h + s(i)) }
		for _, i := range []float64{500, 4000, 15000, 29000, 30000, 31000, 45000, 120000} {
			fl := curveFleet(i)
			fl.Prefill.VP, fl.Prefill.CurveSlope, fl.Prefill.CurveCurvature = truth(ivp), b, c
			priced := mustAnswer(t, PrefillKnee(fl.Prefill)).Value * i
			if !(priced <= truth(i)*(1+1e-12)) {
				t.Errorf("h %g, prompt %g: V_P priced %g above the step model's %g", h, i, priced, truth(i))
			}
			if h == 0 && i >= ivp {
				relNear(t, "exact at h = 0", priced, truth(i), 1e-12)
			}
		}
	}
}

// Run 2's 37k level (P2, PR #482): the pod served 115,953 tok/s at a mean
// prompt of 36,965 tokens, 14% under the busy-minute V_P of 134,775 read at
// 31,816. Rescaled along the pooled curve, V_P reads 1.5% above what it
// served: an over-read, on the unsafe side, which the step model says cannot
// happen (P1's low-load b and c are not exactly the busy step's cost). With
// the margin it is 3.6% under. The test holds the sign and the size, so a
// change that moves either says so.
func TestVPRescaleOnRun2At37k(t *testing.T) {
	fl := curveFleet(36965)
	fl.Prefill.VPPromptTokens = 31816.49
	scaled := mustAnswer(t, PrefillKnee(fl.Prefill)).Value * 36965
	if over := scaled/115953 - 1; !(over > 0.010 && over < 0.020) {
		t.Errorf("rescaled V_P %g against the served 115,953: %+.4f, want the measured over-read of +1.0%% to +2.0%%", scaled, over)
	}
	fl.Prefill.VPMargin = DefaultVPMargin
	if m := mustAnswer(t, PrefillKnee(fl.Prefill)).Value*36965/115953 - 1; !(m > -0.040 && m < -0.030) {
		t.Errorf("margined V_P against the served 115,953: %+.4f, want -3.0%% to -4.0%%", m)
	}
	t.Logf("run 2, 37k: V_P rescaled %.0f tok/s against 115,953 served (ratio %.4f)", scaled, scaled/115953)
	// Its rate, 3.137 r/s, is past that knee: refused, which S1 reads as
	// unmet. Held at 30k's V_P it was priced at utilization 0.86.
	if u, ok := ttftAt(t, fl, 3.137, 0.9).(Unanswerable); !ok || u.Reason != Saturated {
		t.Errorf("37k at 3.137 r/s: %#v", ttftAt(t, fl, 3.137, 0.9))
	}
}

// With no curve V_P is held, and flagged VPShapeMoved when the prompt is more
// than MaxVPShapeRatio from I_vp either way or I_vp is unknown; an assumed V_P
// is neither rescaled nor flagged.
func TestVPShapeMovedWithoutACurve(t *testing.T) {
	for _, c := range []struct {
		ivp, prompt float64
		moved       bool
	}{
		{1000, 1000, false}, {1000, 1190, false}, {1000, 840, false},
		{1000, 1201, true}, {1000, 830, true}, {0, 1000, true},
	} {
		p := measuredFleet(0.04).Prefill
		p.VPPromptTokens, p.PromptTokens = c.ivp, c.prompt
		k := mustAnswer(t, PrefillKnee(p))
		relNear(t, "knee", k.Value, 30000/c.prompt, 1e-12)
		if k.HasFlag(VPShapeMoved) != c.moved || k.HasFlag(VPScaledToPrompt) {
			t.Errorf("I_vp %g, prompt %g: flags %v, want moved %v", c.ivp, c.prompt, k.Flags, c.moved)
		}
	}
	assumed := curveFleet(60000).Prefill
	assumed.VPAssumed = true
	k := mustAnswer(t, PrefillKnee(assumed))
	relNear(t, "assumed V_P", k.Value, 134775.41/60000, 1e-12)
	if k.HasFlag(VPShapeMoved) || k.HasFlag(VPScaledToPrompt) {
		t.Errorf("assumed V_P flagged %v", k.Flags)
	}
	// One of the two curve terms missing is no curve.
	half := curveFleet(60000).Prefill
	half.CurveCurvature = 0
	if k := mustAnswer(t, PrefillKnee(half)); !k.HasFlag(VPShapeMoved) || k.Value != 134775.41/60000 {
		t.Errorf("no curvature: %v at %g", k.Flags, k.Value)
	}
}

// The margin: none below MarginRampStart, so TTFT is unchanged there; whole
// from MarginRampFull, where TTFT is exactly that of a pod at V_P*(1 - m)
// with no margin; linear between; and the knee is (1 - m) of the unmargined.
func TestVPMargin(t *testing.T) {
	const m = 0.05
	base := measuredFleet(0.04)
	base.Prefill.PromptCV = 1
	margined := base
	margined.Prefill.VPMargin = m
	slow := base
	slow.Prefill.VP *= 1 - m
	knee := 30.0 // V_P / I
	relNear(t, "margined knee", mustAnswer(t, PrefillKnee(margined.Prefill)).Value, knee*(1-m), 1e-12)
	for _, rho := range []float64{0.1, 0.3, 0.5} {
		a, b := mustTTFT(t, ttftAt(t, base, rho*knee, 0.9)), mustTTFT(t, ttftAt(t, margined, rho*knee, 0.9))
		if a.Seconds != b.Seconds || b.VPMarginApplied != 0 {
			t.Errorf("rho %g: margin spent below the ramp: %g vs %g (applied %g)", rho, a.Seconds, b.Seconds, b.VPMarginApplied)
		}
	}
	for _, rho := range []float64{0.65, 0.8, 0.9} {
		for _, q := range []float64{0, 0.9, 0.99} {
			b, c := mustTTFT(t, ttftAt(t, margined, rho*knee, q)), mustTTFT(t, ttftAt(t, slow, rho*knee, q))
			relNear(t, "whole margin", b.Seconds, c.Seconds, 1e-12)
			relNear(t, "applied", b.VPMarginApplied, m, 1e-12)
		}
	}
	// The floor has no queue, so it is at the unmargined V_P even where the
	// whole margin is taken.
	for _, q := range []float64{0, 0.9} {
		b, a := mustTTFT(t, ttftAt(t, margined, 0.8*knee, q)), mustTTFT(t, ttftAt(t, base, 0, q))
		if b.VPMarginApplied != m || b.Floor != a.Floor {
			t.Errorf("q %g at rho 0.8: floor %g (margin %g), want the unmargined %g", q, b.Floor, b.VPMarginApplied, a.Floor)
		}
	}
	mid := mustTTFT(t, ttftAt(t, margined, 0.55*knee, 0.9))
	relNear(t, "half the ramp", mid.VPMarginApplied, m/2, 1e-9)
	half := base
	half.Prefill.VP *= 1 - m/2
	relNear(t, "half the margin", mid.Seconds, mustTTFT(t, ttftAt(t, half, 0.55*knee, 0.9)).Seconds, 1e-9)
	for name, v := range map[string]float64{"negative": -0.01, "above the most": MaxVPMargin + 0.01, "NaN": math.NaN()} {
		bad := base
		bad.Prefill.VPMargin = v
		if !isInvalid(ttftAt(t, bad, 1, 0.9)) {
			t.Errorf("margin %s: %#v", name, ttftAt(t, bad, 1, 0.9))
		}
	}
}

// Why the ramp sits where it does: on the M/M/1 sojourn a V_P read 5% high
// puts TTFT (1 - rho)/((1 + e)(1 - rho) + e*rho) of the truth, which crosses
// the S1 mark's 10% under-side at rho 0.55, the middle of the ramp. Checked
// on the model at cv 1 against a pod 5% slower, with a near zero.
func TestVPMarginRampIsWhereTTFTTurnsSensitive(t *testing.T) {
	const e = DefaultVPMargin
	fast := prefillOnly(1)
	fast.Prefill.FixedLatency, fast.Prefill.VPAssumed = 1e-9, false
	fast.Prefill.VPPromptTokens = fast.Prefill.PromptTokens
	truth := fast
	truth.Prefill.VP /= 1 + e
	trueKnee := truth.Prefill.VP / truth.Prefill.PromptTokens
	ratio := func(rho float64) float64 {
		return mustTTFT(t, ttftAt(t, fast, rho*trueKnee, 0.9)).Seconds / mustTTFT(t, ttftAt(t, truth, rho*trueKnee, 0.9)).Seconds
	}
	for _, rho := range []float64{0.3, 0.5, 0.55, 0.6, 0.7, 0.85} {
		relNear(t, "under-read", ratio(rho), (1-rho)/((1+e)*(1-rho)+e*rho), 1e-6)
	}
	if !(ratio(MarginRampStart) > 0.9) || !(ratio(MarginRampFull) < 0.9) {
		t.Errorf("the 10%% crossing is not inside the ramp: %g at %g, %g at %g",
			ratio(MarginRampStart), MarginRampStart, ratio(MarginRampFull), MarginRampFull)
	}
	// With the margin the 5%-high V_P prices at or above the truth once the
	// margin is whole (its own utilization rho/(1 + e) at MarginRampFull).
	fast.Prefill.VPMargin = e
	for _, rho := range []float64{0.65, 0.8, 0.95} {
		if r := ratio(rho); !(r >= 1-1e-9) {
			t.Errorf("rho %g with the margin: %g of the truth", rho, r)
		}
	}
}

// PrefillFromEstimate carries the prompt V_P was read at, the curve where a
// came from it, and the default margin.
func TestPrefillFromEstimateCarriesTheShape(t *testing.T) {
	est := prefill.Estimate{VP: 134775, A: 0.011, VPPromptTokens: 31816,
		AModel: prefill.AModelQuadratic, ASlope: 7.2e-7, ACurve: 2e-10}
	p, u := PrefillFromEstimate(est)
	if u != nil {
		t.Fatal(u)
	}
	if p.VPPromptTokens != 31816 || p.CurveSlope != 7.2e-7 || p.CurveCurvature != 2e-10 || p.VPMargin != DefaultVPMargin {
		t.Errorf("mapped %+v", p)
	}
	line := est
	line.AModel = prefill.AModelLinear
	if p, _ := PrefillFromEstimate(line); p.CurveSlope != 0 || p.CurveCurvature != 0 {
		t.Errorf("a line taken as a curve: %+v", p)
	}
}

// On random prefill-only fleets with a curve, the margin and the decode
// engine's TTFT: TTFT never falls as the rate rises across the ramp, it is
// refused at the knee, and question 1 round-trips.
func TestProperty_MeasuredFleetMonotone(t *testing.T) {
	r := rand.New(rand.NewPCG(19, 23))
	for range ttftDraws {
		fl := TTFTFleet{Prefill: Prefill{
			VP: 5000 + 150000*r.Float64(), FixedLatency: 0.002 + 0.03*r.Float64(), Replicas: 1,
			PromptTokens: 500 + 40000*r.Float64(), VPPromptTokens: 500 + 40000*r.Float64(),
			PromptCV: 2 * r.Float64(), VPMargin: MaxVPMargin * r.Float64(),
			CurveSlope: 7e-7 * r.Float64(), CurveCurvature: 2e-10 * r.Float64(),
		}, DecodeTTFT: DecodeTTFT{Seconds: 0.2 * r.Float64(), PromptTokens: 500 + 40000*r.Float64()}}
		knee := mustAnswer(t, PrefillKnee(fl.Prefill)).Value
		q := []float64{0, 0.9, 0.99}[r.IntN(3)]
		prev := 0.0
		for i := 0; i <= 198; i++ {
			got := ttftSeconds(t, fl, knee*float64(i)/200, q)
			if !(got >= prev*(1-1e-9)) || math.IsInf(got, 1) {
				t.Errorf("%+v q %g: TTFT %g at %.3f of the knee, after %g", fl.Prefill, q, got, float64(i)/200, prev)
				break
			}
			prev = got
		}
		if v, ok := ttftAt(t, fl, knee, q).(Unanswerable); !ok || v.Reason != Saturated {
			t.Errorf("at the knee: %#v", ttftAt(t, fl, knee, q))
		}
		floor := mustTTFT(t, ttftAt(t, fl, 0, q)).Floor
		target := TTFTTarget{Seconds: floor * (1.2 + 3*r.Float64()), Percentile: q}
		a := mustAnswer(t, rateForTTFT(t, fl, target))
		relNear(t, "TTFT at the answer", mustTTFT(t, ttftAt(t, fl, a.Value, q)).Seconds, target.Seconds, 1e-6)
	}
}
