package whatif

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// The TTFT model's property cases (docs/proposals/slo-capacity-planner.md,
// "Validation"), on seeded random fleets. Each was run against a mutant that
// reintroduces the defect it guards.

const ttftDraws = 40

// randomTTFTFleet draws a P/D fleet: the decode side from randomDecode, a
// prefill side of 1 to 6 pods with a prompt spread either side of 1.
func randomTTFTFleet(r *rand.Rand) TTFTFleet {
	d := randomDecode(r)
	np := 1 + r.IntN(6)
	p := Prefill{
		VP: 5000 + 60000*r.Float64(), FixedLatency: 0.002 + 0.03*r.Float64(), Replicas: np,
		PromptTokens: 200 + 6000*r.Float64(), PromptCV: 2 * r.Float64(), ArrivalCV2: 0.5 + 2*r.Float64(),
		Imbalance: 1 + (float64(np)-1)*0.3*r.Float64(),
	}
	if r.IntN(3) == 0 {
		p.TransferSecondsPerToken = 1e-6 * r.Float64()
	}
	return TTFTFleet{Prefill: p, Decode: &d}
}

// ttftSeconds is the TTFT at rate and q, +Inf where it is refused as
// saturated.
func ttftSeconds(t *testing.T, fl TTFTFleet, rate, q float64) float64 {
	t.Helper()
	switch v := ttftAt(t, fl, rate, q).(type) {
	case TTFT:
		return v.Seconds
	case Unanswerable:
		if v.Reason == Saturated {
			return math.Inf(1)
		}
		t.Fatalf("%+v at %g: %v", fl.Prefill, rate, v)
	}
	return 0
}

// TTFT never falls as the rate rises, and never rises as either role gains
// a replica; at or above the lower knee it is refused, never a number.
func TestProperty_TTFTMonotone(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for range ttftDraws {
		fl := randomTTFTFleet(r)
		pl, u := fl.plan()
		if u != nil {
			t.Fatalf("draw refused: %v", u)
		}
		knee := pl.lowestKnee()
		q := []float64{0, 0.5, 0.9, 0.99}[r.IntN(4)]
		prev := 0.0
		for _, frac := range []float64{0, 0.2, 0.5, 0.7, 0.85, 0.95, 0.99} {
			got := ttftSeconds(t, fl, frac*knee, q)
			if !(got >= prev*(1-1e-9)) || math.IsInf(got, 1) {
				t.Errorf("%+v q %g: TTFT %g at %.2f of the knee, after %g", fl.Prefill, q, got, frac, prev)
			}
			prev = got
		}
		if v, ok := ttftAt(t, fl, knee, q).(Unanswerable); !ok || v.Reason != Saturated {
			t.Errorf("at the knee %g: %#v", knee, ttftAt(t, fl, knee, q))
		}

		rate := 0.6 * knee
		base := ttftSeconds(t, fl, rate, q)
		morePrefill := fl
		morePrefill.Prefill.Replicas++
		if got := ttftSeconds(t, morePrefill, rate, q); !(got <= base*(1+1e-9)) {
			t.Errorf("%+v: a prefill pod more raised TTFT %g -> %g", fl.Prefill, base, got)
		}
		moreDecode := fl
		d := *fl.Decode
		d.Replicas++
		moreDecode.Decode = &d
		if got := ttftSeconds(t, moreDecode, rate, q); !(got <= base*(1+1e-9)) {
			t.Errorf("%+v: a decode replica more raised TTFT %g -> %g", fl.Prefill, base, got)
		}
		longer := fl
		longer.Prefill.PromptTokens *= 1.2
		if got := ttftSeconds(t, longer, rate, q); !(got >= base*(1-1e-9)) {
			t.Errorf("%+v: longer prompts lowered TTFT %g -> %g", fl.Prefill, base, got)
		}
	}
}

