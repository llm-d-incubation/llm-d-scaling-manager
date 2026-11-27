package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
)

// The bands' intercept draw is a replay switch (saturation.SwitchITLBandIntercept,
// issue llm-d-incubation/llm-d-scaling-manager#307), on for the planner. Off,
// the worked answer's mean-TPOT band is the one the planner printed before
// it, to the last bit, and no band names its blocks; on, the band widens
// about the same figure and names them.
func TestBandInterceptSwitch(t *testing.T) {
	args := slices.Concat([]string{"replicas-to-slo", "--recording", olsSweep,
		"--model", "model-02", "--tpot-percentile", "90", "--assume-tpot-ratio", "1.3", "-o", "json"}, workedShape)
	run := func(on bool) (wireOutput, string) {
		testReplaySwitches = map[string]map[string]bool{learnreplay.PackageSaturation: {saturation.SwitchITLBandIntercept: on}}
		defer func() { testReplaySwitches = nil }()
		out, code := runCmd(t, args...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		return decodeOutput(t, out), out
	}
	off, offJSON := run(false)
	on, _ := run(true)
	a, b := off.Roles[0].Results.MeanTPOT.Answer, on.Roles[0].Results.MeanTPOT.Answer
	if a == nil || b == nil || a.Band == nil || b.Band == nil {
		t.Fatalf("no banded mean TPOT: off %+v, on %+v", a, b)
	}
	// The figures from the planner's golden before the switch
	// (replicas-to-slo-worked-json.golden at the parent of #307's change).
	if want := (wireBand{Lo: 0.0036463320177845945, Hi: 0.0036982043841233066}); *a.Band != want {
		t.Errorf("off: band %+v, want the band before the switch %+v", *a.Band, want)
	}
	if strings.Contains(offJSON, `"blocks"`) {
		t.Error("off: a band names its blocks")
	}
	if a.Value != b.Value {
		t.Errorf("the switch moved the figure: off %v, on %v", a.Value, b.Value)
	}
	if b.Band.Blocks < 1 || !(b.Band.Lo <= b.Value && b.Value <= b.Band.Hi) || !(b.Band.Hi-b.Band.Lo > a.Band.Hi-a.Band.Lo) {
		t.Errorf("on: band %+v about %v, want it to hold the figure, name its blocks and be wider than off's %+v", *b.Band, b.Value, *a.Band)
	}
}
