package whatif

import (
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The property cases of docs/proposals/slo-capacity-planner.md ("Validation"),
// under the learner rules' property-test discipline
// (docs/proposals/analyzer-learner.md, 2.6): each runs on seeded random inputs
// as well as on the worked example, and each was run against a mutant that
// reintroduces the defect it guards.

// rateFor is question 1 with the target and headroom as arguments.
func rateFor(d Decode, target TPOTTarget, headroom float64) Result {
	return RateForTPOT(d, RateQuery{Target: target, Headroom: headroom})
}

// ratioFrom is TPOTRatioFromWindows as a Result: the ratio as an Answer
// carrying its flags, or the refusal.
func ratioFrom(windows []TPOTWindow, q float64) Result {
	r, flags, u := TPOTRatioFromWindows(windows, q)
	if u != nil {
		if r != (TPOTRatio{}) || flags != nil {
			panic("a refused ratio came back with a value")
		}
		return *u
	}
	return Answer{Value: r.Value, Flags: flags}
}

// randomDecode draws a decode role from the ranges real fleets span: a line
// from 2 to 60 ms across the band, 4k to 2M KV tokens, prompts and outputs
// from 50 to 20k tokens, batch caps from 1 to 1024 on up to 8 ranks.
func randomDecode(r *rand.Rand) Decode {
	lo := 0.05 + 0.2*r.Float64()
	hi := lo + 0.1 + (0.95-lo-0.1)*r.Float64()
	il, ol := 50+20000*r.Float64(), 50+20000*r.Float64()
	return Decode{
		Line:           itl.Model{A: 0.001 + 0.05*r.Float64(), B: 0.0005 + 0.01*r.Float64()},
		Band:           FillBand{Lo: lo, Hi: hi},
		CapacityTokens: (il + ol/2) / hi * math.Exp(math.Log(2000)*r.Float64()) * 1.01,
		KVPerRequest:   il + ol/2,
		OutputTokens:   ol,
		ReplicaCap:     int64(1+r.IntN(1024)) * int64(1+r.IntN(8)),
		Replicas:       1 + r.IntN(16),
	}
}

// pricedRate is a rate answer's value, and 0 for a refusal: a target too
// tight to meet carries nothing, which keeps monotonicity defined across the
// floor.
func pricedRate(t *testing.T, r Result) float64 {
	t.Helper()
	switch v := r.(type) {
	case Answer:
		return v.Value
	case Unanswerable:
		// A target under the floor carries nothing, and so does a request
		// longer than the cache holds.
		if v.Reason != TargetBelowFloor && v.Reason != NoCapacity {
			t.Fatalf("unexpected refusal %v", v)
		}
		return 0
	}
	t.Fatalf("unexpected result %#v", r)
	return 0
}

const propertyDraws = 2000

// minCappedDraws is the fewest of propertyDraws that must reach the operating-point
// cap for TestProperty_RateUnderOperatingPoint to be exercising it.
const minCappedDraws = propertyDraws * 3 / 4

// Round trip, at unclamped points only (fill inside the band, n under the
// cap, rate under the operating-point cap, which a headroom of 1 lifts):
// question 1 at ITL(k)*R_q gives back the rate whose fill is k, and question
// 2 at that rate gives back the target. For the mean and for a percentile.
func TestProperty_RoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	checked := 0
	for range propertyDraws {
		d := randomDecode(r)
		full := d.full().fill
		if !(full > d.Band.Lo) {
			continue
		}
		k := d.Band.Lo + (full-d.Band.Lo)*(0.01+0.98*r.Float64())
		for _, tg := range []TPOTTarget{
			{},
			{Percentile: 0.9, Ratio: TPOTRatio{Value: 1 + 2*r.Float64()}},
		} {
			ratio := 1.0
			if tg.Percentile > 0 {
				ratio = tg.Ratio.Value
			}
			tg.Seconds = d.Line.ITLAt(k) * ratio
			a := mustAnswer(t, rateFor(d, tg, 1))
			if !slices.Equal(a.Basis.BoundBy, []Bound{BoundTPOT}) {
				t.Fatalf("%+v at k=%g: bound by %v, want TPOT at an unclamped point", d, k, a.Basis.BoundBy)
			}
			want := float64(d.Replicas) * d.replicaRate(k)
			if math.Abs(a.Value-want) > 1e-6*want {
				t.Fatalf("%+v: rate %.12g at k=%g, want %.12g", d, a.Value, k, want)
			}
			fill := mustAnswer(t, FillAtRate(d, a.Value)).Value
			if math.Abs(fill-k) > 1e-6*k {
				t.Fatalf("%+v: fill back %.12g, want %.12g", d, fill, k)
			}
			q := ReplicasQuery{Rate: a.Value}
			if tg.Percentile > 0 {
				q.PercentileTarget = &tg
			}
			slo := SLOAtReplicas(d, q)
			back := mustAnswer(t, slo.MeanTPOT).Value
			if tg.Percentile > 0 {
				back = mustAnswer(t, slo.PercentileTPOT).Value
			}
			if math.Abs(back-tg.Seconds) > 1e-6*tg.Seconds {
				t.Fatalf("%+v: TPOT back %.12g, want %.12g", d, back, tg.Seconds)
			}
			checked++
		}
	}
	if checked < propertyDraws {
		t.Fatalf("only %d round trips checked; the draw is not exercising the property", checked)
	}
}