// Question 1's rate never falls as the target loosens or a prefill pod is
// added, never rises as prompts lengthen, stays below the lower knee, and
// round-trips: the TTFT at the rate it returns is the target.
func TestProperty_RateForTTFT(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 5))
	answered := 0
	for range ttftDraws {
		fl := randomTTFTFleet(r)
		q := []float64{0, 0.9, 0.95}[r.IntN(3)]
		floor := mustTTFT(t, ttftAt(t, fl, 0, q)).Floor
		target := TTFTTarget{Seconds: floor * (1.2 + 3*r.Float64()), Percentile: q}
		res := rateForTTFT(t, fl, target)
		a, ok := res.(Answer)
		if !ok {
			t.Fatalf("%+v: %#v", fl.Prefill, res)
		}
		answered++
		pl, _ := fl.plan()
		if !(a.Value < pl.lowestKnee()) || !(a.Value > 0) {
			t.Errorf("rate %g, knee %g", a.Value, pl.lowestKnee())
		}
		if !(a.Basis.TTFT.PrefillOnlyRate >= a.Value*(1-1e-9)) {
			t.Errorf("prefill side alone %g below the answer %g", a.Basis.TTFT.PrefillOnlyRate, a.Value)
		}
		back := mustTTFT(t, ttftAt(t, fl, a.Value, q))
		relNear(t, "TTFT at the answer", back.Seconds, target.Seconds, 1e-6)

		looser := target
		looser.Seconds *= 1.3
		if got := mustAnswer(t, rateForTTFT(t, fl, looser)).Value; !(got >= a.Value) {
			t.Errorf("a looser target lowered the rate %g -> %g", a.Value, got)
		}
		more := fl
		more.Prefill.Replicas++
		if got := mustAnswer(t, rateForTTFT(t, more, target)).Value; !(got >= a.Value*(1-1e-9)) {
			t.Errorf("a prefill pod more lowered the rate %g -> %g", a.Value, got)
		}
		longer := fl
		longer.Prefill.PromptTokens *= 1.2
		switch v := rateForTTFT(t, longer, target).(type) {
		case Answer:
			if !(v.Value <= a.Value*(1+1e-9)) {
				t.Errorf("longer prompts raised the rate %g -> %g", a.Value, v.Value)
			}
		case Unanswerable:
			if v.Reason != TargetBelowFloor {
				t.Errorf("longer prompts: %v", v)
			}
		}
	}
	if answered < ttftDraws {
		t.Fatalf("answered %d of %d", answered, ttftDraws)
	}
}

// The union bound never sits below the independent figure, the pooled figure
// never above the worst pod's, and the mean is the sum of the parts' means.
func TestProperty_TTFTFigures(t *testing.T) {
	r := rand.New(rand.NewPCG(13, 17))
	for range ttftDraws {
		fl := randomTTFTFleet(r)
		pl, _ := fl.plan()
		rate := (0.3 + 0.65*r.Float64()) * pl.lowestKnee()
		for _, q := range []float64{0.9, 0.99} {
			v := mustTTFT(t, ttftAt(t, fl, rate, q))
			if !(v.Seconds >= v.Independent*(1-1e-9)) || !(v.Pooled <= v.Seconds*(1+1e-9)) || !(v.Floor <= v.Seconds*(1+1e-9)) {
				t.Errorf("%+v q %g: union %g independent %g pooled %g floor %g", fl.Prefill, q, v.Seconds, v.Independent, v.Pooled, v.Floor)
			}
			if !v.HasFlag(UnvalidatedTTFT) {
				t.Errorf("not flagged UNVALIDATED: %v", v.Flags)
			}
		}
		m := mustTTFT(t, ttftAt(t, fl, rate, 0))
		p := m.Parts
		relNear(t, "mean", m.Seconds, p.EngineWait+p.OwnPrefill+p.DecodeWait+p.FirstStep+p.DecodeEngine+p.RouterHop, 1e-12)
	}
}

