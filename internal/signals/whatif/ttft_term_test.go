package whatif

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// stateTerm is a learned term with figures of no recording: D = 2 ms +
// 3e-6*I + 5*ITL, R = 4 ms + 1e-6*I.
func stateTerm() *DecodeTerm {
	return &DecodeTerm{Engine: PromptLine{Base: 0.002, PerToken: 3e-6}, PerStep: 5,
		Hop: PromptLine{Base: 0.004, PerToken: 1e-6}}
}

func termFleet(t *DecodeTerm) TTFTFleet {
	fl := workedFleet()
	fl.DecodeTerm = t
	return fl
}

// The term replaces T_xfer + ITL(k): the hand sum at the fill, T_xfer gone, the
// per-token parts in the own latency, and TransferOmitted lifted, since D holds
// the pull.
func TestTerm_PricesTheHandSum(t *testing.T) {
	fl := termFleet(stateTerm())
	fl.Prefill.TransferSecondsPerToken = 1e-6 // not priced: D holds the pull
	const rate = 8.0
	v := mustTTFT(t, ttftAt(t, fl, rate, 0))
	d := *fl.Decode
	k, ok := d.fillAtReplicaRate(rate / float64(d.Replicas))
	if !ok {
		t.Fatal("no fill")
	}
	i := fl.Prefill.PromptTokens
	want := v.Parts.EngineWait + v.Parts.DecodeWait + fl.Prefill.FixedLatency + i/fl.Prefill.VP +
		0.002 + 3e-6*i + 5*d.Line.ITLAt(k) + 0.004 + 1e-6*i
	relNear(t, "mean", v.Seconds, want, 1e-12)
	relNear(t, "D's fixed part", v.Parts.DecodeEngine, 0.002+5*d.Line.ITLAt(k), 1e-12)
	relNear(t, "R's fixed part", v.Parts.RouterHop, 0.004, 1e-12)
	relNear(t, "own", v.Parts.OwnPrefill, fl.Prefill.FixedLatency+i/fl.Prefill.VP+4e-6*i, 1e-9)
	if v.Parts.FirstStep != 0 || v.HasFlag(TransferOmitted) || v.HasFlag(DecodeTermOmitted) || v.HasFlag(DecodeStepSlopeAssumed) || v.HasFlag(RouterHopAssumed) {
		t.Errorf("first step %g, flags %v", v.Parts.FirstStep, v.Flags)
	}
	if !v.HasFlag(UnvalidatedTTFT) {
		t.Errorf("UnvalidatedTTFT stays: %v", v.Flags)
	}
}

// The line is read at the actual fill below the band, and at fill 0 for the
// floor: at the clamp the staircase's bucket A overshot its floor by 28 ms. The
// flag stands: below the band the line is an extrapolation.
func TestTerm_ReadsTheLineAtTheActualFill(t *testing.T) {
	fl := termFleet(stateTerm())
	d := *fl.Decode
	k, ok := d.fillAtReplicaRate(0.5 / float64(d.Replicas))
	if !ok || !(k < d.Band.Lo) {
		t.Fatalf("want a fill under the band's bottom, got %g", k)
	}
	v := mustTTFT(t, ttftAt(t, fl, 0.5, 0))
	relNear(t, "D at the actual fill", v.Parts.DecodeEngine, 0.002+5*d.Line.ITLAt(k), 1e-12)
	if !v.HasFlag(BelowFittedBand) {
		t.Errorf("flags %v", v.Flags)
	}
	i := fl.Prefill.PromptTokens
	relNear(t, "floor", v.Floor, fl.Prefill.FixedLatency+i/fl.Prefill.VP+0.002+3e-6*i+5*d.Line.B+0.004+1e-6*i, 1e-9)
}

