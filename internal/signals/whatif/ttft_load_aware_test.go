package whatif

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// loadAwarePrefill is three prefill pods at V_P 30,000 tok/s, 1000-token
// prompts at cv 1, f 1.3 read at a mean pod utilization of 0.4: the pooled
// knee is 90 r/s.
func loadAwarePrefill(on bool, margin float64) Prefill {
	return Prefill{
		VP: 30000, FixedLatency: 0.0126, Replicas: 3, PromptTokens: 1000, PromptCV: 1,
		Imbalance: 1.3, ImbalanceUtilization: 0.4, LoadAwareRouter: on, VPMargin: margin,
	}
}

func kneeOf(t *testing.T, p Prefill) float64 {
	t.Helper()
	a, ok := PrefillKnee(p).(Answer)
	if !ok {
		t.Fatalf("no knee: %#v", PrefillKnee(p))
	}
	return a.Value
}

// With a load-aware router and no margin the knee is the pooled one,
// N_p V_P / E[I]; off, it is that over f. With the margin it is where the
// worst pod at f(rho) fills the margined V_P.
func TestLoadAware_TheKneeIsPooled(t *testing.T) {
	relNear(t, "knee, load-aware", kneeOf(t, loadAwarePrefill(true, 0)), 90, 1e-12)
	relNear(t, "knee, off", kneeOf(t, loadAwarePrefill(false, 0)), 90/1.3, 1e-12)

	p := loadAwarePrefill(true, DefaultVPMargin)
	k := kneeOf(t, p)
	pl, u := p.check()
	if u != nil {
		t.Fatal(u)
	}
	rho := k / 90
	relNear(t, "f(rho) rho at the margined knee", pl.fAt(k)*rho, 1-DefaultVPMargin, 1e-9)
	if !(k > 90*(1-DefaultVPMargin)/1.3) || !(k < 90) {
		t.Errorf("margined knee %.6g, want between the held one %.6g and the pooled 90", k, 90*(1-DefaultVPMargin)/1.3)
	}
}

// f is held up to the utilization it was read at, and the TTFT there is the
// one with the switch off; past it f falls, to 1 at the pooled knee.
func TestLoadAware_FIsHeldBelowWhereItWasRead(t *testing.T) {
	on, off := loadAwarePrefill(true, DefaultVPMargin), loadAwarePrefill(false, DefaultVPMargin)
	plOn, _ := on.check()
	for _, rate := range []float64{5, 20, 35, 36} { // rho up to 0.4 is 36 r/s
		if got := plOn.fAt(rate); got != 1.3 {
			t.Errorf("f at %g r/s = %g, want 1.3 held", rate, got)
		}
		a := mustTTFT(t, TTFTAt(TTFTFleet{Prefill: on}, rate, 0.9))
		b := mustTTFT(t, TTFTAt(TTFTFleet{Prefill: off}, rate, 0.9))
		if a.Seconds != b.Seconds {
			t.Errorf("TTFT p90 at %g r/s: %.9g on, %.9g off; want equal below rho_m", rate, a.Seconds, b.Seconds)
		}
	}
	relNear(t, "f at rho 0.7", plOn.fAt(63), 1+0.3*0.3/0.6, 1e-12)
	if got := plOn.fAt(90); got != 1 {
		t.Errorf("f at the pooled knee = %g, want 1", got)
	}
}

// riseProblem sweeps fleet on's TTFT p90 over 199 rates up to its prefill
// knee and says where it first falls with the rate, or reads above off's,
// the same fleet with f held: "" where it does neither.
func riseProblem(t *testing.T, on, off TTFTFleet) string {
	t.Helper()
	k := kneeOf(t, on.Prefill)
	prev := 0.0
	for i := 1; i < 200; i++ {
		rate := k * float64(i) / 200
		a := mustTTFT(t, TTFTAt(on, rate, 0.9)).Seconds
		if !(a >= prev) {
			return fmt.Sprintf("TTFT p90 falls at %.4g r/s: %.6g after %.6g", rate, a, prev)
		}
		prev = a
		if b, ok := TTFTAt(off, rate, 0.9).(TTFT); ok && a > b.Seconds*(1+1e-12) {
			return fmt.Sprintf("at %.4g r/s load-aware %.6g above held %.6g", rate, a, b.Seconds)
		}
	}
	return ""
}

