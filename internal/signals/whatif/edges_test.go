package whatif

import (
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"testing"
)

// A batch cap whose slots fill less than the band's bottom: the knee is read at
// the band's bottom, as ReachableFill reads it, and flagged, so question 1 and
// question 2 agree on it. Before, the knee read the line extrapolated below
// the band, unflagged: 2.27 r/s on this fleet, 26% above the 1.80 the
// band's bottom gives, while question 2 at the same fleet already read
// 2.22 ms at the bottom.
func TestKneeBelowTheFittedBand(t *testing.T) {
	d := olsSweep()
	d.ReplicaCap = 4 // 4 * 1500 / 131072 = 0.046, under the band's 0.15
	floor := d.Line.ITLAt(d.Band.Lo)
	want := 4 / (floor * d.OutputTokens)

	knee := mustAnswer(t, KneeRate(d))
	near(t, "knee", knee.Value, want, 1e-12)
	near(t, "knee's fill", knee.Basis.Fill, d.Band.Lo, 0)
	if !knee.HasFlag(BelowFittedBand) || !slices.Equal(knee.Basis.BoundBy, []Bound{BoundBatchCap}) {
		t.Errorf("knee flags %v bound %v, want BelowFittedBand and the batch cap", knee.Flags, knee.Basis.BoundBy)
	}

	slo := SLOAtReplicas(d, ReplicasQuery{Headroom: 0.85})
	if !mustAnswer(t, slo.KneeRate).HasFlag(BelowFittedBand) {
		t.Error("question 2's knee is not flagged")
	}
	mean := mustAnswer(t, slo.MeanTPOT)
	near(t, "question 2's TPOT", mean.Value, floor, 1e-15)
	if !mean.HasFlag(BelowFittedBand) {
		t.Error("question 2's TPOT is not flagged")
	}

	// Question 1 with no cap lands on the same knee, bound by the batch cap,
	// not by an operating point it does not have.
	q1 := mustAnswer(t, rateFor(d, TPOTTarget{Seconds: 0.004}, 1))
	near(t, "question 1 at headroom 1", q1.Value, knee.Value, 1e-12)
	if !slices.Equal(q1.Basis.BoundBy, []Bound{BoundBatchCap}) || !q1.HasFlag(BelowFittedBand) {
		t.Errorf("question 1 at headroom 1: bound %v flags %v", q1.Basis.BoundBy, q1.Flags)
	}
	// Every state of the chain fills less than the band's bottom, so each is
	// read there: the chain is M/M/4 at one ITL, and its wait is Erlang C.
	adm, ok := DecodeAdmission(d, 0.9*knee.Value).(Admission)
	if !ok {
		t.Fatal("no admission at 0.9 of the knee")
	}
	mu1 := 1 / (d.OutputTokens * floor)
	near(t, "chain below the band", adm.WaitProbability, erlangC(4, 0.9*knee.Value/mu1), 1e-12)

	// Capped at a small headroom the fill is below the band too, and says so.
	q1c := mustAnswer(t, rateFor(d, TPOTTarget{Seconds: 0.004}, 0.85))
	near(t, "question 1 at 0.85", q1c.Value, 0.85*knee.Value, 1e-12)
	if !slices.Equal(q1c.Basis.BoundBy, []Bound{BoundOperatingPoint}) || !q1c.HasFlag(BelowFittedBand) || !(q1c.Basis.Fill < d.Band.Lo) {
		t.Errorf("question 1 at 0.85: bound %v flags %v fill %g", q1c.Basis.BoundBy, q1c.Flags, q1c.Basis.Fill)
	}
}

