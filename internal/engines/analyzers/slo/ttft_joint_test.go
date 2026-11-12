package slo

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// jointRows is cycle i of a 2+3 fleet: prefill pods at 10 r/s that never
// queue, S 25 ms at 4000 computed tokens (8000 sent, half of them prefix
// hits: SGLang reads 8000), T = S/(1 - 0.25), a0 15 ms; decode pods with a
// pull of 25 ms unshared, a batch the generated-token rate g moves cycle by
// cycle, ITL = 2 ms + 0.2 ms*k at k = g*ITL, and an own part 5*ITL + 10 ms.
func jointRows(i int) []domain.ReplicaMetrics {
	rows := make([]domain.ReplicaMetrics, 0, 5)
	t := 0.025 / (1 - 10*0.025)
	for p := range 2 {
		rows = append(rows, domain.ReplicaMetrics{
			PodName: fmt.Sprintf("pre-%d", p), VariantName: "pre", Ready: true,
			RequestRate: 10, RequestRateKnown: true, AvgInputTokensRecent: 8000,
			PrefillComputedTokenRate: 40000, PrefillComputedTokenRateKnown: true,
			AvgPrefillTime: t, AvgPrefillTimeKnown: true, AvgQueueTime: 0, AvgQueueTimeKnown: true,
			AvgTTFT: t + 0.02, AvgTTFTKnown: true,
		})
	}
	g := 1000 + 100*float64(i%20)
	itl := 0.002 / (1 - 0.0002*g)
	pull := 0.025 / (1 - 10*0.025)
	for d := range 3 {
		rows = append(rows, domain.ReplicaMetrics{
			PodName: fmt.Sprintf("dec-%d", d), VariantName: "dec", Ready: true,
			RequestRate: 10, RequestRateKnown: true, AvgOutputTokens: 200,
			GenerationTokenRate: g, GenerationTokenRateKnown: true,
			AvgKVTransferTime: pull, AvgKVTransferTimeKnown: true,
			AvgITL: itl, AvgITLKnown: true, AvgTTFT: pull + 5*itl + 0.01, AvgTTFTKnown: true,
			KvUsageInstant: 0.3, TotalKvCapacityTokens: 100000,
		})
	}
	return rows
}

// jointRun is a rig with the switch set, at arrival rate 20.
func jointRun(t *testing.T, est prefill.Estimate, on bool) *ttftRig {
	t.Helper()
	r := newTTFTRig(est, jointRows(0))
	r.in.VariantStates = append(r.in.VariantStates, decState)
	r.in.ArrivalRate = 20
	require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchTTFTJointInputs: on}))
	return r
}

// cycles runs n cycles of the rig from cycle from, 15 s apart, and returns
// the last reading.
func (r *ttftRig) cycles(t *testing.T, from, n int) metrics.SLOTTFTReading {
	t.Helper()
	var last metrics.SLOTTFTReading
	for i := from; i < from+n; i++ {
		r.in.ReplicaMetrics = jointRows(i)
		last = r.one(t)
		r.now = r.now.Add(15 * time.Second)
	}
	return last
}

// jointRig runs the rig for 5 minutes of cycles, past the settle, and
// returns its last reading.
func jointRig(t *testing.T, est prefill.Estimate, on bool) metrics.SLOTTFTReading {
	t.Helper()
	return jointRun(t, est, on).cycles(t, 0, 20)
}

// jointEstimate is the window's estimate, as the analyzer holds it.
func (r *ttftRig) jointEstimate(t *testing.T) prefill.JointEstimate {
	t.Helper()
	w := r.a.joint[ttftWindowKey("ns", "m", "pre")]
	require.NotNil(t, w)
	return w.readings.Estimate()
}

// With the switch on, the joint inputs are published on the prefill
// variant's reading; P1's V_P, which a queue gave, still stands. The mean is
// whatif's at the fleet that ran, 2 prefill and 3 decode pods, and the
// arrival rate, with the assumed hop.
func TestTTFTJoint_PublishedBesideP1(t *testing.T) {
	r := jointRun(t, ttftEstimate(), true)
	got := r.cycles(t, 0, 20)
	require.NotNil(t, got.Joint)
	assert.Equal(t, constants.SLOTTFTVPBusyMinutes, got.VPSource, "a queueing source wins")
	assert.InDelta(t, 0.025, got.Joint.Inputs[constants.SLOTTFTJointPrefillWork], 1e-12)
	assert.InDelta(t, 4000, got.Joint.Inputs[constants.SLOTTFTJointPromptTokens], 1e-9, "vLLM: computed tokens")
	assert.InDelta(t, 0.02*(1-0.25), got.Joint.Inputs[constants.SLOTTFTJointOverhead], 1e-12)
	assert.InDelta(t, 0.25, got.Joint.Inputs[constants.SLOTTFTJointPrefillUtilization], 1e-12)
	assert.InDelta(t, 200, got.Joint.Inputs[constants.SLOTTFTJointOutputTokens], 1e-9)
	assert.InDelta(t, 0.002, got.Joint.Inputs[constants.SLOTTFTJointITLAlpha], 1e-9)
	assert.InDelta(t, 0.0002, got.Joint.Inputs[constants.SLOTTFTJointITLBeta], 1e-9)
	for name := range got.Joint.Inputs {
		assert.Contains(t, constants.SLOTTFTJointInputs, name)
	}

	require.Equal(t, constants.SLOTTFTJointReasonPriced, got.Joint.Reason)
	want, refused := whatif.JointTTFTMean(r.jointEstimate(t), whatif.JointQuestion{
		PrefillReplicas: 2, DecodeReplicas: 3, Rate: 20, HopSeconds: whatif.AssumedRouterHopSeconds, HopAssumed: true})
	require.Empty(t, refused)
	assert.InDelta(t, want.Seconds, got.Joint.MeanSeconds, 1e-12)
	assert.InDelta(t, want.PullWork, got.Joint.Inputs[constants.SLOTTFTJointPull], 1e-12)
	assert.Equal(t, []string{"OverheadMeasured", "RouterHopAssumed"}, got.Joint.Flags)
}

