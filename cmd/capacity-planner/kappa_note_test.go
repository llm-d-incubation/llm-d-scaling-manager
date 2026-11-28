package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

func kappaPtr(k float64) *float64 { return &k }

// The note names each figure past KappaNoteAbove, and only those.
func TestKappaNote(t *testing.T) {
	if got := kappaNote("", kappaAt{"the operating rate", kappaPtr(1.3)}, kappaAt{"the knee", nil},
		kappaAt{"at the line", kappaPtr(whatif.KappaNoteAbove)}); got != "" {
		t.Errorf("note %q with no kappa past %g, want none", got, whatif.KappaNoteAbove)
	}
	got := kappaNote("  ", kappaAt{"the operating rate", kappaPtr(6.14)}, kappaAt{"the knee", kappaPtr(9)}, kappaAt{"2x", kappaPtr(2)})
	want := "  condition: kappa 6.1 at the operating rate, 9.0 at the knee, past 4: a 1% error in the ITL line's slope moves the fill " +
		"and the TPOT there about kappa%, so these figures rest mostly on the slope\n"
	if got != want {
		t.Errorf("note\n%q\nwant\n%q", got, want)
	}
}

// The worked example's figures carry kappa in the JSON: the knee at its
// rate, the operating rate and both TPOTs at theirs, each A*k/B with no
// HeldBefore, burst or batch term; question 1's rate, an inverse figure,
// carries none. The worked example is well conditioned, so its text has
// no note.
func TestKappa_OnTheWorkedExample(t *testing.T) {
	args := slices.Concat([]string{"replicas-to-slo", "--recording", olsSweep, "--tpot-percentile", "90", "--assume-tpot-ratio", "1.3"}, workedShape)
	out, code := runCmd(t, append(slices.Clone(args), "-o", "json")...)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	w := decodeOutput(t, out)
	in, r := w.Roles[0].Inputs, w.Roles[0].Results
	for name, res := range map[string]*wireResult{"knee": r.Knee, "operating rate": r.OperatingRate, "mean TPOT": r.MeanTPOT, "TPOT p90": r.PercentileTPOT} {
		a := res.Answer
		if a.Kappa == nil {
			t.Fatalf("%s: no kappa", name)
		}
		near(t, name+" kappa", *a.Kappa, in.Line.A*a.Basis.Fill/in.Line.B, 1e-9)
	}
	near(t, "operating kappa", *r.OperatingRate.Answer.Kappa, 1.35, 0.01)
	if *r.MeanTPOT.Answer.Kappa != *r.OperatingRate.Answer.Kappa {
		t.Errorf("mean TPOT kappa %g, want the operating rate's %g", *r.MeanTPOT.Answer.Kappa, *r.OperatingRate.Answer.Kappa)
	}
	text, _ := runCmd(t, args...)
	if strings.Contains(text, "condition: kappa") {
		t.Errorf("the worked example (kappa under %g) has a note:\n%s", whatif.KappaNoteAbove, text)
	}

	out, _ = runCmd(t, slices.Concat([]string{"slo-to-rate", "--recording", olsSweep, "--tpot", "4ms", "-o", "json"}, workedShape)...)
	if k := decodeOutput(t, out).Roles[0].Results.Rate.Answer.Kappa; k != nil {
		t.Errorf("slo-to-rate's rate carries kappa %g: an inverse figure is not amplified by the loop", *k)
	}
}

// The 0.6B staircase is a high-gain engine (B about 1 ms): its knee reads
// a kappa past 4, and both texts that print forward decode figures say so,
// replicas-to-slo's for the knee and at-replicas' for the decode knee.
func TestKappa_NoteOnAHighGainLine(t *testing.T) {
	out, code := runCmd(t, "replicas-to-slo", "--recording", stairBOn, "-o", "json")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	k := decodeOutput(t, out).Roles[0].Results.Knee.Answer.Kappa
	if k == nil || !(*k > whatif.KappaNoteAbove) {
		t.Fatalf("knee kappa %v, want past %g", k, whatif.KappaNoteAbove)
	}
	text, _ := runCmd(t, "replicas-to-slo", "--recording", stairBOn)
	if !strings.Contains(text, "condition: kappa") || !strings.Contains(text, "at the knee") {
		t.Errorf("replicas-to-slo: no kappa note at the knee:\n%s", text)
	}
	text, _ = runCmd(t, atStair()...)
	if !strings.Contains(text, "  condition: kappa") || !strings.Contains(text, "at the decode knee") {
		t.Errorf("at-replicas: no kappa note at the decode knee:\n%s", text)
	}
}