// The answered rate never exceeds the operating point, headroom times the
// knee, and never exceeds what the target alone allows.
func TestProperty_RateUnderOperatingPoint(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	capped := 0
	for range propertyDraws {
		d := randomDecode(r)
		h := 0.5 + 0.5*r.Float64()
		tg := TPOTTarget{Seconds: d.Line.ITLAt(d.Band.Lo) * (1 + 3*r.Float64())}
		a, ok := rateFor(d, tg, h).(Answer)
		if !ok {
			continue
		}
		if a.Value > h*a.Basis.KneeRate*(1+1e-12) || a.Value > a.Basis.Uncapped*(1+1e-12) {
			t.Fatalf("%+v at headroom %g: rate %g above the cap %g or the target's %g", d, h, a.Value, h*a.Basis.KneeRate, a.Basis.Uncapped)
		}
		if slices.Contains(a.Basis.BoundBy, BoundOperatingPoint) {
			capped++
		}
	}
	t.Logf("%d of %d draws capped at the operating point", capped, propertyDraws)
	if capped < minCappedDraws {
		t.Fatalf("only %d draws reached the operating-point cap, want at least %d", capped, minCappedDraws)
	}
}

// Monotonicity: the rate never falls when the target loosens, the ratio
// falls, or replicas are added; never rises when prompts or answers lengthen.
// Question 2's TPOT never falls as the rate rises, and the admission wait
// never falls either.
func TestProperty_Monotonicity(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	priced, waited := 0, 0
	for range propertyDraws {
		d := randomDecode(r)
		h := 0.5 + 0.5*r.Float64()
		floor := d.Line.ITLAt(d.Band.Lo)
		tg := TPOTTarget{Seconds: floor * (0.5 + 3*r.Float64())}
		base := pricedRate(t, rateFor(d, tg, h))
		if base > 0 {
			priced++
		}

		looser := tg
		looser.Seconds *= 1 + r.Float64()
		if got := pricedRate(t, rateFor(d, looser, h)); got < base*(1-1e-12) {
			t.Fatalf("%+v: target %g -> %g lowered the rate %g -> %g", d, tg.Seconds, looser.Seconds, base, got)
		}

		pct := TPOTTarget{Seconds: tg.Seconds, Percentile: 0.9, Ratio: TPOTRatio{Value: 1 + r.Float64()}}
		higher := pct
		higher.Ratio.Value *= 1 + r.Float64()
		pBase := pricedRate(t, rateFor(d, pct, h))
		if got := pricedRate(t, rateFor(d, higher, h)); got > pBase*(1+1e-12) {
			t.Fatalf("%+v: R_q %g -> %g raised the rate %g -> %g", d, pct.Ratio.Value, higher.Ratio.Value, pBase, got)
		}
		if pBase > base*(1+1e-12) {
			t.Fatalf("%+v: a percentile target priced above the mean at the same seconds: %g > %g", d, pBase, base)
		}

		more := d
		more.Replicas += 1 + r.IntN(8)
		if got := pricedRate(t, rateFor(more, tg, h)); got < base*(1-1e-12) {
			t.Fatalf("%+v: %d -> %d replicas lowered the rate %g -> %g", d, d.Replicas, more.Replicas, base, got)
		}

		longOut := d
		extra := 1 + 5000*r.Float64()
		longOut.OutputTokens += extra
		longOut.KVPerRequest += extra / 2
		if got := pricedRate(t, rateFor(longOut, tg, h)); got > base*(1+1e-12) {
			t.Fatalf("%+v: output +%g raised the rate %g -> %g", d, extra, base, got)
		}
		longIn := d
		longIn.KVPerRequest += extra
		if got := pricedRate(t, rateFor(longIn, tg, h)); got > base*(1+1e-12) {
			t.Fatalf("%+v: prompt +%g raised the rate %g -> %g", d, extra, base, got)
		}
		for _, longer := range []Decode{longOut, longIn} {
			if k1, k2 := pricedRate(t, KneeRate(d)), pricedRate(t, KneeRate(longer)); k2 > k1*(1+1e-12) {
				t.Fatalf("%+v: a longer request raised the knee %g -> %g", d, k1, k2)
			}
		}

		knee := mustAnswer(t, KneeRate(d)).Value
		x1 := knee * 0.98 * r.Float64()
		x2 := x1 + (knee*0.98-x1)*r.Float64()
		s1 := mustAnswer(t, SLOAtReplicas(d, ReplicasQuery{Rate: x1}).MeanTPOT).Value
		s2 := mustAnswer(t, SLOAtReplicas(d, ReplicasQuery{Rate: x2}).MeanTPOT).Value
		if s2 < s1*(1-1e-12) {
			t.Fatalf("%+v: rate %g -> %g lowered the TPOT %g -> %g", d, x1, x2, s1, s2)
		}
		// A chain whose slots the KV sets can saturate below the closed-form
		// knee, which prices fractional sequences: Saturated is the longest
		// wait there is, so it may follow an answer but never precede one.
		p1, w1 := admissionWait(t, d, x1)
		p2, w2 := admissionWait(t, d, x2)
		if p2 < p1*(1-1e-9) || w2 < w1*(1-1e-9) {
			t.Fatalf("%+v: rate %g -> %g lowered the wait %g/%g -> %g/%g", d, x1, x2, p1, w1, p2, w2)
		}
		if p1 > 1e-6 && !math.IsInf(p2, 1) {
			waited++
		}
	}
	t.Logf("%d of %d draws priced a rate; %d compared two finite, non-negligible waits", priced, propertyDraws, waited)
	if priced < propertyDraws/2 || waited < propertyDraws/10 {
		t.Fatalf("priced %d, waited %d of %d draws: the draw is not exercising the property", priced, waited, propertyDraws)
	}
}