// The hop is the decode term's where it is fitted, and assumed otherwise.
func TestTTFTJoint_TheHop(t *testing.T) {
	hop, assumed := jointHop(nil, 8000)
	assert.True(t, assumed)
	assert.Equal(t, whatif.AssumedRouterHopSeconds, hop)
	fitted := &whatif.DecodeTerm{Hop: whatif.PromptLine{Base: 0.005, PerToken: 1e-6}}
	hop, assumed = jointHop(fitted, 8000)
	assert.False(t, assumed)
	assert.InDelta(t, 0.013, hop, 1e-12)
	fitted.HopAssumed = true
	_, assumed = jointHop(fitted, 8000)
	assert.True(t, assumed, "an assumed hop on the term is assumed here too")
}

// A change of accelerator sweeps the window, and a refused mean publishes no
// pull figure.
func TestTTFTJoint_ANewAcceleratorSweepsTheWindow(t *testing.T) {
	r := jointRun(t, ttftEstimate(), true)
	require.Equal(t, constants.SLOTTFTJointReasonPriced, r.cycles(t, 0, 20).Joint.Reason)
	r.in.VariantStates[len(r.in.VariantStates)-1].AcceleratorName = "accelerator-b"
	got := r.cycles(t, 20, 1)
	assert.Equal(t, string(whatif.JointNoPrefillCapacity), got.Joint.Reason)
	assert.NotContains(t, got.Joint.Inputs, constants.SLOTTFTJointPrefillWork, "the old engine's readings are gone")
	assert.NotContains(t, got.Joint.Inputs, constants.SLOTTFTJointPull, "no pull figure without a mean")
	assert.True(t, math.IsNaN(got.Joint.MeanSeconds))
}

// On SGLang the prompt is the prompt length net of hits, not computed
// tokens over requests.
func TestTTFTJoint_SGLangReadsThePromptLength(t *testing.T) {
	r := jointRun(t, ttftEstimate(), true)
	r.a.store = capacity.NewStore()
	r.a.store.FileScaleTarget("ns", "m", "pre", "H100", 1, capacity.EngineParams{Engine: inferenceengine.EngineSGLang},
		false, r.now)
	got := r.cycles(t, 0, 20)
	assert.InDelta(t, 8000, got.Joint.Inputs[constants.SLOTTFTJointPromptTokens], 1e-9)
}

// Where P1 refused V_P, the inverted V_P stands, and says so.
func TestTTFTJoint_TheInvertedVPStandsWhereP1Refused(t *testing.T) {
	refused := prefill.Estimate{VPRefused: prefill.NoPrefillCapacity, A: 0.01}
	got := jointRig(t, refused, true)
	assert.Equal(t, constants.SLOTTFTVPInverted, got.VPSource)
	assert.NotEqual(t, constants.SLOTTFTReasonNoPrefillCapacity, got.Reason)

	off := jointRig(t, refused, false)
	assert.Nil(t, off.Joint, "off, nothing is published")
	assert.Equal(t, constants.SLOTTFTVPNone, off.VPSource)
	assert.Equal(t, constants.SLOTTFTReasonNoPrefillCapacity, off.Reason)
}

// withInvertedVP replaces only a refused V_P, and only with a learned one.
func TestTTFTJoint_WithInvertedVP(t *testing.T) {
	learned := &jointCycle{est: prefill.JointEstimate{VP: 320000, PrefillWork: 0.025, PrefillWorkPrompt: 8000,
		PrefillWorkSamples: 40}}
	p1 := ttftEstimate()
	assert.Equal(t, p1, withInvertedVP(p1, learned), "P1's V_P stands")
	refused := prefill.Estimate{VPRefused: prefill.NoPrefillCapacity, VPLowerBound: true}
	got := withInvertedVP(refused, learned)
	assert.Equal(t, 320000.0, got.VP)
	assert.Equal(t, prefill.SourceInverted, got.VPSource)
	assert.Empty(t, got.VPRefused)
	assert.False(t, got.VPLowerBound, "the inversion is no bound")
	assert.Equal(t, 8000.0, got.VPPromptTokens)
	assert.Equal(t, refused, withInvertedVP(refused, nil))
	assert.Equal(t, refused, withInvertedVP(refused, &jointCycle{est: prefill.JointEstimate{
		PrefillWorkRefused: prefill.NoQueueFreeMinutes}}))
}

// The joint reasons and flags constants spells are whatif's.
func TestTTFTJoint_ConstantsAreWhatifs(t *testing.T) {
	want := append(make([]string, 0, len(constants.SLOTTFTJointReasons)), constants.SLOTTFTJointReasonPriced)
	for _, r := range []whatif.JointRefusal{whatif.JointNoPrefillCapacity, whatif.JointNoOverhead,
		whatif.JointNoPull, whatif.JointNoITLLine, whatif.JointNoDecodeOwnLine, whatif.JointNoOutputLength,
		whatif.JointNoReplicas, whatif.JointSaturated, whatif.JointBatchUnheld, whatif.JointInvalidInput} {
		want = append(want, string(r))
	}
	assert.Equal(t, want, constants.SLOTTFTJointReasons)
	assert.Equal(t, constants.SLOTTFTVPInverted, string(prefill.SourceInverted))
	assert.Equal(t, []string{string(whatif.OverheadMeasured), string(whatif.PullUnseen), string(whatif.RouterHopAssumed)},
		constants.SLOTTFTJointFlags)
}
