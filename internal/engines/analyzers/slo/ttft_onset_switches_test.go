package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The onset switches (SwitchTTFTPSOnsets, SwitchTTFTDecodeGateCharge): off by
// default at the CLI's defaults, carried to the fleet, and with them on the
// analyzer's verdict and published rate are the shared whatif ones on that
// fleet. The charge moves only the published rate: never above the uncharged
// one, and met/unmet at any rate does not move.
func TestTTFT_TheOnsetSwitches(t *testing.T) {
	a := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000)).a
	require.Equal(t, whatif.DefaultPSOnsets, a.ttftPSOnsets, "the analyzer's default is the CLI's")
	require.Equal(t, whatif.DefaultChargeDecodeGate, a.ttftDecodeGateCharge, "the analyzer's default is the CLI's")
	require.False(t, a.ttftPSOnsets || a.ttftDecodeGateCharge, "off by default")
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchTTFTPSOnsets: true, SwitchTTFTDecodeGateCharge: true}))
	require.True(t, a.ttftPSOnsets && a.ttftDecodeGateCharge)

	in := ttftInputsAt(2, 1.5)
	in.flow = whatif.FlowControl{State: whatif.FlowControlOn, QueueThreshold: whatif.FlowControlQueueThreshold, KVThreshold: whatif.FlowControlKVThreshold}
	in.decode = testDecode()
	in.processorSharing = true
	in.spread = whatif.PromptSpread{CV: 0.06}
	off, _ := ttftFleet(in, 2)
	in.psOnsets, in.decodeGateCharge = true, true
	on, _ := ttftFleet(in, 2)
	require.True(t, on.PSOnsets && on.ChargeDecodeGate)
	require.Zero(t, on.Prefill.RunningCap, "the analyzer reads no running cap")
	offOnsets, ok := whatif.FlowControlOnsets(off).(whatif.Onsets)
	require.True(t, ok)
	onOnsets, ok := whatif.FlowControlOnsets(on).(whatif.Onsets)
	require.True(t, ok)
	t.Logf("onsets off %+v, on %+v", offOnsets, onOnsets)
	assert.NotEqual(t, offOnsets.Prefill, onOnsets.Prefill, "the switch moves the prefill onset")

	seen := 0
	for _, secs := range []float64{0.5, 2, 100} {
		for _, q := range []float64{0, 0.9} {
			target := whatif.TTFTTarget{Seconds: secs, Percentile: q}
			p := priceTTFT(in, 1, target)
			shared, ok := whatif.RateForTTFT(on, target).(whatif.Answer)
			if p.rate == nil {
				assert.False(t, ok, "target %g s q %g", secs, q)
				continue
			}
			require.True(t, ok)
			assert.Equal(t, shared.Value, p.rate.Value, "target %g s q %g: the shared rate", secs, q)
			at := priceTTFT(in, p.rate.Value, target)
			assert.False(t, at.unmet, "target %g s q %g: rate %g r/s (bound %v) is unmet at itself", secs, q, p.rate.Value, p.rate.Basis.BoundBy)
			gate := in
			gate.decodeGateCharge = false
			if u := priceTTFT(gate, 1, target).rate; u != nil {
				assert.LessOrEqual(t, p.rate.Value, u.Value, "target %g s q %g: charged above uncharged", secs, q)
				for _, rate := range []float64{0.5 * u.Value, u.Value} {
					assert.Equal(t, priceTTFT(gate, rate, target).unmet, priceTTFT(in, rate, target).unmet, "target %g s q %g at %g r/s: the charge moved the verdict", secs, q, rate)
				}
			}
			seen++
		}
	}
	assert.Positive(t, seen, "no rate answered")
}