// admissionWait is the wait probability and mean wait at rate, both +Inf when
// the chain is saturated.
func admissionWait(t *testing.T, d Decode, rate float64) (float64, float64) {
	t.Helper()
	switch v := DecodeAdmission(d, rate).(type) {
	case Admission:
		return v.WaitProbability, v.MeanWait()
	case Unanswerable:
		if v.Reason == Saturated {
			return math.Inf(1), math.Inf(1)
		}
	}
	t.Fatalf("%+v: no admission at %g", d, rate)
	return 0, 0
}

// nonFiniteInputs are the inputs every question reads, one of which a case of
// TestProperty_NonFiniteIsRefused sets to NaN or infinity.
type nonFiniteInputs struct {
	d           Decode
	target, pct TPOTTarget
	// h1 is question 1's headroom, h2 question 2's (asked without a rate),
	// and rate the rate FillAtRate, DecodeAdmission and question 2 (asked
	// without a headroom) price at.
	h1, h2, rate float64
}

// results asks every question of in and names each result.
func (in nonFiniteInputs) results() map[string]Result {
	byRate := SLOAtReplicas(in.d, ReplicasQuery{Rate: in.rate, PercentileTarget: &in.pct})
	byHeadroom := SLOAtReplicas(in.d, ReplicasQuery{Headroom: in.h2, PercentileTarget: &in.pct})
	return map[string]Result{
		"RateForTPOT":     RateForTPOT(in.d, RateQuery{Target: in.target, Headroom: in.h1}),
		"FillAtRate":      FillAtRate(in.d, in.rate),
		"DecodeAdmission": DecodeAdmission(in.d, in.rate),
		"KneeRate":        KneeRate(in.d),
		"Q2(rate).Knee":   byRate.KneeRate,
		"Q2(rate).Rate":   byRate.Rate,
		"Q2(rate).Mean":   byRate.MeanTPOT,
		"Q2(rate).Pct":    byRate.PercentileTPOT,
		"Q2(h).Knee":      byHeadroom.KneeRate,
		"Q2(h).Rate":      byHeadroom.Rate,
		"Q2(h).Mean":      byHeadroom.MeanTPOT,
		"Q2(h).Pct":       byHeadroom.PercentileTPOT,
	}
}