// A fill below the band is read at the band's bottom and flagged; inside the
// band it is not.
func TestBelowFittedBandIsFlagged(t *testing.T) {
	d := olsSweep()
	low := SLOAtReplicas(d, ReplicasQuery{Rate: 1})
	mean := mustAnswer(t, low.MeanTPOT)
	if !(mustAnswer(t, low.Rate).Basis.Fill < d.Band.Lo) {
		t.Fatalf("fill %g at 1 r/s is not below the band", mustAnswer(t, low.Rate).Basis.Fill)
	}
	near(t, "TPOT below the band", mean.Value, d.Line.ITLAt(d.Band.Lo), 1e-15)
	if !mean.HasFlag(BelowFittedBand) {
		t.Errorf("a fill below the band is not flagged: %v", mean.Flags)
	}
	if op := mustAnswer(t, SLOAtReplicas(d, ReplicasQuery{Headroom: 0.85}).MeanTPOT); op.HasFlag(BelowFittedBand) {
		t.Errorf("the operating point at fill 0.474 is flagged")
	}
}

// kneeDraws is how many random fleets the knee and chain are checked on: the
// batch cap binds in about 8% of them (76 on the seeded draw) and the knee falls
// below the band in about 2.5% (25), so 1000 covers each with a margin.
const kneeDraws = 1000

// The knee and the admission chain saturate at the same rate, whether the
// batch cap sets the slots inside the band, below it, at the tie with the
// band's top, or the band's top sets them (KV-bound): below the knee both
// answer, at or above it both refuse. The random draws must cover each of
// those cases, or the property is not being exercised.
func TestKneeAndChainSaturateTogether(t *testing.T) {
	named := map[string]Decode{}
	named["batch cap inside the band"] = olsSweep()
	top := olsSweep()
	top.ReplicaCap = 256 // the band's top holds floor(0.8 * 131072 / 1500) = 69
	named["band top (KV-bound)"] = top
	tie := olsSweep()
	tie.ReplicaCap = 69 // the cap equals what the band's top holds
	named["batch cap tied with the band top"] = tie
	below := olsSweep()
	below.ReplicaCap = 4
	named["batch cap below the band"] = below

	check := func(name string, d Decode) Answer {
		kneeAnswer := mustAnswer(t, KneeRate(d))
		knee := kneeAnswer.Value
		for _, f := range []float64{1, 1.02} {
			if u, ok := FillAtRate(d, f*knee).(Unanswerable); !ok || u.Reason != Saturated {
				t.Errorf("%s: FillAtRate at %g of the knee: %#v", name, f, FillAtRate(d, f*knee))
			}
			if u, ok := DecodeAdmission(d, f*knee).(Unanswerable); !ok || u.Reason != Saturated {
				t.Errorf("%s: DecodeAdmission at %g of the knee: %#v", name, f, DecodeAdmission(d, f*knee))
			}
		}
		if _, ok := FillAtRate(d, 0.999*knee).(Answer); !ok {
			t.Errorf("%s: FillAtRate refuses below the knee", name)
		}
		a, ok := DecodeAdmission(d, 0.999*knee).(Admission)
		if !ok {
			t.Errorf("%s: DecodeAdmission refuses below the knee: %#v", name, DecodeAdmission(d, 0.999*knee))
			return kneeAnswer
		}
		near(t, name+": chain's knee", a.Basis.KneeRate, knee, 0)
		kvBound := kneeAnswer.Basis.BoundBy[0] == BoundBandTop
		if a.HasFlag(KVBoundSlots) != kvBound {
			t.Errorf("%s: KVBoundSlots %v, band top bound %v", name, a.HasFlag(KVBoundSlots), kvBound)
		}
		return kneeAnswer
	}
	for name, d := range named {
		check(name, d)
	}
	if by := mustAnswer(t, KneeRate(tie)).Basis.BoundBy; !slices.Equal(by, []Bound{BoundBatchCap}) {
		t.Errorf("the tie is bound by %v, want the batch cap", by)
	}

	r := rand.New(rand.NewPCG(12, 13))
	byCap, byTop, belowBand := 0, 0, 0
	for i := range kneeDraws {
		k := check(fmt.Sprintf("random %d", i), randomDecode(r))
		switch k.Basis.BoundBy[0] {
		case BoundBatchCap:
			byCap++
		case BoundBandTop:
			byTop++
		}
		if k.HasFlag(BelowFittedBand) {
			belowBand++
		}
	}
	t.Logf("of %d random fleets: %d batch-cap bound, %d band-top bound, %d below the band", kneeDraws, byCap, byTop, belowBand)
	if byCap < 50 || byTop < 500 || belowBand < 15 {
		t.Errorf("random fleets cover too little: %d batch cap, %d band top, %d below the band; want 50, 500, 15",
			byCap, byTop, belowBand)
	}
}

