package whatif

import (
	"math"
	"slices"
	"testing"
)

// The flow-control onsets on the processor-sharing model (TTFTFleet.PSOnsets)
// and the decode gate's closed share charged to question 1
// (TTFTFleet.ChargeDecodeGate).

// onFleet is fl with flow control on at the upstream thresholds and both
// switches as given, the running cap at k.
func onFleet(fl TTFTFleet, psOnsets, charge bool, k int) TTFTFleet {
	fl = fcFleet(fl, false)
	fl.PSOnsets, fl.ChargeDecodeGate, fl.Prefill.RunningCap = psOnsets, charge, k
	return fl
}

// mustPlan is fl's plan, failing the test on a refusal.
func mustPlan(t *testing.T, fl TTFTFleet) ttftPlan {
	t.Helper()
	pl, u := fl.plan()
	if u != nil {
		t.Fatalf("no plan: %+v", *u)
	}
	return pl
}

// workedOn is the worked fleet with flow control on: its decode onset, 12.50
// r/s, sits under its prefill onset and its knees, so it is what caps
// question 1.
func workedOn(charge bool) TTFTFleet {
	fl := workedFleet()
	fl.FlowControl = FlowControl{State: FlowControlOn, QueueThreshold: FlowControlQueueThreshold, KVThreshold: FlowControlKVThreshold}
	fl.ChargeDecodeGate = charge
	return fl
}

func TestOnsetSwitches_OffByDefault(t *testing.T) {
	if DefaultPSOnsets || DefaultChargeDecodeGate {
		t.Fatalf("PSOnsets %v, ChargeDecodeGate %v: both are off by default", DefaultPSOnsets, DefaultChargeDecodeGate)
	}
}

// psWaiting is the stage's own law of the prompts on a pod: at Poisson the
// M/G/1-PS law, P(N >= n) = rho^n, so E[(N - k)+] = rho^(k + 1)/(1 - rho);
// at any burst factor its mean (k 0) is Little's law on the stage's sojourn,
// rho + s rho^2/(1 - rho), and it falls with the cap.
func TestPSWaiting_IsTheStagesLaw(t *testing.T) {
	for _, rho := range []float64{0.1, 0.5, 0.9, 0.99} {
		for _, k := range []int{0, 1, 5, 10} {
			nearRel(t, "Poisson", psWaiting(rho, 1, k), math.Pow(rho, float64(k+1))/(1-rho), 1e-12)
		}
		for _, s := range []float64{0.75, 1, 2.5} {
			nearRel(t, "the stage's mean", psWaiting(rho, s, 0), rho+s*rho*rho/(1-rho), 1e-12)
			for k := range 20 {
				if !(psWaiting(rho, s, k+1) < psWaiting(rho, s, k)) {
					t.Errorf("rho %g s %g: the count does not fall from cap %d to %d", rho, s, k, k+1)
				}
			}
		}
	}
	if psWaiting(0, 1, 1) != 0 || !math.IsInf(psWaiting(1, 1, 1), 1) {
		t.Errorf("edges: rho 0 reads %g, rho 1 %g", psWaiting(0, 1, 1), psWaiting(1, 1, 1))
	}
}

