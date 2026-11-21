package whatif

import "testing"

// A rate at or past a prefill knee priced from a lower-bound V_P is not
// priced (PastLowerBoundKnee) with TTFTFleet.LowerBoundKneeUnpriced, and
// Saturated without it, as the SLO analyzer reads it; a V_P that is no
// bound, or the caller's, stays Saturated; and past decode's knee the fleet
// saturates on decode whatever V_P is (issue
// llm-d-incubation/llm-d-scaling-manager#280).
func TestTTFT_PastALowerBoundKneeIsNotPriced(t *testing.T) {
	base := workedFleet()
	base.Prefill.VP, base.Prefill.Replicas, base.Prefill.Imbalance = 4000, 2, 1.25
	pk := mustAnswer(t, PrefillKnee(base.Prefill)).Value
	dk := mustAnswer(t, KneeRate(*base.Decode)).Value
	if !(dk > pk*1.5) {
		t.Fatalf("the worked fleet's decode knee %g must sit well above the prefill knee %g", dk, pk)
	}
	for _, tc := range []struct {
		name                string
		lowerBound, assumed bool
		unpriced            bool
		rate                float64
		want                Reason
	}{
		{"lower bound, unpriced", true, false, true, pk * 1.2, PastLowerBoundKnee},
		{"lower bound, at the knee", true, false, true, pk, PastLowerBoundKnee},
		{"lower bound, off", true, false, false, pk * 1.2, Saturated},
		{"no bound", false, false, true, pk * 1.2, Saturated},
		{"assumed", true, true, true, pk * 1.2, Saturated},
		{"past decode's knee", true, false, true, dk * 1.1, Saturated},
		{"at decode's knee", true, false, true, dk, Saturated},
	} {
		fl := base
		fl.Prefill.VPLowerBound, fl.Prefill.VPAssumed, fl.LowerBoundKneeUnpriced = tc.lowerBound, tc.assumed, tc.unpriced
		for _, q := range []float64{0, 0.9} {
			res := within(t, tc.name, func() Result { return TTFTAt(fl, tc.rate, q) })
			u, ok := res.(Unanswerable)
			if !ok || u.Reason != tc.want {
				t.Errorf("%s, q %g: %#v, want %s", tc.name, q, res, tc.want)
			}
		}
	}
	fl := base
	fl.Prefill.VPLowerBound, fl.LowerBoundKneeUnpriced = true, true
	mustTTFT(t, within(t, "under the knee", func() Result { return TTFTAt(fl, pk*0.9, 0.9) }))
}

// Under flow control a rate past a lower-bound prefill knee and past the
// decode gate's onset, under decode's knee, is the miss it was before the
// knee went unpriced: AboveDecodeOnset, not PastLowerBoundKnee.
func TestTTFT_PastALowerBoundKneeKeepsTheDecodeOnset(t *testing.T) {
	fl := workedFleet()
	fl.Prefill.VP, fl.Prefill.Replicas, fl.Prefill.Imbalance = 4000, 2, 1.25
	fl.FlowControl = FlowControl{State: FlowControlOn, QueueThreshold: 5, KVThreshold: 0.8}
	fl.Prefill.VPLowerBound, fl.Prefill.VPAssumed, fl.LowerBoundKneeUnpriced = true, false, true
	pk := mustAnswer(t, PrefillKnee(fl.Prefill)).Value
	dk := mustAnswer(t, KneeRate(*fl.Decode)).Value
	on, ok := FlowControlOnsets(fl).(Onsets)
	if !ok {
		t.Fatalf("no onsets: %#v", FlowControlOnsets(fl))
	}
	if !(pk < on.Decode && on.Decode < dk) {
		t.Fatalf("want prefill knee %g < decode onset %g < decode knee %g", pk, on.Decode, dk)
	}
	for _, c := range []struct {
		rate float64
		want Reason
	}{{on.Decode, AboveDecodeOnset}, {(pk + on.Decode) / 2, PastLowerBoundKnee}} {
		res := within(t, "flow control", func() Result { return TTFTAt(fl, c.rate, 0.9) })
		if u, ok := res.(Unanswerable); !ok || u.Reason != c.want {
			t.Errorf("rate %g: %#v, want %s", c.rate, res, c.want)
		}
	}
}
