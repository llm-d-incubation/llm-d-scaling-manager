package slo

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The router-hop switch is off from the constructor: the controller reads the
// hop as before.
func TestTTFT_TheHopUnqueuedOffFromTheConstructor(t *testing.T) {
	a := New(nil, nil)
	fields := a.switchFields()
	require.Contains(t, fields, SwitchTTFTHopUnqueued)
	assert.False(t, *fields[SwitchTTFTHopUnqueued])
}

// Through Analyze: a prefill pod that holds a capacity queue makes every
// router window over it no reading of the hop with the switch on, so R is
// refused for too few windows; off, the windows count and R is learned.
func TestTTFT_TheHopUnqueuedSwitch(t *testing.T) {
	hop := func(on bool) prefill.HopFit {
		r := newTTFTRig(ttftEstimate(), nil)
		require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchTTFTHopUnqueued: on}))
		r.a.router = fakeRouter{latency: &domain.RouterLatency{WindowSeconds: 300, TTFTSumRate: 2.2, TTFTCountRate: 10,
			TTFTBuckets: []domain.LatencyBucket{{UpperBound: 1, Rate: 10}, {UpperBound: math.Inf(1), Rate: 10}}}}
		r.in.VariantStates = append(r.in.VariantStates, decState)
		rows := prefillRows("pre", 4000)
		rows[0].AvgTTFT, rows[0].AvgTTFTKnown, rows[0].RequestRate = 0.05, true, 10
		rows[0].CapacityQueueLength, rows[0].CapacityQueueKnown, rows[0].QueueLength = 4, true, 4
		rows = append(rows, domain.ReplicaMetrics{PodName: "d", VariantName: "dec", Ready: true, AvgTTFT: 0.1, AvgTTFTKnown: true,
			RequestRate: 10, RequestRateKnown: true})
		r.in.ReplicaMetrics = rows
		for range 80 {
			r.analyze(t)
			r.now = r.now.Add(15 * time.Second)
		}
		return r.a.decodeTerms[ttftWindowKey("ns", "m", "dec")].readings.Estimate().Hop
	}
	off := hop(false)
	require.Empty(t, off.Refused)
	assert.InDelta(t, 0.07, off.Base, 1e-9, "220 ms less 50 and 100")
	assert.Equal(t, prefill.TooFewHopWindows, hop(true).Refused)
}
