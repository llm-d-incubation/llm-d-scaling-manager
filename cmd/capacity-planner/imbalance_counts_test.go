package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// --imbalance-across-counts is the SLO analyzer's
// slo.SwitchTTFTImbalanceAcrossCounts, on by default in the planner
// (whatif.DefaultPlannerImbalanceAcrossCounts, issue
// llm-d-incubation/llm-d-scaling-manager#300) where the analyzer's is off,
// and off with =false.
func TestTTFT_ImbalanceAcrossCountsFlag(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{{nil, true}, {[]string{"--imbalance-across-counts"}, true}, {[]string{"--imbalance-across-counts=false"}, false}} {
		var f answerFlags
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		f.register(fs)
		require.NoError(t, fs.Parse(c.args))
		assert.Equal(t, c.want, f.imbalanceAcrossCounts, "%v", c.args)
	}
	require.True(t, whatif.DefaultPlannerImbalanceAcrossCounts, "the planner reads the curve by default")
	require.False(t, whatif.DefaultImbalanceAcrossCounts, "the SLO analyzer's stays opt-in")
}

// f at a count read across counts: the learned count's own, one pod's none,
// else the curve or the default, and the raw share at the recorded count
// where it reads higher, and through the curve at the others.
func TestTTFT_ImbalanceAcross(t *testing.T) {
	two := prefill.Estimate{FByPods: map[int]float64{2: 1.0691}}
	curve := whatif.ImbalanceReadings{ByPods: two.FByPods}.At(3)
	hot := whatif.ImbalanceReadings{Pods: 3, RawShare: 1.4}.At(4)
	for _, c := range []struct {
		name    string
		ps      learnreplay.PrefillState
		pods    int
		want    float64
		assumed bool
		source  string
	}{
		{"one pod is exact", learnreplay.PrefillState{Pods: 1}, 1, 0, false, "one pod"},
		{"learned at the count", learnreplay.PrefillState{Pods: 2, Estimate: two}, 2, 1.0691, false, sourceLearned},
		{"another count reads the curve", learnreplay.PrefillState{Pods: 2, Estimate: two, RawShare: 1.9}, 3, curve.F, true, "fitted from counts {2}"},
		{"no count learned is the default", learnreplay.PrefillState{Pods: 3}, 3, whatif.FreshImbalance, true, whatif.ImbalanceDefault},
		{"the raw share at the recorded count", learnreplay.PrefillState{Pods: 3, RawShare: 1.4}, 3, 1.4, true, whatif.ImbalanceRawShare},
		{"the raw share capped at the pods", learnreplay.PrefillState{Pods: 2, RawShare: 2.5}, 2, 2, true, whatif.ImbalanceRawShare},
		{"the raw share reaches another count", learnreplay.PrefillState{Pods: 3, RawShare: 1.4}, 4, hot.F, true, "fitted from the raw share at 3"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, assumed, source := imbalanceAcross(&c.ps, c.pods)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.assumed, assumed)
			assert.Equal(t, c.source, source)
		})
	}
}

// The flag reaches the fleet, and the assumed list names where f came from.
func TestTTFT_ImbalanceAcrossCountsNamesItsSource(t *testing.T) {
	st := learnedState()
	st.Prefill[0].Pods = 2
	st.Prefill[0].Estimate.FByPods = map[int]float64{2: 1.0691}

	off := fleetOf(t, st, answerFlags{prefill: 3}, "prefill")
	require.Nil(t, off.refused)
	assert.Equal(t, whatif.ImbalanceFloor, off.fleet.Prefill.Imbalance)
	assert.Contains(t, off.in.Assumed, "f 1.5 on 3 pods (none learned at that count)")

	on := fleetOf(t, st, answerFlags{prefill: 3, imbalanceAcrossCounts: true}, "prefill")
	require.Nil(t, on.refused)
	assert.InDelta(t, 1.104, on.fleet.Prefill.Imbalance, 0.001)
	assert.True(t, on.fleet.Prefill.ImbalanceAssumed)
	assert.Equal(t, "fitted from counts {2}", on.in.ImbalanceSource)
	assert.Contains(t, on.in.Assumed, "f "+num(on.fleet.Prefill.Imbalance)+" on 3 pods (fitted from counts {2})")
}

