package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// queuedRun is the Qwen3-32B warm-up's shape, in small: 15 minutes at 1k
// prompts with no queue and R 4 ms, then 15 minutes at 8k with the prefill pods
// queued, where the router's mean reads 0.3 s over the engines'. unqueued is
// DecodeTermCycle.HopUnqueued.
func queuedRun(unqueued, capacityKnown bool) HopFit {
	w := NewDecodeTermWindow()
	at := termT0
	for i := range 120 {
		prompt, r, queued := 1000.0, 0.004, false
		if i >= 60 {
			prompt, r, queued = 8000, 0.3, true
		}
		c := hopCycle(at, 0.08+r, 10, prompt)
		c.HopUnqueued = unqueued
		if queued {
			pre := &c.Prefill[0]
			if capacityKnown {
				pre.CapacityQueueKnown, pre.CapacityQueueLength = true, 12
			}
			pre.QueueLength = 12
		}
		w.Observe(c)
		at = at.Add(15 * time.Second)
	}
	return fitHop(w.hops)
}

// A window whose prefill pods held a queue is no reading of the hop with
// HopUnqueued on: R stays at the unqueued windows' 4 ms, held at their prompt.
// Off, the queued windows' 0.3 s sets a line that puts about 0.3 s of hop on
// every 8k prompt, as on the warm-up.
func TestHop_AQueuedPrefillWindowIsNoReading(t *testing.T) {
	off := queuedRun(false, true)
	require.Empty(t, off.Refused)
	assert.Greater(t, off.Base+off.PerToken*8000, 0.25, "off: the queue's 0.3 s is read as hop")

	on := queuedRun(true, true)
	require.Empty(t, on.Refused)
	assert.InDelta(t, 0.004, on.Base, 1e-9)
	assert.Zero(t, on.PerToken)
	assert.Equal(t, 1000.0, on.HeldPromptTokens)
	assert.Less(t, on.Windows, off.Windows)

	// A pod with no capacity series is read by its whole waiting count.
	whole := queuedRun(true, false)
	assert.Equal(t, on, whole)
}

// Only the capacity part of the waiting count is a queue: a pod holding
// deferred requests alone (its waiting count above its capacity part, which is
// zero) still teaches the hop.
func TestHop_ADeferredWaitIsNotAQueue(t *testing.T) {
	w := NewDecodeTermWindow()
	at := termT0
	for range 60 {
		c := hopCycle(at, 0.087, 10, 1000)
		c.HopUnqueued = true
		pre := &c.Prefill[0]
		pre.QueueLength, pre.CapacityQueueKnown, pre.CapacityQueueLength = 5, true, 0
		w.Observe(c)
		at = at.Add(15 * time.Second)
	}
	f := fitHop(w.hops)
	require.Empty(t, f.Refused)
	assert.InDelta(t, 0.007, f.Base, 1e-9)
}

// A prefill row that is not serving does not make the window queued.
func TestPrefillQueued_ReadsServingRows(t *testing.T) {
	pre := hopCycle(termT0, 0.1, 10, 1000).Prefill[0]
	assert.False(t, prefillQueued(nil))
	assert.False(t, prefillQueued([]domain.ReplicaMetrics{pre}))
	pre.QueueLength = 3
	assert.True(t, prefillQueued([]domain.ReplicaMetrics{pre}))
	pre.Ready = false
	assert.False(t, prefillQueued([]domain.ReplicaMetrics{pre}))
}
