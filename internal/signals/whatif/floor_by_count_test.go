package whatif

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// zeroSixBFold1 is the decode role of the Qwen3-0.6B P/D staircase warm-up
// (two prefill and two decode replicas, flow control off) as the planner
// priced it from a fit to 23:34, its self-check's first fold: the held line
// (A = 30.72 ms, B = 1.13 ms), C = 1,163,136 tokens, batch cap 256, and the
// floor below the band, 2.375 ms, read in the 1000-in / 1000-out phase at 15
// to 111 sequences a replica, with the line through its floors by count
// (learnreplay's VariantState.BelowBandITL, BelowBandLeastSequences,
// BelowBandByCount). kvPerRequest and outputTokens are the step's.
func zeroSixBFold1(kvPerRequest, outputTokens float64) Decode {
	return Decode{
		Line:           itl.Model{A: 0.030721074027870363, B: 0.001129303622146004},
		Band:           DefaultBand(),
		CapacityTokens: 1163136,
		KVPerRequest:   kvPerRequest,
		OutputTokens:   outputTokens,
		ReplicaCap:     256,
		Replicas:       2,
		Tier:           TierHeld,
		BelowBand: BelowBand{Read: true, LeastITL: 0.002375, LeastSequences: 14.727,
			ByCount: itl.Model{A: 4.0969367219560485e-05, B: 0.0017510469056899467}},
	}
}

// zeroSixBStep is one five-minute step the warm-up ran after the fold's fit:
// the rate sent, the step's prompt and output lengths, and the mean TPOT the
// router measured.
type zeroSixBStep struct {
	at       string
	rate     float64
	in, out  float64
	measured float64 // seconds
}

// zeroSixBFold1Steps are the steps the fold judged: prompts of 1k to 8k tokens
// with 8 to 32 output tokens, the decode replicas at a fill of 0.001 or less
// with well under one sequence each.
var zeroSixBFold1Steps = []zeroSixBStep{
	{"23:40", 10.86, 1322, 31.5, 0.001671}, {"23:45", 7.37, 2284, 31.5, 0.001705},
	{"23:50", 5.22, 4015, 31.5, 0.001746}, {"23:55", 3.77, 5732, 31.5, 0.001783},
	{"00:00", 2.81, 7415, 31.5, 0.001816}, {"00:05", 58.83, 7995, 8.9, 0.001556},
	{"00:10", 71.95, 7996, 7.5, 0.001562}, {"00:15", 28.76, 8003, 7.5, 0.001581},
	{"00:20", 2.73, 8006, 21.2, 0.001726},
}

// A floor read at 15 sequences a replica or more does not stand for a replica
// running well under one (issue llm-d-incubation/llm-d-scaling-manager#286):
// each step of the 0.6B warm-up's first fold reads its mean TPOT within 20%
// of what the router measured, held to the floor kept by count at its own
// count, and flagged ExtrapolatedBelowFloor. The negative control is the
// floor taken as one number (LeastSequences unknown), as before: every step
// reads the 2.375 ms floor, 1.3x to 1.5x what was measured.
func TestMeanTPOTBelowTheFloorsCounts_ZeroSixBFold1(t *testing.T) {
	for _, s := range zeroSixBFold1Steps {
		d := zeroSixBFold1(s.in+s.out, s.out)
		a := mustAnswer(t, SLOAtReplicas(d, ReplicasQuery{Rate: s.rate}).MeanTPOT)
		if n := CappedSequences(a.Basis.Fill, d.CapacityTokens, d.KVPerRequest, float64(d.ReplicaCap)); !(n < 1) {
			t.Fatalf("%s: premise: %.2f sequences a replica, want under one", s.at, n)
		}
		if r := a.Value / s.measured; r < 0.8 || r > 1.2 {
			t.Errorf("%s: mean TPOT %.3f ms, measured %.3f ms: ratio %.2f, want within 20%%", s.at, 1e3*a.Value, 1e3*s.measured, r)
		}
		if !a.HasFlag(ExtrapolatedBelowFloor) {
			t.Errorf("%s: not flagged %s: %v", s.at, ExtrapolatedBelowFloor, a.Flags)
		}

		d.BelowBand.LeastSequences = 0
		one := mustAnswer(t, SLOAtReplicas(d, ReplicasQuery{Rate: s.rate}).MeanTPOT)
		near(t, s.at+": negative control, the floor as one number", one.Value, d.BelowBand.LeastITL, 1e-12)
		if r := one.Value / s.measured; r < 1.3 {
			t.Errorf("%s: negative control: ratio %.2f, want the 1.3x or more the floor read", s.at, r)
		}
		if one.HasFlag(ExtrapolatedBelowFloor) {
			t.Errorf("%s: negative control flagged %s", s.at, ExtrapolatedBelowFloor)
		}
	}
}