// NaN or infinity anywhere gives a reason, never 0, +Inf or NaN as a number:
// each input in turn is set to each non-finite value, and every result that
// reads it must refuse while every other must still answer.
func TestProperty_NonFiniteIsRefused(t *testing.T) {
	everything := []string{"RateForTPOT", "FillAtRate", "DecodeAdmission", "KneeRate",
		"Q2(rate).Knee", "Q2(rate).Rate", "Q2(rate).Mean", "Q2(rate).Pct",
		"Q2(h).Knee", "Q2(h).Rate", "Q2(h).Mean", "Q2(h).Pct"}
	cases := []struct {
		input string
		set   func(*nonFiniteInputs, float64)
		reads []string
	}{
		{"A", func(in *nonFiniteInputs, v float64) { in.d.Line.A = v }, everything},
		{"B", func(in *nonFiniteInputs, v float64) { in.d.Line.B = v }, everything},
		{"band lo", func(in *nonFiniteInputs, v float64) { in.d.Band.Lo = v }, everything},
		{"band hi", func(in *nonFiniteInputs, v float64) { in.d.Band.Hi = v }, everything},
		{"C", func(in *nonFiniteInputs, v float64) { in.d.CapacityTokens = v }, everything},
		{"KVreq", func(in *nonFiniteInputs, v float64) { in.d.KVPerRequest = v }, everything},
		{"OL", func(in *nonFiniteInputs, v float64) { in.d.OutputTokens = v }, everything},
		{"target", func(in *nonFiniteInputs, v float64) { in.target.Seconds = v }, []string{"RateForTPOT"}},
		{"q", func(in *nonFiniteInputs, v float64) { in.target.Percentile, in.pct.Percentile = v, v },
			[]string{"RateForTPOT", "Q2(rate).Pct", "Q2(h).Pct"}},
		{"R_q", func(in *nonFiniteInputs, v float64) { in.target.Ratio.Value, in.pct.Ratio.Value = v, v },
			[]string{"RateForTPOT", "Q2(rate).Pct", "Q2(h).Pct"}},
		{"question 1 headroom", func(in *nonFiniteInputs, v float64) { in.h1 = v }, []string{"RateForTPOT"}},
		{"question 2 headroom", func(in *nonFiniteInputs, v float64) { in.h2 = v },
			[]string{"Q2(h).Rate", "Q2(h).Mean", "Q2(h).Pct"}},
		{"rate", func(in *nonFiniteInputs, v float64) { in.rate = v },
			[]string{"FillAtRate", "DecodeAdmission", "Q2(rate).Rate", "Q2(rate).Mean", "Q2(rate).Pct"}},
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, c := range cases {
			in := nonFiniteInputs{
				d:      olsSweep(),
				target: TPOTTarget{Seconds: 0.004, Percentile: 0.9, Ratio: TPOTRatio{Value: 1.3}},
				pct:    TPOTTarget{Percentile: 0.9, Ratio: TPOTRatio{Value: 1.3}},
				h1:     0.85, h2: 0.85, rate: 5,
			}
			c.set(&in, v)
			for fn, res := range in.results() {
				reads := slices.Contains(c.reads, fn)
				switch got := res.(type) {
				case Unanswerable:
					if !reads {
						t.Errorf("%s = %g: %s refused (%v) on an input it does not read", c.input, v, fn, got)
					}
				case Answer:
					if reads {
						t.Errorf("%s = %g: %s answered %g instead of refusing", c.input, v, fn, got.Value)
					} else if !Finite(got.Value) {
						t.Errorf("%s = %g: %s answered a non-finite %g", c.input, v, fn, got.Value)
					}
				case Admission:
					if reads {
						t.Errorf("%s = %g: %s answered %+v instead of refusing", c.input, v, fn, got)
					}
				default:
					t.Errorf("%s = %g: %s returned %#v", c.input, v, fn, res)
				}
			}
		}
		// The fix D2 makes in PriceTPOT: a NaN band edge priced OK with a NaN
		// fill.
		for _, b := range []FillBand{{Lo: v, Hi: 0.8}, {Lo: 0.15, Hi: v}} {
			if p := PriceTPOT(b, olsSweep().Line, 0.004); p.OK {
				t.Errorf("PriceTPOT with band %+v: OK with fill %g, want OK false", b, p.Fill)
			}
		}
	}
}

