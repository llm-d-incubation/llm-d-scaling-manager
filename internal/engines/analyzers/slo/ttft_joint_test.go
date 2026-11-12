package slo

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// jointRows is a 2+2 fleet at 10 r/s a pod whose prefill pods never queue:
// S 25 ms at 8000 tokens, so T = S/(1 - 0.25); a pull of 25 ms unshared; a
// decode pod reading ITL 4 ms.
func jointRows() []domain.ReplicaMetrics {
	rows := make([]domain.ReplicaMetrics, 0, 4)
	t := 0.025 / (1 - 10*0.025)
	for i := range 2 {
		rows = append(rows, domain.ReplicaMetrics{
			PodName: fmt.Sprintf("pre-%d", i), VariantName: "pre", Ready: true,
			RequestRate: 10, RequestRateKnown: true, AvgInputTokensRecent: 8000,
			AvgPrefillTime: t, AvgPrefillTimeKnown: true, AvgQueueTime: 0, AvgQueueTimeKnown: true,
			AvgTTFT: t + 0.02, AvgTTFTKnown: true,
		})
		rows = append(rows, domain.ReplicaMetrics{
			PodName: fmt.Sprintf("dec-%d", i), VariantName: "dec", Ready: true,
			RequestRate: 10, RequestRateKnown: true, AvgOutputTokens: 200,
			AvgKVTransferTime: 0.025 / (1 - 10*0.025), AvgKVTransferTimeKnown: true,
			AvgITL: 0.004 + 0.0001*float64(i), AvgITLKnown: true, AvgTTFT: 0.08, AvgTTFTKnown: true,
			KvUsageInstant: 0.3, TotalKvCapacityTokens: 100000,
		})
	}
	return rows
}

// jointRig runs the rig for 5 minutes of cycles, past the settle, and
// returns its last reading.
func jointRig(t *testing.T, est prefill.Estimate, on bool) metrics.SLOTTFTReading {
	t.Helper()
	r := newTTFTRig(est, jointRows())
	r.in.VariantStates = append(r.in.VariantStates, decState)
	r.in.ArrivalRate = 20
	require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchTTFTJointInputs: on}))
	var last metrics.SLOTTFTReading
	for range 20 {
		last = r.one(t)
		r.now = r.now.Add(15 * time.Second)
	}
	return last
}

// With the switch on, the joint inputs are published on the prefill
// variant's reading; P1's V_P, which a queue gave, still stands.
func TestTTFTJoint_PublishedBesideP1(t *testing.T) {
	got := jointRig(t, ttftEstimate(), true)
	require.NotNil(t, got.Joint)
	assert.Equal(t, constants.SLOTTFTVPBusyMinutes, got.VPSource, "a queueing source wins")
	assert.InDelta(t, 0.025, got.Joint.Inputs[constants.SLOTTFTJointPrefillWork], 1e-12)
	assert.InDelta(t, 8000, got.Joint.Inputs[constants.SLOTTFTJointPromptTokens], 1e-9)
	assert.InDelta(t, 0.02*(1-0.25), got.Joint.Inputs[constants.SLOTTFTJointOverhead], 1e-12)
	assert.InDelta(t, 0.25, got.Joint.Inputs[constants.SLOTTFTJointPrefillUtilization], 1e-12)
	assert.InDelta(t, 200, got.Joint.Inputs[constants.SLOTTFTJointOutputTokens], 1e-9)
	assert.Contains(t, constants.SLOTTFTJointReasons, got.Joint.Reason)
	for name := range got.Joint.Inputs {
		assert.Contains(t, constants.SLOTTFTJointInputs, name)
	}
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
	assert.Equal(t, []string{constants.SLOTTFTJointReasonPriced, whatif.JointNoPrefillCapacity, whatif.JointNoOverhead,
		whatif.JointNoPull, whatif.JointNoITLLine, whatif.JointNoDecodeOwnLine, whatif.JointNoOutputLength,
		whatif.JointNoReplicas, whatif.JointSaturated, whatif.JointBatchUnheld, whatif.JointInvalidInput},
		constants.SLOTTFTJointReasons)
	assert.Equal(t, constants.SLOTTFTVPInverted, string(prefill.SourceInverted))
}