// At the floor's own counts and above, the floor is LeastITL as before; under
// them it is the floor by count at the TPOT's own count, never above
// LeastITL, never under the line itself, and with no line by count the
// lowest-count floor, LeastITL, still flagged.
func TestMeanTPOTBelowTheFloorsCounts_ReadsTheFloorAtItsCount(t *testing.T) {
	d := zeroSixBFold1(1500, 500)
	// 30 sequences a replica: k = 30 * 1500 / C.
	at30 := 30 * d.KVPerRequest / d.CapacityTokens
	fill, extrapolated := d.lowestFillAt(at30)
	near(t, "at 30 sequences, the floor's own fill", fill, d.lowestFill(), 0)
	if extrapolated {
		t.Errorf("30 sequences, above the floor's 14.7, flagged")
	}

	at2 := 2 * d.KVPerRequest / d.CapacityTokens
	fill, extrapolated = d.lowestFillAt(at2)
	want := d.BelowBand.ByCount.A*2 + d.BelowBand.ByCount.B
	near(t, "at 2 sequences, the floor by count", d.Line.ITLAt(fill), want, 1e-12)
	if !extrapolated {
		t.Errorf("2 sequences, under the floor's 14.7, not flagged")
	}

	// A line by count that reads over LeastITL does not raise it.
	high := d
	high.BelowBand.ByCount = itl.Model{A: 1e-4, B: 0.003}
	fill, _ = high.lowestFillAt(at2)
	near(t, "a floor by count over LeastITL", fill, high.lowestFill(), 0)

	// A floor by count under B leaves the line itself.
	low := d
	low.BelowBand.ByCount = itl.Model{A: 1e-6, B: 0.0005}
	fill, _ = low.lowestFillAt(at2)
	near(t, "a floor by count under B", fill, 0, 0)

	// No line by count: the lowest-count floor, flagged all the same.
	none := d
	none.BelowBand.ByCount = itl.Model{}
	fill, extrapolated = none.lowestFillAt(at2)
	near(t, "no line by count", fill, none.lowestFill(), 0)
	if !extrapolated {
		t.Errorf("no line by count, under the floor's counts, not flagged")
	}

	// A line whose intercept is a guess is read at the band's bottom, as before.
	guess := d
	guess.Tier, guess.Line.B = TierPinnedB, itl.DefaultBaselineSec
	fill, extrapolated = guess.lowestFillAt(at2)
	near(t, "a guessed intercept", fill, guess.Band.Lo, 0)
	if extrapolated {
		t.Errorf("a guessed intercept, never read down to the floor, flagged")
	}

	// A valid line by count can read under zero at a count under one, where
	// the replicas of #286 ran: it holds nothing up, and the line is read
	// from its intercept, as under any floor below B. It does not fall back
	// to LeastITL, which would put a lighter load over a heavier one.
	neg := d
	neg.BelowBand.ByCount = itl.Model{A: 1e-3, B: -2e-4}
	if !itl.ValidModel(neg.BelowBand.ByCount.A, neg.BelowBand.ByCount.B) {
		t.Fatalf("premise: %+v is not a valid line", neg.BelowBand.ByCount)
	}
	atTenth := 0.1 * d.KVPerRequest / d.CapacityTokens
	fill, extrapolated = neg.lowestFillAt(atTenth)
	near(t, "a floor by count under zero", fill, 0, 0)
	if !extrapolated {
		t.Errorf("a floor by count under zero, under the floor's counts, not flagged")
	}
	fill, _ = neg.lowestFillAt(at2)
	near(t, "the same line at 2 sequences", neg.Line.ITLAt(fill), neg.BelowBand.ByCount.ITLAt(2), 1e-12)
}

// ExtrapolatedBelowFloor is set only where the floor read at its own counts
// would have held the TPOT up. A TPOT under the floor's counts that the line
// reads above that floor is the line's, unchanged, and not flagged; under the
// floor it is flagged.
func TestMeanTPOTBelowTheFloorsCounts_FlagsOnlyWhereTheFloorHolds(t *testing.T) {
	d := zeroSixBFold1(1500, 500)
	d.BelowBand.LeastSequences = 1000 // every fill below the band is under it
	above := 2 * d.lowestFill()
	if !(above < d.Band.Lo) {
		t.Fatalf("premise: fill %g is not below the band's bottom %g", above, d.Band.Lo)
	}
	a := d.tpotAt(above, 0, one(), nil)
	near(t, "the line above the floor", a.Value, d.Line.ITLAt(above), 1e-12)
	if a.HasFlag(ExtrapolatedBelowFloor) {
		t.Errorf("a TPOT the line reads above the floor flagged: %v", a.Flags)
	}
	under := d.lowestFill() / 2
	if b := d.tpotAt(under, 0, one(), nil); !b.HasFlag(ExtrapolatedBelowFloor) {
		t.Errorf("a TPOT the floor held up not flagged: %v", b.Flags)
	}
}