// Every Unanswerable reason has a case that produces it.
func TestProperty_EveryReasonHasACase(t *testing.T) {
	d := olsSweep()
	with := func(f func(*Decode)) Decode { c := d; f(&c); return c }
	knee := mustAnswer(t, KneeRate(d)).Value
	cases := map[Reason]Result{
		NoLine:             KneeRate(with(func(c *Decode) { c.Line = itl.Model{} })),
		PinnedDefaultB:     KneeRate(with(func(c *Decode) { c.Tier, c.Line = TierPinnedB, itl.Model{A: 0.004, B: itl.DefaultBaselineSec} })),
		NoCapacity:         KneeRate(with(func(c *Decode) { c.CapacityTokens = 0 })),
		NoBatchCap:         KneeRate(with(func(c *Decode) { c.ReplicaCap = 0 })),
		NoTraffic:          KneeRate(with(func(c *Decode) { c.OutputTokens = 0 })),
		TokenGapPercentile: rateFor(d, TPOTTarget{Seconds: 0.004, Percentile: 0.9, OverTokenGaps: true}, 0.85),
		NoTPOTHistogram:    rateFor(d, TPOTTarget{Seconds: 0.004, Percentile: 0.9}, 0.85),
		TPOTQuantileUnbounded: ratioFrom([]TPOTWindow{{
			Buckets: []HistogramBucket{{UpperEdge: 0.005, Count: 50}, {UpperEdge: math.Inf(1), Count: 100}},
			Sum:     1, Count: 100,
		}}, 0.9),
		TPOTBucketTooWide: engineRatioFrom([]TPOTWindow{{
			Buckets: []HistogramBucket{{UpperEdge: 0.05, Count: 100}}, Sum: 1, Count: 100,
		}}, 0.9),
		TargetBelowFloor: rateFor(d, TPOTTarget{Seconds: 0.002}, 0.85),
		Saturated:        FillAtRate(d, knee),
		NoDecodeReplicas: KneeRate(with(func(c *Decode) { c.Replicas = 0 })),
		MalformedHistogram: ratioFrom([]TPOTWindow{{
			Buckets: []HistogramBucket{{UpperEdge: 0.005, Count: 50}, {UpperEdge: 0.002, Count: 100}},
			Sum:     1, Count: 100,
		}}, 0.9),
		TooManySlots: DecodeAdmission(with(func(c *Decode) { c.ReplicaCap, c.CapacityTokens = 1e10, 1e16 }), 1),
		InvalidInput: rateFor(d, TPOTTarget{Seconds: 0.004}, 1.5),
	}
	maps.Copy(cases, ttftReasonCases(t))
	for _, reason := range Reasons() {
		res, ok := cases[reason]
		if !ok {
			t.Errorf("reason %s has no case", reason)
			continue
		}
		u, ok := res.(Unanswerable)
		if !ok || u.Reason != reason {
			t.Errorf("the case for %s gave %#v", reason, res)
		}
	}
	if len(cases) != len(Reasons()) {
		t.Errorf("%d cases for %d reasons", len(cases), len(Reasons()))
	}
}

