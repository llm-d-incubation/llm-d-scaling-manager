package whatif

import "testing"

// The fixtures here are synthetic: no committed recording has a prefix hit
// (issue llm-d-incubation/llm-d-scaling-manager#311). The curve is the
// issue's arithmetic: b = 1 unit a token, c*8k = 0.13 units, so a fresh 8k
// prompt costs 1.13 units a computed token.
const (
	hitCurveB = 1e-6
	hitCurveC = 0.13e-6 / 8000
)

// hitFleet is V_P learned on fresh 8k prompts, asked at a mean computed
// prompt of eff tokens out of a whole prompt of whole at hit rate hit.
func hitFleet(eff, whole, hit float64) Prefill {
	return Prefill{VP: 120000, FixedLatency: 0.01, Replicas: 1, PromptTokens: eff,
		VPPromptTokens: 8000, VPWholePromptTokens: 8000, CurveSlope: hitCurveB, CurveCurvature: hitCurveC,
		WholePromptTokens: whole, PromptHitRate: hit}
}

// pricedVP is the V_P a prefill knee was priced at.
func pricedVP(t *testing.T, p Prefill) (float64, Answer) {
	t.Helper()
	k := mustAnswer(t, PrefillKnee(p))
	return k.Value * p.PromptTokens, k
}

// At 32k with a 75% hit a pod computes the 8k a fresh 8k prompt does, but
// each computed token attends over 24k cached ones: s = b + c*(I_eff + 2h),
// 1.91 units against 1.13. Off, V_P is held at the learned figure, about
// 1.7x high; on, it is rescaled by 1.13/1.91.
func TestVPRescale_PrefixAttentionReadsTheCachedContext(t *testing.T) {
	s := func(x float64) float64 { return hitCurveB + hitCurveC*x }
	relNear(t, "fresh 8k, units a token", s(8000)/hitCurveB, 1.13, 1e-12)
	relNear(t, "32k at 75%, units a token", s(8000+2*24000)/hitCurveB, 1.91, 1e-12)

	off := hitFleet(8000, 32000, 0.75)
	vp, k := pricedVP(t, off)
	relNear(t, "off: held", vp, 120000, 1e-12)
	if k.HasFlag(VPScaledToPrompt) {
		t.Errorf("off: flags %v, want no rescale", k.Flags)
	}

	on := off
	on.PrefixAttention = true
	vp, k = pricedVP(t, on)
	relNear(t, "on: rescaled along the cached context", vp, 120000*1.13/1.91, 1e-12)
	if !k.HasFlag(VPScaledToPrompt) {
		t.Errorf("on: flags %v, want VPScaledToPrompt", k.Flags)
	}

	// A fresh prompt caches nothing: on and off agree.
	fresh := hitFleet(16000, 16000, 0)
	want, _ := pricedVP(t, fresh)
	fresh.PrefixAttention = true
	got, _ := pricedVP(t, fresh)
	relNear(t, "fresh prompt, on", got, want, 1e-12)
	relNear(t, "fresh prompt, the curve", got, 120000*s(8000)/s(16000), 1e-12)
}

// h is read from the measured hit rate, h = I_eff*hit/(1 - hit), not as the
// whole prompt less I_eff: the two are read over different windows, and a
// whole prompt that drifted from I_eff with no cache is no cached context.
// At or below MaterialPrefixHitMove on both sides nothing is cached either.
func TestVPRescale_PrefixAttentionTakesTheCachedTokensFromTheHit(t *testing.T) {
	s := func(x float64) float64 { return hitCurveB + hitCurveC*x }
	for _, c := range []struct {
		name       string
		eff, whole float64
		hit        float64
		want       float64
	}{
		// 16k computed, the whole prompt read at 24k over another window,
		// no hit: rescaled on I_eff, as off.
		{"drift with no cache", 16000, 24000, 0, 120000 * s(8000) / s(16000)},
		{"a hit at the floor", 16000, 24000, MaterialPrefixHitMove, 120000 * s(8000) / s(16000)},
		// 8k computed at a 50% hit: 8k cached, whatever the whole prompt
		// read.
		{"a hit, the whole read elsewhere", 8000, 20000, 0.5, 120000 * s(8000) / s(8000+2*8000)},
	} {
		p := hitFleet(c.eff, c.whole, c.hit)
		p.PrefixAttention = true
		vp, _ := pricedVP(t, p)
		relNear(t, c.name, vp, c.want, 1e-12)
	}
}

// Where a whole prompt is not known, on either side, the rescale reads
// E[I_eff] as before, switch or not.
func TestVPRescale_PrefixAttentionNeedsBothWholePrompts(t *testing.T) {
	for _, c := range []struct {
		name      string
		vp, asked float64
	}{
		{"V_P's unknown", 0, 32000},
		{"the question's unknown", 8000, 0},
	} {
		p := hitFleet(8000, c.asked, 0.75)
		p.VPWholePromptTokens, p.PrefixAttention = c.vp, true
		vp, k := pricedVP(t, p)
		relNear(t, c.name, vp, 120000, 1e-12)
		if k.HasFlag(VPScaledToPrompt) || k.HasFlag(VPShapeMoved) {
			t.Errorf("%s: flags %v, want none of the shape's", c.name, k.Flags)
		}
	}
}