// On a committed recording (the staircase, two prefill pods, f learned
// there), --prefill 3 prices f off the curve through the two-pod f with the
// flag, and at the 1.5 floor without it; the rate that holds the target rises.
func TestTTFT_ImbalanceAcrossCountsOnTheStaircase(t *testing.T) {
	read := func(extra ...string) (*wireTTFT, float64) {
		args := append([]string{"slo-to-rate", "--recording", stairBOn, "--assume-vp", "200000", "--assume-a", "20ms",
			"--ttft", "2s@p90", "--prefill", "3", "-o", "json"}, extra...)
		out, _ := runCmd(t, args...)
		var w wireOutput
		require.NoError(t, json.Unmarshal([]byte(out), &w), out)
		for _, r := range w.Roles {
			if r.Role == rolePrefill && r.TTFT != nil {
				require.NotNil(t, r.Results.Rate, out)
				require.NotNil(t, r.Results.Rate.Answer, out)
				return r.TTFT, r.Results.Rate.Answer.Value
			}
		}
		require.FailNow(t, "no prefill role", out)
		return nil, 0
	}
	f := answerFlags{recording: stairBOn, cycle: "last"}
	st, err := f.readState(context.Background())
	require.NoError(t, err)
	require.Len(t, st.Prefill, 1)
	require.Equal(t, 2, st.Prefill[0].Pods)
	require.Contains(t, st.Prefill[0].Estimate.FByPods, 2, "precondition: P1 learned f at two pods")
	learned := whatif.ImbalanceReadingsOf(st.Prefill[0].Estimate, st.Prefill[0].Pods, st.Prefill[0].RawShare).At(3)

	off, offRate := read("--imbalance-across-counts=false")
	on, onRate := read()
	t.Logf("f(2) %.4f; at 3 pods off f %.4f (%s) rate %.3f, on f %.4f (%s) rate %.3f",
		st.Prefill[0].Estimate.FByPods[2], off.Imbalance, off.ImbalanceSource, offRate, on.Imbalance, on.ImbalanceSource, onRate)
	assert.Equal(t, whatif.ImbalanceFloor, off.Imbalance)
	assert.Equal(t, "assumed", off.ImbalanceSource)
	assert.InDelta(t, learned.F, on.Imbalance, 1e-12)
	assert.Equal(t, "fitted from counts {2}", on.ImbalanceSource)
	assert.Greater(t, onRate, offRate, "a lower f holds the TTFT target to a higher rate")
}

// at-replicas takes the flag to every count its search prices: the staircase
// sized past its two prefill pods notes f off the curve, not the 1.5 floor.
func TestTTFT_ImbalanceAcrossCountsReachesAtReplicas(t *testing.T) {
	args := atStair("--shape", "measured", "--load", "seen", "--load-seen-x", "1.2")
	off, code := runCmd(t, append(args, "--imbalance-across-counts=false")...)
	require.Equal(t, 0, code, off)
	require.Contains(t, off, "prefill f 1.5 assumed at 3 pods", "precondition: the search reaches three prefill pods")
	on, code := runCmd(t, args...)
	require.Equal(t, 0, code, on)
	assert.NotContains(t, on, "prefill f 1.5 assumed")
	assert.Contains(t, on, fmt.Sprintf("prefill f %.3g assumed at 3 pods", whatif.ImbalanceReadings{ByPods: map[int]float64{2: stairF2(t)}}.At(3).F))
}

// stairF2 is the f P1 learned at two pods on the staircase.
func stairF2(t *testing.T) float64 {
	t.Helper()
	f := answerFlags{recording: stairBOn, cycle: "last"}
	st, err := f.readState(context.Background())
	require.NoError(t, err)
	v, ok := st.Prefill[0].Estimate.FByPods[2]
	require.True(t, ok)
	return v
}
