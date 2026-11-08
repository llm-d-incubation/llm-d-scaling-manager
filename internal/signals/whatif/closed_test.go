package whatif

import (
	"math"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// olsSweep is the worked example of docs/proposals/slo-capacity-planner.md
// ("Worked example: ols-sweep"): the ols-sweep golden's last published line,
// C = 131,072 tokens, batch cap 64 on one rank, one decode replica, IL = OL =
// 1000 with no prefix hits, so KVreq = 1000 + 1000/2.
func olsSweep() Decode {
	return Decode{
		Line:           itl.Model{A: 0.004438, B: 0.001557},
		Band:           DefaultBand(),
		CapacityTokens: 131072,
		KVPerRequest:   1500,
		OutputTokens:   1000,
		ReplicaCap:     64,
		Replicas:       1,
	}
}

func mustAnswer(t *testing.T, r Result) Answer {
	t.Helper()
	a, ok := r.(Answer)
	if !ok {
		t.Fatalf("want an answer, got %#v", r)
	}
	return a
}

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if !(math.Abs(got-want) <= tol) {
		t.Errorf("%s = %.6g, want %.6g within %g", what, got, want, tol)
	}
}

// The page's figures, which two independent scripts reproduced, to the
// precision the page prints them at.
func TestWorkedExample_ReplicasToSLO(t *testing.T) {
	d := olsSweep()
	knee := mustAnswer(t, KneeRate(d))
	near(t, "knee", knee.Value, 13.31, 0.005)
	near(t, "batch-cap fill", knee.Basis.Fill, 0.732, 0.0005)
	if len(knee.Basis.BoundBy) != 1 || knee.Basis.BoundBy[0] != BoundBatchCap {
		t.Errorf("knee bound by %v, want the batch cap (0.732 is below the band top 0.80)", knee.Basis.BoundBy)
	}

	slo := SLOAtReplicas(d, ReplicasQuery{Headroom: 0.85,
		PercentileTarget: &TPOTTarget{Percentile: 0.9, Ratio: TPOTRatio{Value: 1.3, Assumed: true}}})
	rate := mustAnswer(t, slo.Rate)
	near(t, "operating rate", rate.Value, 11.32, 0.005)
	near(t, "fill at the operating rate", rate.Basis.Fill, 0.474, 0.0005)
	mean := mustAnswer(t, slo.MeanTPOT)
	near(t, "mean TPOT", mean.Value, 0.00366, 0.000005)
	near(t, "TPOT floor", mean.Basis.Floor, 0.00222, 0.000005)
	p90 := mustAnswer(t, slo.PercentileTPOT)
	near(t, "TPOT p90", p90.Value, 0.00476, 0.000005)
	if !p90.HasFlag(AssumedTPOTRatio) {
		t.Errorf("an assumed R_q is not flagged: %v", p90.Flags)
	}
}

func TestWorkedExample_SLOToRate(t *testing.T) {
	d := olsSweep()
	a := mustAnswer(t, rateFor(d, TPOTTarget{Seconds: 0.004}, 0.85))
	near(t, "TPOT alone", a.Basis.Uncapped, 12.03, 0.005)
	near(t, "capped rate", a.Value, 11.32, 0.005)
	// The capped answer stands on the operating point's own fill, not the
	// target's 0.551.
	near(t, "fill at the cap", a.Basis.Fill, 0.474, 0.0005)
	if len(a.Basis.BoundBy) != 1 || a.Basis.BoundBy[0] != BoundOperatingPoint {
		t.Errorf("bound by %v, want the operating-point cap", a.Basis.BoundBy)
	}
	// Below the cap: 3.0 ms inverts to 9.47 r/s, and the fill there gives
	// back 3.00 ms.
	b := mustAnswer(t, rateFor(d, TPOTTarget{Seconds: 0.003}, 0.85))
	near(t, "rate at 3 ms", b.Value, 9.47, 0.005)
	if len(b.Basis.BoundBy) != 1 || b.Basis.BoundBy[0] != BoundTPOT {
		t.Errorf("bound by %v, want TPOT", b.Basis.BoundBy)
	}
	back := mustAnswer(t, SLOAtReplicas(d, ReplicasQuery{Rate: b.Value}).MeanTPOT)
	near(t, "TPOT back at 9.47 r/s", back.Value, 0.003, 1e-12)
}

// The page: "the wait probability is 0.032 at 0.85 of the knee and 0.131 at
// 0.90, with a p90 wait of 203 ms there".
func TestWorkedExample_Admission(t *testing.T) {
	d := olsSweep()
	knee := mustAnswer(t, KneeRate(d)).Value
	at85, ok := DecodeAdmission(d, 0.85*knee).(Admission)
	if !ok {
		t.Fatal("no admission at 0.85 of the knee")
	}
	near(t, "wait probability at 0.85", at85.WaitProbability, 0.032, 0.0005)
	at90 := DecodeAdmission(d, 0.90*knee).(Admission)
	near(t, "wait probability at 0.90", at90.WaitProbability, 0.131, 0.0005)
	near(t, "p90 wait at 0.90", at90.WaitQuantile(0.9), 0.203, 0.0005)
	if len(at90.Flags) != 0 || at90.Slots != 64 {
		t.Errorf("slots %d flags %v, want the batch cap's 64 unflagged", at90.Slots, at90.Flags)
	}
}

// The chain at a constant completion rate per slot is M/M/c, and its wait
// probability is Erlang C, computed here independently by the Erlang B
// recursion.
func TestBirthDeathAtConstantRateIsErlangC(t *testing.T) {
	for _, c := range []int{1, 2, 8, 64, 512, 4096} {
		for _, rho := range []float64{0.05, 0.5, 0.85, 0.99} {
			const mu1 = 0.7
			lam := rho * float64(c) * mu1
			got, ok := birthDeath(lam, func(m int) float64 { return float64(m) * mu1 }, c)
			if !ok {
				t.Fatalf("c=%d rho=%g: no chain", c, rho)
			}
			want := erlangC(c, lam/mu1)
			// Relative: at a low load the wait probability is far below 1
			// (2e-7 at c = 64 and rho = 0.5), where an absolute 1e-9 would
			// pass anything.
			if want < 1e-280 {
				// Underflowed in the reference: the chain must agree it is nil.
				if got.wait > 1e-270 {
					t.Errorf("c=%d rho=%g: wait probability %g, Erlang C underflows", c, rho, got.wait)
				}
				continue
			}
			if math.Abs(got.wait-want) > 1e-9*want {
				t.Errorf("c=%d rho=%g: wait probability %.12g, Erlang C %.12g", c, rho, got.wait, want)
			}
			near(t, "decay", got.decay, float64(c)*mu1-lam, 1e-9)
		}
	}
}

func erlangC(c int, a float64) float64 {
	b := 1.0
	for n := 1; n <= c; n++ {
		b = a * b / (float64(n) + a*b)
	}
	return float64(c) * b / (float64(c) - a*(1-b))
}
