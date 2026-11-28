package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A decode backlog is the capacity wait where vLLM names it, so a queue of KV
// pulls in flight is none; without that series it is unknown, never guessed
// from the queue's length.
func TestDecodeBacklog(t *testing.T) {
	n, known := DecodeBacklog(domain.ReplicaMetrics{QueueLength: 26, CapacityQueueKnown: true})
	assert.Zero(t, n)
	assert.True(t, known)
	n, known = DecodeBacklog(domain.ReplicaMetrics{QueueLength: 52, CapacityQueueLength: 48, CapacityQueueKnown: true})
	assert.Equal(t, 48, n)
	assert.True(t, known)
	n, known = DecodeBacklog(domain.ReplicaMetrics{QueueLength: 300})
	assert.Zero(t, n)
	assert.False(t, known)
}

// Of a variant's rows, the busiest serving row's backlog, and unread when a
// serving row does not say why it waits; a row that is not serving reads
// neither.
func TestDecodeBacklogOf(t *testing.T) {
	rows := []domain.ReplicaMetrics{
		{Ready: true, CapacityQueueLength: 3, CapacityQueueKnown: true},
		{Ready: true, CapacityQueueLength: 40, CapacityQueueKnown: true},
		{CapacityQueueLength: 900, CapacityQueueKnown: true},
		{Ready: true, FromWarmPool: true, QueueLength: 900},
	}
	n, unread := DecodeBacklogOf(rows)
	assert.Equal(t, 40, n)
	assert.False(t, unread)
	n, unread = DecodeBacklogOf(append(rows, domain.ReplicaMetrics{Ready: true, QueueLength: 300}))
	assert.Equal(t, 40, n)
	assert.True(t, unread)
	n, unread = DecodeBacklogOf(nil)
	assert.Zero(t, n)
	assert.False(t, unread)
}

// A backlog is carried for DecodeTTFTLag past the last cycle that read it,
// and no longer; a later backlog replaces it.
func TestDecodeBacklogCarry(t *testing.T) {
	var c DecodeBacklogCarry
	t0 := time.Unix(1000, 0)
	assert.Zero(t, c.Carry(0, t0), "nothing read yet")
	assert.Equal(t, 136, c.Carry(136, t0))
	assert.Equal(t, 136, c.Carry(0, t0.Add(DecodeTTFTLag)), "inside the lag")
	assert.Zero(t, c.Carry(0, t0.Add(DecodeTTFTLag+time.Second)), "past it")
	assert.Equal(t, 7, c.Carry(7, t0.Add(2*DecodeTTFTLag)))
	assert.Equal(t, 7, c.Carry(0, t0.Add(2*DecodeTTFTLag+time.Second)), "the latest backlog")
}

// One rule reads both backlogs (DecodeBacklogBy): the capacity planner's and
// the slo analyzer's decode TTFT backlog at the zero rule (DecodeBacklog),
// the capacity queue alone, and the saturation analyzer's decode capacity
// view with the landed pulls up to the full queue, and the full queue at
// the cap. One row of 30 waiting, 4 of them on capacity and 6 landed, reads
// 4 and 10; at its cap the view reads it whole and the backlog still 4.
// saturation's TestParity_TheDecodeBacklogIsOneRule pins the view's side.
func TestDecodeBacklogBy_TheTwoRules(t *testing.T) {
	rm := domain.ReplicaMetrics{QueueLength: 30, CapacityQueueKnown: true, CapacityQueueLength: 4,
		LandedQueueKnown: true, LandedQueueLength: 6, RunningRequests: 12}
	n, known := DecodeBacklog(rm)
	assert.Equal(t, 4, n)
	assert.True(t, known)
	view := DecodeBacklogRule{Landed: true, MaxNumSeqs: 16}
	waiting, busiest, ok := DecodeBacklogBy(rm, view)
	assert.True(t, ok)
	assert.Equal(t, 10, waiting)
	assert.Equal(t, 10, busiest, "one rank")

	rm.LandedQueueLength = 40
	waiting, _, _ = DecodeBacklogBy(rm, view)
	assert.Equal(t, 30, waiting, "never past the full queue")

	rm.RunningRequests = 16
	_, _, ok = DecodeBacklogBy(rm, view)
	assert.False(t, ok, "at the cap the queue is read whole")
	n, known = DecodeBacklog(rm)
	assert.Equal(t, 4, n, "the zero rule states no cap")
	assert.True(t, known)
}