// sweepFleets are the fleets riseProblem sweeps, load-aware and held, by
// queue: the first-come queue; processor sharing by the model's rule at a
// spread it prices alone (cv 0.1) and one past MaxPSPromptCV, where the
// price is the larger of the two queues (cv 1); and processor sharing by the
// engine's word on 4 slots, alone below them and the larger of the two past
// them (PSSlotsExceeded) on the way to the knee.
func sweepFleets() map[string][2]TTFTFleet {
	out := map[string][2]TTFTFleet{}
	for name, c := range map[string]struct {
		ps    bool
		cv    float64
		queue QueueChoice
	}{
		"first come":                     {false, 1, QueueChoice{}},
		"processor sharing, cv 0.1":      {true, 0.1, QueueChoice{}},
		"processor sharing, cv 1":        {true, 1, QueueChoice{}},
		"processor sharing, by 4 slots":  {true, 1, QueueChoice{Model: QueueProcessorSharing, Slots: 4}},
		"processor sharing, by 64 slots": {true, 1, QueueChoice{Model: QueueProcessorSharing, Slots: 64}},
	} {
		var pair [2]TTFTFleet
		for i, on := range []bool{true, false} {
			p := loadAwarePrefill(on, DefaultVPMargin)
			p.PromptCV, p.Queue = c.cv, c.queue
			pair[i] = TTFTFleet{Prefill: p, ProcessorSharing: c.ps}
		}
		out[name] = pair
	}
	return out
}

// Past rho_m the price is never above the held one, and it still rises with
// the rate up to the knee, so RateForTTFT's bisection holds: with the
// first-come queue and with processor sharing, alone and capped by the
// first-come queue.
func TestLoadAware_TTFTRisesWithTheRate(t *testing.T) {
	for name, c := range sweepFleets() {
		if why := riseProblem(t, c[0], c[1]); why != "" {
			t.Errorf("%s: %s", name, why)
		}
	}
}

// The processor-sharing sweeps price processor sharing: alone (cv 0.1, 64
// slots, 4 slots at a fifth of the knee) another figure than the first-come
// queue's, capped (cv 1) at least it, and on 4 slots near the knee the
// larger of the two, flagged PSSlotsExceeded.
func TestLoadAware_TheSweepsPriceProcessorSharing(t *testing.T) {
	fleets := sweepFleets()
	k := kneeOf(t, fleets["first come"][0].Prefill)
	firstCome := func(fl TTFTFleet, rate float64) TTFT {
		fl.ProcessorSharing, fl.Prefill.Queue = false, QueueChoice{}
		return mustTTFT(t, TTFTAt(fl, rate, 0.9))
	}
	for _, c := range []struct {
		name  string
		share float64
		alone bool
	}{
		{"processor sharing, cv 0.1", 0.5, true}, {"processor sharing, by 64 slots", 0.5, true},
		{"processor sharing, by 4 slots", 0.2, true}, {"processor sharing, cv 1", 0.5, false},
	} {
		fl, rate := fleets[c.name][0], c.share*k
		ps, fc := mustTTFT(t, TTFTAt(fl, rate, 0.9)), firstCome(fl, rate)
		if c.alone && ps.Seconds == fc.Seconds {
			t.Errorf("%s: %.6g s at %.4g r/s is the first-come price", c.name, ps.Seconds, rate)
		}
		if !c.alone && ps.Seconds < fc.Seconds {
			t.Errorf("%s: %.6g s at %.4g r/s, under the first-come %.6g s", c.name, ps.Seconds, rate, fc.Seconds)
		}
	}
	slots := fleets["processor sharing, by 4 slots"][0]
	if r := mustTTFT(t, TTFTAt(slots, 0.99*kneeOf(t, slots.Prefill), 0.9)); !r.HasFlag(PSSlotsExceeded) {
		t.Errorf("4 slots near the knee: flags %v, want PSSlotsExceeded", r.Flags)
	}
}