// A target between the old floor and the corrected one refuses, where the old
// sum gave a rate: the goal of the term.
func TestTerm_ATargetBelowTheCorrectedFloorRefuses(t *testing.T) {
	old := workedFleet()
	old.LegacyDecodeSum = true
	oldFloor := mustTTFT(t, ttftAt(t, old, 1, 0)).Floor
	fl := termFleet(stateTerm())
	newFloor := mustTTFT(t, ttftAt(t, fl, 1, 0)).Floor
	if !(newFloor > oldFloor*1.1) {
		t.Fatalf("floors %g, %g", oldFloor, newFloor)
	}
	target := TTFTTarget{Seconds: (oldFloor + newFloor) / 2}
	if a := mustAnswer(t, rateForTTFT(t, old, target)); !(a.Value > 0) {
		t.Errorf("the old sum gave %g", a.Value)
	}
	u, ok := rateForTTFT(t, fl, target).(Unanswerable)
	if !ok || u.Reason != TargetBelowFloor {
		t.Errorf("want TargetBelowFloor, got %#v", rateForTTFT(t, fl, target))
	}
	if a := mustAnswer(t, rateForTTFT(t, fl, TTFTTarget{Seconds: newFloor * 1.5})); !(a.Value > 0) {
		t.Errorf("a target above the floor: %g", a.Value)
	}
}

// TTFT still rises with the rate, so the bisection stands, at each shape.
func TestTerm_RisesWithTheRate(t *testing.T) {
	for _, term := range []*DecodeTerm{stateTerm(), nil,
		{Engine: PromptLine{Base: 0.04, HeldPromptTokens: 500}, PerStep: AssumedDecodeStepSlope, FromITL: 0.01, StepAssumed: true}} {
		fl := termFleet(term)
		pl, u := fl.plan()
		if u != nil {
			t.Fatal(u)
		}
		last := 0.0
		for f := 0.0; f < 0.99; f += 0.02 {
			v := mustTTFT(t, ttftAt(t, fl, f*pl.lowestKnee(), 0.9))
			if v.Seconds < last-1e-12 {
				t.Fatalf("%+v: TTFT fell to %g from %g at %g of the knee", term, v.Seconds, last, f)
			}
			last = v.Seconds
		}
	}
}

// With no term the old sum stands, and says so: never silently. The figures are
// the old sum's to the last digit.
func TestTerm_NoTermIsFlagged(t *testing.T) {
	old := workedFleet()
	old.LegacyDecodeSum = true
	nilTerm := workedFleet()
	for _, q := range []float64{0, 0.5, 0.9, 0.99} {
		o, n := mustTTFT(t, ttftAt(t, old, 8, q)), mustTTFT(t, ttftAt(t, nilTerm, 8, q))
		if o.Seconds != n.Seconds || o.Floor != n.Floor {
			t.Errorf("q %g: %g, %g; floors %g, %g", q, o.Seconds, n.Seconds, o.Floor, n.Floor)
		}
		if o.HasFlag(DecodeTermOmitted) || !n.HasFlag(DecodeTermOmitted) {
			t.Errorf("q %g: flags %v / %v", q, o.Flags, n.Flags)
		}
	}
}

// A fall-back never prices under the old sum, whatever the transfer, the
// measured figure, the prompt or the rate: a term with no fit of D may only
// add, and its flag is on the answer.
func TestTerm_AFallBackIsNeverUnderTheOldSum(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 5))
	for range 2000 {
		fl := workedFleet()
		fl.Prefill.PromptTokens = 200 + 9000*r.Float64()
		fl.Prefill.TransferSecondsPerToken = 1e-5 * r.Float64()
		term := DecodeTerm{Engine: PromptLine{Base: 0.2 * r.Float64() * r.Float64(), HeldPromptTokens: 200 + 9000*r.Float64()},
			PerStep: AssumedDecodeStepSlope, FromITL: 0.02 * r.Float64(), StepAssumed: true, HopAssumed: true,
			Hop: PromptLine{Base: AssumedRouterHopSeconds}}
		if r.IntN(3) == 0 {
			term = DecodeTerm{EngineOmitted: true, HopAssumed: true, Hop: PromptLine{Base: AssumedRouterHopSeconds}}
		}
		fl.DecodeTerm = &term
		old := fl
		old.DecodeTerm, old.LegacyDecodeSum = nil, true
		pl, u := fl.plan()
		if u != nil {
			t.Fatal(u)
		}
		rate := 0.9 * r.Float64() * pl.lowestKnee()
		for _, q := range []float64{0, 0.5, 0.9, 0.99} {
			v, o := mustTTFT(t, ttftAt(t, fl, rate, q)), mustTTFT(t, ttftAt(t, old, rate, q))
			noneUnder(t, fmt.Sprintf("%+v at %g, q %g", term, rate, q), v, o)
			if !v.HasFlag(RouterHopAssumed) || (!v.HasFlag(DecodeStepSlopeAssumed) && !v.HasFlag(DecodeTermOmitted)) {
				t.Fatalf("flags %v", v.Flags)
			}
		}
	}
}