// Placed on processor sharing, the prefill onset is where the stage's score
// crosses 1, it rises with the running cap, and an unknown cap is the lower
// of cap 1 and first come, here cap 1. Pinned on the second staircase's frozen fleets, so a move shows:
// first come put them at 56.08 (B-onset) and 49.06 r/s (C-onset), unchanged
// with the switch off. Cap 10, the most a pod was seen to run, puts B-onset
// at 55.76 and C-onset at 51.23: C-onset's Ca^2 4 reaches each pod at 2.5
// (splitCA2), which the first-come count read at the fleet's 4.
func TestPSOnsets_ThePrefillOnsetIsTheStagesCrossing(t *testing.T) {
	for _, c := range []struct {
		bucket               string
		fcfs, unknown, atTen float64
	}{
		{"B-onset", 56.08243310655091, 53.46217116970281, 55.764298552772956},
		{"C-onset", 49.062792791985345, 46.75836065994076, 51.22773157032857},
	} {
		off := mustOnsets(t, onFleet(r2Fleet(c.bucket), false, false, 10))
		nearRel(t, c.bucket+" first come", off.Prefill, c.fcfs, 1e-9)
		for _, k := range []int{0, 1} {
			nearRel(t, c.bucket+" unknown cap", mustOnsets(t, onFleet(r2Fleet(c.bucket), true, false, k)).Prefill, c.unknown, 1e-9)
		}
		fl := onFleet(r2Fleet(c.bucket), true, false, 10)
		on := mustOnsets(t, fl)
		nearRel(t, c.bucket+" cap 10", on.Prefill, c.atTen, 1e-9)
		if on.Decode != off.Decode {
			t.Errorf("%s: the switch moved the decode onset, %g to %g", c.bucket, off.Decode, on.Decode)
		}
		pl := mustPlan(t, fl)
		qt := pl.fc.QueueThreshold
		if lo, hi := pl.pf.psMeanScore(on.Prefill*(1-1e-9), qt, 10), pl.pf.psMeanScore(on.Prefill*(1+1e-9), qt, 10); !(lo < 1 && hi >= 1) {
			t.Errorf("%s: the stage's score reads %g just under the onset and %g just over it", c.bucket, lo, hi)
		}
		prev := 0.0
		for _, k := range []int{1, 2, 5, 10, 64, 256} {
			got := mustOnsets(t, onFleet(r2Fleet(c.bucket), true, false, k)).Prefill
			if !(got > prev && got < pl.pf.knee()) {
				t.Errorf("%s cap %d: onset %g, want above %g (the lower cap's) and under the knee %g", c.bucket, k, got, prev, pl.pf.knee())
			}
			prev = got
		}
	}
}

// An unknown running cap is never placed above the first-come onset, so the
// switch is never more optimistic than with it off. Cap 1 alone can be:
// processor sharing reads each pod at its own Ca^2 (splitCA2), which falls
// toward 1 as pods are added, where first come reads the fleet's. The
// C-onset fleet balanced (f 1) on 64 decode pods: at Ca^2 4 over 4 prefill
// pods first come is 105.26 r/s and cap 1 107.92; over 16, 421.05 and
// 459.11; at Ca^2 9 over 16, 349.80 and 443.11.
func TestPSOnsets_AnUnknownCapIsNeverAboveFirstCome(t *testing.T) {
	for _, c := range []struct {
		ca2          float64
		pods         int
		fcfs, capOne float64
	}{{4, 4, 105.2624, 107.9188}, {4, 16, 421.0497, 459.1122}, {9, 16, 349.7999, 443.1082}} {
		fl := r2Fleet("C-onset")
		fl.Prefill.Imbalance, fl.Prefill.Replicas, fl.Prefill.ArrivalCV2 = 1, c.pods, c.ca2
		fl.Decode.Replicas = 64
		fcfs := mustOnsets(t, onFleet(fl, false, false, 0)).Prefill
		capOne := mustOnsets(t, onFleet(fl, true, false, 1)).Prefill
		unknown := mustOnsets(t, onFleet(fl, true, false, 0)).Prefill
		nearRel(t, "first come", fcfs, c.fcfs, 1e-5)
		nearRel(t, "cap 1", capOne, c.capOne, 1e-5)
		if unknown != fcfs {
			t.Errorf("Ca^2 %g, %d pods: unknown cap %g, want the first-come %g (cap 1 reads %g)", c.ca2, c.pods, unknown, fcfs, capOne)
		}
	}
}