// With the shipped fall, every f a plan admits (1 < f <= N_p), read at any
// rho_m up to past its cap, at any margin, prices a finite TTFT at every rate
// below the knee on each queue: f(rho) rho stays under 1 - margin there, so
// no wait tail is lost. A Saturated refusal with a NaN decay rate is an f
// that breaks that, as the one TestLoadAware_TheSweepsCatchANonMonotoneF
// injects does.
func TestLoadAware_EveryFPricesBelowTheKnee(t *testing.T) {
	for _, f := range []float64{1.01, 1.3, 1.8, 2.5, 3} {
		for _, rm := range []float64{0.01, 0.4, 0.9, 0.99} {
			for _, margin := range []float64{0, DefaultVPMargin, MaxVPMargin} {
				for name, c := range sweepFleets() {
					fl := c[0]
					fl.Prefill.Imbalance, fl.Prefill.ImbalanceUtilization, fl.Prefill.VPMargin = f, rm, margin
					pl, u := fl.Prefill.check()
					if u != nil {
						t.Fatalf("%s f %g rho_m %g margin %g: %v", name, f, rm, margin, u)
					}
					k := kneeOf(t, fl.Prefill)
					for i := 1; i <= 50; i++ {
						rate := k * min(float64(i)/50, 0.999)
						if g := pl.fAt(rate); !(g >= 1 && g <= f) {
							t.Errorf("%s f %g rho_m %g margin %g: f %g at %.4g r/s", name, f, rm, margin, g, rate)
						}
						if r, ok := TTFTAt(fl, rate, 0.9).(TTFT); !ok || !Finite(r.Seconds) {
							t.Errorf("%s f %g rho_m %g margin %g at %.4g r/s (knee %.4g): %#v", name, f, rm, margin, rate, k, TTFTAt(fl, rate, 0.9))
						}
					}
				}
			}
		}
	}
}

// Every sweep fails on an f(rho) that does not fall smoothly: f held at even
// twentieths of the pooled utilization and 1 at odd ones. It is never above
// the held f, so it is the fall with the rate the sweeps catch. Never
// t.Parallel: it swaps loadAwareFall for the package, and a sequential test
// ends, Cleanup included, before any parallel one resumes.
func TestLoadAware_TheSweepsCatchANonMonotoneF(t *testing.T) {
	saved := loadAwareFall
	t.Cleanup(func() { loadAwareFall = saved })
	loadAwareFall = func(f, rho, _ float64) float64 {
		if int(rho*20)%2 == 1 {
			return 1
		}
		return f
	}
	for name, c := range sweepFleets() {
		if why := riseProblem(t, c[0], c[1]); !strings.Contains(why, "falls") {
			t.Errorf("%s: the sweep passed a non-monotone f (%q)", name, why)
		}
	}
}

// A pod hotter than the router can steer away from: f 1.8 read at 0.5 has
// an excess of 0.8 against 0.5 of idle capacity, and saturates first, at
// rho 0.625 (k = 1.6: the smaller root of 1.6 rho^2 - 2.6 rho + 1).
func TestLoadAware_AHotPodSaturatesBeforeThePooledKnee(t *testing.T) {
	p := loadAwarePrefill(true, 0)
	p.Imbalance, p.ImbalanceUtilization = 1.8, 0.5
	relNear(t, "knee", kneeOf(t, p), 90*0.625, 1e-12)
	// Read where it already saturates the busiest pod: the held knee.
	p.Imbalance, p.ImbalanceUtilization = 2.5, 0.5
	relNear(t, "knee held", kneeOf(t, p), 90/2.5, 1e-12)
}

