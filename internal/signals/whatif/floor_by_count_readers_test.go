package whatif

import (
	"testing"
)

// The knee, the admission chain, the TPOT-target inversion and the TTFT's
// decode step read the floor at their own count, as the TPOT mean does
// (issue llm-d-incubation/llm-d-scaling-manager#485). Each test prices a
// figure below the lowest count the floor was read at (14.7 sequences a
// replica on the 0.6B warm-up's first fold) and checks it is held to the
// floor by count, not to LeastITL, and flagged ExtrapolatedBelowFloor. On the
// parent commit each reads LeastITL, 2.375 ms, and carries no flag.

// byCountAt is the floor by count at n sequences, in seconds.
func byCountAt(d Decode, n float64) float64 {
	return d.BelowBand.ByCount.A*n + d.BelowBand.ByCount.B
}

// A replica capped at 2 sequences saturates well under the floor's counts:
// its knee is read at the floor by count at 2, not at LeastITL.
func TestFloorByCount_Knee(t *testing.T) {
	d := zeroSixBFold1(1500, 500)
	d.ReplicaCap = 2
	a := d.kneeAnswer()
	near(t, "the knee's ITL", d.itlAt(a.Basis.Fill), byCountAt(d, 2), 1e-12)
	relNear(t, "the knee", a.Value, 2*2/(500*byCountAt(d, 2)), 1e-12)
	if !a.HasFlag(ExtrapolatedBelowFloor) || !a.HasFlag(BelowFittedBand) {
		t.Errorf("knee flags %v, want ExtrapolatedBelowFloor and BelowFittedBand", a.Flags)
	}
	// At the floor's counts and above, the knee is LeastITL's, unflagged.
	d.ReplicaCap = 20
	if a := d.kneeAnswer(); a.HasFlag(ExtrapolatedBelowFloor) {
		t.Errorf("a knee at 20 sequences flagged: %v", a.Flags)
	}
}

// The admission chain's state m completes at the floor by count at m: one
// sequence steps at the floor read at 1, and a light rate, whose mean
// sequences sit under the floor's counts, is flagged.
func TestFloorByCount_Admission(t *testing.T) {
	d := zeroSixBFold1(1500, 500)
	relNear(t, "chainMu(1)", d.chainMu(1), 1/(500*byCountAt(d, 1)), 1e-12)
	r := DecodeAdmission(d, 1)
	a, ok := r.(Admission)
	if !ok {
		t.Fatalf("want an admission, got %#v", r)
	}
	if !a.HasFlag(ExtrapolatedBelowFloor) {
		t.Errorf("admission at 1 r/s flags %v, want ExtrapolatedBelowFloor", a.Flags)
	}
}

// A TPOT target under LeastITL but over the floor by count at one sequence is
// met at the count where the floor by count reaches it, found by searching
// the count; the rate answered prices that TPOT mean back (SLOAtReplicas at
// the same rate). A target under the floor at no sequences is still refused.
func TestFloorByCount_RateForTPOT(t *testing.T) {
	d := zeroSixBFold1(1500, 500)
	const target = 0.002 // under LeastITL, 2.375 ms
	r := RateForTPOT(d, RateQuery{Target: TPOTTarget{Seconds: target}, Headroom: 1})
	a, ok := r.(Answer)
	if !ok {
		t.Fatalf("target %g s: want an answer, got %#v", target, r)
	}
	if !a.HasFlag(ExtrapolatedBelowFloor) {
		t.Errorf("flags %v, want ExtrapolatedBelowFloor", a.Flags)
	}
	n := (target - d.BelowBand.ByCount.B) / d.BelowBand.ByCount.A
	relNear(t, "the answer's sequences", CappedSequences(a.Basis.Fill, d.CapacityTokens, d.KVPerRequest, 256), n, 1e-9)
	near(t, "the answer's floor", a.Basis.Floor, target, 1e-12)
	back := SLOAtReplicas(d, ReplicasQuery{Rate: a.Value})
	mean, ok := back.MeanTPOT.(Answer)
	if !ok {
		t.Fatalf("SLOAtReplicas at %g r/s: %#v", a.Value, back.MeanTPOT)
	}
	relNear(t, "the TPOT mean at the answered rate", mean.Value, target, 1e-6)

	if r := RateForTPOT(d, RateQuery{Target: TPOTTarget{Seconds: 0.0017}, Headroom: 1}); !isRefusal(r, TargetBelowFloor) {
		t.Errorf("target under the floor by count at 0 sequences: %#v, want TargetBelowFloor", r)
	}
	// A target over LeastITL is answered as before, unflagged.
	if a, ok := RateForTPOT(d, RateQuery{Target: TPOTTarget{Seconds: 0.004}, Headroom: 1}).(Answer); !ok || a.HasFlag(ExtrapolatedBelowFloor) {
		t.Errorf("target 4 ms: %#v, want an unflagged answer", a)
	}
}

// The TTFT's decode first step at a light rate is read at the floor by count
// at the request's own count, as its TPOT is, and flagged.
func TestFloorByCount_TTFTDecodeStep(t *testing.T) {
	fl := workedFleet()
	d := zeroSixBFold1(1500, 500)
	fl.Decode = &d
	const rate = 0.5
	got := mustTTFT(t, TTFTAt(fl, rate, 0))
	k, ok := d.fillAtReplicaRate(rate / float64(d.Replicas))
	if !ok {
		t.Fatalf("no fill at %g r/s", rate)
	}
	n := CappedSequences(k, d.CapacityTokens, d.KVPerRequest, float64(d.ReplicaCap))
	near(t, "the first step", got.Parts.FirstStep, byCountAt(d, n), 1e-12)
	if !got.HasFlag(ExtrapolatedBelowFloor) {
		t.Errorf("flags %v, want ExtrapolatedBelowFloor", got.Flags)
	}
}