// The switch takes only on processor sharing: on the first-come model the
// onsets do not move. Past MaxPSPromptCV, where the price is the larger of
// the two queues', the onset is the lower of the two placements.
func TestPSOnsets_TakesOnProcessorSharingAlone(t *testing.T) {
	fcfs := r2Fleet("B-onset")
	fcfs.ProcessorSharing = false
	if a, b := mustOnsets(t, onFleet(fcfs, false, false, 10)), mustOnsets(t, onFleet(fcfs, true, false, 10)); a != b {
		t.Errorf("first come: the switch moved the onsets, %+v to %+v", a, b)
	}
	for _, k := range []int{1, 64} {
		wide := r2Fleet("B-onset")
		wide.Prefill.PromptCV = 0.5
		pl := mustPlan(t, onFleet(wide, true, false, k))
		qt := pl.fc.QueueThreshold
		ps := onset(pl.pf.knee(), func(r float64) float64 { return pl.pf.psMeanScore(r, qt, k) })
		first := mustOnsets(t, onFleet(wide, false, false, k)).Prefill
		got := mustOnsets(t, onFleet(wide, true, false, k)).Prefill
		nearRel(t, "past MaxPSPromptCV", got, min(ps, first), 1e-9)
		t.Logf("cv 0.5, cap %d: processor sharing %.3f, first come %.3f r/s", k, ps, first)
	}
	bad := onFleet(r2Fleet("B-onset"), true, false, -1)
	if u, ok := FlowControlOnsets(bad).(Unanswerable); !ok || u.Reason != InvalidInput {
		t.Errorf("a negative running cap: %#v, want InvalidInput", FlowControlOnsets(bad))
	}
}

// C-onset's frozen target, the model's own p90 and p95 at 50.12 r/s: first
// come capped question 1 at its onset, 49.06 r/s. On processor sharing at cap
// 10 the onset is past the target's own rate, and the target sets it, 50.12;
// at an unknown cap the onset is lower, and caps it at 46.76.
func TestPSOnsets_TheCOnsetTarget(t *testing.T) {
	for _, q := range []float64{0.9, 0.95} {
		target := TTFTTarget{Seconds: psTTFT(t, r2Fleet("C-onset"), 50.12249218644599, q).Seconds, Percentile: q}
		for _, c := range []struct {
			name string
			ps   bool
			k    int
			want float64
			by   Bound
		}{
			{"off", false, 10, 49.062792791985345, BoundPrefillOnset},
			{"cap 10", true, 10, 50.12249218644599, BoundTTFT},
			{"cap unknown", true, 0, 46.75836065994076, BoundPrefillOnset},
		} {
			a := mustAnswer(t, rateForTTFT(t, onFleet(r2Fleet("C-onset"), c.ps, false, c.k), target))
			nearRel(t, c.name, a.Value, c.want, 1e-9)
			if !slices.Equal(a.Basis.BoundBy, []Bound{c.by}) {
				t.Errorf("%s q %g: bound %v, want %s", c.name, q, a.Basis.BoundBy, c.by)
			}
		}
	}
}

// The chain's tail is its stationary law: on one slot at a constant
// completion rate P(n >= k) = rho^k (M/M/1), on c slots at m mu P(n >= c) is
// Erlang C, and at c it is the admission's wait probability.
func TestWeights_AtLeastIsTheChainsTail(t *testing.T) {
	for _, rho := range []float64{0.2, 0.7, 0.95} {
		w, ok := solveChain(rho, func(int) float64 { return 1 }, 1)
		if !ok {
			t.Fatalf("rho %g: no chain", rho)
		}
		for _, k := range []int{0, 1, 2, 7, 40} {
			nearRel(t, "M/M/1", w.atLeast(k), math.Pow(rho, float64(k)), 1e-12)
		}
		for _, c := range []int{2, 5, 32} {
			lam := rho * float64(c)
			w, ok := solveChain(lam, func(m int) float64 { return float64(m) }, c)
			if !ok {
				t.Fatalf("rho %g c %d: no chain", rho, c)
			}
			nearRel(t, "Erlang C", w.atLeast(c), erlangCProb(c, lam), 1e-10)
			nearRel(t, "past the slots", w.atLeast(c+3), w.atLeast(c)*rho*rho*rho, 1e-10)
			if !(w.atLeast(c-1) > w.atLeast(c)) {
				t.Errorf("rho %g c %d: the tail does not fall at the slots", rho, c)
			}
		}
	}
	d := *workedFleet().Decode
	rate := 0.9 * d.kneeRate()
	adm := mustAdmission(t, DecodeAdmission(d, rate))
	w, _ := solveChain(rate/float64(d.Replicas), func(m int) float64 { return d.chainMu(float64(m)) }, adm.Slots)
	nearRel(t, "the wait probability", w.atLeast(adm.Slots), adm.WaitProbability, 1e-12)
	if w, _ := solveChain(0, func(int) float64 { return 1 }, 3); w.atLeast(0) != 1 || w.atLeast(1) != 0 {
		t.Errorf("no arrivals: P(n >= 0) %g, P(n >= 1) %g", w.atLeast(0), w.atLeast(1))
	}
}