// An omitted D prices the old step and the transfer the caller gave, and only
// that one keeps TransferOmitted: a measured D holds the pull.
func TestTerm_TransferOmittedStandsOnlyWithNoD(t *testing.T) {
	om := mustTTFT(t, ttftAt(t, termFleet(&DecodeTerm{EngineOmitted: true}), 8, 0))
	if !om.HasFlag(TransferOmitted) || om.Parts.FirstStep == 0 || om.Parts.DecodeEngine != 0 {
		t.Errorf("%+v, %v", om.Parts, om.Flags)
	}
	held := mustTTFT(t, ttftAt(t, termFleet(&DecodeTerm{Engine: PromptLine{Base: 0.05, HeldPromptTokens: 1000}, PerStep: 10, StepAssumed: true}), 8, 0))
	if held.HasFlag(TransferOmitted) || held.Parts.DecodeEngine == 0 {
		t.Errorf("%+v, %v", held.Parts, held.Flags)
	}
}

// The measured D holds the pull, and the own latency still holds T_xfer on a
// fall-back: the two are not added. The mean carries D once.
func TestTerm_AFallBackCountsThePullOnce(t *testing.T) {
	plain := termFleet(&DecodeTerm{Engine: PromptLine{Base: 0.05, HeldPromptTokens: 1000}, PerStep: 10, FromITL: 1, StepAssumed: true})
	with := plain
	with.Prefill.TransferSecondsPerToken = 1e-5 // 10 ms at the 1000 tokens, under D
	a, b := mustTTFT(t, ttftAt(t, plain, 2, 0)), mustTTFT(t, ttftAt(t, with, 2, 0))
	relNear(t, "the mean with T_xfer beside D", b.Seconds, a.Seconds, 1e-12)
}

// An assumed slope adds only above the ITL D was measured at: below it D is
// held, the higher figure, and the fit's FromITL of 0 adds the whole step.
func TestTerm_AnAssumedSlopeClimbsFromItsReading(t *testing.T) {
	tp := termPlan{engine: 0.05, perStep: 10, fromITL: 0.004}
	engine, _ := tp.at(0.003, 0)
	relNear(t, "held below", engine, 0.05, 0)
	engine, _ = tp.at(0.006, 0)
	relNear(t, "climbs above", engine, 0.07, 1e-12)
	tp.fromITL = 0
	engine, _ = tp.at(0.006, 0)
	relNear(t, "a fit adds it all", engine, 0.11, 1e-12)
}

func TestPromptLine_Split(t *testing.T) {
	l := PromptLine{Base: 0.04, HeldPromptTokens: 2000}
	if f, p := l.split(1000); f != 0.04 || p != 0 {
		t.Errorf("held for a shorter prompt: %g, %g", f, p)
	}
	if f, p := l.split(4000); f != 0 || p != 2e-5 {
		t.Errorf("scaled up for a longer one: %g, %g", f, p)
	}
	if f, p := (PromptLine{Base: 0.01, PerToken: 3e-6}).split(4000); f != 0.01 || p != 3e-6 {
		t.Errorf("a line: %g, %g", f, p)
	}
}