// NaN or infinity in any input is refused, never priced.
func TestProperty_TTFTNonFiniteIsRefused(t *testing.T) {
	sets := map[string]func(*TTFTFleet, *float64, *TTFTTarget, float64){
		"V_P":      func(fl *TTFTFleet, _ *float64, _ *TTFTTarget, v float64) { fl.Prefill.VP = v },
		"a":        func(fl *TTFTFleet, _ *float64, _ *TTFTTarget, v float64) { fl.Prefill.FixedLatency = v },
		"f":        func(fl *TTFTFleet, _ *float64, _ *TTFTTarget, v float64) { fl.Prefill.Imbalance = v },
		"prompt":   func(fl *TTFTFleet, _ *float64, _ *TTFTTarget, v float64) { fl.Prefill.PromptTokens = v },
		"cv":       func(fl *TTFTFleet, _ *float64, _ *TTFTTarget, v float64) { fl.Prefill.PromptCV = v },
		"Ca^2":     func(fl *TTFTFleet, _ *float64, _ *TTFTTarget, v float64) { fl.Prefill.ArrivalCV2 = v },
		"transfer": func(fl *TTFTFleet, _ *float64, _ *TTFTTarget, v float64) { fl.Prefill.TransferSecondsPerToken = v },
		"decode A": func(fl *TTFTFleet, _ *float64, _ *TTFTTarget, v float64) { fl.Decode.Line.A = v },
		"queue threshold": func(fl *TTFTFleet, _ *float64, _ *TTFTTarget, v float64) {
			fl.FlowControl = FlowControl{State: FlowControlOn, QueueThreshold: v, KVThreshold: 0.8}
		},
		"rate":       func(_ *TTFTFleet, rate *float64, _ *TTFTTarget, v float64) { *rate = v },
		"percentile": func(_ *TTFTFleet, _ *float64, tg *TTFTTarget, v float64) { tg.Percentile = v },
		"target":     func(_ *TTFTFleet, _ *float64, tg *TTFTTarget, v float64) { tg.Seconds = v },
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		for name, set := range sets {
			fl := workedFleet()
			d := *fl.Decode
			fl.Decode = &d
			rate, tg := 5.0, TTFTTarget{Seconds: 0.2, Percentile: 0.9}
			set(&fl, &rate, &tg, v)
			results := map[string]Result{"RateForTTFT": rateForTTFT(t, fl, tg)}
			if name != "target" {
				results["TTFTAt"] = ttftAt(t, fl, rate, tg.Percentile)
			}
			for fn, res := range results {
				if name == "rate" && fn == "RateForTTFT" {
					continue
				}
				if _, ok := res.(Unanswerable); !ok {
					t.Errorf("%s = %g: %s gave %#v", name, v, fn, res)
				}
			}
		}
	}
}

// TTFT never falls as the prompt spread rises, over cv 0 to 3 in steps of
// 0.1 (across the boundary at 1, where the tail changes form), at p90, p95
// and p99 and at 0.3 to 0.85 of the prefill knee. Above cv 3 the gamma's
// p90 can fall at very low load (most of its mass sits below the mean);
// S1 passes no cv above 3, assumed or measured, and this property stops there.
func TestProperty_TTFTMonotoneInPromptSpread(t *testing.T) {
	for _, q := range []float64{0.9, 0.95, 0.99} {
		for _, frac := range []float64{0.3, 0.5, 0.7, 0.85} {
			prev := 0.0
			for i := 0; i <= 30; i++ {
				cv := float64(i) / 10
				fl := prefillOnly(cv)
				rate := frac * mustAnswer(t, PrefillKnee(fl.Prefill)).Value
				got := mustTTFT(t, ttftAt(t, fl, rate, q)).Seconds
				if !(got >= prev*(1-1e-9)) {
					t.Errorf("q %g at %.2f of the knee: TTFT %g at cv %.1f, after %g", q, frac, got, cv, prev)
				}
				prev = got
			}
		}
	}
}

// The union bound's split: with no decode wait the decode share is 0 and the
// answer is block 1's own quantile; with one, the share never exceeds the
// decode wait probability or 1 - q.
func TestUnionSplitShare(t *testing.T) {
	b1 := block{wait: expWait(0.5, 20), own: newOwnTime(0.03, 1)}
	v, beta := split(b1, waitTail{}, 0.9)
	if beta != 0 || v != b1.quantile(0.1) {
		t.Errorf("no decode wait: %g at beta %g, want %g at 0", v, beta, b1.quantile(0.1))
	}
	for _, pd := range []float64{1e-14, 0.01, 0.05, 0.3, 0.9} {
		for _, q := range []float64{0.5, 0.9, 0.99} {
			v, beta := split(b1, expWait(pd, 5), q)
			if !(beta > 0) || beta > min(pd, 1-q) {
				t.Errorf("pd %g q %g: beta %g", pd, q, beta)
			}
			// Brute force over a fine grid never beats the search by more
			// than the grid's own error.
			best := math.Inf(1)
			for i := 1; i < 4000; i++ {
				b := min(pd, 1-q) * float64(i) / 4000
				d := 0.0
				if b < pd {
					d = math.Log(pd/b) / 5
				}
				best = min(best, b1.quantile(1-q-b)+d)
			}
			if !(v <= best*(1+1e-6)) {
				t.Errorf("pd %g q %g: search %g, grid %g", pd, q, v, best)
			}
		}
	}
	if !slices.Contains(ttftReasons(), AboveDecodeOnset) {
		t.Error("AboveDecodeOnset is not listed")
	}
}