// mustAdmission is r as an Admission, failing the test otherwise.
func mustAdmission(t *testing.T, r Result) Admission {
	t.Helper()
	a, ok := r.(Admission)
	if !ok {
		t.Fatalf("want an admission, got %#v", r)
	}
	return a
}

// The problem the charge fixes: just under the worked fleet's decode onset,
// where question 1 capped, the gate is closed nearly a quarter of the time
// (23.7 %); the share falls fast below it, and rises with the rate.
func TestChargeDecodeGate_TheGateClosesBelowTheOnset(t *testing.T) {
	pl := mustPlan(t, workedOn(true))
	on := pl.onsets.Decode
	at := pl.gateClosed(on * (1 - 1e-9))
	t.Logf("decode onset %.4f r/s: closed %.4f at it, %.4f at 0.9 of it, %.6f at 0.8", on, at, pl.gateClosed(0.9*on), pl.gateClosed(0.8*on))
	if !(at > 0.2) {
		t.Errorf("closed %g of the time at the onset, want over 0.2", at)
	}
	prev := 0.0
	for _, s := range []float64{0.5, 0.8, 0.9, 0.95, 1 - 1e-9} {
		got := pl.gateClosed(s * on)
		if !(got >= prev && got < 1) {
			t.Errorf("at %g of the onset: closed %g, want at least %g and under 1", s, got, prev)
		}
		prev = got
	}
	if got := pl.gateClosed(pl.d.kneeRate()); got != 1 {
		t.Errorf("at the decode knee: closed %g, want 1", got)
	}
}

// A mean target has no tail to charge: it is capped where the closed share
// reaches MaxDecodeGateClosedShare. The boundary: at the answer the share is
// under it, a relative 1e-6 past it the share is over it, and the cap is
// BoundDecodeGateCharge, not BoundDecodeOnset. On the worked fleet the cap
// moves from the onset, 12.50 r/s, to 11.20; with the switch off it does
// not move.
func TestChargeDecodeGate_TheMeanCapIsTheClosedShare(t *testing.T) {
	target := TTFTTarget{Seconds: 30}
	off := mustAnswer(t, rateForTTFT(t, workedOn(false), target))
	nearRel(t, "off", off.Value, 12.500975816823301, 1e-9)
	a := mustAnswer(t, rateForTTFT(t, workedOn(true), target))
	nearRel(t, "charged", a.Value, 11.19528267724617, 1e-6)
	if !slices.Equal(a.Basis.BoundBy, []Bound{BoundDecodeGateCharge}) {
		t.Errorf("bound %v, want the gate charge", a.Basis.BoundBy)
	}
	pl := mustPlan(t, workedOn(true))
	if under, over := pl.gateClosed(a.Value), pl.gateClosed(a.Value*(1+1e-6)); !(under < MaxDecodeGateClosedShare && over >= MaxDecodeGateClosedShare) {
		t.Errorf("closed %g at the answer and %g just past it, want %g between", under, over, MaxDecodeGateClosedShare)
	}
}