// The chain against a reference that shares none of its arithmetic: a
// discrete-event simulation of the same replica (state-dependent completion
// rates, Poisson arrivals, first come first served), measuring the share of
// arrivals that wait, the mean wait and the tail of the wait.
//
// What pins the chain's arithmetic is Erlang C (exact, 1e-9 relative) and the
// worked example (the page's figures); this case checks the model, that the
// chain describes the queue it claims to, and is loose by design. Near the
// knee a busy period is long, so 2M arrivals leave the simulated means
// several percent from the truth: across seeds, up to about 8% on the mean
// wait. The 10% tolerance sits just above that noise, so it catches a wrong
// model (a constant ITL is off by orders of magnitude) and not a few percent.
func TestProperty_AdmissionMatchesSimulation(t *testing.T) {
	cases := []struct {
		name string
		d    Decode
		frac float64
	}{
		{"worked example at 0.90 of the knee", olsSweep(), 0.90},
		{"worked example at 0.95 of the knee", olsSweep(), 0.95},
		{"small cap, steep line", Decode{Line: itl.Model{A: 0.04, B: 0.01}, Band: DefaultBand(),
			CapacityTokens: 20000, KVPerRequest: 1000, OutputTokens: 200, ReplicaCap: 8, Replicas: 2}, 0.9},
		{"KV-bound slots", Decode{Line: itl.Model{A: 0.02, B: 0.005}, Band: DefaultBand(),
			CapacityTokens: 12000, KVPerRequest: 1500, OutputTokens: 500, ReplicaCap: 256, Replicas: 1}, 0.85},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The slots, worked out here rather than read off the chain: the
			// replica cap, or the whole requests the band's top holds. A
			// rounding up would admit a request the band cannot hold.
			c := int(min(float64(tc.d.ReplicaCap), math.Floor(tc.d.Band.Hi*tc.d.CapacityTokens/tc.d.KVPerRequest)))
			// The completion rates are the chain's own (chainMu): what the
			// simulation checks is the chain's queueing, not its ITL reading.
			mu := func(m int) float64 { return tc.d.chainMu(float64(m)) }
			rate := tc.frac * mu(c) * float64(tc.d.Replicas)
			ch := DecodeAdmission(tc.d, rate).(Admission)
			if ch.Slots != c {
				t.Fatalf("chain slots %d, want %d", ch.Slots, c)
			}
			sim := simulate(rand.New(rand.NewPCG(uint64(i)+7, 11)), rate/float64(tc.d.Replicas), mu, c, 2_000_000)
			t.Logf("c=%d: wait probability chain %.4f sim %.4f; mean wait chain %.4g s sim %.4g s",
				c, ch.WaitProbability, sim.waitShare, ch.MeanWait(), sim.meanWait)
			near(t, "wait probability", sim.waitShare, ch.WaitProbability, 0.10*ch.WaitProbability+0.002)
			near(t, "mean wait", sim.meanWait, ch.MeanWait(), 0.10*ch.MeanWait()+1e-4)
			// The tail, P(W > t), at one and two of the chain's decay times.
			// (A quantile is the wrong statistic to compare: near q it moves
			// by ln of the wait probability, so sampling noise in that
			// probability is amplified several-fold.)
			for _, n := range []float64{1, 2} {
				x := n / ch.Decay
				want := ch.WaitExceeds(x)
				near(t, "P(W > t)", sim.exceeds(x), want, 0.10*want+0.002)
			}
		})
	}
}

type simResult struct {
	waitShare, meanWait float64
	waits               []float64 // sorted
}

// exceeds is the share of simulated waits above x.
func (s simResult) exceeds(x float64) float64 {
	i, _ := slices.BinarySearch(s.waits, math.Nextafter(x, math.Inf(1)))
	return float64(len(s.waits)-i) / float64(len(s.waits))
}

// simulate runs the replica for n arrivals after a warm-up of n/10, with
// state-dependent completions min(n, c) * mu(min(n, c)) / min(n, c) = mu(m)
// and a FIFO queue of the arrivals that found every slot taken.
func simulate(r *rand.Rand, lam float64, mu func(m int) float64, c, n int) simResult {
	var now float64
	inSystem := 0
	// waiter is an arrival that found every slot taken; counted is false for
	// one that arrived in the warm-up, whose wait is not recorded.
	type waiter struct {
		at      float64
		counted bool
	}
	var queue []waiter
	waits := make([]float64, 0, n)
	warm := n / 10
	arrivals := 0
	for arrivals < n+warm {
		death := 0.0
		if inSystem > 0 {
			death = mu(min(inSystem, c))
		}
		total := lam + death
		now += r.ExpFloat64() / total
		if r.Float64()*total < lam {
			arrivals++
			counted := arrivals > warm
			if inSystem < c {
				if counted {
					waits = append(waits, 0)
				}
			} else {
				queue = append(queue, waiter{at: now, counted: counted})
			}
			inSystem++
			continue
		}
		inSystem--
		if len(queue) > 0 && inSystem >= c {
			if w := queue[0]; w.counted {
				waits = append(waits, now-w.at)
			}
			queue = queue[1:]
		}
	}
	waited, sum := 0, 0.0
	for _, w := range waits {
		if w > 0 {
			waited++
		}
		sum += w
	}
	slices.Sort(waits)
	return simResult{
		waitShare: float64(waited) / float64(len(waits)),
		meanWait:  sum / float64(len(waits)),
		waits:     waits,
	}
}