// No change where there is no imbalance to fall: one pod, f 1, or the
// switch off.
func TestLoadAware_NothingToFall(t *testing.T) {
	one := loadAwarePrefill(true, 0)
	one.Replicas, one.Imbalance = 1, 0
	relNear(t, "one pod", kneeOf(t, one), 30, 1e-12)
	even := loadAwarePrefill(true, 0)
	even.Imbalance = 1
	relNear(t, "f 1", kneeOf(t, even), 90, 1e-12)
	unknown := loadAwarePrefill(true, 0)
	unknown.ImbalanceUtilization = 0
	pl, _ := unknown.check()
	relNear(t, "rho_m unknown: f held", pl.fAt(45), 1.3, 1e-12)
	relNear(t, "rho_m unknown: the knee held", kneeOf(t, unknown), 90/1.3, 1e-12)
}

// rho_m is read at the V_P priced: a longer prompt than V_P was read at
// rescales V_P down (VPScaledToPrompt), and the same load fills more of it.
func TestLoadAware_RhoMAtTheShapedVP(t *testing.T) {
	p := loadAwarePrefill(true, 0)
	p.VPPromptTokens, p.PromptTokens, p.CurveSlope, p.CurveCurvature = 1000, 2000, 1e-5, 1e-8
	pl, u := p.check()
	if u != nil {
		t.Fatal(u)
	}
	if !(pl.vp < p.VP) {
		t.Fatalf("precondition: V_P %g not rescaled below %g", pl.vp, p.VP)
	}
	relNear(t, "rho_m", pl.rhoM(), 0.4*p.VP/pl.vp, 1e-12)
}

// ImbalanceUtilizationAt reads the load at the count P1 learned f at, else
// the load F is read at, over V_P, held below 1; 0 with none.
func TestImbalanceUtilizationAt(t *testing.T) {
	est := prefill.Estimate{FLoad: 3000, FByPods: map[int]float64{2: 1.1}, FLoadByPods: map[int]float64{2: 6000}}
	relNear(t, "learned count", ImbalanceUtilizationAt(est, 2, 10000), 0.6, 1e-12)
	relNear(t, "another count", ImbalanceUtilizationAt(est, 3, 10000), 0.3, 1e-12)
	relNear(t, "over another V_P", ImbalanceUtilizationAt(est, 3, 20000), 0.15, 1e-12)
	est.FLoadByPods[2] = 20000
	relNear(t, "held below 1", ImbalanceUtilizationAt(est, 2, 10000), MaxImbalanceUtilization, 1e-12)
	if got := ImbalanceUtilizationAt(prefill.Estimate{}, 2, 10000); got != 0 {
		t.Errorf("none read: %g", got)
	}
	if got := ImbalanceUtilizationAt(prefill.Estimate{FLoad: 1}, 2, 0); got != 0 || math.IsNaN(got) {
		t.Errorf("no V_P: %g", got)
	}
}

// PrefillLoadAt is f at the rate and the pooled utilization, whose product
// is the busiest pod's: off, the plan's f; on, the f that has fallen.
func TestPrefillLoadAt(t *testing.T) {
	f, pooled, ok := PrefillLoadAt(loadAwarePrefill(false, 0), 63)
	if !ok || f != 1.3 {
		t.Fatalf("off: f %g ok %v", f, ok)
	}
	relNear(t, "pooled", pooled, 0.7, 1e-12)
	f, pooled, ok = PrefillLoadAt(loadAwarePrefill(true, 0), 63)
	if !ok {
		t.Fatal("on: not priced")
	}
	relNear(t, "f on", f, 1.15, 1e-12)
	relNear(t, "busiest pod", f*pooled, 1.15*0.7, 1e-12)
}