// A percentile target is charged the closed share in its tail: the answer is
// the largest rate whose TTFT at q plus the share meets the target, so a
// request meeting a closed gate is counted a miss. At the answer the charged
// percentile meets the target and a relative 1e-6 past it does not; the
// bound is the target, then the gate charge; and it is under the uncharged
// rate. The worked fleet's loose 30 s target, which the decode onset capped
// at 12.50 r/s, reads 12.09 at p90 and 11.80 at p95.
func TestChargeDecodeGate_APercentileIsChargedItsTail(t *testing.T) {
	for _, c := range []struct {
		q, want float64
	}{{0.9, 12.09347339913577}, {0.95, 11.798074401859225}} {
		target := TTFTTarget{Seconds: 30, Percentile: c.q}
		off := mustAnswer(t, rateForTTFT(t, workedOn(false), target))
		a := mustAnswer(t, rateForTTFT(t, workedOn(true), target))
		nearRel(t, "charged", a.Value, c.want, 1e-6)
		if !(a.Value < off.Value) || !slices.Equal(a.Basis.BoundBy, []Bound{BoundTTFT, BoundDecodeGateCharge}) {
			t.Errorf("q %g: %g r/s bound %v, off %g; want under it, bound TTFT then the gate charge", c.q, a.Value, a.Basis.BoundBy, off.Value)
		}
		pl := mustPlan(t, workedOn(true))
		meets := func(r float64) bool {
			q := c.q + pl.gateClosed(r)
			if !(q < 1) {
				return false
			}
			v, ok := TTFTAt(workedOn(true), r, q).(TTFT)
			return ok && v.Seconds <= target.Seconds
		}
		if !meets(a.Value) || meets(a.Value*(1+1e-6)) {
			t.Errorf("q %g: the charged target holds at %g r/s %v, just past it %v", c.q, a.Value, meets(a.Value), meets(a.Value*(1+1e-6)))
		}
	}
	// A target the decode gate does not reach is not charged: one met at
	// half the decode onset, where the gate is almost never closed, answers
	// the same rate within the bisection's resolution and the same bound.
	pl := mustPlan(t, workedOn(true))
	target := TTFTTarget{Seconds: mustTTFT(t, ttftAt(t, workedOn(false), 0.5*pl.onsets.Decode, 0.9)).Seconds, Percentile: 0.9}
	off, on := mustAnswer(t, rateForTTFT(t, workedOn(false), target)), mustAnswer(t, rateForTTFT(t, workedOn(true), target))
	if share := pl.gateClosed(off.Value); !(share < 1e-6) {
		t.Fatalf("precondition: the gate closed %g of the time at the uncharged %g r/s, want under 1e-6", share, off.Value)
	}
	nearRel(t, "tight target", on.Value, off.Value, 1e-6)
	if !slices.Equal(on.Basis.BoundBy, off.Basis.BoundBy) {
		t.Errorf("tight target: bound %v charged, %v off", on.Basis.BoundBy, off.Basis.BoundBy)
	}
}

// Where the charge and decode admission both hold the rate, both bounds
// stand and HeldByDecodeAdmission with them: bucket B's fleet on one decode
// pod, a 1 s p90 target.
func TestChargeDecodeGate_KeepsDecodeAdmissionBesideIt(t *testing.T) {
	fl := onFleet(psFleet(0.0577, 1), true, true, 0)
	fl.Decode.Replicas = 1
	a := mustAnswer(t, rateForTTFT(t, fl, TTFTTarget{Seconds: 1, Percentile: 0.9}))
	if want := []Bound{BoundTTFT, BoundDecodeGateCharge, BoundDecodeAdmission}; !slices.Equal(a.Basis.BoundBy, want) || !slices.Contains(a.Flags, HeldByDecodeAdmission) {
		t.Errorf("%g r/s: bound %v, flags %v; want %v and HeldByDecodeAdmission", a.Value, a.Basis.BoundBy, a.Flags, want)
	}
}

// The gate closes only with some pod over 1, so over N_d independent pods it
// is closed at most 1 - (1 - p)^N_d of the time, p one pod's share: at 0.9 of
// the decode onset, the worked fleet's decode line on 1, 2, 16 and 32 pods,
// each pod at the same load, p is 1.17 % and the share 1.17 %, 2.33 %, 17.2 %
// and 31.5 %. The bound loosens as the pods grow.
func TestChargeDecodeGate_TheShareCompoundsOverThePods(t *testing.T) {
	for _, c := range []struct {
		pods int
		want float64
	}{{1, 0.01174}, {2, 0.02333}, {16, 0.17211}, {32, 0.31460}} {
		fl := workedOn(true)
		d := *fl.Decode
		d.Replicas = c.pods
		fl.Decode = &d
		pl := mustPlan(t, fl)
		rate := 0.9 * pl.onsets.Decode
		p := decodeScoreTail(d, rate, pl.fc.QueueThreshold, pl.fc.KVThreshold)
		got := pl.gateClosed(rate)
		nearRel(t, "one pod's share", p, 0.01174, 1e-3)
		nearRel(t, "1 - (1 - p)^N", got, 1-math.Pow(1-p, float64(c.pods)), 1e-12)
		nearRel(t, "the share", got, c.want, 1e-3)
		if c.pods > 1 && !(got > p) {
			t.Errorf("%d pods: share %g, not above one pod's %g", c.pods, got, p)
		}
	}
}

