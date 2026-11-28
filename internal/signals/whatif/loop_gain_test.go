package whatif

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// smallIntercept is a decode whose intercept is small beside its slope, as on
// an engine whose step the KV fill sets: two replicas, 8k-token requests, 200
// output tokens.
func smallIntercept() Decode {
	return Decode{
		Line:           itl.Model{A: 0.040, B: 0.004},
		Band:           DefaultBand(),
		CapacityTokens: 400000,
		KVPerRequest:   8400,
		OutputTokens:   200,
		ReplicaCap:     256,
		Replicas:       2,
	}
}

// rateAtGain is the fleet rate at which d's loop gain is g: g = lam*S*KVreq/C*A.
func rateAtGain(d Decode, g float64) float64 {
	return g * d.CapacityTokens / (d.steps() * d.KVPerRequest * d.Line.A) * float64(d.Replicas)
}

// The gain is the fill term's share of the ITL at the priced fill, A*k/(A*k +
// B), and a 5% steeper slope moves the TPOT by (1 - g)/(1 - 1.05*g): the
// closed form's amplification, with no HeldBefore, burst or batch term.
func TestLoopGainAt_IsTheFillTermShare(t *testing.T) {
	d := smallIntercept()
	for _, want := range []float64{0.7, 0.79, 0.81} {
		rate := rateAtGain(d, want)
		g, ok := LoopGainAt(d, rate)
		if !ok {
			t.Fatalf("g %.2f: not priced at %.4g r/s", want, rate)
		}
		k := mustAnswer(t, FillAtRate(d, rate)).Value
		near(t, "gain", g.Gain, want, 1e-9)
		near(t, "gain against the fill", g.Gain, d.Line.A*k/(d.Line.A*k+d.Line.B), 1e-9)
		near(t, "move", g.Move, (1-want)/(1-(1+LoopGainSlopeError)*want), 1e-9)
		if g.Saturates {
			t.Errorf("g %.2f: a 5%% steeper slope saturates", want)
		}
	}
}

// The threshold is where a 5% slope error alone puts the TPOT 1.25x off:
// g = 0.8 with no HeldBefore.
func TestLoopGainAt_FlagsFromPointEight(t *testing.T) {
	d := smallIntercept()
	for _, c := range []struct {
		g    float64
		high bool
	}{{0.5, false}, {0.79, false}, {0.81, true}} {
		g, ok := LoopGainAt(d, rateAtGain(d, c.g))
		if !ok {
			t.Fatalf("g %.2f: not priced", c.g)
		}
		if g.High() != c.high {
			t.Errorf("g %.2f: High %v, want %v (%s)", c.g, g.High(), c.high, g)
		}
	}
}

// Near the knee a 5% steeper slope lowers the knee past the rate: the
// replicas saturate, which is flagged, and the move is not a number.
func TestLoopGainAt_SaturatesNearTheKnee(t *testing.T) {
	d := smallIntercept()
	knee := mustAnswer(t, KneeRate(d)).Value
	g, ok := LoopGainAt(d, 0.99*knee)
	if !ok {
		t.Fatal("not priced at 0.99 of the knee")
	}
	if !g.Saturates || g.Move != 0 || !g.High() {
		t.Errorf("at 0.99 of the knee: %+v, want saturated and high", g)
	}
	if !(g.KneeGain >= g.Gain) {
		t.Errorf("knee gain %.3f under the rate's %.3f", g.KneeGain, g.Gain)
	}
	if _, ok := LoopGainAt(d, knee); ok {
		t.Error("priced at the knee, where no fill carries the rate")
	}
}

// A large intercept keeps the gain low at every rate under the knee: the knee
// gain, the line's own, says no rate is flagged.
func TestLoopGainAt_LargeInterceptIsNeverFlagged(t *testing.T) {
	d := smallIntercept()
	d.Line.B = 0.030
	knee := mustAnswer(t, KneeRate(d)).Value
	g, ok := LoopGainAt(d, 0.85*knee)
	if !ok {
		t.Fatal("not priced at 0.85 of the knee")
	}
	if g.KneeGain >= 0.8 || g.High() {
		t.Errorf("%+v: want a knee gain under 0.8 and no flag", g)
	}
}

// A KV pull held before the first token adds to the residence but not to the
// loop: the gain is still lam*S*KVreq/C*A, and the move is what repricing the
// figure with the steeper slope reads.
func TestLoopGainAt_HeldBeforeRepricesTheFigure(t *testing.T) {
	d := smallIntercept()
	rate := rateAtGain(d, 0.7)
	d.HeldBefore = 0.05
	held, ok := LoopGainAt(d, rate)
	if !ok {
		t.Fatal("not priced with a pull")
	}
	near(t, "gain with a pull", held.Gain, rate/float64(d.Replicas)*d.steps()*d.KVPerRequest/d.CapacityTokens*d.Line.A, 1e-12)
	steep := d
	steep.Line.A *= 1 + LoopGainSlopeError
	want := mustAnswer(t, SLOAtReplicas(steep, ReplicasQuery{Rate: rate}).MeanTPOT).Value /
		mustAnswer(t, SLOAtReplicas(d, ReplicasQuery{Rate: rate}).MeanTPOT).Value
	near(t, "move with a pull", held.Move, want, 1e-12)
	if !(held.Move > 1) {
		t.Errorf("move %.4f with a pull, want above 1", held.Move)
	}
}

// A refused decode, or a refused rate, has no loop gain.
func TestLoopGainAt_RefusedHasNone(t *testing.T) {
	d := smallIntercept()
	if _, ok := LoopGainAt(d, -1); ok {
		t.Error("a negative rate priced")
	}
	d.Line = itl.Model{}
	if _, ok := LoopGainAt(d, 1); ok {
		t.Error("a decode with no line priced")
	}
}
