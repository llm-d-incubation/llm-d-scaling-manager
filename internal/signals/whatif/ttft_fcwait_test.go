package whatif

import (
	"fmt"
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
// at a prompt spread up to MaxPSPromptCV and a rate up to
// MaxRouterWaitKneeShare of the knee: past either, and on the first-come
// queue, the wait keeps its flag. Bucket C's fleet (Ca^2 4) on four decode
// pods, whose prefill onset sits under the share.
func TestFlowControlWait_FlagFollowsTheSwitch(t *testing.T) {
	if DefaultPriceRouterWait {
		t.Fatal("PriceRouterWait is experimental: off by default")
	}
	base := psFleet(0.0577, 4)
	base.Decode.Replicas = 4
	on := mustOnsets(t, fcFleet(base, false))
	knee := mustTTFT(t, ttftAt(t, fcFleet(base, false), 1, 0.9)).PrefillKnee
	within, past, below := on.Prefill*1.01, (MaxRouterWaitKneeShare+1)/2*knee, 0.9*on.Prefill
	if !(within < MaxRouterWaitKneeShare*knee && knee < on.Decode) {
		t.Fatalf("precondition: prefill onset %g under %g of the knee %g, under the decode onset %g", on.Prefill, MaxRouterWaitKneeShare, knee, on.Decode)
	}
	for _, c := range []struct {
		name     string
		cv       float64
		ps       bool
		priced   bool
		rate     float64
		wantFlag Flag
	}{
		{"switch off", 0.0577, true, false, within, AboveFlowControlOnset},
		{"switch on", 0.0577, true, true, within, FlowControlWaitPriced},
		{"switch on, past the share", 0.0577, true, true, past, AboveFlowControlOnset},
		{"past MaxPSPromptCV", 0.5, true, true, within, AboveFlowControlOnset},
		{"first come", 0.0577, false, true, within, AboveFlowControlOnset},
	} {
		fl := base
		fl.Prefill.PromptCV, fl.ProcessorSharing = c.cv, c.ps
		other := map[Flag]Flag{AboveFlowControlOnset: FlowControlWaitPriced, FlowControlWaitPriced: AboveFlowControlOnset}[c.wantFlag]
		for _, q := range []float64{0, 0.9} {
			got := mustTTFT(t, ttftAt(t, fcFleet(fl, c.priced), c.rate, q))
			if !got.HasFlag(c.wantFlag) || got.HasFlag(other) {
				t.Errorf("%s, q %g, above the onset: flags %v, want %s and not %s", c.name, q, got.Flags, c.wantFlag, other)
			}
			if flip := mustTTFT(t, ttftAt(t, fcFleet(fl, !c.priced), c.rate, q)); flip.Seconds != got.Seconds {
				t.Errorf("%s, q %g: the switch moved the figure, %g to %g", c.name, q, got.Seconds, flip.Seconds)
			}
			if b := mustTTFT(t, ttftAt(t, fcFleet(fl, c.priced), below, q)); b.HasFlag(AboveFlowControlOnset) || b.HasFlag(FlowControlWaitPriced) {
				t.Errorf("%s, q %g, below the onset: flags %v", c.name, q, b.Flags)
			}
		}
	}
	// Question 1 carries it from the answer's price: a target met only above
	// the onset, inside the share.
	fl := fcFleet(base, true)
	target := TTFTTarget{Seconds: mustTTFT(t, ttftAt(t, fl, within, 0.9)).Seconds, Percentile: 0.9}
	a := mustAnswer(t, rateForTTFT(t, fl, target))
	if !(a.Value >= on.Prefill) || !slices.Contains(a.Flags, FlowControlWaitPriced) || slices.Contains(a.Flags, AboveFlowControlOnset) {
		t.Errorf("rate %g r/s (onset %g), flags %v: want FlowControlWaitPriced alone", a.Value, on.Prefill, a.Flags)
	}
}

// The verdict: a TTFT above the onset flagged AboveFlowControlOnset misses
// however low its figure; FlowControlWaitPriced is read by its figure; a
// refusal past the knee or the decode onset misses, any other says nothing.
func TestTTFTMeets(t *testing.T) {
	target := TTFTTarget{Seconds: 1, Percentile: 0.9}
	for _, c := range []struct {
		name       string
		r          Result
		met, known bool
	}{
		{"under", TTFT{Seconds: 0.5}, true, true},
		{"at", TTFT{Seconds: 1}, true, true},
		{"over", TTFT{Seconds: 1.5}, false, true},
		{"held, unpriced", TTFT{Seconds: 0.5, Flags: []Flag{AboveFlowControlOnset}}, false, true},
		{"held, priced", TTFT{Seconds: 0.5, Flags: []Flag{FlowControlWaitPriced}}, true, true},
		{"held, priced, over", TTFT{Seconds: 1.5, Flags: []Flag{FlowControlWaitPriced}}, false, true},
		{"saturated", Unanswerable{Reason: Saturated}, false, true},
		{"decode onset", Unanswerable{Reason: AboveDecodeOnset}, false, true},
		{"no capacity", Unanswerable{Reason: NoPrefillCapacity}, false, false},
	} {
		met, known := TTFTMeets(c.r, target)
		if met != c.met || known != c.known {
			t.Errorf("%s: met %v known %v, want %v %v", c.name, met, known, c.met, c.known)
		}
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
// while it is below. pick is a random draw weighted by the pods' shares (the
// busiest at f/N_p), not the first pod to free: the router's own scorer is
// not modelled. With poll 0 the gate reads the current count at every event;
// with poll > 0 it reads the count as of the last poll, every poll seconds,
// and dispatches against that stale count until the next one. held counts
// the requests the router held.
func gatedStage(capL int, qt, poll float64, held *int) stageSim {
	return func(arr, size []float64, es float64, pods int, pick func() int) []float64 {
		type job struct {
			i   int
			rem float64
		}
		res := make([]float64, len(arr))
		active := make([][]job, pods)
		waiting := make([][]int, pods)
		var router []int
		waitingAll, next, done, seen := 0, 0, 0, 0
		now, tick := 0.0, poll
		open := func() bool {
			w := waitingAll
			if poll > 0 {
				w = seen
			}
			return float64(w) < qt*float64(pods)
		}
		dispatch := func() {
			for len(router) > 0 && open() {
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
			tt := math.Inf(1)
			if poll > 0 {
				tt = tick
			}
			at := min(na, tf, tt)
			for p, a := range active {
				if len(a) == 0 {
					continue
				}
				d := (at - now) / float64(len(a))
				if p == fp && tf <= na && tf <= tt {
					d = least
				}
				for j := range a {
					a[j].rem -= d
				}
			}
			now = at
			if tt < na && tt < tf {
				seen = waitingAll
				tick += poll
				dispatch()
				continue
			}
			if na <= tf {
				router = append(router, next)
				if len(router) > 1 || !open() {
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

// The gate moves the wait, it does not add to it, up to
// MaxRouterWaitKneeShare of the knee. Bucket B's fleet on four decode pods,
// each case run twice on the same seed, so on the same arrivals, prompts and
// pod draws: once with the prefill pods gated (running cap 1, 10, 64 and 256,
// the router holding at the upstream threshold of 5, its gate read live or
// every 50 ms and 200 ms) and once ungated (tandemStage, the processor-sharing
// stage the price models). Paired, the difference is the gate's own effect.
// At prompt cv 0.058 and 0.25, Poisson and Ca^2 4, six seeds, at 0.85 and
// 0.90 of the knee, the gated pods are never more than 2 % slower than the
// ungated ones at the mean, p90 and p95 (the worst seen was +1.3 %). How
// far the price itself reads from the ungated stage is
// TestPS_AgainstATandemSimulation's matter, not the gate's. Past the share
// the gate's own effect grows, and one point pins it: at 0.97 of the knee,
// cv 0.058, Ca^2 4, cap 256, a 200 ms poll, seed 2, the gated pods read over
// 10 % slower (+16.1 % seen).
func TestFlowControlWait_GatedPodsAreNoSlower(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	const n = 200000
	paired := func(t *testing.T, cv, ca2, share float64, seed uint64) (prefillPlan, float64, simSample) {
		fl := psFleet(cv, ca2)
		fl.Decode.Replicas = 4
		pl, u := fl.plan()
		if u != nil {
			t.Fatal(u)
		}
		rate := share * pl.pf.knee()
		return pl.pf, rate, simulateTandem(pl.pf, rate, 4, n, seed, tandemStage)
	}
	slower := func(g, un simSample) [3]float64 {
		return [3]float64{g.mean/un.mean - 1, g.quantile(0.9)/un.quantile(0.9) - 1, g.quantile(0.95)/un.quantile(0.95) - 1}
	}
	for _, cv := range []float64{0.0577, 0.25} {
		for _, ca2 := range []float64{1, 4} {
			t.Run(fmt.Sprintf("cv %g, Ca^2 %g", cv, ca2), func(t *testing.T) {
				t.Parallel()
				worst := math.Inf(-1)
				for _, share := range []float64{0.85, MaxRouterWaitKneeShare} {
					for seed := uint64(1); seed <= 6; seed++ {
						pf, rate, un := paired(t, cv, ca2, share, seed)
						for _, capL := range []int{1, 10, 64, 256} {
							for _, poll := range []float64{0, 0.05, 0.2} {
								var held int
								g := simulateTandem(pf, rate, 4, n, seed, gatedStage(capL, FlowControlQueueThreshold, poll, &held))
								for i, e := range slower(g, un) {
									worst = max(worst, e)
									if e > 0.02 {
										t.Errorf("%.2f of the knee, cap %d, poll %g s, seed %d (router held %.1f %%), %s: the gated pods %+.1f %% slower than ungated",
											share, capL, poll, seed, 100*float64(held)/n, [3]string{"mean", "p90", "p95"}[i], 100*e)
									}
								}
							}
						}
					}
				}
				t.Logf("worst, gated over ungated: %+.1f %%", 100*worst)
			})
		}
	}
	t.Run("past the share", func(t *testing.T) {
		t.Parallel()
		pf, rate, un := paired(t, 0.0577, 4, 0.97, 2)
		var held int
		e := slower(simulateTandem(pf, rate, 4, n, 2, gatedStage(256, FlowControlQueueThreshold, 0.2, &held)), un)
		t.Logf("0.97 of the knee, Ca^2 4, cap 256, poll 200 ms, seed 2 (router held %.1f %%): gated over ungated %+.1f / %+.1f / %+.1f %%",
			100*float64(held)/n, 100*e[0], 100*e[1], 100*e[2])
		if !(max(e[0], e[1], e[2]) > 0.1) {
			t.Errorf("past the share the gated pods read %v of ungated: the pinned slowdown is gone, revisit MaxRouterWaitKneeShare", e)
		}
	})
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
// control on. This checks the processor-sharing price near the onset, not
// the priced router wait: the router held almost nothing at these steps (at
// most 2 % of requests past 10 ms, a mean wait under 4 ms), so none of them
// validates FlowControlWaitPriced. Every step that held reads at or above
// the slowest of the three repeats' client TTFT, at the mean, p90 and p95,
// 1.04x to 4.58x, and the steps at or past the onset carry the flag the
// switch asks for: FlowControlWaitPriced inside MaxRouterWaitKneeShare of
// the knee, AboveFlowControlOnset past it and with the switch off.
func TestStaircaseR2_PSPriceReadsAboveTheClientNearTheOnset(t *testing.T) {
	for _, bucket := range []string{"B-onset", "C-onset"} {
		fl := fcFleet(r2Fleet(bucket), true)
		on := mustOnsets(t, fl)
		knee := mustTTFT(t, ttftAt(t, fl, 1, 0.9)).PrefillKnee
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
				above, inside := s.rate >= on.Prefill, s.rate <= MaxRouterWaitKneeShare*knee
				if got.HasFlag(FlowControlWaitPriced) != (above && inside) || got.HasFlag(AboveFlowControlOnset) != (above && !inside) {
					t.Errorf("%s %.2f r/s (onset %.2f, knee %.2f): flags %v", bucket, s.rate, on.Prefill, knee, got.Flags)
				}
			}
		}
	}
}

// A known failure, asserted so that a fix shows: C-onset's step at 56.6 r/s,
// 0.96 of the frozen prefill knee, is the one step of the run where the
// router really held. Over its three repeats the router queue peaked at 1839
// to 1892, its wait averaged 18.8 to 20.9 s, the client's p50 read 28.4 to
// 29.8 s and its p90 41.5 to 43.9 s (one repeat past the histogram), and 9 to
// 14 % of the step's requests failed: the fleet did not hold the rate at
// Ca^2 4. The model answers a mean of 1.33 s and a p90 of 3.58 s, over ten
// times short. The step is past MaxRouterWaitKneeShare, so the answer keeps
// AboveFlowControlOnset with the switch on: it is not priced as a wait.
func TestStaircaseR2_TheStepWithARouterWaitReadsShort(t *testing.T) {
	const rate = 56.60893235175076
	// The least of the three repeats, seconds.
	const routerWait, clientP50, clientP90 = 18.8406, 28.3652, 41.4814
	fl := fcFleet(r2Fleet("C-onset"), true)
	mean := mustTTFT(t, ttftAt(t, fl, rate, 0))
	p90 := mustTTFT(t, ttftAt(t, fl, rate, 0.9))
	t.Logf("model mean %.2f s, p90 %.2f s; the router's wait alone %.1f s, client p50 %.1f s, p90 %.1f s", mean.Seconds, p90.Seconds, routerWait, clientP50, clientP90)
	if !(10*mean.Seconds < routerWait && 10*p90.Seconds < clientP90) {
		t.Errorf("the known failure is gone: model mean %.2f s, p90 %.2f s against a router wait of %.1f s and a client p90 of %.1f s; update the TTFT page", mean.Seconds, p90.Seconds, routerWait, clientP90)
	}
	if !mean.HasFlag(AboveFlowControlOnset) || mean.HasFlag(FlowControlWaitPriced) {
		t.Errorf("flags %v: past the share the wait keeps AboveFlowControlOnset", mean.Flags)
	}
}

// A rate answer never stands where its own verdict is a miss: across fleets
// with flow control on (the worked fleet, whose decode onset binds; bucket B
// and C's on four decode pods, whose prefill onset binds; the second
// staircase's frozen B-onset and C-onset), the router's wait priced or not,
// and targets from tight to loose, every rate RateForTTFT answers meets the
// target by TTFTMeets at that rate. A cap names its bound.
func TestRateForTTFT_TheAnswerMeetsItsOwnVerdict(t *testing.T) {
	worked := workedFleet()
	worked.FlowControl = FlowControl{State: FlowControlOn, QueueThreshold: 5, KVThreshold: 0.8}
	b, c := psFleet(0.0577, 1), psFleet(0.0577, 4)
	b.Decode.Replicas, c.Decode.Replicas = 4, 4
	fleets := map[string]TTFTFleet{"worked": worked, "B": fcFleet(b, false), "C": fcFleet(c, false),
		"B-onset": fcFleet(r2Fleet("B-onset"), false), "C-onset": fcFleet(r2Fleet("C-onset"), false)}
	bounds := map[Bound]int{}
	for name, base := range fleets {
		for _, priced := range []bool{false, true} {
			fl := base
			fl.PriceRouterWait = priced
			for _, secs := range []float64{0.2, 0.5, 1, 2, 5, 30} {
				for _, q := range []float64{0, 0.9, 0.95} {
					target := TTFTTarget{Seconds: secs, Percentile: q}
					a, ok := RateForTTFT(fl, target).(Answer)
					if !ok {
						continue
					}
					at := TTFTAt(fl, a.Value, q)
					met, known := TTFTMeets(at, target)
					if !met || !known {
						t.Errorf("%s, priced %v, %g s at q %g: rate %g r/s (bound %v, flags %v) reads %v there, met %v known %v",
							name, priced, secs, q, a.Value, a.Basis.BoundBy, a.Flags, at, met, known)
					}
					if slices.Contains(a.Flags, AboveFlowControlOnset) {
						t.Errorf("%s, priced %v, %g s at q %g: rate answer flagged AboveFlowControlOnset", name, priced, secs, q)
					}
					for _, by := range a.Basis.BoundBy {
						bounds[by]++
					}
				}
			}
		}
	}
	t.Logf("bounds: %v", bounds)
	if bounds[BoundPrefillOnset] == 0 || bounds[BoundDecodeOnset] == 0 || bounds[BoundTTFT] == 0 {
		t.Errorf("not every bound reached: %v", bounds)
	}
}
