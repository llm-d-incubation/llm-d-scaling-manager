package main

import (
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// --ps-onsets, --prefill-running-cap and --charge-decode-gate are
// whatif.TTFTFleet.PSOnsets, Prefill.RunningCap and ChargeDecodeGate: off and
// not known by default, at the SLO analyzer's defaults, and each set by its
// own flag alone, so a swapped pair shows.
func TestTTFT_OnsetFlags(t *testing.T) {
	for _, c := range []struct {
		args          []string
		ps, charge    bool
		runningCapped int
	}{
		{nil, whatif.DefaultPSOnsets, whatif.DefaultChargeDecodeGate, 0},
		{[]string{"--ps-onsets", "--prefill-running-cap", "10"}, true, false, 10},
		{[]string{"--charge-decode-gate"}, false, true, 0},
	} {
		var f answerFlags
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		f.register(fs)
		require.NoError(t, fs.Parse(c.args))
		assert.Equal(t, c.ps, f.psOnsets, "%v", c.args)
		assert.Equal(t, c.charge, f.chargeDecodeGate, "%v", c.args)
		assert.Equal(t, c.runningCapped, f.prefillRunningCap, "%v", c.args)
	}

	// The running cap's range is checked first, then that --ps-onsets reads
	// it; the charge is read by slo-to-rate alone.
	for _, c := range []struct {
		question string
		args     []string
		want     string
	}{
		{rateQuestion, []string{"--prefill-running-cap", "10"}, "read only with --ps-onsets"},
		{rateQuestion, []string{"--prefill-running-cap", "-1"}, "want 0 (unknown) to 1000000"},
		{rateQuestion, []string{"--ps-onsets", "--prefill-running-cap", "-1"}, "want 0 (unknown) to 1000000"},
		{rateQuestion, []string{"--ps-onsets", "--prefill-running-cap", "1000001"}, "want 0 (unknown) to 1000000"},
		{rateQuestion, []string{"--ps-onsets", "--prefill-running-cap", "1000000"}, ""},
		{rateQuestion, []string{"--ps-onsets"}, ""},
		{rateQuestion, []string{"--charge-decode-gate"}, ""},
		{replicasQuestion, []string{"--charge-decode-gate"}, "read only by slo-to-rate"},
		{replicasQuestion, []string{"--charge-decode-gate=false"}, ""},
		{replicasQuestion, []string{"--ps-onsets"}, ""},
	} {
		f := answerFlags{question: c.question, set: map[string]bool{}}
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		f.register(fs)
		require.NoError(t, fs.Parse(c.args))
		fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
		err := f.checkTTFT()
		if c.want == "" {
			assert.NoError(t, err, "%s %v", c.question, c.args)
			continue
		}
		require.Error(t, err, "%s %v", c.question, c.args)
		assert.Contains(t, err.Error(), c.want, "%s %v", c.question, c.args)
	}

	pf := fleetOf(t, learnedState(), answerFlags{})
	require.Nil(t, pf.refused)
	assert.False(t, pf.fleet.PSOnsets || pf.fleet.ChargeDecodeGate, "the defaults")
	assert.Zero(t, pf.fleet.Prefill.RunningCap)
	pf = fleetOf(t, learnedState(), answerFlags{psOnsets: true, prefillRunningCap: 10})
	assert.True(t, pf.fleet.PSOnsets, "--ps-onsets alone")
	assert.False(t, pf.fleet.ChargeDecodeGate, "--ps-onsets alone")
	assert.Equal(t, 10, pf.fleet.Prefill.RunningCap)
	pf = fleetOf(t, learnedState(), answerFlags{chargeDecodeGate: true})
	assert.False(t, pf.fleet.PSOnsets, "--charge-decode-gate alone")
	assert.True(t, pf.fleet.ChargeDecodeGate, "--charge-decode-gate alone")
}

// The output says what the switches did, in the assumed inputs the text and
// the JSON both carry: the prefill onset's placement, and the decode gate's
// charge or why it was not applied. Neither flag, no line.
func TestTTFT_OnsetFlagsSayWhatTheyDid(t *testing.T) {
	// At a narrow spread: the CLI's assumed cv, 2, is past MaxPSPromptCV.
	notes := func(f answerFlags, flowControl bool) string {
		st := learnedState()
		st.FlowControlSeries = flowControl
		f.assumePromptCV = 0.06
		pf := fleetOf(t, st, f, "assume-prompt-cv")
		require.Nil(t, pf.refused)
		return strings.Join(pf.in.Assumed, "; ")
	}
	off := notes(answerFlags{}, true)
	assert.NotContains(t, off, "--ps-onsets")
	assert.NotContains(t, off, "--charge-decode-gate")
	for _, c := range []struct {
		name string
		f    answerFlags
		fc   bool
		want string
	}{
		{"cap 10", answerFlags{psOnsets: true, prefillRunningCap: 10}, true, "prefill onset on processor sharing at a running cap of 10 (--ps-onsets)"},
		{"cap unknown", answerFlags{psOnsets: true}, true, "prefill onset " + string(whatif.PlacedLowerCapUnknown) + " (--ps-onsets)"},
		{"no flow control", answerFlags{psOnsets: true}, false, "--ps-onsets not applied: no prefill onset (flow control is not on)"},
		{"no decode line", answerFlags{chargeDecodeGate: true}, true, string(whatif.GateNoDecodeLine) + " (--charge-decode-gate)"},
		{"charge, flow control off", answerFlags{chargeDecodeGate: true}, false, string(whatif.GateFlowControlNotOn) + " (--charge-decode-gate)"},
	} {
		assert.Contains(t, notes(c.f, c.fc), c.want, c.name)
	}
	// Past MaxPSPromptCV, at the CLI's assumed cv of 2, the cap is one side
	// of the lower of two.
	for _, c := range []struct {
		cap  int
		want string
	}{{10, "10"}, {0, "1 (not known)"}} {
		st := learnedState()
		st.FlowControlSeries = true
		pf := fleetOf(t, st, answerFlags{psOnsets: true, prefillRunningCap: c.cap})
		require.Nil(t, pf.refused)
		assert.Contains(t, strings.Join(pf.in.Assumed, "; "),
			"prefill onset the lower of processor sharing at a running cap of "+c.want+" and first come: prompt cv 2 is past 0.25 (--ps-onsets)")
	}
}
