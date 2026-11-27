package whatif

import (
	"slices"
	"testing"
)

// A learned V_P priced at a prompt the minutes it was read over did not run
// (Prefill.promptNotCovered; issue
// llm-d-incubation/llm-d-scaling-manager#405).

// coveredFleet is the worked fleet with a learned V_P read over prompts of
// lo to hi tokens, priced at a mean prompt of prompt.
func coveredFleet(lo, hi, prompt float64) TTFTFleet {
	fl := workedFleet()
	d := *fl.Decode
	fl.Decode = &d
	fl.Prefill.VPAssumed = false
	fl.Prefill.VPPromptTokens = (lo + hi) / 2
	fl.Prefill.VPPromptLow, fl.Prefill.VPPromptHigh = lo, hi
	fl.Prefill.PromptTokens = prompt
	return fl
}

// Outside the range by more than MaxVPShapeRatio, either way, no TTFT is
// priced: the refusal names V_P's prompts and carries VPPromptNotCovered,
// and the knee is still given, flagged.
func TestPromptNotCovered_PricesNoTTFTOutsideTheRange(t *testing.T) {
	for name, prompt := range map[string]float64{"shorter": 1000, "longer": 30000} {
		fl := coveredFleet(7900, 8100, prompt)
		u, ok := ttftAt(t, fl, 0.5, 0.9).(Unanswerable)
		if !ok || u.Reason != NoPrefillCapacity || !slices.Contains(u.Flags, VPPromptNotCovered) {
			t.Errorf("%s: %#v, want NoPrefillCapacity flagged VPPromptNotCovered", name, ttftAt(t, fl, 0.5, 0.9))
		}
		k, ok := PrefillKnee(fl.Prefill).(Answer)
		if !ok || !slices.Contains(k.Flags, VPPromptNotCovered) {
			t.Errorf("%s: knee %#v, want it flagged VPPromptNotCovered", name, PrefillKnee(fl.Prefill))
		}
		if v := DefaultEnvelope().Judge(Figure{Kind: FigurePrefillKnee, Flags: k.Flags}); v.Validated() {
			t.Errorf("%s: a knee from a V_P read at other prompts judged in region: %+v", name, v)
		}
	}
}

// Inside the range, or within MaxVPShapeRatio of it, the figure is priced
// as before; an unknown range or an assumed V_P is never refused for it.
func TestPromptNotCovered_InsideTheRangeIsPriced(t *testing.T) {
	for name, fl := range map[string]TTFTFleet{
		"inside":        coveredFleet(800, 1200, 1000),
		"within 1.2x":   coveredFleet(1150, 1500, 1000),
		"unknown range": coveredFleet(0, 0, 1000),
	} {
		v := mustTTFT(t, ttftAt(t, fl, 0.5, 0.9))
		if v.HasFlag(VPPromptNotCovered) {
			t.Errorf("%s: flagged VPPromptNotCovered: %v", name, v.Flags)
		}
	}
	fl := coveredFleet(7900, 8100, 1000)
	fl.Prefill.VPAssumed = true
	if v := mustTTFT(t, ttftAt(t, fl, 0.5, 0.9)); v.HasFlag(VPPromptNotCovered) {
		t.Errorf("an assumed V_P flagged VPPromptNotCovered: %v", v.Flags)
	}
	bad := coveredFleet(2000, 1000, 1000)
	if u, ok := ttftAt(t, bad, 0.5, 0.9).(Unanswerable); !ok || u.Reason != InvalidInput {
		t.Errorf("a range with low above high: %#v, want InvalidInput", ttftAt(t, bad, 0.5, 0.9))
	}
}