// A negative intercept with a small step never prices a negative term.
func TestTerm_NeverNegative(t *testing.T) {
	engine, hop := termPlan{engine: -5, perStep: 1, hop: -1}.at(0.01, 0.01)
	if engine != 0 || hop != 0 {
		t.Errorf("engine %g, hop %g", engine, hop)
	}
}

// noneUnder fails if any published figure of v is under the old sum's o.
func noneUnder(t *testing.T, what string, v, o TTFT) {
	t.Helper()
	const tol = 1 - 1e-12
	if v.Seconds < o.Seconds*tol || v.Floor < o.Floor*tol || v.Independent < o.Independent*tol || v.Pooled < o.Pooled*tol {
		t.Fatalf("%s: under the old sum: seconds %g, %g; floor %g, %g; independent %g, %g; pooled %g, %g",
			what, v.Seconds, o.Seconds, v.Floor, o.Floor, v.Independent, o.Independent, v.Pooled, o.Pooled)
	}
}

// The review's probe: a D just over the old step at the band's bottom, read at a
// rate where the old sum stands over it. Every figure printed beside the
// headline stands with it, at the mean and at p90.
func TestTerm_TheFloorHoldsBesideTheHeadline(t *testing.T) {
	old := workedFleet()
	old.LegacyDecodeSum = true
	fl := workedFleet()
	d := *fl.Decode
	fl.DecodeTerm = &DecodeTerm{Engine: PromptLine{Base: 1.01 * d.Line.ITLAt(d.Band.Lo)}}
	for _, q := range []float64{0, 0.9} {
		v, o := mustTTFT(t, ttftAt(t, fl, 8, q)), mustTTFT(t, ttftAt(t, old, 8, q))
		noneUnder(t, fmt.Sprintf("q %g", q), v, o)
		if !v.HasFlag(DecodeTermFloored) {
			t.Errorf("q %g: flags %v", q, v.Flags)
		}
	}
}

// No fit prices under the old sum, on any path: every published figure at every
// percentile and rate, and the rate answered. Where the floor binds the flag says
// so, and the parts still sum to the mean.
func TestTerm_NoFitPricesUnderTheOldSum(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	bound := 0
	for range 100 {
		fl := workedFleet()
		fl.Prefill.PromptTokens = 200 + 9000*r.Float64()
		fl.Prefill.PromptCV = 3 * r.Float64()
		fl.Prefill.TransferSecondsPerToken = 3e-5 * r.Float64()
		term := DecodeTerm{Engine: PromptLine{Base: 0.06*r.Float64() - 0.05, PerToken: 3e-5 * r.Float64() * r.Float64()},
			PerStep: 8 * r.Float64() * r.Float64(), Hop: PromptLine{Base: 0.02 * r.Float64(), PerToken: 1e-6 * r.Float64()}}
		fl.DecodeTerm = &term
		old := fl
		old.DecodeTerm, old.LegacyDecodeSum = nil, true
		pl, u := fl.plan()
		if u != nil {
			t.Fatal(u)
		}
		rate := 0.9 * r.Float64() * pl.lowestKnee()
		for _, q := range []float64{0, 0.5, 0.9, 0.95, 0.99} {
			v, o := mustTTFT(t, ttftAt(t, fl, rate, q)), mustTTFT(t, ttftAt(t, old, rate, q))
			noneUnder(t, fmt.Sprintf("%+v at %g, q %g", term, rate, q), v, o)
			if !(v.Floor <= v.Seconds*(1+1e-9)) || !(v.Independent <= v.Seconds*(1+1e-9)) {
				t.Fatalf("over the headline %g: floor %g, independent %g", v.Seconds, v.Floor, v.Independent)
			}
			// The pooled figure can stand over the worst pod's at cv under 1
			// on the old sum too: the term must not make that so where it
			// was not.
			if v.Pooled > v.Seconds*(1+1e-9) && o.Pooled <= o.Seconds*(1+1e-9) {
				t.Fatalf("pooled %g over the headline %g, and the old sum's was under", v.Pooled, v.Seconds)
			}
			if v.Seconds == o.Seconds && !v.HasFlag(DecodeTermFloored) {
				t.Fatalf("%+v at %g, q %g: the old sum stands unflagged: %v", term, rate, q, v.Flags)
			}
			if v.HasFlag(DecodeTermFloored) {
				bound++
			}
			if q == 0 {
				p := v.Parts
				relNear(t, "parts", v.Seconds, p.EngineWait+p.OwnPrefill+p.DecodeWait+p.FirstStep+p.DecodeEngine+p.RouterHop, 1e-9)
			}
			target := TTFTTarget{Seconds: o.Floor * (1.05 + r.Float64()), Percentile: q}
			a, ok := rateForTTFT(t, old, target).(Answer)
			if !ok {
				continue
			}
			switch n := rateForTTFT(t, fl, target).(type) {
			case Answer:
				if n.Value > a.Value*(1+1e-6) {
					t.Fatalf("%+v at %+v: rate %g over the old sum's %g", term, target, n.Value, a.Value)
				}
			case Unanswerable:
				if n.Reason != TargetBelowFloor {
					t.Fatalf("%+v", n)
				}
			}
		}
	}
	if bound == 0 {
		t.Fatal("the floor never bound: this property tested nothing")
	}
	t.Logf("the floor bound on %d figures", bound)
}