// The chain holds one state per slot, so a slot count read absurdly large is
// refused before anything is allocated, and fast.
func TestChainSizeIsBounded(t *testing.T) {
	for name, d := range map[string]Decode{
		"a MaxInt64 cap": func() Decode { d := olsSweep(); d.ReplicaCap, d.CapacityTokens = math.MaxInt64, 1e300; return d }(),
		"a 1e10 cap":     func() Decode { d := olsSweep(); d.ReplicaCap, d.CapacityTokens = 1e10, 1e16; return d }(),
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		res := DecodeAdmission(d, 1)
		runtime.ReadMemStats(&after)
		if u, ok := res.(Unanswerable); !ok || u.Reason != TooManySlots {
			t.Errorf("%s: %#v, want TooManySlots", name, res)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
			t.Errorf("%s: allocated %d bytes refusing", name, grew)
		}
	}
	inf := olsSweep()
	inf.CapacityTokens = math.Inf(1)
	if u, ok := DecodeAdmission(inf, 1).(Unanswerable); !ok || u.Reason != NoCapacity {
		t.Errorf("an infinite capacity: %#v, want NoCapacity", DecodeAdmission(inf, 1))
	}
	// The solver guards itself too: one slot past the bound (8 MB, safe to
	// try) is refused, not solved.
	if _, ok := birthDeath(1, func(m int) float64 { return float64(m) }, maxSlots+1); ok {
		t.Errorf("birthDeath solved %d slots, above maxSlots", maxSlots+1)
	}
	// At the bound itself the chain answers.
	atMax := olsSweep()
	atMax.ReplicaCap, atMax.CapacityTokens = maxSlots, 1e16
	if _, ok := DecodeAdmission(atMax, 1).(Admission); !ok {
		t.Errorf("%d slots: %#v, want an answer", maxSlots, DecodeAdmission(atMax, 1))
	}
}

// The floor is inclusive: a target equal to ITL(band.Lo)*R_q is below what
// any fleet reaches, and the next float above it is not.
func TestFloorBoundary(t *testing.T) {
	d := olsSweep()
	for _, tg := range []TPOTTarget{{}, {Percentile: 0.9, Ratio: TPOTRatio{Value: 1.3}}} {
		r := 1.0
		if tg.Percentile > 0 {
			r = tg.Ratio.Value
		}
		tg.Seconds = d.Line.ITLAt(d.Band.Lo) * r
		if u, ok := rateFor(d, tg, 0.85).(Unanswerable); !ok || u.Reason != TargetBelowFloor {
			t.Errorf("a target at the floor %g: %#v", tg.Seconds, rateFor(d, tg, 0.85))
		}
		tg.Seconds = math.Nextafter(tg.Seconds, 1)
		if _, ok := rateFor(d, tg, 0.85).(Answer); !ok {
			t.Errorf("a target just above the floor %g is refused", tg.Seconds)
		}
	}
}

// Question 2 refuses a headroom of 1 (the knee is no operating point);
// question 1 takes it as "no cap" and does not name the operating point.
func TestHeadroomOne(t *testing.T) {
	d := olsSweep()
	slo := SLOAtReplicas(d, ReplicasQuery{Headroom: 1})
	if u, ok := slo.Rate.(Unanswerable); !ok || u.Reason != InvalidInput {
		t.Errorf("question 2 at headroom 1: %#v", slo.Rate)
	}
	if _, ok := slo.KneeRate.(Answer); !ok {
		t.Errorf("question 2 at headroom 1 lost the knee: %#v", slo.KneeRate)
	}
	for _, target := range []float64{0.003, 1.0} {
		a := mustAnswer(t, rateFor(d, TPOTTarget{Seconds: target}, 1))
		if slices.Contains(a.Basis.BoundBy, BoundOperatingPoint) {
			t.Errorf("target %g at headroom 1: bound by %v", target, a.Basis.BoundBy)
		}
	}
}
