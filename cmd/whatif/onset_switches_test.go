package main

import (
	"flag"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// --ps-onsets, --prefill-running-cap and --charge-decode-gate are
// whatif.TTFTFleet.PSOnsets, Prefill.RunningCap and ChargeDecodeGate: off and
// not known by default, at the SLO analyzer's defaults, and set by the flags.
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

	pf := fleetOf(t, learnedState(), answerFlags{})
	require.Nil(t, pf.refused)
	assert.False(t, pf.fleet.PSOnsets || pf.fleet.ChargeDecodeGate, "the defaults")
	assert.Zero(t, pf.fleet.Prefill.RunningCap)
	pf = fleetOf(t, learnedState(), answerFlags{psOnsets: true, chargeDecodeGate: true, prefillRunningCap: 10})
	assert.True(t, pf.fleet.PSOnsets && pf.fleet.ChargeDecodeGate, "with the flags")
	assert.Equal(t, 10, pf.fleet.Prefill.RunningCap)
}