// The reviewer's case: a fit whose plane read under the old sum gave 5.4x the
// rate, unflagged. The learner refuses that plane; priced directly, past the
// refusal, the floor holds the rate and says so. Refused or floored, it is
// flagged either way and the rate is never over the old sum's.
func TestTerm_AFitUnderTheOldSumHoldsItsRate(t *testing.T) {
	old := workedFleet()
	old.Prefill.TransferSecondsPerToken = 5e-5
	old.LegacyDecodeSum = true
	fl := old
	fl.LegacyDecodeSum = false
	fl.DecodeTerm = &DecodeTerm{Engine: PromptLine{Base: -0.03, PerToken: 1e-6}, PerStep: 7}
	target := TTFTTarget{Seconds: mustTTFT(t, ttftAt(t, old, 0.5, 0.9)).Seconds * 1.02, Percentile: 0.9}
	was, now := mustAnswer(t, rateForTTFT(t, old, target)), mustAnswer(t, rateForTTFT(t, fl, target))
	relNear(t, "rate", now.Value, was.Value, 1e-6)
	if !now.HasFlag(DecodeTermFloored) {
		t.Errorf("flags %v", now.Flags)
	}
}

// The floor can bind at the rate and not at no load: a D with no slope clears
// the old sum at the band's bottom and falls under it as ITL climbs. The rate
// answer carries the flag.
func TestTerm_TheFloorBindsAtTheRateAlone(t *testing.T) {
	old := workedFleet()
	old.LegacyDecodeSum = true
	fl := workedFleet()
	d := *fl.Decode
	fl.DecodeTerm = &DecodeTerm{Engine: PromptLine{Base: 1.01 * d.Line.ITLAt(d.Band.Lo)}}
	if mustTTFT(t, ttftAt(t, fl, 0, 0.9)).HasFlag(DecodeTermFloored) {
		t.Fatal("bound at no load: this tests nothing")
	}
	target := TTFTTarget{Seconds: mustTTFT(t, ttftAt(t, old, 8, 0.9)).Seconds, Percentile: 0.9}
	a := mustAnswer(t, rateForTTFT(t, fl, target))
	if !a.HasFlag(DecodeTermFloored) {
		t.Errorf("flags %v", a.Flags)
	}
}