// A decode pod's score reaches 1 at the fewer of two states: the waiting
// count at the queue threshold past the slots, or the fill at the KV
// threshold where the slots hold that many. On the OLS-sweep line with a
// 256 cap and the band's top at 0.95, the slots are 83 and the fill reaches
// 0.8 at 70 requests, before the queue limit 88: the tail is P(n >= 70).
// With the KV threshold out of reach it is the queue's, P(n >= 88).
func TestDecodeScoreTail_TheKVLimitBindsFirst(t *testing.T) {
	d := olsSweep()
	d.ReplicaCap, d.Band.Hi = 256, 0.95
	c := int(d.full().slots)
	if c != 83 {
		t.Fatalf("precondition: %d slots, want 83", c)
	}
	rate := 0.9 * d.kneeRate()
	w, ok := solveChain(rate, func(m int) float64 { return d.chainMu(float64(m)) }, c)
	if !ok {
		t.Fatal("no chain")
	}
	kv, queue := w.atLeast(70), w.atLeast(c+5)
	if !(kv > queue) {
		t.Fatalf("precondition: P(n >= 70) %g not above P(n >= 88) %g", kv, queue)
	}
	nearRel(t, "KV-bound", decodeScoreTail(d, rate, 5, 0.8), kv, 1e-12)
	nearRel(t, "queue-bound", decodeScoreTail(d, rate, 5, 10), queue, 1e-12)
}

// Both switches on, every rate question 1 answers still meets its own
// verdict, and the charge never answers above the uncharged rate: across the
// fleets TestRateForTTFT_TheAnswerMeetsItsOwnVerdict holds, the running cap
// unknown and at 10, targets tight to loose.
func TestOnsetSwitches_TheAnswerMeetsItsOwnVerdict(t *testing.T) {
	b, c := psFleet(0.0577, 1), psFleet(0.0577, 4)
	b.Decode.Replicas, c.Decode.Replicas = 4, 4
	fleets := map[string]TTFTFleet{"worked": workedFleet(), "B": b, "C": c, "B-onset": r2Fleet("B-onset"), "C-onset": r2Fleet("C-onset")}
	bounds := map[Bound]int{}
	for name, base := range fleets {
		for _, k := range []int{0, 10} {
			for _, secs := range []float64{0.2, 0.5, 1, 2, 5, 30} {
				for _, q := range []float64{0, 0.9, 0.95} {
					target := TTFTTarget{Seconds: secs, Percentile: q}
					fl := onFleet(base, true, true, k)
					a, ok := RateForTTFT(fl, target).(Answer)
					if !ok {
						continue
					}
					if met, known := TTFTMeets(TTFTAt(fl, a.Value, q), target); !met || !known {
						t.Errorf("%s cap %d, %g s at q %g: rate %g r/s (bound %v) misses its own verdict", name, k, secs, q, a.Value, a.Basis.BoundBy)
					}
					if u, ok := RateForTTFT(onFleet(base, true, false, k), target).(Answer); ok && a.Value > u.Value {
						t.Errorf("%s cap %d, %g s at q %g: charged %g r/s above uncharged %g", name, k, secs, q, a.Value, u.Value)
					}
					for _, by := range a.Basis.BoundBy {
						bounds[by]++
					}
				}
			}
		}
	}
	t.Logf("bounds: %v", bounds)
	if bounds[BoundPrefillOnset] == 0 || bounds[BoundDecodeGateCharge] == 0 || bounds[BoundTTFT] == 0 {
		t.Errorf("not every bound reached: %v", bounds)
	}
}

