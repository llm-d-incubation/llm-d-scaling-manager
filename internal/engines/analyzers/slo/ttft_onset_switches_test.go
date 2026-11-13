package slo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The onset switches (SwitchTTFTPSOnsets, SwitchTTFTDecodeGateCharge): off by
// default at the CLI's defaults, and carried by priceTTFTVariants into the
// inputs it prices, on and off. On those inputs, with flow control on and
// sixteen decode pods so that the prefill onset is what caps question 1, the
// analyzer's verdict and published rate are the shared whatif ones: the
// prefill onset moves with the switch, the rate it caps moves with it, and
// the charge never answers above the uncharged rate nor moves met/unmet.
func TestTTFT_TheOnsetSwitches(t *testing.T) {
	var last ttftInputs
	onTTFTPriced = func(in ttftInputs, _ float64, _ whatif.TTFTTarget) { last = in }
	t.Cleanup(func() { onTTFTPriced = nil })
	read := func(on bool) ttftInputs {
		rows := func(i int) []domain.ReplicaMetrics {
			return append(prefillRows("pre", 4000), cycleRows("dec", i)...)
		}
		r := newTTFTRig(ttftEstimate(), rows(0))
		require.Equal(t, whatif.DefaultPSOnsets, r.a.ttftPSOnsets, "the analyzer's default is the CLI's")
		require.Equal(t, whatif.DefaultChargeDecodeGate, r.a.ttftDecodeGateCharge, "the analyzer's default is the CLI's")
		require.False(t, r.a.ttftPSOnsets || r.a.ttftDecodeGateCharge, "off by default")
		r.a.store = capacity.NewStore()
		r.a.store.Update("ns", "m", "dec", capacity.Record{EngineParams: &capacity.EngineParams{MaxNumSeqs: 64}}, ttftNow)
		r.in.VariantStates = append(r.in.VariantStates, decState)
		require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchTTFTPSOnsets: on, SwitchTTFTDecodeGateCharge: on}))
		last = ttftInputs{}
		for i := range 13 {
			r.now = r.now.Add(time.Minute)
			r.in.ReplicaMetrics = rows(i)
			r.one(t)
		}
		require.Equal(t, on, last.psOnsets, "priceTTFTVariants carries SwitchTTFTPSOnsets")
		require.Equal(t, on, last.decodeGateCharge, "priceTTFTVariants carries SwitchTTFTDecodeGateCharge")
		return last
	}
	require.False(t, read(false).psOnsets)
	in := read(true)

	in.pods, in.otherPods = 2, 0
	in.est.FByPods = map[int]float64{2: 1.5}
	in.flow = whatif.FlowControl{State: whatif.FlowControlOn, QueueThreshold: whatif.FlowControlQueueThreshold, KVThreshold: whatif.FlowControlKVThreshold}
	d := *testDecode()
	d.Replicas = 16
	in.decode, in.term, in.legacyDecodeSum = &d, nil, false
	in.processorSharing = true
	in.spread = whatif.PromptSpread{CV: 0.06}
	in.prompt = 2000
	off := in
	off.psOnsets, off.decodeGateCharge = false, false
	onFleet, _ := ttftFleet(in, 2)
	offFleet, _ := ttftFleet(off, 2)
	require.True(t, onFleet.PSOnsets && onFleet.ChargeDecodeGate)
	require.Zero(t, onFleet.Prefill.RunningCap, "the analyzer reads no running cap")
	onOnsets, ok := whatif.FlowControlOnsets(onFleet).(whatif.Onsets)
	require.True(t, ok)
	offOnsets, ok := whatif.FlowControlOnsets(offFleet).(whatif.Onsets)
	require.True(t, ok)
	t.Logf("onsets off %+v, on %+v", offOnsets, onOnsets)
	require.Less(t, onOnsets.Prefill, onOnsets.Decode, "precondition: the prefill onset is the lower")
	assert.Less(t, onOnsets.Prefill, offOnsets.Prefill, "an unknown cap moves the onset down, never up")

	loose := whatif.TTFTTarget{Seconds: 100, Percentile: 0.9}
	pOn, pOff := priceTTFT(in, 1, loose), priceTTFT(off, 1, loose)
	require.NotNil(t, pOn.rate)
	require.NotNil(t, pOff.rate)
	assert.Contains(t, pOn.rate.Basis.BoundBy, whatif.BoundPrefillOnset)
	assert.Contains(t, pOff.rate.Basis.BoundBy, whatif.BoundPrefillOnset)
	assert.Less(t, pOn.rate.Value, pOff.rate.Value, "the published rate moves with the onset")
	between := (onOnsets.Prefill + offOnsets.Prefill) / 2
	assert.True(t, priceTTFT(in, between, loose).unmet, "past the switch's onset the verdict is a miss")
	assert.False(t, priceTTFT(off, between, loose).unmet, "under the default onset it is met")

	seen := 0
	for _, secs := range []float64{0.5, 2, 100} {
		for _, q := range []float64{0, 0.9} {
			target := whatif.TTFTTarget{Seconds: secs, Percentile: q}
			p := priceTTFT(in, 1, target)
			shared, ok := whatif.RateForTTFT(onFleet, target).(whatif.Answer)
			if p.rate == nil {
				assert.False(t, ok, "target %g s q %g", secs, q)
				continue
			}
			require.True(t, ok)
			assert.Equal(t, shared.Value, p.rate.Value, "target %g s q %g: the shared rate", secs, q)
			assert.False(t, priceTTFT(in, p.rate.Value, target).unmet, "target %g s q %g: rate %g r/s (bound %v) is unmet at itself", secs, q, p.rate.Value, p.rate.Basis.BoundBy)
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
