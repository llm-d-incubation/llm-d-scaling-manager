package whatif

import (
	"math"
	"slices"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The router's flow-control wait above the prefill gate's onset
// (TTFTFleet.PriceRouterWait): the flag it takes, a simulation of the gated
// pods, and the second TTFT staircase's flow-control steps above the onset.

// fcFleet is fl with flow control on at the upstream thresholds and the
// router's wait priced or not.
func fcFleet(fl TTFTFleet, priced bool) TTFTFleet {
	fl.FlowControl = FlowControl{State: FlowControlOn, QueueThreshold: FlowControlQueueThreshold, KVThreshold: FlowControlKVThreshold}
	fl.PriceRouterWait = priced
	return fl
}

// Above the prefill onset the switch trades AboveFlowControlOnset for
// FlowControlWaitPriced and moves no figure. It takes only processor sharing
// at a prompt spread up to MaxPSPromptCV: past it, and on the first-come
// queue, the wait keeps its flag.
func TestFlowControlWait_FlagFollowsTheSwitch(t *testing.T) {
	base := psFleet(0.0577, 1)
	base.Decode.Replicas = 4
	on := mustOnsets(t, fcFleet(base, false))
	knee := mustTTFT(t, ttftAt(t, fcFleet(base, false), 1, 0.9)).PrefillKnee
	if !(on.Prefill < knee && knee < on.Decode) {
		t.Fatalf("precondition: prefill onset %g under the knee %g under the decode onset %g", on.Prefill, knee, on.Decode)
	}
	above, below := (on.Prefill+knee)/2, 0.9*on.Prefill
	for _, c := range []struct {
		name     string
		cv       float64
		ps       bool
		priced   bool
		wantFlag Flag
	}{
		{"switch off", 0.0577, true, false, AboveFlowControlOnset},
		{"switch on", 0.0577, true, true, FlowControlWaitPriced},
		{"past MaxPSPromptCV", 0.5, true, true, AboveFlowControlOnset},
		{"first come", 0.0577, false, true, AboveFlowControlOnset},
	} {
		fl := base
		fl.Prefill.PromptCV, fl.ProcessorSharing = c.cv, c.ps
		other := map[Flag]Flag{AboveFlowControlOnset: FlowControlWaitPriced, FlowControlWaitPriced: AboveFlowControlOnset}[c.wantFlag]
		for _, q := range []float64{0, 0.9} {
			got := mustTTFT(t, ttftAt(t, fcFleet(fl, c.priced), above, q))
			if !got.HasFlag(c.wantFlag) || got.HasFlag(other) {
				t.Errorf("%s, q %g, above the onset: flags %v, want %s and not %s", c.name, q, got.Flags, c.wantFlag, other)
			}
			if flip := mustTTFT(t, ttftAt(t, fcFleet(fl, !c.priced), above, q)); flip.Seconds != got.Seconds {
				t.Errorf("%s, q %g: the switch moved the figure, %g to %g", c.name, q, got.Seconds, flip.Seconds)
			}
			if b := mustTTFT(t, ttftAt(t, fcFleet(fl, c.priced), below, q)); b.HasFlag(AboveFlowControlOnset) || b.HasFlag(FlowControlWaitPriced) {
				t.Errorf("%s, q %g, below the onset: flags %v", c.name, q, b.Flags)
			}
		}
	}
	// Question 1 carries it from the answer's price: a target met only above
	// the onset.
	fl := fcFleet(base, true)
	target := TTFTTarget{Seconds: mustTTFT(t, ttftAt(t, fl, above, 0.9)).Seconds, Percentile: 0.9}
	a := mustAnswer(t, rateForTTFT(t, fl, target))
	if !(a.Value >= on.Prefill) || !slices.Contains(a.Flags, FlowControlWaitPriced) || slices.Contains(a.Flags, AboveFlowControlOnset) {
		t.Errorf("rate %g r/s (onset %g), flags %v: want FlowControlWaitPriced alone", a.Value, on.Prefill, a.Flags)
	}
}

// mustOnsets is fl's onsets, failing the test on a refusal.
func mustOnsets(t *testing.T, fl TTFTFleet) Onsets {
	t.Helper()
	on, ok := FlowControlOnsets(fl).(Onsets)
	if !ok {
		t.Fatalf("no onsets: %#v", FlowControlOnsets(fl))
	}
	return on
}

// gatedStage is tandemStage behind EPP flow control's gate, as it holds
// requests: each pod runs at most capL requests at once, sharing itself
// between them, and queues the rest first come (vLLM's waiting count); the
// router holds arrivals in one first-come queue while the pods' mean waiting
// count is at or above qt, and dispatches the head to the pod pick names
// while it is below. One dispatch cycle per event, no stale score: the gate
// reads the current count. held counts the requests the router held.
func gatedStage(capL int, qt float64, held *int) stageSim {
	return func(arr, size []float64, es float64, pods int, pick func() int) []float64 {
		type job struct {
			i   int
			rem float64
		}
		res := make([]float64, len(arr))
		active := make([][]job, pods)
		waiting := make([][]int, pods)
		var router []int
		waitingAll, next, done := 0, 0, 0
		now := 0.0
		dispatch := func() {
			for len(router) > 0 && float64(waitingAll) < qt*float64(pods) {
				i := router[0]
				router = router[1:]
				p := pick()
				if len(active[p]) < capL {
					active[p] = append(active[p], job{i: i, rem: es * size[i]})
				} else {
					waiting[p] = append(waiting[p], i)
					waitingAll++
				}
			}
		}
		for done < len(arr) {
			na := math.Inf(1)
			if next < len(arr) {
				na = arr[next]
			}
			tf, fp, least := math.Inf(1), -1, 0.0
			for p, a := range active {
				if len(a) == 0 {
					continue
				}
				m := a[0].rem
				for _, j := range a[1:] {
					m = min(m, j.rem)
				}
				if at := now + m*float64(len(a)); at < tf {
					tf, fp, least = at, p, m
				}
			}
			at := min(na, tf)
			for p, a := range active {
				if len(a) == 0 {
					continue
				}
				d := (at - now) / float64(len(a))
				if p == fp && tf <= na {
					d = least
				}
				for j := range a {
					a[j].rem -= d
				}
			}
			now = at
			if na <= tf {
				router = append(router, next)
				if len(router) > 1 || float64(waitingAll) >= qt*float64(pods) {
					*held++
				}
				next++
				dispatch()
				continue
			}
			kept := active[fp][:0]
			for _, j := range active[fp] {
				if j.rem <= 1e-15 {
					res[j.i] = now - arr[j.i]
					done++
				} else {
					kept = append(kept, j)
				}
			}
			active[fp] = kept
			for len(active[fp]) < capL && len(waiting[fp]) > 0 {
				i := waiting[fp][0]
				waiting[fp] = waiting[fp][1:]
				waitingAll--
				active[fp] = append(active[fp], job{i: i, rem: es * size[i]})
			}
			dispatch()
		}
		return res
	}
}

// The gate moves the wait, it does not add to it. Bucket B's fleet on four
// decode pods, prompts at the staircase's cv: with the prefill pods gated
// (running cap 1, 4 and 10, the router holding at the upstream threshold of
// 5), every request's prefill and pull, router wait included, is no slower
// than the processor-sharing stage prices, at the mean, p90 and p95, from
// below the onset to 0.97 of the knee, Poisson and at Ca^2 4. The price may
// read high (it prices no cap: at this spread a cap serves sooner); it may
// read at most 5 % short, TestPS_AgainstATandemSimulation's band below.
func TestFlowControlWait_GatedPodsAreNoSlower(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	for _, ca2 := range []float64{1, 4} {
		fl := psFleet(0.0577, ca2)
		fl.Decode.Replicas = 4
		pl, u := fl.plan()
		if u != nil {
			t.Fatal(u)
		}
		knee := pl.pf.knee()
		for _, share := range []float64{0.85, 0.95, 0.97} {
			rate := share * knee
			mix, _, bad := pl.pf.psStages(rate, 4, false)
			if bad != nil {
				t.Fatal(bad)
			}
			for _, capL := range []int{1, 4, 10} {
				var held int
				sim := simulateTandem(pl.pf, rate, 4, 200000, 1, gatedStage(capL, FlowControlQueueThreshold, &held))
				for _, k := range []struct {
					what       string
					model, sim float64
				}{
					{"mean", mix.mean(), sim.mean},
					{"p90", mix.quantile(0.1), sim.quantile(0.9)},
					{"p95", mix.quantile(0.05), sim.quantile(0.95)},
				} {
					e := k.model/k.sim - 1
					t.Logf("Ca^2 %g, %.2f of the knee, cap %d (router held %.2f %%) %s: model %.1f ms, gated simulation %.1f ms, %+.1f %%",
						ca2, share, capL, 100*float64(held)/200000, k.what, 1e3*k.model, 1e3*k.sim, 100*e)
					if e < -0.05 {
						t.Errorf("Ca^2 %g, %.2f of the knee, cap %d, %s: the price reads %+.1f %% under the gated pods", ca2, share, capL, k.what, 100*e)
					}
				}
			}
		}
	}
}

// r2Fleet is the second TTFT staircase's frozen fleet for B-onset (8000-token
// prompts, two prefill pods and four decode pods, Poisson) or C-onset (the
// same at Ca^2 4), on the processor-sharing model with flow control off: the
// freeze's fleet to the bit (TestStaircaseR2_TheFixtureIsTheFrozenCurve).
func r2Fleet(bucket string) TTFTFleet {
	ca2 := 0.0
	if bucket == "C-onset" {
		ca2 = 4
	}
	return TTFTFleet{
		Prefill: Prefill{
			VP: 289512.02, FixedLatency: 0.009058506509559468, Imbalance: 1.166048145014318, Replicas: 2,
			PromptTokens: 8000, PromptCV: 0.0577, ArrivalCV2: ca2, TransferSecondsPerToken: 2.550978238664303e-06,
			VPPromptTokens: 7996.199322963447, VPMargin: DefaultVPMargin,
		},
		Decode: &Decode{
			Line: itl.Model{A: 0.03797690970912876, B: 0.0017939947848729556}, Band: FillBand{Lo: 0.15, Hi: 0.8},
			CapacityTokens: 1162240, KVPerRequest: 8107.454733896249, OutputTokens: 200, ReplicaCap: 256, Replicas: 4, Tier: "held",
		},
		DecodeTerm: &DecodeTerm{
			Engine:  PromptLine{Base: -0.0009056688548874092, PerToken: 4.072428985508958e-06},
			PerStep: 6.6858916415532095,
			Hop:     PromptLine{Base: 0.006682283936955476, PerToken: 1.4086512819624945e-06},
		},
		ProcessorSharing: true,
		FlowControl:      FlowControl{State: FlowControlOff},
	}
}

// r2Step is one step of an onset phase: the rate asked, the frozen curve's
// mean, p90 and p95 there (seconds), and the client's over the three
// repeats, the largest of the three at each (ms), over each step's judged
// window. held is false where the step did not hold (requests failed).
type r2Step struct {
	rate   float64
	frozen [3]float64
	client [3]float64
	held   bool
}

// r2Phases are the onset phases' steps under their frozen prefill knee; the
// steps at and past it the model refuses (Saturated), and the client held
// B-onset's to 1.04 of it and lost both phases' next step.
var r2Phases = map[string][]r2Step{
	"B-onset": {
		{41.27734650648493, [3]float64{0.1714077317038026, 0.2628252131531569, 0.306197156413447}, [3]float64{165.0, 249.9, 294.0}, true},
		{50.12249218644599, [3]float64{0.245374137336538, 0.44616693300152344, 0.5555154682554199}, [3]float64{204.5, 327.6, 388.6}, true},
		{54.25022683709449, [3]float64{0.3565587162451949, 0.7565695561476165, 0.9820477075653027}, [3]float64{238.1, 391.8, 467.8}, true},
		{56.60893235175076, [3]float64{0.5775767576351867, 1.4066513771233633, 1.8837637761458752}, [3]float64{267.5, 446.3, 536.4}, true},
		{57.19860873041483, [3]float64{0.7214680200493364, 1.8368052288054986, 2.482270876962318}, [3]float64{269.0, 457.3, 542.4}, true},
	},
	"C-onset": {
		{41.27734650648493, [3]float64{0.2596183058603909, 0.5015701786372746, 0.6375822673154117}, [3]float64{226.4, 379.6, 471.8}, true},
		{50.12249218644599, [3]float64{0.44158572482708447, 0.9814249841675681, 1.3041205282377846}, [3]float64{316.5, 580.1, 683.6}, true},
		{54.25022683709449, [3]float64{0.7314641244506076, 1.8128326269264088, 2.4585918906379494}, [3]float64{402.0, 741.9, 893.4}, true},
		{56.60893235175076, [3]float64{1.3264139692419823, 3.580382130116787, 4.9196100998085734}, [3]float64{}, false},
	},
}

// The fixture is the frozen fleet: its TTFT at every step is the frozen
// curve's, to the bit printed.
func TestStaircaseR2_TheFixtureIsTheFrozenCurve(t *testing.T) {
	for bucket, steps := range r2Phases {
		for _, s := range steps {
			for i, q := range []float64{0, 0.9, 0.95} {
				got := psTTFT(t, r2Fleet(bucket), s.rate, q).Seconds
				nearRel(t, bucket+" frozen curve", got, s.frozen[i], 1e-9)
			}
		}
	}
}

// The second staircase sent steps just above the prefill onset with flow
// control on. With the router's wait priced, every step that held reads at
// or above the slowest of the three repeats' client TTFT, at the mean, p90
// and p95, and the steps at or past the onset carry FlowControlWaitPriced.
// The router held almost nothing there (at most 2 % of requests past 10 ms,
// a mean wait under 4 ms): the onset sits under where the gate closed, since
// the pods served faster than the frozen V_P, and the price reads 1.0x to
// 3.7x the client.
func TestStaircaseR2_OnsetStepsReadAboveTheClient(t *testing.T) {
	for _, bucket := range []string{"B-onset", "C-onset"} {
		fl := fcFleet(r2Fleet(bucket), true)
		on := mustOnsets(t, fl)
		for _, s := range r2Phases[bucket] {
			if !s.held {
				continue
			}
			for i, q := range []float64{0, 0.9, 0.95} {
				got := mustTTFT(t, ttftAt(t, fl, s.rate, q))
				t.Logf("%s %.2f r/s (%.3fx the onset) q %g: model %.1f ms, client %.1f ms, %.2fx",
					bucket, s.rate, s.rate/on.Prefill, q, 1e3*got.Seconds, s.client[i], 1e3*got.Seconds/s.client[i])
				if 1e3*got.Seconds < s.client[i] {
					t.Errorf("%s %.2f r/s q %g: model %.1f ms under the client's %.1f ms", bucket, s.rate, q, 1e3*got.Seconds, s.client[i])
				}
				if above := s.rate >= on.Prefill; got.HasFlag(FlowControlWaitPriced) != above || got.HasFlag(AboveFlowControlOnset) {
					t.Errorf("%s %.2f r/s (onset %.2f): flags %v", bucket, s.rate, on.Prefill, got.Flags)
				}
			}
		}
	}
}