// With no decode line there is no decode gate: ChargeDecodeGate answers what
// it answers off, and says it did nothing. The second staircase's B-onset
// prefill side, decode left out.
func TestChargeDecodeGate_NoDecodeLineIsUncharged(t *testing.T) {
	fl := r2Fleet("B-onset")
	fl.Decode, fl.DecodeTerm = nil, nil
	for _, q := range []float64{0, 0.9} {
		target := TTFTTarget{Seconds: 0.4, Percentile: q}
		off := mustAnswer(t, rateForTTFT(t, onFleet(fl, false, false, 0), target))
		on := mustAnswer(t, rateForTTFT(t, onFleet(fl, false, true, 0), target))
		if on.Value != off.Value || !slices.Equal(on.Basis.BoundBy, off.Basis.BoundBy) {
			t.Errorf("q %g: charged %g r/s %v, off %g %v", q, on.Value, on.Basis.BoundBy, off.Value, off.Basis.BoundBy)
		}
		if g, ok := DecodeGateChargeFor(onFleet(fl, false, true, 0), target); !ok || g != GateNoDecodeLine {
			t.Errorf("q %g: charge %q %v, want %q", q, g, ok, GateNoDecodeLine)
		}
	}
}

// The running cap is priced up to MaxRunningCap and refused past it.
func TestPSOnsets_TheRunningCapsBound(t *testing.T) {
	if _, ok := FlowControlOnsets(onFleet(r2Fleet("B-onset"), true, false, MaxRunningCap)).(Onsets); !ok {
		t.Errorf("cap %d: %#v, want onsets", MaxRunningCap, FlowControlOnsets(onFleet(r2Fleet("B-onset"), true, false, MaxRunningCap)))
	}
	if u, ok := FlowControlOnsets(onFleet(r2Fleet("B-onset"), true, false, MaxRunningCap+1)).(Unanswerable); !ok || u.Reason != InvalidInput {
		t.Errorf("cap %d: %#v, want InvalidInput", MaxRunningCap+1, FlowControlOnsets(onFleet(r2Fleet("B-onset"), true, false, MaxRunningCap+1)))
	}
}

// What the output says produced an answer: the placement each fleet takes,
// and what the charge does on each.
func TestOnsetSwitches_SayWhatTheyDid(t *testing.T) {
	wide := r2Fleet("B-onset")
	wide.Prefill.PromptCV = 0.5
	fcfs := r2Fleet("B-onset")
	fcfs.ProcessorSharing = false
	for _, c := range []struct {
		name string
		fl   TTFTFleet
		want OnsetPlacement
	}{
		{"off", onFleet(r2Fleet("B-onset"), false, false, 10), PlacedFirstCome},
		{"first come", onFleet(fcfs, true, false, 10), PlacedFirstCome},
		{"cap 10", onFleet(r2Fleet("B-onset"), true, false, 10), PlacedProcessorSharing},
		{"cap unknown", onFleet(r2Fleet("B-onset"), true, false, 0), PlacedLowerCapUnknown},
		{"wide spread", onFleet(wide, true, false, 10), PlacedLowerWideSpread},
	} {
		if got, ok := PrefillOnsetPlacement(c.fl); !ok || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, ok, c.want)
		}
	}
	if _, ok := PrefillOnsetPlacement(r2Fleet("B-onset")); ok {
		t.Error("flow control off: a placement, want none")
	}
	noFC := r2Fleet("B-onset")
	noFC.ChargeDecodeGate = true
	for _, c := range []struct {
		name string
		fl   TTFTFleet
		q    float64
		want GateCharge
		ok   bool
	}{
		{"off", workedOn(false), 0.9, "", false},
		{"p90", workedOn(true), 0.9, GateChargedToTail, true},
		{"mean", workedOn(true), 0, GateCappedForMean, true},
		{"flow control off", noFC, 0.9, GateFlowControlNotOn, true},
	} {
		if got, ok := DecodeGateChargeFor(c.fl, TTFTTarget{Seconds: 1, Percentile: c.q}); got != c.want || ok != c.ok {
			t.Errorf("%s: %q %v, want %q %v", c.name, got, ok, c.want, c.ok)
		}
	}
}