// Prefix hits past the share are flagged: D goes with the whole prompt. A term
// with no D has nothing to flag, and an unread prompt says nothing.
func TestTerm_PrefixHitsAreFlagged(t *testing.T) {
	flagged := func(whole float64, omitted bool) bool {
		term := stateTerm()
		term.WholePromptTokens, term.EngineOmitted = whole, omitted
		return mustTTFT(t, ttftAt(t, termFleet(term), 2, 0.9)).HasFlag(DecodeTermPrefixHits)
	}
	i := workedPrefill().PromptTokens
	for _, c := range []struct {
		whole   float64
		omitted bool
		want    bool
	}{{0, false, false}, {i, false, false}, {i / (1 - MaterialPrefixHitShare) * 0.999, false, false}, {i * 2, false, true}, {i * 2, true, false}} {
		if got := flagged(c.whole, c.omitted); got != c.want {
			t.Errorf("whole %g, omitted %v: %v", c.whole, c.omitted, got)
		}
	}
}

// A refusal keeps the flags of the term it rests on, and says when an assumed
// part alone refused it: with that part out, the fleet answers.
func TestTerm_ARefusalKeepsWhatItRestsOn(t *testing.T) {
	old := workedFleet()
	old.LegacyDecodeSum = true
	oldFloor := mustTTFT(t, ttftAt(t, old, 1, 0.9)).Floor

	assumed := termFleet(&DecodeTerm{EngineOmitted: true, HopAssumed: true, Hop: PromptLine{Base: 0.02}})
	u, ok := rateForTTFT(t, assumed, TTFTTarget{Seconds: oldFloor + 0.01, Percentile: 0.9}).(Unanswerable)
	if !ok || u.Reason != TargetBelowFloor {
		t.Fatalf("%#v", u)
	}
	if !u.AssumedTermDecides || !slices.Contains(u.Flags, RouterHopAssumed) || !slices.Contains(u.Flags, DecodeTermOmitted) {
		t.Errorf("%+v", u)
	}

	// Under the old floor too: the fleet refuses it whatever is assumed.
	u, _ = rateForTTFT(t, assumed, TTFTTarget{Seconds: oldFloor * 0.9, Percentile: 0.9}).(Unanswerable)
	if u.AssumedTermDecides || !slices.Contains(u.Flags, RouterHopAssumed) {
		t.Errorf("%+v", u)
	}

	// A held D, slope assumed, decides too.
	held := termFleet(&DecodeTerm{Engine: PromptLine{Base: 0.3, HeldPromptTokens: 1000}, PerStep: 10, FromITL: 0.004, StepAssumed: true})
	u, _ = rateForTTFT(t, held, TTFTTarget{Seconds: oldFloor + 0.01, Percentile: 0.9}).(Unanswerable)
	if u.Reason != TargetBelowFloor || !u.AssumedTermDecides || !slices.Contains(u.Flags, DecodeStepSlopeAssumed) {
		t.Errorf("%+v", u)
	}

	// A learned term decides nothing assumed.
	learned := termFleet(stateTerm())
	u, _ = rateForTTFT(t, learned, TTFTTarget{Seconds: oldFloor + 0.001, Percentile: 0.9}).(Unanswerable)
	if u.Reason != TargetBelowFloor || u.AssumedTermDecides || slices.ContainsFunc(u.Flags, func(f Flag) bool {
		return slices.Contains(termFlagsOf, f)
	}) {
		t.Errorf("%+v", u)
	}

	// The prompt's own prefill is in the floor too: an assumed cv and V_P stay.
	spread := termFleet(stateTerm())
	spread.Prefill.PromptCVAssumed, spread.Prefill.VPAssumed = true, true
	u, _ = rateForTTFT(t, spread, TTFTTarget{Seconds: oldFloor + 0.001, Percentile: 0.9}).(Unanswerable)
	if !slices.Contains(u.Flags, AssumedPromptSpread) || !slices.Contains(u.Flags, AssumedVP) {
		t.Errorf("%+v", u)
	}
}

// termFlagsOf is what says a decode term is assumed or floored.
var termFlagsOf = []Flag{DecodeTermOmitted, DecodeStepSlopeAssumed, RouterHopAssumed, DecodeTermFloored, DecodeTermPrefixHits}

