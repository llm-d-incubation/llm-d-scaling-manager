package slo

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// onsetInputs are the inputs priceTTFTVariants builds with the replay
// switches set as asked, captured through onTTFTPriced after 13 cycles of a
// prefill variant beside a decode line.
func onsetInputs(t *testing.T, switches map[string]bool) ttftInputs {
	t.Helper()
	var last ttftInputs
	onTTFTPriced = func(in ttftInputs, _ float64, _ whatif.TTFTTarget) { last = in }
	t.Cleanup(func() { onTTFTPriced = nil })
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
	require.NoError(t, r.a.SetSwitchesForReplay(switches))
	require.Equal(t, switches[SwitchTTFTPSOnsets], r.a.ttftPSOnsets, "%v: the replay switch sets its own field", switches)
	require.Equal(t, switches[SwitchTTFTDecodeGateCharge], r.a.ttftDecodeGateCharge, "%v: the replay switch sets its own field", switches)
	for i := range 13 {
		r.now = r.now.Add(time.Minute)
		r.in.ReplicaMetrics = rows(i)
		r.one(t)
	}
	return last
}

// onsetFleetInputs are in with flow control on, two prefill pods at f 1.5
// and sixteen decode pods, so that the prefill onset is what caps question 1.
func onsetFleetInputs(in ttftInputs) ttftInputs {
	in.pods, in.otherPods = 2, 0
	in.est.FByPods = map[int]float64{2: 1.5}
	in.flow = whatif.FlowControl{State: whatif.FlowControlOn, QueueThreshold: whatif.FlowControlQueueThreshold, KVThreshold: whatif.FlowControlKVThreshold}
	d := *testDecode()
	d.Replicas = 16
	in.decode, in.term, in.legacyDecodeSum = &d, nil, false
	in.processorSharing = true
	in.spread = whatif.PromptSpread{CV: 0.06}
	in.prompt = 2000
	return in
}

// The onset switches (SwitchTTFTPSOnsets, SwitchTTFTDecodeGateCharge): off by
// default at the CLI's defaults, and each carried alone by priceTTFTVariants
// into the inputs it prices, and by ttftFleet into the fleet, so a swapped or
// cross-wired switch shows. On those inputs, with the prefill onset capping
// question 1, the analyzer's verdict and published rate are the shared
// whatif ones: the prefill onset moves with the switch, the rate it caps
// moves with it, and the charge never answers above the uncharged rate nor
// moves met/unmet.
func TestTTFT_TheOnsetSwitches(t *testing.T) {
	for _, c := range []struct{ ps, charge bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		in := onsetInputs(t, map[string]bool{SwitchTTFTPSOnsets: c.ps, SwitchTTFTDecodeGateCharge: c.charge})
		require.Equal(t, c.ps, in.psOnsets, "%+v: priceTTFTVariants carries SwitchTTFTPSOnsets", c)
		require.Equal(t, c.charge, in.decodeGateCharge, "%+v: priceTTFTVariants carries SwitchTTFTDecodeGateCharge", c)
		fl, _ := ttftFleet(onsetFleetInputs(in), 2)
		require.Equal(t, c.ps, fl.PSOnsets, "%+v: ttftFleet carries psOnsets", c)
		require.Equal(t, c.charge, fl.ChargeDecodeGate, "%+v: ttftFleet carries decodeGateCharge", c)
	}

	in := onsetFleetInputs(onsetInputs(t, map[string]bool{SwitchTTFTPSOnsets: true, SwitchTTFTDecodeGateCharge: true}))
	off := in
	off.psOnsets, off.decodeGateCharge = false, false
	onFleet, _ := ttftFleet(in, 2)
	offFleet, _ := ttftFleet(off, 2)
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

	compared := 0
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
			u := priceTTFT(gate, 1, target).rate
			require.NotNil(t, u, "target %g s q %g: the charged rate answered, the uncharged did not", secs, q)
			assert.LessOrEqual(t, p.rate.Value, u.Value, "target %g s q %g: charged above uncharged", secs, q)
			for _, rate := range []float64{0.5 * u.Value, u.Value} {
				assert.Equal(t, priceTTFT(gate, rate, target).unmet, priceTTFT(in, rate, target).unmet, "target %g s q %g at %g r/s: the charge moved the verdict", secs, q, rate)
			}
			compared++
		}
	}
	assert.Equal(t, 6, compared, "every target answered and compared")
}

// The charge does not stop the analyzer publishing: on a P/D fleet loaded
// toward its decode onset (four decode pods), at every target, the mean and
// two percentiles, that the uncharged rate publishes a per-replica rate at,
// the charged one publishes one too, priced, never decode-bound, at or under
// the uncharged rate. Some mean and some percentile target must be ones the
// charge lowers.
func TestTTFT_TheChargeStillPublishes(t *testing.T) {
	in := onsetFleetInputs(ttftInputsAt(2, 1.5))
	d := *testDecode()
	in.decode = &d
	off := in
	in.decodeGateCharge = true
	lowered := map[bool]int{}
	for _, secs := range []float64{0.1, 0.15, 0.2, 0.3, 0.5, 1, 2, 5} {
		for _, q := range []float64{0, 0.9, 0.95} {
			target := whatif.TTFTTarget{Seconds: secs, Percentile: q}
			pOff := priceTTFT(off, 1, target)
			if pOff.reason != constants.SLOTTFTReasonPriced {
				continue
			}
			pOn := priceTTFT(in, 1, target)
			require.NotNil(t, pOn.rate, "target %g s q %g", secs, q)
			assert.Equal(t, constants.SLOTTFTReasonPriced, pOn.reason, "target %g s q %g: rate %g r/s, bound %v", secs, q, pOn.rate.Value, pOn.rate.Basis.BoundBy)
			assert.False(t, math.IsNaN(pOn.perReplica), "target %g s q %g: no per-replica rate published", secs, q)
			assert.LessOrEqual(t, pOn.rate.Value, pOff.rate.Value)
			if pOn.rate.Value < pOff.rate.Value*(1-1e-6) {
				lowered[q > 0]++
			}
		}
	}
	t.Logf("targets lowered by the charge, mean (false) and percentile (true): %v", lowered)
	assert.Positive(t, lowered[false], "no mean target was lowered by the charge")
	assert.Positive(t, lowered[true], "no percentile target was lowered by the charge")
}
