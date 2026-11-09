package whatif

import (
	"math"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The first repeat of the TTFT staircase (docs/plans/analyzers/ttft-staircase-repeat.md;
// the TTFT page, "As built: processor sharing"), replayed: the fleet each
// bucket was frozen at, and what the client measured at each step. The
// recording is not committed here, so its figures are literals: stairFleet is
// the frozen file's fleet to the bit (TestStaircaseR1_TheFixtureIsTheFrozenFleet
// holds that), and the ladders are the client's p90 and p95 per step, in
// milliseconds, over each step's judged window.

// stairFleet is a bucket's frozen fleet, flow control off, on the first-come
// model: A is 1000-token prompts, B 8000, C B at Ca^2 4, and B-onset B on four
// decode pods.
func stairFleet(bucket string) TTFTFleet {
	prompt, output, kv, ca2, decode := 8000.0, 200.0, 8126.294768534018, 0.0, 2
	switch bucket {
	case "A":
		prompt, output, kv = 1000, 1000, 1631.4738426700906
	case "C":
		ca2 = 4
	case "B-onset":
		decode = 4
	}
	return TTFTFleet{
		Prefill: Prefill{
			VP: 289923.64, FixedLatency: 0.009016663648254916, Imbalance: 1.232429351816981, Replicas: 2,
			PromptTokens: prompt, PromptCV: 0.0577, ArrivalCV2: ca2, TransferSecondsPerToken: 2.541210992753907e-06,
			VPPromptTokens: 8003.553867780625, VPMargin: DefaultVPMargin,
		},
		Decode: &Decode{
			Line: itl.Model{A: 0.04154137591367684, B: 0.001129303622146004}, Band: FillBand{Lo: 0.15, Hi: 0.8},
			CapacityTokens: 1162240, KVPerRequest: kv, OutputTokens: output, ReplicaCap: 256, Replicas: decode, Tier: "held",
		},
		DecodeTerm: &DecodeTerm{
			Engine:  PromptLine{Base: -0.001740079349918086, PerToken: 5.615532299312609e-06},
			PerStep: 6.074839666585669,
			Hop:     PromptLine{Base: 0.005707023644712073, PerToken: 1.1166174203928517e-06},
		},
		FlowControl: FlowControl{State: FlowControlOff},
	}
}

// stairStep is one step of a phase: the rate asked, the client's p90 and
// p95 in ms, and void where the report voided it (it sent more or less than
// asked, or sat under the noise floor).
type stairStep struct {
	rate, p90, p95 float64
	void           bool
}

// stairPhase is a phase of the run: its bucket, the operating rate, the lower
// knee (where the last step sat, held), and its steps.
type stairPhase struct {
	name, bucket string
	op, knee     float64
	frozen       [2]float64 // the frozen p90 and p95 targets, seconds
	steps        []stairStep
}

var stairPhases = []stairPhase{
	{"A off", "A", 27.102727684919522, 31.88556198225826, [2]float64{0.05688341124429633, 0.05797671551977093}, []stairStep{
		{12.754, 44.6, 47.5, true}, {19.131, 48.0, 50.9, false}, {23.914, 52.5, 55.9, false}, {25.508, 55.0, 58.3, false},
		{27.103, 57.6, 61.0, false}, {28.697, 61.6, 64.9, false}, {30.291, 65.1, 69.8, false}, {31.886, 68.1, 71.9, true}}},
	{"A on", "A", 27.102727684919522, 31.88556198225826, [2]float64{0.05688341124429633, 0.05797671551977093}, []stairStep{
		{12.754, 43.8, 46.7, false}, {19.131, 52.9, 64.6, false}, {23.914, 53.2, 56.4, false}, {25.508, 55.6, 58.7, false},
		{27.103, 58.4, 61.5, false}, {28.697, 61.6, 65.2, false}, {30.291, 64.8, 68.4, false}, {31.886, 68.1, 71.7, true}}},
	{"B off", "B", 28.299340940257448, 33.29334228265582, [2]float64{0.1817828131854907, 0.19634589242743325}, []stairStep{
		{13.317, 130.1, 152.1, false}, {19.976, 161.3, 180.9, false}, {24.970, 184.7, 204.5, false}, {26.635, 190.6, 213.7, false},
		{28.299, 203.6, 224.5, false}, {29.964, 213.9, 239.1, false}, {31.629, 221.3, 249.2, false}, {33.293, 233.3, 265.6, true}}},
	{"B on", "B", 28.299340940257448, 33.29334228265582, [2]float64{0.1817828131854907, 0.19634589242743325}, []stairStep{
		{13.317, 128.1, 143.6, false}, {19.976, 160.0, 180.4, false}, {24.970, 183.2, 204.0, false}, {26.635, 196.9, 218.9, false},
		{28.299, 203.6, 225.6, false}, {29.964, 214.7, 239.9, false}, {31.629, 223.1, 250.7, false}, {33.293, 231.0, 262.4, true}}},
	{"B-onset on", "B-onset", 47.490214947178245, 55.87084111432735, [2]float64{0.3299886941957555, 0.3937969920490114}, []stairStep{
		{39.110, 229.4, 266.1, false}, {47.490, 299.4, 356.1, false}, {51.401, 336.1, 392.0, false}, {53.636, 363.4, 431.4, false},
		{54.195, 393.4, 465.4, false}, {55.871, 404.1, 485.0, true}}},
	{"C off", "C", 28.299340940257448, 33.29334228265582, [2]float64{0.32575719610323683, 0.38386454157907585}, []stairStep{
		{13.317, 152.9, 171.5, false}, {19.976, 196.0, 221.5, false}, {24.970, 229.2, 260.9, false}, {26.635, 246.8, 283.6, false},
		{28.299, 270.1, 311.0, true}, {29.964, 286.5, 332.4, false}, {31.629, 301.6, 349.1, true}, {33.293, 332.0, 386.8, true}}},
}

// crossing is where the client's ladder crosses target (ms) at percentile i
// (0 p90, 1 p95), over the steps not void: [lo, hi] in r/s, a point where two
// steps bracket it and an interval where the ladder does not (under its first
// step, or over its last and so up to the knee, where the last step held).
func (ph stairPhase) crossing(target float64, i int) (lo, hi float64) {
	var prevRate, prevV float64
	first := true
	for _, s := range ph.steps {
		if s.void {
			continue
		}
		v := [2]float64{s.p90, s.p95}[i]
		if v > target {
			if first {
				return 0, s.rate
			}
			x := prevRate + (target-prevV)/(v-prevV)*(s.rate-prevRate)
			return x, x
		}
		prevRate, prevV, first = s.rate, v, false
	}
	return prevRate, ph.knee
}

// ratio is the gate's ratio, the predicted rate over the measured: an interval
// where the crossing is.
func (ph stairPhase) ratio(target float64, i int) (lo, hi float64) {
	x0, x1 := ph.crossing(target, i)
	return ph.op / x1, ph.op / x0
}

// The staircase's mark: the predicted rate between 0.75 and 1.10 of the
// measured (hack/benchmark/staircase_lib.py).
const stairMarkLow, stairMarkHigh = 0.75, 1.10

// The fixture is the frozen fleet: on the first-come model each bucket's
// TTFT at its operating rate is the frozen target, to the bit printed.
func TestStaircaseR1_TheFixtureIsTheFrozenFleet(t *testing.T) {
	for _, ph := range stairPhases {
		for i, q := range []float64{0.9, 0.95} {
			got := psTTFT(t, stairFleet(ph.bucket), ph.op, q).Seconds
			nearRel(t, ph.name+" frozen target", got, ph.frozen[i], 1e-9)
		}
	}
}

// On the processor-sharing model, bucket B's TTFT at every step of both
// phases is within 10 % of the client's, at p90 and p95, through 0.9 of the
// knee. At the step at 0.95 of the knee the decode admission wait, which this
// model does not change, comes in (38.8 ms mean) and the union bound reads
// high: there it may read over, not under. The first-come model, the switch
// off, reads p90 11-14 % short in the middle of the ladder, out of the band:
// the control.
func TestStaircaseR1_BucketBStepsReadAsTheClient(t *testing.T) {
	const band = 0.10
	offOut := 0
	for _, ph := range stairPhases[2:4] {
		on, off := stairFleet(ph.bucket), stairFleet(ph.bucket)
		on.ProcessorSharing = true
		for _, s := range ph.steps {
			if s.void {
				continue
			}
			for i, q := range []float64{0.9, 0.95} {
				want := [2]float64{s.p90, s.p95}[i]
				got := 1e3 * psTTFT(t, on, s.rate, q).Seconds
				old := 1e3 * psTTFT(t, off, s.rate, q).Seconds
				e, eOld := got/want-1, old/want-1
				t.Logf("%s %.3f r/s p%d: client %.1f ms, processor sharing %.1f (%+.1f %%), first-come %.1f (%+.1f %%)",
					ph.name, s.rate, int(100*q), want, got, 100*e, old, 100*eOld)
				switch {
				case s.rate <= 0.9*ph.knee && math.Abs(e) > band:
					t.Errorf("%s %.3f r/s p%d: %+.1f %%, outside +-%.0f %%", ph.name, s.rate, int(100*q), 100*e, 100*band)
				case s.rate > 0.9*ph.knee && e < -band:
					t.Errorf("%s %.3f r/s p%d: %+.1f %%, under by more than %.0f %%", ph.name, s.rate, int(100*q), 100*e, 100*band)
				}
				if s.rate <= 0.9*ph.knee && math.Abs(eOld) > band {
					offOut++
				}
			}
		}
	}
	if offOut == 0 {
		t.Error("the first-come model read every step of bucket B within the band: the control no longer fails")
	}
	t.Logf("first-come readings outside +-%.0f %% through 0.9 of the knee: %d", 100*band, offOut)
}

// The gate, as the staircase report reads it: the target is the model's TTFT
// at the operating rate, the measured rate is where the client's ladder
// crosses it, and the ratio of the two must sit in the mark. On the
// processor-sharing model bucket B (both phases), A off and B-onset are in the
// mark at p90 and p95; B-onset's crossing lies past its last step, so its
// ratio is an interval up to the knee, in the mark at both ends. The
// first-come model over-promises bucket B, at 1.15 to 1.22: the control.
// Reported, not asserted: A on's p95, which a one-minute transient at its
// second step decides, and C, whose fifth step is void.
func TestStaircaseR1_TheGateOnTheOperatingRate(t *testing.T) {
	asserted := map[string]bool{"A off": true, "B off": true, "B on": true, "B-onset on": true}
	for _, ph := range stairPhases {
		on, off := stairFleet(ph.bucket), stairFleet(ph.bucket)
		on.ProcessorSharing = true
		for i, q := range []float64{0.9, 0.95} {
			target := 1e3 * psTTFT(t, on, ph.op, q).Seconds
			lo, hi := ph.ratio(target, i)
			oldLo, oldHi := ph.ratio(1e3*psTTFT(t, off, ph.op, q).Seconds, i)
			t.Logf("%s p%d: target %.1f ms, ratio %.3f to %.3f; first-come target %.1f ms, ratio %.3f to %.3f",
				ph.name, int(100*q), target, lo, hi, 1e3*psTTFT(t, off, ph.op, q).Seconds, oldLo, oldHi)
			if asserted[ph.name] && (lo < stairMarkLow || hi > stairMarkHigh) {
				t.Errorf("%s p%d: ratio %.3f to %.3f, out of the mark [%g, %g]", ph.name, int(100*q), lo, hi, stairMarkLow, stairMarkHigh)
			}
			if ph.bucket == "B" && ph.name != "" && !(oldLo > stairMarkHigh) {
				t.Errorf("%s p%d: the first-come model's ratio %.3f is in the mark: the control no longer fails", ph.name, int(100*q), oldLo)
			}
		}
	}
}

// Bucket A's operating point is decode's: the PS stages move its TTFT by under
// 1 ms, and it reads within 5 % of the client off. B-onset's is above the
// prefill onset, with the V_P margin whole (rho_w 0.85): the model reads high
// there, never low.
func TestStaircaseR1_TheOperatingPoints(t *testing.T) {
	for _, c := range []struct {
		phase  int
		lo, hi float64
	}{
		{0, -0.05, 0.05},
		{4, 0, 0.60},
	} {
		ph := stairPhases[c.phase]
		on := stairFleet(ph.bucket)
		on.ProcessorSharing = true
		var at stairStep
		for _, s := range ph.steps {
			if math.Abs(s.rate-ph.op) < 1e-3 {
				at = s
			}
		}
		for i, q := range []float64{0.9, 0.95} {
			want := [2]float64{at.p90, at.p95}[i]
			got := 1e3 * psTTFT(t, on, ph.op, q).Seconds
			e := got/want - 1
			t.Logf("%s at %.3f r/s p%d: client %.1f ms, model %.1f ms (%+.1f %%)", ph.name, ph.op, int(100*q), want, got, 100*e)
			if e < c.lo || e > c.hi {
				t.Errorf("%s p%d: %+.1f %%, outside [%+.0f %%, %+.0f %%]", ph.name, int(100*q), 100*e, 100*c.lo, 100*c.hi)
			}
		}
	}
}
