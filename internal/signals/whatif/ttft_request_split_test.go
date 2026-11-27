package whatif

import (
	"math"
	"slices"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// V_P priced along its source's split of a request's work
// (Prefill.VPRequestSeconds, shapedVP; issue
// llm-d-incubation/llm-d-scaling-manager#368).

// splitPrefill is five learned pods at V_P 20000 tokens/s read at 2000
// tokens a request, 0.1 pod-seconds of work a request there, r of it paid
// per request, priced at a mean prompt of prompt, each request attending
// over splitCached cached tokens on both sides.
func splitPrefill(r, prompt float64) Prefill {
	return Prefill{
		VP: 20000, VPPromptTokens: 2000, VPRequestSeconds: r, VPMargin: DefaultVPMargin,
		FixedLatency: 0.05, Imbalance: 1.3, Replicas: 5,
		PromptTokens: prompt, PromptCV: 1, PromptCVAssumed: true,
		CachedContextRead: true, CachedContextTokens: splitCached, VPCachedContextTokens: splitCached,
	}
}

// splitCached is splitPrefill's cached context a request.
const splitCached = 80000

// splitGrowth is the attention span's growth from 2000 tokens to prompt at
// a cached context of h on both sides, at least 1 (Prefill.spanGrowth).
func splitGrowth(prompt, h float64) float64 {
	return max(1, (prompt+2*h)/(2000+2*h))
}

// splitKnee is p's prefill knee, failing the test where it is refused.
func splitKnee(t *testing.T, p Prefill) Answer {
	t.Helper()
	return mustAnswer(t, PrefillKnee(p))
}

// The knee is N_p (1 - margin) / (f (r + E[I] k g)), k the per-token part
// of the work at I_vp and g the attention span's growth: requests a second
// at the mix's mean work, not the token rate at I_vp over the prompt. With r
// two thirds of the work at I_vp and the context mostly cached, the one-rate
// knee is about two thirds of it at twice I_vp and 5/3 of it at half
// (shapedVP's comment). Priced at twice I_vp, the knee is flagged
// VPScaledToPrompt, not VPShapeMoved: V_P is not held.
func TestRequestSplit_KneeIsRequestsAtTheMixesWork(t *testing.T) {
	const r = 0.1 * 2 / 3
	k := (0.1 - r) / 2000
	for _, c := range []struct {
		name          string
		prompt, ratio float64
	}{
		{"twice I_vp", 4000, (r + 4000*k*splitGrowth(4000, splitCached)) / 0.2}, {"half I_vp", 1000, 5.0 / 3},
	} {
		on := splitKnee(t, splitPrefill(r, c.prompt))
		want := 5 * (1 - DefaultVPMargin) / (1.3 * (r + c.prompt*k*splitGrowth(c.prompt, splitCached)))
		relNear(t, c.name+" knee", on.Value, want, 1e-12)
		off := splitKnee(t, splitPrefill(0, c.prompt))
		relNear(t, c.name+" one-rate knee over the split's", off.Value/on.Value, c.ratio, 1e-12)
	}
	on := splitKnee(t, splitPrefill(r, 4000))
	if !slices.Contains(on.Flags, VPScaledToPrompt) || slices.Contains(on.Flags, VPShapeMoved) {
		t.Errorf("split knee at twice I_vp flagged %v, want VPScaledToPrompt and not VPShapeMoved", on.Flags)
	}
	if off := splitKnee(t, splitPrefill(0, 4000)); !slices.Contains(off.Flags, VPShapeMoved) {
		t.Errorf("one-rate knee at twice I_vp flagged %v, want VPShapeMoved", off.Flags)
	}
	if near := splitKnee(t, splitPrefill(r, 2200)); slices.Contains(near.Flags, VPScaledToPrompt) || slices.Contains(near.Flags, VPShapeMoved) {
		t.Errorf("split knee within MaxVPShapeRatio of I_vp flagged %v, want neither shape flag", near.Flags)
	}
}

// Above I_vp the cost a token may grow as the attention span does, and the
// work is never priced above the one-rate figure. On fresh prompts the span
// doubles with the prompt: at twice I_vp the bound is the one-rate work, and
// the knee is the one-rate knee to the bit, flagged VPShapeMoved as before.
// Where r is more than two thirds of the work at I_vp, the span's growth
// leaves the work under the one-rate figure there, and the split prices it.
// Below I_vp nothing grows, and the split prices it. With some context
// cached the knee lies between.
func TestRequestSplit_AboveIVPTheSpanBoundsTheRaise(t *testing.T) {
	fresh := func(prompt float64) Prefill {
		p := splitPrefill(0.05, prompt)
		p.CachedContextRead, p.CachedContextTokens, p.VPCachedContextTokens = false, 0, 0
		return p
	}
	held, one := splitKnee(t, fresh(4000)), fresh(4000)
	one.VPRequestSeconds = 0
	if want := splitKnee(t, one); held.Value != want.Value || !slices.Equal(held.Flags, want.Flags) {
		t.Errorf("fresh prompts at twice I_vp: knee %v %v, want the one-rate %v %v", held.Value, held.Flags, want.Value, want.Flags)
	}
	most := fresh(4000)
	most.VPRequestSeconds = 0.08
	raised := splitKnee(t, most)
	relNear(t, "fresh prompts, r four fifths of the work", raised.Value, 5*(1-DefaultVPMargin)/(1.3*(0.08+4000*(0.02/2000)*2)), 1e-12)
	if !(raised.Value > splitKnee(t, one).Value) || !slices.Contains(raised.Flags, VPScaledToPrompt) {
		t.Errorf("fresh prompts, r four fifths of the work: knee %v %v, want above the one-rate %v, flagged VPScaledToPrompt",
			raised.Value, raised.Flags, splitKnee(t, one).Value)
	}
	short, oneShort := splitKnee(t, fresh(1000)), fresh(1000)
	oneShort.VPRequestSeconds = 0
	if !(short.Value < splitKnee(t, oneShort).Value) {
		t.Errorf("fresh prompts at half I_vp: knee %v, want under the one-rate %v", short.Value, splitKnee(t, oneShort).Value)
	}
	some := splitPrefill(0.05, 4000)
	some.CachedContextTokens, some.VPCachedContextTokens = 4000, 4000
	k := (0.1 - 0.05) / 2000
	relNear(t, "knee at some cache", splitKnee(t, some).Value,
		5*(1-DefaultVPMargin)/(1.3*(0.05+4000*k*splitGrowth(4000, 4000))), 1e-12)
	if v := splitKnee(t, some).Value; !(v > held.Value) {
		t.Errorf("some cache: knee %v, want above the fresh prompts' %v", v, held.Value)
	}
}

// At I_vp the split prices the work V_P was read at: the knee is the
// one-rate knee, to rounding.
func TestRequestSplit_AtTheReadPromptTheKneeHolds(t *testing.T) {
	relNear(t, "knee at I_vp", splitKnee(t, splitPrefill(0.05, 2000)).Value, splitKnee(t, splitPrefill(0, 2000)).Value, 1e-12)
}

// Where the split is not measured or cannot be the work at I_vp (r at or
// above I_vp/V_P), where I_vp is unknown, and for an assumed V_P, V_P is one
// token rate: the knee is the one-rate knee to the bit, flags and all.
func TestRequestSplit_UnusableSplitPricesAsBefore(t *testing.T) {
	for _, c := range []struct {
		name string
		mod  func(*Prefill)
	}{
		{"r at the whole work", func(p *Prefill) { p.VPRequestSeconds = 0.1 }},
		{"r above the whole work", func(p *Prefill) { p.VPRequestSeconds = 0.2 }},
		{"I_vp unknown", func(p *Prefill) { p.VPPromptTokens = 0 }},
		{"assumed V_P", func(p *Prefill) { p.VPAssumed = true }},
	} {
		p := splitPrefill(0.05, 4000)
		c.mod(&p)
		base := p
		base.VPRequestSeconds = 0
		got, want := splitKnee(t, p), splitKnee(t, base)
		if got.Value != want.Value || !slices.Equal(got.Flags, want.Flags) {
			t.Errorf("%s: knee %v %v, want the one-rate %v %v", c.name, got.Value, got.Flags, want.Value, want.Flags)
		}
	}
}

// A split that is no figure, or below zero, is the caller's mistake.
func TestRequestSplit_InvalidSplitIsRefused(t *testing.T) {
	for _, r := range []float64{-0.01, math.NaN(), math.Inf(1)} {
		if u, ok := PrefillKnee(splitPrefill(r, 4000)).(Unanswerable); !ok || u.Reason != InvalidInput {
			t.Errorf("r %g: %#v, want InvalidInput", r, PrefillKnee(splitPrefill(r, 4000)))
		}
	}
}

// With P1's curve the per-token part is rescaled as the token rate is,
// by s(I_vp)/s(I) above I_vp, and r is held.
func TestRequestSplit_TheCurveRescalesThePerTokenPartOnly(t *testing.T) {
	const r, b, c = 0.05, 2e-5, 1e-9
	p := splitPrefill(r, 4000)
	p.CurveSlope, p.CurveCurvature = b, c
	s := func(x float64) float64 { return b + c*x }
	k := (0.1 - r) / 2000 * s(4000) / s(2000)
	relNear(t, "curved split knee", splitKnee(t, p).Value, 5*(1-DefaultVPMargin)/(1.3*(r+4000*k)), 1e-12)
}

// The pooled utilization a rate puts on the pods is rate (r + E[I] k) over
// N_p (1 - margin): the same work the knee is priced on, so a rate at the
// knee fills the busiest pod.
func TestRequestSplit_LoadIsPricedOnTheSameWork(t *testing.T) {
	p := splitPrefill(0.05, 4000)
	knee := splitKnee(t, p).Value
	f, pooled, ok := PrefillLoadAt(p, knee)
	if !ok {
		t.Fatal("PrefillLoadAt refused the split fleet")
	}
	relNear(t, "busiest pod at the knee", f*pooled, 1, 1e-12)
}

// The sweep moves V_P and the per-request work together (Prefill.WithVP),
// so each corner's knee is the knee times its V_P ratio at any prompt, as
// it is with no split.
func TestRequestSplit_SensitivityMovesTheWholeWork(t *testing.T) {
	p := splitPrefill(0.05, 4000)
	p.Imbalance = 0
	p.Replicas = 1
	knee := splitKnee(t, p).Value
	for _, at := range SensitivityFleets(TTFTFleet{Prefill: p}) {
		relNear(t, "corner knee over the knee", splitKnee(t, at.Prefill).Value/knee, at.Prefill.VP/p.VP, 1e-12)
	}
	if q := splitPrefill(0, 4000).WithVP(1000); q.VPRequestSeconds != 0 || q.VP != 1000 {
		t.Errorf("WithVP on an unmeasured split: %+v, want VP 1000 and no split", q)
	}
	for _, vp := range []float64{0, -1, math.Inf(1), math.NaN()} {
		q := p.WithVP(vp)
		if q.VPRequestSeconds != p.VPRequestSeconds {
			t.Errorf("WithVP(%g): split %g, want it left at %g", vp, q.VPRequestSeconds, p.VPRequestSeconds)
		}
		if u, ok := PrefillKnee(q).(Unanswerable); !ok || u.Reason != NoPrefillCapacity {
			t.Errorf("WithVP(%g): knee %#v, want NoPrefillCapacity", vp, PrefillKnee(q))
		}
	}
}

// PrefillFromEstimate carries P1's split to the model.
func TestRequestSplit_FromEstimate(t *testing.T) {
	p, u := PrefillFromEstimate(prefill.Estimate{VP: 20000, VPPromptTokens: 2000, VPRequestSeconds: 0.05, A: 0.05})
	if u != nil || p.VPRequestSeconds != 0.05 {
		t.Errorf("PrefillFromEstimate: %+v, %v, want VPRequestSeconds 0.05", p, u)
	}
}