// routerEdges are the router's default per-request TPOT bucket edges up to
// 0.1 s (gateway-api-inference-extension v1.2.1, pkg/epp/metrics/metrics.go,
// request_tpot_seconds), then +Inf.
var routerEdges = []float64{0.0005, 0.00205, 0.005, 0.01, 0.02, 0.04, 0.06, 0.08, 0.1, math.Inf(1)}

// layouts are the bucket layouts the R_q property is drawn on: the router's
// defaults, and a non-uniform override of the kind the page recommends (fine
// around the target, coarse away from it).
var layouts = [][]float64{
	routerEdges,
	{0.001, 0.0015, 0.002, 0.0025, 0.00275, 0.003, 0.00325, 0.0035, 0.004, 0.005, 0.0075, 0.012, 0.03, math.Inf(1)},
}

func bucketed(samples []float64, edges []float64) TPOTWindow {
	w := TPOTWindow{Buckets: make([]HistogramBucket, len(edges))}
	for i, e := range edges {
		w.Buckets[i].UpperEdge = e
	}
	for _, s := range samples {
		w.Sum += s
		w.Count++
		for i, e := range edges {
			if s <= e {
				w.Buckets[i].Count++
			}
		}
	}
	return w
}

// A known per-request TPOT distribution, bucketed on the router's default
// edges, gives an R_q at or above the true ratio; R_q is never below 1, even
// from a window whose bucketed quantile falls below its mean.
func TestProperty_TPOTRatio(t *testing.T) {
	r := rand.New(rand.NewPCG(8, 9))
	const q = 0.9
	checked := 0
	for draw := range 200 {
		mean := 0.002 + 0.006*r.Float64()
		sigma := 0.05 + 0.6*r.Float64()
		n := 1 + r.IntN(20)
		windows := make([]TPOTWindow, 0, n)
		truth := make([]float64, 0, n)
		for range n {
			samples := make([]float64, 50+r.IntN(500))
			for i := range samples {
				samples[i] = mean * math.Exp(sigma*r.NormFloat64()-sigma*sigma/2)
			}
			w := bucketed(samples, layouts[draw%len(layouts)])
			windows = append(windows, w)
			s := slices.Clone(samples)
			slices.Sort(s)
			truth = append(truth, s[int(math.Ceil(q*float64(len(s))))-1]/(w.Sum/w.Count))
		}
		slices.Sort(truth)
		trueRatio := max(1, truth[int(math.Ceil(0.90*float64(len(truth))))-1])
		res := ratioFrom(windows, q)
		if u, ok := res.(Unanswerable); ok {
			if u.Reason != TPOTQuantileUnbounded {
				t.Fatalf("draw %d: %v", draw, u)
			}
			continue
		}
		got := mustAnswer(t, res)
		checked++
		if got.Value < trueRatio*(1-1e-12) {
			t.Fatalf("draw %d (mean %g, sigma %g): R_q %g below the true %g", draw, mean, sigma, got.Value, trueRatio)
		}
		if got.Value < 1 {
			t.Fatalf("draw %d: R_q %g below 1", draw, got.Value)
		}
	}

	if checked < 190 {
		t.Fatalf("only %d of 200 draws gave a ratio; the draw is not exercising the property", checked)
	}

	// A window whose 90th percentile bucket tops out under its mean: nine in
	// ten requests at 1 ms, one at 100 ms, so the mean is 10.9 ms and the
	// p90 bucket's edge 2.05 ms.
	skew := bucketed(append(slices.Repeat([]float64{0.001}, 90), slices.Repeat([]float64{0.1}, 10)...), routerEdges)
	if got := mustAnswer(t, ratioFrom([]TPOTWindow{skew}, q)); got.Value != 1 {
		t.Errorf("R_q from a quantile below the mean = %g, want 1", got.Value)
	}

	// The page's resolution note: at a 3.66 ms mean the p90 lands in (2.05, 5]
	// and reads 1.37, flagged coarse.
	flat := bucketed(slices.Repeat([]float64{0.00366}, 100), routerEdges)
	got := mustAnswer(t, ratioFrom([]TPOTWindow{flat}, q))
	near(t, "R_q on the default edges at 3.66 ms", got.Value, 0.005/0.00366, 1e-12)
	if !got.HasFlag(CoarseTPOTBucket) {
		t.Errorf("a (2.05, 5] ms bucket at a 3.66 ms mean is not flagged coarse")
	}
	fine := bucketed(slices.Repeat([]float64{0.00366}, 100), []float64{0.0035, 0.00375, 0.004, math.Inf(1)})
	if got := mustAnswer(t, ratioFrom([]TPOTWindow{fine}, q)); got.HasFlag(CoarseTPOTBucket) {
		t.Errorf("a 0.25 ms bucket at a 3.66 ms mean is flagged coarse")
	}

	// rate() over buckets scraped a moment apart leaves small dips in the
	// cumulative counts; they are made monotone, as histogram_quantile does,
	// not refused. Here +Inf reads 99.5 under the 5 ms bucket's 100: the
	// running maximum makes the total 100, so the p99.9 rank is 99.9 and lands
	// in (2.05, 5] ms. Ranked against the dipped 99.5 it would be 99.4, and
	// read 2.05 ms, under-stating the tail.
	dip := TPOTWindow{Buckets: []HistogramBucket{
		{UpperEdge: 0.00205, Count: 99.6}, {UpperEdge: 0.005, Count: 100}, {UpperEdge: math.Inf(1), Count: 99.5},
	}, Sum: 0.2, Count: 100}
	near(t, "R_q over a dip", mustAnswer(t, ratioFrom([]TPOTWindow{dip}, 0.999)).Value, 0.005/0.002, 1e-12)

	// The rank is against the +Inf bucket's count, the histogram's own total,
	// not _count, scraped separately: here _count reads 101 against 100 in
	// +Inf. At q = 0.99 the rank 99 falls in the 5 ms bucket (99 of 100);
	// against _count it would be 99.99, past every finite bucket, and read
	// as unbounded.
	skewCount := TPOTWindow{Buckets: []HistogramBucket{
		{UpperEdge: 0.00205, Count: 50}, {UpperEdge: 0.005, Count: 99}, {UpperEdge: math.Inf(1), Count: 100},
	}, Sum: 0.202, Count: 101}
	near(t, "R_q ranked against +Inf", mustAnswer(t, ratioFrom([]TPOTWindow{skewCount}, 0.99)).Value, 0.005/0.002, 1e-12)

	// Data that cannot be read is a refusal with a data reason (exit 3), not
	// a usage error; only a percentile outside (0, 1) is the caller's.
	for name, w := range map[string]TPOTWindow{
		"a NaN edge":        {Buckets: []HistogramBucket{{UpperEdge: math.NaN(), Count: 1}}, Sum: 1, Count: 1},
		"a negative count":  {Buckets: []HistogramBucket{{UpperEdge: 0.005, Count: -1}, {UpperEdge: math.Inf(1), Count: 1}}, Sum: 1, Count: 1},
		"a NaN count":       {Buckets: []HistogramBucket{{UpperEdge: 0.005, Count: math.NaN()}}, Sum: 1, Count: 1},
		"falling edges":     {Buckets: []HistogramBucket{{UpperEdge: 0.005, Count: 1}, {UpperEdge: 0.002, Count: 1}}, Sum: 1, Count: 1},
		"no sum":            {Buckets: []HistogramBucket{{UpperEdge: math.Inf(1), Count: 1}}, Sum: 0, Count: 1},
		"an infinite count": {Buckets: []HistogramBucket{{UpperEdge: math.Inf(1), Count: 1}}, Sum: 1, Count: math.Inf(1)},
		"no buckets":        {Sum: 1, Count: 1},
	} {
		if u, ok := ratioFrom([]TPOTWindow{w}, q).(Unanswerable); !ok || u.Reason != MalformedHistogram {
			t.Errorf("%s: %#v, want MalformedHistogram", name, ratioFrom([]TPOTWindow{w}, q))
		}
	}
	if u, ok := ratioFrom([]TPOTWindow{flat}, 1).(Unanswerable); !ok || u.Reason != InvalidInput {
		t.Errorf("q = 1: want InvalidInput")
	}

	if res := ratioFrom(nil, q); res.(Unanswerable).Reason != NoTPOTHistogram {
		t.Errorf("no windows: %#v", res)
	}
}