// VPShapeMoved, on whatever the switch says, where the prefix hits moved
// from those V_P was read at: the hit rate by more than
// MaterialPrefixHitMove, or the whole prompt by more than MaxVPShapeRatio
// with a hit rate above it on either side. With no hit on either side the whole prompt
// moves with E[I_eff], and nothing new is raised: no golden moves.
func TestVPShapeMoved_WhenThePrefixHitsMove(t *testing.T) {
	for _, c := range []struct {
		name                     string
		vpWhole, vpHit           float64
		eff, whole, hit          float64
		assumed, prefixAttention bool
		moved                    bool
	}{
		{"same shape", 8000, 0, 8000, 8000, 0, false, false, false},
		{"32k at 75% against fresh 8k", 8000, 0, 8000, 32000, 0.75, false, false, true},
		{"the same, switch on", 8000, 0, 8000, 32000, 0.75, false, true, true},
		{"hit within the share", 10000, 0.5, 5500, 11000, 0.5, false, false, false},
		{"hit moved past the share", 10000, 0.5, 4000, 10000, 0.6 + 1e-9, false, false, true},
		{"whole moved at the same hit", 10000, 0.5, 6500, 13000, 0.5, false, false, true},
		{"no hit, whole moved: E[I_eff] says it", 8000, 0, 8000 * 1.5, 8000 * 1.5, 0, false, false, false},
		{"V_P's whole unknown", 0, 0, 8000, 32000, 0.75, false, false, false},
		{"the question's whole unknown", 8000, 0, 8000, 0, 0.75, false, false, false},
		{"an assumed V_P", 8000, 0, 8000, 32000, 0.75, true, false, false},
	} {
		p := hitFleet(c.eff, c.whole, c.hit)
		p.VPWholePromptTokens, p.VPPromptHitRate = c.vpWhole, c.vpHit
		if c.vpWhole > 0 {
			p.VPPromptTokens = c.vpWhole * (1 - c.vpHit)
		}
		p.VPAssumed, p.PrefixAttention = c.assumed, c.prefixAttention
		k := mustAnswer(t, PrefillKnee(p))
		if k.HasFlag(VPShapeMoved) != c.moved {
			t.Errorf("%s: flags %v, want VPShapeMoved %v", c.name, k.Flags, c.moved)
		}
	}
}

// With the cached context read, VPShapeMoved compares each side's cached
// share of the whole prompt, as the rescale prices the span, not the
// prefix-cache hit rates (issue llm-d-incubation/llm-d-scaling-manager#383).
// On a fleet with a host tier the GPU's hit rate sees part of the cache: two
// sides at the same cached share but read with different GPU hit rates have
// not moved, and two at the same GPU hit rate with different cached shares
// have.
func TestVPShapeMoved_ComparesTheCachedShares(t *testing.T) {
	for _, c := range []struct {
		name          string
		hit, vpHit    float64
		cached, vpCtx float64
		moved         bool
	}{
		// 8k computed on both sides, 24k cached (0.75 of the whole) on both.
		{"same share, GPU hit rates apart", 0.1, 0.5, 24000, 24000, false},
		// The same GPU hit rate, but 0.75 of the whole cached against 0.5.
		{"same GPU hit rate, shares apart", 0.25, 0.25, 24000, 8000, true},
		{"same share, same hit", 0.25, 0.25, 24000, 24000, false},
	} {
		p := hitFleet(8000, 8000+c.cached, c.hit)
		p.VPPromptTokens, p.VPWholePromptTokens, p.VPPromptHitRate = 8000, 8000+c.vpCtx, c.vpHit
		p.PrefixAttention = true
		p.CachedContextRead, p.CachedContextTokens, p.VPCachedContextTokens = true, c.cached, c.vpCtx
		k := mustAnswer(t, PrefillKnee(p))
		if k.HasFlag(VPShapeMoved) != c.moved {
			t.Errorf("%s: flags %v, want VPShapeMoved %v", c.name, k.Flags, c.moved)
		}
	}
}

// A hit rate outside [0, 1) or a negative whole prompt is the caller's
// mistake.
func TestPrefill_PrefixInputsChecked(t *testing.T) {
	for name, p := range map[string]Prefill{
		"hit 1":           hitFleet(8000, 32000, 1),
		"V_P's hit below": func() Prefill { p := hitFleet(8000, 8000, 0); p.VPPromptHitRate = -0.1; return p }(),
		"whole below 0":   hitFleet(8000, -1, 0),
	} {
		u, ok := PrefillKnee(p).(Unanswerable)
		if !ok || u.Reason != InvalidInput {
			t.Errorf("%s: %#v, want InvalidInput", name, PrefillKnee(p))
		}
	}
	// A hit rate beside an unknown whole prompt is read by nothing: priced
	// as before, not refused.
	unknownVP := hitFleet(8000, 8000, 0)
	unknownVP.VPWholePromptTokens, unknownVP.VPPromptHitRate = 0, 1
	for name, p := range map[string]Prefill{
		"hit 1, whole unknown":       hitFleet(8000, 0, 1),
		"V_P's hit 1, whole unknown": unknownVP,
	} {
		if _, ok := PrefillKnee(p).(Answer); !ok {
			t.Errorf("%s: %#v, want an answer", name, PrefillKnee(p))
		}
	}
}