func TestWithoutAssumedTerms(t *testing.T) {
	if _, ok := WithoutAssumedTerms(workedFleet()); ok {
		t.Error("no term has nothing assumed")
	}
	if _, ok := WithoutAssumedTerms(termFleet(stateTerm())); ok {
		t.Error("a learned term has nothing assumed")
	}
	fl, ok := WithoutAssumedTerms(termFleet(&DecodeTerm{Engine: PromptLine{Base: 0.3}, PerStep: 10, StepAssumed: true,
		Hop: PromptLine{Base: 0.02}, HopAssumed: true}))
	if !ok || !fl.DecodeTerm.EngineOmitted || fl.DecodeTerm.Hop != (PromptLine{}) || fl.DecodeTerm.PerStep != 0 {
		t.Errorf("%+v", fl.DecodeTerm)
	}
}

func TestTermFromEstimate(t *testing.T) {
	held := prefill.HeldDecode{Seconds: 0.05, PromptTokens: 1000, ITLSeconds: 0.004}
	fit := prefill.DecodeFit{Base: 0.002, PerToken: 3e-6, PerStep: 5, Minutes: 40}
	hop := prefill.HopFit{Base: 0.004, PerToken: 1e-6, Windows: 9}

	got := TermFromEstimate(prefill.DecodeTermEstimate{Decode: fit, Hop: hop, Held: held, WholePromptTokens: 2000})
	want := DecodeTerm{Engine: PromptLine{Base: 0.002, PerToken: 3e-6}, PerStep: 5, Hop: PromptLine{Base: 0.004, PerToken: 1e-6},
		WholePromptTokens: 2000}
	if got != want {
		t.Errorf("learned: %+v", got)
	}

	got = TermFromEstimate(prefill.DecodeTermEstimate{Decode: prefill.DecodeFit{Refused: prefill.NoStepSpread}, Held: held,
		Hop: prefill.HopFit{Refused: prefill.NoRouterMean}})
	want = DecodeTerm{Engine: PromptLine{Base: 0.05, HeldPromptTokens: 1000}, PerStep: AssumedDecodeStepSlope, FromITL: 0.004,
		StepAssumed: true, Hop: PromptLine{Base: AssumedRouterHopSeconds}, HopAssumed: true,
		EngineRefused: prefill.NoStepSpread, HopRefused: prefill.NoRouterMean}
	if got != want {
		t.Errorf("refused, held: %+v", got)
	}

	got = TermFromEstimate(prefill.DecodeTermEstimate{Decode: prefill.DecodeFit{Refused: prefill.NonPositiveDecodePlane}})
	want = DecodeTerm{EngineOmitted: true, Hop: PromptLine{Base: AssumedRouterHopSeconds}, HopAssumed: true,
		EngineRefused: prefill.NonPositiveDecodePlane, HopRefused: prefill.NoRouterMean}
	if got != want {
		t.Errorf("no quiet row: %+v", got)
	}
	got = TermFromEstimate(prefill.DecodeTermEstimate{Hop: prefill.HopFit{Windows: 9, Refused: prefill.NonPositiveHop}})
	if !got.HopAssumed || got.HopRefused != prefill.NonPositiveHop || got.EngineRefused != prefill.TooFewDecodeMinutes {
		t.Errorf("the reasons travel: %+v", got)
	}
}

func TestTerm_RefusesWhatNoReadingGives(t *testing.T) {
	for _, term := range []DecodeTerm{
		{Engine: PromptLine{Base: math.NaN()}},
		{Engine: PromptLine{PerToken: math.Inf(1)}},
		{Hop: PromptLine{Base: math.NaN()}},
		{PerStep: math.Inf(1)},
		{PerStep: -1},
		{FromITL: -1},
		{Engine: PromptLine{PerToken: -1}},
		{Hop: PromptLine{PerToken: -1}},
		{Engine: PromptLine{HeldPromptTokens: -1}},
		{Hop: PromptLine{HeldPromptTokens: -1}},
	} {
		u, ok := TTFTAt(termFleet(&term), 1, 0.9).(Unanswerable)
		if !ok || u.Reason != InvalidInput {
			t.Errorf("%+v: %#v", term, TTFTAt(termFleet(&term), 1, 0.9))
		}
	}
}
